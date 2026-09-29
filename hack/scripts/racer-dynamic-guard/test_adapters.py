import copy
import json
import unittest
from unittest import mock
import contextlib
from types import SimpleNamespace

import contract as c
import local
import watcher as w
from test_contract import fixture


class FakeAPI:
    def __init__(self):
        self.nodes, self.pods, self.ds = fixture()
        self.cm = dict(metadata=dict(name=c.SOURCE_CM, namespace=c.NAMESPACE, uid="cm", resourceVersion="1"),
                       data={"state.json": c.canonical({})})
        self.writes = []

    def request(self, path, patch=None):
        if patch is not None:
            assert path == w.CM
            assert patch[0]["value"] == self.cm["metadata"]["uid"]
            assert patch[1]["value"] == self.cm["metadata"]["resourceVersion"]
            self.cm["data"]["state.json"] = patch[2]["value"]
            self.cm["metadata"]["resourceVersion"] = str(int(self.cm["metadata"]["resourceVersion"]) + 1)
            self.writes.append(copy.deepcopy(patch))
        return copy.deepcopy(self.ds[w.DS.index(path)] if path in w.DS else self.cm)

    def listing(self, path):
        return copy.deepcopy(self.nodes if path == w.NODES else self.pods)


class AdaptersTest(unittest.TestCase):
    def test_projected_token_and_ca_reloaded_each_request(self):
        connection = mock.MagicMock()
        connection.getresponse.return_value.status = 200
        connection.getresponse.return_value.read.return_value = b'{}'
        with mock.patch.object(w.Path, "read_text", side_effect=["token-one", "token-two"]), \
                mock.patch.object(w.ssl, "create_default_context") as ca, \
                mock.patch.object(w.http.client, "HTTPSConnection", return_value=connection):
            api = w.API("api.test", 443)
            api.request(w.CM)
            api.request(w.CM)
        self.assertEqual(2, ca.call_count)
        headers = [call.args[3] for call in connection.request.call_args_list]
        self.assertEqual(["Bearer token-one", "Bearer token-two"], [h["Authorization"] for h in headers])
        self.assertEqual(2, connection.close.call_count)

    def test_expired_authority_closes_before_any_peer_swap(self):
        api = FakeAPI()
        w.cycle(api, "a", lambda: 100)
        host = local.Host("10.1.0.1", ["10.3.0.1"], clock=lambda: 160)
        host.lock = contextlib.nullcontext
        host.verify_policy = mock.Mock()
        host.close_admission = mock.Mock()
        host.replace = mock.Mock()
        with mock.patch.object(local.os, "stat", return_value=SimpleNamespace(st_ino=0xF0000000)):
            with self.assertRaises(ValueError):
                host.tick(api.cm, "node", "uid")
        host.close_admission.assert_called_once()
        host.replace.assert_not_called()

    def test_fixed_chain_and_admission_prefix_exact(self):
        own = "10.224.0.5"
        host = local.Host(own, ["10.3.0.1"])
        p = host.policy
        rows = [local.admission_rule(own), ["-A", "INPUT", *p["input_jump"]]]
        rows += [["-A", p["chain"], *r] for r in p["ordered_rules"]]
        def run(args, data=None):
            if args[0] == "iptables":
                return "\n".join(" ".join(row) for row in rows)
            return "create R47_FRESH hash:ip family inet maxelem 1 timeout 60\n"
        host.run = run
        host.verify_policy()
        rows.insert(0, ["-A", "INPUT", "-j", "ACCEPT"])
        with self.assertRaises(ValueError):
            host.verify_policy()

    def test_staging_reference_blocks_before_mutation(self):
        calls = []
        def run(args, data=None):
            calls.append(args)
            return "-A other -m set --match-set R47_DYNAMIC_TMP src -j ACCEPT"
        host = local.Host("10.1.0.1", ["10.3.0.1"], run=run)
        with self.assertRaises(ValueError):
            host.replace(["10.1.0.2"])
        self.assertEqual(1, len(calls))

    def test_heartbeat_does_not_change_content_sequence(self):
        api = FakeAPI()
        self.assertEqual("published", w.cycle(api, "a", lambda: 100))
        first, sources = w.authority(api.cm, 101)
        self.assertEqual("published", w.cycle(api, "a", lambda: 110))
        second, again = w.authority(api.cm, 111)
        self.assertEqual(first["contentDigest"], second["contentDigest"])
        self.assertEqual(first["sequence"], second["sequence"])
        self.assertGreater(second["valid_until"], first["valid_until"])
        self.assertEqual(sources, again)
        api.pods[0]["metadata"]["uid"] = "replacement"
        w.cycle(api, "a", lambda: 120)
        third, _ = w.authority(api.cm, 121)
        self.assertEqual(second["sequence"] + 1, third["sequence"])

    def test_takeover_and_failed_inventory_never_refresh_authority(self):
        api = FakeAPI()
        w.cycle(api, "a", lambda: 100)
        self.assertEqual("follower", w.cycle(api, "b", lambda: 110))
        api.pods.pop()
        with self.assertRaises(ValueError):
            w.cycle(api, "b", lambda: 131)
        old = json.loads(api.cm["data"]["state.json"])["authority"]
        self.assertEqual(160, old["valid_until"])
        with self.assertRaises(ValueError):
            w.authority(api.cm, 160)

    def test_bounded_inventory_and_content_integrity(self):
        api = FakeAPI()
        clock = iter([100, 121])
        with self.assertRaises(ValueError):
            w.cycle(api, "a", lambda: next(clock))
        self.assertNotIn("authority", json.loads(api.cm["data"]["state.json"]))
        w.cycle(api, "a", lambda: 130)
        state = json.loads(api.cm["data"]["state.json"])
        state["authority"]["contentDigest"] = "forged"
        api.cm["data"]["state.json"] = c.canonical(state)
        with self.assertRaises(ValueError):
            w.authority(api.cm, 131)

    def test_api_boundary_before_credentials_or_network(self):
        api = w.API("never-connect", credentials="/does-not-exist")
        for path, patch in (("/api/v1/secrets", None), (w.NODES, []), (w.DS[0], [])):
            with self.assertRaises(ValueError):
                api.request(path, patch)

    def test_pagination_rejects_inconsistent_snapshot(self):
        api = w.API("never-connect")
        responses = iter([dict(metadata=dict(resourceVersion="1", **{"continue": "next"}), items=[1]),
                          dict(metadata=dict(resourceVersion="2"), items=[2])])
        api.request = lambda path: next(responses)
        with self.assertRaises(ValueError):
            api.listing(w.NODES)

    def test_kernel_parser_rejects_extensions_and_expired_timeout(self):
        text = "create R47_PEERS hash:ip family inet hashsize 2048 maxelem 1511\nadd R47_PEERS 10.1.0.1\n"
        self.assertEqual(["10.1.0.1"], local.members(text, "R47_PEERS"))
        for bad in (text.replace("family", "timeout 60 family"), text + "add R47_PEERS 10.1.0.1\n"):
            with self.assertRaises(ValueError):
                local.members(bad, "R47_PEERS")
        timed = "create R47_FRESH hash:ip family inet maxelem 1 timeout 60\nadd R47_FRESH 10.1.0.1 timeout 0\n"
        with self.assertRaises(ValueError):
            local.members(timed, "R47_FRESH", True)

    def test_swap_only_after_exact_staging_verification(self):
        commands = []
        def run(args, data=None):
            commands.append(args)
            if args[:2] == ["ipset", "save"]:
                name = args[2]
                return f"create {name} hash:ip family inet maxelem 1511\nadd {name} 10.1.0.1\n"
            return ""
        host = local.Host("10.1.0.2", ["10.3.0.1"], run=run)
        host.replace(["10.1.0.1"])
        self.assertEqual(1, sum(args[1] == "swap" for args in commands))
        self.assertNotIn(["ipset", "flush", "R47_PEERS"], commands)
        commands.clear()
        with self.assertRaises(ValueError):
            host.replace(["10.1.0.9"])
        self.assertFalse(any(args[1] == "swap" for args in commands))


if __name__ == "__main__":
    unittest.main()
