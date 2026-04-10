#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""analyze-bootstrap-log.py — Parse unbounded-agent bootstrap logs and print a
phase duration breakdown.

Usage:
    ./analyze-bootstrap-log.py <logfile>
    ./analyze-bootstrap-log.py <logfile> --json

Arguments:
    logfile  - path to the agent bootstrap log file (pretty or JSON format)
    --json   - optional flag to emit output as JSON instead of a table

The script handles both the pretty console format and the JSON log format
(produced with --log-format json). ANSI color codes are stripped automatically.

Output (table mode):
    A table of phases with columns: Phase, Duration, Status, and indentation
    showing nesting within parallel groups. A summary with total wall time is
    printed at the end.

Output (JSON mode):
    A JSON object with "phases" array and "summary" object.
"""

import json
import re
import sys
from datetime import datetime


def strip_ansi(s: str) -> str:
    """Remove ANSI escape sequences."""
    return re.sub(r"\x1b\[[0-9;]*m", "", s)


def parse_pretty_line(line: str):
    """Parse a single pretty-format log line.

    Pretty format examples:
        2026-04-01T23:36:53Z [I] [install-packages] started
        2026-04-01T23:36:59Z [I] [install-packages] completed [status=ok] [duration=6.1s]
        2026-04-01T23:36:59Z [E] [some-task] failed [status=failed] [duration=1s] [error=msg]
    """
    line = strip_ansi(line).strip()
    if not line:
        return None

    # Match: <timestamp> [<level>] [<task>] <message> [key=val]...
    # The task field may contain spaces for parallel(...) groups.
    m = re.match(
        r"^(\S+)\s+\[([IEDW])\]\s+"
        r"\[([^\]]+(?:\([^\)]*\))?[^\]]*)\]\s+"
        r"(started|completed|failed)(.*)",
        line,
    )
    if not m:
        return None

    timestamp = m.group(1)
    task = m.group(3)
    event = m.group(4)
    rest = m.group(5)

    duration = "-"
    status = "-"
    error = None

    dur_m = re.search(r"\[duration=([^\]]+)\]", rest)
    if dur_m:
        duration = dur_m.group(1)

    status_m = re.search(r"\[status=([^\]]+)\]", rest)
    if status_m:
        status = status_m.group(1)

    err_m = re.search(r"\[error=([^\]]+)\]", rest)
    if err_m:
        error = err_m.group(1)

    return {
        "timestamp": timestamp,
        "task": task,
        "event": event,
        "duration": duration,
        "status": status,
        "error": error,
    }


def format_ns_duration(ns: int) -> str:
    """Convert nanoseconds to a human-readable Go-style duration string."""
    if ns < 1_000:
        return f"{ns}ns"
    us = ns / 1_000
    if us < 1_000:
        return f"{us:.3f}\u00b5s"
    ms = us / 1_000
    if ms < 1_000:
        return f"{ms:.3f}ms"
    s = ms / 1_000
    if s < 60:
        return f"{s:.6f}s"
    m = int(s // 60)
    rem = s - m * 60
    return f"{m}m{rem:.6f}s"


def parse_json_line(line: str):
    """Parse a single JSON-format log line."""
    line = line.strip()
    if not line or not line.startswith("{"):
        return None
    try:
        obj = json.loads(line)
    except json.JSONDecodeError:
        return None

    msg = obj.get("msg", "")
    if msg not in ("started", "completed", "failed"):
        return None

    task = obj.get("task", "")
    if not task:
        return None

    duration = "-"
    dur_ns = obj.get("duration")
    if dur_ns is not None:
        duration = format_ns_duration(int(dur_ns))

    return {
        "timestamp": obj.get("time", "-"),
        "task": task,
        "event": msg,
        "duration": duration,
        "status": obj.get("status", "-"),
        "error": obj.get("error"),
    }


def parse_timestamp(ts: str):
    """Parse an ISO 8601 / RFC 3339 timestamp."""
    try:
        return datetime.fromisoformat(ts.replace("Z", "+00:00"))
    except (ValueError, TypeError):
        return None


def compute_wall_time(entries: list):
    """Compute total wall time from first to last entry."""
    timestamps = []
    for e in entries:
        t = parse_timestamp(e["timestamp"])
        if t:
            timestamps.append(t)
    if len(timestamps) < 2:
        return None
    delta = (timestamps[-1] - timestamps[0]).total_seconds()
    if delta < 60:
        return f"{delta:.1f}s"
    m = int(delta // 60)
    s = delta - m * 60
    return f"{m}m{s:.1f}s"


def main():
    if len(sys.argv) < 2 or sys.argv[1] in ("-h", "--help"):
        print(__doc__.strip(), file=sys.stderr)
        sys.exit(1)

    logfile = sys.argv[1]
    output_format = "table"
    for arg in sys.argv[2:]:
        if arg == "--json":
            output_format = "json"
        else:
            print(f"error: unknown option: {arg}", file=sys.stderr)
            sys.exit(1)

    with open(logfile) as f:
        raw_lines = f.readlines()

    # Detect format from the first non-empty line.
    is_json = False
    for line in raw_lines:
        stripped = line.strip()
        if stripped:
            is_json = stripped.startswith("{")
            break

    # Parse all lines.
    entries = []
    for line in raw_lines:
        entry = parse_json_line(line) if is_json else parse_pretty_line(line)
        if entry:
            entries.append(entry)

    if not entries:
        print("error: no phase lines found in log file", file=sys.stderr)
        sys.exit(1)

    # Build display rows from completed/failed events, tracking parallel depth.
    rows = []
    parallel_depth = 0
    ok_count = 0
    fail_count = 0

    for entry in entries:
        is_parallel = entry["task"].startswith("parallel(")

        if entry["event"] == "started":
            if is_parallel:
                parallel_depth += 1
            continue

        # completed or failed
        if is_parallel:
            parallel_depth = max(0, parallel_depth - 1)
            depth = parallel_depth
            row_type = "parallel-group"
        elif parallel_depth > 0:
            depth = parallel_depth
            row_type = "parallel-child"
        else:
            depth = 0
            row_type = "sequential"

        if entry["status"] == "ok":
            ok_count += 1
        elif entry["status"] == "failed":
            fail_count += 1

        rows.append(
            {
                "depth": depth,
                "type": row_type,
                "task": entry["task"],
                "duration": entry["duration"],
                "status": entry["status"],
                "error": entry.get("error"),
            }
        )

    wall_time = compute_wall_time(entries)

    if output_format == "json":
        emit_json(rows, ok_count, fail_count, wall_time)
    else:
        emit_table(rows, ok_count, fail_count, wall_time)


def emit_json(rows, ok_count, fail_count, wall_time):
    phases = []
    for row in rows:
        phase = {
            "phase": row["task"],
            "duration": row["duration"],
            "status": row["status"],
            "type": row["type"],
        }
        if row["error"]:
            phase["error"] = row["error"]
        phases.append(phase)

    summary = {
        "total_phases": len(phases),
        "ok": ok_count,
        "failed": fail_count,
    }
    if wall_time:
        summary["total_wall_time"] = wall_time

    print(json.dumps({"phases": phases, "summary": summary}, indent=2))


def emit_table(rows, ok_count, fail_count, wall_time):
    if not rows:
        print("No phases found.")
        return

    # Compute column widths.
    phase_width = max(len("  " * r["depth"] + r["task"]) for r in rows)
    dur_width = max(len(r["duration"]) for r in rows)
    status_width = max(len(r["status"]) for r in rows)

    phase_width = max(phase_width, 5)
    dur_width = max(dur_width, 8)
    status_width = max(status_width, 6)

    # Header.
    hdr = (
        f"  {'#':>3}  {'Phase':<{phase_width}}"
        f"  {'Duration':>{dur_width}}"
        f"  {'Status':<{status_width}}"
    )
    print(hdr)
    print("  " + "-" * (len(hdr) - 2))

    # Rows.
    for i, row in enumerate(rows, 1):
        indent = "  " * row["depth"]
        phase_str = indent + row["task"]
        line = (
            f"  {i:>3}  {phase_str:<{phase_width}}"
            f"  {row['duration']:>{dur_width}}"
            f"  {row['status']:<{status_width}}"
        )
        if row["error"]:
            line += f"  {row['error']}"
        print(line)

    # Summary.
    print()
    parts = []
    if wall_time:
        parts.append(f"Total wall time: {wall_time}")
    parts.append(f"{len(rows)} phases ({ok_count} ok, {fail_count} failed)")
    print("  " + ", ".join(parts))


if __name__ == "__main__":
    main()
