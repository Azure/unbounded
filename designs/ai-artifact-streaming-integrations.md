# Artifact Streaming Integrations for AI

**Status:** Draft proposal

## Summary

AI workloads need fast startup and scale-out for more than container images.
They need to deliver OCI images, model weights, tokenizer assets, adapters,
checkpoint shards, and, later, KV or context cache snapshots. Today those
artifacts can enter the cluster through different stacks:

- ACR Artifact Streaming for OCI images converted to OverlayBD-backed remote
  layers;
- Gantry for cluster-local peer-to-peer OCI distribution;
- DACS-style blob and model-weight caches for non-OCI model artifacts; and
- higher-level runtime caches for KV or context reuse.

This proposal keeps one customer-facing product story while allowing the
underlying data paths to specialize by artifact shape. The first integration is
OCI image streaming: Gantry should work with ACR Artifact Streaming so an image
can start lazily from OverlayBD ranges while Gantry concurrently seeds complete
verified layers to cluster peers. The next integration is blob/model streaming:
the AI platform should expose a managed cache profile that can accelerate model
weights and other byte-addressable artifacts without forcing customers to
install a separate cache stack for each use case.

## Problem

Large AI clusters commonly scale many GPU nodes at once. The slow path is often
not scheduling or compute; it is moving bytes to the node:

- container images with CUDA libraries and inference servers;
- model weights and adapters stored outside images;
- checkpoint shards or safetensor files from Blob, object stores, or model hubs;
- shared runtime assets reused across many replicas; and
- future KV or context cache snapshots.

ACR Artifact Streaming solves the first-start problem for OCI images by allowing
containerd to mount an image before every layer is downloaded. Gantry solves a
different problem: it reduces repeated origin pulls by letting a small set of
nodes seed verified OCI content to the rest of the cluster. DACS-style systems
solve another problem: they fan in overlapping requests for large non-OCI
artifacts and cache chunks close to the workload.

If these remain separate customer choices, AI users face confusing questions:

- Should image acceleration use ACR Artifact Streaming, Gantry, DACS, or more
  than one?
- Does the cache cover DockerHub images, ACR images, model weights, and runtime
  snapshots, or only one category?
- Is cache capacity shared across node pools, GPU pools, and unrelated
  workloads?
- How are private registries, CMK-protected content, and identity-scoped blobs
  prevented from leaking through shared cache state?
- Who owns the managed experience when the customer just wants faster AI
  startup?

The product needs a single integration plan that makes these boundaries
explicit.

## Goals

- Provide one AKS/AI customer-facing story for artifact streaming and caching.
- Preserve ACR Artifact Streaming's fast first-start behavior for OCI images.
- Preserve Gantry's peer-to-peer distribution and verified complete-layer model.
- Support non-OCI model artifacts whose industry formats are not moving to OCI
  immediately.
- Let managed AI experiences enable the right cache path by default without
  requiring customers to install multiple overlapping systems.
- Keep artifact authorization, encryption, and cache visibility scoped to the
  identity and policy that allowed the original read.
- Allow separate cache policies for GPU node pools, CPU node pools, and
  workload classes so unrelated artifacts do not evict critical model data.

## Non-goals

- Requiring all AI artifacts to become OCI images.
- Making ACR responsible for all model-weight, KV-cache, or context-cache
  scenarios.
- Replacing model servers, inference runtimes, or application-level cache APIs.
- Designing a single physical cache that stores every artifact type with the
  same consistency, security, and eviction semantics.
- Exposing a required customer UX for every managed AI deployment. Managed
  experiences can enable opinionated defaults and expose only the controls that
  customers need.

## Proposal

Treat "Artifact Streaming for AI" as the product umbrella and split the
implementation into three integration lanes.

### Lane 1: OCI image streaming

Use ACR Artifact Streaming and Gantry together for OCI images.

ACR Artifact Streaming remains responsible for converting eligible OCI images
into OverlayBD-backed remote layers and allowing containerd to start a
container before the complete layer is local. Gantry remains responsible for
cluster-local provider discovery, complete-layer seeding, and peer transfer.

The desired cold path is:

```text
first Pod:    OverlayBD reads required byte ranges immediately
background:   Gantry seeds complete verified OCI layers to chair nodes
later Pods:   OverlayBD ranges are served from Gantry peers when available
```

This is the design direction in
[gantry support for acr artifact streaming](https://github.com/Azure/unbounded/pull/833).
That design should be the first concrete milestone because it connects existing
shipping concepts without changing the Pod spec.

The integration needs two Gantry surfaces:

1. a complete-object path for manifests, configs, and full OCI layers; and
2. a range-read path that OverlayBD can call while complete layers are missing
   or only partially needed.

Signed ACR data URLs remain local to the node that obtained them. Peer requests
use only immutable digests and byte ranges. Complete layers are advertised only
after containerd verifies them against their OCI digest.

### Lane 2: Blob and model-weight streaming

Use a DACS-style data path for non-OCI model artifacts.

Many model assets are byte-addressable blobs rather than container images. For
these artifacts, the platform should provide a managed artifact gateway that can
fan in overlapping cache misses, fetch chunks in parallel, and seed data across
the cluster or node pool.

The data path should be source-aware but format-agnostic:

```text
model loader / runtime
  -> node-local artifact endpoint
  -> node-pool or regional cache coordinator
  -> origin: Azure Blob, object store, model hub, or private artifact service
```

The gateway should optimize for large immutable files and shard sets. It should
not require customers to repackage every model into an OCI image before they can
benefit from streaming and cache reuse.

This lane can share product policy with OCI streaming, but it should not be
forced through the OCI registry protocol. Its cache keys, auth model, and
eviction behavior need to account for blob URLs, versioned model assets, and
identity-scoped access.

### Lane 3: KV and context caching

Keep KV and context caching as a separate runtime integration until the
requirements are clearer.

KV and context cache snapshots have different semantics from immutable images
or model weights. They may be generated by a tenant workload, tied to model
version and runtime behavior, and have stricter freshness or privacy
requirements. They should be included in the overall product narrative, but not
blocked on the image and model-weight streaming work.

The initial requirement is to leave an extension point:

- cache policy can name runtime-managed cache classes;
- cache capacity can reserve space for them; and
- the managed AI experience can enable or disable them independently from image
  and model streaming.

## Customer experience

Managed AI deployments should get artifact acceleration by default. Customers
should not have to decide whether to install Gantry, DACS, both, or neither for
the common path.

The customer-facing model should be a profile, not a list of daemonsets:

```yaml
apiVersion: unbounded.azure.com/v1alpha1
kind: ArtifactStreamingProfile
metadata:
  name: ai-default
spec:
  scope:
    nodePools:
      - gpu
  oci:
    enabled: true
    artifactStreaming: Auto
    peerDistribution: Auto
  blobs:
    enabled: true
    sources:
      - azureBlob
      - modelHub
  cache:
    size: 2Ti
    eviction:
      protect:
        - modelWeights
        - activeImages
    isolation: WorkloadIdentity
```

The platform can translate that profile into the right components:

- Gantry and OverlayBD configuration for OCI images;
- node-local blob/model streaming endpoints for model assets;
- node-pool cache sizing and eviction policy; and
- future runtime cache hooks when they are ready.

For managed AI experiences, this profile can be generated and reconciled by the
service. Advanced customers can bring their own cache infrastructure or opt out
where the managed defaults do not fit.

## Architecture

```text
+-------------------- AI workload --------------------+
|                                                       |
|  containerd / OverlayBD      model loader / runtime   |
|           |                         |                 |
+-----------|-------------------------|-----------------+
            |                         |
            v                         v
   +----------------+        +-------------------------+
   | OCI streaming  |        | Blob/model streaming    |
   | endpoint       |        | endpoint                |
   | (Gantry)       |        | (DACS-style gateway)    |
   +-------+--------+        +-----------+-------------+
           |                             |
           v                             v
   +----------------+        +-------------------------+
   | Gantry peers   |        | Node-pool / regional    |
   | complete OCI   |        | chunk cache             |
   | layers         |        |                         |
   +-------+--------+        +-----------+-------------+
           |                             |
           v                             v
   +----------------+        +-------------------------+
   | ACR Artifact   |        | Blob / object / model   |
   | Streaming      |        | origins                 |
   +----------------+        +-------------------------+
```

The two data paths intentionally differ:

- OCI image data remains content-addressed by digest and verified by containerd.
- Blob/model data is keyed by source identity, version, byte range, and cache
  policy.
- Runtime-generated KV/context data remains opt-in and runtime-owned.

The shared layer is policy: which workloads, node pools, artifact classes, and
cache budgets are eligible for acceleration.

## Security and isolation

The integration must make cache boundaries explicit.

### Authorization

The cache must not make an artifact readable by a workload that could not read
it from origin.

For OCI images, peer-visible records should remain digest-keyed and should not
include bearer tokens, signed ACR data URLs, SAS query strings, or per-node
registry credentials.

For blob/model artifacts, cache keys must include the security boundary used for
the original read. Depending on the source, that may be tenant, subscription,
registry, storage account, workload identity, repository, or object version.

### Encryption and CMK

CMK-protected content must not be placed into a cache scope where another
identity can bypass the origin encryption policy. If the cache stores decrypted
bytes, cache access must be protected by an equivalent identity boundary and
encrypted at rest with a key appropriate for that boundary.

### Cache partitioning

Cache policy should support partitions by node pool, workload class, or artifact
class. GPU model weights should not be evicted by unrelated image pulls unless
the operator explicitly chooses a shared budget.

### Observability

Metrics should show hit rate, origin egress avoided, warmup time, eviction
pressure, and fallback reasons. They must not include signed URLs, tokens, SAS
parameters, or full private object paths.

## Phasing

### Phase 0: OCI design alignment

- Land the Gantry + ACR Artifact Streaming design.
- Confirm the OverlayBD range endpoint contract.
- Confirm fail-open behavior when the node-local Gantry endpoint is unavailable.
- Validate that complete-layer seeding does not block first container start.

### Phase 1: Managed OCI integration

- Add AKS/Unbounded deployment wiring for Gantry + OverlayBD on eligible node
  pools.
- Expose policy that lets managed AI experiences enable the OCI path without
  customer-managed daemonset installs.
- Add telemetry for origin fallback, peer range reads, and complete-layer
  convergence.

### Phase 2: Blob/model streaming profile

- Define the managed profile for non-OCI artifacts.
- Integrate a node-local blob/model endpoint with a DACS-style cache backend.
- Support cache partitioning and eviction controls for GPU model data.
- Document supported origins and identity boundaries.

### Phase 3: Unified AI artifact policy

- Present one customer-facing Artifact Streaming for AI profile across OCI and
  blob/model artifacts.
- Allow AI Manager or similar managed experiences to generate the profile by
  default.
- Add opt-out and bring-your-own-cache hooks for customers with private
  infrastructure.

### Phase 4: Runtime cache extensions

- Add optional hooks for KV and context cache snapshots when runtime
  requirements are ready.
- Keep runtime cache admission, privacy, freshness, and capacity controls
  separate from immutable artifact delivery.

## Open questions

- Which component owns the customer-facing `ArtifactStreamingProfile` API?
- Should the blob/model streaming data path live in Unbounded, DACS, AKS
  managed infrastructure, or a shared component consumed by all three?
- What is the minimum set of origins for the first model-weight streaming
  milestone?
- What cache partition model is required for CMK-protected registries and
  identity-scoped blobs?
- Should cache size be customer-visible for managed AI deployments, or should it
  be service-managed with telemetry-only visibility?
- How should customers bring their own private cache infrastructure while still
  using the managed profile?
- What is the right fallback behavior when a cache is present but overloaded:
  origin direct, throttled local read, or workload admission delay?

## Related work

- [Gantry with ACR Artifact Streaming](https://github.com/Azure/unbounded/pull/833)
- [Gantry detailed design](gantry-detailed-design.md)
- [Gantry and Unbounded integration](gantry-unbounded-integration.md)
- [OCI registry cache proxy design](oci-registry-high-level.md)
