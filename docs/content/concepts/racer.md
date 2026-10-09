---
title: "Racer Distributed Cache"
weight: 4
description: "How Racer lets nodes share cached object data so each object is read from its source once."
---

## Why Racer?

Many workloads read the same large files on many nodes at once: container image
layers, model weights, datasets. Without a shared cache, every node downloads
its own copy from the same registry or object store. This:

- Multiplies egress cost.
- Overloads the source when a large job starts.
- Makes scale-out as slow as the slowest download.

**Racer** is a read-through cache that runs on every node. Nodes share cached
data with each other, so the cluster normally reads each piece of an object from
its source only once. Apps read through a local Unix socket. When no node has
the data yet, Racer asks an **origin**, a small program you write that fetches
objects from your own storage.

## Architecture

Racer runs two components:

**Controller** (`racer-controller`) -- A Deployment with three replicas and one
elected leader:

- Builds the member list from Nodes and dataplane pods, and publishes it to
  every node.
- Enrolls each dataplane pod and issues it a short-lived certificate.
- Creates and rotates the encryption keys for each cache.

**Dataplane** (`racer-dataplane`) -- A DaemonSet on every node:

- Serves app reads on per-cache Unix sockets under `/run/racer/<cache>/`.
- Keeps pages in memory and on local disk.
- Fetches pages from other nodes over TCP, or over RDMA where both nodes
  support it.
- Calls the local origin when this node is responsible for a page that no
  node has yet.

Apps and origins use the Go SDK (`pkg/racersdk`). It provides a `Client` for
reads and `ServeOrigin` for origins.

You do not install Racer directly. `unbounded-operator` deploys the controller
and dataplane when the first `ClusterCache` is created.

## Core Concepts

### API

A **ClusterCache** represents a logical cache namespace:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
```

Each cache gets two sockets on every node:

- `/run/racer/<name>/client/socket` -- Apps read objects here.
- `/run/racer/<name>/origin/socket` -- The origin listens here.

There is no access list on the volume. A pod can use a volume only if it mounts
that volume's socket directory. The volume's Kubernetes UID is its identity, so
deleting and recreating a volume starts a new, empty cache. Racer also accepts
the `ClusterCache` kind for the same purpose. One name cannot be used by both
kinds.

### Objects, Keys, and Versions

- **Key** -- 32 bytes chosen by the app, often a content digest such as a
  SHA-256.
- **Version** -- One fixed set of bytes for a key, named by an **ETag** that the
  origin returns. The bytes of a version must never change. Every page of a read
  comes from the same version.
- **Expiry** -- The origin can optionally set `ExpiresAt` for each version. After that time,
  Racer asks the origin again before serving a fresh read. A read pinned to a
  specific ETag can still use the older version.

### Pages

Racer splits every object into **16 MiB pages**. A page is the unit for caching,
transfer between nodes, placement, and origin reads.

### Origins

An origin answers two kinds of requests:

- **HEAD** -- Returns the size, ETag, and expiry for a key.
- **GET** -- Returns one page-aligned range of a version.

Origins do not push data into Racer. Any node can own a page, so run the origin
on every node, usually as a DaemonSet that mounts the volume's origin socket.

The client can pass `Metadata` and `Authorization` headers with each read. Racer sends
them to the origin unchanged and never caches them. Use them to tell the origin
where to find the object and to securely pass the caller's credentials.

### Owners and Shares

Each page has up to **three owner nodes**. Racer picks them with weighted
rendezvous hashing over node IDs. Each node has a weight called **shares**
(default `4`). A node with more shares owns more pages. Set it with the Node
annotation `racer.unbounded-cloud.io/shares`.

Owners do not depend on the version, so a new version of an object lands on the
same nodes as the old one. Only owners call the origin. Every other node gets
the page from an owner.

To keep a node out of Racer, label it `racer.unbounded-cloud.io/exclude`.

## How a Read Works

1. The app calls `Get` on the volume's client socket.
2. The local dataplane checks memory, then local disk.
3. On a miss, it asks the page's owners in rank order. Nodes connect only to a
   limited set of neighbors, so a request may pass through a few other nodes on
   the way.
4. If no owner has the page, an owner calls its local origin, encrypts the
   page, and keeps it.
5. The page streams back to the app. The SDK holds back the last byte until
   Racer confirms the whole range arrived intact.

If many readers on one node want the same page at the same time, the node makes
one request and shares the result.

### What Stays on Disk

Owners keep a page on disk the first time it is read. Other nodes keep a page on
disk only after it is read a second time. This stops one-off reads from pushing
out useful data. This policy is called **second-sight**.

By default, disk data lives in slab files under `/var/lib/racer/slabs`. You can
give Racer whole raw block devices instead. On restart, a node reloads its
checkpoint and keeps its cached pages.

## Security

- **Enrollment** -- Each dataplane pod proves its identity to the controller
  with a projected ServiceAccount token. The controller checks that the pod
  belongs to the Racer DaemonSet, then issues a certificate valid for 24 hours.
- **Encryption** -- Pages are encrypted with a per-volume key, both between
  nodes and on disk. The controller rotates keys every 24 hours by default.
- **Signed requests** -- Every node signs each request it sends or forwards to
  another node.
- **Credentials** -- `Authorization` and `Metadata` are sealed when they cross
  nodes, and are never written to disk.
