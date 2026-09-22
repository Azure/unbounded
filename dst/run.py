#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Bounded Racer libtest campaigns. Only stdlib dependencies are required."""

import argparse
import collections
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
CRATE = ROOT / "cmd/racer-dataplane"
MANIFEST = ROOT / "dst/scenarios/baseline.json"
MEMORY_MAX = 23_000_000_000
OUTCOMES = {"pass", "product_failure", "simulator_failure", "replay_divergence",
            "infrastructure_failure", "unexercised", "optional_skip"}


def save(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def execute(command, seconds, env=None):
    start = time.monotonic()
    process = subprocess.Popen(command, cwd=ROOT, env=env, start_new_session=True,
                               stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    timed_out = False
    try:
        output, _ = process.communicate(timeout=seconds)
    except subprocess.TimeoutExpired:
        timed_out = True
        os.killpg(process.pid, signal.SIGKILL)
        output, _ = process.communicate()
    return process.returncode, output.decode(errors="replace"), timed_out, time.monotonic() - start


def checked(command, seconds=10):
    code, output, timed_out, _ = execute(command, seconds)
    if code or timed_out:
        raise RuntimeError(f"command failed ({code}, timeout={timed_out}): {command}\n{output}")
    return output


def enforce_memory():
    # Verify the actual inherited cgroup, never trust an environment marker.
    membership = Path("/proc/self/cgroup").read_text().splitlines()
    relative = next(line[3:] for line in membership if line.startswith("0::"))
    group = Path("/sys/fs/cgroup") / relative.lstrip("/")
    while group != Path("/sys/fs"):
        limit = group / "memory.max"
        swap = group / "memory.swap.max"
        if limit.exists() and swap.exists():
            value = limit.read_text().strip()
            if value != "max" and int(value) <= MEMORY_MAX and swap.read_text().strip() == "0":
                return
        group = group.parent
    # The whole campaign (including compiler children) shares this hard ceiling.
    os.execvp("systemd-run", ["systemd-run", "--user", "--scope", "--quiet",
                             "-p", f"MemoryMax={MEMORY_MAX}", "-p", "MemorySwapMax=0",
                             sys.executable, str(Path(__file__).resolve()), *sys.argv[1:]])


def build(directory, manifest):
    code, output, timed_out, elapsed = execute(
        ["cargo", "test", "--manifest-path", str(CRATE / "Cargo.toml"),
         "--locked", "--lib", "--no-run", "-j", "2", "--message-format=json"],
        manifest["build_timeout_seconds"])
    (directory / "build.log").write_text(output)
    if code or timed_out:
        raise RuntimeError(f"build failed ({code}, timeout={timed_out}); see build.log")
    binaries = []
    for line in output.splitlines():
        if not line.startswith("{"):
            continue
        item = json.loads(line)
        if (item.get("reason") == "compiler-artifact" and item.get("executable")
                and item["profile"]["test"] and "lib" in item["target"]["kind"]):
            binaries.append(item["executable"])
    if len(binaries) != 1:
        raise RuntimeError(f"expected exactly one libtest executable, found {binaries}")
    return Path(binaries[0]), elapsed


def discover(binary, ignored=False):
    args = [str(binary), "--list", "--format=terse"]
    if ignored:
        args.append("--ignored")
    return sorted(line.removesuffix(": test") for line in checked(args).splitlines()
                  if line.endswith(": test"))


def inventory(binary, manifest):
    names, ignored = discover(binary), set(discover(binary, True))
    entries = []
    for name in names:
        suite = next(s for s in manifest["suites"] if s["selector"] in name)
        entries.append({"selector": name, "ignored": name in ignored,
                        "suite": suite["id"], "owner": suite["owner"],
                        "tier": "opt-in" if name in ignored else suite["tier"],
                        "contract": suite["contract"]})
    for regression in manifest["regressions"]:
        name = regression["selector"]
        if name not in names or name in ignored:
            raise RuntimeError(f"required regression missing or ignored: {name}")
    return entries


def classify(code, output, timed_out, count):
    if timed_out or code < 0:
        return "infrastructure_failure"
    # Libtest accepts a selector that matches zero tests. Require the full count.
    match = re.search(r"test result: ok\. (\d+) passed; 0 failed; 0 ignored;", output)
    if code == 0:
        return "pass" if match and int(match[1]) == count and count > 0 else "unexercised"
    return "product_failure" if "test result: FAILED." in output else "infrastructure_failure"


def report(directory):
    result = json.loads((directory / "result.json").read_text())
    totals = collections.Counter(run["outcome"] for run in result["runs"])
    print(json.dumps({"outcomes": totals, "planned": result["planned"],
                      "attempted": len(result["runs"]),
                      "passed_tests": sum(r["tests"] for r in result["runs"] if r["outcome"] == "pass")}, indent=2))
    return 0 if result["complete"] and all(r["outcome"] == "pass" for r in result["runs"]) else 1


def run(args):
    manifest = json.loads(MANIFEST.read_text())
    if manifest["schema"] != 1 or manifest["memory_bytes"] > MEMORY_MAX:
        raise ValueError("unsupported manifest or memory budget")
    directory = args.artifacts.resolve()
    directory.mkdir(parents=True, exist_ok=False)
    result = {"schema": 1, "complete": False, "planned": 0, "runs": []}
    save(directory / "result.json", result)
    try:
        binary, build_seconds = build(directory, manifest)
        entries = inventory(binary, manifest)
        save(directory / "inventory.json", entries)
        patch = checked(["git", "diff", "HEAD", "--binary"])
        (directory / "source.patch").write_text(patch)
        save(directory / "build.json", {
            "revision": checked(["git", "rev-parse", "HEAD"]).strip(),
            "untracked": checked(["git", "ls-files", "--others", "--exclude-standard"]).splitlines(),
            "patch_sha256": digest(directory / "source.patch"),
            "binary": str(binary), "binary_sha256": digest(binary),
            "rustc": checked(["rustc", "--version", "--verbose"]),
            "build_seconds": build_seconds, "memory_bytes": MEMORY_MAX,
            "platform": sys.platform, "profile": "test", "features": []})
        suites = [s for s in manifest["suites"]
                  if (s["id"] == args.scenario if args.scenario else s["tier"] == args.profile)]
        if not suites:
            raise ValueError("no matching scenario")
        result["planned"] = len(suites)
        save(directory / "manifest.json", manifest)
        for suite in suites:
            names = [e["selector"] for e in entries
                     if suite["selector"] in e["selector"] and not e["ignored"]]
            if not names:
                raise RuntimeError(f"zero test matches: {suite['id']}")
            env = dict(os.environ, RUST_TEST_THREADS="1", RACER_DST_SEED=str(args.seed))
            if suite["tier"] == "native":
                env["RACER_REQUIRE_URING"] = "1"
            command = [str(binary), suite["selector"], "--test-threads=1"]
            for entry in entries:
                if entry["ignored"] and suite["selector"] in entry["selector"]:
                    command.extend(["--skip", entry["selector"]])
            code, output, timed_out, elapsed = execute(command, suite["timeout_seconds"], env)
            (directory / f"{suite['id']}.log").write_text(output)
            record = {"suite": suite["id"], "seed": args.seed, "tests": len(names),
                      "selectors": names, "exit_code": code, "timeout": timed_out,
                      "seconds": elapsed, "outcome": classify(code, output, timed_out, len(names))}
            result["runs"].append(record)
            save(directory / "result.json", result)
            print(f"{suite['id']}: {record['outcome']} ({elapsed:.2f}s)", flush=True)
        result["complete"] = True
    except (OSError, ValueError, RuntimeError) as error:
        result["runs"].append({"suite": "runner", "tests": 0,
                               "outcome": "infrastructure_failure", "error": str(error)})
    save(directory / "result.json", result)
    return report(directory)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    campaign = sub.add_parser("run")
    campaign.add_argument("--profile", choices=["pr", "native"], default="pr")
    campaign.add_argument("--scenario")
    campaign.add_argument("--seed", type=int, default=19)
    campaign.add_argument("--artifacts", type=Path,
                          default=ROOT / "dst/artifacts" / time.strftime("%Y%m%d-%H%M%S"))
    summary = sub.add_parser("report")
    summary.add_argument("directory", type=Path)
    args = parser.parse_args()
    if args.command == "report":
        return report(args.directory)
    enforce_memory()
    return run(args)


if __name__ == "__main__":
    sys.exit(main())
