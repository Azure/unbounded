#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Kind smoke test for the unbounded-storage supervisor.

The test creates a dedicated two-node kind cluster, deploys the supervisor
alongside the real unbounded-storage daemon, annotates the two kind Nodes as a
source/target benchmark pair, and waits for the cache-miss load generator to
serve reads over the libfabric TCP transport.

All kubectl calls use the named kind context created by this script. No other
configured Kubernetes cluster is read or modified.
"""

from __future__ import annotations

import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import textwrap
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parent.parent
TMPDIR = REPO_ROOT / "tmp" / "storage-supervisor-kind-smoke"
KUBECONFIG = TMPDIR / "kubeconfig"

CLUSTER = os.environ.get(
    "SMOKE_STORAGE_SUPERVISOR_KIND_CLUSTER",
    "unbounded-storage-supervisor-smoke",
)
CONTEXT = f"kind-{CLUSTER}"
NAMESPACE = os.environ.get(
    "SMOKE_STORAGE_SUPERVISOR_NAMESPACE",
    "unbounded-storage-supervisor-smoke",
)
IMAGE = os.environ.get(
    "SMOKE_STORAGE_SUPERVISOR_IMAGE",
    "localhost/unbounded-storage-supervisor-smoke:latest",
)
CONTAINER_ENGINE = os.environ.get("CONTAINER_ENGINE") or os.environ.get(
    "SMOKE_STORAGE_CONTAINER_ENGINE",
    "",
)
RECREATE_CLUSTER = os.environ.get(
    "SMOKE_STORAGE_SUPERVISOR_RECREATE_CLUSTER",
    "1",
) != "0"
KEEP_CLUSTER = os.environ.get("SMOKE_STORAGE_SUPERVISOR_KEEP_CLUSTER", "0") == "1"

FABRIC_PORT = int(os.environ.get("SMOKE_STORAGE_SUPERVISOR_FABRIC_PORT", "19001"))
METRICS_PORT = int(os.environ.get("SMOKE_STORAGE_SUPERVISOR_METRICS_PORT", "19100"))
TIMEOUT_SECONDS = int(os.environ.get("SMOKE_STORAGE_SUPERVISOR_TIMEOUT", "300"))

SUPERVISOR_BIN = Path(
    os.environ.get(
        "SMOKE_STORAGE_SUPERVISOR_BINARY",
        str(REPO_ROOT / "bin" / "unbounded-storage-supervisor"),
    )
)


@dataclass(frozen=True)
class Pod:
    name: str
    node: str
    ip: str


class PortForward:
    def __init__(self, pod: str, remote_port: int) -> None:
        self.local_port = free_port()
        env = os.environ.copy()
        env["KUBECONFIG"] = str(KUBECONFIG)
        self.proc = subprocess.Popen(
            [
                "kubectl",
                "--context",
                CONTEXT,
                "-n",
                NAMESPACE,
                "port-forward",
                f"pod/{pod}",
                f"{self.local_port}:{remote_port}",
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            env=env,
        )

    def close(self) -> None:
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
            try:
                self.proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                self.proc.wait(timeout=5)


def log(msg: str) -> None:
    print(f"[storage-supervisor-kind] {msg}", flush=True)


def die(msg: str) -> None:
    raise RuntimeError(msg)


def run(
    args: list[str],
    *,
    input_text: str | None = None,
    capture: bool = False,
    check: bool = True,
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[str]:
    log("$ " + " ".join(args))
    proc_env = os.environ.copy()
    proc_env["KUBECONFIG"] = str(KUBECONFIG)
    if env:
        proc_env.update(env)
    proc = subprocess.run(
        args,
        input=input_text,
        text=True,
        capture_output=capture,
        check=False,
        env=proc_env,
    )
    if check and proc.returncode != 0:
        if capture and proc.stdout:
            print(proc.stdout, end="")
        if capture and proc.stderr:
            print(proc.stderr, end="", file=sys.stderr)
        die(f"command failed with exit code {proc.returncode}: {' '.join(args)}")
    return proc


def kubectl(
    args: list[str],
    *,
    input_text: str | None = None,
    capture: bool = False,
    check: bool = True,
) -> subprocess.CompletedProcess[str]:
    return run(
        ["kubectl", "--context", CONTEXT, *args],
        input_text=input_text,
        capture=capture,
        check=check,
    )


def kind(args: list[str], *, capture: bool = False, check: bool = True) -> subprocess.CompletedProcess[str]:
    return run(["kind", *args], capture=capture, check=check)


def tool(name: str) -> str:
    path = shutil.which(name)
    if not path:
        die(f"required tool not found in PATH: {name}")
    return path


def container_engine() -> str:
    if CONTAINER_ENGINE:
        tool(CONTAINER_ENGINE)
        return CONTAINER_ENGINE
    for candidate in ("docker", "podman"):
        if shutil.which(candidate):
            return candidate
    die("required container engine not found in PATH: docker or podman")


def storage_tarball() -> Path:
    explicit = os.environ.get("SMOKE_STORAGE_TARBALL", "")
    if explicit:
        return Path(explicit)

    arch = os.uname().machine
    arch = {"x86_64": "amd64", "aarch64": "arm64"}.get(arch, arch)
    return REPO_ROOT / "dist" / f"unbounded-storage-linux-{arch}.tar.gz"


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def prepare_image() -> None:
    tarball = storage_tarball()
    if not SUPERVISOR_BIN.exists():
        die(f"missing supervisor binary: {SUPERVISOR_BIN}")
    if not tarball.exists():
        die(f"missing storage tarball: {tarball}")

    engine = container_engine()
    context = TMPDIR / "image"
    shutil.rmtree(context, ignore_errors=True)
    context.mkdir(parents=True, exist_ok=True)
    shutil.copy2(SUPERVISOR_BIN, context / "unbounded-storage-supervisor")
    shutil.copy2(tarball, context / tarball.name)
    (context / "Containerfile").write_text(
        textwrap.dedent(
            f"""
            FROM ubuntu:noble
            RUN apt-get update \
                && apt-get install -y --no-install-recommends ca-certificates \
                && rm -rf /var/lib/apt/lists/*
            COPY unbounded-storage-supervisor /unbounded/bin/unbounded-storage-supervisor
            ADD {tarball.name} /unbounded/storage-release/
            RUN set -eux; \
                release_dir="$(find /unbounded/storage-release -mindepth 1 -maxdepth 1 -type d | sort | head -n 1)"; \
                mkdir -p /unbounded/storage; \
                cp -a "$release_dir"/. /unbounded/storage/; \
                rm -rf /unbounded/storage-release; \
                chmod 0755 /unbounded/bin/unbounded-storage-supervisor /unbounded/storage/bin/unbounded-storage
            ENV LD_LIBRARY_PATH=/unbounded/storage/lib
            ENV PATH=/unbounded/bin:/unbounded/storage/bin:$PATH
            """
        ).lstrip(),
        encoding="utf-8",
    )

    run([engine, "build", "-t", IMAGE, "-f", str(context / "Containerfile"), str(context)])
    if engine == "docker":
        kind(["load", "docker-image", "--name", CLUSTER, IMAGE])
    else:
        archive = TMPDIR / "image.tar"
        run([engine, "save", "-o", str(archive), IMAGE])
        kind(["load", "image-archive", "--name", CLUSTER, str(archive)])


def cluster_exists() -> bool:
    proc = kind(["get", "clusters"], capture=True)
    clusters = {line.strip() for line in proc.stdout.splitlines() if line.strip()}
    return CLUSTER in clusters


def export_kubeconfig() -> None:
    kind(["export", "kubeconfig", "--name", CLUSTER, "--kubeconfig", str(KUBECONFIG)])


def ensure_cluster() -> None:
    if RECREATE_CLUSTER and cluster_exists():
        kind(["delete", "cluster", "--name", CLUSTER])

    if cluster_exists():
        export_kubeconfig()
        return

    TMPDIR.mkdir(parents=True, exist_ok=True)
    config = TMPDIR / "kind-config.yaml"
    config.write_text(
        textwrap.dedent(
            """
            kind: Cluster
            apiVersion: kind.x-k8s.io/v1alpha4
            nodes:
            - role: control-plane
            - role: worker
            """
        ).lstrip(),
        encoding="utf-8",
    )
    kind(["create", "cluster", "--name", CLUSTER, "--config", str(config), "--wait", "120s"])
    export_kubeconfig()


def manifest() -> str:
    return textwrap.dedent(
        f"""
        apiVersion: v1
        kind: Namespace
        metadata:
          name: {NAMESPACE}
        ---
        apiVersion: v1
        kind: ServiceAccount
        metadata:
          name: unbounded-storage-supervisor
          namespace: {NAMESPACE}
        ---
        apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRole
        metadata:
          name: unbounded-storage-supervisor-smoke-{CLUSTER}
        rules:
        - apiGroups: [""]
          resources: ["nodes"]
          verbs: ["list", "watch"]
        ---
        apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRoleBinding
        metadata:
          name: unbounded-storage-supervisor-smoke-{CLUSTER}
        subjects:
        - kind: ServiceAccount
          name: unbounded-storage-supervisor
          namespace: {NAMESPACE}
        roleRef:
          apiGroup: rbac.authorization.k8s.io
          kind: ClusterRole
          name: unbounded-storage-supervisor-smoke-{CLUSTER}
        ---
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: unbounded-storage-config
          namespace: {NAMESPACE}
        data:
          config.yaml: |
            version: 1
            startup:
              memory:
                no_hugepages: true
                memory_total_bytes: 67108864
              fabric:
                tcp:
                  addr: "0.0.0.0:{FABRIC_PORT}"
                progress_threads: 1
                progress_poll_us: 10
                rpc_worker_threads: 1
                max_inflight: 128
              topology:
                ignore_isolated: true
                include_node_cpu0: true
                disable_rdma: true
                serving_cores: 1
                nic_workers: 1
              metrics:
                addr: "0.0.0.0:{METRICS_PORT}"
        ---
        apiVersion: apps/v1
        kind: DaemonSet
        metadata:
          name: unbounded-storage-supervisor-e2e
          namespace: {NAMESPACE}
        spec:
          selector:
            matchLabels:
              app.kubernetes.io/name: unbounded-storage-supervisor-e2e
          template:
            metadata:
              labels:
                app.kubernetes.io/name: unbounded-storage-supervisor-e2e
            spec:
              serviceAccountName: unbounded-storage-supervisor
              hostNetwork: true
              dnsPolicy: ClusterFirstWithHostNet
              tolerations:
              - operator: Exists
              initContainers:
              - name: raise-inotify-limits
                image: {IMAGE}
                imagePullPolicy: IfNotPresent
                command: ["/bin/sh", "-c"]
                args:
                - |
                  printf 8192 > /proc/sys/fs/inotify/max_user_instances || true
                  printf 524288 > /proc/sys/fs/inotify/max_user_watches || true
                securityContext:
                  privileged: true
              containers:
              - name: supervisor
                image: {IMAGE}
                imagePullPolicy: IfNotPresent
                command: ["/bin/sh", "-c"]
                args:
                - |
                  ulimit -n 1048576 || true
                  exec /unbounded/bin/unbounded-storage-supervisor run
                env:
                - name: NODE_NAME
                  valueFrom:
                    fieldRef:
                      fieldPath: spec.nodeName
                - name: POD_NAMESPACE
                  valueFrom:
                    fieldRef:
                      fieldPath: metadata.namespace
                - name: CONFIG_PATH
                  value: /etc/unbounded-storage/config.binpb
                - name: CONFIG_SOURCE_DIR
                  value: /etc/unbounded-storage-source
                securityContext:
                  privileged: true
                volumeMounts:
                - name: rendered-config
                  mountPath: /etc/unbounded-storage
                - name: source-config
                  mountPath: /etc/unbounded-storage-source
                  readOnly: true
              - name: daemon
                image: {IMAGE}
                imagePullPolicy: IfNotPresent
                command: ["/bin/sh", "-c"]
                args:
                - |
                  ulimit -n 1048576 || true
                  while [ ! -s /etc/unbounded-storage/config.binpb ]; do sleep 0.2; done
                  ulimit -l unlimited || true
                  exec /unbounded/storage/bin/unbounded-storage --config /etc/unbounded-storage/config.binpb
                env:
                - name: LD_LIBRARY_PATH
                  value: /unbounded/storage/lib
                ports:
                - name: metrics
                  containerPort: {METRICS_PORT}
                - name: fabric
                  containerPort: {FABRIC_PORT}
                securityContext:
                  privileged: true
                volumeMounts:
                - name: rendered-config
                  mountPath: /etc/unbounded-storage
                - name: data
                  mountPath: /var/lib/unbounded-storage-e2e
              volumes:
              - name: rendered-config
                emptyDir: {{}}
              - name: data
                emptyDir: {{}}
              - name: source-config
                configMap:
                  name: unbounded-storage-config
        """
    ).lstrip()


def deploy() -> None:
    kubectl(["apply", "-f", "-"], input_text=manifest())
    kubectl(
        [
            "-n",
            NAMESPACE,
            "rollout",
            "status",
            "daemonset/unbounded-storage-supervisor-e2e",
            "--timeout=240s",
        ]
    )


def pods() -> list[Pod]:
    proc = kubectl(
        [
            "-n",
            NAMESPACE,
            "get",
            "pods",
            "-l",
            "app.kubernetes.io/name=unbounded-storage-supervisor-e2e",
            "-o",
            "json",
        ],
        capture=True,
    )
    obj = json.loads(proc.stdout)
    out: list[Pod] = []
    for item in obj.get("items", []):
        status = item.get("status", {})
        spec = item.get("spec", {})
        if status.get("phase") != "Running":
            continue
        ip = status.get("podIP", "")
        node = spec.get("nodeName", "")
        name = item.get("metadata", {}).get("name", "")
        if name and node and ip:
            out.append(Pod(name=name, node=node, ip=ip))
    return sorted(out, key=lambda p: (p.node, p.name))


def wait_for_pods() -> tuple[Pod, Pod]:
    deadline = time.time() + 180
    while time.time() < deadline:
        ready = pods()
        if len(ready) >= 2:
            return ready[0], ready[1]
        time.sleep(2)
    die("timed out waiting for two running supervisor pods")


def annotate_nodes(source: Pod, target: Pod) -> None:
    kubectl(
        [
            "label",
            "node",
            source.node,
            target.node,
            "unbounded-cloud.io/storage-ring=e2e",
            "--overwrite",
        ]
    )
    kubectl(
        [
            "annotate",
            "node",
            target.node,
            f"unbounded-cloud.io/storage-tcp.addr={target.ip}:{FABRIC_PORT}",
            "--overwrite",
        ]
    )
    kubectl(
        [
            "annotate",
            "node",
            source.node,
            "unbounded-cloud.io/storage-benchmark.scenario=tcp-cache-miss",
            f"unbounded-cloud.io/storage-benchmark.target-node={target.node}",
            f"unbounded-cloud.io/storage-tcp.addr={source.ip}:{FABRIC_PORT}",
            "unbounded-cloud.io/storage-benchmark.workers=1",
            "unbounded-cloud.io/storage-benchmark.object-count=8",
            "unbounded-cloud.io/storage-benchmark.warmup-operations=2",
            "unbounded-cloud.io/storage-benchmark.stripe-size-bytes=16384",
            "unbounded-cloud.io/storage-benchmark.object-size-bytes=16384",
            "unbounded-cloud.io/storage-benchmark.read-bytes=4096",
            "unbounded-cloud.io/storage-benchmark.verify=true",
            "unbounded-cloud.io/storage-benchmark.disk-path=/var/lib/unbounded-storage-e2e/bench-cache.bin",
            "unbounded-cloud.io/storage-benchmark.disk-size-bytes=67108864",
            "--overwrite",
        ]
    )


def scrape(port: int) -> str:
    with urllib.request.urlopen(f"http://127.0.0.1:{port}/metrics", timeout=5) as resp:
        return resp.read().decode("utf-8")


def parse_labels(raw: str) -> dict[str, str]:
    labels: dict[str, str] = {}
    if not raw:
        return labels
    for part in raw.split(","):
        key, sep, value = part.partition("=")
        if not sep:
            continue
        labels[key.strip()] = value.strip().strip('"')
    return labels


def metric_sum(text: str, name: str, want_labels: dict[str, str] | None = None) -> float:
    want_labels = want_labels or {}
    total = 0.0
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        metric, _, value = line.partition(" ")
        if not value:
            continue
        labels: dict[str, str] = {}
        metric_name = metric
        if "{" in metric and metric.endswith("}"):
            metric_name, _, rest = metric.partition("{")
            labels = parse_labels(rest[:-1])
        if metric_name != name:
            continue
        if any(labels.get(k) != v for k, v in want_labels.items()):
            continue
        try:
            total += float(value.strip())
        except ValueError:
            continue
    return total


def wait_for_benchmark(source: Pod) -> None:
    pf = PortForward(source.name, METRICS_PORT)
    try:
        deadline = time.time() + TIMEOUT_SECONDS
        last_frontend = 0.0
        last_peer = 0.0
        while time.time() < deadline:
            if pf.proc.poll() is not None:
                output = pf.proc.stdout.read() if pf.proc.stdout else ""
                die(f"kubectl port-forward exited early:\n{output}")
            try:
                metrics = scrape(pf.local_port)
            except (urllib.error.URLError, TimeoutError, OSError):
                time.sleep(2)
                continue

            last_frontend = metric_sum(
                metrics,
                "unbounded_storage_frontend_requests_total",
                {"status": "200"},
            )
            last_peer = metric_sum(
                metrics,
                "unbounded_storage_bufferpool_miss_source_total",
                {"source": "peer"},
            )
            if last_frontend > 0 and last_peer > 0:
                log(
                    "benchmark observed "
                    f"frontend_200={last_frontend} peer_misses={last_peer}"
                )
                return
            time.sleep(2)

        die(
            "timed out waiting for benchmark metrics "
            f"frontend_200={last_frontend} peer_misses={last_peer}"
        )
    finally:
        pf.close()


def diagnostics() -> None:
    log("collecting diagnostics")
    kubectl(["get", "nodes", "-o", "wide"], check=False)
    kubectl(["get", "nodes", "-o", "yaml"], check=False)
    kubectl(["-n", NAMESPACE, "get", "pods", "-o", "wide"], check=False)
    kubectl(["-n", NAMESPACE, "describe", "daemonset/unbounded-storage-supervisor-e2e"], check=False)
    kubectl(["-n", NAMESPACE, "describe", "pods"], check=False)
    for pod in pods():
        for container in ("supervisor", "daemon"):
            kubectl(
                ["-n", NAMESPACE, "logs", pod.name, "-c", container, "--tail=200"],
                check=False,
            )


def main() -> int:
    tool("kind")
    tool("kubectl")
    TMPDIR.mkdir(parents=True, exist_ok=True)

    try:
        ensure_cluster()
        prepare_image()
        deploy()
        source, target = wait_for_pods()
        log(f"source pod {source.name} on {source.node} at {source.ip}")
        log(f"target pod {target.name} on {target.node} at {target.ip}")
        annotate_nodes(source, target)
        wait_for_benchmark(source)
        return 0
    except Exception as exc:
        log(f"FAILED: {exc}")
        diagnostics()
        return 1
    finally:
        if not KEEP_CLUSTER:
            kind(["delete", "cluster", "--name", CLUSTER], check=False)


if __name__ == "__main__":
    raise SystemExit(main())
