"""Offline operational prototype. No Kubernetes client or host command execution.

Adapters are deliberately not supplied: this is not an install/rollout tool.
All policy/program references stay stable across source generations.
"""

import copy
import hashlib
import ipaddress
import json


DENY11 = frozenset(
    "aks-ddsv6-84072342-vmss" + suffix
    for suffix in (
        "00000h", "00002i", "00003z", "00005f", "000066", "00006d",
        "000078", "0000b1", "0000bl", "0000cp", "0000d9",
    )
)
HOST_DS = "racer-dataplane"
POD_DS = "racer-dataplane-podnet"
SOURCE_CM = "racer-stage47-sources"
PROGRAM_CM = "racer-stage47-program-v1"
POLICY_CM = "racer-stage47-policy-v1"
NAMESPACE = "unbounded-system"


def require(condition, message):
    if not condition:
        raise ValueError(message)


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)


def digest(value):
    return hashlib.sha256(canonical(value).encode()).hexdigest()


def content_digest(body):
    return digest({k: v for k, v in body.items()
                   if k not in ("sequence", "observed", "expires")})


def ipv4(value):
    require(isinstance(value, str), "IP must be a string")
    result = ipaddress.IPv4Address(value)
    require(str(result) == value and not result.is_unspecified
            and not result.is_multicast and not result.is_loopback,
            "exact unicast IPv4 required")
    return value


def identity(obj):
    meta = obj["metadata"]
    require(meta.get("uid") and meta.get("name") and not meta.get("deletionTimestamp"),
            "missing or deleting identity")
    return meta["name"], meta["uid"]


def validate(generation, now):
    body = generation["body"]
    require(generation["digest"] == digest(body), "generation digest mismatch")
    require(body["schema"] == 1 and type(body["sequence"]) is int
            and body["sequence"] > 0, "invalid generation sequence")
    require(type(body["observed"]) is int and type(body["expires"]) is int
            and body["observed"] <= now < body["expires"]
            and 0 < body["expires"] - body["observed"] <= 60,
            "stale/future authority or excessive validity")
    require(set(body["daemonsets"]) == {HOST_DS, POD_DS}
            and len(set(body["daemonsets"].values())) == 2
            and all(body["daemonsets"].values()), "current two DS identities required")
    nodes, pods = body["nodes"], body["pods"]
    require(len(nodes) == 1500 and len(pods) == 11, "exact 1500+11 required")
    for rows, key in ((nodes, "name"), (nodes, "uid"), (pods, "uid"), (pods, "node")):
        require(all(row[key] for row in rows)
                and len({row[key] for row in rows}) == len(rows), "duplicate/missing identity")
    mapping = {node["name"]: node for node in nodes}
    require({pod["node"] for pod in pods} == DENY11, "fixed DENY11 changed")
    for pod in pods:
        require(pod["node"] in mapping
                and pod["nodeUID"] == mapping[pod["node"]]["uid"]
                and pod["ownerUID"] == body["daemonsets"][POD_DS], "pod ownership drift")
    ips = [ipv4(row["ip"]) for row in nodes + pods]
    require(len(set(ips)) == 1511, "overlapping source addresses")
    return sorted(ips)


def snapshot(nodes, pods, daemonsets, sequence, observed):
    """Consume COMPLETE fresh lists, never a label-filtered or partial watch cache.

    API adapter must reject pagination gaps and recheck both DS UIDs after listing.
    Readiness does not determine membership. Terminating/ambiguous podnet blocks
    publication rather than granting a bridge union or guessing the replacement.
    """
    owners = dict(identity(ds) for ds in daemonsets)
    require(len(daemonsets) == 2 and set(owners) == {HOST_DS, POD_DS}, "wrong DS inventory")
    require(all(ds["metadata"]["namespace"] == NAMESPACE for ds in daemonsets), "wrong namespace")
    node_rows = []
    for node in nodes:
        name, uid = identity(node)
        ips = [a["address"] for a in node["status"]["addresses"] if a["type"] == "InternalIP"]
        require(len(ips) == 1, "ambiguous node IP")
        node_rows.append(dict(name=name, uid=uid, ip=ipv4(ips[0])))
    mapping = {row["name"]: row for row in node_rows}
    pod_rows = []
    for pod in pods:
        refs = [r for r in pod["metadata"].get("ownerReferences", []) if r.get("controller")]
        matched = [r for r in refs if r.get("uid") in owners.values()]
        named = [r for r in refs if r.get("kind") == "DaemonSet" and r.get("name") in owners]
        require(not named or matched, "stale named DS pod in complete inventory")
        if not matched:
            continue
        require(len(refs) == 1 and matched[0]["kind"] == "DaemonSet"
                and matched[0]["apiVersion"] == "apps/v1"
                and matched[0]["name"] in owners
                and owners[matched[0]["name"]] == matched[0]["uid"], "invalid controller")
        require(pod["metadata"]["namespace"] == NAMESPACE, "wrong pod namespace")
        node = pod["spec"].get("nodeName")
        if matched[0]["uid"] == owners[HOST_DS]:
            require(node not in DENY11, "host DP on DENY11")
            continue
        name, uid = identity(pod)
        require(node in DENY11 and node in mapping
                and not pod["spec"].get("hostNetwork", False), "podnet placement drift")
        ip = ipv4(pod["status"]["podIP"])
        require(pod["status"].get("podIPs") == [{"ip": ip}], "ambiguous PodIP")
        pod_rows.append(dict(name=name, uid=uid, node=node, nodeUID=mapping[node]["uid"],
                             ip=ip, ownerUID=owners[POD_DS]))
    body = dict(schema=1, sequence=sequence, observed=observed, expires=observed + 60,
                daemonsets=owners, nodes=sorted(node_rows, key=lambda n: n["name"]),
                pods=sorted(pod_rows, key=lambda p: p["node"]))
    result = dict(body=body, digest=digest(body))
    validate(result, observed)
    return result


def source_cas(cm, generation, holder, now):
    """JSON Patch only, never executes. Leadership shares the single source CM.

    An API adapter acquires/renews its lease by UID/RV CAS before relisting.
    This publication CAS fences expired or replaced holders and concurrent writes.
    """
    require(cm["metadata"]["name"] == SOURCE_CM
            and cm["metadata"]["namespace"] == NAMESPACE
            and not cm.get("immutable", False), "wrong source object")
    state = json.loads(cm["data"]["state.json"])
    lease = state["lease"]
    require(lease["holder"] == holder and now < lease["until"] <= now + 30,
            "leadership absent/expired")
    validate(generation, now)
    previous = state.get("generation")
    require(generation["body"]["sequence"] ==
            (previous["body"]["sequence"] + 1 if previous else 1), "sequence conflict")
    state["generation"] = generation
    return [dict(op="test", path="/metadata/" + key, value=cm["metadata"][key])
            for key in ("uid", "resourceVersion")] + [
        dict(op="replace", path="/data/state.json", value=canonical(state))]


def rules(own, monitors):
    """Fixed chain contract, including the destination-local Prometheus exception."""
    ipv4(own)
    require(1 <= len(monitors) <= 2, "fixed observed monitors required")
    sources = {ipv4(ip) for ip in monitors} | {own}
    if own == "10.224.0.5":
        sources.add("10.240.0.104")
    marker = ["-m", "comment", "--comment", "racer-stage47-owned"]
    chain = [["-p", "tcp", "--dport", "18082", "-m", "set", "--match-set",
              "R47_PEERS", "src", *marker, "-j", "RETURN"]]
    chain += [["-s", ip + "/32", "-p", "tcp", "--dport", "19090", *marker, "-j", "RETURN"]
              for ip in sorted(sources)]
    chain += [["-p", "tcp", *marker, "-j", "REJECT", "--reject-with", "tcp-reset"]]
    jump = ["-d", own + "/32", "-p", "tcp", "-m", "multiport", "--dports",
            "18082,19090", *marker, "-j", "RACER_STAGE47"]
    return dict(chain="RACER_STAGE47", ordered_rules=chain, input_jump=jump)


def reconcile(generation, node, node_uid, own, monitors, backend, now):
    """Proof interface for an existing installed guard; no bootstrap/chain repair.

    backend.lock must be the SAME host-wide bounded flock for refresh, bootstrap,
    verification and start gating (not xtables.lock, which tools acquire themselves).
    replace performs verified staging then ONE atomic ipset swap, never flush/add
    on the live set. verify includes exact type/options/membership and all references.
    Adapters must use TERM/kill10 command deadlines and invalidate proofs on errors.
    """
    sources = validate(generation, now)
    require(any(n == dict(name=node, uid=node_uid, ip=own)
                for n in generation["body"]["nodes"]), "local node identity drift")
    policy = rules(own, monitors)
    with backend.lock():
        validate(generation, backend.now())
        backend.verify_policy(policy)
        # Adapter checks persisted high-water mark plus boot identity. Equality is
        # allowed after swap-before-proof crash; lower sequence is never accepted.
        backend.check_sequence(generation["body"]["sequence"], generation["digest"])
        if backend.members() != sources:
            backend.replace(sources)
        backend.verify(sources, policy)
        verified = backend.now()
        validate(generation, verified)
        proof = dict(node=node, nodeUID=node_uid, ip=own, bootID=backend.boot_id(),
                     sequence=generation["body"]["sequence"], digest=generation["digest"],
                     verified=verified, expires=generation["body"]["expires"], count=1511)
        backend.persist(copy.deepcopy(proof))
        return proof
