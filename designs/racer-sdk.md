# Racer Go SDK

## Scope and evidence

Approved design for a standard-library-only `pkg/racersdk`: a concurrent streaming
client and a single-callback origin server. This document and the normative
[client/origin v1 contract](../cmd/racer-dataplane/CLIENT_ORIGIN_API.md) define the
contract. Step 2 implements validated types and private protocol helpers; client
and origin lifecycles follow in step 3. SDK packages must not import `cmd/` packages.

Evidence read in implementation, test, then prose order (Rust paths below are
relative to `cmd/racer-dataplane/`):

- `src/model/identity.rs:13-24` defines a 32-byte key and private strong tag, but
  tag parsing is a stub. `src/model/range.rs:6-19,29-39` defines 16 MiB pages and
  closed/open/suffix shapes; normalization and page slicing are stubs.
- `src/client/request.rs:16-34` has HEAD/bootstrap/pinned request shapes, not a
  parser. `src/origin/client.rs:46-64` and `src/origin/page.rs:14-19` do not fetch or
  validate bytes. `src/error.rs:43-50` makes these boundaries fail explicitly.
- `src/read/range_stream.rs:15-30,46-63` carries a bounded window and metadata but
  cannot stream yet. Delegating page scheduling to it is intended architecture,
  not a measured property of a working dataplane.
- `src/app.rs:386-418` tests dependency composition and explicitly asserts an
  unimplemented poll. The client, origin, and range inline test modules contain
  only comments (`src/client/request.rs:36-38`, `src/origin/page.rs:21-23`,
  `src/model/range.rs:42-45`); they enforce no wire behavior today.
- `README.md:16-19,34-48,61-66` describes this scaffold and previously unspecified
  wire details. The v1 contract fills in client/origin details only. The original
  read-only `tmp/design.md:7-9` (outside this worktree) sketches client pools for
  concurrent pages. The approved SDK design supersedes that sketch: concurrency
  across Values is in the SDK, bounded page concurrency within a range is in Racer.
  The original sketch is not a build input and need not be copied into the repo.

## Minimal public surface

These are the target public signatures and semantic constraints. Step 2 supplies
the value types; the client and origin entry points are still pending:

```go
type Key [32]byte
type ByteLength uint64
type ByteOffset uint64

type Metadata struct {
    Size      ByteLength
    ETag      ETag
    ExpiresAt time.Time
}

type Request struct {
    Key     Key
    Context FetchContext
}

func NewClient(config ClientConfig) (*Client, error)
func (c *Client) Get(ctx context.Context, request Request) (*Value, error)
func (c *Client) Close() error
func (v *Value) Metadata() Metadata
func (v *Value) Read(p []byte) (int, error)
func (v *Value) WriteTo(w io.Writer) (int64, error)
func (v *Value) Close() error

type Origin func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error)
func ServeOrigin(ctx context.Context, config OriginConfig, origin Origin) error
```

- `ParseKey(string) (Key, error)` accepts only v1 lowercase hex; `Key.String()`
  emits it. All 32-byte values, including zero, are valid.
- `CacheName`, `ETag`, `AdapterMetadata`, and `Authorization` have private storage
  and validating `Parse<Type>(string) (<Type>, error)` constructors following v1.
  Never normalize opaque fields. ETag's zero value means absent only in a request
  pin; it is invalid in Metadata. Zero CacheName is invalid. Opaque zero values
  mean absent, while parsing an explicitly empty field fails.
- `NewFetchContext(AdapterMetadata, Authorization) (FetchContext, error)` creates
  an immutable private value; zero context means both absent. Accessors return
  immutable strings or value copies, never mutable header maps or byte slices.
  Authorization exposes bytes only via an explicitly named `ForOrigin()` string
  accessor. Diagnostic formatting of Authorization, FetchContext, Request,
  OriginRequest, and AdapterMetadata is redacted, including `%v`, `%+v`, `%#v`;
  no serialization hooks expose credentials. The SDK never logs callback errors
  or raw headers.
- `Range` has private state. `ClosedRange(ByteOffset, ByteOffset)`,
  `FromRange(ByteOffset)`, and `SuffixRange(ByteLength)` return `(Range, error)`;
  reject syntactic overflow/reversal during construction. Zero suffix is valid
  syntax but unsatisfiable. Zero Range means unspecified. Public Request contains
  only Key and Context; Get always opens a whole fresh stream. Pins, ranges, and
  HEAD remain wire operations for internal continuation and origin handling.
  Public range constructors/accessors and Resolve support OriginRequest consumers.
  Client.Get validates its request and aggregate wire-head size before I/O.
- `OriginRequest` has private fields and value accessors `Key()`, `Context()`,
  `Operation()`, `Pin() (ETag, bool)`, and `Range() (Range, bool)`. `Operation` is a
  typed enum `OperationHead`, `OperationBootstrap`, `OperationPinned`; zero is
  invalid. Range accessors expose immutable kind/bounds, not setters. Only the
  server constructs OriginRequest, after wire validation. The callback can resolve
  the whole-page request against the selected metadata; the SDK checks it again.
- Metadata is a value snapshot: total object size even for a partial Value, not
  remaining bytes. Validate Size <= MaxInt64, strong ETag, and nonnegative expiry
  representable as signed 64-bit Unix milliseconds. Require millisecond precision
  (reject sub-millisecond time rather than silently changing it); serialize in UTC.
  Metadata returned from a Value is a copy. Expiry is an admission hint, not a local
  deadline for a pinned stream. No synthetic TTL default or SDK metadata cache.

No object `[]byte` result, SDK page cache/scheduler, public HTTP client injection,
retry policy, adapter registry, separate metadata callback, or caller-owned server
listener is needed. Internal transport/listener seams support tests.

## Streaming algorithm and lifetimes

For a fresh full-object Get:

1. Acquire bounded request capacity under ctx, issue unpinned bootstrap GET, and
   validate status and all metadata/framing before returning Value. Do not consume
   a page to obtain metadata. Empty bootstrap returns a valid immediately-EOF Value.
2. Read page zero directly from its HTTP body into the caller's buffer. Pin its
   ETag and total size in Value. Expose no later version, even after expiry.
3. After page zero is consumed, if more bytes remain, lazily issue exactly one
   `Range: bytes=16777216-<size-1>` GET with that If-Match and identical context.
   Validate 206, ETag, unchanged total size, and exact boundaries before reading.
   The remainder may span any number of pages. Racer schedules them within its
   own bounded window. Retain the initial Metadata snapshot if expiry is refreshed.
4. End with EOF only after the full expected count. Do not open a continuation
   for a one-page object. Closing before page zero ends never fetches the remainder.

Continuation pin errors never fall back to a fresh version.
The client holds at most one response body per Value, and no SDK-owned page buffer.
`Read` reads directly into `p`; small stdlib framing buffers still exist.
`WriteTo` uses a bounded reusable 32 KiB copy buffer, honors partial writes and
`io.ErrShortWrite`, and never dispatches to a body fast path that bypasses boundary
checks. Do not allocate arrays of page descriptors proportional to object size.

Client is safe for concurrent Get and Close. Value has one consuming goroutine
(Read or WriteTo, including sequential mixing); Close may run concurrently and
must cancel a blocked read or continuation acquisition. Metadata access is safe
concurrently. Get's ctx governs the entire Value, not only response headers.
Each Value has a derived cancel function registered atomically with the client;
Client.Close rejects new work, cancels pending Get calls and all active Values,
closes active bodies, then closes idle transport connections. Handle the race
between registration, successful headers, and Close without leaking a response.

EOF, terminal errors, and Close all release capacity and request-scoped context.
Close is idempotent; never drain an unread object to keep a socket alive. Aborted
bodies are not reused. After explicit Close, consumption returns a typed closed
error; an already observed clean EOF remains EOF. Preserve partial byte counts on
errors, including WriteTo destination failures; a terminal failure cancels the
Value. Callers should defer Close even when intending to read through EOF.

## Origin ownership, limits, and deadlines

ServeOrigin binds only the canonical origin socket derived from CacheName. It
requires a precreated endpoint directory and never creates/chmods parent paths.
Validate the entire path and reject symlink traversal before bind. Existing paths,
including stale sockets and symlinks, fail; do not unlink first. Disable automatic
UnixListener unlink and remove only the socket inode this invocation created,
checking identity on cleanup so a replacement is preserved. Endpoint directories
must not be writable by untrusted peers; stdlib pathname checks do not promise
race-free traversal against an adversary changing parent directories.

Invoke Origin once per accepted operation, including HEAD. No callback retry.
Callback metadata and body must describe the same immutable version; the SDK
enforces the pin, size, and body bounds as specified in v1. Ownership transfers
even on error, so close every nonnil returned body exactly once. Unexpected HEAD
bodies are contract errors. Canceling the request must close its body to unblock
Read, and callback implementations must honor ctx; Close must safely interrupt a
concurrent Read. The SDK cannot kill a callback or reader that ignores this contract.

Use these explicit zero-config defaults; zero numeric config fields select these
defaults, negative durations or limits are invalid. Both configs require a
`Cache CacheName` field; configuration is copied and validated at entry.
No root/socket override, custom transport, or global singleton:

| Config field | Default and scope |
| --- | --- |
| `ClientConfig.MaxConnections` | 16 per Client; also cap active/pending-on-wire Values. Capacity waits are cancelable. Idle pool cap equals this bound. |
| `ClientConfig.DialTimeout` | 5 seconds per Unix dial |
| `ClientConfig.ResponseHeaderTimeout` | 10 seconds per request after writing headers |
| `ClientConfig.IdleConnTimeout` | 90 seconds for idle pooled connections |
| `OriginConfig.MaxConnections` | 128 accepted connections, including idle; cap before spawning handlers |
| `OriginConfig.MaxConcurrentRequests` | 64 callbacks/bodies; no waiting callback queue, return 503 on saturation |
| `OriginConfig.ReadHeaderTimeout` | 5 seconds per head, including the first |
| `OriginConfig.RequestTimeout` | 60 seconds from accepted complete head through callback, body, EOF probe, and final write |
| `OriginConfig.WriteTimeout` | 30 seconds of blocked write; reset before each bounded chunk, bounded by request deadline |
| `OriginConfig.IdleTimeout` | 30 seconds waiting for the next request |
| `OriginConfig.SocketMode` | 0600; allow only permission bits, apply to the newly created socket, fail and clean up on chmod failure |

Wire header limits are fixed constants, not configurable. Copy scratch space is
32 KiB per active origin stream/WriteTo; use a hard-bounded pool or bounded active
allocation, not an unbounded free list. Connection and concurrency limits bound
the parser buffers, goroutines, and body scratch. At the connection cap, stop
accepting until capacity frees (the OS backlog is finite); an already accepted
well-formed request at the callback cap gets empty 503. A stuck callback continues
to occupy its slot even after its client disconnects, preventing unlimited leaks.

Client uses a private stdlib Transport with Unix-only DialContext, Proxy nil,
DisableCompression true, HTTP/2 disabled, bounded MaxConnsPerHost/idle counts,
and redirect rejection. There is no total Client.Timeout or implicit body-read
deadline: caller ctx governs long streams and pool waits. Set a caller deadline
when bounded completion is required. The SDK has no retry loop; stdlib Transport
may replay an eligible bodyless GET on a failed reused connection. Do not claim
exactly-once network delivery; callbacks must support repeated read operations.

ServeOrigin's ctx is the server lifetime. Each request inherits its cancellation
and the local request timeout; peer disconnect cancels it too. There is no new
deadline wire header: the dataplane closes a canceled origin request, and the
origin timeout independently bounds work. Deadline before headers is 503; after
headers, abort the connection. On server cancellation, stop admission, close
listeners/connections and active bodies, cancel callbacks, and return ctx.Err().
Do not wait forever for a noncooperative callback; close any late-returned body.
Other ServeOrigin errors preserve their bind/serve causes. No grace-period API.

## Error model

Define `Error` with typed `ErrorKind`, operation, optional HTTP status, and a
private cause exposed by Unwrap. Kinds distinguish invalid argument, closed,
protocol, unauthorized, forbidden, not found, version unavailable, unsatisfiable
range, header limit, internal, bad gateway, unavailable, canceled, deadline, and
I/O failure. `errors.As` exposes kind/status; `errors.Is` traverses context and I/O
causes, especially `context.Canceled`, `context.DeadlineExceeded`, and
`io.ErrUnexpectedEOF`. Clean EOF remains bare `io.EOF`. Invalid arguments fail
before network/filesystem access. Errors contain no context header or payload.

Provide `NewOriginError(kind ErrorKind, cause error) error` for callback failures;
only documented origin kinds map to 400/401/403/404/412/416/431/500/502/503.
Untyped callback errors map to 500, canceled/deadline errors to 503 before headers.
Pinned not-found maps to 412. Construct 416 only when a selected-version size is
known; the server normally derives it from callback Metadata. For callback
unsatisfiable errors, require valid Metadata to construct `bytes */N`; invalid or
absent metadata makes this a 502 callback-contract failure. Other error metadata
is ignored. SDK-detected invalid returned metadata/body contract maps to 502.
Do not include cause text in Error.Error() or default diagnostics, while retaining
the cause for explicit inspection. The SDK cannot redact a caller's own logging
or the underlying error obtained via Unwrap.

## Implementation steps and files

1. **Contract:** review the API, wire grammar, defaults, and acceptance
   below. The source remains a nonoperational scaffold.
2. **Types and protocol:** add `pkg/racersdk/doc.go`, `types.go`, `range.go`,
   `errors.go`, `wire.go`, `wire_conn.go`, and corresponding focused tests. Implement
   validated private values, safe formatting, range math, metadata/status parsing,
   bounded raw-head validation, and shared canonical vectors.
3. **Client and origin:** add `client.go`, `value.go`, `origin.go`, corresponding
   tests, and Unix lifecycle helpers as needed. Implement bounded Unix transport,
   bootstrap/continuation, buffer-independent streaming, ownership, cancellation,
   the callback, raw-wire enforcement, bounded admission, deadlines, body probing,
   safe socket cleanup, and late-error aborts.
4. **Package documentation and examples:** document the public API, streaming and
   callback ownership, credentials, deadlines, and errors; add `example_test.go`.
5. **Verification and performance:** add `integration_test.go` and
   `benchmark_test.go`. Exercise client against an origin shim/fake dataplane and
   raw peers. Document verification and performance results.
   A real Rust compatibility claim requires implementing its still-stubbed codec,
   parser, response writer, and origin validators in a separately scoped change;
   a fake transport passing is not evidence that Racer itself serves data.

## Test and performance acceptance

### Private protocol integration interface

The step 2 implementation exposes these package-private seams for client/origin:

- `readRawHead(*bufio.Reader, response)` reads at most 32 KiB and runs
  `validateRawHead`. Preserve that same buffered reader across head, body, and
  sequential messages; it can contain read-ahead. Never scan body bytes for a
  header delimiter. Any head error requires connection closure. The connection
  owner supplies deadlines/cancellation and must release credential-bearing head
  buffers after use. No pooled buffer retains context.
- `parseRequestHead(head, origin)` returns the private fields of `OriginRequest`.
  `origin=true` enforces page shape; selected-size validation follows the callback.
  `requestHead` constructs a canonical head from that private operation descriptor.
  Bootstrap uses `bootstrapRange()`. The public Request remains Key/Context only.
- `parseResponseHead(head, request, snapshot)` returns `wireResponse` metadata,
  inclusive bounds, and actual body length (zero for HEAD). The optional initial
  snapshot checks immutable tag/size, allowing refreshed expiry. Protocol error
  responses return typed errors after validating their empty framing.
- `originResponse(request, metadata)` validates successful callback metadata, pin,
  and whole-page bounds. `metadataHeaders`, `contentRangeValue`, `callbackStatus`,
  and `originStatus` support the writer. The server still owns callback-body
  contracts and the selected-size requirement for callback 416 errors.
- `frameReader` bounds a streaming HTTP body, preserves final-byte I/O errors, and
  reports short EOF through `errors.Is(err, io.ErrUnexpectedEOF)`. It neither closes
  its source nor probes beyond the HTTP frame. Callback EOF probing/final-byte
  holdback belongs in the origin implementation, not this framing reader.

These are building blocks, not an installed transport guard. When integrating a
stdlib Transport, validate each raw response head before replaying it to net/http;
advance only by its validated body length, using the known request method for HEAD.
Reject informational responses before Transport can consume them. On the origin
side validate raw heads before net/http can normalize them or emit its own error
bodies. Parsed Header maps alone cannot enforce context separator whitespace or
duplicate Content-Length. Raw validation is mandatory even if semantic validation
is also applied to parsed objects. Connection wrappers must account for sequential
request/response boundaries and read-ahead without adding an object/page buffer.

### Acceptance checks

- Table tests and fuzzing for keys/names/ETags, expiry precision/overflow, private
  type zero values, ranges at 0, P-1, P, P+1, and MaxInt64; never panic or allocate
  proportional to claimed size. Verify quoted commas/backslashes and empty ETag.
- Raw Unix tests for exact target/no query, duplicate identical/conflicting lengths,
  whitespace preservation/rejection, 32 KiB and 8 KiB boundaries, request bodies,
  chunking/compression, unknown status/redirect, HEAD/body rules, and empty errors.
  Cover normalization by Go's parser, not just Header.Get-based validation.
- Client request transcripts for empty, short page, exactly one page, multi-page,
  unchanged credentials, and lazy cancellation.
  Verify at most two GETs for full reads regardless of page count; no HEAD preflight.
  Verify pin/size mismatch, 412/503, short body, late failure, expiry during read,
  partial destination writes, and EOF semantics. Document overlong HTTP detection
  limits in tests rather than asserting impossible guarantees.
- Origin success/failure tests for atomic pin handling, whole-page validation,
  short final page, nil/unexpected/error-associated body, exactly-once Close,
  short/overlong/terminal-error body, blocked EOF probe, callback errors, and panic
  recovery (500 before headers, abort after). Ensure default server logging cannot
  print callback panic values or headers. Test cancellation/deadlines before and
  after headers, admission caps, slow readers/headers, and late callback return.
- `go test -race ./pkg/racersdk` must cover Get/Close/Read races and repeated
  start/cancel cycles without retained sockets, descriptors, or goroutine growth.
  Verify existing socket/file/symlink refusal, cleanup on failure, replacement
  preservation, path-length limits, and separate cache paths. Tests use a private
  internal socket-path seam under the test temporary directory, not `/run/racer`.
- Benchmarks use generated/discarded streams, not preallocated test objects:
  0, 4 KiB, P, and 1 GiB; concurrency 1 and 16; Read buffers 4 KiB/32 KiB/256 KiB
  and WriteTo. Report throughput, allocs/op, allocated bytes/op, peak live heap,
  and comparison to a bare stdlib Unix HTTP stream on the same host/toolchain.
  Warm pools separately. SDK allocations must not scale with read-call count,
  page count, or object length; live memory scales with configured concurrency
  and fixed scratch/framing buffers. Verify no 16 MiB SDK allocation in profiles.
  Use long steady-state streams to demonstrate a plateau, and run with slow
  destinations to verify backpressure rather than growing buffering. Record any
  throughput regression above 10% versus the same-buffer baseline for review;
  no hardware-independent absolute throughput promise is made.
- Run `make fmt`, focused Go tests/race tests, and project lint for implementation
  commits. Run benchmarks with `go test ./pkg/racersdk -run '^$' -bench . -benchmem`.
  Step 1 needs documentation/link/whitespace review, not tests of nonexistent Go.
