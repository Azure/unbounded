"""Bounded polling authority writer, stdlib only. No workload/Secret access.

Run each invocation under timeout --signal=TERM --kill-after=10s 300s.
Requires precreated source CM and narrowly scoped RBAC; never creates objects.
"""

import argparse
import http.client
import json
import os
from pathlib import Path
import signal
import ssl
import time
import urllib.parse
import uuid

import contract as c


CM = f"/api/v1/namespaces/{c.NAMESPACE}/configmaps/{c.SOURCE_CM}"
NODES = "/api/v1/nodes"
PODS = f"/api/v1/namespaces/{c.NAMESPACE}/pods"
DS = [f"/apis/apps/v1/namespaces/{c.NAMESPACE}/daemonsets/{name}"
      for name in (c.HOST_DS, c.POD_DS)]
SA = Path("/var/run/secrets/kubernetes.io/serviceaccount")
LIST_PAGE_SIZE = 100
LIST_MAX_PAGES = 500  # Preserve the previous 50,000-object listing capacity.


def fields(obj, names):
    return {name: obj[name] for name in names if name in obj}


def inventory_item(path, item):
    """Retain only snapshot inputs, without filtering identities or owner refs.

    Labels cannot prove complete coverage of stale named DS pods. Keep every Pod
    and every controller reference, including unrelated and malformed owners.
    podIPs stays exact because snapshot rejects extra keys as well as extra IPs.
    """
    meta = fields(item["metadata"], ("name", "uid", "deletionTimestamp"))
    if path == NODES:
        return dict(metadata=meta, status=dict(addresses=[
            fields(a, ("type", "address")) for a in item["status"]["addresses"]]))
    meta.update(fields(item["metadata"], ("namespace",)))
    if "ownerReferences" in item["metadata"]:
        meta["ownerReferences"] = [fields(r, ("controller", "kind", "apiVersion", "name", "uid"))
                                   for r in item["metadata"]["ownerReferences"]]
    result = dict(metadata=meta)
    # Unowned Pods may lack spec/status; do not invent defaults for missing fields.
    for key, names in (("spec", ("nodeName", "hostNetwork")), ("status", ("podIP", "podIPs"))):
        if key in item:
            result[key] = fields(item[key], names)
    return result


class Deadline(Exception):
    pass


def expired(signum, frame):
    raise Deadline("bounded operation deadline")


def terminate(signum, frame):
    raise SystemExit(0)


class API:
    def __init__(self, host=None, port=None, credentials=SA, source_uid=None):
        self.host = host or os.environ["KUBERNETES_SERVICE_HOST"]
        self.port = int(port or os.environ.get("KUBERNETES_SERVICE_PORT_HTTPS", "443"))
        self.credentials = Path(credentials)
        self.source_uid = source_uid

    def request(self, path, patch=None):
        base = path.split("?", 1)[0]
        c.require(base in [CM, NODES, PODS, *DS] and (patch is None or path == CM),
                  "API path/method outside authority boundary")
        # Reopen projected files on every request: token and CA may rotate by rename.
        token = (self.credentials / "token").read_text().strip()
        context = ssl.create_default_context(cafile=str(self.credentials / "ca.crt"))
        conn = http.client.HTTPSConnection(self.host, self.port, timeout=5, context=context)
        headers = {"Authorization": "Bearer " + token, "Accept": "application/json"}
        body = None
        if patch is not None:
            headers["Content-Type"] = "application/json-patch+json"
            body = c.canonical(patch).encode()
        try:
            conn.request("GET" if patch is None else "PATCH", path, body, headers)
            response = conn.getresponse()
            # No redirects, credential logging, retry-on-conflict or Secret calls.
            c.require(response.status == 200, f"API status {response.status}")
            data = response.read(16 * 1024 * 1024 + 1)
            c.require(len(data) <= 16 * 1024 * 1024, "API response exceeds limit")
            result = json.loads(data)
            if base == CM and self.source_uid is not None:
                c.require(result["metadata"]["uid"] == self.source_uid, "source CM recreated")
            return result
        finally:
            conn.close()

    def listing(self, path):
        c.require(path in (NODES, PODS), "not an inventory list")
        items, continuation, version = [], "", None
        seen = set()
        for _ in range(LIST_MAX_PAGES):
            query = urllib.parse.urlencode(dict(limit=LIST_PAGE_SIZE, **({"continue": continuation} if continuation else {})))
            page = self.request(path + "?" + query)
            rv = page["metadata"]["resourceVersion"]
            c.require(rv and (version is None or rv == version), "list snapshot changed")
            version = rv
            items.extend(inventory_item(path, item) for item in page["items"])
            continuation = page["metadata"].get("continue", "")
            # Do not overlap a decoded full page with the next response/JSON parse.
            del page
            if not continuation:
                return items
            c.require(continuation not in seen, "pagination loop")
            seen.add(continuation)
        raise ValueError("pagination budget exceeded")


def state_patch(cm, state):
    c.require(cm["metadata"]["name"] == c.SOURCE_CM
              and cm["metadata"]["namespace"] == c.NAMESPACE
              and not cm.get("immutable", False), "source CM identity/type drift")
    return [dict(op="test", path="/metadata/" + key, value=cm["metadata"][key])
            for key in ("uid", "resourceVersion")] + [
        dict(op="replace", path="/data/state.json", value=c.canonical(state))]


def acquire(api, holder, now):
    cm = api.request(CM)
    state = json.loads(cm["data"]["state.json"])
    lease = state.get("lease", {})
    if lease.get("holder") != holder and lease.get("until", 0) > now:
        return None
    state["lease"] = dict(holder=holder, until=now + 30)
    # Acquiring leadership MUST NOT refresh source validity.
    return api.request(CM, state_patch(cm, state))


def inventory(api, observed):
    before = [api.request(path) for path in DS]
    nodes = api.listing(NODES)
    pods = api.listing(PODS)
    after = [api.request(path) for path in DS]
    c.require([d["metadata"]["uid"] for d in before] ==
              [d["metadata"]["uid"] for d in after], "DS changed during relist")
    return c.snapshot(nodes, pods, after, 1, observed)


def cycle(api, holder, clock=time.time):
    started = int(clock())
    cm = acquire(api, holder, started)
    if cm is None:
        return "follower"
    # Two complete observations reject changing identities without using readiness.
    # Still not a cross-kind API transaction; races after the last read remain.
    first = inventory(api, started)
    second = inventory(api, started)
    c.require(c.content_digest(first["body"]) == c.content_digest(second["body"]),
              "inventory not stable across bounded relists")
    now = int(clock())
    c.require(0 <= now - started <= 20, "relist freshness budget exceeded")
    state = json.loads(cm["data"]["state.json"])
    c.require(state["lease"]["holder"] == holder and state["lease"]["until"] > now,
              "lease expired during inventory")
    old = state.get("authority")
    content = c.content_digest(second["body"])
    sequence = 1 if old is None else old["sequence"] + (old["contentDigest"] != content)
    # Validity begins at collection START, not publication, so slow reads do not
    # turn an old observation into a newly fresh authority heartbeat.
    state["authority"] = dict(sequence=sequence, contentDigest=content,
                              generation=second, valid_until=started + 60)
    state["lease"] = dict(holder=holder, until=now + 30)
    api.request(CM, state_patch(cm, state))  # acquired RV fences all concurrent writers
    return "published"


def authority(cm, now):
    c.require(cm["metadata"]["name"] == c.SOURCE_CM
              and cm["metadata"]["namespace"] == c.NAMESPACE, "wrong authority CM")
    result = json.loads(cm["data"]["state.json"])["authority"]
    generation = result["generation"]
    sources = c.validate(generation, now)
    c.require(type(result["sequence"]) is int and result["sequence"] > 0
              and result["contentDigest"] == c.content_digest(generation["body"])
              and result["valid_until"] == generation["body"]["expires"], "authority envelope drift")
    return result, sources


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seconds", type=int, default=0, help="0: continuous bounded cycles")
    parser.add_argument("--source-uid", required=True)
    args = parser.parse_args()
    c.require(0 <= args.seconds <= 240, "run budget must be 0..240 seconds")
    signal.signal(signal.SIGALRM, expired)
    signal.signal(signal.SIGTERM, terminate)
    api, holder = API(source_uid=args.source_uid), str(uuid.uuid4())
    end = time.monotonic() + args.seconds if args.seconds else float("inf")
    while end - time.monotonic() >= 25:
        signal.alarm(25)
        try:
            print(json.dumps(dict(result=cycle(api, holder))), flush=True)
        except (ValueError, KeyError, OSError, Deadline, http.client.HTTPException):
            # Never emit API response bodies, tokens or exception details.
            print('{"result":"failed-cycle-no-authority-renewal"}', flush=True)
        finally:
            signal.alarm(0)
        time.sleep(min(10, max(0, end - time.monotonic())))


if __name__ == "__main__":
    main()
