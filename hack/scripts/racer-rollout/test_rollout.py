import copy
import json
import unittest
from unittest.mock import patch

import yaml

import firewall
import rollout


def overrides():
    entries = []
    for component, kind, name, role in (
        ("racer", "Deployment", "", "controller"),
        ("racer", "DaemonSet", "racer-dataplane", "dataplane"),
        ("racer", "DaemonSet", "racer-dataplane-podnet", "dataplane"),
        ("gantry", "DaemonSet", "", "gantry"),
    ):
        entries.append({"component": component, "kind": kind, "name": name,
                        "addInitContainers": ["secure-directory", "guard-launch-copy"],
                        "patch": {"spec": {"template": {"spec": {
                            "hostNetwork": name != "racer-dataplane-podnet",
                            "affinity": {"nodeAffinity": {"keep": ["node-a"]}},
                            "containers": [{"name": role, "image": "old", "command": ["/guard-launch/racer-guard-launch"],
                                            "resources": {"requests": {"cpu": "2"}},
                                            "env": [{"name": "RACER_ROUTING_ALGORITHM", "value": "3"},
                                                    {"name": "RACER_MIGRATION_GUARD_SOURCE_CONFIGMAP", "value": "old"}],
                                            "volumeMounts": [{"name": "sockets"}, {"name": "guard-socket"}]}],
                            "initContainers": [{"name": "guard-launch-copy"}, {"name": "secure-directory"}],
                            "volumes": [{"name": v} for v in sorted(rollout.VOLUMES)] + [{"name": "sockets"}],
                        }}}}})
    return {"racer.yaml": yaml.safe_dump({"overrides": entries}),
            "net.yaml": "overrides: [{component: net, kind: Deployment}]\n"}


class TransformTest(unittest.TestCase):
    def test_preserves_config_removes_exact_fields_and_pins(self):
        original = overrides()
        result = rollout.transform_overrides(original)
        self.assertEqual(result["net.yaml"], original["net.yaml"])
        self.assertEqual(result, rollout.transform_overrides(result))
        before = yaml.safe_load(original["racer.yaml"])["overrides"]
        after = yaml.safe_load(result["racer.yaml"])["overrides"]
        for old, new in zip(before, after):
            pod = new["patch"]["spec"]["template"]["spec"]
            oldpod = old["patch"]["spec"]["template"]["spec"]
            self.assertEqual(pod["affinity"], oldpod["affinity"])
            self.assertEqual(pod["hostNetwork"], oldpod["hostNetwork"])
            self.assertEqual(pod["volumes"], [{"name": "sockets"}])
            self.assertEqual(pod["initContainers"], [{"name": "secure-directory"}])
            self.assertEqual(new["addInitContainers"], ["secure-directory"])
            container = pod["containers"][0]
            self.assertNotIn("command", container)
            self.assertEqual(container["resources"], oldpod["containers"][0]["resources"])
            self.assertEqual(container["env"], [{"name": "RACER_ROUTING_ALGORITHM", "value": "3"}])
            self.assertEqual(container["image"], rollout.IMAGES[container["name"]])
            if new["kind"] == "DaemonSet":
                self.assertEqual(new["patch"]["spec"]["updateStrategy"]["rollingUpdate"], {"maxSurge": 0, "maxUnavailable": "10%"})

    def test_missing_override_is_error(self):
        with self.assertRaisesRegex(ValueError, "missing"):
            rollout.transform_overrides({"empty.yaml": "overrides: []"})

    def test_unrelated_command_and_environment_untouched(self):
        pod = {"containers": [{"name": "x", "command": ["custom"],
                               "env": [{"name": "OTHER_GUARD", "value": "keep"}]}]}
        before = copy.deepcopy(pod)
        rollout.sanitize_pod(pod)
        self.assertEqual(pod, before)

    def test_cleanup_manifest_placement_and_privileges(self):
        spec = rollout.cleanup_manifest()["spec"]["template"]["spec"]
        self.assertTrue(spec["hostNetwork"])
        self.assertFalse(spec["automountServiceAccountToken"])
        self.assertEqual(spec["nodeSelector"], {"kubernetes.io/os": "linux"})
        self.assertEqual(spec["tolerations"], [{"operator": "Exists"}])
        self.assertNotIn("volumeMounts", spec["containers"][0])
        self.assertTrue(spec["containers"][0]["securityContext"]["runAsNonRoot"])
        init = spec["initContainers"][0]
        self.assertEqual(set(init["securityContext"]["capabilities"]["add"]), {"NET_ADMIN", "NET_RAW", "SYS_CHROOT"})
        self.assertTrue(init["volumeMounts"][0]["readOnly"])
        self.assertIn("240s", init["command"])


class RolloutOrderTest(unittest.TestCase):
    def runner(self, phases, replicas=0):
        runner = object.__new__(rollout.Runner)
        runner.get = unittest.mock.Mock(return_value={"spec": {"replicas": replicas}})
        runner.kubectl = unittest.mock.Mock(return_value=json.dumps({
            "items": [{"status": {"phase": phase}} for phase in phases],
        }))
        return runner

    def test_terminal_operator_pods_do_not_block(self):
        self.runner(["Succeeded", "Failed"]).stopped()
        self.runner([]).stopped()

    def test_nonterminal_operator_pods_block(self):
        for phase in ("Pending", "Running", "Unknown", None):
            with self.subTest(phase=phase):
                with self.assertRaisesRegex(RuntimeError, "pods still present"):
                    self.runner(["Succeeded", phase]).stopped()

    def test_desired_operator_replicas_still_block(self):
        with self.assertRaisesRegex(RuntimeError, "run stop first"):
            self.runner([], replicas=1).stopped()

    def test_stop_wait_excludes_terminal_pods(self):
        runner = self.runner(["Failed", "Succeeded"])
        runner.stop()
        wait = runner.kubectl.call_args_list[1]
        self.assertEqual(wait.args[0], "wait")
        self.assertIn("--field-selector=status.phase!=Succeeded,status.phase!=Failed", wait.args)
        self.assertEqual(wait.kwargs["seconds"], 50)

    def test_controller_rolls_only_through_new_operator(self):
        self.assertFalse(any(name == "racer-controller" or role == "controller"
                             for _, name, role, _ in rollout.ROLLOUT_TARGETS))
        result = yaml.safe_load(rollout.transform_overrides(overrides())["racer.yaml"])
        controller = result["overrides"][0]["patch"]["spec"]["template"]["spec"]["containers"][0]
        self.assertEqual(controller["image"], rollout.IMAGES["controller"])


class FirewallTest(unittest.TestCase):
    def test_exact_cleanup_and_no_flush(self):
        rules = '\n'.join([
            '-P INPUT ACCEPT', '-N RACER_STAGE47', '-N KUBE-SERVICES',
            '-A INPUT -j KUBE-SERVICES',
            '-A INPUT -m set --match-set R47_FRESH src -m comment --comment racer-stage47-fresh -j REJECT',
            '-A INPUT -m comment --comment racer-stage47-owned -j RACER_STAGE47',
            '-A RACER_STAGE47 -m comment --comment racer-stage47-owned -j RETURN',
        ])
        with patch.object(firewall, "run", side_effect=[rules, "", "", "", "", "R47_PEERS\nR47_FRESH\nR47_DYNAMIC_TMP\nR47_TMP_0123456789abcdef\nR47_TMP_bad\nKUBE-IPVS\n", "", "", "", ""]) as run:
            firewall.cleanup()
        calls = [c.args for c in run.call_args_list]
        self.assertEqual(len(calls), 10)
        self.assertFalse(any("-F" in c or "flush" in c for c in calls))
        self.assertFalse(any("KUBE-SERVICES" in c or "KUBE-IPVS" in c or "R47_TMP_bad" in c for c in calls))
        self.assertIn(("iptables", "-w", "5", "-t", "filter", "-X", "RACER_STAGE47"), calls)

    def test_foreign_chain_rule_fails_before_mutation(self):
        with patch.object(firewall, "run", return_value="-N RACER_STAGE47\n-A RACER_STAGE47 -j ACCEPT") as run:
            with self.assertRaisesRegex(RuntimeError, "untagged"):
                firewall.cleanup()
            self.assertEqual(run.call_count, 1)

    def test_already_clean(self):
        with patch.object(firewall, "run", side_effect=["-P INPUT ACCEPT", "KUBE-IPVS\n"]) as run:
            firewall.cleanup()
            self.assertEqual(run.call_count, 2)

    def test_tag_alone_does_not_authorize_deletion(self):
        self.assertFalse(firewall.removable(["-A", "INPUT", "--comment", "racer-stage47-owned", "-j", "ACCEPT"]))
        self.assertFalse(firewall.removable(["-A", "INPUT", "--comment", "racer-stage47-fresh", "-j", "REJECT", "--match-set", "OTHER"]))


if __name__ == "__main__":
    unittest.main()
