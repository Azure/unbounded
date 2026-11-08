---
title: "Racer Reference"
weight: 7
description: "Racer resources, configuration, diagnostics, Go SDK, and RDMA requirements."
---

{{< callout type="warning" >}}
Racer is pre-release. Its APIs, flags, and on-disk formats may change without a
migration path. Cached data is disposable; Racer is not durable storage. Start
with the [Azure Blob cache guide]({{< relref "guides/azure-blob-cache" >}}).
{{< /callout >}}

See [Racer concepts]({{< relref "concepts/racer" >}}) and the [Azure Blob cache guide]({{< relref "guides/azure-blob-cache" >}}).

## ClusterCache

| Property | Contract |
|----------|----------|
| API | `racer.unbounded-cloud.io/v1alpha1`, kind `ClusterCache` |
| Resource / short name | `clustercaches` / `ccache` |
| Scope | Cluster-scoped |
| Fields | Optional `spec.origin` for an Azure Blob origin that the dataplane reads directly; optional `spec.priority` for the cache's share of node capacity; optional `spec.passthrough` to proxy reads without caching; optional `spec.nodeSelector` to limit the cache to some nodes; no `status` |
| Name | DNS subdomain, at most 82 characters total and 63 per dot-separated label |
| Identity | Kubernetes UID, not name; deleting and recreating a cache creates a new identity |

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: artifacts
```

Any ClusterCache triggers installation by the Unbounded operator, not a Site
component flag. This resource is not a Kubernetes PersistentVolume or
PersistentVolumeClaim (PVC), does not set absolute capacity, and makes no
durability promise. `spec.priority` sets only its relative share of each node's
cache capacity (see [Priority](#priority)). It configures an origin only when `spec.origin` is set (see
[Azure origin](#azure-origin)). Cached data is disposable. A cache's Kubernetes UID is its
wire cache identity (`CacheID`).

| Path on the node | Owner and purpose |
|------------------|-------------------|
| `/run/racer/<name>/client/socket` | Dataplane; SDK clients connect here |
| `/run/racer/<name>/origin/socket` | Application origin adapter; dataplane calls it for upstream reads |

Pod mounts grant access: expose only the needed cache/role. Origins need existing trusted
parent directories. Clients do not automatically bypass Racer to connect directly to origins.

### Priority

`spec.priority` is a relative weight, 0 to 1000. Unset means 500. You can
change it on a live cache; the new value takes effect without a restart.

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
spec:
  priority: 900
```

Each cache's weighted share of a resource is `capacity * priority / sum of all
priorities`. Memory shares count active caches. If all counted priorities are
0, shares are equal. Shares apply to disk, the pending write queue, and memory:
plaintext (working memory held by readers) and ciphertext (the only data the
memory cache retains). Request-serving resources such as request context,
connections, and in-flight fetches are split equally between active caches
regardless of priority, so a priority 0 cache can still serve requests.
Plaintext and ciphertext also keep a per-cache working allowance of one range
window (`RACER_RANGE_WINDOW_PAGES`) plus one page, capped at an equal split.
Concurrent requests on that cache share this allowance. The cap can make it
smaller, so a read can still be rejected with Overloaded. Each I/O worker
computes shares on its own; pages are spread across workers by hash, so this
is close to a node-wide share.

A cache above its share is over share. Under pressure, eviction prefers data
from over-share caches. Scans are bounded, so this is a preference, not a
guarantee. Idle capacity is still used by any cache, so a cache can grow past
its share while others do not need the space. Priority is not a quota or a
reservation, and nodes do not coordinate.

Priority 0 has no weighted share when competing with positive-priority caches.
The cache keeps its working allowance, is preferred for eviction, and is still
cached while capacity is idle.

Memory-cache entries do not expire with time at normal memory pressure. An
entry leaves the memory cache only when its space is needed: a new read or
fill reclaims least recently used idle entries when the ciphertext budget is
full, publication keeps headroom for in-flight reads, and entries whose page
key leaves the keyring or whose cache turns on passthrough are dropped. When
`RACER_MEMORY_PRESSURE` reports elevated or critical pressure, entries not
read or written for 30 seconds are also released, and critical pressure
reclaims idle entries directly.

### Passthrough

Set `spec.passthrough` to turn off caching for a cache. Racer still serves
reads through the client socket, but proxies them to the origin and keeps no
data:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: artifacts
spec:
  passthrough:
    route: Peers
```

| `route` | Behavior |
|---------|----------|
| `Peers` (default) | Each page goes to its owner nodes through the mesh, as for a cached cache. The owner reads from its local origin. Concurrent reads of the same page anywhere in the cluster share one origin read, and many pages are in flight at once. |
| `Local` | The reading node reads from its own local origin and never asks peers. Concurrent reads of the same page on one node still share one origin read. Every node that reads needs an origin; with `spec.origin`, every dataplane node has one. |

- Pages are not written to disk and stay in memory only while concurrent
  readers share an in-flight origin read.
- Object metadata (ETag, size, expiry) is not reused across reads, so each read
  revalidates with the origin. One read still pins one object version for all
  of its pages.
- Prefetch is refused. A [prefetch](#prefetch) `PUT` returns
  `409 passthrough`, and owners refuse prefetch fills for the cache.
- `spec.priority` still weights in-flight memory use.
- With the `Local` route, `racer_passthrough_local_origin_fills_total` counts
  pages that a node read from its own origin.
- `spec.passthrough` can be added, changed, or removed at any time. When a cache
  switches to passthrough, data previously cached for it is dropped.

### Node selector

Set `spec.nodeSelector` to a label selector to choose which Racer nodes store,
serve, and read the cache, by Node labels. Unset or empty means every Racer
node.

```yaml
spec:
  nodeSelector:
    matchLabels:
      pool: gpu
```

- Client pods that use the cache can be scheduled only on matching nodes. The
  Racer admission webhook adds this restriction. A pod that sets
  `spec.nodeName` skips the scheduler, so the webhook rejects it unless that
  Node matches the selector.
- A managed `spec.origin` runs only on matching nodes.
- Changing the selector moves the cache to a new set of nodes. Cached data is
  not moved; pages are read from the origin again. Prefetch tickets on nodes
  that leave are canceled with `cache_removed`.

The controller and dataplane must run the same version. A mismatched pair
rejects the cache catalog.

### Azure origin

Set `spec.origin` to have the Racer dataplane read the cache's misses directly
from an Azure Blob container. No separate origin process runs. Leave it unset
to deploy your own origin with the [Go SDK](#go-sdk). Do not do both for one cache: when
`spec.origin` is set, the dataplane ignores the cache's origin socket. Set or
omit `spec.origin` when you create the cache: it cannot be added or removed
later. To change the origin, create a new ClusterCache, which starts with an
empty cache.

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: artifacts
spec:
  origin:
    azure:
      containerURL: https://account.blob.core.windows.net/models
      credential:
        workloadIdentity:
          clientID: 00000000-0000-0000-0000-000000000000
```

| Field | Meaning |
|-------|---------|
| `metadataTTL` | Optional. How long object metadata is reused; not negative; rounded down to whole milliseconds. Default `30s` |
| `requestTimeout` | Optional. Limit for each origin operation, including token acquisition and one retry after an authentication challenge; positive; rounded up to whole milliseconds. Default `1m` |
| `maxConcurrentRequests` | Optional. Upstream operations in flight for this cache on each node, 1 to 65536. Operations over the limit fail as overloaded rather than wait. Default `512` |
| `azure.containerURL` | Required. The one blob container the cache may read, as `<account URL>/<container>`, for example `https://account.blob.core.windows.net/models`. `http` or `https`, no credentials, query, fragment, or trailing slash. The container name follows Azure rules: 3 to 63 lowercase letters, digits, and single hyphens. Azurite-style URLs may include one account path segment. Immutable |
| `azure.credential` | Optional. Omit for anonymous reads. Set exactly one of `default` or `workloadIdentity`; either requires `https` |
| `azure.credential.default` | The node's managed identity, through the Azure Instance Metadata Service (IMDS). Optional `clientID` selects a user-assigned identity |
| `azure.credential.workloadIdentity` | Microsoft Entra Workload ID for the `racer` ServiceAccount. Requires `clientID`; optional `tenantID`, otherwise discovered from the storage account |

All caches on a node share one dataplane pod, so every credential runs as that
pod:

- `default` asks IMDS at `169.254.169.254` for a storage token. Each Racer node
  needs the identity attached.
- `workloadIdentity` uses a ServiceAccount token that the operator always
  projects into the dataplane, with audience `api://AzureADTokenExchange`, at
  `/var/run/secrets/azure/tokens/azure-identity-token`
  (`RACER_AZURE_FEDERATED_TOKEN_FILE`). The identity needs a federated credential
  with subject `system:serviceaccount:<Racer namespace>:racer`, for example
  `system:serviceaccount:unbounded-system:racer`. No webhook is needed.

Any ClusterCache can name any identity that trusts the `racer` ServiceAccount
or is attached to the nodes. Limit who can create ClusterCaches accordingly.
Tokens are cached per node and never appear in the cache catalog.

Every dataplane node needs network access to the storage account, and to IMDS
when you use `default`. With `hostNetwork` off, the pod network must allow it.

Each ClusterCache maps to one container. You cannot change
`azure.containerURL` after creation. To read a different source, create a new
ClusterCache. It gets a new UID, so it starts with an empty cache. A pod that
reads two containers needs two ClusterCaches and two sidecars.

Sidecars attach with `racer.unbounded-cloud.io/object-cache: <ClusterCache name>`.
They can read any blob in the cache's container. Racer rejects requests for any
other container with `403`.

If the spec is not valid, the operator reports `OriginInvalid`.

#### Upgrading from per-cache origin DaemonSets

Earlier versions ran a `racer-origin-<cache>` DaemonSet and ServiceAccount per
cache. The operator deletes them on upgrade, and the `racer` dataplane pod
reads the origin instead. Do these steps before you upgrade:

- **`workloadIdentity` caches.** The old federated credential trusts
  `system:serviceaccount:<Racer namespace>:racer-origin-<cache>`. That does not
  cover the new token, so reads fail until the identity also trusts the `racer`
  ServiceAccount. Add a federated credential to each identity with your cluster's
  OIDC issuer, subject `system:serviceaccount:<Racer namespace>:racer`, and
  audience `api://AzureADTokenExchange`. See
  [Private Containers]({{< relref "guides/azure-blob-cache" >}}#private-containers)
  for an `az` example. Remove the old `racer-origin-<cache>` credentials after
  the rollout.
- **`default` caches.** The dataplane gets tokens from IMDS on its own node. Attach
  the identity to every Racer node.
- **`default.secretRef` caches.** This field is no longer supported, and the API
  rejects new uses of it. Racer cannot read the
  Secret. If the authority service still sees the field, it rejects the
  whole catalog and keeps the last published one, so no cache changes take
  effect. Before you upgrade, change the cache's `credential` to
  `workloadIdentity` or `default` with an identity attached to the nodes. You
  can edit `credential` in place; only `containerURL` is immutable. Then delete
  the Secret if nothing else uses it.

## Node requirements

Run Racer on Linux nodes with working `io_uring` support and enough local disk.
Kernel and container security policy must not block `io_uring`. The dataplane
runs a privileged UID 0 container with these host paths:

| Host path | Use |
|-----------|-----|
| `/var/lib/racer/identity` | Node identity state; keep it across restarts and upgrades |
| `/var/lib/racer/slabs` | Slab files and checkpoints; must stay writable in device mode too |
| `/run/racer` | Client and origin sockets |
| `/dev`, mounted read-only at `/host/dev` | Optional [raw cache devices](#raw-device-storage) |

Every admitted connection and splice pipe is a file descriptor. At startup the
dataplane computes the descriptors its final budgets need: 1,024, plus the
node-wide `RACER_CLIENT_CONNECTIONS`, `RACER_PEER_CONNECTIONS`, and
`RACER_OUTBOUND_CONNECTIONS`, plus two per `RACER_PIPES`, plus 64 per I/O worker.
With defaults, a 16-worker node needs about 37,000. The dataplane raises its soft
`RLIMIT_NOFILE` to that amount when needed, and refuses to start (`stage=fd-limit`)
when the hard limit is lower. Raise the container runtime's `nofile` limit or lower
those budgets.

Do not turn off cluster-wide admission protections. Authorize these workloads
narrowly. RDMA is optional; see [RDMA](#rdma).

Origins that you deploy for caches without `spec.origin` run as the non-root
UID/GID 65532 and do not need privileged mode.
For each cache, the dataplane creates `/run/racer/<name>/origin` owned by
UID/GID 65532 with mode `0700`, and repairs its owner and mode on every cache
setup. Origins do not create this directory: mount it with hostPath type
`Directory`, so origin pods wait until the dataplane has created it. The origin
creates its socket with mode `0600`; the privileged dataplane can still connect.
When the dataplane takes over a directory left by an older root origin, it
re-owns that origin's leftover lock, socket, and staging entries to UID/GID 65532
instead of deleting them, so the new origin can recover them. Run exactly one origin owner per cache on every
eligible node that can supply origin reads, not only on nodes that read. A
DaemonSet with the same placement and tolerations as Racer does this. Every
origin must return the same bytes for the same key and version. Never delete a
live socket to take over from another owner.

## Node labels and annotations

Keys below use the `racer.unbounded-cloud.io/` prefix unless shown in full.

| Key | Meaning |
|-----|---------|
| Label `exclude` | Presence excludes the node from membership and dataplane placement, regardless of value |
| Label `unbounded-cloud.io/site` | Canonical Site boundary for RDMA; absent or empty means HTTP-only |
| Annotation `shares` | Positive decimal `uint32` placement weight; overrides reported shares; default 4 |
| Annotation `rdma-nics` | Administrator NIC/rail policy; see [RDMA](#rdma); explicit `[]` disables eligibility |
| Annotation `block-devices` | Startup-only regex selecting raw cache devices by `disk/by-id` basename; see [raw-device storage](#raw-device-storage) |

Each dataplane writes its shares and NIC report to its own Pod's
`racer.unbounded-cloud.io/report` annotation. Control seats do not write Node
annotations. Old `enrolled-shares`, `enrolled-rdma-nics`, and
`last-admitted-member` annotations are no longer runtime state.

Malformed membership annotations retain admitted attributes; invalid new nodes are omitted.
Site uses current labels. Invalid `block-devices` selectors instead fall back to slab files.

## Dataplane configuration

Tune `racer-settings` in the installation namespace (normally `unbounded-system`).
The operator preserves administrator data and hashes edits into managed rollouts.
Explicit workload environment wiring takes precedence over ConfigMap imports.
Numeric values are unsigned decimal strings: `268435456`, not `256Mi`.
The operator checks each value with the same per-key rules the dataplane uses at
startup, read from the same generated table: known names, enum and boolean
values, decimal syntax, range limits, the 2 MiB multiples of the GPU byte
settings, at most one form of each shared budget, and setting
`RACER_IO_WORKERS` and `RACER_CRYPTO_HELPERS` together. When a key is unknown or
a value is invalid, reconciliation fails with an error that names every bad
key, and the DaemonSet keeps its current configuration until the ConfigMap is
fixed. Limits that depend on the node, such
as CPU count or memory, are still checked only by the dataplane at startup.

### Environment variables

Every variable the dataplane accepts is listed below; the tables are generated
from the dataplane's configuration table (`cmd/racer/src/config/table.rs`).
Any other `RACER_*` variable fails startup, which catches typos and retired
names. A value outside its accepted range fails startup with an error that
names the variable, the value, and the rule. Rules that involve several
variables, such as exact worker counts fitting the thread cap, name every
variable involved.

Budgets that I/O workers share come in two forms. The `*_PER_WORKER` form,
which carries the default, is multiplied by the final I/O worker count, so
automatic sizing keeps each worker's budget constant on any core count. The
plain name is a node-wide total that is divided among the workers; setting it
can reduce the worker count when the total cannot fund every worker. Set at
most one form of each budget. Other variables marked "Per I/O worker" apply to
each worker and are never divided.

<!-- BEGIN GENERATED: racer configuration table -->

#### Control and identity

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_CLUSTER_ID` | Unset: managed mode adopts the elected cluster | Node | UUID | Standalone cluster UUID. Set together with `RACER_CONTROL_ENDPOINT`; managed deployments set neither. |
| `RACER_CONTROL_ENDPOINT` | Unset: managed mode | Node | `https://` URL | Standalone controller URL. Set together with `RACER_CLUSTER_ID`. |
| `RACER_CONTROL_TRUST_BUNDLE` | `/etc/racer/trust/ca.crt` | Node | absolute path | CA bundle that verifies the standalone controller. |
| `RACER_CONTROL_TOKEN` | Unset: `/var/run/racer-token/token` in managed mode, `/var/run/secrets/racer-control/token` in standalone mode | Node | absolute path | Projected token with the `racer-control` audience, presented to the controller and to control seats. |
| `RACER_IDENTITY_DIRECTORY` | `/var/lib/racer/identity` | Node | absolute path | Node-private persistent keys. Must not overlap any other configured path. |
| `RACER_SHARES` | `4` | Node | 1..4,294,967,295 | Reported shares; an explicit Node `shares` value wins. |

#### Threads

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_MAX_THREADS` | Unset: no cap | Node | 2..2^64-1 | Total I/O, crypto, and managed control threads. Managed deployments need at least 3. Kernel io_uring workers are not counted. |
| `RACER_IO_WORKERS` | Unset: automatic | Node | 1..65,536 | Exact I/O worker count. Requires `RACER_CRYPTO_HELPERS`; their sum must fit `RACER_MAX_THREADS`. |
| `RACER_CRYPTO_HELPERS` | Unset: automatic | Node | 1..65,536 | Exact crypto helper count, at most `RACER_IO_WORKERS`. Requires `RACER_IO_WORKERS`. |
| `RACER_ALLOW_SMT` | `false` | Node | `true`, `false` | `true` sizes from logical CPUs instead of one eligible CPU per physical core. |

#### RDMA and CPU

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_ENABLE_RDMA` | `auto` | Node | `auto`, `true`, `false` | `auto` uses native RDMA when built in and ports exist at startup; `true` reserves RDMA resources even without ports, for hotplug; `false` turns it off. |
| `RACER_RDMA_BREAKER` | `false` | Node | `true`, `false` | Per peer and rail circuit breaker, off by default: after a hard RDMA failure with a peer (link break, setup failure, grant or write error), or 3 page timeouts in a row with no RDMA success between them, pages to and from it use HTTP at once until a cooldown passes and one probe page succeeds over RDMA. Load (no grant slot or flight room) never trips it. |
| `RACER_RDMA_BREAKER_COOLDOWN_MS` | `2000` | Node | 1..3,600,000 ms | First breaker cooldown; it doubles for each consecutive trip, up to `RACER_RDMA_BREAKER_MAX_COOLDOWN_MS`, less up to a fifth as jitter. |
| `RACER_RDMA_BREAKER_MAX_COOLDOWN_MS` | `30000` | Node | 1..3,600,000 ms | Longest breaker cooldown; at least `RACER_RDMA_BREAKER_COOLDOWN_MS`. |
| `RACER_RDMA_PAGE_TIMEOUT_MS` | `5000` | Node | 1..60,000 ms | Longest one page's RDMA grant or write may take before the page goes over HTTP in the same exchange; counts a soft breaker failure. |
| `RACER_MEMORY_PRESSURE` | `true` | Node | `true`, `false` | cgroup v2 memory-pressure monitoring and reclaim. |

#### GPU reads

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_ENABLE_GPU` | `auto` | Node | `auto`, `true`, `false` | `auto` turns GPU reads on at the first `gpu-read` if a usable GPU is found; `true` sets GPUs up at startup and fails without one; `false` turns them off. |
| `RACER_GPU_STAGING_BYTES` | `268435456` (256 MiB) | Node | 2,097,152..2^64-1 bytes, multiple of 2,097,152 | Ciphertext staging memory per GPU. Below about 34 MiB the GPU does not decrypt and the CPU does. |
| `RACER_GPU_LANDING_BYTES` | `268435456` (256 MiB) | Node | 0..2^64-1 bytes, multiple of 2,097,152 | GPU memory per GPU that peers' NICs write GPU-read pages into over RDMA; `0` turns landing off. |
| `RACER_GPU_PCI_P2PDMA` | `false` | Node | `true`, `false` | `true` asks cuFile to read local NVMe into GPU memory over PCI P2PDMA instead of `nvidia-fs`. Needs cuFile 1.13 (CUDA 12.8) or later. |
| `RACER_GPU_PEER` | `auto` | Node | `auto`, `off` | `auto` lets one GPU copy another GPU's encrypted page over NVLink or PCIe peer-to-peer where the driver allows it; `off` moves such pages through host memory. |

#### Storage

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_SLAB_DIRECTORY` | `/var/lib/racer/slabs` | Node | absolute path | Slab files and checkpoints; still required and writable in device mode. |
| `RACER_DEVICE_DIRECTORY` | `/host/dev` | Node | absolute path | Device root whose `disk/by-id` directory is scanned for raw devices. |
| `RACER_DEVICE_BUSY_WAIT_SECS` | `120` | Node | 0..3,600 s | Startup bound for retrying selected devices whose exclusive open is busy (`EBUSY`); `0` skips busy devices immediately. |
| `RACER_SLAB_BYTES_PER_WORKER` | `17179869184` (16 GiB) | Per I/O worker | 1..9,223,372,036,854,775,807 bytes | File-backed slab size of each I/O worker; a multiple of `RACER_SEGMENT_BYTES`. Does not limit device-mode capacity. |
| `RACER_SEGMENT_BYTES` | `1073741824` (1 GiB) | Node | 16,797,712..9,223,372,036,854,775,807 bytes | Segment size; must fit one page plus storage overhead. |
| `RACER_FREE_SEGMENT_RESERVE_PER_WORKER` | `2` | Per I/O worker | 1..1,048,575 | Reserved free segments in each I/O worker, fewer than that worker's segments. |
| `RACER_DISK_PAGE_ENTRIES` | `65536` | Node | 1..1,048,576 | Disk page-index budget in file mode; device mode can raise it. |
| `RACER_CHECKPOINT_BYTES` | `67108864` (64 MiB) | Node | 1..536,870,912 bytes | Checkpoint working-set budget; device mode can raise it. |
| `RACER_RETAINED_SEGMENT_PERCENT` | `50` | Per I/O worker | 1..100 % | Spill-hold cap of each I/O worker, as a percentage of its segments and page-index capacity. |
| `RACER_RANGE_SPILL_PAGES` | `0` | Node | 0..1,024 | Pages past the read window that a read may fetch to local disk under a short hold; `0` disables spill. |

#### Disk retention

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_ADMISSION_MODE` | `second-sight` | Node | `disabled`, `second-sight` | Disk retention policy. Does not change request admission. |
| `RACER_ADMISSION_HISTORY_BYTES` | `4194304` (4 MiB) | Node | 32..536,870,912 bytes | Bloom history, divided among I/O workers; each needs at least 32 bytes. |
| `RACER_ADMISSION_PERIOD_SECS` | `60` | Node | 1..86,400 s | History rotation period. |

#### Admission budgets

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_WAITERS_PER_FLIGHT` | `64` | Node | 1..4,096 | Readers that may wait on one single-flight fetch. Flights times waiters is at most 1,048,576. |
| `RACER_NEIGHBOR_CONNECTIONS_PER_WORKER` | `8` | Per I/O worker | 1..1,024 | Busy connections each I/O worker keeps to one neighbor; at most the worker's outbound connections. |
| `RACER_RANGE_WINDOW_PAGES` | `4` | Node | 1..64 | Pages each read fetches ahead of delivery, in any order. |
| `RACER_HEADER_BYTES` | `32768` (32 KiB) | Node | 1,024..32,768 bytes | Largest accepted HTTP header block. |
| `RACER_ACTIVE_PATH_SEARCHES_PER_WORKER` | `8` | Per I/O worker | 1..64 | Distinct unfinished path searches in each I/O worker. |
| `RACER_RETAINED_SNAPSHOTS` | `2` | Node | 2..64 | Retained topology snapshots. |
| `RACER_PLAINTEXT_BYTES` | Unset: `RACER_PLAINTEXT_BYTES_PER_WORKER` applies | Node | 1..68,719,476,736 bytes | Plaintext admission budget for in-flight decrypts and pages held by range reads. Each worker's share must fit one page more than `RACER_RANGE_WINDOW_PAGES`. Node-wide total, divided among I/O workers. |
| `RACER_PLAINTEXT_BYTES_PER_WORKER` | `134217728` (128 MiB) | Per I/O worker | 1..68,719,476,736 bytes | Plaintext admission budget for in-flight decrypts and pages held by range reads. Each worker's share must fit one page more than `RACER_RANGE_WINDOW_PAGES`. Multiplied by the final I/O worker count. |
| `RACER_CIPHERTEXT_BYTES` | Unset: `RACER_CIPHERTEXT_BYTES_PER_WORKER` applies | Node | 1..68,719,476,736 bytes | Ciphertext admission budget, including the memory cache. Each worker's share must fit the range window plus one stored record. Node-wide total, divided among I/O workers. |
| `RACER_CIPHERTEXT_BYTES_PER_WORKER` | `134217728` (128 MiB) | Per I/O worker | 1..68,719,476,736 bytes | Ciphertext admission budget, including the memory cache. Each worker's share must fit the range window plus one stored record. Multiplied by the final I/O worker count. |
| `RACER_DIRTY_BYTES` | Unset: `RACER_DIRTY_BYTES_PER_WORKER` applies | Node | 16,777,232..68,719,476,736 bytes | Budget for pages waiting to be written to disk. Node-wide total, divided among I/O workers. |
| `RACER_DIRTY_BYTES_PER_WORKER` | `167772160` (160 MiB) | Per I/O worker | 16,777,232..68,719,476,736 bytes | Budget for pages waiting to be written to disk. Multiplied by the final I/O worker count. |
| `RACER_REGISTERED_BYTES` | Unset: `RACER_REGISTERED_BYTES_PER_WORKER` applies | Node | 1..68,719,476,736 bytes | Native RDMA page-buffer pool, used only when RDMA is enabled; then at least one page plus 16 bytes. One native thread owns the whole pool, so it is never divided. When neither variable is set, the scaled default is capped at 8 GiB node-wide. The pool is split evenly over RDMA rails, and a rail without a whole buffer is not used. Each grant holds one 18 MiB buffer until its page is consumed; `racer_rdma_free_buffers` shows what is left. Node-wide total, divided among I/O workers. |
| `RACER_REGISTERED_BYTES_PER_WORKER` | `402653184` (384 MiB) | Per I/O worker | 1..68,719,476,736 bytes | Native RDMA page-buffer pool, used only when RDMA is enabled; then at least one page plus 16 bytes. One native thread owns the whole pool, so it is never divided. When neither variable is set, the scaled default is capped at 8 GiB node-wide. The pool is split evenly over RDMA rails, and a rail without a whole buffer is not used. Each grant holds one 18 MiB buffer until its page is consumed; `racer_rdma_free_buffers` shows what is left. Multiplied by the final I/O worker count. |
| `RACER_REQUEST_CONTEXT_BYTES` | Unset: `RACER_REQUEST_CONTEXT_BYTES_PER_WORKER` applies | Node | 131,072..68,719,476,736 bytes | Request-context budget. Node-wide total, divided among I/O workers. |
| `RACER_REQUEST_CONTEXT_BYTES_PER_WORKER` | `33554432` (32 MiB) | Per I/O worker | 131,072..68,719,476,736 bytes | Request-context budget. Multiplied by the final I/O worker count. |
| `RACER_FLIGHTS` | Unset: `RACER_FLIGHTS_PER_WORKER` applies | Node | 1..65,536 | Concurrent single-flight page fetches. Node-wide total, divided among I/O workers. |
| `RACER_FLIGHTS_PER_WORKER` | `64` | Per I/O worker | 1..65,536 | Concurrent single-flight page fetches. Multiplied by the final I/O worker count. |
| `RACER_QUEUE_ENTRIES` | Unset: `RACER_QUEUE_ENTRIES_PER_WORKER` applies | Node | 2..65,536 | Queued I/O operations. Node-wide total, divided among I/O workers. |
| `RACER_QUEUE_ENTRIES_PER_WORKER` | `256` | Per I/O worker | 2..65,536 | Queued I/O operations. Multiplied by the final I/O worker count. |
| `RACER_CLIENT_CONNECTIONS` | Unset: `RACER_CLIENT_CONNECTIONS_PER_WORKER` applies | Node | 1..65,536 | Client and control connections. A quarter of each worker's share, at most three, is kept for control connections; the rest accept client connections. Peer and outbound connections have their own limits. Node-wide total, divided among I/O workers. |
| `RACER_CLIENT_CONNECTIONS_PER_WORKER` | `64` | Per I/O worker | 1..65,536 | Client and control connections. A quarter of each worker's share, at most three, is kept for control connections; the rest accept client connections. Peer and outbound connections have their own limits. Multiplied by the final I/O worker count. |
| `RACER_PEER_CONNECTIONS` | Unset: `RACER_PEER_CONNECTIONS_PER_WORKER` applies | Node | 1..65,536 | Inbound peer connections. A peer connection runs one exchange at a time and stays open between them; when every worker's share is in use the listener answers new connections with the unsigned overload head and closes them, so the sender asks elsewhere. Each connection is a file descriptor. Node-wide total, divided among I/O workers. |
| `RACER_PEER_CONNECTIONS_PER_WORKER` | `2048` | Per I/O worker | 1..65,536 | Inbound peer connections. A peer connection runs one exchange at a time and stays open between them; when every worker's share is in use the listener answers new connections with the unsigned overload head and closes them, so the sender asks elsewhere. Each connection is a file descriptor. Multiplied by the final I/O worker count. |
| `RACER_OUTBOUND_CONNECTIONS` | Unset: `RACER_OUTBOUND_CONNECTIONS_PER_WORKER` applies | Node | 1..65,536 | Outbound peer and origin connections, busy or idle. One slot per worker is kept for origin metadata when the share is at least two. Raise it when many concurrent page reads to different peers are refused as `Overloaded`. Node-wide total, divided among I/O workers. |
| `RACER_OUTBOUND_CONNECTIONS_PER_WORKER` | `64` | Per I/O worker | 1..65,536 | Outbound peer and origin connections, busy or idle. One slot per worker is kept for origin metadata when the share is at least two. Raise it when many concurrent page reads to different peers are refused as `Overloaded`. Multiplied by the final I/O worker count. |
| `RACER_PIPES` | Unset: `RACER_PIPES_PER_WORKER` applies | Node | 1..65,536 | Splice pipes. Node-wide total, divided among I/O workers. |
| `RACER_PIPES_PER_WORKER` | `16` | Per I/O worker | 1..65,536 | Splice pipes. Multiplied by the final I/O worker count. |
| `RACER_PLACEMENT_CACHE_BYTES` | Unset: `RACER_PLACEMENT_CACHE_BYTES_PER_WORKER` applies | Node | 1,024..536,870,912 bytes | Cached page placement rankings. Node-wide total, divided among I/O workers. |
| `RACER_PLACEMENT_CACHE_BYTES_PER_WORKER` | `16777216` (16 MiB) | Per I/O worker | 1,024..536,870,912 bytes | Cached page placement rankings. Multiplied by the final I/O worker count. |
| `RACER_CACHED_PATHS` | Unset: `RACER_CACHED_PATHS_PER_WORKER` applies | Node | 1..1,048,576 | Cached topology paths. Node-wide total, divided among I/O workers. |
| `RACER_CACHED_PATHS_PER_WORKER` | `128` | Per I/O worker | 1..1,048,576 | Cached topology paths. Multiplied by the final I/O worker count. |
| `RACER_PATH_CACHE_BYTES` | Unset: `RACER_PATH_CACHE_BYTES_PER_WORKER` applies | Node | 1..536,870,912 bytes | Memory for cached topology paths. Node-wide total, divided among I/O workers. |
| `RACER_PATH_CACHE_BYTES_PER_WORKER` | `8388608` (8 MiB) | Per I/O worker | 1..536,870,912 bytes | Memory for cached topology paths. Multiplied by the final I/O worker count. |
| `RACER_METADATA_ENTRIES` | Unset: `RACER_METADATA_ENTRIES_PER_WORKER` applies | Node | 1..1,048,576 | Object metadata catalog entries. Node-wide total, divided among I/O workers. |
| `RACER_METADATA_ENTRIES_PER_WORKER` | `4096` | Per I/O worker | 1..1,048,576 | Object metadata catalog entries. Multiplied by the final I/O worker count. |
| `RACER_RELAY_TRANSFERS` | Unset: `RACER_RELAY_TRANSFERS_PER_WORKER` applies | Node | 1..65,536 | Concurrent relayed transfers. Node-wide total, divided among I/O workers. |
| `RACER_RELAY_TRANSFERS_PER_WORKER` | `16` | Per I/O worker | 1..65,536 | Concurrent relayed transfers. Multiplied by the final I/O worker count. |

#### Listeners and peers

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_PEER_LISTEN` | `0.0.0.0:7443` | Node | `ip:port` | Peer listener. A bracketed IPv4 address, as Kubernetes expands `[$(POD_IP)]`, is accepted. Its port is the peer port published to members. |
| `RACER_DIAGNOSTICS_LISTEN` | `127.0.0.1:9090` | Node | `ip:port` | Diagnostics listener; must differ from the peer listener. |
| `RACER_PEER_TCP_NODELAY` | `true` | Node | `true`, `false` | TCP_NODELAY on peer sockets. `false` only reproduces the old socket behavior. |
| `RACER_OPAQUE_RELAY` | `false` | Node | `true`, `false` | Experimental opaque HTTP transit instead of materialized relay. |
| `RACER_PEER_INFLIGHT_MAX` | `4096` | Node | 1..65,536 | Peer exchanges in flight. |
| `RACER_PEER_PER_NEIGHBOR_MAX` | `512` | Node | 1..65,536 | Peer exchanges in flight to one neighbor; at most `RACER_PEER_INFLIGHT_MAX`. |
| `RACER_PEER_ADMISSION_RECOVERY_MS` | `1000` | Node | 1..60,000 ms | Longest time between one-slot recoveries of a reduced peer-exchange limit; it also recovers one slot per `limit` verified completions. |
| `RACER_PEER_RECEIVE_MAX` | `0` | Node | 0..65,536 | Concurrently served peer requests; `0` turns the receive gate off. |
| `RACER_PEER_RECEIVE_QUEUE_MAX` | `256` | Node | 1..65,536 | Peer requests queued at the receive gate. |
| `RACER_PEER_RECEIVE_WAIT_MS` | `1000` | Node | 1..30,000 ms | Longest wait at the receive gate. |
| `RACER_PAGE_HEDGE_DELAY_MS` | `100` | Node | 1..30,000 ms | Delay before a hedged second page request. |
| `RACER_PAGE_HEDGE_SLOTS` | `0` | Node | 0..32 | Concurrent hedged page requests; `0` turns hedging off. |
| `RACER_PAGE_HEDGE_BYTES` | `33554448` | Node | 0..1,073,742,336 bytes | Memory for hedged duplicates; with hedging on, at least one duplicate (two pages plus 16 bytes). |
| `RACER_REPLICA_SELECTION` | `spread` | Node | `primary`, `spread` | Which owner a non-owner asks first. `spread`: direct neighbors first, then a per-node hash spreads pages across owners. `primary`: the first-ranked owner. |

#### Origins

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_CACHE_ORIGIN_CONNECTIONS_PER_WORKER` | `32` | Per I/O worker | 1..1,024 | Origin connections per cache in each I/O worker, independent of peer connections; at most the client connections. When unset it is lowered to fit them. Also caps connections to each Azure Blob endpoint, which caches in one storage account share. |
| `RACER_ORIGIN_TRUST_BUNDLE` | `/etc/ssl/certs/ca-certificates.crt` | Node | absolute path | CA bundle for HTTPS origins and the Entra authority, loaded once at startup; if it cannot be loaded, HTTPS origins fail until restart. Empty selects the default. |
| `RACER_AZURE_AUTHORITY_HOST` | `https://login.microsoftonline.com/` | Node | `https://` URL | Microsoft Entra authority, ending in `/`. Empty selects the default. |
| `RACER_AZURE_FEDERATED_TOKEN_FILE` | Unset: workload identity is unavailable | Node | absolute path | Projected token exchanged for Entra tokens when an origin uses `workloadIdentity`. |

#### Prefetch

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_PREFETCH_MAX_BODY_BYTES` | `4194304` (4 MiB) | Node | 4,096..67,108,864 bytes | `PUT` body limit. |
| `RACER_PREFETCH_MAX_REFS` | `16384` | Node | 1..1,048,576 | Refs per `PUT`. |
| `RACER_PREFETCH_MAX_TICKETS` | `1024` | Node | 1..65,536 | Live and finished tickets, all caches. |
| `RACER_PREFETCH_MAX_TTL_SECS` | `3600` | Node | 1..86,400 s | TTL limit. |
| `RACER_PREFETCH_TICKET_RETENTION_SECS` | `300` | Node | 0..86,400 s | How long finished tickets stay visible. |
| `RACER_PREFETCH_MAX_WAIT_SECS` | `60` | Node | 1..600 s | `wait` limit. |
| `RACER_PREFETCH_DEFAULT_PENDING_BYTES` | `1073741824` (1 GiB) | Node | 16,777,216..1,125,899,906,842,624 bytes | Per-ticket in-flight bound when `max_pending_bytes` is 0; at most `RACER_PREFETCH_MAX_PENDING_BYTES`. |
| `RACER_PREFETCH_MAX_PENDING_BYTES` | `17179869184` (16 GiB) | Node | 16,777,216..1,125,899,906,842,624 bytes | Limit on `max_pending_bytes`. |
| `RACER_PREFETCH_NODE_PENDING_BYTES` | `68719476736` (64 GiB) | Node | 16,777,216..4,503,599,627,370,496 bytes | Queued and in-flight prefetch bytes; checked on each `PUT` that creates or changes a ticket; over it is `429`. |
| `RACER_PREFETCH_CONCURRENT_FILLS` | `16` | Node | 1..1,024 | Concurrent prefetch fills per owner. |
| `RACER_PREFETCH_ORIGIN_BYTES_PER_SEC` | `0` | Node | 0..1,099,511,627,776 bytes/s | Prefetch origin read rate per owner; `0` turns the limit off. |
| `RACER_PREFETCH_MAX_ATTEMPTS` | `5` | Node | 1..32 | Tries per page. |

#### Timeouts

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_REQUEST_TIMEOUT_MS` | `30000` | Node | 1..86,400,000 ms | Data request deadline. |
| `RACER_PEER_ATTEMPT_TIMEOUT_MS` | `30000` | Node | 1..86,400,000 ms | Local cap on one peer exchange; never sent to peers. |
| `RACER_READER_STALL_TIMEOUT_MS` | `10000` | Node | 1..86,400,000 ms | Longest a reader may stall; at most `RACER_REQUEST_TIMEOUT_MS`. |
| `RACER_SHUTDOWN_TIMEOUT_MS` | `30000` | Node | 1..3,600,000 ms | Drain deadline at shutdown. |

#### Diagnostics

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_PPROF_ENABLED` | `false` | Node | `true`, `false` | Serve CPU profiles on the diagnostics listener. |
| `RACER_SEND_TAG_PAIR` | Unset: off | Node | text | `sender,receiver` node UUIDs whose peer sends get a sampled AEAD tag check for diagnosis. |

#### Cluster control (managed mode)

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_NAMESPACE` | Unset: standalone mode | Node | DNS label | Pod namespace. Setting it selects managed mode and starts embedded cluster control. |
| `RACER_POD_NAME` | Unset: required in managed mode | Node | DNS label | Pod name. |
| `RACER_POD_UID` | Unset: required in managed mode | Node | UUID | Pod UID. |
| `RACER_NODE_NAME` | Unset: required in managed mode | Node | text | Kubernetes Node name. |
| `RACER_POD_IP` | Unset: required in managed mode | Node | IP address | Primary Pod IP (`status.podIP`). |
| `RACER_POD_IPS` | Unset: single stack | Node | comma-separated IP addresses | Every Pod IP (`status.podIPs`), led by `RACER_POD_IP`, at most one per family. The webhook also serves the other family. |
| `RACER_CONTROL_SEATS` | `auto` | Node | text | `auto` picks 1 to 16 seats from the cluster size; a decimal from 1 to 16 fixes the count. |
| `RACER_CONTROL_LISTEN` | `:8443` | Node | `ip:port` | Control seat listener. `:port` binds every IPv4 address. |
| `RACER_WEBHOOK_LISTEN` | `:9443` | Node | `ip:port` | Object sidecar admission webhook listener. `:port` binds every IPv4 address. |
| `RACER_OBJECT_IMAGE` | Unset: webhook serving off | Node | image reference | Image injected as the object sidecar. |
| `RACER_RETAINED_PAGE_KEYS` | `7` | Node | 1..16 | Readable page keys per cache: the active key plus older decrypt keys. Read by the authority. |
| `RACER_PAGE_BYTES` | `16777216` (16 MiB) | Node | 65,536..16,777,216 bytes | Cluster-wide logical page size, a power of two. Read by the authority and published to every node. |
| `RACER_KUBE_API_CA` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | Node | absolute path | Kubernetes API CA bundle. |
| `RACER_KUBE_API_TOKEN` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | Node | absolute path | Default-audience token, used only for the Kubernetes API. |
| `RACER_DAEMONSET` | `racer` | Node | DNS label | Dataplane DaemonSet name. |
| `RACER_SERVICE_ACCOUNT` | `racer` | Node | DNS label | Dataplane ServiceAccount name. |
| `KUBERNETES_SERVICE_HOST` | Unset: required in managed mode | Node | text | Kubernetes API host, set by the kubelet. |
| `KUBERNETES_SERVICE_PORT` | `443` | Node | 1..65,535 | Kubernetes API port, set by the kubelet. |

#### Operator

| Variable | Default | Scope | Accepted | Description |
| --- | --- | --- | --- | --- |
| `RACER_NVIDIA` | Unset: `env` | Operator | `off`, `env`, `cdi` | How the operator asks the container runtime for NVIDIA driver libraries. Read by the operator from `racer-settings`; the dataplane ignores it. |

<!-- END GENERATED: racer configuration table -->

Exact sizing requires both count settings. Their sum, plus the managed control
thread, must fit `RACER_MAX_THREADS`;
the cap is never raised implicitly. The first NUMA node in the normal placement
preference order must be known and have enough eligible CPUs for the whole split.
Other host NUMA nodes are allowed, but exact placement does not span nodes or skip
an undersized preferred node. Insufficient CPU quota or per-I/O resource budgets
fails startup rather than reducing counts. For a managed 20-I/O comparison, use
10 helpers with cap 31 or 20 helpers with cap 41. Keep all resource budgets fixed.
Omit both count settings to retain automatic sizing.
Extra helpers change available crypto execution capacity, not I/O ownership or
per-I/O budgets. They do not guarantee higher throughput; compare matched workloads
before drawing a performance conclusion.

Automatic sizing considers CPU affinity, quota, NUMA locality, and progress reserves.
Without an explicit thread cap, one unpinned control thread runs alongside the
automatically sized I/O and crypto workers. With a cap, it consumes one slot.
Node-wide budgets divide by final I/O worker count, not crypto thread count; tight budgets
can reduce workers. File-backed storage grows with I/O workers; raw-device capacity is
divided among them. Admission budgets are not RSS limits:
allocator, TLS, and filesystem cache are additional. Invalid combinations fail validation.

### Raw-device storage

Set the Node annotation `racer.unbounded-cloud.io/block-devices` to a regex over
basenames in `/host/dev/disk/by-id`, not full paths. On nodes without udev, where
`/host/dev/disk/by-id` does not exist, the selector instead matches the kernel
names in `/sys/block` (such as `nvme1n1`) and, for NVMe devices, the name udev
would give them (`nvme-<model>_<serial>`, whitespace replaced by `_`), so the
same selector works on both. The selector is limited to
1 KiB (1,024 bytes) and compiled with Rust's `regex` crate. Invalid or oversized
values fall back to file-backed slabs. Matching is **unanchored**:
use `^` and `$` to avoid unintended substring and partition matches.

Discovery runs once after startup enrollment. Changing or removing the annotation
does not change a running layout or trigger a rollout; restart the affected
dataplane to apply it. An absent/empty selector, Rust compile failure, discovery
failure, no usable matches, or too few segments for all workers' reserves falls
back to file-backed slabs. Rejected candidates are skipped with diagnostics;
remaining usable devices can still be selected. Aliases for one device are
deduplicated.

**Selected devices are overwritten except for reserved guards. There is no
general filesystem or data-signature check, formatting step, or preservation of
cache-area contents.** Racer skips
devices with child partitions, active holders, read-only state, or visible mounts.
It opens devices with `O_RDWR | O_DIRECT | O_EXCL`. A device whose open fails
with `EBUSY`, for example while a replaced dataplane is still releasing its
claim, is retried every 250 ms until `RACER_DEVICE_BUSY_WAIT_SECS` elapses;
all busy devices share that one deadline. Devices still busy at the deadline
are skipped. Other rejections, including active holders, are not retried.
These checks are not proof that a device is safe to erase. A partition itself is
allowed: a broad selector can overwrite an unmounted OS or data partition.
Select only dedicated disposable cache devices after checking each match.

The first and last **1 MiB (1,048,576 bytes)** of each device are reserved and must
already be entirely zero. Startup reads each full guard through the same held
exclusive `O_DIRECT` descriptor, using one aligned 1 MiB buffer. Nonzero bytes,
short reads, I/O errors, or unsupported alignment reject that device. Racer never
zeros the guards or bypasses partition, holder, or mount checks. Clearing old
contents is a separate operator-approved operation, not an automatic repair.
Keeping cache records out of these ends prevents cache data from looking like a
partition table or filesystem signature; it is not proof against every partition parser.

The managed privileged container mounts host `/dev` read-only at `/host/dev`.
This protects directory entries, **not device contents**: raw writes are
intentional. `RACER_SLAB_DIRECTORY` still holds `checkpoint.0` and `checkpoint.1`
and must remain writable and available.

Device mode uses `floor((device_bytes - 2 MiB) / segment_bytes)` segments per
device, at offsets `1 MiB + k * segment_bytes`. These are split among
the final I/O workers with remainder segments assigned to workers. Each device's
subsegment tail is unusable. `RACER_SLAB_BYTES_PER_WORKER` is ignored for device capacity,
but still must pass configuration validation for fallback. The normal per-worker
free-segment reserve, record headers, AEAD tags, alignment padding, and record
packing reduce payload capacity. Hard limits can reduce it further:

- At most 1,000,000 segments per worker, also bounded by signed 64-bit byte offsets.
- Each worker's page index grows to the larger of its estimated full-page capacity
  after reserves and its configured share of `RACER_DISK_PAGE_ENTRIES`, capped at
  1,000,000 entries. The estimate includes the largest record header, AEAD tag,
  and direct-I/O alignment. Small objects can exhaust the index before disk bytes.
- Checkpoint working memory grows from a conservative estimate of page entries,
  metadata entries, and segments, never below `RACER_CHECKPOINT_BYTES`, and is
  capped at 512 MiB node-wide. Encoded checkpoint files still have a 64 MiB cap.
  Oversized snapshots are skipped; recovery rejects snapshots over its budget.

Plan memory for the larger index, resident heat tracking, segment tables, and
checkpoint scratch, not just the configured admission budgets. The automatic
checkpoint budget is not an RSS limit or a guarantee that a full cache can be
checkpointed. Limit warnings explain discarded capacity and checkpoint pressure.

Checkpoints store the remaining time of each completed-page hold, measured
on the monotonic clock, so a restart re-arms holds without consulting wall
time. Time between the checkpoint and the restart does not count against a
hold, so a restored hold can outlast its original deadline by that gap. They
bind each worker to
file mode or its raw-device IDs, sizes, and segment offsets. The raw-layout
digest also includes both guard sizes. Each checkpoint candidate is
validated for the whole node: one mismatched worker rejects the entire candidate.
A changed device layout, storage mode, worker count, or incompatible
geometry rejects recovery and starts cold when no compatible checkpoint remains.
Racer does not scan payloads to rebuild the index or migrate cached data. Preserve
cluster and node identity state; it is separate from disposable cache checkpoints.

### Range reads

The dataplane always sends a read's pages to the client in order, but fetches
them in any order:

- It checks local memory for every page in the read window. A page already in
  memory is ready without waiting for earlier pages.
- It fills the next page to send and the page after it through the normal
  fill path.
- It asks peers for the other missing pages in the window. A peer sends them in
  any order it chooses. Several requests can be in flight at once, to one or
  more peers.
- Pages that arrive early wait in memory until their turn. If
  `RACER_RANGE_SPILL_PAGES` is above zero, a read can also fetch pages past the
  window to local disk under a short hold. Each hold is released after its page
  is sent or the read ends. If a hold is refused or a page fails to spill, the
  read stops spilling and uses its memory window only.
- Reads of the same version on one node share page fetches.

The window is `RACER_RANGE_WINDOW_PAGES` pages, also limited by the client's
unreleased credits. Clients cannot choose the order. The `Racer-Ordered` request
header is no longer used and is ignored.

Older dataplanes send pages out of order unless the client sends
`Racer-Ordered: 1`, which current SDKs no longer send. Upgrade dataplanes
before SDKs. Older peers accept only one page request at a time from a node.
The dataplane detects this and falls back to one request at a time for that peer.

### Host reads

A client on the node can ask the dataplane to write an object straight into a
file it owns, such as a `memfd`, instead of streaming it over the socket:

```
POST /v1/objects/<key>/host-read HTTP/1.1
Host: racer
Content-Length: 0
Range: bytes=<start>-<end>          optional
If-Match: "<etag>"                  optional
Racer-Destination-Offset: <offset>
Racer-Destination-Length: <length>
```

- Send exactly one file descriptor with the request, as `SCM_RIGHTS` data on
  the same `sendmsg` as the start of the request. It must be a regular file
  opened for writing, without `O_APPEND`.
- The bytes at object offset `o` go to file offset
  `Racer-Destination-Offset + (o - range start)`.
- The dataplane answers 400 if there is no descriptor, more than one, a
  descriptor on any other request, a file that is not regular, or a length
  smaller than the range. Range and version errors keep their usual statuses.
- A success has `Racer-Delivery: host`. The body uses the normal frames, but
  page frames carry no data: a page frame means that page is already in the
  file. The last frame is the usual completion frame. The client sends no
  credits.
- If the read fails partway, the connection closes without the completion frame
  and the file may hold partial data.
- If the file is a `memfd` created with `MFD_ALLOW_SEALING`, carries the
  `F_SEAL_SHRINK` seal (and no write seal), is open for reading and writing,
  and is at least `Racer-Destination-Offset + Racer-Destination-Length` bytes
  long, the dataplane maps the destination and decrypts pages straight into
  it on its crypto workers. That skips a plaintext copy in the dataplane and
  the kernel copy of a positioned write. Pages that are already plaintext in
  dataplane memory are still written with positioned writes. The client sees
  the page's ciphertext briefly before the plaintext, and a page that fails
  authentication is zeroed, but a page frame is still sent only after the
  page is authenticated. Any other file gets positioned writes only.
  `racer_host_read_direct_decrypt_pages_total` and
  `racer_host_read_direct_decrypt_bytes_total` count pages and plaintext bytes
  decrypted this way. When such a page arrives from a peer over RDMA, the
  crypto worker copies it out of the RDMA receive buffer into the dataplane's
  ciphertext cache and decrypts it in the same pass, 128 KiB at a time, so
  the received bytes are read from memory once. The receive buffer is held
  until that pass ends. A page that fails authentication is not cached or
  persisted. `racer_host_read_fused_decrypt_pages_total` counts pages
  decrypted this way. Such a page enters the memory cache only after it
  passes authentication. `racer_host_read_fused_skipped_total` counts host-read pages
  decrypted without the fused pass, by `reason`: `not_registered` (the page
  came from memory, disk, or HTTP rather than an RDMA receive buffer, which
  is expected for local hits), `retained` (another reader already copied the
  page out of the receive buffer, for example a plaintext or GPU read of the
  same page), or `reservation` (ciphertext admission refused the copy, so the
  page is copied out before the decrypt instead).
- The dataplane keeps mappings of such files in a process-wide cache keyed
  by the file's device and inode, so repeated requests for the same `memfd`
  (for example, parallel ranges of one buffer, or a pooled buffer reused for
  later objects) skip a `mmap`, `munmap`, and page-table populate per request.
  Every request still presents the descriptor and passes every check above.
  The cache holds up to 128 mappings and 256 GiB of mapped files, evicting the
  least recently used beyond that, and unmaps a mapping that no request has
  used for 5 seconds. While a mapping is cached the client's `memfd` memory
  stays allocated even if the client closed it, and the client cannot add
  `F_SEAL_WRITE` to it. Unmapping happens on a background thread.
  `racer_host_read_mappings_created_total`,
  `racer_host_read_mapping_reuses_total`, and
  `racer_host_read_mappings_evicted_total` count new, reused, and evicted
  mappings; `racer_host_read_mappings_cached` and
  `racer_host_read_mappings_cached_bytes` report the cache's current size.
- Pages of one request that are decrypted into a mapped `memfd` run on crypto
  workers concurrently, up to 4 at a time, instead of one after another.
  Page frames are still sent in page order, each after its page and every
  earlier page is in the file. `racer_host_read_inflight_pages_sum` and
  `racer_host_read_inflight_pages_count` give the average number of such
  decrypts running for a request each time one starts; a value near 1 means
  pages arrive more slowly than they decrypt.
- `racer_host_read_request_nanoseconds_sum` and
  `racer_host_read_request_nanoseconds_count` time each successful host-read
  from the start of its delivery (after the request is parsed and the
  descriptor checked) to the final frame.
  `racer_host_read_first_page_nanoseconds_sum` and
  `racer_host_read_first_page_nanoseconds_count` time the same start to the
  first page being ready, before the response head is sent. The difference
  is time spent delivering pages and sending frames.

`POST /v1/objects/<key>/gpu-read` writes into GPU memory. It works like
`host-read`, with these differences:

- The descriptor comes from `cuMemExportToShareableHandle` with the POSIX file
  descriptor handle type. `Racer-Allocation-Size` is required and must be the
  size passed to `cuMemCreate`.
- `Racer-Gpu-Uuid` is optional: the UUID of the GPU that holds the memory, as
  `nvidia-smi` prints it (`GPU-xxxxxxxx-...`). Device numbers in the client's
  container can differ from the dataplane's, so the dataplane uses the UUID to
  find the GPU. Without it, the dataplane tries each GPU. The Python client
  sends it.
- The answer is 501 when GPU mode is off or the memory is on a GPU the
  dataplane did not find, including an unknown `Racer-Gpu-Uuid`. It is 400 when the size does not match the memory or
  the window does not fit in it.
- A success has `Racer-Delivery: gpu`. A page frame means the GPU copy of that
  page is complete.
- When the read ends for any reason, the dataplane waits for its GPU work,
  then releases the memory. See [GPU reads](#gpu-reads).

### Prefetch

Prefetch asks Racer to load object ranges into the cache before a job reads
them. It is best effort. Racer does not pin or retain prefetched pages: they are
stored like any other cached page and can be evicted later.

Prefetch uses a JSON API on the client socket
(`/run/racer/<cache>/client/socket`) with HTTP/1.1 and `Host: racer`, like
object reads. A client submits a ticket that lists object ranges. The node finds
the missing 16 MiB pages and asks each page's owner to fill it through the
normal fill path. Tickets live in memory on the node that received them. A
restart loses them; submit the same ticket again and only missing pages are
loaded.

| Method | Path | Purpose |
|--------|------|---------|
| `PUT` | `/v1/prefetches/{id}` | Create or update a ticket |
| `GET` | `/v1/prefetches/{id}` | Ticket status; query `wait`, `coverage`, `refs`, `cursor` |
| `DELETE` | `/v1/prefetches/{id}` | Cancel a ticket |
| `GET` | `/v1/prefetches` | List this cache's tickets on this node |

Other methods return `405` with `Allow: GET, PUT, DELETE` for a ticket path, or
`Allow: GET` for the list path. `{id}` is chosen by the client, matches
`[A-Za-z0-9._-]{1,128}`, and is scoped to the socket's cache. `.` and `..` are
invalid. Query parameters are form-encoded; unknown or repeated parameters are
`400`.

`PUT` needs `Content-Type: application/json` and a body of at most
`RACER_PREFETCH_MAX_BODY_BYTES`. `Transfer-Encoding`, `Expect`, and
`Content-Encoding` are rejected. `GET` and `DELETE` take no body. `Authorization`
is accepted on `PUT` only. Racer sends it to the origin for every ref, keeps it in
memory only, and replaces it on each accepted `PUT`; a `PUT` without it clears it.
Prefetch responses close the connection.

#### Request body

```json
{
  "cache_uid": "5b0c7a5e-2f7d-4b8e-9d0e-6f6f2f3b1a10",
  "priority": "normal",
  "generation": 0,
  "sequence": 0,
  "ttl_seconds": 600,
  "max_pending_bytes": 0,
  "refs": [
    {"key": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
     "etag": "\"0x8DC1A2B3C4D5E6F\"", "offset": 0, "length": 1073741824,
     "metadata": "opaque"},
    {"key": "60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752"}
  ]
}
```

| Field | Default | Rules |
|-------|---------|-------|
| `cache_uid` | Absent | If set, must equal the live ClusterCache UID, else `409 cache_uid_mismatch` |
| `priority` | `normal` | `high`, `normal`, or `low` |
| `generation` | `0` | 0 to 2^53-1 |
| `sequence` | `0` | 0 to 2^53-1 |
| `ttl_seconds` | `600` | At least 1; capped at `RACER_PREFETCH_MAX_TTL_SECS`. Status shows the applied value |
| `max_pending_bytes` | `0` | Bound on the ticket's in-flight bytes. `0` means `RACER_PREFETCH_DEFAULT_PENDING_BYTES`; capped at `RACER_PREFETCH_MAX_PENDING_BYTES` |
| `refs` | Required | 1 to `RACER_PREFETCH_MAX_REFS` entries |
| `refs[].key` | Required | 64 lowercase hex characters, as in `/v1/objects/{key}` |
| `refs[].etag` | Absent | Strong quoted ETag, as in `If-Match`. If absent, Racer resolves the current version once at submit |
| `refs[].offset` | `0` | Must be less than the object length, else the ref fails with `invalid_range` |
| `refs[].length` | To end | At least 1 when set; clamped to the object end |
| `refs[].metadata` | Absent | Sent to the origin like `Racer-Metadata` |

Unknown fields are `400`. Duplicate refs are allowed; each page is loaded once.
If version lookup fails for a ref, that ref fails and the `PUT` still succeeds.
A ref to an empty object with offset 0 and no length succeeds with no pages.
`max_pending_bytes` limits dispatched bytes, but a ticket with nothing in flight
can always send one page.

#### Generation and sequence

Each ticket stores the `(generation, sequence)` of its last accepted `PUT`, here
`(G, S)`. Racer also stores a hash of the body and of whether `Authorization` was
present. The hash ignores whitespace and field order but not omitted fields: an
explicit default such as `"ttl_seconds": 600` is a different body from leaving
the field out. The SDKs always encode the same request the same way, so their
retries match.

| Incoming `(g, s)` | Result |
|-------------------|--------|
| No ticket | Created, `201` |
| `g == G`, `s == S`, same body | `200` with current status; renews the TTL of a ticket that has not expired or been canceled |
| `g == G`, `s == S`, different body | `409 payload_mismatch` |
| `g == G`, `s > S` | Replaces the wanted set, `200` |
| `g == G`, `s < S` | `409 stale_sequence` |
| `g > G` | Replaces the wanted set and starts a new generation, `200` |
| `g < G` | `409 stale_generation` |

A one-shot batch is one `PUT` with `(0, 0)`. A rolling window is the same ticket
updated with a higher `sequence`. When the wanted set is replaced, pages no
longer wanted lose this ticket's interest. Queued pages that no ticket wants are
dropped. Fills already in flight finish. A `PUT` with a higher position also
restarts a finished ticket.

Every accepted `PUT` renews the TTL. When the TTL ends, a ticket that is still
working becomes `CANCELED` with reason `expired`. A settled ticket keeps its
state. Either way it releases its interest. Nothing is evicted. Finished tickets
stay visible for `RACER_PREFETCH_TICKET_RETENTION_SECS` after expiry or cancel.

#### Status

`GET /v1/prefetches/{id}` and successful `PUT` and `DELETE` calls return:

```json
{
  "id": "epoch-17",
  "cache_uid": "5b0c7a5e-2f7d-4b8e-9d0e-6f6f2f3b1a10",
  "state": "PARTIAL",
  "reason": null,
  "priority": "normal",
  "generation": 2,
  "sequence": 5,
  "settled": true,
  "ttl_seconds": 600,
  "expires_at_ms": 1793750400000,
  "created_at_ms": 1793749800000,
  "updated_at_ms": 1793749810000,
  "refs_total": 2,
  "refs_succeeded": 1,
  "refs_failed": 1,
  "refs_pending": 0,
  "bytes": {
    "logical_requested": 1073741824,
    "aligned_requested": 1090519040,
    "already_resident": 268435456,
    "queued": 0,
    "in_flight": 0,
    "joined": 16777216,
    "fetched_from_origin": 805306368,
    "admitted": 1090519040,
    "failed": 16777216
  },
  "coverage": {"resident_bytes": 1073741824, "observed_at_ms": 1793749811000},
  "refs": [
    {"index": 1, "key": "60303ae2...c752", "etag": null,
     "offset": 0, "length": 16777216, "outcome": "failed",
     "error": "version_changed"}
  ],
  "next_cursor": null
}
```

| `state` | Meaning |
|---------|---------|
| `QUEUED` | Accepted; nothing sent yet |
| `RUNNING` | Pages are queued or in flight |
| `SUCCEEDED` | Settled; every ref succeeded |
| `PARTIAL` | Settled; some refs failed |
| `FAILED` | Settled; every ref failed |
| `CANCELED` | Stopped. `reason` is `client`, `expired`, `superseded` (cache UID changed), `cache_removed`, or `passthrough` |

`reason` is `null` unless the state is `CANCELED`. `settled` is true when
nothing for the current position is queued or in flight. A settled window goes
back to `RUNNING` on its next update.

A ref succeeds when every one of its pages was stored or already present at an
owner when the ticket settled. Success does not mean the pages stay in the
cache; normal eviction can remove them later.

`bytes` values are 16 MiB aligned except `logical_requested`:

| Field | Meaning |
|-------|---------|
| `logical_requested` | Sum of ref lengths |
| `aligned_requested` | Bytes of distinct pages |
| `already_resident` | Present when planned |
| `queued`, `in_flight` | Current |
| `joined` | Joined a fill already in progress |
| `fetched_from_origin` | Started a new origin fill |
| `admitted` | Stored or already present |
| `failed` | Belong to failed refs |

Query parameters:

- `wait=<duration>` waits until the ticket settles or the time ends, then
  returns status with `200` either way. Use Go-style units `ms`, `s`, or `m`
  (`500ms`, `30s`, `2m`). The limit is `RACER_PREFETCH_MAX_WAIT_SECS`; larger is
  `400`. Closing the connection ends the wait.
- `coverage=1` asks the owners which pages of the wanted set they hold now and
  adds `coverage`. It is a snapshot taken at `observed_at_ms`, not a promise.
  Owners that do not answer before the request deadline count as not holding
  the page, so `resident_bytes` is a lower bound.
- `refs=failed` or `refs=all` adds per-ref results, 256 per response. Pass
  `next_cursor` back as `cursor` for the next page. A cursor from another
  position is `400`. `index` is the ref position in the `PUT`. `outcome` is
  `succeeded`, `failed`, or `pending`. `etag` is the resolved ETag, or `null`.

Ref `error` values:

| Error | Meaning |
|-------|---------|
| `version_changed` | The version is no longer available |
| `not_found` | The object does not exist |
| `forbidden` | The origin rejected the request |
| `origin_error` | The origin or owner failed after retries |
| `peer_unsupported` | The owner runs a dataplane without prefetch |
| `budget_timeout` | The owner stayed busy after `RACER_PREFETCH_MAX_ATTEMPTS` tries |
| `invalid_range` | The offset is past the object end |

`GET /v1/prefetches` returns `{"prefetches": [...]}`: summaries sorted by id,
without `refs` or `coverage`.

`DELETE` returns `200` with the `CANCELED` status and reason `client`. It
releases interest. Fills in flight finish and nothing is evicted.

#### Errors

Every non-2xx prefetch response has `Content-Type: application/json` and a body
like:

```json
{"code": "stale_sequence", "message": "sequence 3 is older than current 5"}
```

`message` is for people and can change. Object read errors still have empty
bodies.

| Status | `code` | When |
|--------|--------|------|
| `400` | `invalid_request` | Bad JSON, field, id, query, or header |
| `404` | `not_found` | Unknown id on `GET` or `DELETE` |
| `405` | `method_not_allowed` | Method not allowed. The `Allow` header lists the allowed methods |
| `409` | `cache_uid_mismatch`, `payload_mismatch`, `stale_sequence`, `stale_generation`, `passthrough` | See above. A `PUT` to a [passthrough](#passthrough) cache is `409 passthrough` |
| `413` | `too_large` | Body over `RACER_PREFETCH_MAX_BODY_BYTES` or refs over `RACER_PREFETCH_MAX_REFS` |
| `429` | `ticket_limit`, `pending_limit` | Node ticket or pending-byte limit. `Retry-After` gives the seconds to wait (currently 5) |
| `503` | `unavailable` | Node is draining, the cache is not ready, or this node is not one of the cache's selected nodes |

Before answering `429 ticket_limit`, Racer drops the oldest finished ticket to
make room.

#### How prefetch runs

- **Priority.** `high` goes before `normal`, and `normal` before `low`. Within a
  priority, tickets take turns. Within a ticket, refs go in order.
- **Owners.** Each page goes to its first-ranked owner, which fills it through the
  shared fill table and stores it with normal eviction. Owners are chosen only
  from the cache's [node selector](#node-selector) nodes. If membership or the
  selector changes, queued pages go to the new owner, and stored pages of live
  tickets whose owner left the cluster or the cache's nodes are queued again.
- **Shared work.** Tickets that want the same page share one fill.
- **Demand reads come first.** A read that needs a page being prefetched joins
  that fill and takes it out of the prefetch limits. Queued prefetch work never
  delays reads.
- **Owner limits.** An owner runs at most `RACER_PREFETCH_CONCURRENT_FILLS`
  prefetch fills and, if set, at most `RACER_PREFETCH_ORIGIN_BYTES_PER_SEC` of
  origin reads for prefetch. Over a limit it tells the sender to retry later.
- **Retries.** Busy, unavailable, and I/O failures are retried with backoff
  (100 ms base, doubling, 10 s cap, random jitter) up to
  `RACER_PREFETCH_MAX_ATTEMPTS` times per page. Then the ref fails with
  `budget_timeout` or `origin_error`.
- **Cache changes.** Switching a cache to passthrough cancels its tickets
  (`passthrough`). Removing the ClusterCache, changing its UID, or a selector
  change that drops this node cancels them (`cache_removed`).

Read the prefetched data with the ETag from the ref status, or pin the ETag in
the ref, so later reads select the same version.

#### Prefetch settings

Set these in `racer-settings` like the [dataplane settings](#dataplane-configuration).
Out-of-range values fail startup.

The prefetch limits are listed under [environment variables](#environment-variables).

The old `RACER_WARM_*` settings are no longer used.

#### Prefetch metrics

All prefetch metrics have a `priority` label.
`racer_prefetch_tickets_finished_total` also has `outcome`: `succeeded`,
`partial`, `failed`, or `cancelled`.

| Metric | Type | Meaning |
| --- | --- | --- |
| `racer_prefetch_requested_bytes_total` | counter | Logical bytes requested |
| `racer_prefetch_aligned_bytes_total` | counter | Page-aligned bytes requested |
| `racer_prefetch_already_resident_bytes_total` | counter | Bytes already present when planned |
| `racer_prefetch_queued_bytes` | gauge | Bytes waiting to be sent |
| `racer_prefetch_fetched_bytes_total` | counter | Bytes that started a new origin fill |
| `racer_prefetch_joined_bytes_total` | counter | Bytes that joined a fill in progress |
| `racer_prefetch_admitted_bytes_total` | counter | Bytes stored or already present |
| `racer_prefetch_failed_bytes_total` | counter | Bytes that failed |
| `racer_prefetch_demand_joins_total` | counter | Reads that joined a prefetch fill |
| `racer_prefetch_demand_wait_seconds` | histogram | Time from a read joining a prefetch fill to fill completion |
| `racer_prefetch_retries_total` | counter | Page retries |
| `racer_prefetch_deferrals_total` | counter | Owner "retry later" answers |
| `racer_prefetch_throttle_seconds_total` | counter | Time spent waiting on owner limits |
| `racer_prefetch_window_updates_total` | counter | Accepted ticket updates |
| `racer_prefetch_window_dropped_bytes_total` | counter | Bytes dropped from the wanted set by updates |
| `racer_prefetch_tickets` | gauge | Tickets on the node |
| `racer_prefetch_tickets_finished_total` | counter | Tickets that finished, by outcome |

### Spill holds

Spill (`RACER_RANGE_SPILL_PAGES`) keeps pages on local disk under a short hard
hold. Holds bypass second-sight and block eviction of the page and reclamation
of its segment. Each held page is charged one whole segment. Each worker must
fit its held pages within `RACER_RETAINED_SEGMENT_PERCENT` of both segments and
index entries, while keeping the free-segment reserve. That setting now governs
only spill holds.

These gauges refresh on worker health ticks (normally every 100 ms) and are
summed across storage workers. They were named `racer_warm_held_*` before.

| Metric | Meaning |
| --- | --- |
| `racer_held_pages` | Published indexed pages under live holds |
| `racer_held_payload_bytes` | Logical payload bytes of those pages |
| `racer_held_segments` | Distinct segments containing those pages |

### Second-sight disk retention

The default policy allows a currently owned page to persist on its first logical
read. A non-owned page becomes eligible after an independent reader observes it
within the recent history window. Retries and copy-only probes do not create
independent demand. Eligibility is determined before inserting the observation;
concurrent followers can qualify without promoting the first reader's own attempt.
Persistence remains best effort and only uses verified ciphertext. A skipped or
failed optional disk enqueue does not fail an otherwise valid read.

Each I/O worker has four rotating Bloom generations for page history. Reader
accounting uses exact operation-local interest tokens, retained across retries
and handoffs, rather than an approximate reader-history filter.
At the default period, observations remain for approximately 180-240 seconds.
History allocations round down to 32-byte units and do not exceed the node budget.
A budget too small for the final worker count fails startup rather than growing
implicitly. Bloom false positives can retain extra pages but cannot suppress
independent reader detection; history is never authorization or proof of integrity.

Bounded exact resident heat entries use the worker's disk page-index capacity plus
its pending-write queue capacity. Heat saturates at three and decays lazily by
one per 60 seconds, independently of history rotation. Current ownership adds a
soft eviction preference, not a pin; owned pages remain evictable. History and
heat are process-local hints, not checkpoint state. `disabled` restores owned-only
disk admission and neutral eviction scores; resource budgets, integrity checks,
and authorization still apply. Configuration values are validated even when the
policy is disabled.

[ClusterCache priority](#priority) runs before the score. Each eviction ranks a
bounded set of candidate pages, first by whether their cache is over share,
then by score, then by age. Candidates come first from the most over-share
caches, so an over-share page is normally evicted before any under-share page.
An under-share page can still go first when the over-share candidates are held.
An over-share page never replaces an under-share page in a full pending write
queue. Segment reclamation favors
segments that hold mostly over-share data; under-share pages in a recycled
segment are still lost. A non-owned page is not admitted when its cache is over
share and the worker has no free segments beyond the reserve. Owned pages are
always admitted. Spill holds are never evicted.

Ownership hints use completed rankings for the current accepted placement.
Missing or obsolete hints provide no bonus until refreshed. Each worker also
refreshes indexed residents, including recovered idle pages, without requiring
foreground reads. A background step visits at most one resident, scores at most
256 members, and visits at most 64 ranking-cache CLOCK entries. Steps are spaced
at least 1 ms apart; completed passes and errors retry after 100 ms. Refresh is
independent of other placement maintenance and stops during shutdown. These
bounds limit background work, not the time to refresh every resident.

Low-cardinality `racer_retention_*` metrics aggregate all I/O workers without page,
cache, or reader labels. Counters report logical observations, qualified
observations, persistence attempts, and accepted persistence attempts. Acceptance
does not prove a completed disk publication; use `racer_disk_publications_total`
for that. Gauges report set/total Bloom bits and tracked heat entries. Samples
refresh on each worker's health tick (normally every 100 ms), so a stalled worker
leaves its last sample visible. The aggregate is not an atomic node-wide snapshot.
Storage metrics use the same health-tick snapshots. `classification` has exactly
two values, `owned` and `nonowned`, sampled from the current cache-only ownership
hint at each event. Missing or stale rankings classify as `nonowned`; the event
does not initiate a ranking. This is an event-time classification, not the class
at insertion, proof of nonownership, or an exact current resident-class aggregate.
Ownership refresh affects subsequent events only, never rewrites old counters.

| Metric | Meaning |
| --- | --- |
| `racer_disk_size_bytes{disk}` | Gauge of segment capacity assigned to Racer on this backing disk, including reserve segments |
| `racer_disk_used_bytes{disk}` | Gauge of reserved record bytes on this backing disk, including headers, tags, and alignment padding, until segment recycling |
| `racer_disk_class_publications_total{classification}` | Completed writer publications; excludes checkpoint restoration and deduplicated enqueue |
| `racer_disk_class_published_payload_bytes_total{classification}` | Logical page payload bytes in those publications |
| `racer_disk_class_segment_evicted_pages_total{classification}` | Victim mappings removed by bounded segment reclamation, including index-only segment reclamation |
| `racer_disk_class_segment_evicted_payload_bytes_total{classification}` | Logical payload bytes of those segment victims, not physical reclaimed disk space |
| `racer_disk_class_read_payload_bytes_total{classification}` | Logical page payload bytes returned successfully by StoreReader after framing and mapping checks |
| `racer_disk_pending_payload_bytes` | Total logical payload bytes owned by pending writes, including submitted writes until cleanup |
| `racer_disk_indexed_payload_bytes` | Total logical payload bytes in retained index mappings, including restored mappings |
| `racer_cache_disk_resident_bytes{cache}` | Gauge of logical payload bytes in retained index mappings for one ClusterCache, summed over workers |
| `racer_cache_disk_share_bytes{cache}` | Gauge of the ClusterCache's fair share of disk, summed over workers. Only published ClusterCaches have series; see [Priority](#priority) |
| `racer_retention_share_rejected_total` | Optional (non-owned) pages not written to disk because their ClusterCache was over its share and free segments were at the reserve |
| `racer_disk_publications_total` | Queued page writes that completed and were published to the disk index |
| `racer_dirty_discards_total` | Queued page writes (accepted persistence attempts) dropped without a publication, all reasons |
| `racer_dirty_discard_reasons_total{reason}` | The same discards split by reason, listed below |
| `racer_dirty_write_deferred_total` | Writes kept queued because no free segment or index entry could be reclaimed during a checkpoint |
| `racer_free_segments` | Gauge of free segments, summed over workers, refreshed as writes complete |
| `racer_free_segment_target` | Gauge of the free segments reclamation keeps ready, summed over workers; see below |

Each worker's writer keeps up to 8 page writes in flight, submitting the next
queued page as each write completes. A periodic checkpoint holds back new
writes on a worker only until that worker has copied its snapshot. After that,
writes continue into free segments while segment eviction and reuse stay frozen
until the checkpoint is published. A write that finds no free segment or
index entry during that window is not discarded: the page stays queued, the
worker stops starting new writes, and it retries once the checkpoint finishes.
Each such deferral counts in `racer_dirty_write_deferred_total`. Outside a
checkpoint, a write that still finds no space after reclamation fails and is
discarded. Failed writes are logged at most once every 10 seconds per worker
with `stage=persist` and the cause.

Because a checkpoint freezes segment reuse, writes during that window can only
fill segments that were already free. Each worker therefore adapts how many
free segments it keeps ready (`racer_free_segment_target`). It starts at
`RACER_FREE_SEGMENT_RESERVE_PER_WORKER`. When a checkpoint ends, the target
becomes the reserve plus 1.5 times the free segments that checkpoint consumed.
If a write was deferred during it, the target doubles instead. Lower demand
lowers the target gradually, never below the reserve. The target is capped at
one eighth of the worker's segments, because free segments hold no cached
pages. Reclamation evicts whole low-value segments by metadata only: it copies,
reads, and trims nothing.

On raw devices, each segment holds only whole page records (the page plus a
small header), so the rest of each segment is unused (`racer_segment_tail_bytes`).
With 16 MiB pages, a 64 MiB segment fits three records and leaves about 25% of
the device unused. Racer logs a warning at startup when the unused share is 10%
or more. The 1 GiB default fits 63 records and leaves under 2% unused.

`reason` takes these values:

| Reason | Meaning |
| --- | --- |
| `displaced` | The writer queue was full, so a lower-ranked queued page made room for a new one |
| `memory_pressure` | A queued page's ciphertext or staging memory was reclaimed for other work, such as new fills |
| `owned_reclaim` | A queued optional page was dropped to make room for an owned page's persistence |
| `write_failed_no_space` | The write failed because no free segment was available after reclamation |
| `write_failed_index_full` | The write failed because no index entry was available after reclamation |
| `write_failed_io` | The write failed with a device or OS I/O error |
| `write_failed` | The write failed for any other reason |
| `superseded` | The write finished, but the page was removed, replaced, or its cache became unavailable first |
| `canceled` | A deadline, cancellation, or shutdown dropped the queued write |
| `cache_removed` | The page's ClusterCache was removed while the write was queued |

For per-disk utilization as a percentage, use:

```promql
100 * racer_disk_used_bytes / racer_disk_size_bytes
```

The `disk` label is `device:<by-id basename>` for raw devices and
`file:worker-<id>-slab-0.dat` for file-backed slabs. Workers sharing a device
contribute only their assigned segments. Size excludes device guards, unused
tails outside assigned segments, and unassigned capacity. Used bytes include
failed-write reservations and records whose index mappings were removed, until
their segments are recycled. These gauges measure Racer's reserved record space,
not filesystem allocation or whole-device fullness. Sealed segment tails and
free-segment reserves can require reclamation before utilization reaches 100%.
They are sampled after recovery and on each worker's health tick.

All payload byte metrics exclude AEAD tags, record headers, and alignment padding.
Victim counters exclude invalidation, replacement, cache removal, and pending-write
discards. A mapping is counted only by the removal path that actually evicts it.
Read bytes count repeated full-page reads, including internal/copy-only consumers;
AEAD tag validation happens later in fill. They measure disk-to-reader payload,
not authenticated client-delivered bytes, requested-range bytes, or device I/O.
Use their rates to compare disk reuse in benchmarks, not as client throughput.
The pending and indexed payload gauges are exact unclassified worker totals at sampling time, maintained
on lifecycle events without scanning all pages. A page can briefly count in both
while a published write is still pending cleanup. They are not physical allocation
or unique resident-byte totals, nor ownership-split gauges. Tracked heat entries
remain a separate measurement; persistence acceptance does not measure eviction.

### Topology capacity and version compatibility

Placement capacity uses a conservative 1,024-byte structural estimate per cached
ranking, rounding each worker's byte share down to whole entries. It is not an
allocator hard bound. Path-cache accounting includes retained buffer capacities
and reference-count headers, but excludes active search scratch, caller-held
results, membership graphs, and allocator overhead. A result larger than the
worker's path-cache budget is returned without retention. Cache entry and byte
limits do not determine search concurrency: identical canonical queries share
one active search, while distinct searches can report overload. The node's total
distinct-search concurrency can reach I/O worker count times
`RACER_ACTIVE_PATH_SEARCHES_PER_WORKER`.

The overlay is a mesh per pool over stable node IDs and is independent of
shares. A pool of up to 256 members is a full mesh: every member is a neighbor
of every other, so a reader fetches each page directly from an owner. A larger
pool is a grid of columns, with about `2.83 * sqrt(k)` neighbors per member,
and some reads take one relay. Neighbors are computed on demand from the
per-pool columns and hash rings. These allocations are not covered by the
path-cache byte limit.

Publications with unchanged validated membership version and content reuse the
whole membership. New versions with identical node IDs share immutable graph
storage, including shares-only or metadata-only updates; changed IDs rebuild
the graph. Membership byte estimates are cached at construction and read in
constant time, but each snapshot's estimate includes the full shared graph.
Summing them, as the publication grace budget does, is conservative rather than
deduplicated allocation accounting.

Live control updates prepare membership off the I/O polling thread. The
publication store admits one unfinished preparation job with no queued backlog;
additional preparation is rejected as overloaded until it finishes. Canceling
the waiter or reaching its deadline does not interrupt a running CPU job or
free its admission slot. Preparation does not publish by itself. Shutdown joins
the owned job rather than detaching it, so cleanup may wait beyond the request
deadline: the join has no independent timeout. Bounded input and job count are
not a hard wall-clock shutdown guarantee.

Routing selection uses `/next-hop/v1`, which independently length-prefixes the
seed, source ID, and destination ID. All participating dataplanes must use the
same routing implementation. Matching membership version numbers do not
negotiate the algorithm, and mixed-version routing is not safe. **When upgrading,
quiesce traffic until all dataplanes run the same version.**

## Elected control plane

`racer` is both the cache daemon and the control-plane binary. There is
no `racer-controller` Deployment, image, or control Service. Pods elect control
seats with Leases named `racer-seat-0` through `racer-seat-15`. One seat also
holds `racer-primary` and commits membership, catalog, keyring, and trust changes.

Clients discover the seats through the Kubernetes API and verify their pinned
TLS certificates before sending credentials. They try another seat if one is
unavailable. They do not depend on a fixed controller DNS name or an
operator-generated serving CA.

During managed startup, election keeps running while enrollment and the first
goal state retry with bounded control requests. The pod stays unready while
waiting. `RACER_REQUEST_TIMEOUT_MS` still limits data requests; it does not limit
how long startup can wait for the control plane. SIGTERM stops startup.

```bash
kubectl -n unbounded-system get leases -l racer.unbounded-cloud.io/control=true
kubectl -n unbounded-system get daemonset racer
kubectl -n unbounded-system get endpointslice racer-webhook
```

The `racer-state` Secret holds durable control state. Its UID is the cluster ID.
Do not delete, replace, or clear it to repair a running installation. The
operator creates it only for a new installation; it does not generate keys or
reconcile the runtime's state back to an empty manifest. Back it up securely.
Never log its data: it contains cache encryption keys.

Each cache has one active page key. Key rotation keeps the replaced page key as
a decrypt-only key, so cached pages stay readable for six more rotations (seven
readable page keys in total) before their key is dropped. Origin credential keys
are replaced without a decrypt-only history. Upgrading to the keyring bundle
format version 3 needs a cold restart of every seat and node.

Seats publish only committed state. A primary change preserves publication and
keyring counters. Losing serving authority withdraws the seat rather than
allowing an isolated former primary to keep writing. API access is needed for
new elections and for confirming authority; an API outage is not permission to
reset state or mint a new cluster.

Every dataplane pod can become a control seat and therefore has the Kubernetes
permissions needed for that role. Lease ownership is a coordination rule, not
an RBAC boundary. Treat compromise of a privileged dataplane node as compromise
of Racer's cluster-wide control state and cache keys.

Allow dataplane-to-seat HTTPS on port 8443 and API-server-to-seat webhook
traffic on port 9443. Preserve peer traffic, diagnostics, DNS, and Kubernetes
API access. The `racer-webhook` Service has no selector; the primary maintains
its EndpointSlice and the injector's CA bundle. NetworkPolicy behavior depends
on the CNI, especially with host-network Pods.

The control protocol keeps its existing byte limits: 64 KiB for bootstrap,
768 KiB for keyring bundles, and 64 MiB for publications. These are document
limits, not process memory limits.

### Operator-managed dataplane

When any `ClusterCache` exists, the operator installs DaemonSet `racer`
with its ServiceAccount, restricted RBAC, election Leases, initial state Secret,
webhook resources, and ConfigMap `racer-settings`. The ConfigMap is seeded
only when absent. The DaemonSet uses the operator's registry and tag and imports
the ConfigMap with `envFrom`. A pod-template annotation carries its payload hash,
so edits roll out.
Existing resources with these names that the operator did not create are not
adopted; reconciliation fails until they are removed. Deleting the last
`ClusterCache` leaves the dataplane and its durable control state installed.
The operator still checks the admission guards. If they drift or disappear, it
revokes dataplane permissions and repairs the guards. It restores permissions
only when a cache exists again.

Earlier releases named these objects `racer-dataplane`, with ConfigMap
`racer-dataplane-config`. On upgrade the operator seeds `racer-settings` from
`racer-dataplane-config`, renaming `RACER_DATAPLANE_NVIDIA` to `RACER_NVIDIA`.
Once the new DaemonSet is applied, it deletes the old objects it created: the
DaemonSet, ServiceAccount, RBAC, and ConfigMap first, then the old admission
policies. Old and new Pods may run side by side briefly. It leaves alone any
old-named object it did not create.

| Setting in `racer-settings` | Default | Purpose |
|------------------------------------|---------|---------|
| `RACER_CONTROL_SEATS` | `auto` | Choose the number of control seats automatically, or request 1 to 16 |
| `RACER_NVIDIA` | `env` | How the dataplane asks for NVIDIA driver libraries: `off`, `env`, or `cdi`; see [GPU reads](#gpu-reads) |

Set `RACER_CONTROL_SEATS` in `racer-settings` to `auto` (the default) or
an integer from 1 to 16. The active count cannot exceed the eligible node count.
For example, to request three seats:

```bash
kubectl -n unbounded-system patch configmap racer-settings \
  --type merge -p '{"data":{"RACER_CONTROL_SEATS":"3"}}'
```

Workload overrides may target `{component: racer, kind: DaemonSet}` to set, for
example, `nodeSelector`, tolerations, `updateStrategy`, annotations, resources, or
additional container `env` entries. Overrides cannot change or remove the
operator-wired `env` entries, `envFrom`, volumes, or service account of the
`dataplane` container. Set the seat count through the ConfigMap, not an override.

The operator wires Pod identity through the downward API and mounts two tokens:
an API-audience token for Kubernetes and a `racer-control` token for enrollment.
Automatic ServiceAccount token mounting remains disabled. Node identity comes
from verified enrollment and local recovery, not a caller-supplied Node UID.

The **bare binary** defaults `RACER_PEER_LISTEN` to `0.0.0.0:7443` and
`RACER_DIAGNOSTICS_LISTEN` to `127.0.0.1:9090`. Managed workloads bind both to the Pod IP
with peer port 8082 and diagnostics port 9090. Managed control startup does not
require `RACER_CLUSTER_ID`, `RACER_CONTROL_ENDPOINT`, or `RACER_CONTROL_TRUST_BUNDLE`.
Static control endpoints remain available for standalone tests, not managed
cluster discovery.

### Membership and credential reconciliation

Membership uses Nodes and live Pods owned by the current managed DaemonSet UID.
The Pod report supplies shares and NICs unless an administrator Node annotation
overrides them. ClusterCache UIDs remain cache identities; their names determine
the local socket paths.

Seats authenticate enrollment with TokenReview and live Pod, ServiceAccount,
DaemonSet, and Node checks. Node private keys stay on the node. Issuer roots and
cache keys are rotated without resetting version counters. Retired roots stay
in durable state until certificates they could have issued expire. Keep node
clocks synchronized.

## Diagnostics and metrics

| Dataplane HTTP path | Use |
|---------------------|-----|
| `/healthz` | Process liveness |
| `/readyz` | Serving readiness, including usable admission |
| `/metrics` | Prometheus metrics, including `racer_ready` and `racer_live` |
| `/debug/membership` | Applied membership state |
| `/debug/failures` | Bounded failure diagnostics |
| `/debug/aead`, `/debug/decrypt-failures`, `/debug/send-tag` | Bounded crypto and tag check failure details |
| `/debug/terminal`, `/debug/admission-final`, `/debug/candidate-final` | Bounded final-outcome records |
| `/debug/gate-events`, `/debug/flight-expiry` | Bounded gate and in-flight expiry events |
| `/debug/hardware` | Live JSON report of block devices, RDMA devices, and Racer's claimed disks and RDMA rails |
| `/debug/pprof/profile?seconds=30` | Opt-in native CPU profile |

`/metrics` is sent with chunked transfer encoding, so its size grows with the
cache catalog without a fixed limit. Counters wrap at 2^64; Prometheus treats
the wrap as a counter reset.

`/debug/hardware` reads sysfs on every request. It never opens block devices.
The read runs on a helper thread, so a slow read does not delay health checks.
Values it cannot read are `null`, and section failures are listed in `errors`.
It returns 503 `response_too_large` if the JSON does not fit in one response
(about 64 KiB), or if discovery exceeds its fixed working-memory budget, which
can happen on hosts with very many block or RDMA devices. Attribute files
larger than 4 KiB are reported as `null`. Only one report is built at a time: a request made while another
is still running returns 409 `busy`. A report that takes longer than 5 seconds
closes the connection without a response. In managed workloads, read it through the API server pod proxy.
This needs RBAC `get` on `pods/proxy`:

```sh
kubectl get --raw "/api/v1/namespaces/<namespace>/pods/<pod>:9090/proxy/debug/hardware"
```

CPU profiling is disabled by default. Set the dataplane environment variable
`RACER_PPROF_ENABLED=true` to enable it; only the exact values `true` and `false`
are accepted. Use `GET /debug/pprof/profile` on the existing diagnostics listener.
The optional `seconds` parameter defaults to 30 and must be an integer from 1 to 60.
Duplicate or unknown parameters return 400. Other methods return 405. Disabled
profiling returns 404, a concurrent capture or download returns 409, and unavailable
native sampling or denied perf permissions return 503 with a fixed error code.

The response is a raw pprof protobuf download, capped at 16 MiB. Capture and download
share a deadline of the requested duration plus 15 seconds, at most 75 seconds.
Headers still have a two-second deadline. One helper performs CPU sampling and
encoding; metrics and health remain available during capture. Profiling uses one
process-wide diagnostic memory reservation (192 MiB, including the output), not a
16 MiB charge on every worker's request-context quota. This reservation is not a
strict process RSS limit. Two extra diagnostic reactor slots and their bookkeeping
are reserved. The enabled per-worker queue floor is nine slots instead of seven;
small configured budgets can reduce worker count or fail startup. Profiling does
not increase configured request or queue budgets automatically.
Shutdown stops new captures, requests cancellation early, and waits for helper cleanup.
Socket resets and full hangups cancel capture. A TCP FIN alone cannot distinguish a
disconnected client from a client that closed only its write side, so capture can
continue until completion or the bounded deadline in that case.

Profiles can contain process addresses and executable paths. This endpoint has no
authentication and uses the existing listener: localhost for the bare binary, but
the Pod IP in managed workloads. Restrict network access before opting in. Do not
expose profiles publicly or grant broad container privileges just to bypass a perf
permission error.

This endpoint captures native CPU samples only, not heap or goroutine profiles.
For a standalone listener, save and inspect a profile with the matching binary:

```sh
curl --fail --max-time 75 'http://127.0.0.1:9090/debug/pprof/profile?seconds=30' -o profile.pb
go tool pprof -top ./racer profile.pb
```

Keep the exact binary and its debug information for symbol resolution. Userspace
frames outside the recorded executable mappings keep their raw address with an
unknown mapping. They do not discard the profile or receive an invented mapping.
Profile comments count unknown locations, frame references, and affected
aggregated samples; these are not CPU-time or lost-sample counts. A changed
executable mapping snapshot still rejects capture with `mapping_changed`.

Native capture requires kernel perf access; the endpoint does not change host perf
settings, container capabilities, or seccomp policy. Release builds retain frame
pointers even when profiling is disabled, which can slightly affect CPU performance.

Monitor `racer_requests_total`, `racer_request_errors_total`, `racer_overloads_total`,
and per-tier `racer_*_lookup_hits_total` / `racer_*_lookup_misses_total`. Page-source events
are not request counts or byte rates. A scrape is not readiness. Restrict diagnostics
network access, especially with host networking.

`racer_delivery_bytes_total` counts every body byte delivered to clients;
`racer_delivery_direct_bytes_total` counts only those sent outside the splice path.

Each I/O worker keeps a pool of page buffers for ciphertext copies, disk
reads, and plaintext. Its series are node-wide sums, updated about once per
second:

| Metric | Type | Meaning |
|--------|------|---------|
| `racer_buffer_pool_bytes` | gauge | Idle buffer bytes kept for reuse, charged to the `pool` admission class |
| `racer_buffer_pool_target` | gauge | Per-worker cap on idle buffers of one size |
| `racer_buffer_pool_hits_total` | counter | Buffers taken from idle storage |
| `racer_buffer_pool_misses_total` | counter | Buffers that needed new memory (an `mmap` for page-sized buffers) |
| `racer_buffer_pool_rejects_total` | counter | Returned buffers freed instead of kept (an `munmap` for page-sized buffers) |
| `racer_buffer_pool_trimmed_total` | counter | Times idle buffers were released after an idle window or under memory pressure |
| `racer_worker_buffer_pool_bytes` | gauge | `racer_buffer_pool_bytes` per worker, labeled `worker` |

`racer_buffer_pool_entries` is always 0. While load is steady, misses and
rejects should stay flat. If they keep rising, idle buffers do not fit the
`pool` class budget, which equals the worker's ciphertext budget.

A failed client request also counts in one of
`racer_request_errors_overloaded_total`, `racer_request_errors_unavailable_total`,
`racer_request_errors_deadline_total`, `racer_request_errors_cancelled_total`, or
`racer_request_errors_other_total`, by the first error it reported, and each I/O
thread logs at most one `stage=client-request state=failed` line every 10 s
(with the number suppressed since). `/debug/terminal` keeps the details.

`racer_origin_fills_total` counts pages this node fetched from origin and published.
`racer_origin_walk_reasons_total{reason}` splits the fills that a candidate walk authorized
by what the walk learned from the page's rank 0 owner:

| `reason` | Meaning |
|----------|---------|
| `rank0_self` | This node is the page's rank 0. |
| `rank0_miss` | Rank 0 answered that it holds no copy of the page. |
| `rank0_overdue` | Rank 0 timed out or refused for load, and has stopped answering this node, so its watch for the page is overdue. |
| `rank0_failed` | Rank 0 answered that its own acquisition failed. |
| `rank0_shed` | Rank 0 shed the probe with a signed `Overloaded`. |
| `rank0_unreachable` | The exchange with rank 0 failed: no route, a connection failure, or an unsigned `Unavailable`. |
| `rank0_unusable` | Rank 0's copy could not be used. |
| `rank0_stale` | Rank 0 was behind this node's membership. |
| `rank0_not_asked` | The walk never probed rank 0. |

An owner above rank 0 that is refused for load while probing rank 0 leaves the
fetch to rank 0 and retries. It asks origin itself only after rank 0 stops answering
this node for the hang interval (half the client request timeout). A rank 0 that
still answers other probes is busy, not down. On a cluster where every node owns
every page, most fills on a node that is not rank 0 should be `rank0_miss`. A
large `rank0_overdue` or `rank0_unreachable` count means pages that rank 0 may
hold went to origin.

A peer connect that fails because this node ran out of a local resource is load,
not an unreachable peer: `EADDRNOTAVAIL` (no free ephemeral port toward that peer,
usually because closed connections still sit in `TIME_WAIT`), `EADDRINUSE`,
`EMFILE`, `ENFILE`, `ENOBUFS`, `ENOMEM`, and `EAGAIN` fail the probe `Overloaded`,
so the walk retries the owner later instead of counting it `rank0_unreachable`
and asking origin. `racer_peer_connect_local_exhausted_total` counts these connects.
`/debug/failures` keeps the original errno at `stage=PeerCheckout`.

A peer page body that is already arriving over HTTP when this worker's ciphertext
budget is full waits for budget, polling every 1 ms for up to 250 ms (at most a
quarter of the time left in the request), instead of failing at once. Failing
would close the connection mid-body and leave its port in `TIME_WAIT`.
`racer_peer_receive_ciphertext_waits_total` counts bodies that waited and
`racer_peer_receive_ciphertext_wait_granted_total` those that then got budget.
A page the peer already wrote over an RDMA link waits for budget the same way, and
the connection that carried its signed completion returns to the pool even if the
wait runs out.

`racer_peer_connection_closes_total{reason}` counts peer connections this node, as the
requester, closed instead of returning to its pool. Each close leaves a local port in
`TIME_WAIT` toward that peer for 60 s, so a sustained rate near the ephemeral port range
divided by 60 exhausts ports (`racer_peer_connect_local_exhausted_total`). `reason` is
`abandoned` (the exchange was dropped mid-flight, such as a lost race or a read whose
client went away), `canceled`, `deadline`, `overloaded` (local admission refused a
response already on the wire), `peer_close` (the peer answered `Connection: close`),
or `error` (any other failure after the request was sent). An exchange that fails
before sending its request, or after a complete bodiless answer such as a peer's
`Overloaded`, keeps its connection and is not counted. Idle connections closed after
30 s unused are not counted.

The diagnostics listener uses port 9090 by default.
Persist scrape configuration on workload templates or supported overrides, not only live Pods.

### GPU metrics

`/metrics` also reports `gpu-read` state. Labels have fixed values or come
from the GPUs the dataplane found and the active RDMA rails.

| Metric | Type | Meaning |
|--------|------|---------|
| `racer_gpu_read_requests_total{outcome}` | counter | `gpu-read` requests by how the dataplane opened them: `gpu` (imported on a GPU; the read can still fail later), `not_enabled` (501, `gpu-read` is off), `wrong_device` (501, the memory is on a GPU the dataplane cannot use, or the `Racer-Gpu-Uuid` header names a GPU it did not find), `busy` (503), `invalid` (400), or `failed`. Requests the parser rejects before it knows the mode are not counted. |
| `racer_gpu_enabled` | gauge | 1 when `gpu-read` is on and found a usable GPU, else 0. |
| `racer_gpu_standby` | gauge | 1 while `auto` mode waits for the first `gpu-read` before it touches a GPU, else 0. |

The rest are reported only when `racer_gpu_enabled` is 1:

| Metric | Type | Meaning |
|--------|------|---------|
| `racer_gpu_decrypt_available{ordinal,uuid}` | gauge | 1 when the GPU can decrypt sealed pages. 0 means pages for it are decrypted by the CPU. |
| `racer_gpu_staging_slots_free{ordinal}` | gauge | Free cuFile staging slots on the GPU. |
| `racer_gpu_executor_queued{ordinal}` | gauge | Pages queued or running on the GPU's executor. |
| `racer_gpu_cufile_enabled` | gauge | 1 while cuFile reads are on. A driver-level failure turns it off until restart. |
| `racer_gpu_cufile_disabled_total{reason}` | counter | Times cuFile was turned off for the node: `driver_open`, `staging_register`, or `file_open` (a file registration error other than unsupported). |
| `racer_gpu_cufile_unsupported_total` | counter | Devices (`st_dev`) whose files cuFile reported as unsupported. Files on such a device are read through host memory; cuFile stays on for other devices. |
| `racer_gpu_read_pages_total{decrypt}` | counter | Pages written to GPU memory: `gpu` (decrypted on the GPU) or `cpu` (plaintext from host memory, decrypted by the CPU or already cached). |
| `racer_gpu_pages_rejected_total` | counter | Sealed pages the GPU rejected for a tag mismatch. |
| `racer_gpu_cufile_staged_pages_total` | counter | Pages cuFile read from disk straight into GPU staging. |
| `racer_gpu_cufile_failures_total` | counter | cuFile reads or registrations that failed. The page is read through host memory instead. |
| `racer_gpu_staging_exhausted_total` | counter | Disk reads that found no free staging slot and went through host memory. |
| `racer_gpu_landed_pages_decrypted_total` | counter | Pages decrypted on the GPU from an RDMA landing buffer. |
| `racer_gpu_peer_copies_total` | counter | Encrypted pages copied from another GPU over NVLink or PCIe peer-to-peer. |
| `racer_gpu_peer_copy_bytes_total` | counter | Encrypted bytes copied between GPUs over a peer path. |
| `racer_gpu_peer_failures_total` | counter | Peer copies that failed. Each turns that GPU pair's peer path off until restart. |
| `racer_gpu_peer_bounces_total` | counter | Encrypted pages moved from another GPU through host memory, because the pair has no peer path or it was turned off. |
| `racer_gpu_executor_queue_full_total` | counter | Pages refused because their GPU's executor queue was full. The read fails as overloaded. |
| `racer_gpu_open_requests` | gauge | `gpu-read` requests that hold GPU resources now. |
| `racer_gpu_leaked_resources` | gauge | GPU resources leaked on purpose after a failed release. |

When GPU landing is configured (GPU decrypt and RDMA both on), each active
rail also reports the following. Each decrypting GPU's landing buffer is split
into one slice per rail, so the `{rail,ordinal}` series are per slice.

| Metric | Type | Meaning |
|--------|------|---------|
| `racer_gpu_landing_enabled{rail}` | gauge | 1 when at least one GPU landing slice is registered on the rail. |
| `racer_gpu_landing_slots{rail,ordinal}` | gauge | NIC slots in GPU `ordinal`'s landing slice on the rail. |
| `racer_gpu_landing_exhausted_total{rail,ordinal}` | counter | Landing grants for that slice that found no free slot. The page lands in host memory instead. |

## Upgrade and recovery

Keep the operator, dataplane, SDK, and origin adapters on matching
versions from the same Unbounded release. Keep cluster identity, version counters,
credentials, and node identity state. Do not delete them to force a fresh start.
Cached bytes are disposable; this installation state is not. Deleting and
recreating a ClusterCache creates a new cache identity, even with the same name.

The move from `racer-controller` to elected dataplane seats is a cold-cache alpha
upgrade. Quiesce traffic and let the operator retire the old authority before
the new one starts. Do not run old and new controllers together. Checkpoint
recovery rejects the previous format and a different cluster ID rather than
restoring entries under unrelated keys. Origins must remain available while
caches refill. Never clear raw-device guards or unrelated host files as part of
this upgrade.

The operator refuses migration if the old `racer-config` requests a non-default
peer port, a diagnostics port, host networking, a pod-network node split, or a
non-default `RACER_NVIDIA`. Managed ports are now fixed at 8082 and
9090. Review and remove these old settings before migration; use a DaemonSet
override for host networking and set `RACER_NVIDIA` in
`racer-config`. The operator does
not silently discard them or stop the old controller first.

The operator writes `racer-settings` only when it is absent, so upgrades keep
existing values. New installs seed `RACER_SEGMENT_BYTES` at 1 GiB and
`RACER_SLAB_BYTES_PER_WORKER` at 4 GiB; earlier releases seeded 64 MiB and
1 GiB. A 64 MiB segment holds only three 16 MiB page records, wasting about a
quarter of each device and leaving a small free-segment reserve. To adopt the
new geometry on an existing install, edit both keys in `racer-settings`; this
changes storage geometry and starts a cold cache. Keys absent from
`racer-settings` take the dataplane defaults of the new release.

Expect a cold cache after changing device layout,
worker count, or storage geometry. Recovery does not scan payloads to rebuild
the cache. Make sure origins can serve the resulting misses.

## Go SDK

The Go SDK is `github.com/Azure/unbounded/pkg/racersdk`. Its
[package documentation](https://pkg.go.dev/github.com/Azure/unbounded/pkg/racersdk)
is the complete SDK reference, including configuration, the copy and no-copy read
paths, the origin contract, and error handling. The SDK does not provision caches
or origins.

### Prefetch

The Go SDK wraps the [prefetch API](#prefetch):

```go
func (c *Client) Prefetch(ctx context.Context, id string, spec PrefetchSpec) (PrefetchStatus, error)
func (c *Client) PrefetchStatus(ctx context.Context, id string, opts StatusOptions) (PrefetchStatus, error)
func (c *Client) CancelPrefetch(ctx context.Context, id string) (PrefetchStatus, error)
func (c *Client) ListPrefetches(ctx context.Context) ([]PrefetchStatus, error)
func SplitPrefetch(id string, spec PrefetchSpec, maxRefs, maxBodyBytes int) []PrefetchPart
func (c *Client) NewPrefetchWindow(id string, spec PrefetchSpec) *PrefetchWindow
func (w *PrefetchWindow) Update(ctx context.Context, refs []PrefetchRef) (PrefetchStatus, error)
func (w *PrefetchWindow) Reset(ctx context.Context, refs []PrefetchRef) (PrefetchStatus, error)
```

`PrefetchSpec.TTL` is rounded up to whole seconds; `0` uses the server default.
`PrefetchRef.Length` `0` means to the end of the object. `PrefetchStatus.Created`
is true when the `PUT` created the ticket. A rejected request returns
`*PrefetchError` with `Status`, `Code`, `Message`, and `RetryAfter`. It matches
`ErrInvalidRequest` for `400` and `413`, `ErrNotFound` for `404`, and
`ErrUnavailable` for `429` and `503`.

With an existing `client`, `ctx`, and object `key`:

```go
status, err := client.Prefetch(ctx, "epoch-17", racersdk.PrefetchSpec{
    Refs: []racersdk.PrefetchRef{{Key: key}},
})
if err != nil {
    return err
}
status, err = client.PrefetchStatus(ctx, "epoch-17", racersdk.StatusOptions{
    Wait: 30 * time.Second,
    Refs: "failed",
})
if err != nil {
    return err
}
// PARTIAL or FAILED: inspect status.Refs. Reads still work through normal fills.
```

`SplitPrefetch` splits a large spec into parts that fit the server's ref and
body limits. Zero limits use the defaults (16,384 refs, 4 MiB). Part ids are
`<id>.<n>`, counting from 0.

`PrefetchWindow` keeps one rolling ticket. `Update` increments the sequence and
`Reset` increments the generation and sets the sequence to 0. A failed call does
not roll the position back.

```go
window := client.NewPrefetchWindow("reader-0", racersdk.PrefetchSpec{Priority: "high"})
if _, err := window.Update(ctx, nextRefs); err != nil {
    return err
}
```

The Python client in `clients/python/racer` has the same calls:
`prefetch(id, spec)`, `prefetch_status(id, wait=None, coverage=False,
refs=None, cursor=None)`, `cancel_prefetch(id)`, `list_prefetches()`,
`split_prefetch`, `PrefetchWindow` (`update` and `reset`), and `PrefetchError`
(`status`, `code`, `message`, `retry_after`).

```python
import racer

client = racer.Client(cache="artifacts")
client.prefetch("epoch-17", racer.PrefetchSpec(refs=[racer.PrefetchRef(key=key)]))
status = client.prefetch_status("epoch-17", wait=30, refs="failed")
```

| Socket | Path |
|--------|------|
| Client | `/run/racer/<name>/client/socket` |
| Origin | `/run/racer/<name>/origin/socket`, mode `0600`, in a dataplane-created directory owned by UID/GID 65532 with mode `0700` |

For tests, `github.com/Azure/unbounded/pkg/racersdk/racersdktest` runs a
noncaching fake Racer. It does not test distributed or RDMA behavior.

## Client socket mount

Pods that talk to the dataplane socket directly, such as the Python client,
opt in with a label. The admission webhook then mounts the cache's client
directory read-only and tells the app where the socket is.

| Key | Meaning |
|-----|---------|
| Label `racer.unbounded-cloud.io/client-socket: enabled` | Turns on the mount |
| Annotation `racer.unbounded-cloud.io/client-cache` | ClusterCache name. Required |
| Annotation `racer.unbounded-cloud.io/client-containers` | Optional comma-separated init or app container names. Default: every init and app container, except the injected `racer-object` sidecar |
| Annotation `racer.unbounded-cloud.io/client-socket-injected` | Set by the webhook to the cache name; do not edit |

Each selected container gets:

- `/run/racer/<cache>/client` mounted read-only at the same path. A read-only
  mount still allows `connect` and `sendmsg` on the socket.
- `RACER_SOCKET=/run/racer/<cache>/client/socket`.

Init containers, including native sidecars (`restartPolicy: Always`), get the
same mount, so an init container can fetch through the socket before the app
starts. The pod is placed on Linux nodes without the `exclude` label, like sidecar pods.
The hostPath type is `Directory`, so the pod waits until the dataplane has
created the cache directory. Host networking is allowed.

This label works alone or with `object-sidecar`. When both name the same cache,
the pod gets one shared volume. The webhook rejects the pod when the cache name
is missing or invalid, when a listed container does not exist, or when a
container already sets `RACER_SOCKET` or mounts that path.

## GPU reads

GPU reads let a client on the node read an object straight into its own NVIDIA
GPU memory. They need:

- A dataplane image built with CUDA. The released `racer` image is,
  so no setup is needed. It runs on nodes without GPUs too. An image built with
  `--build-arg RACER_NATIVE_CUDA=false` has no GPU reads.
- An NVIDIA node set up by the unbounded agent, which installs the NVIDIA
  container runtime and a CDI spec.
- `RACER_ENABLE_GPU` in `racer-settings`. `auto`, the default, does
  not touch any GPU until a client sends the first `gpu-read`. Only then does
  the dataplane load CUDA, look for GPUs, and turn GPU mode on, or off if it
  finds no usable GPU. It does not try again until the pod restarts. `true`
  sets up the GPUs at startup and makes startup fail when there is no usable
  GPU. `false` turns it off.

When it sets up the GPUs, the dataplane logs each GPU it finds: ordinal, UUID, PCI address,
NUMA node, compute mode, and whether it can use it. It skips GPUs in
`EXCLUSIVE_PROCESS` or `PROHIBITED` mode. One helper thread does all GPU work
for the process, except cuFile disk reads, which run on four reader threads. At most 256 GPU reads run at once; more get 503.

Where the bytes come from decides who decrypts them:

- A page whose plaintext another read still holds in memory is copied to the
  GPU. The memory cache itself keeps only encrypted pages.
- A page fetched from the origin is encrypted on the CPU, so its plaintext is
  copied to the GPU too.
- Other pages reach the GPU still encrypted, and the GPU checks and decrypts
  them. This covers pages kept encrypted in memory, pages on local disk, and
  pages from peers. The CPU does not decrypt them.
- A page on local disk is read straight from the disk into GPU memory with
  cuFile when it can be. Otherwise the CPU reads it and copies it to the GPU.
  This needs GPUDirect Storage on the host (`nvidia-fs`, or PCIe P2PDMA).
  For PCI P2PDMA, set `RACER_GPU_PCI_P2PDMA=true`. cuFile then does not fall
  back to `nvidia-fs`, and if it cannot turn P2PDMA on, the dataplane logs
  `stage=gpu state=cufile-disabled` and reads through host memory. The
  `stage=gpu state=enabled` line reports the setting as `pci_p2pdma=<bool>`.
  `racer_gpu_direct_disk_reads_total` and
  `racer_gpu_direct_disk_read_bytes_total` count these reads. After a cuFile
  driver or registration failure the dataplane logs
  `stage=gpu state=cufile-disabled` and reads through host memory until it
  restarts. When cuFile only rejects a file because its filesystem or device
  is not supported, the dataplane logs
  `stage=gpu state=cufile-unsupported dev=<major>:<minor> reason=<error>` once
  for that device, reads files on that device through host memory until it
  restarts, and keeps cuFile on for other devices.
- When several `gpu-read` requests want the same page at once, such as every
  GPU loading the same model weights, the page is read from disk or a peer
  only once. A request for the same GPU, or for a GPU that can copy from it
  directly (peer access), reuses that encrypted copy. GPUs that cannot copy
  from each other, such as with `RACER_GPU_PEER=off`, each read the page
  once and share it among requests for that GPU. Each GPU still checks
  and decrypts the page itself. The copy is reused only while some request
  still holds it. `racer_gpu_direct_joined_total` counts reused copies.

- With RDMA on, a page from a peer can be written by the peer's NIC straight
  into GPU memory on this node (GPUDirect RDMA). See below.

If the GPU finds a damaged page, the read fails and the dataplane drops that
copy from memory or disk, as it does when the CPU finds one.

When a shared GPU fetch finishes without putting the page in GPU memory, the
requests waiting on it read the page through host memory rather than each
trying cuFile or a landing again. If the fetching request is canceled, a
waiting request tries the GPU fetch itself.

When both GPU reads and RDMA are on, each GPU that can decrypt gets a landing
buffer of `RACER_GPU_LANDING_BYTES` (per GPU, not per rail), split into page
slots. The buffer is divided across the RDMA rails, so every GPU can land
pages over every rail. If the buffer has fewer slots than there are rails,
the rails on the GPU's NUMA node come first. A GPU read that needs a page
from a peer asks up to two providers, in turn, to write the encrypted page
into a free slot of that GPU's share on the link's rail. The GPU then checks
and decrypts it. Such a page is not kept in memory or written to disk,
because the host never has its bytes, but concurrent GPU reads of the same
page may share it while any of them still holds it. When no slot is free or
anything fails, the page comes through host memory as before. Landing buffers are registered when GPU mode turns
on and at each RDMA activation, and freed when the RDMA node closes or when no rail could register
them. They are not resized at runtime; changing the setting restarts the
pods. The dataplane logs `stage=gpu-landing` lines when it registers them, and
`racer_rdma_pages_landed_total` counts pages that landed in GPU memory.

`RACER_GPU_STAGING_BYTES` is the GPU memory the dataplane keeps on each GPU for
encrypted pages. It is split into slots of one page rounded up to whole 2 MiB pages (18 MiB). Two
slots are needed for decryption, so a smaller cap means the CPU decrypts. The
other slots hold pages read with cuFile. When all of them are in use, the
dataplane reads the next page through host memory instead. If the image has no
GPU decrypt support, the CPU decrypts every page.

A GPU read fetches its whole `RACER_RANGE_WINDOW_PAGES` window directly, with
cuFile where it can, so a larger window keeps more disk reads in flight. To
load from fast local disks, raise the window, raise `RACER_GPU_STAGING_BYTES`
to hold every page in flight, and raise the client's reads in flight. Each IO
worker's share of `RACER_PLAINTEXT_BYTES` and `RACER_CIPHERTEXT_BYTES` must
still hold the window plus one page. On one NVMe drive, a window of 8, 1 GiB
of staging, and 8 loader reads loaded a 7B model in vLLM in 2.34 seconds,
against 2.44 seconds with the defaults. That is close to the drive's speed.

The dataplane does not request `nvidia.com/gpu`. It is privileged, so it
already sees `/dev/nvidia*`. It only needs the host driver libraries.
`RACER_NVIDIA` in `racer-settings` controls how it asks for them.
The operator reads it and changes the DaemonSet:

| `RACER_NVIDIA` | Behavior |
|--------------------------|----------|
| `env` (default) | Sets `NVIDIA_VISIBLE_DEVICES=all` and `NVIDIA_DRIVER_CAPABILITIES=compute,utility` on the dataplane container, mounts the host `/run/udev` read-only at `/run/udev` (cuFile reads each slab device's filesystem type from the udev database), and mounts a 64 MiB `emptyDir` at `/var/log/cufile` for the cuFile log. The NVIDIA runtime injects the libraries. Other runtimes ignore these variables, so nodes without NVIDIA are not affected |
| `cdi` | Everything `env` adds, plus the Pod annotation `cdi.k8s.io/racer: nvidia.com/gpu=all`. containerd fails to create the container on any node that cannot resolve this CDI device, so use it only when every dataplane node has an NVIDIA CDI spec |
| `off` | Adds none of these: no NVIDIA variables, no CDI annotation, no `/run/udev` mount, and no cuFile log volume. Without driver libraries there is no GPU, so cuFile is not used |

The dataplane image ships `/etc/racer/cufile.json` and sets
`CUFILE_ENV_PATH_JSON` to it. It turns off cuFile compatibility mode, so
cuFile fails instead of copying through host memory and Racer uses its own
host read path, and it logs errors to `/var/log/cufile/cufile.log`. Other
cuFile settings keep their built-in defaults. To use your own file, build an
image from the dataplane image that adds it, and set `CUFILE_ENV_PATH_JSON`
in `racer-settings`. Setting `CUFILE_ALLOW_COMPAT_MODE=true`
turns compatibility mode back on.

On nodes set up by the unbounded agent, the NVIDIA runtime is the default
containerd runtime and runs in CDI mode, so `env` is enough there. These env
entries are operator wiring: workload overrides cannot change them.

### Testing on GPUs

Regular CI has no GPU. It compiles the decrypt kernels with `nvcc` and runs
them on the CPU through an emulation, but never loads them on a GPU. On a
machine with an NVIDIA driver, `nvcc`, and Rust, run:

```sh
make racer-gpu-test
```

This builds `libracer_gpu_cuda.so.1` with its kernels and runs the hardware
tests in `cmd/racer/gpu/tests/hardware.rs`: shim load, device
discovery, import of an exported client allocation, GPU decrypt checked
against the CPU, and cuFile reads. It fails when no GPU is found; pass
`RACER_GPU_TESTS=1` to skip instead. cuFile tests skip when cuFile cannot open
or the directory does not support `O_DIRECT` (set `RACER_GPU_TEST_DIR`); set
`RACER_GPU_TESTS_CUFILE=require` to make that a failure. To also test
`gpu-read` end to end from the Python client, set `RACER_GPU_E2E_SOCKET` to a
running dataplane's client socket and `RACER_GPU_E2E_KEY` to the hex key of an
object it can serve (`RACER_GPU_E2E_METADATA` and
`RACER_GPU_E2E_AUTHORIZATION` are optional). Without the socket, that test is
skipped; set `RACER_GPU_E2E=require` to make it a failure.

The `racer gpu` workflow (`.github/workflows/racer-gpu.yaml`) runs this target
nightly and on demand. It runs only when the repository variable
`RACER_GPU_RUNNER` holds the GPU runner labels as a JSON array, for example
`["self-hosted", "linux", "gpu"]`.

See [Load vLLM models from Racer]({{< relref "guides/racer-vllm" >}}).

## RDMA

RDMA is optional per hop. Peers need the same **nonempty Site**, the page's selected rail
in both published NIC lists, enabled native resources, and compatible local devices.
Different/missing Sites or unavailable rails retain HTTP, without remapping the page.
Site uses only `unbounded-cloud.io/site`, not `net.unbounded-cloud.io/site`. It does not
change placement; in-flight snapshots mean label changes are not immediate revocation.

| `RACER_ENABLE_RDMA` | Behavior |
|---------------------|----------|
| `auto` (default) | Reserve native resources only with usable startup hardware; otherwise HTTP-only until restart |
| `true` | Reserve native capacity even without startup hardware, allowing later discovery to activate devices |
| `false` | Disable native RDMA |

Release images include native verbs support; discovery/provider failures retain HTTP.

### RDMA circuit breaker

Each node can keep a circuit breaker per peer and rail (`RACER_RDMA_BREAKER`, off by
default). Without it, a page whose grant or write fails or times out still falls back
to HTTP within the same exchange, and a lost device is withdrawn at once, so the
breaker only saves the timeout on pages sent to a peer whose link is already known to
be bad. On a 150-node MANA cluster with RDMA cut on 10 nodes for 60 s, it did not
change cold-read time measurably and cost about half a core per node. A busy link is slow, not broken, so the breaker sorts failure signals in
three kinds:

- Hard failures trip it at once: a link being torn down (`link_down`), a link setup
  this node started that failed and was not declined (`setup`), a grant that failed
  with an error (`grant`), or a write, or the arrival of a page the sender reported
  written, that failed with an error (`send`).
- Soft failures trip it only after 3 in a row with no RDMA success with that peer on
  that rail in between: a grant, write, or arrival that did not finish within
  `RACER_RDMA_PAGE_TIMEOUT_MS` (5 s) (`timeout`), or a page whose sender answered
  the grant over HTTP only after that timeout (`slow_fallback`). Any RDMA success
  ends the streak, and a streak with no soft failure for
  `RACER_RDMA_BREAKER_MAX_COOLDOWN_MS` starts over. On a congested link pages keep
  completing between the slow ones; on a stuck link none do.
- Load never trips it (`load`): no free grant slot, flight room, or ciphertext
  budget. That page goes over HTTP and the next one tries RDMA again.

While a breaker is open, pages to and from that peer on that rail
use HTTP at once, with no grant, grant wait, or write. The link keeps reconnecting
in the background. After a cooldown (`RACER_RDMA_BREAKER_COOLDOWN_MS`, 2 s, doubling
for each consecutive trip up to `RACER_RDMA_BREAKER_MAX_COOLDOWN_MS`, 30 s, less up
to a fifth as jitter), one probe page at a time uses RDMA. Its success closes the
breaker, and its hard or soft failure opens it again for the next longer cooldown.
A probe that meets load frees the probe slot for the next page.

A write that fails or runs out of `RACER_RDMA_PAGE_TIMEOUT_MS` goes over HTTP in the
same exchange, so it does not use up one of the page's retries. The sender releases the
receiver's grant, and the receiver keeps the buffer reserved until that release
arrives or the link breaks. A write that lands late can never land in a reused buffer.
`racer_rdma_breaker_trips_total{reason}` counts trips by `reason`: `link_down`,
`grant`, `send`, `timeout`, `slow_fallback`, or `setup`.
`racer_rdma_breaker_soft_failures_total{reason}` counts soft and load failures that
did not trip a breaker, by `reason`: `timeout`, `slow_fallback`, or `load`; a high
count with few trips means congestion, not broken links. `racer_rdma_breaker_closes_total`
counts breakers closed by a probe, `racer_rdma_breaker_open` is the number of breakers
open or half-open now, `racer_rdma_breaker_http_total` counts pages sent or planned over
HTTP because a breaker was open, and `racer_rdma_page_timeouts_total` counts grants and
writes that ran out of `RACER_RDMA_PAGE_TIMEOUT_MS`. Trips and closes are logged, at most
one line per second.

Native activation registers the page-buffer pool within 5 s plus 1 s per GiB of
`RACER_REGISTERED_BYTES`. When it fails, the dataplane logs
`stage=rdma-activation state=failed`, counts `racer_rdma_activation_failures_total`,
reports no RDMA rails (so peers stop planning RDMA to it), and retries; it
advertises its rails again once activation succeeds. `racer_rdma_active` is 1
while rails are active, and `racer_rdma_free_buffers` counts free page buffers.
`racer_rdma_receive_buffer_hold_nanoseconds_sum` and `_count` time each received
page's buffer from its grant until it returns to the pool. A received page is
copied out of its buffer before it is decrypted, delivered, or persisted, so
divide the sum by the count: a hold well above the sender's send time means
something still pins received buffers, and the pool then limits receive rate.
`racer_rdma_http_fallback_total` counts pages that planned RDMA but went over
HTTP; `racer_rdma_fallback_no_link_total` and `racer_rdma_fallback_grant_total`
split out the two common reasons: no ready link to the peer yet, and a grant that
failed. `racer_rdma_fallback_grant_overloaded_total` counts the subset of grant
failures that found every page buffer of the rail, or every grant slot of the
link, in use; for page requests it should stay near 0, since those push back
instead. A receiver whose grant finds no free slot on its chosen rail first tries
the peer's other ready rails on the same NUMA node, then waits in line on the
chosen rail for up to 1 s (at most half the time left in the request). Waiting
requests are queued in the native thread and use no CPU: each released buffer or
grant slot goes to the oldest waiting request on its rail. Nothing is reserved
while a request waits. A grant is held from the request until the sender's page
lands, so exhaustion on a healthy link usually means a slow sender, and the
registered pool sets how many pages a node receives at once. If no slot frees in
time, a page request fails `Overloaded` and its range window retries it with
backoff, rather than moving the page over HTTP at a much higher receive CPU cost.
`racer_rdma_grant_backpressure_total` counts those page requests. Non-page
bodies, and grants that fail because a link broke or a device was lost, still
fall back to HTTP. `racer_rdma_grant_waits_total` counts grant requests that
waited, `racer_rdma_grant_wait_granted_total` those that then got a slot, and
`racer_rdma_grant_waiters` the requests waiting now. A steady backpressure count
means the pool is too small for the senders' disk latency: raise
`RACER_REGISTERED_BYTES`. Each link may hold as many grants as its rail has page buffers
(the `RACER_REGISTERED_BYTES` pool split across rails, at most 1024), capped by
the largest value whose queue pair and completion queue fit the device's
reported `max_qp_wr` and `max_cqe` (and, with memory windows, `max_mw`), and by
the peer's advertised limit. Registration mode posts up to 4 send work requests
per send, so a device that reports 2048 for both, as MANA does, allows 111
grants per link. A device that cannot fit even one grant fails RDMA activation.
A single native thread per node (the RDMA owner) handles every grant, write,
and release. If the owner spends 2 s inside one pass, or leaves a worker's
commands queued for 2 s, `racer_rdma_owner_stalled` reads 1 and no rail counts as
ready: new exchanges go over HTTP until the owner catches up. A grant whose command
finds the worker's queue to the owner full also goes over HTTP at once, instead of
pushing back, and counts in `racer_rdma_grant_owner_queue_full_total`.
`racer_rdma_owner_passes_total` and `racer_rdma_owner_commands_total` count owner
passes and the commands they handled. `racer_rdma_owner_immediate_wakes_total{reason}`
counts passes followed at once by another pass instead of a sleep: `commands` (the
pass used its whole command budget) or `completions` (a link reaped a full
completion batch, or has completions that raise no event). Passes that keep growing
on an idle node mean the owner is spinning; a stalled owner with passes that do not
grow means it is stuck inside one pass.
`racer_rdma_links_ready`, `racer_rdma_links_connecting`, `racer_rdma_links_backoff`,
and `racer_rdma_links_breaking` count the node's links by state.
`racer_rdma_link_breaks_total{reason}` counts links torn down, by `reason`:
`error` and `timeout` (back off), `abandoned` (a failed setup exchange; backs off),
`declined` (the peer refused the setup with `409`, `429`, or `503`; retries after
1 s), `peer_closed` and `closed` (no backoff), and `grant_expired` (an exposed grant
was neither written nor released within 10 s of its deadline, so the link closed;
also counted as `closed`), and `device_lost` (the link's RDMA device was removed;
no backoff). `racer_rdma_link_setups_total{outcome}` counts link
setups this node initiated as `success`, `declined`, or `failed`.
`racer_rdma_prefetch_failures_total` counts sends whose source page could not
be mapped into the NIC ahead of the write (window mode prefetches each page
through implicit on-demand paging); those writes may stall on NIC page faults.
`racer_rdma_fallback_overflow_total` counts page bodies received over HTTP because the hop
qualified by Site and rail but was not among the 256 RDMA peers both nodes chose.
The receiving node of each such hop counts each delivered body once. Requests that
deliver no body, such as misses, overloads, and failed exchanges, do not count.
`racer_rdma_plan_http_total{reason}` counts page exchanges this node sends to a peer
over HTTP, without ever planning RDMA, while it publishes RDMA rails itself. `reason` is
`no_rail` (both nodes share a Site and publish rails, but not the page's rail, which
usually means the nodes number their rails differently), `ineligible` (different or
missing Site, or the peer publishes no rails), `overflow` (the hop qualified but is not
among the RDMA peers both nodes chose), or `direct_http` (the exchange was sent directly
over HTTP). Nodes that publish no rails count nothing.
`racer_rdma_send_declined_total{reason}` counts granted pages the sending node declined
to write, so the body went over HTTP and the receiver's grant was released: `not_page`
(the response carried no page body) or `plan_mismatch` (the sender's plan for the page
and hop disagreed with the granted rail, for example across a Site or rail change).

An RDMA device can disappear while the dataplane runs, for example when an
Azure host hot-unplugs and later replugs a MANA VF (`hv_netvsc ... VF slot 0
removed` in `dmesg`), sometimes bringing it back under a new name such as
`roceP30832p0s0`. The dataplane notices in either of two ways: the RDMA
connection manager reports the device removed, or the device's PCI address is
no longer at its activated name under `/sys/class/infiniband` (checked every
2 s for rails whose PCI address is known). It then breaks every link on the
device at once, so pending RDMA transfers fail and their reads fall back to
HTTP without waiting for their deadlines. It also deactivates RDMA, so
`racer_rdma_active` drops to 0 and new reads plan HTTP, and it stops
advertising its rails, the same way as after a failed activation. Each loss
logs `stage=rdma-device state=lost` and counts `racer_rdma_device_losses_total`.
While the device is missing, the dataplane does not retry activation. Once
every lost PCI address is back in sysfs and the old links are gone, it
reactivates RDMA. It does not wait for received pages that are still held, for
example by a read in progress: no NIC can reach them once their device is gone,
so their page buffers are freed later, when the pages are released. Until then
they stay counted against `RACER_REGISTERED_BYTES`, so a reactivation that
cannot fund its pool yet fails and retries. While it waits, the dataplane logs
`stage=rdma-device state=waiting reason=...` each time the reason changes:
`device_missing:<device>` (nothing at its PCI address that discovery would
keep: a device whose ports have no GID table or only a null GID, such as the
`rdmaP...` twin that stays while a MANA VF's RoCE device is unbound, does not
count, and the
dataplane waits for as long as it takes), `device_inactive:<device>` (back, but
no port is ACTIVE yet), `device_settling`, `drain_<blocker>` (the old round is still
releasing its `links`, its `buffers` that a NIC may still use, its
`registering` pools, or never releases a `leaked` native object),
`inventory_refresh:<error>`, or `device_undiscovered`.
Device names at each lost PCI address must hold still for 5 seconds before
the dataplane lists devices again, so a udev rename of a replugged VF (for
example `mana_0` to `roceP30832p0s0`) lands first: libibverbs keeps the name it
first saw for the life of the process. Before reopening after a loss, the
dataplane creates fresh `/dev/char/<major>:<minor>` nodes for every verbs
character device in `/sys/class/infiniband_verbs`, because a container's
bind-mounted `/dev/infiniband/uverbsN` keeps reaching the removed device and
rdma-core falls back to `/dev/char`. It refreshes them again whenever the
numbers sysfs lists change or one of their nodes is missing, including after
an attempt that could not discover the device, so a device that returns with
a new minor number after any outage is reopened without a restart. After a loss, activation retries every
2 seconds instead of backing off. Once the device is back with an ACTIVE port
and a non-null GID, 6 consecutive attempts that cannot use it (activation
failures or `device_undiscovered`; waiting while the device is missing,
inactive, or settling never counts) log `stage=rdma-device
state=restarting reason=device_unusable` and fail the worker, so the process
restarts with a fresh verbs device list. Reads use HTTP until then.
`racer_rdma_reactivation_waiting_device` is 1 while it waits for a device and
`racer_rdma_reactivation_waiting_drain` is 1 while it waits for the old round.
A failed activation logs `stage=rdma-activation state=failed` when it withdraws
the rails, and again on the 1st, 2nd, 4th, 8th, and so on consecutive failed
retry. A device that came back under a new name
refreshes the device inventory first; rails are keyed by PCI address, so it
keeps its rail. Only ports with a non-null GID are inventoried, so a VF that
comes back as two devices at one PCI address (for example `rdmaP30832p0s0`
with a null GID and `roceP30832p0s0` with the RoCE GID) uses the RoCE one.
While its rails are withdrawn, the membership lists none for this node, so
reactivation (like the retry after a failed activation) uses the discovered
rails and then advertises them again. A successful reactivation logs
`stage=rdma-device state=reactivated` and counts
`racer_rdma_device_reactivations_total`.

A dataplane with RDMA enabled that discovered no usable rail (it logs
`stage=rdma-activation state=waiting reason=publication_empty:discovered=0`),
for example because it started while a device was unbound, rescans every 10
seconds. The rescan first reads `/sys/class/infiniband` only, listing devices
with an ACTIVE port and a non-null GID, and logs `stage=rdma-rescan
state=sysfs devices=[...]` each time that list changes. The list must then hold
still for 5 seconds, so a returning device is opened under its final udev name.
Only when it lists a
device the inventory lacks does the dataplane refresh `/dev/char` nodes and
rediscover ports (without starting a connection manager listener). Once that
finds rails, it logs `stage=rdma-rescan state=discovered`, activates them, and
advertises them on success. A node without RDMA hardware therefore costs one
directory read per rescan and logs once.

The RDMA owner thread hands queue pair and connection manager teardowns to
helper threads named `racer-rdma-reaper`, at most one teardown every 2 ms per
helper. A burst of link closes, such as every link going idle at once or a node
close, therefore reaches the device spread out. A teardown that hangs in the
kernel also cannot stall the owner. The dataplane keeps every buffer the queue
pair may still use until the teardown returns, so a teardown that never returns
leaks that memory instead of freeing it early. Such a hang also keeps RDMA from
reactivating until the dataplane restarts. Restarting may not help either: a
thread in uninterruptible sleep can keep the process from exiting, and that
takes a node reboot.

The Node annotation `racer.unbounded-cloud.io/rdma-nics` accepts at most 64 entries:

```json
[{"device":"mlx5_0","port":1,"rail":0}]
```

`port` is 1..255; `rail` is 0..65535. Optional `gid` is 32 lowercase hex digits;
optional `numa_node` is `uint32`. Device/port pairs must be unique; NICs may share a rail.

Absent administrator policy uses eligible active ports in PCI BDF/port order,
published from the dataplane's own Pod report. A port is eligible on a device with
type 2B memory windows (window mode), or on a device without memory windows that
supports RC queue pairs and has an IPv4-mapped RoCE GID (registration mode, for
example Azure MANA guest RDMA).

### Registration mode

Registration mode keeps transfers zero copy without memory windows or on-demand
paging. A grant registers the granted page buffer itself for remote write and
sends that registration's key; the arrival deregisters it, so the key works for
exactly one transfer into exactly one buffer. A write registers the sender's
page memory for local access, writes straight from it, and deregisters it on
completion. Page memory is 2 MiB aligned and huge-page backed, so these
registrations are cheap. Queue pairs connect through the RDMA connection manager
on the node's IPv4 address:

- The dataplane must run in the host network namespace: patch the `racer`
  DaemonSet with `hostNetwork: true` and `dnsPolicy: ClusterFirstWithHostNet`.
- Each dataplane listens on TCP port `RACER_RDMA_CM_PORT` (default 18515) of its
  RDMA device's address for connection requests. Requests must name a link both
  peers set up through the authenticated exchange.
- The image ships rdma-core 65, which has the MANA RC provider (rdma-core 54 or
  later is required).
- Window keys rotate per grant in both modes; registration mode reports the
  whole written length through the immediate's top byte, so link tags use 24 bits. Explicit
`[]` disables eligibility; a list overrides automatic rails. Discovery cannot infer cabling
or correct asymmetric initial inventories: use explicit mappings when ordinals do not match.

The private identity directory's `rdma-rails.json` (on the host,
`/var/lib/racer/identity/rdma-rails.json`) preserves physical-port reservations across
renewal/restart. A port is identified by its PCI address and port number, or by its device
name when sysfs reports no PCI address, so a device renamed by a VF hot-unplug/replug (for
example `mana_0` becoming `roceP30832p0s0`) keeps its rail. Missing ports retain their rails
until a new port appears; the new port then takes the lowest rail whose port is missing, so
a replaced device reuses its predecessor's rail. Ports with no reservation append. The
journal is versioned; a legacy unversioned journal, which was keyed by device name, is
discarded on start and rails are renumbered in PCI BDF/port order. Do not delete it
independently of coordinated topology changes. Corruption fails enrollment instead of renumbering rails.

Dataplanes run privileged as root with escalation allowed and a read-only root filesystem.
HostPaths `/var/lib/racer/identity`, `/var/lib/racer/slabs`, and `/run/racer` are writable;
host `/dev` at `/host/dev` and `/dev/infiniband` are read-only for directory entries,
not device-I/O isolation. Raw cache writes through `/host/dev` are intentional. This is
hostPath access, not device-plugin/CDI allocation or a host security boundary. Kubelet may
create an empty device directory on HTTP-only nodes; Racer does not provision drivers/devices.

Verify from the actual Pod namespaces:

- Compatible drivers/firmware/providers, active MW2B ports, and sufficient pinned-memory/resources.
- Visible `/sys/class/infiniband`, `/sys/class/infiniband_verbs`, and `/sys/devices`
  targets; no host `/sys` mount is automatically added.
- RDMA namespace mode allows device access. Privilege/mounts do not override exclusive
  namespace ownership; explicitly enable host networking if needed, reviewing port exposure/conflicts.
- RoCE has the correct namespace-local Ethernet device, address, routing, and GID.
  The adapter uses **GID index 0**. Annotation `gid` is a matching constraint, not an index
  selector or RoCE configuration; retain HTTP when another index is required.

Coordinate dataplane and origin-adapter upgrades and quiesce traffic; rolling updates
do not make mixed versions safe.
Validate transfers, HTTP fallback, and recovery on target hardware before relying on RDMA.
