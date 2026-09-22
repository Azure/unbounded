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
import shutil
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
CRATE = ROOT / "cmd/racer-dataplane"
MANIFEST = ROOT / "dst/scenarios/baseline.json"
MEMORY_MAX = 23_000_000_000
ADAPTER = "runtime::dst::artifact_campaign"
ARTIFACT_SCENARIOS = {"artifact", "overlap-reconfigure-restart", "overlap-namespace", "overlap-checkpoint-crash"}
OUTCOMES = {"pass", "product_failure", "simulator_failure", "replay_divergence",
            "infrastructure_failure", "unexercised", "optional_skip", "invalid_scenario"}


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
    if "replay divergence:" in output:
        return "replay_divergence"
    if "infrastructure:" in output:
        return "infrastructure_failure"
    if "invalid scenario" in output:
        return "invalid_scenario"
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
                      "coverage": coverage(directory),
                      "passed_tests": sum(r["tests"] for r in result["runs"] if r["outcome"] == "pass")}, indent=2))
    return 0 if result["complete"] and all(r["outcome"] == "pass" for r in result["runs"]) else 1


def coverage(directory):
    """Summarize typed witnesses, never infer overlap from a scenario name."""
    path = directory / "journal.jsonl"
    counts = collections.Counter()
    faults = collections.defaultdict(set)
    live_faults = set()
    peak = 0
    terminal = False
    if path.exists():
        with path.open() as stream:
            for line in stream:
                try:
                    item = json.loads(json.loads(line)["payload"])
                except (ValueError, KeyError, TypeError):
                    break  # An interrupted journal is a prefix, not full coverage.
                if item.get("kind") == "terminal":
                    terminal = True
                if item.get("kind") != "history":
                    continue
                transition = item["value"]["transition"]
                kind, fields = next(iter(transition.items()))
                counts[kind] += 1
                if kind.startswith("Fault"):
                    fault = fields["fault"]
                    faults[kind].add(fault)
                    if kind == "FaultEffective":
                        live_faults.add(fault)
                    elif kind == "FaultReleased":
                        live_faults.discard(fault)
                    peak = max(peak, len(live_faults))
                elif kind == "Publish" and len(live_faults) >= 2:
                    counts["publication_with_two_effective_faults"] += 1
    return {"terminal_record_present": terminal, "transitions": dict(counts),
            "faults": {kind: len(ids) for kind, ids in faults.items()},
            "peak_effective_faults": peak}


def run(args, retained_binary=None, deadline=None):
    manifest = json.loads(MANIFEST.read_text())
    if deadline is not None:
        manifest["build_timeout_seconds"] = min(manifest["build_timeout_seconds"], max(0.01, deadline - time.monotonic()))
    if manifest["schema"] != 1 or manifest["memory_bytes"] > MEMORY_MAX:
        raise ValueError("unsupported manifest or memory budget")
    directory = args.artifacts.resolve()
    directory.mkdir(parents=True, exist_ok=False)
    result = {"schema": 1, "complete": False, "planned": 0, "runs": []}
    save(directory / "result.json", result)
    try:
        binary, build_seconds = (retained_binary, 0) if retained_binary else build(directory, manifest)
        if args.scenario in ARTIFACT_SCENARIOS:
            if retained_binary:
                os.link(binary, directory / "libtest")
            else:
                shutil.copy2(binary, directory / "libtest")
            binary = directory / "libtest"
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
            "platform": sys.platform, "profile": "test", "features": [],
            "build_environment": {key: value for key, value in os.environ.items()
                                  if key.startswith("CARGO_PROFILE_") or key in
                                  ("CARGO_INCREMENTAL", "RUSTFLAGS", "CARGO_ENCODED_RUSTFLAGS")}})
        suites = [s for s in manifest["suites"]
                  if (s["id"] == args.scenario if args.scenario else s["tier"] == args.profile)]
        if args.scenario in ARTIFACT_SCENARIOS:
            suites = [{"id": "artifact", "selector": ADAPTER, "tier": "pr", "timeout_seconds": 90}]
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
            if suite["id"] == "artifact":
                command.append("--exact")
                env.update(RACER_DST_INPUT=str(directory / "input.json"),
                           RACER_DST_SCENARIO=args.scenario,
                           RACER_DST_JOURNAL=str(directory / "journal.jsonl"),
                           RACER_DST_RESULT=str(directory / "semantic.json"), RACER_DST_MODE="record")
                if args.input:
                    shutil.copyfile(args.input, directory / "input.json")
            for entry in entries:
                if entry["ignored"] and suite["selector"] in entry["selector"]:
                    command.extend(["--skip", entry["selector"]])
            seconds = suite["timeout_seconds"]
            if deadline is not None:
                seconds = min(seconds, max(0.01, deadline - time.monotonic()))
            code, output, timed_out, elapsed = execute(command, seconds, env)
            (directory / f"{suite['id']}.log").write_text(output)
            record = {"suite": suite["id"], "seed": args.seed, "tests": len(names),
                      "selectors": names, "exit_code": code, "timeout": timed_out,
                      "seconds": elapsed, "outcome": classify(code, output, timed_out, len(names))}
            if suite["id"] == "artifact" and (directory / "semantic.json").exists():
                semantic = json.loads((directory / "semantic.json").read_text())
                if record["outcome"] == "product_failure":
                    record["outcome"] = semantic["status"]
            result["runs"].append(record)
            save(directory / "result.json", result)
            print(f"{suite['id']}: {record['outcome']} ({elapsed:.2f}s)", flush=True)
        result["complete"] = True
    except (OSError, ValueError, RuntimeError) as error:
        result["runs"].append({"suite": "runner", "tests": 0,
                               "outcome": "infrastructure_failure", "error": str(error)})
    save(directory / "result.json", result)
    return report(directory)


def replay(directory, seconds=90):
    directory = directory.resolve()
    try:
        metadata = json.loads((directory / "build.json").read_text())
        binary = directory / "libtest"
        if not binary.exists():
            binary = Path(metadata["binary"])
        if digest(binary) != metadata["binary_sha256"]:
            raise ValueError("exact replay requires the recorded binary hash")
        if ADAPTER not in discover(binary):
            raise ValueError("exact adapter missing")
        env = dict(os.environ, RUST_TEST_THREADS="1", RACER_DST_MODE="exact",
                   RACER_DST_INPUT=str(directory / "input.json"),
                   RACER_DST_JOURNAL=str(directory / "journal.jsonl"),
                   RACER_DST_RESULT=str(directory / "replay-semantic.json"))
        (directory / "replay-semantic.json").unlink(missing_ok=True)
        code, output, timed_out, elapsed = execute(
            [str(binary), ADAPTER, "--exact", "--test-threads=1"], seconds, env)
        (directory / "replay.log").write_text(output)
        outcome = classify(code, output, timed_out, 1)
        # A recorded failure must reproduce its terminal record too. The Rust adapter
        # validates it before writing semantic output, so arbitrary panics do not pass.
        semantic = directory / "replay-semantic.json"
        if outcome == "product_failure" and semantic.exists():
            if json.loads(semantic.read_text()) == json.loads((directory / "semantic.json").read_text()):
                outcome = "pass"
        save(directory / "replay-result.json", {"outcome": outcome, "seconds": elapsed,
                                               "exit_code": code, "timeout": timed_out})
        print(f"exact replay: {outcome}")
        return int(outcome != "pass")
    except (OSError, ValueError, RuntimeError) as error:
        print(f"replay infrastructure failure: {error}", file=sys.stderr)
        return 1


def failure_identity(semantic):
    if semantic.get("status") != "product_failure":
        return None
    failure = semantic.get("failure")
    return failure.get("oracle") if isinstance(failure, dict) else None


def gate(cell, outcome, semantic, witnesses, replayed, controls):
    """A required cell needs execution, its contract, witnesses, and exact replay."""
    if outcome != cell["expected"]:
        return outcome if outcome != "pass" else "unexercised"
    if cell.get("oracle") and failure_identity(semantic) != cell["oracle"]:
        return "unexercised"
    if cell.get("control") and controls.get(cell["control"]) != "pass":
        return "unexercised"
    if not replayed:
        return "replay_divergence"
    if not witnesses["terminal_record_present"]:
        return "unexercised"
    if any(witnesses["transitions"].get(kind, 0) < minimum
           for kind, minimum in cell["minimum_transitions"].items()):
        return "unexercised"
    return "pass"


def run_campaign(args):
    directory = args.artifacts.resolve()
    directory.mkdir(parents=True, exist_ok=False)
    definition = json.loads((ROOT / "dst/scenarios/campaign.json").read_text())
    if definition["schema"] != 1:
        raise ValueError("unsupported campaign schema")
    cells = definition["cells"]
    if args.tier == "nightly":
        # Constrained sampling only extends implemented cells. Each derived seed
        # is saved here and each adapter saves all resolved domain seeds.
        for index in range(args.samples):
            seed = int.from_bytes(hashlib.sha256(f"dst/nightly/v1/{args.seed}/{index}".encode()).digest()[:8], "little")
            template = cells[index % 2]
            cells.append(dict(template, id=f"sample-{index:04d}", seed=seed))
    save(directory / "campaign-manifest.json", definition)
    result = {"complete": False, "tier": args.tier, "planned": len(cells), "runs": []}
    save(directory / "campaign-result.json", result)
    deadline = time.monotonic() + args.timeout
    retained = None
    controls = {}
    try:
        for cell in cells:
            if time.monotonic() >= deadline:
                raise RuntimeError("campaign host deadline exhausted")
            source = None
            if cell.get("input"):
                source = ROOT / "dst/scenarios" / cell["input"]
                if cell.get("disable_mutant"):
                    control = json.loads(source.read_text())
                    control["mutant"] = None
                    source = directory / f"{cell['id']}-input.json"
                    save(source, control)
            bundle = directory / cell["id"]
            run(argparse.Namespace(artifacts=bundle, scenario=cell["scenario"],
                                   profile="pr", seed=cell["seed"], input=source), retained, deadline)
            execution = json.loads((bundle / "result.json").read_text())
            outcome = execution["runs"][-1]["outcome"]
            if retained is None and (bundle / "libtest").exists():
                retained = bundle / "libtest"
            semantic_path = bundle / "semantic.json"
            semantic = json.loads(semantic_path.read_text()) if semantic_path.exists() else {}
            replayed = False
            if semantic and outcome in {"pass", "product_failure"} and time.monotonic() < deadline:
                replayed = replay(bundle, min(90, deadline - time.monotonic())) == 0
            witnesses = coverage(bundle)
            verdict = gate(cell, outcome, semantic, witnesses, replayed, controls)
            controls[cell["id"]] = verdict
            result["runs"].append({"cell": cell["id"], "outcome": verdict,
                                   "record_outcome": outcome, "exact_replay": replayed,
                                   "coverage": witnesses})
            save(directory / "campaign-result.json", result)
        result["complete"] = True
    except (OSError, ValueError, RuntimeError) as error:
        result["error"] = str(error)
    result["gated"] = sum(item["outcome"] == "pass" for item in result["runs"])
    save(directory / "campaign-result.json", result)
    print(json.dumps(result, indent=2))
    return int(not result["complete"] or result["gated"] != result["planned"])


def deletions(actions):
    """Coarse-to-fine deletion proposals; each candidate gets a new execution."""
    width = max(1, len(actions) // 2)
    while actions:
        for start in range(0, len(actions), width):
            yield actions[:start] + actions[start + width:]
        if width == 1:
            return
        width = max(1, width // 2)


def witness_signature(records):
    """Conservative path contract, independent of ticks and allocated request IDs."""
    armed = {}
    required = set()
    terminal = False
    for record in records:
        if record.get("kind") == "terminal":
            terminal = True
        if record.get("kind") != "history":
            continue
        value = record["value"]
        kind, fields = next(iter(value["transition"].items()))
        fields = dict(fields)
        if kind == "Invoke":
            continue  # Deleting unrelated callers is the purpose of reduction.
        fields.pop("request", None)
        if kind == "FaultArmed":
            armed[fields["fault"]] = {"node": value.get("node"),
                                      "target": fields.get("target")}
        if "fault" in fields:
            fault = fields.pop("fault")
            if fault not in armed:
                raise ValueError("fault witness has no preceding arm record")
            fields["scope"] = armed[fault]
        required.add(json.dumps({"kind": kind, "fields": fields,
                                 "node": value.get("node"),
                                 "worker": value.get("worker", 0),
                                 "incarnation": value.get("incarnation")}, sort_keys=True))
    if not terminal:
        raise ValueError("reduction requires a complete terminal witness history")
    return required


def reduction_witnesses(directory):
    # Source and accepted candidate integrity is established by exact replay.
    # This pass extracts semantic requirements, not a second checksum validator.
    with (directory / "journal.jsonl").open() as stream:
        return witness_signature(json.loads(json.loads(line)["payload"]) for line in stream)


def reduce_artifact(args):
    source, destination = args.directory.resolve(), args.artifacts.resolve()
    destination.mkdir(parents=True, exist_ok=False)
    start = time.monotonic()
    deadline = start + args.timeout
    attempts = []
    summary = {"complete": False, "attempts": attempts, "accepted": None}
    save(destination / "reduction.json", summary)
    try:
        identity = failure_identity(json.loads((source / "semantic.json").read_text()))
        if not identity:
            raise ValueError("reduction requires a named product failure")
        if replay(source, min(90, max(0.01, deadline - time.monotonic()))):
            raise ValueError("source does not exactly replay")
        witnesses = reduction_witnesses(source)
        metadata = json.loads((source / "build.json").read_text())
        binary = source / "libtest"
        if not binary.exists():
            binary = Path(metadata["binary"])
        current = json.loads((source / "input.json").read_text())
        if any(current.get(key) for key in ("overlap", "namespace_overlap", "checkpoint_overlap", "flight_cancellation", "local_attribution")):
            raise ValueError("actor reduction requires configurable actor inputs")
        summary.update(oracle=identity, original_actions=len(current["actions"]),
                       required_witnesses=[json.loads(item) for item in sorted(witnesses)])
        changed = True
        while changed and len(attempts) < args.max_candidates and time.monotonic() < deadline:
            changed = False
            for actions in deletions(current["actions"]):
                if len(attempts) >= args.max_candidates or time.monotonic() >= deadline:
                    break
                candidate = destination / f"candidate-{len(attempts):04d}"
                candidate.mkdir()
                # Hard links retain a standalone, hash-checked executable without
                # duplicating hundreds of MB for every rejected proposal.
                os.link(binary, candidate / "libtest")
                save(candidate / "build.json", metadata)
                if (source / "source.patch").exists():
                    shutil.copyfile(source / "source.patch", candidate / "source.patch")
                proposal = dict(current, actions=actions)
                save(candidate / "input.json", proposal)
                env = dict(os.environ, RUST_TEST_THREADS="1", RACER_DST_MODE="record",
                           RACER_DST_INPUT=str(candidate / "input.json"),
                           RACER_DST_JOURNAL=str(candidate / "journal.jsonl"),
                           RACER_DST_RESULT=str(candidate / "semantic.json"))
                code, output, expired, elapsed = execute(
                    [str(candidate / "libtest"), ADAPTER, "--exact", "--test-threads=1"],
                    min(90, max(0.01, deadline - time.monotonic())), env)
                (candidate / "artifact.log").write_text(output)
                outcome = classify(code, output, expired, 1)
                semantic = candidate / "semantic.json"
                same = (outcome == "product_failure" and semantic.exists()
                        and failure_identity(json.loads(semantic.read_text())) == identity)
                preserved = same and witnesses.issubset(reduction_witnesses(candidate))
                accepted = (preserved and time.monotonic() < deadline and replay(
                    candidate, min(90, max(0.01, deadline - time.monotonic()))) == 0)
                attempts.append({"candidate": candidate.name, "actions": len(actions),
                                 "outcome": outcome, "witnesses_preserved": preserved,
                                 "accepted": accepted, "seconds": elapsed})
                if accepted:
                    current = proposal
                    summary["accepted"] = candidate.name
                    changed = True
                summary["remaining_actions"] = len(current["actions"])
                save(destination / "reduction.json", summary)
                if accepted:
                    break
        summary.update(complete=True, budget_exhausted=(time.monotonic() >= deadline
                       or len(attempts) >= args.max_candidates), seconds=time.monotonic() - start)
    except (OSError, ValueError, RuntimeError) as error:
        summary["error"] = str(error)
    save(destination / "reduction.json", summary)
    print(json.dumps(summary, indent=2))
    return int(not summary["complete"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    campaign = sub.add_parser("run")
    campaign.add_argument("--profile", choices=["pr", "native"], default="pr")
    campaign.add_argument("--scenario")
    campaign.add_argument("--seed", type=int, default=19)
    campaign.add_argument("--input", type=Path, help="resolved artifact scenario with explicit seeds and actions")
    campaign.add_argument("--artifacts", type=Path,
                          default=ROOT / "dst/artifacts" / time.strftime("%Y%m%d-%H%M%S"))
    summary = sub.add_parser("report")
    summary.add_argument("directory", type=Path)
    exact = sub.add_parser("replay")
    exact.add_argument("directory", type=Path)
    reducer = sub.add_parser("reduce")
    reducer.add_argument("directory", type=Path)
    reducer.add_argument("--artifacts", type=Path, required=True)
    reducer.add_argument("--timeout", type=float, default=300)
    reducer.add_argument("--max-candidates", type=int, default=64)
    matrix = sub.add_parser("campaign")
    matrix.add_argument("--tier", choices=["pr", "nightly"], default="pr")
    matrix.add_argument("--seed", type=int, default=19)
    matrix.add_argument("--samples", type=int, default=8)
    matrix.add_argument("--timeout", type=float, default=600)
    matrix.add_argument("--artifacts", type=Path, required=True)
    args = parser.parse_args()
    if args.command == "report":
        return report(args.directory)
    enforce_memory()
    if args.command == "campaign":
        if args.timeout <= 0 or not 1 <= args.samples <= 256:
            parser.error("campaign requires a positive timeout and 1..256 samples")
        return run_campaign(args)
    if args.command == "replay":
        return replay(args.directory)
    if args.command == "reduce":
        if args.timeout <= 0 or args.max_candidates <= 0:
            parser.error("reduction budgets must be positive")
        return reduce_artifact(args)
    return run(args)


if __name__ == "__main__":
    sys.exit(main())
