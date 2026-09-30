#!/usr/bin/env python3
"""Prepare ONLY racer-loadgen layer concurrency 4 -> 1 (or its rollback).

No apply operation. Kubernetes operations are GET and server dry-run only.
"""

import argparse
import copy
import datetime
import hashlib
import json
from pathlib import Path
import subprocess
import sys


def stamp():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def prepare(before, source=None):
    if (before.get("apiVersion"), before.get("kind"),
            before["metadata"]["namespace"], before["metadata"]["name"]) != (
            "apps/v1", "DaemonSet", "unbounded-system", "racer-loadgen"):
        raise ValueError("not the active racer-loadgen DaemonSet")
    meta = before["metadata"]
    if not meta.get("uid") or not meta.get("resourceVersion"):
        raise ValueError("UID and resourceVersion required")
    after = copy.deepcopy(before)
    containers = after["spec"]["template"]["spec"]["containers"]
    indices = [i for i, c in enumerate(containers) if c["name"] == "racer-loadgen"]
    if len(indices) != 1:
        raise ValueError("expected exactly one racer-loadgen container")
    index = indices[0]
    args = containers[index]["args"]
    matches = [i for i, arg in enumerate(args)
               if arg.split("=", 1)[0] in ("--layer-concurrency", "-layer-concurrency")]
    if len(matches) != 1 or "--" in args:
        raise ValueError("expected one explicit layer-concurrency flag without separator")
    slot = matches[0]
    old, new = ("1", "4") if source else ("4", "1")
    if "=" in args[slot]:
        flag, value = args[slot].split("=", 1)
        replacement = flag + "=" + new
    else:
        slot += 1
        value = args[slot] if slot < len(args) else None
        replacement = new
    if value != old:
        raise ValueError(f"expected explicit layer-concurrency {old}")
    args[slot] = replacement
    if source:
        expected, _ = prepare(source)
        if meta["uid"] != source["metadata"]["uid"] or before["spec"] != expected["spec"]:
            raise ValueError("rollback requires original UID and exact trial spec")
        if after["spec"] != source["spec"]:
            raise ValueError("rollback does not restore original spec")
    path = f"/spec/template/spec/containers/{index}/args"
    patch = [
        {"op": "test", "path": "/metadata/uid", "value": meta["uid"]},
        {"op": "test", "path": "/metadata/resourceVersion", "value": meta["resourceVersion"]},
        {"op": "test", "path": "/spec", "value": before["spec"]},
        {"op": "test", "path": path,
         "value": before["spec"]["template"]["spec"]["containers"][index]["args"]},
        {"op": "replace", "path": path + f"/{slot}", "value": replacement},
    ]
    return after, patch


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True)
    parser.add_argument("--state", required=True, type=Path)
    parser.add_argument("--rollback-of", type=Path, help="original before.json; requires live trial spec")
    opts = parser.parse_args()
    state = opts.state.resolve()
    if not state.is_relative_to(Path.cwd().resolve() / "tmp"):
        parser.error("state must be a fresh directory under this worktree's ignored tmp/")
    state.mkdir(parents=True, exist_ok=False)

    def save(name, value):
        (state / name).write_text(json.dumps(value, indent=2) + "\n")

    def checkpoint(event, **details):
        record = {"time": stamp(), "event": event, **details}
        with (state / "checkpoint.jsonl").open("a") as stream:
            stream.write(json.dumps(record) + "\n")
        print(json.dumps(record), flush=True)

    def run(args):
        command = ["timeout", "--signal=TERM", "--kill-after=10s", "45s", *args]
        checkpoint("before-command", command=command, error=None)
        result = subprocess.run(command, capture_output=True, text=True, check=False)
        checkpoint("after-command", command=command, returncode=result.returncode,
                   error=result.stderr or None)
        if result.returncode:
            raise RuntimeError(result.stderr or f"command exited {result.returncode}")
        return json.loads(result.stdout)

    checkpoint("before-prepare", deadline_seconds=180, heartbeat_seconds=60,
               next="GET, prepare, server dry-run, compare", error=None)
    try:
        base = ["kubectl", "--context", opts.context, "--request-timeout=35s",
                "-n", "unbounded-system"]
        before = run([*base, "get", "daemonset", "racer-loadgen", "-o", "json"])
        save("before.json", before)
        source = json.loads(opts.rollback_of.read_text()) if opts.rollback_of else None
        after, patch = prepare(before, source)
        save("after.expected.json", after)
        save("patch.json", patch)
        # This is deliberately NOT an executable rollback patch: a real rollback
        # must GET its own resourceVersion after the forward mutation/rollout.
        save("rollback.json", {"original": str(state / "before.json"),
                              "requires": "fresh GET, exact trial spec, new review and dry-run",
                              "restoreSpec": before["spec"]})
        dry = run([*base, "patch", "daemonset", "racer-loadgen", "--type=json",
                   "--patch-file", str(state / "patch.json"), "--dry-run=server", "-o", "json"])
        save("after.server-dry-run.json", dry)
        if dry["spec"] != after["spec"] or dry["metadata"]["uid"] != before["metadata"]["uid"]:
            raise ValueError("server dry-run differs from exact expected spec/UID")
        hashes = {name: hashlib.sha256((state / name).read_bytes()).hexdigest()
                  for name in ("before.json", "after.expected.json", "patch.json",
                               "after.server-dry-run.json", "rollback.json")}
        save("plan.json", {"context": opts.context, "time": stamp(), "sha256": hashes,
                           "mutation": "ONLY one layer-concurrency args element",
                           "applied": False, "serverDryRun": "passed"})
        checkpoint("prepared-not-applied", hashes=hashes, error=None,
                   next="parent review, baseline gates, hash check, guarded apply")
    except Exception as error:
        checkpoint("failed", error=str(error), next="inspect artifacts; do not apply")
        raise


if __name__ == "__main__":
    try:
        main()
    except (ValueError, RuntimeError, OSError, KeyError) as error:
        sys.exit(str(error))
