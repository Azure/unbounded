# Gantry: unbounded-storage as High-Priority Origin

**Status:** Draft
**Date:** 2026-06-23
**Scope:** Add unbounded-storage as an optional, high-priority origin for gantry blob
fetches, accelerating cross-node image distribution via RDMA while keeping gantry
fully functional on clusters where unbounded-storage is not deployed.

---

## Problem Statement

Gantry currently fetches blobs it does not have locally by pulling directly from
the upstream OCI registry. This works correctly but does not leverage the
RDMA-accelerated P2P cache that unbounded-storage provides on Unbounded-managed
clusters. Blobs that are already resident in the cluster's unbounded-storage
cache are re-fetched over the internet instead of being served at local NVMe or
RDMA speeds.

Gantry is also deployed on plain Kubernetes clusters that have no unbounded-storage.
Any integration must be strictly opt-in and must not alter gantry's behavior on
those clusters in any way.

---

## Goals

- When unbounded-storage is deployed and configured, gantry fetches blobs from the
  local unbounded-storage HTTP frontend first. unbounded-storage's P2P RDMA fabric
  serves the bytes from NVMe cache or from a peer node.
- When unbounded-storage is absent (not configured, not deployed, temporarily
  unavailable), gantry falls through to the OCI registry transparently. No operator
  action, no error, no degraded state.
- The feature is off by default. An explicit `unbounded_storage.endpoint` in the
  gantry ConfigMap is the only way to enable it.
- The code path on non-unbounded clusters is byte-for-byte identical to the
  pre-integration baseline (no runtime overhead, no startup check, no health probe).

---

## Non-Goals

- Replacing containerd as gantry's local content store. Blobs are still committed
  to containerd after fetch; kubelet reads from containerd as today.
- Replacing gantry's libp2p DHT or peer transfer layer. unbounded-storage is used
  only on the origin fetch path.
- Making unbounded-storage understand the OCI Distribution Spec. unbounded-storage
  is treated as a plain HTTP cache keyed by URL path.
- Circuit breaker or availability tracking for unbounded-storage in v1. Fall-through
  on every failure is sufficient.
- Auth support for unbounded-storage's origin backend in v1. Private registries
  behind unbounded-storage are a follow-on concern; the cache path works for public
  registries and any registry for which unbounded-storage is separately configured
  with credentials.

---

## When unbounded-storage Is Consulted

The origin pull path is reached only when containerd does NOT have a blob locally.
The full sequence is:

1. kubelet requests an image pull; containerd's OCI mirror redirects to gantry at
   `http://127.0.0.1:5000`.
2. Gantry's mirror checks `LocalContentStore.Has(digest)` against containerd.
3. **Hit:** gantry streams from containerd. unbounded-storage is never consulted.
4. **Miss:** gantry runs the coord/please_pull flow to elect a puller, then the
   designated puller calls `OriginPuller.Pull`. This is the only entry point for
   the unbounded-storage path.

unbounded-storage accelerates the "cold fetch" - the first pull of a blob to a
node. Once the blob is committed to containerd, all subsequent serves bypass it.

---

## Design

### Data Flow

```
Pull(ctx, ref)
  ChainedPuller.Pull
    |
    +-- 1. unstore.Client.Pull
    |       GET http://<endpoint>/v2/<repo>/blobs/<digest>
    |       (or /manifests/<digest> for KindManifest)
    |       |
    |       +-- 200 OK  -> stream bytes to caller (from NVMe / RDMA peer)
    |       |
    |       +-- 404     -> OriginError{Class: FailureNotFound}
    |       +-- error   -> OriginError{Class: FailureTransient}
    |
    +-- 2. On any OriginError from step 1: log WARN once, fall through
    |
    +-- 3. origin.Client.Pull  (existing OCI registry path, unchanged)
```

For `Head`, the same two-step chain applies with an HTTP HEAD request to
unbounded-storage and the same fallback logic.

### New Packages

#### `internal/gantry/unstore/`

A new package. No dependency on `origin/`.

- `client.go` - `Client struct` implementing `ifaces.OriginPuller`:
  - `New(cfg *config.UnboundedStorageConfig, opts ...Option) *Client`
  - `Pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error)`
    - Constructs the URL: `<endpoint>/v2/<repository>/blobs/<digest>` for
      `KindBlob`/`KindConfig`, `<endpoint>/v2/<repository>/manifests/<digest>`
      for `KindManifest`.
    - Sends HTTP GET. On 200 returns the response body and `Content-Length`.
    - On 404 returns `*ifaces.OriginError{Class: FailureNotFound}`.
    - On any other error (connection refused, timeout, non-404/2xx status) returns
      `*ifaces.OriginError{Class: FailureTransient}`.
  - `Head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error)`
    - Sends HTTP HEAD to the same URL.
    - Returns `(size, Content-Type, nil)` on 200.
    - Returns `OriginError` on 404 or error, mirroring `Pull` semantics.
  - Optional functional options: `WithLogger`, `WithMetrics`.
  - Compile-time check: `var _ ifaces.OriginPuller = (*Client)(nil)`.

#### `internal/gantry/origin/chain.go`

A new file inside the existing `origin` package.

- `ChainedPuller struct` implementing `ifaces.OriginPuller`:
  - `NewChainedPuller(primary, fallback ifaces.OriginPuller) *ChainedPuller`
  - `Pull`: calls primary; on any `*ifaces.OriginError` (including `FailureNotFound`,
    `FailureTransient`, or any other class) logs WARN and calls fallback. Returns the
    fallback's result. Returns an error only when both fail (fallback's error is
    returned, not primary's, because fallback is the authoritative origin).
    Rationale: unbounded-storage is a cache, not an authority. Any error it returns -
    including auth or rate-limit signals that would never arise from a local HTTP
    daemon in a correctly deployed system - should not block the OCI registry path.
  - `Head`: same logic.
  - No metrics on the chained puller itself; metrics belong to `unstore.Client`
    (primary) and `origin.Client` (fallback).
  - Compile-time check: `var _ ifaces.OriginPuller = (*ChainedPuller)(nil)`.

### Config

New optional struct in `internal/gantry/config/config.go`:

```go
// UnboundedStorageConfig holds the optional connection parameters for using
// a local unbounded-storage daemon as a high-priority blob origin.
// When nil or Endpoint is empty, the unbounded-storage path is disabled
// and gantry uses the OCI registry directly.
type UnboundedStorageConfig struct {
    Endpoint string        `yaml:"endpoint"` // e.g. "http://127.0.0.1:8080"
    Timeout  time.Duration `yaml:"timeout"`  // per-request; 0 uses a 30s default
}
```

Added to `Config`:

```go
UnboundedStorage *UnboundedStorageConfig `yaml:"unbounded_storage"`
```

Validation (in `Validate()`):
- If `UnboundedStorage != nil` and `Endpoint` is non-empty: must start with
  `http://` or `https://`.
- If `UnboundedStorage` is nil or `Endpoint` is empty: no validation; feature is off.

**Config invariant:** When `unbounded_storage` is absent from the ConfigMap,
`Config.UnboundedStorage` is nil after parse. The `ChainedPuller` and
`unstore.Client` are never constructed. The `origin.Client` is wired directly,
identical to today. Zero runtime overhead.

### Wiring (`cmd/gantry/agent_origin.go`)

```
if cfg.UnboundedStorage != nil && cfg.UnboundedStorage.Endpoint != "" {
    unstoreClient = unstore.New(cfg.UnboundedStorage, unstore.WithLogger(logger), unstore.WithMetrics(...))
    puller = origin.NewChainedPuller(unstoreClient, ociPuller)
    mirror = origin.NewChainedPuller(unstoreClient, ociMirror)
} else {
    puller = ociPuller
    mirror = ociMirror
}
```

The two separate `origin.Client` instances (puller vs. mirror) are preserved. The
`ChainedPuller` wraps each independently so their metric hooks remain isolated.

### Metrics

New Prometheus counters on `unstore.Client`, populated via `WithMetrics`:

| Counter | Labels | Meaning |
|---|---|---|
| `gantry_unstore_pull_total` | `kind` | Total Pull attempts to unbounded-storage |
| `gantry_unstore_pull_hit_total` | `kind` | 200 OK responses (bytes from unbounded-storage) |
| `gantry_unstore_pull_miss_total` | `kind` | 404 responses (fell through to OCI registry) |
| `gantry_unstore_unavailable_total` | - | Transient/connection errors |

HEAD requests are not counted in pull arithmetic (consistent with the existing
`origin.Client` design - see `ifaces.OriginPuller.Head` doc comment).

Registered alongside the existing gantry Prometheus metrics in
`cmd/gantry/main.go`.

---

## Availability and Graceful Degradation

| Scenario | Behavior |
|---|---|
| `unbounded_storage` not in ConfigMap | Feature off; OCI registry path unchanged |
| unbounded-storage not deployed | Feature off (not configured) |
| unbounded-storage configured but unreachable | `unstore.Client.Pull` returns `FailureTransient`; `ChainedPuller` logs WARN and falls through to OCI registry |
| unbounded-storage returns 404 (blob not cached) | `unstore.Client.Pull` returns `FailureNotFound`; `ChainedPuller` falls through to OCI registry |
| unbounded-storage returns 200 | Blob served from cache; OCI registry not contacted |
| OCI registry unreachable (unbounded-storage also misses) | Standard error propagation, same as today |

The WARN log on fallback fires once per request, not per connection failure. A
chatty cluster where unbounded-storage is routinely cold will produce one WARN per
cache miss. Operators can monitor `gantry_unstore_pull_miss_total` and
`gantry_unstore_unavailable_total` for cache effectiveness and availability.

---

## Testing

### `internal/gantry/unstore/client_test.go`

Table-driven tests against `httptest.NewServer`:

- `Pull` on 200: returns body and correct content-length.
- `Pull` on 404: returns `*ifaces.OriginError{Class: FailureNotFound}`.
- `Pull` on connection refused: returns `*ifaces.OriginError{Class: FailureTransient}`.
- `Pull` URL construction: `KindBlob`/`KindConfig` use `/blobs/`, `KindManifest`
  uses `/manifests/`.
- `Head` on 200: returns size and Content-Type.
- `Head` on 404 and connection refused: same error classification as `Pull`.
- Timeout: a server that hangs respects the configured `Timeout`.

### `internal/gantry/origin/chain_test.go`

- Primary hit: fallback is never called; primary result is returned.
- Primary miss (`FailureNotFound`): fallback is called; fallback result returned.
- Primary transient (`FailureTransient`): fallback is called; fallback result returned.
- Both fail: fallback's error is returned (not primary's).
- `Head` variants of each case above.

### Existing tests

No changes to existing `origin/`, `mirror/`, `coord/`, or `coldstart/` tests.
When `UnboundedStorage` is nil, the code path is identical to today.

---

## Deployment Configuration

unbounded-storage must be configured with the OCI registries as its origin
backends. Gantry sends paths of the form `/v2/<repository>/blobs/<digest>`; on a
miss, unbounded-storage fetches from `<backend_url>/v2/<repository>/blobs/<digest>`.

Example gantry ConfigMap addition:

```yaml
unbounded_storage:
  endpoint: http://127.0.0.1:8080
  timeout: 30s
```

Example unbounded-storage config excerpt (operator's responsibility):

```toml
[[backends]]
name = "registry-k8s-io"
[backends.config.http]
url = "https://registry.k8s.io"

[[frontends]]
name = "local"
source = "registry-k8s-io"
[frontends.config.http]
addr = "127.0.0.1:8080"
```

---

## Alternatives Considered

### Extend `origin.Client` directly

Adding an unbounded-storage pre-check inside `origin.Client.pull()` would confine
the change to one package but conflates two very different concepts: OCI registry
token auth + endpoint routing vs. plain HTTP cache lookup. It would make
`origin.Client` harder to reason about and test. The `ChainedPuller` composition
is explicit, testable independently, and reusable for any future "try A then B"
origin strategy.

### OCI Distribution Spec proxy in unbounded-storage

Adding OCI-specific handling (auth token flow, media-type routing) to
unbounded-storage's frontend and pointing containerd's `hosts.toml` mirror
directly at it would remove gantry from the image-serving path entirely. This is
a larger architectural change with a different risk profile and is not in scope.

### Replace gantry P2P with unbounded-storage fabric

Using unbounded-storage's RDMA fabric for gantry's peer data movement (replacing
libp2p peer transfer) would yield larger performance gains but requires removing
gantry's `PeerDialer` interface and reworking the coord/please_pull coordination
model. Treating unbounded-storage as a high-priority origin achieves RDMA
acceleration for the dominant use case (first-time pulls) at a fraction of the
implementation scope.

---

## Open Questions

- Should `ChainedPuller` suppress WARN logs after N consecutive misses from the
  same unbounded-storage endpoint within a window, to avoid log noise during cold
  cluster startup?
- Should the unstore client expose a `Ping` method for gantry's readiness probe,
  or is the feature purely opportunistic (unavailability is not a readiness failure)?
- Is one shared `unstore.Client` instance sufficient for both the puller and mirror
  paths, or do they need separate HTTP transport pools (consistent with how two
  `origin.Client` instances are built today)?
