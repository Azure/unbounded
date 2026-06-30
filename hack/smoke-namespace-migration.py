#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""End-to-end smoke test for the namespace-consolidation migration.

It stands up a kind cluster, stages a simulated *legacy* install (machina +
metalman in ``unbounded-kube``, net in ``unbounded-net``) plus operator-owned
state (Secrets, the machina-config ConfigMap, a metalman PXE Deployment, and
cluster-scoped CRs), exercises both migration paths, and asserts the resulting
consolidated ``unbounded-system`` state:

  * the standalone ``hack/scripts/migrate-namespace.sh`` (air-gapped / non-
    operator path), which also deletes the old namespaces; and
  * the operator's one-shot ``unbounded-operator migrate-legacy`` subcommand
    (the same migrate-then-reap logic the always-on operator runs behind
    ``--reap-legacy-resources``), which reaps operator-owned resources but
    deliberately leaves the empty legacy Namespace objects for manual deletion.

Because the real net data plane (eBPF/hostNetwork) cannot run in vanilla kind,
the staged workloads are *neutralized* to inert ``pause`` pods. This validates
the migration choreography (namespaces, Secrets, ConfigMap, RBAC, CR secret
references, sequenced teardown, idempotency) at the Kubernetes-object level,
which is exactly what each path orchestrates. Real data-plane behavior is
covered elsewhere (agent e2e) and by POC validation.

Scenarios:
  * happy            - full script migration; assert consolidated state; re-run asserts idempotency.
  * dry-run          - ``--dry-run`` makes no changes.
  * resume           - ``--keep-old-namespaces`` then a normal re-run completes the cutover.
  * release-download - ``--release`` fetches the manifests tarball over HTTP (served locally).
  * operator-reap    - operator ``migrate-legacy`` copies/rewrites/reaps and leaves the namespaces; re-run asserts idempotency.

Usage:
  python3 hack/smoke-namespace-migration.py
      [--scenario happy|dry-run|resume|release-download|operator-reap|all] [--keep-cluster]

Requires: docker, kind, kubectl, go (to render manifests / run the operator). Run from anywhere.
"""

import argparse
import base64
import functools
import http.server
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tarfile
import tempfile
import threading

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
SCRIPT = REPO_ROOT / "hack" / "scripts" / "migrate-namespace.sh"

CLUSTER = "ns-migration-e2e"
PAUSE = "registry.k8s.io/pause:3.10"

TARGET = "unbounded-system"
OLD_MACHINA = "unbounded-kube"
OLD_NET = "unbounded-net"

API_ENDPOINT = "https://test-api.example:6443"

# Net controller manifests that need a running backend / aggregated API; dropped
# in the neutered test env (irrelevant to the namespace migration itself).
NET_ADMISSION_PREFIXES = ("06-", "07-", "08-", "09-")


# ── shell helpers ──────────────────────────────────────────────────────────
def log(msg: str) -> None:
    print(f">> {msg}", flush=True)


def run(cmd, *, check=True, capture=False, stdin=None, cwd=None, env=None):
    printable = " ".join(str(c) for c in cmd)
    log(f"$ {printable}")
    proc_env = None
    if env:
        proc_env = {**os.environ, **env}
    result = subprocess.run(
        [str(c) for c in cmd],
        check=False,
        text=True,
        input=stdin,
        capture_output=capture,
        cwd=cwd,
        env=proc_env,
    )
    if check and result.returncode != 0:
        if capture:
            sys.stderr.write(result.stdout or "")
            sys.stderr.write(result.stderr or "")
        raise RuntimeError(f"command failed ({result.returncode}): {printable}")
    return result


def kubectl(*args, check=True, capture=False, stdin=None):
    return run(["kubectl", *args], check=check, capture=capture, stdin=stdin)


def kubectl_json(*args):
    res = kubectl(*args, capture=True)
    return json.loads(res.stdout)


def kubectl_apply(docs) -> None:
    payload = "\n---\n".join(json.dumps(d) for d in docs)
    kubectl("apply", "-f", "-", stdin=payload)


# ── assertions ─────────────────────────────────────────────────────────────
class Checks:
    def __init__(self, name: str):
        self.name = name
        self.failures = []

    def ok(self, cond: bool, desc: str) -> None:
        status = "PASS" if cond else "FAIL"
        print(f"   [{status}] {desc}", flush=True)
        if not cond:
            self.failures.append(desc)

    def finish(self) -> None:
        if self.failures:
            raise AssertionError(
                f"scenario '{self.name}' had {len(self.failures)} failure(s): "
                + "; ".join(self.failures)
            )
        log(f"scenario '{self.name}': all checks passed")


def ns_exists(ns: str) -> bool:
    return kubectl("get", "namespace", ns, check=False, capture=True).returncode == 0


def secret_names(ns: str):
    if not ns_exists(ns):
        return set()
    data = kubectl_json("get", "secrets", "-n", ns, "-o", "json")
    return {i["metadata"]["name"] for i in data["items"]}


# ── manifest rendering + neutralization ────────────────────────────────────
def render(component: str, namespace: str, out_dir: pathlib.Path) -> None:
    out_dir.mkdir(parents=True, exist_ok=True)
    run(
        [
            "go", "run", "./hack/cmd/render-manifests",
            "--templates-dir", f"deploy/{component}",
            "--output-dir", str(out_dir),
            "--set", f"Namespace={namespace}",
            "--set", f"ControllerImage={PAUSE}",
            "--set", f"NodeImage={PAUSE}",
            "--set", "ForceNotLeader=true",
        ],
        cwd=REPO_ROOT,
    )


def neutralize_workload(doc: dict) -> dict:
    """Turn a Deployment/DaemonSet into an inert, schedulable pause workload
    while preserving its identity (name, namespace, labels, serviceAccount)."""
    if doc.get("kind") not in ("Deployment", "DaemonSet"):
        return doc
    spec = doc["spec"]
    if doc["kind"] == "Deployment":
        spec["replicas"] = 1
    pod = spec["template"]["spec"]
    for key in ("affinity", "nodeSelector", "hostNetwork", "hostPID", "hostIPC",
                "volumes", "initContainers"):
        pod.pop(key, None)
    pod["tolerations"] = [{"operator": "Exists"}]
    for container in pod.get("containers", []):
        container["image"] = PAUSE
        container["imagePullPolicy"] = "IfNotPresent"
        container["command"] = ["/pause"]
        for key in ("args", "livenessProbe", "readinessProbe", "startupProbe",
                    "volumeMounts", "securityContext", "lifecycle", "env",
                    "ports", "resources"):
            container.pop(key, None)
    return doc


def neutralize_tree(root: pathlib.Path) -> None:
    import yaml  # local import keeps the dependency obvious

    for path in sorted(root.rglob("*.yaml")):
        if path.parent.name == "controller" and path.name.startswith(NET_ADMISSION_PREFIXES):
            path.unlink()
            continue
        docs = [d for d in yaml.safe_load_all(path.read_text()) if d]
        docs = [neutralize_workload(d) for d in docs]
        path.write_text(yaml.safe_dump_all(docs, sort_keys=False))


# ── cluster lifecycle ──────────────────────────────────────────────────────
def kind_up() -> None:
    existing = run(["kind", "get", "clusters"], capture=True).stdout.split()
    if CLUSTER in existing:
        log(f"reusing existing kind cluster '{CLUSTER}'")
    else:
        run(["kind", "create", "cluster", "--name", CLUSTER, "--wait", "120s"])
    # Best-effort preload of the pause image. kind nodes already ship a pause
    # image and can pull registry.k8s.io/pause on demand (imagePullPolicy
    # IfNotPresent), so a failed preload is not fatal.
    run(["docker", "pull", PAUSE], check=False)
    run(["kind", "load", "docker-image", PAUSE, "--name", CLUSTER], check=False)
    kubectl("apply", "-f", str(REPO_ROOT / "deploy" / "machina" / "crd"))
    kubectl("apply", "-f", str(REPO_ROOT / "deploy" / "net" / "crd"))


def kind_down() -> None:
    run(["kind", "delete", "cluster", "--name", CLUSTER], check=False)


def reset_state() -> None:
    """Return the cluster to a clean (CRDs-only) slate between scenarios."""
    kubectl("delete", "machine", "--all", check=False)
    kubectl("delete", "machineoperationcredential", "--all", check=False)
    for ns in (TARGET, OLD_MACHINA, OLD_NET):
        kubectl("delete", "namespace", ns, "--ignore-not-found", "--wait=true",
                "--timeout=120s", check=False)


# ── staging the legacy install ─────────────────────────────────────────────
def opaque_secret(name: str, ns: str, data: dict) -> dict:
    return {
        "apiVersion": "v1", "kind": "Secret", "type": "Opaque",
        "metadata": {"name": name, "namespace": ns},
        "data": {k: base64.b64encode(v.encode()).decode() for k, v in data.items()},
    }


def stage_legacy(workdir: pathlib.Path) -> None:
    log("staging legacy install (unbounded-kube + unbounded-net)")
    machina_dir = workdir / "old-machina"
    net_dir = workdir / "old-net"
    render("machina", OLD_MACHINA, machina_dir)
    render("net", OLD_NET, net_dir)
    neutralize_tree(machina_dir)
    neutralize_tree(net_dir)
    kubectl("apply", "-R", "-f", str(machina_dir))
    kubectl("apply", "-R", "-f", str(net_dir))

    # Operator machina-config carrying the per-cluster API endpoint.
    kubectl_apply([{
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": "machina-config", "namespace": OLD_MACHINA},
        "data": {"config.yaml": f"apiServerEndpoint: {API_ENDPOINT}\n"},
    }])

    # Metalman PXE Deployment (operator-created via deploy-pxe).
    kubectl_apply([neutralize_workload({
        "apiVersion": "apps/v1", "kind": "Deployment",
        "metadata": {
            "name": "metalman-controller-dc1", "namespace": OLD_MACHINA,
            "labels": {"app": "unbounded-pxe", "unbounded-cloud.io/site": "dc1"},
        },
        "spec": {
            "replicas": 1,
            "selector": {"matchLabels": {"app": "unbounded-pxe", "unbounded-cloud.io/site": "dc1"}},
            "template": {
                "metadata": {"labels": {"app": "unbounded-pxe", "unbounded-cloud.io/site": "dc1"}},
                "spec": {
                    "serviceAccountName": "metalman-controller",
                    "containers": [{"name": "metalman", "image": "metalman:latest",
                                    "args": ["serve-pxe", "--site=dc1"]}],
                },
            },
        },
    })])

    # Operator-owned Secrets (precious) + auto/regenerable ones (must be skipped).
    kubectl_apply([
        opaque_secret("ssh-dc1", OLD_MACHINA, {"ssh-privatekey": "FAKEKEY"}),
        opaque_secret("azure-creds", OLD_MACHINA, {"clientID": "abc"}),
        opaque_secret("bmc-secret", OLD_MACHINA, {"password": "hunter2"}),
        {
            "apiVersion": "v1", "kind": "Secret",
            "type": "kubernetes.io/dockerconfigjson",
            "metadata": {"name": "unbounded-net-acr-pull", "namespace": OLD_NET},
            "data": {".dockerconfigjson": base64.b64encode(b"{}").decode()},
        },
        {
            "apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls",
            "metadata": {"name": "unbounded-net-serving-cert", "namespace": OLD_NET},
            "data": {"tls.crt": base64.b64encode(b"x").decode(),
                     "tls.key": base64.b64encode(b"y").decode()},
        },
        {
            "apiVersion": "v1", "kind": "Secret",
            "type": "kubernetes.io/service-account-token",
            "metadata": {
                "name": "stale-sa-token", "namespace": OLD_NET,
                "annotations": {"kubernetes.io/service-account.name": "default"},
            },
            "data": {"token": base64.b64encode(b"t").decode()},
        },
    ])

    # Cluster-scoped CRs: survival witnesses + enforced secret references.
    kubectl_apply([
        {
            "apiVersion": "unbounded-cloud.io/v1alpha3", "kind": "Machine",
            "metadata": {"name": "m1"},
            "spec": {"pxe": {
                "image": "pxe:latest",
                "redfish": {
                    "url": "https://bmc.example",
                    "username": "admin",
                    "passwordRef": {"name": "bmc-secret", "namespace": OLD_MACHINA},
                },
            }},
        },
        {
            "apiVersion": "unbounded-cloud.io/v1alpha3", "kind": "MachineOperationCredential",
            "metadata": {"name": "cred1"},
            "spec": {
                "provider": "azure", "siteName": "dc1",
                "auth": {"mode": "ExternalPlugin",
                         "secretRef": {"name": "azure-creds", "namespace": OLD_MACHINA}},
            },
        },
    ])

    kubectl("-n", OLD_MACHINA, "rollout", "status", "deploy/machina-controller", "--timeout=120s")
    kubectl("-n", OLD_NET, "rollout", "status", "deploy/unbounded-net-controller", "--timeout=120s")


def build_new_manifests(workdir: pathlib.Path) -> pathlib.Path:
    manifests = workdir / "new"
    render("machina", TARGET, manifests / "machina")
    render("net", TARGET, manifests / "net")
    neutralize_tree(manifests / "machina")
    neutralize_tree(manifests / "net")
    return manifests


def run_migration(manifests: pathlib.Path, *extra) -> subprocess.CompletedProcess:
    return run(["bash", str(SCRIPT), "--manifests-dir", str(manifests), "--yes", *extra])


def stage_target_install(workdir: pathlib.Path) -> None:
    """Deploy a neutralized, Ready unbounded-system install so the operator
    reaper's per-component health gate can pass. In production the operator
    creates these from Sites; the e2e pre-stages them (as inert pause workloads)
    to keep the reaper choreography deterministic in vanilla kind."""
    manifests = build_new_manifests(workdir)

    # Drop the rendered machina-config so the reaper must copy the operator's
    # live one (carrying apiServerEndpoint) from the legacy namespace, rather
    # than create-if-absent skipping over a pre-existing target ConfigMap.
    rendered_config = manifests / "machina" / "03-config.yaml"
    if rendered_config.exists():
        rendered_config.unlink()

    kubectl("apply", "-R", "-f", str(manifests / "machina"))
    kubectl("apply", "-R", "-f", str(manifests / "net"))
    kubectl("-n", TARGET, "rollout", "status", "deploy/machina-controller", "--timeout=120s")
    kubectl("-n", TARGET, "rollout", "status", "deploy/unbounded-net-controller", "--timeout=120s")
    kubectl("-n", TARGET, "rollout", "status", "ds/unbounded-net-node", "--timeout=120s")


def run_operator_migration(*extra) -> subprocess.CompletedProcess:
    """Invoke the operator's one-shot `migrate-legacy` subcommand against the
    current kube context (the same migrate-then-reap logic the always-on
    operator runs behind --reap-legacy-resources)."""
    return run(
        ["go", "run", "./cmd/unbounded-operator", "migrate-legacy", "--timeout=4m", *extra],
        cwd=REPO_ROOT,
    )


class _QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *_args):  # silence per-request logging
        pass


def serve_dir(root: pathlib.Path):
    handler = functools.partial(_QuietHandler, directory=str(root))
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    return httpd, httpd.server_address[1]


# ── scenarios ──────────────────────────────────────────────────────────────
def assert_consolidated(c: Checks) -> None:
    c.ok(not ns_exists(OLD_MACHINA), f"{OLD_MACHINA} namespace deleted")
    c.ok(not ns_exists(OLD_NET), f"{OLD_NET} namespace deleted")
    c.ok(ns_exists(TARGET), f"{TARGET} namespace present")

    secrets = secret_names(TARGET)
    for name in ("ssh-dc1", "azure-creds", "bmc-secret", "unbounded-net-acr-pull"):
        c.ok(name in secrets, f"operator secret '{name}' copied to {TARGET}")
    c.ok("unbounded-net-serving-cert" not in secrets,
         "regenerable serving-cert secret NOT copied")
    c.ok("stale-sa-token" not in secrets, "service-account-token secret NOT copied")

    cm = kubectl_json("get", "configmap", "machina-config", "-n", TARGET, "-o", "json")
    c.ok(API_ENDPOINT in cm["data"]["config.yaml"],
         "machina-config apiServerEndpoint preserved")

    cred = kubectl_json("get", "machineoperationcredential", "cred1", "-o", "json")
    c.ok(cred["spec"]["auth"]["secretRef"]["namespace"] == TARGET,
         "MachineOperationCredential secretRef.namespace rewritten")

    machine = kubectl_json("get", "machine", "m1", "-o", "json")
    c.ok(machine["spec"]["pxe"]["redfish"]["passwordRef"]["namespace"] == TARGET,
         "Machine redfish passwordRef.namespace rewritten")
    c.ok(machine["metadata"]["name"] == "m1", "cluster-scoped Machine CR survived")

    for kind, name in (("deploy", "machina-controller"),
                       ("deploy", "unbounded-net-controller"),
                       ("ds", "unbounded-net-node"),
                       ("deploy", "metalman-controller-dc1")):
        rc = kubectl("get", kind, name, "-n", TARGET, check=False, capture=True).returncode
        c.ok(rc == 0, f"{kind}/{name} present in {TARGET}")


def scenario_happy(workdir: pathlib.Path) -> None:
    log("=== scenario: happy path + idempotency ===")
    stage_legacy(workdir)
    manifests = build_new_manifests(workdir)

    run_migration(manifests)
    c = Checks("happy")
    assert_consolidated(c)

    log("re-running migration (idempotency: old namespaces already gone)")
    run_migration(manifests)  # exits 0 at preflight: nothing to migrate
    assert_consolidated(c)
    c.finish()


def scenario_dry_run(workdir: pathlib.Path) -> None:
    log("=== scenario: --dry-run makes no changes ===")
    stage_legacy(workdir)
    manifests = build_new_manifests(workdir)

    run_migration(manifests, "--dry-run")

    c = Checks("dry-run")
    c.ok(ns_exists(OLD_MACHINA), f"{OLD_MACHINA} still present after dry-run")
    c.ok(ns_exists(OLD_NET), f"{OLD_NET} still present after dry-run")
    c.ok("ssh-dc1" not in secret_names(TARGET), "no secrets copied during dry-run")
    cred = kubectl_json("get", "machineoperationcredential", "cred1", "-o", "json")
    c.ok(cred["spec"]["auth"]["secretRef"]["namespace"] == OLD_MACHINA,
         "CR secretRef untouched during dry-run")
    c.finish()


def scenario_resume(workdir: pathlib.Path) -> None:
    log("=== scenario: --keep-old-namespaces then resume to completion ===")
    stage_legacy(workdir)
    manifests = build_new_manifests(workdir)

    run_migration(manifests, "--keep-old-namespaces")
    c = Checks("resume")
    c.ok(ns_exists(TARGET), f"{TARGET} created on first pass")
    c.ok(ns_exists(OLD_MACHINA), f"{OLD_MACHINA} retained with --keep-old-namespaces")
    c.ok("ssh-dc1" in secret_names(TARGET), "secrets copied on first pass")

    log("resuming without --keep-old-namespaces (idempotent re-apply + decommission)")
    run_migration(manifests)
    assert_consolidated(c)
    c.finish()


def scenario_release_download(workdir: pathlib.Path) -> None:
    log("=== scenario: --release downloads the manifests tarball over HTTP ===")
    stage_legacy(workdir)
    manifests = build_new_manifests(workdir)

    # Package the manifests exactly like `make release-manifests`: a tarball
    # whose single top-level dir is unbounded-manifests-<tag>/, served from
    # <root>/<repo>/releases/download/<tag>/<asset> so the script's curl URL
    # (<base>/<repo>/releases/download/<tag>/<asset>) resolves locally.
    tag = "v0.0.0-smoke"
    asset = f"unbounded-manifests-{tag}.tar.gz"
    srv_root = workdir / "srv"
    asset_dir = srv_root / "Azure" / "unbounded" / "releases" / "download" / tag
    asset_dir.mkdir(parents=True)
    with tarfile.open(asset_dir / asset, "w:gz") as tar:
        tar.add(manifests, arcname=f"unbounded-manifests-{tag}")

    httpd, port = serve_dir(srv_root)
    try:
        run(["bash", str(SCRIPT), "--release", tag, "--yes"],
            env={"UNBOUNDED_RELEASE_BASE_URL": f"http://127.0.0.1:{port}"})
    finally:
        httpd.shutdown()

    c = Checks("release-download")
    assert_consolidated(c)
    c.finish()


def assert_operator_consolidated(c: Checks) -> None:
    """Operator-driven end state. Unlike the script, the operator NEVER deletes
    the legacy Namespace objects: it reaps only the operator-owned resources and
    leaves the (now-empty) namespaces for a human to delete."""
    c.ok(ns_exists(OLD_MACHINA), f"{OLD_MACHINA} namespace retained (operator must not delete namespaces)")
    c.ok(ns_exists(OLD_NET), f"{OLD_NET} namespace retained (operator must not delete namespaces)")
    c.ok(ns_exists(TARGET), f"{TARGET} namespace present")

    # Operator-owned legacy workloads are reaped from the legacy namespaces.
    for kind, name, namespace in (
        ("deploy", "machina-controller", OLD_MACHINA),
        ("deploy", "metalman-controller-dc1", OLD_MACHINA),
        ("deploy", "unbounded-net-controller", OLD_NET),
        ("ds", "unbounded-net-node", OLD_NET),
    ):
        rc = kubectl("get", kind, name, "-n", namespace, check=False, capture=True).returncode
        c.ok(rc != 0, f"legacy {kind}/{name} reaped from {namespace}")

    # Non-regenerable state copied into the target; regenerable/auto skipped.
    secrets = secret_names(TARGET)
    for name in ("ssh-dc1", "azure-creds", "bmc-secret", "unbounded-net-acr-pull"):
        c.ok(name in secrets, f"operator secret '{name}' copied to {TARGET}")
    c.ok("unbounded-net-serving-cert" not in secrets, "regenerable serving-cert secret NOT copied")
    c.ok("stale-sa-token" not in secrets, "service-account-token secret NOT copied")

    cm = kubectl_json("get", "configmap", "machina-config", "-n", TARGET, "-o", "json")
    c.ok(API_ENDPOINT in cm["data"]["config.yaml"], "machina-config apiServerEndpoint preserved")

    cred = kubectl_json("get", "machineoperationcredential", "cred1", "-o", "json")
    c.ok(cred["spec"]["auth"]["secretRef"]["namespace"] == TARGET,
         "MachineOperationCredential secretRef.namespace rewritten")
    machine = kubectl_json("get", "machine", "m1", "-o", "json")
    c.ok(machine["spec"]["pxe"]["redfish"]["passwordRef"]["namespace"] == TARGET,
         "Machine redfish passwordRef.namespace rewritten")

    # The freshly-staged target workloads must be left intact.
    for kind, name in (("deploy", "machina-controller"),
                       ("deploy", "unbounded-net-controller"),
                       ("ds", "unbounded-net-node")):
        rc = kubectl("get", kind, name, "-n", TARGET, check=False, capture=True).returncode
        c.ok(rc == 0, f"target {kind}/{name} present in {TARGET}")


def scenario_operator_reap(workdir: pathlib.Path) -> None:
    log("=== scenario: operator-driven migrate-then-reap (one-shot subcommand) ===")
    stage_legacy(workdir)
    stage_target_install(workdir)

    run_operator_migration()
    c = Checks("operator-reap")
    assert_operator_consolidated(c)

    log("re-running operator migration (idempotency: legacy already reaped)")
    run_operator_migration()
    assert_operator_consolidated(c)
    c.finish()


SCENARIOS = {
    "happy": scenario_happy,
    "dry-run": scenario_dry_run,
    "resume": scenario_resume,
    "release-download": scenario_release_download,
    "operator-reap": scenario_operator_reap,
}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scenario", choices=[*SCENARIOS, "all"], default="all")
    parser.add_argument("--keep-cluster", action="store_true",
                        help="do not delete the kind cluster on exit")
    args = parser.parse_args()

    for tool in ("docker", "kind", "kubectl", "go"):
        if shutil.which(tool) is None:
            log(f"required tool '{tool}' not found; skipping (treated as pass)")
            return 0

    selected = list(SCENARIOS) if args.scenario == "all" else [args.scenario]

    kind_up()
    try:
        for name in selected:
            with tempfile.TemporaryDirectory(prefix=f"nsmig-{name}-") as tmp:
                SCENARIOS[name](pathlib.Path(tmp))
            reset_state()
        log("ALL SCENARIOS PASSED")
        return 0
    finally:
        if not args.keep_cluster:
            kind_down()


if __name__ == "__main__":
    sys.exit(main())
