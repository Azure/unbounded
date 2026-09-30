import copy
import unittest

from layer_concurrency import prepare


def fixture(args):
    return {"apiVersion": "apps/v1", "kind": "DaemonSet",
            "metadata": {"name": "racer-loadgen", "namespace": "unbounded-system",
                         "uid": "original", "resourceVersion": "123"},
            "spec": {"updateStrategy": {"rollingUpdate": {"maxUnavailable": "100%"}},
                     "template": {"spec": {"containers": [
                         {"name": "sidecar", "args": ["unchanged"]},
                         {"name": "racer-loadgen", "image": "pinned", "args": args,
                          "volumeMounts": [{"name": "control"}]}]}}}}


class LayerConcurrencyTest(unittest.TestCase):
    def test_only_one_element_changes_and_guards(self):
        for flag in ("--layer-concurrency=4", "-layer-concurrency=4",
                     "--layer-concurrency", "-layer-concurrency"):
            with self.subTest(flag=flag):
                args = ["--catalog-images=512", flag]
                if "=" not in flag:
                    args.append("4")
                args += ["--verify=true", "--concurrency=6", "--concurrency-file=/control"]
                before = fixture(args)
                saved = copy.deepcopy(before)
                after, patch = prepare(before)
                self.assertEqual(before, saved)
                self.assertEqual([p["op"] for p in patch], ["test"] * 4 + ["replace"])
                self.assertEqual(patch[0]["value"], "original")
                self.assertEqual(patch[1]["value"], "123")
                self.assertEqual(patch[2]["value"], before["spec"])
                self.assertEqual(patch[3]["value"], args)
                slot = int(patch[-1]["path"].split("/")[-1])
                expected = copy.deepcopy(before)
                expected["spec"]["template"]["spec"]["containers"][1]["args"][slot] = (
                    flag.replace("=4", "=1") if "=" in flag else "1")
                self.assertEqual(after, expected)

    def test_rejects_ambiguous_missing_or_wrong_old_value(self):
        for args in ([], ["--layer-concurrency"], ["--layer-concurrency=1"],
                     ["--layer-concurrency=4", "--layer-concurrency", "4"],
                     ["--", "--layer-concurrency=4"], ["--layer-concurrency", "--verify"]):
            with self.subTest(args=args), self.assertRaises(ValueError):
                prepare(fixture(args))

    def test_rollback_uses_fresh_version_and_rejects_drift(self):
        source = fixture(["--layer-concurrency", "4", "--verify=true"])
        live, _ = prepare(source)
        live["metadata"]["resourceVersion"] = "456"
        restored, patch = prepare(live, source)
        self.assertEqual(restored["spec"], source["spec"])
        self.assertEqual(patch[1]["value"], "456")
        for key in ("uid", "spec"):
            drift = copy.deepcopy(live)
            if key == "uid":
                drift["metadata"]["uid"] = "recreated"
            else:
                drift["spec"]["template"]["spec"]["containers"][1]["image"] = "changed"
            with self.subTest(key=key), self.assertRaises(ValueError):
                prepare(drift, source)

    def test_wrong_target_or_missing_identity(self):
        for key in ("name", "namespace", "uid", "resourceVersion"):
            before = fixture(["--layer-concurrency=4"])
            before["metadata"][key] = ""
            with self.subTest(key=key), self.assertRaises(ValueError):
                prepare(before)


if __name__ == "__main__":
    unittest.main()
