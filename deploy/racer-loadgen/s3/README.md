# Synthetic S3 benchmark

For measured results, image pins, and known startup limitations, see the internal
[October 6, 2026 benchmark report](../../../designs/racer-s3-benchmark-2026-10-06.md).

This standalone Kustomize base replaces `DaemonSet/racer-loadgen` with two
containers: an origin-only synthetic S3 server on 8080 (metrics on 9090), and
`racer-object origin`. It preserves the existing immutable selector and
`Service/racer-loadgen-origin:8080` with node-local routing. Origins run on eligible
Linux nodes without the benchmark label; nodes labeled
`racer.unbounded-cloud.io/exclude` are excluded, matching the Racer examples.

The separate `DaemonSet/racer-s3-loadgen` runs the non-root HTTP consumer and a
root `racer-object sidecar` listening only on `127.0.0.1:8080`. Only nodes labeled
`racer-s3-benchmark=enabled` receive consumers. The default is **zero concurrency**
from the new `racer-s3-loadgen-control` ConfigMap. The old loadgen control ConfigMap
is not referenced anywhere. The origin is fixed at zero concurrency, with no live
control mount. Applying this base again resets its control value to zero.

The catalog is 16 objects of 67,108,864 bytes (1 GiB logical content), bucket
`benchmark`, seed `benchmark-v1`, with SHA-256 verification enabled. Both adapters
use `ClusterVolume/racer-object` with `spec.type: Cache` and cache-key namespace `s3-benchmark-v1` (not the
Kubernetes namespace). Keep these values and the image version identical across
all nodes. Change the cache-key namespace on both adapters when changing the
catalog to avoid mixing cached content. The synthetic fixture is unauthenticated,
read-only, and unsuitable for production S3 traffic. Its fake AWS signing values
are explicitly synthetic environment placeholders, not real credentials; do not
replace them with cloud credentials.

## Prerequisites and security

- Install Racer's CRD, controller, dataplane, trust, admission policies, and storage
  configuration first. ClusterVolume requires `spec.type: Cache`. The namespace `unbounded-system`
  must already exist; Kustomize scopes workloads but leaves ClusterVolume global.
- Ensure Racer runs on all selected nodes, including any custom tolerations.
  Creating the cache must provision `/run/racer/racer-object/client` before client
  pods can mount it. No permission-fixing init container is used.
- Adapters run as UID/GID 0 for root-owned Racer sockets. Only the origin directory
  is writable, only by the origin adapter; the sidecar mounts the client directory
  read-only (socket connections still grant cache access). The consumer has no
  socket mount. Both adapters drop all capabilities, use read-only root filesystems,
  and forbid privilege escalation. No container is privileged; service-account
  token mounting is disabled. Admission must permit these narrow hostPaths and UID
  0, so this is not compatible with Restricted Pod Security.
- CPU requests are 250m per loadgen and 100m per adapter, **without CPU limits**.
  Each container requests 128Mi memory and has a 1Gi memory limit. These are starting
  budgets, not guarantees for arbitrary concurrency. Account separately for Racer
  dataplane resources. At 1,500 nodes, do not enable all consumers at once.
- Startup catalog hashing has a 4-minute deadline and a 5-minute startup probe
  allowance. Readiness checks only loadgen initialization, not successful adapter
  or Racer reads. Adapters have no HTTP health endpoint; validate with real reads.

## Render and apply

Build/publish both images, then make a local overlay (for example under
`deploy/racer-loadgen/s3-run/`) with this `kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../s3
images:
  - name: ghcr.io/azure/racer-loadgen
    newTag: REPLACE_WITH_BUILD_SHA
  - name: ghcr.io/azure/racer-object
    newTag: REPLACE_WITH_BUILD_SHA
```

The base uses `ghcr.io/azure/racer-loadgen:dev` and
`ghcr.io/azure/racer-object:dev` only as placeholders. Pin published tags or digests
in the overlay. The images workflow accepts either Containerfile or Dockerfile,
preferring Containerfile when both exist. No relaxed Kustomize load restriction is
needed.

Before replacing a running Gantry benchmark, its operator must explicitly pause
and drain the old clients, confirm zero in-flight work, and coordinate cache
deletion. **The deletion of the old Gantry cache volume is operational,
not performed by this base**. Only after confirming no other workload needs it:

```sh
timeout --signal=TERM --kill-after=10s 300s kubectl delete clustervolume gantry --wait=true --timeout=4m
```

This deletes the cluster-scoped cache resource; do not use blanket pruning or
delete its data directories manually. Old Gantry Services/DaemonSets are not
pruned by this base either. The operator owns their separate retirement. Use only
one of the parent, direct, or S3 loadgen variants because they share the original
DaemonSet name. Review the rendered configuration, then apply:

```sh
timeout --signal=TERM --kill-after=10s 300s kubectl kustomize deploy/racer-loadgen/s3-run
timeout --signal=TERM --kill-after=10s 300s kubectl apply -k deploy/racer-loadgen/s3-run
timeout --signal=TERM --kill-after=10s 300s kubectl -n unbounded-system rollout status daemonset/racer-loadgen --timeout=4m
```

With 1,500 origins and `maxUnavailable: 1`, a full rollout may exceed one bounded
check. A timeout is not completion: inspect rollout/pod state, diagnose failures,
and retain responsibility for C0 before another bounded check. `maxSurge: 0`
prevents competing origin socket owners on one node.

## Canary, ramp, and pause

First inspect existing benchmark labels; do not assume none are set. Select one
eligible node whose Racer dataplane and new origin are ready. Replace `NODE`:

```sh
timeout --signal=TERM --kill-after=10s 300s kubectl get nodes -l racer-s3-benchmark=enabled
timeout --signal=TERM --kill-after=10s 300s kubectl label node NODE racer-s3-benchmark=enabled --overwrite
timeout --signal=TERM --kill-after=10s 300s kubectl -n unbounded-system rollout status daemonset/racer-s3-loadgen --timeout=4m
timeout --signal=TERM --kill-after=10s 300s kubectl -n unbounded-system patch configmap racer-s3-loadgen-control --type=merge -p '{"data":{"concurrency":"1"}}'
```

The directory mount (not subPath) receives projected ConfigMap updates. Kubelet
propagation is asynchronous; the loadgen polls the local file every second.
Inspect `racer_loadgen_applied_concurrency`, successful pulls, verified bytes,
failure counts, logs, and actual in-flight work on **each selected pod** before
adding more labeled nodes or increasing per-pod concurrency. Metrics are exposed
via pod scrape annotations and `Service/racer-s3-loadgen-metrics:9090`; a Service
port-forward samples only one pod, not the fleet:

```sh
timeout --signal=TERM --kill-after=10s 300s kubectl -n unbounded-system port-forward service/racer-s3-loadgen-metrics 9090:9090
```

To pause, set concurrency back to zero and confirm all selected clients have
observed C0 and drained in-flight reads (the pull deadline is 2 minutes):

```sh
timeout --signal=TERM --kill-after=10s 300s kubectl -n unbounded-system patch configmap racer-s3-loadgen-control --type=merge -p '{"data":{"concurrency":"0"}}'
```

Removing a node label deletes that node's consumer pod; pause and drain first.
The origins remain available regardless of client labels or concurrency. Live
control accepts integers 0-256, but a valid value is not evidence that its memory
or CPU demand fits. Ramp incrementally and stop on verification errors or resource
pressure. For a direct-origin comparison, patch only the consumer endpoint to
`http://racer-loadgen-origin:8080`, keep the catalog and load settings identical,
and report the different network path. Do not expose the loopback sidecar through
a Service.

## Offline validation

Requires Python 3, PyYAML, and kubectl. These tests do not contact a cluster:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 deploy/racer-loadgen/s3/render_test.py
```

Tests check resource scope, immutable selectors, placement, catalog flags, live
control isolation, socket/security boundaries, CPU limits, probes, Service ports,
and image resolution success/failure and Containerfile precedence.
