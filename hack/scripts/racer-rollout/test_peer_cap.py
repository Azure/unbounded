import copy
import unittest

import yaml

from peer_cap import CAP, CM, KEY, NS, TARGETS, guarded_patch, transform, verify_live


def fixture():
    text = "apiVersion: overrides.unbounded-cloud.io/v1alpha1\noverrides:\n"
    for index, name in enumerate(TARGETS):
        env = '- &shared {name: OTHER, value: "yes"}' if index == 0 else '- *shared'
        text += f'''- component: racer
  kind: DaemonSet
  name: {name}
  patch:
    spec:
      template:
        spec:
          containers:
          - name: dataplane
            image: "example@sha256:unchanged" # preserve quotes and comments
            env:
            {env}
'''
    return {"kind": "ConfigMap", "metadata": {"name": CM, "namespace": NS, "uid": "u1", "resourceVersion": "42"},
            "data": {KEY: text, "other.yaml": "overrides: []\n# untouched\n"}}


class PeerCapTests(unittest.TestCase):
    def test_exact_insertions_preserve_aliases_images_and_other_data(self):
        before = fixture()["data"]
        original = copy.deepcopy(before)
        after = transform(before)
        insertion = f'            - name: {CAP}\n              value: "4"\n'
        self.assertEqual(after[KEY].count(insertion), 2)
        self.assertEqual(after[KEY].replace(insertion, ""), before[KEY])
        self.assertEqual(after["other.yaml"], before["other.yaml"])
        self.assertEqual(before, original)
        parsed = yaml.safe_load(after[KEY])["overrides"]
        envs = [e["patch"]["spec"]["template"]["spec"]["containers"][0]["env"] for e in parsed]
        self.assertIs(envs[0][1], envs[1][1])

    def test_fail_closed_on_changed_layout(self):
        before = fixture()["data"]
        cases = [before[KEY].replace(TARGETS[1], "unknown"),
                 before[KEY].replace(TARGETS[1], TARGETS[0]),
                 before[KEY].replace("name: OTHER", f"name: {CAP}"),
                 before[KEY].replace("kind: DaemonSet", "kind: Deployment"),
                 before[KEY].replace("            env:\n            - *shared", "            env: []")]
        for text in cases:
            with self.subTest(text=text), self.assertRaises(ValueError):
                transform({KEY: text})
        with self.assertRaises(ValueError):
            transform(transform(before))

    def test_forward_and_fresh_rollback_guards(self):
        before = fixture()
        after = transform(before["data"])
        patch = guarded_patch(before, after)
        self.assertEqual(patch[:3], [
            {"op": "test", "path": "/metadata/uid", "value": "u1"},
            {"op": "test", "path": "/metadata/resourceVersion", "value": "42"},
            {"op": "test", "path": "/data", "value": before["data"]}])
        self.assertEqual(patch[3], {"op": "replace", "path": "/data/racer-v2.yaml", "value": after[KEY]})
        fresh = copy.deepcopy(before)
        fresh["metadata"]["resourceVersion"] = "99"
        fresh["data"] = after
        rollback = guarded_patch(fresh, before["data"])
        self.assertEqual(rollback[1]["value"], "99")
        self.assertEqual(rollback[2]["value"], after)
        self.assertEqual(rollback[3]["value"], before["data"][KEY])

    def test_effective_cap_guards(self):
        current = fixture()
        config = {"data": {CAP: "16"}}
        workloads = [{"metadata": {"name": e["name"]}, "spec": copy.deepcopy(e["patch"]["spec"])}
                     for e in yaml.safe_load(current["data"][KEY])["overrides"]]
        for ds in workloads:
            ds["spec"]["template"]["spec"]["containers"][0]["envFrom"] = [{"configMapRef": {"name": "racer-dataplane-config"}}]
        verify_live(current, config, workloads, "16")
        rolling_back = copy.deepcopy(current)
        rolling_back["data"] = transform(current["data"])
        partial = copy.deepcopy(workloads)
        partial[0]["spec"]["template"]["spec"]["containers"][0]["env"].insert(0, {"name": CAP, "value": "4"})
        verify_live(rolling_back, config, partial, "4")
        for mutation in ("cap", "image", "envFrom", "duplicate"):
            changed = copy.deepcopy(workloads)
            c = changed[0]["spec"]["template"]["spec"]["containers"][0]
            if mutation == "cap":
                c["env"].append({"name": CAP, "value": "4"})
            elif mutation == "image":
                c["image"] = "different"
            elif mutation == "envFrom":
                c["envFrom"].append({"secretRef": {"name": "unknown"}})
            else:
                changed[0]["spec"]["template"]["spec"]["containers"].append(copy.deepcopy(c))
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                verify_live(current, config, changed, "16")
        with self.assertRaises(ValueError):
            verify_live(current, {"data": {CAP: "2"}}, workloads, "16")
        current["data"]["competing.yaml"] = current["data"][KEY]
        with self.assertRaises(ValueError):
            verify_live(current, config, workloads, "16")


if __name__ == "__main__":
    unittest.main()
