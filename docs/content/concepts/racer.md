---
title: "Racer"
weight: 4
description: "A high-performance distributed blob cache for data-intensive workloads."
---

{{< callout type="note" >}}
Racer is a new component, and this documentation is preliminary. More guides
and reference documentation are coming later.
{{< /callout >}}

Racer is a distributed blob cache that keeps frequently accessed data close to
your workloads. It sits between applications and underlying storage, reducing
repeated data transfers and helping workloads spend less time waiting for data.

## How It Works

Racer uses storage across participating nodes as a shared cache. Requests reuse
cached data locally or from peers, fetching missing data from the underlying
source as needed. The same cache layer can serve different kinds of blobs,
including datasets, model weights, and application artifacts.

Objects are split into **64 MiB pages**. Metadata has its own hashed owner,
and each page is independently hashed across the cluster using its object
identity, version, length, and aligned offset. Different pages can therefore
use different nodes, although hash collisions can place them on the same node.
Each page starts its own bounded fallback chain when an owner is unavailable.
HTTP and RDMA use the same placement rules.

Cache capacity is rounded up to a 64 MiB boundary, with a minimum of 512 MiB
per storage shard. The managed agent uses eight page buffers (512 MiB total)
on one NUMA node and requests 4 GiB of memory.

## Built for Performance

Racer is designed for high throughput, low latency, and efficient CPU use:

- **RDMA support** accelerates transfers between cache nodes on compatible
  networks, reducing CPU overhead when moving large amounts of data.
- **Fast local storage** serves cached data close to workloads and provides
  cache capacity beyond memory alone.
- **Distributed reuse** lets nodes share cached data, reducing repeated reads
  from the underlying storage as more workloads request the same content.

Racer also supports standard TCP networking, so RDMA hardware is optional.

## When to Use Racer

Use Racer when many workers read the same blobs, repeated downloads limit
throughput, or shared storage becomes a bottleneck. It is a good fit for
data-intensive workloads such as machine learning and analytics.

## Enable Racer for a Site

With the Unbounded operator installed, enable Racer on an existing Site by
setting `spec.components.racer.enabled` to `true`. Replace `my-site` with your
Site's name:

```bash
kubectl patch sites.unbounded-cloud.io my-site --type=merge \
  -p '{"spec":{"components":{"racer":{"enabled":true}}}}'
```

The operator deploys the shared Racer control plane and cache agents for the
Site. Racer is disabled by default.
