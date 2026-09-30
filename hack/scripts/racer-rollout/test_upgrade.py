import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch

import yaml

import upgrade


SHA = "3235e717a8e11a625600f13e2dd542c07b903f0e"
IMAGES = upgrade.images_for(SHA)


def pod(role):
    return {
        "serviceAccountName": "keep-identity", "hostNetwork": True,
        "containers": [{"name": role, "image": "old", "command": ["/guard-launch/racer-guard-launch"],
                        "args": ["--catalog-images=512", "--verify=true", "--layer-concurrency=4"],
                        "env": [{"name": "RACER_GUARD", "value": "keep"}],
                        "resources": {"requests": {"cpu": "8"}},
                        "volumeMounts": [{"name": "guard-socket", "mountPath": "/keep"}]},
                       {"name": "sidecar", "image": "sidecar:keep"}],
        "initContainers": [{"name": "guard-launch-copy", "image": "guard:keep"}],
        "volumes": [{"name": "guard-socket", "hostPath": {"path": "/keep"}}],
        "affinity": {"nodeAffinity": {"keep": True}},
    }


def overrides():
    entries = []
    for (component, kind, name), role in upgrade.OVERRIDES.items():
        entries.append({"component": component, "kind": kind, "name": name,
                        "addInitContainers": ["guard-launch-copy"],
                        "patch": {"spec": {"updateStrategy": {"type": "OnDelete"},
                                           "template": {"metadata": {"annotations": {"prometheus.io/scrape": "true"}},
                                                        "spec": pod(role)}}}})
    return {"racer.yaml": "# Preserve this comment\n" + yaml.safe_dump({"overrides": entries}, sort_keys=False),
            "net.yaml": "overrides: [{component: net, kind: Deployment}]\n"}


def workload(kind, name, role, namespace=upgrade.NS):
    return {"apiVersion": "apps/v1", "kind": kind,
            "metadata": {"name": name, "namespace": namespace, "uid": name + "-uid", "resourceVersion": "10",
                         "annotations": {"kubectl.kubernetes.io/last-applied-configuration": "keep"}},
            "spec": {"replicas": 3, "selector": {"matchLabels": {"app": name}},
                     "template": {"metadata": {"annotations": {"prometheus.io/scrape": "true"}}, "spec": pod(role)}},
            "status": {"observedGeneration": 1}}


def inventory():
    cm = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "unbounded-component-overrides",
          "namespace": upgrade.NS, "uid": "overrides-uid", "resourceVersion": "10"}, "data": overrides()}
    objects = [cm, workload("Deployment", "unbounded-operator", "controller"),
               workload("Deployment", "racer-controller", "controller"),
               workload("DaemonSet", "racer-dataplane", "dataplane"),
               workload("DaemonSet", "racer-dataplane-podnet", "dataplane"),
               workload("DaemonSet", "gantry", "gantry")]
    for kind, name, ns in (("DaemonSet", "racer-loadgen", upgrade.NS),
                           ("Deployment", "racer-loadgen-client", upgrade.NS),
                           ("DaemonSet", "racer-loadgen", "racer-loadgen")):
        obj = workload(kind, name, "loadgen", ns)
        obj["spec"]["template"]["spec"]["containers"][0]["image"] = upgrade.REPOSITORIES["loadgen"] + ":old"
        objects.append(obj)
    objects.append({"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "racer-config",
                    "namespace": upgrade.NS, "uid": "config-uid", "resourceVersion": "10"},
                    "data": {"RACER_CLUSTER_ID": "keep", "RACER_GUARD": "keep"}})
    return objects


class ImageOnlyTests(unittest.TestCase):
    def test_override_changes_only_images_including_guard_and_scrape_preservation(self):
        old = overrides()
        untouched = copy.deepcopy(old)
        new = upgrade.upgrade_overrides(old, IMAGES)
        self.assertEqual(old, untouched)
        self.assertEqual(new["net.yaml"], old["net.yaml"])
        self.assertTrue(new["racer.yaml"].startswith("# Preserve this comment\n"))
        expected = yaml.safe_load(old["racer.yaml"])
        for entry in expected["overrides"]:
            container = entry["patch"]["spec"]["template"]["spec"]["containers"][0]
            container["image"] = IMAGES[container["name"]]
        self.assertEqual(yaml.safe_load(new["racer.yaml"]), expected)
        self.assertEqual(upgrade.upgrade_overrides(new, IMAGES), new)

    def test_missing_wrong_duplicate_and_ambiguous_overrides_fail(self):
        entries = yaml.safe_load(overrides()["racer.yaml"])["overrides"]
        cases = [[], entries[:-1], entries + [entries[0]]]
        wrong = copy.deepcopy(entries)
        wrong[0]["patch"]["spec"]["template"]["spec"]["containers"][0]["name"] = "racer-controller"
        cases.append(wrong)
        missing_image = copy.deepcopy(entries)
        del missing_image[0]["patch"]["spec"]["template"]["spec"]["containers"][0]["image"]
        cases.append(missing_image)
        for case in cases:
            with self.subTest(case=case), self.assertRaises(ValueError):
                upgrade.upgrade_overrides({"test.yaml": yaml.safe_dump({"overrides": case})}, IMAGES)
        for text in ("overrides: []\noverrides: []", "not a document", "overrides: []\n---\noverrides: []"):
            with self.subTest(text=text), self.assertRaises((ValueError, yaml.YAMLError)):
                upgrade.upgrade_overrides({"test.yaml": text}, IMAGES)

    def test_aliases_cannot_expand_image_edit_to_another_field(self):
        data = overrides()
        data["racer.yaml"] = data["racer.yaml"].replace("image: old", "image: &shared old", 1)
        data["racer.yaml"] += "unrelated: *shared\n"
        with self.assertRaisesRegex(ValueError, "anchors/aliases"):
            upgrade.upgrade_overrides(data, IMAGES)

    def test_workloads_change_only_images_no_reconciliation_or_config_changes(self):
        objects = inventory()
        original = copy.deepcopy(objects)
        changes = upgrade.build_changes(objects, IMAGES)
        self.assertEqual(objects, original)
        self.assertEqual(len(changes), 5)
        for change in changes:
            expected = copy.deepcopy(change["before"])
            if change["phase"] == "apply-overrides":
                expected["data"] = upgrade.upgrade_overrides(expected["data"], IMAGES)
            else:
                role = "operator" if change["phase"] == "apply-operator" else "loadgen"
                expected["spec"]["template"]["spec"]["containers"][0]["image"] = IMAGES[role]
            self.assertEqual(change["after"], expected)

    def test_required_live_layout_and_secret_rejection(self):
        objects = inventory()
        for index in range(6):
            with self.subTest(index=index), self.assertRaisesRegex(ValueError, "missing required"):
                upgrade.build_changes(objects[:index] + objects[index + 1:], IMAGES)
        with self.assertRaisesRegex(ValueError, "missing live loadgen"):
            upgrade.build_changes(objects[:6], IMAGES)
        objects[-1]["kind"] = "Secret"
        with self.assertRaisesRegex(ValueError, "non-secret"):
            upgrade.build_changes(objects, IMAGES)

    def test_image_pins_and_project_boundary(self):
        self.assertEqual(IMAGES["operator"], upgrade.REPOSITORIES["operator"] + ":" + SHA)
        refs = {role: repo + "@sha256:" + "a" * 64 for role, repo in upgrade.REPOSITORIES.items()}
        self.assertEqual(upgrade.images_for(**refs), refs)
        for sha in ("latest", SHA[:8], "A" * 40):
            with self.assertRaises(ValueError):
                upgrade.images_for(sha)
        with self.assertRaises(ValueError):
            upgrade.images_for(**{**refs, "gantry": refs["loadgen"]})
        for path in ("/tmp/upgrade", str(upgrade.PROJECT), str(upgrade.PROJECT / "../outside")):
            with self.assertRaises(ValueError):
                upgrade.state_path(path)
        with self.assertRaises(ValueError):
            upgrade.Runner(upgrade.PROJECT / "tmp/unused", "other-context")


class ApplyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=upgrade.PROJECT / "tmp")
        self.addCleanup(self.temp.cleanup)
        self.runner = upgrade.Runner(self.temp.name, upgrade.CONTEXT)
        self.runner.note = Mock()
        self.objects = inventory()
        self.plan = {"version": 1, "context": upgrade.CONTEXT, "images": IMAGES,
                     "objects": self.objects, "changes": upgrade.build_changes(self.objects, IMAGES)}
        self.write_plan()

    def write_plan(self):
        Path(self.temp.name, "plan.json").write_text(upgrade.encoded(self.plan))
        self.approval = upgrade.plan_hash(self.plan)

    def test_apply_fresh_resource_version_preserves_metadata_and_skips_completed(self):
        live = copy.deepcopy(self.objects[1])
        live["metadata"]["resourceVersion"] = "11"
        live["metadata"]["managedFields"] = [{"manager": "keep"}]
        live["status"]["observedGeneration"] = 2
        self.runner.kubectl = Mock(side_effect=lambda *args, **kwargs: kwargs.get("payload", live))
        self.runner.apply("apply-operator", self.approval)
        calls = self.runner.kubectl.call_args_list
        self.assertEqual(len(calls), 3)
        self.assertIn("--dry-run=server", calls[1].args)
        replacement = calls[2].kwargs["payload"]
        self.assertEqual(replacement["metadata"], live["metadata"])
        self.assertNotIn("status", replacement)
        expected = copy.deepcopy(live)
        expected.pop("status")
        expected["spec"]["template"]["spec"]["containers"][0]["image"] = IMAGES["operator"]
        self.assertEqual(replacement, expected)
        replacement["metadata"]["generation"] = 42
        self.runner.kubectl = Mock(return_value=replacement)
        self.runner.apply("apply-operator", self.approval)
        self.assertEqual(self.runner.kubectl.call_count, 1)

    def test_drift_identity_and_unreviewed_plans_fail_before_write(self):
        for field in ("uid", "annotations"):
            live = copy.deepcopy(self.objects[1])
            live["metadata"][field] = "changed"
            self.runner.kubectl = Mock(return_value=live)
            with self.assertRaisesRegex(ValueError, "drift"):
                self.runner.apply("apply-operator", self.approval)
            self.assertEqual(self.runner.kubectl.call_count, 1)
        self.runner.kubectl = Mock()
        with self.assertRaisesRegex(ValueError, "reviewed plan"):
            self.runner.apply("apply-operator", "bad-hash")
        self.runner.kubectl.assert_not_called()
        self.plan["changes"][1]["after"]["spec"]["replicas"] = 0
        self.write_plan()
        with self.assertRaisesRegex(ValueError, "image-only"):
            self.runner.apply("apply-operator", self.approval)
        self.runner.kubectl.assert_not_called()

    def test_conflict_or_dry_run_failure_not_retried(self):
        admitted = copy.deepcopy(self.plan["changes"][1]["after"])
        for results in ([self.objects[1], RuntimeError("dry-run failed")],
                        [self.objects[1], admitted, RuntimeError("409 conflict")]):
            self.runner.kubectl = Mock(side_effect=results)
            with self.assertRaises(RuntimeError):
                self.runner.apply("apply-operator", self.approval)
            self.assertEqual(self.runner.kubectl.call_count, len(results))

    def test_admission_configuration_mutation_rejected_before_write(self):
        admitted = copy.deepcopy(self.plan["changes"][1]["after"])
        admitted["spec"]["replicas"] = 0
        self.runner.kubectl = Mock(side_effect=[self.objects[1], admitted])
        with self.assertRaisesRegex(ValueError, "server dry-run changed configuration"):
            self.runner.apply("apply-operator", self.approval)
        self.assertEqual(self.runner.kubectl.call_count, 2)

    def test_all_loadgen_drift_checked_before_any_write(self):
        live = copy.deepcopy(self.objects[7])
        live["spec"]["template"]["spec"]["containers"][0]["args"] = ["--catalog-images=1"]
        self.runner.kubectl = Mock(side_effect=[self.objects[6], live])
        with self.assertRaisesRegex(ValueError, "drift"):
            self.runner.apply("apply-loadgen", self.approval)
        self.assertEqual([call.args[0] for call in self.runner.kubectl.call_args_list], ["get", "get"])

    def test_plan_and_snapshot_are_cluster_read_only(self):
        self.runner.kubectl = Mock(return_value={"items": []})
        self.runner.inventory()
        for call in self.runner.kubectl.call_args_list:
            self.assertEqual(call.args[0], "get")
            self.assertNotIn("secrets", call.args[1])
        self.runner.inventory = Mock(return_value=self.objects)
        self.runner.snapshot()
        Path(self.temp.name, "plan.json").unlink()
        with patch("builtins.print"):
            self.runner.plan(IMAGES)
        self.assertTrue(Path(self.temp.name, "plan.diff").is_file())
        self.assertEqual(json.loads(Path(self.temp.name, "plan.json").read_text())["changes"], self.plan["changes"])
        with self.assertRaises(FileExistsError):
            self.runner.snapshot()


if __name__ == "__main__":
    unittest.main()
