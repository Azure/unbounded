import copy
import contextlib
import json
from pathlib import Path
import struct
from types import SimpleNamespace
import unittest
from unittest import mock

import contract as c
import local
import ready
import server
import watcher as w
from test_adapters import FakeAPI


class ReadyTest(unittest.TestCase):
    def test_exact_current_fleet_and_reboot_rejection(self):
        api = FakeAPI()
        w.cycle(api, "a", lambda: 100)
        a, _ = w.authority(api.cm, 110)
        proofs = []
        for node in api.nodes:
            node["status"]["nodeInfo"] = dict(bootID="boot-" + node["metadata"]["uid"])
            proofs.append(dict(node=node["metadata"]["name"], nodeUID=node["metadata"]["uid"],
                ip=node["status"]["addresses"][0]["address"], bootID=node["status"]["nodeInfo"]["bootID"],
                sequence=a["sequence"], contentDigest=a["contentDigest"], valid_until=160, verified=110, sourceVerified=110))
        self.assertEqual(1500, ready.fleet(api.cm, proofs, api.nodes, 111)["verified"])
        for change in (lambda p: p.update(bootID="previous"), lambda p: p.update(contentDigest="old"),
                       lambda p: p.update(verified=50), lambda p: p.update(valid_until=110),
                       lambda p: p.update(sourceVerified=95)):
            broken = copy.deepcopy(proofs)
            change(broken[0])
            with self.assertRaises(ValueError):
                ready.fleet(api.cm, broken, api.nodes, 111)


class StatusTest(unittest.TestCase):
    def setUp(self):
        self.api = FakeAPI()
        w.cycle(self.api, "leader", lambda: 100)
        row = next(n for n in self.api.nodes if n["metadata"]["name"] not in c.DENY11)
        self.node, self.uid = row["metadata"]["name"], row["metadata"]["uid"]
        self.ip = row["status"]["addresses"][0]["address"]
        self.now = 110
        self.policy = dict(sourceUID="cm", denyNodes=sorted(c.DENY11),
                           nodes={self.node: dict(uid=self.uid, ip=self.ip)})
        self.host = local.Host(self.ip, ["10.3.0.1"], clock=lambda: self.now)
        self.host.lock = contextlib.nullcontext
        self.a, self.sources = w.authority(self.api.cm, self.now)
        p = self.host.policy
        rules = [local.admission_rule(self.ip), ["-A", "INPUT", *p["input_jump"]]]
        rules += [["-A", p["chain"], *r] for r in p["ordered_rules"]]
        self.rules = "\n".join(" ".join(row) for row in rules)
        self.fresh = f"create R47_FRESH hash:ip family inet maxelem 1 timeout 60\nadd R47_FRESH {self.ip} timeout 20\n"
        def run(args):
            if args == ["iptables", "-w", "2", "-S"]:
                return self.rules
            if args == ["ipset", "save", "R47_FRESH"]:
                return self.fresh
            self.assertEqual(["ipset", "save", "R47_PEERS"], args)
            return "create R47_PEERS hash:ip family inet maxelem 1511\n" + "".join(f"add R47_PEERS {ip}\n" for ip in self.sources)
        self.host.run = mock.Mock(side_effect=run)
        self.proof = dict(node=self.node, nodeUID=self.uid, ip=self.ip,
                          bootID=Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
                          sequence=self.a["sequence"], contentDigest=self.a["contentDigest"], valid_until=160)
        self.host.tick = mock.Mock(return_value=self.proof)
        for patch in (mock.patch.object(server.os, "stat", return_value=SimpleNamespace(st_ino=0xF0000000)),
                      mock.patch.object(server.time, "monotonic", side_effect=lambda: self.now),
                      mock.patch.object(server.time, "time", side_effect=lambda: self.now)):
            patch.start()
            self.addCleanup(patch.stop)
        self.api.request = mock.Mock(wraps=self.api.request)
        self.cache = {}
        server.reconcile(self.api, self.host, self.policy, self.node, self.cache)
        self.host.tick.reset_mock()

    def status(self):
        return server.status(self.host, self.policy, self.node, self.cache)

    def test_readiness_never_fetches_or_renews_and_checks_live_kernel_each_time(self):
        original = copy.deepcopy(self.cache)
        for now in (110, 120, 125):
            self.now = now
            proof = self.status()
            self.assertEqual(now, proof["verified"])
            self.assertEqual(110, proof["sourceVerified"])
            self.assertEqual(160, proof["valid_until"])
        self.assertEqual(original, self.cache)
        self.assertEqual(1, self.api.request.call_count)
        self.host.tick.assert_not_called()
        self.assertEqual(12, self.host.run.call_count)
        self.now = 125.001
        with self.assertRaisesRegex(ValueError, "observation too old"):
            self.status()

    def test_missed_update_only_accepted_within_bounded_observation_age(self):
        self.api.pods[0]["metadata"]["uid"] = "replacement"
        w.cycle(self.api, "leader", lambda: 120)
        latest, _ = w.authority(self.api.cm, 120)
        self.now = 120
        self.assertNotEqual(latest["contentDigest"], self.status()["contentDigest"])
        self.now = 126
        with self.assertRaises(ValueError):
            self.status()
        # A successful fresh observation immediately replaces the old content.
        self.host.tick.return_value = dict(self.proof, sequence=latest["sequence"], contentDigest=latest["contentDigest"], valid_until=180)
        server.reconcile(self.api, self.host, self.policy, self.node, self.cache)
        self.assertEqual(latest["contentDigest"], self.status()["contentDigest"])

    def test_fresh_read_of_old_authority_never_extends_hard_expiry(self):
        self.now = 150
        server.reconcile(self.api, self.host, self.policy, self.node, self.cache)
        self.assertEqual(160, self.status()["valid_until"])
        self.now = 160  # API age is only 10s; original authority still expires.
        with self.assertRaises(ValueError):
            self.status()

    def test_current_boot_uid_content_source_and_live_kernel_required(self):
        for field, value in (("bootID", "old-boot"), ("nodeUID", "old-uid"),
                             ("contentDigest", "old-content"), ("valid_until", 999)):
            with self.subTest(field=field):
                original = copy.deepcopy(self.cache)
                self.cache["proof"][field] = value
                with self.assertRaises(ValueError):
                    self.status()
                self.cache = original
        self.cache["cm"]["metadata"]["uid"] = "recreated"
        with self.assertRaises(ValueError):
            self.status()
        self.cache["cm"]["metadata"]["uid"] = "cm"
        self.fresh = self.fresh.replace("timeout 20", "timeout 0")
        with self.assertRaises(ValueError):
            self.status()
        self.fresh = self.fresh.replace("timeout 0", "timeout 20")
        self.sources.pop()
        with self.assertRaises(ValueError):
            self.status()
        self.sources.append("10.9.9.9")  # Still 1511, but not the authoritative set.
        with self.assertRaises(ValueError):
            self.status()

    def test_age_checked_after_kernel_and_clock_rollback_rejected(self):
        self.now = 109
        with self.assertRaises(ValueError):
            self.status()
        self.now = 124
        def slow_policy():
            self.now = 126
        self.host.verify_policy = slow_policy
        with self.assertRaises(ValueError):
            self.status()

    def test_failed_fetch_or_restart_cannot_reuse_proof(self):
        self.api.request.side_effect = OSError("API unavailable")
        with self.assertRaises(OSError):
            server.reconcile(self.api, self.host, self.policy, self.node, self.cache)
        self.assertEqual({}, self.cache)
        with self.assertRaises(ValueError):
            self.status()
        # New process has no memory cache, regardless of durable proof.json.
        with mock.patch.object(server.Path, "read_text", return_value=c.canonical(self.proof)):
            with self.assertRaises(ValueError):
                server.status(self.host, self.policy, self.node, {})

    def test_uds_readiness_distinct_from_every_start_fresh_api_and_kernel_tick(self):
        connection = mock.Mock()
        connection.getsockopt.return_value = struct.pack("3i", 1, 0, 0)
        connection.recv.return_value = c.canonical(dict(ready=self.node)).encode() + b"\n"
        server.respond(connection, self.api, self.host, self.policy, self.node, self.cache)
        self.assertEqual(1, self.api.request.call_count)
        self.host.tick.assert_not_called()
        for _ in range(2):  # Every container restart, even with a valid cache.
            connection.recv.return_value = c.canonical(dict(node=self.node, nonce="a" * 64)).encode() + b"\n"
            self.host.tick.return_value = dict(self.proof)
            server.respond(connection, self.api, self.host, self.policy, self.node, self.cache)
        self.assertEqual(3, self.api.request.call_count)
        self.assertEqual(2, self.host.tick.call_count)
        self.assertIn("ip", self.cache["proof"])
        self.api.request.side_effect = OSError("API unavailable")
        with self.assertRaises(OSError):
            server.respond(connection, self.api, self.host, self.policy, self.node, self.cache)
        self.assertEqual({}, self.cache)

    def test_client_uses_authenticated_uds_and_rejects_stale_response(self):
        proof = self.status()
        connection = mock.MagicMock()
        connection.__enter__.return_value = connection
        connection.getsockopt.return_value = struct.pack("3i", 1, 0, 0)
        connection.recv.return_value = c.canonical(proof).encode() + b"\n"
        with mock.patch.object(ready.socket, "socket", return_value=connection), \
                mock.patch.object(ready.Path, "lstat", side_effect=lambda: SimpleNamespace(st_mode=0o140600, st_uid=0)), \
                mock.patch.object(ready.stat, "S_ISDIR", return_value=True):
            self.assertEqual(proof, ready.active(self.host, self.policy, self.node))
            self.assertEqual(dict(ready=self.node), json.loads(connection.sendall.call_args.args[0]))
            self.now = 126
            with self.assertRaises(ValueError):
                ready.active(self.host, self.policy, self.node)
            self.now = 110
            connection.getsockopt.return_value = struct.pack("3i", 1, 65532, 65532)
            with self.assertRaises(ValueError):
                ready.active(self.host, self.policy, self.node)

    def test_poll_schedule_is_ten_seconds_and_spreads_nodes(self):
        times = [server.next_poll_at(f"node-{i}", 100) for i in range(1500)]
        self.assertTrue(all(100 < t <= 110 for t in times))
        self.assertEqual(10, len({int(t) for t in times}))
        for i, t in enumerate(times):
            self.assertAlmostEqual(10, server.next_poll_at(f"node-{i}", t + 0.001) - t)


if __name__ == "__main__":
    unittest.main()
