#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Agent E2E Kind test.

Creates a QEMU VM, joins it to a Kind cluster using the production
provision install script, and validates workloads run on the new node.

Subcommands (called as individual workflow steps):
    create-vm          Create bridge networking and launch a QEMU VM.
    run-agent          Build agent, extract cluster info, run provision script.
    wait-for-node      Wait for the node to appear and become Ready.
    validate-workload  Deploy test pods on the agent node.
    cleanup            Tear down VM, networking, and Kind cluster.
"""

from __future__ import annotations

import base64
import json
import os
import secrets
import shutil
import subprocess
import sys
import textwrap
import time
from http.server import HTTPServer, SimpleHTTPRequestHandler
from pathlib import Path
from threading import Thread
from typing import Any

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

KIND_CLUSTER_NAME = os.environ.get("KIND_CLUSTER_NAME", "kind")
AGENT_MACHINE_NAME = os.environ.get("AGENT_MACHINE_NAME", "agent-e2e")
AGENT_DEBUG = os.environ.get("AGENT_DEBUG", "")

BRIDGE_NAME = "virbr-e2e"
TAP_NAME = "tap-e2e"
SERVE_PORT = 8199

SSH_KEY_DIR = VM_DIR / "ssh"
SSH_KEY = SSH_KEY_DIR / "id_ed25519"
SSH_OPTS = [
    "-o", "StrictHostKeyChecking=no",
    "-o", "UserKnownHostsFile=/dev/null",
    "-o", "ConnectTimeout=10",
    "-i", str(SSH_KEY),
]
SSH_TARGET = f"ubuntu@{VM_IP}"

INSTALL_SCRIPT = REPO_ROOT / "internal" / "provision" / "assets" / "unbounded-agent-install.sh"
KUBECTL = "kubectl"

TEST_NS = "e2e-workload-test"


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
def log(msg: str) -> None:
    print(f"[INFO]  {msg}", flush=True)


def die(msg: str) -> None:
    print(f"[ERROR] {msg}", file=sys.stderr, flush=True)
    sys.exit(1)


def run(args: list[str], **kw: Any) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, check=True, **kw)


def run_quiet(args: list[str], **kw: Any) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        args, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, **kw,
    )


def capture(args: list[str], **kw: Any) -> str:
    result = subprocess.run(args, capture_output=True, text=True, **kw)
    if result.returncode != 0:
        raise subprocess.CalledProcessError(result.returncode, args, result.stdout, result.stderr)
    return result.stdout.strip()


def ssh_cmd(*remote_args: str) -> subprocess.CompletedProcess[str]:
    return run(["ssh", *SSH_OPTS, SSH_TARGET, *remote_args])


def scp_cmd(src: str, dst: str) -> subprocess.CompletedProcess[str]:
    return run(["scp", *SSH_OPTS, src, dst])


def kubectl(args: list[str], **kw: Any) -> subprocess.CompletedProcess[str]:
    return run([KUBECTL, *args], **kw)


def kubectl_capture(args: list[str]) -> str:
    return capture([KUBECTL, *args])


def _b64(val: str) -> str:
    """Base64-encode a string (no newlines)."""
    return base64.b64encode(val.encode()).decode()


# ---------------------------------------------------------------------------
# create-vm
# ---------------------------------------------------------------------------
def create_vm() -> None:
    """Create bridge networking and launch a QEMU VM."""

    # Pre-flight
    for cmd in ("qemu-system-x86_64", "qemu-img", "genisoimage"):
        if shutil.which(cmd) is None:
            die(f"{cmd} is required but not found in PATH")
    if not os.access("/dev/kvm", os.R_OK):
        die("/dev/kvm is not accessible. Enable KVM for hardware acceleration.")

    VM_DIR.mkdir(parents=True, exist_ok=True)
    SSH_KEY_DIR.mkdir(parents=True, exist_ok=True)

    # Generate SSH key pair
    if not SSH_KEY.exists():
        log("Generating SSH key pair...")
        run(["ssh-keygen", "-t", "ed25519", "-f", str(SSH_KEY), "-N", "", "-q"])

    ssh_pub_key = SSH_KEY.with_suffix(".pub").read_text().strip()

    # Create bridge network
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

    # Download Ubuntu cloud image
    image_url = "https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img"
    image_file = VM_DIR / "ubuntu-cloud-amd64.img"
    if not image_file.exists():
        log("Downloading Ubuntu 24.04 cloud image...")
        run(["curl", "-fsSL", "-o", str(image_file), image_url])
    else:
        log(f"Using existing image: {image_file}")

    # Create VM disk
    vm_disk = VM_DIR / f"{VM_NAME}.qcow2"
    log(f"Creating snapshot disk: {vm_disk}")
    run(["qemu-img", "create", "-f", "qcow2", "-b", str(image_file),
         "-F", "qcow2", str(vm_disk), VM_DISK_SIZE])

    # cloud-init configuration
    log("Generating cloud-init configuration...")

    user_data = VM_DIR / "user-data"
    user_data.write_text(textwrap.dedent(f"""\
        #cloud-config
        users:
          - name: ubuntu
            sudo: ALL=(ALL) NOPASSWD:ALL
            shell: /bin/bash
            groups: [sudo]
            lock_passwd: false
            ssh_authorized_keys:
              - {ssh_pub_key}

        package_update: true
        package_upgrade: false
        packages:
          - curl
          - jq
          - apt-transport-https
          - ca-certificates
          - net-tools

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

        runcmd:
          - netplan apply
          - mkdir -p /etc/agent
          - |
            cat > /etc/agent/provisioned <<'MARKER'
            provisioned=true
            MARKER
          - 'echo "cloud-init: done"'
    """))

    meta_data = VM_DIR / "meta-data"
    meta_data.write_text(textwrap.dedent(f"""\
        instance-id: {VM_NAME}
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
    log(f"  Memory:       {VM_MEMORY} MB")
    log(f"  CPUs:         {VM_CPUS}")
    log(f"  Disk:         {vm_disk}")
    log(f"  IP:           {VM_IP}")
    log(f"  Bridge:       {BRIDGE_NAME}")
    log(f"  Log:          {qemu_log}")
    log("============================================")

    run([
        "qemu-system-x86_64",
        "-cpu", "host", "-accel", "kvm",
        "-m", VM_MEMORY, "-smp", VM_CPUS,
        "-drive", f"file={vm_disk},format=qcow2,if=virtio",
        "-drive", f"file={seed_iso},format=raw,if=virtio",
        "-netdev", f"tap,id=net0,ifname={TAP_NAME},script=no,downscript=no",
        "-device", "virtio-net-pci,netdev=net0",
        "-daemonize", "-pidfile", str(pid_file),
        "-serial", f"file:{qemu_log}",
        "-display", "none",
    ])

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
    log(f"VM is ready! SSH: ssh -i {SSH_KEY} ubuntu@{VM_IP}")


# ---------------------------------------------------------------------------
# run-agent
# ---------------------------------------------------------------------------
def run_agent() -> None:
    """Build agent, extract cluster info, run provision install script on VM."""

    if not SSH_KEY.exists():
        die(f"SSH key not found: {SSH_KEY}. Run create-vm first.")
    if not INSTALL_SCRIPT.exists():
        die(f"Provision install script not found: {INSTALL_SCRIPT}")
    for cmd in (KUBECTL, "jq"):
        if shutil.which(cmd) is None:
            die(f"{cmd} is required but not found in PATH")

    # Build agent binary and package as tarball
    log("Building unbounded-agent...")
    agent_bin = REPO_ROOT / "bin" / "unbounded-agent"
    run(["go", "build", "-o", str(agent_bin), str(REPO_ROOT / "cmd" / "agent" / "main.go")],
        env={**os.environ, "GOOS": "linux", "GOARCH": "amd64"})
    log(f"Agent binary built: {agent_bin}")

    log("Packaging agent binary as tarball...")
    agent_tarball = VM_DIR / "unbounded-agent-linux-amd64.tar.gz"
    run(["tar", "-czf", str(agent_tarball), "-C", str(REPO_ROOT / "bin"), "unbounded-agent"])
    log(f"Agent tarball: {agent_tarball}")

    # Serve the tarball over HTTP
    runner_ip = VM_GATEWAY
    agent_url = f"http://{runner_ip}:{SERVE_PORT}/unbounded-agent-linux-amd64.tar.gz"

    log(f"Starting HTTP file server on {runner_ip}:{SERVE_PORT}...")
    handler = _make_handler(str(VM_DIR))
    httpd = HTTPServer((runner_ip, SERVE_PORT), handler)
    server_thread = Thread(target=httpd.serve_forever, daemon=True)
    server_thread.start()
    log(f"Agent download URL: {agent_url}")

    try:
        _run_agent_inner(agent_url)
    finally:
        httpd.shutdown()

    log("Agent bootstrap completed")


def _make_handler(directory: str) -> type:
    """Create a SimpleHTTPRequestHandler bound to *directory*."""
    class Handler(SimpleHTTPRequestHandler):
        def __init__(self, *args: Any, **kwargs: Any) -> None:
            super().__init__(*args, directory=directory, **kwargs)
        def log_message(self, format: str, *args: Any) -> None:  # noqa: A002
            pass  # suppress request logs
    return Handler


def _run_agent_inner(agent_url: str) -> None:
    """Core logic for run-agent (after HTTP server is up)."""

    # Extract cluster information from Kind
    log(f"Extracting cluster info from Kind cluster '{KIND_CLUSTER_NAME}'...")
    kind_container = f"{KIND_CLUSTER_NAME}-control-plane"
    kind_ip = capture([
        "docker", "inspect", kind_container,
        "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
    ])
    if not kind_ip:
        die("Could not determine Kind control-plane container IP")

    api_server = f"https://{kind_ip}:6443"
    log(f"API server: {api_server}")

    ca_cert_b64 = kubectl_capture([
        "config", "view", "--raw",
        "-o", "jsonpath={.clusters[0].cluster.certificate-authority-data}",
    ])
    if not ca_cert_b64:
        die("Could not extract CA certificate from kubeconfig")

    version_json = capture([KUBECTL, "version", "-o", "json"])
    kube_version = json.loads(version_json)["serverVersion"]["gitVersion"]
    if not kube_version:
        die("Could not determine Kubernetes version")
    log(f"Kubernetes version: {kube_version}")

    # Cluster DNS
    cluster_dns = ""
    for svc in ("kube-dns", "coredns"):
        try:
            cluster_dns = kubectl_capture([
                "get", "svc", "-n", "kube-system", svc,
                "-o", "jsonpath={.spec.clusterIP}",
            ])
            if cluster_dns:
                break
        except subprocess.CalledProcessError:
            log(f"  DNS service '{svc}' not found, trying next...")
    if not cluster_dns:
        die("Could not determine cluster DNS IP")
    log(f"Cluster DNS: {cluster_dns}")

    # Create bootstrap token.
    # Kind clusters use kubeadm, which creates ClusterRoleBindings for TLS
    # bootstrap (CSR creation + auto-approval) scoped to the group
    # "system:bootstrappers:kubeadm:default-node-token".  Without
    # auth-extra-groups the token only belongs to the generic
    # "system:bootstrappers" group, and the kubelet's CSR is rejected with
    # "certificatesigningrequests is forbidden".
    log("Creating bootstrap token...")
    token_id = secrets.token_hex(3)
    token_secret = secrets.token_hex(8)
    bootstrap_token = f"{token_id}.{token_secret}"
    bootstrap_group = "system:bootstrappers:kubeadm:default-node-token"

    token_manifest = json.dumps({
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {
            "name": f"bootstrap-token-{token_id}",
            "namespace": "kube-system",
        },
        "type": "bootstrap.kubernetes.io/token",
        "data": {
            "token-id": _b64(token_id),
            "token-secret": _b64(token_secret),
            "usage-bootstrap-authentication": _b64("true"),
            "usage-bootstrap-signing": _b64("true"),
            "auth-extra-groups": _b64(bootstrap_group),
        },
    })
    kubectl(["apply", "-f", "-"], input=token_manifest.encode())
    log(f"Bootstrap token created: {token_id}.xxxxxxxxxxxxxxxx")

    # Generate agent configuration
    log("Generating agent configuration...")
    agent_config_path = VM_DIR / "agent-config.json"

    agent_config: dict[str, Any] = {
        "MachineName": AGENT_MACHINE_NAME,
        "Cluster": {
            "CaCertBase64": ca_cert_b64,
            "ClusterDNS": cluster_dns,
            "Version": kube_version,
        },
        "Kubelet": {
            "ApiServer": api_server,
            "BootstrapToken": bootstrap_token,
            "Labels": {},
            "RegisterWithTaints": [],
        },
    }

    agent_config_path.write_text(json.dumps(agent_config, indent=2))
    agent_config_path.chmod(0o600)  # restrict access; contains bootstrap token
    log(f"Agent config written to {agent_config_path}")

    # Copy install script and config to VM
    log("Copying provision install script and config to VM...")
    scp_cmd(str(INSTALL_SCRIPT), f"{SSH_TARGET}:/tmp/unbounded-agent-install.sh")
    scp_cmd(str(agent_config_path), f"{SSH_TARGET}:/tmp/agent-config.json")
    ssh_cmd("chmod +x /tmp/unbounded-agent-install.sh")

    # Wait for cloud-init and verify connectivity
    log("Waiting for cloud-init to complete on VM...")
    subprocess.run(["ssh", *SSH_OPTS, SSH_TARGET, "sudo cloud-init status --wait"],
                    check=False)

    log("Verifying VM can reach agent download URL...")
    ssh_cmd(f"curl -fsSL --connect-timeout 10 -o /dev/null {agent_url}")

    log("Verifying VM can reach Kind API server...")
    ssh_cmd(f"curl -fsSk --connect-timeout 10 {api_server}/healthz")

    # Run the provision install script on the VM
    log("Running provision install script on VM...")
    log("This will download the agent, bootstrap the node, and join it to the Kind cluster.")
    env_prefix = (
        f"UNBOUNDED_AGENT_CONFIG_FILE=/tmp/agent-config.json "
        f"AGENT_URL={agent_url} "
        f"AGENT_DEBUG={AGENT_DEBUG}"
    )
    run([
        "timeout", "1200",
        "ssh", *SSH_OPTS, "-o", "ServerAliveInterval=30", SSH_TARGET,
        f"sudo {env_prefix} /tmp/unbounded-agent-install.sh",
    ])


# ---------------------------------------------------------------------------
# wait-for-node
# ---------------------------------------------------------------------------
def wait_for_node() -> None:
    """Wait for the agent node to appear and become Ready."""

    node_timeout = int(os.environ.get("NODE_TIMEOUT", "180"))
    ready_timeout = int(os.environ.get("READY_TIMEOUT", "120"))

    # Wait for node to appear
    log(f"Waiting for node '{AGENT_MACHINE_NAME}' to appear (timeout: {node_timeout}s)...")
    elapsed = 0
    while elapsed < node_timeout:
        ret = subprocess.run(
            [KUBECTL, "get", "node", AGENT_MACHINE_NAME, "-o", "name"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        if ret.returncode == 0:
            log(f"Node '{AGENT_MACHINE_NAME}' appeared after {elapsed}s")
            break
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) Node not yet visible...")
        time.sleep(5)
        elapsed += 5
    else:
        log("Current nodes:")
        subprocess.run([KUBECTL, "get", "nodes", "-o", "wide"], check=False)
        die(f"Timed out waiting for node '{AGENT_MACHINE_NAME}' after {node_timeout}s")

    # Wait for Ready
    log(f"Waiting for node '{AGENT_MACHINE_NAME}' to become Ready (timeout: {ready_timeout}s)...")
    elapsed = 0
    while elapsed < ready_timeout:
        result = subprocess.run(
            [KUBECTL, "get", "node", AGENT_MACHINE_NAME,
             "-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}"],
            capture_output=True, text=True,
        )
        status = result.stdout.strip() if result.returncode == 0 else "unknown"
        if status == "True":
            log(f"Node '{AGENT_MACHINE_NAME}' is Ready after {elapsed}s")
            break
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) Node not yet Ready (status: {status})")
        time.sleep(5)
        elapsed += 5
    else:
        log("Node status:")
        subprocess.run([KUBECTL, "describe", "node", AGENT_MACHINE_NAME], check=False)
        die(f"Timed out waiting for node '{AGENT_MACHINE_NAME}' to become Ready after {ready_timeout}s")

    log("============================================")
    log("  Node join PASSED")
    log("============================================")
    kubectl(["get", "nodes", "-o", "wide"])


# ---------------------------------------------------------------------------
# validate-workload
# ---------------------------------------------------------------------------
def validate_workload() -> None:
    """Deploy test pods on the agent node and verify they run."""

    timeout_secs = 300

    # Create test namespace (idempotent)
    log(f"Creating test namespace '{TEST_NS}'...")
    ns_yaml = capture([KUBECTL, "create", "namespace", TEST_NS,
                       "--dry-run=client", "-o", "yaml"])
    kubectl(["apply", "-f", "-"], input=ns_yaml.encode())

    # Clean up any stale pods from a previous run (e.g. after reset + rejoin)
    for pod_name in ("e2e-hello", "e2e-dns-test"):
        run_quiet([KUBECTL, "delete", "pod", pod_name, "-n", TEST_NS,
                   "--ignore-not-found"], check=False)

    # Deploy hello pod
    log(f"Deploying test pod on node '{AGENT_MACHINE_NAME}'...")
    hello_pod = {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": "e2e-hello", "namespace": TEST_NS, "labels": {"app": "e2e-hello"}},
        "spec": {
            "nodeName": AGENT_MACHINE_NAME,
            "containers": [{
                "name": "hello",
                "image": "busybox:1.36",
                "command": ["sh", "-c", "echo 'Hello from unbounded agent node!' && sleep 3600"],
            }],
            "restartPolicy": "Never",
            "tolerations": [{"operator": "Exists"}],
        },
    }
    kubectl(["apply", "-f", "-"], input=json.dumps(hello_pod).encode())

    # Wait for Running
    log("Waiting for pod 'e2e-hello' to be Running...")
    elapsed = 0
    while elapsed < timeout_secs:
        result = subprocess.run(
            [KUBECTL, "get", "pod", "e2e-hello", "-n", TEST_NS,
             "-o", "jsonpath={.status.phase}"],
            capture_output=True, text=True,
        )
        phase = result.stdout.strip() if result.returncode == 0 else ""
        if phase == "Running":
            log(f"Pod 'e2e-hello' is Running after {elapsed}s")
            break
        if phase in ("Failed", "Unknown"):
            subprocess.run([KUBECTL, "describe", "pod", "e2e-hello", "-n", TEST_NS], check=False)
            die(f"Pod 'e2e-hello' entered {phase} state")
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"  ({elapsed}s) Pod phase: {phase or 'Pending'}")
        time.sleep(5)
        elapsed += 5
    else:
        subprocess.run([KUBECTL, "describe", "pod", "e2e-hello", "-n", TEST_NS], check=False)
        die(f"Timed out waiting for pod 'e2e-hello' to be Running after {timeout_secs}s")

    # Check logs
    log("Checking pod logs...")
    logs = capture([KUBECTL, "logs", "e2e-hello", "-n", TEST_NS])
    print(logs, flush=True)
    if "Hello from unbounded agent node!" not in logs:
        die("Pod logs do not contain expected message")
    log("Pod logs contain expected message")

    # Verify node placement
    pod_node = kubectl_capture(["get", "pod", "e2e-hello", "-n", TEST_NS,
                                 "-o", "jsonpath={.spec.nodeName}"])
    if pod_node != AGENT_MACHINE_NAME:
        die(f"Pod is running on '{pod_node}' instead of '{AGENT_MACHINE_NAME}'")
    log(f"Pod is running on the correct node: {pod_node}")

    # DNS test (non-fatal)
    log("Deploying DNS test pod on agent node...")
    dns_pod = {
        "apiVersion": "v1",
        "kind": "Pod",
        "metadata": {"name": "e2e-dns-test", "namespace": TEST_NS},
        "spec": {
            "nodeName": AGENT_MACHINE_NAME,
            "containers": [{
                "name": "dns",
                "image": "busybox:1.36",
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
            [KUBECTL, "get", "pod", "e2e-dns-test", "-n", TEST_NS,
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
        [KUBECTL, "logs", "e2e-dns-test", "-n", TEST_NS],
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
    """Run unbounded-agent reset on the VM and verify the node is removed."""

    if not SSH_KEY.exists():
        die(f"SSH key not found: {SSH_KEY}. Run create-vm first.")

    log(f"Running 'unbounded-agent reset' on VM for machine '{AGENT_MACHINE_NAME}'...")
    run([
        "timeout", "300",
        "ssh", *SSH_OPTS, "-o", "ServerAliveInterval=30", SSH_TARGET,
        f"sudo UNBOUNDED_AGENT_CONFIG_FILE=/tmp/agent-config.json unbounded-agent reset",
    ])
    log("Agent reset completed on VM")

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

    # Verify the nspawn machine is no longer running on the VM
    log("Verifying nspawn machine is stopped on VM...")
    result = subprocess.run(
        ["ssh", *SSH_OPTS, SSH_TARGET,
         f"sudo machinectl show {AGENT_MACHINE_NAME}"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    if result.returncode == 0:
        die(f"nspawn machine '{AGENT_MACHINE_NAME}' is still running after reset")
    log(f"nspawn machine '{AGENT_MACHINE_NAME}' is not running")

    log("============================================")
    log("  Agent reset PASSED")
    log("============================================")


# ---------------------------------------------------------------------------
# cleanup
# ---------------------------------------------------------------------------
def cleanup() -> None:
    """Tear down VM, networking, and Kind cluster."""

    # Stop QEMU VM
    pid_file = VM_DIR / f"{VM_NAME}.pid"
    if pid_file.exists():
        pid = int(pid_file.read_text().strip())
        try:
            os.kill(pid, 0)
            log(f"Stopping VM '{VM_NAME}' (PID: {pid})...")
            os.kill(pid, 15)
            time.sleep(2)
            try:
                os.kill(pid, 0)
                log("Force killing VM...")
                os.kill(pid, 9)
            except OSError:
                # Process already exited after SIGTERM; nothing to force kill.
                pass
        except OSError:
            # Process already gone or cannot be signaled; safe to ignore during cleanup.
            pass
        pid_file.unlink(missing_ok=True)

    # Remove networking
    log("Cleaning up networking...")
    run_quiet(["sudo", "ip", "link", "del", TAP_NAME], check=False)
    run_quiet(["sudo", "ip", "link", "del", BRIDGE_NAME], check=False)

    # Remove iptables rules (best-effort)
    for rule in [
        ["sudo", "iptables", "-D", "FORWARD", "-i", BRIDGE_NAME, "-j", "ACCEPT"],
        ["sudo", "iptables", "-D", "FORWARD", "-o", BRIDGE_NAME, "-j", "ACCEPT"],
        ["sudo", "iptables", "-t", "raw", "-D", "PREROUTING", "-i", BRIDGE_NAME, "-j", "ACCEPT"],
        ["sudo", "iptables", "-t", "nat", "-D", "POSTROUTING",
         "-s", f"{VM_SUBNET}.0/24", "!", "-d", f"{VM_SUBNET}.0/24", "-j", "MASQUERADE"],
    ]:
        run_quiet(rule, check=False)

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
COMMANDS = {
    "create-vm": create_vm,
    "run-agent": run_agent,
    "wait-for-node": wait_for_node,
    "validate-workload": validate_workload,
    "reset-agent": reset_agent,
    "cleanup": cleanup,
}


def main() -> None:
    if len(sys.argv) != 2 or sys.argv[1] not in COMMANDS:
        cmds = ", ".join(COMMANDS)
        die(f"Usage: {sys.argv[0]} <{cmds}>")

    COMMANDS[sys.argv[1]]()


if __name__ == "__main__":
    main()
