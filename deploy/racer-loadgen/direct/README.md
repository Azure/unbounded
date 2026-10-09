# Direct Racer UDS load generator

This alternative to the parent Gantry example reuses its DaemonSet with a patch.
It creates a dedicated, cluster-scoped `ClusterVolume/racer-loadgen` with
`spec.type: Cache`, runs direct Racer SDK reads and a synthetic UDS origin on each
selected Linux node, and exposes
`racer-loadgen-metrics:9090`. It does not deploy or require Gantry, an OCI origin
Service, or a Gantry Service. The parent deployment files are unchanged.

## Prerequisites

- Install and configure the Unbounded operator, including the
  `racer.unbounded-cloud.io/v1alpha1` ClusterVolume CRD. Any ClusterVolume triggers
  operator-managed Racer installation. Wait for the controller, node dataplane,
  installation identity/trust, and required admission policies before starting load.
- Configure Racer's node storage and capacity before starting load. The volume
  requires immutable `spec.type: Cache`; only Cache volumes enter the cache catalog.
  Capacity is not configured through this object. It is not a PVC or a durability
  promise. The catalog is 128 independent 64 MiB blobs (8 GiB of logical content), not an 8 GiB
  in-memory allocation in each load generator. Racer needs its own storage and
  resource budget, separate from these pods.
- Ensure Racer runs on every node selected by this DaemonSet and will provide
  `/run/racer/racer-loadgen/client/socket`. Match node selectors/tolerations to
  Racer's placement if it does not run on every schedulable Linux node.
- The `unbounded-system` namespace must exist. The deploying identity must be
  allowed by RBAC and admission to create ClusterVolumes and this workload. Pod
  admission must permit UID 0 and the narrowly scoped writable hostPath below;
  this is not compatible with the Restricted Pod Security Standard. Do not
  disable cluster-wide admission protections to run the example.
- Build/publish a `racer-loadgen` image that includes the UDS backend and generic
  blob flags, and make it accessible from all selected nodes.

## Render and deploy

Run from the repository root. Edit the `images` entry in
`deploy/racer-loadgen/direct/kustomization.yaml` to use your published image, for
example (replace the registry, repository, and tag with your own):

```yaml
images:
  - name: racer-loadgen
    newName: registry.example.com/bench/racer-loadgen
    newTag: uds-v1
```

This variant references `../daemonset.yaml` directly to leave the parent
deployment unchanged. Kustomize requires `--load-restrictor LoadRestrictionsNone`
for that reference. Use it only with this trusted local checkout; plain
`kubectl apply -k deploy/racer-loadgen/direct` will not work. Render locally,
review the result, then apply the rendered stream explicitly:

```sh
kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/racer-loadgen/direct
kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/racer-loadgen/direct | kubectl apply -f -
kubectl -n unbounded-system rollout status daemonset/racer-loadgen --timeout=5m
kubectl -n unbounded-system port-forward service/racer-loadgen-metrics 9090:9090
```

Metrics are at `http://127.0.0.1:9090/metrics` during port forwarding. The inherited
Prometheus pod annotations, startup/readiness probes (`/readyz`), and liveness
probe (`/healthz`) remain on port 9090. Startup includes catalog hashing, so retain
the startup probe allowance. Readiness establishes this process's origin, not
successful Racer reads; inspect load metrics/logs for acquisition failures.

Use **either** the parent example **or** this variant in a namespace: they share
the DaemonSet name and selector, so applying this variant replaces its pod
configuration. Kustomize omits the parent Services; applying does not prune old
Services from a previous deployment. Prefer a fresh example deployment rather
than assuming an in-place apply cleans up those resources.

## Workload and memory

The patch uses `--backend=uds --volume=racer-loadgen --catalog-blobs=128
--blob-bytes=67108864 --concurrency=8 --blob-concurrency=1` with verification and
the shuffle profile. Keep the **same image version, seed, blob count, and blob
size on every node** so all origins serve the same catalog. Avoid mixed catalog
versions during a rolling update; coordinate catalog changes across the nodes.
There is no `--cache` alias. Do not point this load generator at `gantry` or another
application's volume: its origin would compete for that volume's node-local origin socket.

Eight generic-blob operations mean up to eight concurrent SDK reads. Each buffers
one 16 MiB page, so page buffers alone can require 128 MiB; the 256 MiB memory
request leaves additional room for the runtime, verification, and origin work.
This is a scheduling request, not an RSS guarantee or limit. Increasing to the
CLI default of 64 workers can require 1024 MiB for page buffers alone; increase
the memory budget and account for Racer's separate resources when tuning load.

## Security and socket ownership

Only `/run/racer/racer-loadgen` is mounted read-write using hostPath
`DirectoryOrCreate`, not all of `/run/racer`. This grants access to that volume's
client and origin sockets and assumes trusted node administrators, Racer, and
load-generator images. Treat permission to mount this path as cache access.

The pod runs as UID/GID 0 with `runAsNonRoot: false`, matching Gantry's Racer UDS
ownership model. It still drops **all capabilities**, disallows privilege
escalation, uses `RuntimeDefault` seccomp, disables service account token mounts,
and keeps its root filesystem read-only. The writable socket volume is the only
exception; there is no privileged container or permission-fixing init container.

Existing socket-path directories must be real directories owned by root (the
process UID here), without group/other write permission. The origin creates
missing directories with mode `0755` and refuses unsafe paths. It does not
recursively chown/chmod the mount or alter Racer's client directory. The SDK owns
origin locking, stale-socket recovery, and cleanup; do not manually delete a live
origin socket to bypass a competing owner. The dedicated volume prevents collision
with Gantry's origin, but still permits only one origin owner per volume per node.

## Upgrade from the old resource

ClusterVolume replaces ClusterCache, with new Kubernetes UIDs even for reused
names. Old cache identities and cached data are not reused; socket paths and wire
cache terms are unchanged. Use `volume.yaml` instead of `cache.yaml` and update
the load generator to `--volume`. Follow the
[Racer upgrade guide](https://unbounded-cloud.io/guides/racer/#upgrade-from-clustercache)
for the coordinated transition. The operator does not remove the old CRD.
Deleting `clustercaches.racer.unbounded-cloud.io` manually is destructive and
deletes every old ClusterCache object, not just this example.

## Offline validation

With `kubectl`, Python 3, and PyYAML installed, run from the repository root:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 deploy/racer-loadgen/direct/render_test.py
```

The checks render both variants locally and assert resource scope, UDS arguments,
memory requests, socket mount boundaries, retained security/probes, and the
parent's Gantry configuration. They do not contact a cluster.
