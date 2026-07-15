#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

from __future__ import annotations

import atexit
import base64
import json
import os
import re
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
import uuid
from pathlib import Path
from typing import Any

REPO_ROOT = Path(__file__).resolve().parent.parent
TMPDIR = Path(tempfile.mkdtemp())
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
SUBNET = "192.168.200"
SERVER_IP = f"{SUBNET}.1"
NODE_IP = f"{SUBNET}.10"
KIND_SMOKE_IP = f"{SUBNET}.2"  # IP assigned to the kind container on virbr-smoke
GATEWAY = SERVER_IP
DNS_SERVER = "8.8.8.8"
MAC_ADDRESS = "52:54:00:aa:bb:01"
REDFISH_PORT = int(os.environ.get("SMOKE_REDFISH_PORT", "8443"))
HTTP_PORT = 8880
AGENT_DOWNLOAD_PORT = 8881
CACHE_DIR = TMPDIR / "cache"
ARTIFACT_DIR = TMPDIR / "artifacts"
SERVE_URL = f"http://{SERVER_IP}:{HTTP_PORT}"
AGENT_TARBALL = ARTIFACT_DIR / "unbounded-agent-linux-amd64.tar.gz"
AGENT_DOWNLOAD_URL = f"http://{SERVER_IP}:{AGENT_DOWNLOAD_PORT}/{AGENT_TARBALL.name}"
REGISTRY_PORT = 5555
REGISTRY_CONTAINER = "unbounded-smoke-registry"
IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/host-ubuntu2404:smoke"
NETBOOT_IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/netboot:smoke"
AGENT_IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/agent-ubuntu2404:smoke"
# The agent runs inside a VM on an isolated bridge network. "localhost" inside
# the VM resolves to the VM's own loopback, not the host.  Use the host's
# bridge IP so the VM can reach the registry over the virtual network.
AGENT_IMAGE_NAME_VM = f"{SERVER_IP}:{REGISTRY_PORT}/unbounded/agent-ubuntu2404:smoke"
BINARY = REPO_ROOT / "bin" / "metalman"
AGENT_BINARY = REPO_ROOT / "bin" / "unbounded-agent"
KUBECTL_UNBOUNDED = REPO_ROOT / "bin" / "kubectl-unbounded"
REDFISH_FIXTURE_BINARY = REPO_ROOT / "bin" / "metalman-redfish-fixture"
BRIDGE = "virbr-smoke"
# The fixture owns the VM state directory: TPM state and the Cloud Hypervisor
# api/serial unix sockets all live here. Cloud Hypervisor exposes exactly one
# socket-backed character device, the legacy serial port (ttyS0), which it
# wires to console.sock. That single socket carries the kernel console and the
# guest image's autologin root shell, which is the automation channel the tests
# drive (via a marker-based protocol that tolerates interleaved kernel output).
STATE_DIR = TMPDIR / "vmstate"
DISK = TMPDIR / "disk.qcow2"
# Custom Cloud Hypervisor OVMF firmware blob (built by
# hack/scripts/build-cloudhv-firmware.sh). Cloud Hypervisor consumes a single
# flat firmware via --firmware; there is no separate NVRAM/vars file.
FIRMWARE = str(REPO_ROOT / "bin" / "cloudhv-firmware" / "CLOUDHV.fd")
# The VM disk starts empty; the VM PXE-boots, installs the OS onto it, then
# reboots into the installed image.
SERIAL_SOCK = STATE_DIR / "console.sock"
REDFISH_URL = f"https://127.0.0.1:{REDFISH_PORT}"
REDFISH_USER = ""
REDFISH_PASS = "smoke"
# The nspawn machine name used by the agent (must match the constant in
# cmd/agent/internal/goalstates/constants.go - NSpawnMachineKube1).
NSPAWN_MACHINE = "kube1"

KUBECTL = "kubectl"
DEVNULL = subprocess.DEVNULL

_procs: list[subprocess.Popen[Any]] = []


def log(msg: str) -> None:
    print(f"==> {msg}", file=sys.stderr)


def die(msg: str) -> None:
    print(f"FAIL: {msg}", file=sys.stderr)
    try:
        collect_debug_logs()
    except Exception as e:
        log(f"  (debug log collection failed: {e})")
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


class _SerialConsole:
    """Command driver for the guest's autologin root shell on ttyS0.

    Cloud Hypervisor exposes exactly one socket-backed character device, the
    legacy serial port, which the fixture wires to SERIAL_SOCK. The guest image
    runs an autologin root getty there, giving a credential-free shell. The
    kernel console shares that port, so its output interleaves with the shell.

    A single background reader thread owns the one connection: it tees every
    byte to stderr (so boot progress and kernel messages are visible even
    before a shell exists) and appends to a shared buffer that exec() searches.
    This lets one connection serve both the human console tee and the command
    channel, which the single serial socket requires.

    Commands are shipped base64-encoded and run via ``eval`` so the shell's
    input echo never contains the result markers (which would otherwise create
    false matches). stdout/stderr/exit-code are captured to files and emitted
    base64-encoded between unique per-command sentinels, so binary output,
    newlines, and interleaved kernel spew never confuse the parser.
    """

    def __init__(self, sock_path: Path) -> None:
        self.sock_path = sock_path
        self.conn: socket.socket | None = None
        self.buf = b""
        self.buf_lock = threading.Lock()
        self.cmd_lock = threading.Lock()
        threading.Thread(target=self._reader, daemon=True).start()

    def _reader(self) -> None:
        """Own the single console connection: tee to stderr and buffer."""
        while True:
            conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            try:
                conn.connect(str(self.sock_path))
            except (FileNotFoundError, ConnectionRefusedError, OSError):
                conn.close()
                time.sleep(1)
                continue

            with self.buf_lock:
                self.conn = conn
                self.buf = b""

            try:
                while True:
                    data = conn.recv(65536)
                    if not data:
                        break
                    sys.stderr.buffer.write(data)
                    sys.stderr.buffer.flush()
                    with self.buf_lock:
                        self.buf += data
                        if len(self.buf) > 4 * 1024 * 1024:
                            self.buf = self.buf[-1024 * 1024:]
            except OSError:
                pass
            finally:
                with self.buf_lock:
                    if self.conn is conn:
                        self.conn = None
                conn.close()

            # Socket closed - VM probably rebooted or powered off.  Retry.
            time.sleep(1)

    def _wait_conn(self, deadline: float) -> socket.socket:
        while True:
            with self.buf_lock:
                if self.conn is not None:
                    return self.conn
            if time.monotonic() > deadline:
                raise ConnectionError("console not connected")
            time.sleep(0.1)

    def exec(self, command: str, timeout: int = 30) -> tuple[int, str, str]:
        gid = uuid.uuid4().hex
        script = (
            f"{{ {command} ; }} >/tmp/ge_{gid}.out 2>/tmp/ge_{gid}.err; rc=$?; "
            f'echo "GEO{gid}:$(base64 -w0 /tmp/ge_{gid}.out)"; '
            f'echo "GEE{gid}:$(base64 -w0 /tmp/ge_{gid}.err)"; '
            f'echo "GER{gid}:$rc"; '
            f"rm -f /tmp/ge_{gid}.out /tmp/ge_{gid}.err"
        )
        payload = base64.b64encode(script.encode("utf-8")).decode("ascii")
        line = f'eval "$(echo {payload} | base64 -d)"\n'
        end_re = re.compile(rf"GER{gid}:(\d+)".encode("ascii"))

        with self.cmd_lock:
            deadline = time.monotonic() + timeout
            for attempt in range(2):
                try:
                    conn = self._wait_conn(deadline)
                    conn.sendall(line.encode("utf-8"))
                    while True:
                        with self.buf_lock:
                            m = end_re.search(self.buf)
                            if m:
                                return self._parse_locked(gid, m)
                        if time.monotonic() > deadline:
                            raise TimeoutError(
                                f"guest command did not complete within {timeout}s"
                            )
                        time.sleep(0.05)
                except (OSError, ConnectionError) as e:
                    # The VM may have rebooted or power-cycled; retry once.
                    if attempt == 1:
                        raise RuntimeError(f"console exec failed: {e}") from e
                    time.sleep(0.5)
            raise RuntimeError("console exec failed")

    def _parse_locked(self, gid: str, end_match: Any) -> tuple[int, str, str]:
        """Parse captured output. Caller must hold buf_lock."""
        exit_code = int(end_match.group(1))
        out = self._extract_locked(f"GEO{gid}:")
        err = self._extract_locked(f"GEE{gid}:")
        # Drop everything up to and including the end marker so the next command
        # starts from a clean buffer.
        self.buf = self.buf[end_match.end():]
        return exit_code, out, err

    def _extract_locked(self, marker: str) -> str:
        mre = re.compile(re.escape(marker).encode("ascii") + rb"([A-Za-z0-9+/=]*)")
        m = mre.search(self.buf)
        if not m:
            return ""
        try:
            return base64.b64decode(m.group(1)).decode("utf-8", errors="replace")
        except Exception:
            return ""


_console = _SerialConsole(SERIAL_SOCK)


def guest_exec(command: str, timeout: int = 30) -> tuple[int, str, str]:
    """Execute a command inside the VM over the ttyS0 autologin shell.

    Returns (exit_code, stdout, stderr). Talks to the fixture's serial console
    (SERIAL_SOCK); requires the guest image's autologin root getty on ttyS0.
    """
    return _console.exec(command, timeout=timeout)


def collect_debug_logs() -> None:
    """Dump kubelet and agent debug information over the automation console.

    Best-effort: failures are logged but do not abort the test.
    """
    log("Collecting debug logs from VM via automation console...")
    commands = [
        # Network diagnostics - must come first to diagnose download hangs.
        ("resolv.conf", "cat /etc/resolv.conf"),
        ("ip addr", "ip -4 addr show"),
        ("ip route", "ip route show"),
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
    # These commands survive the automation console dying inside the VM, which
    # is critical because the in-guest collectors above frequently fail by the
    # time the test gives up.
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


def clean_stale() -> None:
    # Kill any leftover Redfish fixture from a previous run before touching the
    # bridge it owns.
    run_quiet(["sudo", "pkill", "-f", "metalman-redfish-fixture"])
    # Kill any leftover cloud-hypervisor guest from a previous run.
    run_quiet(["sudo", "pkill", "-f", f"cloud-hypervisor.*{VM_NAME}"])
    run_quiet(["sudo", "pkill", "-f", f"cloud-hypervisor.*{STATE_DIR}"])
    run_quiet(["sudo", "pkill", "-f", f"swtpm.*{STATE_DIR}"])
    time.sleep(1)
    # Remove stale bridge/tap/veth left behind by a previous run. The fixture
    # recreates the bridge on startup.
    run_quiet(["sudo", "ip", "link", "delete", BRIDGE])
    run_quiet(["sudo", "ip", "link", "delete", "veth-kind-smoke"])
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
    clean_stale()
    # Remove the raw PREROUTING rule added for VM to kind connectivity. The
    # fixture removes its own FORWARD/MASQUERADE rules on shutdown, but the
    # FORWARD deletes below stay as best-effort cleanup for older runs.
    # Use check=False so these are best-effort (rules may not exist if setup
    # failed before they were inserted).
    run_quiet(["sudo", "iptables", "-D", "FORWARD", "-i", BRIDGE, "-j", "ACCEPT"], check=False)
    run_quiet(["sudo", "iptables", "-D", "FORWARD", "-o", BRIDGE, "-j", "ACCEPT"], check=False)
    run_quiet(["sudo", "iptables", "-t", "raw", "-D", "PREROUTING",
               "-i", BRIDGE, "-j", "ACCEPT"], check=False)
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


def configure_kind_kube_proxy_apiserver(api_server: str) -> None:
    """Make newly scheduled kind kube-proxy pods use the VM-reachable API URL."""
    log(f"Configuring kind kube-proxy API server as {api_server}")
    result = run(
        [KUBECTL, "-n", "kube-system", "get", "configmap", "kube-proxy", "-o", "json"],
        capture_output=True,
        text=True,
    )
    config_map = json.loads(result.stdout)
    kubeconfig = config_map["data"]["kubeconfig.conf"]
    lines = kubeconfig.splitlines()
    server_lines = [index for index, line in enumerate(lines) if line.lstrip().startswith("server:")]
    if len(server_lines) != 1:
        die(f"Expected one kube-proxy API server entry, got {len(server_lines)}")
    index = server_lines[0]
    indentation = lines[index][:-len(lines[index].lstrip())]
    lines[index] = f"{indentation}server: {api_server}"
    config_map["data"]["kubeconfig.conf"] = "\n".join(lines) + "\n"
    run(
        [KUBECTL, "replace", "-f", "-"],
        input=json.dumps(config_map),
        text=True,
    )


def _probe_vm_network() -> None:
    """Run quick network diagnostics inside the VM via the automation console."""
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


def redfish_power_state() -> str:
    """Return the VM power state ("On"/"Off") via the fixture's Redfish API."""
    import ssl
    import urllib.request

    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE

    req = urllib.request.Request(
        f"{REDFISH_URL}/redfish/v1/Systems/{VM_NAME}",
        headers=_redfish_auth_header(),
    )
    with urllib.request.urlopen(req, timeout=10, context=ctx) as resp:
        body = json.loads(resp.read().decode("utf-8"))
    return str(body.get("PowerState", ""))


def _redfish_auth_header() -> dict[str, str]:
    if not REDFISH_USER and not REDFISH_PASS:
        return {}
    token = base64.b64encode(f"{REDFISH_USER}:{REDFISH_PASS}".encode()).decode()
    return {"Authorization": f"Basic {token}"}


def _vm_is_running() -> bool:
    """Return True if the VM is powered on."""
    try:
        return redfish_power_state() == "On"
    except Exception:
        return False


def wait_vm_state(expected: str, timeout: int = 300) -> None:
    """Wait for the VM power state to reach *expected* ("On" or "Off").

    For backwards compatibility with the previous libvirt-based callers,
    "running" is treated as "On" and "shut off" as "Off".
    """
    target = {"running": "On", "shut off": "Off"}.get(expected, expected)
    log(f"  Waiting for VM '{VM_NAME}' power state {target!r}...")
    last_state = ""
    for elapsed in range(timeout):
        check_procs()
        try:
            state = redfish_power_state()
        except Exception as e:  # noqa: BLE001 - transient during power cycles.
            state = f"(unavailable: {e})"
        if state == target:
            log(f"  VM '{VM_NAME}' power state is {state!r}")
            return
        if elapsed > 0 and elapsed % 15 == 0 and state != last_state:
            last_state = state
            log(f"    ({elapsed}s) VM power state={state or 'unknown'}")
        time.sleep(1)
    die(f"Timed out waiting for VM '{VM_NAME}' power state {target!r}")


def wait_guest_agent(timeout: int = 300) -> None:
    """Wait until the guest OS responds through the ttyS0 automation console."""
    log("  Waiting for guest automation console to respond...")
    for elapsed in range(timeout):
        check_procs()
        try:
            exit_code, _, _ = guest_exec("true", timeout=10)
            if exit_code == 0:
                log("  Guest automation console is responsive")
                return
        except (RuntimeError, TimeoutError, subprocess.TimeoutExpired, OSError):
            pass
        if elapsed > 0 and elapsed % 15 == 0:
            log(f"    ({elapsed}s) guest console not responsive yet")
        time.sleep(1)
    die("Timed out waiting for guest automation console")


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


def operation_targets_node(op: dict[str, Any]) -> bool:
    if op.get("spec", {}).get("machineRef") == NODE_NAME:
        return True
    for target in op.get("status", {}).get("targets", []):
        if target.get("machineRef") == NODE_NAME:
            return True
    return False


def find_host_replace_operation(items: list[dict[str, Any]]) -> dict[str, Any] | None:
    candidates = [
        op for op in items
        if op.get("spec", {}).get("operationKind") == "HostReplace" and operation_targets_node(op)
    ]
    if not candidates:
        return None
    return sorted(candidates, key=lambda op: (
        op.get("metadata", {}).get("creationTimestamp", ""),
        op.get("metadata", {}).get("name", ""),
    ))[0]


def assert_cloud_init_done(timeout: int = 900) -> None:
    """Assert the HostReplace operation's CloudInitDone condition is True/Succeeded.

    Called before waiting for the Kubernetes Node to appear because
    cloud-init must finish before the kubelet can join the cluster.
    Fails fast if the condition transitions to Failed so that the
    smoke test does not wait for the full node-join timeout.
    """
    log(f"  Waiting for HostReplace MachineOperation CloudInitDone condition for '{NODE_NAME}'...")
    for elapsed in range(timeout):
        check_procs()
        result = subprocess.run(
            [KUBECTL, "get", f"machineoperations.{API_GROUP}", "-o", "json"],
            capture_output=True, text=True,
        )
        op_name = ""
        phase = ""
        status = ""
        reason = ""
        message = ""
        if result.returncode == 0:
            try:
                op = find_host_replace_operation(json.loads(result.stdout).get("items", []))
                if op is not None:
                    op_name = op.get("metadata", {}).get("name", "")
                    op_status = op.get("status", {})
                    phase = op_status.get("phase", "")
                    message = op_status.get("message", "")
                    for c in op_status.get("conditions", []):
                        if c.get("type") == "CloudInitDone":
                            status = c.get("status", "")
                            reason = c.get("reason", "")
                            message = c.get("message", "")
                            break
            except (json.JSONDecodeError, KeyError):
                pass

        if phase == "Failed":
            die(f"HostReplace MachineOperation '{op_name}' failed: {message}")
        if status == "True":
            if reason != "Succeeded":
                die(f"CloudInitDone condition is True but reason is {reason!r}, expected 'Succeeded'")
            log(f"  MachineOperation '{op_name}' CloudInitDone condition is True/Succeeded")
            return
        if status == "False" and reason == "Failed":
            die(f"Cloud-init failed: {message}")
        if elapsed > 0 and elapsed % 30 == 0:
            if op_name:
                log(f"    ({elapsed}s) MachineOperation '{op_name}' phase={phase or 'empty'} CloudInitDone status={status or 'not set'} reason={reason or 'not set'}")
            else:
                log(f"    ({elapsed}s) HostReplace MachineOperation not found yet")
        time.sleep(1)
    die(f"Timed out waiting for CloudInitDone condition on HostReplace MachineOperation for '{NODE_NAME}'")


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

    log("Cleaning up stale resources")
    clean_stale()

    log("Preparing VM state directory and disk")
    STATE_DIR.mkdir(parents=True, exist_ok=True)
    os.chmod(STATE_DIR, 0o755)
    run_quiet(["qemu-img", "create", "-f", "qcow2", str(DISK), "20G"], check=True)

    log("Generating Redfish fixture TLS certificate")
    run_quiet([
        "openssl", "req", "-x509", "-newkey", "rsa:2048",
        "-keyout", str(TMPDIR / "redfish.key"),
        "-out", str(TMPDIR / "redfish.crt"),
        "-days", "1", "-nodes",
        "-subj", "/CN=metalman-redfish-fixture",
        "-addext", "subjectAltName=IP:127.0.0.1",
    ], check=True)

    log("Building metalman-redfish-fixture")
    run(["go", "build", "-o", str(REDFISH_FIXTURE_BINARY),
         "./hack/metalman-redfish-fixture"], cwd=str(REPO_ROOT))

    # The fixture owns the VM and its networking: it creates the bridge,
    # assigns SERVER_IP, installs outbound NAT, and launches qemu on power-on.
    # It must start before we attach the kind container to the bridge.
    log("Starting recording Redfish fixture (creates bridge and NAT)")
    proc = spawn([
        "sudo", "-E",
        str(REDFISH_FIXTURE_BINARY),
        "--domain", VM_NAME, "--mac", MAC_ADDRESS,
        "--bind", "127.0.0.1", "--port", str(REDFISH_PORT),
        "--cert", str(TMPDIR / "redfish.crt"),
        "--key", str(TMPDIR / "redfish.key"),
        "--record", str(TMPDIR / "redfish.jsonl"),
        "--username", REDFISH_USER, "--password", REDFISH_PASS,
        "--disk", str(DISK),
        "--state-dir", str(STATE_DIR),
        "--firmware", FIRMWARE,
        "--bridge", BRIDGE,
        "--bridge-address", SERVER_IP,
        "--manage-boot-order",
    ], TMPDIR / "redfish.log")
    log(f"  Redfish fixture PID={proc.pid}")

    log(f"Waiting for bridge {BRIDGE} to appear")
    for _ in range(60):
        if subprocess.run(["ip", "link", "show", BRIDGE],
                          capture_output=True).returncode == 0:
            break
        check_procs()
        time.sleep(1)
    else:
        die(f"bridge {BRIDGE} did not appear")
    check_procs()

    # Docker may insert a raw PREROUTING DROP rule that blocks non-Docker
    # traffic to its container IPs.  Insert an ACCEPT before it so the VM
    # can reach the kind API server.  (The fixture already installs the
    # FORWARD ACCEPT and MASQUERADE rules for the bridge subnet.)
    log("Adding iptables raw PREROUTING rule for VM to kind connectivity")
    run(["sudo", "iptables", "-t", "raw", "-I", "PREROUTING",
         "-i", BRIDGE, "-j", "ACCEPT"])

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
    run(["sudo", "ip", "link", "set", "veth-kind-smoke", "master", BRIDGE])
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
    configure_kind_kube_proxy_apiserver(f"https://{KIND_SMOKE_IP}:6443")

    # The _SerialConsole reader thread (started at module import) already tees
    # the guest console to stderr as soon as the fixture creates the socket, so
    # no separate forwarder is needed here.

    redfish_url = REDFISH_URL
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
    for name, tag in [("host-ubuntu2404", IMAGE_NAME),
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
                    "url": redfish_url,
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
                "url": AGENT_DOWNLOAD_URL,
            },
            "kubernetes": {
                "nodeLabels": {NODE_LABEL_KEY: NODE_LABEL_VALUE},
            },
        },
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

    wait_process_success(operation_proc, timeout=900)
    assert_log_contains(operation_log, "Condition CloudInitDone: True/Succeeded")

    log("Waiting for kubelet to join the cluster...")
    wait_k8s_node(NODE_NAME, timeout=900)
    assert_node_ready(NODE_NAME, timeout=720)
    assert_node_label(NODE_NAME, NODE_LABEL_KEY, NODE_LABEL_VALUE)

    run_operation_smoke_suite()

    log("")
    log("Smoke test PASSED")


if __name__ == "__main__":
    main()
