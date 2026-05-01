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

Subcommands (called as individual workflow steps):
    create-vm                          Create bridge networking and launch a QEMU VM.
    ensure-kind-bridge                 Verify/repair veth pair connecting Kind to VM bridge.
    run-agent                          Build agent, generate bootstrap script, run on VM.
    wait-for-node                      Wait for the node to appear and become Ready.
    validate-workload                  Deploy test pods on the agent node.
    validate-kube-proxy                Verify kube-proxy is Running on all nodes.
    install-machine-crd                Install Machine CRD and bootstrapper RBAC.
    delete-machine-cr                  Delete the Machine CR.
    validate-machine-cr-created        Verify agent self-registered a Machine CR.
    reset-agent                        Run agent reset and verify cleanup.
    cleanup                            Tear down VM, networking, and Kind cluster.
"""

from __future__ import annotations

import argparse
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
KIND_CONTAINER = f"{KIND_CLUSTER_NAME}-control-plane"
AGENT_MACHINE_NAME = os.environ.get("AGENT_MACHINE_NAME", "agent-e2e")
AGENT_DEBUG = os.environ.get("AGENT_DEBUG", "")

# Site name used when generating the bootstrap script via kubectl-unbounded.
E2E_SITE_NAME = "e2e"

# Fixed nspawn machine names used by unbounded-agent (decoupled from the kube node name).
NSPAWN_MACHINE_NAMES = ["kube1", "kube2"]

BRIDGE_NAME = "virbr-e2e"
TAP_NAME = "tap-e2e"
SERVE_PORT = 8199

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
SSH_TARGET = f"ubuntu@{VM_IP}"

KUBECTL = "kubectl"
KUBECTL_UNBOUNDED = str(REPO_ROOT / "bin" / "kubectl-unbounded")

TEST_NS = "e2e-workload-test"


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
# create-vm / recreate-vm helpers
# ---------------------------------------------------------------------------
def _stop_qemu() -> None:
    """Stop the QEMU VM process if it is running."""
    pid_file = VM_DIR / f"{VM_NAME}.pid"
    if not pid_file.exists():
        return

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
            pass  # already exited after SIGTERM
    except OSError:
        pass  # already gone
    pid_file.unlink(missing_ok=True)


def _launch_vm(ssh_pub_key: str) -> None:
    """Create a fresh VM disk, cloud-init ISO, launch QEMU, and wait for SSH.

    Assumes VM_DIR, the base cloud image, and the SSH key pair already exist.
    Networking (bridge, TAP, NAT) must already be configured.
    """

    image_file = VM_DIR / "ubuntu-cloud-amd64.img"
    if not image_file.exists():
        die(f"Base cloud image not found: {image_file}. Run create-vm first.")

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

    # Prevent NetworkManager from detaching interfaces from the bridge.
    _nm_unmanage(BRIDGE_NAME)
    _nm_unmanage(TAP_NAME)

    # Download Ubuntu cloud image
    image_url = "https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-amd64.img"
    image_file = VM_DIR / "ubuntu-cloud-amd64.img"
    if not image_file.exists():
        log("Downloading Ubuntu 24.04 cloud image...")
        run(["curl", "-fsSL", "-o", str(image_file), image_url])
    else:
        log(f"Using existing image: {image_file}")

    _launch_vm(ssh_pub_key)


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
# run-agent
# ---------------------------------------------------------------------------
def run_agent() -> None:
    """Build agent, generate bootstrap script, and run it on the VM."""

    if not SSH_KEY.exists():
        die(f"SSH key not found: {SSH_KEY}. Run create-vm first.")
    for cmd in (KUBECTL,):
        if shutil.which(cmd) is None:
            die(f"{cmd} is required but not found in PATH")

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

    # Determine the Kind control-plane IP so connectivity checks have the
    # correct address even when the local kubeconfig uses 127.0.0.1.
    log(f"Resolving Kind control-plane IP for '{KIND_CLUSTER_NAME}'...")
    kind_ip = capture([
        "docker", "inspect", KIND_CONTAINER,
        "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}",
    ])
    if not kind_ip:
        die("Could not determine Kind control-plane container IP")
    api_server = f"https://{kind_ip}:6443"
    log(f"API server: {api_server}")

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
    (VM_DIR / "token-id").write_text(token_id)
    bootstrap_group = "system:bootstrappers:kubeadm:default-node-token"

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
            "usage-bootstrap-authentication": _b64("true"),
            "usage-bootstrap-signing": _b64("true"),
            "auth-extra-groups": _b64(bootstrap_group),
        },
    })
    kubectl(["apply", "-f", "-"], input=token_manifest.encode())
    log(f"Bootstrap token created: {token_id}.xxxxxxxxxxxxxxxx")

    # Generate bootstrap script via kubectl-unbounded.
    # manual-bootstrap auto-detects the API server, CA cert, Kubernetes
    # version, and cluster DNS from the active kubeconfig. The bootstrap
    # token is resolved via the site label on the secret.
    log("Generating bootstrap script with kubectl-unbounded machine manual-bootstrap...")

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

    bootstrap_script = capture([
        KUBECTL_UNBOUNDED, "machine", "manual-bootstrap",
        AGENT_MACHINE_NAME,
        "--site", E2E_SITE_NAME,
    ])

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
    log("Bootstrap script contents:")
    print(bootstrap_script, flush=True)

    # Wait for cloud-init and verify connectivity
    log("Waiting for cloud-init to complete on VM...")
    subprocess.run(["ssh", *SSH_OPTS, SSH_TARGET, "sudo cloud-init status --wait"],
                    check=False)

    log("Verifying VM can reach agent download URL...")
    ssh_cmd(f"curl -fsSL --connect-timeout 10 -o /dev/null {agent_url}")

    log("Verifying VM can reach Kind API server...")
    ssh_cmd(f"curl -fsSk --connect-timeout 10 {api_server}/healthz")

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
            [KUBECTL, "logs", "e2e-hello", "-n", TEST_NS],
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
            subprocess.run([KUBECTL, "describe", "pod", "e2e-hello", "-n", TEST_NS], check=False)
            die(f"kubectl logs failed after {log_attempts} attempts")

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

    log("Running 'unbounded-agent reset' on VM...")
    run([
        "timeout", "300",
        "ssh", *SSH_OPTS, "-o", "ServerAliveInterval=30", SSH_TARGET,
        "sudo unbounded-agent reset",
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
    """Install the Machine CRD and bootstrapper RBAC."""

    crd_path = REPO_ROOT / "deploy" / "machina" / "crd" / "unbounded-cloud.io_machines.yaml"
    rbac_path = REPO_ROOT / "deploy" / "machina" / "rendered" / "07-bootstrapper-rbac.yaml"

    if not crd_path.exists():
        die(f"Machine CRD not found: {crd_path}")

    log("Rendering machina manifests...")
    run(["make", "machina-manifests"], cwd=str(REPO_ROOT))

    if not rbac_path.exists():
        die(f"Bootstrapper RBAC not found after render: {rbac_path}")

    log("Installing Machine CRD...")
    kubectl(["apply", "-f", str(crd_path)])

    log("Installing bootstrapper RBAC...")
    kubectl(["apply", "-f", str(rbac_path)])

    log("Machine CRD and RBAC installed")


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
def validate_machine_cr_created() -> None:
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

    log(f"bootstrapTokenRef is correct: {token_ref}")

    log("============================================")
    log("  Machine CR validation PASSED (created)")
    log("============================================")


# ---------------------------------------------------------------------------
# cleanup
# ---------------------------------------------------------------------------
def cleanup() -> None:
    """Tear down VM, networking, and Kind cluster."""

    # Stop QEMU VM
    _stop_qemu()

    # Remove networking
    log("Cleaning up networking...")
    run_quiet(["sudo", "ip", "link", "del", TAP_NAME], check=False)
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
COMMANDS = {
    "create-vm": create_vm,
    "ensure-kind-bridge": ensure_kind_bridge,
    "run-agent": run_agent,
    "wait-for-node": wait_for_node,
    "validate-kube-proxy": validate_kube_proxy,
    "validate-workload": validate_workload,
    "install-machine-crd": install_machine_crd,
    "delete-machine-cr": delete_machine_cr,
    "validate-machine-cr-created": validate_machine_cr_created,
    "reset-agent": reset_agent,
    "cleanup": cleanup,
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
    args = parser.parse_args()
    VERBOSE = args.verbose

    COMMANDS[args.command]()


if __name__ == "__main__":
    main()
