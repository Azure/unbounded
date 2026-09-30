"""Bounded offline two-pass RSS comparison, including the released negative control.

Run: timeout --signal=TERM --kill-after=10s 300s python3 -B inventory_memory.py
Synthetic Kubernetes-shaped JSON pages, not captured production data. Each worker
is a fresh process capped at 1536MiB address space and 90 CPU seconds. No network.
"""

import copy
import json
from pathlib import Path
import resource
import subprocess
import sys
import time
import types
import urllib.parse

import contract as c
import watcher
from test_adapters import FakeAPI


RELEASE = "dfb028607a22ae1bde4c01c703640441dc067dfa"


def worker(mode):
    resource.setrlimit(resource.RLIMIT_AS, (1536 * 1024 * 1024,) * 2)
    resource.setrlimit(resource.RLIMIT_CPU, (90, 90))
    w = watcher
    if mode == "released":
        source = subprocess.check_output([
            "timeout", "--signal=TERM", "--kill-after=10s", "30s", "git", "show",
            RELEASE + ":hack/scripts/racer-dynamic-guard/watcher.py"], timeout=40)
        w = types.ModuleType("released_watcher")
        exec(compile(source, "released_watcher.py", "exec"), w.__dict__)

    class InventoryAPI(FakeAPI):
        listing = w.API.listing

        def __init__(self):
            super().__init__()
            self.requests = 0
            self.max_bytes = 0

        def request(self, path, patch=None):
            base, _, query = path.partition("?")
            if base not in (w.NODES, w.PODS):
                return super().request(path, patch)
            self.requests += 1
            params = urllib.parse.parse_qs(query)
            assert set(params) <= {"limit", "continue"}
            start, size = int(params.get("continue", [0])[0]), int(params["limit"][0])
            total = 1500 if base == w.NODES else 9000
            end = min(start + size, total)
            items = []
            for i in range(start, end):
                if base == w.NODES:
                    item = copy.deepcopy(self.nodes[i])
                    item["status"]["images"] = [dict(names=[
                        f"registry.example.test/team/image-{j}@sha256:" + "a" * 64], sizeBytes=100000000)
                        for j in range(80)]
                elif i < 11:
                    item = copy.deepcopy(self.pods[i])
                else:
                    item = dict(metadata=dict(name=f"workload-{i}", uid=f"uid-{i}", namespace=c.NAMESPACE),
                                spec=dict(nodeName=self.nodes[i % 1500]["metadata"]["name"],
                                          containers=[dict(name="main", image="registry.example.test/workload:v1",
                                                           env=[dict(name=f"CONFIG_{j}", value="x" * 64) for j in range(20)])]),
                                status=dict(phase="Running", containerStatuses=[dict(name="main", ready=True)]))
                    if i < 1500:
                        item["spec"]["nodeName"] = self.nodes[i]["metadata"]["name"]
                        item["metadata"]["ownerReferences"] = [dict(
                            controller=True, kind="DaemonSet", apiVersion="apps/v1",
                            name=c.HOST_DS, uid="dsuid-" + c.HOST_DS)]
                item["metadata"]["managedFields"] = [dict(
                    manager="kube-controller-manager", operation="Update", apiVersion="v1",
                    fieldsType="FieldsV1", fieldsV1={"f:spec": {
                        f"f:field-{j}": {".": {}, "f:name": {}, "f:value": {}} for j in range(180)}})]
                item["metadata"]["annotations"] = {"example.test/config": "x" * 1024}
                items.append(item)
            payload = c.canonical(dict(metadata=dict(resourceVersion="17", **{
                "continue": str(end) if end < total else ""}), items=items)).encode()
            # Simulate API.request's bounded bytes and JSON parse, not shared dicts.
            del items, item
            self.max_bytes = max(self.max_bytes, len(payload))
            c.require(len(payload) <= 16 * 1024 * 1024, "fixture exceeds API byte limit")
            return json.loads(payload)

    api = InventoryAPI()
    started = time.monotonic()
    assert w.cycle(api, "memory-fixture", lambda: 100) == "published"
    authority, sources = w.authority(api.cm, 101)
    assert len(sources) == 1511
    print(json.dumps(dict(mode=mode, nodes=1500, pods=9000, passes=2,
                         peak_rss_mib=resource.getrusage(resource.RUSAGE_SELF).ru_maxrss / 1024,
                         elapsed_seconds=round(time.monotonic() - started, 3),
                         list_requests=api.requests, max_page_bytes=api.max_bytes,
                         content_digest=authority["contentDigest"])), flush=True)


def compare():
    results = []
    for mode in ("released", "projected"):
        print(f"running {mode}: deadline120s, address-space1536MiB, CPU90s", flush=True)
        result = subprocess.run([
            "timeout", "--signal=TERM", "--kill-after=10s", "120s",
            sys.executable, "-B", str(Path(__file__).resolve()), "--worker", mode],
            check=True, capture_output=True, text=True, timeout=135)
        row = json.loads(result.stdout)
        results.append(row)
        print(json.dumps(row), flush=True)
    released, projected = results
    assert released["content_digest"] == projected["content_digest"]
    assert released["peak_rss_mib"] > 512, results
    assert projected["peak_rss_mib"] < 256, results
    assert projected["peak_rss_mib"] < released["peak_rss_mib"] / 3, results
    print("PASS: same authority; released >512MiB, projection <256MiB and >3x reduction", flush=True)


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--worker" and sys.argv[2] in ("released", "projected"):
        worker(sys.argv[2])
    else:
        compare()
