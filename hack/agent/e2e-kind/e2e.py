#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Agent E2E Kind test.

Creates a QEMU VM, joins it to a Kind cluster using the production
provision install script, and validates workloads run on the new node.

The test follows a single linear sequence:
  1. Start node without a Machine CR (agent self-registers).
  2. Wait for the node to become Ready.
  3. Validate the Machine CR created by the agent.
   4. Update the Machine CR version and repave counter.
  5. Wait for the node to be upgraded.
  6. Reset the node.
  7. Rejoin the node and validate again.

Options:
    --verbose                          Enable diagnostic output (network diags).
    --node-config PATH                 JSON file with node config variant settings.
Subcommands (called as individual workflow steps):
    create-vm                          Create bridge networking and launch a QEMU VM.
    ensure-kind-bridge                 Verify/repair veth pair connecting Kind to VM bridge.
    configure-kind-node-ip             Advertise the Kind control-plane's VM-bridge IP.
    run-agent                          Build agent, generate bootstrap script, run on VM.
    prepare-blocked-network-vm         Install host packages before blocking VM egress.
    block-external-network             Block VM egress outside the local e2e networks.
    unblock-external-network           Remove VM external egress block rules.
    wait-for-node                      Wait for the node to appear and become Ready.
    validate-host-nspawn-distro        Verify the nspawn machine distro matches the host.
    validate-node-config               Verify configured node and kubelet settings.
    dump-persisted-agent-config        Print persisted agent config files from the VM.
    validate-workload                  Deploy test pods on the agent node.
    validate-kube-proxy                Verify kube-proxy is Running on all nodes.
    install-machine-crd                Install Machine CRD and bootstrapper RBAC.
    deploy-unbounded-net-controller    Deploy unbounded-net controller into Kind.
    start-machina-controller           Deploy the machina controller into Kind.
    validate-machina-controller        Verify machina creates an MCV.
    validate-controllers-healthy       Verify controllers are not crashing or repeating errors.
    delete-machine-cr                  Delete the Machine CR.
    validate-machine-cr-created        Verify agent self-registered a Machine CR.
    validate-node-reboot-operation     Verify NodeReboot MachineOperation restarts the node.
    validate-agent-upgrade-operation   Verify AgentUpgrade switches the host daemon binary.
    validate-agent-upgrade-rollback    Verify AgentUpgrade rollback restores last-known-good.
    validate-node-repave-upgrade       Verify OnDelete repave applies a new MCV Kubernetes version.
                                       Also verifies private bpffs isolation across repave.
    validate-node-configs              Discover and validate node config scenarios in parallel.
    collect-logs                       Collect VM and cluster diagnostic logs.
    reset-agent                        Trigger AgentReset and verify cleanup.
    cleanup                            Tear down VM, networking, and Kind cluster.
"""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import hashlib
import json
import os
import re
import secrets
import shutil
import subprocess
import sys
import textwrap
import time
from dataclasses import dataclass, field, replace
from http.server import HTTPServer, SimpleHTTPRequestHandler
from pathlib import Path
from threading import Thread
from typing import Any, Callable

# ---------------------------------------------------------------------------
# Paths and defaults
# ---------------------------------------------------------------------------
REPO_ROOT = Path(__file__).resolve().parent.parent.parent.parent

VM_NAME = os.environ.get("VM_NAME", "agent-e2e")
VM_MEMORY = os.environ.get("VM_MEMORY", "4096")
VM_CPUS = os.environ.get("VM_CPUS", "2")
VM_DISK_SIZE = os.environ.get("VM_DISK_SIZE", "20G")
VM_SUBNET = os.environ.get("VM_SUBNET", "192.168.100")
VM_IP = os.environ.get("VM_IP", f"{VM_SUBNET}.10")
VM_GATEWAY = f"{VM_SUBNET}.1"
VM_DIR = Path(os.environ.get("VM_DIR", str(REPO_ROOT / ".vm-e2e")))
HOST_BASE_OS = os.environ.get("HOST_BASE_OS", "ubuntu2404")
HOST_IMAGE_URL = os.environ.get("HOST_IMAGE_URL", "")
VM_SSH_USER = os.environ.get("VM_SSH_USER", "ubuntu")
NODE_CONFIG_DIR = REPO_ROOT / "hack" / "agent" / "e2e-kind" / "node-configs"

KIND_CLUSTER_NAME = os.environ.get("KIND_CLUSTER_NAME", "kind")
KIND_CONTAINER = f"{KIND_CLUSTER_NAME}-control-plane"
AGENT_MACHINE_NAME = os.environ.get("AGENT_MACHINE_NAME", "agent-e2e")
AGENT_DEBUG = os.environ.get("AGENT_DEBUG", "")
OFFLINE_BOOTSTRAP = os.environ.get("OFFLINE_BOOTSTRAP", "").lower() in ("1", "true", "yes")

# Site name used when generating the bootstrap script via kubectl-unbounded.
E2E_SITE_NAME = os.environ.get("E2E_SITE_NAME", "e2e")

# Fixed nspawn machine names used by unbounded-agent (decoupled from the kube node name).
NSPAWN_MACHINE_NAMES = ["kube1", "kube2"]

BRIDGE_NAME = "virbr-e2e"
TAP_NAME = os.environ.get("TAP_NAME", "tap-e2e")
SERVE_PORT = 8199
AGENT_UPGRADE_ROLLBACK_MESSAGE_FRAGMENT = "rolled back"

# Set to True by --verbose flag; gates diagnostic output.
VERBOSE = False

SSH_KEY_DIR = VM_DIR / "ssh"
SSH_KEY = SSH_KEY_DIR / "id_ed25519"
SSH_OPTS = [
    "-o", "StrictHostKeyChecking=no",
    "-o", "UserKnownHostsFile=/dev/null",
    "-o", "ConnectTimeout=10",
    "-i", str(SSH_KEY),
]
SSH_TARGET = f"{VM_SSH_USER}@{VM_IP}"

KUBECTL = "kubectl"
KUBECTL_UNBOUNDED = str(REPO_ROOT / "bin" / "kubectl-unbounded")
CONTAINER_ENGINE = os.environ.get("CONTAINER_ENGINE", "docker")
MACHINA_E2E_IMAGE = os.environ.get("MACHINA_E2E_IMAGE", "machina:agent-e2e")
NET_CONTROLLER_E2E_IMAGE = os.environ.get(
    "NET_CONTROLLER_E2E_IMAGE",
    "unbounded-net-controller:agent-e2e",
)
AGENT_ARTIFACTS_BUILDER = str(REPO_ROOT / "bin" / "agent-artifacts-builder")
ORAS = str(REPO_ROOT / "bin" / "oras")
LOCAL_ARTIFACT_REGISTRY_NAME = f"{KIND_CLUSTER_NAME}-bootstrap-artifacts-registry"
LOCAL_ARTIFACT_REGISTRY_PORT = os.environ.get("LOCAL_ARTIFACT_REGISTRY_PORT", "5000")
ORAS_LOGGED_IN_REGISTRIES: set[str] = set()

TEST_NS = "e2e-workload-test"
# Unified install namespace for unbounded components (machina, net, ...).
# Matches the Makefile UNBOUNDED_NAMESPACE default that the rendered
# manifests deploy into.
UNBOUNDED_NS = "unbounded-system"
E2E_WORKLOAD_IMAGE = "docker.io/library/busybox:1.36"
MACHINE_CONFIG_NAME = f"{AGENT_MACHINE_NAME}-config"
DAEMON_BINARY_CURRENT = "/usr/local/bin/unbounded-agent-current"
DAEMON_BINARY_LAST_GOOD = "/usr/local/bin/unbounded-agent-last-good"
BPFFS_SENTINEL = "unbounded-e2e-bpffs-sentinel"


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
def log(msg: str) -> None:
    print(f"[INFO]  {msg}", flush=True)


def die(msg: str) -> None:
    print(f"[ERROR] {msg}", file=sys.stderr, flush=True)
    sys.exit(1)


def diag(msg: str) -> None:
    """Print a diagnostic message only when --verbose is active."""
    if VERBOSE:
        print(f"[DIAG]  {msg}", flush=True)


def _nm_unmanage(iface: str) -> None:
    """Tell NetworkManager to leave *iface* alone.

    NetworkManager can silently detach interfaces from their bridge master.
    Calling ``nmcli device set <iface> managed no`` prevents this.  The
    setting is runtime-only (resets when NM restarts) so it does not touch
    any persistent configuration files.

    No-op if ``nmcli`` is not installed or if the command fails (NM may not
    be running).
    """
    if shutil.which("nmcli") is None:
        return
    result = subprocess.run(
        ["sudo", "nmcli", "device", "set", iface, "managed", "no"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False,
    )
    if result.returncode == 0:
        diag(f"Told NetworkManager to ignore {iface}")
    else:
        diag(f"nmcli device set {iface} managed no failed (rc={result.returncode}), continuing")


def run(args: list[str], **kw: Any) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, check=True, **kw)


def run_quiet(args: list[str], **kw: Any) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, **kw,
    )


def download_file(url: str, destination: Path) -> None:
    run([
        "curl",
        "-fsSL",
        "--connect-timeout", "30",
        "--retry", "5",
        "--retry-delay", "5",
        "--retry-all-errors",
        "--remove-on-error",
        "-o", str(destination),
        url,
    ])


def capture(args: list[str], **kw: Any) -> str:
    result = subprocess.run(args, capture_output=True, text=True, **kw)
    if result.returncode != 0:
        raise subprocess.CalledProcessError(result.returncode, args, result.stdout, result.stderr)
    return result.stdout.strip()


def ssh_cmd(*remote_args: str) -> subprocess.CompletedProcess[str]:
    return run(["ssh", *SSH_OPTS, SSH_TARGET, *remote_args])


def ssh_capture(command: str) -> str:
    return capture(["ssh", *SSH_OPTS, SSH_TARGET, command])


def ssh_capture_quiet(command: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["ssh", *SSH_OPTS, SSH_TARGET, command],
        capture_output=True,
        text=True,
        check=False,
    )


def scp_cmd(src: str, dst: str) -> subprocess.CompletedProcess[str]:
    return run(["scp", *SSH_OPTS, src, dst])


def scp_from_vm(src: str, dst: Path) -> subprocess.CompletedProcess[str]:
    return run(["scp", *SSH_OPTS, f"{SSH_TARGET}:{src}", str(dst)])


def kubectl(args: list[str], **kw: Any) -> subprocess.CompletedProcess[str]:
    return run([KUBECTL, *args], **kw)


def kubectl_capture(args: list[str]) -> str:
    return capture([KUBECTL, *args])


def kind_api_server_url() -> str:
    """Return the Kind API server URL reachable from the VM bridge."""

    kind_ip = capture([
        "docker", "inspect", KIND_CONTAINER,
        "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
    ])
    if not kind_ip:
        die("Could not determine Kind control-plane container IP")

    return f"https://{kind_ip}:6443"


def image_context_name(image: str) -> str:
    """Return a filesystem-safe name for an e2e image build context."""

    return re.sub(r"[^A-Za-z0-9_.-]+", "-", image).strip("-") or "image"


def build_e2e_controller_image(image: str, binary_path: Path, entrypoint: str) -> None:
    """Build a small local controller image from an already-built binary."""

    if shutil.which(CONTAINER_ENGINE) is None:
        die(f"{CONTAINER_ENGINE} is required but not found in PATH")

    if not binary_path.exists():
        die(f"Controller binary not found: {binary_path}")

    context_dir = VM_DIR / "images" / image_context_name(image)
    context_dir.mkdir(parents=True, exist_ok=True)
    image_binary = context_dir / entrypoint
    dockerfile = context_dir / "Containerfile"
    shutil.copy2(binary_path, image_binary)
    dockerfile.write_text(textwrap.dedent(f"""\
        FROM ubuntu:24.04
        COPY {entrypoint} /usr/local/bin/{entrypoint}
        ENTRYPOINT ["/usr/local/bin/{entrypoint}"]
    """))

    log(f"Building local e2e image {image}...")
    run([CONTAINER_ENGINE, "build", "-t", image, "-f", str(dockerfile), str(context_dir)])


def load_image_into_kind(image: str) -> None:
    """Load a locally-built image into the Kind cluster."""

    if shutil.which("kind") is None:
        die("kind is required but not found in PATH")

    log(f"Loading image {image} into Kind cluster '{KIND_CLUSTER_NAME}'...")
    if CONTAINER_ENGINE == "docker":
        run(["kind", "load", "docker-image", image, "--name", KIND_CLUSTER_NAME])
        return

    archive = VM_DIR / f"{image_context_name(image)}.tar"
    run([CONTAINER_ENGINE, "save", "-o", str(archive), image])
    run(["kind", "load", "image-archive", str(archive), "--name", KIND_CLUSTER_NAME])


def apply_manifest(path: Path) -> None:
    """Apply a manifest file, failing with a clear message when missing."""

    if not path.exists():
        die(f"Manifest not found: {path}")

    kubectl(["apply", "-f", str(path)])


def set_manifest_image_pull_policy(path: Path, policy: str) -> None:
    """Set the single imagePullPolicy in a rendered manifest."""

    if not path.exists():
        die(f"Manifest not found: {path}")

    old = "imagePullPolicy: Always"
    new = f"imagePullPolicy: {policy}"
    contents = path.read_text()
    if contents.count(old) != 1:
        die(f"Expected exactly one '{old}' entry in {path}")

    path.write_text(contents.replace(old, new, 1))


def wait_for_rollout(namespace: str, resource: str, timeout: str = "180s") -> None:
    """Wait for a Kubernetes workload rollout."""

    kubectl(["-n", namespace, "rollout", "status", resource, f"--timeout={timeout}"])


def print_controller_logs(namespace: str, label: str) -> None:
    """Print current and previous logs for matching controller pods."""

    subprocess.run(
        [KUBECTL, "logs", "-n", namespace, "--all-containers", "--prefix", "-l", label],
        check=False,
    )
    subprocess.run(
        [
            KUBECTL, "logs", "-n", namespace, "--all-containers", "--prefix",
            "--previous", "-l", label,
        ],
        check=False,
    )


def _normalize_controller_error_line(line: str) -> str:
    """Normalize volatile fields in controller log lines before comparing."""

    line = line.strip()
    if not line:
        return ""

    parts = line.split(maxsplit=1)
    if len(parts) == 2 and "/" in parts[0]:
        line = parts[1]

    line = re.sub(r"\b\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z?\b", "<ts>", line)
    line = re.sub(r"\b[0-9a-f]{8,}\b", "<hex>", line)
    return re.sub(r"\s+", " ", line)


def _controller_logs(namespace: str, label: str, previous: bool = False) -> str:
    args = [
        KUBECTL, "logs", "-n", namespace, "--all-containers", "--prefix",
        "-l", label,
    ]
    if previous:
        args.insert(-2, "--previous")

    result = subprocess.run(args, capture_output=True, text=True, check=False)
    if result.returncode != 0 and not previous:
        raise subprocess.CalledProcessError(result.returncode, args, result.stdout, result.stderr)

    return result.stdout


def _validate_controller_health(name: str, namespace: str, label: str) -> None:
    log(f"Checking {name} controller pod health...")
    pod_list = json.loads(kubectl_capture([
        "get", "pods", "-n", namespace, "-l", label, "-o", "json",
    ]))
    pods = pod_list.get("items", [])
    if not pods:
        die(f"{name} controller has no pods matching {label}")

    unhealthy: list[str] = []
    for pod in pods:
        pod_name = pod["metadata"]["name"]
        phase = pod.get("status", {}).get("phase", "")
        if phase != "Running":
            unhealthy.append(f"{pod_name}: phase={phase}")

        for status in pod.get("status", {}).get("containerStatuses", []):
            container = status.get("name", "<unknown>")
            restart_count = int(status.get("restartCount", 0))
            if restart_count != 0:
                unhealthy.append(f"{pod_name}/{container}: restartCount={restart_count}")

            state = status.get("state", {})
            waiting = state.get("waiting")
            if waiting:
                reason = waiting.get("reason", "")
                if reason in {"CrashLoopBackOff", "Error"}:
                    unhealthy.append(f"{pod_name}/{container}: waiting={reason}")

            terminated = state.get("terminated")
            if terminated and int(terminated.get("exitCode", 0)) != 0:
                unhealthy.append(
                    f"{pod_name}/{container}: terminated={terminated.get('reason', '')}"
                )

    if unhealthy:
        print_controller_logs(namespace, label)
        die(f"{name} controller unhealthy: {'; '.join(unhealthy)}")

    log(f"Checking {name} controller logs for repeated errors...")
    repeated_errors: dict[str, int] = {}
    for line in _controller_logs(namespace, label).splitlines():
        if not re.search(r"\b(error|fatal|panic)\b|level=error|\"level\":\"error\"", line, re.I):
            continue

        normalized = _normalize_controller_error_line(line)
        if not normalized:
            continue

        repeated_errors[normalized] = repeated_errors.get(normalized, 0) + 1

    repeated_errors = {
        line: count for line, count in repeated_errors.items()
        if count >= 3
    }
    if repeated_errors:
        print_controller_logs(namespace, label)
        details = "; ".join(
            f"{count}x {line}" for line, count in sorted(repeated_errors.items())
        )
        die(f"{name} controller has repeating error logs: {details}")

    previous_logs = _controller_logs(namespace, label, previous=True).strip()
    if previous_logs:
        print(previous_logs, flush=True)
        die(f"{name} controller has previous container logs")

    log(f"{name} controller is healthy")


def active_nspawn_machine() -> str:
    for machine in NSPAWN_MACHINE_NAMES:
        result = ssh_capture_quiet(f"sudo machinectl show {machine}")
        if result.returncode == 0:
            return machine
    die("no active nspawn machine found")


def machine_shell(machine: str, command: str) -> str:
    quoted = json.dumps(command)
    return ssh_capture(f"sudo machinectl shell {machine} /bin/sh -lc {quoted}")


def machine_shell_quiet(machine: str, command: str) -> subprocess.CompletedProcess[str]:
    quoted = json.dumps(command)
    return ssh_capture_quiet(f"sudo machinectl shell {machine} /bin/sh -lc {quoted}")


def nspawn_os_release(machine: str) -> str:
    result = ssh_capture_quiet(f"sudo cat /var/lib/machines/{machine}/etc/os-release")
    if result.returncode == 0 and result.stdout.strip():
        return result.stdout

    diag(f"Could not read /var/lib/machines/{machine}/etc/os-release directly; trying machinectl shell")
    return machine_shell(machine, "cat /etc/os-release")


def parse_os_release(content: str) -> dict[str, str]:
    values: dict[str, str] = {}
    for raw_line in content.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key.strip()] = value.strip().strip("\"'")
    return values


def normalize_os_release_id(value: str) -> str:
    return "".join(ch for ch in value.lower() if ch.isalnum())


def is_rpm_based_os_release_id(value: str) -> bool:
    return normalize_os_release_id(value) in {
        "almalinux",
        "amzn",
        "azurelinux",
        "centos",
        "fedora",
        "ol",
        "rhel",
        "rocky",
        "sles",
        "suse",
    }


def is_rpm_based_os_release(values: dict[str, str]) -> bool:
    if is_rpm_based_os_release_id(values.get("ID", "")):
        return True
    return any(is_rpm_based_os_release_id(token) for token in values.get("ID_LIKE", "").split())


def distro_from_os_release(values: dict[str, str]) -> str:
    distro_id = normalize_os_release_id(values.get("ID", ""))
    version_id = values.get("VERSION_ID", "")
    if distro_id == "ubuntu":
        if version_id.startswith("24.04"):
            return "ubuntu2404"
        if version_id.startswith("26.04"):
            return "ubuntu2604"
    if distro_id in {"azurelinux", "azlinux"} and version_id.startswith("3"):
        return "azlinux3"
    if is_rpm_based_os_release(values):
        return "rpm"
    return ""


def expected_nspawn_distro_for_host(host_distro: str) -> str:
    if host_distro in {"ubuntu2404", "ubuntu2604"}:
        return host_distro
    if host_distro in {"azlinux3", "rpm"}:
        return "azlinux3"
    return "ubuntu2404"


def validate_host_nspawn_distro() -> None:
    host_release = parse_os_release(ssh_capture("cat /etc/os-release"))
    host_distro = distro_from_os_release(host_release)
    if not host_distro:
        die(f"unsupported host distro from /etc/os-release: ID={host_release.get('ID', '')!r} "
            f"VERSION_ID={host_release.get('VERSION_ID', '')!r}")

    machine = active_nspawn_machine()
    nspawn_release = parse_os_release(nspawn_os_release(machine))
    nspawn_distro = distro_from_os_release(nspawn_release)
    expected = expected_nspawn_distro_for_host(host_distro)
    if nspawn_distro != expected:
        die(f"host/nspawn distro mismatch: host={host_distro}, "
            f"nspawn={nspawn_distro or '<unknown>'}, expected nspawn={expected}")

    log(f"Host/nspawn distro pair is valid: host={host_distro}, "
        f"nspawn={nspawn_distro}, machine={machine}")


def create_bpffs_sentinel(machine: str) -> None:
    log(f"Creating bpffs sentinel in nspawn machine '{machine}'...")
    machine_shell(machine, textwrap.dedent(f"""\
        mountpoint -q /sys/fs/bpf
        rm -f /sys/fs/bpf/{BPFFS_SENTINEL}
        bpftool map create /sys/fs/bpf/{BPFFS_SENTINEL} type hash key 4 value 4 entries 1 name unb_e2e
        test -e /sys/fs/bpf/{BPFFS_SENTINEL}
    """))


def assert_bpffs_sentinel_absent(machine: str) -> None:
    result = machine_shell_quiet(machine, f"test ! -e /sys/fs/bpf/{BPFFS_SENTINEL}")
    if result.returncode != 0:
        die(f"bpffs sentinel from previous machine is visible in '{machine}'")


def _b64(val: str) -> str:
    """Base64-encode a string (no newlines)."""
    return base64.b64encode(val.encode()).decode()


@dataclass(frozen=True)
class NodeConfig:
    name: str
    node_labels: dict[str, str]
    register_with_taints: list[str]
    node_ip: str = ""
    kubelet_configuration: dict[str, Any] = field(default_factory=dict)
    offline_artifacts_oci_ref: str = ""
    rootfs_oci_image: str = ""
    block_external_network: bool = False
    additional_host_mounts: tuple[dict[str, Any], ...] = ()
    additional_host_devices: tuple[str, ...] = ()
    path: str = ""


def load_node_config(
    path: str | None,
    offline_artifacts_oci_ref_override: str = "",
    offline_rootfs_oci_image_override: str = "",
) -> NodeConfig:
    """Load a node config variant from *path*, or return the default config."""
    if not path:
        return NodeConfig(
            name="default",
            node_labels={},
            register_with_taints=[],
            offline_artifacts_oci_ref=offline_artifacts_oci_ref_override,
            rootfs_oci_image=offline_rootfs_oci_image_override,
        )

    config_path = Path(path)
    if not config_path.is_absolute():
        config_path = REPO_ROOT / config_path

    try:
        cfg = json.loads(config_path.read_text())
    except Exception as exc:
        die(f"failed to read node config {config_path}: {exc}")

    if not isinstance(cfg, dict):
        die(f"node config {config_path} must contain a JSON object")

    name = cfg.get("name", config_path.stem)
    node_labels = cfg.get("nodeLabels", {})
    register_with_taints = cfg.get("registerWithTaints", [])
    node_ip = cfg.get("nodeIP", "")
    kubelet_configuration = cfg.get("kubeletConfiguration", {})
    offline_artifacts_oci_ref = offline_artifacts_oci_ref_override or cfg.get("offlineArtifactsOCIRef", "")
    rootfs_oci_image = offline_rootfs_oci_image_override or cfg.get("offlineRootfsOCIImage", "")
    block_external_network = cfg.get("blockExternalNetwork", False)
    additional_host_mounts = cfg.get("additionalHostMounts", [])
    additional_host_devices = cfg.get("additionalHostDevices", [])

    if not isinstance(name, str) or not name:
        die(f"node config {config_path} field 'name' must be a non-empty string")
    if not isinstance(node_labels, dict) or not all(
        isinstance(key, str) and isinstance(value, str)
        for key, value in node_labels.items()
    ):
        die(f"node config {config_path} field 'nodeLabels' must be an object of string values")
    if not isinstance(register_with_taints, list) or not all(
        isinstance(taint, str) for taint in register_with_taints
    ):
        die(f"node config {config_path} field 'registerWithTaints' must be a list of strings")
    if not isinstance(node_ip, str):
        die(f"node config {config_path} field 'nodeIP' must be a string")
    if not isinstance(kubelet_configuration, dict):
        die(f"node config {config_path} field 'kubeletConfiguration' must be an object")
    if not isinstance(offline_artifacts_oci_ref, str):
        die(f"node config {config_path} field 'offlineArtifactsOCIRef' must be a string")
    if not isinstance(rootfs_oci_image, str):
        die(f"node config {config_path} field 'offlineRootfsOCIImage' must be a string")
    if not isinstance(block_external_network, bool):
        die(f"node config {config_path} field 'blockExternalNetwork' must be a boolean")
    if not isinstance(additional_host_mounts, list) or not all(
        isinstance(m, dict)
        and isinstance(m.get("source", ""), str) and m.get("source", "")
        and isinstance(m.get("target", ""), str)
        and isinstance(m.get("readOnly", False), bool)
        for m in additional_host_mounts
    ):
        die(
            f"node config {config_path} field 'additionalHostMounts' must be a list of objects "
            f"with string 'source', optional string 'target', and optional bool 'readOnly'"
        )
    if not isinstance(additional_host_devices, list) or not all(
        isinstance(d, str) and d for d in additional_host_devices
    ):
        die(
            f"node config {config_path} field 'additionalHostDevices' must be a list of "
            f"non-empty strings"
        )

    return NodeConfig(
        name=name,
        node_labels=dict(node_labels),
        register_with_taints=list(register_with_taints),
        node_ip=node_ip,
        kubelet_configuration=dict(kubelet_configuration),
        offline_artifacts_oci_ref=offline_artifacts_oci_ref,
        rootfs_oci_image=rootfs_oci_image,
        block_external_network=block_external_network,
        additional_host_mounts=tuple(dict(m) for m in additional_host_mounts),
        additional_host_devices=tuple(additional_host_devices),
        path=str(config_path),
    )


def expected_node_labels(node_config: NodeConfig) -> dict[str, str]:
    """Return labels configured for this e2e node variant."""
    return dict(node_config.node_labels)


def expected_node_taint_strings(node_config: NodeConfig) -> list[str]:
    """Return configured taint strings for this e2e node variant."""
    return list(node_config.register_with_taints)


def expected_node_ip(node_config: NodeConfig) -> str:
    """Return the expected Node InternalIP for this e2e node variant."""
    node_ip = node_config.node_ip
    if node_ip in ("$VM_IP", "${VM_IP}"):
        return VM_IP
    return node_ip or VM_IP


def expected_node_taints(node_config: NodeConfig) -> list[dict[str, str]]:
    """Return taints configured for this e2e node variant."""
    taints: list[dict[str, str]] = []
    for item in expected_node_taint_strings(node_config):
        if ":" not in item:
            die(f"invalid registerWithTaints entry {item!r}, expected key[=value]:Effect")
        body, effect = item.rsplit(":", 1)
        if "=" in body:
            key, value = body.split("=", 1)
        else:
            key, value = body, ""
        if not key or not effect:
            die(f"invalid registerWithTaints entry {item!r}, key and effect are required")
        taints.append({"key": key, "value": value, "effect": effect})
    return taints


def node_config_bootstrap_args(node_config: NodeConfig) -> list[str]:
    """Return manual-bootstrap flags for the active node config variant."""
    args: list[str] = []
    if node_config.node_ip:
        args.extend(["--node-ip", expected_node_ip(node_config)])
    for key, value in sorted(expected_node_labels(node_config).items()):
        args.extend(["--node-label", f"{key}={value}"])
    for taint in expected_node_taint_strings(node_config):
        args.extend(["--register-with-taint", taint])
    if node_config.rootfs_oci_image:
        args.extend(["--oci-image", node_config.rootfs_oci_image])
    for mount in node_config.additional_host_mounts:
        spec = mount["source"]
        if mount.get("target"):
            spec += ":" + mount["target"]
        if mount.get("readOnly"):
            spec += ":ro"
        args.extend(["--additional-host-mount", spec])
    for device in node_config.additional_host_devices:
        args.extend(["--additional-host-device", device])
    return args


def inject_kubelet_configuration(bootstrap_script: str, node_config: NodeConfig) -> str:
    """Inject a scenario's kubelet configuration into generated agent config JSON."""
    if not node_config.kubelet_configuration:
        return bootstrap_script

    start_marker = "cat > \"${UNBOUNDED_AGENT_CONFIG_FILE}\" <<'AGENT_CONFIG_EOF'\n"
    end_marker = "\nAGENT_CONFIG_EOF"
    prefix, separator, remainder = bootstrap_script.partition(start_marker)
    if not separator:
        die("generated bootstrap script does not contain the agent config heredoc")

    agent_config_json, separator, suffix = remainder.partition(end_marker)
    if not separator:
        die("generated bootstrap script has an unterminated agent config heredoc")

    try:
        agent_config = json.loads(agent_config_json)
        kubelet = agent_config["Kubelet"]
    except (KeyError, TypeError, json.JSONDecodeError) as exc:
        die(f"generated bootstrap script contains invalid agent config: {exc}")

    kubelet["Configuration"] = node_config.kubelet_configuration
    rendered_config = json.dumps(agent_config, indent=2)

    return prefix + start_marker + rendered_config + end_marker + suffix


def log_active_node_config(node_config: NodeConfig) -> None:
    """Log the active e2e node config variant."""
    labels = [f"{key}={value}" for key, value in sorted(expected_node_labels(node_config).items())]
    taints = expected_node_taint_strings(node_config)
    log(f"Agent e2e node config variant: {node_config.name}")
    log(f"  node ip: {expected_node_ip(node_config) if node_config.node_ip else '<default>'}")
    log(f"  node labels: {', '.join(labels) if labels else '<none>'}")
    log(f"  register-with-taints: {', '.join(taints) if taints else '<none>'}")
    kubelet_configuration = (
        json.dumps(node_config.kubelet_configuration, sort_keys=True)
        if node_config.kubelet_configuration
        else "<none>"
    )
    log(f"  kubelet configuration: {kubelet_configuration}")
    log(f"  offline artifacts OCI ref: {node_config.offline_artifacts_oci_ref or '<none>'}")
    log(f"  rootfs OCI image: {node_config.rootfs_oci_image or '<default>'}")
    log(f"  block external network: {node_config.block_external_network}")
    if node_config.additional_host_mounts:
        mounts = [
            f"{m['source']}" + (f":{m['target']}" if m.get("target") else "")
            + (" (ro)" if m.get("readOnly") else "")
            for m in node_config.additional_host_mounts
        ]
        log(f"  additional host mounts: {', '.join(mounts)}")
    else:
        log(f"  additional host mounts: <none>")
    if node_config.additional_host_devices:
        log(f"  additional host devices: {', '.join(node_config.additional_host_devices)}")
    else:
        log(f"  additional host devices: <none>")


def _safe_name(value: str) -> str:
    """Return a DNS-label-safe name fragment for VM and node names."""
    safe = re.sub(r"[^a-z0-9-]+", "-", value.lower()).strip("-")
    return safe or "config"


def qemu_mac_address() -> str:
    """Return a stable, per-VM MAC address for the QEMU tap interface."""
    try:
        octets = [int(part) for part in VM_IP.split(".")]
        if len(octets) == 4 and all(0 <= part <= 255 for part in octets):
            return f"52:54:00:{octets[1]:02x}:{octets[2]:02x}:{octets[3]:02x}"
    except ValueError:
        pass

    digest = hashlib.sha256(f"{VM_NAME}-{VM_IP}".encode()).digest()
    return f"52:54:00:{digest[0]:02x}:{digest[1]:02x}:{digest[2]:02x}"


def discover_node_configs() -> list[NodeConfig]:
    """Load all node config scenario files in deterministic order."""
    configs: list[NodeConfig] = []
    for path in sorted(NODE_CONFIG_DIR.glob("*.json")):
        configs.append(load_node_config(str(path)))
    if not configs:
        die(f"No node config scenarios found in {NODE_CONFIG_DIR}")
    return configs


def scenario_env(node_config: NodeConfig, index: int) -> dict[str, str]:
    """Return per-scenario environment overrides for a parallel e2e node."""
    name = _safe_name(node_config.name)
    vm_name = f"{VM_NAME}-{name}"
    return {
        "VM_NAME": vm_name,
        "AGENT_MACHINE_NAME": vm_name,
        "E2E_SITE_NAME": f"{E2E_SITE_NAME}-{name}",
        "VM_IP": f"{VM_SUBNET}.{10 + index}",
        "VM_DIR": str(VM_DIR / name),
        "TAP_NAME": f"tap-e2e-{index}",
    }


def _machine_operation_resource() -> str:
    """Return the fully-qualified MachineOperation resource name."""
    return "machineoperations.v1alpha3.unbounded-cloud.io"


def create_machine_operation(
    name: str,
    machine_name: str,
    operation_kind: str,
    parameters: dict[str, str] | None = None,
    ttl_seconds: int | None = None,
) -> None:
    """Create a MachineOperation CR targeting *machine_name*."""

    spec: dict[str, Any] = {
        "machineRef": machine_name,
        "operationKind": operation_kind,
    }
    if ttl_seconds is not None:
        spec["ttlSecondsAfterFinished"] = ttl_seconds
    if parameters is not None:
        spec["parameters"] = parameters

    operation = {
        "apiVersion": "unbounded-cloud.io/v1alpha3",
        "kind": "MachineOperation",
        "metadata": {"name": name},
        "spec": spec,
    }

    log(f"Creating MachineOperation '{name}' ({operation_kind}) for '{machine_name}'...")
    kubectl(["apply", "-f", "-"], input=json.dumps(operation).encode())


def wait_for_machine_operation_complete(name: str, timeout_secs: int = 180) -> dict[str, Any]:
    """Wait for a MachineOperation to complete and return its JSON object."""

    log(f"Waiting for MachineOperation '{name}' to complete (timeout: {timeout_secs}s)...")
    elapsed = 0
    resource = _machine_operation_resource()
    last_phase = ""
    last_message = ""

    while elapsed < timeout_secs:
        result = subprocess.run(
            [KUBECTL, "get", resource, name, "-o", "json"],
            capture_output=True, text=True,
        )
        if result.returncode == 0:
            operation = json.loads(result.stdout)
            status = operation.get("status", {})
            phase = status.get("phase", "")
            message = status.get("message", "")
            if phase != last_phase or message != last_message:
                log(f"  MachineOperation phase={phase or '<empty>'}")
                last_phase = phase
                last_message = message
            if phase == "Complete":
                log(f"MachineOperation '{name}' completed after {elapsed}s")
                return operation
            if phase == "Failed":
                die(f"MachineOperation '{name}' failed: {message}")

        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) MachineOperation not complete yet")
        time.sleep(5)
        elapsed += 5

    subprocess.run([KUBECTL, "get", resource, name, "-o", "yaml"], check=False)
    die(f"Timed out waiting for MachineOperation '{name}' to complete after {timeout_secs}s")


def wait_for_machine_operation_failed(
    name: str,
    timeout_secs: int = 180,
    allow_complete_before_failure: bool = False,
) -> dict[str, Any]:
    """Wait for a MachineOperation to fail and return its JSON object."""

    log(f"Waiting for MachineOperation '{name}' to fail (timeout: {timeout_secs}s)...")
    elapsed = 0
    resource = _machine_operation_resource()
    last_phase = ""
    last_message = ""

    while elapsed < timeout_secs:
        result = subprocess.run(
            [KUBECTL, "get", resource, name, "-o", "json"],
            capture_output=True, text=True,
        )
        if result.returncode == 0:
            operation = json.loads(result.stdout)
            status = operation.get("status", {})
            phase = status.get("phase", "")
            message = status.get("message", "")
            if phase != last_phase or message != last_message:
                log(f"  MachineOperation phase={phase or '<empty>'}")
                last_phase = phase
                last_message = message
            if phase == "Failed":
                log(f"MachineOperation '{name}' failed after {elapsed}s")
                return operation
            if phase == "Complete" and not allow_complete_before_failure:
                die(f"MachineOperation '{name}' unexpectedly completed: {message}")

        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) MachineOperation not failed yet")
        time.sleep(5)
        elapsed += 5

    subprocess.run([KUBECTL, "get", resource, name, "-o", "yaml"], check=False)
    die(f"Timed out waiting for MachineOperation '{name}' to fail after {timeout_secs}s")


def node_boot_id(node_name: str) -> str:
    """Return the node boot ID reported by kubelet."""

    return kubectl_capture([
        "get", "node", node_name,
        "-o", "jsonpath={.status.nodeInfo.bootID}",
    ]).strip()


def node_kubelet_version(node_name: str) -> str:
    """Return the kubelet version reported by the Node object."""

    return kubectl_capture([
        "get", "node", node_name,
        "-o", "jsonpath={.status.nodeInfo.kubeletVersion}",
    ]).strip()


def restart_crashing_daemonset_pods(node_name: str, namespace: str, label: str) -> None:
    """Delete matching DaemonSet pods stuck in restart backoff on *node_name*."""

    result = subprocess.run(
        [KUBECTL, "get", "pods", "-n", namespace,
         "-l", label, "--field-selector", f"spec.nodeName={node_name}",
         "-o", "json"],
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        return

    pods = json.loads(result.stdout).get("items", [])
    for pod in pods:
        pod_name = pod["metadata"]["name"]
        for container_status in pod.get("status", {}).get("containerStatuses", []):
            if container_status.get("ready"):
                continue
            waiting = container_status.get("state", {}).get("waiting", {})
            terminated = container_status.get("state", {}).get("terminated", {})
            restart_count = container_status.get("restartCount", 0)
            waiting_reason = waiting.get("reason")
            terminated_reason = terminated.get("reason")
            backoff_reasons = {"CrashLoopBackOff", "ImagePullBackOff", "ErrImagePull"}
            if restart_count >= 2 or waiting_reason in backoff_reasons:
                log(f"  Deleting unhealthy pod {pod_name} "
                    f"(restarts={restart_count}, waiting={waiting_reason or 'none'}, "
                    f"terminated={terminated_reason or 'none'}) to reset backoff")
                subprocess.run(
                    [KUBECTL, "delete", "pod", "-n", namespace, pod_name,
                     "--grace-period=0", "--force"],
                    capture_output=True, text=True,
                )


def wait_for_node_ready(node_name: str, timeout_secs: int = 120) -> None:
    """Wait until *node_name* reports Ready=True."""

    log(f"Waiting for node '{node_name}' to be Ready (timeout: {timeout_secs}s)...")
    elapsed = 0
    last_restart_attempt = 0
    while elapsed < timeout_secs:
        result = subprocess.run(
            [KUBECTL, "get", "node", node_name,
             "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}"],
            capture_output=True, text=True,
        )
        status = result.stdout.strip() if result.returncode == 0 else "unknown"
        if status == "True":
            log(f"Node '{node_name}' is Ready after {elapsed}s")
            return
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) Node not yet Ready (status: {status})")
        if elapsed >= 30 and elapsed - last_restart_attempt >= 30:
            restart_crashing_daemonset_pods(node_name, "kube-system", "app=kindnet")
            last_restart_attempt = elapsed
        time.sleep(5)
        elapsed += 5

    kubectl(["describe", "node", node_name])
    die(f"Timed out waiting for node '{node_name}' to become Ready after {timeout_secs}s")


def wait_for_node_absent(node_name: str, timeout_secs: int = 120) -> None:
    """Wait until *node_name* no longer exists."""

    log(f"Waiting for node '{node_name}' to be deleted (timeout: {timeout_secs}s)...")
    elapsed = 0
    while elapsed < timeout_secs:
        ret = subprocess.run(
            [KUBECTL, "get", "node", node_name, "-o", "name"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        if ret.returncode != 0:
            log(f"Node '{node_name}' deleted after {elapsed}s")
            return
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) Node still present...")
        time.sleep(5)
        elapsed += 5

    kubectl(["get", "node", node_name, "-o", "wide"])
    die(f"Timed out waiting for node '{node_name}' to be deleted after {timeout_secs}s")


def wait_for_node_kubelet_version(
    node_name: str,
    expected_version: str,
    timeout_secs: int = 300,
) -> None:
    """Wait until *node_name* reports *expected_version* as kubeletVersion."""

    log(f"Waiting for node '{node_name}' kubeletVersion={expected_version}...")
    elapsed = 0
    last_version = ""
    while elapsed < timeout_secs:
        result = subprocess.run(
            [KUBECTL, "get", "node", node_name,
             "-o", "jsonpath={.status.nodeInfo.kubeletVersion}"],
            capture_output=True, text=True,
        )
        if result.returncode == 0:
            last_version = result.stdout.strip()
            if last_version == expected_version:
                log(f"Node kubeletVersion reached {expected_version} after {elapsed}s")
                return
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) kubeletVersion={last_version or '<unavailable>'}")
        time.sleep(5)
        elapsed += 5

    kubectl(["describe", "node", node_name])
    die(f"Timed out waiting for kubeletVersion={expected_version}; last={last_version!r}")


def wait_for_node_boot_id_change(node_name: str, previous_boot_id: str, timeout_secs: int = 120) -> str:
    """Wait for kubelet to report a different node boot ID."""

    log(f"Waiting for node '{node_name}' boot ID to change (timeout: {timeout_secs}s)...")
    elapsed = 0
    while elapsed < timeout_secs:
        current_boot_id = node_boot_id(node_name)
        if current_boot_id and current_boot_id != previous_boot_id:
            log(f"Node boot ID changed: {previous_boot_id} -> {current_boot_id}")
            return current_boot_id
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) Node boot ID unchanged: {current_boot_id or '<empty>'}")
        time.sleep(5)
        elapsed += 5

    kubectl(["describe", "node", node_name])
    die(f"Timed out waiting for node '{node_name}' boot ID to change after {timeout_secs}s")


def wait_for_node_reboot_event(node_name: str, boot_id: str, timeout_secs: int = 120) -> None:
    """Wait for the Kubernetes Node Rebooted event for *boot_id*."""

    log(f"Waiting for Node Rebooted event for boot ID '{boot_id}'...")
    elapsed = 0
    while elapsed < timeout_secs:
        result = subprocess.run(
            [
                KUBECTL, "get", "events",
                "--field-selector", f"involvedObject.kind=Node,involvedObject.name={node_name},reason=Rebooted",
                "-o", "json",
            ],
            capture_output=True, text=True,
        )
        if result.returncode == 0:
            events = json.loads(result.stdout).get("items", [])
            for event in events:
                message = event.get("message", "")
                if boot_id in message:
                    log("Observed Node Rebooted event")
                    return

        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) Rebooted event not observed yet")
        time.sleep(5)
        elapsed += 5

    kubectl(["get", "events", "--field-selector", f"involvedObject.kind=Node,involvedObject.name={node_name}",
             "--sort-by=.lastTimestamp"])
    die(f"Timed out waiting for Node Rebooted event for '{node_name}' boot ID '{boot_id}'")


def read_daemon_current_target() -> str:
    """Return the target path of the host daemon current binary symlink."""

    return ssh_capture(f"sudo readlink -f {DAEMON_BINARY_CURRENT}").strip()


def read_daemon_last_good_target() -> str:
    """Return the target path of the host daemon last-good binary symlink."""

    return ssh_capture(f"sudo readlink -f {DAEMON_BINARY_LAST_GOOD}").strip()


def wait_for_daemon_current_target(expected_target: str, timeout_secs: int = 180) -> None:
    """Wait for the daemon current symlink to point to *expected_target*."""

    log(f"Waiting for daemon current symlink to point to {expected_target}...")
    elapsed = 0
    last_target = ""
    while elapsed < timeout_secs:
        result = subprocess.run(
            ["ssh", *SSH_OPTS, SSH_TARGET,
             f"sudo readlink -f {DAEMON_BINARY_CURRENT}"],
            capture_output=True, text=True,
        )
        if result.returncode == 0:
            last_target = result.stdout.strip()
            if last_target == expected_target:
                log(f"Daemon current symlink restored after {elapsed}s")
                return
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) daemon current target={last_target or '<unavailable>'}")
        time.sleep(5)
        elapsed += 5

    die(f"Timed out waiting for daemon current target {expected_target}; last={last_target!r}")


def wait_for_daemon_active(timeout_secs: int = 180) -> None:
    """Wait for unbounded-agent-daemon.service to be active on the VM."""

    log("Waiting for unbounded-agent-daemon.service to be active...")
    elapsed = 0
    last_status = ""
    while elapsed < timeout_secs:
        result = subprocess.run(
            ["ssh", *SSH_OPTS, SSH_TARGET,
             "sudo systemctl is-active unbounded-agent-daemon.service"],
            capture_output=True, text=True,
        )
        last_status = result.stdout.strip() or result.stderr.strip()
        if result.returncode == 0 and last_status == "active":
            log(f"unbounded-agent-daemon.service is active after {elapsed}s")
            return
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) daemon status={last_status or '<empty>'}")
        time.sleep(5)
        elapsed += 5

    subprocess.run(["ssh", *SSH_OPTS, SSH_TARGET,
                    "sudo systemctl status unbounded-agent-daemon.service --no-pager"], check=False)
    die(f"Timed out waiting for daemon to become active; last status={last_status!r}")


def _serve_agent_upgrade_tarball(tarball: Path, operation_name: str, expect_complete: bool = True) -> dict[str, Any]:
    """Serve *tarball* to the VM, create AgentUpgrade, and wait for it."""

    runner_ip = VM_GATEWAY
    agent_url = f"http://{runner_ip}:{SERVE_PORT}/{tarball.name}"
    log(f"Starting HTTP file server on {runner_ip}:{SERVE_PORT} for {tarball.name}...")
    handler = _make_handler(str(tarball.parent))
    httpd = HTTPServer((runner_ip, SERVE_PORT), handler)
    server_thread = Thread(target=httpd.serve_forever, daemon=True)
    server_thread.start()
    try:
        log(f"Verifying VM can reach agent upgrade URL: {agent_url}")
        ssh_cmd(f"curl -fsSL --connect-timeout 10 -o /dev/null {agent_url}")
        run_quiet([KUBECTL, "delete", _machine_operation_resource(), operation_name,
                   "--ignore-not-found"], check=False)
        create_machine_operation(
            operation_name,
            AGENT_MACHINE_NAME,
            "AgentUpgrade",
            parameters={"downloadURL": agent_url},
        )
        if expect_complete:
            return wait_for_machine_operation_complete(operation_name)
        return wait_for_machine_operation_failed(operation_name)
    finally:
        httpd.shutdown()


def _build_agent_upgrade_tarball(tarball: Path) -> None:
    """Build the current repo agent binary and package it as a working upgrade tarball."""

    build_dir = tarball.parent / "agent-upgrade-good"
    shutil.rmtree(build_dir, ignore_errors=True)
    build_dir.mkdir(parents=True)
    run(["go", "build", "-o", str(build_dir / "unbounded-agent"),
         str(REPO_ROOT / "cmd" / "agent" / "main.go")],
        env={**os.environ, "GOOS": "linux", "GOARCH": "amd64"})
    run(["tar", "-czf", str(tarball), "-C", str(build_dir), "unbounded-agent"])


def _build_failing_agent_tarball(tarball: Path) -> None:
    """Package a deliberately failing executable as an agent upgrade tarball."""

    _build_script_agent_tarball(
        tarball,
        "agent-upgrade-bad",
        "#!/bin/sh\n"
        "echo failing upgraded agent >&2\n"
        "exit 42\n",
    )


def _build_daemon_failing_agent_tarball(tarball: Path) -> None:
    """Package an executable that passes preflight but fails as the daemon."""

    _build_script_agent_tarball(
        tarball,
        "agent-upgrade-daemon-bad",
        "#!/bin/sh\n"
        "if [ \"${1:-}\" = \"version\" ]; then\n"
        "    echo unbounded-agent e2e-daemon-failing\n"
        "    exit 0\n"
        "fi\n"
        "echo failing upgraded agent daemon >&2\n"
        "exit 42\n",
    )


def _build_script_agent_tarball(tarball: Path, build_name: str, script: str) -> None:
    """Package script content as the agent binary in an upgrade tarball."""

    build_dir = tarball.parent / build_name
    shutil.rmtree(build_dir, ignore_errors=True)
    build_dir.mkdir(parents=True)
    agent_path = build_dir / "unbounded-agent"
    agent_path.write_text(script)
    agent_path.chmod(0o755)
    run(["tar", "-czf", str(tarball), "-C", str(build_dir), "unbounded-agent"])


# ---------------------------------------------------------------------------
# create-vm / recreate-vm helpers
# ---------------------------------------------------------------------------
def _stop_qemu_by_pid_file(pid_file: Path, vm_name: str) -> None:
    if not pid_file.exists():
        return

    pid = int(pid_file.read_text().strip())
    try:
        os.kill(pid, 0)
        log(f"Stopping VM '{vm_name}' (PID: {pid})...")
        os.kill(pid, 15)
        time.sleep(2)
        try:
            os.kill(pid, 0)
            log("Force killing VM...")
            os.kill(pid, 9)
        except OSError:
            pass  # already exited after SIGTERM
    except OSError:
        pass  # already gone
    pid_file.unlink(missing_ok=True)


def _stop_qemu() -> None:
    """Stop the QEMU VM process if it is running."""
    _stop_qemu_by_pid_file(VM_DIR / f"{VM_NAME}.pid", VM_NAME)


@dataclass(frozen=True)
class HostImage:
    url: str
    file_name: str
    backing_format: str
    sudo_group: str
    packages: list[str]
    write_files: str = ""
    pre_marker_commands: list[str] | None = None


def host_image() -> HostImage:
    if HOST_BASE_OS == "ubuntu2404":
        return HostImage(
            url=HOST_IMAGE_URL
            or "https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img",
            file_name="ubuntu-cloud-amd64.img",
            backing_format="qcow2",
            sudo_group="sudo",
            packages=["curl", "jq", "apt-transport-https", "ca-certificates", "net-tools"],
            write_files=ubuntu_netplan_write_files(),
            pre_marker_commands=["netplan apply"],
        )
    if HOST_BASE_OS == "ubuntu2604":
        return HostImage(
            url=HOST_IMAGE_URL
            or "https://cloud-images.ubuntu.com/minimal/releases/resolute/release/ubuntu-26.04-minimal-cloudimg-amd64.img",
            file_name="ubuntu-26.04-minimal-cloudimg-amd64.img",
            backing_format="qcow2",
            sudo_group="sudo",
            packages=["curl", "jq", "apt-transport-https", "ca-certificates", "net-tools"],
            write_files=ubuntu_netplan_write_files(),
            pre_marker_commands=["netplan apply"],
        )
    if HOST_BASE_OS == "fedora":
        return HostImage(
            url=HOST_IMAGE_URL
            or "https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/x86_64/images/Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2",
            file_name="fedora-cloud-amd64.qcow2",
            backing_format="qcow2",
            sudo_group="wheel",
            packages=["curl", "jq", "ca-certificates", "net-tools"],
        )

    die(f"Unsupported HOST_BASE_OS {HOST_BASE_OS!r}; expected ubuntu2404, ubuntu2604, or fedora")


def ubuntu_netplan_write_files() -> str:
    return textwrap.dedent(f"""\
        write_files:
          - path: /etc/netplan/99-static.yaml
            content: |
              network:
                version: 2
                ethernets:
                  ens3:
                    addresses:
                      - {VM_IP}/24
                    routes:
                      - to: default
                        via: {VM_GATEWAY}
                    nameservers:
                      addresses:
                        - 8.8.8.8
                        - 8.8.4.4
            permissions: "0600"
    """)


def yaml_list(items: list[str], indent: str) -> str:
    return "\n".join(f"{indent}- {item}" for item in items)


def _cloud_init_user_data(image: HostImage, ssh_pub_key: str) -> str:
    packages = yaml_list(image.packages, "  ")
    commands = [*(image.pre_marker_commands or []), "mkdir -p /etc/agent"]
    runcmd = yaml_list(commands, "  ")
    write_files = image.write_files.rstrip()
    write_files_block = f"\n{write_files}\n" if write_files else ""

    return (
        f"#cloud-config\n"
        f"users:\n"
        f"  - name: {VM_SSH_USER}\n"
        f"    sudo: ALL=(ALL) NOPASSWD:ALL\n"
        f"    shell: /bin/bash\n"
        f"    groups: [{image.sudo_group}]\n"
        f"    lock_passwd: false\n"
        f"    ssh_authorized_keys:\n"
        f"      - {ssh_pub_key}\n"
        f"\n"
        f"package_update: true\n"
        f"package_upgrade: false\n"
        f"packages:\n"
        f"{packages}\n"
        f"{write_files_block}"
        f"runcmd:\n"
        f"{runcmd}\n"
        f"  - |\n"
        f"    cat > /etc/agent/provisioned <<'MARKER'\n"
        f"    provisioned=true\n"
        f"    MARKER\n"
        f"  - 'echo \"cloud-init: done\"'\n"
    )



def _launch_vm(ssh_pub_key: str) -> None:
    """Create a fresh VM disk, cloud-init ISO, launch QEMU, and wait for SSH.

    Assumes VM_DIR, the base cloud image, and the SSH key pair already exist.
    Networking (bridge, TAP, NAT) must already be configured.
    """

    image = host_image()
    image_file = VM_DIR / image.file_name
    if not image_file.exists():
        die(f"Base cloud image not found: {image_file}. Run create-vm first.")

    # Create VM disk
    vm_disk = VM_DIR / f"{VM_NAME}.qcow2"
    log(f"Creating snapshot disk: {vm_disk}")
    run(["qemu-img", "create", "-f", "qcow2", "-b", str(image_file),
         "-F", image.backing_format, str(vm_disk), VM_DISK_SIZE])

    # cloud-init configuration
    log("Generating cloud-init configuration...")
    mac_address = qemu_mac_address()

    user_data = VM_DIR / "user-data"
    user_data.write_text(_cloud_init_user_data(image, ssh_pub_key))

    meta_data = VM_DIR / "meta-data"
    # Use a unique instance-id so cloud-init treats this as a new instance
    # even when reusing the same VM_DIR.
    instance_id = f"{VM_NAME}-{secrets.token_hex(4)}"
    meta_data.write_text(textwrap.dedent(f"""\
        instance-id: {instance_id}
        local-hostname: {VM_NAME}
    """))

    network_config = VM_DIR / "network-config"
    network_config.write_text(textwrap.dedent(f"""\
        version: 2
        ethernets:
          ens3:
            addresses:
              - {VM_IP}/24
            gateway4: {VM_GATEWAY}
            nameservers:
              addresses:
                - 8.8.8.8
                - 8.8.4.4
    """))

    # Build cloud-init seed ISO
    seed_iso = VM_DIR / f"{VM_NAME}-seed.iso"
    log(f"Creating cloud-init seed ISO: {seed_iso}")
    run(["genisoimage", "-output", str(seed_iso), "-volid", "cidata",
         "-joliet", "-rock",
         str(user_data), str(meta_data), str(network_config)])

    # Launch QEMU VM
    pid_file = VM_DIR / f"{VM_NAME}.pid"
    qemu_log = VM_DIR / f"{VM_NAME}.log"
    log("============================================")
    log(f"  Launching VM: {VM_NAME}")
    log(f"  Host OS:      {HOST_BASE_OS}")
    log(f"  Memory:       {VM_MEMORY} MB")
    log(f"  CPUs:         {VM_CPUS}")
    log(f"  Disk:         {vm_disk}")
    log(f"  IP:           {VM_IP}")
    log(f"  MAC:          {mac_address}")
    log(f"  Bridge:       {BRIDGE_NAME}")
    log(f"  Log:          {qemu_log}")
    log("============================================")

    qemu_args = [
        "qemu-system-x86_64",
        "-cpu", "host", "-accel", "kvm",
        "-m", VM_MEMORY, "-smp", VM_CPUS,
        "-drive", f"file={vm_disk},format=qcow2,if=virtio",
        "-drive", f"file={seed_iso},format=raw,if=virtio",
        "-netdev", f"tap,id=net0,ifname={TAP_NAME},script=no,downscript=no",
        "-device", f"virtio-net-pci,netdev=net0,mac={mac_address}",
        "-daemonize", "-pidfile", str(pid_file),
        "-serial", f"file:{qemu_log}",
        "-display", "none",
    ]
    run(qemu_args)

    qemu_pid = pid_file.read_text().strip()
    log(f"VM started in background (PID: {qemu_pid})")

    # Wait for SSH
    log(f"Waiting for SSH to become available on {VM_IP}...")
    max_attempts = 120
    for attempt in range(1, max_attempts + 1):
        # Check QEMU is still alive
        try:
            os.kill(int(qemu_pid), 0)
        except OSError:
            die(f"QEMU process exited unexpectedly. Check log: {qemu_log}")

        ret = subprocess.run(
            ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=2",
             *SSH_OPTS, SSH_TARGET, "true"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        if ret.returncode == 0:
            break
        if attempt % 10 == 0:
            print(".", end="", flush=True)
        time.sleep(3)
    else:
        die(f"SSH did not become available after {max_attempts} attempts. Check log: {qemu_log}")

    print(flush=True)
    log(f"VM is ready at {VM_IP}")


# ---------------------------------------------------------------------------
# create-vm
# ---------------------------------------------------------------------------
def _check_vm_prereqs() -> None:
    # Pre-flight
    for cmd in ("qemu-system-x86_64", "qemu-img", "genisoimage"):
        if shutil.which(cmd) is None:
            die(f"{cmd} is required but not found in PATH")
    if not os.access("/dev/kvm", os.R_OK):
        die("/dev/kvm is not accessible. Enable KVM for hardware acceleration.")


def _ensure_vm_ssh_key() -> str:
    VM_DIR.mkdir(parents=True, exist_ok=True)
    SSH_KEY_DIR.mkdir(parents=True, exist_ok=True)

    # Generate SSH key pair
    if not SSH_KEY.exists():
        log("Generating SSH key pair...")
        run(["ssh-keygen", "-t", "ed25519", "-f", str(SSH_KEY), "-N", "", "-q"])

    return SSH_KEY.with_suffix(".pub").read_text().strip()


def create_vm_bridge() -> None:
    """Create bridge networking shared by e2e VMs."""
    log(f"Creating bridge network {BRIDGE_NAME}...")
    run_quiet(["sudo", "ip", "link", "del", BRIDGE_NAME], check=False)
    run(["sudo", "ip", "link", "add", BRIDGE_NAME, "type", "bridge"])
    run(["sudo", "ip", "addr", "add", f"{VM_GATEWAY}/24", "dev", BRIDGE_NAME])
    run(["sudo", "ip", "link", "set", BRIDGE_NAME, "up"])

    # NAT for the VM subnet
    run(["sudo", "iptables", "-t", "nat", "-A", "POSTROUTING",
         "-s", f"{VM_SUBNET}.0/24", "!", "-d", f"{VM_SUBNET}.0/24", "-j", "MASQUERADE"])

    # TAP device
    run(["sudo", "ip", "tuntap", "add", "dev", TAP_NAME, "mode", "tap"])
    run(["sudo", "ip", "link", "set", TAP_NAME, "master", BRIDGE_NAME])
    run(["sudo", "ip", "link", "set", TAP_NAME, "up"])

    # Prevent NetworkManager from detaching interfaces from the bridge.
    _nm_unmanage(BRIDGE_NAME)


def launch_vm() -> None:
    """Launch a QEMU VM on an existing e2e bridge."""
    _check_vm_prereqs()
    ssh_pub_key = _ensure_vm_ssh_key()

    # TAP device
    run_quiet(["sudo", "ip", "link", "delete", TAP_NAME], check=False)
    run(["sudo", "ip", "tuntap", "add", "dev", TAP_NAME, "mode", "tap"])
    run(["sudo", "ip", "link", "set", TAP_NAME, "master", BRIDGE_NAME])
    run(["sudo", "ip", "link", "set", TAP_NAME, "up"])
    _nm_unmanage(TAP_NAME)

    image = host_image()
    image_file = VM_DIR / image.file_name
    if not image_file.exists():
        log(f"Downloading {HOST_BASE_OS} cloud image...")
        download_file(image.url, image_file)
    else:
        log(f"Using existing image: {image_file}")
    run(["qemu-img", "info", "-f", image.backing_format, str(image_file)])

    _launch_vm(ssh_pub_key)


def create_vm() -> None:
    """Create bridge networking and launch a QEMU VM."""
    _check_vm_prereqs()
    create_vm_bridge()
    launch_vm()


# ---------------------------------------------------------------------------
# external network block
# ---------------------------------------------------------------------------
def _container_ips(container: str) -> list[str]:
    """Return Docker/Podman network IPs for *container*."""
    if shutil.which(CONTAINER_ENGINE) is None:
        return []

    result = subprocess.run(
        [CONTAINER_ENGINE, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", container],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        return []

    return [ip for ip in result.stdout.split() if ip]


def _kind_control_plane_ips() -> list[str]:
    """Return known control plane IPs that blocked-network VMs may reach."""
    ips = {f"{VM_SUBNET}.2"}

    if shutil.which(KUBECTL) is not None:
        result = subprocess.run(
            [KUBECTL, "get", "node", KIND_CONTAINER, "-o", "jsonpath={.status.addresses[?(@.type=='InternalIP')].address}"],
            capture_output=True,
            text=True,
        )
        if result.returncode == 0:
            ips.update(ip for ip in result.stdout.split() if ip)

    ips.update(_container_ips(KIND_CONTAINER))

    return sorted(ips)


def _local_registry_ips() -> list[str]:
    """Return local registry container IPs that blocked-network VMs may reach."""
    return _container_ips(LOCAL_ARTIFACT_REGISTRY_NAME)


def _external_network_block_rules(vm_ip: str = VM_IP) -> list[list[str]]:
    """Return iptables FORWARD rules that block VM egress outside e2e networks."""
    rules = [["-s", vm_ip, "-d", f"{VM_SUBNET}.0/24", "-j", "ACCEPT"]]

    for ip in _kind_control_plane_ips() + _local_registry_ips():
        rules.append(["-s", vm_ip, "-d", ip, "-j", "ACCEPT"])

    rules.extend([
        ["-s", vm_ip, "-d", "10.244.0.0/16", "-j", "ACCEPT"],
        ["-s", vm_ip, "-d", "10.96.0.0/12", "-j", "ACCEPT"],
        ["-s", vm_ip, "-j", "REJECT"],
    ])

    return rules


def unblock_external_network(vm_ip: str = VM_IP) -> None:
    """Remove VM external egress block rules."""
    if shutil.which("iptables") is None:
        return

    for rule in _external_network_block_rules(vm_ip):
        for _ in range(10):
            result = run_quiet(["sudo", "iptables", "-D", "FORWARD", *rule], check=False)
            if result.returncode != 0:
                break


def unblock_all_external_network_rules() -> None:
    """Remove external egress block rules for default and config e2e VMs."""
    unblock_external_network(VM_IP)
    if VM_NAME != "agent-config-e2e":
        return

    for index, _cfg in enumerate(discover_node_configs()):
        unblock_external_network(f"{VM_SUBNET}.{10 + index}")


def block_external_network() -> None:
    """Block VM egress outside local e2e networks while keeping cluster access."""
    log(f"Blocking external network egress from VM {VM_IP}...")
    if shutil.which("iptables") is None:
        die("iptables is required but not found in PATH")

    unblock_external_network(VM_IP)
    for rule in reversed(_external_network_block_rules(VM_IP)):
        run(["sudo", "iptables", "-I", "FORWARD", "1", *rule])


def prepare_blocked_network_vm() -> None:
    """Install host packages that are outside the bootstrap artifact bundle."""
    log("Preparing VM host packages before blocking external egress...")
    ssh_cmd("sudo cloud-init status --wait || true")
    ssh_cmd(r"""
sudo bash -s <<'SH'
set -euo pipefail
if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    rm -f /var/cache/apt/pkgcache.bin /var/cache/apt/srcpkgcache.bin
    apt-get update
    apt-get install -y systemd-container curl nftables util-linux
elif command -v tdnf >/dev/null 2>&1; then
    tdnf install -y systemd-container curl nftables util-linux
elif command -v dnf >/dev/null 2>&1; then
    dnf install -y systemd-container curl nftables util-linux
elif command -v yum >/dev/null 2>&1; then
    yum install -y systemd-container curl nftables util-linux
else
    echo "no supported package manager found for blocked-network preparation" >&2
    exit 1
fi
SH
""")


# ---------------------------------------------------------------------------
# ensure-kind-bridge
# ---------------------------------------------------------------------------
VETH_HOST = "veth-kind-e2e"
VETH_KIND = "eth-e2e"


def ensure_kind_bridge() -> None:
    """Ensure the Kind container is connected to the VM bridge via a veth pair.

    Checks whether veth-kind-e2e is attached to virbr-e2e on the host and
    eth-e2e exists inside the Kind container with the correct IP.  If
    anything is missing or broken the veth pair is (re)created.

    This is safe to call repeatedly (idempotent) and is used between
    Case 1 and Case 2 to guard against the veth pair being detached from
    the bridge by external events (Docker, kernel, etc.).
    """
    log(f"Ensuring Kind container is attached to bridge {BRIDGE_NAME}...")

    needs_repair = False

    # 1. Check if veth-kind-e2e exists on the host and is a member of
    #    the correct bridge.
    result = subprocess.run(
        ["ip", "-j", "link", "show", VETH_HOST],
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        log(f"  {VETH_HOST} does not exist on host - will create")
        needs_repair = True
    else:
        try:
            link_info = json.loads(result.stdout)
            master = link_info[0].get("master", "")
            if master != BRIDGE_NAME:
                log(f"  {VETH_HOST} exists but master='{master}' (expected '{BRIDGE_NAME}') - will recreate")
                needs_repair = True
            else:
                log(f"  {VETH_HOST} is correctly attached to {BRIDGE_NAME}")
        except (ValueError, IndexError, KeyError):
            log(f"  Could not parse ip -j output for {VETH_HOST} - will recreate")
            needs_repair = True

    # 2. Check if eth-e2e exists inside the Kind container with the
    #    expected IP address.
    if not needs_repair:
        result = subprocess.run(
            ["docker", "exec", KIND_CONTAINER, "ip", "addr", "show", VETH_KIND],
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            log(f"  {VETH_KIND} does not exist in Kind container - will recreate")
            needs_repair = True
        elif f"{VM_SUBNET}.2/24" not in result.stdout:
            log(f"  {VETH_KIND} exists but missing {VM_SUBNET}.2/24 - will recreate")
            needs_repair = True

    if not needs_repair:
        log("  Bridge attachment is healthy - no action needed")
        return

    # 3. Repair: delete any stale veth and recreate the pair.
    log("  Repairing bridge attachment...")
    kind_pid = capture([
        "docker", "inspect", KIND_CONTAINER,
        "--format", "{{.State.Pid}}",
    ])

    # Deleting either end destroys the whole pair, so this is safe even
    # if only one end still exists.
    run_quiet(["sudo", "ip", "link", "delete", VETH_HOST], check=False)

    run(["sudo", "ip", "link", "add", VETH_HOST, "type", "veth",
         "peer", "name", VETH_KIND])
    run(["sudo", "ip", "link", "set", VETH_HOST, "master", BRIDGE_NAME])
    run(["sudo", "ip", "link", "set", VETH_HOST, "up"])
    run(["sudo", "ip", "link", "set", VETH_KIND, "netns", kind_pid])
    run(["sudo", "nsenter", "-t", kind_pid, "-n",
         "ip", "addr", "add", f"{VM_SUBNET}.2/24", "dev", VETH_KIND])
    run(["sudo", "nsenter", "-t", kind_pid, "-n",
         "ip", "link", "set", VETH_KIND, "up"])

    # Prevent NetworkManager from detaching the veth from the bridge.
    _nm_unmanage(VETH_HOST)

    log(f"  Repaired: {VETH_HOST} -> {BRIDGE_NAME} -> {VETH_KIND} in Kind container")


# ---------------------------------------------------------------------------
# configure-kind-node-ip
# ---------------------------------------------------------------------------
# The Kind control-plane container's address on the VM bridge (assigned to
# eth-e2e / VETH_KIND by ensure_kind_bridge). Kept in sync with that setup.
CONTROL_PLANE_BRIDGE_IP = f"{VM_SUBNET}.2"


def configure_kind_node_ip() -> None:
    """Advertise the Kind control-plane Node's VM-bridge IP.

    By default the Kind control-plane Node advertises its Docker-network
    address (for example 172.18.0.2). Kindnet running on a QEMU worker node -
    which is only attached to the VM bridge subnet - then tries to install the
    control-plane pod CIDR route with that Docker address as the
    next hop. The Docker address is not on any interface the worker owns, so
    ``ip route add ... via <docker-ip>`` fails with "network is unreachable",
    kindnet panics ("Maximum retries reconciling node routes"), and the worker
    never receives a CNI config (kubelet stays NetworkPluginNotReady).

    Reconfiguring the control-plane kubelet to advertise the bridge-reachable
    address (CONTROL_PLANE_BRIDGE_IP, assigned to eth-e2e by
    ensure_kind_bridge) makes that next hop on-link for every worker, so route
    reconciliation succeeds and the worker becomes Ready promptly.

    Must run after the Kind container is attached to the VM bridge (so the IP
    is a local address kubelet accepts) and before any worker node joins. It is
    idempotent: re-running replaces any existing --node-ip with the same value.

    The control-plane API TLS cert SANs cover the Docker address, so kindnet's
    CONTROL_PLANE_ENDPOINT (API connection) is left pointing at the Docker IP;
    only the Node's advertised InternalIP - used as a routing next hop, not for
    TLS - is changed here.
    """
    node_ip = CONTROL_PLANE_BRIDGE_IP
    log(f"Configuring {KIND_CONTAINER} kubelet to advertise node IP {node_ip}")

    # Rewrite KUBELET_KUBEADM_ARGS in place: drop any existing --node-ip and
    # append ours, then restart kubelet so the Node re-registers the new IP.
    script = textwrap.dedent(f"""\
        set -eu
        . /var/lib/kubelet/kubeadm-flags.env
        set -- $KUBELET_KUBEADM_ARGS
        new_args=""
        for arg do
          case "$arg" in
            --node-ip=*) ;;
            *) new_args="$new_args $arg" ;;
          esac
        done
        new_args="${{new_args# }}"
        printf 'KUBELET_KUBEADM_ARGS="%s --node-ip={node_ip}"\\n' "$new_args" \\
          >/var/lib/kubelet/kubeadm-flags.env
        systemctl restart kubelet
    """)
    run([CONTAINER_ENGINE, "exec", KIND_CONTAINER, "sh", "-c", script])

    log(f"Waiting for Node '{KIND_CONTAINER}' to advertise InternalIP {node_ip}...")
    for elapsed in range(0, 120, 3):
        result = subprocess.run(
            [KUBECTL, "get", "node", KIND_CONTAINER, "-o",
             "jsonpath={.status.addresses[?(@.type=='InternalIP')].address}"],
            capture_output=True, text=True,
        )
        if result.returncode == 0 and result.stdout.split() == [node_ip]:
            log(f"Node '{KIND_CONTAINER}' advertises InternalIP {node_ip} after {elapsed}s")
            return
        time.sleep(3)

    kubectl(["get", "node", KIND_CONTAINER, "-o", "wide"])
    die(f"Timed out waiting for Node '{KIND_CONTAINER}' to advertise InternalIP {node_ip}")


# ---------------------------------------------------------------------------
# run-agent
# ---------------------------------------------------------------------------
def run_agent(node_config: NodeConfig) -> None:
    """Build agent, generate bootstrap script, and run it on the VM."""

    if not SSH_KEY.exists():
        die(f"SSH key not found: {SSH_KEY}. Run create-vm first.")
    for cmd in (KUBECTL,):
        if shutil.which(cmd) is None:
            die(f"{cmd} is required but not found in PATH")

    agent_url_override = os.environ.get("AGENT_URL", "")
    if agent_url_override:
        _run_agent_inner(agent_url_override, node_config)
        log("Agent bootstrap completed")
        return

    agent_url = prepare_agent_artifacts()
    log(f"Starting HTTP file server on {VM_GATEWAY}:{SERVE_PORT}...")
    handler = _make_handler(str(VM_DIR))
    httpd = HTTPServer((VM_GATEWAY, SERVE_PORT), handler)
    server_thread = Thread(target=httpd.serve_forever, daemon=True)
    server_thread.start()
    log(f"Agent download URL: {agent_url}")

    try:
        _run_agent_inner(agent_url, node_config)
    finally:
        httpd.shutdown()

    log("Agent bootstrap completed")


def prepare_agent_artifacts() -> str:
    """Build agent artifacts and return the URL that serves the tarball."""
    VM_DIR.mkdir(parents=True, exist_ok=True)

    # Build agent binary and package as tarball
    log("Building unbounded-agent...")
    agent_bin = REPO_ROOT / "bin" / "unbounded-agent"
    run(["go", "build", "-o", str(agent_bin), str(REPO_ROOT / "cmd" / "agent" / "main.go")],
        env={**os.environ, "GOOS": "linux", "GOARCH": "amd64"})
    log(f"Agent binary built: {agent_bin}")

    log("Rendering manifests for embedded fs...")
    run(["make", "machina-manifests", "net-manifests"], cwd=str(REPO_ROOT))

    log("Building kubectl-unbounded...")
    kubectl_unbounded_bin = Path(KUBECTL_UNBOUNDED)
    run(["go", "build", "-o", str(kubectl_unbounded_bin),
         str(REPO_ROOT / "cmd" / "kubectl-unbounded" / "main.go")])
    log(f"kubectl-unbounded binary built: {kubectl_unbounded_bin}")

    log("Packaging agent binary as tarball...")
    agent_tarball = VM_DIR / "unbounded-agent-linux-amd64.tar.gz"
    run(["tar", "-czf", str(agent_tarball), "-C", str(REPO_ROOT / "bin"), "unbounded-agent"])
    log(f"Agent tarball: {agent_tarball}")

    # Serve the tarball over HTTP
    runner_ip = VM_GATEWAY
    agent_url = f"http://{runner_ip}:{SERVE_PORT}/unbounded-agent-linux-amd64.tar.gz"
    return agent_url


def kubernetes_server_version() -> str:
    """Return the normalized Kubernetes server version for the active cluster."""
    version_raw = capture([KUBECTL, "version", "-o", "json"])
    version_info = json.loads(version_raw)
    kube_version = version_info.get("serverVersion", {}).get("gitVersion", "")
    if not kube_version:
        die("could not determine Kubernetes server gitVersion")
    if not kube_version.startswith("v"):
        kube_version = "v" + kube_version
    return kube_version


def build_agent_artifacts_builder() -> None:
    """Build the offline artifact builder binary."""
    log("Building agent-artifacts-builder...")
    run(["go", "build", "-o", AGENT_ARTIFACTS_BUILDER,
         str(REPO_ROOT / "hack" / "cmd" / "agent-artifacts-builder" / "main.go")])


def offline_artifact_manifest(kube_version: str, container_images: list[str] | None = None) -> dict[str, Any]:
    """Return a manifest for an e2e offline bootstrap artifact bundle."""
    manifest: dict[str, Any] = {
        "versions": {
            "kubernetes": kube_version,
            "containerd": "2.1.8",
            "runc": "1.5.0",
            "cni": "1.5.1",
            "crictl": _crictl_version_for_kubernetes(kube_version),
        },
    }
    if container_images is not None:
        manifest["containerImages"] = sorted(set(container_images))
    return manifest


def default_offline_container_images(kube_version: str) -> list[str]:
    """Return the agent's default offline container images for e2e bundles."""
    return [
        "mcr.microsoft.com/oss/v2/kubernetes/pause:3.9",
        f"mcr.microsoft.com/oss/v2/kubernetes/kube-proxy:{kube_version}",
    ]


def e2e_offline_container_images(kube_version: str, cluster_images: list[str]) -> list[str]:
    """Return images needed for a blocked-network e2e node to become useful."""
    return sorted(set(default_offline_container_images(kube_version) + cluster_images + [E2E_WORKLOAD_IMAGE]))


def prepare_offline_bootstrap_artifacts(node_config: NodeConfig) -> str:
    """Prepare offline bootstrap artifacts and return an agent config source."""
    oci_ref = node_config.offline_artifacts_oci_ref
    if not OFFLINE_BOOTSTRAP and not oci_ref:
        return ""

    log("Preparing offline bootstrap artifact bundle...")

    kube_version = kubernetes_server_version()

    if oci_ref:
        log(f"Using offline artifact bundle from OCI ref {oci_ref}")
        return oci_ref

    offline_dir = VM_DIR / "offline-bootstrap-artifacts" / kube_version
    if offline_dir.exists():
        shutil.rmtree(offline_dir)
    offline_dir.parent.mkdir(parents=True, exist_ok=True)

    if not oci_ref:
        build_agent_artifacts_builder()

        manifest_path = VM_DIR / "offline-bootstrap-manifest.json"
        manifest_path.write_text(json.dumps(offline_artifact_manifest(kube_version), indent=2))

        arch = "amd64"
        log(f"Building offline artifact bundle for Kubernetes {kube_version} ({arch})...")
        run([
            AGENT_ARTIFACTS_BUILDER,
            "--output-dir", str(offline_dir),
            "--manifest", str(manifest_path),
            "--arch", arch,
        ])

    tarball = VM_DIR / "offline-bootstrap-artifacts.tar.gz"
    if tarball.exists():
        tarball.unlink()
    run(["tar", "-czf", str(tarball), "-C", str(offline_dir.parent), kube_version])

    log("Copying offline artifact bundle to VM...")
    scp_cmd(str(tarball), f"{SSH_TARGET}:/tmp/offline-bootstrap-artifacts.tar.gz")
    ssh_cmd("sudo rm -rf /opt/unbounded/artifacts && sudo mkdir -p /opt/unbounded/artifacts")
    ssh_cmd("sudo tar -xzf /tmp/offline-bootstrap-artifacts.tar.gz -C /opt/unbounded/artifacts")

    source = f"file:///opt/unbounded/artifacts/{kube_version}"
    log(f"Offline artifact source installed on VM: {source}")
    return source


def start_local_artifact_registry() -> str:
    """Start a local OCI registry for mirrored offline bootstrap artifacts."""
    if shutil.which(CONTAINER_ENGINE) is None:
        die(f"{CONTAINER_ENGINE} is required but not found in PATH")

    registry = f"{VM_GATEWAY}:{LOCAL_ARTIFACT_REGISTRY_PORT}"
    run_quiet([CONTAINER_ENGINE, "rm", "-f", LOCAL_ARTIFACT_REGISTRY_NAME], check=False)
    run([
        CONTAINER_ENGINE,
        "run",
        "-d",
        "--name",
        LOCAL_ARTIFACT_REGISTRY_NAME,
        "--restart",
        "no",
        "-p",
        f"{registry}:5000",
        "registry:2",
    ])

    deadline = time.time() + 30
    while time.time() < deadline:
        result = subprocess.run(
            ["curl", "-fsS", f"http://{registry}/v2/"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        if result.returncode == 0:
            log(f"Local artifact registry is ready at {registry}")
            return registry
        time.sleep(1)

    die(f"timed out waiting for local artifact registry at {registry}")


def mirror_oci_refs_to_local_registry(configs: list[NodeConfig]) -> list[NodeConfig]:
    """Mirror configured OCI refs to a local e2e registry."""
    blocked_network = any(cfg.block_external_network for cfg in configs)
    if not any(cfg.offline_artifacts_oci_ref or cfg.rootfs_oci_image for cfg in configs) and not blocked_network:
        return configs

    registry = start_local_artifact_registry()
    mirrored: dict[str, str] = {}
    out: list[NodeConfig] = []
    e2e_offline_ref = ""

    if blocked_network:
        e2e_offline_ref = build_e2e_offline_artifact_bundle(registry, cluster_preload_images())

    for cfg in configs:
        offline_ref = cfg.offline_artifacts_oci_ref
        rootfs_ref = cfg.rootfs_oci_image
        if cfg.block_external_network:
            if not rootfs_ref:
                die(f"node config {cfg.name!r} blocks external network but does not set offlineRootfsOCIImage")
            offline_ref = e2e_offline_ref
        elif offline_ref:
            source_ref = offline_ref
            local_ref = mirrored.get(source_ref)
            if local_ref is None:
                local_ref = local_artifact_ref(registry, source_ref)
                log(f"Mirroring offline artifacts {source_ref} -> {local_ref}")
                oras_copy(source_ref, local_ref)
                mirrored[source_ref] = local_ref
            offline_ref = local_ref

        if rootfs_ref:
            source_ref = rootfs_ref
            local_ref = mirrored.get(source_ref)
            if local_ref is None:
                local_ref = local_rootfs_ref(registry, source_ref)
                log(f"Mirroring rootfs image {source_ref} -> {local_ref}")
                oras_copy(source_ref, local_ref)
                mirrored[source_ref] = local_ref
            rootfs_ref = local_ref

        out.append(replace(
            cfg,
            offline_artifacts_oci_ref=offline_ref,
            rootfs_oci_image=rootfs_ref,
        ))

    return out


def build_e2e_offline_artifact_bundle(registry: str, cluster_images: list[str]) -> str:
    """Build and push e2e offline bundles for bootstrap and repave versions."""
    current_version = kubernetes_server_version()
    target_version = _next_patch_version(current_version)

    build_agent_artifacts_builder()
    for kube_version in (current_version, target_version):
        build_one_e2e_offline_artifact_bundle(registry, kube_version, cluster_images)

    return f"oci://{registry}/unbounded/bootstrap-artifacts:e2e-k8s-{{{{ .KubernetesVersionNoV }}}}"


def build_one_e2e_offline_artifact_bundle(registry: str, kube_version: str, cluster_images: list[str]) -> str:
    """Build and push one e2e offline artifact bundle version."""
    image_refs = e2e_offline_container_images(kube_version, cluster_images)
    output_dir = VM_DIR / "offline-bootstrap-artifacts-e2e" / kube_version
    manifest_path = VM_DIR / f"offline-bootstrap-manifest-e2e-{kube_version}.json"
    local_ref = f"oci://{registry}/unbounded/bootstrap-artifacts:e2e-k8s-{kube_version.removeprefix('v')}"

    if output_dir.exists():
        shutil.rmtree(output_dir)
    output_dir.parent.mkdir(parents=True, exist_ok=True)
    manifest_path.write_text(json.dumps(offline_artifact_manifest(kube_version, image_refs), indent=2))

    log(f"Building e2e offline artifact bundle for Kubernetes {kube_version} with cluster and workload images:")
    for image in image_refs:
        log(f"  {image}")

    run([
        AGENT_ARTIFACTS_BUILDER,
        "--output-dir", str(output_dir),
        "--manifest", str(manifest_path),
        "--arch", "amd64",
        "--oci-ref", local_ref,
    ])

    log(f"Built e2e offline artifact bundle {local_ref}")
    return local_ref


def local_artifact_ref(registry: str, source_ref: str) -> str:
    """Return the local registry ref used for a mirrored artifact bundle."""
    _name, tag = split_tagged_oci_ref(source_ref, "offline artifact OCI ref")
    return f"oci://{registry}/unbounded/bootstrap-artifacts:{tag}"


def local_rootfs_ref(registry: str, source_ref: str) -> str:
    """Return the local registry ref used for a mirrored rootfs image."""
    name, tag = split_tagged_oci_ref(source_ref, "rootfs OCI image")
    repo = name.split("/", 1)[1] if "/" in name else name
    repo = re.sub(r"[^a-z0-9._/-]+", "-", repo.lower()).strip("/") or "rootfs"
    return f"{registry}/rootfs/{repo}:{tag}"



def split_tagged_oci_ref(ref: str, label: str) -> tuple[str, str]:
    ref = ref.removeprefix("oci://")
    last = ref.rsplit("/", 1)[-1]
    if ":" not in last:
        die(f"{label} must include a tag: {ref}")
    name, tag = ref.rsplit(":", 1)
    if not name or not tag:
        die(f"{label} must include a non-empty image name and tag: {ref}")
    return name, tag


def oras_copy(source_ref: str, target_ref: str) -> None:
    maybe_oras_login(source_ref)
    maybe_oras_login(target_ref)

    cmd = [ensure_oras(), "copy"]
    if is_plain_http_oci_ref(source_ref):
        cmd.append("--from-plain-http")
    if is_plain_http_oci_ref(target_ref):
        cmd.append("--to-plain-http")
    cmd.extend([source_ref.removeprefix("oci://"), target_ref.removeprefix("oci://")])
    run(cmd)


def cluster_preload_images() -> list[str]:
    """Return cluster DaemonSet images that blocked-network nodes must preload."""
    result = subprocess.run(
        [KUBECTL, "get", "daemonsets", "-n", "kube-system", "-o", "json"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        die(f"failed to list kube-system DaemonSets: {result.stderr.strip()}")

    images: set[str] = set()
    daemonsets = json.loads(result.stdout).get("items", [])
    for daemonset in daemonsets:
        pod_spec = daemonset.get("spec", {}).get("template", {}).get("spec", {})
        for field in ("initContainers", "containers"):
            for container in pod_spec.get(field, []) or []:
                image = container.get("image", "")
                if image:
                    images.add(image)

    return sorted(images)


def is_plain_http_oci_ref(ref: str) -> bool:
    ref = ref.removeprefix("oci://")
    return (
        ref.startswith(f"{VM_GATEWAY}:{LOCAL_ARTIFACT_REGISTRY_PORT}/")
        or ref.startswith("localhost:")
        or ref.startswith("127.0.0.1:")
    )


def oci_registry_host(ref: str) -> str:
    return ref.removeprefix("oci://").split("/", 1)[0]


def maybe_oras_login(ref: str) -> None:
    registry = oci_registry_host(ref)
    if registry != "ghcr.io" or registry in ORAS_LOGGED_IN_REGISTRIES:
        return

    token = os.environ.get("GITHUB_TOKEN", "")
    if not token:
        return

    log("Logging into ghcr.io for offline e2e artifact mirror...")
    result = subprocess.run(
        [ensure_oras(), "login", "ghcr.io", "-u", "github-actions", "--password-stdin"],
        input=token,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        die("oras login to ghcr.io failed")

    ORAS_LOGGED_IN_REGISTRIES.add(registry)


def ensure_oras() -> str:
    """Return an oras CLI path, installing it locally if needed."""
    if shutil.which("oras"):
        return "oras"
    if Path(ORAS).exists():
        return ORAS

    log("Installing oras CLI...")
    run(
        ["go", "install", "oras.land/oras/cmd/oras@v1.3.0"],
        env={**os.environ, "GOBIN": str(REPO_ROOT / "bin")},
    )
    if not Path(ORAS).exists():
        die(f"oras install did not produce expected binary: {ORAS}")
    return ORAS


def _crictl_version_for_kubernetes(kube_version: str) -> str:
    match = re.match(r"^v?(\d+)\.(\d+)", kube_version)
    if not match:
        die(f"invalid Kubernetes version for crictl derivation: {kube_version}")
    return f"{match.group(1)}.{match.group(2)}.0"



def _make_handler(directory: str) -> type:
    """Create a SimpleHTTPRequestHandler bound to *directory*."""
    class Handler(SimpleHTTPRequestHandler):
        def __init__(self, *args: Any, **kwargs: Any) -> None:
            super().__init__(*args, directory=directory, **kwargs)
        def log_message(self, format: str, *args: Any) -> None:  # noqa: A002
            pass  # suppress request logs
    return Handler


def _run_agent_inner(agent_url: str, node_config: NodeConfig) -> None:
    """Core logic for run-agent (after HTTP server is up)."""

    # Determine the Kind control-plane IP so connectivity checks have the
    # correct address even when the local kubeconfig uses 127.0.0.1.
    log(f"Resolving Kind control-plane IP for '{KIND_CLUSTER_NAME}'...")
    api_server = kind_api_server_url()
    log(f"API server: {api_server}")

    # Create bootstrap token.
    # Add the daemon-specific bootstrap group so Machina can approve the
    # daemon controller CSR after kubelet bootstrap completes.
    log("Creating bootstrap token...")
    token_id = secrets.token_hex(3)
    token_secret = secrets.token_hex(8)
    token_expiration = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 24 * 60 * 60))
    (VM_DIR / "token-id").write_text(token_id)
    bootstrap_group = "system:bootstrappers:unbounded-agent-daemons"

    token_manifest = json.dumps({
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {
            "name": f"bootstrap-token-{token_id}",
            "namespace": "kube-system",
            "labels": {
                "unbounded-cloud.io/site": E2E_SITE_NAME,
            },
        },
        "type": "bootstrap.kubernetes.io/token",
        "data": {
            "token-id": _b64(token_id),
            "token-secret": _b64(token_secret),
            "expiration": _b64(token_expiration),
            "usage-bootstrap-authentication": _b64("true"),
            "usage-bootstrap-signing": _b64("true"),
            "auth-extra-groups": _b64(bootstrap_group),
        },
    })
    kubectl(["apply", "-f", "-"], input=token_manifest.encode())
    log("Bootstrap token created")

    # Generate bootstrap script via kubectl-unbounded.
    # manual-bootstrap auto-detects the API server, CA cert, Kubernetes
    # version, and cluster DNS from the active kubeconfig. The bootstrap
    # token is resolved via the site label on the secret.
    log("Generating bootstrap script with kubectl-unbounded machine manual-bootstrap...")
    log_active_node_config(node_config)

    # Capture the local API server URL from the kubeconfig (typically
    # https://127.0.0.1:<port> for Kind) so we can replace it with the
    # VM-reachable container IP after generating the script.
    # Use --minify to scope to the current context only, avoiding picking up
    # the wrong cluster when multiple contexts exist in the kubeconfig.
    local_api_server = kubectl_capture([
        "config", "view", "--minify", "--raw",
        "-o", "jsonpath={.clusters[0].cluster.server}",
    ])
    if not local_api_server:
        die("Could not determine local API server URL from kubeconfig")

    # Wait for cloud-init and verify connectivity before preparing optional
    # offline artifacts because preparing them copies files to the VM.
    log("Waiting for cloud-init to complete on VM...")
    subprocess.run(["ssh", *SSH_OPTS, SSH_TARGET, "sudo cloud-init status --wait"],
                    check=False)

    log("Verifying VM can reach agent download URL...")
    ssh_cmd(f"curl -fsSL --connect-timeout 10 -o /dev/null {agent_url}")

    log("Verifying VM can reach Kind API server...")
    ssh_cmd(f"curl -fsSk --connect-timeout 10 {api_server}/healthz")

    offline_source = prepare_offline_bootstrap_artifacts(node_config)

    bootstrap_args = [
        KUBECTL_UNBOUNDED, "machine", "manual-bootstrap",
        AGENT_MACHINE_NAME,
        "--site", E2E_SITE_NAME,
        *node_config_bootstrap_args(node_config),
    ]
    if offline_source:
        bootstrap_args.extend(["--offline-artifacts-source", offline_source])

    bootstrap_script = capture(bootstrap_args)
    bootstrap_script = inject_kubelet_configuration(bootstrap_script, node_config)

    # The kubeconfig uses a localhost address that is not reachable from the VM.
    # Patch the generated script to use the Kind container IP instead.
    if local_api_server in bootstrap_script:
        log(f"Patching bootstrap script: replacing {local_api_server} -> {api_server}")
        bootstrap_script = bootstrap_script.replace(local_api_server, api_server)
    else:
        log(f"[WARN] Local API server {local_api_server!r} not found in bootstrap script; "
            f"VM may not be able to reach the API server")

    bootstrap_script_path = VM_DIR / "bootstrap.sh"
    bootstrap_script_path.write_text(bootstrap_script)
    bootstrap_script_path.chmod(0o600)
    log(f"Bootstrap script written to {bootstrap_script_path}")

    bootstrap_preflight_script = bootstrap_script.replace(
        'echo "Running unbounded-agent start..."\n"${AGENT_BIN}" start ${_START_ARGS}',
        textwrap.dedent("""\
        echo "Running unbounded-agent preflight..."
        "${AGENT_BIN}" preflight ${_START_ARGS} --output text | tee /tmp/unbounded-agent-preflight.txt
        "${AGENT_BIN}" preflight ${_START_ARGS} --output json > /tmp/unbounded-agent-preflight.json
        echo "Running unbounded-agent start..."
        "${AGENT_BIN}" start ${_START_ARGS}"""),
    )
    if bootstrap_preflight_script == bootstrap_script:
        die("failed to inject unbounded-agent preflight into bootstrap script")

    bootstrap_script = bootstrap_preflight_script
    bootstrap_script_path.write_text(bootstrap_script)

    # Copy bootstrap script to VM and execute it.
    log("Copying bootstrap script to VM...")
    scp_cmd(str(bootstrap_script_path), f"{SSH_TARGET}:/tmp/bootstrap.sh")
    ssh_cmd("chmod +x /tmp/bootstrap.sh")

    log("Running bootstrap script on VM...")
    log("This will download the agent, bootstrap the node, and join it to the Kind cluster.")
    env_prefix = f"AGENT_URL={agent_url} AGENT_DEBUG={AGENT_DEBUG}"
    run([
        "timeout", "1200",
        "ssh", *SSH_OPTS, "-o", "ServerAliveInterval=30", SSH_TARGET,
        f"sudo {env_prefix} /tmp/bootstrap.sh",
    ])

    log("Copying preflight reports from VM...")
    scp_from_vm("/tmp/unbounded-agent-preflight.txt", VM_DIR / "unbounded-agent-preflight.txt")
    scp_from_vm("/tmp/unbounded-agent-preflight.json", VM_DIR / "unbounded-agent-preflight.json")


# ---------------------------------------------------------------------------
# wait-for-node
# ---------------------------------------------------------------------------
def wait_for_node_registered() -> None:
    """Wait for the agent node object to appear."""

    node_timeout = int(os.environ.get("NODE_TIMEOUT", "300"))

    log(f"Waiting for node '{AGENT_MACHINE_NAME}' to appear (timeout: {node_timeout}s)...")
    elapsed = 0
    while elapsed < node_timeout:
        ret = subprocess.run(
            [KUBECTL, "get", "node", AGENT_MACHINE_NAME, "-o", "name"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        if ret.returncode == 0:
            log(f"Node '{AGENT_MACHINE_NAME}' appeared after {elapsed}s")
            return
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) Node not yet visible...")
        time.sleep(5)
        elapsed += 5

    log("Current nodes:")
    subprocess.run([KUBECTL, "get", "nodes", "-o", "wide"], check=False)
    die(f"Timed out waiting for node '{AGENT_MACHINE_NAME}' after {node_timeout}s")


def wait_for_node() -> None:
    """Wait for the agent node to appear and become Ready."""

    ready_timeout = int(os.environ.get("READY_TIMEOUT", "720"))

    wait_for_node_registered()
    wait_for_node_ready(AGENT_MACHINE_NAME, ready_timeout)

    log("============================================")
    log("  Node join PASSED")
    log("============================================")
    kubectl(["get", "nodes", "-o", "wide"])


# ---------------------------------------------------------------------------
# validate-node-config
# ---------------------------------------------------------------------------
def _assert_expected_node_config(node: dict[str, Any], node_config: NodeConfig) -> None:
    expected_labels = expected_node_labels(node_config)
    expected_taints = expected_node_taints(node_config)

    labels = node.get("metadata", {}).get("labels", {})
    for key, value in expected_labels.items():
        actual = labels.get(key)
        if actual != value:
            die(f"node label mismatch for {key!r}: got {actual!r}, expected {value!r}")

    taints = node.get("spec", {}).get("taints", [])
    for expected in expected_taints:
        if not any(
            taint.get("key") == expected["key"]
            and taint.get("value", "") == expected["value"]
            and taint.get("effect") == expected["effect"]
            for taint in taints
        ):
            die(f"expected node taint not found: {expected}; node taints: {taints}")

    internal_ips = [
        address.get("address")
        for address in node.get("status", {}).get("addresses", [])
        if address.get("type") == "InternalIP"
    ]
    node_ip = expected_node_ip(node_config)
    if node_ip not in internal_ips:
        die(f"node InternalIP mismatch: got {internal_ips}, expected {node_ip!r}")

    kubelet_configuration = node_config.kubelet_configuration or {}
    if "maxPods" in kubelet_configuration:
        expected_max_pods = str(kubelet_configuration["maxPods"])
        actual_max_pods = str(node.get("status", {}).get("capacity", {}).get("pods", ""))
        if actual_max_pods != expected_max_pods:
            die(
                f"node pod capacity mismatch: got {actual_max_pods!r}, "
                f"expected {expected_max_pods!r}"
            )


def validate_node_config(node_config: NodeConfig) -> None:
    """Verify configured node and kubelet settings."""

    log_active_node_config(node_config)
    node = json.loads(kubectl_capture(["get", "node", AGENT_MACHINE_NAME, "-o", "json"]))
    _assert_expected_node_config(node, node_config)
    validate_kubelet_configuration_config(node_config)
    validate_offline_bootstrap_config(node_config)
    validate_additional_host_mounts_config(node_config)
    validate_additional_host_devices_config(node_config)

    log("============================================")
    log("  Node config validation PASSED")
    log("============================================")
    kubectl(["get", "node", AGENT_MACHINE_NAME, "-o", "wide"])


def validate_kubelet_configuration_config(node_config: NodeConfig) -> None:
    """Verify the kubelet overlay was persisted, rendered, and passed to kubelet."""
    if not node_config.kubelet_configuration:
        return

    log("Validating kubelet configuration overlay...")
    expected_configuration = json.dumps(node_config.kubelet_configuration, sort_keys=True)
    expected_configuration_literal = json.dumps(expected_configuration)
    ssh_cmd(f"""
sudo python3 - <<'PY'
import json
import pathlib
import sys

expected = json.loads({expected_configuration_literal})
paths = sorted(pathlib.Path("/tmp").glob("unbounded-agent-config.*.json"))
paths.append(pathlib.Path("/etc/unbounded/agent/config.json"))
paths.extend(sorted(pathlib.Path("/etc/unbounded/agent").glob("*-applied-config.json")))
for config_path in paths:
    if not config_path.exists():
        continue
    cfg = json.loads(config_path.read_text())
    actual = (cfg.get("Kubelet") or {{}}).get("Configuration") or {{}}
    if actual == expected:
        print(f"Kubelet.Configuration verified in {{config_path}}: {{actual}}")
        break
else:
    sys.exit(f"Kubelet.Configuration {{expected!r}} not found in agent config files")
PY
""")

    machine = active_nspawn_machine()
    machine_root = f"/var/lib/machines/{machine}"
    rendered = ssh_capture(f"sudo cat {machine_root}/var/lib/kubelet/config.yaml")
    for key, value in node_config.kubelet_configuration.items():
        if isinstance(value, bool):
            rendered_value = str(value).lower()
        elif isinstance(value, (int, float)):
            rendered_value = str(value)
        else:
            continue

        expected_line = f"{key}: {rendered_value}"
        if expected_line not in rendered.splitlines():
            die(
                f"generated kubelet config is missing {expected_line!r}; "
                f"full config:\n{rendered}"
            )

    service = ssh_capture(f"sudo cat {machine_root}/etc/systemd/system/kubelet.service")
    if "--config=/var/lib/kubelet/config.yaml" not in service:
        die(f"kubelet service does not reference generated config: {service}")

    log("Kubelet configuration overlay validated")


def validate_offline_bootstrap_config(node_config: NodeConfig) -> None:
    """Verify an offline scenario persisted and staged offline artifacts."""
    if not node_config.offline_artifacts_oci_ref:
        return

    log("Validating offline bootstrap agent config...")
    expected_source = json.dumps(node_config.offline_artifacts_oci_ref)
    ssh_cmd(f"""
sudo python3 - <<'PY'
import json
import pathlib
import sys

expected_source = {expected_source}
paths = sorted(pathlib.Path("/tmp").glob("unbounded-agent-config.*.json"))
paths.append(pathlib.Path("/etc/unbounded/agent/config.json"))
for config_path in paths:
    if not config_path.exists():
        continue
    cfg = json.loads(config_path.read_text())
    offline = cfg.get("OfflineArtifacts") or {{}}
    source = offline.get("Source", "")
    if source == expected_source:
        if "Downloads" in cfg:
            sys.exit("Downloads must not be present when OfflineArtifacts is configured")
        print(f"OfflineArtifacts.Source verified in {{config_path}}: {{source}}")
        break
else:
    sys.exit(f"OfflineArtifacts.Source {{expected_source!r}} not found in bootstrap config files")
PY
""")

    validate_local_registry_served_offline_artifacts(node_config.offline_artifacts_oci_ref)


def validate_local_registry_served_offline_artifacts(oci_ref: str) -> None:
    """Verify the local registry served offline artifact blobs."""
    if shutil.which(CONTAINER_ENGINE) is None:
        die(f"{CONTAINER_ENGINE} is required but not found in PATH")

    ref = oci_ref.removeprefix("oci://")
    registry, rest = ref.split("/", 1)
    if registry != f"{VM_GATEWAY}:{LOCAL_ARTIFACT_REGISTRY_PORT}":
        die(f"offline artifact ref {oci_ref!r} does not use the local e2e registry")

    repository = rest.rsplit(":", 1)[0]
    logs = capture([CONTAINER_ENGINE, "logs", LOCAL_ARTIFACT_REGISTRY_NAME])
    marker = f"GET /v2/{repository}/blobs/sha256:"
    if marker not in logs:
        die(f"local registry logs do not show offline artifact blob GETs for {repository}")

    log(f"Local registry served offline artifacts for {repository}")


def validate_additional_host_mounts_config(node_config: NodeConfig) -> None:
    """Verify AdditionalHostMounts are present in the persisted agent config and nspawn config."""
    if not node_config.additional_host_mounts:
        return

    log("Validating additional host mounts configuration...")

    # Check 1: the persisted agent config JSON must contain all configured mounts.
    # Double-encode with json.dumps so the inner script receives a Python string literal
    # that it can parse with json.loads(). Direct substitution would fail because
    # json.dumps() produces lowercase JSON booleans (true/false) which are not valid
    # Python literals.
    expected_mounts_json = json.dumps(
        [
            {
                "Source": m["source"],
                **({"Target": m["target"]} if m.get("target") else {}),
                **({"ReadOnly": True} if m.get("readOnly") else {}),
            }
            for m in node_config.additional_host_mounts
        ],
        sort_keys=True,
    )
    expected_mounts_literal = json.dumps(expected_mounts_json)
    ssh_cmd(f"""
sudo python3 - <<'PY'
import json
import pathlib
import sys

expected_mounts = json.loads({expected_mounts_literal})
paths = sorted(pathlib.Path("/tmp").glob("unbounded-agent-config.*.json"))
paths.append(pathlib.Path("/etc/unbounded/agent/config.json"))
for config_path in paths:
    if not config_path.exists():
        continue
    cfg = json.loads(config_path.read_text())
    mounts = cfg.get("AdditionalHostMounts") or []
    for want in expected_mounts:
        src = want["Source"]
        tgt = want.get("Target", src)
        ro = want.get("ReadOnly", False)
        found = any(
            m.get("Source") == src
            and m.get("Target", m.get("Source")) == tgt
            and bool(m.get("ReadOnly")) == ro
            for m in mounts
        )
        if not found:
            sys.exit(
                f"AdditionalHostMounts entry Source={{src!r}} Target={{tgt!r}} ReadOnly={{ro}} "
                f"not found in {{config_path}}: mounts={{mounts}}"
            )
    print(f"AdditionalHostMounts verified in {{config_path}}: {{len(expected_mounts)}} entries")
    sys.exit(0)
sys.exit("No agent config file with AdditionalHostMounts found")
PY
""")

    # Check 2: the nspawn config file must contain the correct Bind / BindReadOnly directives.
    log("Validating additional host mounts in nspawn config...")
    machine = active_nspawn_machine()
    nspawn_config_path = f"/etc/systemd/nspawn/{machine}.nspawn"
    nspawn_config = ssh_capture(f"sudo cat {nspawn_config_path}")

    for mount in node_config.additional_host_mounts:
        source = mount["source"]
        target = mount.get("target") or source
        read_only = bool(mount.get("readOnly"))
        directive = "BindReadOnly" if read_only else "Bind"
        expected_line = f"{directive}={source}:{target}"
        if expected_line not in nspawn_config:
            die(
                f"nspawn config {nspawn_config_path} missing expected directive "
                f"{expected_line!r}; full config:\n{nspawn_config}"
            )
        log(f"  found nspawn directive: {expected_line}")

    log("Additional host mounts configuration validated")


def validate_additional_host_devices_config(node_config: NodeConfig) -> None:
    """Verify AdditionalHostDevices are present in the persisted agent config and nspawn files."""
    if not node_config.additional_host_devices:
        return

    log("Validating additional host devices configuration...")

    # Check 1: the persisted agent config JSON must contain all configured devices.
    expected_devices_json = json.dumps(list(node_config.additional_host_devices))
    ssh_cmd(f"""
sudo python3 - <<'PY'
import json
import pathlib
import sys

expected_devices = {expected_devices_json}
paths = sorted(pathlib.Path("/tmp").glob("unbounded-agent-config.*.json"))
paths.append(pathlib.Path("/etc/unbounded/agent/config.json"))
for config_path in paths:
    if not config_path.exists():
        continue
    cfg = json.loads(config_path.read_text())
    devices = cfg.get("AdditionalHostDevices") or []
    for want in expected_devices:
        if want not in devices:
            sys.exit(
                f"AdditionalHostDevices entry {{want!r}} not found in {{config_path}}: "
                f"devices={{devices}}"
            )
    print(f"AdditionalHostDevices verified in {{config_path}}: {{len(expected_devices)}} entries")
    sys.exit(0)
sys.exit("No agent config file with AdditionalHostDevices found")
PY
""")

    # Check 2: for /dev/* entries, the nspawn config must have the Bind= directive and the
    # service override must have the DeviceAllow= directive.
    dev_paths = [d for d in node_config.additional_host_devices if d.startswith("/dev/")]
    if dev_paths:
        log("Validating additional host devices in nspawn config and service override...")
        machine = active_nspawn_machine()
        nspawn_config_path = f"/etc/systemd/nspawn/{machine}.nspawn"
        override_path = (
            f"/etc/systemd/system/systemd-nspawn@{machine}.service.d/override.conf"
        )
        nspawn_config = ssh_capture(f"sudo cat {nspawn_config_path}")
        service_override = ssh_capture(f"sudo cat {override_path}")

        for device in dev_paths:
            bind_line = f"Bind={device}"
            if bind_line not in nspawn_config:
                die(
                    f"nspawn config {nspawn_config_path} missing expected directive "
                    f"{bind_line!r}; full config:\n{nspawn_config}"
                )
            log(f"  found nspawn directive: {bind_line}")

            allow_line = f"DeviceAllow={device} rwm"
            if allow_line not in service_override:
                die(
                    f"service override {override_path} missing expected directive "
                    f"{allow_line!r}; full override:\n{service_override}"
                )
            log(f"  found service override directive: {allow_line}")

    log("Additional host devices configuration validated")


def _run_scenario_command(command: str, node_config: NodeConfig, env: dict[str, str]) -> None:
    args = [sys.executable, str(Path(__file__))]
    if VERBOSE:
        args.append("--verbose")
    if node_config.path:
        args.extend(["--node-config", node_config.path])
    if node_config.offline_artifacts_oci_ref:
        args.extend(["--offline-artifacts-oci-ref", node_config.offline_artifacts_oci_ref])
    if node_config.rootfs_oci_image:
        args.extend(["--offline-rootfs-oci-image", node_config.rootfs_oci_image])
    args.append(command)

    child_env = {**os.environ, **env}
    run(args, env=child_env)


def _validate_node_config_scenario(node_config: NodeConfig, index: int, agent_url: str) -> None:
    name = node_config.name
    env = scenario_env(node_config, index)
    env["AGENT_URL"] = agent_url

    log(f"Starting agent config scenario {name!r} on {env['VM_NAME']} ({env['VM_IP']})")
    _run_scenario_command("launch-vm", node_config, env)
    if node_config.block_external_network:
        _run_scenario_command("prepare-blocked-network-vm", node_config, env)
        _run_scenario_command("block-external-network", node_config, env)

    _run_scenario_command("run-agent", node_config, env)

    wait_command = "wait-for-node"
    for command in (
        wait_command,
        "validate-node-config",
        "dump-persisted-agent-config",
        "validate-machine-cr-created",
    ):
        _run_scenario_command(command, node_config, env)

    for command in (
        "validate-workload",
        "validate-node-repave-upgrade",
    ):
        _run_scenario_command(command, node_config, env)

    log(f"Agent config scenario {name!r} passed")


def patch_kind_control_plane_node_ip() -> None:
    """Advertise the Kind control plane IP reachable from e2e VMs."""
    patch = [
        {
            "op": "replace",
            "path": "/status/addresses",
            "value": [
                {"type": "InternalIP", "address": f"{VM_SUBNET}.2"},
                {"type": "Hostname", "address": KIND_CONTAINER},
            ],
        },
    ]
    run_quiet([
        KUBECTL, "patch", "node", KIND_CONTAINER,
        "--subresource", "status",
        "--type", "json",
        "-p", json.dumps(patch),
    ], check=False)


def validate_node_config_scenarios() -> None:
    """Discover node config scenarios and validate them in parallel."""
    patch_kind_control_plane_node_ip()
    configs = mirror_oci_refs_to_local_registry(discover_node_configs())
    agent_url = prepare_agent_artifacts()

    log(f"Starting HTTP file server on {VM_GATEWAY}:{SERVE_PORT}...")
    handler = _make_handler(str(VM_DIR))
    httpd = HTTPServer((VM_GATEWAY, SERVE_PORT), handler)
    server_thread = Thread(target=httpd.serve_forever, daemon=True)
    server_thread.start()
    log(f"Agent download URL: {agent_url}")

    failures: list[str] = []

    def record_failure(name: str, exc: Exception) -> None:
        if isinstance(exc, subprocess.CalledProcessError):
            failures.append(f"{name}: {exc.cmd} exited with {exc.returncode}")
        else:
            failures.append(f"{name}: {exc}")

    def run_parallel(scenarios: list[tuple[int, NodeConfig]]) -> None:
        if not scenarios:
            return

        with concurrent.futures.ThreadPoolExecutor(max_workers=len(scenarios)) as executor:
            futures = {
                executor.submit(_validate_node_config_scenario, cfg, index, agent_url): cfg.name
                for index, cfg in scenarios
            }
            for future in concurrent.futures.as_completed(futures):
                name = futures[future]
                try:
                    future.result()
                except Exception as exc:
                    record_failure(name, exc)

    try:
        indexed_configs = list(enumerate(configs))
        run_parallel([(index, cfg) for index, cfg in indexed_configs if not cfg.block_external_network])
        if not failures:
            for index, cfg in indexed_configs:
                if not cfg.block_external_network:
                    continue
                try:
                    _validate_node_config_scenario(cfg, index, agent_url)
                except Exception as exc:
                    record_failure(cfg.name, exc)
    finally:
        httpd.shutdown()

    if failures:
        die("agent config scenario validation failed: " + "; ".join(failures))

    validate_kube_proxy()


# ---------------------------------------------------------------------------
# dump-persisted-agent-config
# ---------------------------------------------------------------------------
def dump_persisted_agent_config() -> None:
    """Print persisted agent config files from the VM with sensitive values redacted."""

    log("Dumping persisted agent config from VM...")
    ssh_cmd(r"""
set -euo pipefail
sudo python3 - <<'PY'
import json
import pathlib

CONFIG_DIR = pathlib.Path("/etc/unbounded/agent")
# Sensitive key names are compared in lowercase.
SENSITIVE_KEYS = {"bootstraptoken"}


def redact(value):
    if isinstance(value, dict):
        return {
            key: "<redacted>" if key.lower() in SENSITIVE_KEYS else redact(item)
            for key, item in value.items()
        }
    if isinstance(value, list):
        return [redact(item) for item in value]
    return value


paths = [CONFIG_DIR / "config.json"]
paths.extend(sorted(CONFIG_DIR.glob("*-applied-config.json")))

seen = set()
for path in paths:
    if path in seen or not path.exists():
        continue
    seen.add(path)
    print(f"===== {path} =====")
    try:
        data = json.loads(path.read_text())
        print(json.dumps(redact(data), indent=2, sort_keys=True))
    except Exception as exc:
        print(f"<failed to read JSON: {exc}>")

for path in sorted(CONFIG_DIR.glob("*.sha256")):
    print(f"===== {path} =====")
    try:
        print(path.read_text().strip())
    except Exception as exc:
        print(f"<failed to read checksum: {exc}>")
PY
""")


# ---------------------------------------------------------------------------
# Network diagnostics (non-fatal)
# ---------------------------------------------------------------------------
def _run_diag(label: str, args: list[str]) -> None:
    """Run a single diagnostic command, printing its output under *label*.

    Only produces output when ``--verbose`` is active.
    """
    if not VERBOSE:
        return
    diag(label)
    result = subprocess.run(args, capture_output=True, text=True, check=False)
    out = (result.stdout or "").rstrip()
    err = (result.stderr or "").rstrip()
    if out:
        for line in out.splitlines():
            print(f"  {line}", flush=True)
    if err:
        for line in err.splitlines():
            print(f"  (stderr) {line}", flush=True)
    if result.returncode != 0:
        print(f"  (exit code {result.returncode})", flush=True)


def _log_network_diagnostics() -> None:
    """Emit non-fatal network diagnostics from the VM, Kind container, and host.

    Only produces output when ``--verbose`` is active.  Called in
    validate_workload() after the pod reaches Running but before we attempt
    ``kubectl logs`` (which proxies through the kubelet and may fail with
    "no route to host" if there is a networking issue between the Kind
    container and the VM).
    """
    if not VERBOSE:
        return
    log("=== Network diagnostics (non-fatal) ===")

    # -- From the VM (via SSH) --
    _run_diag("VM: nft list ruleset",
              ["ssh", *SSH_OPTS, SSH_TARGET, "sudo", "nft", "list", "ruleset"])
    # Show ALL listening TCP sockets (unfiltered) so we can see what port
    # the kubelet is actually on.  sudo is needed to see process names for
    # sockets owned by nspawn processes.
    _run_diag("VM: sudo ss -tlnp (all listening)",
              ["ssh", *SSH_OPTS, SSH_TARGET, "sudo", "ss", "-tlnp"])
    _run_diag("VM: ip addr show",
              ["ssh", *SSH_OPTS, SSH_TARGET, "ip", "addr", "show"])

    # -- From the Kind container --
    _run_diag("Kind: ip addr show eth-e2e",
              ["docker", "exec", KIND_CONTAINER, "ip", "addr", "show", "eth-e2e"])
    _run_diag("Kind: ip route",
              ["docker", "exec", KIND_CONTAINER, "ip", "route"])
    _run_diag("Kind: ip neigh show",
              ["docker", "exec", KIND_CONTAINER, "ip", "neigh", "show"])
    _run_diag("Kind: ping VM",
              ["docker", "exec", KIND_CONTAINER,
               "ping", "-c", "2", "-W", "2", VM_IP])
    _run_diag("Kind: curl kubelet /healthz",
              ["docker", "exec", KIND_CONTAINER,
               "curl", "-sk", "--connect-timeout", "5",
               f"https://{VM_IP}:10250/healthz"])

    # -- From the host --
    # Show ALL interfaces so we can verify veth-kind-e2e exists and is UP.
    _run_diag("Host: ip link show (all)",
              ["ip", "link", "show"])
    _run_diag("Host: ip -d link show type veth",
              ["ip", "-d", "link", "show", "type", "veth"])
    _run_diag("Host: bridge link show",
              ["bridge", "link", "show"])

    log("=== End network diagnostics ===")


# ---------------------------------------------------------------------------
# validate-kube-proxy
# ---------------------------------------------------------------------------
def validate_kube_proxy() -> None:
    """Validate that kube-proxy pods are Running on every node in the cluster.

    kube-proxy requires /lib/modules from the host kernel to load kernel
    modules via modprobe. This check catches regressions where the nspawn
    container does not bind-mount /lib/modules.
    """

    timeout_secs = 180

    # Get all node names.
    node_names_raw = kubectl_capture(["get", "nodes", "-o", "jsonpath={.items[*].metadata.name}"])
    all_nodes = set(node_names_raw.split())
    if not all_nodes:
        die("No nodes found in the cluster")
    log(f"Cluster nodes: {sorted(all_nodes)}")

    # Wait for kube-proxy pods to be Running on every node.
    log(f"Waiting for kube-proxy pods to be Running on all {len(all_nodes)} node(s) "
        f"(timeout: {timeout_secs}s)...")
    elapsed = 0
    while elapsed < timeout_secs:
        result = subprocess.run(
            [KUBECTL, "get", "pods", "-n", "kube-system",
             "-l", "k8s-app=kube-proxy",
             "-o", "json"],
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            if elapsed > 0 and elapsed % 30 == 0:
                log(f"  ({elapsed}s) Failed to list kube-proxy pods")
            time.sleep(5)
            elapsed += 5
            continue

        pods = json.loads(result.stdout).get("items", [])
        running_nodes: set[str] = set()
        for pod in pods:
            phase = pod.get("status", {}).get("phase", "")
            node = pod.get("spec", {}).get("nodeName", "")
            if phase == "Running" and node:
                running_nodes.add(node)

        if running_nodes >= all_nodes:
            log(f"kube-proxy Running on all nodes after {elapsed}s")
            break

        missing = sorted(all_nodes - running_nodes)
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) kube-proxy not yet Running on: {missing}")
        time.sleep(5)
        elapsed += 5
    else:
        log("kube-proxy pod status:")
        subprocess.run([KUBECTL, "get", "pods", "-n", "kube-system",
                        "-l", "k8s-app=kube-proxy", "-o", "wide"], check=False)
        # Show logs from non-Running pods for debugging.
        for pod in pods:
            phase = pod.get("status", {}).get("phase", "")
            name = pod.get("metadata", {}).get("name", "")
            if phase != "Running" and name:
                log(f"Logs for {name}:")
                subprocess.run([KUBECTL, "logs", name, "-n", "kube-system",
                                "--tail=50"], check=False)
        die(f"kube-proxy not Running on all nodes after {timeout_secs}s. "
            f"Missing: {sorted(all_nodes - running_nodes)}")

    log("============================================")
    log("  kube-proxy validation PASSED")
    log("============================================")
    kubectl(["get", "pods", "-n", "kube-system", "-l", "k8s-app=kube-proxy", "-o", "wide"])


# ---------------------------------------------------------------------------
# validate-workload
# ---------------------------------------------------------------------------
def validate_workload() -> None:
    """Deploy test pods on the agent node and verify they run."""

    timeout_secs = 300
    pod_suffix = _safe_name(AGENT_MACHINE_NAME)
    hello_pod_name = f"e2e-hello-{pod_suffix}"
    dns_pod_name = f"e2e-dns-test-{pod_suffix}"

    # Create test namespace (idempotent)
    log(f"Creating test namespace '{TEST_NS}'...")
    ns_yaml = capture([KUBECTL, "create", "namespace", TEST_NS,
                       "--dry-run=client", "-o", "yaml"])
    kubectl(["apply", "-f", "-"], input=ns_yaml.encode())

    # Clean up any stale pods from a previous run (e.g. after reset + rejoin)
    for pod_name in (hello_pod_name, dns_pod_name):
        run_quiet([KUBECTL, "delete", "pod", pod_name, "-n", TEST_NS,
                   "--ignore-not-found"], check=False)

    # Deploy hello pod
    log(f"Deploying test pod on node '{AGENT_MACHINE_NAME}'...")
    hello_pod = {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": hello_pod_name, "namespace": TEST_NS, "labels": {"app": "e2e-hello"}},
        "spec": {
            "nodeName": AGENT_MACHINE_NAME,
            "containers": [{
                "name": "hello",
                "image": E2E_WORKLOAD_IMAGE,
                "command": ["sh", "-c", "echo 'Hello from unbounded agent node!' && sleep 3600"],
            }],
            "restartPolicy": "Never",
            "tolerations": [{"operator": "Exists"}],
        },
    }
    kubectl(["apply", "-f", "-"], input=json.dumps(hello_pod).encode())

    # Wait for Running
    log(f"Waiting for pod '{hello_pod_name}' to be Running...")
    elapsed = 0
    while elapsed < timeout_secs:
        result = subprocess.run(
            [KUBECTL, "get", "pod", hello_pod_name, "-n", TEST_NS,
             "-o", "jsonpath={.status.phase}"],
            capture_output=True, text=True,
        )
        phase = result.stdout.strip() if result.returncode == 0 else ""
        if phase == "Running":
            log(f"Pod '{hello_pod_name}' is Running after {elapsed}s")
            break
        if phase in ("Failed", "Unknown"):
            subprocess.run([KUBECTL, "describe", "pod", hello_pod_name, "-n", TEST_NS], check=False)
            die(f"Pod '{hello_pod_name}' entered {phase} state")
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) Pod phase: {phase or 'Pending'}")
        time.sleep(5)
        elapsed += 5
    else:
        subprocess.run([KUBECTL, "describe", "pod", hello_pod_name, "-n", TEST_NS], check=False)
        die(f"Timed out waiting for pod '{hello_pod_name}' to be Running after {timeout_secs}s")

    # Emit network diagnostics before attempting kubectl logs.  The API
    # server proxies log requests through the kubelet (port 10250) on the
    # agent node.  If the Kind container cannot reach the VM this will fail
    # with "no route to host".  The diagnostics help pinpoint the cause.
    _log_network_diagnostics()

    # Check logs (retry; kubectl logs can fail transiently right after a pod
    # starts because the API server proxies to the kubelet which may not have
    # the log stream ready yet).
    log("Checking pod logs...")
    logs = ""
    log_attempts = 6
    for attempt in range(1, log_attempts + 1):
        result = subprocess.run(
            [KUBECTL, "logs", hello_pod_name, "-n", TEST_NS],
            capture_output=True, text=True,
        )
        if result.returncode == 0:
            logs = result.stdout.strip()
            break
        if attempt < log_attempts:
            log(f"  kubectl logs failed (attempt {attempt}/{log_attempts}): {result.stderr.strip()}")
            time.sleep(5)
        else:
            log(f"  kubectl logs failed (attempt {attempt}/{log_attempts}): {result.stderr.strip()}")
            subprocess.run([KUBECTL, "describe", "pod", hello_pod_name, "-n", TEST_NS], check=False)
            die(f"kubectl logs failed after {log_attempts} attempts")

    print(logs, flush=True)
    if "Hello from unbounded agent node!" not in logs:
        die("Pod logs do not contain expected message")
    log("Pod logs contain expected message")

    # Verify node placement
    pod_node = kubectl_capture(["get", "pod", hello_pod_name, "-n", TEST_NS,
                                 "-o", "jsonpath={.spec.nodeName}"])
    if pod_node != AGENT_MACHINE_NAME:
        die(f"Pod is running on '{pod_node}' instead of '{AGENT_MACHINE_NAME}'")
    log(f"Pod is running on the correct node: {pod_node}")

    # DNS test (non-fatal)
    log("Deploying DNS test pod on agent node...")
    dns_pod = {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": dns_pod_name, "namespace": TEST_NS, "labels": {"app": "e2e-dns-test"}},
        "spec": {
            "nodeName": AGENT_MACHINE_NAME,
            "containers": [{
                "name": "dns",
                "image": E2E_WORKLOAD_IMAGE,
                "command": ["sh", "-c",
                            "nslookup kubernetes.default.svc.cluster.local && echo 'DNS_OK'"],
            }],
            "restartPolicy": "Never",
            "tolerations": [{"operator": "Exists"}],
        },
    }
    kubectl(["apply", "-f", "-"], input=json.dumps(dns_pod).encode())

    log("Waiting for DNS test pod to complete...")
    dns_passed = False
    elapsed = 0
    while elapsed < timeout_secs:
        result = subprocess.run(
            [KUBECTL, "get", "pod", dns_pod_name, "-n", TEST_NS,
             "-o", "jsonpath={.status.phase}"],
            capture_output=True, text=True,
        )
        phase = result.stdout.strip() if result.returncode == 0 else ""
        if phase == "Succeeded":
            log(f"DNS test pod completed successfully after {elapsed}s")
            dns_passed = True
            break
        if phase == "Failed":
            log("DNS test pod failed (this is non-fatal)")
            break
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) DNS test pod phase: {phase or 'Pending'}")
        time.sleep(5)
        elapsed += 5

    dns_result = subprocess.run(
        [KUBECTL, "logs", dns_pod_name, "-n", TEST_NS],
        capture_output=True, text=True,
    )
    dns_logs = dns_result.stdout.strip() if dns_result.returncode == 0 else ""
    if dns_logs:
        print(dns_logs, flush=True)

    if dns_passed and "DNS_OK" in dns_logs:
        log("Cluster DNS resolution works from agent node")
    else:
        log("[WARN] Cluster DNS resolution did not work from agent node (non-fatal)")

    log("============================================")
    log("  Workload validation PASSED")
    log("============================================")
    kubectl(["get", "pods", "-n", TEST_NS, "-o", "wide"])


# ---------------------------------------------------------------------------
# reset-agent
# ---------------------------------------------------------------------------
def reset_agent() -> None:
    """Trigger AgentReset and verify the node is removed."""

    if not SSH_KEY.exists():
        die(f"SSH key not found: {SSH_KEY}. Run create-vm first.")

    operation_name = f"e2e-agent-reset-{int(time.time())}"

    run_quiet([KUBECTL, "delete", _machine_operation_resource(), operation_name,
               "--ignore-not-found"], check=False)

    create_machine_operation(operation_name, AGENT_MACHINE_NAME, "AgentReset")

    operation = wait_for_machine_operation_complete(operation_name, timeout_secs=300)
    status = operation.get("status", {})
    if status.get("message") != "AgentReset completed":
        die(f"unexpected MachineOperation message: {status.get('message')!r}")

    log("AgentReset MachineOperation completed")

    # Verify the node is removed from the cluster
    node_timeout = int(os.environ.get("NODE_TIMEOUT", "120"))
    log(f"Waiting for node '{AGENT_MACHINE_NAME}' to be removed (timeout: {node_timeout}s)...")

    # Delete the node object from the cluster (reset only cleans up the host,
    # the node object must be removed separately).
    run_quiet([KUBECTL, "delete", "node", AGENT_MACHINE_NAME, "--ignore-not-found"], check=False)

    elapsed = 0
    while elapsed < node_timeout:
        ret = subprocess.run(
            [KUBECTL, "get", "node", AGENT_MACHINE_NAME, "-o", "name"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        if ret.returncode != 0:
            log(f"Node '{AGENT_MACHINE_NAME}' removed after {elapsed}s")
            break
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) Node still present...")
        time.sleep(5)
        elapsed += 5
    else:
        die(f"Timed out waiting for node '{AGENT_MACHINE_NAME}' to be removed after {node_timeout}s")

    # Verify the nspawn machines are no longer running on the VM
    log("Verifying nspawn machines are stopped on VM...")
    for nspawn_name in NSPAWN_MACHINE_NAMES:
        result = subprocess.run(
            ["ssh", *SSH_OPTS, SSH_TARGET,
             f"sudo machinectl show {nspawn_name}"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        if result.returncode == 0:
            die(f"nspawn machine '{nspawn_name}' is still running after reset")
        log(f"nspawn machine '{nspawn_name}' is not running")

    log("============================================")
    log("  Agent reset PASSED")
    log("============================================")


# ---------------------------------------------------------------------------
# install-machine-crd
# ---------------------------------------------------------------------------
def install_machine_crd() -> None:
    """Install Machine-related CRDs and bootstrapper RBAC."""

    crd_paths = [
        REPO_ROOT / "deploy" / "machina" / "crd" / "unbounded-cloud.io_machines.yaml",
        REPO_ROOT / "deploy" / "machina" / "crd" / "unbounded-cloud.io_machineoperations.yaml",
        REPO_ROOT / "deploy" / "machina" / "crd" / "unbounded-cloud.io_machineconfigurations.yaml",
        REPO_ROOT / "deploy" / "machina" / "crd" / "unbounded-cloud.io_machineconfigurationversions.yaml",
    ]
    rbac_path = REPO_ROOT / "deploy" / "machina" / "rendered" / "07-bootstrapper-rbac.yaml"

    for crd_path in crd_paths:
        if not crd_path.exists():
            die(f"Machina CRD not found: {crd_path}")

    log("Rendering machina manifests...")
    run(["make", "machina-manifests"], cwd=str(REPO_ROOT))

    if not rbac_path.exists():
        die(f"Bootstrapper RBAC not found after render: {rbac_path}")

    log("Installing Machine-related CRDs...")
    for crd_path in crd_paths:
        kubectl(["apply", "-f", str(crd_path)])

    log("Installing bootstrapper RBAC...")
    kubectl(["apply", "-f", str(rbac_path)])

    log("Machine CRDs and RBAC installed")


# ---------------------------------------------------------------------------
# deploy-unbounded-net-controller
# ---------------------------------------------------------------------------
def deploy_unbounded_net_controller() -> None:
    """Build and deploy the unbounded-net controller into the Kind cluster."""

    log("Building unbounded-net controller...")
    run(["make", "unbounded-net-controller-build"], cwd=str(REPO_ROOT))
    build_e2e_controller_image(
        NET_CONTROLLER_E2E_IMAGE,
        REPO_ROOT / "bin" / "unbounded-net-controller",
        "unbounded-net-controller",
    )
    load_image_into_kind(NET_CONTROLLER_E2E_IMAGE)

    api_server = kind_api_server_url()
    log("Rendering unbounded-net manifests...")
    run([
        "make", "net-manifests",
        f"NET_CONTROLLER_IMAGE={NET_CONTROLLER_E2E_IMAGE}",
        "NET_NODE_IMAGE=unbounded-net-node:agent-e2e-unused",
        f"NET_APISERVER_URL={api_server}",
    ], cwd=str(REPO_ROOT))

    rendered = REPO_ROOT / "deploy" / "net" / "rendered"
    controller = rendered / "controller"
    set_manifest_image_pull_policy(controller / "03-deployment.yaml", "IfNotPresent")
    log("Installing unbounded-net CRDs and controller manifests...")
    for crd_path in sorted((rendered / "crd").glob("*.yaml")):
        apply_manifest(crd_path)
    for manifest_path in sorted(rendered.glob("*.yaml")) + sorted(controller.glob("*.yaml")):
        apply_manifest(manifest_path)

    try:
        wait_for_rollout(UNBOUNDED_NS, "deployment/unbounded-net-controller")
    except subprocess.CalledProcessError:
        print_controller_logs(UNBOUNDED_NS, "app.kubernetes.io/name=unbounded-net-controller")
        die("unbounded-net controller rollout failed")

    log("unbounded-net controller deployed")


# ---------------------------------------------------------------------------
# start-machina-controller
# ---------------------------------------------------------------------------
def start_machina_controller() -> None:
    """Build and deploy the machina controller into the Kind cluster."""

    log("Building machina controller...")
    run(["make", "machina-build"], cwd=str(REPO_ROOT))
    build_e2e_controller_image(MACHINA_E2E_IMAGE, REPO_ROOT / "bin" / "machina", "machina")
    load_image_into_kind(MACHINA_E2E_IMAGE)

    api_server = kind_api_server_url()
    log("Rendering machina manifests...")
    run([
        "make", "machina-manifests",
        f"MACHINA_IMAGE={MACHINA_E2E_IMAGE}",
        f"MACHINA_API_SERVER_ENDPOINT={api_server}",
    ], cwd=str(REPO_ROOT))

    rendered = REPO_ROOT / "deploy" / "machina" / "rendered"
    set_manifest_image_pull_policy(rendered / "04-deployment.yaml", "IfNotPresent")
    log("Installing machina controller manifests...")
    for manifest_path in [
        rendered / "01-namespace.yaml",
        rendered / "03-config.yaml",
        rendered / "02-rbac.yaml",
        rendered / "05-service.yaml",
        rendered / "04-deployment.yaml",
    ]:
        apply_manifest(manifest_path)

    try:
        wait_for_rollout(UNBOUNDED_NS, "deployment/machina-controller")
    except subprocess.CalledProcessError:
        print_controller_logs(UNBOUNDED_NS, "app=machina-controller")
        die("machina controller rollout failed")

    log("Machina controller deployed")


# ---------------------------------------------------------------------------
# validate-controllers-healthy
# ---------------------------------------------------------------------------
def validate_controllers_healthy() -> None:
    """Verify e2e controllers are not crashing or repeating error logs."""

    _validate_controller_health(
        "unbounded-net",
        UNBOUNDED_NS,
        "app.kubernetes.io/name=unbounded-net-controller",
    )
    _validate_controller_health("machina", UNBOUNDED_NS, "app=machina-controller")

    log("Controllers are healthy")


# ---------------------------------------------------------------------------
# validate-machina-controller
# ---------------------------------------------------------------------------
def mcv_name(config_name: str, version: int) -> str:
    """Return the canonical MachineConfigurationVersion name."""

    return f"{config_name}-v{version}"


def validate_machina_controller() -> None:
    """Verify the machina controller reconciles MachineConfiguration objects."""

    name = MACHINE_CONFIG_NAME

    def wait_for_mcv(
        mcv_name: str,
        expected_version: int,
        expected_node_labels: dict[str, str] | None,
    ) -> dict[str, Any]:
        timeout_secs = 60
        elapsed = 0
        while elapsed < timeout_secs:
            result = subprocess.run(
                [KUBECTL, "get", "machineconfigurationversion", mcv_name, "-o", "json"],
                capture_output=True, text=True,
            )
            if result.returncode == 0:
                mcv = json.loads(result.stdout)
                version = mcv.get("spec", {}).get("version")
                config = mcv.get("metadata", {}).get("labels", {}).get(
                    "unbounded-cloud.io/machine-configuration",
                )
                node_labels = mcv.get("spec", {}).get("template", {}).get(
                    "kubernetes", {},
                ).get("nodeLabels")
                if (
                    version == expected_version
                    and config == name
                    and node_labels == expected_node_labels
                ):
                    return mcv

            if elapsed > 0 and elapsed % 15 == 0:
                log(f"  ({elapsed}s) waiting for MachineConfigurationVersion '{mcv_name}'...")
            time.sleep(5)
            elapsed += 5

        print_controller_logs(UNBOUNDED_NS, "app=machina-controller")
        die(f"MachineConfigurationVersion '{mcv_name}' was not ready after {timeout_secs}s")

    log(f"Validating machina controller with MachineConfiguration '{name}'...")
    version_json = json.loads(kubectl_capture(["version", "-o", "json"]))
    server_version = version_json.get("serverVersion", {}).get("gitVersion")
    if not server_version:
        die(f"Could not resolve server version from kubectl version: {version_json}")

    manifest = {
        "apiVersion": "unbounded-cloud.io/v1alpha3",
        "kind": "MachineConfiguration",
        "metadata": {
            "name": name,
            "labels": {"e2e.unbounded-cloud.io/test": "agent-kind"},
        },
        "spec": {
            "template": {
                "kubernetes": {
                    "version": server_version,
                },
            },
        },
    }
    kubectl(["apply", "-f", "-"], input=json.dumps(manifest).encode())

    v1_name = mcv_name(name, 1)
    v1 = wait_for_mcv(v1_name, 1, None)
    log(f"MachineConfigurationVersion '{v1_name}' created")

    kubectl([
        "patch", "machineconfigurationversion", v1_name,
        "--subresource=status", "--type=merge",
        "-p", json.dumps({
            "status": {
                "deployed": True,
                "deployedMachines": 1,
            },
        }),
    ])

    updated_node_labels = {"e2e.unbounded-cloud.io/config-version": "v2"}
    manifest["spec"]["template"]["kubernetes"]["nodeLabels"] = updated_node_labels
    kubectl(["apply", "-f", "-"], input=json.dumps(manifest).encode())

    v2_name = mcv_name(name, 2)
    wait_for_mcv(v2_name, 2, updated_node_labels)
    log(f"MachineConfigurationVersion '{v2_name}' created after config change")

    v1 = json.loads(kubectl_capture([
        "get", "machineconfigurationversion", v1_name, "-o", "json",
    ]))
    v1_node_labels = v1.get("spec", {}).get("template", {}).get(
        "kubernetes", {},
    ).get("nodeLabels")
    if v1_node_labels is not None:
        die(f"MachineConfigurationVersion '{v1_name}' changed unexpectedly: {v1_node_labels}")


# ---------------------------------------------------------------------------
# delete-machine-cr
# ---------------------------------------------------------------------------
def delete_machine_cr() -> None:
    """Delete the Machine CR (idempotent)."""

    log(f"Deleting Machine CR '{AGENT_MACHINE_NAME}'...")
    run_quiet([KUBECTL, "delete", "machine", AGENT_MACHINE_NAME,
               "--ignore-not-found"], check=False)
    log(f"Machine CR '{AGENT_MACHINE_NAME}' deleted")


# ---------------------------------------------------------------------------
# validate-machine-cr-created
# ---------------------------------------------------------------------------
def validate_machine_cr_created(node_config: NodeConfig) -> None:
    """Validate the agent self-registered a Machine CR during bootstrap.

    The daemon registers the Machine CR at startup, so this function polls
    until the CR appears (with a timeout). Once found, it asserts the CR
    does NOT have the pre-created marker annotation and has the correct
    ``bootstrapTokenRef`` derived from the bootstrap token created by
    run-agent.
    """

    token_id_file = VM_DIR / "token-id"
    if not token_id_file.exists():
        die(f"Token ID file not found: {token_id_file}. Run run-agent first.")
    token_id = token_id_file.read_text().strip()

    log(f"Validating agent-created Machine CR '{AGENT_MACHINE_NAME}'...")

    # Poll for the Machine CR to appear (the daemon registers it
    # asynchronously after startup).
    timeout_secs = 120
    elapsed = 0
    machine_json = None
    while elapsed < timeout_secs:
        result = subprocess.run(
            [KUBECTL, "get", "machine", AGENT_MACHINE_NAME, "-o", "json"],
            capture_output=True, text=True,
        )
        if result.returncode == 0:
            machine_json = result.stdout
            log(f"Machine CR '{AGENT_MACHINE_NAME}' found after {elapsed}s")
            break
        if elapsed > 0 and elapsed % 15 == 0:
            log(f"  ({elapsed}s) Machine CR not yet created...")
        time.sleep(5)
        elapsed += 5
    else:
        die(f"Machine CR '{AGENT_MACHINE_NAME}' not found after {timeout_secs}s - "
            f"expected daemon to create it")

    machine = json.loads(machine_json)

    # Must NOT have the pre-created marker.
    annotations = machine.get("metadata", {}).get("annotations", {})
    if "e2e-test/precreated" in annotations:
        die("e2e-test/precreated annotation found - CR was not created by the agent")

    # Verify bootstrapTokenRef.
    k8s_spec = machine.get("spec", {}).get("kubernetes", {})
    token_ref = k8s_spec.get("bootstrapTokenRef", {}).get("name", "")
    expected_ref = f"bootstrap-token-{token_id}"
    if token_ref != expected_ref:
        die(f"bootstrapTokenRef mismatch: got '{token_ref}', expected '{expected_ref}'")

    log("bootstrapTokenRef is correct")

    expected_labels = expected_node_labels(node_config)
    actual_labels = k8s_spec.get("nodeLabels") or {}
    for key, value in expected_labels.items():
        actual = actual_labels.get(key)
        if actual != value:
            die(f"Machine CR nodeLabels mismatch for {key!r}: got {actual!r}, expected {value!r}")

    expected_taints = expected_node_taint_strings(node_config)
    actual_taints = k8s_spec.get("registerWithTaints") or []
    for taint in expected_taints:
        if taint not in actual_taints:
            die(f"Machine CR registerWithTaints missing {taint!r}: {actual_taints}")

    log("============================================")
    log("  Machine CR validation PASSED (created)")
    log("============================================")


# ---------------------------------------------------------------------------
# validate-node-reboot-operation
# ---------------------------------------------------------------------------
def validate_node_reboot_operation() -> None:
    """Validate that a NodeReboot MachineOperation restarts the agent node."""

    operation_name = f"e2e-node-reboot-{int(time.time())}"

    log(f"Validating NodeReboot operation for '{AGENT_MACHINE_NAME}'...")
    previous_boot_id = node_boot_id(AGENT_MACHINE_NAME)
    if not previous_boot_id:
        die(f"Node '{AGENT_MACHINE_NAME}' did not report a boot ID")
    log(f"Current node boot ID: {previous_boot_id}")

    run_quiet([KUBECTL, "delete", _machine_operation_resource(), operation_name,
               "--ignore-not-found"], check=False)

    create_machine_operation(operation_name, AGENT_MACHINE_NAME, "NodeReboot")

    operation = wait_for_machine_operation_complete(operation_name)
    status = operation.get("status", {})
    if status.get("message") != "NodeReboot completed":
        die(f"unexpected MachineOperation message: {status.get('message')!r}")

    conditions = status.get("conditions", [])
    completed_conditions = [
        c for c in conditions
        if c.get("type") == "Completed" and c.get("status") == "True" and c.get("reason") == "Succeeded"
    ]
    if not completed_conditions:
        die(f"MachineOperation missing Completed=True/Succeeded condition: {conditions}")

    new_boot_id = wait_for_node_boot_id_change(AGENT_MACHINE_NAME, previous_boot_id)
    wait_for_node_reboot_event(AGENT_MACHINE_NAME, new_boot_id)
    wait_for_node_ready(AGENT_MACHINE_NAME)

    log("============================================")
    log("  NodeReboot operation validation PASSED")
    log("============================================")
    kubectl(["get", _machine_operation_resource(), operation_name, "-o", "wide"])
    kubectl(["get", "node", AGENT_MACHINE_NAME, "-o", "wide"])


# ---------------------------------------------------------------------------
# validate-agent-upgrade-operation
# ---------------------------------------------------------------------------
def validate_agent_upgrade_operation() -> None:
    """Validate AgentUpgrade stages a new daemon binary and updates symlinks."""

    operation_name = f"e2e-agent-upgrade-{int(time.time())}"
    before_current = read_daemon_current_target()
    if not before_current:
        die("daemon current binary symlink target was empty")
    log(f"Current daemon binary before upgrade: {before_current}")

    tarball = VM_DIR / "unbounded-agent-upgrade-good.tar.gz"
    _build_agent_upgrade_tarball(tarball)
    operation = _serve_agent_upgrade_tarball(tarball, operation_name)

    status = operation.get("status", {})
    if status.get("message") != "AgentUpgrade completed":
        die(f"unexpected MachineOperation message: {status.get('message')!r}")

    wait_for_daemon_active()
    after_current = read_daemon_current_target()
    last_good = read_daemon_last_good_target()
    log(f"Current daemon binary after upgrade: {after_current}")
    log(f"Last-good daemon binary after upgrade: {last_good}")

    if after_current == before_current:
        die(f"AgentUpgrade did not switch the daemon current symlink (still points to {after_current})")
    if last_good != before_current:
        die(f"last-good symlink mismatch: got {last_good!r}, expected {before_current!r}")

    log("============================================")
    log("  AgentUpgrade operation validation PASSED")
    log("============================================")
    kubectl(["get", _machine_operation_resource(), operation_name, "-o", "wide"])


# ---------------------------------------------------------------------------
# validate-agent-upgrade-rollback
# ---------------------------------------------------------------------------
def validate_agent_upgrade_rollback() -> None:
    """Validate systemd recovery rolls back from a failing upgraded daemon."""

    previous_good = read_daemon_current_target()
    if not previous_good:
        die("daemon current binary symlink target was empty")
    log(f"Current daemon binary before failing upgrade: {previous_good}")

    broken_operation_name = f"e2e-agent-upgrade-broken-{int(time.time())}"
    broken_tarball = VM_DIR / "unbounded-agent-upgrade-broken.tar.gz"
    _build_failing_agent_tarball(broken_tarball)
    broken_operation = _serve_agent_upgrade_tarball(
        broken_tarball, broken_operation_name, expect_complete=False)
    broken_status = broken_operation.get("status", {})
    log(f"Broken AgentUpgrade failure reason: {broken_status.get('reason')!r}")
    if "verify agent binary" not in broken_status.get("message", ""):
        die(f"unexpected broken AgentUpgrade failure message: {broken_status.get('message')!r}")
    if read_daemon_current_target() != previous_good:
        die("broken AgentUpgrade changed current daemon binary symlink")

    operation_name = f"e2e-agent-upgrade-rollback-{int(time.time())}"
    tarball = VM_DIR / "unbounded-agent-upgrade-daemon-bad.tar.gz"
    _build_daemon_failing_agent_tarball(tarball)
    failed_operation = _serve_agent_upgrade_tarball(tarball, operation_name, expect_complete=False)
    wait_for_daemon_current_target(previous_good)
    wait_for_daemon_active()
    failed_status = failed_operation.get("status", {})
    log(f"Rollback AgentUpgrade failure reason: {failed_status.get('reason')!r}")
    if AGENT_UPGRADE_ROLLBACK_MESSAGE_FRAGMENT not in failed_status.get("message", ""):
        die(f"unexpected rollback AgentUpgrade failure message: {failed_status.get('message')!r}")

    log("============================================")
    log("  AgentUpgrade rollback validation PASSED")
    log("============================================")
    kubectl(["get", _machine_operation_resource(), operation_name, "-o", "jsonpath={.status.reason}{'\\n'}{.status.message}{'\\n'}"])


# ---------------------------------------------------------------------------
# validate-node-repave-upgrade
# ---------------------------------------------------------------------------
def _next_patch_version(version: str) -> str:
    """Return the next Kubernetes patch version for a vMAJOR.MINOR.PATCH string."""

    base = version.strip().lstrip("v")
    parts = base.split(".")
    if len(parts) != 3 or not all(part.isdigit() for part in parts):
        die(f"Cannot derive patch upgrade from Kubernetes version {version!r}")

    parts[2] = str(int(parts[2]) + 1)
    return "v" + ".".join(parts)


def ensure_machine_configuration_for_repave(
    config_name: str,
    kubernetes_version: str,
    node_config: NodeConfig,
) -> None:
    """Create the per-machine MachineConfiguration if setup did not pre-create it."""

    result = subprocess.run(
        [KUBECTL, "get", "machineconfiguration", config_name],
        capture_output=True,
        text=True,
    )
    if result.returncode == 0:
        return

    output = result.stdout + result.stderr
    if "NotFound" not in output and "not found" not in output:
        die(f"failed to get MachineConfiguration '{config_name}': {output.strip()}")

    log(f"Creating MachineConfiguration '{config_name}' for repave validation...")
    kubernetes_template: dict[str, Any] = {"version": kubernetes_version}
    labels = expected_node_labels(node_config)
    taints = expected_node_taints(node_config)
    if labels:
        kubernetes_template["nodeLabels"] = labels
    if taints:
        kubernetes_template["registerWithTaints"] = taints

    manifest = {
        "apiVersion": "unbounded-cloud.io/v1alpha3",
        "kind": "MachineConfiguration",
        "metadata": {
            "name": config_name,
            "labels": {"e2e.unbounded-cloud.io/test": "agent-kind"},
        },
        "spec": {
            "updateStrategy": {"type": "OnDelete"},
            "template": {
                "kubernetes": kubernetes_template,
            },
        },
    }
    kubectl(["apply", "-f", "-"], input=json.dumps(manifest).encode())


def validate_node_repave_upgrade(node_config: NodeConfig) -> None:
    """Validate OnDelete repave applies a new MCV Kubernetes version."""

    config_name = MACHINE_CONFIG_NAME

    current_kubelet_version = node_kubelet_version(AGENT_MACHINE_NAME)
    if not current_kubelet_version:
        die(f"Node '{AGENT_MACHINE_NAME}' did not report a kubelet version")
    target_kubelet_version = _next_patch_version(current_kubelet_version)

    log("Validating OnDelete repave upgrade...")
    log(f"Current kubelet version: {current_kubelet_version}")
    log(f"Target kubelet version: {target_kubelet_version}")

    ensure_machine_configuration_for_repave(config_name, current_kubelet_version, node_config)
    manifest = json.loads(kubectl_capture(["get", "machineconfiguration", config_name, "-o", "json"]))
    metadata = manifest.setdefault("metadata", {})
    for key in ["creationTimestamp", "generation", "resourceVersion", "uid", "managedFields"]:
        metadata.pop(key, None)
    manifest.pop("status", None)
    kubernetes_template = manifest.setdefault("spec", {}).setdefault(
        "template", {},
    ).setdefault("kubernetes", {})
    kubernetes_template["version"] = target_kubelet_version
    kubernetes_template["nodeLabels"] = {
        **expected_node_labels(node_config),
        "e2e.unbounded-cloud.io/config-version": "v3",
    }
    kubectl(["apply", "-f", "-"], input=json.dumps(manifest).encode())

    timeout_secs = 120
    elapsed = 0
    target_version_number = 0
    target_mcv = ""
    while elapsed < timeout_secs:
        result = subprocess.run(
            [KUBECTL, "get", "machineconfigurationversion",
             "-l", f"unbounded-cloud.io/machine-configuration={config_name}", "-o", "json"],
            capture_output=True, text=True,
        )
        if result.returncode == 0:
            mcvs = json.loads(result.stdout).get("items", [])
            matching_mcvs = [
                mcv for mcv in mcvs
                if mcv.get("spec", {}).get("template", {}).get("kubernetes", {}).get("version")
                == target_kubelet_version
            ]
            if matching_mcvs:
                latest = max(matching_mcvs, key=lambda mcv: mcv.get("spec", {}).get("version", 0))
                target_version_number = latest.get("spec", {}).get("version", 0)
                target_mcv = latest.get("metadata", {}).get("name", "")
                log(f"MachineConfigurationVersion '{target_mcv}' is ready")
                break
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) waiting for MCV with Kubernetes {target_kubelet_version}...")
        time.sleep(5)
        elapsed += 5
    else:
        print_controller_logs(UNBOUNDED_NS, "app=machina-controller")
        die(f"No MachineConfigurationVersion reached Kubernetes {target_kubelet_version} after {timeout_secs}s")

    if target_version_number == 0 or not target_mcv:
        die("Resolved target MachineConfigurationVersion was empty")

    old_nspawn = active_nspawn_machine()
    create_bpffs_sentinel(old_nspawn)

    log(f"Assigning Machine '{AGENT_MACHINE_NAME}' to {target_mcv}...")
    run([KUBECTL_UNBOUNDED, "machine", "config", "assign", AGENT_MACHINE_NAME,
         "--config", config_name, "--version", str(target_version_number)])

    log(f"Deleting Node '{AGENT_MACHINE_NAME}' to trigger OnDelete repave...")
    kubectl(["delete", "node", AGENT_MACHINE_NAME])
    wait_for_node_absent(AGENT_MACHINE_NAME)
    wait_for_node()
    new_nspawn = active_nspawn_machine()
    if new_nspawn == old_nspawn:
        die(f"repave did not switch nspawn machines: still using {new_nspawn}")
    assert_bpffs_sentinel_absent(new_nspawn)
    wait_for_node_kubelet_version(AGENT_MACHINE_NAME, target_kubelet_version)
    node = json.loads(kubectl_capture(["get", "node", AGENT_MACHINE_NAME, "-o", "json"]))
    _assert_expected_node_config(node, node_config)

    machine = json.loads(kubectl_capture(["get", "machine", AGENT_MACHINE_NAME, "-o", "json"]))
    status_config = machine.get("status", {}).get("configuration", {})
    if status_config.get("version") != target_version_number or status_config.get("versionName") != target_mcv:
        die(f"Machine status.configuration did not record {target_mcv}: {status_config}")

    conditions = machine.get("status", {}).get("conditions", [])
    repave_applied = [
        c for c in conditions
        if c.get("type") == "RepavePending" and c.get("status") == "False" and c.get("reason") == "Applied"
    ]
    if not repave_applied:
        die(f"Machine missing RepavePending=False/Applied condition: {conditions}")

    log("============================================")
    log("  OnDelete repave upgrade validation PASSED")
    log("============================================")
    kubectl(["get", "machine", AGENT_MACHINE_NAME, "-o", "wide"])
    kubectl(["get", "node", AGENT_MACHINE_NAME, "-o", "wide"])

# ---------------------------------------------------------------------------
# collect-logs
# ---------------------------------------------------------------------------
def _write_command_log(path: Path, args: list[str]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w") as out:
        subprocess.run(args, stdout=out, stderr=subprocess.STDOUT, check=False)


def _collect_one_vm_logs(logs_dir: Path, vm_name: str, vm_ip: str, vm_dir: Path, prefix: str) -> None:
    serial_log = vm_dir / f"{vm_name}.log"
    if serial_log.exists():
        shutil.copyfile(serial_log, logs_dir / f"{prefix}vm-serial.log")

    ssh_opts = [
        "-o", "StrictHostKeyChecking=no",
        "-o", "UserKnownHostsFile=/dev/null",
        "-o", "ConnectTimeout=5",
        "-i", str(vm_dir / "ssh" / "id_ed25519"),
    ]
    ssh_target = f"{VM_SSH_USER}@{vm_ip}"

    for name in ("unbounded-agent-preflight.txt", "unbounded-agent-preflight.json"):
        src = vm_dir / name
        if src.exists():
            shutil.copyfile(src, logs_dir / f"{prefix}{name}")

    for name in ("unbounded-agent-preflight.txt", "unbounded-agent-preflight.json"):
        result = subprocess.run(
            ["scp", *ssh_opts, f"{ssh_target}:/tmp/{name}", str(logs_dir / f"{prefix}{name}")],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        if result.returncode == 0:
            diag(f"Collected {name} from VM")

    def ssh_log(name: str, command: str) -> None:
        _write_command_log(logs_dir / f"{prefix}{name}", ["ssh", *ssh_opts, ssh_target, command])

    ssh_log("vm-journal.log", "sudo journalctl --no-pager -l")
    ssh_log("vm-unbounded-agent.log", "sudo journalctl -u unbounded-agent --no-pager -l")
    ssh_log("vm-unbounded-agent-daemon.log", "sudo journalctl -u unbounded-agent-daemon --no-pager -l")
    ssh_log("vm-systemd-machined.log", "sudo journalctl -u systemd-machined --no-pager -l")
    ssh_log("vm-machines.txt", "sudo machinectl list --no-pager")
    ssh_log("vm-nspawn-locks.txt", "sudo ls -la /run/systemd/nspawn/locks 2>&1 || true")
    ssh_log("vm-lslocks.txt", "sudo lslocks || true")
    ssh_log("vm-selinux-avc.txt", "sudo ausearch -m AVC,USER_AVC -ts recent 2>&1 || true")
    for machine in NSPAWN_MACHINE_NAMES:
        ssh_log(f"nspawn-{machine}-journal.log", f"sudo journalctl -M {machine} --no-pager -l")
        ssh_log(f"nspawn-{machine}-kubelet.log", f"sudo journalctl -M {machine} -u kubelet --no-pager -l")
        ssh_log(f"nspawn-{machine}-containerd.log", f"sudo journalctl -M {machine} -u containerd --no-pager -l")
        ssh_log(f"vm-machine-{machine}-status.txt", f"sudo machinectl status {machine} --no-pager")
        ssh_log(f"vm-machine-{machine}-service-status.txt", f"sudo systemctl status systemd-nspawn@{machine}.service --no-pager")
        ssh_log(f"vm-machine-{machine}-mounts.txt", f"sudo findmnt -R /var/lib/machines/{machine} 2>&1 || true")
        ssh_log(f"vm-machine-{machine}-rootfs.txt", f"sudo ls -la /var/lib/machines/{machine} 2>&1 || true")
        ssh_log(
            f"nspawn-{machine}-units.txt",
            f"sudo machinectl shell {machine} /usr/bin/systemctl list-units --no-pager",
        )


def collect_logs() -> None:
    """Collect VM and cluster diagnostics into the logs directory."""
    logs_dir = REPO_ROOT / "logs"
    logs_dir.mkdir(parents=True, exist_ok=True)

    if os.environ.get("COLLECT_NODE_CONFIG_LOGS", "").lower() == "true":
        for index, cfg in enumerate(discover_node_configs()):
            env = scenario_env(cfg, index)
            prefix = f"{_safe_name(cfg.name)}-"
            _collect_one_vm_logs(
                logs_dir,
                env["VM_NAME"],
                env["VM_IP"],
                Path(env["VM_DIR"]),
                prefix,
            )
    else:
        _collect_one_vm_logs(logs_dir, VM_NAME, VM_IP, VM_DIR, "")

    _write_command_log(logs_dir / "nodes.txt", [KUBECTL, "get", "nodes", "-o", "wide"])
    _write_command_log(logs_dir / "nodes-describe.txt", [KUBECTL, "describe", "nodes"])
    _write_command_log(logs_dir / "pods.txt", [KUBECTL, "get", "pods", "-A", "-o", "wide"])
    _write_command_log(logs_dir / "events.txt", [KUBECTL, "get", "events", "-A", "--sort-by=.lastTimestamp"])
    _write_command_log(logs_dir / "machina-controller.log", [KUBECTL, "logs", "-n", UNBOUNDED_NS, "--all-containers", "--prefix", "-l", "app=machina-controller"])
    _write_command_log(logs_dir / "machina-controller-previous.log", [KUBECTL, "logs", "-n", UNBOUNDED_NS, "--all-containers", "--prefix", "--previous", "-l", "app=machina-controller"])
    _write_command_log(logs_dir / "unbounded-net-controller.log", [KUBECTL, "logs", "-n", UNBOUNDED_NS, "--all-containers", "--prefix", "-l", "app.kubernetes.io/name=unbounded-net-controller"])
    _write_command_log(logs_dir / "unbounded-net-controller-previous.log", [KUBECTL, "logs", "-n", UNBOUNDED_NS, "--all-containers", "--prefix", "--previous", "-l", "app.kubernetes.io/name=unbounded-net-controller"])
    _write_command_log(logs_dir / "kindnet.log", [KUBECTL, "logs", "-n", "kube-system", "--all-containers", "--prefix", "-l", "app=kindnet"])
    _write_command_log(logs_dir / "kindnet-previous.log", [KUBECTL, "logs", "-n", "kube-system", "--all-containers", "--prefix", "--previous", "-l", "app=kindnet"])
    _write_command_log(logs_dir / "machines.txt", [KUBECTL, "get", "machines", "-o", "wide"])
    _write_command_log(logs_dir / "machines-full.yaml", [KUBECTL, "get", "machines", "-o", "yaml"])
    _write_command_log(logs_dir / "machineconfigurations.txt", [KUBECTL, "get", "machineconfigurations", "-o", "wide"])
    _write_command_log(logs_dir / "machineconfigurations-full.yaml", [KUBECTL, "get", "machineconfigurations", "-o", "yaml"])
    _write_command_log(logs_dir / "machineconfigurationversions.txt", [KUBECTL, "get", "machineconfigurationversions", "-o", "wide"])
    _write_command_log(logs_dir / "machineconfigurationversions-full.yaml", [KUBECTL, "get", "machineconfigurationversions", "-o", "yaml"])
    _write_command_log(logs_dir / "machineoperations.txt", [KUBECTL, "get", "machineoperations", "-o", "wide"])
    _write_command_log(logs_dir / "machineoperations-full.yaml", [KUBECTL, "get", "machineoperations", "-o", "yaml"])
    _write_command_log(logs_dir / "kind-kubelet.log", ["docker", "exec", KIND_CONTAINER, "journalctl", "-u", "kubelet", "--no-pager", "-l"])
    kube_apiserver = subprocess.run(
        ["docker", "exec", KIND_CONTAINER, "crictl", "ps", "-a", "--name", "kube-apiserver", "-q"],
        capture_output=True, text=True, check=False,
    )
    apiserver_id = kube_apiserver.stdout.splitlines()[0] if kube_apiserver.stdout.splitlines() else ""
    if apiserver_id:
        _write_command_log(logs_dir / "kube-apiserver.log", ["docker", "exec", KIND_CONTAINER, "crictl", "logs", apiserver_id])
    _write_command_log(logs_dir / "clusterrolebindings.txt", [KUBECTL, "get", "clusterrolebindings", "-o", "wide"])
    _write_command_log(logs_dir / "clusterrolebindings-full.yaml", [KUBECTL, "get", "clusterrolebindings", "-o", "yaml"])
    _write_command_log(logs_dir / "csrs.txt", [KUBECTL, "get", "csr", "-o", "wide"])
    _write_command_log(logs_dir / "csrs-describe.txt", [KUBECTL, "describe", "csr"])
    _write_command_log(
        logs_dir / "bootstrap-tokens.txt",
        [KUBECTL, "get", "secrets", "-n", "kube-system", "-l", "kubernetes.io/legacy-token-last-used", "-o", "wide"],
    )
    _write_command_log(
        logs_dir / "bootstrap-token-secrets.yaml",
        [KUBECTL, "get", "secrets", "-n", "kube-system", "--field-selector", "type=bootstrap.kubernetes.io/token", "-o", "yaml"],
    )
    _write_command_log(logs_dir / "workload-pods-describe.txt", [KUBECTL, "describe", "pods", "-n", TEST_NS])
    _write_command_log(logs_dir / "workload-hello.log", [KUBECTL, "logs", "-n", TEST_NS, "--all-containers", "--prefix", "-l", "app=e2e-hello"])
    _write_command_log(logs_dir / "workload-dns.log", [KUBECTL, "logs", "-n", TEST_NS, "--all-containers", "--prefix", "-l", "app=e2e-dns-test"])


# ---------------------------------------------------------------------------
# cleanup
# ---------------------------------------------------------------------------
def cleanup() -> None:
    """Tear down VM, networking, and Kind cluster."""

    # Remove blocked-network rules while local infrastructure containers still
    # exist, so rule reconstruction can include their current IPs.
    unblock_all_external_network_rules()

    if shutil.which(CONTAINER_ENGINE):
        run_quiet([CONTAINER_ENGINE, "rm", "-f", LOCAL_ARTIFACT_REGISTRY_NAME], check=False)

    # Stop QEMU VM
    _stop_qemu()
    if os.environ.get("COLLECT_NODE_CONFIG_LOGS", "").lower() == "true" or VM_NAME == "agent-config-e2e":
        for index, cfg in enumerate(discover_node_configs()):
            env = scenario_env(cfg, index)
            _stop_qemu_by_pid_file(Path(env["VM_DIR"]) / f"{env['VM_NAME']}.pid", env["VM_NAME"])

    # Remove networking
    log("Cleaning up networking...")
    unblock_all_external_network_rules()
    run_quiet(["sudo", "ip", "link", "del", TAP_NAME], check=False)
    if VM_NAME == "agent-config-e2e":
        for index, _cfg in enumerate(discover_node_configs()):
            run_quiet(["sudo", "ip", "link", "del", f"tap-e2e-{index}"], check=False)
    run_quiet(["sudo", "ip", "link", "del", BRIDGE_NAME], check=False)

    # Remove iptables/nftables forwarding rules (best-effort).
    # Rules may have been inserted via legacy iptables (into FORWARD) or
    # via native nft (into the nftables DOCKER-USER chain). We attempt
    # removal from both paths since we don't know which was used.
    for rule in [
        ["sudo", "iptables", "-D", "FORWARD", "-i", BRIDGE_NAME, "-j", "ACCEPT"],
        ["sudo", "iptables", "-D", "FORWARD", "-o", BRIDGE_NAME, "-j", "ACCEPT"],
        ["sudo", "iptables", "-t", "raw", "-D", "PREROUTING", "-i", BRIDGE_NAME, "-j", "ACCEPT"],
        ["sudo", "iptables", "-t", "nat", "-D", "POSTROUTING",
         "-s", f"{VM_SUBNET}.0/24", "!", "-d", f"{VM_SUBNET}.0/24", "-j", "MASQUERADE"],
    ]:
        run_quiet(rule, check=False)

    # On nftables-managed Docker (Fedora, Arch, etc.) rules were inserted
    # directly via nft into ip filter DOCKER-USER. Remove them by handle.
    if shutil.which("nft"):
        try:
            out = subprocess.run(
                ["sudo", "nft", "-a", "list", "chain", "ip", "filter", "DOCKER-USER"],
                capture_output=True, text=True,
            )
            if out.returncode == 0:
                for line in out.stdout.splitlines():
                    if BRIDGE_NAME in line and "handle" in line:
                        handle = line.strip().split()[-1]
                        run_quiet(["sudo", "nft", "delete", "rule", "ip",
                                   "filter", "DOCKER-USER", "handle", handle],
                                  check=False)
        except Exception:
            pass  # best-effort

    # Delete Kind cluster
    if shutil.which("kind"):
        log(f"Deleting Kind cluster '{KIND_CLUSTER_NAME}'...")
        run_quiet(["kind", "delete", "cluster", "--name", KIND_CLUSTER_NAME], check=False)

    # Remove VM artifacts
    if VM_DIR.exists():
        log(f"Removing VM artifacts: {VM_DIR}")
        shutil.rmtree(VM_DIR, ignore_errors=True)

    log("Cleanup complete")


# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
Command = Callable[[NodeConfig], None]


def _without_node_config(func: Callable[[], None]) -> Command:
    """Adapt a command that does not use node config settings."""
    def command(_node_config: NodeConfig) -> None:
        func()

    return command


COMMANDS: dict[str, Command] = {
    "collect-logs": _without_node_config(collect_logs),
    "create-vm-bridge": _without_node_config(create_vm_bridge),
    "create-vm": _without_node_config(create_vm),
    "prepare-blocked-network-vm": _without_node_config(prepare_blocked_network_vm),
    "block-external-network": _without_node_config(block_external_network),
    "unblock-external-network": _without_node_config(unblock_external_network),
    "ensure-kind-bridge": _without_node_config(ensure_kind_bridge),
    "configure-kind-node-ip": _without_node_config(configure_kind_node_ip),
    "dump-persisted-agent-config": _without_node_config(dump_persisted_agent_config),
    "launch-vm": _without_node_config(launch_vm),
    "run-agent": run_agent,
    "wait-for-node": _without_node_config(wait_for_node),
    "wait-for-node-registered": _without_node_config(wait_for_node_registered),
    "validate-host-nspawn-distro": _without_node_config(validate_host_nspawn_distro),
    "validate-node-config": validate_node_config,
    "validate-kube-proxy": _without_node_config(validate_kube_proxy),
    "validate-workload": _without_node_config(validate_workload),
    "install-machine-crd": _without_node_config(install_machine_crd),
    "deploy-unbounded-net-controller": _without_node_config(deploy_unbounded_net_controller),
    "start-machina-controller": _without_node_config(start_machina_controller),
    "validate-machina-controller": _without_node_config(validate_machina_controller),
    "validate-controllers-healthy": _without_node_config(validate_controllers_healthy),
    "delete-machine-cr": _without_node_config(delete_machine_cr),
    "validate-machine-cr-created": validate_machine_cr_created,
    "validate-node-reboot-operation": _without_node_config(validate_node_reboot_operation),
    "validate-agent-upgrade-operation": _without_node_config(validate_agent_upgrade_operation),
    "validate-agent-upgrade-rollback": _without_node_config(validate_agent_upgrade_rollback),
    "validate-node-repave-upgrade": validate_node_repave_upgrade,
    "validate-node-configs": _without_node_config(validate_node_config_scenarios),
    "reset-agent": _without_node_config(reset_agent),
    "cleanup": _without_node_config(cleanup),
}


def main() -> None:
    global VERBOSE  # noqa: PLW0603

    parser = argparse.ArgumentParser(
        description="Agent E2E Kind test harness",
    )
    parser.add_argument(
        "command",
        choices=sorted(COMMANDS),
        help="Subcommand to run",
    )
    parser.add_argument(
        "--verbose",
        action="store_true",
        default=False,
        help="Enable verbose diagnostic output",
    )
    parser.add_argument(
        "--node-config",
        default="",
        help="Path to a JSON node config variant file",
    )
    parser.add_argument(
        "--offline-artifacts-oci-ref",
        default="",
        help="Override offlineArtifactsOCIRef from the node config JSON",
    )
    parser.add_argument(
        "--offline-rootfs-oci-image",
        default="",
        help="Override offlineRootfsOCIImage from the node config JSON",
    )
    args = parser.parse_args()
    VERBOSE = args.verbose
    node_config = load_node_config(
        args.node_config,
        offline_artifacts_oci_ref_override=args.offline_artifacts_oci_ref,
        offline_rootfs_oci_image_override=args.offline_rootfs_oci_image,
    )

    COMMANDS[args.command](node_config)


if __name__ == "__main__":
    main()
