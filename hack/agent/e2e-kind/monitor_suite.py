# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Run a suite with periodic runner evidence and a deadline before CI teardown."""

import argparse
import datetime
import os
from pathlib import Path
import signal
import subprocess
import time


def stop_command_tree(process: subprocess.Popen) -> None:
    """Include scenario commands in separate sessions; preserve daemonized VMs."""
    listing = subprocess.run(["ps", "-eo", "pid=,ppid="], capture_output=True, text=True, timeout=10, check=True)
    parents = {int(pid): int(parent) for pid, parent in (line.split() for line in listing.stdout.splitlines())}
    descendants = {process.pid}
    while True:
        expanded = descendants | {pid for pid, parent in parents.items() if parent in descendants}
        if expanded == descendants:
            break
        descendants = expanded
    for pid in descendants:
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    # Descendants may outlive a parent that exits immediately. Give all of them
    # the grace interval before enforcing shutdown, not just the top-level PID.
    time.sleep(5)
    for pid in descendants:
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    process.wait(timeout=10)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timeout", type=int, default=2700)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command and command[0] == "--":
        command = command[1:]
    if not command or args.timeout <= 0:
        parser.error("a positive timeout and command are required")
    logs = Path("logs")
    logs.mkdir(exist_ok=True)
    process = subprocess.Popen(command, start_new_session=True)
    deadline = time.monotonic() + args.timeout
    try:
        with (logs / "runner-resources.log").open("w") as output:
            while process.poll() is None:
                output.write(datetime.datetime.now(datetime.timezone.utc).isoformat() + "\n")
                output.flush()
                for diagnostic in (["free", "-m"], ["df", "-h", "."],
                                   ["ps", "-eo", "pid,ppid,stat,etime,pcpu,rss,wchan:25,args", "--forest"],
                                   ["docker", "stats", "--no-stream"]):
                    try:
                        subprocess.run(diagnostic, stdout=output, stderr=subprocess.STDOUT, timeout=10)
                    except subprocess.TimeoutExpired:
                        output.write("Diagnostic timed out\n")
                output.flush()
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    print("Suite deadline reached; stopping commands and preserving VMs for diagnostics", flush=True)
                    return 124
                try:
                    return process.wait(timeout=min(30, remaining))
                except subprocess.TimeoutExpired:
                    pass
        return process.returncode
    finally:
        if process.poll() is None:
            stop_command_tree(process)


if __name__ == "__main__":
    raise SystemExit(main())
