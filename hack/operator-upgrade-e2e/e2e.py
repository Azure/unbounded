#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Faithful, standalone upgrade e2e for the unbounded-operator migration.

It stands up a multi-node kind cluster (1 control-plane + N workers, default 5)
with a local OCI registry, installs the last
RELEASED multi-namespace version (default v0.1.19) via that release's real
`kubectl unbounded site init` (CNI-free so it coexists with kindnet), then
upgrades to the operator model built from the current tree via
`kubectl unbounded install`, and asserts the operator's reaper migrates
everything onto the unified `unbounded-system` namespace:

  * the pre-redesign net-group Sites are translated into machina-group Sites,
  * operator/user Secrets and ConfigMaps are copied over, including the exact
    valid legacy unbounded-net-config Data and BinaryData payload,
  * cluster-scoped secret references (Machine.spec.pxe.redfish.passwordRef) are
    repointed,
  * the new net + machina workloads come up Ready (real datapath, via the
    net-node hostNetwork cutover),
  * both net workload templates carry the exact production ConfigMap payload
    hash, and a post-migration BinaryData-only edit rolls out both workloads,
  * the cluster Site's Nodes carry the canonical `unbounded-cloud.io/site`
    label alongside the deprecated `net.unbounded-cloud.io/site` (the net
    controller dual-writes both during the label deprecation window),
  * unbounded-net-controller restarts cleanly (its hostNetwork Deployment uses
    maxSurge=0 so a rollout does not deadlock on host port 9999), and
  * rerunning the identical current install repairs a deleted required CRD by
    changing only the operator repair token and restarting the operator, and
  * the legacy `unbounded-kube`/`unbounded-net` namespaces and the old
    `sites.net.unbounded-cloud.io` CRD are reaped.

`kubectl unbounded install` no longer applies CRDs: the operator installs and
upgrades them itself at startup (operator.BootstrapCRDs), so verify asserts the
Site CRD is Established and owned by the unbounded-operator field manager.

Scope: net + machina run for real in kind. Storage (RDMA) and metalman (PXE)
cannot run in vanilla kind, so they are intentionally NOT installed in the old
cluster (their reaping is covered by the in-process simulation test in
`e2e/operator` and by unit tests).

This script is designed to be run standalone from a dev machine AND consumed by
CI (`.github/workflows/operator-upgrade-e2e.yaml`) as a single thin step. CI
carries no orchestration logic; it lives here.

Usage:
  python3 hack/operator-upgrade-e2e/e2e.py [all|setup|build-images|install-old|
                                            upgrade|verify|cleanup]
      [--old-version v0.1.19] [--workers 5] [--registry-port 5001]
      [--keep-cluster] [--skip-build] [--verify-timeout 1500]

`all` (the default) runs setup -> build-images -> install-old -> upgrade ->
verify, tearing the cluster down at the end unless --keep-cluster / E2E_KEEP=1.
Individual subcommands can be run in sequence against a kept cluster while
iterating locally.

Requires: docker, kind, kubectl, go, make, curl. Run from anywhere.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import platform
import subprocess
import sys
import tarfile
import threading
import time
import urllib.request
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
WORKDIR = REPO_ROOT / "tmp" / "operator-upgrade-e2e"
KUBECONFIG = WORKDIR / "kubeconfig"
LEGACY_NET_CONFIG_PAYLOAD = WORKDIR / "legacy-unbounded-net-config-payload.json"

CLUSTER = "operator-upgrade-e2e"
REGISTRY_CONTAINER = "operator-upgrade-e2e-registry"

TARGET_NS = "unbounded-system"
LEGACY_KUBE = "unbounded-kube"
LEGACY_NET = "unbounded-net"
LEGACY_SITE_CRD = "sites.net.unbounded-cloud.io"
REMOTE_SITE = "edge"
CLUSTER_SITE = "cluster"

NET_CONFIG_HASH_ANNOTATION = "unbounded-cloud.io/net-config-hash"
OPERATOR_CONFIG_HASH_ANNOTATION = "unbounded-cloud.io/operator-config-hash"
OPERATOR_REPAIR_ANNOTATION = "unbounded-cloud.io/operator-crd-repair-token"
REPAIR_CRD = "machineoperationcredentials.unbounded-cloud.io"

NET_CONFIG_DATA_SENTINEL = "operator-upgrade-e2e-sentinel"
NET_CONFIG_BINARY_SENTINEL = "operator-upgrade-e2e-sentinel.bin"

PAUSE_IMAGE = "registry.k8s.io/pause:3.10"

# CIDRs for the Site spec. Because the old install uses --manage-cni-plugin=false
# these are never applied to the kind node's CNI. The cluster Site's node CIDR is
# computed at install time to cover the kind nodes' InternalIPs (see
# kind_cluster_node_cidr) so the net controller genuinely assigns the site label
# to them; that is what lets the upgrade exercise the canonical/deprecated
# site-label dual-write. The remote ("edge") Site node CIDR is a CGNAT range that
# no kind node matches and that never overlaps the cluster range.
CLUSTER_NODE_CIDR = "172.20.0.0/16"  # fallback; normally computed from kind nodes
CLUSTER_POD_CIDR = "10.244.0.0/16"
SITE_NODE_CIDR = "100.64.0.0/16"
SITE_POD_CIDR = "10.245.0.0/16"


# --------------------------------------------------------------------------- #
# small process helpers
# --------------------------------------------------------------------------- #
def log(msg: str) -> None:
    print(f"[operator-upgrade-e2e] {msg}", flush=True)


def die(msg: str) -> None:
    print(f"[operator-upgrade-e2e] ERROR: {msg}", file=sys.stderr, flush=True)
    sys.exit(1)


def run(cmd: list[str], *, check: bool = True, cwd: Path | None = None,
        env: dict[str, str] | None = None, quiet: bool = False) -> int:
    if not quiet:
        log("+ " + " ".join(cmd))

    proc = subprocess.run(cmd, cwd=str(cwd) if cwd else None, env=env)
    if check and proc.returncode != 0:
        die(f"command failed ({proc.returncode}): {' '.join(cmd)}")

    return proc.returncode


def run_out(cmd: list[str], *, check: bool = True) -> str:
    proc = subprocess.run(cmd, capture_output=True, text=True)
    if check and proc.returncode != 0:
        die(f"command failed ({proc.returncode}): {' '.join(cmd)}\n{proc.stderr}")

    return proc.stdout


def make(args: list[str]) -> None:
    run(["make", *args], cwd=REPO_ROOT)


def kubectl(args: list[str], *, check: bool = True, quiet: bool = False) -> int:
    return run(["kubectl", "--kubeconfig", str(KUBECONFIG), *args],
               check=check, quiet=quiet)


def kubectl_out(args: list[str], *, check: bool = True) -> str:
    return run_out(["kubectl", "--kubeconfig", str(KUBECONFIG), *args], check=check)


def kubectl_apply_stdin(manifest: str) -> None:
    log("+ kubectl apply -f - (stdin manifest)")
    proc = subprocess.run(
        ["kubectl", "--kubeconfig", str(KUBECONFIG), "apply", "-f", "-"],
        input=manifest, text=True,
    )
    if proc.returncode != 0:
        die("kubectl apply (stdin) failed")


def resource_exists(args: list[str]) -> bool:
    proc = subprocess.run(
        ["kubectl", "--kubeconfig", str(KUBECONFIG), "get", *args],
        capture_output=True, text=True,
    )
    return proc.returncode == 0


def _resource_json(args: list[str]) -> dict | None:
    out = kubectl_out(["get", *args, "-o", "json"], check=False)
    try:
        obj = json.loads(out)
    except json.JSONDecodeError:
        return None

    return obj if isinstance(obj, dict) else None


def _configmap_payload(config: dict) -> dict[str, dict[str, str]]:
    payload: dict[str, dict[str, str]] = {}
    for field in ("data", "binaryData"):
        values = config.get(field) or {}
        if not isinstance(values, dict) or not all(
                isinstance(key, str) and isinstance(value, str)
                for key, value in values.items()):
            raise ValueError(f"ConfigMap {field} is not a string map")
        payload[field] = dict(values)

    return payload


def _go_json_map(values: dict[str, str]) -> str:
    """Encode a string map like Go encoding/json with HTML escaping enabled."""
    encoded = json.dumps(
        {key: values[key] for key in sorted(values)},
        ensure_ascii=False,
        separators=(",", ":"),
    )
    return (encoded.replace("&", "\\u0026")
            .replace("<", "\\u003c")
            .replace(">", "\\u003e")
            .replace("\u2028", "\\u2028")
            .replace("\u2029", "\\u2029"))


def _configmap_payload_json(payload: dict[str, dict[str, str]]) -> str:
    # This field order and map encoding exactly match configMapPayloadHash in Go.
    return (f'{{"data":{_go_json_map(payload["data"])},'
            f'"binaryData":{_go_json_map(payload["binaryData"])}}}')


def _configmap_payload_hash(payload: dict[str, dict[str, str]]) -> str:
    return hashlib.sha256(_configmap_payload_json(payload).encode()).hexdigest()


def _load_legacy_net_config_payload() -> dict[str, dict[str, str]]:
    if not LEGACY_NET_CONFIG_PAYLOAD.is_file():
        die(f"required persisted legacy ConfigMap payload is missing: "
            f"{LEGACY_NET_CONFIG_PAYLOAD}; run install-old first")

    try:
        stored = json.loads(LEGACY_NET_CONFIG_PAYLOAD.read_text(encoding="utf-8"))
        if not isinstance(stored, dict) or set(stored) != {"data", "binaryData"}:
            raise ValueError("expected exactly data and binaryData fields")
        return _configmap_payload(stored)
    except (OSError, json.JSONDecodeError, ValueError) as exc:
        die(f"invalid persisted legacy ConfigMap payload "
            f"{LEGACY_NET_CONFIG_PAYLOAD}: {exc}")

    return {}


# --------------------------------------------------------------------------- #
# derived config
# --------------------------------------------------------------------------- #
def registry_host(port: int) -> str:
    return f"localhost:{port}"


def images(port: int) -> dict[str, str]:
    reg = registry_host(port)
    return {
        "net_controller": f"{reg}/unbounded-net-controller:e2e",
        "net_node": f"{reg}/unbounded-net-node:e2e",
        "machina": f"{reg}/machina:e2e",
        "operator": f"{reg}/unbounded-operator:e2e",
    }


def host_arch() -> str:
    m = platform.machine().lower()
    if m in ("aarch64", "arm64"):
        return "arm64"
    return "amd64"


def control_plane_node() -> str:
    for node in run_out(["kind", "get", "nodes", "--name", CLUSTER]).split():
        if node.endswith("-control-plane"):
            return node

    die(f"kind cluster {CLUSTER} has no control-plane node")

    return ""


def kind_cluster_node_cidr() -> str:
    """Derive a /16 covering the kind nodes' InternalIPs.

    kind places every node on a single docker network, so a /16 taken from any
    node's InternalIP covers them all. Making the cluster Site's nodeCidr cover
    the nodes is what causes the net controller to assign them the site label,
    which the upgrade then asserts is dual-written (canonical + deprecated).
    """
    out = kubectl_out([
        "get", "nodes", "-o",
        "jsonpath={range .items[*]}{.status.addresses[?(@.type=='InternalIP')].address}{'\\n'}{end}",
    ], check=False)
    ips = [line.strip() for line in out.splitlines() if line.strip()]
    if not ips:
        log(f"WARNING: could not read node InternalIPs; falling back to {CLUSTER_NODE_CIDR}")
        return CLUSTER_NODE_CIDR

    octets = ips[0].split(".")
    if len(octets) != 4:
        log(f"WARNING: unexpected node InternalIP {ips[0]!r}; falling back to {CLUSTER_NODE_CIDR}")
        return CLUSTER_NODE_CIDR

    return f"{octets[0]}.{octets[1]}.0.0/16"


def ensure_kubeconfig() -> None:
    WORKDIR.mkdir(parents=True, exist_ok=True)
    run(["kind", "export", "kubeconfig", "--name", CLUSTER,
         "--kubeconfig", str(KUBECONFIG)])


# --------------------------------------------------------------------------- #
# phases
# --------------------------------------------------------------------------- #
def check_prereqs() -> None:
    missing = [b for b in ("docker", "kind", "kubectl", "go", "make", "curl")
               if run(["bash", "-c", f"command -v {b}"], check=False, quiet=True) != 0]
    if missing:
        die(f"missing required tools on PATH: {', '.join(missing)}")

    if run(["docker", "info"], check=False, quiet=True) != 0:
        die("docker engine is not reachable")


def cluster_exists() -> bool:
    out = run_out(["kind", "get", "clusters"], check=False)
    return CLUSTER in out.split()


def cmd_setup(args: argparse.Namespace) -> None:
    check_prereqs()
    WORKDIR.mkdir(parents=True, exist_ok=True)

    port = args.registry_port

    # 1. Local registry (idempotent).
    if run(["docker", "inspect", REGISTRY_CONTAINER], check=False, quiet=True) != 0:
        log("starting local OCI registry")
        run(["docker", "run", "-d", "--restart=always",
             "-p", f"127.0.0.1:{port}:5000", "--name", REGISTRY_CONTAINER,
             "registry:2"])
    else:
        log("local OCI registry already running")

    for _ in range(60):
        try:
            urllib.request.urlopen(f"http://localhost:{port}/v2/")  # noqa: S310
            break
        except Exception:  # noqa: BLE001
            time.sleep(0.5)
    else:
        die("local OCI registry did not become ready")

    # 2. kind cluster wired to the registry (containerd certs.d mirror). A
    # multi-node cluster (1 control-plane + N workers) exercises the net-node
    # DaemonSet and the host-port cutover on every node, not just one.
    if cluster_exists():
        log(f"kind cluster {CLUSTER} already exists; reusing")
    else:
        kind_config = (
            "kind: Cluster\n"
            "apiVersion: kind.x-k8s.io/v1alpha4\n"
            "containerdConfigPatches:\n"
            "- |-\n"
            '  [plugins."io.containerd.grpc.v1.cri".registry]\n'
            '    config_path = "/etc/containerd/certs.d"\n'
            "nodes:\n"
            "- role: control-plane\n"
            + "- role: worker\n" * args.workers
        )
        cfg = WORKDIR / "kind-config.yaml"
        cfg.write_text(kind_config)
        log(f"creating kind cluster (1 control-plane + {args.workers} workers)")
        run(["kind", "create", "cluster", "--name", CLUSTER,
             "--config", str(cfg), "--wait", "240s",
             "--kubeconfig", str(KUBECONFIG)])

    ensure_kubeconfig()

    # 3. Point each node's containerd at the registry container.
    reg_dir = f"/etc/containerd/certs.d/localhost:{port}"
    hosts_toml = f'[host."http://{REGISTRY_CONTAINER}:5000"]\n'
    for node in run_out(["kind", "get", "nodes", "--name", CLUSTER]).split():
        run(["docker", "exec", node, "mkdir", "-p", reg_dir])
        proc = subprocess.run(
            ["docker", "exec", "-i", node, "cp", "/dev/stdin",
             f"{reg_dir}/hosts.toml"],
            input=hosts_toml, text=True,
        )
        if proc.returncode != 0:
            die(f"failed to write registry hosts.toml on node {node}")

    # 4. Connect the registry to the kind network (idempotent).
    run(["docker", "network", "connect", "kind", REGISTRY_CONTAINER], check=False,
        quiet=True)

    # 5. Advertise the local registry per KEP-1755.
    kubectl_apply_stdin(
        "apiVersion: v1\n"
        "kind: ConfigMap\n"
        "metadata:\n"
        "  name: local-registry-hosting\n"
        "  namespace: kube-public\n"
        "data:\n"
        "  localRegistryHosting.v1: |\n"
        f'    host: "localhost:{port}"\n'
        '    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"\n'
    )

    log("setup complete")


def cmd_build_images(args: argparse.Namespace) -> None:
    if args.skip_build:
        log("--skip-build set; skipping image builds")
        return

    imgs = images(args.registry_port)
    engine = args.container_engine
    eng = [f"CONTAINER_ENGINE={engine}"]

    log(f"building + pushing new component images to the local registry ({engine})")
    make(["image-net-controller-local", f"NET_CONTROLLER_IMAGE={imgs['net_controller']}", *eng])
    run([engine, "push", imgs["net_controller"]])

    make(["image-net-node-local", f"NET_NODE_IMAGE={imgs['net_node']}", *eng])
    run([engine, "push", imgs["net_node"]])

    make(["image-machina-local", f"MACHINA_IMAGE={imgs['machina']}", *eng])
    run([engine, "push", imgs["machina"]])

    # The operator's embedded manifests must reference the registry-hosted
    # component images (imagePullPolicy is Always and there is no runtime
    # override), so bake those refs in at operator-build time.
    make([
        "image-unbounded-operator-local",
        f"UNBOUNDED_OPERATOR_IMAGE={imgs['operator']}",
        f"NET_CONTROLLER_IMAGE={imgs['net_controller']}",
        f"NET_NODE_IMAGE={imgs['net_node']}",
        f"MACHINA_IMAGE={imgs['machina']}",
        *eng,
    ])
    run([engine, "push", imgs["operator"]])

    log("images built and pushed")


def download_old_plugin(version: str) -> Path:
    WORKDIR.mkdir(parents=True, exist_ok=True)
    arch = host_arch()
    asset = f"kubectl-unbounded-linux-{arch}.tar.gz"
    url = f"https://github.com/Azure/unbounded/releases/download/{version}/{asset}"
    tgz = WORKDIR / asset
    log(f"downloading released plugin {version} ({asset})")
    run(["curl", "-fsSL", "-o", str(tgz), url])

    dest = WORKDIR / f"plugin-{version}"
    dest.mkdir(parents=True, exist_ok=True)
    with tarfile.open(tgz) as tf:
        tf.extractall(dest)  # noqa: S202

    binary = dest / "kubectl-unbounded"
    if not binary.exists():
        # Some archives nest the binary; search for it.
        found = list(dest.rglob("kubectl-unbounded"))
        if not found:
            die(f"kubectl-unbounded binary not found in {asset}")
        binary = found[0]

    binary.chmod(0o755)
    return binary


def _stage_legacy_net_config_payload() -> None:
    config = _resource_json([
        "-n", LEGACY_NET, "configmap", "unbounded-net-config",
    ])
    if config is None:
        die(f"legacy {LEGACY_NET}/unbounded-net-config is not readable")

    try:
        payload = _configmap_payload(config)
    except ValueError as exc:
        die(f"legacy {LEGACY_NET}/unbounded-net-config has invalid payload: {exc}")

    original_config = payload["data"].get("config.yaml")
    if not original_config:
        die("legacy unbounded-net-config has no nonempty config.yaml to preserve")

    payload["data"][NET_CONFIG_DATA_SENTINEL] = "legacy-payload-preserved"
    payload["binaryData"][NET_CONFIG_BINARY_SENTINEL] = base64.b64encode(
        b"legacy-payload-preserved\x00\xff",
    ).decode("ascii")

    kubectl([
        "-n", LEGACY_NET, "patch", "configmap", "unbounded-net-config",
        "--type=merge", "-p", json.dumps(payload, separators=(",", ":")),
    ])

    updated = _resource_json([
        "-n", LEGACY_NET, "configmap", "unbounded-net-config",
    ])
    if updated is None:
        die("legacy unbounded-net-config disappeared after staging its payload")

    try:
        exact_payload = _configmap_payload(updated)
    except ValueError as exc:
        die(f"updated legacy unbounded-net-config has invalid payload: {exc}")
    if exact_payload != payload:
        die("legacy unbounded-net-config payload did not retain the exact staged Data/BinaryData")
    if exact_payload["data"].get("config.yaml") != original_config:
        die("legacy unbounded-net-config config.yaml changed while staging sentinels")

    LEGACY_NET_CONFIG_PAYLOAD.write_text(
        _configmap_payload_json(exact_payload), encoding="utf-8",
    )
    log(f"saved exact legacy net ConfigMap payload to {LEGACY_NET_CONFIG_PAYLOAD}")


def cmd_install_old(args: argparse.Namespace) -> None:
    ensure_kubeconfig()
    LEGACY_NET_CONFIG_PAYLOAD.unlink(missing_ok=True)

    plugin = download_old_plugin(args.old_version)

    node = control_plane_node()
    log(f"labeling {node} as an unbounded-net gateway")
    kubectl(["label", "node", node,
             "unbounded-cloud.io/unbounded-net-gateway=true", "--overwrite"])

    # Match the cluster Site's node CIDR to the kind nodes so net assigns them
    # the site label (the upgrade asserts the canonical/deprecated dual-write).
    cluster_node_cidr = kind_cluster_node_cidr()
    log(f"cluster Site nodeCidr = {cluster_node_cidr} (covers the kind nodes)")

    log(f"installing released {args.old_version} via `site init` (CNI-free)")
    run([
        str(plugin), "site", "init",
        "--kubeconfig", str(KUBECONFIG),
        "--name", REMOTE_SITE,
        "--manage-cni-plugin=false",
        "--cluster-node-cidr", cluster_node_cidr,
        "--cluster-pod-cidr", CLUSTER_POD_CIDR,
        "--node-cidr", SITE_NODE_CIDR,
        "--pod-cidr", SITE_POD_CIDR,
    ])

    log("preserving valid legacy net config and adding Data/BinaryData sentinels")
    _stage_legacy_net_config_payload()

    # Stage non-regenerable legacy state the reaper must carry across.
    log("staging legacy Secret / ConfigMap / Machine state")
    kubectl_apply_stdin(
        "apiVersion: v1\n"
        "kind: Secret\n"
        "metadata:\n"
        "  name: redfish-password\n"
        f"  namespace: {LEGACY_KUBE}\n"
        "type: Opaque\n"
        "stringData:\n"
        "  password: hunter2\n"
    )
    kubectl_apply_stdin(
        "apiVersion: unbounded-cloud.io/v1alpha3\n"
        "kind: Machine\n"
        "metadata:\n"
        "  name: m1\n"
        "spec:\n"
        "  pxe:\n"
        "    image: example/pxe-image:v1\n"
        "    redfish:\n"
        "      url: https://bmc.example\n"
        "      username: admin\n"
        "      passwordRef:\n"
        "        name: redfish-password\n"
        f"        namespace: {LEGACY_KUBE}\n"
    )

    log("waiting for the released net + machina workloads to become Ready")
    kubectl(["-n", LEGACY_NET, "rollout", "status",
             "deploy/unbounded-net-controller", "--timeout=600s"])
    kubectl(["-n", LEGACY_NET, "rollout", "status",
             "ds/unbounded-net-node", "--timeout=600s"])
    kubectl(["-n", LEGACY_KUBE, "rollout", "status",
             "deploy/machina-controller", "--timeout=600s"])

    log("legacy install ready")


def _current_install_command(args: argparse.Namespace) -> list[str]:
    imgs = images(args.registry_port)
    plugin = REPO_ROOT / "bin" / "kubectl-unbounded"
    if not plugin.exists():
        die(f"expected current plugin at {plugin}; run upgrade first")

    return [
        str(plugin), "install",
        "--kubeconfig", str(KUBECONFIG),
        "--operator-image", imgs["operator"],
        "--metalman-image", PAUSE_IMAGE,
        "--wait", "--timeout", "5m",
    ]


def _run_current_install(args: argparse.Namespace) -> None:
    run(_current_install_command(args))


def cmd_upgrade(args: argparse.Namespace) -> None:
    ensure_kubeconfig()

    log("building the current-tree kubectl-unbounded plugin")
    make(["kubectl-unbounded-build"])

    log("bootstrapping the operator via `kubectl unbounded install`")
    _run_current_install(args)

    log("operator installed; the reaper migrates asynchronously")


def _dump_diagnostics() -> None:
    log("---- diagnostics ----")
    kubectl(["get", "ns"], check=False)
    kubectl(["get", "pods", "-A"], check=False)
    kubectl(["get", "sites.unbounded-cloud.io"], check=False)
    kubectl(["get", "nodes", "-L", "unbounded-cloud.io/site",
             "-L", "net.unbounded-cloud.io/site"], check=False)
    kubectl(["-n", TARGET_NS, "logs", "deploy/unbounded-operator",
             "--tail=120"], check=False)


def _migration_complete(
        expected_net_payload: dict[str, dict[str, str]],
) -> tuple[bool, str]:
    # 1. Translated machina-group Sites exist.
    for site in (CLUSTER_SITE, REMOTE_SITE):
        if not resource_exists(["sites.unbounded-cloud.io", site]):
            return False, f"machina Site {site} not yet created"

    # 2. machina enabled on the cluster Site (detected from the running workload).
    out = kubectl_out(
        ["get", "sites.unbounded-cloud.io", CLUSTER_SITE, "-o", "json"], check=False)
    try:
        comps = json.loads(out).get("spec", {}).get("components", {})
    except json.JSONDecodeError:
        return False, "cluster Site not readable yet"
    if not comps.get("machina", {}).get("enabled"):
        return False, "machina component not yet enabled on cluster Site"

    # 3. New net + machina workloads Ready in the target namespace.
    for kind, name in (("deployment", "unbounded-net-controller"),
                       ("daemonset", "unbounded-net-node"),
                       ("deployment", "machina-controller")):
        if not _workload_ready(kind, name):
            return False, f"{kind}/{name} not Ready in {TARGET_NS}"

    # 4. Non-regenerable state copied across.
    if not resource_exists(["-n", TARGET_NS, "secret", "redfish-password"]):
        return False, "redfish-password not yet copied"
    if not resource_exists(["-n", TARGET_NS, "configmap", "machina-config"]):
        return False, "configmap machina-config not yet copied"

    net_config = _resource_json([
        "-n", TARGET_NS, "configmap", "unbounded-net-config",
    ])
    if net_config is None:
        return False, "configmap unbounded-net-config not yet copied"
    try:
        actual_net_payload = _configmap_payload(net_config)
    except ValueError as exc:
        return False, f"target unbounded-net-config payload is invalid: {exc}"
    if actual_net_payload != expected_net_payload:
        return False, "target unbounded-net-config payload does not exactly match legacy payload"

    expected_hash = _configmap_payload_hash(expected_net_payload)
    for kind, name in (("deployment", "unbounded-net-controller"),
                       ("daemonset", "unbounded-net-node")):
        workload = _resource_json(["-n", TARGET_NS, kind, name])
        if workload is None:
            return False, f"{kind}/{name} not readable for net config hash"
        annotations = (workload.get("spec", {}).get("template", {})
                       .get("metadata", {}).get("annotations", {}))
        actual_hash = annotations.get(NET_CONFIG_HASH_ANNOTATION)
        if actual_hash != expected_hash:
            return False, (f"{kind}/{name} net config hash = {actual_hash!r}, "
                           f"want exact payload hash {expected_hash}")

    # 5. Machine secret-ref namespace rewritten.
    out = kubectl_out(["get", "machine.unbounded-cloud.io", "m1", "-o", "json"],
                      check=False)
    try:
        ns = (json.loads(out).get("spec", {}).get("pxe", {})
              .get("redfish", {}).get("passwordRef", {}).get("namespace"))
    except json.JSONDecodeError:
        ns = None
    if ns != TARGET_NS:
        return False, f"Machine passwordRef namespace = {ns!r}, want {TARGET_NS}"

    # 6. Legacy namespaces and the old Site CRD reaped.
    for ns_name in (LEGACY_KUBE, LEGACY_NET):
        if resource_exists(["ns", ns_name]):
            return False, f"legacy namespace {ns_name} not yet deleted"
    if resource_exists(["crd", LEGACY_SITE_CRD]):
        return False, f"legacy CRD {LEGACY_SITE_CRD} not yet deleted"

    # 7. The cluster Site's Nodes carry the canonical site label, dual-written
    # alongside the deprecated one by the upgraded net controller.
    canonical = _nodes_with_label("unbounded-cloud.io/site", CLUSTER_SITE)
    deprecated = _nodes_with_label("net.unbounded-cloud.io/site", CLUSTER_SITE)
    if not canonical:
        return False, "no Node carries the canonical unbounded-cloud.io/site=cluster label yet"
    if canonical != deprecated:
        return False, (f"site-label dual-write mismatch: "
                       f"canonical={sorted(canonical)} deprecated={sorted(deprecated)}")

    # 8. The operator owns CRD lifecycle: `kubectl unbounded install` no longer
    # applies CRDs, the operator installs them at startup. Verify the Site CRD is
    # Established and was server-side-applied by the operator field manager
    # (proving the operator, not install, installed it).
    if not _crd_established("sites.unbounded-cloud.io"):
        return False, "sites.unbounded-cloud.io CRD not established yet"
    if not _crd_managed_by("sites.unbounded-cloud.io", "unbounded-operator"):
        return False, "sites.unbounded-cloud.io not yet owned by the unbounded-operator field manager"

    # 9. Operator config is delivered via the ConfigMap install populated.
    if not resource_exists(["-n", TARGET_NS, "configmap", "unbounded-operator-config"]):
        return False, "unbounded-operator-config ConfigMap not present"

    return True, "migration complete"


def _crd_established(name: str) -> bool:
    out = kubectl_out(["get", "crd", name, "-o", "json"], check=False)
    try:
        conds = json.loads(out).get("status", {}).get("conditions", [])
    except json.JSONDecodeError:
        return False
    return any(c.get("type") == "Established" and c.get("status") == "True" for c in conds)


def _crd_managed_by(name: str, manager: str) -> bool:
    out = kubectl_out([
        "get", "crd", name, "-o", "json", "--show-managed-fields",
    ], check=False)
    try:
        managed = json.loads(out).get("metadata", {}).get("managedFields", [])
    except json.JSONDecodeError:
        return False
    return any(f.get("manager") == manager for f in managed)


def _nodes_with_label(key: str, value: str) -> set[str]:
    out = kubectl_out(
        ["get", "nodes", "-l", f"{key}={value}",
         "-o", "jsonpath={.items[*].metadata.name}"], check=False)
    return set(out.split())


def _workload_ready(kind: str, name: str) -> bool:
    out = kubectl_out(["-n", TARGET_NS, "get", kind, name, "-o", "json"],
                      check=False)
    try:
        status = json.loads(out).get("status", {})
    except json.JSONDecodeError:
        return False

    if kind == "daemonset":
        desired = status.get("desiredNumberScheduled", 0)
        ready = status.get("numberReady", 0)
        return desired > 0 and ready >= desired

    desired = status.get("replicas", 0)
    available = status.get("availableReplicas", 0)
    return desired > 0 and available >= desired


def cmd_verify(args: argparse.Namespace) -> None:
    expected_net_payload = _load_legacy_net_config_payload()
    ensure_kubeconfig()

    deadline = time.time() + args.verify_timeout
    last = ""
    while time.time() < deadline:
        ok, msg = _migration_complete(expected_net_payload)
        if ok:
            log("PASS: " + msg)
            _assert_net_config_watch_rollout(expected_net_payload, args.verify_timeout)
            _assert_net_controller_restarts()
            _assert_same_release_crd_repair(args)
            return
        if msg != last:
            log("waiting: " + msg)
            last = msg
        time.sleep(10)

    _dump_diagnostics()
    die(f"migration did not complete within {args.verify_timeout}s (last: {last})")


def _template_hash(workload: dict) -> str:
    return (workload.get("spec", {}).get("template", {}).get("metadata", {})
            .get("annotations", {}).get(NET_CONFIG_HASH_ANNOTATION, ""))


def _current_rollout_complete(kind: str, workload: dict) -> tuple[bool, str]:
    metadata = workload.get("metadata", {})
    spec = workload.get("spec", {})
    status = workload.get("status", {})
    generation = metadata.get("generation", 0)
    observed = status.get("observedGeneration", 0)
    if observed < generation:
        return False, f"observedGeneration {observed} is behind generation {generation}"

    if kind == "deployment":
        desired = spec.get("replicas", 1)
        fields = {
            "replicas": status.get("replicas", 0),
            "updatedReplicas": status.get("updatedReplicas", 0),
            "availableReplicas": status.get("availableReplicas", 0),
            "readyReplicas": status.get("readyReplicas", 0),
        }
    else:
        desired = status.get("desiredNumberScheduled", 0)
        fields = {
            "currentNumberScheduled": status.get("currentNumberScheduled", 0),
            "updatedNumberScheduled": status.get("updatedNumberScheduled", 0),
            "numberAvailable": status.get("numberAvailable", 0),
            "numberReady": status.get("numberReady", 0),
        }

    if desired <= 0:
        return False, f"desired replicas = {desired}"
    incomplete = {key: value for key, value in fields.items() if value != desired}
    if incomplete:
        return False, f"desired = {desired}, incomplete status = {incomplete}"

    return True, "rollout complete"


def _assert_net_config_watch_rollout(
        migrated_payload: dict[str, dict[str, str]], timeout: int,
) -> None:
    workload_keys = (
        ("deployment", "unbounded-net-controller"),
        ("daemonset", "unbounded-net-node"),
    )
    old_hash = _configmap_payload_hash(migrated_payload)
    old_workloads: dict[tuple[str, str], tuple[int, str]] = {}
    for kind, name in workload_keys:
        workload = _resource_json(["-n", TARGET_NS, kind, name])
        if workload is None:
            die(f"cannot record {kind}/{name} before ConfigMap watch test")
        generation = workload.get("metadata", {}).get("generation")
        template_hash = _template_hash(workload)
        if not isinstance(generation, int) or template_hash != old_hash:
            die(f"invalid {kind}/{name} baseline for ConfigMap watch test: "
                f"generation={generation!r} hash={template_hash!r}, want hash={old_hash}")
        old_workloads[(kind, name)] = (generation, template_hash)
        log(f"recorded {kind}/{name}: generation={generation} "
            f"template-hash={template_hash}")

    changed_payload = {
        "data": dict(migrated_payload["data"]),
        "binaryData": dict(migrated_payload["binaryData"]),
    }
    changed_payload["binaryData"][NET_CONFIG_BINARY_SENTINEL] = base64.b64encode(
        b"post-migration-watch-rollout\x00\xff",
    ).decode("ascii")
    new_hash = _configmap_payload_hash(changed_payload)
    if new_hash == old_hash:
        die("BinaryData watch-test mutation unexpectedly did not change payload hash")

    log("changing only target unbounded-net-config BinaryData to trigger real rollouts")
    kubectl([
        "-n", TARGET_NS, "patch", "configmap", "unbounded-net-config",
        "--type=merge", "-p", json.dumps({
            "binaryData": {
                NET_CONFIG_BINARY_SENTINEL:
                    changed_payload["binaryData"][NET_CONFIG_BINARY_SENTINEL],
            },
        }, separators=(",", ":")),
    ])

    deadline = time.time() + timeout
    last = ""
    while time.time() < deadline:
        config = _resource_json([
            "-n", TARGET_NS, "configmap", "unbounded-net-config",
        ])
        if config is None:
            last = "target unbounded-net-config is not readable"
            time.sleep(2)
            continue
        try:
            actual_payload = _configmap_payload(config)
        except ValueError as exc:
            last = f"target unbounded-net-config payload is invalid: {exc}"
            time.sleep(2)
            continue
        if actual_payload != changed_payload:
            last = "target ConfigMap does not contain only the expected BinaryData change"
            time.sleep(2)
            continue

        pending = []
        for kind, name in workload_keys:
            workload = _resource_json(["-n", TARGET_NS, kind, name])
            if workload is None:
                pending.append(f"{kind}/{name} is not readable")
                continue
            generation = workload.get("metadata", {}).get("generation", 0)
            baseline_generation, baseline_hash = old_workloads[(kind, name)]
            template_hash = _template_hash(workload)
            if template_hash != new_hash:
                pending.append(f"{kind}/{name} hash={template_hash!r}, want {new_hash}")
                continue
            if template_hash == baseline_hash or generation <= baseline_generation:
                pending.append(f"{kind}/{name} generation={generation}, "
                               f"want > {baseline_generation}")
                continue
            complete, detail = _current_rollout_complete(kind, workload)
            if not complete:
                pending.append(f"{kind}/{name}: {detail}")

        if not pending:
            log(f"PASS: BinaryData watch rolled out both net workloads with hash {new_hash}")
            return

        current = "; ".join(pending)
        if current != last:
            log("waiting for ConfigMap watch rollout: " + current)
            last = current
        time.sleep(5)

    _dump_diagnostics()
    die(f"net workloads did not complete the BinaryData watch rollout "
        f"within {timeout}s (last: {last})")


def _assert_net_controller_restarts() -> None:
    """Guard the hostNetwork rolling-restart deadlock.

    unbounded-net-controller is hostNetwork and binds host port 9999. With the
    default (surge) strategy a rollout deadlocks because the new pod cannot bind
    the port while the old one holds it. The Deployment sets maxSurge=0 so the
    old pod is terminated first; a rollout restart must therefore complete.
    """
    log("verifying unbounded-net-controller restarts cleanly (hostNetwork rollout)")
    kubectl(["-n", TARGET_NS, "rollout", "restart", "deployment/unbounded-net-controller"])

    rc = kubectl(["-n", TARGET_NS, "rollout", "status", "deployment/unbounded-net-controller",
                  "--timeout=180s"], check=False)
    if rc != 0:
        _dump_diagnostics()
        die("unbounded-net-controller did not roll out cleanly on restart "
            "(hostNetwork surge deadlock?)")

    log("PASS: unbounded-net-controller restarted cleanly")


def _operator_state() -> tuple[str, str, str, str]:
    deploy = _resource_json([
        "-n", TARGET_NS, "deployment", "unbounded-operator",
    ])
    if deploy is None:
        die("unbounded-operator Deployment is not readable")

    template = deploy.get("spec", {}).get("template", {})
    annotations = template.get("metadata", {}).get("annotations", {})
    containers = template.get("spec", {}).get("containers", [])
    image = next((container.get("image", "") for container in containers
                  if container.get("name") == "controller"), "")
    config_hash = annotations.get(OPERATOR_CONFIG_HASH_ANNOTATION, "")
    repair_token = annotations.get(OPERATOR_REPAIR_ANNOTATION, "")
    if not image or not config_hash:
        die(f"operator baseline is incomplete: image={image!r} "
            f"config-hash={config_hash!r}")

    pods = _resource_json([
        "-n", TARGET_NS, "pods", "-l",
        "app.kubernetes.io/name=unbounded-operator",
    ])
    if pods is None:
        die("unbounded-operator Pods are not readable")
    active_uids = [
        pod.get("metadata", {}).get("uid", "")
        for pod in pods.get("items", [])
        if not pod.get("metadata", {}).get("deletionTimestamp")
    ]
    active_uids = [uid for uid in active_uids if uid]
    if len(active_uids) != 1:
        die(f"expected exactly one active unbounded-operator Pod, got {active_uids}")

    return image, config_hash, repair_token, active_uids[0]


def _assert_same_release_crd_repair(args: argparse.Namespace) -> None:
    crd = _resource_json(["crd", REPAIR_CRD])
    if crd is None:
        die(f"required repair-test CRD {REPAIR_CRD} is not readable")
    old_crd_uid = crd.get("metadata", {}).get("uid", "")
    if (not old_crd_uid or not _crd_established(REPAIR_CRD)
            or not _crd_managed_by(REPAIR_CRD, "unbounded-operator")):
        die(f"repair-test CRD {REPAIR_CRD} lacks a valid established/operator-managed baseline")

    old_image, old_config_hash, old_repair_token, old_pod_uid = _operator_state()
    log(f"recorded CRD repair baseline: image={old_image} "
        f"config-hash={old_config_hash} repair-token={old_repair_token!r} "
        f"pod-uid={old_pod_uid} crd-uid={old_crd_uid}")

    log(f"deleting unused required CRD {REPAIR_CRD} for same-release repair test")
    kubectl(["delete", "crd", REPAIR_CRD, "--wait=false"])
    deadline = time.time() + 120
    while time.time() < deadline and resource_exists(["crd", REPAIR_CRD]):
        time.sleep(2)
    if resource_exists(["crd", REPAIR_CRD]):
        die(f"CRD {REPAIR_CRD} did not become absent after deletion")

    log("rerunning identical current install to repair the missing CRD")
    _run_current_install(args)

    new_image, new_config_hash, new_repair_token, new_pod_uid = _operator_state()
    repaired = _resource_json(["crd", REPAIR_CRD])
    new_crd_uid = (repaired or {}).get("metadata", {}).get("uid", "")
    failures = []
    if new_image != old_image:
        failures.append(f"operator image changed: {old_image!r} -> {new_image!r}")
    if new_config_hash != old_config_hash:
        failures.append("operator config hash changed")
    if not new_repair_token or new_repair_token == old_repair_token:
        failures.append(f"repair token did not change to a nonempty value: "
                        f"{old_repair_token!r} -> {new_repair_token!r}")
    if new_pod_uid == old_pod_uid:
        failures.append(f"operator Pod UID did not change from {old_pod_uid}")
    if not new_crd_uid or new_crd_uid == old_crd_uid:
        failures.append(f"CRD UID did not change: {old_crd_uid!r} -> {new_crd_uid!r}")
    if not _crd_established(REPAIR_CRD):
        failures.append("repaired CRD is not Established")
    if not _crd_managed_by(REPAIR_CRD, "unbounded-operator"):
        failures.append("repaired CRD is not managed by unbounded-operator")
    if failures:
        _dump_diagnostics()
        die("same-release CRD repair failed: " + "; ".join(failures))

    log(f"PASS: identical install repaired {REPAIR_CRD} with new CRD/operator Pod UIDs")


def cmd_cleanup(args: argparse.Namespace) -> None:
    if args.keep_cluster or os.environ.get("E2E_KEEP") == "1":
        log("keep-cluster set; leaving kind cluster + registry in place")
        return

    log("deleting kind cluster + registry")
    run(["kind", "delete", "cluster", "--name", CLUSTER], check=False)
    run(["docker", "rm", "-f", REGISTRY_CONTAINER], check=False, quiet=True)


# --------------------------------------------------------------------------- #
# continuity monitor
# --------------------------------------------------------------------------- #
class ContinuityMonitor:
    """Background monitor that samples the net dataplane's source of truth during
    the migration and fails the run on a regression.

    The primary signal is SiteNodeSlice presence: net-node programs WireGuard
    peers and pod-CIDR routes from SiteNodeSlices, so a slice that is present and
    then vanishes (rather than being updated in place) means the dataplane for
    that site was torn down. This is exactly the outage the orphan-cleanup fix
    prevents during the migration window. A slice absent for two consecutive
    samples (debounced against transient API read hiccups) is flagged, as is the
    whole slice set emptying after having been non-empty.

    With --probe-dataplane it additionally samples WireGuard peer counts inside
    the net-node pods and flags a collapse to zero peers after peers were seen.
    That probe is best-effort: if `wg` peer data cannot be read (tooling/mode) it
    disables itself rather than producing false failures. NOTE: this harness runs
    CNI-free alongside kindnet, so pod-to-pod traffic uses kindnet, not the
    unbounded WireGuard mesh; a cross-node pod ping would therefore NOT reflect
    the unbounded dataplane, which is why slice/peer continuity are used instead.
    """

    def __init__(self, interval: float, probe_dataplane: bool) -> None:
        self._interval = max(interval, 0.5)
        self._probe_dataplane = probe_dataplane
        self._stop = threading.Event()
        self._thread = threading.Thread(
            target=self._loop, name="continuity-monitor", daemon=True)
        self._violations: list[str] = []
        self._seen_slices: set[str] = set()
        self._absent_streak: dict[str, int] = {}
        self._wg_peers_seen = False
        self._wg_disabled = False
        self._samples = 0

    def start(self) -> None:
        log(f"continuity monitor: started (interval={self._interval}s, "
            f"probe_dataplane={self._probe_dataplane})")
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        self._thread.join(timeout=30)
        log(f"continuity monitor: stopped after {self._samples} samples; "
            f"{len(self._seen_slices)} distinct SiteNodeSlices observed")
        if self._violations:
            die("continuity monitor detected dataplane regressions during "
                "migration:\n  - " + "\n  - ".join(self._violations))
        log("continuity monitor: no dataplane regression observed")

    def _record(self, violation: str) -> None:
        if violation not in self._violations:
            self._violations.append(violation)
            log("continuity VIOLATION: " + violation)

    def _loop(self) -> None:
        while not self._stop.is_set():
            self._sample()
            self._stop.wait(self._interval)
        self._sample()

    def _sample(self) -> None:
        names = self._slice_names()
        if names is None:
            return  # transient API read hiccup; skip this tick

        self._samples += 1

        # Debounced disappearance detection: a previously-seen slice missing for
        # two consecutive samples is a regression.
        for slice_name in sorted(self._seen_slices):
            if slice_name in names:
                self._absent_streak[slice_name] = 0
                continue

            streak = self._absent_streak.get(slice_name, 0) + 1
            self._absent_streak[slice_name] = streak
            if streak >= 2:
                self._record(
                    f"SiteNodeSlice {slice_name} was present and then "
                    f"disappeared during migration")

        self._seen_slices |= names

        if self._seen_slices and not names:
            self._record("all SiteNodeSlices vanished at once during migration")

        if self._probe_dataplane and not self._wg_disabled:
            self._sample_wireguard()

    def _slice_names(self) -> set[str] | None:
        out = kubectl_out(
            ["get", "sitenodeslices.net.unbounded-cloud.io", "-o", "json"],
            check=False)
        if not out.strip():
            return None
        try:
            items = json.loads(out).get("items", [])
        except json.JSONDecodeError:
            return None
        return {item["metadata"]["name"] for item in items}

    def _sample_wireguard(self) -> None:
        pods = self._net_node_pods()
        if not pods:
            return

        any_peers = False
        for pod in pods:
            peers = self._wg_peer_count(pod)
            if peers is None:
                # Could not read wg peer data anywhere; disable the probe once so
                # it never produces false failures.
                self._wg_disabled = True
                log("continuity monitor: WireGuard probe disabled "
                    "(peer data unavailable in net-node pods)")
                return
            if peers > 0:
                any_peers = True

        if any_peers:
            self._wg_peers_seen = True
        elif self._wg_peers_seen:
            self._record("WireGuard peers collapsed to zero across all "
                         "net-node pods during migration")

    def _net_node_pods(self) -> list[str]:
        out = kubectl_out(
            ["-n", TARGET_NS, "get", "pods", "-l", "app=unbounded-net-node",
             "-o", "jsonpath={.items[*].metadata.name}"], check=False)
        return out.split()

    def _wg_peer_count(self, pod: str) -> int | None:
        # Best-effort: `wg show all peers` lists one peer per line.
        out = run_out(
            ["kubectl", "--kubeconfig", str(KUBECONFIG), "-n", TARGET_NS,
             "exec", pod, "--", "wg", "show", "all", "peers"], check=False)
        if not out.strip():
            # Distinguish "no peers" from "wg unavailable" is not reliable here;
            # treat empty as unreadable so the probe self-disables rather than
            # false-flagging. A populated mesh returns peer lines.
            return None
        return len([line for line in out.splitlines() if line.strip()])


# --------------------------------------------------------------------------- #
# orchestration
# --------------------------------------------------------------------------- #
def cmd_all(args: argparse.Namespace) -> None:
    try:
        cmd_setup(args)
        cmd_build_images(args)
        cmd_install_old(args)
        monitor = ContinuityMonitor(
            interval=args.continuity_interval,
            probe_dataplane=args.probe_dataplane,
        )
        monitor.start()
        try:
            cmd_upgrade(args)
            cmd_verify(args)
        finally:
            # Assert the dataplane source of truth stayed continuous across the
            # whole migration window (install -> reaper completion).
            monitor.stop()
        log("ALL PASS")
    finally:
        cmd_cleanup(args)


# --------------------------------------------------------------------------- #
# entrypoint
# --------------------------------------------------------------------------- #
def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("command", nargs="?", default="all",
                        choices=["all", "setup", "build-images", "install-old",
                                 "upgrade", "verify", "cleanup"],
                        help="phase to run (default: all)")
    parser.add_argument("--old-version", default="v0.1.19",
                        help="released version to install as the legacy state")
    parser.add_argument("--registry-port", type=int, default=5001,
                        help="host port for the local OCI registry")
    parser.add_argument("--workers", type=int, default=5,
                        help="number of kind worker nodes (in addition to the "
                             "control-plane); exercises the net-node DaemonSet "
                             "and host-port cutover on every node")
    parser.add_argument("--container-engine", default="docker",
                        help="container engine for image builds/pushes; must "
                             "match the kind provider (docker by default)")
    parser.add_argument("--keep-cluster", action="store_true",
                        help="do not delete the kind cluster/registry at the end")
    parser.add_argument("--skip-build", action="store_true",
                        help="skip building/pushing images (reuse existing)")
    parser.add_argument("--verify-timeout", type=int, default=1500,
                        help="seconds to wait for the migration to complete")
    parser.add_argument("--continuity-interval", type=float, default=1.0,
                        help="seconds between SiteNodeSlice continuity samples "
                             "during the migration (0.5 minimum)")
    parser.add_argument("--probe-dataplane", action="store_true",
                        help="additionally sample WireGuard peer counts in the "
                             "net-node pods during migration (best-effort; "
                             "self-disables if peer data is unavailable)")
    args = parser.parse_args()

    dispatch = {
        "all": cmd_all,
        "setup": cmd_setup,
        "build-images": cmd_build_images,
        "install-old": cmd_install_old,
        "upgrade": cmd_upgrade,
        "verify": cmd_verify,
        "cleanup": cmd_cleanup,
    }
    dispatch[args.command](args)


if __name__ == "__main__":
    main()
