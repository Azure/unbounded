import contextlib
import copy
import os
from pathlib import Path
import shlex
import tempfile
import unittest
from unittest import mock

import bootstrap
import contract as c
import local
import watcher as w
from test_adapters import FakeAPI


class Kernel:
    def __init__(self):
        self.sets, self.refs, self.calls = {}, {}, []
        self.rules = ""
        self.fail = None

    def run(self, args, data=None):
        self.calls.append((args, data))
        if args[0] == "iptables":
            return self.rules
        if args[0] == "iptables-restore":
            assert "'!'" not in data, "shell quoting is not restore syntax"
            if self.fail == "transaction":
                raise RuntimeError("injected transaction failure")
            self.rules = "\n".join(shlex.join(["-A", "INPUT", *shlex.split(line)[3:]])
                                   for line in reversed(data.splitlines()) if line.startswith("-I"))
            self.rules += "\n" + "\n".join(line for line in data.splitlines() if line.startswith("-A"))
            return ""
        if args == ["ipset", "list", "-name"]:
            return "\n".join(self.sets)
        if args[:2] == ["ipset", "list"]:
            return f"References: {self.refs.get(args[2], 0)}\n"
        if args[:2] == ["ipset", "save"]:
            return self.sets[args[2]]
        if args[:2] == ["ipset", "destroy"]:
            del self.sets[args[2]]
            return ""
        assert args == ["ipset", "restore"], args
        for line in data.splitlines():
            action, name, *_ = shlex.split(line)
            if action == "create":
                assert name not in self.sets
                self.sets[name] = line + "\n"
            else:
                self.sets[name] += line + "\n"
            if self.fail == name or self.fail == line:
                raise RuntimeError("injected creation failure")
        return ""


class BootstrapTest(unittest.TestCase):
    def setUp(self):
        self.api = FakeAPI()
        w.cycle(self.api, "a", lambda: 100)
        row = next(n for n in self.api.nodes if n["metadata"]["name"] not in c.DENY11)
        self.node, self.uid = row["metadata"]["name"], row["metadata"]["uid"]
        self.ip = row["status"]["addresses"][0]["address"]
        self.policy = dict(sourceUID="cm", denyNodes=sorted(c.DENY11),
                           nodes={self.node: dict(uid=self.uid, ip=self.ip)})
        self.directory = tempfile.TemporaryDirectory(dir=Path(__file__).parent)
        self.addCleanup(self.directory.cleanup)
        self.kernel = Kernel()
        self.host = local.Host(self.ip, ["10.3.0.1"], directory=self.directory.name,
                               run=self.kernel.run, clock=lambda: 110)
        self.host.lock = contextlib.nullcontext
        self.listeners = mock.patch.object(bootstrap, "listeners", return_value=[]).start()
        self.addCleanup(mock.patch.stopall)
        # Unit fakes model root ownership; the private kernel test uses real root.
        original = os.fstat
        def root_stat(fd):
            result = list(original(fd))
            result[4] = 0
            return os.stat_result(result)
        mock.patch.object(bootstrap.os, "fstat", side_effect=root_stat).start()

    def install(self):
        bootstrap.install(self.host, self.api.cm, self.policy, self.node, [])

    def interrupt(self, point):
        self.kernel.fail = point
        with self.assertRaises(RuntimeError):
            self.install()
        self.kernel.fail = None
        self.assertEqual("", self.kernel.rules)
        self.assertTrue((self.host.directory / bootstrap.MARKER).exists())

    def test_each_creation_partial_population_and_transaction_retry(self):
        _, sources = w.authority(self.api.cm, 110)
        for point in ("R47_FRESH", "R47_PEERS", f"add R47_PEERS {sources[5]}", "transaction"):
            with self.subTest(point=point):
                self.kernel = Kernel()
                self.host.run = self.kernel.run
                self.interrupt(point)
                self.install()
                self.host.verify_policy()
                self.assertEqual([], local.members(self.kernel.sets["R47_FRESH"], "R47_FRESH", True))
                self.assertEqual(sources, local.members(self.kernel.sets["R47_PEERS"], "R47_PEERS"))
                self.assertFalse((self.host.directory / bootstrap.MARKER).exists())
                self.assertFalse(any("add R47_FRESH" in (data or "") for _, data in self.kernel.calls))

    def test_foreign_partial_never_mutated(self):
        self.kernel.sets["R47_FRESH"] = "create R47_FRESH hash:ip family inet maxelem 1 timeout 60\n"
        with self.assertRaisesRegex(ValueError, "without ownership"):
            self.install()
        self.assertFalse((self.host.directory / bootstrap.MARKER).exists())
        self.assertFalse(any(args[1] in ("destroy", "restore") for args, _ in self.kernel.calls))

    def test_owned_drift_and_listeners_never_cleaned_or_opened(self):
        self.interrupt("transaction")
        clean = copy.deepcopy(self.kernel.sets)
        marker = self.host.directory / bootstrap.MARKER
        for bad in ("fresh", "type", "source", "reference", "staging", "order", "listener", "mode", "policy"):
            with self.subTest(bad=bad):
                self.kernel.sets = copy.deepcopy(clean)
                self.kernel.refs = {}
                self.listeners.return_value = []
                marker.chmod(0o600)
                policy = copy.deepcopy(self.policy)
                if bad == "fresh":
                    self.kernel.sets["R47_FRESH"] += f"add R47_FRESH {self.ip} timeout 30\n"
                elif bad == "type":
                    self.kernel.sets["R47_PEERS"] = clean["R47_PEERS"].replace("hash:ip", "hash:net")
                elif bad == "source":
                    self.kernel.sets["R47_PEERS"] += "add R47_PEERS 192.0.2.123\n"
                elif bad == "reference":
                    self.kernel.refs["R47_PEERS"] = 1
                elif bad == "staging":
                    self.kernel.sets["R47_DYNAMIC_TMP"] = "foreign"
                elif bad == "order":
                    del self.kernel.sets["R47_FRESH"]
                elif bad == "listener":
                    self.listeners.return_value = ["tcp"]
                elif bad == "mode":
                    marker.chmod(0o644)
                elif bad == "policy":
                    self.policy["allowLegacyTransition"] = True
                before = copy.deepcopy(self.kernel.sets)
                self.kernel.calls.clear()
                with self.assertRaises(ValueError):
                    self.install()
                self.assertEqual(before, self.kernel.sets)
                self.assertFalse(any(args[0] == "iptables-restore" or args[1] == "destroy"
                                     for args, _ in self.kernel.calls))
                self.policy = policy

    def test_marker_publication_failure_leaves_no_sets(self):
        with mock.patch.object(bootstrap.os, "replace", side_effect=OSError("crash before rename")):
            with self.assertRaises(OSError):
                self.install()
        self.assertEqual({}, self.kernel.sets)
        self.assertFalse((self.host.directory / bootstrap.MARKER).exists())
        self.install()

    def test_symlink_marker_rejected(self):
        target = self.host.directory / "foreign"
        target.write_text("untouched")
        (self.host.directory / bootstrap.MARKER).symlink_to(target)
        with self.assertRaises(OSError):
            self.install()
        self.assertEqual("untouched", target.read_text())
        self.assertEqual({}, self.kernel.sets)

    def test_listener_appearing_under_lock_blocks_transaction(self):
        self.listeners.side_effect = [[], ["tcp6"]]
        with self.assertRaisesRegex(ValueError, "listener appeared"):
            self.install()
        self.assertEqual("", self.kernel.rules)

    def test_post_commit_crash_requires_closed_exact_completion(self):
        marker = self.host.directory / bootstrap.MARKER
        original = Path.unlink
        def fail_marker(path, *args, **kwargs):
            if path == marker:
                raise OSError("crash after COMMIT")
            return original(path, *args, **kwargs)
        with mock.patch.object(Path, "unlink", fail_marker):
            with self.assertRaises(OSError):
                self.install()
        self.host.verify_policy()
        rules = self.kernel.rules
        self.kernel.rules = "-A INPUT -j ACCEPT\n" + rules
        with self.assertRaises(ValueError):
            self.install()
        self.assertTrue(marker.exists())
        self.kernel.rules = rules
        self.kernel.sets["R47_FRESH"] += f"add R47_FRESH {self.ip} timeout 30\n"
        with self.assertRaisesRegex(ValueError, "not CLOSED"):
            self.install()
        self.kernel.sets["R47_FRESH"] = self.kernel.sets["R47_FRESH"].splitlines()[0] + "\n"
        self.install()
        self.assertFalse(marker.exists())
        self.assertEqual(rules, self.kernel.rules)

    def test_generation_drift_never_reclaims_owned_sets(self):
        self.interrupt("R47_FRESH")
        self.api.pods[0]["metadata"]["uid"] = "replacement"
        w.cycle(self.api, "a", lambda: 110)
        with self.assertRaisesRegex(ValueError, "generation mismatch"):
            self.install()
        self.assertEqual({"R47_FRESH"}, set(self.kernel.sets))

    def test_cleanup_crash_is_recoverable_in_reverse_order(self):
        self.interrupt("transaction")
        def fail(args, data=None):
            result = self.kernel.run(args, data)
            if args == ["ipset", "destroy", "R47_PEERS"]:
                raise RuntimeError("crash during cleanup")
            return result
        self.host.run = fail
        with self.assertRaises(RuntimeError):
            self.install()
        self.assertEqual({"R47_FRESH"}, set(self.kernel.sets))
        self.host.run = self.kernel.run
        self.install()
        self.host.verify_policy()

    def test_nonroot_marker_rejected(self):
        self.interrupt("R47_FRESH")
        root_stat = bootstrap.os.fstat
        def nonroot(fd):
            result = list(root_stat(fd))
            result[4] = 65532
            return os.stat_result(result)
        with mock.patch.object(bootstrap.os, "fstat", side_effect=nonroot):
            with self.assertRaisesRegex(ValueError, "unsafe bootstrap marker"):
                self.install()
        self.assertEqual({"R47_FRESH"}, set(self.kernel.sets))


if __name__ == "__main__":
    unittest.main()
