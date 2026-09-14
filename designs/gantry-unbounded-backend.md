# Gantry: Cache-Origin Chain

**Status:** Draft  
**Date:** 2026-06-23

---

## Problem

Gantry fetches blobs missing from containerd directly from the upstream OCI registry.
On Unbounded-managed clusters, blobs may already be resident in unbounded-storage's
RDMA-accelerated P2P cache, but gantry ignores it and re-fetches over the internet.

Gantry also runs on plain Kubernetes clusters without unbounded-storage. Any integration
must be strictly opt-in with zero behavioral change when not configured.

---

## Approach

Introduce a **priority chain** of cache origins that gantry consults before falling
through to the OCI registry on the "cold fetch" path - the one pull per blob per node
that reaches the origin. Once a blob is committed to containerd it is served locally
forever; the cache chain is never consulted again for that blob.

The chain is a simple ordered list: try each entry in turn, fall through to the OCI
registry on any miss or failure. The OCI registry is always the mandatory final
fallback and cannot be misconfigured away.

**unbounded-storage** is the first supported cache origin. It runs as a daemon on each
node and is consulted over loopback (HTTP on port 8080). Cache hits are served at
local NVMe or RDMA speeds. On a cache miss, unbounded-storage closes the TCP connection
without sending an HTTP response - gantry treats this as a clean miss and falls through.

---

## Goals

- Gantry tries configured cache origins in order before the OCI registry.
- Adding a future cache origin type requires only a new implementation and one config
  wiring change - no changes to the chain, config schema, or metrics.
- When no cache origins are configured, the code path is identical to today.
- Any cache origin failure or miss is transparent; gantry continues to function.

## Non-Goals

- Writing blobs into unbounded-storage. Gantry only reads from it; cache population
  is the operator's concern.
- OCI Distribution Spec support in unbounded-storage. It is treated as a plain HTTP
  cache keyed by URL path.
- Circuit breaker or availability tracking in v1. Fall-through on every failure is
  sufficient.
- Auth support for cache origin backends in v1.

---

## Design

### Data Flow

```
cold fetch (containerd miss)
  --> PriorityChain
        [1] unbounded-storage  -- hit: done; miss or error: next
        [2] ...future backends...
        [N] OCI registry       -- always the final fallback
```

### Components

**`internal/gantry/unstore/`** - protocol shim for unbounded-storage. Implements the
existing `OriginPuller` interface. Owns all wire quirks (notably: connection close
signals a miss, not 404) so nothing outside this package knows about them.

**`internal/gantry/origin/chain.go`** - `PriorityChain`, a thin ordered list of
`OriginPuller` implementations. No logic beyond iterating entries and calling the
fallback on miss. Each entry owns its own metrics.

### Config

```yaml
cache_origins:
  - type: unbounded-storage
    endpoint: http://127.0.0.1:8080
    timeout: 30s
```

An empty or absent `cache_origins` disables the feature entirely.

### Observability

Shared Prometheus counters per backend: pull attempts, hits, misses, transient errors.
All labeled with `backend` so a single dashboard query covers all cache origin types.

---

## Graceful Degradation

| Scenario | Behavior |
|---|---|
| `cache_origins` absent or empty | Feature off; OCI registry path unchanged |
| cache origin unreachable | WARN log; falls through to OCI registry |
| cache origin miss | Falls through to next entry or OCI registry |
| cache origin hit | Blob served; OCI registry not contacted |
| all cache origins miss and OCI also fails | Same error behavior as today |


