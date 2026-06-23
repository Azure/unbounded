# Gantry: Extensible Cache-Origin Chain

**Status:** Draft
**Date:** 2026-06-23
**Scope:** Add an extensible priority chain of cache origins that gantry consults
before falling through to the OCI registry. The first backend is unbounded-storage,
using its RDMA-accelerated P2P cache to serve blobs at NVMe/fabric speeds. The
chain is generic: adding a future backend requires only a new `OriginPuller`
implementation and one `case` in the wiring - no changes to the chain, config
schema, or metrics.

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

- When one or more cache origins are configured, gantry tries each in priority
  order before falling through to the OCI registry.
- unbounded-storage is the first supported cache origin type.
- Adding a future cache origin type requires implementing `ifaces.OriginPuller` and
  adding one `case` to the wiring loop. Config, chain, and metrics require no
  changes.
- When no cache origins are configured, the code path is byte-for-byte identical
  to the pre-integration baseline (no runtime overhead, no startup check, no
  health probe).
- Any cache origin failure (unavailable, miss) is transparent. Gantry continues
  to function using only the OCI registry.

---

## Non-Goals

- Replacing containerd as gantry's local content store. Blobs are still committed
  to containerd after fetch; kubelet reads from containerd as today.
- Replacing gantry's libp2p DHT or peer transfer layer. Cache origins are used
  only on the origin fetch path.
- Making unbounded-storage understand the OCI Distribution Spec. It is treated as
  a plain HTTP cache keyed by URL path.
- Circuit breaker or availability tracking for cache origins in v1. Fall-through
  on every failure is sufficient.
- Auth support for cache origin backends in v1.
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
3. **Hit:** gantry streams from containerd. Cache origins are never consulted.
4. **Miss:** gantry runs the coord/please_pull flow to elect a puller, then the
   designated puller calls `OriginPuller.Pull`. This is the only entry point for
   the cache origin chain.

Cache origins accelerate the "cold fetch" - the first pull of a blob to a node.
Once the blob is committed to containerd, all subsequent serves bypass them.

---

## Design

### Data Flow

```
Pull(ctx, ref)
  PriorityChain.Pull
    |
    +-- 1. cache_origins[0].Pull  (e.g. unstore.Client for unbounded-storage)
    |       GET http://<endpoint>/v2/<repo>/blobs/<digest>
    |       |
    |       +-- 200 OK  -> stream bytes to caller; chain stops here
    |       +-- any OriginError -> log WARN, advance to next entry
    |
    +-- 2. cache_origins[1].Pull  (future second cache origin, if configured)
    |       ...same pattern...
    |
    +-- N. origin.Client.Pull  (OCI registry; always the final fallback)
              returns result; chain stops here
```

`origin.Client` is not a member of the chain slice - it is the mandatory final
fallback, always called when all cache origins miss or fail. `PriorityChain`
holds `[]ifaces.OriginPuller` (cache origins only) and the fallback separately
so the invariant "OCI registry is always reachable" cannot be misconfigured away.

For `Head`, the same chain applies with HTTP HEAD and the same fallback logic.

### New Packages

#### `internal/gantry/unstore/`

A new package. No dependency on `origin/`. Implements `ifaces.OriginPuller`
backed by a local unbounded-storage HTTP frontend.

- `client.go` - `Client struct`:
  - `New(endpoint string, timeout time.Duration, opts ...Option) *Client`
  - `Pull(ctx context.Context, ref ifaces.OriginRef) (io.ReadCloser, int64, error)`
    - URL: `<endpoint>/v2/<repository>/blobs/<digest>` for `KindBlob`/`KindConfig`;
      `<endpoint>/v2/<repository>/manifests/<digest>` for `KindManifest`.
    - 200: returns body and `Content-Length`.
    - 404: returns `*ifaces.OriginError{Class: FailureNotFound}`.
    - Any other error (connection refused, timeout, non-404/2xx): returns
      `*ifaces.OriginError{Class: FailureTransient}`.
  - `Head(ctx context.Context, ref ifaces.OriginRef) (int64, string, error)`
    - Same URL construction. Returns `(size, Content-Type, nil)` on 200.
    - 404 and errors: same classification as `Pull`.
  - Optional functional options: `WithLogger`, `WithMetrics(backend string)`.
    The `backend` string is set to the `Type` from config and becomes the label
    value on the shared metric counters.
  - Compile-time check: `var _ ifaces.OriginPuller = (*Client)(nil)`.

#### `internal/gantry/origin/chain.go`

A new file in the existing `origin` package.

- `PriorityChain struct` implementing `ifaces.OriginPuller`:
  - `NewPriorityChain(chain []ifaces.OriginPuller, fallback ifaces.OriginPuller, logger *slog.Logger) *PriorityChain`
  - `Pull`: iterates `chain` in order; on any `*ifaces.OriginError` from an entry,
    logs WARN (including which backend and the error class) and advances. If all
    chain entries fail or chain is empty, calls `fallback`. Returns `fallback`'s
    result. Only if `fallback` also fails is an error returned - and that error is
    `fallback`'s, not a chain entry's.
  - `Head`: same iteration and fallback logic.
  - When `chain` is empty, `PriorityChain` is a thin pass-through to `fallback`
    (zero overhead beyond one slice-length check).
  - No metrics on `PriorityChain` itself. Each cache origin implementation owns
    its own counters via `WithMetrics`.
  - Compile-time check: `var _ ifaces.OriginPuller = (*PriorityChain)(nil)`.

**Adding a future backend:** implement `ifaces.OriginPuller`, add one `case` to
the wiring loop in `agent_origin.go`. `PriorityChain`, config schema, and metrics
require no changes.

### Config

New list-based config in `internal/gantry/config/config.go`:

```go
// CacheOriginSpec configures one cache origin in the priority chain.
// Gantry tries cache origins in the order they appear in the list before
// falling through to the OCI registry. An empty list disables the feature.
type CacheOriginSpec struct {
    // Type identifies the backend implementation. "unbounded-storage" is
    // the only supported value; future types are added here.
    Type     string        `yaml:"type"`
    Endpoint string        `yaml:"endpoint"` // e.g. "http://127.0.0.1:8080"
    Timeout  time.Duration `yaml:"timeout"`  // per-request; 0 uses 30s default
}
```

Added to `Config`:

```go
CacheOrigins []CacheOriginSpec `yaml:"cache_origins"`
```

Validation (in `Validate()`):
- Each entry: `Type` must be a known value (`"unbounded-storage"`; list extends as
  types are added). `Endpoint` must start with `http://` or `https://`.
- Duplicate `Type` values in the same list are allowed (two endpoints of the same
  type, tried in order).

**Config invariant:** When `cache_origins` is absent or empty, `cfg.CacheOrigins`
is nil/empty after parse. `PriorityChain` is never constructed. `origin.Client`
is wired directly. Zero runtime overhead. The code path is identical to the
pre-integration baseline.

### Wiring (`cmd/gantry/agent_origin.go`)

```go
var cacheChain []ifaces.OriginPuller
for _, spec := range cfg.CacheOrigins {
    switch spec.Type {
    case "unbounded-storage":
        c := unstore.New(spec.Endpoint, spec.Timeout,
            unstore.WithLogger(logger),
            unstore.WithMetrics(spec.Type, inst),
        )
        cacheChain = append(cacheChain, c)
    // future types: add a case here
    }
}

if len(cacheChain) > 0 {
    puller = origin.NewPriorityChain(cacheChain, ociPuller, logger)
    mirror = origin.NewPriorityChain(cacheChain, ociMirror, logger)
} else {
    puller = ociPuller
    mirror = ociMirror
}
```

The two separate `origin.Client` instances (puller vs. mirror) are preserved as
the fallback for each chain so their metric hooks remain isolated.

### Metrics

Shared counter family on all cache origin implementations, keyed by `backend`
label (set to `CacheOriginSpec.Type`):

| Counter | Labels | Meaning |
|---|---|---|
| `gantry_origin_cache_pull_total` | `backend`, `kind` | Pull attempts to this cache origin |
| `gantry_origin_cache_hit_total` | `backend`, `kind` | 200 OK responses (bytes served) |
| `gantry_origin_cache_miss_total` | `backend`, `kind` | 404 responses (fell through) |
| `gantry_origin_cache_unavailable_total` | `backend` | Transient/connection errors |

HEAD requests are not counted in pull arithmetic (consistent with the existing
`origin.Client` design - see `ifaces.OriginPuller.Head` doc comment).

`unstore.WithMetrics(backend string, inst *phase1Metrics)` receives the registered
counters from `cmd/gantry/main.go` and closes over the `backend` label value.

---

## Availability and Graceful Degradation

| Scenario | Behavior |
|---|---|
| `cache_origins` absent or empty | Feature off; OCI registry path unchanged |
| cache origin not deployed | Feature off (not configured) |
| cache origin configured but unreachable | Returns `FailureTransient`; `PriorityChain` logs WARN and tries next / falls through to OCI registry |
| cache origin returns 404 (blob not cached) | Returns `FailureNotFound`; `PriorityChain` tries next / falls through to OCI registry |
| cache origin returns 200 | Blob served; remaining chain entries and OCI registry not contacted |
| all cache origins miss, OCI registry also fails | Standard error propagation, same as today |

The WARN log on fallback fires once per request, not per connection failure.
Operators can monitor `gantry_origin_cache_miss_total` and
`gantry_origin_cache_unavailable_total` per backend for cache effectiveness and
availability.

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

- Empty chain: calls fallback directly; zero WARN logs.
- First entry hit: fallback and remaining entries never called; first entry's
  result returned.
- First entry miss (`FailureNotFound`): second entry tried; on hit, fallback not
  called.
- All entries fail: fallback called; fallback's result returned (not any entry's
  error).
- `Head` variants of each case above.

### `internal/gantry/config/config_test.go`

- Unknown `type` in `cache_origins` entry fails validation.
- Invalid `endpoint` (missing scheme) fails validation.
- Empty `cache_origins` passes validation; feature is off.
- Valid `unbounded-storage` entry passes validation.

### Existing tests

No changes to existing `origin/`, `mirror/`, `coord/`, or `coldstart/` tests.
When `cache_origins` is empty, the code path is identical to today.

---

## Deployment Configuration

unbounded-storage must be configured with the OCI registries as its origin
backends. Gantry sends paths of the form `/v2/<repository>/blobs/<digest>`; on a
miss, unbounded-storage fetches from `<backend_url>/v2/<repository>/blobs/<digest>`.

Example gantry ConfigMap addition:

```yaml
cache_origins:
  - type: unbounded-storage
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

## Extensibility Contract

Adding a new cache origin type in future:

1. Create `internal/gantry/<newtype>/client.go` implementing `ifaces.OriginPuller`.
2. Add the type's config fields to `CacheOriginSpec` (or as a nested struct if the
   type needs more than `endpoint`/`timeout`).
3. Add one `case "<newtype>":` to the loop in `cmd/gantry/agent_origin.go`.
4. Add `"<newtype>"` to the validation allowlist in `config.Validate()`.

`PriorityChain`, the metrics counter family, and the fallback wiring require zero
changes.

---

## Alternatives Considered

### Two-slot `ChainedPuller` instead of slice-based `PriorityChain`

A two-slot `ChainedPuller{primary, fallback ifaces.OriginPuller}` is simpler to
implement initially but forces awkward nesting for a third backend and requires
a breaking API change to add the slice. The slice-based `PriorityChain` is the
same complexity for the one-backend case (the slice has one entry) and requires
no changes for future backends.

### Per-backend metric counter families

Using `gantry_unstore_pull_total` (backend-specific names) instead of
`gantry_origin_cache_pull_total{backend="unbounded-storage"}` means every new
backend adds a new counter family that operators must add to dashboards and
alerts. A shared family with a `backend` label lets dashboards query across all
cache origins with one PromQL selector.

### OCI Distribution Spec proxy in unbounded-storage

Adding OCI-specific handling to unbounded-storage's frontend and pointing
containerd's `hosts.toml` mirror at it would remove gantry from the
image-serving path. This is a larger architectural change and is not in scope.

### Replace gantry P2P with unbounded-storage fabric

Replacing gantry's `PeerDialer` with unbounded-storage's RDMA fabric would yield
larger gains but requires reworking the coord/please_pull model. Treating
unbounded-storage as a high-priority origin achieves RDMA acceleration for the
dominant use case at a fraction of the scope.

---

## Open Questions

- Should `PriorityChain` suppress WARN logs after N consecutive misses from the
  same backend within a window, to avoid log noise during cold cluster startup?
- Should cache origin clients expose a `Ping` method for gantry's readiness probe,
  or is the feature purely opportunistic (unavailability is not a readiness failure)?
- Is one shared `unstore.Client` instance sufficient for both the puller and mirror
  paths, or should the wiring build two instances to match the two `origin.Client`
  instances today?

