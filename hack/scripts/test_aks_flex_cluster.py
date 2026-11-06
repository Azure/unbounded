# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "hack/scripts/aks-flex-cluster.sh"


class FlexClusterTests(unittest.TestCase):
    def setUp(self):
        (ROOT / "tmp").mkdir(exist_ok=True)
        self.directory = tempfile.TemporaryDirectory(dir=ROOT / "tmp")
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name)
        self.state = self.path / "state"
        self.state.mkdir()
        (self.state / "cache").mkdir()
        self.key = self.path / "key"
        self.key.write_text("test key fixture")
        self.args = [
            "--name", "test-flex", "--location", "canadacentral",
            "--subscription", "00000000-0000-0000-0000-000000000000",
            "--ssh-source-ip", "8.8.8.8",
            "--state-dir", str(self.state), "--ssh-key", str(self.key),
        ]

    def run_shell(self, code, args=(), environment=None, success=True):
        env = dict(os.environ, TEST_DIR=str(self.path))
        env.update(environment or {})
        result = subprocess.run(
            ["bash", "-c", 'source "$1"; shift; parse_args "$@"; validate_args;\n' + code,
             "test", str(SCRIPT), *self.args, *args],
            env=env, text=True, capture_output=True, timeout=15,
        )
        if success:
            self.assertEqual(result.returncode, 0, result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout)
        return result

    def test_architecture_defaults_and_explicit_vm_size(self):
        for args, arch, sku, image in (
            ([], "amd64", "Standard_D4s_v6", "server"),
            (["--worker-arch", "arm64"], "arm64", "Standard_D4ps_v6", "server-arm64"),
            (["--worker-arch", "arm64", "--vm-size", "Standard_D8ps_v6"],
             "arm64", "Standard_D8ps_v6", "server-arm64"),
        ):
            with self.subTest(arch=arch, sku=sku):
                result = self.run_shell(
                    'printf "%s\\n" "$WORKER_ARCH" "$VM_SIZE" "$WORKER_IMAGE_SKU" "$SYSTEM_VM_SIZE"',
                    args,
                )
                self.assertEqual(
                    result.stdout.splitlines(), [arch, sku, image, "Standard_D4s_v6"]
                )
        result = self.run_shell(":", ["--worker-arch", "riscv64"], success=False)
        self.assertIn("--worker-arch must be amd64 or arm64", result.stderr)

    def test_sku_architecture_check(self):
        for architecture, success in (("Arm64", True), ("x64", False), (None, False)):
            with self.subTest(architecture=architecture):
                capabilities = [] if architecture is None else [
                    {"name": "CpuArchitectureType", "value": architecture}
                ]
                result = self.run_shell(
                    'az_cmd() { printf "%s\\n" "$SKUS"; }; validate_worker_architecture',
                    ["--worker-arch", "arm64"],
                    {"SKUS": json.dumps([{"name": "Standard_D4ps_v6",
                                          "capabilities": capabilities}])},
                    success=success,
                )
                if not success:
                    self.assertIn("does not support --worker-arch arm64", result.stderr)

    def test_vmss_uses_selected_host_image(self):
        code = r'''
STATE_DIR="$TEST_DIR/state"
CONFIG_HASH=fixture
FLEX_SUBNET_ID=subnet
NSG_ID=nsg
WORKER_BACKEND_ID=backend
POOL_ID=pool
write_vmss_custom_data() { printf '#!/bin/bash\n' > "$1"; }
lookup_resource() { printf 'null\n'; }
assert_owned() { :; }
ensure_role() { :; }
az_cmd() {
  case "$*" in
    *"--method put"*) ;;
    *"--method get"*)
      jq '.identity.principalId="fixture" | .properties.provisioningState="Succeeded"' "$STATE_DIR/vmss.json" ;;
    *) return 1 ;;
  esac
}
printf 'ssh-ed25519 fixture\n' > "$STATE_DIR/ssh.pub"
ensure_vmss
cat "$STATE_DIR/vmss.json"
'''
        for arch, image in (("amd64", "server"), ("arm64", "server-arm64")):
            with self.subTest(arch=arch):
                model = json.loads(self.run_shell(code, ["--worker-arch", arch]).stdout)
                self.assertEqual(
                    model["properties"]["virtualMachineProfile"]["storageProfile"]
                    ["imageReference"]["sku"], image,
                )

    def test_agent_archive_selected_before_azure_provisioning(self):
        code = r'''
preflight() { :; }
print_plan() { :; }
prepare_state() { STATE_DIR="$TEST_DIR/state"; }
release_digest() { printf '%s\n' "$3" > "$TEST_DIR/asset"; printf '%064d\n' 0; }
fetch_file() { printf '#!/bin/bash\n' > "$2"; }
ensure_features() { exit 0; }
main --name test-flex --location canadacentral --worker-arch "$WORKER_ARCH" --yes
'''
        for arch in ("amd64", "arm64"):
            with self.subTest(arch=arch):
                self.run_shell(code, ["--worker-arch", arch])
                self.assertEqual(
                    (self.path / "asset").read_text().strip(),
                    "aks-flex-node-linux-" + arch + ".tar.gz",
                )
        failure = code.replace(
            "printf '%064d\\n' 0;", "return 1;"
        ).replace("ensure_features() { exit 0; }", "ensure_features() { touch \"$TEST_DIR/azure\"; }")
        self.run_shell(failure, ["--worker-arch", "arm64"], success=False)
        self.assertFalse((self.path / "azure").exists())

    def test_arm64_bootstrap_rejects_wrong_host(self):
        (self.state / "cache/bootstrap-v0.2.0.sh").write_text("#!/bin/bash\n")
        self.run_shell(r'''
STATE_DIR="$TEST_DIR/state"
AKS_ID=cluster
VMSS_ID=vmss
AGENT_DIGEST=fixture
write_remote_script "$TEST_DIR/remote.sh"
''', ["--worker-arch", "arm64"])
        binaries = self.path / "bin"
        binaries.mkdir()
        uname = binaries / "uname"
        uname.write_text("#!/bin/bash\nprintf 'x86_64\\n'\n")
        uname.chmod(0o700)
        result = subprocess.run(
            ["bash", str(self.path / "remote.sh")],
            env=dict(os.environ, PATH=str(binaries) + ":" + os.environ["PATH"]),
            capture_output=True, text=True, timeout=15,
        )
        self.assertEqual(result.returncode, 78)
        self.assertIn("expected an ARM64 Flex host", result.stderr)

    def test_streaming_bootstrap_preloads_nbd_before_flex(self):
        (self.state / "cache/bootstrap-v0.2.0.sh").write_text("#!/bin/bash\n")
        for args in ([], ["--artifact-streaming"],
                     ["--artifact-streaming", "--worker-arch", "arm64"]):
            with self.subTest(args=args):
                self.run_shell(r'''
AKS_ID=cluster
VMSS_ID=vmss
AGENT_DIGEST=fixture
write_remote_script "$TEST_DIR/remote.sh"
bash -n "$TEST_DIR/remote.sh"
''', args)
                script = (self.path / "remote.sh").read_text()
                if "--artifact-streaming" not in args:
                    self.assertNotIn("modprobe nbd", script)
                    continue
                start = script.index("DEBIAN_FRONTEND=noninteractive apt-get install -y -qq kmod")
                end = script.index("install -d -m 0700 /run/aks-flex-node-bootstrap", start)
                self.assertLess(end, script.index("bash /run/aks-flex-node-bootstrap/bootstrap.sh"))
                for directory in ("etc/modules-load.d", "etc/modprobe.d", "sys/module/nbd/parameters"):
                    (self.path / directory).mkdir(parents=True, exist_ok=True)
                snippet = script[start:end].replace("/etc/", str(self.path / "etc") + "/")
                snippet = snippet.replace("/sys/module/nbd/parameters",
                                          str(self.path / "sys/module/nbd/parameters"))
                for count, expected in ((1024, 0), (16, 78)):
                    (self.path / "sys/module/nbd/parameters/nbds_max").write_text(str(count))
                    result = subprocess.run(
                        ["bash", "-c", "set -e\napt-get() { :; }\nmodprobe() { :; }\n" + snippet],
                        text=True, capture_output=True, timeout=15,
                    )
                    self.assertEqual(result.returncode, expected, result.stderr)
                self.assertEqual((self.path / "etc/modules-load.d/gantry-nbd.conf").read_text(), "nbd\n")

    def test_streaming_site_selects_only_requested_flex_workers(self):
        code = r'''
kube() {
  case "$*" in
    "get deployments -A -o json")
      printf '{"items":[{"metadata":{"name":"unbounded-operator","namespace":"unbounded-system"}}]}\n' ;;
    *) : ;;
  esac
}
wait_component_rollout() { :; }
setup_cni
cat "$STATE_DIR/sites.json"
'''
        for arch in ("amd64", "arm64"):
            for enabled in (False, True):
                with self.subTest(arch=arch, enabled=enabled):
                    args = ["--worker-arch", arch] + (["--artifact-streaming"] if enabled else [])
                    sites = json.loads(self.run_shell(code, args).stdout)["items"]
                    self.assertNotIn("gantry", sites[0]["spec"].get("components", {}))
                    gantry = sites[1]["spec"].get("components", {}).get("gantry")
                    if enabled:
                        self.assertEqual(gantry, {"enabled": True, "artifactStreaming": {
                            "enabled": True, "nodeSelector": {
                                "kubernetes.io/arch": arch,
                                "kubernetes.azure.com/agentpool": "flexpool",
                            },
                        }})
                    else:
                        self.assertIsNone(gantry)

    def prepare_intent(self, args=(), success=True):
        return self.run_shell(
            'ssh-keygen() { printf "ssh-ed25519 fixture\\n"; }; prepare_state',
            args, success=success,
        )

    def test_agent_checksum_download_with_fresh_state(self):
        (self.state / "cache").rmdir()
        self.prepare_intent(["--worker-arch", "arm64"])
        result = self.run_shell(r'''
curl() {
  for destination; do :; done
  printf '%064d  aks-flex-node-linux-arm64.tar.gz\n' 0 > "$destination"
}
release_digest Azure/AKSFlexNode v0.2.0 aks-flex-node-linux-arm64.tar.gz
''', ["--worker-arch", "arm64"])
        self.assertEqual(result.stdout.strip(), "0" * 64)

    def test_default_saved_intent_stays_backward_compatible(self):
        self.prepare_intent()
        expected = {
            "subscription": "00000000-0000-0000-0000-000000000000",
            "name": "test-flex", "resourceGroup": "test-flex", "location": "canadacentral",
            "workerTopology": "vmss", "bootstrapTimeout": 3600, "flexCount": 3,
            "vmSize": "Standard_D4s_v6", "systemVMSize": "Standard_D4s_v6",
            "kubernetesVersion": "", "unboundedVersion": "v0.8.0", "agentVersion": "v0.2.0",
            "vnetCIDR": "10.91.0.0/16", "clusterPodCIDR": "10.93.0.0/16",
            "flexPodCIDR": "10.96.0.0/14", "serviceCIDR": "10.94.0.0/16",
            "sshKey": "ssh-ed25519 fixture\n",
        }
        original = json.dumps(expected, indent=2) + "\n"
        self.assertEqual((self.state / "intent.json").read_text(), original)
        self.prepare_intent()
        result = self.prepare_intent(["--worker-arch", "arm64"], success=False)
        self.assertIn("provisioning arguments differ", result.stderr)
        self.assertEqual((self.state / "intent.json").read_text(), original)

    def test_streaming_is_pinned_in_saved_intent(self):
        self.prepare_intent(["--artifact-streaming"])
        intent = json.loads((self.state / "intent.json").read_text())
        self.assertTrue(intent["artifactStreaming"])
        result = self.prepare_intent(success=False)
        self.assertIn("provisioning arguments differ", result.stderr)

    def test_custom_installer_is_pinned_and_receives_image_options(self):
        plugin = self.path / "plugin"
        plugin.write_text('#!/bin/bash\nprintf "%s\\n" "$@" > "$TEST_DIR/install-args"\n')
        plugin.chmod(0o700)
        args = [
            "--worker-arch", "arm64", "--plugin-binary", str(plugin),
            "--image-registry", "ghcr.io/example", "--operator-image",
            "unbounded-operator:branch-test",
        ]
        self.prepare_intent(args)
        intent = json.loads((self.state / "intent.json").read_text())
        self.assertEqual(intent["workerArchitecture"], "arm64")
        self.assertEqual(intent["imageRegistry"], "ghcr.io/example")
        self.run_shell(r'''
STATE_DIR="$TEST_DIR/state"
PLUGIN_DIGEST=$(sha256 "$PLUGIN_BINARY")
install_plugin
kube() {
  case "$*" in
    "get deployments -A -o json") printf '{"items":[]}\n' ;;
    *) : ;;
  esac
}
wait_component_rollout() { :; }
setup_cni
''', args)
        self.assertEqual((self.path / "install-args").read_text().splitlines(), [
            "install", "--kubeconfig", str(self.state / "kubeconfig"), "--timeout", "3600s",
            "--image-registry", "ghcr.io/example", "--operator-image",
            "unbounded-operator:branch-test",
        ])
        plugin.write_text(plugin.read_text() + "# changed installer\n")
        result = self.prepare_intent(args, success=False)
        self.assertIn("provisioning arguments differ", result.stderr)

    def test_invalid_custom_installer_options(self):
        for args in (
            ["--plugin-binary", str(self.path / "missing")],
            ["--image-registry", "https://ghcr.io/example"],
            ["--operator-image", "ghcr.io/example/unbounded-operator:test"],
        ):
            with self.subTest(args=args):
                self.run_shell(":", args, success=False)

    def test_ready_workers_must_match_requested_architecture(self):
        code = r'''
STATE_DIR="$TEST_DIR/state"
POOL_ID=pool
FLEX_COUNT=1
TIMEOUT=1
sleep() { :; }
az_cmd() {
  case "$*" in
    "vmss list-instances "*) printf '[{"instanceId":"0"}]\n' ;;
    "rest --method get "*) printf '{"value":[{"name":"test-flex-flex0","properties":{"provisioningState":"Succeeded"}}]}\n' ;;
    *) return 1 ;;
  esac
}
kube() { printf '%s\n' "$NODES"; }
wait_component_rollout() { printf '%s\n' "$1"; }
validate_deployment
'''
        for arch, streaming, success in (
                ("arm64", False, True), ("amd64", False, False), ("arm64", True, True)):
            with self.subTest(arch=arch, streaming=streaming):
                node = {
                    "metadata": {"name": "test-flex-flex0", "labels": {
                        "kubernetes.azure.com/agentpool": "flexpool",
                        "unbounded-cloud.io/site": "flex-site",
                        "kubernetes.io/arch": arch,
                    }},
                    "status": {
                        "nodeInfo": {"architecture": arch},
                        "conditions": [{"type": "Ready", "status": "True"}],
                    },
                }
                args = ["--worker-arch", "arm64"] + (["--artifact-streaming"] if streaming else [])
                result = self.run_shell(
                    code, args,
                    {"NODES": json.dumps({"items": [node]})}, success=success,
                )
                if success:
                    expected = ["daemonset/unbounded-net-node",
                                "daemonset/unbounded-net-kube-proxy-flex-site"]
                    if streaming:
                        expected += ["daemonset/gantry", "daemonset/gantry-overlaybd-config"]
                    self.assertEqual(result.stdout.splitlines(), expected)


if __name__ == "__main__":
    unittest.main()
