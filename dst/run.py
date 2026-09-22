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
import resource
import signal
import shutil
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
CRATE = ROOT / "cmd/racer-dataplane"
MANIFEST = ROOT / "dst/scenarios/baseline.json"
MEMORY_MAX = 23_000_000_000
# Admission headroom for compiler output, portable binaries, journals, and native
# scratch files, not a reservation or an upper bound on campaign disk consumption.
MIN_FREE_DISK_BYTES = 10 * 1024**3
ADAPTER = "runtime::dst::artifact_campaign"
ARTIFACT_SCENARIOS = {"artifact", "overlap-reconfigure-restart", "overlap-namespace", "overlap-checkpoint-crash"}
OUTCOMES = {"pass", "product_failure", "simulator_failure", "replay_divergence",
            "infrastructure_failure", "unexercised", "optional_skip", "invalid_scenario"}
COMPOSITION_VERSION = "composition-v2"
LIFECYCLE_VERSION = "composition-v3"
LIFECYCLE_PER_ROUND = {
    "LifecycleRoundPlanned": 1, "LifecycleFaultCohort": 2,
    "LifecyclePublicationOverlap": 1, "LifecycleHealthyProgress": 1,
    "LifecycleCrashOverlap": 1, "LifecycleRestarted": 1, "LifecycleRoundRecovered": 1,
    "FaultArmed": 2, "FaultEffective": 2, "FaultReleased": 2, "Publish": 2,
    "DurabilityWitness": 1, "DirtyCheckpointCrash": 1, "DurableRecovery": 1,
    "Cancel": 2, "ProcessLost": 2, "Response": 8,
}
BUFFER_SIZE = 4 * 1024 * 1024  # buffers.rs; the response destination is one page.
COMPOSITION_SOCKET_CAPACITIES = (4096, 16384, 65536)
COMPOSITION_CAPACITY_POLICY = "page-fill-deadline-floor-v1"
COMPOSITION_BOUNDS = {"nodes": 4, "windows": 4, "live_requests": 6,
                      "actions": 80, "turns_per_window": 4,
                      "object_bytes": BUFFER_SIZE + 1, "overlap_wait_ticks": 500}
COMPOSITION_LIMITS = {
    "required_action": "AwaitOverlap",
    "overlap": "entire post-arm cohort accepted and live at an effective held peer Request gate",
    "stream_capacity": "sample 4096/16384/65536 bytes; objects >= BUFFER_SIZE-1 require at least 65536 bytes",
    "capacity_policy": COMPOSITION_CAPACITY_POLICY,
    "uncovered": ["arbitrary simultaneous action gates and live-request reloads"],
}


def save(path, value):
    try:
        path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")
    except OSError as error:
        print(json.dumps({"outcome": "infrastructure_failure", "artifact": str(path),
                          "error": str(error), "unsaved": value}), file=sys.stderr, flush=True)
        raise


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


class DiskPrerequisiteError(RuntimeError):
    def __init__(self, evidence):
        self.evidence = evidence
        super().__init__(f"host disk-capacity prerequisite failed before {evidence['phase']}")


def disk_snapshot(directory, phase, minimum=MIN_FREE_DISK_BYTES, building=False, extra_paths=()):
    """Observe each destination without creating it or probing with temporary files.

    Rust's Linux temp_dir uses TMPDIR or /tmp, not Python's writable-dir fallback.
    Cargo config-file overrides must be supplied via --disk-path; environment
    overrides and the crate's default target directory are checked automatically.
    """
    if minimum <= 0:
        raise ValueError("minimum free disk bytes must be positive")
    paths = [("artifacts", directory), ("scratch", Path(os.environ.get("TMPDIR", "/tmp")))]
    if building:
        paths.extend([
            ("cargo-target", Path(os.environ.get("CARGO_TARGET_DIR",
                                 os.environ.get("CARGO_BUILD_TARGET_DIR", str(CRATE / "target"))))),
            ("cargo-home", Path(os.environ.get("CARGO_HOME", str(Path.home() / ".cargo")))),
        ])
        if "CARGO_BUILD_BUILD_DIR" in os.environ:
            paths.append(("cargo-build", Path(os.environ["CARGO_BUILD_BUILD_DIR"])))
    paths.extend(("additional", path) for path in extra_paths)
    evidence = {"phase": phase, "minimum_free_bytes": minimum, "paths": []}
    for role, path in paths:
        item = {"role": role, "path": str(path)}
        try:
            path = Path(path)
            path = (ROOT / path).resolve() if not path.is_absolute() else path.resolve()
            item["path"] = str(path)
            probe = path
            while not probe.exists() and probe != probe.parent:
                probe = probe.parent
            item["observed_path"] = str(probe)
            if not probe.is_dir():
                raise NotADirectoryError(str(probe))
            usage = shutil.disk_usage(probe)
            item.update(total_bytes=usage.total, used_bytes=usage.used, free_bytes=usage.free,
                        sufficient=usage.free >= minimum)
        except OSError as error:
            item.update(sufficient=False, error=str(error))
        evidence["paths"].append(item)
    evidence["sufficient"] = all(item["sufficient"] for item in evidence["paths"])
    return evidence


def disk_check(directory, phase, minimum=MIN_FREE_DISK_BYTES, building=False,
               extra_paths=(), admission=True):
    evidence = disk_snapshot(directory, phase, minimum, building, extra_paths)
    evidence["admission"] = admission
    try:
        with (directory / "disk-capacity.jsonl").open("a") as stream:
            stream.write(json.dumps(evidence, sort_keys=True) + "\n")
    except OSError as error:
        evidence["evidence_write_error"] = str(error)
        # A completely full artifact filesystem may not retain even the result.
        print(json.dumps({"outcome": "infrastructure_failure", "disk_capacity": evidence}),
              file=sys.stderr, flush=True)
    if admission and (not evidence["sufficient"] or "evidence_write_error" in evidence):
        raise DiskPrerequisiteError(evidence)
    return evidence


def disk_options(args):
    # Keep programmatic Namespace callers compatible with the original API.
    return {"minimum": getattr(args, "min_free_disk_bytes", MIN_FREE_DISK_BYTES),
            "extra_paths": getattr(args, "disk_path", ())}


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
    matches = re.findall(r"test result: (ok|FAILED)\. (\d+) passed; (\d+) failed; (\d+) ignored;", output)
    if code == 0:
        expected = ("ok", str(count), "0", "0")
        return "pass" if matches and matches[-1] == expected and count > 0 else "unexercised"
    return "product_failure" if "test result: FAILED." in output else "infrastructure_failure"


def kernel_outcome(code, output, timed_out):
    # Only setup failures identify unavailable prerequisites. A later failed
    # assertion remains a product failure, even on a provisioned kernel.
    if "ring setup:" in output or "SKIP io_uring kernel tests:" in output:
        return "infrastructure_failure"
    return classify(code, output, timed_out, 1)


class ExecutionReportingError(RuntimeError):
    """Carry an executed probe's result when its diagnostics cannot be retained."""

    def __init__(self, record, error):
        self.record = record
        super().__init__(str(error))


def native_capabilities(binary, directory, entries, minimum=MIN_FREE_DISK_BYTES, extra_paths=()):
    before = disk_check(directory, "native-kernel", minimum, extra_paths=extra_paths)
    selector = "uring::tests::kernel_integration"
    observed = {
        "kernel": os.uname().release,
        "architecture": os.uname().machine,
        "memlock_bytes": resource.getrlimit(resource.RLIMIT_MEMLOCK),
        "allowed_cpus": sorted(os.sched_getaffinity(0)),
        "scratch_free_bytes": next(p["free_bytes"] for p in before["paths"] if p["role"] == "scratch"),
        "disk_capacity_before": before,
        "provider_coverage": "not_requested",
        "kernel_selector": selector,
        "kernel_outcome": "unexercised",
    }
    save(directory / "capabilities.json", observed)
    if not any(e["selector"] == selector and not e["ignored"] for e in entries):
        raise RuntimeError("required native kernel selector missing or ignored")
    env = dict(os.environ, RACER_REQUIRE_URING="1", RUST_TEST_THREADS="1")
    code, output, expired, elapsed = execute(
        [str(binary), selector, "--exact", "--nocapture", "--test-threads=1"], 30, env)
    outcome = kernel_outcome(code, output, expired)
    record = {"suite": "required-kernel", "tests": 1, "selectors": [selector], "attempted": True,
              "outcome": outcome, "seconds": elapsed, "exit_code": code, "timeout": expired,
              "disk_capacity_before": before}
    try:
        record["disk_capacity_after"] = disk_check(
            directory, "after-native-kernel", minimum, extra_paths=extra_paths, admission=False)
        observed.update(kernel_outcome=outcome, seconds=elapsed, exit_code=code, timeout=expired,
                        disk_capacity_after=record["disk_capacity_after"])
        (directory / "kernel-capability.log").write_text(output)
        save(directory / "capabilities.json", observed)
    except (OSError, ValueError, RuntimeError) as error:
        raise ExecutionReportingError(record, error) from error
    return record


def report(directory):
    if (directory / "campaign-result.json").exists():
        result = json.loads((directory / "campaign-result.json").read_text())
        definition = json.loads((directory / "campaign-manifest.json").read_text())
        summary = campaign_summary(definition["cells"], result["runs"])
        print(json.dumps(dict(summary, complete=result["complete"], error=result.get("error"),
                              outcome=result.get("outcome"), disk_capacity=result.get("disk_capacity")), indent=2))
        return int(not result["complete"] or summary["exercised"] != summary["planned"])
    result = json.loads((directory / "result.json").read_text())
    totals = collections.Counter(run["outcome"] for run in result["runs"])
    print(json.dumps({"outcomes": totals, "planned": result["planned"],
                      "attempted": sum(r.get("attempted", True) for r in result["runs"]),
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
    requests = {}
    armed = {}
    overlaps = set()
    # Defer parsing the full causal history until a lifecycle record is seen.
    # Existing v2 journals keep their streaming-only coverage memory behavior.
    has_lifecycle = False
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
                has_lifecycle |= kind == "LifecycleRoundPlanned"
                counts[kind] += 1
                if kind == "Invoke":
                    requests[fields["request"]] = fields
                elif kind in {"Response", "Cancel", "ProcessLost"}:
                    request = requests.pop(fields["request"], None)
                    if kind == "Response" and request and fields["status"] in {200, 206}:
                        counts["successful_head" if request["head"] else "successful_get"] += 1
                elif kind == "ActionFaultOverlap":
                    # Proposed Rust contract: emit at a hit action gate after
                    # observing >=2 accepted, live callers for its exact target,
                    # before cancellation/release. The source/destination are the
                    # armed gate's endpoints, not inferred from the scenario name.
                    # Counts of Invoke/ActionExecuted alone never establish this.
                    fault = fields.get("fault")
                    ids = fields.get("requests", [])
                    target = armed.get(fault)
                    if (fault in live_faults and fault not in overlaps and target is not None
                            and fields.get("boundary") == "peer-request"
                            and fields.get("phase") == "Request"
                            and type(fields.get("source")) is int
                            and type(fields.get("destination")) is int
                            and fields["source"] != fields["destination"]
                            and fields.get("target") == target
                            and isinstance(ids, list) and all(type(i) is int for i in ids)
                            and len(set(ids)) >= 2 and len(set(ids)) == len(ids)
                            and all(i in requests and requests[i]["target"] == target for i in ids)):
                        overlaps.add(fault)
                        counts["action_fault_overlap"] += 1
                if kind.startswith("Fault"):
                    fault = fields["fault"]
                    faults[kind].add(fault)
                    if kind == "FaultArmed":
                        armed[fault] = fields["target"]
                    elif kind == "FaultEffective":
                        live_faults.add(fault)
                    elif kind == "FaultReleased":
                        live_faults.discard(fault)
                    peak = max(peak, len(live_faults))
                elif kind == "Publish" and len(live_faults) >= 2:
                    counts["publication_with_two_effective_faults"] += 1
    certified = 0
    if has_lifecycle:
        def histories():
            with path.open() as stream:
                for line in stream:
                    try:
                        item = json.loads(json.loads(line)["payload"])
                    except (ValueError, KeyError, TypeError):
                        return
                    if item.get("kind") == "history":
                        yield item["value"]
        certified = lifecycle_coverage(histories())
    if certified:
        counts["lifecycle_round_certified"] = certified
    return {"terminal_record_present": terminal, "transitions": dict(counts),
            "faults": {kind: len(ids) for kind, ids in faults.items()},
            "peak_effective_faults": peak}


def lifecycle_coverage(history):
    """Certify ordered per-round typed evidence, not aggregate transition counts.

    LifecycleFaultCohort is Rust's assertion that all named callers were accepted
    and are still live. Independently cross-check their Invoke identity/method,
    ingress, active gates, subsequent publication/crash and terminal retirement.
    """
    invocations, live, responses, retired, armed, effective = {}, set(), {}, {}, {}, set()
    response_processes = {}
    state = None
    certified = set()
    seen = set()
    for value in history:
        kind, fields = next(iter(value["transition"].items()))
        if kind == "Invoke":
            request = fields["request"]
            invocations[request] = dict(fields, node=value.get("node"), incarnation=value.get("incarnation"))
            live.add(request)
        elif kind in {"Response", "Cancel", "ProcessLost"}:
            request = fields["request"]
            live.discard(request)
            retired[request] = kind
            if kind == "Response":
                responses[request] = fields["status"]
                response_processes[request] = (value.get("node"), value.get("incarnation"))
        elif kind == "FaultArmed":
            armed[fields["fault"]] = fields["target"]
        elif kind == "FaultEffective":
            effective.add(fields["fault"])
        elif kind == "FaultReleased":
            effective.discard(fields["fault"])
        if kind == "LifecycleRoundPlanned":
            round_id = fields["round"]
            state = {"plan": fields, "counts": collections.Counter(), "cohorts": {},
                     "published": {}, "stage": 0, "valid": round_id not in seen,
                     "durable": set(), "recovered": set(), "released": set(), "dirty": None,
                     "invoked": set(), "after_restart": set(), "healthy_invoked": set(),
                     "restarted_process": None}
            seen.add(round_id)
        if state is None:
            continue
        state["counts"][kind] += 1
        if kind.startswith("Lifecycle") and fields.get("round") != state["plan"]["round"]:
            state["valid"] = False
        if kind == "Invoke":
            state["invoked"].add(fields["request"])
            if state["stage"] == 4:
                state["after_restart"].add(fields["request"])
            elif len(state["cohorts"]) == 2 and state["stage"] <= 1:
                state["healthy_invoked"].add(fields["request"])
        elif kind == "Publish":
            state["published"][value.get("node")] = fields["revision"]
        elif kind == "DurabilityWitness":
            if state["stage"] <= 2 and state["dirty"] is None:
                state["durable"].add(fields["target"])
            else:
                state["valid"] = False
        elif kind == "DirtyCheckpointCrash":
            state["dirty"] = (fields["dirty"], fields["persisted"])
        elif kind == "FaultReleased":
            state["released"].add(fields["fault"])
        try:
            cohorts = state["cohorts"]
            if kind == "DurableRecovery":
                # Recovery is evidence only after a successful retained GET in
                # the reconstructed process, never a survivor/cache hit or a
                # response that arrives after this claimed recovery boundary.
                process = state["restarted_process"]
                ok = (state["stage"] == 4 and process is not None
                      and fields["target"] in state["durable"]
                      and any(invocations[i]["target"] == fields["target"]
                              and not invocations[i]["head"] and responses.get(i) == 200
                              and (invocations[i]["node"], invocations[i]["incarnation"]) == process
                              and response_processes.get(i) == process
                              for i in state["after_restart"]))
                state["valid"] &= ok
                if ok:
                    state["recovered"].add(fields["target"])
            elif kind == "LifecycleFaultCohort":
                source, fault, ids = fields["source"], fields["fault"], fields["requests"]
                ok = (state["stage"] == 0 and source in (0, 1) and source not in cohorts
                      and fields["destination"] == 1 - source and fault in effective
                      and armed.get(fault) == fields["target"]
                      and len(ids) == state["plan"]["callers"] and 2 <= len(ids) <= 3
                      and len(set(ids)) == len(ids)
                      and all(i in live and i in state["invoked"] and invocations[i]["node"] == source
                              and invocations[i]["target"] == fields["target"] for i in ids)
                      and {invocations[i]["head"] for i in ids} == {False, True})
                state["valid"] &= ok
                cohorts[source] = fields
            elif kind in {"LifecyclePublicationOverlap", "LifecycleHealthyProgress", "LifecycleCrashOverlap"}:
                faults = [cohorts[n]["fault"] for n in (0, 1)]
                ids = [i for n in (0, 1) for i in cohorts[n]["requests"]]
                ok = (len(set(faults)) == 2 and len(set(ids)) == len(ids)
                      and fields["faults"] == faults and set(faults) <= effective
                      and set(ids) <= live)
                if kind == "LifecyclePublicationOverlap":
                    ok &= (state["stage"] == 0 and fields["requests"] == ids
                           and fields["revisions"] == [state["published"][n] for n in (0, 1)])
                    state["stage"] = 1
                elif kind == "LifecycleHealthyProgress":
                    healthy = fields["requests"]
                    ok &= (state["stage"] == 1 and len(healthy) == len(set(healthy)) == 3
                           and not set(healthy) & set(ids) and set(healthy) <= state["healthy_invoked"]
                           and all(responses.get(i) == 200 and not invocations[i]["head"] for i in healthy)
                           and {invocations[i]["node"] for i in healthy} == {0, 1})
                    state["stage"] = 2
                else:
                    persisted = fields["persisted"]
                    ok &= (state["stage"] == 2 and fields["requests"] == ids
                           and fields["node"] == state["plan"]["crash_node"]
                           and fields["dirty"] >= 3 and 0 < len(persisted) < fields["dirty"]
                           and len(set(persisted)) == len(persisted)
                           and state["dirty"] == (fields["dirty"], persisted))
                    state["stage"] = 3
                state["valid"] &= ok
            elif kind == "LifecycleRestarted":
                node = state["plan"]["crash_node"]
                lost, canceled = cohorts[node]["requests"], cohorts[1 - node]["requests"]
                state["valid"] &= (state["stage"] == 3 and fields["node"] == node
                                   and fields["lost"] == lost
                                   and all(retired.get(i) == "ProcessLost" and
                                           fields["incarnation"] == invocations[i]["incarnation"] + 1 for i in lost)
                                   and all(retired.get(i) == "Cancel" for i in canceled))
                state["restarted_process"] = (fields["node"], fields["incarnation"])
                state["stage"] = 4
            elif kind == "LifecycleRoundRecovered":
                cold = fields["cold_requests"]
                faults = {cohorts[n]["fault"] for n in (0, 1)}
                ok = (state["stage"] == 4 and faults <= state["released"] and not faults & effective
                      and fields["target"] in state["durable"] & state["recovered"]
                      and len(cold) == len(set(cold)) == 2 and set(cold) <= state["after_restart"]
                      and all(responses.get(i) == 200 and not invocations[i]["head"] for i in cold)
                      and {invocations[i]["node"] for i in cold} == {0, 1}
                      and all(invocations[i]["target"] != fields["target"] for i in cold)
                      and all(state["counts"][name] >= minimum for name, minimum in LIFECYCLE_PER_ROUND.items()))
                if state["valid"] and ok:
                    certified.add(fields["round"])
                state["stage"] = 5
        except (KeyError, TypeError, ValueError):
            state["valid"] = False
    return len(certified)


def run(args, retained_binary=None, deadline=None):
    manifest = json.loads(MANIFEST.read_text())
    scale = json.loads((ROOT / "dst/scenarios/scale.json").read_text()) if args.profile == "scale" else None
    if deadline is not None:
        manifest["build_timeout_seconds"] = min(manifest["build_timeout_seconds"], max(0.01, deadline - time.monotonic()))
    if manifest["schema"] != 1 or manifest["memory_bytes"] > MEMORY_MAX:
        raise ValueError("unsupported manifest or memory budget")
    directory = args.artifacts.resolve()
    directory.mkdir(parents=True, exist_ok=False)
    result = {"schema": 1, "complete": False, "planned": 0, "runs": []}
    save(directory / "result.json", result)
    try:
        suites = [s for s in manifest["suites"]
                  if (s["id"] == args.scenario if args.scenario else s["tier"] == args.profile)]
        if scale:
            suites = [s for s in scale["suites"] if not args.scenario or s["id"] == args.scenario]
            save(directory / "scale-manifest.json", scale)
        if args.scenario in ARTIFACT_SCENARIOS:
            suites = [{"id": "artifact", "selector": ADAPTER, "tier": "pr", "timeout_seconds": 90}]
        if not suites:
            raise ValueError("no matching scenario")
        result["planned"] = len(suites) + int(any(s["tier"] == "native" for s in suites))
        save(directory / "manifest.json", manifest)
        save(directory / "result.json", result)
        options = disk_options(args)
        disk_check(directory, "build" if not retained_binary else "retain-binary",
                   building=not retained_binary, **options)
        try:
            binary, build_seconds = (retained_binary, 0) if retained_binary else build(directory, manifest)
        finally:
            disk_check(directory, "after-build" if not retained_binary else "after-retain-binary",
                       building=not retained_binary, admission=False, **options)
        disk_check(directory, "prepare-tests", **options)
        if args.scenario in ARTIFACT_SCENARIOS or scale:
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
        if any(suite["tier"] == "native" for suite in suites):
            probe = native_capabilities(binary, directory, entries, **options)
            result["runs"].append(probe)
            save(directory / "result.json", result)
            if probe["outcome"] != "pass":
                # The remaining required suites stay planned and unattempted.
                return report(directory)
        for suite in suites:
            before = disk_check(directory, f"suite:{suite['id']}", **options)
            names = selected_names(entries, suite)
            if not names:
                raise RuntimeError(f"zero test matches: {suite['id']}")
            env = dict(os.environ, RUST_TEST_THREADS="1", RACER_DST_SEED=str(args.seed))
            if scale:
                env["RACER_DST_NODES"] = str(scale["nodes"])
            if suite["tier"] == "native":
                env["RACER_REQUIRE_URING"] = "1"
            command = [str(binary), suite["selector"], "--test-threads=1"]
            if suite.get("exact"):
                command.extend(["--exact", "--nocapture"])
            if suite.get("ignored"):
                command.append("--ignored")
            if suite["id"] == "artifact":
                command.append("--exact")
                env.update(RACER_DST_INPUT=str(directory / "input.json"),
                           RACER_DST_SCENARIO=args.scenario,
                           RACER_DST_JOURNAL=str(directory / "journal.jsonl"),
                           RACER_DST_RESULT=str(directory / "semantic.json"), RACER_DST_MODE="record")
                if args.input:
                    shutil.copyfile(args.input, directory / "input.json")
            for entry in entries:
                if (entry["ignored"] and not suite.get("ignored") and suite["selector"] in entry["selector"]
                        or not suite["selector"] and entry["suite"] != suite["id"]):
                    command.extend(["--skip", entry["selector"]])
            seconds = suite["timeout_seconds"]
            if deadline is not None:
                seconds = min(seconds, max(0.01, deadline - time.monotonic()))
            code, output, timed_out, elapsed = execute(command, seconds, env)
            record = {"suite": suite["id"], "seed": args.seed, "tests": len(names),
                      "disk_capacity_before": before, "attempted": True,
                      "selectors": names, "exit_code": code, "timeout": timed_out,
                      "seconds": elapsed, "outcome": classify(code, output, timed_out, len(names))}
            result["runs"].append(record)
            record["disk_capacity_after"] = disk_check(
                directory, f"after-suite:{suite['id']}", admission=False, **options)
            if scale:
                record["nodes"] = scale["nodes"] if suite.get("ignored") else None
                record["execution_contract"] = "seeded-regression-without-exact-journal"
                record["children_peak_rss_bytes_cumulative"] = resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss * 1024
            if suite["id"] == "artifact" and (directory / "semantic.json").exists():
                semantic = json.loads((directory / "semantic.json").read_text())
                if (not isinstance(semantic, dict) or not isinstance(semantic.get("status"), str)
                        or semantic["status"] not in OUTCOMES):
                    raise ValueError("invalid artifact semantic status")
                if record["outcome"] == "product_failure":
                    record["outcome"] = semantic["status"]
            (directory / f"{suite['id']}.log").write_text(output)
            save(directory / "result.json", result)
            print(f"{suite['id']}: {record['outcome']} ({elapsed:.2f}s)", flush=True)
        result["complete"] = True
    except (OSError, ValueError, RuntimeError) as error:
        if isinstance(error, ExecutionReportingError):
            result["runs"].append(error.record)
        # Reporting failures are not additional suite attempts. Any completed
        # execution is already retained separately with its original outcome.
        record = {"suite": "runner", "tests": 0, "attempted": False,
                  "outcome": "infrastructure_failure", "error": str(error)}
        if isinstance(error, DiskPrerequisiteError):
            record["disk_capacity"] = error.evidence
        result["runs"].append(record)
    save(directory / "result.json", result)
    return report(directory)


def selected_names(entries, suite):
    return [e["selector"] for e in entries
            if (suite["selector"] == e["selector"] if suite.get("exact")
                else suite["selector"] in e["selector"])
            and e["ignored"] == bool(suite.get("ignored"))
            and (suite["selector"] or e["suite"] == suite["id"])]


def replay(directory, seconds=90, minimum=MIN_FREE_DISK_BYTES, extra_paths=()):
    directory = directory.resolve()
    attempted = False
    try:
        before = disk_check(directory, "replay", minimum, extra_paths=extra_paths)
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
        attempted = True
        code, output, timed_out, elapsed = execute(
            [str(binary), ADAPTER, "--exact", "--test-threads=1"], seconds, env)
        after = disk_check(directory, "after-replay", minimum, extra_paths=extra_paths, admission=False)
        (directory / "replay.log").write_text(output)
        outcome = classify(code, output, timed_out, 1)
        # A recorded failure must reproduce its terminal record too. The Rust adapter
        # validates it before writing semantic output, so arbitrary panics do not pass.
        semantic = directory / "replay-semantic.json"
        if outcome == "product_failure" and semantic.exists():
            if json.loads(semantic.read_text()) == json.loads((directory / "semantic.json").read_text()):
                outcome = "pass"
        save(directory / "replay-result.json", {"outcome": outcome, "seconds": elapsed,
                                               "disk_capacity_before": before, "disk_capacity_after": after,
                                               "exit_code": code, "timeout": timed_out})
        print(f"exact replay: {outcome}")
        return int(outcome != "pass")
    except (OSError, ValueError, RuntimeError) as error:
        failure = {"outcome": "infrastructure_failure", "error": str(error), "attempted": attempted}
        if isinstance(error, DiskPrerequisiteError):
            failure["disk_capacity"] = error.evidence
        try:
            save(directory / "replay-result.json", failure)
        except OSError:
            pass  # save already emitted the structured result to stderr.
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


def feasible(cell):
    """Static adapter/input availability, not a claim about executed coverage."""
    return (cell["scenario"] in ARTIFACT_SCENARIOS and
            (not cell.get("input") or (ROOT / "dst/scenarios" / cell["input"]).is_file()))


def campaign_summary(cells, runs):
    by_id = {run["cell"]: run for run in runs}
    totals = collections.Counter()
    gaps = []
    for cell in cells:
        run = by_id.get(cell["id"], {})
        observed = run.get("coverage", {}).get("transitions", {})
        totals.update(observed)
        if run.get("outcome") != "pass":
            gaps.append({"cell": cell["id"], "feasible": feasible(cell),
                         "attempted": run.get("attempted", bool(run)),
                         "outcome": run.get("outcome", "not_attempted"),
                         "exact_replay": run.get("exact_replay", False),
                         "missing_transitions": {kind: minimum - observed.get(kind, 0)
                                                 for kind, minimum in cell["minimum_transitions"].items()
                                                 if observed.get(kind, 0) < minimum}})
    return {"planned": len(cells), "feasible": sum(feasible(cell) for cell in cells),
            "attempted": sum(run.get("attempted", True) for run in by_id.values()),
            "exercised": sum(run["outcome"] == "pass" for run in runs),
            "outcomes": dict(collections.Counter(run["outcome"] for run in runs)),
            "transitions": dict(totals), "gaps": gaps}


def coverage_weights(cells, manifest, result):
    """Favor missing typed witnesses and templates without a successful gate."""
    identities = {cell["id"]: cell.get("template", cell["id"])
                  for cell in manifest["cells"]}
    observed = collections.defaultdict(collections.Counter)
    passed = set()
    for run in result["runs"]:
        template = identities.get(run["cell"])
        if template is None:
            raise ValueError("coverage result references a cell outside its manifest")
        for kind, count in run.get("coverage", {}).get("transitions", {}).items():
            observed[template][kind] = max(observed[template][kind], count)
        if run["outcome"] == "pass" and run.get("exact_replay"):
            passed.add(template)
    return {cell["id"]: 1 + int(cell["id"] not in passed) + sum(
                observed[cell["id"]][kind] < minimum
                for kind, minimum in cell.get("minimum_transitions", {}).items())
            for cell in cells}


def nightly_samples(cells, seed, count, weights=None):
    """Fair template selection, optionally weighted, with independently named seeds."""
    templates = sorted((cell for cell in cells if cell["expected"] == "pass"
                        and not cell.get("disable_mutant") and not cell.get("control")),
                       key=lambda cell: cell["id"])
    if not templates:
        raise ValueError("nightly campaign has no passing scenario templates")
    samples = []
    allocated = collections.Counter()
    for index in range(count):
        if weights is None:
            template = templates[index % len(templates)]
        else:
            # Integer cross-products give deterministic weighted fair allocation.
            template = templates[0]
            for candidate in templates[1:]:
                if ((allocated[candidate["id"]] + 1) * weights[template["id"]] <
                        (allocated[template["id"]] + 1) * weights[candidate["id"]]):
                    template = candidate
            allocated[template["id"]] += 1
        prefix = f"dst/nightly/v2/{seed}/{index}/{template['id']}"
        domains = {name: int.from_bytes(hashlib.sha256(f"{prefix}/{name}".encode()).digest()[:8], "little")
                   for name in ("scenario", "workload", "faults", "scheduler", "timing", "entropy")}
        sample = dict(template, id=f"sample-{index:04d}", template=template["id"],
                      seed=domains["scenario"])
        if template.get("input"):
            sample["resolved_seeds"] = domains
        samples.append(sample)
    return samples


def short_target_owner(target, nodes):
    """corpus::owner for one-block ASCII keys, using BLAKE3's compression.

    Deliberately bounded to 64 bytes: no dependency or general-purpose hash API.
    BLAKE3 is only needed to constrain peer gates to the actual canonical owner.
    """
    data = target.encode("ascii")
    if len(data) > 64 or not 2 <= nodes <= 8:
        raise ValueError("owner lookup requires a one-block key and 2..8 nodes")
    iv = [0x6A09E667, 0xBB67AE85, 0x3C6EF372, 0xA54FF53A,
          0x510E527F, 0x9B05688C, 0x1F83D9AB, 0x5BE0CD19]
    words = [int.from_bytes(data.ljust(64, b"\0")[i:i + 4], "little") for i in range(0, 64, 4)]
    # CHUNK_START | CHUNK_END | ROOT, chunk counter zero.
    state = iv + iv[:4] + [0, 0, len(data), 11]

    def rotate(value, bits):
        return ((value >> bits) | (value << (32 - bits))) & 0xffffffff

    def mix(a, b, c, d, x, y):
        state[a] = (state[a] + state[b] + x) & 0xffffffff
        state[d] = rotate(state[d] ^ state[a], 16)
        state[c] = (state[c] + state[d]) & 0xffffffff
        state[b] = rotate(state[b] ^ state[c], 12)
        state[a] = (state[a] + state[b] + y) & 0xffffffff
        state[d] = rotate(state[d] ^ state[a], 8)
        state[c] = (state[c] + state[d]) & 0xffffffff
        state[b] = rotate(state[b] ^ state[c], 7)

    for _ in range(7):
        for a, b, c, d, x, y in ((0, 4, 8, 12, 0, 1), (1, 5, 9, 13, 2, 3),
                               (2, 6, 10, 14, 4, 5), (3, 7, 11, 15, 6, 7),
                               (0, 5, 10, 15, 8, 9), (1, 6, 11, 12, 10, 11),
                               (2, 7, 8, 13, 12, 13), (3, 4, 9, 14, 14, 15)):
            mix(a, b, c, d, words[x], words[y])
        words = [words[i] for i in (2, 6, 3, 10, 7, 0, 4, 13, 1, 11, 12, 5, 9, 14, 15, 8)]
    low = state[0] ^ state[8]
    high = state[1] ^ state[9]
    return (low | (high << 32)) % nodes


def generated_samples(seed, count):
    """Bounded peer-request fault windows, independent of templates-v2.

    Every window has a cold exact target and a direct edge to its canonical
    owner. Two GETs share that held target; HEAD and extra callers overlap them.
    AwaitOverlap bounds actual accepted/live overlap before cancellation; a
    fixed Turn count or AwaitGate alone cannot prove this. Release performs
    mandatory settle and cold-owner heal probes. Certification additionally
    requires typed evidence of live callers at the effective fault boundary.
    """
    if (type(seed) is not int or not 0 <= seed < 2**64
            or type(count) is not int or not 1 <= count <= 256):
        raise ValueError("composition requires a u64 seed and 1..256 samples")
    samples = []
    for index in range(count):
        prefix = f"dst/{COMPOSITION_VERSION}/{seed}/{index}"

        def number(name):
            return int.from_bytes(hashlib.sha256(f"{prefix}/{name}".encode()).digest()[:8], "little")

        def pick(name, values):
            return values[number(name) % len(values)]

        seeds = {name: number(name) for name in
                 ("scenario", "workload", "faults", "scheduler", "timing", "entropy")}
        nodes = pick("config/nodes", (2, 3, 4))
        windows = pick("workload/windows", (1, 2, 3, 4))
        resolved = {"seeds": seeds, "nodes": nodes, "rdma": pick("config/rdma", (False, True)),
                    "socket_capacity": pick("config/socket_capacity", COMPOSITION_SOCKET_CAPACITIES),
                    "phase_policy": pick("config/phase_policy", ("Fixed", "Permuted")),
                    "callback_policy": pick("config/callback_policy", ("Fifo", "ReadyBatch")),
                    "actions": []}
        actions = resolved["actions"]
        dimensions = []
        for window in range(windows):
            name = f"workload/window/{window}"
            size = pick(f"{name}/size", (0, 1, 257, 4095, 4096,
                                         BUFFER_SIZE - 1, BUFFER_SIZE, BUFFER_SIZE + 1))
            key = pick(f"{name}/key", (0, 1, 2))
            target = f"/sized/{size}/generated/{key}?exact=%2f&version=7"
            # Release heals with a whole-object GET, so its target must stay
            # small even when the subsequent workload probes page boundaries.
            held_target = f"/sized/{min(size, 4096)}/generated/held/{window}/{key}?exact=%2f"
            owner = short_target_owner(held_target, nodes)
            degree = next(d for d in range(1, nodes + 1) if d**3 >= nodes)
            sources = tuple(n for n in range(nodes) if n != owner and any(
                (n * degree + digit) % nodes == owner for digit in range(degree)))
            node = pick(f"{name}/node", sources)
            # Large bodies only use short boundary ranges; never overflow the
            # single-page destination (or turn a full-page status into coverage).
            ranged = size > 4096 or pick(f"{name}/range", (False, True))
            span = [max(0, size - 9), size + 7] if ranged else None
            request = {"node": node, "target": target, "range": span}
            admissions = pick(f"{name}/admissions", (3, 4, 5, 6))
            turns = pick(f"{name}/turns", (1, 2, 3, 4))
            offset = pick(f"faults/window/{window}/wall", (-3000, -1, 1, 3000))
            actions.append({"Hold": [node, owner, held_target]})
            for caller in range(admissions):
                kind = "Get" if caller < 2 else "Head" if caller == 2 else pick(
                    f"{name}/method/{caller}", ("Get", "Head"))
                actions.append({kind: dict(request, target=held_target, range=None)})
            actions.extend(["AwaitGate", {"WallOffset": [node, offset]}, {"Turn": turns},
                            "AwaitOverlap", {"Cancel": node}, {"WallOffset": [node, 0]}, "Release"])
            # Healthy mixed-key/cross-node traffic after the fault window. The
            # cold-owner recovery probes also run inside Release itself.
            actions.extend([{"Get": dict(request, node=(node + 1) % nodes)},
                            {"Head": dict(request, range=None)}, "Drain"])
            dimensions.append({"node": node, "owner": owner, "object_bytes": size, "key": key,
                               "ranged": ranged, "admissions": admissions,
                               "turns": turns, "wall_offset": offset})
        # A short external range can require an entire upstream cache page.
        # Serialized transfer/effect/CQ/POLL delays through a 4/16 KiB queue can
        # exhaust the unchanged production deadline. Resolve a feasible success
        # profile after object selection, retaining 64+ chunks for page objects.
        sampled_capacity = resolved["socket_capacity"]
        minimum_capacity = 65536 if any(window["object_bytes"] >= BUFFER_SIZE - 1
                                        for window in dimensions) else 4096
        resolved["socket_capacity"] = max(sampled_capacity, minimum_capacity)
        # Configuration intent is not evidence that backpressure actually ran.
        capacity = resolved["socket_capacity"]
        pressure = {"evidence": "configuration-only", "socket_capacity_bytes": capacity,
                    "capacity_policy": COMPOSITION_CAPACITY_POLICY,
                    "sampled_socket_capacity_bytes": sampled_capacity,
                    "minimum_socket_capacity_bytes": minimum_capacity,
                    "page_to_queue_ratio": BUFFER_SIZE // capacity,
                    "object_queue_chunks": [(window["object_bytes"] + capacity - 1) // capacity
                                            for window in dimensions],
                    "windows_exceeding_queue": sum(window["object_bytes"] > capacity
                                                   for window in dimensions)}
        validate_generated_input(resolved)
        samples.append({"id": f"generated-{index:04d}", "scenario": "artifact",
                        "seed": seeds["scenario"], "expected": "pass",
                        "generator": COMPOSITION_VERSION, "generation_seed": seed,
                        "generation_index": index, "bounds": dict(COMPOSITION_BOUNDS),
                        "model_limits": dict(COMPOSITION_LIMITS),
                        "pressure": pressure,
                        "windows": dimensions, "resolved_input": resolved,
                        "minimum_transitions": {"action_fault_overlap": windows,
                                                "FaultArmed": windows, "FaultEffective": windows,
                                                "FaultReleased": windows, "Cancel": windows,
                                                "successful_get": windows, "successful_head": windows}})
    return samples


def validate_generated_input(source):
    """Conservative subset of corpus::valid plus generator resource limits.

    This is static input validation, not evidence that production paths executed.
    The Rust adapter remains authoritative and validates every recorded input.
    """
    nodes, actions = source["nodes"], source["actions"]
    if not 2 <= nodes <= COMPOSITION_BOUNDS["nodes"] or len(actions) > COMPOSITION_BOUNDS["actions"]:
        raise ValueError("generated resource bounds")
    if source["socket_capacity"] not in COMPOSITION_SOCKET_CAPACITIES:
        raise ValueError("generated socket capacity bounds")
    pending = [0] * nodes
    gated = False
    cohort = []
    gate_source = gate_target = None
    overlap_ready = False
    seen_gates = set()
    for action in actions:
        if isinstance(action, str):
            kind, value = action, None
        elif isinstance(action, dict) and len(action) == 1:
            kind, value = next(iter(action.items()))
        else:
            raise ValueError("invalid generated action")
        if kind in {"Get", "Head"}:
            used = [value["node"]]
        elif kind in {"Hold", "WallOffset"}:
            used = value[:2] if kind == "Hold" else value[:1]
        elif kind == "Cancel":
            used = [value]
        else:
            used = []
        if any(type(n) is not int or not 0 <= n < nodes for n in used):
            raise ValueError("invalid generated node")
        if kind in {"Get", "Head"}:
            match = re.fullmatch(r"/sized/(\d+)/.+", value["target"])
            if not match or int(match[1]) > COMPOSITION_BOUNDS["object_bytes"]:
                raise ValueError("generated object bounds")
            size, span = int(match[1]), value.get("range")
            if size >= BUFFER_SIZE - 1 and source["socket_capacity"] < 65536:
                raise ValueError("generated page fill requires a 65536-byte queue for the success profile")
            if span is not None:
                if (len(span) != 2 or not 0 <= span[0] <= span[1]
                        or span[1] - span[0] + 1 > BUFFER_SIZE):
                    raise ValueError("generated response bounds")
            elif kind == "Get" and size > BUFFER_SIZE:
                raise ValueError("generated response bounds")
            pending[value["node"]] += 1
            if gated:
                if value["node"] != gate_source or value["target"] != gate_target:
                    raise ValueError("generated held cohort must share ingress and exact target")
                cohort.append(kind)
                overlap_ready = False
        elif kind == "Hold":
            degree = next(d for d in range(1, nodes + 1) if d**3 >= nodes)
            if (gated or any(pending) or value[0] == value[1] or short_target_owner(value[2], nodes) != value[1]
                    or not any((value[0] * degree + digit) % nodes == value[1] for digit in range(degree))):
                raise ValueError("generated gate requires a direct owner edge")
            if value[2] in seen_gates:
                raise ValueError("generated gate target must be cold")
            seen_gates.add(value[2])
            if len(seen_gates) > COMPOSITION_BOUNDS["windows"]:
                raise ValueError("generated fault-window bounds")
            gated = True
            gate_source, _, gate_target = value
            cohort = []
            overlap_ready = False
        elif kind == "AwaitGate":
            if not gated:
                raise ValueError("generated gate required")
        elif kind == "AwaitOverlap":
            if (not gated or cohort.count("Get") < 2 or "Head" not in cohort
                    or pending[gate_source] != len(cohort)):
                raise ValueError("generated overlap barrier requires an intact mixed caller cohort")
            overlap_ready = True
        elif kind == "Release":
            if not gated:
                raise ValueError("generated gate required")
            gated = False
            pending = [0] * nodes
            overlap_ready = False
        elif kind == "Cancel":
            if not pending[value] or value != gate_source or not overlap_ready:
                raise ValueError("generated cancel requires the accepted-overlap barrier")
            pending[value] -= 1
            overlap_ready = False
        elif kind == "Drain":
            if gated:
                raise ValueError("generated drain cannot wait on a held gate")
            pending = [0] * nodes
        elif kind == "Turn":
            if not 0 <= value <= COMPOSITION_BOUNDS["turns_per_window"]:
                raise ValueError("generated turn bounds")
            overlap_ready = False  # The barrier must follow all speculative turns.
        elif kind == "WallOffset":
            if abs(value[1]) > 3000:
                raise ValueError("generated wall offset bounds")
        else:
            raise ValueError(f"unsupported generated action: {kind}")
        if sum(pending) > COMPOSITION_BOUNDS["live_requests"]:
            raise ValueError("generated admission bounds")
    if gated or any(pending):
        raise ValueError("generated workload must release and drain")


def lifecycle_samples(seed, count):
    """Alternate preserved v2 action samples with independent bounded lifecycles.

    Even slots use v2 sample index slot/2 without changing any resolved input.
    Odd slots derive six world streams and a separate actor stream from v3.
    Only actual ArtifactInput fields are sent to Rust; bounds/contracts stay in
    the campaign manifest and its per-cell resolved input copy.
    """
    actions = generated_samples(seed, (count + 1) // 2) if type(count) is int and 1 <= count <= 256 else None
    if actions is None:
        raise ValueError("composition-v3 requires 1..256 samples")
    samples = []
    for index in range(count):
        if index % 2 == 0:
            samples.append(dict(actions[index // 2], id=f"generated-v3-{index:04d}",
                                sampling_policy=LIFECYCLE_VERSION, sampling_index=index))
            continue
        prefix = f"dst/{LIFECYCLE_VERSION}/{seed}/{index}"

        def number(name):
            return int.from_bytes(hashlib.sha256(f"{prefix}/{name}".encode()).digest()[:8], "little")

        seeds = {name: number(name) for name in
                 ("scenario", "workload", "faults", "scheduler", "timing", "entropy")}
        rounds = 1 + number("config/rounds") % 3
        resolved = {"seeds": seeds, "nodes": 2, "rdma": False, "actions": [],
                    "generated_lifecycle": {"seed": number("actor/lifecycle"), "rounds": rounds},
                    "socket_capacity": (4096, 16384, 65536)[number("config/socket_capacity") % 3],
                    "phase_policy": ("Fixed", "Permuted")[number("config/phase_policy") % 2],
                    "callback_policy": ("Fifo", "ReadyBatch")[number("config/callback_policy") % 2]}
        samples.append({"id": f"generated-v3-{index:04d}", "scenario": "artifact",
                        "seed": seeds["scenario"], "expected": "pass", "generator": LIFECYCLE_VERSION,
                        "generation_seed": seed, "generation_index": index,
                        "sampling_policy": LIFECYCLE_VERSION, "sampling_index": index,
                        "bounds": {"nodes": 2, "rounds": 3, "held_callers_per_node": 3,
                                   "object_bytes": 4096, "crash_window_ticks": 2500},
                        "resolved_input": resolved,
                        "minimum_transitions": dict({name: minimum * rounds
                                                     for name, minimum in LIFECYCLE_PER_ROUND.items()},
                                                    lifecycle_round_certified=rounds)})
    return samples


def sampled_input(cell, source):
    """Preserve fixture constraints while replacing every random domain explicitly."""
    resolved = dict(source)
    if cell.get("disable_mutant"):
        resolved["mutant"] = None
    if "resolved_seeds" in cell:
        resolved["seeds"] = dict(cell["resolved_seeds"])
    return resolved


def run_campaign(args):
    directory = args.artifacts.resolve()
    directory.mkdir(parents=True, exist_ok=False)
    definition = json.loads((ROOT / "dst/scenarios/campaign.json").read_text())
    if definition["schema"] != 1:
        raise ValueError("unsupported campaign schema")
    cells = definition["cells"]
    if args.tier == "nightly":
        weights = None
        sampler = getattr(args, "sampler", "templates-v2")
        if sampler in {COMPOSITION_VERSION, LIFECYCLE_VERSION} and args.coverage_from:
            raise ValueError(f"{sampler} does not use template coverage weights")
        if args.coverage_from:
            prior_manifest = (args.coverage_from / "campaign-manifest.json").read_bytes()
            prior_result = (args.coverage_from / "campaign-result.json").read_bytes()
            weights = coverage_weights(cells, json.loads(prior_manifest), json.loads(prior_result))
            save(directory / "sampling-source-manifest.json", json.loads(prior_manifest))
            save(directory / "sampling-source-result.json", json.loads(prior_result))
            definition["sampling"] = {"policy": "witness-weighted-fair-v1", "weights": weights,
                                      "manifest_sha256": hashlib.sha256(prior_manifest).hexdigest(),
                                      "result_sha256": hashlib.sha256(prior_result).hexdigest()}
        if sampler == LIFECYCLE_VERSION:
            definition["sampling"] = {"policy": LIFECYCLE_VERSION, "seed": args.seed,
                                      "allocation": "even: composition-v2; odd: generated_lifecycle",
                                      "maximum_lifecycle_rounds": 3}
            cells.extend(lifecycle_samples(args.seed, args.samples))
        elif sampler == COMPOSITION_VERSION:
            definition["sampling"] = {"policy": COMPOSITION_VERSION, "seed": args.seed,
                                      "bounds": COMPOSITION_BOUNDS, "model_limits": COMPOSITION_LIMITS}
            cells.extend(generated_samples(args.seed, args.samples))
        else:
            cells.extend(nightly_samples(cells, args.seed, args.samples, weights))
    save(directory / "campaign-manifest.json", definition)
    result = {"complete": False, "tier": args.tier, "planned": len(cells), "runs": []}
    result["coverage"] = campaign_summary(cells, [])
    save(directory / "campaign-result.json", result)
    deadline = time.monotonic() + args.timeout
    retained = None
    controls = {}
    options = disk_options(args)
    try:
        for cell in cells:
            if time.monotonic() >= deadline:
                raise RuntimeError("campaign host deadline exhausted")
            disk_check(directory, f"cell:{cell['id']}", **options)
            source = None
            if "resolved_input" in cell:
                source = directory / f"{cell['id']}-input.json"
                save(source, cell["resolved_input"])
            elif cell.get("input"):
                source = ROOT / "dst/scenarios" / cell["input"]
                if cell.get("disable_mutant") or "resolved_seeds" in cell:
                    control = sampled_input(cell, json.loads(source.read_text()))
                    source = directory / f"{cell['id']}-input.json"
                    save(source, control)
            bundle = directory / cell["id"]
            run(argparse.Namespace(artifacts=bundle, scenario=cell["scenario"],
                                   profile="pr", seed=cell["seed"], input=source,
                                   min_free_disk_bytes=options["minimum"],
                                   disk_path=options["extra_paths"]), retained, deadline)
            execution = json.loads((bundle / "result.json").read_text())
            outcome = execution["runs"][-1]["outcome"]
            executed = [r for r in execution["runs"] if r.get("attempted", True)]
            cell_record = {"cell": cell["id"], "outcome": "infrastructure_failure",
                           "attempted": bool(executed),
                           "record_outcome": executed[-1]["outcome"] if executed else outcome,
                           "exact_replay": False, "coverage": {}}
            result["runs"].append(cell_record)
            witnesses = coverage(bundle)
            cell_record["coverage"] = witnesses
            if retained is None and (bundle / "libtest").exists():
                retained = bundle / "libtest"
            semantic_path = bundle / "semantic.json"
            semantic = json.loads(semantic_path.read_text()) if semantic_path.exists() else {}
            replayed = False
            replay_failure = None
            replay_diagnostics_error = None
            if semantic and outcome in {"pass", "product_failure"} and time.monotonic() < deadline:
                replayed = replay(bundle, min(90, deadline - time.monotonic()), **options) == 0
                try:
                    replay_result = json.loads((bundle / "replay-result.json").read_text())
                    if (not isinstance(replay_result, dict) or not isinstance(replay_result.get("outcome"), str)
                            or replay_result["outcome"] not in OUTCOMES):
                        raise ValueError("invalid replay result outcome")
                except (OSError, ValueError) as error:
                    replay_diagnostics_error = f"replay diagnostics unavailable: {error}"
                    replay_result = {"outcome": "infrastructure_failure", "error": replay_diagnostics_error}
                    replayed = False
                if replay_result["outcome"] == "infrastructure_failure":
                    replay_failure = replay_result
            verdict = gate(cell, outcome, semantic, witnesses, replayed, controls)
            if replay_failure:
                verdict = "infrastructure_failure"
            controls[cell["id"]] = verdict
            cell_record.update(outcome=verdict, exact_replay=replayed)
            disk_failure = next((r["disk_capacity"] for r in execution["runs"]
                                 if "disk_capacity" in r), None)
            if replay_failure:
                result["runs"][-1]["replay_failure"] = replay_failure
                disk_failure = replay_failure.get("disk_capacity", disk_failure)
            result["coverage"] = campaign_summary(cells, result["runs"])
            save(directory / "campaign-result.json", result)
            if disk_failure:
                raise DiskPrerequisiteError(disk_failure)
            if replay_diagnostics_error:
                raise RuntimeError(replay_diagnostics_error)
        result["complete"] = True
    except (OSError, ValueError, RuntimeError) as error:
        result["error"] = str(error)
        result["outcome"] = "infrastructure_failure"
        if isinstance(error, DiskPrerequisiteError):
            result["disk_capacity"] = error.evidence
    result["gated"] = sum(item["outcome"] == "pass" for item in result["runs"])
    result["coverage"] = campaign_summary(cells, result["runs"])
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


def reductions(current):
    """Strict simplifications only; the adapter validates each new scenario."""
    prefix = current.get("schedule_prefix", [])
    for length in sorted({0, len(prefix) // 2, len(prefix) - 1}):
        if 0 <= length < len(prefix):
            yield "schedule_prefix", dict(current, schedule_prefix=prefix[:length])
    for actions in deletions(current["actions"]):
        yield "actions", dict(current, actions=actions)
    lifecycle = current.get("generated_lifecycle")
    if lifecycle is not None:
        yield "generated_lifecycle", dict(current, generated_lifecycle=None)
        for rounds in range(1, lifecycle["rounds"]):
            yield "generated_lifecycle.rounds", dict(current, generated_lifecycle=dict(lifecycle, rounds=rounds))
    for field in ("overlap", "namespace_overlap", "checkpoint_overlap",
                  "flight_cancellation", "local_attribution", "confirmation_admission",
                  "zc_retirement", "rdma_recovery", "confirmation_reload", "shared_workers",
                  "wall_authentication", "stream_policies", "checkpoint_versions", "shared_workers_crash"):
        if current.get(field):
            disabled = {field: False}
            dependent = {"checkpoint_overlap": "checkpoint_versions", "shared_workers": "shared_workers_crash"}.get(field)
            if dependent and current.get(dependent):
                disabled[dependent] = False
            yield field, dict(current, **disabled)
    nodes = current["nodes"]
    for count in sorted({2, max(2, nodes // 2), nodes - 1}):
        if 2 <= count < nodes:
            yield "nodes", dict(current, nodes=count)
    if current.get("rdma"):
        yield "rdma", dict(current, rdma=False)
    if current.get("phase_policy") == "Permuted":
        yield "phase_policy", dict(current, phase_policy="Fixed")
    if current.get("callback_policy") == "ReadyBatch":
        yield "callback_policy", dict(current, callback_policy="Fifo")
    capacity = current.get("socket_capacity")
    if capacity is not None:
        for value in sorted({1, 4096, capacity // 2}):
            if 0 < value < capacity:
                yield "socket_capacity", dict(current, socket_capacity=value)
    delay = current.get("peer_failure_delay", 0)
    for value in sorted({0, delay // 2}):
        if value < delay:
            yield "peer_failure_delay", dict(current, peer_failure_delay=value)
    # /sized/N is an actual corpus length input, not a new ignored JSON flag.
    # Rewrite every use together, retaining key skew, gate binding, and ranges.
    # Ranges intentionally remain unchanged: an out-of-bounds range is a valid
    # oracle case, and only the same failure + witnesses + replay can accept it.
    targets = set()
    for action in current["actions"]:
        if isinstance(action, dict):
            for kind, value in action.items():
                if kind in {"Get", "Head"}:
                    targets.add(value["target"])
    for target in sorted(targets):
        match = re.fullmatch(r"/sized/(\d+)(/.*)", target)
        if not match:
            continue
        size = int(match[1])
        for value in sorted({0, 1, 257, 4096, BUFFER_SIZE - 1, size // 2}):
            if value >= size:
                continue
            replacement = f"/sized/{value}{match[2]}"
            if replacement in targets:
                continue  # Never merge distinct workload objects during shrinking.
            # A changed key can change the canonical owner. Do not propose an
            # unreachable gate for short keys in the bounded canonical topology.
            gates = [payload for action in current["actions"] if isinstance(action, dict)
                     for kind, payload in action.items()
                     if kind in {"Hold", "Refuse"} and payload[-1] == target]
            if gates and len(replacement.encode()) <= 64 and 2 <= current["nodes"] <= 8:
                if any(short_target_owner(replacement, current["nodes"]) != gate[1] for gate in gates):
                    continue
            actions = []
            for action in current["actions"]:
                if isinstance(action, dict) and len(action) == 1:
                    kind, payload = next(iter(action.items()))
                    if kind in {"Get", "Head"} and payload["target"] == target:
                        action = {kind: dict(payload, target=replacement)}
                    elif kind in {"Hold", "Refuse", "Durable"} and payload[-1] == target:
                        action = {kind: [*payload[:-1], replacement]}
                actions.append(action)
            proposal = dict(current, actions=actions)
            try:
                size_target_mapping(current, proposal)
            except ValueError:
                continue
            yield "object_bytes", proposal
    for index, action in enumerate(current["actions"]):
        if not isinstance(action, dict) or len(action) != 1:
            continue
        kind, value = next(iter(action.items()))
        variants = []
        if kind == "Turn" and value > 0:
            variants = sorted({0, value // 2})
        elif kind == "WallOffset" and value[1] != 0:
            magnitude = abs(value[1]) // 2
            variants = [[value[0], offset] for offset in
                        sorted({0, magnitude if value[1] > 0 else -magnitude})]
        elif kind == "CrashSectors":
            variants = [[value[0], sectors] for sectors in deletions(value[1])]
        for variant in variants:
            actions = list(current["actions"])
            actions[index] = {kind: variant}
            yield f"actions.{index}.{kind}", dict(current, actions=actions)


def size_target_mapping(current, proposal):
    """Authorize exactly one consistent sized-key decrease, with no other edits.

    Keep the logical suffix, request methods/ranges, gate endpoints, seeds and
    configuration exact. Disallow merging with an existing object identity.
    This authorization is derived from inputs, never from candidate witnesses.
    """
    def targets(source):
        for action in source["actions"]:
            if isinstance(action, dict) and len(action) == 1:
                kind, value = next(iter(action.items()))
                if kind in {"Get", "Head"}:
                    yield value["target"]
                elif kind in {"Hold", "Refuse", "Durable"}:
                    yield value[-1]

    before, after = list(targets(current)), list(targets(proposal))
    changes = {(old, new) for old, new in zip(before, after) if old != new}
    if len(before) != len(after) or len(changes) != 1:
        raise ValueError("object reduction requires one consistent target mapping")
    old, new = changes.pop()
    old_size = re.fullmatch(r"/sized/(0|[1-9]\d*)(/.*)", old)
    new_size = re.fullmatch(r"/sized/(0|[1-9]\d*)(/.*)", new)
    if (not old_size or not new_size or old_size[2] != new_size[2]
            or int(new_size[1]) >= int(old_size[1]) or new in before):
        raise ValueError("object reduction must only decrease size without merging keys")
    expected = json.loads(json.dumps(current))
    for action in expected["actions"]:
        if isinstance(action, dict) and len(action) == 1:
            kind, value = next(iter(action.items()))
            if kind in {"Get", "Head"} and value["target"] == old:
                value["target"] = new
            elif kind in {"Hold", "Refuse", "Durable"} and value[-1] == old:
                value[-1] = new
    if json.dumps(expected, sort_keys=True) != json.dumps(proposal, sort_keys=True):
        raise ValueError("object reduction changed fields other than the sized target")
    return {old: new}


def mapped_witnesses(required, target_mapping):
    """Apply an input-checked size mapping only to typed target fields.

    Cause strings, ranges, status, cohort order, and all other fields stay exact.
    Callers must obtain the mapping from size_target_mapping before using it.
    """
    mapped = []
    for signature in required:
        item = json.loads(signature)
        fields = item["fields"]
        for container in (fields, fields.get("scope", {}), *fields.get("scopes", [])):
            if container.get("target") in target_mapping:
                container["target"] = target_mapping[container["target"]]
        mapped.append(json.dumps(item, sort_keys=True))
    return mapped


def witness_signature(records):
    """Ordered causal contract with request identities in involved-Invoke order.

    Retain every Invoke referenced by a witness, including its target, method,
    and process scope. Only unreferenced admissions may disappear. One request
    identity is shared by its cohort membership and every later observation;
    neither cohort reordering nor canceling a different caller is equivalent.
    """
    # The journal also contains potentially large event/choice streams. Keep
    # only the semantic history needed for the two-pass identity assignment.
    records = [record for record in records if record.get("kind") in {"history", "terminal"}]
    involved = set()
    for record in records:
        if record.get("kind") != "history":
            continue
        kind, fields = next(iter(record["value"]["transition"].items()))
        if kind != "Invoke" and "request" in fields:
            involved.add(fields["request"])
        for field in request_list_fields(kind):
            ids = fields[field]
            if len(ids) != len(set(ids)):
                raise ValueError("overlap cohort contains duplicate request identities")
            involved.update(ids)
    armed = {}
    requests = {}
    required = []
    terminal = False
    for record in records:
        if record.get("kind") == "terminal":
            terminal = True
        if record.get("kind") != "history":
            continue
        value = record["value"]
        kind, fields = next(iter(value["transition"].items()))
        fields = dict(fields)
        if kind == "ActionExecuted":
            continue
        if kind == "Invoke":
            request = fields["request"]
            if request not in involved:
                continue
            if request in requests:
                raise ValueError("duplicate Invoke request identity")
            requests[request] = len(requests)
        if "request" in fields:
            if fields["request"] not in requests:
                raise ValueError("request witness has no preceding Invoke")
            fields["request"] = requests[fields["request"]]
        for field in request_list_fields(kind):
            if any(request not in requests for request in fields[field]):
                raise ValueError("overlap witness has no preceding Invoke")
            fields[field] = [requests[request] for request in fields[field]]
        if kind == "FaultArmed":
            armed[fields["fault"]] = {"node": value.get("node"),
                                      "target": fields.get("target")}
        if "fault" in fields:
            fault = fields.pop("fault")
            if fault not in armed:
                raise ValueError("fault witness has no preceding arm record")
            fields["scope"] = armed[fault]
        if kind in {"LifecyclePublicationOverlap", "LifecycleHealthyProgress", "LifecycleCrashOverlap"}:
            if any(fault not in armed for fault in fields["faults"]):
                raise ValueError("fault witness has no preceding arm record")
            fields["scopes"] = [armed[fault] for fault in fields.pop("faults")]
        required.append(json.dumps({"kind": kind, "fields": fields,
                                 "node": value.get("node"),
                                 "worker": value.get("worker", 0),
                                 "incarnation": value.get("incarnation")}, sort_keys=True))
    if not terminal:
        raise ValueError("reduction requires a complete terminal witness history")
    return required


def request_list_fields(kind):
    if kind in {"ActionFaultOverlap", "LifecycleFaultCohort", "LifecyclePublicationOverlap",
                "LifecycleHealthyProgress", "LifecycleCrashOverlap"}:
        return ("requests",)
    return {"LifecycleRestarted": ("lost",), "LifecycleRoundRecovered": ("cold_requests",)}.get(kind, ())


def preserves_witnesses(required, observed):
    """Require the original observations as a subsequence, including repetitions."""
    remaining = iter(observed)
    return all(any(actual == wanted for actual in remaining) for wanted in required)


def reduction_witnesses(directory):
    # Source and accepted candidate integrity is established by exact replay.
    # This pass extracts semantic requirements, not a second checksum validator.
    with (directory / "journal.jsonl").open() as stream:
        return witness_signature(json.loads(json.loads(line)["payload"]) for line in stream)


def exploration_input(source, records, count, seed):
    """A checked prefix is new scenario input, never an exact transcript claim."""
    choices = []
    terminal = False
    for record in records:
        if record.get("kind") == "choice" and len(choices) < count:
            choices.append(record["value"])
        terminal |= record.get("kind") == "terminal"
    if not terminal or len(choices) != count:
        raise ValueError("exploration requires a complete source and enough choices")
    return dict(source, schedule_prefix=choices,
                seeds=dict(source["seeds"], scheduler=seed))


def explore(args):
    source = args.directory.resolve()
    deadline = time.monotonic() + args.timeout
    options = disk_options(args)
    if replay(source, min(90, args.timeout), **options):
        raise ValueError("exploration source does not exactly replay")
    with (source / "journal.jsonl").open() as stream:
        resolved = exploration_input(json.loads((source / "input.json").read_text()),
            (json.loads(json.loads(line)["payload"]) for line in stream),
            args.choices, args.seed)
    # Preserve the exact executable whose prefix was just validated. The source
    # adapter must support schedule_prefix; incompatible older adapters reject it.
    directory = args.artifacts.resolve()
    directory.mkdir(parents=True, exist_ok=False)
    try:
        disk_check(directory, "exploration", **options)
    except DiskPrerequisiteError as error:
        save(directory / "exploration.json", {"source": str(source), "outcome": "infrastructure_failure",
             "exact_replay": False, "error": str(error), "disk_capacity": error.evidence})
        return 1
    save(directory / "exploration-input.json", resolved)
    bundle = directory / "record"
    run(argparse.Namespace(artifacts=bundle, scenario="artifact", profile="pr",
                          seed=args.seed, input=directory / "exploration-input.json",
                          min_free_disk_bytes=options["minimum"], disk_path=options["extra_paths"]),
        source / "libtest", deadline)
    result = json.loads((bundle / "result.json").read_text())
    outcome = result["runs"][-1]["outcome"]
    if (bundle / "libtest").exists():
        # The executable was retained, not rebuilt from the runner's current checkout.
        shutil.copyfile(source / "build.json", bundle / "build.json")
        if (source / "source.patch").exists():
            shutil.copyfile(source / "source.patch", bundle / "source.patch")
    replayed = (outcome in {"pass", "product_failure"} and time.monotonic() < deadline
                and replay(bundle, min(90, deadline - time.monotonic()), **options) == 0)
    summary = {"source": str(source), "choices": args.choices,
               "scheduler_seed": args.seed, "outcome": outcome, "exact_replay": replayed}
    replay_result_path = bundle / "replay-result.json"
    if not replayed and replay_result_path.exists():
        replay_result = json.loads(replay_result_path.read_text())
        if replay_result["outcome"] == "infrastructure_failure":
            summary.update(outcome="infrastructure_failure", record_outcome=outcome,
                           replay_failure=replay_result)
    save(directory / "exploration.json", summary)
    return int(not replayed)


def reduce_artifact(args):
    source, destination = args.directory.resolve(), args.artifacts.resolve()
    destination.mkdir(parents=True, exist_ok=False)
    start = time.monotonic()
    deadline = start + args.timeout
    attempts = []
    summary = {"complete": False, "attempts": attempts, "accepted": None}
    save(destination / "reduction.json", summary)
    options = disk_options(args)
    try:
        disk_check(destination, "reduction", **options)
        identity = failure_identity(json.loads((source / "semantic.json").read_text()))
        if not identity:
            raise ValueError("reduction requires a named product failure")
        if replay(source, min(90, max(0.01, deadline - time.monotonic())), **options):
            raise ValueError("source does not exactly replay")
        witnesses = reduction_witnesses(source)
        metadata = json.loads((source / "build.json").read_text())
        binary = source / "libtest"
        if not binary.exists():
            binary = Path(metadata["binary"])
        current = json.loads((source / "input.json").read_text())
        summary.update(oracle=identity, original_actions=len(current["actions"]),
                       remaining_actions=len(current["actions"]),
                       original_input=current, remaining_input=current,
                       witness_policy="causal-subsequence-sized-target-v2",
                       required_witnesses=[json.loads(item) for item in witnesses])
        visited = {json.dumps(current, sort_keys=True)}
        changed = True
        while changed and len(attempts) < args.max_candidates and time.monotonic() < deadline:
            changed = False
            for dimension, proposal in reductions(current):
                if len(attempts) >= args.max_candidates or time.monotonic() >= deadline:
                    break
                signature = json.dumps(proposal, sort_keys=True)
                if signature in visited:
                    continue
                visited.add(signature)
                target_mapping = size_target_mapping(current, proposal) if dimension == "object_bytes" else {}
                required = mapped_witnesses(witnesses, target_mapping) if target_mapping else witnesses
                disk_check(destination, "reduction-candidate", **options)
                candidate = destination / f"candidate-{len(attempts):04d}"
                candidate.mkdir()
                # Hard links retain a standalone, hash-checked executable without
                # duplicating hundreds of MB for every rejected proposal.
                os.link(binary, candidate / "libtest")
                save(candidate / "build.json", metadata)
                if (source / "source.patch").exists():
                    shutil.copyfile(source / "source.patch", candidate / "source.patch")
                save(candidate / "input.json", proposal)
                env = dict(os.environ, RUST_TEST_THREADS="1", RACER_DST_MODE="record",
                           RACER_DST_INPUT=str(candidate / "input.json"),
                           RACER_DST_JOURNAL=str(candidate / "journal.jsonl"),
                           RACER_DST_RESULT=str(candidate / "semantic.json"))
                code, output, expired, elapsed = execute(
                    [str(candidate / "libtest"), ADAPTER, "--exact", "--test-threads=1"],
                    min(90, max(0.01, deadline - time.monotonic())), env)
                after = disk_check(candidate, "after-reduction-candidate", admission=False, **options)
                (candidate / "artifact.log").write_text(output)
                outcome = classify(code, output, expired, 1)
                semantic = candidate / "semantic.json"
                same = (outcome == "product_failure" and semantic.exists()
                        and failure_identity(json.loads(semantic.read_text())) == identity)
                preserved = same and preserves_witnesses(required, reduction_witnesses(candidate))
                accepted = (preserved and time.monotonic() < deadline and replay(
                    candidate, min(90, max(0.01, deadline - time.monotonic())), **options) == 0)
                attempts.append({"candidate": candidate.name, "actions": len(proposal["actions"]),
                                 "dimension": dimension, "target_mapping": target_mapping,
                                 "disk_capacity_after": after,
                                 "outcome": outcome, "witnesses_preserved": preserved,
                                 "accepted": accepted, "seconds": elapsed})
                replay_result_path = candidate / "replay-result.json"
                if replay_result_path.exists():
                    replay_result = json.loads(replay_result_path.read_text())
                    if "disk_capacity" in replay_result:
                        raise DiskPrerequisiteError(replay_result["disk_capacity"])
                if accepted:
                    current = proposal
                    witnesses = required
                    summary["accepted"] = candidate.name
                    changed = True
                summary["remaining_actions"] = len(current["actions"])
                summary["remaining_input"] = current
                summary["remaining_required_witnesses"] = [json.loads(item) for item in witnesses]
                save(destination / "reduction.json", summary)
                if accepted:
                    break
        summary.update(complete=True, budget_exhausted=(time.monotonic() >= deadline
                       or len(attempts) >= args.max_candidates), seconds=time.monotonic() - start)
    except (OSError, ValueError, RuntimeError) as error:
        summary["error"] = str(error)
        summary["outcome"] = "infrastructure_failure"
        if isinstance(error, DiskPrerequisiteError):
            summary["disk_capacity"] = error.evidence
    save(destination / "reduction.json", summary)
    print(json.dumps(summary, indent=2))
    return int(not summary["complete"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    campaign = sub.add_parser("run")
    campaign.add_argument("--profile", choices=["pr", "native", "scale"], default="pr")
    campaign.add_argument("--scenario")
    campaign.add_argument("--seed", type=int, default=19)
    campaign.add_argument("--input", type=Path, help="resolved artifact scenario with explicit seeds and actions")
    campaign.add_argument("--artifacts", type=Path,
                          default=ROOT / "dst/artifacts" / time.strftime("%Y%m%d-%H%M%S"))
    summary = sub.add_parser("report")
    summary.add_argument("directory", type=Path)
    exact = sub.add_parser("replay")
    exact.add_argument("directory", type=Path)
    prefix = sub.add_parser("explore")
    prefix.add_argument("directory", type=Path)
    prefix.add_argument("--choices", type=int, required=True)
    prefix.add_argument("--seed", type=int, required=True)
    prefix.add_argument("--timeout", type=float, default=300)
    prefix.add_argument("--artifacts", type=Path, required=True)
    reducer = sub.add_parser("reduce")
    reducer.add_argument("directory", type=Path)
    reducer.add_argument("--artifacts", type=Path, required=True)
    reducer.add_argument("--timeout", type=float, default=300)
    reducer.add_argument("--max-candidates", type=int, default=64)
    matrix = sub.add_parser("campaign")
    matrix.add_argument("--tier", choices=["pr", "nightly"], default="pr")
    matrix.add_argument("--seed", type=int, default=19)
    matrix.add_argument("--samples", type=int, default=8)
    matrix.add_argument("--sampler", choices=["templates-v2", COMPOSITION_VERSION, LIFECYCLE_VERSION], default="templates-v2",
                        help="nightly sampler; composition-v3 mixes v2 actions with bounded lifecycle actors")
    matrix.add_argument("--coverage-from", type=Path,
                        help="prior campaign directory used to weight nightly template selection")
    matrix.add_argument("--timeout", type=float, default=600)
    matrix.add_argument("--artifacts", type=Path, required=True)
    for command in (campaign, exact, prefix, reducer, matrix):
        command.add_argument("--min-free-disk-bytes", type=int, default=MIN_FREE_DISK_BYTES,
                             help="required available bytes per checked filesystem before work "
                                  "(default: 10737418240 / 10 GiB; positive; no cleanup)")
        command.add_argument("--disk-path", type=Path, action="append", default=[],
                             help="additional disk destination to check (repeatable); use for "
                                  "Cargo config-file target/build directories or custom scratch")
    args = parser.parse_args()
    if args.command == "report":
        return report(args.directory)
    if args.min_free_disk_bytes <= 0:
        parser.error("--min-free-disk-bytes must be positive")
    enforce_memory()
    if args.command == "campaign":
        if args.timeout <= 0 or not 1 <= args.samples <= 256:
            parser.error("campaign requires a positive timeout and 1..256 samples")
        if args.coverage_from and args.tier != "nightly":
            parser.error("--coverage-from requires --tier nightly")
        if args.sampler != "templates-v2" and args.tier != "nightly":
            parser.error(f"--sampler {args.sampler} requires --tier nightly")
        if args.sampler in {COMPOSITION_VERSION, LIFECYCLE_VERSION} and args.coverage_from:
            parser.error(f"{args.sampler} does not use template coverage weights")
        if args.sampler in {COMPOSITION_VERSION, LIFECYCLE_VERSION} and not 0 <= args.seed < 2**64:
            parser.error(f"{args.sampler} requires a u64 seed")
        return run_campaign(args)
    if args.command == "replay":
        return replay(args.directory, **disk_options(args))
    if args.command == "explore":
        if args.timeout <= 0 or not 0 <= args.choices <= 1024 or not 0 <= args.seed < 2**64:
            parser.error("exploration needs a positive timeout, 0..1024 choices, and a u64 seed")
        return explore(args)
    if args.command == "reduce":
        if args.timeout <= 0 or args.max_candidates <= 0:
            parser.error("reduction budgets must be positive")
        return reduce_artifact(args)
    return run(args)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, RuntimeError) as error:
        print(json.dumps({"outcome": "infrastructure_failure", "error": str(error)}),
              file=sys.stderr, flush=True)
        sys.exit(1)
