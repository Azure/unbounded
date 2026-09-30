"""Offline projection and pagination regressions; no Kubernetes connection."""

import copy
import json
import unittest
import urllib.parse
import weakref
from unittest import mock

import contract as c
import watcher as w
from test_adapters import FakeAPI
from test_contract import fixture


class PagedAPI(FakeAPI):
    listing = w.API.listing

    def __init__(self):
        super().__init__()
        self.queries = []

    def request(self, path, patch=None):
        base, _, query = path.partition("?")
        if base not in (w.NODES, w.PODS):
            return super().request(path, patch)
        params = urllib.parse.parse_qs(query)
        self.queries.append(params)
        self.assert_query(params)
        start = int(params.get("continue", [0])[0])
        size = int(params["limit"][0])
        source = self.nodes if base == w.NODES else self.pods
        end = min(start + size, len(source))
        return dict(metadata=dict(resourceVersion="17", **{"continue": str(end) if end < len(source) else ""}),
                    items=copy.deepcopy(source[start:end]))

    @staticmethod
    def assert_query(params):
        assert set(params) <= {"limit", "continue"}
        assert params["limit"] == ["100"]


class InventoryTest(unittest.TestCase):
    def test_projected_snapshot_matches_full_inputs(self):
        api = PagedAPI()
        api.pods.append(dict(metadata=dict(ownerReferences=[])))
        for item in api.nodes + api.pods:
            item["metadata"].update(labels={"arbitrary": "value"}, annotations={"large": "unused"},
                                    managedFields=[{"fieldsV1": {"f:spec": {}}}])
            item.setdefault("status", {}).update(images=[{"names": ["unused"]}], conditions=[{"type": "Ready"}])
            item.setdefault("spec", {}).update(containers=[{"name": "unused"}])
        expected = c.snapshot(api.nodes, api.pods, api.ds, 1, 100)
        self.assertEqual(expected, w.inventory(api, 100))
        nodes, pods = api.listing(w.NODES), api.listing(w.PODS)
        self.assertEqual({"metadata", "status"}, set(nodes[0]))
        self.assertEqual({"name", "uid"}, set(nodes[0]["metadata"]))
        self.assertEqual({"addresses"}, set(nodes[0]["status"]))
        self.assertEqual({"nodeName", "hostNetwork"}, set(pods[0]["spec"]))
        self.assertEqual({"podIP", "podIPs"}, set(pods[0]["status"]))
        self.assertEqual(len(api.pods), len(pods))

    def test_security_rejections_survive_projection(self):
        mutations = {
            "deleting node": lambda a: a.nodes[0]["metadata"].update(deletionTimestamp="now"),
            "missing uid": lambda a: a.nodes[0]["metadata"].pop("uid"),
            "duplicate node": lambda a: a.nodes.append(copy.deepcopy(a.nodes[0])),
            "ambiguous node IP": lambda a: a.nodes[0]["status"]["addresses"].append(
                dict(type="InternalIP", address="10.9.0.1")),
            "stale unlabeled owner": lambda a: a.pods[0]["metadata"]["ownerReferences"][0].update(uid="old"),
            "wrong kind": lambda a: a.pods[0]["metadata"]["ownerReferences"][0].update(kind="ReplicaSet"),
            "wrong api": lambda a: a.pods[0]["metadata"]["ownerReferences"][0].update(apiVersion="v1"),
            "wrong name": lambda a: a.pods[0]["metadata"]["ownerReferences"][0].update(name="other"),
            "extra controller": lambda a: a.pods[0]["metadata"]["ownerReferences"].append(dict(controller=True)),
            "noncontroller": lambda a: a.pods[0]["metadata"]["ownerReferences"][0].update(controller=False),
            "wrong namespace": lambda a: a.pods[0]["metadata"].update(namespace="other"),
            "deleting pod": lambda a: a.pods[0]["metadata"].update(deletionTimestamp="now"),
            "host network": lambda a: a.pods[0]["spec"].update(hostNetwork=True),
            "wrong placement": lambda a: a.pods[0]["spec"].update(nodeName="other"),
            "extra podIP key": lambda a: a.pods[0]["status"]["podIPs"][0].update(extra="reject"),
            "ambiguous podIP": lambda a: a.pods[0]["status"]["podIPs"].append(dict(ip="10.9.0.1")),
            "missing podIP": lambda a: a.pods[0]["status"].pop("podIP"),
            "duplicate pod": lambda a: a.pods.append(copy.deepcopy(a.pods[0])),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                api = PagedAPI()
                mutate(api)
                with self.assertRaises((ValueError, KeyError)):
                    c.snapshot(api.nodes, api.pods, api.ds, 1, 100)
                with self.assertRaises((ValueError, KeyError)):
                    w.cycle(api, "holder", lambda: 100)
                self.assertNotIn("authority", json.loads(api.cm["data"]["state.json"]))

    def test_unlabeled_host_on_deny_node_blocks(self):
        api = PagedAPI()
        host = copy.deepcopy(api.pods[0])
        host["metadata"]["ownerReferences"][0].update(name=c.HOST_DS, uid="dsuid-" + c.HOST_DS)
        api.pods.append(host)
        with self.assertRaisesRegex(ValueError, "host DP on DENY11"):
            w.inventory(api, 100)

    def test_page_released_before_next_request(self):
        class Page(dict):
            pass
        api = w.API("offline")
        previous = None
        calls = 0
        node = fixture()[0][0]

        def request(path):
            nonlocal previous, calls
            if previous is not None:
                self.assertIsNone(previous())
            calls += 1
            page = Page(metadata=dict(resourceVersion="1", **{"continue": "next" if calls == 1 else ""}),
                        items=[copy.deepcopy(node)])
            previous = weakref.ref(page)
            return page

        api.request = request
        self.assertEqual(2, len(api.listing(w.NODES)))
        self.assertIsNone(previous())

    def test_pagination_failures_never_publish(self):
        for mode in ("rv", "empty-rv", "loop", "budget", "failure"):
            with self.subTest(mode=mode):
                api = PagedAPI()
                original = api.request
                count = 0

                def request(path, patch=None):
                    nonlocal count
                    if "?" not in path:
                        return original(path, patch)
                    count += 1
                    if mode == "failure" and count == 2:
                        raise OSError("page unavailable")
                    rv = "2" if mode == "rv" and count == 2 else "1"
                    if mode == "empty-rv":
                        rv = ""
                    token = "same" if mode == "loop" else f"token+/={count}"
                    return dict(metadata=dict(resourceVersion=rv, **{"continue": token}), items=[])

                api.request = request
                with mock.patch.object(w, "LIST_MAX_PAGES", 3):
                    with self.assertRaises((ValueError, OSError)):
                        w.cycle(api, "holder", lambda: 100)
                self.assertNotIn("authority", json.loads(api.cm["data"]["state.json"]))

    def test_ds_recreation_and_second_pass_drift_block(self):
        for mode in ("ds", "pod"):
            with self.subTest(mode=mode):
                api = PagedAPI()
                original = api.request
                reads = 0

                def request(path, patch=None):
                    nonlocal reads
                    if path == w.DS[0]:
                        reads += 1
                        if mode == "ds" and reads == 2:
                            api.ds[0]["metadata"]["uid"] = "replacement"
                        if mode == "pod" and reads == 3:
                            api.pods[0]["metadata"]["uid"] = "replacement"
                    return original(path, patch)

                api.request = request
                with self.assertRaises(ValueError):
                    w.cycle(api, "holder", lambda: 100)
                self.assertNotIn("authority", json.loads(api.cm["data"]["state.json"]))


if __name__ == "__main__":
    unittest.main()
