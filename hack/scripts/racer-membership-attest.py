#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Read-only membership attestation; never changes load or Kubernetes resources.

Uses authenticated API metadata and one existing net-node host session. No private
identity files or Secrets are read. A successful observation is not a future lease.
"""
import argparse
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
from datetime import datetime, timezone

PREFIX = "racer.unbounded-cloud.io/"
DP = ("racer-dataplane", "racer-dataplane-podnet")
NET = "unbounded-net-node"


def require(ok, message):
    if not ok:
        raise ValueError(message)


def stamp():
    return datetime.now(timezone.utc).isoformat()


def note(message):
    print(stamp(), message, file=sys.stderr, flush=True)


def unique(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON key")
        result[key] = value
    return result


def decode(text):
    return json.loads(text, object_pairs_hook=unique)


def canonical(cluster, members):
    """Go wire canonical.go/types.go order, no HTML escaping or trailing newline."""
    result = []
    for member in sorted(members, key=lambda m: m["node"]):
        require(set(member) <= {"node", "shares", "peer_endpoint", "rails", "alignment_enabled", "site"},
                "unknown member field")
        require(type(member["shares"]) is int and 0 < member["shares"] < 2**32, "invalid shares")
        require(type(member["alignment_enabled"]) is bool, "invalid alignment")
        rails = []
        for rail in sorted(member["rails"], key=lambda r: r["rail"]):
            require(set(rail) <= {"rail", "fabric", "numa_node"}, "unknown rail field")
            row = {"rail": rail["rail"], "fabric": rail["fabric"]}
            if rail.get("numa_node") is not None:
                row["numa_node"] = rail["numa_node"]
            rails.append(row)
        row = {"node": member["node"], "shares": member["shares"],
               "peer_endpoint": member["peer_endpoint"], "rails": rails,
               "alignment_enabled": member["alignment_enabled"]}
        if member.get("site"):
            row["site"] = member["site"]
        result.append(row)
    text = json.dumps({"schema_version": 1, "cluster": cluster, "members": result},
                      ensure_ascii=False, separators=(",", ":"), allow_nan=False)
    return text.replace("\u2028", "\\u2028").replace("\u2029", "\\u2029").encode()


def identity(obj):
    meta = obj["metadata"]
    require(meta.get("uid") and not meta.get("deletionTimestamp"), "missing/deleting identity")
    return meta["uid"]


def authority(marker, version):
    mid, vid = identity(marker), identity(version)
    require(marker["metadata"].get("resourceVersion") and version["metadata"].get("resourceVersion"),
            "authority resourceVersion missing")
    m, v = marker["data"], version["data"]
    require(marker.get("immutable") is True and m["state"] == "consumed"
            and m["version_configmap"] == "racer-version" and m["cluster"] == v["cluster"],
            "installation binding")
    require(re.fullmatch(r"[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}", v["cluster"]), "cluster UUID")
    require(version["metadata"]["annotations"][PREFIX + "installation-uid"] == mid,
            "version installation UID")
    for key in ("sequence", "membership_version"):
        require(re.fullmatch(r"[1-9][0-9]*", v[key]) and int(v[key]) < 2**64, "invalid counter")
    require(int(v["membership_version"]) <= int(v["sequence"]), "counter order")
    for key in ("membership_hash", "content_hash"):
        require(re.fullmatch(r"[0-9a-f]{64}", v[key]), "invalid hash")
    return {"installation_uid": mid, "version_uid": vid, "data": v}


def pod_record(pod, owners, container):
    uid = identity(pod)
    refs = [r for r in pod["metadata"].get("ownerReferences", []) if r.get("controller")]
    require(len(refs) == 1 and refs[0]["kind"] == "DaemonSet"
            and owners.get(refs[0]["name"]) == refs[0]["uid"], "pod owner mismatch")
    specs = [c for c in pod["spec"]["containers"] if c["name"] == container]
    states = [c for c in pod["status"].get("containerStatuses", []) if c["name"] == container]
    require(len(specs) == len(states) == 1, "container missing/ambiguous")
    spec, status = specs[0], states[0]
    require(status.get("state", {}).get("running") and status.get("containerID")
            and status.get("imageID") and status.get("ready"), "container not running/ready")
    ip = str(ipaddress.ip_address(pod["status"]["podIP"]))
    ports = [p["containerPort"] for p in spec.get("ports", []) if p["name"] == "diagnostics"]
    if container == "dataplane":
        require(len(ports) == 1 and 0 < ports[0] < 65536, "diagnostic port")
        peer = [p["containerPort"] for p in spec.get("ports", []) if p["name"] == "peer"]
        require(len(peer) == 1 and 0 < peer[0] < 65536, "peer port")
    else:
        peer = []
    return {"pod": pod["metadata"]["name"], "pod_uid": uid, "node": pod["spec"]["nodeName"],
            "owner_uid": refs[0]["uid"], "ip": ip, "port": ports[0] if ports else 0,
            "peer_endpoint": (f"[{ip}]" if ":" in ip else ip) + ":" + str(peer[0]) if peer else None,
            "container_id": status["containerID"], "image_id": status["imageID"],
            "restart_count": status["restartCount"], "image": spec["image"]}


def inventory(raw, count, image, plan=None):
    auth = authority(raw["marker"], raw["version"])
    members, nodes = [], {}
    for node in raw["nodes"]:
        ann = node["metadata"].get("annotations", {})
        if PREFIX + "last-admitted-member" not in ann:
            continue
        uid = identity(node)
        require(PREFIX + "exclude" not in node["metadata"].get("labels", {}), "excluded admitted node")
        member = decode(ann[PREFIX + "last-admitted-member"])
        name = node["metadata"]["name"]
        require(member["node"] == uid and name not in nodes, "node UID/name mismatch")
        effective = int(ann.get(PREFIX + "shares", ann.get(PREFIX + "enrolled-shares", "4")))
        require(member["shares"] == effective, "desired/admitted shares differ")
        nodes[name] = {"uid": uid, "annotation": ann.get(PREFIX + "shares"), "shares": effective}
        members.append(member)
    require(len(nodes) == count and len({m["node"] for m in members}) == count, "missing/duplicate nodes")
    encoded = canonical(auth["data"]["cluster"], members)
    require(hashlib.sha256(encoded).hexdigest() == auth["data"]["membership_hash"], "canonical hash mismatch")
    if plan is not None:
        rows = plan["nodes"]
        require(len(rows) == count and len({r["node"] for r in rows}) == count, "candidate count")
        expected = {r["node"]: {"uid": r["uid"], "annotation": r["candidate_annotation"],
                               "shares": int(r["candidate_annotation"])} for r in rows}
        require(nodes == expected, "candidate map mismatch")
        pairs = sorted((r["uid"], int(r["candidate_annotation"])) for r in rows)
        require(hashlib.sha256(json.dumps(pairs, separators=(",", ":")).encode()).hexdigest()
                == plan["map_sha256"], "candidate hash mismatch")
    owners = {d["metadata"]["name"]: identity(d) for d in raw["ds"]}
    targets, hosts = [], []
    for pod in raw["pods"]:
        name = pod["metadata"]["labels"].get("app.kubernetes.io/name")
        if name in DP:
            target = pod_record(pod, {k: v for k, v in owners.items() if k in DP}, "dataplane")
            require(target["image"] == image and target["node"] in nodes, "image/node mismatch")
            target["node_uid"] = nodes[target["node"]]["uid"]
            member = next(m for m in members if m["node"] == target["node_uid"])
            require(target["peer_endpoint"] == member["peer_endpoint"], "admitted endpoint mismatch")
            targets.append(target)
        elif name == NET:
            hosts.append(pod_record(pod, {NET: owners.get(NET)}, "node"))
    require(len(targets) == count and {r["node"] for r in targets} == set(nodes), "missing/duplicate dataplanes")
    require(len({(r["ip"], r["port"]) for r in targets}) == count, "duplicate endpoints")
    require(bool(hosts), "no existing host access")
    return {"authority": auth, "nodes": nodes, "owners": owners,
            "targets": sorted(targets, key=lambda r: r["node"]),
            "host": min(hosts, key=lambda r: r["pod"])}


def diagnostic(text, version):
    require(len(text.encode()) <= 1024, "oversized diagnostic")
    fields = unique(part.split("=", 1) for part in text.split())
    require(set(fields) == {"accepted_sequence", "accepted_membership", "accepted_membership_hash",
                           "pending_sequence", "pending_membership", "expected_workers",
                           "matching_workers", "fully_applied"}, "diagnostic fields")
    require(fields["accepted_membership_hash"] == version["membership_hash"], "diagnostic hash mismatch")
    for key in fields.keys() - {"accepted_membership_hash"}:
        require(re.fullmatch(r"0|[1-9][0-9]*", fields[key]), "invalid diagnostic integer")
    require(fields["accepted_sequence"] == version["sequence"]
            and fields["accepted_membership"] == version["membership_version"], "diagnostic version mismatch")
    require(fields["pending_sequence"] == fields["pending_membership"] == "0", "pending publication")
    require(fields["fully_applied"] == "1" and int(fields["expected_workers"]) > 0
            and fields["matching_workers"] == fields["expected_workers"], "workers missing/stale/lagging")
    return fields


def attest(before, after, results):
    require(before == after, "inventory/authority bracket changed")
    expected = {r["pod_uid"] for r in before["targets"]}
    require(len(results) == len(expected) and {r["pod_uid"] for r in results} == expected,
            "missing/duplicate diagnostic result")
    for result in results:
        require(result["exit"] == 0, "endpoint request failed")
        result["diagnostic"] = diagnostic(result.pop("body"), before["authority"]["data"])
    return {"membership_attested": True, "controller_replicas_serving_attested": False,
            "inventory": before, "results": results}


# One host process, 32 independent bounded curls, no file creation or credentials.
REMOTE = r'''
import concurrent.futures,json,subprocess,sys,time
rows=json.load(sys.stdin)
def probe(row):
    ip=row["ip"]; host="["+ip+"]" if ":" in ip else ip
    argv=["timeout","--signal=TERM","--kill-after=10s","4s","curl","--noproxy","*",
          "--connect-timeout","1","--max-time","2","--max-filesize","1024",
          "--fail","--silent","--show-error","http://"+host+":"+str(row["port"])+"/debug/membership"]
    try:
        r=subprocess.run(argv,capture_output=True,text=True,timeout=15)
        return {"pod_uid":row["pod_uid"],"exit":r.returncode,"body":r.stdout[:1025],"observed_unix":time.time()}
    except Exception:
        return {"pod_uid":row["pod_uid"],"exit":1,"body":""}
with concurrent.futures.ThreadPoolExecutor(max_workers=32) as pool:
    futures={pool.submit(probe,r) for r in rows}; done=0
    while futures:
        ready,futures=concurrent.futures.wait(futures,timeout=30,return_when=concurrent.futures.FIRST_COMPLETED)
        for f in ready:
            print(json.dumps(f.result()),flush=True); done+=1
        if not ready: print(json.dumps({"heartbeat":done}),flush=True)
'''


class Reader:
    def __init__(self, args):
        self.args = args
        self.deadline = time.monotonic() + 265

    def command(self, argv, seconds=18, data=None):
        seconds = min(seconds, int(self.deadline - time.monotonic()))
        require(seconds > 0, "operation deadline")
        p = subprocess.Popen(["timeout", "--signal=TERM", "--kill-after=10s", f"{seconds}s", *argv],
                             stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             text=True, start_new_session=True)
        try:
            end = time.monotonic() + seconds + 10
            while True:
                try:
                    out, _ = p.communicate(data, timeout=min(30, max(0.1, end - time.monotonic())))
                    break
                except subprocess.TimeoutExpired:
                    data = None
                    require(time.monotonic() < end, "command cleanup deadline")
                    note("bounded command in progress; awaiting host collection/read")
        except BaseException:
            os.killpg(p.pid, signal.SIGTERM)
            try:
                p.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(p.pid, signal.SIGKILL)
                p.communicate()
            raise
        require(p.returncode == 0, "bounded read failed; stderr suppressed")
        return out

    def kube(self, *args, seconds=18, data=None):
        return self.command(["kubectl", "--context=" + self.args.context,
                             f"--request-timeout={seconds}s", "-n", self.args.namespace, *args], seconds, data)

    def inventory(self):
        def get(*args):
            return decode(self.kube("get", *args, "-o", "json"))
        note("reading authority/inventory bracket")
        marker = get("cm", "racer-installation")
        version = get("cm", "racer-version")
        nodes = get("nodes", "--chunk-size=200")["items"]
        ds = get("ds", "racer-dataplane", "racer-dataplane-podnet", NET)["items"]
        pods = get("pods", "-l", "app.kubernetes.io/name in (racer-dataplane,racer-dataplane-podnet,unbounded-net-node)",
                   "--chunk-size=200")["items"]
        require(authority(marker, version) == authority(get("cm", "racer-installation"), get("cm", "racer-version")),
                "authority changed during inventory")
        return inventory(dict(marker=marker, version=version, nodes=nodes, ds=ds, pods=pods),
                         self.args.expected_count, self.args.expected_image, self.args.plan)


def interrupted(signum, frame):
    raise TimeoutError("interrupted or operation deadline")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--context", required=True)
    parser.add_argument("--expected-image", required=True)
    parser.add_argument("--expected-count", type=int, required=True)
    parser.add_argument("--namespace", default="unbounded-system")
    parser.add_argument("--candidate-plan", type=Path)
    parser.add_argument("--output", type=Path, help="exclusive-create output; defaults to stdout")
    args = parser.parse_args()
    require(bool(args.context) and bool(args.expected_image), "context and image must be explicit/nonempty")
    require(0 < args.expected_count <= 1500, "expected count must be 1..1500 for bounded sweep")
    if args.output:
        require(args.output.parent.is_dir() and not args.output.exists(), "output must be a fresh path")
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(265)  # Leave cleanup headroom within the external 280-second bound.
    args.plan = decode(args.candidate_plan.read_text()) if args.candidate_plan else None
    started = stamp()
    reader = Reader(args)
    before = reader.inventory()
    note("one host session: collecting all endpoints, concurrency=32, curl=2s")
    budget = min(210, int(reader.deadline - time.monotonic()) - 30)
    require(budget > 0, "insufficient collection budget")
    out = reader.kube("exec", "-i", before["host"]["pod"], "-c", "node", "--",
                      "nsenter", "-t", "1", "-m", "-n", "--", "timeout", "--signal=TERM",
                      "--kill-after=10s", f"{max(1, budget - 10)}s", "python3", "-B", "-c", REMOTE,
                      seconds=budget, data=json.dumps(before["targets"]))
    results = [decode(line) for line in out.splitlines()]
    results = [r for r in results if "heartbeat" not in r]
    note(f"collected {len(results)} results; rechecking identity/authority")
    result = attest(before, reader.inventory(), results)
    result.update(started_at=started, finished_at=stamp(), context=args.context)
    text = json.dumps(result, indent=2, allow_nan=False) + "\n"
    if args.output:
        with args.output.open("x") as stream:
            stream.write(text)
    else:
        print(text, end="")
    note("all identified processes matched committed membership; not a future lease")


if __name__ == "__main__":
    try:
        main()
    except (Exception, KeyboardInterrupt):
        # Never echo remote payloads, kubectl credential-plugin errors or raw metadata.
        note("attestation FAILED; no success artifact; inspect rollout/identity/deadline gates")
        sys.exit(1)
