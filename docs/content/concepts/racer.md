---
title: "Racer Distributed Cache"
weight: 4
description: "Use Racer to turn node-local disks and peer bandwidth into a read-through cache for large objects, with no separate cache tier to operate."
---

Racer is the Unbounded distributed cache for large, read-mostly objects. It runs
on every worker node as part of one dataplane DaemonSet, keeps immutable object
pages on local disks and in memory, and serves reads to local applications over
a Unix domain socket. When a node does not have a page, it asks the nodes that
own that page instead of going back to the source.

This page explains the problem Racer solves, the cases where it is a better fit
than a dedicated cache tier, the capacity and membership decisions you need to
make, and the internal model you need in order to operate it. The API details
live in the [Racer reference]({{< relref "reference/racer" >}}), and the
application-facing adapters live in the
[racer-object reference]({{< relref "reference/racer-object" >}}).

## Why Racer?

Serving the same large files to many nodes is expensive in a way that is easy
to underestimate. A model, dataset, container layer, or training checkpoint may
be hundreds of megabytes or several gigabytes. When a job scales from a handful
of replicas to hundreds, each replica pulls the same bytes again. The source
sees a fan-out proportional to the replica count, egress grows with every new
node, and the source's request rate or throughput limit becomes the ceiling on
how fast the job can start.

Putting a cache in front of the source helps, but a conventional cache tier
moves the problem rather than removing it. You still need machines for the cache
servers, a network path from every worker to that tier, and enough cache
bandwidth for the peak rollout. The cache tier has its own capacity limit, its
own failure modes, and its own operational lifecycle. The workers already have
local disks and network interfaces; leaving them idle while building a separate
cache fleet is often the wrong trade.

Racer takes the other approach. Every node runs the same dataplane and can store
pages for the objects it reads. A local cache hit is served from memory or local
disk over a Unix socket, so it does not cross the network at all. A cache miss is
served by a small set of peer nodes that own the page, and only those owners
contact the origin. The result is a cache whose aggregate capacity and serving
bandwidth grow as you add worker nodes, without a separate cache Deployment or
a controller Service.

Racer is a read-through cache, not a storage system. The origin remains the
source of truth. Objects are immutable once a version has been identified by an
ETag, and Racer never writes to the origin. A cache may be lost at any time and
refilled from the source. That is a deliberate property: it keeps the data path
simple, avoids a second durability problem, and lets the cache be sized for
performance instead of retention.

### What Racer Gives You

Racer does not require cache servers, a cache controller Deployment, or a
PersistentVolume per application. The Unbounded operator installs one Racer
DaemonSet, and each Pod runs both the node dataplane and the control role it is
elected to hold. There is no separate cache cluster to upgrade, monitor, or pay
for.

Capacity and serving bandwidth grow with the worker fleet. Every node in a
cache's pool contributes disk for pages and a network endpoint for peer reads.
Adding a matching node adds both storage and a new source of cached bytes, which
is the opposite of a fixed cache tier that becomes the bottleneck as the
workload grows.

Pages are 16 MiB. That size is large enough to make disk I/O and peer transfers
efficient, and small enough to spread a large object across many owners. The
page is the unit of placement, transfer, disk storage, and origin range reads. A
single read can stream many pages, and the final page of an object may be
shorter.

Placement is distributed across page owners. Each page has up to three owners,
chosen by weighted rendezvous hashing over the cache's node pool. Owners are the
nodes that fetch the page from the origin and the nodes that other readers ask
first. This bounds origin load: a hundred readers of the same page do not create
a hundred origin requests under normal conditions.

Any origin-backed data source can be used. Azure Blob Storage is built in, and
the Go SDK lets you register your own origin for an object store, registry, or
internal service. The `racer-object` sidecar turns a cache into a read-only S3
or Azure Blob endpoint for applications that cannot use the SDK directly.

Per-cache encryption and integrity verification are part of the data path. Pages
are sealed with XAES-256-GCM under a cache-specific key, and every page is
verified before it is served or stored. The cache key is not shared between
caches, so data from one ClusterCache cannot be decrypted as another.

Caches and prefetch share node capacity. Prefetch does not have a private
storage pool. It submits tickets to the normal read path, and the pages it loads
compete for the same memory and disk as pages loaded by foreground readers. A
prefetch that is no longer useful can be cancelled, and its pages are evicted
like any other page.

A single read is pinned to one version. Racer resolves the object's current
version once, then requests every page from that version. Separate reads of the
same key may see different versions if the origin publishes a new ETag between
them. When several reads must see identical bytes, take the ETag from `Stat` and
pin the later reads to it.

RDMA and NVIDIA GPU paths are available where the hardware and configuration
support them. Peer transfers can use RDMA instead of TCP, and GPU reads can move
and decrypt pages in GPU memory rather than staging them through host memory.
Both paths fall back to the portable path when the hardware or driver is not
available.

{{< callout type="note" >}}
Racer reduces duplicate origin reads, but it does not guarantee exactly one
origin read per page cluster-wide. Concurrent readers normally coalesce at an
owner, but retries, races between owners, and placement changes can still
produce more than one request for the same bytes. Size origin rate limits for
best-effort deduplication, not for exactly-once delivery.
{{< /callout >}}

## When to Choose Racer

Racer is a good fit when a workload repeatedly reads the same large objects
across many nodes and the source is expensive to scale. Large model loads,
dataset preprocessing, checkpoint restores, container image distribution, and
batch jobs that share inputs all have this shape. The more replicas and the
larger the objects, the more the peer path saves.

GPU and storage-dense nodes are a particularly good fit. Those nodes already
have local NVMe capacity and high-bandwidth network interfaces, so they have
both the disk and the fabric to serve pages to their neighbors. A cache tier
would duplicate hardware that is already present.

Hybrid, multi-cloud, and edge clusters also benefit. A cache tier may be far
from some sites, or may not be allowed to cross a network boundary. Racer keeps
the cache close to the reader and uses the cluster's existing connectivity, so a
site can serve its own workloads even when the origin is remote.

Racer also fits internal or custom data sources. If you can express metadata
and a page-aligned range read, you can implement an origin with the SDK. You do
not have to wait for a built-in adapter for your storage system.

Workloads behind a rate-limited, throttled, or costly source are strong
candidates. Owner-mediated reads reduce the number of source connections and
requests, and `spec.origin.maxConcurrentRequests` provides a per-node ceiling
for the built-in Azure origin. The cache does not make a slow source fast on the
first read, but it prevents every new replica from adding another full copy of
the load.

When data must be encrypted at rest and in transit, Racer can be a better fit
than a home-grown shared directory. Page encryption, authenticated peer
requests, and per-cache keys are built into the data path rather than added by
each application.

A cluster that serves images, models, and datasets at the same time can use one
Racer installation with multiple ClusterCaches. Each cache has its own name,
identity, keys, origin, and optional node pool. The caches share node capacity
according to their priorities, so a large model load does not permanently push
out the image cache.

Fast model loads on GPU nodes are a specific case of the same pattern. A model
file is immutable after publication, is read in full by every replica, and
benefits from being cached near the GPUs that need it. Racer can stream pages to
a GPU-aware reader while other nodes fill their own copies from the owners.

Racer is not the right tool for every access pattern. It is a read-through cache
for immutable versions, not a writable filesystem, a database, or a durable
object store. Workloads dominated by tiny random reads may see less benefit from
a 16 MiB page granularity than workloads that read large objects sequentially.
A cache tier with a different granularity or a local application cache may be a
better fit there.

### Racer Compared With a Dedicated Cache Tier

A dedicated cache tier is a separate set of cache servers, usually behind a
Service or a load balancer, with its own capacity planning and failure domain.
Racer uses the worker fleet itself. The two designs solve the same fan-out
problem with different costs, and the right choice depends on which cost you
want to carry.

| Consideration | Racer | Dedicated cache tier |
| --- | --- | --- |
| Extra machines | None beyond the worker fleet | A separate cache fleet to size and operate |
| Capacity growth | Grows with every worker node | Grows only when you add cache servers |
| Serving bandwidth | Comes from peer nodes and local disks | Comes from the cache tier's network and disks |
| Cache work placement | On the nodes that run the workload, with optional per-cache node pools | Centralized, usually one hop away from every worker |
| New data source support | Implement an origin with the SDK | Add the source to the cache server software or a proxy |
| Durability | None. The cache is disposable and refills from the origin | Depends on the cache product; can be made durable |
| Cross-cluster sharing | Not provided. Each cluster has its own caches | Possible if the tier is shared |
| Isolation | Shared node capacity, separated by cache priority and node pools | Dedicated capacity per tenant or workload is easier |

Choose a dedicated cache tier when you need capacity that is independent of the
worker fleet, a durable shared cache across clusters, hard tenant isolation, or
a cache product with features Racer does not provide. Choose Racer when you want
to avoid extra machines, keep the cache close to the workload, and let capacity
grow with the cluster.

### Planning Considerations

Racer consumes resources on every node that runs it, and the defaults are
designed for a dataplane host rather than a small sidecar. Plan disk, memory,
CPU, and network together. A node that is short on one of them can make the
whole cache slower even when the others have headroom.

#### Node Resources

Disk is the largest commitment. Each I/O worker has a file-backed slab, and the
default `RACER_SLAB_BYTES_PER_WORKER` is 16 GiB. The node's total slab capacity
is the per-worker value multiplied by the number of I/O workers, so a node with
many workers can reserve a large amount of disk. Slabs live under
`/var/lib/racer/slabs`, and each slab is divided into segments whose default size
is 1 GiB. The slab is a cache, not a volume: it can be deleted, and Racer will
start cold.

Memory is governed by explicit budgets rather than a single cache size. The
defaults are 128 MiB of plaintext, 128 MiB of ciphertext, and 160 MiB of dirty
pages per I/O worker, with additional budgets for request contexts, flights, and
connection state. RDMA reserves a separate registered buffer pool, and GPU reads
reserve staging and landing memory per GPU. All of these are configurable in the
`racer-settings` ConfigMap and are validated by the operator before the
DaemonSet rolls.

CPU is used by I/O workers, crypto helpers, and the managed control threads. The
dataplane sizes itself automatically by default, using one eligible CPU per
physical core unless `RACER_ALLOW_SMT` is set to `true`. You can set exact
worker and helper counts when you need predictable placement, but the sum plus
the RDMA owner thread must fit `RACER_MAX_THREADS`. Managed deployments need at
least four threads, or three when RDMA is disabled.

Network is used in two directions. Local clients connect over a Unix socket, so
a local cache hit does not consume pod network bandwidth. Peer reads do use the
pod network, and an object read by many nodes can create a substantial amount of
east-west traffic. RDMA can move that traffic more efficiently when the nodes
share an eligible fabric, but the TCP path remains the default and the fallback.

#### Dataplane Sizing

Automatic sizing is the right starting point for most clusters. It chooses a
worker count from the node's CPUs and spreads workers over the NUMA nodes of the
RDMA NICs when RDMA is in use. If you set `RACER_IO_WORKERS`, you must also set
`RACER_CRYPTO_HELPERS`, and the two counts must fit the thread cap. The helper
count may not exceed the worker count.

Sizing is a per-node decision, but it affects the whole cache. A node with more
workers contributes more slab capacity and more concurrent page transfers. A
node with too few workers can become a hot spot when it owns pages that many
readers want. The placement hash spreads ownership by node weight, not by
worker count, so keep node shapes reasonably similar within a cache's pool.

#### Capacity Budgets and Devices

The `RACER_*_BYTES` budgets can be set either as a node-wide total or as a
per-worker amount. The per-worker form is the default and is multiplied by the
final worker count. The node-wide form is divided among workers. Setting both
forms of the same budget is an error. This matters when you copy a budget from
one node shape to another: a value that fits a four-worker node may reserve four
times as much on a sixteen-worker node.

Budgets also have internal relationships. Each worker's plaintext share must fit
one page more than `RACER_RANGE_WINDOW_PAGES`, and its ciphertext share must fit
the range window plus one stored record. The range window defaults to four
pages, and the SDK's `PageCredits` setting is capped by the smaller of the
client's request and the dataplane's `RACER_RANGE_WINDOW_PAGES`. Raising the
window increases read-ahead and memory use together.

Raw block devices can replace or supplement slab files. A node advertises a
device selector through the `racer.unbounded-cloud.io/block-devices` Node
annotation, and the dataplane scans `RACER_DEVICE_DIRECTORY` (by default
`/host/dev`) for matching `disk/by-id` entries. Devices are opened exclusively,
must be unpartitioned, and must have an all-zero guard area. Checkpoints still
live in the slab directory. Use raw devices only when you can dedicate them to
Racer and accept that their contents are disposable.

#### Membership and Pools

Every Racer node is a member of the cache mesh by default. A Node label of
`racer.unbounded-cloud.io/exclude` removes that node from dataplane placement
and from membership. Excluding a node changes page ownership, so the pages it
held are no longer reachable as an owner; readers refill them from the origin or
from another owner.

A ClusterCache can restrict itself to a subset of nodes with
`spec.nodeSelector`. The selector is evaluated against Node labels, and the
cache gets a pool containing only the matching members. A cache with an empty
pool has no owners, and reads fail as unavailable until a matching node joins.
Changing the selector moves the cache to a new pool with no migration: pages are
refilled from the origin.

The admission webhook uses the same selector to keep client pods on nodes that
can serve their cache. If a pod names a cache with a selector, the webhook adds
the selector to the pod's required node affinity. A pod that bypasses the
scheduler by setting `spec.nodeName` is still checked against the selected
Node, and an unknown cache or a non-matching node is denied. This prevents a pod
from landing on a node that has no socket for its cache.

#### Shares and Priorities

There are two different weighting controls, and mixing them up leads to
surprising placement or eviction behavior.

Node shares decide who stores pages. A node reports its shares in its own Pod
annotation, and an administrator can override that value with the
`racer.unbounded-cloud.io/shares` Node annotation. The default is four. Weighted
rendezvous hashing gives a node with more shares a proportionally larger chance
to own a page, so shares control placement and peer load, not cache capacity.

Cache priority decides how much capacity each cache gets on a node. It is
`spec.priority`, an integer from 0 to 1000, defaulting to 500. Disk shares are
weighted across published caches plus any unpublished cache that still holds
data, and memory shares are weighted across caches actively using memory. A
cache's fair share of capacity `C` is `C * priority / sum(priority)`. A priority
of 0 gets no weighted share while positive-priority caches compete, but it keeps
a bounded working allowance. If every cache has priority 0, they share equally.
Priority can be changed at any time.

#### Failure Behavior

Racer does not fall back to a direct origin read when its own data path fails. It
tries the ranked owners and any predecessor copies, then returns a retryable
error if no permitted source can serve the page. The SDK exposes that as
`ErrUnavailable`. A caller that wants resilience should retry with backoff; the
cache does the deduplication and placement work, but it does not hide a cache
outage by bypassing itself.

The same rule applies to Gantry's Racer backend. Gantry deliberately does not
fall back to its peer-to-peer agent on a Racer failure. Its mirror startup gate
holds requests until the Racer sockets accept connections, and a cold or failed
read returns `503` so the client retries. That keeps one content path and avoids
silently switching to a different cache with different credentials and
semantics.

A cache restart is a cold-cache event, not a data-loss event. Pages that were
not checkpointed are gone, and the next reads refill them from the origin. A
deleted ClusterCache loses its catalog entry and its data becomes unusable. A
new cache created with the same name has a new Kubernetes UID, a new wire
identity, and an empty cache.

#### The Trust Boundary

Racer does not authenticate individual client requests. Access is controlled by
the Unix socket directory: a pod that can mount
`/run/racer/<cache>/client` can read any object in that cache. Every pod that
mounts the socket is inside the cache's trust boundary. Every enrolled dataplane
node is inside it as well, because the keyring is delivered to every dataplane
node, not only to a cache's owners.

This is a deliberate sharing model, not per-tenant isolation. All pods attached
to a cache see the container's whole key space, and the origin request's
metadata and authorization are supplied by the caller rather than checked by
Racer. Use separate ClusterCaches and separate namespaces when workloads must
not share cache contents. Do not expose a cache socket through a host path, a
Service, or a network proxy.

## Architecture

Racer has two kinds of work, and both run in the same binary. The dataplane
serves the cache's client and origin sockets, stores pages, and talks to peers.
The control role maintains membership, the cache catalog, encryption keys, and
trust. Every Pod runs the same image; a Pod that wins a control seat does the
control work in addition to its dataplane work.

That design is why Racer has no separate controller Deployment. The operator
installs static objects and the dataplane elects its own control seats. A short
control outage does not stop an already published cache from serving, and a
control restart does not require a separate workload to be rescheduled. Serving
authority has a bounded lifetime and is withdrawn if lease ownership or
committed-state freshness cannot be confirmed for too long.

### The Dataplane DaemonSet

The operator installs a DaemonSet named `racer` in the operator namespace. Each
Pod runs the `dataplane` container, mounts the node's slab directory and device
root, and joins the peer network. The Pod also carries the identity, the
projected control token, and the settings from the `racer-settings` ConfigMap.

The dataplane opens a peer listener, by default on `0.0.0.0:7443`, and publishes
the address and port that other members should use. It also opens a diagnostics
listener on port 9090 by default; the managed DaemonSet binds it to the Pod IP
rather than to loopback. RDMA, when enabled, has its own connection manager
port, by default `18515`, and its own set of eligible links.

Each cache gets two Unix sockets under `/run/racer/<cache>/`:

| Socket | Path | Used by |
| --- | --- | --- |
| Client | `/run/racer/<cache>/client/socket` | Applications and the `racer-object` sidecar reading objects |
| Origin | `/run/racer/<cache>/origin/socket` | The process that supplies cache misses for that cache |

The client socket is not a network service. A pod reaches it by mounting the
cache's client directory into the pod, and the socket's directory permissions are
the access control. The origin socket is local to the node; an owner calls its
own origin rather than a remote origin Service.

### Control Seats and the Primary

Pods claim control seats through Leases named `racer-seat-0` through
`racer-seat-15`. A node holds at most one seat, and one seat also holds the
`racer-primary` Lease. The seat count is selected by `RACER_CONTROL_SEATS`,
which accepts an automatic value or a fixed count from 1 to 16.

The primary observes Nodes, managed dataplane Pods, ClusterCaches, and the seats.
It computes membership and the cache catalog, reconciles encryption keys and
trust roots, and commits state before publishing it. The catalog carries each
cache's socket paths, priority, node pool, and the cluster's page size. Other
seats serve the same committed state. Clients discover seats through the
Kubernetes API, spread connections across them, and try another seat when a
connection fails.

Durable control state lives in the `racer-state` Secret, whose UID is the
cluster ID. The Secret holds publication counters and hashes, keyring
generations, rotation schedules, retired trust roots, and the primary's fencing
term. A large publication is not stored in the Secret. The primary commits its
term with a resource version compare-and-swap, and every later commit checks
that term, so an old writer cannot overwrite a successor's state.

Seat election works before enrollment. Requiring an enrolled node to win the
first seat would deadlock startup, so a node may claim a seat first and is not
advertised as ready until it can serve authenticated, committed state.

### Enrollment and Admission

A dataplane Pod authenticates to the Kubernetes API with a projected token and
the API server CA. Seat Leases publish the control endpoint and the seat's
serving certificate. A client pins that certificate, verifies TLS, and only then
sends an enrollment token. Lease changes invalidate pooled connections to the
previous holder.

Enrollment uses a separate projected token with the `racer-control` audience.
The seat performs a TokenReview and then checks the live ServiceAccount, Pod,
DaemonSet owner, and Node binding. A node name supplied by the caller is not
proof of identity. The node's private key stays on the node, and the issued leaf
certificate is short-lived, currently 24 hours.

Each seat has its own issuer key. The primary commits the seat's public root to
the keyring before that seat issues certificates. Retired roots remain trusted
for the lifetime of the leaves they could have issued. Every process start
offers a new root, so restarts can fill the keyring; when it is full, the
primary evicts the root that no seat offers and that expires first.

The control seats also serve the object sidecar admission webhook. The operator
installs the `racer-object-sidecar-injector` MutatingWebhookConfiguration and a
selectorless `racer-webhook` Service. The primary maintains the CA bundle and
the EndpointSlice that route admission to ready seats. A pod labeled
`racer.unbounded-cloud.io/object-sidecar=enabled` gets the `racer-object`
sidecar, and a pod labeled `racer.unbounded-cloud.io/client-socket=enabled` gets
a read-only mount of a cache's client socket. Both opt-ins are independent.
The webhook fails closed, so a pod is not admitted without the node affinity its
cache requires.

### Replication and Transport

Peers exchange signed HTTP requests over TCP, and over RDMA when both ends are
eligible and the edge is selected for RDMA. A request head is signed with the
sender's node certificate, and a relay preserves the original signature chain.
Page bodies are not signed as separate messages; they are encrypted and
authenticated with the cache's page key, so a body cannot be forged or silently
altered even though it is not part of the signed head.

The mesh keeps the number of hops to an owner small. In the current mesh, a
reader normally reaches an owner in at most two links, with a bounded fallback
path after a timeout. Per-neighbor connections are limited per worker, by
default eight busy connections, so one hot neighbor cannot consume every
outbound slot. A relay never caches a page; it forwards bytes, so every extra
hop costs another copy of the page on the network.

RDMA is optional. `RACER_ENABLE_RDMA=auto` uses native RDMA when the binary
supports it and ports exist at startup. `true` reserves RDMA resources even
without ports, for hotplug, and `false` turns it off. A per-peer and per-rail
circuit breaker can move traffic back to HTTP after a hard failure or repeated
page timeouts.

GPU reads are a separate fast path. With `RACER_ENABLE_GPU=auto`, Racer can read
local NVMe into GPU memory with cuFile, decrypt pages on the GPU, and receive
RDMA landing buffers directly into GPU memory. It falls back to host staging
when the driver or the device pair does not support the fast path. The dataplane
does not request a `nvidia.com/gpu` resource from Kubernetes; it uses the host's
NVIDIA devices through the container runtime.

### The SDK and Adapters

Applications read through the Go SDK. `racersdk.NewClient` connects to the
cache's client socket, `Get` starts a read at the current version or at a pinned
ETag, `Stat` returns metadata without reading content, and `Object.WriteTo`
forwards a body without copying it through user space when the destination
supports `ReaderFrom`.

Origins implement `racersdk.Origin` and register it with `racersdk.ServeOrigin`.
The origin socket is owned by that process. For a cache with a custom origin,
run the origin process on every node that can own pages, which in practice means
every dataplane node in the cache's pool. The SDK validates every response, so a
short, long, or mismatched body is reported to Racer instead of being cached.

The `racer-object` sidecar is a prebuilt adapter for applications that speak S3
or the Azure Blob API. It reads through the cache's client socket and exposes a
read-only endpoint on loopback, by default `127.0.0.1:8080`. It holds no
upstream credentials and serves trusted pod-local clients without
authentication. It is a protocol adapter, not a second cache.

## Core Concepts

### API

A cache is a `ClusterCache` custom resource in the
`racer.unbounded-cloud.io/v1alpha1` group. It is cluster-scoped, is available as
`clustercaches`, and has the short name `ccache`. It has no status field. Its
Kubernetes UID is its wire identity, so deleting and recreating a cache with the
same name creates a different cache with an empty key space.

A `ClusterCache` name must be a DNS subdomain of at most 82 characters, with each
label at most 63 characters. The limit is not arbitrary: the name is part of the
canonical socket path `/run/racer/<name>/origin/socket`, and Linux limits a Unix
socket address to 107 bytes.

| Field | Type | Rules and effect |
| --- | --- | --- |
| `spec.origin` | object | Optional. Configures the built-in Azure Blob origin. It can be set only at creation and cannot be removed; create a new cache to change the source. |
| `spec.origin.metadataTTL` | duration | Optional. How long Azure object metadata is reused before revalidation. Defaults to 30 seconds and must not be negative. |
| `spec.origin.requestTimeout` | duration | Optional. Bound on each upstream request. Defaults to 1 minute and must be positive. |
| `spec.origin.maxConcurrentRequests` | integer | Optional. Concurrent upstream requests per cache on each dataplane node. Defaults to 512, range 1 to 65536. |
| `spec.origin.azure.containerURL` | string | Immutable container URL. The last path segment is the container name. A credential requires HTTPS. |
| `spec.origin.azure.credential.default` | object | Uses the node's managed identity through IMDS. Optional `clientID` selects a user-assigned identity. |
| `spec.origin.azure.credential.workloadIdentity` | object | Uses Microsoft Entra Workload ID with a projected token. Requires `clientID`, and an optional `tenantID` is discovered from the storage account challenge. |
| `spec.priority` | integer | Optional cache capacity weight, 0 to 1000, default 500. Mutable. |
| `spec.passthrough` | object | Optional. Turns off caching and proxies reads to the origin. |
| `spec.passthrough.route` | string | `Peers` (default) or `Local`. Selects which node's origin serves a passthrough read. |
| `spec.nodeSelector` | label selector | Optional. Restricts the cache to matching Nodes and constrains client pods to them. Mutable. |

A minimal cache with the built-in Azure origin looks like this:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
spec:
  priority: 900
  origin:
    azure:
      containerURL: https://<account>.blob.core.windows.net/models
      credential:
        workloadIdentity:
          clientID: <client-id>
  nodeSelector:
    matchLabels:
      unbounded-cloud.io/site: gpu-a
```

Apply it with `kubectl apply -f cache.yaml` and inspect it with
`kubectl get ccache models -o yaml`. The dataplane creates the socket
directories, and the control plane publishes the cache to the dataplane nodes
that join its pool. A cache is not a PersistentVolume or a
PersistentVolumeClaim; it does not appear as storage in the pod spec and it
cannot be mounted as a filesystem.

#### Passthrough

Passthrough is useful when you want Racer's owner routing and origin abstraction
without retaining data. With `spec.passthrough` set, pages are not written to
disk and are held in memory only while concurrent readers share an in-flight
origin read. Object metadata is not reused across reads, so each read
revalidates with the origin, though one read still pins one version for all of
its pages. Prefetch requests are rejected with `409`.

The route controls which origin is called. `Peers` routes each page to its owner
nodes through the mesh, so origin reads are shared cluster-wide and pages are
pipelined. `Local` makes the reading node call its own origin directly and never
asks peers; every reading node then needs an origin. Switching a cache to
passthrough drops data previously cached for it. Priority still weights the
cache's in-flight memory use.

### Objects, Keys, and Versions

A Racer key is exactly 32 bytes, written as 64 lowercase hexadecimal characters.
The application chooses the key. It is often a content digest, such as the
SHA-256 of a blob, but Racer does not require that and does not interpret the
key. The key identifies an object whose content may change over time.

A version is identified by a strong ETag, a quoted ASCII value such as
`"v1"`. The bytes of a version must never change once the ETag is published.
Racer uses the ETag to ensure that every page of a read comes from the same
version. If an origin reuses an ETag for different bytes, Racer can mix pages
from two contents, so origins must treat the ETag as immutable version identity,
not as a cache-busting label.

`Metadata` carries the object size, the ETag, an optional content type, and a
required `ExpiresAt`. `ExpiresAt` is when Racer must stop treating that version
as fresh. An expired version is not returned as the current version, even if its
pages are still resident. An explicit ETag pin is different: it names an
immutable version rather than asking for the current one, so it can still read
that version while its metadata or pages remain available. `ExpiresAt` is
separate from `spec.origin.metadataTTL`: the latter controls how long the
built-in Azure origin reuses metadata before it asks Blob Storage again.

A client `Get` reads one version from start to end. Two separate unpinned reads
of the same key may see different versions if the origin publishes a new ETag
between them. To read the same version more than once, call `Stat` or read
`Object.Metadata`, then pass the ETag in `ReadOptions.ETag`. If that version is
no longer available, the read fails with `ErrVersionMismatch` rather than
silently returning the current version.

Internally, an object identity is the cache UID plus the key, a version adds the
ETag, and a page adds a zero-based page number. Placement uses the cache, key,
and page number without the ETag, so a page tends to keep the same owners across
versions. Page identity, in-flight request identity, and disk records include
the ETag, so pages from two versions are never confused.

The cache is keyed by the ClusterCache UID, not by its source. That is why
`spec.origin` is immutable at creation. Removing and re-adding an origin, or
switching between the built-in Azure origin and a self-deployed origin, would
otherwise serve entries fetched from a different source under the same identity.
A different source needs a new ClusterCache with a new UID and an empty cache.

### Pages

The page size is 16 MiB. `RACER_PAGE_BYTES` accepts only `16777216`, and the size
is fixed at cluster creation: the primary that makes the first publication
records it in `racer-state`, later primaries publish the recorded value, and a
running node rejects a publication that changes it. The client SDKs and the disk
index do not yet support another size.

The page is the unit of caching and transfer. Placement chooses owners per page,
a peer transfer carries at most one page, a disk record holds one page, and an
origin read asks for a page-aligned range. The final page of an object may be
shorter; the page descriptor carries its plaintext length. A read that spans
many pages streams them through a bounded window rather than materializing the
whole object.

Objects no larger than one page can use the SDK's `SmallObject` option. Small
reads have a separate admission lane, so they are not stuck behind large
transfers. A `SmallObject` read of an object larger than `PageSize` fails with
`ErrInvalidRequest`.

### Origins

An origin supplies metadata for an object and the bytes for one page at a time.
The SDK interface has two shapes. A metadata request sets `Head` and returns
`Metadata` with a nil body. A content request returns the metadata of the
version being served and a body that yields exactly the bytes in `[Offset,
min(Offset+Length, Size))`. When that range is empty, the origin returns a nil
body. If the request has an ETag, the origin must serve only that version and
return `ErrVersionMismatch` if it is gone.

An origin request always has an offset that is a multiple of `PageSize`, and a
length of at most `PageSize`. The range may extend past the end of the object;
the SDK validates the response against the metadata and reports a short, long,
or mismatched body instead of caching it. This lets an origin use a simple
range read without special-casing the final page.

The origin receives the request's opaque `Metadata` and `Authorization` values
unchanged. Racer does not interpret them, and they may be empty. Each is limited
to 8 KiB and rejects leading or trailing ASCII space, bytes below `0x20`, and
`0x7f`. Bytes above `0x7f` are allowed and the values need not be valid UTF-8.
Formatting a request redacts both values, and a cache hit never calls the origin,
so credentials are not reused or revalidated on a hit.

The built-in Azure origin reads one Blob Storage container directly from the
dataplane. Azure is the only built-in provider today; every other source uses
the SDK. There is no separate origin process and no per-cache origin
DaemonSet. The container URL is immutable, and every pod attached to the cache
sees the container's whole key space. Authentication can use the node's managed
identity through IMDS, or Microsoft Entra Workload ID with a projected token for
the Racer dataplane ServiceAccount. Anonymous access is used when no credential
is configured.

Older operator versions ran one `racer-object` origin DaemonSet per cache. Those
leftovers are pruned once every dataplane Pod runs a template that reads origins
in process. If you are upgrading, keep the source objects available until the
caches refill.

Custom origins are the extension point for everything else. An origin process
listens on `/run/racer/<cache>/origin/socket`, implements the callback, and is
deployed on every node that can own pages. Gantry uses this path for registry
content: it creates a `ClusterCache` named `gantry` with no `spec.origin`, runs
the Gantry agent as the origin on each dataplane node, and serves registry pulls
through Racer. The [Gantry guide]({{< relref "guides/gantry" >}}) describes that
backend and its `RacerOriginConflict` rule.

### Owners and Shares

Each page has up to three owners. Owners are chosen by weighted rendezvous
hashing over the members of the cache's node pool, using the cache, key, and
page number without the ETag. A pool with fewer than three members has fewer
owners. The first owner in rank order is the one a reader asks first; if it
cannot serve the page, the reader tries the next rank.

Ownership is stable across versions and changes incrementally as members join,
leave, or change weight. A member that was pushed out of the owner set by later
arrivals may still hold a copy it fetched while it was an owner. Readers can ask
such a predecessor copy-only before they go to the origin, which avoids a source
read when a recently demoted owner still has the page.

For a cached cache, only a page's owner calls the origin for that page. A
non-owner asks an owner over the mesh. This is what bounds source load: if the
owner has the page, the origin is not contacted at all, and concurrent readers
of the same page coalesce on the owner's in-flight fill. If the owner does not
have the page, it fetches it, seals it with the cache key, and streams it to the
reader while other readers join the same fill.

Node shares control ownership weight. A node reports its shares in its Pod's
`racer.unbounded-cloud.io/report` annotation, and an administrator can override
it with the `racer.unbounded-cloud.io/shares` Node annotation. The dataplane
setting `RACER_SHARES` supplies the default, which is four. A node with more
shares is more likely to own any given page, so increasing its shares shifts
both storage and serving load toward it. A Node label of
`racer.unbounded-cloud.io/exclude` removes the node from membership and from
dataplane placement entirely.

A cache's `spec.nodeSelector` defines its pool. Members that do not match the
selector do not store, serve, or read that cache. A cache with no matching
members has no owners, and its reads fail as unavailable. This is the correct
behavior for a cache that was deliberately restricted to a site that is down;
it is not a signal to fall back to a direct source read.

## How a Read Works

A read is a cooperation between the client, the reading node, the page owners,
and the origin. The important mental model is that the client talks only to its
local dataplane, and the local dataplane talks to peers only when it does not
have the page. The origin is contacted by an owner, not by the reader.

1. The application calls `Client.Get` with a key, optional byte range, optional
   ETag, and the opaque metadata and authorization values for the origin. The
   SDK connects to `/run/racer/<cache>/client/socket` and sends the request. A
   `Get` returns once Racer has accepted the read, with the object metadata
   available from `Object.Metadata`; the body is consumed from the returned
   object.

2. The reading node resolves the object's current version, unless the request
   pins an ETag. Metadata is reused only for a short freshness window. An
   explicit pin may use expired metadata because the pin itself is the
   freshness decision. The resolved version fixes the size and the ETag for
   every page in this read.

3. The dataplane looks for each page in memory and then on local disk. A
   plaintext or ciphertext copy that passes verification can be served without
   leaving the node. A warm local page is the cheapest case: no peer traffic,
   no origin traffic, and no disk read if it is already in memory.

4. On a miss, the node ranks the page's owners and asks them in order. If the
   owner is a direct neighbor, the request is one hop. Otherwise the request is
   forwarded through the mesh, normally within two links. Per-neighbor
   connections are bounded per I/O worker, so a hot owner cannot consume every
   outbound slot.

5. An owner checks its own memory and disk. If it has the page, it returns the
   sealed page. If it does not, it mints origin authority for that page, asks
   the origin for the page-aligned range, validates the response against the
   metadata and ETag, encrypts the page once with the cache key, and publishes
   it. The reader may receive the page while the owner is still writing it to
   disk.

6. The reader verifies the page's authentication tag and length, decrypts it,
   and delivers the requested bytes. The SDK and the `racer-object` sidecar hold
   back the last byte until Racer confirms that the whole range was delivered
   intact. A read that is cut short therefore ends in an error rather than a
   silently truncated result, even if the destination already received the
   earlier bytes.

7. Concurrent requests for the same page on one node join a single-flight
   fill. The first request leads, later requests wait, and the number of waiters
   is bounded. Requests for different pages proceed independently, up to the
   configured flight and connection limits. Metadata refreshes use a similar
   cohort so that a burst of readers does not create a burst of `HEAD` calls.

In a cached cache, a page owner that cannot serve the page does not make the
reader fall back to a direct origin read. The reader can try another owner or a
predecessor copy, but the origin remains behind the ownership rules. If no
permitted source can serve the page within the request's deadline, the read
fails with `ErrUnavailable` or the more specific origin error, and the caller
decides whether to retry.

Retries are bounded and partitioned. A reader's attempt budget is split across
owners and links, and a forwarded request carries its own remaining allowance.
This prevents one slow owner from consuming all of a read's time and prevents a
relay from retrying forever. A stale membership can cause a retry under a newer
ranking, but it cannot extend the caller's deadline.

Page credits control read-ahead. The SDK's `PageCredits` sets how many pages
Racer may send ahead of the reader on one `Get`, and the dataplane's
`RACER_RANGE_WINDOW_PAGES` caps the same window. The effective window is the
smaller of the two. The SDK default is 2, the maximum is 64, and the dataplane
default is 4. A larger window improves throughput on high-latency links and
increases the memory held for the read.

### What Stays on Disk

Racer uses a second-sight admission policy by default. A page that this node
owns is always eligible for disk because the node is expected to serve it.
A page that the node does not own is eligible only if it has been seen before,
as recorded in a bounded rotating Bloom history. The history is a hint, not an
identity: a false positive may admit a page that will not be read again, and a
false negative may keep a page out of disk that would have been useful. The
policy is designed to keep the disk for pages with evidence of reuse rather than
for every page that passes through the node.

Admission also depends on headroom. Pages waiting to be written are charged to
the dirty budget, and a non-owned page without optional dirty headroom is not
persisted. A disk failure may discard dirty bytes; the page can still be served
from memory or fetched again from a peer or origin. Admission is about
persistence, not about whether the current read succeeds.

Eviction is segment-granular rather than page-granular. The store keeps
fixed-size segments, and when it needs space it chooses whole segments using a
score. The score combines read heat, capped at three, with one extra point when
this node is a placement owner. Under pressure, pages from caches that are over
their fair share are evicted before pages from caches that are under share.
Within the same share class, the lower score goes first, and age breaks ties.
Segments with a live hold or an unpublished write are not evicted. The scan is
bounded, so eviction work does not stall the read path.

Cache priority determines the fair share. Disk capacity is weighted across
published caches plus any unpublished cache that still holds data on the node,
which counts at the default weight. Memory capacity is weighted across caches
that are actively using memory, and each cache also has a bounded working
allowance. A cache below its share is protected relative to one above its
share, but no cache is promised that its data will survive. Holding a cache at
a high priority does not reserve physical space that another cache cannot use
when the high-priority cache is idle.

Slab files live under `/var/lib/racer/slabs`. Each I/O worker has its own slab,
and the default slab size is 16 GiB per worker, divided into segments whose
default size is 1 GiB. A segment must fit one page plus storage overhead, and
the slab size must be a multiple of the segment size. The files are sparse, so
the node does not need to allocate the full slab immediately, but the
filesystem must have room for the pages that are actually written.

Raw block devices are an alternative to slab files. A Node annotation named
`racer.unbounded-cloud.io/block-devices` supplies a device selector, and the
dataplane scans `RACER_DEVICE_DIRECTORY` for matching `disk/by-id` entries.
Selected devices are opened exclusively, must be unpartitioned, and must have a
zero guard area. Device mode changes where payloads live, not how pages are
indexed or evicted. Checkpoints remain in the slab directory.

The disk index is checkpointed in two alternating files, `checkpoint.0` and
`checkpoint.1`. A checkpoint records the index and segment geometry, not a
payload copy. Publication freezes the owners, writes a consistent image, and
then releases them. There is no `fsync`, so a checkpoint is not a durability
guarantee. On restart, Racer selects the newest checkpoint it can validate. A
checkpoint cut at another page size, or one that does not match the current
storage geometry, is discarded and the cache starts cold. Pages that were
written but not covered by a valid checkpoint are also lost.

A ClusterCache is not durable storage, and that is the point. Do not use it as
the only copy of data, do not back up a cache, and do not treat a cache restart
as an incident that requires restoring bytes. The origin is the recovery plan:
reads refill from the source, and the owner path rebuilds the cache as the
workload runs. The same applies to deleting a cache: a new ClusterCache with
the same name is a new object with a new UID and an empty cache.

Prefetch follows the same rules. A prefetch ticket names object ranges, and
Racer schedules the missing pages to their owners through the normal read path.
Nothing is pinned, so loaded pages are evicted like any other page. Tickets are
local to the node and kept in memory; after a Racer restart, resubmitting the
same ticket id loads only the pages that are still missing. Prefetch can improve
the first read of a known object set, but it cannot reserve capacity ahead of
foreground demand, and it is not a way to make a cache durable.

## Security

Racer's security model has three layers: control-plane identity, peer and origin
transport, and page encryption at rest and in flight. Each layer answers a
different question. Identity answers who may join the cache cluster. Transport
answers whether a request or response came from the peer it claims. Page
encryption answers whether a node or disk can read the bytes.

### Enrollment and Node Identity

A dataplane Pod uses a projected Kubernetes ServiceAccount token to authenticate
to the control API, and a separate projected token with the `racer-control`
audience to enroll. The seat validates the token with TokenReview and then
checks the live ServiceAccount, Pod, DaemonSet owner, and Node binding. A caller
cannot claim a node name without the matching Kubernetes identity.

A node generates its private key locally and keeps it on the node. It submits a
certificate signing request, and a seat issues a short-lived leaf certificate
bound to the cluster and node identity. The current leaf lifetime is 24 hours.
The seat publishes its own serving certificate and endpoint in its Lease, and
clients pin that certificate before sending an enrollment token. A Lease change
invalidates pooled connections to the previous holder, so a failover cannot
silently reuse a connection to a stale seat.

Each seat has its own issuer root. The primary commits a seat's public root to
the keyring before that seat issues certificates. Retired roots remain trusted
for the lifetime of leaves they could have issued. The keyring has capacity and
encoded-byte limits, and rotation is bounded so a restart cannot grow trust
state without limit.

### Page Encryption and Integrity

Pages are encrypted with XAES-256-GCM, a 256-bit key construction with a 192-bit
nonce and a 128-bit authentication tag. Every page has a key identifier, a
nonce, its cluster page size, its page identity, and its plaintext and
ciphertext lengths. The page identity is part of the authenticated data, so a
page cannot be moved to another object, version, or page number without the tag
check failing.

Encryption is per cache. A cache's keys are created and rotated by the control
plane, and a key is bound to a purpose: page data and origin credentials use
different keys. The primary reconciles the keyring and commits key state before
publishing it. A rotation activates a new key while older keys remain available
for decryption for a bounded time. `RACER_RETAINED_PAGE_KEYS` controls how many
readable page keys a cache keeps, with a default of seven. Lowering the value
does not drop keys immediately; extra decrypt keys are dropped at the next
rotation.

A page is verified before it is served or stored. A corrupted page fails the
authentication check and is not returned as valid data. The same authentication
check applies to pages read from local disk or received from a peer. Origin
bytes are validated against the metadata and the requested range, then sealed
before they are served or stored. A failed check is a cache miss or an error,
not a reason to return partial content.

### Signed Peer Requests

Peer request heads are signed with the sender's node key and cover the logical
request fields, the route, and the forwarding chain. A relay preserves the
original signature and adds its own, so the receiver can verify both the
original requester and every hop that forwarded the request. A response is bound
to the outstanding request and signed by the responder. Replaying a request id
alone is not enough to forge a request.

Page bodies are not separately signed. They are encrypted and authenticated
with the cache's page key, and the page descriptor binds the body to its object,
version, and page number. This avoids signing every 16 MiB body while still
rejecting a body that was altered, truncated, or taken from another page.

### Sealed Credentials and Metadata

The `Metadata` and `Authorization` values a client supplies are origin request
context, not Racer authorization. When the owner is remote, they travel in a
cache-scoped encrypted envelope; a local owner passes them to the origin as
request headers. They are not part of the page cache, a metadata cache, a
checkpoint, a log line, or a metric label. Formatting a request redacts both
values, and the origin client does not retain them in pooled connections after
an operation ends.

A cache hit never calls the origin and never reuses a client's credential. That
has two consequences. First, a cached object is available to any pod that can
use the cache, regardless of which pod originally fetched it. Second, rotating
or revoking an origin credential does not invalidate cached pages; it affects
the next origin fetch. If that is not acceptable, have the origin return an
earlier `ExpiresAt`, or do not cache the object.

For the built-in Azure origin, `containerURL` must use HTTPS when a credential is
configured. Workload identity exchanges a projected token with the
`api://AzureADTokenExchange` audience for an Entra token, so the identity needs a
federated credential for the Racer dataplane ServiceAccount. The node managed
identity path uses IMDS and an optional user-assigned `clientID`. The deprecated
`secretRef` field is rejected rather than silently falling back to a different
identity.

### The Socket Is the Trust Boundary

Racer does not authenticate individual client requests. The client socket's
directory is the boundary: a pod that can mount
`/run/racer/<cache>/client` can read any object in that cache, and a pod that
cannot mount it cannot reach the cache at all. The `racer-object` sidecar follows
the same rule and serves trusted pod-local clients without authentication.

This makes cache isolation a Kubernetes scheduling and volume problem, not a
per-request authorization problem. All pods attached to a cache share its key
space, including objects fetched with another pod's origin credentials. Every
enrolled dataplane node is also inside the boundary, because the keyring is
delivered to every dataplane node rather than only to owners. If two workloads
must not see each other's cached objects, give them separate ClusterCaches and
separate socket mounts. Do not expose the socket through a host path, a Service,
a sidecar that forwards arbitrary requests, or a network proxy.

The control plane's RBAC is also part of the boundary. The operator grants the
dataplane the permissions it needs to become a seat, and admission policies
constrain Pod report writes to the caller's own Pod and the single report
annotation. Kubernetes RBAC cannot make those permissions depend on Lease
ownership, so seat placement is not a security boundary. The security boundary
is the identity checks, the signed transport, and the page keys.

### What the Model Does Not Protect

Racer protects page data from other caches and from a peer or disk that does not
have the cache's keys. It does not protect data from a compromised dataplane
node, a compromised pod that can mount the client socket, or a cluster
administrator who can read control state. Those principals are inside the trust
boundary by design.

Eviction is metadata-only. When a segment is reclaimed, Racer stops referencing
its pages, but it does not securely erase the old bytes from the slab file or
device. The bytes are encrypted, so they are not readable without the cache
keys, but the ciphertext remains on the medium until it is overwritten. If a
node or disk is leaving your control, treat the slab as sensitive and wipe it
according to your normal media handling policy rather than relying on cache
eviction.

A ClusterCache does not provide tenant isolation by itself. It provides a named
cache with a UID, a key set, an origin, and an optional node pool. Isolation
comes from how you combine those with namespaces, volume mounts, and separate
caches. The `spec.nodeSelector` restriction is a placement control and an
admission control for client pods, not a cryptographic boundary.

## See Also

- [Gantry image distribution]({{< relref "guides/gantry" >}}) for the peer-to-peer
  OCI path and the Racer backend that uses a `ClusterCache` named `gantry`.
- [Racer reference]({{< relref "reference/racer" >}}) for the generated
  ClusterCache fields and Racer configuration settings.
- [racer-object reference]({{< relref "reference/racer-object" >}}) for the
  read-only S3 and Azure Blob adapter, its flags, and its API behavior.
- [Cache Azure Blob Storage with Racer]({{< relref "guides/azure-blob-cache" >}})
  for an end-to-end cache and sidecar setup.
- [Load vLLM models from Racer]({{< relref "guides/racer-vllm" >}}) for the GPU
  read path and a model-serving integration.
- [Unbounded architecture]({{< relref "reference/architecture" >}}) for how Racer
  fits with the agent, the operator, and the rest of the control plane.
- [Operations]({{< relref "guides/operations" >}}) for day-two checks that apply
  to the dataplane and its host nodes.
