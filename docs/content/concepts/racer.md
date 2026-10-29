---
title: "Racer Distributed Cache"
weight: 4
description: "How Racer turns cluster nodes into one shared cache, when to use it, and how it works."
---

## Why Racer?

Many workloads read the same large files on many nodes at once: container image
layers, model weights, datasets. Without a shared cache, every node downloads
its own copy from the same registry or object store. This:

- Multiplies egress cost.
- Overloads the source when a large job starts.
- Makes scale-out as slow as the slowest download.

**Racer** is a read-through cache that runs on each participating node.
Nodes share cached data with each other to reduce repeated reads from the
source. Apps read through a local Unix socket. When no node has the data yet,
Racer asks an **origin**, an adapter that fetches objects from your storage.
An integration can provide this adapter, or you can write one with the SDK.

Sharing and combining reads reduces source traffic; it does not guarantee
exactly one source read. Retries, eviction, expiry, and node failures can cause
another fetch.

### What Racer Gives You

- **No cache servers to run.** Racer pools the memory and local disks of the
  nodes you already have into one cache. There is no separate cache fleet to
  buy, size, or patch, and no controller Deployment. The operator installs
  Racer when you create a `ClusterCache`.
- **Capacity and bandwidth that grow with the cluster.** Every node stores
  pages and serves them to other nodes. Adding nodes adds cache space and
  serving bandwidth. Racer places each 16 MiB page on its own owners, so the
  pages of one large file are spread across many nodes and served in parallel.
- **Low, bounded source load.** Only a page's owners, at most three nodes,
  read it from the source. All other nodes get the page from those owners. A
  rollout to a thousand nodes reads each page from the source a few times at
  most in normal operation, not a thousand times.
- **Any source.** Racer reads from any store you can write an origin for.
  Ready-made origins read S3, S3-compatible stores, Azure Blob Storage, and
  OCI registries. Racer runs on Linux nodes in any cloud, on-premises, or at
  the edge.
- **No code changes for most apps.** The
  [`racer-object` sidecar]({{< relref "reference/racer-object" >}}) serves an
  S3 or Azure Blob API on pod loopback. A webhook adds it to labeled pods.
  Existing S3 and Azure SDK clients change only settings: the endpoint, plus
  [path-style addressing and checksum settings]({{< relref "reference/racer-object#s3-api" >}})
  for S3. vLLM and
  containerd have their own integrations.
- **Encrypted and verified data.** Pages are encrypted and authenticated with
  a per-cache key between nodes and on disk. Nodes sign their requests. The
  SDK checks that every range arrived intact before it hands over the last
  byte. A corrupt or tampered page makes the read fail, so an app never
  completes a read with bad data.
- **One cache for images, models, and datasets.** Several caches share each
  node's capacity by priority. Gantry can store container images in Racer, and
  apps can warm and hold specific model or dataset versions ahead of a job.
- **Consistent versions.** Every page of a read comes from the same version.
  Readers never get a mix of old and new bytes while the source changes.
- **Fast paths for GPU nodes.** Racer can move pages over RDMA within a Site
  and write objects straight into NVIDIA GPU memory.

## When to Choose Racer

Racer fits best when many nodes read the same data and you want the cache to
scale with the cluster instead of running a separate cache tier.

| Use case | Why Racer fits |
|----------|----------------|
| Scale-out of a large job, such as hundreds of inference replicas loading the same model | Source reads depend on the number of pages, not the number of nodes. Serving bandwidth grows with each node that joins, so the cache does not become the bottleneck as the job grows. |
| GPU or storage-dense nodes with unused local NVMe | Racer turns local disk you already pay for into shared cache. You need no extra machines. |
| Hybrid, multi-cloud, or edge clusters | The same DaemonSet and API work on every Linux node in the cluster. S3-compatible support covers on-premises object stores. Nothing depends on one cloud's managed services. |
| Internal or custom data sources | An origin answers two requests, HEAD and GET, through the Go SDK. A new source needs a small adapter, not a new cache product. |
| Rate-limited, throttled, or costly sources | Only page owners read from the source, and Racer does not silently send reads to the source when the cache fails. Source load stays bounded during both rollouts and failures. |
| Data that must stay encrypted and verified | Pages are encrypted on node disks and on the wire, and every range is checked before the app uses it. |
| Container images, models, and datasets in one cluster | One cache layer serves all three. Priority decides how they share node capacity. |
| Fast model loads on NVIDIA GPU nodes | The [vLLM integration]({{< relref "guides/racer-vllm" >}}) reads weights straight into GPU memory, and RDMA speeds node-to-node transfers. |

### Racer Compared With a Dedicated Cache Tier

| | Racer | Dedicated cache servers |
|---|---|---|
| Extra machines | None | Cache servers to buy, size, and run |
| Cache capacity | Grows as nodes join | Grows only when you add cache servers |
| Serving bandwidth | Every node serves; total grows with the cluster | Limited by the cache servers' network links |
| Where cache work runs | On cluster nodes; you set each node's share | On separate machines |
| New data sources | Write an origin adapter | Limited to the sources the tier supports |

### Planning Considerations

- Racer uses some disk, memory, CPU, and network on each node in the cache.
  The [dataplane settings]({{< relref "reference/racer#dataplane-configuration" >}})
  size it on each node: the thread cap (`RACER_MAX_THREADS`), the
  `RACER_*_BYTES` memory budgets, and the slab files or raw devices it is
  given. The `shares` annotation, the `exclude` label, and `spec.priority` only
  choose which nodes hold pages and how capacity is split between caches. They
  do not limit Racer's use of a node.
- Racer does not fall back to the source on errors, which keeps source load
  predictable. The caller decides what to do. Containerd moves to its next
  mirror host, and apps can retry or read the source directly.
- Every node in the cache stores and forwards encrypted pages for the whole
  cache. Share a cache only among nodes and readers that may read its data.

## Architecture

Racer runs as one **DaemonSet**, `racer`. Each pod runs the same Rust
binary. It:

- Serves app reads on per-cache Unix sockets under `/run/racer/<cache>/`.
- Keeps pages in memory and on local disk.
- Fetches pages from other nodes over TCP, or over RDMA within the same Site
  when both nodes have compatible RDMA devices and rails (network paths).
  RDMA is not a cross-Site transport.
- Calls the local origin when this node is responsible for a page that no
  node has yet.

Some dataplane pods also hold **control seats**, elected through Kubernetes
Leases. One seat is the **primary**. It builds the member list and cache catalog
and commits their versions and encryption keys to Kubernetes. The seats serve
that committed state, enroll dataplane pods, and run the object-sidecar webhook.
Every node discovers seats through the Leases and connects to them directly.
There is no separate controller Deployment or controller image.

Apps and origins use the Go SDK (`pkg/racersdk`). It provides a `Client` for
reads and `ServeOrigin` for origins.

The Unbounded operator installs the DaemonSet and its supporting Kubernetes
resources when any `ClusterCache` exists. Origin adapters and application
sidecars remain separate from the Racer control plane.

## Core Concepts

### API

A **ClusterCache** represents a logical cache namespace:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
```

Each cache uses two Unix domain sockets (UDS) on every participating node:

- `/run/racer/<name>/client/socket` -- The dataplane listens here. Apps connect
  with `racersdk.Client` to read objects.
- `/run/racer/<name>/origin/socket` -- The origin listens here with
  `racersdk.ServeOrigin`. The local dataplane connects to fetch missing data
  from your storage.

There is no access list on the `ClusterCache`. A pod needs access to the
cache's socket directory, normally through a mount, and the required filesystem
permissions. The `ClusterCache`'s Kubernetes UID is its identity, so deleting
and recreating it gives it a new cache identity.

Optional `spec.priority` (0 to 1000, default 500) sets how a cache shares each
node's capacity with other caches. It is a relative weight, not a quota. See
[Priority]({{< relref "reference/racer" >}}#priority).

### Objects, Keys, and Versions

- **Key** -- 32 bytes chosen by the app, often a content digest such as a
  SHA-256.
- **Version** -- One fixed set of bytes for a key, named by a strong **ETag**
  that the origin returns. The bytes of a version must never change. Every page
  of a read comes from the same version.
- **Expiry** -- The origin must set `ExpiresAt` to define freshness. Pinning a
  read to an ETag selects that exact version but does not guarantee the version
  remains available.

### Pages

Racer splits every object into **16 MiB pages**. A page is the unit for caching,
transfer between nodes, placement, and origin reads.

### Origins

An origin answers two kinds of requests:

- **HEAD** -- Returns the size, ETag, and expiry for a key.
- **GET** -- Returns one page-aligned range of a version.

Origins do not push data into Racer. Any participating node can own a page, so
Racer runs the origin on each such node, usually as a DaemonSet
that mounts the cache's origin socket directory. For Azure Blob, the operator
can run this DaemonSet for you; see the
[Azure Blob cache guide]({{< relref "guides/azure-blob-cache" >}}).

The client can pass `Metadata` and `Authorization` values with each read. Racer
forwards them to the origin unchanged on a cache miss. Use them to tell the
origin where to find the object and to pass the caller's credentials.

A cache hit does not call the origin to reauthorize the caller. These values
are not a per-read access check for cached data. Only share a cache among
readers allowed to read its contents; enforce any further authorization before
the request reaches Racer.

### Owners and Shares

Each page has up to **three owner nodes**. Racer picks them
with weighted rendezvous hashing over node IDs. Each node has a weight called
**shares** (default `4`). A node with more shares owns more pages. Set it with
the Node annotation `racer.unbounded-cloud.io/shares`.

Owners do not depend on the version, so a new version of an object lands on the
same nodes as the old one while membership and shares stay the same. Only
owners call the origin. Every other node gets the page from an owner.

To keep a node out of Racer, label it `racer.unbounded-cloud.io/exclude`.

## How a Read Works

The read path is:

1. The app calls `Get` on the cache's client socket.
2. The local dataplane checks memory, then local disk.
3. On a miss, it asks the page's owners in rank order. Nodes connect only to a
   limited set of neighbors, so a request may pass through a few other nodes on
   the way.
4. If no owner has the page, an owner calls its local origin, encrypts the
   page, and makes it available to readers.
5. The page streams back to the app. The SDK holds back the last byte until
   Racer confirms the whole range arrived intact.

If many readers on one node want the same page at the same time, the dataplane
combines their requests and share the result.

### What Stays on Disk

The disk policy makes owner pages eligible for retention on the first
read. Other nodes' pages become eligible after a second read. This reduces the
chance of one-off reads pushing out useful data. This policy is called
**second-sight**. Eligibility does not guarantee retention: admission limits,
available capacity, and eviction still apply.

When caches compete for space, each gets a share of the node's capacity in
proportion to its `spec.priority`. Under pressure, eviction prefers data from
caches above their share, within bounded scans. Among pages in the same state,
hotter pages and pages this node owns stay longer. A cache can use idle space
beyond its share.

The default storage is slab files under `/var/lib/racer/slabs`, with
raw block devices as an alternative. Checkpoints let a node
recover cached pages after a restart. The cache is not durable storage; keep
the source available to refill missing pages.

## Security

The security model combines credentials issued by elected seats with dataplane
protections:

- **Enrollment** -- Each dataplane pod proves its identity to a control seat
  with a projected ServiceAccount token. The seat checks that the pod
  belongs to the Racer DaemonSet, then issues a short-lived certificate
  (24 hours by default).
- **Encryption** -- Pages are encrypted and authenticated (XAES-256-GCM) with
  a per-cache key, both between nodes and on disk. The elected control plane
  creates and rotates these keys.
- **Signed requests** -- Every node signs each request it sends or forwards to
  another node.
- **Credentials** -- `Authorization` and `Metadata` are sealed
  when they cross nodes and kept out of disk storage. They do not authorize
  cache hits; access to the cache socket remains a trust boundary.

## See Also

- **[Gantry Guide]({{< relref "guides/gantry" >}})** -- Container image
  distribution with Gantry.
- **[Architecture]({{< relref "reference/architecture" >}})** -- Unbounded's
  components and deployment model.
