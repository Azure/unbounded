#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import atexit
import base64
import gzip
import json
import os
import signal
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import textwrap
import threading
import time
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parent.parent
TMP_ROOT = Path(os.environ.get("SMOKE_TMP_ROOT", tempfile.gettempdir()))
TMP_ROOT.mkdir(parents=True, exist_ok=True)
TMPDIR = Path(tempfile.mkdtemp(prefix="unbounded-metal-smoke-", dir=TMP_ROOT))
os.chmod(TMPDIR, 0o755)
SITE = "smoke"
NODE_NAME = "smoke-node"
NODE_NS = "default"
API_GROUP = "unbounded-cloud.io"
API_VERSION = f"{API_GROUP}/v1alpha3"
NODE_LABEL_KEY = "unbounded-cloud.io/smoke-test"
NODE_LABEL_VALUE = "metalman"
METALMAN_NAMESPACE = "unbounded-kube"
METALMAN_CONTROLLER_SA = "metalman-controller"
VM_NAME = "unbounded-metal-smoke"
NET_NAME = "unbounded-metal-smoke"
SUBNET = "192.168.200"
SERVER_IP = f"{SUBNET}.1"
NODE_IP = f"{SUBNET}.10"
KIND_SMOKE_IP = f"{SUBNET}.2"  # IP assigned to the kind container on virbr-smoke
GATEWAY = SERVER_IP
DNS_SERVER = "8.8.8.8"
MAC_ADDRESS = "52:54:00:aa:bb:01"
SUSHY_PORT = 8443
HTTP_PORT = 8880
AGENT_DOWNLOAD_PORT = 8881
CACHE_DIR = TMPDIR / "cache"
ARTIFACT_DIR = TMPDIR / "artifacts"
SERVE_URL = f"http://{SERVER_IP}:{HTTP_PORT}"
AGENT_TARBALL = ARTIFACT_DIR / "unbounded-agent-linux-amd64.tar.gz"
AGENT_DOWNLOAD_URL = f"http://{SERVER_IP}:{AGENT_DOWNLOAD_PORT}/{AGENT_TARBALL.name}"
REGISTRY_PORT = 5555
REGISTRY_CONTAINER = "unbounded-smoke-registry"
RAW_IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/host-ubuntu2404:smoke"
RAID_IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/host-ubuntu2404-raid:smoke"
NETBOOT_IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/netboot:smoke"
AGENT_IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/agent-ubuntu2404:smoke"
# The agent runs inside a VM on an isolated libvirt network. "localhost" inside
# the VM resolves to the VM's own loopback, not the host.  Use the host's
# bridge IP so the VM can reach the registry over the virtual network.
AGENT_IMAGE_NAME_VM = f"{SERVER_IP}:{REGISTRY_PORT}/unbounded/agent-ubuntu2404:smoke"
BINARY = REPO_ROOT / "bin" / "metalman"
AGENT_BINARY = REPO_ROOT / "bin" / "unbounded-agent"
KUBECTL_UNBOUNDED = REPO_ROOT / "bin" / "kubectl-unbounded"
SERIAL_SOCK = TMPDIR / "console.sock"
QGA_SOCK = TMPDIR / "qga.sock"
INSTALL_MODE = os.environ.get("SMOKE_INSTALL_MODE", "Raw")
if INSTALL_MODE.lower() == "raw":
    INSTALL_MODE = "Raw"
elif INSTALL_MODE.lower() == "raid1":
    INSTALL_MODE = "RAID1"
else:
    print(f"FAIL: unsupported SMOKE_INSTALL_MODE={INSTALL_MODE!r}; expected Raw or RAID1", file=sys.stderr)
    sys.exit(1)
BOOT_PROTOCOL = os.environ.get("SMOKE_BOOT_PROTOCOL", "PXE")
if BOOT_PROTOCOL.lower() == "pxe":
    BOOT_PROTOCOL = "PXE"
elif BOOT_PROTOCOL.lower() == "http":
    BOOT_PROTOCOL = "HTTP"
else:
    print(f"FAIL: unsupported SMOKE_BOOT_PROTOCOL={BOOT_PROTOCOL!r}; expected PXE or HTTP", file=sys.stderr)
    sys.exit(1)
IMAGE_NAME = RAID_IMAGE_NAME if INSTALL_MODE == "RAID1" else RAW_IMAGE_NAME
RAID_DISK_A_SERIAL = "unbounded-raid-a"
RAID_DISK_B_SERIAL = "unbounded-raid-b"
RAID_DISK_A_PATH = f"/dev/disk/by-id/virtio-{RAID_DISK_A_SERIAL}"
RAID_DISK_B_PATH = f"/dev/disk/by-id/virtio-{RAID_DISK_B_SERIAL}"
# The nspawn machine name used by the agent (must match the constant in
# cmd/agent/internal/goalstates/constants.go - NSpawnMachineKube1).
NSPAWN_MACHINE = "kube1"

KUBECTL = "kubectl"
VIRSH = ["virsh", "--connect", "qemu:///system"]
DEVNULL = subprocess.DEVNULL

_procs: list[subprocess.Popen[Any]] = []

_TRANSIENT_QGA_ERRORS = (
    "domain is not running",
    "guest agent is not responding",
    "qemu guest agent is not connected",
    "qemu guest agent is not configured",
    "guest agent is not available",
)


def log(msg: str) -> None:
    print(f"==> {msg}", file=sys.stderr)


def die(msg: str) -> None:
    print(f"FAIL: {msg}", file=sys.stderr)
    try:
        collect_debug_logs()
    except Exception as e:
        log(f"  (debug log collection failed: {e})")
    sys.exit(1)


def find_ovmf_firmware() -> tuple[Path, Path]:
    candidates = [
        (Path("/usr/share/OVMF/OVMF_CODE_4M.fd"), Path("/usr/share/OVMF/OVMF_VARS_4M.fd")),
        (Path("/usr/share/OVMF/OVMF_CODE.fd"), Path("/usr/share/OVMF/OVMF_VARS.fd")),
        (Path("/usr/share/edk2/x64/OVMF_CODE.4m.fd"), Path("/usr/share/edk2/x64/OVMF_VARS.4m.fd")),
        (Path("/usr/share/edk2/x64/OVMF_CODE.fd"), Path("/usr/share/edk2/x64/OVMF_VARS.fd")),
        (Path("/usr/share/edk2/ovmf/OVMF_CODE.fd"), Path("/usr/share/edk2/ovmf/OVMF_VARS.fd")),
    ]
    for code, vars_template in candidates:
        if code.is_file() and vars_template.is_file():
            return code, vars_template

    die("could not find OVMF firmware; install ovmf or edk2-ovmf")


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
    log_file = open(log_path, "w")  # noqa: SIM115 - intentionally long-lived
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

        # Socket closed - VM probably rebooted.  Retry.
        time.sleep(1)


def _is_transient_qga_error(stderr: str) -> bool:
    err = stderr.lower()
    return any(marker in err for marker in _TRANSIENT_QGA_ERRORS)


def guest_exec(command: str, timeout: int = 30) -> tuple[int, str, str]:
    """Execute a command inside the VM via the QEMU guest agent.

    Returns (exit_code, stdout, stderr).  Requires the guest agent channel
    to be configured on the VM and the qemu-guest-agent service running
    inside the guest.
    """
    deadline = time.monotonic() + timeout
    last_error = ""

    while time.monotonic() < deadline:
        if not _vm_is_running():
            time.sleep(1)
            continue

        exec_req = json.dumps({
            "execute": "guest-exec",
            "arguments": {
                "path": "/bin/bash",
                "arg": ["-c", command],
                "capture-output": True,
            },
        })
        result = subprocess.run(
            [*VIRSH, "qemu-agent-command", VM_NAME, exec_req],
            capture_output=True, text=True,
            timeout=max(1, min(10, deadline - time.monotonic())),
        )
        if result.returncode != 0:
            last_error = result.stderr.strip()
            if _is_transient_qga_error(last_error):
                time.sleep(1)
                continue
            raise RuntimeError(f"guest-exec failed: {last_error}")

        pid = json.loads(result.stdout)["return"]["pid"]

        # Poll guest-exec-status until the process exits.  If the guest
        # reboots while the command is active, restart the command.
        restart_command = False
        while time.monotonic() < deadline:
            status_req = json.dumps({
                "execute": "guest-exec-status",
                "arguments": {"pid": pid},
            })
            result = subprocess.run(
                [*VIRSH, "qemu-agent-command", VM_NAME, status_req],
                capture_output=True, text=True,
                timeout=max(1, min(10, deadline - time.monotonic())),
            )
            if result.returncode != 0:
                last_error = result.stderr.strip()
                if _is_transient_qga_error(last_error):
                    restart_command = True
                    break
                raise RuntimeError(f"guest-exec-status failed: {last_error}")

            status = json.loads(result.stdout)["return"]
            if status.get("exited"):
                exit_code = status.get("exitcode", -1)
                stdout = base64.b64decode(status.get("out-data", "")).decode("utf-8", errors="replace")
                stderr = base64.b64decode(status.get("err-data", "")).decode("utf-8", errors="replace")
                return exit_code, stdout, stderr

            time.sleep(0.5)

        if restart_command:
            time.sleep(1)
            continue

        break

    if last_error:
        raise TimeoutError(f"guest-exec did not complete within {timeout}s; last error: {last_error}")

    raise TimeoutError(f"guest-exec did not complete within {timeout}s")

def collect_debug_logs() -> None:
    """Use the QEMU guest agent to dump kubelet and agent debug information.

    Best-effort: failures are logged but do not abort the test.
    """
    log("Collecting debug logs from VM via QEMU guest agent...")
    commands = [
        # Network diagnostics - must come first to diagnose download hangs.
        ("resolv.conf", "cat /etc/resolv.conf"),
        ("ip addr", "ip -4 addr show"),
        ("ip route", "ip route show"),
        ("block devices", "lsblk -o NAME,PATH,SIZE,FSTYPE,TYPE,MOUNTPOINTS"),
        ("fstab", "cat /etc/fstab"),
        ("mdstat", "cat /proc/mdstat 2>/dev/null || true"),
        ("mdadm detail", "mdadm --detail /dev/md/unbounded-root 2>/dev/null || mdadm --detail /dev/md* 2>/dev/null || true"),
        ("efibootmgr", "efibootmgr -v 2>/dev/null || true"),
        ("dns test (dl.k8s.io)", "timeout 5 getent hosts dl.k8s.io || echo 'DNS FAILED'"),
        ("curl test (dl.k8s.io)", "timeout 10 curl -sS -o /dev/null -w '%{http_code}' https://dl.k8s.io/ || echo 'CURL FAILED'"),
        ("dns test (github.com)", "timeout 5 getent hosts github.com || echo 'DNS FAILED'"),
        ("iptables nat", "iptables -t nat -L -n 2>/dev/null || nft list ruleset 2>/dev/null | head -60"),
        # Agent and service logs.
        ("systemctl status", "systemctl --no-pager status"),
        ("unbounded-agent journal", "journalctl --no-pager -n 200 -u cloud-final.service"),
        ("machinectl list", "machinectl list --no-pager"),
        ("nspawn machine status", f"machinectl status {NSPAWN_MACHINE} --no-pager"),
        ("kubelet journal (nspawn)", (
            f"systemd-run --pipe --wait --machine={NSPAWN_MACHINE} "
            "journalctl --no-pager -n 200 -u kubelet.service"
        )),
        ("containerd journal (nspawn)", (
            f"systemd-run --pipe --wait --machine={NSPAWN_MACHINE} "
            "journalctl --no-pager -n 100 -u containerd.service"
        )),
        ("kubelet service status (nspawn)", (
            f"systemd-run --pipe --wait --machine={NSPAWN_MACHINE} "
            "systemctl --no-pager status kubelet.service"
        )),
    ]
    for label, cmd in commands:
        log(f"  --- {label} ---")
        try:
            exit_code, stdout, stderr = guest_exec(cmd, timeout=30)
            if stdout:
                sys.stderr.write(stdout)
                sys.stderr.flush()
            if stderr:
                sys.stderr.write(stderr)
                sys.stderr.flush()
        except (RuntimeError, TimeoutError, subprocess.TimeoutExpired, OSError) as e:
            log(f"  (failed to collect {label}: {e})")

    # Kubernetes-side diagnostics (run from the host via kubectl).
    # These commands survive the QEMU guest agent dying inside the VM, which
    # is critical because the in-guest collectors above frequently fail with
    # "Guest agent is not responding" by the time the test gives up.
    k8s_commands = [
        ("kubectl describe node", [KUBECTL, "describe", "node", NODE_NAME]),
        ("kubectl get pods -A", [KUBECTL, "get", "pods", "-A", "-o", "wide"]),
        ("kubectl get events", [
            KUBECTL, "get", "events", "-A", "--sort-by=.lastTimestamp",
        ]),
    ]
    # Logs from system pods scheduled on the smoke-node kubelet. kindnet and
    # kube-proxy crashing in CrashLoopBackOff is the most common failure mode
    # seen in CI; capturing both the current and previous container logs is
    # what makes the failure diagnosable after the fact.
    node_pod_labels = [
        ("kindnet", "app=kindnet"),
        ("kube-proxy", "k8s-app=kube-proxy"),
    ]
    for name, selector in node_pod_labels:
        k8s_commands.append((
            f"kubectl get {name} pods on smoke-node",
            [KUBECTL, "-n", "kube-system", "get", "pods",
             "-l", selector,
             "--field-selector", f"spec.nodeName={NODE_NAME}",
             "-o", "wide"],
        ))
    for label, cmd in k8s_commands:
        log(f"  --- {label} ---")
        try:
            result = subprocess.run(
                cmd, capture_output=True, text=True, timeout=15,
            )
            if result.stdout:
                sys.stderr.write(result.stdout)
                sys.stderr.flush()
            if result.stderr:
                sys.stderr.write(result.stderr)
                sys.stderr.flush()
        except Exception as e:
            log(f"  (failed to collect {label}: {e})")

    # For each system pod scheduled on smoke-node, also dump per-pod
    # describe, current container logs, and previous (crashed) container
    # logs. The previous logs are usually the only place that records why
    # kindnet exited.
    for name, selector in node_pod_labels:
        try:
            result = subprocess.run(
                [KUBECTL, "-n", "kube-system", "get", "pods",
                 "-l", selector,
                 "--field-selector", f"spec.nodeName={NODE_NAME}",
                 "-o", "jsonpath={.items[*].metadata.name}"],
                capture_output=True, text=True, timeout=15,
            )
            pod_names = result.stdout.split() if result.returncode == 0 else []
        except Exception as e:
            log(f"  (failed to list {name} pods: {e})")
            continue
        for pod in pod_names:
            for label, cmd in (
                (f"kubectl describe pod {pod}",
                 [KUBECTL, "-n", "kube-system", "describe", "pod", pod]),
                (f"kubectl logs {pod}",
                 [KUBECTL, "-n", "kube-system", "logs", pod,
                  "--all-containers=true", "--tail=200"]),
                (f"kubectl logs --previous {pod}",
                 [KUBECTL, "-n", "kube-system", "logs", pod,
                  "--all-containers=true", "--previous", "--tail=200"]),
            ):
                log(f"  --- {label} ---")
                try:
                    result = subprocess.run(
                        cmd, capture_output=True, text=True, timeout=15,
                    )
                    if result.stdout:
                        sys.stderr.write(result.stdout)
                        sys.stderr.flush()
                    if result.stderr:
                        sys.stderr.write(result.stderr)
                        sys.stderr.flush()
                except Exception as e:
                    log(f"  (failed to collect {label}: {e})")

    log("  --- end debug logs ---")


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
    # Remove veth pair used to connect kind container to virbr-smoke.
    run_quiet(["sudo", "ip", "link", "delete", "veth-kind-smoke"])
    # Kill any leftover sushy-emulator from a previous run.
    run_quiet(["sudo", "pkill", "-f", "sushy-emulator"])
    # Kill any leftover metalman serve-pxe from a previous run.
    # Use the binary path to avoid matching this script (smoke-metalman.py).
    run_quiet(["sudo", "pkill", "-f", "bin/metalman"])
    # Kill any leftover artifact download server from a previous run.
    run_quiet(["sudo", "pkill", "-f", f"python3 -m http.server {AGENT_DOWNLOAD_PORT}"])
    # Stop and remove leftover local registry container.
    run_quiet(["docker", "rm", "-f", REGISTRY_CONTAINER])
    # Delete stale leader-election leases so new processes acquire immediately.
    run_quiet([KUBECTL, "-n", METALMAN_NAMESPACE, "delete", "lease",
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
    run_quiet(["sudo", "iptables", "-D", "INPUT", "-i", "virbr-smoke", "-j", "ACCEPT"], check=False)
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


def write_service_account_kubeconfig(namespace: str, service_account: str, path: Path) -> None:
    token = run(
        [KUBECTL, "-n", namespace, "create", "token", service_account, "--duration=2h"],
        capture_output=True,
        text=True,
    ).stdout.strip()
    if not token:
        die(f"Failed to create token for ServiceAccount {namespace}/{service_account}")

    raw_config = run(
        [KUBECTL, "config", "view", "--raw", "--minify", "-o", "json"],
        capture_output=True,
        text=True,
    ).stdout
    current = json.loads(raw_config)
    clusters = current.get("clusters", [])
    if len(clusters) != 1:
        die(f"Expected one current kubeconfig cluster, got {len(clusters)}")
    cluster_name = clusters[0]["name"]

    user_name = f"{namespace}:{service_account}"
    kubeconfig = {
        "apiVersion": "v1",
        "kind": "Config",
        "clusters": clusters,
        "contexts": [{
            "name": user_name,
            "context": {
                "cluster": cluster_name,
                "namespace": namespace,
                "user": user_name,
            },
        }],
        "current-context": user_name,
        "users": [{"name": user_name, "user": {"token": token}}],
    }

    path.write_text(json.dumps(kubeconfig), encoding="utf-8")
    os.chmod(path, 0o600)


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
    # 127.0.0.1:<nodeport> which is unreachable from the VM.  Rewrite to
    # KIND_SMOKE_IP which is the kind container's address on virbr-smoke,
    # the same L2 network the VM is on.  The Docker bridge IP
    # (172.18.0.x) is NOT routable from the VM because iptables isolation
    # rules block forwarding between bridges.
    from urllib.parse import urlparse
    parsed = urlparse(url)
    if parsed.hostname in ("127.0.0.1", "localhost", "::1"):
        url = f"{parsed.scheme}://{KIND_SMOKE_IP}:6443"
        log(f"  Rewrote apiserver URL to {url} (kind container on virbr-smoke)")

    return url


def configure_kind_control_plane_node_ip(container: str, node_ip: str) -> None:
    """Make the kind control-plane Node advertise its VM-reachable IP."""
    log(f"Configuring {container} kubelet node IP as {node_ip}")
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
        printf 'KUBELET_KUBEADM_ARGS="%s --node-ip={node_ip}"\n' "$new_args" >/var/lib/kubelet/kubeadm-flags.env
        systemctl restart kubelet
    """)
    run(["docker", "exec", container, "sh", "-c", script])

    for elapsed in range(120):
        result = subprocess.run(
            [KUBECTL, "get", "node", container, "-o", "json"],
            capture_output=True, text=True,
        )
        if result.returncode == 0:
            node = json.loads(result.stdout)
            internal_ips = [
                address.get("address", "")
                for address in node.get("status", {}).get("addresses", [])
                if address.get("type") == "InternalIP"
            ]
            if internal_ips == [node_ip]:
                log(f"  Node '{container}' advertises InternalIP {node_ip}")
                return
            if elapsed > 0 and elapsed % 15 == 0:
                log(f"    ({elapsed}s) InternalIP addresses: {internal_ips}")
        time.sleep(1)

    die(f"Timed out waiting for Node '{container}' to advertise only InternalIP {node_ip}")


def _probe_vm_network() -> None:
    """Run quick network diagnostics inside the VM via guest agent."""
    log("  Probing VM network (one-time diagnostic)...")
    for label, cmd in [
        ("resolv.conf", "cat /etc/resolv.conf"),
        ("ip route", "ip route show"),
        ("dns dl.k8s.io", "timeout 5 getent hosts dl.k8s.io 2>&1 || echo 'DNS FAILED'"),
        ("curl dl.k8s.io", "timeout 10 curl -sSf -o /dev/null -w '%{http_code}' https://dl.k8s.io/ 2>&1 || echo 'CURL FAILED'"),
    ]:
        try:
            _, stdout, stderr = guest_exec(cmd, timeout=15)
            out = (stdout + stderr).strip()
            log(f"    [{label}] {out}")
        except Exception as e:
            log(f"    [{label}] (failed: {e})")


def _vm_is_running() -> bool:
    """Return True if the VM domain is in 'running' state."""
    result = subprocess.run(
        [*VIRSH, "domstate", VM_NAME],
        capture_output=True, text=True,
    )
    return result.returncode == 0 and "running" in result.stdout.strip()


def wait_vm_state(expected: str, timeout: int = 300) -> None:
    """Wait for the libvirt domain state to contain *expected*."""
    log(f"  Waiting for VM '{VM_NAME}' state to contain {expected!r}...")
    last_state = ""
    for elapsed in range(timeout):
        check_procs()
        result = subprocess.run(
            [*VIRSH, "domstate", VM_NAME],
            capture_output=True, text=True,
        )
        state = result.stdout.strip() if result.returncode == 0 else result.stderr.strip()
        if expected in state:
            log(f"  VM '{VM_NAME}' state is {state!r}")
            return
        if elapsed > 0 and elapsed % 15 == 0 and state != last_state:
            last_state = state
            log(f"    ({elapsed}s) VM state={state or 'unknown'}")
        time.sleep(1)
    die(f"Timed out waiting for VM '{VM_NAME}' state to contain {expected!r}")


def wait_guest_agent(timeout: int = 300) -> None:
    """Wait until the guest OS responds through the QEMU guest agent."""
    log("  Waiting for QEMU guest agent to respond...")
    for elapsed in range(timeout):
        check_procs()
        try:
            exit_code, _, _ = guest_exec("true", timeout=10)
            if exit_code == 0:
                log("  QEMU guest agent is responsive")
                return
        except (RuntimeError, TimeoutError, subprocess.TimeoutExpired, OSError):
            pass
        if elapsed > 0 and elapsed % 15 == 0:
            log(f"    ({elapsed}s) guest agent not responsive yet")
        time.sleep(1)
    die("Timed out waiting for QEMU guest agent")


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
    net_diag_done = False
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

                # Check if the VM is still alive; bail early if it crashed.
                if not _vm_is_running():
                    # Log host disk free space to help diagnose qcow2 growth.
                    df = subprocess.run(
                        ["df", "-h", str(TMPDIR)],
                        capture_output=True, text=True,
                    )
                    log(f"    Host disk:\n{df.stdout}")
                    die(f"VM '{VM_NAME}' is no longer running (crashed or shut down)")

                # Run network diagnostics once after 180s if still stuck.
                if elapsed >= 180 and not net_diag_done:
                    net_diag_done = True
                    _probe_vm_network()
            time.sleep(1)
            continue
        log(f"  Node '{name}' appeared in cluster")
        return
    die(f"Timed out waiting for Node '{name}'")


def get_node_boot_id(name: str) -> str:
    result = subprocess.run(
        [KUBECTL, "get", "node", name, "-o", "jsonpath={.status.nodeInfo.bootID}"],
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        die(f"Failed to read Node '{name}' boot ID: {result.stderr.strip()}")
    boot_id = result.stdout.strip()
    if not boot_id:
        die(f"Node '{name}' has no status.nodeInfo.bootID")
    return boot_id


def wait_node_boot_id_changed(name: str, previous_boot_id: str, timeout: int = 600) -> None:
    log(f"  Waiting for Node '{name}' boot ID to change...")
    for elapsed in range(timeout):
        check_procs()
        result = subprocess.run(
            [KUBECTL, "get", "node", name, "-o", "jsonpath={.status.nodeInfo.bootID}"],
            capture_output=True, text=True,
        )
        boot_id = result.stdout.strip() if result.returncode == 0 else ""
        if boot_id and boot_id != previous_boot_id:
            log(f"  Node '{name}' boot ID changed")
            return
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"    ({elapsed}s) bootID={boot_id or 'not set'}")
        time.sleep(1)
    die(f"Timed out waiting for Node '{name}' boot ID to change")


def _restart_crashing_pods(node_name: str, namespace: str, label: str) -> None:
    """Delete pods matching *label* on *node_name* that are in CrashLoopBackOff.

    This resets the exponential backoff timer so the pod gets a fresh start.
    Useful when a DaemonSet pod crashes transiently during node initialization
    (e.g. kindnet racing with network setup on a QEMU VM).
    """
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
        for cs in pod.get("status", {}).get("containerStatuses", []):
            if cs.get("ready"):
                continue
            waiting = cs.get("state", {}).get("waiting", {})
            terminated = cs.get("state", {}).get("terminated", {})
            restart_count = cs.get("restartCount", 0)
            waiting_reason = waiting.get("reason")
            terminated_reason = terminated.get("reason")
            if restart_count >= 2 or waiting_reason == "CrashLoopBackOff":
                log(f"    Deleting crashing pod {pod_name} "
                    f"(restarts={restart_count}, waiting={waiting_reason or 'none'}, "
                    f"terminated={terminated_reason or 'none'}) to reset backoff")
                subprocess.run(
                    [KUBECTL, "delete", "pod", "-n", namespace, pod_name,
                     "--grace-period=0", "--force"],
                    capture_output=True, text=True,
                )


def assert_node_ready(name: str, timeout: int = 720) -> None:
    """Assert the Node reaches Ready status within timeout seconds.

    The timeout must be generous enough to survive multiple kindnet
    CrashLoopBackOff cycles. In CI kindnet can need several fresh pod
    attempts before it writes the CNI config; 720s accommodates the slow
    tail without hiding real boot failures.
    """
    log(f"  Waiting for Node '{name}' to become Ready...")
    pod_restart_interval = 30  # seconds between CrashLoopBackOff resets
    last_restart_attempt = 0
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
        # Periodically reset failing critical DaemonSet pods. Kindnet can fail
        # transiently during VM network initialization and may sit in Error
        # before Kubernetes reports CrashLoopBackOff.
        if elapsed >= 30 and elapsed - last_restart_attempt >= pod_restart_interval:
            _restart_crashing_pods(name, "kube-system", "app=kindnet")
            last_restart_attempt = elapsed
        time.sleep(1)
    die(f"Timed out waiting for Node '{name}' to become Ready")


def assert_node_label(name: str, key: str, value: str) -> None:
    result = subprocess.run(
        [KUBECTL, "get", "node", name, "-o", "json"],
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        die(f"Failed to read Node '{name}': {result.stderr.strip()}")

    node = json.loads(result.stdout)
    labels = node.get("metadata", {}).get("labels", {})
    got = labels.get(key)
    if got != value:
        die(f"Node '{name}' label {key!r} = {got!r}, want {value!r}")

    log(f"  Node '{name}' has label {key}={value}")


def assert_cloud_init_done(timeout: int = 900) -> None:
    """Assert the Machine's CloudInitDone condition reaches True/Succeeded.

    Called before waiting for the Kubernetes Node to appear because
    cloud-init must finish before the kubelet can join the cluster.
    Fails fast if the condition transitions to Failed so that the
    smoke test does not wait for the full node-join timeout.
    """
    log(f"  Waiting for Machine '{NODE_NAME}' CloudInitDone condition...")
    for elapsed in range(timeout):
        check_procs()
        result = subprocess.run(
            [KUBECTL, "get", f"machines.{API_GROUP}", NODE_NAME, "-o", "json"],
            capture_output=True, text=True,
        )
        status = ""
        reason = ""
        message = ""
        if result.returncode == 0:
            try:
                conditions = json.loads(result.stdout).get("status", {}).get("conditions", [])
                for c in conditions:
                    if c.get("type") == "CloudInitDone":
                        status = c.get("status", "")
                        reason = c.get("reason", "")
                        message = c.get("message", "")
                        break
            except (json.JSONDecodeError, KeyError):
                pass

        if status == "True":
            if reason != "Succeeded":
                die(f"CloudInitDone condition is True but reason is {reason!r}, expected 'Succeeded'")
            log(f"  Machine '{NODE_NAME}' CloudInitDone condition is True/Succeeded")
            return
        if status == "False" and reason == "Failed":
            die(f"Cloud-init failed: {message}")
        if BOOT_PROTOCOL == "HTTP" and elapsed > 15 and not _vm_is_running():
            die("VM powered off during HTTP boot before CloudInitDone")
        if elapsed > 0 and elapsed % 30 == 0:
            log(f"    ({elapsed}s) CloudInitDone status={status or 'not set'} reason={reason or 'not set'}")
        time.sleep(1)
    die(f"Timed out waiting for CloudInitDone condition on Machine '{NODE_NAME}'")


def assert_machine_condition(
    condition_type: str,
    expected_status: str,
    expected_reason: str | None = None,
    timeout: int = 300,
) -> None:
    """Assert a Machine condition reaches the requested status and reason."""
    log(f"  Waiting for Machine '{NODE_NAME}' condition {condition_type}={expected_status}...")
    last: str | None = None
    for elapsed in range(timeout):
        check_procs()
        result = subprocess.run(
            [KUBECTL, "get", f"machines.{API_GROUP}", NODE_NAME, "-o", "json"],
            capture_output=True,
            text=True,
        )
        if result.returncode == 0:
            machine = json.loads(result.stdout)
            for cond in machine.get("status", {}).get("conditions", []):
                if cond.get("type") != condition_type:
                    continue
                status = cond.get("status", "")
                reason = cond.get("reason", "")
                current = f"{status}/{reason}"
                if status == expected_status and (expected_reason is None or reason == expected_reason):
                    log(f"  Machine condition {condition_type} is {current}")
                    return
                if current != last:
                    last = current
                    log(f"    ({elapsed}s) {condition_type}={current}")
        elif elapsed % 15 == 0:
            log(f"    ({elapsed}s) Machine not readable yet: {result.stderr.strip()}")
        time.sleep(1)

    if expected_reason is None:
        die(f"Timed out waiting for Machine condition {condition_type}={expected_status}")
    die(f"Timed out waiting for Machine condition {condition_type}={expected_status}/{expected_reason}")


def wait_machine_operation_complete(name: str, timeout: int = 1800) -> None:
    log(f"  Waiting for MachineOperation '{name}' to complete...")
    for elapsed in range(timeout):
        check_procs()
        result = subprocess.run(
            [KUBECTL, "get", f"machineoperations.{API_GROUP}", name, "-o", "json"],
            capture_output=True, text=True,
        )
        if result.returncode == 0:
            op = json.loads(result.stdout)
            status = op.get("status", {})
            phase = status.get("phase", "")
            message = status.get("message", "")
            targets = status.get("targets", [])
            if phase == "Complete":
                if not targets:
                    die(f"MachineOperation '{name}' completed without status.targets")
                target = targets[0]
                if target.get("machineRef") != NODE_NAME or target.get("phase") != "Complete":
                    die(f"MachineOperation '{name}' has unexpected target status: {target}")
                log(f"  MachineOperation '{name}' completed")
                return
            if phase == "Failed":
                die(f"MachineOperation '{name}' failed: {message}; targets={targets}")
            if elapsed > 0 and elapsed % 30 == 0:
                log(f"    ({elapsed}s) MachineOperation phase={phase or 'empty'} message={message or 'empty'} targets={targets}")
        elif elapsed > 0 and elapsed % 30 == 0:
            log(f"    ({elapsed}s) MachineOperation '{name}' not found yet")
        time.sleep(1)
    die(f"Timed out waiting for MachineOperation '{name}'")


def create_machine_operation(
    name: str,
    kind: str,
    *,
    machine_ref: str | None = NODE_NAME,
    site_selector: str | None = None,
    ttl_seconds: int = 3600,
) -> str:
    spec: dict[str, Any] = {
        "operationKind": kind,
        "ttlSecondsAfterFinished": ttl_seconds,
    }
    if site_selector is not None:
        spec["machineSelector"] = {"matchLabels": {f"{API_GROUP}/site": site_selector}}
    elif machine_ref is not None:
        spec["machineRef"] = machine_ref

    operation = {
        "apiVersion": API_VERSION,
        "kind": "MachineOperation",
        "metadata": {"name": name},
        "spec": spec,
    }
    kubectl(["apply", "-f", "-"], input=json.dumps(operation).encode(), stdout=DEVNULL)
    return name


def run_kubectl_unbounded_operation(args: list[str], log_name: str) -> subprocess.Popen[Any]:
    proc = spawn([str(KUBECTL_UNBOUNDED), "machine", *args], TMPDIR / log_name)
    log(f"  kubectl-unbounded {' '.join(args)} PID={proc.pid}")
    return proc


def wait_process_success(proc: subprocess.Popen[Any], timeout: int) -> None:
    try:
        rc = proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        die(f"Process {proc.args} did not finish within {timeout}s")
    try:
        _procs.remove(proc)
    except ValueError:
        # Process cleanup is best-effort because another cleanup path may have removed it.
        pass
    if rc != 0:
        die(f"Process {proc.args} exited with code {rc}")


def assert_log_contains(path: Path, needle: str) -> None:
    text = path.read_text(encoding="utf-8", errors="replace")
    if needle not in text:
        die(f"Expected {path} to contain {needle!r}")


def guestfs_env() -> dict[str, str]:
    env = os.environ.copy()
    env.setdefault("LIBGUESTFS_BACKEND", "direct")
    env["TMPDIR"] = str(TMPDIR)
    env["TEMP"] = str(TMPDIR)
    env["TMP"] = str(TMPDIR)
    return env


def require_tools(tools: list[str]) -> None:
    missing = [tool for tool in tools if shutil.which(tool) is None]
    if missing:
        die("Missing required tool(s): " + ", ".join(missing))


def docker_copy_from_image(image: str, src: str, dest: Path) -> None:
    result = run(
        ["docker", "create", image, "/copy-only"],
        capture_output=True,
        text=True,
    )
    cid = result.stdout.strip()
    if not cid:
        die(f"docker create {image} did not return a container id")

    try:
        run(["docker", "cp", f"{cid}:{src}", str(dest)])
    finally:
        run_quiet(["docker", "rm", "-f", cid], check=False)


HTTP_BOOT_SELECTOR_SCRIPT = """\
#!/bin/bash
set -euxo pipefail

log_file=/var/log/metalman-httpboot-selector.log
touch "${log_file}"
chmod 0644 "${log_file}"
if [ -w /dev/ttyS0 ]; then
    exec > >(tee -a "${log_file}" >/dev/ttyS0) 2>&1
else
    exec >>"${log_file}" 2>&1
fi

state_dir=/var/lib/metalman-httpboot-selector
state_file="${state_dir}/requested"
mkdir -p "${state_dir}"

echo "=== metalman HTTP boot selector ==="
date -u

if [ -f "${state_file}" ]; then
    echo "=== returned to helper disk after requesting HTTP BootNext ==="
    echo "Firmware HTTP boot likely failed or fell through to disk."
    efibootmgr -v || true
    systemctl poweroff --force
    exit 0
fi

echo requested >"${state_file}"

for attempt in $(seq 1 10); do
    echo "=== efibootmgr before, attempt ${attempt} ==="
    efibootmgr -v || true
    entry="$(efibootmgr -v 2>/dev/null | sed -n 's/^Boot\\([0-9A-Fa-f]\\{4\\}\\).*UEFI HTTPv4.*/\\1/p' | head -n1)"
    if [ -n "${entry}" ]; then
        break
    fi
    sleep 1
done

if [ -z "${entry:-}" ]; then
    echo "=== no UEFI HTTPv4 boot entry found ==="
    systemctl poweroff --force
    exit 1
fi

echo "=== setting BootNext to UEFI HTTPv4 Boot${entry} ==="
efibootmgr -n "${entry}"
efibootmgr -v || true
echo "=== rebooting into UEFI HTTPv4 ==="
systemctl reboot --force
exit 0
"""


HTTP_BOOT_SELECTOR_SERVICE = """\
[Unit]
Description=One-shot metalman UEFI HTTP boot selector
After=local-fs.target

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/metalman-httpboot-selector.sh

[Install]
WantedBy=multi-user.target
"""


def prepare_http_boot_helper_disk(raw_image: str, dest: Path) -> None:
    log(f"Preparing HTTP boot helper disk {dest.name}")
    require_tools(["docker", "virt-customize", "qemu-img"])

    workdir = TMPDIR / "http-boot-helper"
    workdir.mkdir(parents=True, exist_ok=True)
    disk_gz = workdir / "disk.img.gz"
    disk_raw = workdir / "disk.img"
    script = workdir / "metalman-httpboot-selector.sh"
    service = workdir / "metalman-httpboot-selector.service"

    log(f"  Extracting /disk/disk.img.gz from {raw_image}")
    docker_copy_from_image(raw_image, "/disk/disk.img.gz", disk_gz)
    with gzip.open(disk_gz, "rb") as src, disk_raw.open("wb") as dst:
        shutil.copyfileobj(src, dst)
    disk_gz.unlink()

    script.write_text(HTTP_BOOT_SELECTOR_SCRIPT, encoding="utf-8")
    service.write_text(HTTP_BOOT_SELECTOR_SERVICE, encoding="utf-8")

    log("  Injecting one-shot efibootmgr selector")
    run([
        "virt-customize",
        "-a", str(disk_raw),
        "--copy-in", f"{script}:/usr/local/sbin",
        "--copy-in", f"{service}:/etc/systemd/system",
        "--run-command", "chmod 0755 /usr/local/sbin/metalman-httpboot-selector.sh",
        "--run-command", "mkdir -p /etc/cloud && touch /etc/cloud/cloud-init.disabled",
        "--run-command", "rm -rf /var/lib/metalman-httpboot-selector",
        "--run-command", "systemctl enable metalman-httpboot-selector.service",
    ], env=guestfs_env())

    log("  Converting helper disk to qcow2")
    run(["qemu-img", "convert", "-f", "raw", "-O", "qcow2", str(disk_raw), str(dest)])
    run(["qemu-img", "resize", str(dest), "20G"])


def virt_filesystems(image: Path) -> list[tuple[str, str, str]]:
    result = run(
        ["virt-filesystems", "-a", str(image), "--filesystems", "--long", "--no-title"],
        capture_output=True,
        text=True,
        env=guestfs_env(),
    )
    filesystems: list[tuple[str, str, str]] = []
    for line in result.stdout.splitlines():
        fields = line.split()
        if len(fields) < 4:
            continue
        filesystems.append((fields[0], fields[2].lower(), fields[3]))
    if not filesystems:
        die(f"virt-filesystems found no filesystems in {image}")
    return filesystems


def virt_has_file(image: Path, device: str, path: str) -> bool:
    result = subprocess.run(
        ["virt-cat", "-a", str(image), "-m", device, path],
        stdout=DEVNULL,
        stderr=DEVNULL,
        env=guestfs_env(),
    )
    return result.returncode == 0


def virt_has_dir(image: Path, device: str, path: str) -> bool:
    result = subprocess.run(
        ["virt-ls", "-a", str(image), "-m", device, path],
        stdout=DEVNULL,
        stderr=DEVNULL,
        env=guestfs_env(),
    )
    return result.returncode == 0


def virt_list(image: Path, device: str, path: str) -> list[str]:
    result = subprocess.run(
        ["virt-ls", "-a", str(image), "-m", device, path],
        capture_output=True,
        text=True,
        env=guestfs_env(),
    )
    if result.returncode != 0:
        return []
    return result.stdout.splitlines()


def find_root_partition(image: Path, filesystems: list[tuple[str, str, str]]) -> str:
    for device, vfs, _label in filesystems:
        if vfs not in ("ext2", "ext3", "ext4", "xfs", "btrfs"):
            continue
        if virt_has_file(image, device, "/etc/os-release"):
            return device
    die(f"Could not identify root filesystem in {image}")


def find_esp_partition(image: Path, filesystems: list[tuple[str, str, str]]) -> str:
    for device, vfs, _label in filesystems:
        if vfs not in ("vfat", "fat", "msdos"):
            continue
        if virt_has_dir(image, device, "/EFI"):
            return device
    die(f"Could not identify EFI system partition in {image}")


def find_boot_partition(image: Path, filesystems: list[tuple[str, str, str]], root_device: str) -> str | None:
    candidates: list[tuple[str, str, str]] = []
    for device, vfs, label in filesystems:
        if device == root_device:
            continue
        if vfs in ("ext2", "ext3", "ext4", "xfs", "btrfs"):
            candidates.append((device, vfs, label))

    for device, _vfs, label in candidates:
        if label.lower() == "boot":
            return device

    for device, _vfs, _label in candidates:
        names = virt_list(image, device, "/")
        if any(name.startswith("vmlinuz-") for name in names) and "grub" in names:
            return device

    return None


def virt_tar_zst(
    image: Path,
    mount_device: str,
    dest: Path,
    extra_mounts: list[tuple[str, str]] | None = None,
) -> None:
    extra_mounts = extra_mounts or []
    mount_args = ["-m", f"{mount_device}:/"]
    for device, mountpoint in extra_mounts:
        mount_args.extend(["-m", f"{device}:{mountpoint}"])

    mount_desc = ", ".join([mount_device] + [f"{device} at {mountpoint}" for device, mountpoint in extra_mounts])
    log(f"  Exporting {mount_desc} to {dest.name}")
    tar_proc = subprocess.Popen(
        ["virt-tar-out", "--ro", "--no-sync", "-a", str(image), *mount_args, "/", "-"],
        stdout=subprocess.PIPE,
        env=guestfs_env(),
    )
    if tar_proc.stdout is None:
        die("virt-tar-out did not provide stdout")
    zstd_proc = subprocess.Popen(
        ["zstd", "-T0", "-q", "-f", "-o", str(dest), "-"],
        stdin=tar_proc.stdout,
    )
    tar_proc.stdout.close()
    zstd_rc = zstd_proc.wait()
    tar_rc = tar_proc.wait()
    if tar_rc != 0 or zstd_rc != 0:
        die(f"Failed to export {mount_device}: virt-tar-out={tar_rc}, zstd={zstd_rc}")


def prepare_raid_machine_image(raw_image: str, raid_image: str) -> None:
    log("Preparing RAID1 machine image from host-ubuntu2404")
    require_tools([
        "docker", "virt-customize", "virt-filesystems", "virt-cat", "virt-ls",
        "virt-tar-out", "zstd",
    ])

    workdir = TMPDIR / "raid-machine-image"
    workdir.mkdir(parents=True, exist_ok=True)
    disk_gz = workdir / "disk.img.gz"
    disk_raw = workdir / "disk.img"

    log(f"  Extracting /disk/disk.img.gz from {raw_image}")
    docker_copy_from_image(raw_image, "/disk/disk.img.gz", disk_gz)
    with gzip.open(disk_gz, "rb") as src, disk_raw.open("wb") as dst:
        shutil.copyfileobj(src, dst)
    disk_gz.unlink()

    log("  Installing RAID boot dependencies into copied host image")
    run([
        "virt-customize",
        "-a", str(disk_raw),
        "--run-command", "apt-get update",
        "--install", "mdadm,grub-efi-amd64,grub-efi-amd64-bin,shim-signed,qemu-guest-agent",
        "--run-command", "systemctl enable qemu-guest-agent || true",
    ], env=guestfs_env())

    filesystems = virt_filesystems(disk_raw)
    root_part = find_root_partition(disk_raw, filesystems)
    esp_part = find_esp_partition(disk_raw, filesystems)
    boot_part = find_boot_partition(disk_raw, filesystems, root_part)
    boot_mounts = [(boot_part, "/boot")] if boot_part else []
    log(f"  Identified root={root_part} boot={boot_part or 'inline'} esp={esp_part}")

    virt_tar_zst(disk_raw, root_part, workdir / "rootfs.tar.zst", boot_mounts)
    virt_tar_zst(disk_raw, esp_part, workdir / "esp.tar.zst")
    (workdir / "install.yaml").write_text("version: 1\nmode: RAID1\n", encoding="utf-8")
    (workdir / "Containerfile").write_text(textwrap.dedent("""\
        FROM scratch
        COPY rootfs.tar.zst /disk/rootfs.tar.zst
        COPY esp.tar.zst /disk/esp.tar.zst
        COPY install.yaml /disk/install.yaml
    """), encoding="utf-8")

    log(f"  Building {raid_image}")
    run(["docker", "build", "-t", raid_image, "-f", str(workdir / "Containerfile"), str(workdir)])


def assert_raid1_install() -> None:
    if INSTALL_MODE != "RAID1":
        return

    log("Verifying RAID1 install inside guest")
    wait_vm_state("running", timeout=180)
    wait_guest_agent(timeout=300)

    command = textwrap.dedent(f"""\
        set -euo pipefail
        root_src="$(findmnt -n -o SOURCE /)"
        case "$root_src" in
          /dev/md*|/dev/mapper/*) ;;
          *) echo "root filesystem is not on md device: $root_src" >&2; exit 1 ;;
        esac
        test -e {RAID_DISK_A_PATH}
        test -e {RAID_DISK_B_PATH}
        grep -q unbounded-root /etc/mdadm/mdadm.conf
        grep -q '/boot/efi-secondary' /etc/fstab
        grep -q '\\[UU\\]' /proc/mdstat
        mdadm --detail "$root_src" | grep -q 'Raid Level : raid1'
        mdadm --detail "$root_src" | grep -q 'Raid Devices : 2'
        findmnt /boot/efi >/dev/null
        lsblk -o NAME,PATH,SIZE,FSTYPE,TYPE,MOUNTPOINTS
        cat /proc/mdstat
        mdadm --detail "$root_src"
    """)
    exit_code, stdout, stderr = guest_exec(command, timeout=60)
    if stdout:
        sys.stderr.write(stdout)
        sys.stderr.flush()
    if stderr:
        sys.stderr.write(stderr)
        sys.stderr.flush()
    if exit_code != 0:
        die(f"RAID1 guest verification failed with exit code {exit_code}")

    log("  RAID1 guest verification passed")


def run_operation_smoke_suite() -> None:
    log("Running bare-metal MachineOperation smoke suite")

    boot_id = get_node_boot_id(NODE_NAME)

    poweroff = create_machine_operation("smoke-host-poweroff", "HostPowerOff")
    wait_machine_operation_complete(poweroff, timeout=600)
    wait_vm_state("shut off", timeout=180)

    poweron = create_machine_operation("smoke-host-poweron", "HostPowerOn")
    wait_machine_operation_complete(poweron, timeout=600)
    wait_vm_state("running", timeout=180)
    wait_k8s_node(NODE_NAME, timeout=300)
    wait_node_boot_id_changed(NODE_NAME, boot_id, timeout=600)
    boot_id = get_node_boot_id(NODE_NAME)

    reboot = create_machine_operation(
        "smoke-selector-host-reboot",
        "HostReboot",
        machine_ref=None,
        site_selector=SITE,
    )
    wait_machine_operation_complete(reboot, timeout=600)
    wait_vm_state("running", timeout=180)
    wait_k8s_node(NODE_NAME, timeout=300)
    wait_node_boot_id_changed(NODE_NAME, boot_id, timeout=600)


def main() -> None:
    signal.signal(signal.SIGINT, _sigint_handler)
    atexit.register(cleanup)

    log(f"Metalman smoke install mode: {INSTALL_MODE}")
    log(f"Metalman smoke boot protocol: {BOOT_PROTOCOL}")

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
    # Some developer machines default-drop INPUT through UFW.  The VM must
    # reach metalman's DHCP, TFTP, HTTP, and health endpoints on this bridge.
    run(["sudo", "iptables", "-I", "INPUT", "-i", "virbr-smoke", "-j", "ACCEPT"])
    run(["sudo", "iptables", "-I", "FORWARD", "-i", "virbr-smoke", "-j", "ACCEPT"])
    run(["sudo", "iptables", "-I", "FORWARD", "-o", "virbr-smoke", "-j", "ACCEPT"])
    # Docker may insert a raw PREROUTING DROP rule that blocks non-Docker
    # traffic to its container IPs.  Insert an ACCEPT before it so the VM
    # can reach the kind API server.
    run(["sudo", "iptables", "-t", "raw", "-I", "PREROUTING",
         "-i", "virbr-smoke", "-j", "ACCEPT"])

    # Connect the kind container directly to virbr-smoke so that the VM
    # subnet is *directly reachable* from inside the container.  Kindnet
    # adds routes of the form "10.244.x.0/24 via <nodeIP>"; the kernel
    # rejects these when the gateway is only reachable via an indirect
    # route.  A direct L2 link avoids this.
    log("Attaching kind container to virbr-smoke bridge")
    kind_pid = run(
        ["docker", "inspect", "kind-control-plane", "--format", "{{.State.Pid}}"],
        capture_output=True, text=True,
    ).stdout.strip()
    kind_ip = run(
        ["docker", "inspect", "kind-control-plane",
         "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"],
        capture_output=True, text=True,
    ).stdout.strip()
    # Clean up any leftover veth from a previous run.
    run_quiet(["sudo", "ip", "link", "delete", "veth-kind-smoke"], check=False)
    # Create a veth pair: host-side attaches to virbr-smoke, container-side
    # gets an IP on the VM subnet.
    run(["sudo", "ip", "link", "add", "veth-kind-smoke", "type", "veth",
         "peer", "name", "eth-smoke"])
    run(["sudo", "ip", "link", "set", "veth-kind-smoke", "master", "virbr-smoke"])
    run(["sudo", "ip", "link", "set", "veth-kind-smoke", "up"])
    # Move the peer into the kind container's network namespace.
    run(["sudo", "ip", "link", "set", "eth-smoke", "netns", kind_pid])
    run(["sudo", "nsenter", "-t", kind_pid, "-n",
         "ip", "addr", "add", f"{KIND_SMOKE_IP}/24", "dev", "eth-smoke"])
    run(["sudo", "nsenter", "-t", kind_pid, "-n",
         "ip", "link", "set", "eth-smoke", "up"])
    configure_kind_control_plane_node_ip("kind-control-plane", KIND_SMOKE_IP)

    # Kindnet's CONTROL_PLANE_ENDPOINT defaults to "kind-control-plane:6443"
    # which is unresolvable from the bare-metal VM (it's not in Docker's DNS).
    # Patch it to use KIND_SMOKE_IP. The API server's TLS cert SANs (set in
    # kind-smoke-config.yaml) only cover this IP, 127.0.0.1, and localhost.
    # Using the Docker bridge IP (kind_ip) would cause TLS verification failures.
    log("Patching kindnet DaemonSet for VM-reachable control plane endpoint")
    patch = json.dumps({
        "spec": {"template": {"spec": {"containers": [{
            "name": "kindnet-cni",
            "env": [
                {"name": "CONTROL_PLANE_ENDPOINT", "value": f"{KIND_SMOKE_IP}:6443"},
            ],
        }]}}}
    })
    kubectl(["-n", "kube-system", "patch", "daemonset", "kindnet",
             "--type=strategic", "-p", patch])

    log("Creating UEFI VM (powered off, with TPM)")
    ovmf_code, ovmf_vars_template = find_ovmf_firmware()
    log(f"  Using OVMF loader {ovmf_code}")
    ovmf_vars = TMPDIR / "OVMF_VARS.fd"
    shutil.copy2(ovmf_vars_template, ovmf_vars)
    disk_args: list[str] = []
    if INSTALL_MODE == "RAID1":
        for index, (name, serial) in enumerate((("disk-a.qcow2", RAID_DISK_A_SERIAL), ("disk-b.qcow2", RAID_DISK_B_SERIAL))):
            disk_path = TMPDIR / name
            if BOOT_PROTOCOL == "HTTP" and index == 0:
                prepare_http_boot_helper_disk(RAW_IMAGE_NAME, disk_path)
            else:
                run_quiet(["qemu-img", "create", "-f", "qcow2", str(disk_path), "20G"], check=True)
            disk_args.extend(["--disk", f"path={disk_path},format=qcow2,bus=virtio,serial={serial}"])
    else:
        disk_path = TMPDIR / "disk.qcow2"
        if BOOT_PROTOCOL == "HTTP":
            prepare_http_boot_helper_disk(RAW_IMAGE_NAME, disk_path)
        else:
            run_quiet(["qemu-img", "create", "-f", "qcow2", str(disk_path), "20G"], check=True)
        disk_args.extend(["--disk", f"path={disk_path},format=qcow2,bus=virtio"])

    run_quiet([
        "virt-install",
        "--connect", "qemu:///system",
        "--name", VM_NAME, "--ram", "4096", "--vcpus", "2",
        *disk_args,
        "--network", f"network={NET_NAME},mac={MAC_ADDRESS}",
        "--boot", f"uefi,loader={ovmf_code},nvram={ovmf_vars},hd,network",
        "--tpm", "backend.type=emulator,backend.version=2.0",
        "--serial", f"unix,path={SERIAL_SOCK},mode=bind",
        "--channel", f"unix,path={QGA_SOCK},mode=bind,target.type=virtio,target.name=org.qemu.guest_agent.0",
        "--os-variant", "generic",
        "--noautoconsole", "--noreboot", "--import",
    ], check=True)
    run_quiet([*VIRSH, "destroy", VM_NAME])

    log("Starting serial console forwarding")
    console_thread = threading.Thread(
        target=forward_console, args=(SERIAL_SOCK,), daemon=True,
    )
    console_thread.start()

    if BOOT_PROTOCOL == "PXE":
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

    # Start Go builds in the background so they overlap with Kubernetes
    # setup and Docker image builds.  Both targets share the Go build
    # cache, so concurrent compilation is safe and efficient.
    # stdout/stderr are inherited so build output streams to the CI log
    # in real-time.
    log("Rendering machina and net manifests")
    run(["make", "machina-manifests", "net-manifests"], cwd=str(REPO_ROOT))

    log("Building metalman, kubectl-unbounded, and unbounded-agent (parallel)")
    go_builds: list[tuple[str, subprocess.Popen[Any]]] = [
        ("metalman", subprocess.Popen(
            ["go", "build", "-o", str(BINARY), "./cmd/metalman"],
            cwd=str(REPO_ROOT),
        )),
        ("unbounded-agent", subprocess.Popen(
            ["go", "build", "-o", str(AGENT_BINARY), "./cmd/agent"],
            cwd=str(REPO_ROOT),
        )),
        ("kubectl-unbounded", subprocess.Popen(
            ["go", "build", "-o", str(KUBECTL_UNBOUNDED), "./cmd/kubectl-unbounded"],
            cwd=str(REPO_ROOT),
        )),
    ]

    # Kubernetes setup runs while Go builds are in progress.
    log("Cleaning up stale Kubernetes resources")
    run_quiet([KUBECTL, "-n", NODE_NS, "delete", "secret", "bmc-pass"])
    run_quiet([KUBECTL, "delete", f"machineoperations.{API_GROUP}", "--all"])
    run_quiet([KUBECTL, "delete", f"machines.{API_GROUP}", NODE_NAME])
    run_quiet([KUBECTL, "delete", "node", NODE_NAME])
    # Remove stale CRDs so that a version change (e.g. storedVersions
    # referencing an old API version) does not block the fresh apply.
    run_quiet([KUBECTL, "delete", "crd", f"machines.{API_GROUP}"])

    log("Applying deploy manifests (CRDs, namespace, RBAC)")
    kubectl(["apply", "--server-side", "--force-conflicts", "-f", str(REPO_ROOT / "deploy" / "machina" / "rendered" / "01-namespace.yaml")])
    kubectl(["apply", "--server-side", "--force-conflicts", "-f", str(REPO_ROOT / "deploy" / "machina" / "crd")])
    kubectl(["apply", "--server-side", "--force-conflicts", "-f", str(REPO_ROOT / "deploy" / "machina" / "rendered" / "06-metalman-rbac.yaml")])

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

    # Docker images are pre-built
    # by the GitHub Actions workflow using docker/build-push-action with GHA
    # layer caching.  They are already loaded into the local Docker daemon
    # with the correct tags.
    log("Verifying pre-built OCI images are available")
    for name, tag in [("host-ubuntu2404", RAW_IMAGE_NAME),
                      ("netboot", NETBOOT_IMAGE_NAME),
                      ("agent-ubuntu2404", AGENT_IMAGE_NAME)]:
        result = subprocess.run(
            ["docker", "image", "inspect", tag],
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            die(f"Pre-built image {tag} not found in local Docker daemon. "
                "Ensure the workflow builds it before running this script.")
        log(f"  {name} image found: {tag}")

    if INSTALL_MODE == "RAID1":
        prepare_raid_machine_image(RAW_IMAGE_NAME, RAID_IMAGE_NAME)

    # Wait for Go builds (likely already finished during k8s setup).
    for name, proc in go_builds:
        rc = proc.wait()
        if rc != 0:
            die(f"go build {name} failed (exit code {rc})")
    log("  Go builds finished")

    log("Packaging branch-built unbounded-agent")
    ARTIFACT_DIR.mkdir(parents=True, exist_ok=True)
    with tarfile.open(AGENT_TARBALL, "w:gz") as tar:
        tar.add(AGENT_BINARY, arcname="unbounded-agent")

    log("Starting agent download server")
    proc = spawn([
        sys.executable, "-m", "http.server", str(AGENT_DOWNLOAD_PORT),
        "--bind", SERVER_IP, "--directory", str(ARTIFACT_DIR),
    ], TMPDIR / "agent-download.log")
    log(f"  agent download PID={proc.pid}")
    time.sleep(1)
    check_procs()

    log("Pushing OCI images to local registry")
    run(["docker", "push", IMAGE_NAME])
    run(["docker", "push", NETBOOT_IMAGE_NAME])
    run(["docker", "push", AGENT_IMAGE_NAME])

    # Reclaim disk space consumed by Docker build cache.  The host-ubuntu2404
    # build downloads a ~2 GB Ubuntu cloud image and converts it to raw; the
    # intermediate layers are no longer needed once the images are pushed.
    # Only prune the build cache (not running container images) to avoid
    # disturbing the registry container.
    log("Pruning Docker build cache to free disk space")
    run_quiet(["docker", "builder", "prune", "-af"], check=False)

    server_url = apiserver_url()
    log(f"  API server URL: {server_url}")
    pxe_spec: dict[str, Any] = {
        "image": IMAGE_NAME,
        "dhcpLeases": [{
            "mac": MAC_ADDRESS,
            "ipv4": NODE_IP,
            "subnetMask": "255.255.255.0",
            "gateway": GATEWAY,
            "dns": [DNS_SERVER],
        }],
    }
    if BOOT_PROTOCOL == "HTTP":
        pxe_spec["bootProtocol"] = "HTTP"
    else:
        pxe_spec["redfish"] = {
            "url": sushy_url,
            "username": "",
            "deviceID": VM_NAME,
            "passwordRef": {"name": "bmc-pass", "key": "password", "namespace": NODE_NS},
        }

    protonode = {
        "apiVersion": API_VERSION,
        "kind": "Machine",
        "metadata": {
            "name": NODE_NAME,
            "labels": {f"{API_GROUP}/site": SITE},
        },
        "spec": {
            "pxe": pxe_spec,
            "agent": {
                "image": AGENT_IMAGE_NAME_VM,
                "url": AGENT_DOWNLOAD_URL,
            },
            "kubernetes": {
                "nodeLabels": {NODE_LABEL_KEY: NODE_LABEL_VALUE},
            },
        },
    }
    if INSTALL_MODE == "RAID1":
        protonode["spec"]["pxe"]["install"] = {
            "mode": "RAID1",
            "targetDisks": [RAID_DISK_A_PATH, RAID_DISK_B_PATH],
        }
    if BOOT_PROTOCOL == "HTTP":
        protonode["spec"]["operations"] = {
            "rebootCounter": 1,
            "repaveCounter": 1,
        }
    kubectl(["apply", "-f", "-"], input=json.dumps(protonode).encode(),
            stdout=DEVNULL)
    log("  Resources created")

    log("Starting metalman serve-pxe")
    metalman_kubeconfig = TMPDIR / "metalman-controller.kubeconfig"
    write_service_account_kubeconfig(METALMAN_NAMESPACE, METALMAN_CONTROLLER_SA, metalman_kubeconfig)
    metalman_env = [f"METALMAN_APISERVER_URL={server_url}"]
    metalman_env.append(f"KUBECONFIG={metalman_kubeconfig}")

    proc = spawn([
        "sudo", "env", *metalman_env,
        str(BINARY), "serve-pxe", f"--site={SITE}", f"--bind-address={SERVER_IP}",
        f"--cache-dir={CACHE_DIR}",
        f"--serve-url={SERVE_URL}", "--dhcp-interface=virbr-smoke",
        f"--default-netboot-image={NETBOOT_IMAGE_NAME}",
        "--leader-elect-lease-duration=60s",
        "--leader-elect-renew-deadline=40s",
        "--leader-elect-retry-period=5s",
    ], TMPDIR / "serve.log")
    log(f"  serve PID={proc.pid}")

    time.sleep(2)
    check_procs()

    operation_log: Path | None = None
    operation_proc: subprocess.Popen[Any] | None = None
    if BOOT_PROTOCOL == "HTTP":
        log("Starting VM for out-of-band UEFI HTTP boot")
        run([*VIRSH, "start", VM_NAME])
        wait_vm_state("running", timeout=180)
    else:
        log("Triggering HostReplace through kubectl-unbounded")
        operation_log = TMPDIR / "kubectl-host-replace.log"
        operation_proc = run_kubectl_unbounded_operation(
            ["replace", NODE_NAME, "--force", "--ttl=3600"],
            operation_log.name,
        )

    # Log free space so we can correlate disk exhaustion with VM failures.
    df = subprocess.run(["df", "-h", str(TMPDIR)], capture_output=True, text=True)
    log(f"  Host disk after image builds:\n{df.stdout.strip()}")

    log("Waiting for cloud-init to complete...")
    assert_cloud_init_done(timeout=900)

    if operation_proc is not None and operation_log is not None:
        wait_process_success(operation_proc, timeout=900)
        assert_log_contains(operation_log, "Condition CloudInitDone: True/Succeeded")
    else:
        assert_machine_condition("Repaved", "True", "Succeeded", timeout=300)

    log("Waiting for kubelet to join the cluster...")
    wait_k8s_node(NODE_NAME, timeout=900)
    assert_node_ready(NODE_NAME, timeout=720)
    assert_node_label(NODE_NAME, NODE_LABEL_KEY, NODE_LABEL_VALUE)
    assert_raid1_install()

    if BOOT_PROTOCOL == "PXE":
        run_operation_smoke_suite()
    else:
        log("Skipping MachineOperation smoke suite for out-of-band HTTP boot")

    log("")
    log("Smoke test PASSED")


if __name__ == "__main__":
    main()
