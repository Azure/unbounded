"""Real reconcile/tick/status with fake time and an expiring kernel set."""

import copy
import json
import math
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import contract as c
import local
import server
import watcher as w
import test_ready


class RefreshTest(unittest.TestCase):
    prepare_node = test_ready.StatusTest.prepare_node
    status = test_ready.StatusTest.status

    def setUp(self):
        test_ready.StatusTest.setUp(self)
        row = next(n for n in self.api.nodes if n["metadata"]["name"] not in c.DENY11
                   and 144.5 < server.next_poll_at(n["metadata"]["name"], 140) < 145.3)
        self.prepare_node(row)
        directory = tempfile.TemporaryDirectory(dir=Path.cwd())
        self.addCleanup(directory.cleanup)
        self.host.directory = Path(directory.name)
        self.host.tick = local.Host.tick.__get__(self.host)
        self.expires = 0
        self.adds = []
        old_run = self.host.run

        def run(args, data=None):
            if args == ["ipset", "save", "R47_FRESH"]:
                text = "create R47_FRESH hash:ip family inet maxelem 1 timeout 60\n"
                if self.now < self.expires:
                    text += f"add R47_FRESH {self.ip} timeout {math.ceil(self.expires - self.now)}\n"
                return text
            if args[:3] == ["ipset", "add", "R47_FRESH"]:
                self.now += 0.1  # Delayed application preserves the existing 6s margin.
                self.expires = self.now + int(args[5])
                self.adds.append((self.now, int(args[5]), self.expires))
                return ""
            if args == ["ipset", "flush", "R47_FRESH"]:
                self.expires = 0
                return ""
            if args == ["ip", "-j", "-4", "address", "show"]:
                self.now += 0.9  # Checks cross a fractional second before TTL calculation.
                return json.dumps([dict(addr_info=[dict(local=self.ip)])])
            return old_run(args)

        self.host.run = mock.Mock(side_effect=run)
        self.cache = {}
        self.api.request.reset_mock()
        self.now = 144.4
        self.reconcile()
        self.normal = server.next_poll_at(self.node, self.now)
        self.assertEqual(160, self.cache["proof"]["valid_until"])
        self.assertEqual(9, self.adds[-1][1])

    def reconcile(self):
        return server.reconcile(self.api, self.host, self.policy, self.node, self.cache)

    def test_fixed_phase_negative_control_then_deadline_refresh(self):
        old_cache = copy.deepcopy(self.cache)
        expiry = self.expires
        w.cycle(self.api, "leader", lambda: 127)
        self.api.request.reset_mock()
        # Same real reconciliation and kernel: fixed phase misses the deadline,
        # despite the API already holding E=187 and observation age below15s.
        self.now = expiry + 0.01
        self.assertLess(self.now, self.normal)
        self.assertLess(self.now - self.cache["observed"], 15)
        with self.assertRaisesRegex(ValueError, "admission expired"):
            self.status()
        self.api.request.assert_not_called()
        self.assertEqual(old_cache, self.cache)
        # Replay only fake time/kernel to the scheduler's earlier wakeup.
        self.now = min(self.normal, self.cache["refresh_at"])
        self.assertLess(self.now, expiry)
        self.reconcile()
        self.assertLess(self.adds[-1][0], expiry)
        self.assertEqual(187, self.cache["proof"]["valid_until"])
        self.now = expiry + 0.01
        self.assertEqual(187, self.status()["valid_until"])
        self.assertEqual(1, self.api.request.call_count)
        self.assertTrue(all(args.args[0][1] not in ("swap", "restore")
                            for args in self.host.run.call_args_list))

    def test_unchanged_near_expiry_is_bounded_and_never_renews_authority(self):
        polls = 0
        while self.now < 160:
            previous = self.now
            self.now = self.cache["refresh_at"]
            self.assertGreater(self.now, previous)
            polls += 1
            try:
                self.reconcile()
            except ValueError:
                break
            self.assertEqual(160, self.cache["proof"]["valid_until"])
            self.assertLess(self.expires, 160)
        self.assertLessEqual(polls, 5)
        self.assertEqual({}, self.cache)
        self.assertEqual(0, self.expires)
        with self.assertRaises(ValueError):
            self.status()

    def test_missing_api_clears_cache_and_kernel_still_expires(self):
        expiry = self.expires
        self.now = self.cache["refresh_at"]
        self.api.request.side_effect = OSError("unavailable")
        with self.assertRaises(OSError):
            self.reconcile()
        self.assertEqual({}, self.cache)
        with self.assertRaises(ValueError):
            self.status()
        self.now = expiry
        self.assertEqual([], local.members(self.host.run(["ipset", "save", "R47_FRESH"]),
                                          "R47_FRESH", True))
        self.assertLess(expiry, 160)

    def test_normal_cost_and_deadline_fleet_spread(self):
        normal, urgent = [], []
        for i in range(1500):
            node = f"node-{i}"
            normal.append(server.next_refresh_at(node, 100, 100, dict(valid_until=160)))
            self.assertEqual(server.next_poll_at(node, 100), normal[-1])
            urgent.append(server.next_refresh_at(node, 140, 140, dict(valid_until=160)))
        self.assertEqual(10, len({int(t) for t in normal}))
        self.assertGreater(len({int(t) for t in urgent}), 5)
        self.assertTrue(all(140 < t < 154 for t in urgent))


if __name__ == "__main__":
    unittest.main()
