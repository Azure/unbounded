#!/usr/bin/env python3
# SPDX-License-Identifier: Apache-2.0
"""Build once, then run a time-budgeted default and fresh-seed DST campaign."""

import argparse
import json
import math
import os
from pathlib import Path
import shlex
import signal
import subprocess
import sys
import time


GENERATED = (
    "app::dst::dst_generated_traffic_churn_oracle",
    "app::dst::dst_generated_native_traffic_churn_oracle",
)


class Failure(Exception):
    pass


def bounded_seconds(value):
    value = float(value)
    if not math.isfinite(value) or not 0 < value <= 300:
        raise argparse.ArgumentTypeError("must be in (0, 300] seconds")
    return value


def positive_seconds(value):
    value = float(value)
    if not math.isfinite(value) or not 0 < value <= 600:
        raise argparse.ArgumentTypeError("must be in (0, 600] seconds")
    return value


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--seconds", type=positive_seconds, default=600)
    result.add_argument("--case-timeout", type=bounded_seconds, default=300)
    result.add_argument("--target-dir", required=True)
    result.add_argument("--log-dir", default="tmp/racer-dst-campaign")
    result.add_argument("cargo", nargs=argparse.REMAINDER)
    return result


class Runner:
    def __init__(self, directory, case_timeout):
        self.directory = directory
        self.case_timeout = case_timeout
        self.index = 0
        self.progress = directory / "progress.log"

    def report(self, message):
        line = f"{time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())} {message}"
        print(line, flush=True)
        with self.progress.open("a") as output:
            output.write(line + "\n")

    def run(self, command, environment, label, limit=None):
        self.index += 1
        log = self.directory / f"{self.index:04d}-{label}.log"
        limit = self.case_timeout if limit is None else limit
        command = ["timeout", "--signal=TERM", "--kill-after=10s", f"{limit}s", *command]
        settings = [f"{key}={value}" for key, value in environment.items()
                    if key.startswith("RACER_DST_")]
        reproduction = shlex.join([
            "env", "-u", "RACER_DST_SEEDS", "-u", "RACER_DST_STEPS",
            *settings, "bash", "hack/scripts/memory-safe-run.sh", "--", *command,
        ])
        self.report(f"START {label} log={log} reproduce: {reproduction}")
        # timeout is inside the verified memory cgroup, not outside systemd-run.
        # Its process group includes Cargo/test descendants even in a new scope.
        with log.open("w") as output:
            process = subprocess.Popen(command, env=environment, stdout=output,
                                       stderr=subprocess.STDOUT, start_new_session=True)
            try:
                code = process.wait(timeout=limit + 15)
            finally:
                # Also clean up descendants if the leader exits without reaping
                # them, or this runner receives SIGTERM/KeyboardInterrupt.
                try:
                    try:
                        os.killpg(process.pid, signal.SIGTERM)
                    except ProcessLookupError:
                        pass
                    process.wait(timeout=10)
                finally:
                    try:
                        os.killpg(process.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    process.wait(timeout=10)
        if code:
            kind = "COMMAND FAILURE"
            if code == 124:
                kind = "PER-COMMAND TIMEOUT"
            elif code in (137, -signal.SIGKILL):
                # SIGKILL can also mean the cgroup OOM killer, not timeout.
                kind = "COMMAND KILLED (timeout escalation or external/OOM kill)"
            raise Failure(f"{kind}: {label} exit={code}; log={log}; reproduce: {reproduction}")
        self.report(f"PASS {label}")
        return log.read_text()


def discover(runner, cargo, target, environment):
    output = runner.run([
        *cargo, "test", "--locked", "--manifest-path", "cmd/racer-dataplane/Cargo.toml",
        "--target-dir", target, "--all-features", "--lib", "--bins", "--jobs", "2",
        "--no-run", "--message-format=json",
    ], environment, "build", limit=300)
    binaries = set()
    for line in output.splitlines():
        if not line.startswith("{"):
            continue
        artifact = json.loads(line)
        if (artifact.get("reason") == "compiler-artifact"
                and artifact.get("profile", {}).get("test") and artifact.get("executable")):
            binaries.add(artifact["executable"])
    tests = []
    for binary in sorted(binaries):
        listing = runner.run([binary, "dst", "--list", "--format=terse"],
                             environment, "list", limit=30)
        tests.extend((binary, line.removesuffix(": test"))
                     for line in listing.splitlines() if line.endswith(": test"))
    generated = [(binary, name) for binary, name in tests if name in GENERATED]
    if len(generated) != 2 or {name for _, name in generated} != set(GENERATED):
        raise Failure("discovery must find both generated DST oracles exactly once")
    if len({binary for binary, _ in generated}) != 1:
        raise Failure("generated DST oracles must share one test executable")
    return tests, generated[0][0]


def run_tests(runner, binary, names, environment, label):
    output = runner.run([binary, *names, "--exact", "--test-threads=1", "--nocapture"],
                        environment, label)
    # A renamed/missing filter must never become a green zero-test campaign.
    summary = f"test result: ok. {len(names)} passed; 0 failed; 0 ignored;"
    if summary not in output:
        raise Failure(f"{label}: missing expected successful test count; see command log")


def campaign(runner, tests, binary, environment, seconds, clock=time.monotonic):
    start = clock()
    completed = 0
    pairs = 0
    try:
        for executable, name in tests:
            if clock() - start >= seconds:
                raise Failure("BUDGET EXHAUSTED: required baseline incomplete")
            run_tests(runner, executable, [name], environment, f"baseline-{completed + 1}")
            completed += 1
            runner.report(f"BASELINE completed={completed}/{len(tests)}")
        seed = 100
        while clock() - start < seconds:
            custom = dict(environment, RACER_DST_SEEDS=str(seed))
            run_tests(runner, binary, GENERATED, custom, f"seed-{seed}")
            pairs += 1
            runner.report(f"SEEDS completed_pairs={pairs} completed_mode_runs={2 * pairs} last_seed={seed}")
            seed += 1
        if not pairs:
            raise Failure("BUDGET EXHAUSTED: no fresh HTTP/native seed pair completed")
        runner.report("SUCCESS: budget reached; final seed pair finished without interruption")
    finally:
        runner.report(f"SUMMARY baseline={completed}/{len(tests)} completed_seed_pairs={pairs} "
                      f"completed_custom_mode_runs={2 * pairs} elapsed_seconds={clock() - start:.3f}")


def main():
    args = parser().parse_args()
    cargo = args.cargo
    if cargo[:1] == ["--"]:
        cargo = cargo[1:]
    if not cargo:
        raise Failure("missing Cargo command after --")
    # Fail rather than silently reduce the baseline's mandatory coverage or
    # accept a misspelled knob. Campaign options deliberately are not env vars.
    overrides = sorted(key for key in os.environ if key.startswith("RACER_DST_")
                       or key == "RACER_TEST_ARGS")
    if overrides:
        raise Failure(f"campaign does not accept test overrides: {', '.join(overrides)}")
    # Verify even when invoked directly. This script never offers an unsafe mode.
    root = Path(__file__).resolve().parents[2]
    subprocess.run(["timeout", "--signal=TERM", "--kill-after=10s", "30s", "bash",
                    str(root / "hack/scripts/memory-safe-run.sh"), "--verify-exec", "--", "true"],
                   check=True, timeout=45)
    directory = Path(args.log_dir) / f"{time.strftime('%Y%m%dT%H%M%S')}-{os.getpid()}"
    directory.mkdir(parents=True, exist_ok=False)
    runner = Runner(directory, args.case_timeout)
    try:
        environment = dict(os.environ)
        tests, binary = discover(runner, cargo, args.target_dir, environment)
        runner.report(f"BUILD/DISCOVERY COMPLETE; starting {args.seconds:g}s test budget")
        campaign(runner, tests, binary, environment, args.seconds)
    except BaseException as error:
        runner.report(f"FAILED: {error}")
        raise


if __name__ == "__main__":
    def terminate(_signal, _frame):
        raise Failure("interrupted by SIGTERM")

    signal.signal(signal.SIGTERM, terminate)
    try:
        main()
    except (Failure, OSError, subprocess.SubprocessError, KeyboardInterrupt) as error:
        print(f"racer-dst-campaign: {error}", file=sys.stderr)
        sys.exit(1)
