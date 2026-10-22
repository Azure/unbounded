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
| Fields | Optional `spec.origin` for an operator-managed Azure origin; optional `spec.priority` for the cache's share of node capacity; no `status` |
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
[Managed Azure origin](#managed-azure-origin)). Cached data is disposable. A cache's Kubernetes UID is its
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

The controller and dataplane must run the same version. A mismatched pair
rejects the cache catalog.

### Managed Azure origin

Set `spec.origin` to have the operator run a `racer-object origin` DaemonSet for
the cache. Leave it unset to deploy your own origin, as described in the
[racer-object reference](racer-object.md#deploying-the-origin). Do not do both
for one cache. Set or omit `spec.origin` when you create the cache: it cannot be
added or removed later. To change the origin, create a new ClusterCache, which
starts with an empty cache. Deleting the ClusterCache removes the managed origin.

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
| `metadataTTL` | Optional. How long object metadata is reused; not negative |
| `requestTimeout` | Optional. Limit for each upstream request; positive |
| `maxConcurrentRequests` | Optional. Upstream requests per origin pod, 1 to 65536 |
| `azure.containerURL` | Required. The one blob container the origin may read, as `<account URL>/<container>`, for example `https://account.blob.core.windows.net/models`. `http` or `https`, no credentials, query, fragment, or trailing slash. The container name follows Azure rules: 3 to 63 lowercase letters, digits, and single hyphens. Azurite-style URLs may include one account path segment. Immutable |
| `azure.credential` | Optional. Omit for anonymous reads. Set exactly one of `default` or `workloadIdentity`; either requires `https` |
| `azure.credential.default` | `DefaultAzureCredential`. Optional `clientID` for a user-assigned managed identity, or `secretRef` to a Secret in the operator namespace whose keys become environment variables |
| `azure.credential.workloadIdentity` | Microsoft Entra Workload ID. Requires `clientID`, optional `tenantID`. The cluster must run the Azure Workload Identity webhook, and the identity must trust the origin ServiceAccount |

Unset optional fields use the `racer-object` defaults.

The operator names the DaemonSet and its ServiceAccount `racer-origin-<cache>`
in the Racer namespace. It copies node affinity, node selector, tolerations, and
image pull secrets from the `racer-dataplane` DaemonSet, and waits until that
DaemonSet exists. The origin runs as UID and GID 65532 and serves
`/run/racer/<cache>/origin/socket`, which the dataplane creates.

Each ClusterCache maps to one container. You cannot change
`azure.containerURL` after creation. To read a different source, create a new
ClusterCache. It gets a new UID, so it starts with an empty cache. A pod that
reads two containers needs two ClusterCaches and two sidecars.

Sidecars attach with `racer.unbounded-cloud.io/object-cache: <ClusterCache name>`.
They can read any blob in the cache's container. The origin rejects requests
for any other container with `403`.

If the spec is not valid, the operator skips that origin and reports
`OriginInvalid`. It reports `OriginPending` while the dataplane DaemonSet is
missing.

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

Do not turn off cluster-wide admission protections. Authorize these workloads
narrowly. RDMA is optional; see [RDMA](#rdma).

Racer origins run as the non-root UID/GID 65532 and do not need privileged mode.
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
| Annotation `shares` | Positive decimal `uint32` placement weight; overrides enrolled shares; default 4 |
| Annotation `rdma-nics` | Administrator NIC/rail policy; see [RDMA](#rdma); explicit `[]` disables eligibility |
| Annotation `block-devices` | Startup-only regex selecting raw cache devices by `disk/by-id` basename; see [raw-device storage](#raw-device-storage) |
| Annotations `enrolled-shares`, `enrolled-rdma-nics` | Controller-managed authenticated dataplane reports; do not edit |
| Annotation `last-admitted-member` | Controller-managed, Node-UID-bound retained membership; do not edit |

Malformed membership annotations retain admitted attributes; invalid new nodes are omitted.
Site uses current labels. Invalid `block-devices` selectors instead fall back to slab files.

## Dataplane configuration

Tune `racer-dataplane-config` in the installation namespace (normally `unbounded-system`).
The operator preserves administrator data and hashes edits into managed rollouts.
Explicit workload environment wiring takes precedence over ConfigMap imports.
Numeric values are unsigned decimal strings: `268435456`, not `256Mi`.

| Setting | Default | Scope / constraint |
|---------|---------|--------------------|
| `RACER_MAX_THREADS` | Unset (automatic) | Total userspace I/O and crypto threads; explicit value at least 2 |
| `RACER_IO_WORKERS` | Unset (automatic) | Exact I/O count; requires `RACER_CRYPTO_HELPERS` |
| `RACER_CRYPTO_HELPERS` | Unset (automatic) | Exact helper count, from 1 through the exact I/O count; requires `RACER_IO_WORKERS` |
| `RACER_ALLOW_SMT` | `false` | Use eligible physical cores by default; `true` opts into logical CPUs |
| `RACER_ENABLE_RDMA` | `auto` | `auto`, `true`, or `false`; see [modes](#rdma) |
| `RACER_ENABLE_GPU` | `auto` | `auto`, `true`, or `false`; see [GPU reads](#gpu-reads) |
| `RACER_GPU_STAGING_BYTES` | `268435456` (256 MiB) | Cap on ciphertext staging memory per GPU, in bytes. Must be a nonzero multiple of 2 MiB. Below about 34 MiB the GPU does not decrypt and the CPU does; see [GPU reads](#gpu-reads) |
| `RACER_SHARES` | `4` | Enrollment proposal; explicit Node `shares` wins |
| `RACER_PLAINTEXT_BYTES` / `RACER_CIPHERTEXT_BYTES` | Unset: `67108864` (64 MiB) / `134217728` (128 MiB) per final I/O worker | Independent plaintext/ciphertext admission budgets; an explicit value is node-wide and divided among I/O workers |
| `RACER_DIRTY_BYTES` | Unset: `167772160` (160 MiB) per final I/O worker | Dirty-data budget; an explicit value is node-wide |
| `RACER_REGISTERED_BYTES` | Unset: `100663296` (96 MiB) per final I/O worker | Native RDMA memory budget when enabled; an explicit value is node-wide |
| `RACER_REQUEST_CONTEXT_BYTES` | Unset: `33554432` (32 MiB) per final I/O worker | Request-context budget; an explicit value is node-wide |
| `RACER_DISK_PAGE_ENTRIES` | `65536` | Node-wide disk page-index budget in file mode; configured value at most 1,048,576; device mode can raise it as described below |
| `RACER_ADMISSION_MODE` | `second-sight` | Disk retention policy: `disabled` or `second-sight`; does not disable request resource admission |
| `RACER_ADMISSION_HISTORY_BYTES` | `4194304` (4 MiB) | Node-wide Bloom history budget; 32 bytes..512 MiB, divided among final I/O workers, requiring at least 32 bytes per worker |
| `RACER_ADMISSION_PERIOD_SECS` | `60` | History rotation period; 1..86,400 seconds |
| `RACER_WARM_MAX_TTL_SECS` | `3600` | Maximum admitted warm TTL; 1..86,400 seconds |
| `RACER_WARM_FILL_TIMEOUT_SECS` | `600` | Independent background fill deadline; 1..86,400 seconds |
| `RACER_WARM_MAX_JOBS` | `16` | Node-wide tracked warm versions, including completed jobs until expiry; 1..1024 |
| `RACER_WARM_WINDOW_PAGES` | `2` | Maximum in-flight pages per warm job; 1..64 |
| `RACER_RANGE_WINDOW_PAGES` | `2` | Pages each read fetches ahead of delivery, in any order; 1..64. Per-worker plaintext budget must fit one page more than the window |
| `RACER_RANGE_SPILL_PAGES` | `0` | Pages past the read window that a read may fetch to local disk under a short hold; 0..1024; `0` disables spill |
| `RACER_RETAINED_SEGMENT_PERCENT` | `50` | Per-worker hard-hold cap as a percentage of segment count and page-index capacity; 1..100; free-segment reserve still applies |
| `RACER_CHECKPOINT_BYTES` | `67108864` (64 MiB) | Node-wide checkpoint working-set budget; at most 512 MiB; device mode can raise it as described below |
| `RACER_ACTIVE_PATH_SEARCHES` | `8` | **Per I/O worker** distinct unfinished path-search limit; 1..64, independent of cache size |
| `RACER_SLAB_BYTES` | `1073741824` (1 GiB) | **Per I/O worker** file-backed slab geometry; positive multiple of segment size; does not limit device-mode capacity |
| `RACER_SEGMENT_BYTES` | `67108864` (64 MiB) | Per-worker segment size; must fit a page plus storage overhead |
| `RACER_FREE_SEGMENT_RESERVE` | `2` | Positive reserved segment count per I/O worker, less than that worker's total segments |
| `RACER_SLAB_DIRECTORY` | `/var/lib/racer/slabs` | Slab files and checkpoints; still required and writable in device mode |
| `RACER_DEVICE_DIRECTORY` | `/host/dev` | Device root; scan its `disk/by-id` directory; explicitly wired by managed workloads |
| `RACER_DEVICE_BUSY_WAIT_SECS` | `120` | Startup bound for retrying selected devices whose exclusive open is busy (`EBUSY`); all busy devices share one deadline; 0..3,600 seconds, `0` skips busy devices immediately |
| `RACER_CONNECTIONS_PER_NEIGHBOR` | `2` | Per-worker neighbor connection cap; at most 1024 |
| `RACER_ORIGIN_CONNECTIONS_PER_CACHE` | `32` | Per-cache, per-worker origin cap, independent of peer cap; at most 1024 and configured client connections (the default is lowered to fit), further clamped to the worker's outbound connection admission (a quarter of client connections). When that admission is at least 2, one slot is reserved for origin metadata requests across all caches, so page and peer connections use at most one fewer |
| `RACER_PEER_INFLIGHT_MAX` / `RACER_PEER_PER_NEIGHBOR_MAX` | `256` / `32` | Node-wide peer-exchange admission caps |
| `RACER_REPLICA_SELECTION` | `spread` | Which page owner a non-owner asks first. `spread`: direct neighbors first, then a per-node hash spreads pages across all owners. `primary`: always the first-ranked owner |
| `RACER_REQUEST_TIMEOUT_MS` / `RACER_PEER_ATTEMPT_TIMEOUT_MS` | Each `30000` | Independent request / local peer-exchange deadlines; 1..86,400,000 ms |
| `RACER_READER_STALL_TIMEOUT_MS` | `10000` | At least 1 ms and no greater than request timeout |
| `RACER_SHUTDOWN_TIMEOUT_MS` | `30000` | 1..3,600,000 ms |

Exact sizing requires both count settings. Their sum must fit `RACER_MAX_THREADS`;
the cap is never raised implicitly. The first NUMA node in the normal placement
preference order must be known and have enough eligible CPUs for the whole split.
Other host NUMA nodes are allowed, but exact placement does not span nodes or skip
an undersized preferred node. Insufficient CPU quota or per-I/O resource budgets
fails startup rather than reducing counts. For an isolated 20-I/O comparison, use
10 helpers with cap 30 or 20 helpers with cap 40. Keep all resource budgets fixed.
Omit both count settings to retain automatic sizing.
Extra helpers change available crypto execution capacity, not I/O ownership or
per-I/O budgets. They do not guarantee higher throughput; compare matched workloads
before drawing a performance conclusion.

Automatic sizing considers CPU affinity, quota, NUMA locality, and progress reserves.

### Raw-device storage

Set the Node annotation `racer.unbounded-cloud.io/block-devices` to a regex over
basenames in `/host/dev/disk/by-id`, not full paths. The controller accepts at most
1 KiB (1,024 bytes) and validates with Go's `regexp.Compile`. Invalid or oversized
values are ignored with a warning. The dataplane compiles the selector with Rust's
`regex` crate; use syntax accepted by both engines. Matching is **unanchored**:
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
subsegment tail is unusable. `RACER_SLAB_BYTES` is ignored for device capacity,
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

Checkpoints store completed-page hold deadlines and bind each worker to
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

`POST /v1/objects/<key>/gpu-read` writes into GPU memory. It works like
`host-read`, with these differences:

- The descriptor comes from `cuMemExportToShareableHandle` with the POSIX file
  descriptor handle type. `Racer-Allocation-Size` is required and must be the
  size passed to `cuMemCreate`.
- The answer is 501 when GPU mode is off or the memory is on a GPU the
  dataplane did not find. It is 400 when the size does not match the memory or
  the window does not fit in it.
- A success has `Racer-Delivery: gpu`. A page frame means the GPU copy of that
  page is complete.
- When the read ends for any reason, the dataplane waits for its GPU work,
  then releases the memory. See [GPU reads](#gpu-reads).

### Warm and hard retention

Send `HEAD` on the client socket with `Cache-Control: max-age=<seconds>` to request
warming of the whole selected version. The value is a positive integer TTL in
seconds, clamped to `RACER_WARM_MAX_TTL_SECS`. No `max-age`, or `max-age=0`, is an
ordinary metadata request. Duplicate, malformed, negative, or overflowing
`max-age` values are rejected. `If-Match` selects one strong ETag; without it,
Racer resolves the current version. The retention hint applies only to HEAD,
not to reads or origin requests.

| HEAD result | Meaning |
|-------------|---------|
| `200` with `Racer-Retain-Until` | Retention admitted; the header is an absolute Unix timestamp in milliseconds, not a TTL or completion signal |
| `200` without `Racer-Retain-Until` | Metadata succeeded but retention was refused; do not assume warming started |
| Non-success status | Metadata, version selection, or request processing failed |

Admission reserves capacity before returning the header. Filling continues
asynchronously with a separate cancellation scope. The TTL starts at HEAD
admission and runs during the fill, so a short TTL can expire before all pages
load. Repeated requests for the same cache, key, and ETag share the job and can
extend, but never shorten, its hold. Admission can be refused because of job,
storage, index, or request-context limits, or shutdown. A successful metadata
response does not make these limits optional.

Warm stores pages in the normal cache topology. Each page is held and stored by
its **primary owner**, the first-ranked node for that page under the membership
the requesting node used at admission. The requesting node holds only pages it
is the primary owner of. Admission works like this:

1. The requesting node finds the primary owner of every page.
2. It reserves holds for its own pages, and sends one hold request to every
   other primary owner. Each owner checks that it is the primary owner of at
   least one page, then reserves holds for the pages it owns.
3. Admission is all or nothing. If any owner refuses or cannot be reached, the
   requesting node releases every hold it asked for and the HEAD response has
   no `Racer-Retain-Until`.
4. `Racer-Retain-Until` is the earliest expiry that any owner accepted. An owner
   caps a hold at its own `RACER_WARM_MAX_TTL_SECS`.

The warm job then asks each owner to fill its pages. It starts fills in
ascending page order, at most `RACER_WARM_WINDOW_PAGES` at a time, and pages can
finish in any order. A remote owner fills the page through its own normal
path, writes it to its own disk under the hold, and replies with an
acknowledgement only. No page bytes go back to the requesting node. Each fill
request carries the admitted version's length and content type, and an owner
that fetches a copy with different metadata refuses to store or acknowledge it,
which fails the job. A busy or
unreachable owner is retried up to 3 times. If a page still fails, the job
stops and sends a best-effort release to every owner. Owners that do not get the
release keep their holds until expiry.

Owners track holds per requesting node and warm job (a lease), so several nodes
can warm the same version on one owner. An owner frees unused hold capacity only
after the last lease for that version is released or expires. A release never
drops pages already written under a hold; they stay held until the hold expires.

Lease entries are bounded. A repeated HEAD for the same requesting node and warm
job extends its existing lease (never shortening it) instead of adding one. A
new lease is refused as overloaded once the version has 64 live leases or the
node has 64 times `RACER_WARM_MAX_JOBS` live leases; expired leases are pruned
first, and existing leases are never affected by a refusal.

Reads of that exact version on the requesting node follow the warm job. Each
page a read needs waits only for that page's warm fill, not for earlier pages.
While it waits, the read starts no competing peer requests or fills. After a page is warm, a read of a page held on this node reads it
from local disk. A read of a page held by another owner uses the normal fill
path, which reaches that owner's held copy. Their own deadlines and credits
still apply. If the warm
job fails or expires, waiting reads use the normal fill path. A different ETag
never joins the old job. Reads on other nodes do not wait for warm progress.

Limits of primary-owner holds:

- **Membership changes.** Holds are not moved when membership changes. If a
  page gets a new primary owner, the old owner still holds its copy until
  expiry, and reads may fill the page again from the new owner's path.
- **Clock skew.** Hold expiry travels between nodes as a wall-clock time. Each
  owner compares it with its own clock, so skew between nodes shifts the
  actual expiry by the same amount.
- **Mixed versions.** Older dataplanes do not support remote holds. A warm that
  needs an older owner is refused.

Retention is separate from freshness. An unpinned request still refreshes expired
current-version metadata. Pin `Get` to the returned ETag to read the warmed version;
retention does not force later unpinned reads to select it.

Hard holds bypass second-sight and veto both page-index eviction and reclamation
of a segment containing a held record. Each owner charges **one whole segment per
held page**, including short pages and existing local copies. This is conservative
capacity accounting, not a dedicated warm tail or one-page physical layout.
Existing local copies stay where they are and are not rewritten. The charge
leaves room for the eviction veto even when records are fragmented across segments.
It can reject a request well before the disk's payload-byte capacity is full.
Each worker must fit its pages within the retained percentage of both segments
and index entries, while preserving the free-segment reserve and unused index
reservations. Worker imbalance can also cause refusal.

Progress means verified ciphertext has been published on disk, not merely queued
for writing. If a later page fails, completed disk pages keep their admitted
deadlines; unused reservation capacity is released after outstanding persistence
finishes. Retention is not a guarantee against corruption, key loss, cache removal,
or storage failure.

The background job owns a charged, zeroizing copy of `Authorization` and `Metadata`
in memory only. It uses the fill timeout (600 seconds by default), separate from
the HEAD deadline and retention TTL. Credentials are never checkpointed. On
restart, jobs, waiters, and unused reservations are gone. Checkpoints restore
only unexpired holds for completed disk records. Live hold expiry waits for both
wall and monotonic deadlines; restart has only the saved wall-clock deadline.
Neither payload nor checkpoint publication adds `fsync`, so process or host
failure can lose pages or holds.

Warm gauges refresh on worker health ticks (normally every 100 ms). They report
the shared node registry once, without worker, object, or version labels. A
stalled worker can leave an older sample visible; samples are not atomic with
storage or request state.

| Metric | Meaning |
| --- | --- |
| `racer_warm_tracked_jobs` | Tracked versions, including completed jobs until expiry or removal; not just running fills |
| `racer_warm_waiters` | Registered GET page waits for warm progress; excludes HEAD admissions and unregistered reads |
| `racer_warm_held_pages` | Published indexed pages under live hard holds; excludes unused reservations and pending writes |
| `racer_warm_held_payload_bytes` | Logical payload bytes of those pages, excluding tags, headers, and padding |
| `racer_warm_held_segments` | Distinct segments containing those pages, summed across storage workers |

Held gauges use each storage worker's latest health snapshot. Expired holds are
excluded after both wall and monotonic deadlines pass. Recovered holds count even
without a live warm job. Repeated samples replace worker values rather than add
them; the node sum is not an atomic storage snapshot.

Counters record retention decisions after metadata succeeds, admitted job outcomes,
and genuine registered GET waits. Immediate warm hits, registry misses, and failed
waiter registration do not count as waits. Caller disconnection does not change a
detached admission's decision or count it twice.

| Counter | Meaning |
| --- | --- |
| `racer_warm_accepted_total` | Accepted HEAD retention admissions, including coalesced requests |
| `racer_warm_refused_total` | Refused HEAD retention admissions |
| `racer_warm_completed_total` | Jobs that finished publishing their pages |
| `racer_warm_failed_total` | Admitted jobs that stopped before completion, including abandoned drivers |
| `racer_warm_wait_started_total` | Registered GET waits, counted once rather than on each poll |
| `racer_warm_wait_completed_total` | Registered waits satisfied by warm publication |
| `racer_warm_wait_fallback_total` | Registered waits released to normal Fill after warm stopped |
| `racer_warm_wait_failed_total` | Registered waits ended by error, cancellation, deadline, or abandonment |
| `racer_warm_remote_hold_accepted_total` | Hold requests this node sent that a remote owner accepted |
| `racer_warm_remote_hold_refused_total` | Hold requests this node sent that a remote owner refused or that failed |
| `racer_warm_remote_fill_completed_total` | Pages a remote owner acknowledged as written under its hold |
| `racer_warm_remote_fill_retried_total` | Remote page fills retried after a busy or unreachable owner |
| `racer_warm_remote_fill_failed_total` | Remote page fills that failed and stopped the job |
| `racer_warm_remote_release_sent_total` | Releases a remote owner acknowledged |
| `racer_warm_remote_release_failed_total` | Releases that failed; that owner keeps its hold until expiry |
| `racer_warm_owner_hold_accepted_total` | Hold requests this node accepted as primary owner |
| `racer_warm_owner_hold_refused_total` | Hold requests this node refused, including when it owns none of the pages |
| `racer_warm_owner_fill_completed_total` | Pages this node wrote to disk for a remote warm job |
| `racer_warm_owner_fill_failed_total` | Remote warm fills this node refused or failed |
| `racer_warm_owner_released_total` | Leases released by remote warm jobs |

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
always admitted. Warm and spill holds are never evicted.

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
| `racer_disk_class_index_evicted_pages_total{classification}` | Victim mappings removed to reserve page-index capacity |
| `racer_disk_class_index_evicted_payload_bytes_total{classification}` | Logical payload bytes of those index victims |
| `racer_disk_class_segment_evicted_pages_total{classification}` | Victim mappings removed by bounded segment reclamation, including index-only segment reclamation |
| `racer_disk_class_segment_evicted_payload_bytes_total{classification}` | Logical payload bytes of those segment victims, not physical reclaimed disk space |
| `racer_disk_class_read_payload_bytes_total{classification}` | Logical page payload bytes returned successfully by StoreReader after framing and mapping checks |
| `racer_disk_pending_payload_bytes` | Total logical payload bytes owned by pending writes, including submitted writes until cleanup |
| `racer_disk_indexed_payload_bytes` | Total logical payload bytes in retained index mappings, including restored mappings |
| `racer_cache_disk_resident_bytes{cache}` | Gauge of logical payload bytes in retained index mappings for one ClusterCache, summed over workers |
| `racer_cache_disk_share_bytes{cache}` | Gauge of the ClusterCache's fair share of disk, summed over workers. Only published ClusterCaches have series; see [Priority](#priority) |
| `racer_retention_share_rejected_total` | Optional (non-owned) pages not written to disk because their ClusterCache was over its share and free segments were at the reserve |

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
CRC and AEAD validation happen later in fill. They measure disk-to-reader payload,
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
`RACER_ACTIVE_PATH_SEARCHES`.

The overlay is the symmetric union of 32 independently hashed rings over
stable node IDs. It has at most 64
neighbors per member and is independent of shares. Building a new graph costs
O(32 N log N) for bounded-size IDs and retains approximately 64N adjacency
`usize` slots plus N vector headers. On a 64-bit target at 100,000 members,
adjacency slots alone are about 48.8 MiB, in addition to membership records,
construction scratch, and any retained old snapshots. These allocations are
not covered by the path-cache byte limit.

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

## Controller and workload wiring

`racer-config` holds controller settings and operator workload inputs. Listener changes
also require matching Services, probes, network policy, and monitoring configuration.

| Setting | Default | Purpose |
|---------|---------|---------|
| `RACER_CONTROL_ADDRESS` | `:8443` | Controller HTTPS listener |
| `RACER_HANDSHAKE_TIMEOUT` | `5s` | Controller TLS handshake deadline; positive duration |
| `RACER_METRICS_ADDRESS` / `RACER_PROBE_ADDRESS` | `:8080` / `:8081` | Controller metrics / health and readiness listeners |
| `RACER_PEER_PORT` | `8082` | Managed dataplane peer listener and membership publication; 1024..65535 |
| `RACER_DIAGNOSTICS_PORT` | Unset | Managed dataplane defaults to 9090, or 9091 if peer port is 9090; explicit port must be 1024..65535 and distinct |
| `RACER_HOST_NETWORK` | `false` | Explicit Racer host-network opt-in; not implied by RDMA |
| `RACER_DATAPLANE_NVIDIA` | `env` | NVIDIA driver library injection for the dataplane: `off`, `env`, or `cdi`; see [GPU reads](#gpu-reads) |
| `RACER_POD_NETWORK_NODES` | Unset | Not supported by operator-managed installations; the controller only enrolls Pods of DaemonSet `racer-dataplane`, so a nonempty value fails reconciliation |
| `RACER_SNAPSHOT_MAX_AGE` | `30s` | Maximum replicated snapshot age |
| `RACER_CERTIFICATE_LIFETIME` | `24h` | Dataplane leaf lifetime; 2 minutes..24 hours |
| `RACER_ROTATION_INTERVAL` / `RACER_ROTATION_PREPARE_FOR` / `RACER_ROTATION_RETAIN_FOR` | `24h` / `1h` / `48h` | Issuer/key rotation policy; interval must cover preparation and retention must cover leaf lifetime |

Duration settings use Go syntax and positive whole seconds. Quiesce traffic for peer-port
transitions; do not independently override the dataplane listener and published port.

Controller HTTPS admission is per process, including replication traffic. The Go
`Config.Limits` defaults allow `2 * wire.MaxMembers + 128` open connections (room
for independent snapshot and keyring polls), 32 concurrent TLS handshakes, 32
concurrent bearer-authenticated operations, and 128 concurrent response writes.
These concurrency counts are programmatic limits, not environment tuning keys.
A connection retains its slot until its socket closes, including silent TCP peers, TLS peers, idle
keep-alives, and long polls. Handshakes retain a separate slot until success or
failure and use `RACER_HANDSHAKE_TIMEOUT` (default `5s`). Excess connections or
handshakes are closed before HTTP without a queued waiter or overload response. Completed TLS
connections do not retain handshake slots. Request authentication and poll
admission remain independent; shutdown forcibly closes all admitted sockets.
Readiness also requires the cached serving certificate's DNS SANs to match the
configured replication server name. A valid certificate for another service is
not ready; reloading a correctly named certificate restores this readiness gate.

Restrict HTTPS ingress to required clients, dataplanes, and controller replicas;
replication also needs direct controller Pod-to-Pod access. Preserve kubelet probe,
monitoring, DNS, and Kubernetes API access. NetworkPolicy enforcement depends on
the CNI, especially for host-network Pods and external clients. Use host or upstream
network controls where Pod policy cannot enforce the boundary. The deployment
does not impose a policy that would block these installation-specific paths.

Standalone rendering creates an installation marker only with
`InitializationState=fresh` and an explicit new `ClusterID` UUID. Initialization
requires `staged-v1`. Later renders omit the marker: preserve the actual immutable
controller-finalized object and its UID commitments, not a regenerated consumed
manifest. Exclude it from pruning. Never recreate committed authority under the
same UUID.

Go wire codec byte limits (64 KiB for bootstrap, 512 KiB for keyring bundles,
and 64 MiB for publications) bound encoded documents, not total heap usage.
The decoder buffers the bounded document, validates tokens against the schema,
then decodes typed state. It rejects unknown or duplicate fields and wrong shapes
without traversing their contents, and rejects excess members or RDMA NICs before
consuming the next element. Validation does not retain a generic JSON tree.
Buffers, individual tokens, base64 validation, typed collections, and subsequent
semantic validation still allocate memory; the byte cap is not a process memory
budget or a substitute for concurrency limits.

### Operator-managed dataplane

When any `ClusterCache` exists, the operator applies ServiceAccount `racer-dataplane`,
ConfigMap `racer-dataplane-config` (created with defaults only when absent), and
DaemonSet `racer-dataplane`. The DaemonSet uses image `racer-dataplane` from the
operator's registry and tag, connects to `https://racer-controller.<namespace>.svc:8443`,
mounts `racer-bootstrap-trust`, and imports `racer-dataplane-config` with `envFrom`.
A pod-template annotation carries the ConfigMap payload hash, so edits roll out.
Existing resources with these names that the operator did not create are not
adopted; reconciliation fails until they are removed. Deleting the last
`ClusterCache` leaves the controller and dataplane installed.

Workload overrides may target `{component: racer, kind: DaemonSet}` to set, for
example, `nodeSelector`, tolerations, `updateStrategy`, annotations, resources, or
additional container `env` entries. Overrides cannot change or remove the
operator-wired `env` entries, `envFrom`, volumes, or service account of the
`dataplane` container.

The operator owns identity/wiring: cluster UUID, URL, images, service accounts, trust,
replication identity, and durable resource names. These are not tuning keys. Node identity
comes from verified enrollment/local recovery, not a supplied Node name/UID. Do not reset
markers, credentials, or version counters to repair an existing identity.

Before any manager runnable starts, controller startup recovery has a 30-second
total deadline covering authoritative installation reads, initialization writes,
and final version validation. Caller cancellation or an earlier caller deadline
still applies. The competing-installer wait retains its own five-second limit
within that total budget. Recovery failure aborts startup without granting serving
authority; a timeout does not authorize resetting durable state.

The **bare binary** defaults `RACER_PEER_LISTEN` to `0.0.0.0:7443` and
`RACER_DIAGNOSTICS_LISTEN` to `127.0.0.1:9090`. Managed workloads bind both to the Pod IP
with the ports above. The operator also wires trust/token paths and
`/var/lib/racer/identity/private`; standalone defaults do not replace managed wiring.

### Membership and credential reconciliation

Endpoint selection uses the newest eligible Pod controlled by the current managed
DaemonSet, breaking creation-time ties by Pod UID. Failed, Succeeded, terminating,
and IP-less Pods are not eligible. Readiness does not gate membership. Pod phase
changes trigger reconciliation; readiness-only changes do not. A previously admitted
Node retains its last endpoint across discovery gaps, while a never-admitted Node
with no eligible endpoint is omitted.

A custom `RACER_DAEMONSET_NAME` must be a valid DNS subdomain and fit a Kubernetes
label value (at most 63 characters), because it also identifies the workload's
immutable instance selector.

Issued leaves start validity one minute before issuance to tolerate modest clock
skew, matching issuer roots. Expiration remains issuance time plus
`RACER_CERTIFICATE_LIFETIME`; the skew allowance does not extend expiration or grant
server-auth usage. Clocks must still be synchronized. A staged issuer must cover
its actual activation, the full interval until its replacement activates, and the
last issued leaf's lifetime. If that horizon no longer fits, the controller stages
a fresh issuer and waits a complete preparation period instead of activating the
stale issuer.

Topology and keyring reconciliation retry dependency-local timeouts or cancellation
errors while their reconcile context remains live. Cancellation or expiration of
the reconcile context itself is terminal for that operation.

## Diagnostics and metrics

| Dataplane HTTP path | Use |
|---------------------|-----|
| `/healthz` | Process liveness |
| `/readyz` | Serving readiness, including usable admission |
| `/metrics` | Prometheus metrics, including `racer_ready` and `racer_live` |
| `/debug/membership` | Applied membership state |
| `/debug/failures` | Bounded failure diagnostics |
| `/debug/aead`, `/debug/decrypt-failures`, `/debug/send-crc` | Bounded crypto and checksum failure details |
| `/debug/terminal`, `/debug/admission-final`, `/debug/candidate-final` | Bounded final-outcome records |
| `/debug/gate-events`, `/debug/flight-expiry` | Bounded gate and in-flight expiry events |
| `/debug/hardware` | Live JSON report of block devices, RDMA devices, and Racer's claimed disks and RDMA rails |
| `/debug/pprof/profile?seconds=30` | Opt-in native CPU profile |

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
go tool pprof -top ./racer-dataplane profile.pb
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

The diagnostics listener uses port 9090 by default.
Persist scrape configuration on workload templates or supported overrides, not only live Pods.

## Upgrade and recovery

Keep the operator, controller, dataplane, SDK, and origin adapters on matching
versions from the same Unbounded release. Keep cluster identity, version counters,
credentials, and node identity state. Do not delete them to force a fresh start.
Cached bytes are disposable; this installation state is not. Deleting and
recreating a ClusterCache creates a new cache identity, even with the same name.

Expect a cold cache after changing device layout,
worker count, or storage geometry. Recovery does not scan payloads to rebuild
the cache. Make sure origins can serve the resulting misses.

## Go SDK

The Go SDK is `github.com/Azure/unbounded/pkg/racersdk`. Its
[package documentation](https://pkg.go.dev/github.com/Azure/unbounded/pkg/racersdk)
is the complete SDK reference, including configuration, the copy and no-copy read
paths, the origin contract, and error handling. The SDK does not provision caches
or origins.

### Warm an exact version

The method signature is:

```go
func (c *Client) Warm(ctx context.Context, request Request, ttl time.Duration, opts WarmOptions) (WarmResult, error)
```

`WarmOptions{}` selects the current version; `WarmOptions{ETag: tag}` pins a strong
ETag. `ttl` must be positive; the SDK rounds it up to whole seconds and rejects
rounding overflow. `WarmResult` contains `Metadata` and `RetainUntil`. A zero
`RetainUntil` means refusal, not a metadata error. A nonzero value is admission,
not fill completion. `Warm` shares the SDK's bounded Stat lane; `Stat` itself does
not request warming.

With an existing `client`, `ctx`, and `request`:

```go
warmed, err := client.Warm(ctx, request, 10*time.Minute, racersdk.WarmOptions{})
if err != nil {
    return err
}
// A zero RetainUntil means this Get may need a normal cache fill.
object, err := client.Get(ctx, request, racersdk.ReadOptions{ETag: warmed.Metadata.ETag})
if err != nil {
    return err
}
defer object.Close()
```

Read and close `object` as usual. Pinning the ETag, rather than calling an unpinned
`Get`, selects the version admitted by `Warm`. See [warm and hard retention](#warm-and-hard-retention)
for server limits, partial failures, and restart behavior.

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
| Annotation `racer.unbounded-cloud.io/client-containers` | Optional comma-separated app container names. Default: every app container. Init containers are never changed |
| Annotation `racer.unbounded-cloud.io/client-socket-injected` | Set by the webhook to the cache name; do not edit |

Each selected container gets:

- `/run/racer/<cache>/client` mounted read-only at the same path. A read-only
  mount still allows `connect` and `sendmsg` on the socket.
- `RACER_SOCKET=/run/racer/<cache>/client/socket`.

The pod is placed on Linux nodes without the `exclude` label, like sidecar pods.
The hostPath type is `Directory`, so the pod waits until the dataplane has
created the cache directory. Host networking is allowed.

This label works alone or with `object-sidecar`. When both name the same cache,
the pod gets one shared volume. The webhook rejects the pod when the cache name
is missing or invalid, when a listed container does not exist, or when a
container already sets `RACER_SOCKET` or mounts that path.

## GPU reads

GPU reads let a client on the node read an object straight into its own NVIDIA
GPU memory. They need:

- A dataplane image built with `--build-arg RACER_NATIVE_CUDA=true`.
- An NVIDIA node set up by the unbounded agent, which installs the NVIDIA
  container runtime and a CDI spec.
- `RACER_ENABLE_GPU` in `racer-dataplane-config`. `auto` turns GPU mode off
  when no usable GPU is found, or when the image was built without CUDA. `true`
  makes startup fail instead. `false` turns it off.

At startup the dataplane logs each GPU it finds: ordinal, UUID, PCI address,
NUMA node, compute mode, and whether it can use it. It skips GPUs in
`EXCLUSIVE_PROCESS` or `PROHIBITED` mode. One helper thread does all GPU work
for the process. At most 256 GPU reads run at once; more get 503.

Where the bytes come from decides who decrypts them:

- A page that is already plaintext in memory is copied to the GPU.
- A page fetched from the origin is encrypted on the CPU, so its plaintext is
  copied to the GPU too.
- Other pages reach the GPU still encrypted, and the GPU checks and decrypts
  them. This covers pages kept encrypted in memory, pages on local disk, and
  pages from peers. The CPU does not decrypt them.
- A page on local disk is read straight from the disk into GPU memory with
  cuFile when it can be. Otherwise the CPU reads it and copies it to the GPU.

If the GPU finds a damaged page, the read fails and the dataplane drops that
copy from memory or disk, as it does when the CPU finds one.

`RACER_GPU_STAGING_BYTES` is the GPU memory the dataplane keeps on each GPU for
encrypted pages. It is split into slots of one page (16 MiB plus 4 KiB). Two
slots are needed for decryption, so a smaller cap means the CPU decrypts. The
other slots hold pages read with cuFile. When all of them are in use, the
dataplane reads the next page through host memory instead. If the image has no
GPU decrypt support, the CPU decrypts every page.

The dataplane does not request `nvidia.com/gpu`. It is privileged, so it
already sees `/dev/nvidia*`. It only needs the host driver libraries.
`RACER_DATAPLANE_NVIDIA` in `racer-config` controls how it asks for them:

| `RACER_DATAPLANE_NVIDIA` | Behavior |
|--------------------------|----------|
| `env` (default) | Sets `NVIDIA_VISIBLE_DEVICES=all` and `NVIDIA_DRIVER_CAPABILITIES=compute,utility` on the dataplane container. The NVIDIA runtime injects the libraries. Other runtimes ignore these variables, so nodes without NVIDIA are not affected |
| `cdi` | Also adds the Pod annotation `cdi.k8s.io/racer-dataplane: nvidia.com/gpu=all`. containerd fails to create the container on any node that cannot resolve this CDI device, so use it only when every dataplane node has an NVIDIA CDI spec |
| `off` | Adds nothing |

On nodes set up by the unbounded agent, the NVIDIA runtime is the default
containerd runtime and runs in CDI mode, so `env` is enough there. These env
entries are operator wiring: workload overrides cannot change them.

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
The Node annotation `racer.unbounded-cloud.io/rdma-nics` accepts at most 64 entries:

```json
[{"device":"mlx5_0","port":1,"rail":0}]
```

`port` is 1..255; `rail` is 0..65535. Optional `gid` is 32 lowercase hex digits;
optional `numa_node` is `uint32`. Device/port pairs must be unique; NICs may share a rail.

Absent administrator policy uses active MW2B-capable ports in PCI BDF/port order, reported
during authenticated enrollment/renewal and published from `enrolled-rdma-nics`. Explicit
`[]` disables eligibility; a list overrides automatic rails. Discovery cannot infer cabling
or correct asymmetric initial inventories: use explicit mappings when ordinals do not match.

The private identity directory's `rdma-rails.json` preserves physical-port reservations
across renewal/restart: missing ports retain rails; new ports append. Do not delete it
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

Coordinate controller/dataplane upgrades and quiesce traffic; rolling updates
do not make mixed versions safe.
Validate transfers, HTTP fallback, and recovery on target hardware before relying on RDMA.
