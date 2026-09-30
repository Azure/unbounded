import copy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

import yaml

import page_window as window
import upgrade
from test_disable_heap_profiling import fixture


def fixtures():
    cms = {}
    for name in window.CMS:
        cms[name] = {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {
            "name": name, "namespace": upgrade.NS, "uid": name, "resourceVersion": "10"}}
    cms[window.NAME]["data"] = {window.KEY: fixture(), "other": "overrides: []\n"}
    cms[window.GANTRY]["data"] = {window.GKEY: "# retained\nracer_page_window: 1 # comment\nother: keep\n"}
    cms[window.DEFAULTS]["data"] = {window.ENV: "1", "RACER_OPAQUE_RELAY": "false"}
    workloads = {}
    for name in window.WORKLOADS:
        c = {"name": "gantry" if name == "gantry" else "dataplane", "image": "keep", "env": []}
        pod = {"containers": [c]}
        if name == "gantry":
            c.update(args=["agent", "--config=/etc/gantry/config.yaml"],
                     resources={"limits": {"memory": "2Gi"}},
                     volumeMounts=[{"name": "config", "mountPath": "/etc/gantry"}])
            pod["volumes"] = [{"name": "config", "configMap": {"name": window.GANTRY}}]
        else:
            c["envFrom"] = [{"configMapRef": {"name": window.DEFAULTS}}]
        workloads[name] = {"apiVersion": "apps/v1", "kind": "DaemonSet", "metadata": {
            "name": name, "namespace": upgrade.NS, "uid": name, "resourceVersion": "20"},
            "spec": {"template": {"spec": pod}}}
    return cms, workloads


def applied(cms, plan):
    result = copy.deepcopy(cms)
    result[plan["name"]]["data"][plan["key"]] = plan["patch"][-1]["value"]
    result[plan["name"]]["metadata"]["resourceVersion"] += "1"
    return result


class TransformTests(unittest.TestCase):
    def test_gantry_exact_single_byte_change(self):
        text = "# retained\nracer_page_window: 1 # retained\nother: 1\n"
        self.assertEqual(window.tune(text), text.replace("window: 1", "window: 2"))

    def test_gantry_fail_closed(self):
        for value in ("0", "2", "true", "'1'", "&v 1", "1\nracer_page_window: 1"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                window.tune("racer_page_window: " + value)

    def test_racer_insert_preserves_bytes_and_images(self):
        text = fixture()
        new = window.tune(text, True)
        restored = "".join(line for line in new.splitlines(True)
                           if window.ENV not in line and 'value: "2"' not in line)
        self.assertEqual(restored, text)
        self.assertEqual(new.count(window.ENV), 2)

    def test_replace_shared_entry_and_reject_unrelated_alias(self):
        text = fixture().replace("RACER_HEAP_PROFILE_ADDR", window.ENV).replace("$(RACER_POD_IP):6060", "'1'")
        result = window.tune(text, True)
        self.assertEqual(result.replace('"2"', "'1'"), text)
        with self.assertRaisesRegex(ValueError, "unrelated"):
            window.tune(text + "unrelated: *id003\n", True)

    def test_racer_missing_duplicate_flow_and_wrong_value(self):
        doc = yaml.safe_load(fixture())
        for entries in ([], doc["overrides"] * 2):
            with self.assertRaises(ValueError):
                window.tune(yaml.safe_dump({"overrides": entries}), True)
        with self.assertRaises(ValueError):
            window.tune(yaml.safe_dump(doc, default_flow_style=True), True)
        for value in ("true", "2", "&flag '1'"):
            with self.assertRaises(ValueError):
                window.tune(fixture().replace("RACER_HEAP_PROFILE_ADDR", window.ENV).replace("$(RACER_POD_IP):6060", value), True)


class PlanTests(unittest.TestCase):
    def setUp(self):
        self.cms, self.ds = fixtures()
        self.plan = window.build_plan(self.cms, self.ds, upgrade.CONTEXT, "gantry")
        self.new = applied(self.cms, self.plan)
        self.temp = tempfile.TemporaryDirectory(dir=upgrade.PROJECT / "tmp")
        self.addCleanup(self.temp.cleanup)
        self.runner = window.WindowRunner(self.temp.name, upgrade.CONTEXT)
        self.runner.note = Mock()
        self.write_plan()

    def write_plan(self):
        Path(self.temp.name, "plan.json").write_text(upgrade.encoded(self.plan))
        Path(self.temp.name, "patch.json").write_text(upgrade.encoded(self.plan["patch"]))
        self.hash = upgrade.plan_hash(self.plan)

    def test_stages_and_reverse_exact_rollback(self):
        with self.assertRaisesRegex(ValueError, "Gantry window 2"):
            window.build_plan(self.cms, self.ds, upgrade.CONTEXT, "racer")
        racer = window.build_plan(self.new, self.ds, upgrade.CONTEXT, "racer")
        both = applied(self.new, racer)
        with self.assertRaises(ValueError):
            window.build_plan(both, self.ds, upgrade.CONTEXT, "gantry", self.plan)
        changed_ds = copy.deepcopy(self.ds)
        for n in window.TARGETS:
            changed_ds[n]["spec"]["template"]["spec"]["containers"][0]["env"] = [{"name": window.ENV, "value": "2"}]
        rollback = window.build_plan(both, changed_ds, upgrade.CONTEXT, "racer", racer)
        reverted = applied(both, rollback)
        gantry_rollback = window.build_plan(reverted, self.ds, upgrade.CONTEXT, "gantry", self.plan)
        self.assertEqual(gantry_rollback["patch"][-1]["value"], self.cms[window.GANTRY]["data"][window.GKEY])
        self.assertEqual(rollback["patch"][-1]["value"], self.cms[window.NAME]["data"][window.KEY])
        self.assertEqual(rollback["patch"][1]["value"], both[window.NAME]["metadata"]["resourceVersion"])

    def test_dryrun_apply_and_idempotence(self):
        self.assertEqual([p["path"] for p in self.plan["patch"]],
                         ["/metadata/uid", "/metadata/resourceVersion", "/data", "/data/config.yaml"])
        self.runner.collect = Mock(return_value=(self.cms, self.ds))
        self.runner.kubectl = Mock(return_value=self.new[window.GANTRY])
        self.runner.apply(self.hash, True)
        self.assertEqual(self.runner.kubectl.call_count, 1)
        self.assertIn("--dry-run=server", self.runner.kubectl.call_args.args)
        self.runner.apply(self.hash)
        self.assertEqual(self.runner.kubectl.call_count, 3)
        self.assertNotIn("--dry-run=server", self.runner.kubectl.call_args.args)
        self.runner.kubectl.reset_mock()
        self.runner.collect.return_value = (self.new, self.ds)
        self.runner.apply(self.hash)
        self.runner.kubectl.assert_not_called()

    def test_rollback_apply_restores_exact_bytes(self):
        self.plan = window.build_plan(self.new, self.ds, upgrade.CONTEXT, "gantry", self.plan)
        self.write_plan()
        self.runner.collect = Mock(return_value=(self.new, self.ds))
        restored = applied(self.new, self.plan)[window.GANTRY]
        self.runner.kubectl = Mock(return_value=restored)
        self.runner.apply(self.hash)
        self.assertEqual(self.runner.kubectl.call_count, 2)
        self.assertEqual(self.runner.kubectl.call_args.kwargs["payload"][-1]["value"],
                         self.cms[window.GANTRY]["data"][window.GKEY])

    def test_tamper_and_artifact_mismatch(self):
        self.runner.collect = Mock()
        with self.assertRaisesRegex(ValueError, "hash"):
            self.runner.apply("wrong")
        Path(self.temp.name, "patch.json").write_text("[]")
        with self.assertRaisesRegex(ValueError, "artifact"):
            self.runner.apply(self.hash)
        self.plan["patch"][-1]["value"] += "unreviewed"
        self.write_plan()
        with self.assertRaisesRegex(ValueError, "reproducible"):
            self.runner.apply(self.hash)
        self.runner.collect.assert_not_called()

    def test_cm_workload_and_admission_drift_no_write(self):
        for name in window.CMS:
            for field in ("uid", "resourceVersion"):
                bad = copy.deepcopy(self.cms)
                bad[name]["metadata"][field] = "drift"
                self.runner.collect = Mock(return_value=(bad, self.ds))
                self.runner.kubectl = Mock()
                with self.assertRaisesRegex(ValueError, "drift"):
                    self.runner.apply(self.hash)
                self.runner.kubectl.assert_not_called()
        bad = copy.deepcopy(self.ds)
        bad["gantry"]["spec"]["template"]["spec"]["containers"][0]["image"] = "drift"
        self.runner.collect = Mock(return_value=(self.cms, bad))
        with self.assertRaisesRegex(ValueError, "drift"):
            self.runner.apply(self.hash)
        self.runner.collect.return_value = (self.cms, self.ds)
        bad = copy.deepcopy(self.new[window.GANTRY])
        bad["data"]["unrelated"] = "drift"
        for responses in ([bad], [RuntimeError("dryrun")], [self.new[window.GANTRY], RuntimeError("conflict")]):
            self.runner.kubectl = Mock(side_effect=responses)
            with self.assertRaises((ValueError, RuntimeError)):
                self.runner.apply(self.hash)
            self.assertEqual(self.runner.kubectl.call_count, len(responses))

    def test_precedence_memory_relay_identity_and_defaults(self):
        for field, value in (("args", ["agent", "--racer-page-window=1"]),
                             ("env", [{"name": "GANTRY_RACER_PAGE_WINDOW", "value": "1"}]),
                             ("resources", {"limits": {"memory": "1Gi"}})):
            bad = copy.deepcopy(self.ds)
            bad["gantry"]["spec"]["template"]["spec"]["containers"][0][field] = value
            with self.assertRaises(ValueError):
                window.build_plan(self.cms, bad, upgrade.CONTEXT, "gantry")
        bad = copy.deepcopy(self.ds)
        bad["racer-dataplane"]["spec"]["template"]["spec"]["containers"][0]["env"] = [{"name": "RACER_OPAQUE_RELAY", "value": "true"}]
        with self.assertRaisesRegex(ValueError, "relay rollback"):
            window.build_plan(self.cms, bad, upgrade.CONTEXT, "gantry")
        for name in window.CMS:
            bad = copy.deepcopy(self.cms)
            del bad[name]["metadata"]["uid"]
            with self.assertRaises(ValueError):
                window.build_plan(bad, self.ds, upgrade.CONTEXT, "gantry")
        bad = copy.deepcopy(self.cms)
        bad[window.DEFAULTS]["data"][window.ENV] = "2"
        with self.assertRaises(ValueError):
            window.build_plan(bad, self.ds, upgrade.CONTEXT, "gantry")

    def test_override_drift_and_unconverged_window(self):
        for key, value in (("other", fixture()),
                           (window.KEY, fixture().replace("RACER_HEAP_PROFILE_ADDR", "RACER_OPAQUE_RELAY").replace("$(RACER_POD_IP):6060", "'true'")),
                           (window.KEY, fixture().replace("RACER_HEAP_PROFILE_ADDR", window.ENV).replace("$(RACER_POD_IP):6060", "'2'"))):
            bad = copy.deepcopy(self.cms)
            bad[window.NAME]["data"][key] = value
            with self.assertRaises(ValueError):
                window.build_plan(bad, self.ds, upgrade.CONTEXT, "gantry")
        bad = copy.deepcopy(self.ds)
        bad["racer-dataplane"]["spec"]["template"]["spec"]["containers"][0]["env"] = [{"name": window.ENV, "value": "2"}]
        with self.assertRaisesRegex(ValueError, "converged"):
            window.build_plan(self.cms, bad, upgrade.CONTEXT, "gantry")

    def test_plan_exclusive_and_gets_only(self):
        for name in ("plan.json", "patch.json"):
            Path(self.temp.name, name).unlink()
        self.runner.kubectl = Mock(side_effect=[{"items": list(self.cms.values())}, {"items": list(self.ds.values())}])
        self.runner.plan("gantry")
        self.assertEqual([c.args[0] for c in self.runner.kubectl.call_args_list], ["get", "get"])
        self.runner.collect = Mock(return_value=(self.cms, self.ds))
        with self.assertRaises(FileExistsError):
            self.runner.plan("gantry")


if __name__ == "__main__":
    unittest.main()
