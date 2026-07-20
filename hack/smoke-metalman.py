#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Metalman PXE smoke test driven against a remote playpen VM.

This test provisions a bare-metal Machine end to end (PXE network boot,
cloud-init, kubelet join, then power operations) WITHOUT running any virtual
machine on the host executing this script. The guest is a cloud-hypervisor VM
that lives inside a playpen demo pod on a remote KVM-capable node, reachable
over a userspace VXLAN-over-WireGuard overlay that `playpen up` establishes.

Two clusters are involved:

  * A local KIND cluster is the metalman control plane. The Machine CR,
    MachineOperations, and the joined smoke-node Node all live here. It is
    addressed by an explicit kubectl context (--kind-context, default
    "kind-kind"). All kubectl / kubectl-unbounded / metalman operations target
    it.

  * The current kubectl context (--context) is a real cluster that has a
    KVM-capable node and the unbounded-net gateway mesh. playpen runs its
    VM-hosting pod there. ONLY playpen talks to this cluster.

Networking is stitched together entirely by playpen's userspace overlay:

  * The overlay client IP (172.31.99.2) is where metalman's DHCP next-server,
    TFTP server, HTTP serve-url, the OCI registry, the agent download server,
    and the KIND API server all appear to the guest. playpen --forward rules
    map guest connections to those overlay ports back to 127.0.0.1 on this
    host, where the real services bind.

  * metalman runs in DHCP relay mode (empty --dhcp-interface). playpen relays
    the guest's DHCP to metalman and metalman advertises 172.31.99.2 as the
    next-server via the new --advertise-ip flag while binding 127.0.0.1.

  * The guest's Redfish BMC is served inside the pod; playpen exposes it at
    https://127.0.0.1:8443 (localforward), which metalman drives for power and
    boot control.

Run as a normal user that has passwordless sudo. playpen runs as the invoking
user (so its Kubernetes client uses the normal kubeconfig / --context); metalman
runs under sudo because its TFTP server binds the privileged port 69, and
playpen is granted CAP_NET_BIND_SERVICE (via setcap) so it can bind the
privileged DHCP relay port 67 without sudo.

The playpen pod image (which must include the guest-disk support this test
relies on) is pulled by the remote pod, so it must be published to a registry
the target cluster can pull from. Pass its fully-qualified reference with
--pod-image.
"""

from __future__ import annotations

import argparse
import atexit
import json
import os
import signal
import shutil
import ssl
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.request
from base64 import b64encode
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

# Overlay addressing established by `playpen up`. The client end of the overlay
# (this host) is OVERLAY_CLIENT_IP; every service the guest must reach is
# advertised there and forwarded back to 127.0.0.1 on this host. OVERLAY_NODE_IP
# is the static lease the guest receives via metalman's DHCP.
OVERLAY_CLIENT_IP = "172.31.99.2"
# The pod (remote) end of the overlay. playpen's in-pod netboot HTTP reverse
# proxy listens here and forwards to the client IP, so the guest bootloader
# fetches the large netboot payload over the fast pod<->guest LAN hop while the
# pod re-originates to the client over the overlay using its real kernel TCP.
OVERLAY_POD_IP = "172.31.99.1"
OVERLAY_NODE_IP = "172.31.99.10"
OVERLAY_MASK = "255.255.255.0"
# The guest's default gateway is the pod end of the overlay. The pod enables IP
# forwarding and masquerades overlay-sourced traffic out its uplink, so the guest
# reaches the internet (for example dl.k8s.io and github.com for Kubernetes, CRI
# and CNI artifacts) via the pod's server-side NAT. Every in-overlay service the
# guest needs (172.31.99.2:<port> forwards) is on-link within OVERLAY_MASK, so
# routing default traffic through the pod does not affect them.
OVERLAY_GATEWAY = OVERLAY_POD_IP
# Use the DNS server the host node provides: the in-cluster resolver (kube-dns
# ClusterIP). Public resolvers such as 8.8.8.8 and the Azure platform DNS
# (168.63.129.16) are unreachable from node pods, and the cluster resolver
# returns IPv4 A records for the artifact hosts (dl.k8s.io, github.com). The
# guest reaches it through the pod: kube-proxy DNATs the ClusterIP in the pod
# netns PREROUTING for the forwarded, masqueraded guest traffic.
DNS_SERVER = "10.0.0.10"
# The guest NIC MAC. Must match playpen's --vm-mac (its default).
MAC_ADDRESS = "52:54:00:12:34:56"

# playpen's in-pod Redfish server, exposed locally by playpen's localforward.
REDFISH_PORT = 8443
REDFISH_URL = f"https://127.0.0.1:{REDFISH_PORT}"
REDFISH_USERNAME = "admin"
REDFISH_PASSWORD = "password"
REDFISH_DEVICE_ID = "1"

HTTP_PORT = 8880
AGENT_DOWNLOAD_PORT = 8881
REGISTRY_PORT = 5555
# metalman TFTP binds this privileged port on the host loopback; playpen's
# TFTP proxy forwards the guest's overlay requests here.
TFTP_PORT = 69
# playpen binds this privileged DHCP relay port on the host loopback; metalman
# unicasts its DHCP replies to it. metalman itself listens on a separate
# unprivileged port to avoid a bind conflict (it has no SO_REUSEADDR).
DHCP_RELAY_PORT = 67
METALMAN_DHCP_PORT = 6767

CACHE_DIR = TMPDIR / "cache"
ARTIFACT_DIR = TMPDIR / "artifacts"

# metalman binds loopback but advertises the pod-side netboot proxy IP to the
# guest as the serve-url. grub and the installer fetch vmlinuz/initrd/init.cpio/
# disk.img.gz from the pod proxy (LAN hop), which forwards to the client IP.
SERVE_URL = f"http://{OVERLAY_POD_IP}:{HTTP_PORT}"
AGENT_TARBALL = ARTIFACT_DIR / "unbounded-agent-linux-amd64.tar.gz"
AGENT_DOWNLOAD_URL = f"http://{OVERLAY_CLIENT_IP}:{AGENT_DOWNLOAD_PORT}/{AGENT_TARBALL.name}"

REGISTRY_CONTAINER = "unbounded-smoke-registry"
# Push targets use localhost so the host Docker daemon (and metalman) reach the
# registry directly.
IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/host-ubuntu2404:smoke"
NETBOOT_IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/netboot:smoke"
AGENT_IMAGE_NAME = f"localhost:{REGISTRY_PORT}/unbounded/agent-ubuntu2404:smoke"
# The agent runs inside the remote guest. It reaches the registry over the
# overlay via the client IP, forwarded back to the host registry.
AGENT_IMAGE_NAME_VM = f"{OVERLAY_CLIENT_IP}:{REGISTRY_PORT}/unbounded/agent-ubuntu2404:smoke"

BINARY = REPO_ROOT / "bin" / "metalman"
AGENT_BINARY = REPO_ROOT / "bin" / "unbounded-agent"
KUBECTL_UNBOUNDED = REPO_ROOT / "bin" / "kubectl-unbounded"
PLAYPEN = REPO_ROOT / "bin" / "playpen"

# The API server URL the guest uses (kindnet, kube-proxy, and the joining
# kubelet). The guest reaches the KIND API server over the overlay, forwarded
# to the KIND API server's loopback port on this host.
GUEST_APISERVER_URL = f"https://{OVERLAY_CLIENT_IP}:6443"

# The nspawn machine name used by the agent (must match the constant in
# cmd/agent/internal/goalstates/constants.go - NSpawnMachineKube1).
NSPAWN_MACHINE = "kube1"

# kubectl targeting the KIND control-plane cluster. Set in main() once the
# --kind-context argument is known.
KUBECTL: list[str] = ["kubectl"]
# The playpen target context (the real cluster). Set in main().
REAL_CONTEXT = ""
# The playpen pod image. Set in main().
POD_IMAGE = ""
# The node to pin the playpen pod on. Set in main().
POD_NODE = ""
# Host loopback port the KIND API server listens on. Set in main().
KIND_APISERVER_PORT = 0
# Path to a standalone kubeconfig scoped to the KIND context. Written in main().
# kubectl-unbounded has no --context flag, so it is pointed at this file via
# --kubeconfig to target the KIND control plane.
KIND_KUBECONFIG: Path | None = None

DEVNULL = subprocess.DEVNULL

# Wall-clock start, used to prefix every log line with elapsed time so slow
# phases are easy to spot when optimizing the smoke test feedback loop.
START_TIME = time.monotonic()

_procs: list[subprocess.Popen[Any]] = []


def log(msg: str) -> None:
    elapsed = int(time.monotonic() - START_TIME)
    print(f"==> [{elapsed // 60:d}m{elapsed % 60:02d}s] {msg}", file=sys.stderr)


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


def collect_debug_logs() -> None:
    """Dump Kubernetes-side debug information from the KIND control plane.

    Best-effort: failures are logged but do not abort the test. There is no
    in-guest collection because this test has no guest-exec channel; the guest
    is remote and only reachable over the overlay via Redfish and network boot.
    """
    log("Collecting Kubernetes debug logs from the KIND control plane...")
    k8s_commands = [
        ("kubectl describe node", [*KUBECTL, "describe", "node", NODE_NAME]),
        ("kubectl get pods -A", [*KUBECTL, "get", "pods", "-A", "-o", "wide"]),
        ("kubectl get events", [
            *KUBECTL, "get", "events", "-A", "--sort-by=.lastTimestamp",
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
            [*KUBECTL, "-n", "kube-system", "get", "pods",
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
                [*KUBECTL, "-n", "kube-system", "get", "pods",
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
                 [*KUBECTL, "-n", "kube-system", "describe", "pod", pod]),
                (f"kubectl logs {pod}",
                 [*KUBECTL, "-n", "kube-system", "logs", pod,
                  "--all-containers=true", "--tail=200"]),
                (f"kubectl logs --previous {pod}",
                 [*KUBECTL, "-n", "kube-system", "logs", pod,
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


def lean_teardown() -> None:
    """Best-effort teardown of host-side resources (no libvirt, no iptables)."""
    # Ask playpen to delete the pod, overlay, Site, and temporary Node it
    # created. Terminating the `playpen up` process (below, in cleanup) also
    # triggers its own cleanup, but `down` is a belt-and-suspenders backstop.
    if PLAYPEN.exists() and REAL_CONTEXT:
        run_quiet([str(PLAYPEN), "down", "--context", REAL_CONTEXT], check=False)
    # Kill any leftover metalman serve-pxe from a previous run. Use the binary
    # path to avoid matching this script (smoke-metalman.py).
    run_quiet(["sudo", "pkill", "-f", "bin/metalman"], check=False)
    # Kill any leftover artifact download server from a previous run.
    run_quiet(["pkill", "-f", f"http.server {AGENT_DOWNLOAD_PORT}"], check=False)
    # Stop and remove leftover local registry container.
    run_quiet(["docker", "rm", "-f", REGISTRY_CONTAINER], check=False)
    # Delete stale leader-election leases so new processes acquire immediately.
    run_quiet([*KUBECTL, "-n", METALMAN_NAMESPACE, "delete", "lease",
               f"metalman-{SITE}"], check=False)
    # Remove the loopback alias used for proxy source-IP preservation.
    run_quiet(["sudo", "-n", "ip", "addr", "del", f"{OVERLAY_NODE_IP}/32", "dev", "lo"], check=False)


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
            proc.wait(timeout=10)
        except (OSError, subprocess.TimeoutExpired):
            try:
                os.killpg(proc.pid, signal.SIGKILL)
                proc.wait(timeout=5)
            except (OSError, subprocess.TimeoutExpired):
                pass
    lean_teardown()
    shutil.rmtree(TMPDIR, ignore_errors=True)


def _sigint_handler(sig: int, frame: Any) -> None:
    cleanup()
    sys.exit(1)


def kubectl(args: list[str], **kw: Any) -> subprocess.CompletedProcess[str]:
    return run([*KUBECTL, *args], **kw)


def write_service_account_kubeconfig(namespace: str, service_account: str, path: Path) -> None:
    token = run(
        [*KUBECTL, "-n", namespace, "create", "token", service_account, "--duration=2h"],
        capture_output=True,
        text=True,
    ).stdout.strip()
    if not token:
        die(f"Failed to create token for ServiceAccount {namespace}/{service_account}")

    raw_config = run(
        [*KUBECTL, "config", "view", "--raw", "--minify", "-o", "json"],
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


def kind_apiserver_port() -> int:
    """Return the loopback port the KIND API server listens on."""
    result = run(
        [
            *KUBECTL, "config", "view", "--minify",
            "-o", "jsonpath={.clusters[0].cluster.server}",
        ],
        capture_output=True,
        text=True,
    )
    url = result.stdout.strip()
    from urllib.parse import urlparse
    parsed = urlparse(url)
    if parsed.hostname not in ("127.0.0.1", "localhost", "::1"):
        die(f"Expected KIND API server on loopback, got {url!r}. "
            "This test requires a local kind cluster for --kind-context.")
    port = parsed.port or 6443
    log(f"  KIND API server loopback port: {port}")
    return port


def configure_kind_kube_proxy_apiserver(api_server: str) -> None:
    """Make newly scheduled kind kube-proxy pods use the VM-reachable API URL."""
    log(f"Configuring kind kube-proxy API server as {api_server}")
    result = run(
        [*KUBECTL, "-n", "kube-system", "get", "configmap", "kube-proxy", "-o", "json"],
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
        [*KUBECTL, "replace", "-f", "-"],
        input=json.dumps(config_map),
        text=True,
    )


def _redfish_opener() -> urllib.request.OpenerDirector:
    """Return a urllib opener that trusts playpen's self-signed Redfish cert."""
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return urllib.request.build_opener(urllib.request.HTTPSHandler(context=ctx))


def redfish_power_state() -> str | None:
    """Return the guest PowerState via playpen's forwarded Redfish, or None."""
    creds = b64encode(f"{REDFISH_USERNAME}:{REDFISH_PASSWORD}".encode()).decode()
    req = urllib.request.Request(
        f"{REDFISH_URL}/redfish/v1/Systems/{REDFISH_DEVICE_ID}",
        headers={"Authorization": f"Basic {creds}"},
    )
    try:
        with _redfish_opener().open(req, timeout=10) as resp:
            body = json.loads(resp.read().decode())
    except (urllib.error.URLError, ssl.SSLError, json.JSONDecodeError, OSError):
        return None
    return body.get("PowerState")


def wait_redfish_power_state(expected: str, timeout: int = 180) -> None:
    """Wait for the guest Redfish PowerState to reach *expected* (On/Off)."""
    log(f"  Waiting for guest Redfish PowerState to be {expected!r}...")
    last_state: str | None = None
    for elapsed in range(timeout):
        check_procs()
        state = redfish_power_state()
        if state == expected:
            log(f"  Guest PowerState is {expected!r}")
            return
        if elapsed > 0 and elapsed % 15 == 0 and state != last_state:
            last_state = state
            log(f"    ({elapsed}s) PowerState={state or 'unknown'}")
        time.sleep(1)
    die(f"Timed out waiting for guest Redfish PowerState to be {expected!r}")


def wait_playpen_ready(timeout: int = 300) -> None:
    """Wait until playpen's forwarded Redfish service root answers."""
    log("  Waiting for playpen overlay + guest Redfish to become reachable...")
    for elapsed in range(timeout):
        check_procs()
        req = urllib.request.Request(f"{REDFISH_URL}/redfish/v1/")
        try:
            with _redfish_opener().open(req, timeout=10) as resp:
                if resp.status == 200:
                    log("  playpen Redfish service root is reachable")
                    return
        except (urllib.error.HTTPError,) as e:
            # Any HTTP response (even 401) proves the forward + server are up.
            if e.code in (401, 403, 404):
                log("  playpen Redfish service root is reachable")
                return
        except (urllib.error.URLError, ssl.SSLError, OSError):
            pass
        if elapsed > 0 and elapsed % 15 == 0:
            log(f"    ({elapsed}s) playpen not reachable yet")
        time.sleep(1)
    die("Timed out waiting for playpen overlay / guest Redfish to be reachable")


def machine_status() -> str | None:
    """Return a short summary of Machine conditions, or None."""
    result = subprocess.run(
        [*KUBECTL, "get", f"machines.{API_GROUP}", NODE_NAME,
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
            [*KUBECTL, "get", "node", name, "-o", "json"],
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            if elapsed > 0 and elapsed % 30 == 0:
                status = machine_status()
                if status != last_status:
                    last_status = status
                log(f"    ({elapsed}s) Machine conditions: {status or 'none'}")

                # Bail early if the guest is no longer powered on.
                if redfish_power_state() == "Off":
                    die(f"Guest '{name}' powered off before joining the cluster")
            time.sleep(1)
            continue
        log(f"  Node '{name}' appeared in cluster")
        return
    die(f"Timed out waiting for Node '{name}'")


def get_node_boot_id(name: str) -> str:
    result = subprocess.run(
        [*KUBECTL, "get", "node", name, "-o", "jsonpath={.status.nodeInfo.bootID}"],
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
            [*KUBECTL, "get", "node", name, "-o", "jsonpath={.status.nodeInfo.bootID}"],
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


def assert_node_ready(name: str, timeout: int = 300) -> None:
    """Assert the Node reaches Ready status within timeout seconds.

    kindnet is excluded from the smoke node and a static CNI conflist is
    written by the smoke-cni DaemonSet, so the node should report Ready
    shortly after the writer pod starts (image pull + kubelet CNI re-check).
    """
    log(f"  Waiting for Node '{name}' to become Ready...")
    for elapsed in range(timeout):
        check_procs()
        result = subprocess.run(
            [*KUBECTL, "get", "node", name, "-o",
             "jsonpath={.status.conditions[?(@.type=='Ready')].status}"],
            capture_output=True, text=True,
        )
        if result.returncode == 0 and result.stdout.strip() == "True":
            log(f"  Node '{name}' is Ready")
            return
        if elapsed > 0 and elapsed % 15 == 0:
            log(f"    ({elapsed}s) Node not yet Ready")
        time.sleep(1)
    die(f"Timed out waiting for Node '{name}' to become Ready")


def assert_node_label(name: str, key: str, value: str) -> None:
    result = subprocess.run(
        [*KUBECTL, "get", "node", name, "-o", "json"],
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
    # Track when each provisioning milestone flips to True so the install phase
    # can be split into netboot/disk-write vs OS-boot/cloud-init sub-phases when
    # profiling smoke test latency.
    milestones_seen: set[str] = set()
    for elapsed in range(timeout):
        check_procs()
        result = subprocess.run(
            [*KUBECTL, "get", f"machineoperations.{API_GROUP}", "-o", "json"],
            capture_output=True, text=True,
        )
        op_name = ""
        phase = ""
        status = ""
        reason = ""
        message = ""
        conditions: list[dict[str, Any]] = []
        if result.returncode == 0:
            try:
                op = find_host_replace_operation(json.loads(result.stdout).get("items", []))
                if op is not None:
                    op_name = op.get("metadata", {}).get("name", "")
                    op_status = op.get("status", {})
                    phase = op_status.get("phase", "")
                    message = op_status.get("message", "")
                    conditions = op_status.get("conditions", [])
                    for c in conditions:
                        if c.get("type") == "CloudInitDone":
                            status = c.get("status", "")
                            reason = c.get("reason", "")
                            message = c.get("message", "")
                            break
            except (json.JSONDecodeError, KeyError):
                pass

        for c in conditions:
            ctype = c.get("type", "")
            if ctype in milestones_seen:
                continue
            if c.get("status") == "True":
                milestones_seen.add(ctype)
                log(f"    milestone {ctype} True/{c.get('reason', '')}")

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
            [*KUBECTL, "get", f"machineoperations.{API_GROUP}", name, "-o", "json"],
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
    proc = spawn(
        [str(KUBECTL_UNBOUNDED), "--kubeconfig", str(KIND_KUBECONFIG), "machine", *args],
        TMPDIR / log_name,
    )
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
    wait_redfish_power_state("Off", timeout=180)

    poweron = create_machine_operation("smoke-host-poweron", "HostPowerOn")
    wait_machine_operation_complete(poweron, timeout=600)
    wait_redfish_power_state("On", timeout=180)
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
    wait_redfish_power_state("On", timeout=180)
    wait_k8s_node(NODE_NAME, timeout=300)
    wait_node_boot_id_changed(NODE_NAME, boot_id, timeout=600)


def parse_args() -> argparse.Namespace:
    default_context = subprocess.run(
        ["kubectl", "config", "current-context"],
        capture_output=True, text=True,
    ).stdout.strip()
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--pod-image", required=True,
        help="fully-qualified playpen pod image reference (must include the "
             "guest-disk support), pullable by the target cluster",
    )
    parser.add_argument(
        "--pod-node", default="node-1",
        help="node in the target cluster to pin the playpen pod on "
             "(must be KVM-capable); default node-1",
    )
    parser.add_argument(
        "--context", default=default_context,
        help="kubectl context of the real cluster playpen targets "
             "(default: current context)",
    )
    parser.add_argument(
        "--kind-context", default="kind-kind",
        help="kubectl context of the local KIND metalman control plane "
             "(default: kind-kind)",
    )
    return parser.parse_args()


def main() -> None:
    global KUBECTL, REAL_CONTEXT, POD_IMAGE, POD_NODE, KIND_APISERVER_PORT, KIND_KUBECONFIG

    args = parse_args()
    if not args.context:
        die("Could not determine the real cluster context; pass --context")
    REAL_CONTEXT = args.context
    POD_IMAGE = args.pod_image
    POD_NODE = args.pod_node
    KUBECTL = ["kubectl", "--context", args.kind_context]

    signal.signal(signal.SIGINT, _sigint_handler)
    atexit.register(cleanup)

    log(f"KIND control plane context: {args.kind_context}")
    log(f"playpen target context:    {REAL_CONTEXT}")
    log(f"playpen pod image:         {POD_IMAGE}")
    log(f"playpen pod node:          {POD_NODE}")

    KIND_APISERVER_PORT = kind_apiserver_port()

    # kubectl-unbounded has no --context flag, so write a standalone kubeconfig
    # scoped to the KIND context and point it there via --kubeconfig.
    KIND_KUBECONFIG = TMPDIR / "kind.kubeconfig"
    KIND_KUBECONFIG.write_text(
        subprocess.run(
            ["kubectl", "config", "view", "--minify", "--flatten",
             "--context", args.kind_context],
            check=True, capture_output=True, text=True,
        ).stdout,
        encoding="utf-8",
    )

    log("Cleaning up stale host resources")
    lean_teardown()

    log("Rendering machina manifests")
    run(["make", "machina-manifests"], cwd=str(REPO_ROOT))

    log("Building metalman, kubectl-unbounded, unbounded-agent, and playpen (parallel)")
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
        ("playpen", subprocess.Popen(
            ["go", "build", "-o", str(PLAYPEN), "./cmd/playpen"],
            cwd=str(REPO_ROOT),
        )),
    ]

    # Kubernetes setup runs while Go builds are in progress.
    log("Cleaning up stale Kubernetes resources (KIND control plane)")
    run_quiet([*KUBECTL, "-n", NODE_NS, "delete", "secret", "bmc-pass"])
    run_quiet([*KUBECTL, "delete", f"machineoperations.{API_GROUP}", "--all"])
    run_quiet([*KUBECTL, "delete", f"machines.{API_GROUP}", NODE_NAME])
    run_quiet([*KUBECTL, "delete", "node", NODE_NAME])
    # Remove stale CRDs so that a version change (e.g. storedVersions
    # referencing an old API version) does not block the fresh apply.
    run_quiet([*KUBECTL, "delete", "crd", f"machines.{API_GROUP}"])

    log("Applying deploy manifests (CRDs, namespace, RBAC)")
    kubectl(["apply", "--server-side", "--force-conflicts", "-f", str(REPO_ROOT / "deploy" / "machina" / "rendered" / "01-namespace.yaml")])
    kubectl(["apply", "--server-side", "--force-conflicts", "-f", str(REPO_ROOT / "deploy" / "machina" / "crd")])
    kubectl(["apply", "--server-side", "--force-conflicts", "-f", str(REPO_ROOT / "deploy" / "machina" / "rendered" / "06-metalman-rbac.yaml")])

    log("Creating Kubernetes resources")
    # playpen's in-pod Redfish requires a non-empty password (its default is
    # "password"); metalman authenticates with it via this secret.
    kubectl(["-n", NODE_NS, "create", "secret", "generic",
             "bmc-pass", f"--from-literal=password={REDFISH_PASSWORD}"])

    log("Starting local OCI registry")
    run_quiet(["docker", "rm", "-f", REGISTRY_CONTAINER])
    run(["docker", "run", "-d", "--name", REGISTRY_CONTAINER,
         "-p", f"127.0.0.1:{REGISTRY_PORT}:5000", "registry:2"])
    # Wait for the registry to be ready.
    for _ in range(30):
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{REGISTRY_PORT}/v2/")
            break
        except Exception:
            time.sleep(0.5)
    else:
        die("Local OCI registry did not become ready")

    # Docker images are pre-built by the GitHub Actions workflow using
    # docker/build-push-action with GHA layer caching. They are already loaded
    # into the local Docker daemon with the correct tags.
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

    log("Starting agent download server (loopback)")
    proc = spawn([
        sys.executable, "-m", "http.server", str(AGENT_DOWNLOAD_PORT),
        "--bind", "127.0.0.1", "--directory", str(ARTIFACT_DIR),
    ], TMPDIR / "agent-download.log")
    log(f"  agent download PID={proc.pid}")
    time.sleep(1)
    check_procs()

    log("Pushing OCI images to local registry")
    run(["docker", "push", IMAGE_NAME])
    run(["docker", "push", NETBOOT_IMAGE_NAME])
    run(["docker", "push", AGENT_IMAGE_NAME])

    # Reclaim disk space consumed by Docker build cache.
    log("Pruning Docker build cache to free disk space")
    run_quiet(["docker", "builder", "prune", "-af"], check=False)

    # Assign the guest's overlay lease IP to loopback so playpen's proxies can
    # bind their egress source to it when dialing metalman on 127.0.0.1. The
    # kernel then reports the guest IP as the peer to the loopback-bound
    # metalman, so metalman resolves the Machine by its real DHCP lease.
    log(f"Assigning {OVERLAY_NODE_IP}/32 to lo for proxy source-IP preservation")
    run_quiet(["sudo", "-n", "ip", "addr", "add", f"{OVERLAY_NODE_IP}/32", "dev", "lo"], check=False)

    # Grant playpen the capability to bind the privileged DHCP relay port (67)
    # so it can run as the invoking user (and use the normal kubeconfig for the
    # real cluster context) without sudo.
    log("Granting playpen CAP_NET_BIND_SERVICE for the DHCP relay port")
    run(["sudo", "setcap", "cap_net_bind_service=+ep", str(PLAYPEN)])

    log("Starting playpen overlay (remote guest VM)")
    playpen_proc = spawn([
        str(PLAYPEN), "up", "--keep-up",
        "--context", REAL_CONTEXT,
        "--pod-node", POD_NODE,
        "--pod-image", POD_IMAGE,
        "--vm-disk-size", "20",
        "--vm-mac", MAC_ADDRESS,
        # Give the guest several vCPUs so the streaming OS-image install
        # (wget | gunzip | dd) can run its stages on separate cores. The pod
        # CPU limit is 4 (requests 0.5), so this bursts without a large request.
        "--vm-cpus", "4",
        # Bind playpen's proxy egress to the guest's overlay lease IP so
        # metalman (bound to 127.0.0.1) sees requests coming from the real
        # guest IP and resolves the Machine by its real DHCP lease. Requires
        # OVERLAY_NODE_IP to be assigned to lo on the host (done in main()).
        "--proxy-source-ip", OVERLAY_NODE_IP,
        # Run an in-pod HTTP reverse proxy on the pod overlay IP:HTTP_PORT that
        # forwards to the client IP. The guest fetches vmlinuz/initrd over the
        # fast pod<->guest LAN hop (grub has a tiny TCP window), and the pod
        # re-originates to the client over the overlay with kernel TCP.
        "--netboot-proxy-port", str(HTTP_PORT),
        # Expose loopback services to the guest over the overlay.
        "--forward", f"6443:{KIND_APISERVER_PORT}",
        "--forward", f"{HTTP_PORT}:{HTTP_PORT}",
        "--forward", f"{AGENT_DOWNLOAD_PORT}:{AGENT_DOWNLOAD_PORT}",
        "--forward", f"{REGISTRY_PORT}:{REGISTRY_PORT}",
        # DHCP relay: metalman listens on METALMAN_DHCP_PORT and unicasts its
        # replies to the giaddr (127.0.0.1) on port 67, which playpen binds.
        "--dhcp-server", f"127.0.0.1:{METALMAN_DHCP_PORT}",
        "--dhcp-giaddr", "127.0.0.1",
        "--dhcp-relay-port", str(DHCP_RELAY_PORT),
        "--tftp-server", f"127.0.0.1:{TFTP_PORT}",
    ], TMPDIR / "playpen.log")
    log(f"  playpen PID={playpen_proc.pid}")
    wait_playpen_ready(timeout=300)

    # Kindnet's CONTROL_PLANE_ENDPOINT and kube-proxy's API server must be
    # reachable by the guest kubelet's pods. They reach the KIND API server over
    # the overlay at the client IP, forwarded to the KIND API server's loopback
    # port. The API server's TLS cert SANs (kind-smoke-config.yaml) cover this
    # overlay IP, 127.0.0.1, and localhost.
    # The smoke node does not need working pod networking: the only pods that
    # ever land on it are hostNetwork system pods (kube-proxy). The real kindnet
    # DaemonSet crash-loops for minutes on the freshly joined node while its
    # in-cluster bootstrap settles, which dominates node-ready latency. Instead,
    # exclude kindnet from the smoke node (nodeAffinity on the label metalman
    # applies at join) and drop a static CNI conflist via a tiny hostNetwork
    # DaemonSet so kubelet reports the node Ready almost immediately. The
    # reference CNI plugin binaries (ptp, host-local, portmap) are already
    # present at /opt/cni/bin, installed by the unbounded-agent cni-plugins
    # artifact. We keep the CONTROL_PLANE_ENDPOINT override for kindnet on the
    # remaining (control-plane) nodes and repoint kube-proxy at the overlay API.
    log("Excluding kindnet from the smoke node")
    patch = json.dumps({
        "spec": {"template": {"spec": {
            "affinity": {"nodeAffinity": {
                "requiredDuringSchedulingIgnoredDuringExecution": {
                    "nodeSelectorTerms": [{"matchExpressions": [
                        {"key": NODE_LABEL_KEY, "operator": "DoesNotExist"},
                    ]}],
                },
            }},
            "containers": [{
                "name": "kindnet-cni",
                "env": [
                    {"name": "CONTROL_PLANE_ENDPOINT", "value": f"{OVERLAY_CLIENT_IP}:6443"},
                ],
            }],
        }}}
    })
    kubectl(["-n", "kube-system", "patch", "daemonset", "kindnet",
             "--type=strategic", "-p", patch])
    configure_kind_kube_proxy_apiserver(GUEST_APISERVER_URL)

    log("Deploying static CNI writer DaemonSet for the smoke node")
    cni_conflist = (
        '{"cniVersion":"0.3.1","name":"smoke","plugins":['
        '{"type":"ptp","ipMasq":true,"ipam":{"type":"host-local",'
        '"dataDir":"/run/cni-ipam-state","routes":[{"dst":"0.0.0.0/0"}],'
        '"ranges":[[{"subnet":"10.244.244.0/24"}]]}},'
        '{"type":"portmap","capabilities":{"portMappings":true}}]}'
    )
    smoke_cni = {
        "apiVersion": "apps/v1",
        "kind": "DaemonSet",
        "metadata": {"name": "smoke-cni", "namespace": "kube-system"},
        "spec": {
            "selector": {"matchLabels": {"app": "smoke-cni"}},
            "template": {
                "metadata": {"labels": {"app": "smoke-cni"}},
                "spec": {
                    "hostNetwork": True,
                    "nodeSelector": {NODE_LABEL_KEY: NODE_LABEL_VALUE},
                    "tolerations": [{"operator": "Exists"}],
                    "terminationGracePeriodSeconds": 1,
                    "containers": [{
                        "name": "cni-writer",
                        "image": "busybox:1.36",
                        "command": ["/bin/sh", "-c"],
                        "args": [
                            "cat > /etc/cni/net.d/10-smoke.conflist <<'EOF'\n"
                            f"{cni_conflist}\nEOF\n"
                            "echo wrote-cni-conflist; sleep infinity"
                        ],
                        "volumeMounts": [
                            {"name": "cni-cfg", "mountPath": "/etc/cni/net.d"},
                        ],
                    }],
                    "volumes": [{
                        "name": "cni-cfg",
                        "hostPath": {"path": "/etc/cni/net.d", "type": "DirectoryOrCreate"},
                    }],
                },
            },
        },
    }
    kubectl(["apply", "-f", "-"], input=json.dumps(smoke_cni).encode(), stdout=DEVNULL)

    log("Creating Machine resource")
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
                # PXE/TFTP boot. metalman's TFTP server streams DATA blocks ahead
                # of client ACKs (SetAnticipate window) so large netboot artifacts
                # (grubx64.efi, vmlinuz, initrd) transfer fast enough over the
                # high-latency WireGuard overlay instead of timing out lockstep.
                # The cloud-hypervisor guest's single virtio-blk disk
                # (--vm-disk-size) appears as /dev/vda.
                "targetDisk": "/dev/vda",
                "redfish": {
                    "url": REDFISH_URL,
                    "username": REDFISH_USERNAME,
                    "deviceID": REDFISH_DEVICE_ID,
                    "passwordRef": {"name": "bmc-pass", "key": "password", "namespace": NODE_NS},
                },
                "dhcpLeases": [
                    {
                        "mac": MAC_ADDRESS,
                        "ipv4": OVERLAY_NODE_IP,
                        "subnetMask": OVERLAY_MASK,
                        "gateway": OVERLAY_GATEWAY,
                        "dns": [DNS_SERVER],
                    },
                ],
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

    log("Starting metalman serve-pxe (DHCP relay mode)")
    metalman_kubeconfig = TMPDIR / "metalman-controller.kubeconfig"
    write_service_account_kubeconfig(METALMAN_NAMESPACE, METALMAN_CONTROLLER_SA, metalman_kubeconfig)
    # The joining kubelet reaches the API server over the overlay, so metalman
    # must generate join material pointing at the guest-reachable URL.
    metalman_env = [
        f"METALMAN_APISERVER_URL={GUEST_APISERVER_URL}",
        f"KUBECONFIG={metalman_kubeconfig}",
    ]

    # metalman runs under sudo because its TFTP server binds the privileged
    # port 69. It binds loopback but advertises the overlay client IP as the
    # DHCP next-server and serve-url host so playpen's proxies (which dial
    # loopback) can reach it while the guest sees an overlay-routable address.
    proc = spawn([
        "sudo", "env", *metalman_env,
        str(BINARY), "serve-pxe", f"--site={SITE}",
        "--bind-address=127.0.0.1",
        f"--advertise-ip={OVERLAY_CLIENT_IP}",
        f"--cache-dir={CACHE_DIR}",
        f"--serve-url={SERVE_URL}",
        f"--dhcp-port={METALMAN_DHCP_PORT}",
        f"--default-netboot-image={NETBOOT_IMAGE_NAME}",
        # grub loads the kernel/initrd through its own (slow) UEFI network
        # stack over the high-latency overlay, so the repave boot stage can take
        # well over the 5m default before the boot image is written. Give it
        # plenty of headroom so metalman does not power-cycle mid-download.
        "--operation-power-action-timeout=30m",
        "--leader-elect-lease-duration=60s",
        "--leader-elect-renew-deadline=40s",
        "--leader-elect-retry-period=5s",
    ], TMPDIR / "serve.log")
    log(f"  serve PID={proc.pid}")

    time.sleep(2)
    check_procs()

    # HTTP boot repave needs the netboot image metadata synchronously when the
    # HostReplace operation is processed; unlike PXE mode it fails hard if the
    # image is not cached yet. Wait for metalman to pull/cache the netboot and
    # host images before triggering the operation to defeat that cold-cache race.
    log("Waiting for metalman to cache the netboot + host OCI images...")
    serve_log = TMPDIR / "serve.log"
    cache_deadline = time.time() + 300
    needed = [NETBOOT_IMAGE_NAME, IMAGE_NAME]
    while True:
        check_procs()
        try:
            served_lines = serve_log.read_text(errors="replace").splitlines()
        except FileNotFoundError:
            served_lines = []
        cached = {
            ref for ref in needed
            if any("OCI image cached" in ln and f"image={ref}" in ln for ln in served_lines)
        }
        if len(cached) == len(needed):
            break
        if time.time() > cache_deadline:
            die("metalman did not cache the OCI images within 300s")
        time.sleep(3)
    log("  OCI images cached")

    log("Triggering HostReplace through kubectl-unbounded")
    operation_log = TMPDIR / "kubectl-host-replace.log"
    operation_proc = run_kubectl_unbounded_operation(
        ["replace", NODE_NAME, "--force", "--ttl=3600"],
        operation_log.name,
    )

    log("Waiting for cloud-init to complete...")
    assert_cloud_init_done(timeout=2400)

    wait_process_success(operation_proc, timeout=2400)
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
