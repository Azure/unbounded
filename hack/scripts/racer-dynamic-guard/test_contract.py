import contextlib
import copy
import unittest

import contract as c


def fixture():
    names = sorted(c.DENY11) + ["node-" + str(i) for i in range(1489)]
    nodes = [dict(metadata=dict(name=name, uid="nodeuid-" + name),
                  status=dict(addresses=[dict(type="InternalIP", address=f"10.1.{i // 250}.{i % 250 + 1}")]))
             for i, name in enumerate(names)]
    ds = [dict(metadata=dict(name=name, uid="dsuid-" + name, namespace=c.NAMESPACE))
          for name in (c.HOST_DS, c.POD_DS)]
    pods = [dict(metadata=dict(name="pod-" + str(i), uid="poduid-" + str(i), namespace=c.NAMESPACE,
                               ownerReferences=[dict(controller=True, kind="DaemonSet", apiVersion="apps/v1",
                                                     name=c.POD_DS, uid="dsuid-" + c.POD_DS)]),
                 spec=dict(nodeName=node, hostNetwork=False),
                 status=dict(podIP=f"10.2.0.{i + 1}", podIPs=[dict(ip=f"10.2.0.{i + 1}")]))
            for i, node in enumerate(sorted(c.DENY11))]
    return nodes, pods, ds


class Fake:
    def __init__(self):
        self.live = []
        self.events = []
        self.sequence = 0
        self.fail = False

    @contextlib.contextmanager
    def lock(self):
        self.events.append("lock")
        try:
            yield
        finally:
            self.events.append("unlock")

    def verify_policy(self, policy):
        self.events.append("policy")
        if self.fail:
            raise ValueError("chain drift")

    def check_sequence(self, sequence, digest):
        c.require(sequence >= self.sequence, "rollback")
        if sequence == self.sequence:
            c.require(digest == self.proof["digest"], "same sequence different generation")

    def members(self):
        return self.live

    def replace(self, sources):
        self.events.append("swap")
        self.live = sources.copy()

    def verify(self, sources, policy):
        c.require(sources == self.live, "set drift")
        self.events.append("verify")

    def boot_id(self):
        return "boot-1"

    def now(self):
        return 110

    def persist(self, proof):
        self.events.append("proof")
        self.proof = proof
        self.sequence = proof["sequence"]


class ContractTest(unittest.TestCase):
    def setUp(self):
        self.nodes, self.pods, self.ds = fixture()
        self.g = c.snapshot(self.nodes, self.pods, self.ds, 1, 100)

    def test_complete_exact_generation_and_readiness_independent(self):
        self.assertEqual(1511, len(c.validate(self.g, 110)))
        self.assertNotIn("conditions", self.pods[0]["status"])
        self.assertEqual(self.g, c.snapshot(list(reversed(self.nodes)), list(reversed(self.pods)), self.ds, 1, 100))

    def test_incomplete_duplicate_cidr_overlap_and_stale(self):
        mutations = [lambda g: g["body"]["nodes"].pop(),
                     lambda g: g["body"]["pods"].append(g["body"]["pods"][0]),
                     lambda g: g["body"]["nodes"][0].update(ip="10.1.0.1/32"),
                     lambda g: g["body"]["pods"][0].update(ip=g["body"]["nodes"][0]["ip"]),
                     lambda g: g["body"]["pods"][0].update(ownerUID="stale"),
                     lambda g: g["body"]["pods"][0].update(nodeUID="stale")]
        for mutate in mutations:
            with self.subTest(mutate=mutate):
                g = copy.deepcopy(self.g)
                mutate(g)
                g["digest"] = c.digest(g["body"])
                with self.assertRaises(ValueError):
                    c.validate(g, 110)
        for now in (99, 160):
            with self.assertRaises(ValueError):
                c.validate(self.g, now)
        self.g["body"]["sequence"] = 2
        with self.assertRaises(ValueError):
            c.validate(self.g, 110)

    def test_wrong_owner_deleting_and_ambiguous_pods_block(self):
        for mutation in (lambda p: p["metadata"]["ownerReferences"][0].update(uid="old-ds"),
                         lambda p: p["metadata"].update(deletionTimestamp="now"),
                         lambda p: p["spec"].update(hostNetwork=True)):
            pods = copy.deepcopy(self.pods)
            mutation(pods[0])
            with self.assertRaises(ValueError):
                c.snapshot(self.nodes, pods, self.ds, 1, 100)
        with self.assertRaises(ValueError):
            c.snapshot(self.nodes, self.pods + [self.pods[0]], self.ds, 1, 100)
        stale = copy.deepcopy(self.pods[0])
        stale["metadata"]["ownerReferences"][0]["uid"] = "previous-ds"
        with self.assertRaises(ValueError):
            c.snapshot(self.nodes, self.pods + [stale], self.ds, 1, 100)

    def test_cas_fences_identity_version_and_lease_only_source_write(self):
        cm = dict(metadata=dict(name=c.SOURCE_CM, namespace=c.NAMESPACE, uid="cmuid", resourceVersion="7"),
                  data={"state.json": c.canonical(dict(lease=dict(holder="writer", until=130)))})
        patch = c.source_cas(cm, self.g, "writer", 110)
        self.assertEqual(["/metadata/uid", "/metadata/resourceVersion", "/data/state.json"],
                         [p["path"] for p in patch])
        with self.assertRaises(ValueError):
            c.source_cas(cm, self.g, "other", 110)
        with self.assertRaises(ValueError):
            c.source_cas(cm, self.g, "writer", 130)

    def test_serial_swap_removes_old_ips_idempotent_and_drift_blocks(self):
        backend = Fake()
        node = self.g["body"]["nodes"][0]
        def reconcile(g):
            return c.reconcile(g, node["name"], node["uid"], node["ip"], ["10.3.0.1"], backend, 110)
        proof = reconcile(self.g)
        self.assertEqual(1511, proof["count"])
        self.assertEqual(["lock", "policy", "swap", "verify", "proof", "unlock"], backend.events)
        reconcile(self.g)
        self.assertEqual(1, backend.events.count("swap"))
        old = self.pods[0]["status"]["podIP"]
        self.pods[0]["status"].update(podIP="10.2.1.1", podIPs=[dict(ip="10.2.1.1")])
        self.pods[0]["metadata"]["uid"] = "replacement"
        replacement = c.snapshot(self.nodes, self.pods, self.ds, 2, 105)
        reconcile(replacement)
        self.assertNotIn(old, backend.live)
        self.assertIn("10.2.1.1", backend.live)
        with self.assertRaises(ValueError):
            reconcile(self.g)
        backend.fail = True
        swaps = backend.events.count("swap")
        with self.assertRaises(ValueError):
            reconcile(replacement)
        self.assertEqual(swaps, backend.events.count("swap"))

    def test_local_prom_exception_is_not_fleet_wide(self):
        local = c.rules("10.224.0.5", ["10.3.0.1"])
        remote = c.rules("10.224.0.6", ["10.3.0.1"])
        self.assertIn("10.240.0.104/32", c.canonical(local))
        self.assertNotIn("10.240.0.104/32", c.canonical(remote))
        self.assertEqual("RACER_STAGE47", local["chain"])
        self.assertIn("R47_PEERS", local["ordered_rules"][0])


if __name__ == "__main__":
    unittest.main()
