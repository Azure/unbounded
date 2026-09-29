import contextlib
import unittest
from unittest import mock

import bootstrap
import contract as c
import local
import render
import watcher as w
from test_adapters import FakeAPI


class Phase4Test(unittest.TestCase):
    def test_term_exits_instead_of_retrying_as_cycle_error(self):
        with self.assertRaises(SystemExit) as exit:
            w.terminate(15, None)
        self.assertEqual(0, exit.exception.code)

    def test_exact_legacy_transition_only_inserts_closed_barrier(self):
        api = FakeAPI()
        w.cycle(api, "a", lambda: 100)
        n = api.nodes[0]
        node, uid = n["metadata"]["name"], n["metadata"]["uid"]
        ip = n["status"]["addresses"][0]["address"]
        policy = dict(sourceUID="cm", denyNodes=sorted(c.DENY11), allowLegacyTransition=True,
                      nodes={node: dict(uid=uid, ip=ip)})
        _, sources = w.authority(api.cm, 110)
        calls = []
        def run(args, data=None):
            calls.append((args, data))
            if args[:3] == ["iptables", "-w", "2"] and args[3] == "-S":
                return "-N RACER_STAGE47\n"
            if args == ["ipset", "save", "R47_PEERS"]:
                return "create R47_PEERS hash:ip family inet maxelem 1511\n" + "".join(f"add R47_PEERS {ip}\n" for ip in sources)
            return "R47_PEERS\n" if args == ["ipset", "list", "-name"] else ""
        host = local.Host(ip, ["10.3.0.1"], run=run, clock=lambda: 110)
        host.lock = contextlib.nullcontext
        host.verify_policy = mock.Mock()
        bootstrap.install(host, api.cm, policy, node, ["existing-authorized-listener"])
        host.verify_policy.assert_any_call(legacy=True)
        writes = [args for args, _ in calls if "-I" in args]
        self.assertEqual(1, len(writes))
        self.assertEqual(["iptables", "-w", "2", "-I", "INPUT", "1"], writes[0][:6])
        self.assertFalse(any("flush" in (data or "") or "swap" in args for args, data in calls))
        calls.clear()
        sources.pop()
        with self.assertRaises(ValueError):
            bootstrap.install(host, api.cm, policy, node, [])
        self.assertFalse(any("-I" in args for args, _ in calls))

    def test_continuous_commands_and_declared_operator_init(self):
        api = FakeAPI()
        policy = dict(sourceUID="cm", denyNodes=sorted(c.DENY11), monitors=["10.3.0.1"], nodes={
            n["metadata"]["name"]: dict(uid=n["metadata"]["uid"], ip=n["status"]["addresses"][0]["address"]) for n in api.nodes})
        image = "ops@sha256:" + "a" * 64
        result = render.bundle(policy, image, image)
        for obj in result["objects"][2:]:
            cmd = obj["spec"]["template"]["spec"]["containers"][0]["command"]
            self.assertEqual("python3", cmd[0])
            self.assertNotIn("--seconds", cmd)
        override = result["hostDPOverride"]
        self.assertEqual(["guard-launch-copy"], override["addInitContainers"])
        self.assertEqual(c.HOST_DS, override["name"])
        fragment = override["patch"]["spec"]["template"]["spec"]
        self.assertNotIn("affinity", fragment)
        self.assertNotIn("args", fragment["containers"][0])


if __name__ == "__main__":
    unittest.main()
