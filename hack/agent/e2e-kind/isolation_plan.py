#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Isolation modes test plan: scenario catalog, driver and report.

Runs the scenarios in designs/agent-isolation-modes-test-plan.md against a real
QEMU VM joined to a Kind control plane, one fresh VM per scenario, using the
suites in e2e.py. A failed scenario does not stop the run unless --fail-fast.
Results and logs land in tmp/isolation-report/<run-id>/ and the report is
rendered from results.json by isolation_report.py.

    python3 hack/agent/e2e-kind/isolation_plan.py list
    python3 hack/agent/e2e-kind/isolation_plan.py run                      # everything
    python3 hack/agent/e2e-kind/isolation_plan.py run --scenario T01 X01   # a selection
    python3 hack/agent/e2e-kind/isolation_plan.py render tmp/isolation-report/<run-id>/results.json
"""

from __future__ import annotations

import argparse
import datetime
import json
import os
import platform
import shutil
import subprocess
import sys
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Protocol

import isolation_report as report
import kind_kubeconfig

HERE = Path(__file__).resolve().parent
REPO_ROOT = HERE.parent.parent.parent
E2E = HERE / "e2e.py"
RUN_LOCAL = HERE / "run-local.sh"
DEFAULT_OUT = REPO_ROOT / "tmp" / "isolation-report"

SUITE_TIMEOUT_SECS = int(os.environ.get("PLAN_SUITE_TIMEOUT", "5400"))
STEP_TIMEOUT_SECS = int(os.environ.get("PLAN_INFRA_TIMEOUT", "1800"))


@dataclass(frozen=True)
class Scenario:
    """One scenario of the plan. *suite* is an e2e.py suite; *isolation_mode* is
    the value of ISOLATION_MODE it runs with (empty: unset, so nspawn with no
    --isolation flag). *requires* lists conditions that make the scenario skip:
    "host-os:<name>" needs that HOST_BASE_OS, "gpu" needs GPU hardware."""

    id: str
    title: str
    category: str
    suite: str | None
    isolation_mode: str
    description: str
    expected: str
    tags: tuple[str, ...] = ()
    requires: tuple[str, ...] = ()
    notes: str = ""

    @property
    def start_mode(self) -> str:
        return self.isolation_mode or "nspawn"

    @property
    def mode_label(self) -> str:
        return self.isolation_mode or "nspawn (default, no flag)"


OPERATIONS_TEXT = (
    "join, mode and status checks, preflight, exec and logs, workload and DNS, NodeReboot, host reboot, "
    "host-driven and operation AgentUpgrade with rollback, repave, AgentReset, reinstall on the same disk"
)

CATALOG: tuple[Scenario, ...] = (
    Scenario(
        "D01", "Default flow with no isolation set: existing lifecycle", "default", "lifecycle", "",
        "The unchanged lifecycle suite on a host bootstrapped without --isolation: join, NodeReboot, host-driven and "
        "operation AgentUpgrade with rollback, workload, host reboot, AgentReset, reinstall, repave.",
        "Passes exactly as it does without the isolation feature; the node runs in nspawn.",
        tags=("baseline", "nspawn"),
        notes="Includes nspawn-only checks (host/nspawn distro pair, bpffs isolation across repave).",
    ),
    Scenario(
        "D02", "Default (no flag): all operations", "operations", "isolation-operations", "",
        f"The isolation-operations suite with no --isolation flag: {OPERATIONS_TEXT}.",
        "Every operation passes; applied config records no Isolation key (nspawn) and status.isolation is nspawn.",
        tags=("nspawn",),
    ),
    Scenario(
        "N01", "Explicit nspawn: all operations", "operations", "isolation-operations", "nspawn",
        f"The isolation-operations suite with --isolation nspawn: {OPERATIONS_TEXT}.",
        "Identical to the default; the flag only makes the choice explicit.",
        tags=("nspawn",),
    ),
    Scenario(
        "X01", "none: all operations", "operations", "isolation-operations", "none",
        f"The isolation-operations suite with --isolation none: {OPERATIONS_TEXT}.",
        "containerd and kubelet run as agent-owned host units, no nspawn machine exists, NodeReboot leaves the host "
        "boot ID unchanged, repave and upgrade flip the version link, and after a configuration-only repave every "
        "pod that is not host-network has an IP in the new Node's pod CIDR. A daemon killed with SIGKILL during a "
        "configuration-only repave restarts and the node comes back Ready with containerd and kubelet active.",
        tags=("none",),
    ),
    Scenario(
        "C01", "container: all operations", "operations", "isolation-operations", "container",
        f"The isolation-operations suite with --isolation container (experimental, so the bootstrap also passes "
        f"--experimental-container-isolation): {OPERATIONS_TEXT}.",
        "One unbounded-node-<slot>.service runs the node stack under runc from its OCI bundle, no nspawn machine "
        "exists, repave switches slots and each slot has its own bpffs, and the same-path contract holds.",
        tags=("container", "provisional"),
        notes="Provisional: designed for Azure Container Linux. On ubuntu2404 this exercises runc, the OCI bundle "
              "and the unit, not the Azure Container Linux assumptions (SELinux label, PID 1 shutdown).",
    ),
    Scenario(
        "R01", "Default (no flag): reset on the host", "reset", "isolation-reset", "",
        "Run `unbounded-agent reset` on a default host, as an operator does without the control plane, and check "
        "what it removes.",
        "No nspawn machine is left, the agent's files and units are gone, and no reprovision handoff unit or staged "
        "configuration remains.",
        tags=("nspawn",),
    ),
    Scenario(
        "R02", "none: reset on the host", "reset", "isolation-reset", "none",
        "Run `unbounded-agent reset` on a none host and check what it removes.",
        "Agent-owned units and /opt/unbounded/node are removed, the agent's files and units are gone, and no "
        "reprovision handoff unit or staged configuration remains.",
        tags=("none",),
    ),
    Scenario(
        "R04", "container: reset on the host", "reset", "isolation-reset", "container",
        "Run `unbounded-agent reset` on a container host and check what it removes.",
        "Each slot's unbounded-node-<slot>.service unit, OCI bundle, root filesystem and bpffs mount are removed, "
        "the agent's files and units are gone, and no reprovision handoff unit or staged configuration remains.",
        tags=("container", "provisional"),
    ),
    Scenario(
        "R03", "Reset after a reprovision mode change", "reset", "isolation-reset-after-reprovision", "",
        "Change a default host to none by reprovision, which stages a handoff (a unit, a copy of the agent and a "
        "configuration that carries the bootstrap token), then run `unbounded-agent reset` on the host.",
        "No reprovision handoff unit, staged agent or staged configuration is left after the change or after the "
        "reset, and the none node stack is gone.",
        tags=("nspawn", "none", "transition"),
        notes="A handoff that gave up after failed runs has no harness fault; it is covered by unit tests only.",
    ),
    Scenario(
        "T01", "Every ordered pair of modes", "transitions", "isolation-matrix", "",
        "Declare each mode in spec.isolation and delete the Node, for every ordered pair: nspawn to container "
        "(in place), container to none and none to container (reprovision), container to nspawn (in place), "
        "nspawn to none and none to nspawn (reprovision). A reprovision runs with the allow-reprovision annotation "
        "set. After the last two changes, run a soft reboot and a host reboot, and the preflight in none.",
        "All six ordered pairs complete with status.isolationChange Succeeded and the strategy the pair calls for, "
        "the new mode recorded, a workload running and exec working, and no reprovision handoff left after a "
        "reprovision. The stops reached by reprovision to none and back to nspawn also survive a soft reboot and "
        "a host reboot, and none passes its preflight.",
        tags=("nspawn", "none", "container", "transition", "provisional"),
        notes="The suite bootstraps with --experimental-container-isolation so the host may change to container. "
              "Moves involving container are provisional on ubuntu2404.",
    ),
    Scenario(
        "T02", "Machine status and drift across mode changes", "transitions", "isolation-drift", "",
        "Check status.isolation and the IsolationDrift condition through the whole life of a change: an "
        "empty spec declares nothing, a declared unapplied mode shows AppliedModeDiffers while nothing moves, "
        "withdrawing it clears the drift, and a change refused for want of the reprovision opt-in shows "
        "ChangeRefused and is not retried. A change that succeeds ends at AppliedModeMatches; the matrix scenario "
        "asserts that after every change.",
        "isolation follows the applied mode; IsolationDrift is NoModeDeclared, AppliedModeMatches, "
        "AppliedModeDiffers or ChangeRefused as the spec and the change dictate.",
        tags=("nspawn", "none", "transition"),
        notes="The harness edits spec.isolation; the agent never does. Watch for side effects on generated agent config.",
    ),
    Scenario(
        "T03", "Mode change that cannot go ahead", "transitions", "isolation-rollback", "",
        "Declare the applied mode and check nothing moves or is recorded, then declare none without the "
        "allow-reprovision annotation, delete the Node and check the change is refused while the repave still "
        "brings the node back in nspawn, nothing retries it and no handoff is staged. Then declare container on "
        "the host, whose agent configuration does not opt in to the experimental mode, and check it is refused "
        "the same way.",
        "status.isolationChange is Failed with reason ReprovisionNotAllowed, then ExperimentalNotEnabled, "
        "IsolationDrift shows ChangeRefused each time, the source mode is recorded and running, and a workload runs.",
        tags=("nspawn", "none", "container", "transition"),
        notes="A failed in-place change and its restore of the shared host configuration are T04. A failed "
              "reprovision has no harness fault and is covered by unit tests only.",
    ),
    Scenario(
        "T04", "Failed in-place change: rollback and config restore", "transitions", "isolation-rollback-in-place",
        "container",
        "Declare the applied mode and check nothing moves or is recorded, check a change to nspawn is refused "
        "before the node is touched while the host lacks the nspawn runtime, then fail an in-place container to "
        "nspawn change after the source stopped and check the source runs again, the failure is not retried, and "
        "the shared host configuration is byte for byte unchanged. Change to nspawn in place, then fail an "
        "in-place nspawn to container change the same way.",
        "status.isolationChange is Failed with a persist error and a rollback to the source, IsolationDrift shows "
        "ChangeFailed, the source mode is recorded and running, no in-place journal is left, and the shared "
        "config digests match.",
        tags=("nspawn", "container", "transition", "provisional"),
        notes="Starts in container mode because only a container host keeps its configuration at the shared host "
              "paths the digest compares. The fault is harness-only (a directory in place of the target's applied "
              "config checksum). Reprovision rollback has no harness fault and is covered by unit tests only.",
    ),
    Scenario(
        "A01", "container on Azure Container Linux", "operations", "isolation-operations", "container",
        "The isolation-operations suite in container mode on an immutable Azure Container Linux host.",
        "Confirms the provisional container assumptions on the intended platform.",
        tags=("container", "acl"), requires=("host-os:acl",),
        notes="Needs HOST_BASE_OS=acl and an image (gallery export or HOST_IMAGE_PATH). Skipped otherwise.",
    ),
    Scenario(
        "G01", "GPU workloads in none mode", "operations", None, "none",
        "NVIDIA device, library and CDI wiring in none mode.",
        "A pod requesting nvidia.com/gpu runs and sees the device.",
        tags=("gpu",), requires=("gpu",),
        notes="Needs GPU passthrough hardware; not runnable in a plain QEMU VM.",
    ),
)

SCENARIOS_BY_ID = {s.id: s for s in CATALOG}


class Runner(Protocol):
    """Runs commands. The driver is tested with a fake."""

    def run(self, argv: list[str], env: dict[str, str], cwd: Path,
            on_line: Callable[[str], None], timeout: float) -> int:
        """Runs argv, passing each output line to on_line, and returns its exit code."""

    def capture(self, argv: list[str], env: dict[str, str], cwd: Path, timeout: float) -> tuple[int, str]:
        """Runs argv and returns its exit code and output."""


class SubprocessRunner:
    """Runs real processes, streaming merged output line by line."""

    def run(self, argv, env, cwd, on_line, timeout):  # noqa: ANN001
        proc = subprocess.Popen(argv, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                text=True, errors="replace", bufsize=1)
        expired = threading.Event()

        def kill() -> None:
            expired.set()
            proc.kill()

        timer = threading.Timer(timeout, kill)
        timer.start()
        try:
            assert proc.stdout is not None
            for line in proc.stdout:
                on_line(line.rstrip("\n"))
            rc = proc.wait()
        finally:
            timer.cancel()
        if expired.is_set():
            on_line(f"[ERROR] timed out after {int(timeout)}s")
        return rc

    def capture(self, argv, env, cwd, timeout):  # noqa: ANN001
        try:
            done = subprocess.run(argv, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout, check=False)
        except (subprocess.TimeoutExpired, OSError) as exc:
            return 124, str(exc)
        return done.returncode, done.stdout + done.stderr


def select(args_scenarios: list[str], categories: list[str], tags: list[str], exclude_tags: list[str]) -> list[Scenario]:
    """Return catalog scenarios matching the filters, in catalog order."""
    unknown = [i for i in args_scenarios if i not in SCENARIOS_BY_ID]
    if unknown:
        raise SystemExit(f"unknown scenario ids: {', '.join(unknown)}; known: {', '.join(SCENARIOS_BY_ID)}")
    chosen = []
    for scenario in CATALOG:
        if args_scenarios and scenario.id not in args_scenarios:
            continue
        if categories and scenario.category not in categories:
            continue
        if tags and not set(tags) & set(scenario.tags):
            continue
        if set(exclude_tags) & set(scenario.tags):
            continue
        chosen.append(scenario)
    return chosen


def skip_reason(scenario: Scenario, host_base_os: str) -> str:
    """Return why a scenario cannot run here, or an empty string."""
    for need in scenario.requires:
        if need == "gpu":
            return "needs GPU passthrough hardware, which a plain QEMU VM does not have"
        if need.startswith("host-os:") and need.split(":", 1)[1] != host_base_os:
            return f"needs HOST_BASE_OS={need.split(':', 1)[1]} (this run uses {host_base_os})"
    if scenario.suite is None:
        return "no automated suite"
    return ""


@dataclass
class Driver:
    runner: Runner
    out_root: Path = DEFAULT_OUT
    repo_root: Path = REPO_ROOT
    host_base_os: str = "ubuntu2404"
    clock: Callable[[], float] = time.time
    base_env: dict[str, str] = field(default_factory=lambda: dict(os.environ))
    fail_fast: bool = False
    keep_env: bool = False
    no_up: bool = False

    # -- helpers -----------------------------------------------------------
    def _iso(self, stamp: float) -> str:
        return datetime.datetime.fromtimestamp(stamp, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    def _env(self, scenario: Scenario | None = None) -> dict[str, str]:
        env = dict(self.base_env)
        tools = self.repo_root / "tmp" / "bin"
        if tools.is_dir():
            env["PATH"] = f"{tools}{os.pathsep}{env.get('PATH', '')}"
        env["HOST_BASE_OS"] = self.host_base_os
        # Never the user's default kubeconfig: e2e.py and run-local.sh pin
        # themselves to the Kind cluster's file, and so does everything else
        # the plan runs.
        cluster = env.get("KIND_CLUSTER_NAME") or "kind"
        env["KUBECONFIG"] = str(kind_kubeconfig.resolve_path(self.repo_root, cluster, env)[0])
        env.pop("ISOLATION_MODE", None)
        env.pop("E2E_SUITE", None)
        if scenario and scenario.isolation_mode:
            env["ISOLATION_MODE"] = scenario.isolation_mode
        return env

    def _e2e(self, *args: str) -> list[str]:
        return [sys.executable, str(E2E), "--verbose", *args]

    def _logged(self, argv: list[str], env: dict[str, str], log_path: Path, timeout: float) -> tuple[int, list[tuple[float, str]]]:
        log_path.parent.mkdir(parents=True, exist_ok=True)
        lines: list[tuple[float, str]] = []
        with log_path.open("w") as out:
            def on_line(text: str) -> None:
                text = report.redact(text)
                lines.append((self.clock(), text))
                out.write(text + "\n")
                out.flush()

            rc = self.runner.run(argv, env, self.repo_root, on_line, timeout)
        return rc, lines

    def environment(self) -> dict[str, str]:
        def tool(argv: list[str]) -> str:
            rc, out = self.runner.capture(argv, self._env(), self.repo_root, 30)
            return out.strip().splitlines()[0] if rc == 0 and out.strip() else "unavailable"

        return {
            "git commit": tool(["git", "rev-parse", "--short=12", "HEAD"]),
            "git branch": tool(["git", "rev-parse", "--abbrev-ref", "HEAD"]),
            "host": platform.node() or "unknown",
            "kernel": platform.release(),
            "host base OS (VM)": self.host_base_os,
            "qemu": tool(["qemu-system-x86_64", "--version"]),
            "kind": tool(["kind", "version"]),
            "docker": tool(["docker", "--version"]),
            "kubectl": tool(["kubectl", "version", "--client"]),
        }

    def preflight(self) -> list[str]:
        """Return problems that would make every scenario fail."""
        problems = []
        env = self._env()
        for cmd in ("docker", "kind", "kubectl", "go", "qemu-system-x86_64", "qemu-img", "genisoimage", "python3", "sudo"):
            rc, _ = self.runner.capture(["sh", "-c", f"command -v {cmd}"], env, self.repo_root, 10)
            if rc != 0:
                problems.append(f"{cmd} not found on PATH")
        rc, _ = self.runner.capture(["sh", "-c", "test -r /dev/kvm -a -w /dev/kvm"], env, self.repo_root, 10)
        if rc != 0:
            problems.append("/dev/kvm is not accessible (add your user to the kvm group, or run under `sg kvm -c`)")
        return problems

    # -- infrastructure ----------------------------------------------------
    def up(self, run_dir: Path) -> bool:
        """Create the Kind cluster, the VM bridge and the controllers, and keep them."""
        env = self._env()
        env.update({"E2E_SUITE": "setup", "KEEP_ENV": "1"})
        rc, _ = self._logged(["bash", str(RUN_LOCAL)], env, run_dir / "infra-up.log", STEP_TIMEOUT_SECS)
        return rc == 0

    def down(self, run_dir: Path) -> None:
        """Remove the VM, the bridge rules and the Kind cluster."""
        self._logged(self._e2e("cleanup"), self._env(), run_dir / "infra-down.log", STEP_TIMEOUT_SECS)

    def prepare_vm(self, scenario: Scenario, run_dir: Path, vm_used: bool) -> bool:
        env = self._env(scenario)
        directory = run_dir / scenario.id
        if vm_used:
            self._logged(self._e2e("retire-lifecycle-vm"), env, directory / "vm-retire.log", STEP_TIMEOUT_SECS)
        rc, _ = self._logged(self._e2e("launch-vm"), env, directory / "vm-launch.log", STEP_TIMEOUT_SECS)
        return rc == 0

    # -- scenarios ---------------------------------------------------------
    def evidence(self, scenario: Scenario, directory: Path) -> dict[str, Any]:
        rc, out = self.runner.capture(self._e2e("collect-isolation-evidence"), self._env(scenario), self.repo_root, 180)
        (directory / "evidence.log").write_text(report.redact(out))
        for line in out.splitlines():
            if line.startswith("ISOLATION_EVIDENCE "):
                try:
                    return json.loads(line[len("ISOLATION_EVIDENCE "):])
                except json.JSONDecodeError:
                    break
        return {"error": f"no evidence collected (exit {rc})"}

    def collect_logs(self, scenario: Scenario, directory: Path) -> None:
        """Save the harness diagnostics of a failed scenario beside its log."""
        self.runner.capture(self._e2e("collect-logs"), self._env(scenario), self.repo_root, 300)
        source = self.repo_root / "logs"
        if source.is_dir():
            target = directory / "diagnostics"
            shutil.rmtree(target, ignore_errors=True)
            shutil.move(str(source), str(target))

    def run_scenario(self, scenario: Scenario, run_dir: Path, vm_used: bool) -> tuple[dict[str, Any], bool]:
        """Run one scenario. Returns its result and whether the VM is now used."""
        directory = run_dir / scenario.id
        result: dict[str, Any] = {
            "id": scenario.id, "title": scenario.title, "category": scenario.category, "suite": scenario.suite,
            "isolation_mode": scenario.isolation_mode, "mode_label": scenario.mode_label,
            "description": scenario.description, "expected": scenario.expected, "notes": scenario.notes,
            "tags": list(scenario.tags), "log_path": str((directory / "suite.log").relative_to(run_dir)),
            "steps": [], "failure": None, "evidence": None,
        }
        reason = skip_reason(scenario, self.host_base_os)
        started = self.clock()
        result["started"] = self._iso(started)
        if reason:
            result.update(status="skip", skip_reason=reason, duration_s=0.0, finished=self._iso(started))
            return result, vm_used

        env = self._env(scenario)
        rc, planned_out = self.runner.capture(self._e2e("list-suite", "--suite", scenario.suite or ""), env, self.repo_root, 60)
        planned = planned_out.split() if rc == 0 else []

        if not self.prepare_vm(scenario, run_dir, vm_used):
            finished = self.clock()
            result.update(status="error", duration_s=round(finished - started, 1), finished=self._iso(finished),
                          failure={"step": "launch-vm", "message": "could not start the QEMU VM; see vm-launch.log",
                                   "log_excerpt": ""})
            return result, True

        rc, lines = self._logged(self._e2e("run-suite", "--suite", scenario.suite or ""), env, directory / "suite.log",
                                 SUITE_TIMEOUT_SECS)
        finished = self.clock()
        parsed = report.parse_suite_log(lines, planned, finished, scenario.suite or "", rc)
        steps = report.annotate_steps(scenario.start_mode, parsed["steps"])
        result.update(status="pass" if parsed["suite_passed"] else "fail", steps=steps, failure=parsed["failure"],
                      duration_s=round(finished - started, 1), finished=self._iso(finished))
        result["evidence"] = self.evidence(scenario, directory)
        if not parsed["suite_passed"]:
            self.collect_logs(scenario, directory)
        return result, True

    # -- the run -----------------------------------------------------------
    def execute(self, scenarios: list[Scenario], run_id: str) -> dict[str, Any]:
        """Run the scenarios and write results.json (after every scenario),
        report.md and report.html. Returns the results document."""
        run_dir = self.out_root / run_id
        run_dir.mkdir(parents=True, exist_ok=True)
        results: dict[str, Any] = {"run_id": run_id, "started": self._iso(self.clock()), "environment": self.environment(),
                                   "scenarios": [], "finished": ""}

        def save() -> None:
            results["finished"] = self._iso(self.clock())
            results["summary"] = report.summarize(results["scenarios"])
            (run_dir / "results.json").write_text(json.dumps(results, indent=2, sort_keys=True) + "\n")

        problems = self.preflight()
        runnable = [s for s in scenarios if not skip_reason(s, self.host_base_os)]
        infra_ok = True
        infra_message = ""
        if problems and runnable:
            infra_ok, infra_message = False, "host preflight failed: " + "; ".join(problems)
        elif runnable and not self.no_up:
            infra_ok = self.up(run_dir)
            infra_message = "could not create the Kind cluster and controllers; see infra-up.log"

        vm_used = False
        stop = False
        for scenario in scenarios:
            if stop:
                results["scenarios"].append(self._not_run(scenario, "not run: --fail-fast after an earlier failure"))
            elif not infra_ok and not skip_reason(scenario, self.host_base_os):
                results["scenarios"].append(self._not_run(scenario, infra_message, status="error"))
            else:
                result, vm_used = self.run_scenario(scenario, run_dir, vm_used)
                results["scenarios"].append(result)
                stop = self.fail_fast and result["status"] in ("fail", "error")
            save()

        if runnable and infra_ok and not self.keep_env and not self.no_up:
            self.down(run_dir)
        save()
        (run_dir / "report.md").write_text(report.render_markdown(results))
        (run_dir / "report.html").write_text(report.render_html(results))
        return results

    def _not_run(self, scenario: Scenario, message: str, status: str = "skip") -> dict[str, Any]:
        now = self._iso(self.clock())
        return {"id": scenario.id, "title": scenario.title, "category": scenario.category, "suite": scenario.suite,
                "isolation_mode": scenario.isolation_mode, "mode_label": scenario.mode_label,
                "description": scenario.description, "expected": scenario.expected, "notes": scenario.notes,
                "tags": list(scenario.tags), "log_path": "-", "steps": [], "evidence": None, "started": now,
                "finished": now, "duration_s": 0.0, "status": status,
                "skip_reason": message if status == "skip" else "",
                "failure": {"step": "", "message": message, "log_excerpt": ""} if status == "error" else None}


def plan_markdown() -> str:
    """Return the catalog as a Markdown table, for the plan document."""
    rows = ["| ID | Scenario | Start mode | e2e suite | Requires |", "|---|---|---|---|---|"]
    for s in CATALOG:
        rows.append(f"| {s.id} | {s.title} | {s.mode_label} | {s.suite or '-'} | {', '.join(s.requires) or '-'} |")
    return "\n".join(rows)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("list", help="print the scenario catalog")
    sub.add_parser("plan", help="print the catalog as a Markdown table")
    run = sub.add_parser("run", help="run scenarios and write the report")
    run.add_argument("--scenario", nargs="+", default=[], metavar="ID", help="run only these scenario ids")
    run.add_argument("--category", nargs="+", default=[], help="run only these categories")
    run.add_argument("--tag", nargs="+", default=[], help="run scenarios with any of these tags")
    run.add_argument("--exclude-tag", nargs="+", default=[], help="skip scenarios with any of these tags")
    run.add_argument("--fail-fast", action="store_true", help="stop after the first failed scenario")
    run.add_argument("--no-up", action="store_true", help="use an existing Kind cluster and bridge; create and remove nothing")
    run.add_argument("--keep-env", action="store_true", help="leave the cluster, bridge and VM in place afterward")
    run.add_argument("--run-id", default="", help="report directory name (default: UTC timestamp)")
    run.add_argument("--out", type=Path, default=DEFAULT_OUT, help="report root (default: tmp/isolation-report)")
    run.add_argument("--host-base-os", default=os.environ.get("HOST_BASE_OS", "ubuntu2404"))
    run.add_argument("--dry-run", action="store_true", help="print the selected scenarios and exit")
    render = sub.add_parser("render", help="render report.md (and report.html) from results.json")
    render.add_argument("results", type=Path)
    render.add_argument("--html", action="store_true")
    return parser.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    if args.command == "list":
        for s in CATALOG:
            print(f"{s.id:4} {s.category:12} {s.mode_label:26} {s.suite or '-':22} {s.title}")
        return 0
    if args.command == "plan":
        print(plan_markdown())
        return 0
    if args.command == "render":
        return report.main([str(args.results), *(["--html"] if args.html else [])])

    scenarios = select(args.scenario, args.category, args.tag, args.exclude_tag)
    if not scenarios:
        print("no scenarios selected", file=sys.stderr)
        return 2
    if args.dry_run:
        for s in scenarios:
            why = skip_reason(s, args.host_base_os)
            print(f"{s.id}  {s.suite or '-'}  ISOLATION_MODE={s.isolation_mode or '<unset>'}" + (f"  SKIP: {why}" if why else ""))
        return 0
    run_id = args.run_id or datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%d-%H%M%S")
    driver = Driver(SubprocessRunner(), out_root=args.out, host_base_os=args.host_base_os,
                    fail_fast=args.fail_fast, keep_env=args.keep_env, no_up=args.no_up)
    results = driver.execute(scenarios, run_id)
    summary = results["summary"]
    print(f"report: {args.out / run_id / 'report.md'}")
    print(f"{summary['pass']} passed, {summary['fail']} failed, {summary['skip']} skipped, {summary['error']} errors")
    return 0 if summary["fail"] == 0 and summary["error"] == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
