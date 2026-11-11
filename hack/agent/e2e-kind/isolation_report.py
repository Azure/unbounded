#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Result parsing and report rendering for the isolation test plan.

isolation_plan.py runs the scenarios and writes results.json; this module turns
the log of one e2e.py suite run into per-step results, annotates each step with
the operation it exercises and the isolation mode the node was in, and renders
the whole run as Markdown or HTML.

    python3 isolation_report.py tmp/isolation-report/<run-id>/results.json [--html]
"""

from __future__ import annotations

import argparse
import html
import json
import re
import sys
from pathlib import Path
from typing import Any

# The isolation modes the report has columns for. A new mode is added here and,
# when it changes in place with another mode, its pairs to IN_PLACE_PAIRS.
MODES = ("nspawn", "none", "container")
# Ordered mode pairs changed in place (pkg/agent/isolation/migrate.Plan); every
# other pair is a reprovision. none has no slot, so only nspawn and container
# change in place, in both directions.
IN_PLACE_PAIRS: frozenset[tuple[str, str]] = frozenset({("nspawn", "container"), ("container", "nspawn")})
STRATEGIES = (("inPlace",) if IN_PLACE_PAIRS else ()) + ("reprovision",)

# What each e2e.py step exercises, for the coverage matrix. Steps that only
# prepare or observe the environment are absent and appear in no matrix row.
STEP_OPERATIONS: dict[str, str] = {
    "run-agent": "Join (bootstrap)",
    "wait-for-node": "Join (bootstrap)",
    "reinstall-agent": "Reinstall on the same disk",
    "validate-isolation-mode": "Mode recorded and node stack running",
    "validate-isolation-status": "Machine status (isolation, IsolationDrift)",
    "reconcile-machine-isolation-spec": "Machine status (isolation, IsolationDrift)",
    "validate-isolation-preflight": "Preflight",
    "validate-isolation-exec-logs": "exec and logs",
    "validate-workload": "Workload and DNS",
    "validate-isolation-soft-reboot": "NodeReboot (node stack restart)",
    "validate-node-reboot-operation": "NodeReboot (node stack restart)",
    "validate-host-agent-upgrade": "AgentUpgrade (host driven)",
    "validate-agent-upgrade-operation": "AgentUpgrade (operation)",
    "validate-agent-upgrade-rollback": "AgentUpgrade rollback",
    "validate-node-repave-upgrade": "Repave (Kubernetes version upgrade)",
    "validate-host-reboot": "Host reboot",
    "reset-agent": "AgentReset",
    "reset-agent-on-host": "Reset on the host (unbounded-agent reset)",
    "validate-no-reprovision-handoff": "No reprovision handoff left on the host",
    "validate-reprovision-refused": "Mode change refused without the reprovision opt-in",
    "validate-experimental-refused": "Mode change refused without the experimental opt-in",
    "validate-change-needs-nspawn-runtime": "Mode change refused before the node is touched",
    "validate-change-rollback-config": "In-place change rollback and config restore",
    "validate-change-rollback-to-container": "In-place change rollback and config restore",
    "validate-same-mode-declaration": "Declaring the applied mode moves nothing",
    "declare-isolation-mismatch": "Machine status (isolation, IsolationDrift)",
    "validate-host-root": "Host and cluster checks",
    "validate-node-config": "Host and cluster checks",
    "dump-persisted-agent-config": "Host and cluster checks",
    "validate-kube-proxy": "Host and cluster checks",
    "validate-machine-cr-created": "Host and cluster checks",
    "validate-controllers-healthy": "Host and cluster checks",
    "validate-host-nspawn-distro": "Host and cluster checks",
}

OPERATION_ORDER = list(dict.fromkeys(STEP_OPERATIONS.values()))

_CHANGE_STEP = re.compile(r"^change-to-(" + "|".join(MODES) + r")$")
_STEP_LINE = re.compile(r"\[(\d+)/(\d+)\]\s+(\S+):\s+(\S+)\s*$")
_SUITE_PASSED = re.compile(r"suite '([^']+)' PASSED")

# A kubeadm style bootstrap token, and the token fields of JSON documents.
_TOKEN = re.compile(r"\b[a-z0-9]{6}\.[a-z0-9]{16}\b")
_JSON_SECRET = re.compile(r'("(?:BootstrapToken|bootstrapToken|token|Token)"\s*:\s*")[^"]+(")')


def redact(text: str) -> str:
    """Remove bootstrap tokens from text that goes into a report or a saved log."""
    return _JSON_SECRET.sub(r"\1<redacted>\2", _TOKEN.sub("<redacted-token>", text))


def parse_change_step(step: str) -> str | None:
    """Return the target mode for a change-to-<mode> step."""
    match = _CHANGE_STEP.match(step)
    return match.group(1) if match else None


def change_strategy(source: str, target: str) -> str:
    """Return the strategy a change from source to target uses: in place for
    the pairs in IN_PLACE_PAIRS, reprovision otherwise
    (pkg/agent/isolation/migrate.Plan)."""
    return "inPlace" if (source, target) in IN_PLACE_PAIRS else "reprovision"


def annotate_steps(start_mode: str, steps: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Add the isolation mode in effect and the operation to each step. A
    successful change step moves the mode on; a failed one leaves it, since
    a failed change rolls back."""
    mode = start_mode
    for step in steps:
        target = parse_change_step(step["name"])
        step["mode"] = mode
        step["operation"] = STEP_OPERATIONS.get(step["name"], "")
        if target:
            strategy = change_strategy(mode, target)
            step["transition"] = {"from": mode, "to": target, "strategy": strategy}
            step["operation"] = f"Mode change to {target} ({strategy})"
            if step["status"] == "pass":
                mode = target
    return steps


def parse_suite_log(
    lines: list[tuple[float, str]],
    planned: list[str],
    finished: float,
    suite: str,
    returncode: int,
) -> dict[str, Any]:
    """Turn the timestamped output lines of `e2e.py run-suite` into steps and a
    failure description. *lines* is (epoch seconds, text); *planned* is the
    suite's step list, so steps after a failure are reported as not run."""
    started: list[tuple[int, str, float]] = []
    passed_suite = False
    last_error = ""
    for stamp, text in lines:
        match = _STEP_LINE.search(text)
        if match and match.group(3) == suite:
            started.append((int(match.group(1)), match.group(4), stamp))
            continue
        found = _SUITE_PASSED.search(text)
        if found and found.group(1) == suite:
            passed_suite = True
        if text.startswith("[ERROR]"):
            last_error = text[len("[ERROR]"):].strip()

    ok = returncode == 0 and passed_suite
    steps: list[dict[str, Any]] = []
    for position, (index, name, stamp) in enumerate(started):
        end = started[position + 1][2] if position + 1 < len(started) else finished
        last = position == len(started) - 1
        status = "pass" if (not last or ok) else "fail"
        steps.append({"index": index, "name": name, "status": status, "duration_s": round(max(end - stamp, 0.0), 1)})
    for index, name in enumerate(planned, start=1):
        if index > len(started):
            steps.append({"index": index, "name": name, "status": "not-run", "duration_s": 0.0})

    failure: dict[str, Any] | None = None
    if not ok:
        failed = next((s for s in steps if s["status"] == "fail"), None)
        tail = [text for _, text in lines[-40:]]
        failure = {
            "step": failed["name"] if failed else "",
            "message": redact(last_error or (tail[-1] if tail else "no output")),
            "log_excerpt": redact("\n".join(tail)),
        }
    return {"steps": steps, "failure": failure, "suite_passed": ok}


def summarize(scenarios: list[dict[str, Any]]) -> dict[str, int]:
    """Count scenarios by status."""
    counts = {"pass": 0, "fail": 0, "skip": 0, "error": 0}
    for scenario in scenarios:
        counts[scenario["status"]] = counts.get(scenario["status"], 0) + 1
    counts["total"] = len(scenarios)
    return counts


def _worst(current: str | None, new: str) -> str:
    order = {"fail": 3, "pass": 2, "not-run": 1}
    if current is None:
        return new
    return new if order.get(new, 0) > order.get(current, 0) else current


def coverage_matrix(scenarios: list[dict[str, Any]]) -> dict[str, dict[str, str]]:
    """Operation by mode: pass, fail or not-run (absent when never planned)."""
    cells: dict[str, dict[str, str]] = {}
    for scenario in scenarios:
        for step in scenario.get("steps", []):
            operation, mode = step.get("operation", ""), step.get("mode", "")
            if not operation or not mode or step.get("transition"):
                continue
            row = cells.setdefault(operation, {})
            row[mode] = _worst(row.get(mode), step["status"])
    return cells


def transition_matrix(scenarios: list[dict[str, Any]]) -> dict[str, dict[str, str]]:
    """Mode pair by strategy: pass, fail or not-run."""
    cells: dict[str, dict[str, str]] = {}
    for scenario in scenarios:
        for step in scenario.get("steps", []):
            move = step.get("transition")
            if not move:
                continue
            row = cells.setdefault(f"{move['from']} -> {move['to']}", {})
            row[move["strategy"]] = _worst(row.get(move["strategy"]), step["status"])
    return cells


STATUS_WORD = {"pass": "PASS", "fail": "FAIL", "skip": "SKIP", "error": "ERROR", "not-run": "not run"}


def _word(status: str | None) -> str:
    return STATUS_WORD.get(status or "", "-")


def _duration(seconds: float) -> str:
    seconds = int(round(seconds))
    minutes, secs = divmod(seconds, 60)
    return f"{minutes}m{secs:02d}s" if minutes else f"{secs}s"


def build_blocks(results: dict[str, Any]) -> list[tuple[str, Any]]:
    """Build the report as a list of (kind, payload) blocks: h1, h2, h3, p,
    table (header, rows), code (text)."""
    scenarios = results["scenarios"]
    counts = summarize(scenarios)
    env = results.get("environment", {})
    blocks: list[tuple[str, Any]] = [("h1", "Isolation modes test report")]
    blocks.append(("p", f"Run {results['run_id']}: {counts['pass']} passed, {counts['fail']} failed, "
                        f"{counts['skip']} skipped, {counts['error']} errors, of {counts['total']} scenarios. "
                        f"Started {results.get('started', '')}, finished {results.get('finished', '')}."))

    blocks.append(("h2", "Environment"))
    blocks.append(("table", (["Item", "Value"], [[k, str(v)] for k, v in env.items()])))

    blocks.append(("h2", "Summary"))
    rows = [[s["id"], s["title"], s.get("mode_label", ""), s.get("suite") or "-", _word(s["status"]),
             _duration(s.get("duration_s", 0))] for s in scenarios]
    blocks.append(("table", (["ID", "Scenario", "Start mode", "e2e suite", "Result", "Time"], rows)))

    cov = coverage_matrix(scenarios)
    if cov:
        blocks.append(("h2", "Operation coverage by isolation mode"))
        blocks.append(("p", "Each cell is the worst result of every step that exercised the operation while the node "
                            "was in that mode. A dash means no scenario ran the operation in that mode."))
        ordered = [op for op in OPERATION_ORDER if op in cov] + sorted(op for op in cov if op not in OPERATION_ORDER)
        blocks.append(("table", (["Operation", *MODES], [[op, *[_word(cov[op].get(m)) for m in MODES]] for op in ordered])))

    trans = transition_matrix(scenarios)
    if trans:
        blocks.append(("h2", "Mode transitions"))
        blocks.append(("p", "In place is supported between nspawn and container only; any move involving none is a "
                            "reprovision, as none has no slot to prepare the target in beside the source."))
        blocks.append(("table", (["Transition", *STRATEGIES],
                                 [[pair, *[_word(trans[pair].get(s)) for s in STRATEGIES]] for pair in sorted(trans)])))

    blocks.append(("h2", "Scenario details"))
    for scenario in scenarios:
        blocks.append(("h3", f"{scenario['id']}: {scenario['title']} ({_word(scenario['status'])})"))
        meta = [["Suite", scenario.get("suite") or "-"], ["Start mode", scenario.get("mode_label", "")],
                ["Duration", _duration(scenario.get("duration_s", 0))], ["Log", scenario.get("log_path", "-")]]
        blocks.append(("table", (["Field", "Value"], meta)))
        if scenario.get("description"):
            blocks.append(("p", scenario["description"]))
        if scenario.get("expected"):
            blocks.append(("p", f"Expected: {scenario['expected']}"))
        if scenario.get("notes"):
            blocks.append(("p", f"Note: {scenario['notes']}"))
        if scenario.get("skip_reason"):
            blocks.append(("p", f"Skipped: {scenario['skip_reason']}"))
        steps = scenario.get("steps", [])
        if steps:
            blocks.append(("table", (["#", "Step", "Mode", "Result", "Time"],
                                     [[str(s["index"]), s["name"], s.get("mode", ""), _word(s["status"]),
                                       _duration(s.get("duration_s", 0))] for s in steps])))
        failure = scenario.get("failure")
        if failure:
            blocks.append(("p", f"Failure at step {failure.get('step') or 'unknown'}: {failure.get('message', '')}"))
            blocks.append(("code", failure.get("log_excerpt", "")))
        evidence = scenario.get("evidence")
        if evidence:
            blocks.append(("p", "Evidence captured after the suite:"))
            blocks.append(("code", json.dumps(evidence, indent=2, sort_keys=True)))
    return blocks


def to_markdown(blocks: list[tuple[str, Any]]) -> str:
    """Render blocks as GitHub flavored Markdown."""
    out: list[str] = []
    for kind, payload in blocks:
        if kind in ("h1", "h2", "h3"):
            out.append(f"{'#' * int(kind[1])} {payload}\n")
        elif kind == "p":
            out.append(f"{payload}\n")
        elif kind == "table":
            header, rows = payload
            cell = lambda value: str(value).replace("|", "\\|").replace("\n", " ")  # noqa: E731
            out.append("| " + " | ".join(cell(h) for h in header) + " |")
            out.append("|" + "|".join("---" for _ in header) + "|")
            out.extend("| " + " | ".join(cell(c) for c in row) + " |" for row in rows)
            out.append("")
        elif kind == "code":
            out.append("```\n" + payload.rstrip("\n") + "\n```\n")
    return "\n".join(out)


_HTML_STYLE = (
    "body{font-family:sans-serif;max-width:1100px;margin:2em auto;padding:0 1em}"
    "table{border-collapse:collapse;margin:1em 0}td,th{border:1px solid #bbb;padding:4px 10px;text-align:left}"
    "pre{background:#f4f4f4;padding:8px;overflow:auto}"
    ".PASS{color:#0a6b0a;font-weight:bold}.FAIL,.ERROR{color:#b00020;font-weight:bold}.SKIP{color:#8a6d00}"
)


def to_html(blocks: list[tuple[str, Any]], title: str = "Isolation modes test report") -> str:
    """Render blocks as a standalone HTML page. Result words are colored."""
    esc = html.escape

    def cell(value: Any) -> str:
        text = str(value)
        if text in ("PASS", "FAIL", "SKIP", "ERROR"):
            return f'<span class="{text}">{text}</span>'
        return esc(text)

    out = [f"<!doctype html><html><head><meta charset='utf-8'><title>{esc(title)}</title>"
           f"<style>{_HTML_STYLE}</style></head><body>"]
    for kind, payload in blocks:
        if kind in ("h1", "h2", "h3"):
            out.append(f"<{kind}>{esc(payload)}</{kind}>")
        elif kind == "p":
            out.append(f"<p>{esc(payload)}</p>")
        elif kind == "table":
            header, rows = payload
            out.append("<table><tr>" + "".join(f"<th>{esc(h)}</th>" for h in header) + "</tr>")
            out.extend("<tr>" + "".join(f"<td>{cell(c)}</td>" for c in row) + "</tr>" for row in rows)
            out.append("</table>")
        elif kind == "code":
            out.append(f"<pre>{esc(payload)}</pre>")
    out.append("</body></html>")
    return "\n".join(out)


def render_markdown(results: dict[str, Any]) -> str:
    return to_markdown(build_blocks(results))


def render_html(results: dict[str, Any]) -> str:
    return to_html(build_blocks(results))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Render an isolation test plan results.json")
    parser.add_argument("results", type=Path, help="path to results.json")
    parser.add_argument("--html", action="store_true", help="also write report.html next to the Markdown report")
    parser.add_argument("--output", type=Path, default=None, help="Markdown output path (default: report.md beside results.json)")
    args = parser.parse_args(argv)
    results = json.loads(args.results.read_text())
    markdown = args.output or args.results.with_name("report.md")
    markdown.write_text(render_markdown(results))
    print(f"wrote {markdown}")
    if args.html:
        page = markdown.with_suffix(".html")
        page.write_text(render_html(results))
        print(f"wrote {page}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
