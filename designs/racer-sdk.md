# Racer Go SDK

## Scope and evidence

This document describes the replacement subscription implementation, including
the Go SDK, its Gantry integration, and Rust page delivery. It supersedes client
bootstrap/remainder GETs, concurrent continuation windows, and SDK splice claims.
The client change is breaking, with no dual body-read protocol. Origin v1 is unchanged.
Implementation citations are repository-relative unless noted otherwise.

`pkg/racersdk` is a standard-library-only concurrent streaming client and
single-callback origin server. See the normative
[client v2/origin v1 contract](../cmd/racer-dataplane/CLIENT_ORIGIN_API.md),
[package guide](../pkg/racersdk/doc.go), and
[examples](../pkg/racersdk/example_test.go). SDK packages must not import `cmd/`.

The [verification summary](racer-sdk-verification.md) records earlier passing
gates, including deployed E2E, for the previous client contract. It and the
[independent conformance report](racer-sdk-conformance.md) are historical evidence,
not acceptance of the replacement wire protocol. Current process, scripted
conformance, and E2E client fixtures use v2 subscriptions. The separate live
Go/Rust harness exercises production ClientListeners and the read graph, including
a 512 MiB + 13 Get and DownloadTo under bounded payload admission. The
[subscription design](racer-hot-subscriptions.md#evidence-and-validation-scope)
records the actual assertions and limits. The
[ordered validation record](racer-ordered-validation-20260929.md) records the
independent live race rerun and separates fixture benchmarks from deployed results.
Historical measurements remain in [throughput performance](racer-sdk-throughput-performance.md).
Scripted wire checks, live single-node interoperability, deployed coverage, and
workload-specific performance measurements are distinct evidence.

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
    PageCredits int
    ByteCredits ByteLength
    Ordered     bool
    SmallObject bool
    Offset      ByteOffset
    Length      ByteLength
    Pin         ETag
    Metadata    *Metadata
}

func NewClient(config ClientConfig) (*Client, error)
func (c *Client) OpenPages(ctx context.Context, request Request, options ...ReadOptions) (*PageStream, error)
func (c *Client) DownloadTo(ctx context.Context, request Request, w io.WriterAt, options ...ReadOptions) (int64, error)
func (c *Client) Get(ctx context.Context, request Request, options ...ReadOptions) (*Value, error)
func (c *Client) Stat(ctx context.Context, request Request) (Metadata, error)
func (c *Client) Stats() Stats
func (c *Client) Close() error
func (v *Value) Metadata() Metadata
func (v *Value) Read(p []byte) (int, error)
func (v *Value) WriteTo(w io.Writer) (int64, error)
func (v *Value) Close() error

func (s *PageStream) Metadata() Metadata
func (s *PageStream) Range() (ByteOffset, ByteOffset) // start, exclusive end
func (s *PageStream) Next() (*PageLease, error)
func (s *PageStream) Close() error
type PageLease struct {
    Number uint64
    Offset ByteOffset // absolute object offset
    Data   []byte
    // Private ownership and release state omitted.
}
func (p *PageLease) Release() error

type Origin func(context.Context, OriginRequest) (Metadata, io.ReadCloser, error)
func ServeOrigin(ctx context.Context, config OriginConfig, origin Origin) error
```

- `ParseKey(string) (Key, error)` accepts only canonical lowercase hex; `Key.String()`
  emits it. All 32-byte values, including zero, are valid.
- `CacheName`, `ETag`, `AdapterMetadata`, and `Authorization` have private storage
  and validating `Parse<Type>(string) (<Type>, error)` constructors using the unchanged value grammar.
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
  Client.Get validates its request before I/O. The subscription path also validates
  the serialized outgoing head against the aggregate limit.
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
`pkg/racersdk/range.go:18-75`, `pkg/racersdk/client.go:241-291`, and
`pkg/racersdk/subscription.go:21-170`.

Get and OpenPages accept zero or one ReadOptions value. With no Pin or Metadata,
the POST resolves fresh metadata and selects the requested range in that exchange.
Neither method issues a HEAD preflight. A snapshot is validated and copied and
pins the subscription to its ETag; a nonzero Pin must match. Associate the snapshot
with the correct Request and do not mutate it during opening: Metadata has no key.
Length zero means through EOF. Overflow fails before I/O; an explicit Length past
EOF fails rather than silently shortening. An explicit range starting at EOF is
unsatisfiable, not a locally synthesized empty Value. An un-ranged empty object
still opens a subscription and validates Complete. Stat remains fresh metadata-only
HEAD on its reserved pool (`pkg/racersdk/subscription.go:72-170`,
`pkg/racersdk/client.go:258-291`).

SmallObject reserves a separate pool for objects whose **total** size is at most
PageSize (16 MiB), not for short ranges of larger objects. Oversized snapshots or
subscription response metadata fail before exposing a Value or PageStream. There
is no fallback into the bulk pool (`pkg/racersdk/subscription.go:82-98,163-169`).

There is no whole-object `[]byte` result, SDK page cache, public HTTP client injection,
retry policy, adapter registry, separate metadata callback, or caller-owned server
listener. Internal transport/listener seams support tests.

## Streaming algorithm and lifetimes

For OpenPages or Get:

1. Acquire one bounded bulk or small-object slot under ctx and send one
   `POST /v2/objects/<key>` with zero HTTP request-body length, optional pin/range,
   and page/byte credits. Validate metadata and response framing before returning.
2. Next reads a complete page slice before exposing a PageLease. Each slice has a
   page number and absolute object offset. Unordered is the OpenPages default;
   Ordered selects ascending delivery. Get always sets Ordered and presents the
   same slices through Read, releasing each once consumed. Get receives ahead in
   one background goroutine; OpenPages.Next remains synchronous, even with Ordered.
3. Hold each lease only while using Data, then call Release. It is idempotent and
   invalidates Data. Never copy a lease or access its Data concurrently with Release.
   Release writes the exact page number and slice length back on the same socket,
   returning local credit. Next waits when outstanding leases exhaust credits.
4. Require Complete with exact page/byte counts before clean EOF. The final slice
   is not exposed until its trailing Complete validates. An empty object validates
   Complete without a page. Close on every path, including consumer failure.

The selected page membership and version do not change with delivery order. Credit
defaults are ClientConfig.PageWindow, or two when zero, and PageCredits*PageSize
bytes. Limits are 1..64 pages and PageSize..64*PageSize bytes. Outstanding payload
allocations are credit-bounded; duplicate tracking uses merged intervals with a
hard limit of 4096. Excessive fragmentation fails rather than allocating state
proportional to object size. There is no whole-object buffer, reconnect/resume,
GET fallback, or continuation request (`pkg/racersdk/subscription.go:80-105,358-555`).

Get owns at most `min(2, PageCredits, floor(ByteCredits / PageSize))` payload
buffers across its current lease, queued leases, and in-flight receive combined.
Credits can reduce this to one; larger credits do not raise the SDK read-ahead
limit above two. Storage is allocated lazily, each buffer at most
`min(PageSize, selected range length)`, and reused only after Release within that
request. A short slice exposes only its exact length/capacity. No payload buffer
crosses request boundaries and this is not a page cache. OpenPages and DownloadTo
keep synchronous, per-slice allocation and explicit lease lifetimes; setting
Ordered on OpenPages changes delivery order, not SDK allocation strategy
(`pkg/racersdk/ordered.go:11-73,100-133`,
`pkg/racersdk/subscription.go:35-45,457-517,653-703`).

The receiver validates each complete payload before queuing its lease, and also
validates Complete before queuing the final lease. A later receive failure is
reported after already verified queued slices are consumed, without exposing an incomplete or invalid final
slice. Cancellation and explicit Close instead terminate consumption and join
cleanup. Wire completion alone does not release the live-Value admission slot:
it stays held until consumption ends or cancellation closes the socket, joins
the receiver, releases current/queued leases, and drops reusable buffers. Terminal
Read and concurrent/idempotent Close wait for that cleanup before returning
(`pkg/racersdk/subscription.go:373-494`, `pkg/racersdk/ordered.go:43-98`,
`pkg/racersdk/value.go:57-103,179-214`).

DownloadTo accepts an io.WriterAt, writes at absolute object offsets (including
for a selected range), releases each lease after WriteAt returns, and closes on
all exit paths. It preserves partial counts and reports short writes. It does
not truncate, size, or verify the destination object. Cancellation cannot interrupt
an arbitrary caller-owned WriteAt (`pkg/racersdk/subscription.go:653-703`).

Initial HTTP errors retain their typed classification. Malformed frames are
protocol errors; short payloads or a missing Complete are terminal I/O errors.
No failure selects a newer version. The context bounds the entire subscription.
SDK page buffers are intentional, unlike the old direct-body reader; Get reuses
its bounded payload storage instead of allocating another buffer for every page.
`Value.WriteTo`, also selected by `io.Copy` and `io.CopyBuffer`, uses bounded
32 KiB scratch and does not invoke the destination's ReadFrom. It counts only
bytes accepted by the writer and preserves short-write errors. Copy scratch has
independent bulk and small-object caps, including copies canceled while blocked
inside caller-owned Write. Exhausted scratch returns ErrorUnavailable. Cancellation
cannot interrupt an arbitrary writer; callers must still Close on writer failure
(`pkg/racersdk/value.go:217-335`). Cleanup does not wait for caller-owned Write:
only the separately admitted scratch remains pinned there, not Get's page buffers
(`pkg/racersdk/ordered.go:76-98`). WriteToHTTP uses this validated copy path without
hijacking the downstream connection. SDK subscription delivery does not splice
framed page data directly to a destination (`pkg/racersdk/http_transfer.go:8-14`).

Client is safe for concurrent opening calls, Stat, Stats, and Close. PageStream
has one Next consumer; Release and Close may run concurrently. Value has one consuming
goroutine (Read or WriteTo, directly or through io.Copy); Close may run concurrently and
must cancel a blocked read or credit wait. Metadata access is safe
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
| `ClientConfig.ResponseHeaderTimeout` | 60 seconds; subscription POST write and response head share one deadline; HEAD retains separate write/head bounds |
| `ClientConfig.BodyReadTimeout` | 60 seconds per blocked subscription read/release write, not total lifetime or caller think time |
| `ClientConfig.PageWindow` | Zero selects two default page credits; 1..64 explicitly selects credits, not parallel GET connections |
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
memory, kernel sockets, Rust pages, and SDK PageLease payload allocations. Each
OpenPages subscription can retain up to its negotiated byte credits in live SDK
payloads. Get additionally caps total request-local payload storage at two pages
(32 MiB), reduced by page/byte credits as described above; default credits are
32 MiB, allocated lazily. Connection and concurrency limits bound
the parser buffers, goroutines, and body scratch. At the connection cap, stop
accepting until capacity frees (the OS backlog is finite); an already accepted
well-formed request at the callback cap gets empty 503. A stuck callback continues
to occupy its slot even after its client disconnects, preventing unlimited leaks.

Client uses private Unix connections with a connection-owned bufio.Reader and
bounded raw-head parsing, not http.Transport. Subscriptions use a fresh connection
and `Connection: close`; neither completed nor aborted subscriptions recycle it.
Sequential HEAD exchanges can return clean reusable connections to the metadata
pool. Idle timers close unused pooled connections
(`pkg/racersdk/subscription.go:114-126,151`, `pkg/racersdk/response_conn.go:71-230`). No proxy, redirect,
compression, HTTP/2, or total HTTP client timeout is involved.

`ClientConfig.MaxConnAge` governs reusable pooled connections; subscription
connections are not reused. Zero selects the 5-minute default; negative values are invalid.
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
exchange or restart a subscription
(`pkg/racersdk/response_conn.go:71-155,215-257`; lifecycle and active-response
assertions in `pkg/racersdk/connection_age_test.go:151-221,313-376`).

Rotation creates opportunities for worker reassignment, not a guarantee of equal
worker utilization. See the [real Racer connection-age validation](racer-sdk-connection-age-validation.md)
for the bounded workload results and measurement limitations.

For the sequential HEAD path, one fresh-connection retry is allowed only after EOF, reset, or broken pipe on
a reused connection **before any response byte**. Timeouts, partial heads,
protocol errors, HTTP errors, and body failures are not retried. There is no
version restart or body resume. Callbacks must tolerate replay; network delivery
is not exactly once (`pkg/racersdk/response_conn.go:232-263`). OpenPages does not
use that exchange retry path (`pkg/racersdk/subscription.go:102-127`).

ServeOrigin's ctx is the server lifetime. Each request inherits its cancellation
and the local request timeout; peer disconnect cancels it too. There is no new
deadline wire header: the dataplane closes a canceled origin request, and the
origin timeout independently bounds work. Deadline before headers is 503; after
headers, abort the connection. On server cancellation, stop admission, close
listeners/connections and active bodies, cancel callbacks, and return ctx.Err().
Do not wait forever for a noncooperative callback; close any late-returned body.
Other ServeOrigin errors preserve their bind/serve causes. No grace-period API.

## Rust range progress and bounded work

The production client listener selects Responses::send_subscription for POST.
It prepares the first slice before success headers, enables progress, and holds
delivered plaintext ownership until exact release or stream teardown. Complete
does not wait for final releases (`cmd/racer-dataplane/src/client/subscription.rs:145-179,246-293`,
`cmd/racer-dataplane/src/read/range_stream.rs:270-276`). Rust paths in the rest of
this section are relative to `cmd/racer-dataplane/`.

- Initial metadata uses the coordinator's bounded acquisition budget. Normal
  subscriptions open a per-selection budgeted stream, then configure credits
  (`src/read/serve.rs:216-267`). Local page/byte credits are distinct from the
  cumulative peer transfer ceilings and per-selection attempts/links; releasing
  a client lease never replenishes remote acquisition authority.
- Idle/header reception and initial metadata/first-slice work retain the configured
  request deadline. After successful headers, newly admitted pages in a normal
  client subscription receive fixed child acquisition deadlines. Already admitted
  pages and retries never receive new time or credits. Pending futures stay in
  the stream; dropping a next_slice future does not restart a page. Explicit
  aggregate budgets partition/reunite only their owned credits and cannot be
  upgraded to progressing budgets (`src/read/range_stream.rs:297-405`).
- Ordered subscriptions reserve exact slice credits before dispatching concurrent
  fixed-page acquisitions through the stable page owner and existing singleflight.
  Pending, ready/reordered, and delivered-unreleased pages together fit the smaller
  of the configured range window and page credits; byte credits independently
  bound selected slice bytes. Later completions wait behind the ordered head.
  Acquisition does not hold a delivery pipe while waiting for the head
  (`src/read/range_stream.rs:259-268,297-355,628-658,699-722`).
- Unordered subscriptions retain per-version exclusive provider selection and
  selected-result fanout. They cannot overlap accepted ordered fixed-page work
  for that version. Capacity-bearing polling demands receive tickets: an earlier
  unordered ticket prevents ordered refill, while selection yields to all earlier
  tickets. Ordered work can batch across ordered tickets. Canceling a waiting
  demand wakes successors but does not drop accepted work's completion guards
  (`src/read/subscription.rs:177-342,483-587`). See the
  [subscription design](racer-hot-subscriptions.md#local-demand-and-ownership)
  for the exact arbitration and ownership boundaries.
- While a page frame or payload is being sent, prefetch polls completions and can
  admit new acquisitions within credits, window, budgets, and ticket arbitration.
  It is not limited to polling already admitted futures
  (`src/client/subscription.rs:262-280`, `src/read/range_stream.rs:278-390`).
- Positive client socket writes renew the reader-stall clock. Backpressure does
  not grow the prefetch window. Peer writes still take the minimum of the original
  deadline and stall deadline (`src/memory/delivery.rs:153-205,261-265`).
- Runtime defaults are `RACER_REQUEST_TIMEOUT_MS=30000`,
  `RACER_READER_STALL_TIMEOUT_MS=10000`. Subscription scheduling uses negotiated
  credits; `RACER_RANGE_WINDOW_PAGES=2` also caps the ordered v2 pipeline, not just
  internal range callers. Unordered selection remains credit-driven
  (`src/read/range_stream.rs:259-268`). Increasing SDK concurrency does not increase Rust
  page, pipe, connection, queue, or origin capacity automatically.

This permits progressing client subscriptions to outlive the initial request timeout;
it does not guarantee successful completion. Acquisition failure, deadline,
overload, cancellation, and stalled downstream delivery can still terminate a
stream. Retained peer contracts keep their original deadline and cannot be renewed
by later selections. No unlimited whole-object remote subscription is promised.
Local peer-attempt timeouts reserve time for fixed-page fallback while retaining
the original signed contract deadline and spent budgets. Timeout still waits for
accepted I/O completion before fallback (`src/read/candidates.rs:460-587`). Selected
ciphertext moves to the stable page owner for local admission, plaintext allocation,
authentication, and publication (`src/read/fill.rs:136-190`,
`src/read/dispatch.rs:243-269`).
Submitted work retains buffers and admission until its completion fence. See
[subscription integration](racer-hot-subscriptions.md) for selection and fanout limits.

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
that snapshot to an exact pinned offset Get subscription, avoiding
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

Client HTTP body reads use v2 subscriptions; origin HTTP remains v1 with this
optional response header. Typed peer
responses additionally sign `racer-metadata-version: 2` and `racer-content-type`;
untyped responses retain the old shape. New decoders reject unknown versions,
missing typed values, and unversioned typed fields. Signature/logical agreement
covers MIME metadata (`cmd/racer-dataplane/src/security/protocol.rs:305-307`,
`cmd/racer-dataplane/src/peer/decode.rs:76-99`,
`cmd/racer-dataplane/src/security/forwarding.rs:1422-1448`).

Peer v5 is a coordinated breaking upgrade: older profiles fail closed, regardless
of content type. There is no capability negotiation or silent downgrade. This is
not a claim that a mixed-version deployment has passed end-to-end validation.

The former v1/v2 disk compatibility description is also superseded independently
of subscriptions. Record v4 is the sole accepted/written format, with CRC-64/XZ
and optional content type (`cmd/racer-dataplane/src/store/format.rs:19-20`). See
[store protocol](racer-store-protocol.md) for record/checkpoint compatibility;
neither client API version nor origin v1 determines disk compatibility.

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
trusted snapshot is resolved in the subscription exchange. Errors contain no context header or payload.

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

The original step 2 implementation exposed these package-private seams, still
used for origin v1 and sequential HEAD handling. Client body subscriptions instead
use `subscription.go` for raw heads, frames, credit accounting, and release writes:

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
account for sequential request/response boundaries and read-ahead. The replacement
subscription reader intentionally adds credit-bounded page buffers. Parsed Header maps alone cannot enforce context separator
whitespace or duplicate Content-Length; raw validation remains mandatory even
when semantic validation is also applied to parsed objects.

### Acceptance checks (requirements, not results)

These checks define replacement-subscription verification requirements, not new
passing results. The [dated acceptance summary](racer-sdk-verification.md) and
[throughput performance](racer-sdk-throughput-performance.md) cover the prior
contract and must not be reused as replacement performance or deployed acceptance.

- Table tests and fuzzing for keys/names/ETags, expiry precision/overflow, private
  type zero values, ranges at 0, P-1, P, P+1, and MaxInt64; never panic or allocate
  proportional to claimed size. Verify quoted commas/backslashes and empty ETag.
- Raw Unix tests for exact target/no query, duplicate identical/conflicting lengths,
  whitespace preservation/rejection, 32 KiB and 8 KiB boundaries, request bodies,
  chunking/compression, unknown status/redirect, HEAD/body rules, and empty errors.
  Cover normalization by Go's parser, not just Header.Get-based validation.
- Client request transcripts for empty, short page, exactly one page, multi-page,
  unchanged credentials, and cancellation.
  Verify one POST for full/ranged reads regardless of page count, no HEAD preflight,
  and no GET fallback. Verify Stat is bodyless and empty objects validate Complete.
  Check arbitrary page order, exact membership and slice offsets, duplicates,
  missing/invalid Complete, page/byte credit waits, exact release, and Close races.
  Verify Get forces ordering and DownloadTo uses absolute offsets and releases
  leases on destination errors. Verify Get overlaps receive with consumption,
  bounds all payload buffers to two (or one with reduced credits), reuses only
  released storage, and joins cleanup before returning terminal reads/admission.
  Check independent bulk,
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
  Warm reusable HEAD pools separately. Subscription connections are not reusable.
  Get's cumulative payload-buffer allocation must plateau at its request-local
  one/two-buffer bound; small per-page bookkeeping allocations may still scale
  with pages consumed. OpenPages/DownloadTo allocate per slice. Live payload
  memory must remain bounded by credits and concurrency, plus bounded
  scratch/framing and interval state. Use matched fixture generators: scalar
  address-sensitive generation overhead must not be mistaken for SDK cost (see
  the [benchmark caveat](racer-ordered-validation-20260929.md#fixture-benchmarks)).
  Use long steady-state streams to demonstrate a plateau, and run with slow
  destinations to verify backpressure rather than growing buffering. Record any
  throughput regression above 10% versus the same-buffer baseline for review;
  no hardware-independent absolute throughput promise is made.
- Verify registry 206 boundaries and complete-200 fit rules, no per-page HEAD on
  the production adapter, no open-ended tail drain, consumer digest rejection,
  typed metadata across peer/disk recovery, and progressing large Rust subscriptions
  without refilling same-page retry budgets. Exercise deployed cold/warm pulls,
  resumes, concurrency, slow readers, restart/recovery, and failure paths with
  origin request/byte accounting. Scripted UDS results are not this E2E evidence.
- Run `make fmt`, focused Go tests/race tests, and project lint for implementation
  commits. Run benchmarks with `go test ./pkg/racersdk -run '^$' -bench . -benchmem`.
  Documentation examples with Output run as Go tests; deployment-only examples
  are compile-checked without requiring Rust or `/run/racer` permissions.
## Superseded throughput options

ClientConfig.PageWindow now sets default subscription page credits; zero selects
two, and every subscription still owns one connection. The unused
PrefetchBootstrap field and Gantry configuration setting have been removed. The
old concurrent GET window and SDK splice implementations have been removed. Historical results
in [throughput integration](racer-throughput-integration.md) do not establish the
replacement subscription's throughput (`pkg/racersdk/client.go:238-262`,
`pkg/racersdk/subscription.go:80-105`, `pkg/racersdk/http_transfer.go:8-14`).
