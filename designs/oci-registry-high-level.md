# OCI Registry Cache Proxy Design

## Introduction

Large Kubernetes and AI clusters with thousands of nodes pulling the same container images face registry rate limiting, high egress costs, slow cold-starts, and pull storms. These problems are amplified for AI/HPC workloads where images contain multi-GB model weights and CUDA libraries.

This design introduces an OCI registry proxy frontend for unbounded-storage. The proxy intercepts container image pulls from containerd, splits OCI layers into fixed-size chunks, and stores them through the existing unbounded-storage P2P cache and regional cache hierarchy. Only a small number of hosts - ideally one per region - ever fetch from the origin registry.

## Architecture

The registry proxy is an OCI registry frontend on the unbounded-storage data plane (see [storage-high-level.md](storage-high-level.md)):

```text
+-------------+
|  kubelet    |
+-------------+
        |
        v
+-------------+
| containerd  |
+-------------+
        |
        v
+----------------------------+
| Local Registry Proxy       |
| (OCI frontend on P2P cache)|
+----------------------------+
        |
        v
+----------------------------+
| unbounded-storage P2P cache|  <-- RDMA/TCP peer transfers, local NVMe
+----------------------------+
        |
        v
+----------------------------+
| Regional Cache             |  <-- pull-through, single-flight origin fetches
+----------------------------+
        |
        v
+----------------------------+
| Origin Registry            |
| docker.io / ghcr.io / ECR  |
+----------------------------+
```

The P2P cache runs on every node and shares hot chunks with peers over RDMA (with TCP fallback). The regional cache deduplicates origin fetches and serves as a stable seed source. This is the same hierarchy used by all unbounded-storage frontends (S3, FUSE, OCI).

## How It Works

### Containerd Integration

The proxy runs as a local registry mirror on each node:

```toml
# /etc/containerd/certs.d/_default/hosts.toml

[host."http://127.0.0.1:65001"]
  capabilities = ["pull", "resolve"]
```

From containerd's perspective, the proxy behaves as a standard OCI registry endpoint.

### Pull Flow

1. The proxy intercepts the manifest request and extracts layer digests and sizes.
2. For each layer blob, the proxy splits it into fixed-size chunks and stores/retrieves them via the local P2P cache.
3. The P2P cache serves chunks from local NVMe, pulls from a peer over RDMA, or pulls through from the regional cache.
4. On a regional miss, the regional cache fetches from the origin registry using single-flight coordination (one upstream fetch per blob, regardless of how many nodes request it simultaneously).
5. The proxy reconstructs the original blob stream and returns it to containerd, which performs standard digest verification.

### Single-Flight Origin Fetching

The regional cache prevents pull storms by coordinating origin access. When thousands of nodes request the same missing layer, only one upstream fetch occurs. All other requesters stream chunks progressively as they arrive. This is the same single-flight behavior provided by the regional cache for all unbounded-storage backends.

## Security and Authentication

The proxy operates as a registry mirror, not a TLS MITM proxy. Registry authentication (imagePullSecrets, OAuth, ECR/GCR credentials, bearer tokens) passes through to the origin as needed. OCI digest verification remains unchanged - containerd validates blob integrity regardless of the cache source.

## Open Issues

### Chunk-to-Origin Mapping on Cache Miss

The P2P cache is content-addressed and registry-agnostic - it deals in chunks identified by checksum and index. When a chunk miss propagates all the way to the regional cache (or beyond), something must map that chunk back to a specific byte range of a specific blob on a specific origin registry, with valid credentials.

**Example:** A pull of `ghcr.io/unbounded/model:v1.0.0` resolves to 4 layers. One layer is split into 20 chunks. The P2P cache requests chunk 12, which routes to a peer that should own it. That peer doesn't have it. How does the system:

1. Map chunk 12 back to the origin blob digest and byte range (e.g. `sha256:aaa...` bytes 768MB-832MB)?
2. Determine the origin registry URL for that blob (`ghcr.io/v2/unbounded/model/blobs/sha256:aaa...`)?
3. Obtain valid auth credentials for that registry (the original puller's imagePullSecret may not be available to the peer or regional cache)?

Possible directions:

- **Chunk metadata in the P2P cache** - store a small metadata record alongside each chunk mapping it back to origin (registry, repository, blob digest, byte offset). The regional cache uses this to issue a `Range` request to the origin.
- **Regional cache as the only origin fetcher** - nodes never fetch from origin directly. The regional cache maintains its own service credentials for all configured registries, avoiding the need to propagate per-user imagePullSecrets.
- **Manifest registration at pull time** - when the proxy first resolves a manifest, it registers the full layer-to-origin mapping (including a credential reference) with the regional cache, so future misses can be resolved without the original requester.

This is unresolved and affects the boundary between the OCI-aware proxy layer and the registry-agnostic P2P/regional cache.

## Related Work

- [Dragonfly](https://d7y.io) - P2P file distribution and image acceleration
- [Spegel](https://spegel.dev) - Stateless OCI image distribution for Kubernetes
- Kraken (Uber) - P2P Docker registry for large-scale deployments
- Harbor pull-through cache
