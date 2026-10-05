---
title: "Racer Distributed Cache"
weight: 4
description: "How Racer shares immutable object data across Kubernetes nodes."
---

## What Is Racer?

Racer is a distributed object cache for Kubernetes workloads. Applications read
through a local Unix socket; Racer retrieves data from memory, local disk, peers,
or an application-provided origin. Use it to share repeatedly read objects
without making every consumer download them from the upstream service.

Racer is **not durable object storage**. Keep an authoritative origin: cached
bytes can be evicted or lost. It is also not a filesystem or a registry by itself.
[Gantry]({{< relref "guides/gantry#use-racer-as-the-backend" >}}) supplies the
container-registry integration; other applications use the Go SDK.

## Components

| Component | Responsibility |
|-----------|----------------|
| **ClusterCache** | Names a cluster-wide cache and its local socket directories. |
| **Controller** | Manages authenticated membership, cache keys, and replicated control state. |
| **Dataplane** | Runs on eligible Linux nodes, stores object pages, and serves client and peer reads. |
| **Origin adapter** | Application code that resolves keys and returns immutable versions from the authoritative source. |
| **Go SDK** | Connects consumers to the local dataplane and serves origin callbacks. |

The Unbounded operator deploys Racer when the first ClusterCache is created.
Creating the resource does not deploy an origin adapter or populate the cache.
Consumers need a local dataplane; adapters must cover every eligible node that
can supply origin reads, with one origin owner per cache per node.

## Objects and Versions

An object has a **32-byte key**, an immutable version identified by a strong
quoted **ETag**, a size, and a metadata expiration time. Your adapter defines the
key mapping and must return the same bytes for the same key/version everywhere.
Keys are opaque bytes, not arbitrary URLs or filenames.

Racer transfers objects in **16 MiB pages**, with a shorter final page. Consumers
can request byte ranges without downloading the entire object. A read stays on
one admitted immutable version; a pinned read requests a particular ETag.

Fresh reads may reuse unexpired metadata, so "fresh" does not guarantee an origin
round-trip. Expiration controls admission of fresh reads, not the lifetime of an
already admitted stream. Choose expiration deliberately when implementing an
adapter for names whose current version can change.

## Placement and Transport

Racer chooses cache locations from its membership and node placement weights.
The `shares` annotation changes relative weight, not a node's memory or disk
limit. Local storage and memory budgets are configured separately.

Peers use HTTP, with optional RDMA on eligible hops. RDMA requires the same
nonempty Site and a compatible selected rail at both ends; other hops retain
HTTP. A Site is an RDMA boundary, not a cache-placement boundary. See the
[RDMA reference]({{< relref "reference/racer#rdma" >}}) for hardware and deployment
requirements.

## Access and Failure Behavior

Applications receive access through mounts of a cache's socket directories.
Mount only the client directory for consumers and the origin directory for
trusted adapters. Ordinary clients do not need a Racer-specific ServiceAccount
or token. Upstream authorization, when needed, is passed to the origin adapter
as request context; it is not a replacement for controlling socket access.

The SDK does not bypass Racer to read directly from an origin when the dataplane
is unavailable. Handle read errors, close values, and treat partial output as
incomplete. Cache bytes are disposable, but cluster identity, credentials, and
version counters are durable installation state and must be preserved.

## Next Steps

- [Cache Objects with Racer]({{< relref "guides/racer" >}}): deploy a cache and connect an application.
- [Racer Reference]({{< relref "reference/racer" >}}): configuration, SDK APIs, diagnostics, and RDMA.
