# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for cluster.py, which run without a cluster, sudo, or the network:

    python3 -m unittest discover -s hack/acl-abupdate -p 'test_cluster.py'
"""

from __future__ import annotations

import base64
import copy
import dataclasses
import datetime
import itertools
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cluster as flexcluster  # noqa: E402
from errors import StepError  # noqa: E402

# The names aclab used, with one VM, whose TAP is tap-aclab1.
SPEC = flexcluster.ClusterSpec(name="aclab", subnet="192.168.113", bridge="virbr-aclab", tap_prefix="tap-aclab",
                               veth_host="veth-aclab-kind", veth_kind="eth-aclab", user="tester", vms=1)
PREFIX = "acl-abupdate.io"
UTC = datetime.timezone.utc
CA_PEM = "-----BEGIN CERTIFICATE-----\nMIIBfake\n-----END CERTIFICATE-----\n"

NODE = {
    "metadata": {
        "name": "acl-node",
        "uid": "uid-1",
        "resourceVersion": "42",
        "annotations": {f"{PREFIX}/a": "1", f"{PREFIX}/b": "2", "other.io/x": "y", f"{PREFIX}x/c": "3"},
    },
    "spec": {
        "taints": [
            {"key": "node.kubernetes.io/unreachable", "effect": "NoExecute", "timeAdded": "2026-11-12T10:00:00Z"},
            {"key": "example.com/t", "value": "v", "effect": "NoSchedule"},
        ],
    },
    "status": {
        "conditions": [
            {"type": "MemoryPressure", "status": "False", "reason": "KubeletHasSufficientMemory"},
            {"type": "Ready", "status": "True", "reason": "KubeletReady", "message": "kubelet is posting ready status"},
        ],
        "addresses": [{"type": "InternalIP", "address": "192.168.113.10"}, {"type": "Hostname", "address": "acl-node"}],
        "nodeInfo": {"kubeletVersion": "v1.37.0"},
    },
}


class FakeRun:
    """A run that records commands and answers them from rules: the first rule whose needle is in
    the command line, or starts it for a needle starting with ^, and that has answers left,
    answers."""

    def __init__(self) -> None:
        self.calls: list[dict] = []
        self.rules: list[list] = []

    def on(self, needle: str, stdout: str = "", stderr: str = "", returncode: int = 0,
           times: int | None = None) -> FakeRun:
        self.rules.append([needle, returncode, stdout, stderr, times])
        return self

    def __call__(self, cmd, *, input=None, timeout=None, stream=False):
        self.calls.append({"cmd": list(cmd), "input": input, "timeout": timeout, "stream": stream})
        line = " ".join(cmd)
        returncode, stdout, stderr = 0, "", ""
        for rule in self.rules:
            needle = rule[0]
            matches = line.startswith(needle[1:]) if needle.startswith("^") else needle in line
            if matches and (rule[4] is None or rule[4] > 0):
                if rule[4] is not None:
                    rule[4] -= 1
                returncode, stdout, stderr = rule[1], rule[2], rule[3]
                break
        if stream:
            return subprocess.CompletedProcess(cmd, returncode, None, None)
        return subprocess.CompletedProcess(cmd, returncode, stdout, stderr)

    def commands(self, needle: str = "") -> list[list[str]]:
        return [call["cmd"] for call in self.calls if needle in " ".join(call["cmd"])]

    def lines(self) -> list[str]:
        return [" ".join(call["cmd"]) for call in self.calls]


class ClusterTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(self.tmp, ignore_errors=True))
        self.run = FakeRun()
        self.logs: list[str] = []

    def cluster(self, spec: flexcluster.ClusterSpec = SPEC) -> flexcluster.Cluster:
        cluster = flexcluster.Cluster(spec, kubeconfig=self.tmp / "kubeconfig",
                                      work_dir=self.tmp / "work", run=self.run, log=self.logs.append)
        cluster.sleep = lambda seconds: None
        clock = itertools.count(step=50)
        cluster.monotonic = lambda: float(next(clock))
        cluster.which = lambda name: f"/home/u/go/bin/{name}"
        cluster.inotify_path = self.tmp / "max_user_instances"
        return cluster

    def identity_rules(self, uid: str = "ns-uid") -> None:
        self.run.on("get namespace kube-system", stdout=uid)
        self.run.on("config view --minify --raw", stdout=base64.b64encode(CA_PEM.encode()).decode())


def links(*entries: dict) -> str:
    return json.dumps(list(entries))


def link(name: str, *, master: str = "", up: bool = True, addresses: tuple[str, ...] = ()) -> dict:
    entry = {"ifname": name, "flags": ["BROADCAST", "MULTICAST"] + (["UP"] if up else []),
             "addr_info": [{"family": "inet", "local": a.split("/")[0], "prefixlen": int(a.split("/")[1])}
                           for a in addresses]}
    if master:
        entry["master"] = master
    return entry


# ---------------------------------------------------------------------------------------------


class ClusterSpecTest(unittest.TestCase):
    def test_defaults_and_properties(self) -> None:
        spec = flexcluster.ClusterSpec()
        spec.validate()
        # Apart from aclab's, on 192.168.113, so both can run on one host.
        self.assertEqual(spec.gateway, "192.168.114.1")
        self.assertEqual(spec.kind_ip, "192.168.114.2")
        self.assertEqual(spec.taps, ["tap-acle2e1", "tap-acle2e2"])
        self.assertEqual([spec.vm_ip(1), spec.vm_ip(2)], ["192.168.114.10", "192.168.114.11"])
        self.assertEqual(spec.api_server, "192.168.114.2:6443")
        self.assertEqual(spec.node_container, "acl-e2e-control-plane")

    def test_round_trip(self) -> None:
        spec = flexcluster.ClusterSpec(name="aclab-dev", subnet="10.20.30", bridge="virbr-x", tap_prefix="tap-x",
                                       veth_host="veth-x", veth_kind="eth-x", node_image="kindest/node:v1.37.0",
                                       user="1000", vms=3)
        data = json.loads(json.dumps(spec.to_dict()))
        self.assertEqual(flexcluster.ClusterSpec.from_dict(data), spec)
        self.assertEqual(flexcluster.ClusterSpec.from_dict({**data, "later": "ignored"}), spec)

    def test_frozen(self) -> None:
        with self.assertRaises(dataclasses.FrozenInstanceError):
            SPEC.name = "other"  # type: ignore[misc]

    def test_invalid(self) -> None:
        cases = {
            "bridge too long": {"bridge": "virbr-sixteen-ch"},
            "tap with a slash": {"tap_prefix": "tap/x"},
            "tap too long": {"tap_prefix": "tap-fifteen-chr"},
            "veth with a space": {"veth_kind": "eth x"},
            "same names": {"tap_prefix": "virbr-acle2e", "vms": 1, "bridge": "virbr-acle2e1"},
            "two octets": {"subnet": "192.168"},
            "four octets": {"subnet": "192.168.113.0"},
            "octet too big": {"subnet": "192.168.300"},
            "leading zero": {"subnet": "192.168.013"},
            "letters": {"subnet": "a.b.c"},
            "uppercase name": {"name": "AcLab"},
            "name too long": {"name": "a" * 50},
            "name ends with a dash": {"name": "aclab-"},
            "user with a space": {"user": "a b"},
            "image with a quote": {"node_image": 'kindest/node:"x"'},
            "not a string": {"subnet": 192},
            "no VMs": {"vms": 0},
            "VMs not a number": {"vms": "2"},
        }
        for name, change in cases.items():
            with self.subTest(name), self.assertRaises(StepError):
                flexcluster.ClusterSpec(**change).validate()
        with self.assertRaises(StepError):
            flexcluster.ClusterSpec.from_dict({"subnet": "300.1.1"})

    def test_cluster_validates_its_spec(self) -> None:
        with self.assertRaises(StepError):
            flexcluster.Cluster(flexcluster.ClusterSpec(subnet="x"), kubeconfig=Path("k"),
                                work_dir=Path("w"), run=FakeRun(), log=print)


class NetworkPlanTest(unittest.TestCase):
    FORBIDDEN = ("MASQUERADE", "sysctl", "nat", "POSTROUTING", "ip_forward", "nft", "nmcli", "DOCKER-USER")

    def test_up(self) -> None:
        up = flexcluster.network_up_commands(SPEC)
        self.assertEqual(up, [
            ["sudo", "-n", "ip", "link", "add", "virbr-aclab", "type", "bridge"],
            ["sudo", "-n", "ip", "addr", "add", "192.168.113.1/24", "dev", "virbr-aclab"],
            ["sudo", "-n", "ip", "link", "set", "virbr-aclab", "up"],
            ["sudo", "-n", "ip", "tuntap", "add", "dev", "tap-aclab1", "mode", "tap", "user", "tester"],
            ["sudo", "-n", "ip", "link", "set", "tap-aclab1", "master", "virbr-aclab"],
            ["sudo", "-n", "ip", "link", "set", "tap-aclab1", "up"],
            ["sudo", "-n", "iptables", "-I", "FORWARD", "-i", "virbr-aclab", "-o", "virbr-aclab", "-j", "ACCEPT"],
        ])

    def test_up_with_two_vms(self) -> None:
        spec = dataclasses.replace(SPEC, vms=2)
        up = [" ".join(cmd[2:]) for cmd in flexcluster.network_up_commands(spec)]
        for tap in ("tap-aclab1", "tap-aclab2"):
            self.assertIn(f"ip tuntap add dev {tap} mode tap user tester", up)
            self.assertIn(f"ip link set {tap} master virbr-aclab", up)
        down = [" ".join(cmd[2:]) for cmd in flexcluster.network_down_commands(spec)]
        self.assertIn("ip link delete tap-aclab2", down)
        present = [link("virbr-aclab", addresses=("192.168.113.1/24",)), link("tap-aclab1", master="virbr-aclab")]
        self.assertEqual([" ".join(cmd[2:]) for cmd in flexcluster.network_repair_commands(spec, present, True)],
                         ["ip tuntap add dev tap-aclab2 mode tap user tester", "ip link set tap-aclab2 master virbr-aclab",
                          "ip link set tap-aclab2 up"])

    def test_no_nat_or_sysctl(self) -> None:
        for cmd in flexcluster.network_up_commands(SPEC) + flexcluster.network_down_commands(SPEC):
            self.assertEqual(cmd[:2], ["sudo", "-n"])
            for word in self.FORBIDDEN:
                self.assertNotIn(word, cmd)

    def test_down_mirrors_up(self) -> None:
        up = flexcluster.network_up_commands(SPEC)
        down = flexcluster.network_down_commands(SPEC)
        made = [cmd[6] if cmd[3] == "tuntap" else cmd[5] for cmd in up
                if cmd[2:5] in (["ip", "link", "add"], ["ip", "tuntap", "add"])]
        deleted = [cmd[-1] for cmd in down if cmd[2:5] == ["ip", "link", "delete"]]
        self.assertEqual(made, ["virbr-aclab", "tap-aclab1"])
        # The rule goes first, then the veth attaching made, then the links in reverse.
        self.assertEqual(deleted, ["veth-aclab-kind", "tap-aclab1", "virbr-aclab"])
        inserted = next(cmd for cmd in up if "-I" in cmd)
        removed = next(cmd for cmd in down if "-D" in cmd)
        self.assertEqual(down[0], removed)
        self.assertEqual(inserted[inserted.index("-I") + 1:], removed[removed.index("-D") + 1:])

    def test_up_needs_a_tap_owner(self) -> None:
        with self.assertRaises(StepError):
            flexcluster.network_up_commands(flexcluster.ClusterSpec())

    def test_repair_from_nothing_is_up(self) -> None:
        self.assertEqual(flexcluster.network_repair_commands(SPEC, [], False), flexcluster.network_up_commands(SPEC))

    def test_repair_whole_network_is_nothing(self) -> None:
        present = [link("virbr-aclab", addresses=("192.168.113.1/24",)), link("tap-aclab1", master="virbr-aclab")]
        self.assertEqual(flexcluster.network_repair_commands(SPEC, present, True), [])

    def test_repair_keeps_an_existing_tap(self) -> None:
        # The bridge was deleted, which detached the TAP; QEMU may hold it, so it isn't recreated.
        present = [link("tap-aclab1")]
        commands = flexcluster.network_repair_commands(SPEC, present, True)
        self.assertEqual(commands, [
            ["sudo", "-n", "ip", "link", "add", "virbr-aclab", "type", "bridge"],
            ["sudo", "-n", "ip", "addr", "add", "192.168.113.1/24", "dev", "virbr-aclab"],
            ["sudo", "-n", "ip", "link", "set", "virbr-aclab", "up"],
            ["sudo", "-n", "ip", "link", "set", "tap-aclab1", "master", "virbr-aclab"],
        ])

    def test_forward_rule_count(self) -> None:
        listing = ("-P FORWARD ACCEPT\n"
                   "-A FORWARD -i virbr-aclab -o virbr-aclab -j ACCEPT\n"
                   "-A FORWARD -i virbr-aclc -j ACCEPT\n"
                   "-A FORWARD -i virbr-aclab -o virbr-aclab -j ACCEPT\n")
        self.assertEqual(flexcluster.forward_rule_count(SPEC, listing), 2)
        self.assertEqual(flexcluster.forward_rule_count(SPEC, "-P FORWARD ACCEPT\n"), 0)

    def test_attach(self) -> None:
        commands = flexcluster.attach_commands(SPEC, "4242")
        self.assertTrue(all(cmd[:2] == ["sudo", "-n"] for cmd in commands))
        self.assertIn(["sudo", "-n", "ip", "link", "set", "eth-aclab", "netns", "4242"], commands)
        self.assertIn(["sudo", "-n", "nsenter", "-t", "4242", "-n", "ip", "addr", "add", "192.168.113.2/24",
                       "dev", "eth-aclab"], commands)
        self.assertEqual(commands[0], ["sudo", "-n", "ip", "link", "add", "veth-aclab-kind", "type", "veth",
                                       "peer", "name", "eth-aclab"])


class KindConfigTest(unittest.TestCase):
    def test_cert_sans(self) -> None:
        config = flexcluster.kind_config(SPEC)
        self.assertIn("kind: Cluster\napiVersion: kind.x-k8s.io/v1alpha4\n", config)
        self.assertIn('certSANs: ["localhost", "127.0.0.1", "192.168.113.2"]', config)
        self.assertIn("kind: ClusterConfiguration", config)
        self.assertNotIn("image:", config)

    def test_node_image(self) -> None:
        config = flexcluster.kind_config(dataclasses.replace(SPEC, node_image="kindest/node:v1.37.0@sha256:ab"))
        self.assertIn('  - role: control-plane\n    image: "kindest/node:v1.37.0@sha256:ab"\n', config)

    def test_advertise_script(self) -> None:
        script = flexcluster.advertise_script(SPEC)
        self.assertIn("--node-ip=192.168.113.2", script)
        self.assertNotIn("@NODE_IP@", script)

    def test_kube_proxy_kubeconfig(self) -> None:
        text = "apiVersion: v1\nclusters:\n- cluster:\n    server: https://aclab-control-plane:6443\n  name: default\n"
        rewritten = flexcluster.kube_proxy_kubeconfig(text, SPEC.api_server)
        self.assertIn("    server: https://192.168.113.2:6443\n", rewritten)
        self.assertEqual(flexcluster.kube_proxy_kubeconfig(rewritten, SPEC.api_server), rewritten)

    def test_kindnet_patch(self) -> None:
        daemonset = {"spec": {"template": {"spec": {"containers": [
            {"name": "kindnet-cni", "env": [{"name": "HOST_IP"}, {"name": "CONTROL_PLANE_ENDPOINT",
                                                                   "value": "aclab-control-plane:6443"}]}]}}}}
        patch = flexcluster.kindnet_patch(daemonset, SPEC.api_server)
        self.assertEqual(patch["spec"]["template"]["spec"]["containers"],
                         [{"name": "kindnet-cni", "env": [{"name": "CONTROL_PLANE_ENDPOINT",
                                                           "value": "192.168.113.2:6443"}]}])
        daemonset["spec"]["template"]["spec"]["containers"][0]["env"][1]["value"] = SPEC.api_server
        self.assertIsNone(flexcluster.kindnet_patch(daemonset, SPEC.api_server))
        with self.assertRaises(StepError):
            flexcluster.kindnet_patch({"spec": {}}, SPEC.api_server)

    def test_inotify_warning(self) -> None:
        self.assertIn("sudo sysctl fs.inotify.max_user_instances=512", flexcluster.inotify_warning("128\n"))
        self.assertIsNone(flexcluster.inotify_warning("512\n"))
        self.assertIsNone(flexcluster.inotify_warning("garbage"))


class ManifestTest(unittest.TestCase):
    OBJECTS = [
        {"apiVersion": "v1", "kind": "ConfigMap", "metadata": {"name": "aks-flex-machines", "namespace": "kube-system"},
         "data": {}},
        {"apiVersion": "apps/v1", "kind": "Deployment",
         "metadata": {"name": "aks-flex-controller", "namespace": "kube-system", "labels": None},
         "spec": {"template": {"metadata": {"labels": {"app.kubernetes.io/name": "aks-flex-controller"}},
                               "spec": {"containers": [{"name": "c", "image": "ghcr.io/x:latest",
                                                        "imagePullPolicy": "Always", "args": ["--x"]}]}}}},
    ]

    def test_redact(self) -> None:
        text = "token abcdef.0123456789abcdef here\n-----BEGIN RSA PRIVATE KEY-----\nxyz\n-----END RSA PRIVATE KEY-----\n"
        redacted = flexcluster.redact(text)
        self.assertNotIn("0123456789abcdef", redacted)
        self.assertNotIn("xyz", redacted)


class RecordTest(ClusterTest):
    def changed_node(self) -> dict:
        node = copy.deepcopy(NODE)
        node["metadata"]["resourceVersion"] = "43"
        node["metadata"]["annotations"].update({f"{PREFIX}/a": "9", f"{PREFIX}/extra": "x"})
        del node["metadata"]["annotations"][f"{PREFIX}/b"]
        node["spec"]["unschedulable"] = True
        node["spec"]["taints"] = [
            {"key": "node.kubernetes.io/unschedulable", "effect": "NoSchedule", "timeAdded": "2026-11-12T11:00:00Z"},
            {"key": "scenario.io/added", "effect": "NoExecute"},
        ]
        return node

    def test_node_record(self) -> None:
        record = flexcluster.node_record(NODE, PREFIX)
        self.assertEqual(record, {
            "node": "acl-node",
            "uid": "uid-1",
            "annotations": {f"{PREFIX}/a": "1", f"{PREFIX}/b": "2"},
            "unschedulable": False,
            "taints": [{"key": "example.com/t", "effect": "NoSchedule", "value": "v"}],
        })

    def test_no_patch_when_equal(self) -> None:
        record = flexcluster.node_record(NODE, PREFIX)
        self.assertIsNone(flexcluster.reset_patch(NODE, record, PREFIX))
        reordered = copy.deepcopy(NODE)
        reordered["spec"]["taints"].reverse()
        reordered["spec"]["taints"][0]["timeAdded"] = "2026-11-12T12:00:00Z"
        self.assertIsNone(flexcluster.reset_patch(reordered, record, PREFIX))

    def test_patch_restores_the_record(self) -> None:
        record = flexcluster.node_record(NODE, PREFIX)
        patch = flexcluster.reset_patch(self.changed_node(), record, PREFIX)
        self.assertEqual(patch["metadata"]["resourceVersion"], "43")
        self.assertEqual(patch["metadata"]["annotations"],
                         {f"{PREFIX}/extra": None, f"{PREFIX}/a": "1", f"{PREFIX}/b": "2"})
        self.assertEqual(patch["spec"]["unschedulable"], None)
        # The node controller's taints stay as they are, timeAdded included.
        self.assertEqual(patch["spec"]["taints"], [
            {"key": "node.kubernetes.io/unschedulable", "effect": "NoSchedule", "timeAdded": "2026-11-12T11:00:00Z"},
            {"key": "example.com/t", "effect": "NoSchedule", "value": "v"},
        ])

    def test_patch_to_cordoned_without_taints(self) -> None:
        record = {"uid": "uid-1", "annotations": {}, "unschedulable": True, "taints": []}
        node = copy.deepcopy(NODE)
        node["spec"]["taints"] = [{"key": "example.com/t", "value": "v", "effect": "NoSchedule"}]
        patch = flexcluster.reset_patch(node, record, PREFIX)
        self.assertEqual(patch["spec"], {"unschedulable": True, "taints": None})
        self.assertEqual(set(patch["metadata"]["annotations"]), {f"{PREFIX}/a", f"{PREFIX}/b"})

    def test_uid_mismatch(self) -> None:
        record = {**flexcluster.node_record(NODE, PREFIX), "uid": "uid-0"}
        with self.assertRaisesRegex(StepError, "recreated.*rejoin"):
            flexcluster.reset_patch(NODE, record, PREFIX)

    def test_record(self) -> None:
        self.identity_rules()
        self.run.on("get node acl-node", stdout=json.dumps(NODE))
        record = self.cluster().record("acl-node", PREFIX)
        self.assertEqual(record["uid"], "uid-1")
        json.dumps(record)

    def test_reset_to(self) -> None:
        self.run.on("get node acl-node", stdout=json.dumps(self.changed_node()))
        self.cluster().reset_to("acl-node", flexcluster.node_record(NODE, PREFIX), PREFIX)
        patches = self.run.commands("patch node acl-node")
        self.assertEqual(len(patches), 1)
        self.assertEqual(json.loads(patches[0][-1])["metadata"]["resourceVersion"], "43")
        self.assertEqual(self.run.commands("delete"), [])

    def test_reset_to_uid_mismatch(self) -> None:
        self.run.on("get node acl-node", stdout=json.dumps(NODE))
        record = {**flexcluster.node_record(NODE, PREFIX), "uid": "uid-0"}
        with self.assertRaisesRegex(StepError, "rejoin"):
            self.cluster().reset_to("acl-node", record, PREFIX)
        self.assertEqual(self.run.commands("patch node"), [])
        self.assertEqual(self.run.commands("delete"), [])

    def test_reset_to_retries_a_conflict(self) -> None:
        self.run.on("get node acl-node", stdout=json.dumps(self.changed_node()))
        self.run.on("patch node", returncode=1, times=1,
                    stderr='Error from server (Conflict): Operation cannot be fulfilled on nodes "acl-node": '
                           "the object has been modified; please apply your changes to the latest version")
        self.cluster().reset_to("acl-node", flexcluster.node_record(NODE, PREFIX), PREFIX)
        self.assertEqual(len(self.run.commands("patch node")), 2)

    def test_reset_to_node_gone(self) -> None:
        with self.assertRaisesRegex(StepError, "gone"):
            self.cluster().reset_to("acl-node", flexcluster.node_record(NODE, PREFIX), PREFIX)


class NodeTest(ClusterTest):
    def test_parsing(self) -> None:
        self.assertEqual(flexcluster.condition(NODE, "Ready")["status"], "True")
        self.assertEqual(flexcluster.condition(NODE, "DiskPressure"), {})
        self.assertIn("Ready=True (KubeletReady: kubelet is posting ready status)",
                      flexcluster.describe_conditions(NODE))
        self.assertEqual(flexcluster.describe_conditions(None), "the Node doesn't exist")

    def test_node_not_found(self) -> None:
        cluster = self.cluster()
        self.assertIsNone(cluster.node("acl-node"))
        self.assertFalse(cluster.node_ready("acl-node"))
        self.assertIn("--ignore-not-found", self.run.commands("get node")[0])

    def test_node_ready_on_api_errors(self) -> None:
        self.run.on("get node", returncode=1, stderr="connection refused")
        self.assertFalse(self.cluster().node_ready("acl-node"))
        with self.assertRaises(StepError):
            self.cluster().node("acl-node")

    def test_wait_ready_times_out_with_conditions(self) -> None:
        node = copy.deepcopy(NODE)
        node["status"]["conditions"][1].update(status="False", reason="KubeletNotReady", message="CNI not ready")
        self.run.on("get node acl-node", stdout=json.dumps(node))
        with self.assertRaisesRegex(StepError, "Ready=False \\(KubeletNotReady: CNI not ready\\)"):
            self.cluster().wait_ready("acl-node", 120)


class LifecycleTest(ClusterTest):
    def create_rules(self) -> None:
        self.run.on("kind get clusters", stdout="")
        self.run.on("kind get kubeconfig", stdout="apiVersion: v1\nclusters:\n- cluster:\n    server: https://127.0.0.1:41234\n")
        self.run.on("podman inspect --format {{.State.Pid}}", stdout="4242\n")
        self.run.on("^ip -j addr show", stdout="[]")
        self.run.on("jsonpath={.status.addresses", stdout="192.168.113.2")
        self.run.on("get daemonset kindnet -o json", stdout=json.dumps({"spec": {"template": {"spec": {"containers": [
            {"name": "kindnet-cni", "env": [{"name": "CONTROL_PLANE_ENDPOINT", "value": "aclab-control-plane:6443"}]}]}}}}))
        self.run.on("get configmap kube-proxy -o json", stdout=json.dumps(
            {"data": {"kubeconfig.conf": "clusters:\n- cluster:\n    server: https://aclab-control-plane:6443\n"}}))

    def test_create(self) -> None:
        self.create_rules()
        cluster = self.cluster()
        cluster.inotify_path.write_text("128\n")
        cluster.create()
        lines = self.run.lines()
        kind_create = next(call for call in self.run.calls if "create cluster" in " ".join(call["cmd"]))
        self.assertEqual(kind_create["cmd"][:5], ["sudo", "-n", "env", "KIND_EXPERIMENTAL_PROVIDER=podman",
                                                  "/home/u/go/bin/kind"])
        self.assertTrue(kind_create["stream"])
        config = Path(kind_create["cmd"][kind_create["cmd"].index("--config") + 1])
        self.assertIn("192.168.113.2", config.read_text())
        self.assertEqual(config.parent.stat().st_mode & 0o777, 0o700)
        # The bridge exists before kind, and kind's node is attached after.
        bridge = lines.index("sudo -n ip link add virbr-aclab type bridge")
        self.assertLess(bridge, lines.index(" ".join(kind_create["cmd"])))
        self.assertLess(lines.index(" ".join(kind_create["cmd"])),
                        lines.index("sudo -n ip link set eth-aclab netns 4242"))
        self.assertTrue(any("--node-ip=192.168.113.2" in line for line in lines))
        self.assertTrue(any("patch configmap kube-proxy" in line for line in lines))
        self.assertTrue(any("patch daemonset kindnet" in line for line in lines))
        self.assertEqual(cluster.kubeconfig.stat().st_mode & 0o777, 0o600)
        self.assertIn("127.0.0.1:41234", cluster.kubeconfig.read_text())
        self.assertTrue(any("sudo sysctl fs.inotify.max_user_instances=512" in line for line in self.logs))
        for line in lines:
            self.assertFalse(line.startswith("sudo") and not line.startswith("sudo -n "), line)

    def test_kind_needs_installing(self) -> None:
        cluster = self.cluster()
        cluster.which = lambda name: None
        with self.assertRaisesRegex(StepError, "kind is needed"):
            cluster.exists()

    def whole(self) -> None:
        self.run.on("kind get clusters", stdout="aclab\nother\n")
        self.run.on("kind get kubeconfig", stdout="clusters:\n- cluster:\n    server: https://127.0.0.1:41234\n")
        self.run.on("{{.State.Running}}", stdout="true\n")
        self.run.on("{{.State.Pid}}", stdout="4242\n")
        self.run.on("iptables -S FORWARD", stdout="-P FORWARD ACCEPT\n-A FORWARD -i virbr-aclab -o virbr-aclab -j ACCEPT\n")
        self.run.on("nsenter -t 4242 -n ip -j addr show", stdout=links(link("eth-aclab", addresses=("192.168.113.2/24",))))
        self.run.on("jsonpath={.status.addresses", stdout="192.168.113.2")
        self.run.on("get node aclab-control-plane", stdout=json.dumps(NODE))

    def test_ensure_leaves_a_whole_cluster(self) -> None:
        self.run.on("^ip -j addr show", stdout=links(
            link("virbr-aclab", addresses=("192.168.113.1/24",)), link("tap-aclab1", master="virbr-aclab"),
            link("veth-aclab-kind", master="virbr-aclab")))
        self.whole()
        self.cluster().ensure()
        lines = self.run.lines()
        for mutation in ("ip link add", "ip addr add", "iptables -I", "podman start", "tuntap", "netns"):
            self.assertFalse(any(mutation in line for line in lines), mutation)

    def test_ensure_after_a_restart(self) -> None:
        self.run.on("{{.State.Running}}", stdout="false\n", times=1)
        self.run.on("iptables -S FORWARD", stdout="-P FORWARD ACCEPT\n", times=1)
        self.run.on("^ip -j addr show", stdout="[]")
        self.whole()
        self.cluster().ensure()
        lines = self.run.lines()
        self.assertIn("sudo -n ip link add virbr-aclab type bridge", lines)
        self.assertIn("sudo -n ip tuntap add dev tap-aclab1 mode tap user tester", lines)
        self.assertIn("sudo -n iptables -I FORWARD -i virbr-aclab -o virbr-aclab -j ACCEPT", lines)
        self.assertIn("sudo -n podman start aclab-control-plane", lines)
        self.assertIn("sudo -n ip link set eth-aclab netns 4242", lines)
        self.assertLess(lines.index("sudo -n ip link add virbr-aclab type bridge"),
                        lines.index("sudo -n podman start aclab-control-plane"))

    def test_ensure_without_a_cluster(self) -> None:
        with self.assertRaisesRegex(StepError, "create it"):
            self.cluster().ensure()

    def test_delete_collects_failures(self) -> None:
        self.run.on("kind delete cluster", returncode=1, stderr="ERROR: failed to delete cluster\nboom")
        self.run.on("iptables -D FORWARD", times=2)
        self.run.on("iptables -D FORWARD", returncode=1, stderr="iptables: Bad rule")
        self.run.on("ip link delete tap-aclab", returncode=1, stderr='Cannot find device "tap-aclab1"')
        self.run.on("ip link delete virbr-aclab", returncode=1, stderr="RTNETLINK answers: Operation not permitted")
        self.run.on("^ip -j addr show", stdout=links(link("virbr-aclab")))
        self.run.on("iptables -S FORWARD", stdout="-P FORWARD ACCEPT\n")
        cluster = self.cluster()
        cluster.kubeconfig.write_text("x")
        failures = cluster.delete()
        self.assertEqual(len(failures), 3, failures)
        self.assertIn("boom", failures[0])
        self.assertIn("Operation not permitted", failures[1])
        self.assertIn("virbr-aclab", failures[2])
        self.assertEqual(len(self.run.commands("iptables -D")), 3)
        self.assertFalse(cluster.kubeconfig.exists())
        lines = self.run.lines()
        self.assertLess(next(i for i, line in enumerate(lines) if "kind delete" in line),
                        next(i for i, line in enumerate(lines) if "ip link delete" in line))

    def test_delete_without_kind(self) -> None:
        self.run.on("^ip -j addr show", stdout="[]")
        cluster = self.cluster()
        cluster.which = lambda name: None
        failures = cluster.delete()
        self.assertEqual(len(failures), 1)
        self.assertIn("kind is needed", failures[0])
        self.assertIn("sudo -n ip link delete virbr-aclab", self.run.lines())

    def test_delete_clean(self) -> None:
        self.run.on("^ip -j addr show", stdout="[]")
        self.run.on("iptables -D FORWARD", returncode=1, stderr="iptables: Bad rule")
        self.run.on("ip link delete", returncode=1, stderr="Cannot find device")
        self.assertEqual(self.cluster().delete(), [])

    def test_delete_never_raises(self) -> None:
        def broken(cmd, **_):
            raise OSError("no such program")

        cluster = flexcluster.Cluster(SPEC, kubeconfig=self.tmp / "k", work_dir=self.tmp / "w",
                                      run=broken, log=self.logs.append)
        cluster.which = lambda name: "/usr/bin/kind"
        self.assertTrue(cluster.delete())

    def test_timeout_is_a_step_error(self) -> None:
        def slow(cmd, *, input=None, timeout=None, stream=False):
            raise subprocess.TimeoutExpired(cmd, timeout)

        cluster = flexcluster.Cluster(SPEC, kubeconfig=self.tmp / "k", work_dir=self.tmp / "w",
                                      run=slow, log=self.logs.append)
        with self.assertRaisesRegex(StepError, "timed out"):
            cluster.kubectl("get", "nodes")

    def test_diagnostics(self) -> None:
        self.run.on("get nodes -o yaml", stdout="token: abcdef.0123456789abcdef\n")
        self.run.on("logs deployment/machina-controller", returncode=1, stderr="not found")
        self.run.on("get events", stdout="HEADER\n" + "".join(f"event {n}\n" for n in range(300)))
        directory = self.tmp / "diag"
        self.cluster().diagnostics(directory, {"sites.yaml": ["get", "sites", "-o", "yaml"]})
        self.assertNotIn("0123456789abcdef", (directory / "nodes.yaml").read_text())
        self.assertIn("not found", (directory / "machina.log").read_text())
        self.assertTrue((directory / "sites.yaml").is_file())
        events = (directory / "events.txt").read_text().splitlines()
        self.assertEqual(events[0], "HEADER")
        self.assertEqual(len(events), 201)
        self.assertEqual(events[-1], "event 299")

    def test_diagnostics_never_raises(self) -> None:
        def broken(cmd, **_):
            raise RuntimeError("broken")

        cluster = flexcluster.Cluster(SPEC, kubeconfig=self.tmp / "k", work_dir=self.tmp / "w",
                                      run=broken, log=self.logs.append)
        cluster.diagnostics(self.tmp / "diag")
        file_in_the_way = self.tmp / "file"
        file_in_the_way.write_text("")
        cluster.diagnostics(file_in_the_way / "diag")


if __name__ == "__main__":
    unittest.main()
