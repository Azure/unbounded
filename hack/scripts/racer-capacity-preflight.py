# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Read-only preflight and server dry-run for the eleven stage41 share overrides.

Does not apply changes. Output records include UID/resourceVersion-guarded patches.
Refresh before applying; optimistic-lock failures must not be bypassed.
"""
import argparse
import datetime
import json
from pathlib import Path
import subprocess


def kube(*args):
    command = ["timeout", "--signal=TERM", "--kill-after=10s", "25s", "kubectl",
               "--context=joolshev-scale-test", "--request-timeout=20s", *args]
    result = subprocess.run(command, check=True, capture_output=True, text=True)
    return json.loads(result.stdout)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("forecast", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    forecast = json.loads(args.forecast.read_text())
    nodes = kube("get", "nodes", "-o", "json")["items"]
    by_name = {n["metadata"]["name"]: n for n in nodes}
    cohort = forecast["cohort"]
    annotation = "racer.unbounded-cloud.io/shares"
    pointer = "/metadata/annotations/racer.unbounded-cloud.io~1shares"
    patches = []
    for row in cohort:
        node = by_name[row["node"]]
        meta = node["metadata"]
        assert meta["uid"] == row["uid"]
        assert "racer.unbounded-cloud.io/exclude" not in meta.get("labels", {})
        annotations = meta["annotations"]
        assert annotation not in annotations, "existing override: stop and review"
        assert annotations.get("racer.unbounded-cloud.io/enrolled-shares") == "4"
        admitted = json.loads(annotations["racer.unbounded-cloud.io/last-admitted-member"])
        assert admitted["node"] == row["uid"] and admitted["shares"] == 4
        patch = [{"op": "test", "path": "/metadata/uid", "value": meta["uid"]},
                 {"op": "test", "path": "/metadata/resourceVersion", "value": meta["resourceVersion"]},
                 {"op": "add", "path": pointer, "value": "1"}]
        proposed = kube("patch", "node", row["node"], "--type=json", "-p", json.dumps(patch), "--dry-run=server", "-o", "json")
        assert proposed["metadata"]["annotations"][annotation] == "1"
        patches.append({"node": row["node"], "uid": row["uid"], "previous_override": None,
                        "patch": patch, "server_dry_run": "passed"})
    workloads = kube("-n", "unbounded-system", "get", "ds", "racer-dataplane", "gantry", "racer-loadgen", "-o", "json")["items"]
    output = {"at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "applied": False, "patches": patches, "workloads": [
        {"name": ds["metadata"]["name"], "status": ds["status"],
         "hostNetwork": ds["spec"]["template"]["spec"].get("hostNetwork", False),
         "images": [c["image"] for c in ds["spec"]["template"]["spec"]["containers"]]} for ds in workloads]}
    args.output.write_text(json.dumps(output, indent=2) + "\n")
    print(f"All {len(patches)} UID-bound Node patches passed server admission dry-run; nothing applied.")


if __name__ == "__main__":
    main()
