#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Faithful, standalone upgrade e2e for the unbounded-operator migration.

It stands up a multi-node kind cluster (1 control-plane + N workers, default 5)
with a local OCI registry, installs the last
RELEASED multi-namespace version (default v0.1.19) via that release's real
`kubectl unbounded site init` (CNI-free so it coexists with kindnet), then
upgrades to the operator model built from the current tree via
`kubectl unbounded install`, and asserts the operator's reaper migrates
everything onto the unified `unbounded-system` namespace:

  * the pre-redesign net-group Sites are translated into machina-group Sites,
  * operator/user Secrets and the machina-config ConfigMap are copied over,
  * cluster-scoped secret references (Machine.spec.pxe.redfish.passwordRef) are
    repointed,
  * the new net + machina workloads come up Ready (real datapath, via the
    net-node hostNetwork cutover),
  * the cluster Site's Nodes carry the canonical `unbounded-cloud.io/site`
    label alongside the deprecated `net.unbounded-cloud.io/site` (the net
    controller dual-writes both during the label deprecation window),
  * unbounded-net-controller restarts cleanly (its hostNetwork Deployment uses
    maxSurge=0 so a rollout does not deadlock on host port 9999), and
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
import json
import os
import platform
import subprocess
import sys
import tarfile
import time
import urllib.request
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
WORKDIR = REPO_ROOT / "tmp" / "operator-upgrade-e2e"
KUBECONFIG = WORKDIR / "kubeconfig"

CLUSTER = "operator-upgrade-e2e"
REGISTRY_CONTAINER = "operator-upgrade-e2e-registry"

TARGET_NS = "unbounded-system"
LEGACY_KUBE = "unbounded-kube"
LEGACY_NET = "unbounded-net"
LEGACY_SITE_CRD = "sites.net.unbounded-cloud.io"
REMOTE_SITE = "edge"
CLUSTER_SITE = "cluster"

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


def cmd_install_old(args: argparse.Namespace) -> None:
    ensure_kubeconfig()

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


def cmd_upgrade(args: argparse.Namespace) -> None:
    ensure_kubeconfig()
    imgs = images(args.registry_port)

    log("building the current-tree kubectl-unbounded plugin")
    make(["kubectl-unbounded-build"])
    plugin = REPO_ROOT / "bin" / "kubectl-unbounded"
    if not plugin.exists():
        die(f"expected plugin at {plugin}")

    log("bootstrapping the operator via `kubectl unbounded install`")
    run([
        str(plugin), "install",
        "--kubeconfig", str(KUBECONFIG),
        "--operator-image", imgs["operator"],
        "--metalman-image", PAUSE_IMAGE,
        "--wait", "--timeout", "5m",
    ])

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


def _migration_complete() -> tuple[bool, str]:
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
    out = kubectl_out(["get", "crd", name, "-o", "json"], check=False)
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
    ensure_kubeconfig()

    deadline = time.time() + args.verify_timeout
    last = ""
    while time.time() < deadline:
        ok, msg = _migration_complete()
        if ok:
            log("PASS: " + msg)
            _assert_net_controller_restarts()
            return
        if msg != last:
            log("waiting: " + msg)
            last = msg
        time.sleep(10)

    _dump_diagnostics()
    die(f"migration did not complete within {args.verify_timeout}s (last: {last})")


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


def cmd_cleanup(args: argparse.Namespace) -> None:
    if args.keep_cluster or os.environ.get("E2E_KEEP") == "1":
        log("keep-cluster set; leaving kind cluster + registry in place")
        return

    log("deleting kind cluster + registry")
    run(["kind", "delete", "cluster", "--name", CLUSTER], check=False)
    run(["docker", "rm", "-f", REGISTRY_CONTAINER], check=False, quiet=True)


def cmd_all(args: argparse.Namespace) -> None:
    try:
        cmd_setup(args)
        cmd_build_images(args)
        cmd_install_old(args)
        cmd_upgrade(args)
        cmd_verify(args)
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
