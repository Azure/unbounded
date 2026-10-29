---
title: "Racer Distributed Cache"
weight: 4
description: "How Racer lets nodes share cached object data and reduce repeated reads from its source."
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
proportion to its `spec.priority`. Under pressure, memory eviction and disk
admission prefer data from caches below their share, within bounded scans. A
cache can use idle space beyond its share.

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
- **Encryption** -- Pages are encrypted with a per-cache key, both between
  nodes and on disk. The elected control plane creates and rotates these keys.
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
