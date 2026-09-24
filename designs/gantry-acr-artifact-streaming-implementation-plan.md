# Gantry ACR Artifact Streaming Implementation Plan

**Status:** Draft for review

This plan implements the behavior described in
[Gantry with ACR Artifact Streaming](gantry-acr-artifact-streaming.md). The
design document is the source of truth for system behavior. This document maps
that behavior to code changes, tests, deployment work, rollout order, and
production acceptance criteria.

## Target behavior

Two independent paths run after Gantry serves a streaming manifest:

1. **Complete-layer seeding:** the existing manifest prefetch path sends
   layer-specific `please_pull` requests to Gantry chairs. Chairs download,
   verify, commit, and advertise complete layers.
2. **Range reads:** OverlayBD sends `Range: bytes=N-M` to a node-local Gantry
   endpoint. Gantry serves the range from a complete local layer, a complete
   peer layer, or the signed Azure origin URL.

The range path does not start, cancel, or own chair pulls. Manifest processing
remains the only owner of proactive complete-layer seeding.

```text
manifest -> existing Gantry prefetch -> chairs pull complete layers

OverlayBD range -> local complete blob
                -> complete peer provider
                -> signed origin URL
```

## Baseline implementation decisions

The plan uses the following baseline so the first implementation has one clear
contract:

- Keep existing manifest-triggered complete-layer prefetch unchanged.
- Add `GET /blobs/<resolved-origin-url>` to the existing node-local Gantry
  listener on port 5000.
- Accept one exact bounded range, `bytes=N-M`. Reject suffix, open-ended, and
  multipart ranges at the OverlayBD endpoint.
- Query only complete digest providers already advertised in the existing DHT.
- Use a short, bounded provider lookup. Do not wait for an in-progress chair
  pull before using the signed origin URL.
- Do not add range-keyed DHT records or range-specific chair RPCs.
- Do not cache partial origin ranges in the first implementation.
- Do not send signed URLs or OverlayBD registry authorization to peers.
- Keep complete content in containerd; do not add another complete-object
  store.
- Ship the feature disabled until the node's OverlayBD configuration is ready.

A transient range cache and cluster-wide range ownership remain possible later
optimizations. Neither is required for the design's local/peer/origin source
transition.

## Package boundaries

### New packages

| Package | Responsibility |
|---|---|
| `internal/gantry/httprange` | Parse exact request ranges and validate `Content-Range` responses. |
| `internal/gantry/streaming` | Handle OverlayBD requests, parse Azure data URLs, choose a source, and stream the response. |

`httprange` is shared by the streaming handler and peer transfer client. It
must not import Gantry networking or storage packages.

`streaming` depends on narrow interfaces for local range reads, DHT discovery,
peer range fetches, and signed-origin range fetches. It must not import the OCI
mirror package or the chair coordinator.

### Existing packages changed

| Package/file | Change |
|---|---|
| `internal/gantry/containerdstore` | Add an efficient complete-object range reader backed by containerd `ReaderAt`. |
| `internal/gantry/transfer/client.go` | Add exact bounded peer range requests. |
| `internal/gantry/transfer/transfer.go` | Keep the existing server wire contract; add conformance tests for exact ranges. |
| `internal/gantry/config/config.go` | Add feature, origin-policy, lookup, and concurrency configuration. |
| `internal/gantry/mirror/mirror.go` | Permit composition of the mirror handler with the streaming handler on one listener. Do not add streaming source logic here. |
| `cmd/gantry/main.go` | Construct and wire the streaming server, shared listener, metrics, and shutdown. |
| `cmd/gantry/agent_metrics.go` | Register bounded streaming metrics. |
| `deploy/gantry/chart` | Render configuration and optional OverlayBD node configuration. |

## Phase 0: Lock and test upstream contracts

Before production code changes, create redacted fixtures and tests for the
assumptions that join the four systems.

### ACR fixture capture

Using a test ACR streaming artifact, capture and redact:

- the tag-resolution response;
- the resolved streaming manifest and config;
- layer descriptor annotations, media types, digests, and sizes;
- the registry blob redirect shape;
- the signed data URL host/path/query shape; and
- representative OverlayBD ranges during attach and initial process startup.

Remove registry names, account identifiers, signatures, tokens, and expiry
values before committing fixtures.

### Contract tests

Add tests proving:

1. Containerd's remote snapshot annotations carry the original image
   reference and layer digest.
2. The OverlayBD snapshotter writes the expected `repoBlobUrl`, digest, and
   size without downloading the complete layer.
3. Gantry's existing `manifest.TypedChildren` classifies the streaming config
   and layers correctly.
4. `layerPrefetchAdapter.OnManifestServed` sends the streaming children through
   `PrefetchManifestChildren` with registry and repository intact.
5. OverlayBD sends the post-redirect URL to `p2pConfig.address` with an exact
   bounded range.
6. ACR Artifact Streaming uses redirect mode in supported environments. Direct
  registry `200` self mode is unsupported by this integration.

### Decision gates

The fixture run must settle these values before their defaults merge:

- peer lookup latency budget;
- maximum peer attempts per range;
- origin concurrency limit; and
- whether an OverlayBD request-size limit is necessary when responses are
  streamed without buffering.

**Phase exit:** the test fixtures reproduce the control flow in the design, and
the supported Azure URL/authentication contract is written down without live
credentials.

## Phase 1: Shared exact-range primitives

Add `internal/gantry/httprange` with a small immutable value type:

```go
type Range struct {
    Start int64
    End   int64 // inclusive
}
```

Required operations:

- parse exactly `bytes=N-M`;
- reject negative, reversed, overflowing, suffix, open-ended, and multipart
  ranges;
- compute length with overflow checks;
- validate a range against a known complete-object size;
- format the outbound `Range` header;
- parse and validate `Content-Range: bytes N-M/TOTAL`; and
- format local `206` and `416` headers.

Do not replace the existing OCI resume parser. Gantry's complete-object path
uses `bytes=N-` to resume interrupted transfers; OverlayBD uses `bytes=N-M`.
Keeping the types separate prevents bounded reads from accidentally becoming
full suffix downloads.

### Tests

Cover boundary values, integer overflow, zero-length objects, one-byte reads,
end-of-object reads, malformed whitespace, multiple ranges, and mismatched
`Content-Length`/`Content-Range`.

**Phase exit:** all range validation is centralized and neither streaming nor
transfer code parses range strings itself.

## Phase 2: Complete-object range access

### Local containerd ranges

Add a narrow production capability to `containerdstore.Store`, for example:

```go
OpenRange(ctx, digest, range) (body io.ReadCloser, totalSize int64, err error)
```

Implementation requirements:

- apply the configured containerd namespace;
- open the committed descriptor through `content.Store.ReaderAt`;
- validate the requested range against `ReaderAt.Size()`;
- return an `io.SectionReader` over only the requested bytes;
- preserve `ErrNotFound` versus `ErrUnavailable`; and
- count a local range hit independently from ordinary mirror cache hits.

Do not seek by reading and discarding a potentially large prefix. The
containerd backend already provides random access.

### Peer ranges

Add a separate exact-range method to `transfer.Client`, rather than changing
the semantics of `FetchFromPeer`:

```go
FetchRangeFromPeer(ctx, peerAddr, digest, range)
```

The method:

- builds the existing digest-addressed peer URL using the valid placeholder
  repository when no repository is needed;
- sets `Gantry-Mirrored: 1`;
- sends `Range: bytes=N-M`;
- does not send registry `Authorization`;
- requires `206`;
- validates start, end, total size, and content length; and
- returns existing typed not-found, unavailable, busy, and protocol errors so
  provider failure handling remains consistent.

The peer server already serves exact ranges from committed objects. Add tests
that pin this as a backward-compatible wire contract. A new Gantry requester
must be able to issue an exact range to an older peer that already has the
current transfer server behavior.

### Narrow interfaces

Define the peer and local range interfaces in `streaming`, or in a neutral
interface file if fakes are reused broadly. Do not add exact-range methods to
the existing `ifaces.PeerDialer` unless the ordinary mirror path needs them;
that would force unrelated fakes and call sites to implement unused behavior.

**Phase exit:** a unit/integration test reads the same byte range from a real
containerd store and from a Gantry transfer server and receives identical
bytes and metadata.

## Phase 3: Azure URL and signed-origin client

### Raw request preservation

OverlayBD constructs:

```text
http://localhost:5000/blobs/<full-resolved-url>
```

The embedded URL may contain repeated slashes, percent escapes, and a SAS query.
The handler must recover it from the raw request target. It must not call
`path.Clean`, round-trip through `url.Values.Encode`, or use a router that
redirects repeated slashes.

Add a test whose embedded URL contains:

- `https://`;
- a `//docker/registry/...` path segment;
- percent-encoded query values;
- `+`, `/`, and `=` inside a signature; and
- several query parameters in their original order.

The reconstructed origin URL must be byte-for-byte equivalent.

### Structured Azure URL parser

Implement structured parsing for the URL forms observed in Phase 0. At minimum,
the parser returns:

```text
origin URL
source digest
redacted host identity
origin class (ACR data, MCR data, Azure Blob)
```

Validation requirements:

- `https` only in production;
- no userinfo or fragment;
- no IP-literal host;
- host must match an explicitly configured exact host or suffix;
- digest must be SHA-256 with 64 lowercase hexadecimal characters;
- if the digest appears in more than one location, all values must match; and
- unsupported URL forms fail before any network request.

Do not copy Peerd's regular expressions as the validation boundary. Use
`net/url` plus host- and path-specific parsing.

### Signed-origin HTTP client

Add a streaming-specific HTTP client with bounded connect, TLS handshake, and
response-header timeouts. It should follow Gantry's existing origin transport
policy where applicable, but use a redirect callback that revalidates every
target against the artifact-streaming allowlist.

For each read:

- send the exact range;
- do not add Gantry registry credentials to a signed data URL;
- require a valid `206` response;
- validate `Content-Range` and `Content-Length` before committing response
  headers to OverlayBD;
- stream the body without buffering the full range;
- close the body on every error path; and
- preserve request cancellation when OverlayBD disconnects.

Direct registry `200` self mode is unsupported. The endpoint accepts only the
structured signed redirect URL forms covered by the fixtures and never forwards
inbound registry authorization to an origin or peer.

### Secret handling

No log, metric, trace attribute, error string, or panic may contain:

- the raw request URI;
- the full origin URL;
- a SAS query or signature;
- an Authorization header; or
- a redirect Location value.

Tests should install a log capture hook and assert a sentinel signature never
appears on success or failure.

**Phase exit:** a local Azure-origin fixture can serve exact ranges through the
client, while SSRF, redirect, malformed response, and secret-leak tests fail
closed.

## Phase 4: Gantry streaming server

Add `internal/gantry/streaming.Server` with these dependencies:

- complete local range store;
- digest-keyed DHT discovery;
- exact peer range client;
- signed-origin range client;
- bounded configuration; and
- metrics/logger hooks.

### Request flow

For each valid `GET /blobs/...` request:

1. Parse the exact range and Azure origin URL.
2. Try the complete local content store.
3. If local content is absent, perform one bounded complete-provider lookup.
4. Filter self and providers currently suppressed as stale, unavailable, busy,
   or suspicious.
5. Try providers within the configured attempt and wall-clock budgets.
6. If no provider serves the exact range, fetch it from the signed origin URL.
7. Return `206` with exact `Content-Range`, `Content-Length`,
   `Accept-Ranges: bytes`, and `application/octet-stream`.

Important ownership rules:

- The range handler does not call `ColdStartResolver.Resolve`.
- It does not issue `please_pull`.
- It does not wait through the mirror's multi-minute peer rediscovery budget.
- It does not write partial bytes into containerd.
- It does not advertise partial bytes.
- It checks local/DHT state again on every request, which provides the automatic
  transition to a completed chair provider.

### Error behavior

- Malformed request or unsupported URL: `400`.
- Syntactically valid but unsatisfiable range with known size: `416` and
  `Content-Range: bytes */TOTAL`.
- Local containerd unavailable: continue to peer/origin; record the storage
  failure separately.
- Peer not found, busy, stale, or unavailable: try the next provider within the
  request budget.
- Signed origin `401`/`403`: return an authorization failure without logging the
  URL.
- Signed origin `404`: return not found.
- Signed origin `429`: preserve a bounded `Retry-After` when valid.
- Origin protocol or transport failure: `502` or `503` according to whether the
  response was malformed or unavailable.
- Failure after response bytes started: terminate the body; do not append an
  HTTP error body to binary data.

### Resource controls

- Separate semaphore for signed-origin range requests so range traffic cannot
  consume every connection while chairs are performing complete pulls.
- Reuse the existing transfer serve cap for peer responses unless measurements
  show range reads require a distinct cap.
- Do not add node-local range singleflight until there is a buffering or fan-out
  design that preserves streaming and bounds memory. Measure duplicate ranges
  first.

### Tests

Use table-driven source-selection tests for:

- local hit;
- local miss and peer hit;
- first peer stale, second peer hit;
- peer busy then origin;
- no providers then origin;
- local storage unavailable then origin;
- chair provider appearing between two range requests; and
- origin failure after all complete providers fail.

Assert that peer calls receive only digest and range, never the signed URL.

**Phase exit:** the streaming server passes the source-transition test: request
one range from signed origin, mark a complete provider available, then request a
second range from the peer without restarting any component.

## Phase 5: Listener, lifecycle, configuration, and metrics

### HTTP listener composition

Keep the existing OCI mirror handler unchanged for `/v2` behavior. Compose the
shared port at the top level:

```text
/blobs/ -> streaming.Server
all else -> mirror.Server.Handler()
```

Do not put `/blobs/` behind `http.ServeMux` path cleaning. Use a small raw-prefix
dispatcher before the existing mirror mux so embedded `https://` and repeated
slashes remain intact.

Refactor `mirror.ListenAndServe` so the existing listener/shutdown helper can
serve a supplied composite handler. Retain the current wrapper for mirror unit
tests and callers that do not enable Artifact Streaming.

### Startup and shutdown

- Feature disabled: `/blobs/` is not registered and returns `404`.
- Feature enabled but Gantry not ready: return a retryable status without
  attempting origin.
- On drain, stop accepting new streaming requests and allow active bodies to
  finish within the existing HTTP shutdown budget.
- Add `streaming.Server.Drain` to the graceful shutdown sequence before closing
  the shared listener.
- Keep the peer transfer endpoint available while active streaming requests
  drain.

### Gantry configuration

Expose only the two decisions an operator actually owns:

```text
artifact_streaming_enabled
artifact_streaming_allowed_host_suffixes
```

The peer lookup budget, peer attempt count, origin read concurrency, and origin
response-header timeout are tuning values rather than operator policy: a wrong
value degrades cold-start latency in ways an operator cannot observe from the
outside. Keep them as defaults in `internal/gantry/streaming` beside the
behavior they govern and change them with a measurement, not a knob.

Validation must reject an enabled configuration with an empty allowlist or a
malformed host entry.

### Metrics

Add bounded metrics with no registry, repository, or URL labels:

```text
gantry_streaming_requests_total{source,outcome}
gantry_streaming_bytes_total{source}
gantry_streaming_request_duration_seconds{source,outcome}
gantry_streaming_time_to_first_byte_seconds{source}
gantry_streaming_rejected_total{reason}
gantry_streaming_inflight{source}
```

Use `source=local|peer|origin`. Materialize bounded label combinations at zero
like existing Gantry metrics. Track provider lookup outcomes with the existing
DHT metrics only if doing so does not mix OCI-mirror and streaming semantics;
otherwise add a streaming-specific bounded lookup counter.

### Chart changes

- Render the new settings in `deploy/gantry/chart/templates/configmap.yaml`.
- Add values to `deploy/gantry/chart/values.yaml` and the operator profile.
- Keep port 5000 and the current loopback `hostPort`; no new port is needed.
- Update the hardening NetworkPolicy example only if the source address seen
  after hostPort translation differs for OverlayBD and containerd.
- Add render tests proving the feature defaults off and enabled values survive
  Helm rendering.

**Phase exit:** `make gantry-manifests`, Helm lint, config tests, metrics tests,
and graceful shutdown tests pass with the feature disabled and enabled.

## Phase 6: Preserve and prove complete-layer seeding

No new chair protocol is planned. Add coverage around the existing path so
future streaming changes cannot accidentally disable Gantry's primary value.

### Tests

Using the redacted streaming manifest fixture, prove:

1. Gantry serves the manifest by digest.
2. `OnManifestServed` opens and parses it from containerd.
3. Every config/layer child has the expected kind.
4. Missing children are grouped by chair and dispatched through existing
   `please_pull` calls.
5. A completed chair pull commits to containerd and triggers advertisement.
6. The streaming handler's next request discovers that provider and stops using
   signed origin for that digest.

Do not add a persistent digest-to-repository catalog unless a demonstrated
failure requires range requests to restart complete pulls. In the current
design, manifest processing already owns complete pull dispatch and the range
request always has a signed fallback capability.

**Phase exit:** one integration test exercises both concurrent paths from a
single manifest: first range from origin, complete chair commit, second range
from peer.

## Phase 7: AKS OverlayBD configuration

AKS Artifact Streaming node pools already install the OverlayBD daemon and
snapshotter. Gantry must configure the existing daemon to use the local endpoint:

```json
{
  "p2pConfig": {
    "enable": true,
    "address": "http://localhost:5000/blobs"
  }
}
```

### Helm/operator resources

Add an explicitly enabled OverlayBD node-configuration workload modeled on the
upstream Peerd configurator:

- privileged access only where required to enter the host namespaces;
- no service-account token when Kubernetes API access is unnecessary;
- node selector/affinity limiting it to Artifact Streaming node pools;
- use the AKS-provided `/opt/acr/tools/overlaybd/config.sh` rather than editing
  JSON with string replacement;
- set only `p2pConfig` and leave every other OverlayBD setting exactly as the
  host had it;
- verify Gantry readiness before enabling the proxy;
- record the previous values for rollback;
- avoid repeated service restarts when the desired config is already present;
  and
- report readiness only after the host config and running service agree.

Expose values such as:

```text
gantry.artifactStreaming.enabled
overlaybdConfig.enabled
overlaybdConfig.nodeSelector
overlaybdConfig.address
```

The operator-managed profile renders the resource into its explicit manifest
allowlist, but reconciliation applies it only when
`spec.components.gantry.artifactStreaming.enabled` is true with a non-empty
`nodeSelector`. Disabled reconciliation explicitly deletes an older
configurator before applying Gantry without the streaming endpoint. Do not
silently deploy privileged host mutation merely because Gantry is enabled.

### Rollout order

1. Deploy Gantry with the streaming endpoint enabled and ready.
2. Configure OverlayBD node by node to call Gantry.
3. Validate a new streaming device on each updated node.
4. Roll back in reverse order: disable OverlayBD P2P first, then roll back
   Gantry.

**Phase exit:** a fresh AKS Artifact Streaming node can be configured and rolled
back declaratively, and reapplying identical desired state does not restart
OverlayBD services.

## Phase 8: End-to-end validation and operational release

### Kind contract tests

Kind does not need a real OverlayBD block device to validate Gantry's HTTP and
peer behavior. The `e2e/gantry` contract uses a strict range client, a
cluster-local TLS origin, and a complete blob inserted on a peer node to prove:

- raw signed URL preservation;
- origin range on a cold miss;
- exact peer range after a complete provider appears;
- stale/busy provider fallback;
- metrics; and
- secret-free logs and artifacts.

### AKS E2E

Use a Premium ACR, an Artifact Streaming-enabled node pool, and a real streaming
artifact. Add repository Make targets for all setup and benchmark operations.
The scenario must cover:

- first cold Pod with no complete provider;
- manifest-triggered complete chair pulls;
- transition from signed origin to peer ranges;
- multi-node scale-out;
- private ACR/private endpoint;
- chair failure;
- signed URL expiry;
- stale provider;
- Gantry rollout while a workload reads files continuously; and
- node reboot/replacement.

### Measurements

Report direct measurements separately from derived values:

- Pod readiness latency;
- first range time to first byte;
- streaming requests and bytes by local/peer/origin source;
- complete origin copies per digest;
- time from manifest observation to first complete provider;
- ACR data-plane bytes;
- peer bytes; and
- errors/retries during component restart.

Do not claim causal improvement unless the scenario holds image, node type,
cluster size, registry topology, and workload access pattern constant.

### Documentation and operations

Update:

- Gantry user guide and chart values;
- AKS installation and rollback instructions;
- Unbounded agent configuration reference;
- metrics and alerting reference;
- compatibility matrix for containerd, snapshotter, OverlayBD daemon, and
  kernel backend; and
- incident procedures for Gantry unavailable, ACR unavailable, stale provider,
  and OverlayBD device failures.

**Phase exit:** all acceptance criteria below pass on the supported production
environment and the rollback procedure has been exercised.

## Pull request sequence

Each item should be independently reviewable and preserve existing OCI pulls.

1. **Contract fixtures and tests**: redacted ACR manifest/URL fixtures and
   existing prefetch assertions.
2. **Exact-range primitives**: `httprange` package and tests.
3. **Complete-object range readers**: containerd range reader and exact peer
   client with transfer conformance tests.
4. **Azure signed-origin client**: structured URL parser, redirect policy,
   response validation, and secret-leak tests.
5. **Streaming server**: local/peer/origin source resolver and handler tests.
6. **Agent wiring**: listener composition, drain, config, metrics, and chart
   rendering. Feature remains disabled by default.
7. **Concurrent-path integration**: manifest `please_pull`, chair completion,
   provider advertisement, and source transition test.
8. **AKS node configurator**: opt-in host configuration, readiness, rollback,
   and chart/operator integration.
9. **Production E2E and operations**: AKS tests, measurements, dashboards,
    alerts, compatibility matrix, and runbooks.

PRs 2 through 7 can merge without enabling the feature. Production enablement
requires the opt-in AKS node configuration and end-to-end acceptance gates.

## Validation commands

Use the narrowest check after each change, then the full Gantry target before
merging.

```text
go test ./internal/gantry/httprange
go test ./internal/gantry/streaming
go test ./internal/gantry/transfer
go test ./internal/gantry/containerdstore
go test ./internal/gantry/config
go test ./cmd/gantry
make gantry-manifests
make gantry-chart-lint
make e2e-gantry
make gantry
```

## Rollout and rollback

### Rollout

1. Upgrade Gantry everywhere with Artifact Streaming disabled.
2. Verify existing OCI pulls, DHT, chairs, and transfer behavior are unchanged.
3. Enable the Gantry streaming endpoint.
4. Verify endpoint readiness on every target node.
5. Enable OverlayBD `p2pConfig` node by node.
6. Run a streaming smoke workload on each updated node group.
7. Expand only after origin/peer metrics and active filesystem reads are healthy.

Mixed Gantry versions are acceptable during step 1 because the existing peer
server already supports exact ranges; only upgraded requesters expose the local
OverlayBD endpoint.

### Rollback

1. Disable OverlayBD P2P configuration so new devices use direct origin.
2. Drain affected streaming workloads before removing the node-local endpoint.
3. Disable the Gantry streaming endpoint.
4. Roll back Gantry.

There is no content migration. Complete layers remain ordinary containerd
content and existing DHT records remain valid.

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Go path cleaning corrupts embedded URLs | Raw-prefix dispatch before `ServeMux`; byte-preservation tests. |
| Signed credentials leak into logs or peers | Redacted structured logging, no URL peer field, sentinel leak tests. |
| Arbitrary URL creates SSRF | HTTPS-only structured parser, explicit host suffixes, redirect revalidation. |
| First reads amplify origin traffic | Preserve bounded chair pulls; bound origin range concurrency; measure duplicates before adding range ownership. |
| Range reads starve complete pulls | Separate concurrency pools and metrics for range origin traffic and chair origin traffic. |
| Partial response is presented as valid | Strict `206`, `Content-Range`, length, and EOF validation. |
| Snapshot annotations are absent on transfer-service pulls | Explicit containerd configuration plus contract/readiness tests. |
| Host configuration disrupts active devices | Idempotent writes, controlled node rollout, no restart on no-op, exercised rollback. |
| Partial bytes are confused with complete providers | Never write partial ranges to containerd or advertise them in the complete-object DHT. |

## Acceptance criteria

### Functional

- A real ACR streaming Pod starts when no complete Gantry provider exists.
- Its first required range can be served from the signed origin URL.
- The same manifest triggers existing complete-layer chair pulls.
- After a chair advertises a layer, later ranges use the peer without ACR data
  traffic for those requests.
- Existing non-streaming OCI pulls behave unchanged.

### Correctness and security

- Every response contains exactly the requested bytes and valid range headers.
- Complete peer layers were committed and digest-verified by containerd.
- Partial origin ranges never produce complete provider records.
- Unsupported URLs and redirects fail closed.
- No signed URL, SAS value, or authorization value appears in logs, metrics,
  DHT records, peer requests, or test artifacts.

### Reliability

- Stale, busy, and unavailable peers fall back within the bounded request
  budget.
- Existing complete peers continue serving when ACR is unavailable.
- Controlled Gantry rollout and workload drain procedures complete without
  corrupting active reads.
- Rollback has been executed successfully.

### Operations

- Source-specific requests, bytes, latency, rejection, and in-flight metrics are
  available with bounded labels.
- Alerts and runbooks distinguish Gantry failure, peer exhaustion, signed-origin
  failure, and ACR failure.
- Supported component versions and kernel prerequisites are documented.

## Out of scope for the first implementation

- Partial-range DHT advertisements.
- Cluster-wide range ownership.
- Persistent partial-range caching.
- Changes to containerd's image pull algorithm.
- Changes to OverlayBD or the OverlayBD snapshotter.
- Provisioning OverlayBD on Unbounded-managed nodes.
- Replacing complete-layer chairs with on-demand range owners.