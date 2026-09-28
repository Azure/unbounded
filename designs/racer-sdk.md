# Racer Go SDK

## Scope and evidence

The approved high-throughput rewrite is **VERIFIED**, as of
2026-09-28. This document describes the current implementation contract, including
the Go SDK, its Gantry integration, and Rust range delivery. It supersedes the
original design's page-by-page continuation, Transport, and digest-holdback claims.
Implementation citations are repository-relative unless noted otherwise.

`pkg/racersdk` is a standard-library-only concurrent streaming client and
single-callback origin server. See the normative
[client/origin v1 contract](../cmd/racer-dataplane/CLIENT_ORIGIN_API.md),
[package guide](../pkg/racersdk/doc.go), and
[examples](../pkg/racersdk/example_test.go). SDK packages must not import `cmd/`.

The [verification summary](racer-sdk-verification.md) records the final passing
gates, including deployed E2E. Older sections there and in the
[independent conformance report](racer-sdk-conformance.md) remain historical.
Detailed measurements are in [throughput performance](racer-sdk-throughput-performance.md).
Source inspection and scripted Go/Rust interoperability remain distinct from the
separately passed deployed gate and workload-specific performance measurements.

## Minimal public surface

These are the implemented public signatures and semantic constraints:

```go
type Key [32]byte
type ByteLength uint64
type ByteOffset uint64

type Metadata struct {
    Size        ByteLength
    ETag        ETag
    ExpiresAt   time.Time
    ContentType string
}

type Request struct {
    Key     Key
    Context FetchContext
}

type ReadOptions struct {
    SmallObject bool
    Offset      ByteOffset
    Length      ByteLength
    Pin         ETag
    Metadata    *Metadata
}

func NewClient(config ClientConfig) (*Client, error)
func (c *Client) Get(ctx context.Context, request Request, options ...ReadOptions) (*Value, error)
func (c *Client) Stat(ctx context.Context, request Request) (Metadata, error)
func (c *Client) Stats() Stats
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
- `Range` has private state. `ClosedRange(ByteOffset, ByteOffset)` returns
  `(Range, error)` and rejects syntactic overflow/reversal during construction.
  `Resolve` validates whole-page alignment and returns bounds shortened at EOF.
  `Bounds()` returns the original inclusive endpoints and a presence flag.
  Zero Range means unspecified. ClosedRange, Bounds, and Resolve support origin
  callbacks. Client byte selection instead uses ReadOptions; Length zero means
  through EOF, so callers need no open-ended or suffix Range constructor.
  Client.Get validates its request before I/O. Per-field bounds ensure the fixed
  outgoing head fits the aggregate limit without serializing it for validation.
- `OriginRequest` has private fields and value accessors `Key()`, `Context()`,
  `Operation()`, `Pin() (ETag, bool)`, and `Range() (Range, bool)`. `Operation` is a
  typed enum `OperationHead`, `OperationBootstrap`, `OperationPinned`; zero is
  invalid. Range exposes immutable requested bounds, not setters. Only the
  server constructs OriginRequest, after wire validation. The callback can resolve
  the whole-page request against the selected metadata; the SDK checks it again.
- Metadata is a value snapshot: total object size even after partial consumption,
  not remaining bytes. Validate Size <= MaxInt64, strong ETag, and nonnegative expiry
  representable as signed 64-bit Unix milliseconds. Require millisecond precision
  (reject sub-millisecond time rather than silently changing it); serialize as Unix milliseconds.
  Metadata returned from a Value is a copy. Expiry is an admission hint, not a local
  deadline for a pinned stream. No synthetic TTL default or SDK metadata cache.

These types and constraints are implemented in `pkg/racersdk/types.go:20-234`,
`pkg/racersdk/range.go:18-75`, `pkg/racersdk/client.go:209-355`, and
`pkg/racersdk/value.go:193-280`.

Get accepts zero or one ReadOptions value. Empty options, including SmallObject
alone, preserve fresh bootstrap. Nonzero Offset/Length/Pin or a supplied Metadata
snapshot select the exact pinned range. Without a snapshot, Get first uses the
reserved HEAD pool; with one, it validates and copies the trusted snapshot and
skips HEAD. A nonzero Pin must match the snapshot. The caller must associate the
snapshot with the correct Request and must not mutate it during Get: Metadata
does not contain a key. Length/offset overflow fails before I/O. Bounds beyond
the selected object fail rather than silently shortening; offset equal to size
with zero length yields an empty Value without a GET when options select a pinned
read. Empty options still bootstrap even an empty object. Stat is a fresh metadata-only
HEAD (`pkg/racersdk/client.go:240-301,322-355`).

SmallObject reserves a separate pool for objects whose **total** size is at most
PageSize (16 MiB), not for short ranges of larger objects. Oversized snapshots or
HEAD metadata fail before GET; oversized bootstrap metadata fails before exposing
a Value or consuming its body. There is no fallback into the bulk pool
(`pkg/racersdk/client.go:235-276`, `pkg/racersdk/response_conn.go:268-275`).

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
3. After page zero is consumed, if more bytes remain, lazily issue **one** pinned
   GET for `Range: bytes=16777216-<size-1>` with the bootstrap If-Match and identical
   context. Validate 206, ETag, unchanged total size, compatible content type, and
   exact boundaries before reading the body. Retain the initial Metadata snapshot
   if expiry is refreshed. Racer schedules underlying whole-page fetches.
4. End with EOF only after the full expected count. Do not open a continuation
   for a one-page object. Closing before page zero ends never fetches the remainder.

Thus a successful full read uses one bootstrap and at most one remainder GET,
regardless of page count, with no SDK HEAD preflight. An options-selected pinned read uses
one exact pinned GET (or none for an empty selection), without a bootstrap body.
These are logical exchange counts; the narrowly permitted stale-connection retry
below can replay an exchange (`pkg/racersdk/client.go:240-319`,
`pkg/racersdk/value.go:119-144`).

Continuation pin errors never fall back to a fresh version. Get's context bounds
the full stream; SDK socket header/write deadlines are removed before exposing
the body (`pkg/racersdk/response_conn.go:235-284`). Rust client delivery uses
bounded page acquisition and progress-based writes, detailed below, so an entire
pinned remainder is not forced into the old absolute response deadline.
Errors received before any continuation's body starts retain their HTTP error
classification, even after earlier pages succeeded. Truncated bodies remain I/O
errors with partial counts. Both are terminal for the Value.
The client holds at most one response body per Value, and no SDK-owned page buffer.
`Read` reads directly into `p`; small stdlib framing buffers still exist.
`Value.WriteTo`, also selected by `io.Copy` and `io.CopyBuffer`, uses bounded
32 KiB scratch and does not invoke the destination's ReadFrom. It counts only
bytes accepted by the writer and preserves short-write errors. Copy scratch has
independent bulk and small-object caps, including copies canceled while blocked
inside caller-owned Write. Exhausted scratch returns ErrorUnavailable. Cancellation
cannot interrupt an arbitrary writer; callers must still Close on writer failure
(`pkg/racersdk/value.go:193-280`). No array of page descriptors scales with object size.

Client is safe for concurrent Get, Stat, Stats, and Close. Value has one consuming
goroutine (Read or WriteTo, directly or through io.Copy); Close may run concurrently and
must cancel a blocked read or continuation acquisition. Metadata access is safe
concurrently. Get's ctx governs the entire Value, not only response headers.
Each Value has a derived cancel function registered atomically with the client;
Client.Close rejects new work, cancels pending Get calls and all active Values,
closes active bodies, then closes idle transport connections. Handle the race
between registration, successful headers, and Close without leaking a response.

One live-Value capacity slot is retained across every page boundary. EOF, terminal
errors, and Close all release capacity and request-scoped context.
Close is idempotent; never drain an unread object to keep a socket alive. Aborted
bodies are not reused. After explicit Close, consumption returns a typed closed
error; an already observed clean EOF remains EOF. Preserve partial byte counts on
errors; a terminal read failure cancels the Value. Callers must defer Close even
when intending to read through EOF, including when io.Copy's destination fails.

## Origin ownership, limits, and deadlines

ServeOrigin binds only the canonical origin socket derived from CacheName. It
requires a precreated endpoint directory and never creates/chmods parent paths.
Validate the entire path and reject symlink traversal before bind. By default existing paths,
including stale sockets and symlinks, fail; do not unlink first. Disable automatic
UnixListener unlink and remove only the socket inode this invocation created,
checking identity on cleanup so a replacement is preserved. Endpoint directories
must not be writable by untrusted peers; stdlib pathname checks do not promise
race-free traversal against an adversary changing parent directories.

`OriginConfig.RecoverStaleSocket` (default false) opts into Linux crash recovery;
Gantry enables it for its persistent hostPath endpoint. This mode pins each path
component with no-follow directory opens and requires an endpoint directory owned
by the effective user, without group/world write permission. Ancestors must still
be trusted against replacement. A persistent `0600` `.racer-origin.lock` regular
file is opened no-follow, checked for owner, permissions, single link and pathname
identity, then held with nonblocking exclusive `flock` through socket cleanup.
The lock is never unlinked or truncated, including after clean shutdown.

Bind `.racer-origin.socket` first, then hard-link that socket inode to `socket`
without replacement. The witness pins inode identity and closes the bind-to-record
crash window. Restart under the lock recovers only a canonical socket matching the
witness (or a witness left before publication), and only after a bounded connection
probe reports `ECONNREFUSED`. A live listener, other probe errors, foreign sockets,
files and symlinks fail closed. Cleanup checks inode identity and retains the
witness if removing the canonical socket fails. The SDK default still refuses all
existing endpoints; sockets left by older releases without a witness require
operator cleanup after independently establishing that the owner is stopped.
This protocol assumes a local Linux filesystem with Unix socket hard links and
`flock`, and trusted directory writers. It handles process death, not power-loss
durability or hostile same-user directory mutation.

Invoke Origin once per accepted operation, including HEAD. No callback retry.
Callback metadata and body must describe the same immutable version; the SDK
enforces the pin, size, and body bounds as specified in v1. Ownership transfers
even on error, so close every nonnil returned body exactly once. Unexpected HEAD
bodies are contract errors. Canceling the request must close its body to unblock
Read, and callback implementations must honor ctx; Close must safely interrupt a
concurrent Read. The SDK cannot kill a callback or reader that ignores this contract.
Before completing a successful body it probes EOF and withholds the final bytes
until that probe succeeds; short, overlong, and terminal-error bodies abort the
response (`pkg/racersdk/origin.go:287-330,435-496`). HTTP Content-Length still
limits what a remote consumer can observe: framing checks cannot prove that a
peer will never send extra bytes, nor identify incorrect content with the right
length and ETag (`pkg/racersdk/wire_conn.go:176-182`).

Use these explicit zero-config defaults; zero numeric config fields select these
defaults, negative durations or limits are invalid. Both configs require a
`Cache CacheName` field; configuration is copied and validated at entry.
No root/socket override, custom transport, or global singleton:

| Config field | Default and scope |
| --- | --- |
| `ClientConfig.MaxConnections` | 64 bulk connections/live Values, including dials |
| `ClientConfig.MaxQueuedRequests` | 128 waiting bulk calls |
| `ClientConfig.MetadataConnections` | 4 reserved HEAD connections/operations |
| `ClientConfig.MetadataQueuedRequests` | 16 waiting HEAD calls, independent of bulk |
| `ClientConfig.SmallObjectConnections` | 4 reserved small-object connections/live Values |
| `ClientConfig.SmallObjectQueuedRequests` | 128 waiting small-object calls, independent of bulk and HEAD |
| `ClientConfig.QueueTimeout` | 5 seconds per admission wait |
| `ClientConfig.DialTimeout` | 5 seconds per Unix dial |
| `ClientConfig.ResponseHeaderTimeout` | 60 seconds for blocked request writes, then a fresh 60 seconds for response headers after writing the request |
| `ClientConfig.IdleConnTimeout` | 90 seconds for idle pooled connections |
| `ClientConfig.MaxConnAge` | 5 minutes maximum reuse age; each successful dial samples a lifetime uniformly from [75%, 100%], or 3m45s..5m by default |
| `OriginConfig.MaxConnections` | 128 accepted connections, including idle; cap before spawning handlers |
| `OriginConfig.MaxConcurrentRequests` | 64 GET callbacks/bodies; no waiting callback queue, return empty 503 on saturation |
| `OriginConfig.MaxConcurrentHeadRequests` | 4 separately reserved HEAD callbacks; the accepted-connection cap is still shared |
| `OriginConfig.ReadHeaderTimeout` | 5 seconds per head, including the first |
| `OriginConfig.RequestTimeout` | 60 seconds from accepted complete head through callback, body, EOF probe, and final write |
| `OriginConfig.WriteTimeout` | 30 seconds of blocked write; reset before each bounded chunk, bounded by request deadline |
| `OriginConfig.IdleTimeout` | 30 seconds waiting for the next request |
| `OriginConfig.SocketMode` | 0600; allow only permission bits, apply to the newly created socket, fail and clean up on chmod failure |
| `OriginConfig.RecoverStaleSocket` | false; opt into exclusive ownership and recovery of witnessed sockets; enabled by Gantry |

Defaults and allocation are in `pkg/racersdk/client.go:82-155` and
`pkg/racersdk/origin.go:59-100,144-156`. Admission queues are independent: full
queues return ErrorUnavailable immediately; expired queue waits return ErrorDeadline.
Waiters are counted before allocating Value/context state. With defaults, one
client admits up to 72 operations/live Values and 272 queued calls across three
pools. This is a capacity bound, not an end-to-end throughput guarantee or a
process RSS estimate (`pkg/racersdk/client.go:134-205`).

Wire header limits are fixed constants, not configurable. Copy scratch is bounded
by 32 KiB times bulk plus small-object copy capacity (2.125 MiB at defaults),
allocated lazily and retained for reuse. Origin GET scratch is 32 KiB per active
copy (up to 2 MiB at the default GET cap). These exclude framing buffers, caller
memory, kernel sockets, and Rust pages. Connection and concurrency limits bound
the parser buffers, goroutines, and body scratch. At the connection cap, stop
accepting until capacity frees (the OS backlog is finite); an already accepted
well-formed request at the callback cap gets empty 503. A stuck callback continues
to occupy its slot even after its client disconnects, preventing unlimited leaks.

Client uses private sequential Unix connection pools with a connection-owned
bufio.Reader, bounded raw-head parsing, and fixed-length bodies, not http.Transport.
Only completely consumed, reusable frames with no buffered surplus return to
their own pool. Aborted bodies close without draining. Idle timers close unused
connections (`pkg/racersdk/response_conn.go:71-169,171-230`). No proxy, redirect,
compression, HTTP/2, or total HTTP client timeout is involved.

`ClientConfig.MaxConnAge` applies to all three pools: bulk, metadata/HEAD, and
small objects. Zero selects the 5-minute default; negative values are invalid.
Each successful dial samples its jittered lifetime once, uniformly over whole
nanoseconds from ceil(75% of MaxConnAge) through MaxConnAge, inclusive. The
resulting monotonic deadline is fixed; reuse does not renew it
(`pkg/racersdk/client.go:91-155`, `pkg/racersdk/response_conn.go:23-43,100-112`).

Age rotation occurs only at response boundaries. An active response may continue
past the deadline without interruption; this is a reuse limit, not a body timeout.
Expired reusable connections are retired on pool checkout, on recycle after a
fully consumed response, or by an idle timer. The idle timer uses
`min(IdleConnTimeout, remaining connection lifetime)`, so frequent reuse cannot
keep a connection alive indefinitely. Replacement is lazy: retiring a connection
does not dial or reserve additional capacity. A later exchange dials only when
needed under the existing pool admission bounds. Rotation does not replay an
exchange or restart a pinned continuation
(`pkg/racersdk/response_conn.go:71-155,215-257`; lifecycle and active-response
assertions in `pkg/racersdk/connection_age_test.go:151-221,313-376`).

Rotation creates opportunities for worker reassignment, not a guarantee of equal
worker utilization. See the [real Racer connection-age validation](racer-sdk-connection-age-validation.md)
for the bounded workload results and measurement limitations.

One fresh-connection retry is allowed only after EOF, reset, or broken pipe on
a reused connection **before any response byte**. Timeouts, partial heads,
protocol errors, HTTP errors, and body failures are not retried. There is no
version restart or body resume. Callbacks must tolerate replay; network delivery
is not exactly once (`pkg/racersdk/response_conn.go:232-263`).

ServeOrigin's ctx is the server lifetime. Each request inherits its cancellation
and the local request timeout; peer disconnect cancels it too. There is no new
deadline wire header: the dataplane closes a canceled origin request, and the
origin timeout independently bounds work. Deadline before headers is 503; after
headers, abort the connection. On server cancellation, stop admission, close
listeners/connections and active bodies, cancel callbacks, and return ctx.Err().
Do not wait forever for a noncooperative callback; close any late-returned body.
Other ServeOrigin errors preserve their bind/serve causes. No grace-period API.

## Rust range progress and bounded work

The production client listener calls Responses::send_observed, which prepares the
first slice before headers, enables progress for normal pinned client ranges, and
polls the existing prefetch window while delivering bytes. Direct Responses::send
and explicit aggregate-budget APIs retain absolute deadlines
(`cmd/racer-dataplane/src/client/listener.rs:556`,
`cmd/racer-dataplane/src/client/response.rs:101-198`). Rust paths in the rest of this
section are relative to `cmd/racer-dataplane/`.

- Initial metadata/bootstrap receives 32 attempts and 96 aggregate forwarded
  links (`src/read/serve.rs:51-52`). Normal pinned client ranges use per-page
  budgets rather than spending a range-wide allowance for each successful page
  (`src/read/serve.rs:263-293`). Each distinct admitted page gets at most eight
  attempts and sixteen links in the bounded window (`src/read/range_stream.rs:30-65`).
- Idle/header reception and initial metadata/first-slice work retain the configured
  request deadline. After successful headers, newly admitted pages in a normal
  pinned client range receive fixed child acquisition deadlines. Already admitted
  pages and retries never receive new time or credits. Pending futures stay in
  the stream; dropping a next_slice future does not restart a page. Explicit
  aggregate budgets partition/reunite only their owned credits and cannot be
  upgraded to progressing budgets (`src/read/range_stream.rs:223-315`).
- Positive client socket writes renew the reader-stall clock. Backpressure does
  not grow the prefetch window. Peer writes still take the minimum of the original
  deadline and stall deadline (`src/memory/delivery.rs:153-205,261-265`).
- Runtime defaults are `RACER_REQUEST_TIMEOUT_MS=30000`,
  `RACER_READER_STALL_TIMEOUT_MS=10000`, and `RACER_RANGE_WINDOW_PAGES=2`
  (`src/config.rs:167-186`). Increasing SDK concurrency does not increase Rust
  page, pipe, connection, queue, or origin capacity automatically.

This permits healthy large remainders to outlive the initial request timeout;
it does not guarantee successful completion. Acquisition failure, deadline,
overload, cancellation, and stalled downstream delivery can still terminate a
stream. Submitted work retains buffers and admission until its completion fence.

## Gantry object identity and origin bandwidth

Gantry uses the SHA-256 object digest as the Racer key and its quoted digest as
the ETag. The mirror validates metadata, counts bytes, and streams through SDK
EOF; it does **not** hash the payload, parse manifest bodies, or withhold a
digest-verified final chunk. The OCI consumer must verify the complete object's
digest, including resumed assembly. Correct length and ETag alone cannot establish
payload identity (`internal/gantry/racer/racer.go:42-74,193-201`,
`internal/gantry/mirror/racer.go:93-135`).

Ordinary digest GETs do not add a HEAD preflight. Manifest GETs use SmallObject;
HEAD uses Stat. A supported open-ended blob resume performs one Stat and supplies
that snapshot to an exact pinned offset Get, avoiding a second HEAD and avoiding
reading/discarding the skipped prefix in Go. The dataplane still acquires the
whole page containing an unaligned starting offset. Unsupported range forms use
the existing full-response behavior. These are distinct from containerd's own
resolve, HEAD, and fallback requests (`internal/gantry/mirror/racer.go:45-86`,
`internal/gantry/mirror/racer_test.go:309-388`).

The production registry client implements OriginRangePuller, so GET callbacks use
one bounded registry Range request per acquired page and learn size/content type
from that response, without per-page HEAD. Only explicit HEAD calls use registry
Head. Legacy external OriginPullers without PullRange retain a compatibility path
that does HEAD plus open-ended Pull and limits the delivered page; that path does
not have the production bounded-fetch guarantee
(`internal/gantry/racer/racer.go:212-255,258-294`).

Registry response rules (`internal/gantry/origin/range.go:16-73`):

- A 206 must have the requested start, exact end shortened only at EOF, a known
  total size, and matching Content-Length if one is present. A supplied
  Docker-Content-Digest must match the requested digest.
- A complete 200 is accepted only at offset zero with a known entire-object
  Content-Length that fits the requested page. This supports registries that
  ignore Range on small manifests. Unknown-size, oversized, or nonzero-offset
  complete responses fail without draining the object.
- At offset zero, an explicitly empty 200 or 416 with `bytes */0` represents an
  empty object. Other unsatisfiable cases remain errors.
- A blob-route 404 may retry the manifest route with the same bounded Range and
  delegated identity. Authentication and routing retries are additional HTTP
  round trips, not extra successful page payloads.

Bounded page requests remove open-ended tail-download amplification; they do not
promise exactly one origin request per page across misses, failures, auth retries,
or concurrent nodes. Measure actual origin bytes and round trips separately from
downstream bytes. No Racer failure selects direct content-origin fallback in the
mirror (`internal/gantry/mirror/racer.go:27-36,86-90`).

## Content type and compatibility

Metadata.ContentType is optional original-object MIME metadata, separate from
transport `Content-Type: application/octet-stream`. A present Racer-Content-Type
is 1-256 ASCII bytes with valid type/subtype and optional parameters; controls,
tabs, edge whitespace, lists, duplicate parameter names, and duplicate fields
fail. Empty Go string means absent, not a present empty header
(`pkg/racersdk/types.go:200-215,236-335`, `pkg/racersdk/wire_conn.go:124-132,153-159`).

Two present values for the same version must agree. Legacy absence is unknown,
not a conflicting MIME claim, and a Value keeps its initial snapshot even when
later metadata includes or omits content type
(`pkg/racersdk/reserved_admission_test.go:303-335`,
`cmd/racer-dataplane/src/model/metadata.rs:99-107`). Gantry writes the selected
metadata's content type without fetching/parsing a manifest to rediscover it.

Client/origin HTTP remains v1 with this optional response header. Typed peer
responses additionally sign `racer-metadata-version: 2` and `racer-content-type`;
untyped responses retain the old shape. New decoders reject unknown versions,
missing typed values, and unversioned typed fields. Signature/logical agreement
covers MIME metadata (`cmd/racer-dataplane/src/security/protocol.rs:305-307`,
`cmd/racer-dataplane/src/peer/decode.rs:76-99`,
`cmd/racer-dataplane/src/security/forwarding.rs:1422-1448`).

Old strict peer decoders reject the typed shape: mixed old/new peer paths carrying
content type may fail until all participating nodes are upgraded. There is no
capability negotiation or silent downgrade. This is the compatibility limit
specified in `cmd/racer-dataplane/CLIENT_ORIGIN_API.md:95-111`, not a claim that a
mixed-version deployment has passed end-to-end validation.

Untyped disk records retain v1 encoding; typed records use v2 with a little-endian
u32 MIME length and MIME bytes before the header digest. New checkpoints use v2
and append a counted MIME string to each descriptor (zero count means absent).
Readers accept v1 and v2, recovering v1 content type as unknown
(`cmd/racer-dataplane/src/store/format.rs:105-127,182-227,288-294`,
`cmd/racer-dataplane/src/store/checkpoint.rs:155`,
`cmd/racer-dataplane/src/store/checkpoint_format.rs:442-456,491-510`). Old binaries
cannot consume v2 data; rollback needs a compatible checkpoint or refetching
disposable cache contents, not an assumption of bidirectional disk compatibility.

## Telemetry and workload configuration

Client.Stats is a fixed-size, credential-free snapshot, sampled independently
during concurrent I/O. It reports aggregate and per-pool queue depth, queue waits
and duration, rejections/timeouts, occupied bulk/HEAD/small-object slots, open/idle
connections, dial/reuse/rotation/retry counters, and consumed body bytes. BytesRead includes
bytes consumed before a later source or destination failure, not just successfully
delivered objects (`pkg/racersdk/stats.go:8-76`).

`Stats.ConnectionRotations` counts reusable connections retired because their
jittered maximum age was reached, including idle-timer age retirement, across all
three pools. It excludes failures, aborted responses, stale-connection retries,
and ordinary idle-timeout retirement before the age deadline. Gantry exports it
as the cumulative, unlabeled Prometheus counter
`gantry_racer_sdk_connection_rotations_total`, alongside
`gantry_racer_sdk_dials_total` and `gantry_racer_sdk_connection_reuses_total`.
The collector samples Stats once per scrape rather than incrementing counters
again when scraped (`pkg/racersdk/response_conn.go:45-69,115-155,215-257`,
`cmd/gantry/agent_racer_metrics.go:101-105,116-121`).

Gantry exports these through `gantry_racer_sdk_*` metrics. Mirror metrics report
method/status/completion, actual accepted downstream bytes, and handler duration;
origin metrics count HTTP round trips (including authentication and failures) and
GET body bytes. They use bounded labels, not object keys, repositories, URLs, or
credentials (`cmd/gantry/agent_racer_metrics.go:25-37,42-69,83-104`). See the
[deployment tuning guide](../deploy/gantry/README.md#workload-tuning-and-capacity)
for exact YAML/environment settings and capacity interpretation. Helm workload
selection and runtime tuning are separate: enable the Racer profile and configure
the Gantry process, with Racer and ClusterCache/gantry provisioned on those nodes.

## Error model

Define `Error` with typed `ErrorKind`, operation, optional HTTP status, and a
private cause exposed by Unwrap. Kinds distinguish invalid argument, closed,
protocol, unauthorized, forbidden, not found, version unavailable, unsatisfiable
range, header limit, internal, bad gateway, unavailable, canceled, deadline, and
I/O failure. `errors.As` exposes kind/status; `errors.Is` traverses context and I/O
causes, especially `context.Canceled`, `context.DeadlineExceeded`, and
`io.ErrUnexpectedEOF`. Clean EOF remains bare `io.EOF`. Syntactically invalid
arguments fail before network/filesystem access; range satisfiability without a
trusted snapshot requires HEAD metadata. Errors contain no context header or payload.

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

The following steps describe the original implementation sequence.

1. **Contract:** review the API, wire grammar, defaults, and acceptance
   below. At that point the Rust dataplane was a nonoperational scaffold.
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
   Real Rust compatibility is checked separately by the opt-in SDK conformance
   fixture; a fake transport passing is not evidence that Racer itself serves data.

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

The installed guards in `pkg/racersdk/response_conn.go` and `origin_conn.go` use
these building blocks before net/http normalization. They validate raw heads and
account for sequential request/response boundaries and read-ahead without adding
an object/page buffer. Parsed Header maps alone cannot enforce context separator
whitespace or duplicate Content-Length; raw validation remains mandatory even
when semantic validation is also applied to parsed objects.

### Acceptance checks (requirements, not results)

These checks define verification requirements. The rewrite's final gates,
including deployed containerd/Gantry/Rust E2E, passed on 2026-09-28; see the
[dated acceptance summary](racer-sdk-verification.md). Historical reports alone
do not establish that result. Measurement scope and limitations remain in
[throughput performance](racer-sdk-throughput-performance.md).

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
  Verify Stat is bodyless; snapshot ranges skip HEAD and all options-based ranges
  skip bootstrap; empty pinned selections issue no GET. Check independent bulk,
  metadata, and small-object queues under saturation and cancellation, oversized
  SmallObject rejection, and bounded scratch with blocked destination writers.
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
  and io.Copy. Report throughput, allocs/op, allocated bytes/op, peak live heap,
  and comparison to a bare stdlib Unix HTTP stream on the same host/toolchain.
  Warm pools separately. SDK allocations must not scale with read-call count,
  page count, or object length; live memory scales with configured concurrency
  and fixed scratch/framing buffers. Verify no 16 MiB SDK allocation in profiles.
  Use long steady-state streams to demonstrate a plateau, and run with slow
  destinations to verify backpressure rather than growing buffering. Record any
  throughput regression above 10% versus the same-buffer baseline for review;
  no hardware-independent absolute throughput promise is made.
- Verify registry 206 boundaries and complete-200 fit rules, no per-page HEAD on
  the production adapter, no open-ended tail drain, consumer digest rejection,
  typed metadata across peer/disk recovery, and progressing large Rust remainders
  without refilling same-page retry budgets. Exercise deployed cold/warm pulls,
  resumes, concurrency, slow readers, restart/recovery, and failure paths with
  origin request/byte accounting. Scripted UDS results are not this E2E evidence.
- Run `make fmt`, focused Go tests/race tests, and project lint for implementation
  commits. Run benchmarks with `go test ./pkg/racersdk -run '^$' -bench . -benchmem`.
  Documentation examples with Output run as Go tests; deployment-only examples
  are compile-checked without requiring Rust or `/run/racer` permissions.
# Throughput integration addendum

`ClientConfig.PageWindow > 1` enables bounded concurrent page continuations with
ordered delivery, cancellation, and shared connection admission. Zero retains
the single pinned-remainder default described below. FD sinks may use Linux
splice while updating the custom pool's body accounting; unsupported or wrapped
bodies use the bounded copy path. See `racer-throughput-integration.md` for final
compatibility decisions and verification.
