#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import atexit
import json
import os
import signal
import shutil
import socket
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parent.parent
TMPDIR = Path(tempfile.mkdtemp())
os.chmod(TMPDIR, 0o755)
SITE = "smoke"
NODE_NAME = "smoke-node"
NODE_NS = "default"
API_GROUP = "unbounded-kube.io"
API_VERSION = f"{API_GROUP}/v1alpha3"
VM_NAME = "unbounded-metal-smoke"
NET_NAME = "unbounded-metal-smoke"
SUBNET = "192.168.200"
SERVER_IP = f"{SUBNET}.1"
NODE_IP = f"{SUBNET}.10"
GATEWAY = SERVER_IP
DNS_SERVER = "8.8.8.8"
MAC_ADDRESS = "52:54:00:aa:bb:01"
SUSHY_PORT = 8443
HTTP_PORT = 8880
CACHE_DIR = TMPDIR / "cache"
ARTIFACT_DIR = TMPDIR / "artifacts"
SERVE_URL = f"http://{SERVER_IP}:{HTTP_PORT}"
REGISTRY_PORT = 5555
REGISTRY_CONTAINER = "unbounded-smoke-registry"
IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/host-ubuntu2404:smoke"
AGENT_IMAGE_DIR = REPO_ROOT / "images" / "agent-ubuntu2404"
AGENT_IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/agent-ubuntu2404:smoke"
# The agent runs inside a VM on an isolated libvirt network. "localhost" inside
# the VM resolves to the VM's own loopback, not the host.  Use the host's
# bridge IP so the VM can reach the registry over the virtual network.
AGENT_IMAGE_NAME_VM = f"{SERVER_IP}:{REGISTRY_PORT}/unbounded/agent-ubuntu2404:smoke"
BINARY = REPO_ROOT / "bin" / "metalman"
KUBECTL_UNBOUNDED = REPO_ROOT / "bin" / "kubectl-unbounded"
SERIAL_SOCK = TMPDIR / "console.sock"

KUBECTL = "kubectl"
VIRSH = ["virsh", "--connect", "qemu:///system"]
DEVNULL = subprocess.DEVNULL

IMAGE_DIR = REPO_ROOT / "images" / "host-ubuntu2404"

_procs: list[subprocess.Popen[Any]] = []


def log(msg: str) -> None:
    print(f"==> {msg}", file=sys.stderr)


def die(msg: str) -> None:
    print(f"FAIL: {msg}", file=sys.stderr)
    sys.exit(1)


def run(args: list[str], **kw: Any) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, check=True, **kw)


def run_quiet(args: list[str], **kw: Any) -> subprocess.CompletedProcess[str]:
    return subprocess.run(args, stdout=DEVNULL, stderr=DEVNULL, **kw)


def _forward_lines(stream: Any, log_file: Any) -> None:
    """Read lines from *stream*, write to both *log_file* and stderr."""
    for line in stream:
        log_file.write(line)
        log_file.flush()
        sys.stderr.write(line)
        sys.stderr.flush()


def spawn(args: list[str], log_path: Path | str) -> subprocess.Popen[Any]:
    """Start a background process, teeing its output to *log_path* and stderr."""
    log_file = open(log_path, "w")  # noqa: SIM115 — intentionally long-lived
    proc = subprocess.Popen(
        args, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
        start_new_session=True,
    )
    threading.Thread(
        target=_forward_lines,
        args=(proc.stdout, log_file),
        daemon=True,
    ).start()
    _procs.append(proc)
    return proc


def check_procs() -> None:
    """Die if any spawned background process has exited non-zero."""
    for proc in _procs:
        ret = proc.poll()
        if ret is not None and ret != 0:
            die(f"Background process {proc.args} exited with code {ret}")


def forward_console(sock_path: Path) -> None:
    """Connect to the VM serial console and copy output to stderr.

    Runs in a daemon thread.  Re-connects whenever the socket disappears
    (the VM may be powered off and back on during the test).
    """
    while True:
        # Wait for the socket to appear.
        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            conn.connect(str(sock_path))
        except (FileNotFoundError, ConnectionRefusedError, OSError):
            conn.close()
            time.sleep(1)
            continue

        try:
            while True:
                data = conn.recv(4096)
                if not data:
                    break
                sys.stderr.buffer.write(data)
                sys.stderr.buffer.flush()
        except OSError:
            pass
        finally:
            conn.close()

        # Socket closed — VM probably rebooted.  Retry.
        time.sleep(1)


def clean_libvirt() -> None:
    for cmd in [
        [*VIRSH, "destroy", VM_NAME],
        [*VIRSH, "undefine", VM_NAME, "--nvram"],
        [*VIRSH, "net-destroy", NET_NAME],
        [*VIRSH, "net-undefine", NET_NAME],
    ]:
        run_quiet(cmd)
    # Remove stale bridge left behind by a previous net-destroy.
    run_quiet(["sudo", "ip", "link", "delete", "virbr-smoke"])
    # Kill any leftover sushy-emulator from a previous run.
    run_quiet(["sudo", "pkill", "-f", "sushy-emulator"])
    # Kill any leftover metalman serve-pxe from a previous run.
    run_quiet(["sudo", "pkill", "-f", "metalman"])
    # Stop and remove leftover local registry container.
    run_quiet(["docker", "rm", "-f", REGISTRY_CONTAINER])
    # Delete stale leader-election leases so new processes acquire immediately.
    run_quiet([KUBECTL, "-n", "unbounded-kube", "delete", "lease",
               f"metalman-{SITE}"])
    time.sleep(1)


_cleaning_up = False


def cleanup() -> None:
    global _cleaning_up
    if _cleaning_up:
        return
    _cleaning_up = True
    log("Cleaning up...")
    for proc in _procs:
        try:
            os.killpg(proc.pid, signal.SIGTERM)
        except OSError:
            pass
    for proc in _procs:
        try:
            proc.wait(timeout=5)
        except (OSError, subprocess.TimeoutExpired):
            try:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait(timeout=5)
            except (OSError, subprocess.TimeoutExpired):
                pass
    clean_libvirt()
    # Remove iptables rules that were added for VM ↔ kind connectivity.
    # Use check=False so these are best-effort (rules may not exist if setup
    # failed before they were inserted).
    run_quiet(["sudo", "iptables", "-D", "FORWARD", "-i", "virbr-smoke", "-j", "ACCEPT"], check=False)
    run_quiet(["sudo", "iptables", "-D", "FORWARD", "-o", "virbr-smoke", "-j", "ACCEPT"], check=False)
    run_quiet(["sudo", "iptables", "-t", "raw", "-D", "PREROUTING",
               "-i", "virbr-smoke", "-j", "ACCEPT"], check=False)
    shutil.rmtree(TMPDIR, ignore_errors=True)


def _sigint_handler(sig: int, frame: Any) -> None:
    cleanup()
    sys.exit(1)


def kubectl(args: list[str], **kw: Any) -> subprocess.CompletedProcess[str]:
    return run([KUBECTL, *args], **kw)


def apiserver_url() -> str:
    result = run(
        [
            KUBECTL, "config", "view", "--minify",
            "-o", "jsonpath={.clusters[0].cluster.server}",
        ],
        capture_output=True,
        text=True,
    )
    url = result.stdout.strip()

    # When running against a kind cluster the kubeconfig points at
    # 127.0.0.1:<nodeport> which is unreachable from the VM.  Detect this
    # and rewrite to the kind container's internal IP on port 6443.
    from urllib.parse import urlparse
    parsed = urlparse(url)
    if parsed.hostname in ("127.0.0.1", "localhost", "::1"):
        try:
            ip = run(
                ["docker", "inspect", "kind-control-plane",
                 "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"],
                capture_output=True, text=True,
            ).stdout.strip()
            if ip:
                url = f"{parsed.scheme}://{ip}:6443"
                log(f"  Rewrote apiserver URL to {url} (kind container IP)")
        except subprocess.CalledProcessError:
            pass

    return url


def machine_status() -> str | None:
    """Return a short summary of Machine conditions, or None."""
    result = subprocess.run(
        [KUBECTL, "get", f"machines.{API_GROUP}", NODE_NAME,
         "-o", "jsonpath={.status.conditions[*].type}"],
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        return None
    return result.stdout.strip() or None


def wait_k8s_node(name: str, timeout: int = 1800) -> None:
    log(f"  Waiting for Kubernetes Node '{name}' to appear...")
    last_status: str | None = None
    for elapsed in range(timeout):
        check_procs()
        result = subprocess.run(
            [KUBECTL, "get", "node", name, "-o", "json"],
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            if elapsed > 0 and elapsed % 30 == 0:
                status = machine_status()
                if status != last_status:
                    last_status = status
                log(f"    ({elapsed}s) Machine conditions: {status or 'none'}")
            time.sleep(1)
            continue
        log(f"  Node '{name}' appeared in cluster")
        return
    die(f"Timed out waiting for Node '{name}'")


def assert_node_ready(name: str, timeout: int = 300) -> None:
    """Assert the Node reaches Ready status within timeout seconds."""
    log(f"  Waiting for Node '{name}' to become Ready...")
    for elapsed in range(timeout):
        check_procs()
        result = subprocess.run(
            [KUBECTL, "get", "node", name, "-o",
             "jsonpath={.status.conditions[?(@.type=='Ready')].status}"],
            capture_output=True, text=True,
        )
        if result.returncode == 0 and result.stdout.strip() == "True":
            log(f"  Node '{name}' is Ready")
            return
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"    ({elapsed}s) Node not yet Ready")
        time.sleep(1)
    die(f"Timed out waiting for Node '{name}' to become Ready")


def main() -> None:
    signal.signal(signal.SIGINT, _sigint_handler)
    atexit.register(cleanup)

    log("Cleaning up stale libvirt resources")
    clean_libvirt()

    log("Creating libvirt network")
    net_xml = TMPDIR / "net.xml"
    net_xml.write_text(textwrap.dedent(f"""\
        <network>
          <name>{NET_NAME}</name>
          <forward mode="nat"/>
          <bridge name="virbr-smoke"/>
          <ip address="{SERVER_IP}" netmask="255.255.255.0"/>
        </network>
    """))
    run([*VIRSH, "net-define", str(net_xml)])
    run([*VIRSH, "net-start", NET_NAME])

    # Allow the VM to reach the kind Docker network (Docker's bridge
    # isolation rules block cross-bridge traffic by default).
    log("Adding iptables rules for VM ↔ kind connectivity")
    run(["sudo", "iptables", "-I", "FORWARD", "-i", "virbr-smoke", "-j", "ACCEPT"])
    run(["sudo", "iptables", "-I", "FORWARD", "-o", "virbr-smoke", "-j", "ACCEPT"])
    # Docker may insert a raw PREROUTING DROP rule that blocks non-Docker
    # traffic to its container IPs.  Insert an ACCEPT before it so the VM
    # can reach the kind API server.
    run(["sudo", "iptables", "-t", "raw", "-I", "PREROUTING",
         "-i", "virbr-smoke", "-j", "ACCEPT"])
    # Add a route inside the kind container so it can reach the VM subnet.
    # Without this, kindnet on the control-plane can't add routes for the
    # smoke-node's pod CIDR and crash-loops.
    log("Adding route inside kind container for VM subnet")
    kind_ip = run(
        ["docker", "inspect", "kind-control-plane",
         "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"],
        capture_output=True, text=True,
    ).stdout.strip()
    run(["docker", "exec", "kind-control-plane",
         "ip", "route", "replace", f"{SUBNET}.0/24", "via", "172.18.0.1"])

    # Kindnet's CONTROL_PLANE_ENDPOINT defaults to "kind-control-plane:6443"
    # which is unresolvable from the bare-metal VM (it's not in Docker's DNS).
    # Patch it to use the kind container's actual IP.
    log("Patching kindnet DaemonSet for VM-reachable control plane endpoint")
    patch = json.dumps({
        "spec": {"template": {"spec": {"containers": [{
            "name": "kindnet-cni",
            "env": [
                {"name": "CONTROL_PLANE_ENDPOINT", "value": f"{kind_ip}:6443"},
            ],
        }]}}}
    })
    kubectl(["-n", "kube-system", "patch", "daemonset", "kindnet",
             "--type=strategic", "-p", patch])

    log("Creating UEFI VM (powered off, with TPM)")
    ovmf_vars = TMPDIR / "OVMF_VARS.fd"
    shutil.copy2("/usr/share/OVMF/OVMF_VARS_4M.fd", ovmf_vars)
    disk = str(TMPDIR / "disk.qcow2")
    run_quiet(["qemu-img", "create", "-f", "qcow2", disk, "20G"], check=True)
    run_quiet([
        "virt-install",
        "--connect", "qemu:///system",
        "--name", VM_NAME, "--ram", "4096", "--vcpus", "2",
        "--disk", f"path={disk},format=qcow2,bus=virtio",
        "--network", f"network={NET_NAME},mac={MAC_ADDRESS}",
        "--boot", f"uefi,loader=/usr/share/OVMF/OVMF_CODE_4M.fd,nvram={ovmf_vars},hd,network",
        "--tpm", "backend.type=emulator,backend.version=2.0",
        "--serial", f"unix,path={SERIAL_SOCK},mode=bind",
        "--os-variant", "generic",
        "--noautoconsole", "--noreboot", "--import",
    ], check=True)
    run_quiet([*VIRSH, "destroy", VM_NAME])

    log("Starting serial console forwarding")
    console_thread = threading.Thread(
        target=forward_console, args=(SERIAL_SOCK,), daemon=True,
    )
    console_thread.start()

    log("Starting sushy-emulator")
    run_quiet([
        "openssl", "req", "-x509", "-newkey", "rsa:2048",
        "-keyout", str(TMPDIR / "sushy.key"),
        "-out", str(TMPDIR / "sushy.crt"),
        "-days", "1", "-nodes",
        "-subj", "/CN=sushy-emulator",
        "-addext", "subjectAltName=IP:127.0.0.1",
    ], check=True)
    sushy_url = f"https://127.0.0.1:{SUSHY_PORT}"
    proc = spawn([
        "sushy-emulator", "--libvirt-uri", "qemu:///system",
        "-i", "127.0.0.1", "-p", str(SUSHY_PORT),
        "--ssl-certificate", str(TMPDIR / "sushy.crt"),
        "--ssl-key", str(TMPDIR / "sushy.key"),
    ], TMPDIR / "sushy.log")
    log(f"  sushy-emulator PID={proc.pid}")
    time.sleep(2)
    check_procs()

    log("Building metalman and kubectl-unbounded")
    run(["go", "build", "-o", str(BINARY), "./cmd/metalman"], cwd=str(REPO_ROOT))
    run(["go", "build", "-o", str(KUBECTL_UNBOUNDED), "./cmd/kubectl-unbounded"], cwd=str(REPO_ROOT))

    log("Cleaning up stale Kubernetes resources")
    run_quiet([KUBECTL, "-n", NODE_NS, "delete", "secret", "bmc-pass"])
    run_quiet([KUBECTL, "delete", f"machines.{API_GROUP}", NODE_NAME])
    run_quiet([KUBECTL, "delete", "node", NODE_NAME])
    # Remove stale CRDs so that a version change (e.g. storedVersions
    # referencing an old API version) does not block the fresh apply.
    run_quiet([KUBECTL, "delete", "crd", f"machines.{API_GROUP}"])

    log("Applying deploy manifests (CRDs, namespace, RBAC)")
    kubectl(["apply", "--server-side", "--force-conflicts", "-f", str(REPO_ROOT / "deploy" / "machina" / "01-namespace.yaml")])
    kubectl(["apply", "--server-side", "--force-conflicts", "-f", str(REPO_ROOT / "deploy" / "machina" / "crd")])
    kubectl(["apply", "--server-side", "--force-conflicts", "-f", str(REPO_ROOT / "deploy" / "metalman" / "01-rbac.yaml")])

    log("Creating Kubernetes resources")
    kubectl(["-n", NODE_NS, "create", "secret", "generic",
             "bmc-pass", "--from-literal=password="])

    log("Starting local OCI registry")
    run_quiet(["docker", "rm", "-f", REGISTRY_CONTAINER])
    run(["docker", "run", "-d", "--name", REGISTRY_CONTAINER,
         "-p", f"{REGISTRY_PORT}:5000", "registry:2"])
    # Wait for the registry to be ready.
    for _ in range(30):
        try:
            import urllib.request
            urllib.request.urlopen(f"http://localhost:{REGISTRY_PORT}/v2/")
            break
        except Exception:
            time.sleep(0.5)
    else:
        die("Local OCI registry did not become ready")

    log("Building host-ubuntu2404 OCI image")
    run(["docker", "build", "-t", IMAGE_NAME,
         "-f", str(IMAGE_DIR / "Containerfile"), str(REPO_ROOT)])

    log("Pushing host-ubuntu2404 OCI image to local registry")
    run(["docker", "push", IMAGE_NAME])

    log("Building agent-ubuntu2404 OCI image")
    run(["docker", "build", "-t", AGENT_IMAGE_NAME,
         "-f", str(AGENT_IMAGE_DIR / "Containerfile"), str(AGENT_IMAGE_DIR)])

    log("Pushing agent-ubuntu2404 OCI image to local registry")
    run(["docker", "push", AGENT_IMAGE_NAME])

    server_url = apiserver_url()
    log(f"  API server URL: {server_url}")
    protonode = {
        "apiVersion": API_VERSION,
        "kind": "Machine",
        "metadata": {
            "name": NODE_NAME,
            "labels": {f"{API_GROUP}/site": SITE},
        },
        "spec": {
            "pxe": {
                "image": IMAGE_NAME,
                "redfish": {
                    "url": sushy_url,
                    "username": "",
                    "deviceID": VM_NAME,
				"passwordRef": {"name": "bmc-pass", "key": "password", "namespace": NODE_NS},
                },
                "dhcpLeases": [{
                    "mac": MAC_ADDRESS,
                    "ipv4": NODE_IP,
                    "subnetMask": "255.255.255.0",
                    "gateway": GATEWAY,
                    "dns": [DNS_SERVER],
                }],
            },
            "agent": {
                "image": AGENT_IMAGE_NAME_VM,
            },
        },
    }
    kubectl(["apply", "-f", "-"], input=json.dumps(protonode).encode(),
            stdout=DEVNULL)
    log("  Resources created")

    log("Starting metalman serve-pxe")
    proc = spawn([
        "sudo", str(BINARY), "serve-pxe", f"--site={SITE}", f"--bind-address={SERVER_IP}",
        f"--cache-dir={CACHE_DIR}", f"--apiserver-url={server_url}",
        f"--serve-url={SERVE_URL}", "--dhcp-interface=virbr-smoke",
        "--leader-elect-lease-duration=60s",
        "--leader-elect-renew-deadline=40s",
        "--leader-elect-retry-period=5s",
    ], TMPDIR / "serve.log")
    log(f"  serve PID={proc.pid}")

    time.sleep(2)
    check_procs()

    log("Triggering reimage")
    run([str(KUBECTL_UNBOUNDED), "machine", "reimage", NODE_NAME])

    log("Waiting for kubelet to join the cluster...")
    wait_k8s_node(NODE_NAME, timeout=900)
    assert_node_ready(NODE_NAME, timeout=300)

    log("")
    log("Smoke test PASSED")


if __name__ == "__main__":
    main()
