---
title: "Racer Distributed Cache"
weight: 4
description: "How Racer is designed to let nodes share cached object data and reduce repeated reads from its source."
---

{{< callout type="note" >}}
Racer is under development. The `ClusterCache` API, Go SDK, and controller are
on `main`; the dataplane application and operator deployment integration are
still pending. This page describes the intended architecture, not a deployment
available from `main` today.
{{< /callout >}}

## Why Racer?

Many workloads read the same large files on many nodes at once: container image
layers, model weights, datasets. Without a shared cache, every node downloads
its own copy from the same registry or object store. This:

- Multiplies egress cost.
- Overloads the source when a large job starts.
- Makes scale-out as slow as the slowest download.

**Racer** is a read-through cache designed to run on each participating node.
Nodes share cached data with each other to reduce repeated reads from the
source. Apps read through a local Unix socket. When no node has the data yet,
Racer asks an **origin**, an adapter that fetches objects from your storage.
An integration can provide this adapter, or you can write one with the SDK.

Sharing and combining reads reduces source traffic; it does not guarantee
exactly one source read. Retries, eviction, expiry, and node failures can cause
another fetch.

## Architecture

Racer's planned deployment has two components:

**Controller** (`racer-controller`) -- A Deployment with one elected leader:

- Builds the member list from Nodes and dataplane pods, and publishes it to
  every node.
- Enrolls each dataplane pod and issues it a short-lived certificate.
- Creates and rotates the encryption keys for each cache.

**Dataplane** (`racer-dataplane`) -- A DaemonSet on participating nodes:

- Serves app reads on per-cache Unix sockets under `/run/racer/<cache>/`.
- Keeps pages in memory and on local disk.
- Fetches pages from other nodes over TCP, or over RDMA within the same Site
  when both nodes have compatible RDMA devices and rails (network paths).
  RDMA is not a cross-Site transport.
- Calls the local origin when this node is responsible for a page that no
  node has yet.

Apps and origins use the Go SDK (`pkg/racersdk`). It provides a `Client` for
reads and `ServeOrigin` for origins.

The planned operator integration will deploy the controller and dataplane
when the first `ClusterCache` is created. Creating a `ClusterCache` on `main`
does not yet install these workloads.

## Core Concepts

### API

A **ClusterCache** represents a logical cache namespace:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
```

Each cache uses two sockets on every participating node:

- `/run/racer/<name>/client/socket` -- Apps read objects here.
- `/run/racer/<name>/origin/socket` -- The origin listens here.

There is no access list on the `ClusterCache`. A pod needs access to the
cache's socket directory, normally through a mount, and the required filesystem
permissions. The `ClusterCache`'s Kubernetes UID is its identity, so deleting
and recreating it gives it a new cache identity.

### Objects, Keys, and Versions

- **Key** -- 32 bytes chosen by the app, often a content digest such as a
  SHA-256.
- **Version** -- One fixed set of bytes for a key, named by a strong **ETag**
  that the origin returns. The bytes of a version must never change. Every page
  of a read comes from the same version.
- **Expiry** -- The origin must set `ExpiresAt`. It limits how long Racer can
  reuse its current-version selection for unpinned reads. A read pinned to an
  ETag may still use that cached version after expiry, but pinning does not
  guarantee the version remains available.

### Pages

Racer splits every object into **16 MiB pages**. A page is the unit for caching,
transfer between nodes, placement, and origin reads.

### Origins

An origin answers two kinds of requests:

- **HEAD** -- Returns the size, ETag, and expiry for a key.
- **GET** -- Returns one page-aligned range of a version.

Origins do not push data into Racer. Any participating node can own a page, so
the planned deployment runs the origin on each such node, usually as a DaemonSet
that mounts the cache's origin socket directory.

The client can pass `Metadata` and `Authorization` values with each read. Racer
forwards them to the origin unchanged on a cache miss. Use them to tell the
origin where to find the object and to pass the caller's credentials.

A cache hit does not call the origin to reauthorize the caller. These values
are not a per-read access check for cached data. Only share a cache among
readers allowed to read its contents; enforce any further authorization before
the request reaches Racer.

### Owners and Shares

Each page is intended to have up to **three owner nodes**. Racer picks them
with weighted rendezvous hashing over node IDs. Each node has a weight called
**shares** (default `4`). A node with more shares owns more pages. Set it with
the Node annotation `racer.unbounded-cloud.io/shares`.

Owners do not depend on the version, so a new version of an object lands on the
same nodes as the old one while membership and shares stay the same. Only
owners call the origin. Every other node gets the page from an owner.

To keep a node out of Racer, label it `racer.unbounded-cloud.io/exclude`.

## How a Read Works

The planned dataplane read path is:

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
is designed to combine their requests and share the result.

### What Stays on Disk

The planned disk policy makes owner pages eligible for retention on the first
read. Other nodes' pages become eligible after a second read. This reduces the
chance of one-off reads pushing out useful data. This policy is called
**second-sight**. Eligibility does not guarantee retention: admission limits,
available capacity, and eviction still apply.

The planned default storage is slab files under `/var/lib/racer/slabs`, with
raw block devices as an alternative. Checkpoints are intended to let a node
recover cached pages after a restart. The cache is not durable storage; keep
the source available to refill missing pages.

## Security

The security model combines controller-issued credentials with planned
dataplane protections:

- **Enrollment** -- Each dataplane pod proves its identity to the controller
  with a projected ServiceAccount token. The controller checks that the pod
  belongs to the Racer DaemonSet, then issues a short-lived certificate
  (24 hours by default).
- **Encryption** -- Pages are encrypted with a per-cache key, both between
  nodes and on disk. The controller rotates keys every 24 hours by default.
- **Signed requests** -- Every node signs each request it sends or forwards to
  another node.
- **Credentials** -- `Authorization` and `Metadata` are intended to be sealed
  when they cross nodes and kept out of disk storage. They do not authorize
  cache hits; access to the cache socket remains a trust boundary.

## See Also

- **[Gantry Guide]({{< relref "guides/gantry" >}})** -- Container image
  distribution with Gantry.
- **[Architecture]({{< relref "reference/architecture" >}})** -- Unbounded's
  components and deployment model.
