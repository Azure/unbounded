import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

import yaml

import opaque_relay as relay
import upgrade
from test_disable_heap_profiling import fixture


def snapshot(text=None, name=relay.NAME):
    return {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {
        "namespace": upgrade.NS, "name": name, "uid": name, "resourceVersion": "10"},
        "data": {relay.KEY: fixture() if text is None else text, "other": "keep exactly\n"}}


def defaults():
    obj = snapshot(name=relay.DEFAULTS)
    obj["data"] = {relay.ENV: "false", "RACER_RANGE_WINDOW_PAGES": "1"}
    return obj


class TransformTests(unittest.TestCase):
    def test_insert_preserves_all_existing_bytes_and_aliases(self):
        text = fixture()
        result, originals = relay.enable(text)
        self.assertEqual(originals, dict.fromkeys(relay.TARGETS))
        # Remove only the newly inserted lines: every original byte survives.
        restored = "".join(line for line in result.splitlines(True)
                           if relay.ENV not in line and 'value: "true"' not in line)
        self.assertEqual(restored, text)
        self.assertIn("*id", result)
        for entry in yaml.safe_load(result)["overrides"]:
            env = entry["patch"]["spec"]["template"]["spec"]["containers"][0]["env"]
            self.assertEqual(env[0], {"name": relay.ENV, "value": "true"})

    def test_replace_shared_env_entry_only(self):
        text = fixture().replace("RACER_HEAP_PROFILE_ADDR", relay.ENV).replace("$(RACER_POD_IP):6060", "'false'")
        result, originals = relay.enable(text)
        self.assertEqual(set(originals.values()), {"false"})
        self.assertEqual(result.replace('"true"', "'false'"), text)

    def test_unrelated_alias_consumer_fails_closed(self):
        text = fixture().replace("RACER_HEAP_PROFILE_ADDR", relay.ENV).replace("$(RACER_POD_IP):6060", "'false'")
        with self.assertRaisesRegex(ValueError, "unrelated"):
            relay.enable(text + "unrelated: *id003\n")

    def test_shared_env_list_and_unrelated_alias(self):
        doc = yaml.safe_load(fixture())
        a, b = [e["patch"]["spec"]["template"]["spec"]["containers"][0] for e in doc["overrides"]]
        b["env"] = a["env"]
        result, _ = relay.enable(yaml.safe_dump(doc))
        self.assertEqual(result.count("name: " + relay.ENV), 1)
        doc["unrelated"] = a["env"]
        with self.assertRaisesRegex(ValueError, "unrelated"):
            relay.enable(yaml.safe_dump(doc))

    def test_invalid_values_layout_duplicates_targets(self):
        doc = json.loads(json.dumps(yaml.safe_load(fixture())))
        cases = []
        for variable in ({"name": relay.ENV, "value": True},
                         {"name": relay.ENV, "value": "true"},
                         {"name": relay.ENV, "valueFrom": {}},
                         {"name": relay.ENV, "value": "false", "extra": 1}):
            case = copy.deepcopy(doc)
            case["overrides"][0]["patch"]["spec"]["template"]["spec"]["containers"][0]["env"].append(variable)
            cases.append(case)
        case = copy.deepcopy(doc)
        case["overrides"].pop()
        cases.append(case)
        case = copy.deepcopy(doc)
        case["overrides"].append(case["overrides"][0])
        cases.append(case)
        for field, value in (("env", []), ("env", [{"name": "X"}, {"name": "X"}]),
                             ("envFrom", [{"configMapRef": {"name": "unknown"}}])):
            case = copy.deepcopy(doc)
            case["overrides"][0]["patch"]["spec"]["template"]["spec"]["containers"][0][field] = value
            cases.append(case)
        for case in cases:
            with self.subTest(case=case), self.assertRaises(ValueError):
                relay.enable(yaml.safe_dump(case))
        with self.assertRaises(ValueError):
            relay.enable(yaml.safe_dump(doc, default_flow_style=True))
        with self.assertRaises(ValueError):
            relay.enable(fixture() + "overrides: []\n")

    def test_scalar_anchor_rejected(self):
        text = fixture().replace("RACER_HEAP_PROFILE_ADDR", relay.ENV).replace("$(RACER_POD_IP):6060", "&flag 'false'")
        with self.assertRaisesRegex(ValueError, "aliased"):
            relay.enable(text)


class PlanTests(unittest.TestCase):
    def setUp(self):
        self.old, self.defaults = snapshot(), defaults()
        self.plan = relay.build_plan(self.old, self.defaults, upgrade.CONTEXT)
        self.new = copy.deepcopy(self.old)
        self.new["data"][relay.KEY] = self.plan["patch"][-1]["value"]
        self.new["metadata"]["resourceVersion"] = "11"
        self.temp = tempfile.TemporaryDirectory(dir=upgrade.PROJECT / "tmp")
        self.addCleanup(self.temp.cleanup)
        self.runner = relay.RelayRunner(self.temp.name, upgrade.CONTEXT)
        self.runner.note = Mock()
        self.write_plan()

    def write_plan(self):
        Path(self.temp.name, "plan.json").write_text(upgrade.encoded(self.plan))
        Path(self.temp.name, "patch.json").write_text(upgrade.encoded(self.plan["patch"]))
        self.hash = upgrade.plan_hash(self.plan)

    def test_guards_and_dryrun_only(self):
        self.assertEqual([p["path"] for p in self.plan["patch"]],
                         ["/metadata/uid", "/metadata/resourceVersion", "/data", "/data/racer-v2.yaml"])
        self.runner.kubectl = Mock(side_effect=[self.defaults, self.old, self.new])
        self.runner.apply(self.hash, dry_run=True)
        calls = self.runner.kubectl.call_args_list
        self.assertEqual([c.args[0] for c in calls], ["get", "get", "patch"])
        self.assertIn("--dry-run=server", calls[-1].args)
        self.assertEqual(calls[-1].kwargs["payload"], self.plan["patch"])

    def test_apply_and_idempotent_skip(self):
        self.runner.kubectl = Mock(side_effect=[self.defaults, self.old, self.new, self.new])
        self.runner.apply(self.hash)
        self.assertNotIn("--dry-run=server", self.runner.kubectl.call_args.args)
        self.runner.kubectl = Mock(side_effect=[self.defaults, self.new])
        self.runner.apply(self.hash)
        self.assertEqual(self.runner.kubectl.call_count, 2)

    def test_exact_rollback_with_fresh_rv(self):
        rollback = relay.build_plan(self.new, self.defaults, upgrade.CONTEXT, self.plan)
        self.assertEqual(rollback["patch"][-1]["value"], self.old["data"][relay.KEY])
        self.assertEqual(rollback["patch"][1]["value"], "11")
        self.plan = rollback
        self.write_plan()
        self.runner.kubectl = Mock(side_effect=[self.defaults, self.new, self.old, self.old])
        self.runner.apply(self.hash)
        changed = copy.deepcopy(self.new)
        changed["data"]["other"] = "drift"
        with self.assertRaisesRegex(ValueError, "drift"):
            relay.build_plan(changed, self.defaults, upgrade.CONTEXT, rollback["source"])

    def test_identity_defaults_drift(self):
        for field in ("uid", "resourceVersion"):
            changed = copy.deepcopy(self.old)
            changed["metadata"][field] = "drift"
            self.runner.kubectl = Mock(side_effect=[self.defaults, changed])
            with self.assertRaisesRegex(ValueError, "drift"):
                self.runner.apply(self.hash)
            self.assertEqual(self.runner.kubectl.call_count, 2)
        changed = copy.deepcopy(self.defaults)
        changed["data"][relay.ENV] = "true"
        with self.assertRaisesRegex(ValueError, "default"):
            relay.build_plan(self.old, changed, upgrade.CONTEXT)
        self.runner.kubectl = Mock(return_value=changed)
        with self.assertRaisesRegex(ValueError, "drift"):
            self.runner.apply(self.hash)
        self.assertEqual(self.runner.kubectl.call_count, 1)
        for context in ("wrong", upgrade.CONTEXT):
            bad = copy.deepcopy(self.old)
            bad["metadata"]["name"] = "wrong"
            with self.assertRaises(ValueError):
                relay.build_plan(bad, self.defaults, context)

    def test_tamper_fails_before_get(self):
        self.runner.kubectl = Mock()
        with self.assertRaisesRegex(ValueError, "hash"):
            self.runner.apply("wrong")
        self.plan["patch"][-1]["value"] += "unreviewed"
        self.write_plan()
        with self.assertRaisesRegex(ValueError, "reproducible"):
            self.runner.apply(self.hash)
        self.runner.kubectl.assert_not_called()

    def test_patch_mismatch_missing_guards_and_data_drift(self):
        Path(self.temp.name, "patch.json").write_text("[]")
        self.runner.kubectl = Mock()
        with self.assertRaisesRegex(ValueError, "artifact"):
            self.runner.apply(self.hash)
        self.runner.kubectl.assert_not_called()
        self.write_plan()
        for field in ("uid", "resourceVersion"):
            bad = copy.deepcopy(self.old)
            del bad["metadata"][field]
            with self.assertRaisesRegex(ValueError, "missing"):
                relay.build_plan(bad, self.defaults, upgrade.CONTEXT)
        bad = copy.deepcopy(self.old)
        bad["data"]["other"] = "unrelated drift"
        self.runner.kubectl = Mock(side_effect=[self.defaults, bad])
        with self.assertRaisesRegex(ValueError, "drift"):
            self.runner.apply(self.hash)
        self.assertEqual(self.runner.kubectl.call_count, 2)

    def test_plan_only_gets_and_exclusive_writes(self):
        for name in ("plan.json", "patch.json"):
            Path(self.temp.name, name).unlink()
        self.runner.kubectl = Mock(side_effect=[self.old, self.defaults])
        self.runner.plan()
        self.assertEqual([c.args[0] for c in self.runner.kubectl.call_args_list], ["get", "get"])
        self.assertTrue(Path(self.temp.name, "snapshot.json").exists())
        self.assertTrue(Path(self.temp.name, "plan.diff").exists())
        self.runner.kubectl = Mock(side_effect=[self.old, self.defaults])
        with self.assertRaises(FileExistsError):
            self.runner.plan()

    def test_admission_failure_and_conflict_no_retry(self):
        changed = copy.deepcopy(self.new)
        changed["data"]["other"] = "drift"
        for responses in ([self.defaults, self.old, RuntimeError("dryrun")],
                          [self.defaults, self.old, changed],
                          [self.defaults, self.old, self.new, RuntimeError("conflict")]):
            self.runner.kubectl = Mock(side_effect=responses)
            with self.assertRaises((RuntimeError, ValueError)):
                self.runner.apply(self.hash)
            self.assertEqual(self.runner.kubectl.call_count, len(responses))


if __name__ == "__main__":
    unittest.main()
