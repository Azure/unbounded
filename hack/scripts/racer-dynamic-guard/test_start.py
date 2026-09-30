import contextlib
import json
import struct
import unittest
from unittest import mock

import bootstrap
import contract as c
import local
import server
import watcher as w
import render
from test_adapters import FakeAPI


class StartTest(unittest.TestCase):
    def test_renderer_stable_references_and_exact_guard_placement(self):
        nodes = FakeAPI().nodes
        policy = dict(sourceUID="cm", denyNodes=sorted(c.DENY11), monitors=["10.3.0.1"], nodes={
            n["metadata"]["name"]: dict(uid=n["metadata"]["uid"], ip=n["status"]["addresses"][0]["address"]) for n in nodes})
        image = "python@sha256:" + "a" * 64
        result = render.bundle(policy, image, image)
        ds = result["objects"][-1]["spec"]["template"]["spec"]
        terms = ds["affinity"]["nodeAffinity"]["requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        self.assertEqual(1500, len(terms))
        self.assertEqual([
            dict(matchFields=[dict(key="metadata.name", operator="In", values=[name])])
            for name in sorted(policy["nodes"])
        ], terms)
        # Terms are ORed, requirements within each term are ANDed.
        def matches(name):
            return any(all(name in field["values"] for field in term["matchFields"]) for term in terms)
        self.assertTrue(all(matches(name) for name in policy["nodes"]))
        for excluded in ("", "outside-policy", "node-1489"):
            self.assertFalse(matches(excluded), excluded)
        self.assertNotIn("affinity", result["hostDPFragment"])
        self.assertEqual(c.PROGRAM_CM, ds["volumes"][0]["configMap"]["name"])
        self.assertNotIn("annotations", result["objects"][-1]["spec"]["template"]["metadata"])
        self.assertEqual("DirectoryOrCreate", ds["volumes"][3]["hostPath"]["type"])
        security = ds["containers"][0]["securityContext"]
        self.assertEqual(dict(drop=["ALL"], add=["NET_ADMIN", "SYS_CHROOT", "NET_RAW"]),
                         security["capabilities"])
        self.assertFalse(security.get("privileged", False))
        self.assertFalse(security["allowPrivilegeEscalation"])
        self.assertTrue(security["readOnlyRootFilesystem"])
        self.assertEqual(dict(type="RuntimeDefault"), security["seccompProfile"])
        publisher = result["objects"][2]["spec"]["template"]["spec"]["containers"][0]["securityContext"]
        self.assertEqual(dict(drop=["ALL"]), publisher["capabilities"])
        self.assertFalse(publisher.get("privileged", False))
        self.assertFalse(publisher["allowPrivilegeEscalation"])
        for fragment in (result["hostDPFragment"], result["hostDPOverride"]["patch"]["spec"]["template"]["spec"]):
            for container in fragment["containers"] + fragment["initContainers"]:
                self.assertNotIn("securityContext", container)

    def setUp(self):
        self.api = FakeAPI()
        w.cycle(self.api, "leader", lambda: 100)
        self.row = next(n for n in self.api.nodes if n["metadata"]["name"] not in c.DENY11)
        self.node = self.row["metadata"]["name"]
        self.uid = self.row["metadata"]["uid"]
        self.ip = self.row["status"]["addresses"][0]["address"]
        self.policy = dict(sourceUID="cm", denyNodes=sorted(c.DENY11), nodes={self.node: dict(uid=self.uid, ip=self.ip)})

    def test_pins_reject_recreated_source_and_changed_local_ip(self):
        self.assertEqual(self.uid, server.pinned(self.policy, self.api.cm, self.node, self.ip))
        with self.assertRaises(ValueError):
            server.pinned(self.policy, self.api.cm, self.node, "10.9.0.1")
        self.api.cm["metadata"]["uid"] = "replacement"
        with self.assertRaises(ValueError):
            server.pinned(self.policy, self.api.cm, self.node, self.ip)

    def test_start_requires_root_nonce_and_fresh_read(self):
        connection = mock.Mock()
        connection.getsockopt.return_value = struct.pack("3i", 1, 0, 0)
        connection.recv.return_value = json.dumps(dict(nonce="a" * 64, node=self.node)).encode() + b"\n"
        host = mock.Mock(own=self.ip)
        host.tick.return_value = dict(node=self.node, nodeUID=self.uid, bootID="boot", ip=self.ip,
                                      sequence=1, contentDigest="b" * 64, valid_until=160)
        with mock.patch.object(server.time, "time", return_value=110):
            server.respond(connection, self.api, host, self.policy, self.node)
        host.tick.assert_called_once()
        self.assertIn(b'"nonce":"' + b"a" * 64, connection.sendall.call_args.args[0])
        connection.getsockopt.return_value = struct.pack("3i", 1, 65532, 65532)
        with self.assertRaises(ValueError):
            server.respond(connection, self.api, host, self.policy, self.node)

    def test_bootstrap_is_closed_nonflush_and_blocks_listeners(self):
        calls = []
        def run(args, data=None):
            calls.append((args, data))
            if args == ["ipset", "save", "R47_PEERS"]:
                _, sources = w.authority(self.api.cm, 110)
                return "create R47_PEERS hash:ip family inet maxelem 1511\n" + "".join(f"add R47_PEERS {ip}\n" for ip in sources)
            return ""
        host = local.Host(self.ip, ["10.3.0.1"], run=run, clock=lambda: 110)
        host.lock = contextlib.nullcontext
        host.verify_policy = mock.Mock()
        with self.assertRaises(ValueError):
            bootstrap.install(host, self.api.cm, self.policy, self.node, ["tcp"])
        self.assertFalse(any(args[0] == "ipset" for args, _ in calls))
        calls.clear()
        with mock.patch.object(bootstrap, "read_intent", return_value=None), \
                mock.patch.object(bootstrap, "write_intent"), \
                mock.patch.object(bootstrap, "sync_directory"), \
                mock.patch.object(bootstrap.Path, "unlink"), \
                mock.patch.object(bootstrap, "listeners", return_value=[]), \
                mock.patch.object(local, "members", side_effect=[w.authority(self.api.cm, 110)[1], []]):
            bootstrap.install(host, self.api.cm, self.policy, self.node, [])
        transaction = next(data for args, data in calls if args[0] == "iptables-restore")
        self.assertIn("-I INPUT 1", transaction)
        self.assertNotIn("-F", transaction)
        self.assertFalse(any("add R47_FRESH" in (data or "") for _, data in calls))


if __name__ == "__main__":
    unittest.main()
