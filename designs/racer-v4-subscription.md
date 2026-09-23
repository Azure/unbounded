# Racer v4 subscription contract

The dataplane uses mutually authenticated `GET /v4/config`, with any deployment
query parameters preserved. Enrollment and fresh TLS proofs remain `/v3/enroll`
and `/v3/proof`. The control endpoint must not emit legacy `ControlCommand`.

## Request

- `Accept: application/x-protobuf`
- `Prefer: wait=28` (server holds unchanged state for 27-30 seconds)
- `Content-Length: 0`
- `X-Racer-Boot`: 64 lowercase hex characters, process incarnation
- `X-Racer-Profile: 1`
- `X-Racer-Cursor`: opaque last-received cursor, initially empty
- `X-Racer-Applied-Revision`: decimal locally committed revision, initially `0`
- `X-Racer-Applied-Digest`: lowercase SHA-256 hex of the applied protobuf Snapshot,
  initially empty
- `X-Racer-Rejected-Revision`: decimal validation-rejected revision, or `0`
- `X-Racer-Local-State`: `preparing`, `committing`, `applied`, or `failed`
- `X-Racer-Worker-Healthy`: `1` or `0`
- `If-None-Match`: last successfully accepted response ETag, when supplied

The cursor controls whether desired content changed. Applied revision/digest and
local state are feedback only, never a condition for releasing another node's
desired state. Receipt advances the cursor after identity and cursor validation,
even if snapshot validation or local preparation fails. A worker preparation
failure reports `failed` with the old applied revision; its desired cursor stays
current, and preparation retries locally. Revisions may be skipped.

Existing independent storage report headers remain:

- `X-Racer-Storage-Policy: 1`
- `X-Racer-Storage-Shards`: decimal process-local shard count
- `X-Racer-Storage-Identity`: 64 lowercase hex characters
- `X-Racer-Storage-Version`: decimal offered version
- `X-Racer-Storage-State`: `pending`, `applied`, or `failed`
- `X-Racer-Storage-Applied-Bytes`: decimal actual capacity
- `X-Racer-Storage-Error`: optional hex-encoded bounded error

Storage identity/version/state/applied bytes are omitted before an offer exists.
Existing credential report headers remain `X-Racer-Trust-Generation`,
`X-Racer-Trust-Digest`, `X-Racer-Certificate-Issuer`, and
`X-Racer-Old-Connections`. Normal control reports are not fresh TLS proofs.
No remote phase or forward-eligibility headers are sent.

## Response and timing

`200` carries `racer.control.v1.DesiredState` from `api/racer/control.proto`:
universe=1, node=2, incarnation=3, snapshot_digest=4, revision=5,
configuration=6, profile=7, pod_uid=8, storage_policy=9, cursor=10.
`configuration` contains the complete Snapshot; its revision and SHA-256 digest
must match the envelope. Cursor is a nonempty, at-most-1024-byte ASCII graphic
string, safe to return in a header. It must cover changes to either desired
topology or desired storage. The identity fields bind the authenticated process.

Use explicit `Content-Length`; chunked transfer encoding is unsupported. A held
unchanged response can be `204 Content-Length: 0`, or `304` when an ETag was
sent. ETag is optional; it does not replace the cursor. Keep-alive is supported,
with one outstanding request per connection. There is no success sleep.

The client permits 35 seconds to first byte, then at most 10 seconds total
transfer and 2 seconds idle between fragments. A change to local applied state,
storage feedback, health, or credential reports cancels the held request within
the 100 ms receive-check interval and reconnects immediately. Connection setup
has its own bounded timeouts. Connection rotation is jittered over 240-300
seconds and leaves enough lifetime for a full response.

Successful control observations refresh operational storage freshness for 75
seconds without acknowledging storage application. Fresh TLS proofs have an
independent five-minute security lifetime. Successful proof refresh is jittered
over 180-240 seconds. Worker installation changes and old-connection drains
unpark the credential manager immediately; proof scheduling runs even when
projection validation or reenrollment fails. Failed proofs retry after five
seconds, while a changed report triggers an immediate fresh attempt.

Local preparation retains the last working generation on failure. Once all local
workers are prepared, commit grants cannot be revoked by a newer publication;
only the newest pending successor is retained until that local commit completes.
Old work drains locally and never blocks publication to other nodes.

## Dataplane integration verification

The Rust dataplane has one local application coordinator for file and HTTP
sources. Remote phase commands and forward grants have been removed, including
their superseded Rust tests. The protobuf `ControlCommand` remains only for the
Go reference protocol until Go retirement.

Coverage migration retains local all-worker commit, duplicate/stale/foreign
acknowledgment fencing, failure recovery, and real-thread publication exclusion.
Additional real-worker tests cover superseded preparation, storage fencing,
last-working listeners, skipped revisions, and local draining. Local mTLS servers
cover received-versus-applied state, successful 204 freshness, report cancellation,
and immediate installation/drain proofs during invalid trust projection.

Full verification uses a short workspace TMPDIR because Unix socket paths have
a 107-byte limit:

```sh
timeout 240s env TMPDIR=/home/azureuser/code/unbounded/tmp RACER_REQUIRE_URING=1 RUST_TEST_THREADS=2 cargo test --locked --manifest-path cmd/racer-dataplane/Cargo.toml --all-targets
timeout 180s env TMPDIR=/home/azureuser/code/unbounded/tmp RACER_REQUIRE_URING=1 RUST_TEST_THREADS=2 cargo test --locked --manifest-path cmd/racer-dataplane/Cargo.toml --doc
```

Results: 330 library tests and 14 binary tests passed, with 30 explicitly ignored
entries; all 73 doctests passed. Twenty-one ignored entries are subprocess
helpers exercised through parent tests. The nine remaining opt-in entries are
the allocator benchmark, three Go snapshot export tests, two RDMA hardware or
Soft-RoCE tests, two NUMA placement tests, and the buffered-cache measurement.
HTTP A-B-A real-handler termination, RDMA wire/fallback budgets, the formerly
failing control reload test, and owner-failure attribution all passed. A separate
visible-output KeyUpdate test confirmed actual TX/RX kTLS on this host.
