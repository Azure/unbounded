# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for centerpivot_e2e.py and nebraska.py, which run without a VM, a cluster, or Podman:

    python3 -m unittest discover -s hack/acl-abupdate -p 'test_centerpivot_e2e.py'
"""

from __future__ import annotations

import base64
import json
import subprocess
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import centerpivot_e2e as e2e  # noqa: E402
import nebraska  # noqa: E402
from errors import StepError  # noqa: E402

SHA384 = "84fd62b2b2c80d267d86ee0d7e9c38b1f9f24d1aea2bc9ccb1abbe4cafeec0cfd4fb371e2a306da9e9c62657089897f3"


def data_url(text: str) -> str:
    return "data:;base64," + base64.b64encode(text.encode()).decode()


def ignition(api_server: str = "https://127.0.0.1:40123") -> dict:
    """The parts of kubectl-unbounded's Ignition config the harness reads."""
    config = {"MachineName": "acl-e2e-1", "Kubelet": {"ApiServer": api_server, "BootstrapToken": "abcdef.0123456789abcdef"}}
    return {
        "ignition": {"version": "3.4.0"},
        "storage": {"files": [
            {"path": "/opt/unbounded/agent/bin/unbounded-agent", "mode": 0o755,
             "contents": {"source": "https://unbounded.invalid/unbounded-agent", "verification": {"hash": "sha256-aa"}}},
            {"path": "/etc/unbounded/agent/config.json", "mode": 0o600, "contents": {"source": data_url(json.dumps(config))}},
        ]},
        "systemd": {"units": [{"name": "unbounded-agent-bootstrap.service", "enabled": True,
                               "contents": "[Service]\nType=oneshot\n"}]},
    }


def machine(state: dict | None) -> dict:
    annotations = {e2e.STATE_ANNOTATION: json.dumps(state)} if state else {}
    return {"metadata": {"name": "acl-e2e-1", "annotations": annotations}}


def node(name: str = "acl-e2e-1", *, ready: bool = True, tainted: bool = False, version: str = "3.0.1") -> dict:
    return {
        "metadata": {"name": name, "uid": f"uid-{name}", "annotations": {
            e2e.AGENT_INFO: json.dumps({"schemaVersion": "1", "machineId": f"id-{name}", "currentVersion": version})}},
        "spec": {"taints": [{"key": e2e.UPDATING_TAINT, "effect": "NoSchedule"}] if tainted else []},
        "status": {"conditions": [{"type": "Ready", "status": "True" if ready else "False"}]},
    }


class SiteTest(unittest.TestCase):
    def test_settings_fit_the_crd_budget(self) -> None:
        self.assertEqual(e2e.budget_problem(e2e.SETTINGS), "")
        defaults = {"updateTimeout": "2h", "statusDeadline": "5m", "drainDeadline": "30m", "restartDeadline": "30m"}
        self.assertEqual(e2e.budget_problem(defaults), "")
        # The defaults need 110m: 40m, two status deadlines, the drain, and the restart.
        self.assertEqual(e2e.budget_problem({**defaults, "updateTimeout": "110m"}), "")
        self.assertIn("shorter than the 111m", e2e.budget_problem({**defaults, "updateTimeout": "110m",
                                                                   "statusDeadline": "5m30s"}))

    def test_durations(self) -> None:
        for text, seconds in {"65m": 3900, "1h5m": 3900, "30s": 30, "2h": 7200, "5m30s": 330}.items():
            self.assertEqual(e2e.parse_duration(text), seconds, text)
        for text in ("", "5", "1d", "m5"):
            with self.subTest(text=text), self.assertRaises(StepError):
                e2e.parse_duration(text)

    def test_allowed_sources(self) -> None:
        # The CEL list is JSON too: a pair of fragments per app, as the request's compact JSON has them.
        server = "https://192.168.114.1:8443/v1/update"
        pairs = json.loads(e2e.allowed_sources(server, ["app-1", "app-2"]))
        self.assertEqual(pairs, [[f'"server":"{server}"', '"appId":"app-1"'], [f'"server":"{server}"', '"appId":"app-2"']])
        request = json.dumps({"server": server, "appId": "app-2", "track": "e2e"}, separators=(",", ":"))
        self.assertTrue(any(a in request and b in request for a, b in pairs))
        self.assertEqual(e2e.allowed_sources(server, []), "[]")

    def test_site(self) -> None:
        site = e2e.site_manifest(e2e.cluster.ClusterSpec(user="tester"),
                                 e2e.center_pivot("https://n/v1/update", "app-1", e2e.SETTINGS))
        self.assertEqual(site["spec"]["nodeCidrs"], ["192.168.114.0/24"])
        center_pivot = site["spec"]["components"]["centerPivot"]
        self.assertTrue(center_pivot["enabled"])
        self.assertEqual(center_pivot["apps"], [{"name": "acl", "appID": "app-1", "defaultTrack": "e2e"}])
        self.assertEqual(center_pivot["caConfigMap"], e2e.CA_CONFIGMAP)
        self.assertEqual(center_pivot["drainDeadline"], "4m")

    def test_bootstrap_token(self) -> None:
        secret = e2e.bootstrap_token_secret("abcdef", "0123456789abcdef", "2026-12-01T00:00:00Z")
        self.assertEqual(secret["metadata"]["labels"], {"unbounded-cloud.io/site": "e2e"})
        self.assertEqual(base64.b64decode(secret["data"]["auth-extra-groups"]).decode(), e2e.BOOTSTRAP_GROUP)

    def test_workload(self) -> None:
        namespace, deployment, budget = e2e.workload_manifests(2)
        self.assertEqual(namespace["metadata"]["name"], e2e.WORKLOAD_NAMESPACE)
        self.assertEqual(deployment["spec"]["replicas"], 2)
        terms = deployment["spec"]["template"]["spec"]["affinity"]["nodeAffinity"][
            "requiredDuringSchedulingIgnoredDuringExecution"]["nodeSelectorTerms"]
        self.assertEqual(terms[0]["matchExpressions"][0]["key"], e2e.MANAGED_LABEL)
        self.assertEqual(budget["spec"], {"minAvailable": 1, "selector": deployment["spec"]["selector"]})


class IgnitionTest(unittest.TestCase):
    def test_payload(self) -> None:
        payload = e2e.ignition_payload(ignition(), "https://127.0.0.1:40123", "https://192.168.114.2:6443")
        self.assertEqual(payload["binary"], "/opt/unbounded/agent/bin/unbounded-agent")
        self.assertEqual(json.loads(payload["config"])["Kubelet"]["ApiServer"], "https://192.168.114.2:6443")
        self.assertEqual((payload["config_path"], payload["config_mode"]), ("/etc/unbounded/agent/config.json", 0o600))
        self.assertEqual(payload["unit"], "unbounded-agent-bootstrap.service")

    def test_payload_needs_the_loopback_server(self) -> None:
        with self.assertRaises(StepError):
            e2e.ignition_payload(ignition("https://10.0.0.1:6443"), "https://127.0.0.1:40123", "https://192.168.114.2:6443")

    def test_payload_needs_its_parts(self) -> None:
        doc = ignition()
        doc["storage"]["files"] = doc["storage"]["files"][1:]
        with self.assertRaises(StepError):
            e2e.ignition_payload(doc, "https://127.0.0.1:40123", "https://192.168.114.2:6443")
        doc = ignition()
        doc["systemd"]["units"] = []
        with self.assertRaises(StepError):
            e2e.ignition_payload(doc, "https://127.0.0.1:40123", "https://192.168.114.2:6443")


class SampleTest(unittest.TestCase):
    def test_node_view(self) -> None:
        state = {"phase": "Draining", "reason": "1 pods left to move", "attempts": 1, "grant": {"version": "3.0.2"},
                 "targetVersion": "3.0.2"}
        view = e2e.node_view(node(tainted=True), machine(state))
        self.assertEqual((view["phase"], view["attempts"], view["grant"], view["endGrant"]), ("Draining", 1, True, False))
        self.assertEqual(view["targetVersion"], "3.0.2")
        self.assertTrue(view["tainted"] and view["ready"])
        self.assertEqual((view["version"], view["machineId"]), ("3.0.1", "id-acl-e2e-1"))
        idle = e2e.node_view(None, machine(None))
        self.assertEqual((idle["phase"], idle["exists"], idle["ready"], idle["version"]), ("Idle", False, False, ""))
        self.assertEqual(e2e.state_of({"metadata": {"annotations": {e2e.STATE_ANNOTATION: "{"}}})["phase"], "Unreadable")

    def test_updated(self) -> None:
        def view(phase: str, version: str, reason: str = "") -> dict:
            state = {"phase": phase, "targetVersion": "3.0.2", "reason": reason}
            return e2e.node_view(node(version=version), machine(state))

        self.assertTrue(e2e.updated(view("Idle", "3.0.2", "updated to 3.0.2"), "3.0.2"))
        # The next check writes its own reason, and the node stays updated.
        self.assertTrue(e2e.updated(view("Idle", "3.0.2"), "3.0.2"))
        self.assertFalse(e2e.updated(view("Recovering", "3.0.2"), "3.0.2"))
        self.assertFalse(e2e.updated(view("Idle", "3.0.1", "staging 3.0.2 failed"), "3.0.2"))
        self.assertFalse(e2e.updated(view("Held", "3.0.1"), "3.0.2"))

    def test_samples(self) -> None:
        def sample(a: str, b: str) -> dict:
            machines = {"items": [{"metadata": {"name": "acl-e2e-1", "annotations": {e2e.STATE_ANNOTATION: json.dumps({"phase": a})}}},
                                  {"metadata": {"name": "acl-e2e-2", "annotations": {e2e.STATE_ANNOTATION: json.dumps({"phase": b})}}}]}
            return e2e.sample_of(["acl-e2e-1", "acl-e2e-2"], {"items": [node("acl-e2e-1"), node("acl-e2e-2")]}, machines,
                                 {"status": {"availableReplicas": 2}})

        samples = [sample("Idle", "Idle"), sample("Staging", "Idle"), sample("Staging", "Idle"), sample("Idle", "Checking")]
        self.assertEqual(samples[0]["available"], 2)
        self.assertEqual(e2e.most_updating(samples), 1)
        self.assertEqual(e2e.most_updating([*samples, sample("Recovering", "Staging")]), 2)
        self.assertEqual(e2e.phases_seen(samples, "acl-e2e-1"), ["Idle", "Staging", "Idle"])
        self.assertEqual(e2e.most_updating([]), 0)

    def test_phase_counts(self) -> None:
        text = "\n".join([
            "# HELP centerpivot_phase_seconds How long nodes spent in each phase of an update.",
            "# TYPE centerpivot_phase_seconds histogram",
            'centerpivot_phase_seconds_bucket{app="acl",next="Draining",phase="Staging",site="e2e",le="5"} 0',
            'centerpivot_phase_seconds_sum{app="acl",next="Draining",phase="Staging",site="e2e"} 61.5',
            'centerpivot_phase_seconds_count{app="acl",next="Draining",phase="Staging",site="e2e"} 2',
            'centerpivot_phase_seconds_count{app="",next="Draining",phase="Staging",site="e2e"} 1',
            'centerpivot_phase_seconds_count{app="acl",next="Idle",phase="Draining",site="e2e"} 1',
            'centerpivot_phase_seconds_count{app="acl",next="Draining",phase="Staging",site="other"} 5',
        ])
        self.assertEqual(e2e.phase_counts(text, "e2e"), {("Staging", "Draining"): 3, ("Draining", "Idle"): 1})
        self.assertEqual(e2e.phase_counts("", "e2e"), {})

    def test_operations_seen(self) -> None:
        def sample(request: str) -> dict:
            return {"nodes": {"acl-e2e-1": {"request": request}}}

        requests = [sample(""), sample(json.dumps({"operation": "stage"})), sample("{"),
                    sample(json.dumps({"operation": "finalize"})), sample("")]
        self.assertEqual(e2e.operations_seen(requests, "acl-e2e-1"), {"stage", "finalize"})


class CommandTest(unittest.TestCase):
    def test_parser(self) -> None:
        parser = e2e.build_parser()
        self.assertEqual(parser.parse_args(["run"]).scenarios, [])
        self.assertEqual(parser.parse_args(["run", "rolling", "boot-failure"]).scenarios, ["rolling", "boot-failure"])
        self.assertTrue(parser.parse_args(["test", "--keep"]).keep)
        self.assertEqual(list(e2e.SCENARIOS)[0], "rolling")

    def test_vm(self) -> None:
        vm = e2e.VM(2, e2e.cluster.ClusterSpec(user="tester"))
        self.assertEqual((vm.name, vm.address, vm.ssh_port), ("acl-e2e-2", "192.168.114.11", e2e.SSH_PORT + 2))
        self.assertEqual(vm.state_dir, e2e.STATE / "vm2")


class NebraskaPayloadTest(unittest.TestCase):
    def test_package(self) -> None:
        package = nebraska.package_payload("app", version="3.0.20261106", base_url="https://h:1/images/",
                                           filename="acl.cosi", size=123, sha384=SHA384.upper())
        self.assertEqual((package["type"], package["arch"], package["size"]), (4, 1, "123"))
        self.assertEqual(package["labels"], {"cosi.metadata.sha384": SHA384})
        self.assertEqual(base64.b64decode(package["hash"]).hex(), SHA384)
        for problem in ({"base_url": "https://h:1/images"}, {"sha384": "abc"}, {"sha384": "zz" * 48}):
            with self.subTest(problem=problem), self.assertRaises(StepError):
                nebraska.package_payload("app", **{"version": "1.0.0", "base_url": "https://h/", "filename": "a",
                                                   "size": 1, "sha384": SHA384, **problem})

    def test_group(self) -> None:
        group = nebraska.group_payload("g", "channel", "e2e", 120)
        self.assertEqual((group["track"], group["channel_id"], group["policy_update_timeout"]), ("e2e", "channel", "120 minutes"))
        # Safe mode would turn the group's updates off when the service ends a grant.
        self.assertFalse(group["policy_safe_mode"])
        self.assertTrue(group["policy_updates_enabled"])

    def test_instances_of_the_app(self) -> None:
        # The list has a row for each app an instance checked in to, from the instance's other apps too.
        rows = {"instances": [
            {"id": "a", "application": {"application_id": "app-1", "status": 3}},
            {"id": "a", "application": {"application_id": "app-0", "status": 4}},
            {"id": "b", "application": {"application_id": "app-1", "status": 4}},
        ]}
        server = nebraska.Nebraska(name="neb", port=18000, work_dir=Path("/nonexistent"), run=subprocess.run,
                                   log=lambda _: None)
        server.request = lambda method, path, body=None: rows  # type: ignore[method-assign]
        app = nebraska.App(id="app-1", name="x", track="e2e", group_id="g", channel_id="c", package_id="p", version="1")
        self.assertEqual([(row["id"], row["application"]["status"]) for row in server.instances(app)], [("a", 3), ("b", 4)])

    def test_pod(self) -> None:
        calls: list[list[str]] = []

        def run(cmd: list[str], *, input: str | None = None, timeout: float | None = None,
                stream: bool = False) -> subprocess.CompletedProcess[str]:
            calls.append(cmd)
            return subprocess.CompletedProcess(cmd, 0, "", "")

        server = nebraska.Nebraska(name="neb", port=18000, work_dir=Path("/nonexistent"), run=run, log=lambda _: None)
        server.healthy = lambda: True  # type: ignore[method-assign]
        server.start()
        commands = [" ".join(cmd) for cmd in calls]
        self.assertEqual(commands[0], "podman pod rm -f --ignore neb")
        self.assertIn("podman pod create --name neb -p 127.0.0.1:18000:8000", commands)
        server_run = next(cmd for cmd in calls if "neb-server" in cmd)
        self.assertIn(f"NEBRASKA_DB_URL={nebraska.DB_URL}", server_run)
        self.assertIn("-auth-mode=noop", server_run)
        # The image exists, so build doesn't build it.
        calls.clear()
        server.build()
        self.assertEqual(calls, [["podman", "image", "exists", nebraska.IMAGE]])


if __name__ == "__main__":
    unittest.main()
