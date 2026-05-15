<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Orca Integration Tests

In-process integration tests for the Orca origin cache. The harness
brings up real LocalStack and Azurite containers via
`testcontainers-go` and constructs N in-process `*app.App` instances
wired to those containers. No Kubernetes cluster is required.

For the Kubernetes-flavored deployment validation harness (Kind +
manifests + headless DNS), see [dev-harness.md](./dev-harness.md). The
two harnesses are complementary: the integration tests cover Go-level
behavior (origin, cachestore, fetch coordinator, cluster routing,
internal-fill RPC); the dev harness covers the manifest + deployment
shape.

## Prerequisites

- Docker (or any `DOCKER_HOST`-compatible daemon) reachable from the
  test process. `testcontainers-go` discovers it via `DOCKER_HOST`,
  `~/.docker/`, or the standard socket location.
- `gcc` for `-race` (CGO is required by Go's race detector). On
  GitHub-hosted Ubuntu runners this is preinstalled. Locally without
  `gcc`, the Makefile target drops `-race` automatically.

## Running

```sh
make orca-inttest
```

Equivalent to:

```sh
go test -tags=integrationtest -timeout 15m ./internal/orca/inttest/...
# CI also adds -race
```

First run pulls `localstack/localstack:3.8` (~700 MB) and
`mcr.microsoft.com/azure-storage/azurite:3.34.0` (~150 MB). Subsequent
runs reuse the cached images. Total run time on a warm runner is on
the order of 25-30 seconds for the entire suite (most of which is
streaming the 64 MiB multi-chunk blob through the full origin ->
fetch coordinator -> cachestore pipeline).

## Topology

Every test (except the lifecycle tests) runs against a 3-replica
in-process cluster, matching the production `deploy/orca` topology.
All replicas bind to `127.0.0.1` with distinct OS-assigned internal
ports. Each replica owns its own `StaticPeerSource` so tests can
mutate one replica's view of the cluster independently.

```
                  ┌──────────────────────────────────────┐
                  │           Test Process               │
                  │                                      │
   ┌─────────┐    │  ┌──────────┐    ┌───────────────┐   │
   │ Test t  │────┼─▶│  Client  │───▶│ Replica 1     │   │
   └─────────┘    │  │ (HTTP)   │    │ 127.0.0.1:e1  │   │
                  │  └──────────┘    │ internal :i1  │   │
                  │                  └───────┬───────┘   │
                  │  ┌─────────────┐         │ peers     │
                  │  │ Per-replica │◀────────┤ via       │
                  │  │ Static      │         │ static    │
                  │  │ PeerSources │         │ source    │
                  │  └─────────────┘         │           │
                  │                  ┌───────▼───────┐   │
                  │                  │ Replica 2     │   │
                  │                  │ 127.0.0.1:e2  │   │
                  │                  │ internal :i2  │   │
                  │                  └───────┬───────┘   │
                  │                  ┌───────▼───────┐   │
                  │                  │ Replica 3     │   │
                  │                  │ 127.0.0.1:e3  │   │
                  │                  │ internal :i3  │   │
                  │                  └───────┬───────┘   │
                  └──────────────────────────┼───────────┘
                                             │
                          ┌──────────────────┴───────────┐
                          ▼                              ▼
                  ┌────────────────┐            ┌────────────┐
                  │  LocalStack    │            │  Azurite   │
                  │  (origin S3 +  │            │  (origin   │
                  │   cachestore)  │            │   blob)    │
                  └────────────────┘            └────────────┘
```

## File layout

```
internal/orca/inttest/
├── doc.go              package overview, build tag, TODOs
├── images.go           pinned container image tags + Azurite dev creds
├── localstack.go       testcontainers wrapper + S3 helpers
├── azurite.go          testcontainers wrapper + azblob helpers
├── seed.go             SmallBlob/MediumBlob/LargeBlob + SeedS3/SeedAzure
├── peersource.go       StaticPeerSource (cluster.PeerSource impl)
├── harness.go          StartCluster orchestrator
├── client.go           typed HTTP helpers (Get / GetRange / Head / List)
├── originwrap.go       CountingOrigin decorator
├── internalwrap.go     CountingInternalHandlerWrap (per-IP status counts)
├── origins_test.go     origin builder helpers
├── main_test.go        TestMain (shared LocalStack + Azurite)
├── e2e_test.go         canonical 3-replica end-to-end suite
└── azure_test.go       azureblob origin smoke (3 replicas)
```

Driver-level branch coverage (versioning gate, blob-type rejection)
lives as fast unit tests in the respective driver packages
(`internal/orca/cachestore/s3`, `internal/orca/origin/azureblob`),
not here. Those tests run as part of `go test ./...` and cover all
state branches (empty / Enabled / Suspended versioning;
BlockBlob / PageBlob / AppendBlob / nil / disabled).

## Test inventory

The integration suite contains **7 tests** focused exclusively on
behavior that requires real LocalStack/Azurite + a real cluster of
in-process orca instances. Driver-level branch coverage (versioning
gate, blob-type rejection, HTTP error mapping, range parsing, chunk
arithmetic, config env-var fallback, manifest YAML validity) lives as
fast unit tests in the respective packages and runs as part of
`make test`.

### `e2e_test.go` (3-replica default)

Tests that exercise chunk fetching naturally exercise both the
local-fill path (when self happens to win rendezvous for a chunk) and
the cross-replica `/internal/fill` path (when a peer wins).

- `TestColdAndWarmGet` - cold + warm, warm phase deletes origin
  object first to prove cache hit.
- `TestRangedGet` - within-chunk and cross-chunk byte ranges plus
  several boundary edge cases against a 64-chunk blob (range starts
  exactly at a boundary, ends exactly at a boundary, covers
  contiguous full chunks, straddles 5 consecutive boundaries).
- `TestMultiChunkGet` - 64 MiB / 64 chunks, byte-exact full GET. With
  3 replicas, statistically every replica is the coordinator for
  many chunks, exercising both fillLocal and FillFromPeer paths.
- `TestRendezvousCoordinatorRouting` - GET against a non-coordinator
  routes through `/internal/fill`; `CountingOrigin` confirms exactly
  one origin GetRange happened cluster-wide.
- `TestSingleflightCollapse` - 3 concurrent GETs from 3 replicas for
  the same 64-chunk blob collapse to >= 64 (and <= 76) origin
  GetRanges, proving cluster-wide singleflight is genuinely deduping.
- `TestPeerNotCoordinatorFallback` - real membership-disagreement
  test. Crafts a phantom peer whose rendezvous score beats the
  coord's for k, mutates the coord's `StaticPeerSource` to include
  the phantom, GET via a non-coord replica that still views the real
  coord as coordinator, asserts (a) byte-exact body and (b)
  `counter409.Count(coord) >= 1` proving the 409 fallback fired.

### `azure_test.go` (3-replica default)

- `TestAzureBlobOrigin_ColdGet` - the `azureblob` driver works
  end-to-end against Azurite for a 2-chunk block blob.

### Where the dropped scenarios moved

| Dropped from integration | Lives now as |
|---|---|
| `TestBootSelfTest_Pass` | implicit in every other `StartCluster` test (boots through the same `app.Start` path) |
| `TestNotFound` | `internal/orca/server.TestWriteOriginError` (covers all 5 error mappings) |
| `TestList` | `internal/orca/server.TestHandleList` (covers normal/empty/truncated/error) |
| `TestHead` | `internal/orca/server.TestHandleHead` (covers normal/missing-fields/404) |
| `TestVersionedCachestoreBucketRefused` | `internal/orca/cachestore/s3.TestValidateBucketVersioning` (covers all 3 statuses) |
| `TestAzureUnsupportedBlobType` | `internal/orca/origin/azureblob.TestValidateBlobType` (covers all 5 cases) |

## Production-code seams used

The harness depends on three test-friendly seams in production code:

1. **`cluster.PeerSource`**: replaces the entire peer-discovery
   mechanism. Production constructs a DNS-backed source implicitly
   from `cfg.Cluster.Service` + `net.DefaultResolver`. Tests inject
   per-replica `StaticPeerSource` instances with explicit ports so
   multiple replicas can share an IP.

2. **`cluster.Peer.Port`**: zero in production (peer addressed on
   `cfg.Cluster.InternalListen` port); set in tests so `FillFromPeer`
   dials each peer's distinct port.

3. **`internal/orca/app.Start(ctx, *config.Config, ...Option)`**:
   programmatic factory wiring origin / cachestore / cluster / fetch
   coordinator / edge + internal listeners. Options:
   - `WithLogger`, `WithResolver`, `WithPeerSource`,
   - `WithOrigin`, `WithCacheStore`, `WithSkipCachestoreSelfTest`,
   - `WithInternalHandlerWrap` for the 409 counter.

Production goes through none of these.

## Adding a scenario

1. Pick the right entry point:
   - 3-replica e2e (most cases): `StartCluster(ctx, t, opts)`.
   - Driver-level branch coverage (versioning gate, blob-type
     rejection, etc.): write a unit test in the driver's package
     against the extracted pure helpers (`validateBucketVersioning`,
     `validateBlobType`).
2. Seed the origin: `SeedS3` or `SeedAzure`.
3. Issue requests via `cl.Get(i).HTTP.Get / GetRange / Head / List`.
4. Assert byte-exact body, status code, and (where relevant) origin
   RPC counts via `CountingOrigin` (`opts.OriginOverride`) or peer
   409 counts via `CountingInternalHandlerWrap`
   (`opts.InternalHandlerWrap`).

## Future work

Tracked in `doc.go` TODOs:

- `TestEtagChange` (mid-fill mutation): requires a deterministic test
  seam in `fetch.Coordinator` to pause between chunk fetches.
- Fault-injection origin / cachestore decorators: timeout, throttle,
  5xx retry-budget assertions.
