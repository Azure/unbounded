import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

import yaml

import disable_heap_profiling as profiling
import upgrade


def fixture():
    env = [{"name": "RACER_ROUTING_ALGORITHM", "value": "3"},
           {"name": "RACER_HEAP_PROFILE_ADDR", "value": "$(RACER_POD_IP):6060"},
           {"name": "_RJEM_MALLOC_CONF", "value": "prof:true,prof_active:true,lg_prof_sample:19"},
           {"name": "TMPDIR", "value": "/run/racer-heap"}]
    mount = {"name": "heap-profile-tmp", "mountPath": "/run/racer-heap"}
    entries = []
    for name in sorted(profiling.TARGETS):
        entries.append({"component": "racer", "kind": "DaemonSet", "name": name,
                        "patch": {"spec": {"updateStrategy": {"type": "RollingUpdate", "rollingUpdate": {"maxUnavailable": "10%"}},
                                           "template": {"metadata": {"annotations": {"prometheus.io/scrape": "true"}},
                                                        "spec": {"containers": [{"name": "dataplane", "image": "keep",
                                                                "env": list(env), "args": ["keep"],
                                                                "resources": {"requests": {"cpu": "2"}},
                                                                "volumeMounts": [mount]}],
                                                                 "initContainers": [{"name": "guard", "image": "keep", "volumeMounts": [mount]}],
                                                                 "volumes": [{"name": "heap-profile-tmp", "emptyDir": {"sizeLimit": "128Mi"}}]}}}}})
    return "# keep comment\n" + yaml.safe_dump({"overrides": entries}, sort_keys=False)


def snapshot():
    return {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": profiling.NAME,
            "namespace": upgrade.NS, "uid": "keep-uid", "resourceVersion": "10"},
            "data": {profiling.KEY: fixture(), "unrelated.yaml": "keep exactly\n"}}


class TransformTests(unittest.TestCase):
    def test_shared_entries_removed_and_all_other_bytes_preserved(self):
        text = fixture()
        result = profiling.disable(text)
        expected = json.loads(json.dumps(yaml.safe_load(text)))
        for entry in expected["overrides"]:
            container = entry["patch"]["spec"]["template"]["spec"]["containers"][0]
            container["env"] = [e for e in container["env"] if e["name"] not in (profiling.ADDRESS, "_RJEM_MALLOC_CONF")]
        self.assertEqual(yaml.safe_load(result), expected)
        self.assertEqual(profiling.disable(result), result)
        # Only deletions, no reserialization or anchor renaming.
        import difflib
        for line in difflib.ndiff(text.splitlines(), result.splitlines()):
            self.assertFalse(line.startswith("+ "), line)
        self.assertIn("*id", result)
        self.assertTrue(result.startswith("# keep comment\n"))

    def test_mixed_allocator_tuning_preserved_for_both_alias_consumers(self):
        text = fixture().replace("prof:true,prof_active:true,lg_prof_sample:19", "narenas:2,prof:true,dirty_decay_ms:500,lg_prof_sample:19")
        for name in profiling.ALLOCATORS:
            with self.subTest(name=name):
                result = profiling.disable(text.replace("_RJEM_MALLOC_CONF", name))
                self.assertIn('value: "narenas:2,dirty_decay_ms:500"', result)
                for entry in yaml.safe_load(result)["overrides"]:
                    env = entry["patch"]["spec"]["template"]["spec"]["containers"][0]["env"]
                    self.assertIn({"name": name, "value": "narenas:2,dirty_decay_ms:500"}, env)

    def test_nonprofiling_allocator_bytes_unchanged(self):
        for conf in ("narenas:2,dirty_decay_ms:500", ""):
            text = fixture().replace("prof:true,prof_active:true,lg_prof_sample:19", "'" + conf + "'")
            result = profiling.disable(text)
            self.assertIn("value: '" + conf + "'", result)
            self.assertIn("name: _RJEM_MALLOC_CONF", result)

    def test_unrelated_alias_consumer_rejected(self):
        text = fixture()
        # The third anchor is the shared heap address entry (volume sorts first).
        text += "unrelated: *id003\n"
        with self.assertRaises((ValueError, yaml.YAMLError)):
            profiling.disable(text)

    def test_mixed_allocator_alias_cannot_change_unrelated_consumer(self):
        text = fixture().replace("prof:true,prof_active:true,lg_prof_sample:19", "narenas:2,prof:true")
        text += "unrelated: *id004\n"
        with self.assertRaisesRegex(ValueError, "unrelated"):
            profiling.disable(text)

    def test_ambiguous_layouts_and_empty_env_fail_closed(self):
        text = fixture()
        with self.assertRaisesRegex(ValueError, "block sequence"):
            profiling.disable(yaml.safe_dump(yaml.safe_load(text), default_flow_style=True))
        doc = json.loads(json.dumps(yaml.safe_load(text)))
        for entry in doc["overrides"]:
            entry["patch"]["spec"]["template"]["spec"]["containers"][0]["env"] = [
                {"name": profiling.ADDRESS, "value": ""}]
        with self.assertRaisesRegex(ValueError, "empty env"):
            profiling.disable(yaml.safe_dump(doc))

    def test_wrong_configmap_identity_and_context_fail_closed(self):
        old = snapshot()
        for context in ("other", upgrade.CONTEXT):
            if context == upgrade.CONTEXT:
                old["metadata"]["name"] = "other"
            with self.assertRaisesRegex(ValueError, "identity"):
                profiling.build_plan(old, context)

    def test_invalid_targets_duplicates_envfrom_and_allocator_fail_closed(self):
        doc = yaml.safe_load(fixture())
        cases = []
        missing = copy.deepcopy(doc)
        missing["overrides"].pop()
        cases.append(missing)
        duplicate = copy.deepcopy(doc)
        duplicate["overrides"].append(duplicate["overrides"][0])
        cases.append(duplicate)
        for field, value in (("envFrom", [{"configMapRef": {"name": "unknown"}}]),
                             ("env", [{"name": "X"}, {"name": "X"}]),
                             ("env", [{"name": "MALLOC_CONF", "valueFrom": {"secretKeyRef": {"name": "unknown"}}}]),
                             ("env", [{"name": "MALLOC_CONF", "value": "prof:true,bad"}])):
            invalid = copy.deepcopy(doc)
            invalid["overrides"][0]["patch"]["spec"]["template"]["spec"]["containers"][0][field] = value
            cases.append(invalid)
        for case in cases:
            with self.subTest(case=case), self.assertRaises(ValueError):
                profiling.disable(yaml.safe_dump(case))
        with self.assertRaises(ValueError):
            profiling.disable(fixture() + "overrides: []\n")


class ApplyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=upgrade.PROJECT / "tmp")
        self.addCleanup(self.temp.cleanup)
        self.runner = profiling.ProfilingRunner(self.temp.name, upgrade.CONTEXT)
        self.runner.note = Mock()
        self.old = snapshot()
        self.plan = profiling.build_plan(self.old, upgrade.CONTEXT)
        self.new = copy.deepcopy(self.old)
        self.new["data"][profiling.KEY] = self.plan["patch"][-1]["value"]
        self.write_plan()

    def write_plan(self):
        Path(self.temp.name, "plan.json").write_text(upgrade.encoded(self.plan))
        Path(self.temp.name, "patch.json").write_text(upgrade.encoded(self.plan["patch"]))
        self.approval = upgrade.plan_hash(self.plan)

    def test_guarded_patch_dryrun_apply_and_completed_skip(self):
        self.runner.kubectl = Mock(side_effect=[self.old, self.new, self.new])
        self.runner.apply(self.approval)
        calls = self.runner.kubectl.call_args_list
        self.assertEqual([c.args[0] for c in calls], ["get", "patch", "patch"])
        self.assertIn("--dry-run=server", calls[1].args)
        self.assertNotIn("--dry-run=server", calls[2].args)
        for call in calls[1:]:
            self.assertEqual(call.kwargs["payload"], self.plan["patch"])
            self.assertEqual(call.args[call.args.index("--patch-file") + 1], "/dev/stdin")
        self.assertEqual([p["path"] for p in self.plan["patch"]],
                         ["/metadata/uid", "/metadata/resourceVersion", "/data", "/data/racer-v2.yaml"])
        self.assertEqual(self.new["data"]["unrelated.yaml"], self.old["data"]["unrelated.yaml"])
        self.runner.kubectl = Mock(return_value=self.new)
        self.runner.apply(self.approval)
        self.assertEqual(self.runner.kubectl.call_count, 1)

    def test_plan_get_only_exclusive_artifacts(self):
        for name in ("plan.json", "patch.json"):
            Path(self.temp.name, name).unlink()
        self.runner.kubectl = Mock(return_value=self.old)
        self.runner.plan()
        self.assertEqual(self.runner.kubectl.call_args.args[0], "get")
        self.assertTrue(Path(self.temp.name, "snapshot.json").is_file())
        self.assertTrue(Path(self.temp.name, "plan.diff").is_file())
        with self.assertRaises(FileExistsError):
            self.runner.plan()

    def test_unapproved_tampered_and_patch_mismatch_fail_before_get(self):
        self.runner.kubectl = Mock()
        with self.assertRaisesRegex(ValueError, "reviewed"):
            self.runner.apply("wrong")
        self.plan["patch"][-1]["value"] = "unreviewed"
        self.write_plan()
        with self.assertRaisesRegex(ValueError, "reproducible"):
            self.runner.apply(self.approval)
        self.plan = profiling.build_plan(self.old, upgrade.CONTEXT)
        self.write_plan()
        Path(self.temp.name, "patch.json").write_text("[]")
        with self.assertRaisesRegex(ValueError, "artifact"):
            self.runner.apply(self.approval)
        self.runner.kubectl.assert_not_called()

    def test_drift_dryrun_failure_admission_and_conflict_do_not_retry(self):
        for field in ("uid", "resourceVersion"):
            live = copy.deepcopy(self.old)
            live["metadata"][field] = "changed"
            self.runner.kubectl = Mock(return_value=live)
            with self.assertRaisesRegex(ValueError, "drift"):
                self.runner.apply(self.approval)
            self.assertEqual(self.runner.kubectl.call_count, 1)
        admitted = copy.deepcopy(self.new)
        admitted["data"]["unrelated.yaml"] = "changed"
        for results in ([self.old, RuntimeError("dryrun failure")],
                        [self.old, admitted],
                        [self.old, self.new, RuntimeError("conflict")]):
            self.runner.kubectl = Mock(side_effect=results)
            with self.assertRaises((ValueError, RuntimeError)):
                self.runner.apply(self.approval)
            self.assertEqual(self.runner.kubectl.call_count, len(results))


if __name__ == "__main__":
    unittest.main()
