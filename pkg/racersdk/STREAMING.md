# Streaming SDK integration

## Request origin data and metadata

Create one `Client` for the client UDS. Derive a request-local view with
`WithOriginData(value []byte) (*Client, error)`. Views share the HTTP and streaming
connection pools, active request admission, and Linux splice-pipe cache and copy
the input. Nil or empty input removes origin data.
Values can contain arbitrary bytes up to `MaxOriginDataBytes` (65,536 bytes).
Neither client is mutated by deriving a view. HTTP transports use one
`Racer-Origin-Data` header containing canonical padded standard base64; RDMA
carries raw bytes in its encrypted request envelope and falls back to HTTP
when that envelope is too large. `ClientOptions.Header` reserves this header.

Origin data is request-scoped input forwarded on cache misses. It is isolated
between in-flight requests but is not part of persistent cache identity. Data
that changes the tenant or representation belongs in the namespace and target.
Origin data is not per-read authorization: cache hits do not contact the origin.
Treat it as sensitive and never log, persist, or echo it in diagnostics. Gantry
uses `[]byte(registryauth.Authorization(ctx))` and interprets those bytes only
inside its own origin adapter; other origins can define their own payloads.

`view.Open(ctx, target)` performs HEAD and returns an immutable `Object`.
`Object.Metadata()` includes `Size`, `ETag`, optional `ContentType` (256 bytes),
and TTL. GET pages must match HEAD's version and content type, including absence.
Use `errors.As(err, &status)` with `var status *racersdk.HTTPError` to obtain
`StatusCode`, `WWWAuthenticate` (1,024 bytes), and `RetryAfter` (128 bytes).
Invalid/duplicate/oversized fields produce `ErrProtocol`; values are never
truncated. Origin data is never included in SDK error text.

## Independent resource controls

`ClientOptions` separates three limits:

| Option | Zero/default | Scope |
| --- | --- | --- |
| `Concurrency` | 8 | Page workers per `Download` or `ReadAt` operation. Does not control stream prefetch. |
| `MaxIdleConnections` | Effective `Concurrency` | Idle sockets retained **in each** of the net/http and raw streaming pools. Independent of worker count when explicitly set. |
| `MaxActiveRequests` | Unlimited | Shared admission across HEAD, download/read page GETs, raw stream GETs, and every `WithOriginData` view. |

All three reject negative values. Existing zero-valued options preserve the
previous worker, idle, and unlimited-active behavior. For example,
`ClientOptions{Concurrency: 8, MaxIdleConnections: 32, MaxActiveRequests: 16}`
allows eight workers per operation, retains up to 32 idle sockets per pool, and
admits at most 16 requests across all operations combined. Idle capacity neither
limits active requests nor reserves capacity for an operation.

Admission happens before dialing or checking out a raw socket. A permit covers
header parsing, validation, and body consumption, including a paused stream after
`Prepare`. Consumption, abandonment, cancellation, and any dial/header/validation/
body error release it. Streams release admission at page boundaries and acquire
again for the next page. No operation reserves all of its workers' permits at
once. Waiting honors context cancellation; `Timeout` includes admission waiting
in the per-request deadline for net/http and the whole-stream deadline for raw
streams. Always close abandoned streams. As with any bounded client, do not wait
for another request while retaining an unconsumed response that occupies the last
permit: consume or close it first.

With idle capacity I, the two pools retain at most 2I idle upstream socket FDs.
With an active limit A, at most A requests are admitted, including those dialing;
this is a request limit rather than a strict process FD limit (transport dialing
and cleanup can overlap). Worker goroutines remain bounded per operation, not
globally; callers still control the number of concurrent operations. Concurrent
socket `WriteTo` calls can each own a pipe even while waiting for their next page.
The pipe bounds below apply independently of request admission.

Stream prefetch uses nonblocking admission: it skips speculation when no permit
is immediately available and transfers the acquired permit with the prepared
socket, without acquiring again. A limit of one therefore runs sequentially.
At each boundary, the consumed foreground response releases its permit before
waiting for a pending page or acquiring the next foreground permit. Competing
streams do not wait for speculative capacity while retaining current responses.

## Opt-in bounded random-access read-ahead

`Object.ReadAt(ctx, p, off)` remains exact: it fetches only requested bytes,
splits GETs at 64 MiB page boundaries, and retains no payload. A single-page
operation executes synchronously without creating a page worker.

For nearby small reads, explicitly create an independent adapter:

```go
reader, err := object.ReadAhead(256 * 1024) // Maximum retained payload bytes.
if err != nil {
    return err
}
n, err := reader.ReadAt(ctx, p, offset)
```

`Object.ReadAhead(maxBytes int) (*ReadAhead, error)` requires a positive limit.
On a miss, a read whose in-bounds length fits the limit fetches a forward window
starting at the requested offset, clipped at the snapshot's EOF. Subsequent reads
fully within that window use memory, including overlapping or backward reads.
A miss replaces the window; distant/random reads can waste up to the window's
unused bytes per fill. Windows may cross page boundaries, issuing one GET per
intersected page. Reads larger than the limit bypass the window and fetch exactly
their requested interval, preserving the previous valid window.

The adapter lazily allocates one payload buffer of at most
`min(maxBytes, object size)` bytes and reuses it for every fill. Caller output,
transport buffers, and bounded page-worker metadata are additional. Each adapter
has its own limit; creating multiple adapters multiplies retained memory. It has
no background goroutine, holds no response between calls, and needs no Close;
drop the adapter to release its payload storage to garbage collection.

`ReadAhead.ReadAt(ctx, p, off)` uses an explicit per-call context rather than
implementing `io.ReaderAt`. Concurrent calls on one adapter are serialized;
waiting for access, request admission, and GET I/O are cancellable. There is no
lifetime context or background speculation. All expanded GETs are foreground
work covered by the calling context and the client's existing active-request
limit. Cancellation or failure does not permanently poison the adapter.

The adapter is tied to the original immutable Object and its origin-data view.
Every fill uses the existing `If-Match`, version, content-type, range, and framing
checks. Only a fully successful window is retained. Failed or canceled fills
invalidate the window, including any previously retained bytes overwritten by
the fill. Errors in speculative bytes are reported even when the requested
prefix was completed; returned counts cover only the contiguous requested prefix.
EOF and offset rules match `Object.ReadAt`, including zero-length reads and short
reads at EOF. In-flight cancellation causes and wrapped page errors are preserved.
Cached hits serve the pinned version without revalidation; a miss never refreshes
HEAD or retries a changed version. Open a new Object and adapter for a new version.
As with exact reads, partial ranges cannot verify a full-object digest: protocol
validation rejects detected errors, but cannot detect arbitrary incorrect payload
bytes supplied with valid framing and the pinned ETag.

## Sequential reads and explicit splice

- `object.Stream(ctx) (*Stream, error)` reads the whole pinned snapshot without
  verifying a content digest.
- `object.ReadRange(ctx, offset, length) (*Stream, error)` reads an exact pinned
  interval without verifying a content digest.
- `stream.Prepare() error` opens and validates the first GET response headers
  before the caller commits downstream HTTP headers. It consumes no payload.
- `stream.Read`, `stream.WriteTo`, `stream.Close`, and `stream.Stats` expose
  consumption, cancellation, and observed syscall traffic.

Streams fetch aligned 64 MiB pages sequentially with `If-Match`. Payload scratch
space is bounded independently of page/object size: a pooled 32 KiB buffer,
8 KiB socket/header buffers, and, on Linux, at most one forwarding pipe per
active socket `WriteTo`. A pipe is acquired lazily after the buffered prefix;
empty and entirely buffered transfers create no pipe and issue no pipe syscalls.
New pipes request 1 MiB with best-effort `F_SETPIPE_SZ`, then use `F_GETPIPE_SZ`
to discover their actual capacity. Denied enlargement retains the default
capacity; a failed capacity query closes both FDs and fails the transfer.
Each socket-to-pipe batch is bounded by the remaining page bytes, the actual
capacity, and 1 MiB. The pipe is fully drained before the next batch.
There are no per-page payload allocations. HTTP request/response metadata still
allocates. Close every stream, including ones abandoned after Prepare.

### Opt-in one-page-ahead stream prefetch

Set `ClientOptions{StreamPrefetch: true}` to prepare the next page while the
current page is consumed. The default is **disabled**. This applies to `Stream`
and `ReadRange`, including origin-data views, and starts only after the first
foreground page's headers validate (`Prepare`, `Read`, or `WriteTo`). It does not
change `ReadAt`, `ReadAhead`, or `Download`.

Each stream has at most one speculative GET, on a separate raw connection, and
one background header-preparation goroutine. The next aligned page is clipped to
the requested interval, including a partial last page. There is no page-sized Go
body buffer: preparation stops after parsing and validating headers, with only
the socket reader's bounded 8 KiB read-ahead. Unconsumed data can occupy kernel
socket buffers. Current and pending responses together own at most two upstream
sockets and two admission permits per stream; the shared `MaxActiveRequests`
limit still applies. Prefetch allocates no additional splice pipe.

This is speculative **upstream work**: the GET can cause a cache miss, origin
fetch, or cache fill of the next page (up to 64 MiB), even if the caller closes
before reaching it. Closing the socket cannot undo work already dispatched
upstream. Enable it only when this tradeoff is appropriate.

Consumption remains ordered. Every prepared page uses the same immutable
`If-Match`, ETag, content-type, range, and framing validation as foreground
pages. Next-page dial/header/validation errors are deferred to that page's
boundary and do not truncate the successful current page. Body errors are
reported when consuming that page. Cancellation and the whole-stream timeout
still interrupt both connections immediately; prefetch does not extend deadlines.
No speculative page starts its own successor: the stream must adopt it first.

Error, cancellation, and `Close` release both responses and permits, including
cancellation between admission and socket handoff. `CloseIdleConnections` also
discards pending speculation without interrupting the current foreground page;
the discarded page is fetched in the foreground when needed. A concurrent
adoption makes that page foreground, so idle cleanup leaves it running. New
prefetches can start after cleanup; the client remains usable.

`WriteTo` explicitly calls `splice(2)` for concrete `*net.TCPConn` and
`*net.UnixConn` destinations on Linux. The header reader's buffered payload
prefix is drained first. `Stats().SpliceCalls`/`SpliceBytes` measure actual
syscalls/forwarded bytes. Read-ahead bytes and portable copies appear in
`BufferedBytes`.

For a Gantry HTTP/1 mirror, prepare the stream, hijack the response connection,
write status and headers (including exact Content-Length and Content-Type),
flush the hijacker's buffered writer, then call `stream.WriteTo(conn)`.
The mirror owns the hijacked connection and request framing. Gantry serves one
response per connection with `Connection: close`.
Close that connection on any transfer error. Do not pass the
buffered writer or `http.ResponseWriter` when splice is required. TLS writers
and non-Linux platforms use bounded userspace copies.

Forwarding uses socket-to-pipe-to-socket splice without a hashing tee or a
withheld final byte. Empty streams complete without issuing a GET. Racer's ETag
is a version identity, not necessarily SHA-256. A partial range cannot establish
the full object's digest; verify a complete object separately when that assurance
is required.

### Migration: inline verification removed

`Object.StreamVerified`, `racersdk.ErrDigestMismatch`, and
`TransferStats.TeeCalls`/`TeeBytes` have been removed. Replace `StreamVerified`
calls with `Stream` and move any required digest verification to the consumer,
using an independently trusted expected digest before accepting the content.
Callers that implement their own hashing must own mismatch errors and any
buffering or response-completion policy; `Stream` does not withhold bytes or
report digest mismatches. Remove SDK tee-stat consumers. Gantry's compatibility
tee metrics remain exported at zero.

### Gantry's integrity boundary

Gantry uses `Stream` for full Racer responses and `ReadRange` for single ranges.
The SDK performs no inline SHA-256 verification. Gantry checks the metadata ETag
against the requested OCI digest, but matching metadata does not prove that the
payload hashes to that digest.
Containerd's expected OCI digest check at commit determines whether downloaded
image content is accepted. HTTP completion and Gantry's
`gantry_racer_stream_total{outcome="completed"}` report forwarding only. The former
`verified` outcome is replaced by `completed`; Racer forwarding no longer emits
`digest_mismatch`. Gantry's `gantry_racer_tee_calls_total` and
`gantry_racer_tee_bytes_total` remain exported at zero for compatibility.
Gantry's direct-origin fallback independently verifies SHA-256 and withholds its
final byte until verification succeeds.

Racer retains CRC64/ECMA-182 validation at peer transfer admission and background
disk scrubbing. Origin admission computes a CRC over received bytes, not an OCI
SHA-256 proof; incorrect origin bytes can have a consistent CRC. File-backed
local hits are not rehashed in the foreground. Version pinning and these CRC
checks therefore do not replace end-to-end content validation. Generic SDK/HTTP
consumers must validate content themselves against an independently trusted
expected digest before accepting it.

Known incorrect cached origin bytes require source repair and explicit cache
generation recovery. A missing downstream commit signal alone does not establish
corruption or trigger global invalidation. See the
[Gantry operator recovery guide](../../docs/content/guides/gantry.md#recover-from-known-incorrect-cached-content)
for activation barriers and retained containerd ingest handling.

### Cancellation and connection reuse

Context cancellation and Close interrupt upstream socket I/O and downstream
splice. The client's Timeout bounds an entire sequential stream. Caller-set
downstream deadlines remain effective. A canceled splice sets the downstream
write deadline to the present; close the downstream after failure. Arbitrary
non-socket writers must provide their own cancellation for a blocked Write.
Only fully consumed valid responses are pooled. Failed/abandoned responses are
closed. Streams do not automatically retry stale idle sockets or version errors.

Successful socket transfers return only drained pipes to an explicit shared
cache holding at most `min(effective MaxIdleConnections, 8)` pipes (zero idle
capacity uses effective `Concurrency`, which defaults to 8). Each pipe owns two
FDs and its kernel-reported capacity; the 1 MiB request is a target, not an
assumption about host limits or granted capacity.
Cached pipes contain no payload, though their capacity counts toward kernel pipe
quotas. Active transfers are not limited by this cache or by `Concurrency`;
`MaxActiveRequests`, when set, limits their upstream requests:
with A active socket transfers and I idle pipes, pipe FDs are bounded by
`2 * (A + I)`. Pipe kernel capacity is the sum of those pipes' actual capacities.
Error, cancellation, abandonment, nonempty, and overflow paths close their pipes.
Streams abandoned before `WriteTo` acquire no pipe; concurrent `Stream.Close`
cancels a running `WriteTo`, whose cleanup closes its pipe before returning.

Call `Client.CloseIdleConnections` on the base client or any origin-data view to
release cached pipe FDs deterministically. Checked-out pipes keep working but
are closed when returned rather than repopulating that cache generation. New
transfers can acquire and cache pipes afterward; the client remains usable.
Unreachable pipe owners also have runtime cleanup as a nondeterministic backstop,
not a replacement for explicit idle cleanup. There is no `sync.Pool` of FDs.

## Positional file downloads

Use `Object.DownloadFile(ctx, dst *os.File, dstOffset int64) (int64, error)`
to download an opened snapshot into a regular file. The convenience method
`Client.DownloadFile(ctx, target, dst, dstOffset) (Metadata, error)` performs HEAD
first. Both use parallel aligned page GETs, bounded by `Concurrency` and shared
`MaxActiveRequests`, including origin-data views. Each page uses the raw stream
pool, existing version/framing checks, and its own timeout including admission.
There is no additional stream prefetch beyond the scheduled pages.

For sequential intervals, use `Object.ReadRange` followed by
`Stream.WriteToFile(dst, dstOffset) (int64, error)`. This consumes the remaining
stream, supports `Prepare`, and exposes `Stream.Stats()`. Linux drains header
read-ahead with `WriteAt`, then uses socket-to-pipe-to-file `splice` with explicit
file offsets. Neither path calls Seek on the destination or moves its cursor;
parallel pages can safely share one `*os.File`. Splice calls and successfully
written file bytes appear in `SpliceCalls` and `SpliceBytes`.

Non-Linux systems use bounded `WriteAt` copies. Unsupported Linux syscalls
(`ENOSYS`, `EINVAL`, `EOPNOTSUPP`, `EXDEV`) switch to that same copy path, first
draining any pipe bytes at the exact next unwritten offset. Bytes already written
are never repeated and bytes already consumed from the socket are never lost.
Other errors are returned, without retrying the request. Pipes use the existing
bounded shared cache and `CloseIdleConnections` lifecycle. Memory is bounded by
per-worker transport buffers, one pooled 32 KiB copy buffer, and one pipe;
page-sized userspace allocations are unnecessary.

The caller retains ownership of the writable regular destination file. Keep it
open until return, avoid overlapping concurrent writes, and do not open it with
`O_APPEND`. Negative/overflowing destination intervals are rejected. Downloads
never truncate, close, or sync the file; existing bytes outside the interval
remain intact. A successful count equals the requested length. On failure the
parallel download count sums actual writes across pages and may include holes;
`WriteToFile` counts a contiguous written prefix. Partial output remains in place.
Short responses return `io.ErrUnexpectedEOF`, short writes return
`io.ErrShortWrite`, and protocol/version errors retain their existing identities.
Socket waits and admission honor cancellation. Regular-file syscalls cannot be
interrupted by context; cancellation is checked between them. No content digest
is verified. Generic `Download(io.WriterAt)` remains available.

## Range-oriented origins

`NewRangeOrigin(store RangeStore)` adapts:

```go
Stat(ctx context.Context, target string, originData []byte) (Metadata, error)
OpenRange(ctx context.Context, target, etag string, offset, length int64, originData []byte) (io.ReadCloser, error)
```

`OpenRange` runs once per accepted GET after conditional/range evaluation. A
registry adapter can issue one upstream Range request and return its validated,
version-pinned body directly. Copy-buffer reads never trigger extra registry
requests. Honor context cancellation, return exactly length bytes, and release
upstream resources on Close. An accepted empty GET has length zero. HEAD calls
only Stat. Both methods receive decoded origin data explicitly, with no context
values. The ordinary context carries cancellation and deadlines. Do not mutate
or retain the request's origin data. Missing or empty data means absent.
Malformed, duplicate, noncanonical, or decoded-oversize data returns 400 before
calling the store; an encoded value over 87,384 bytes returns 431.

### Request-scoped metadata resolution

A `RangeStore` may additionally implement `ResolvedRangeStore`:

```go
ResolveRange(ctx context.Context, target string, originData []byte) (ResolvedRange, error)

// ResolvedRange:
Metadata() Metadata
OpenRange(ctx context.Context, offset, length int64) (io.ReadCloser, error)
Close() error
```

`NewRangeOrigin` detects this capability and uses it instead of the separate
`Stat`/`OpenRange` calls. Resolve once per request without reading payloads;
the handle's immutable metadata drives preconditions and range decisions.
HEAD, failed preconditions, 304, and 416 responses never open payloads. Accepted GETs call the
handle's `OpenRange` once, including zero-length GETs. The handle must pin the
resolved representation atomically when opening, or return `ErrVersionChanged`;
resolution alone does not relax the immutable ETag contract.

Origin closes a successful body before closing the handle. Every successful
resolution is closed exactly once, including invalid metadata, rejected
preconditions/ranges, open failures, cancellation, and interrupted responses.
Return a non-nil handle/body on success. A handle or body returned with an error
remains owned by the implementation. Store resolution must support concurrent
requests; methods on each handle are called serially.

Only this capability permits retaining decoded origin data in the handle until
`Close`. Release it then, and never mutate, log, persist, or share it across
requests. This supports registry metadata, resolved URL kinds, and delegated
authorization without a global metadata or credential cache. Existing `Store`
and `RangeStore` implementations need no changes.

Return `*HTTPError` to preserve upstream HTTP errors and bounded challenge/retry
fields. `ErrVersionChanged`, `fs.ErrNotExist`, and `fs.ErrPermission` also map to
412, 404, and 403. Origin aborts short streaming responses rather than completing
them successfully. Existing local-file adapters can keep using `Store`,
`Source` (`ReaderAt` plus `Close`), and `NewOrigin`. `Store.Stat` has the same
signature above and `Store.Open` accepts
`Open(ctx context.Context, target, etag string, originData []byte) (Source, error)`.

### Optional pinned file ranges

A successful `Store.Open` source, `RangeStore.OpenRange` body, or resolved
`OpenRange` body may implement:

```go
// racersdk.PinnedFileRange
FileRange() (file *os.File, offset, length int64)
```

The bounds describe the source's exact logical contents within a physical file:
the complete metadata-sized object for `Source`, or the requested interval for
a range body. Origin adjusts a full Source's physical offset for HTTP ranges.
It checks the nonnegative bounds, regular-file type, and available size before
committing success headers. Invalid capabilities produce 500; truncation or I/O
failure during copying aborts the HTTP response. Open errors retain the existing
HTTP mapping, including `ErrVersionChanged` to 412. HEAD and rejected requests
never open a payload. Existing adapters need no changes.

The successful source owns the file and must close it in its `Close`. Origin
closes only the source, once on success, validation failure, cancellation, or
aborted transfer, before closing a resolved handle. Objects returned with open
errors remain adapter-owned. Provide an exclusively owned open-file description
(a duplicated FD shares a cursor and is insufficient). Origin may seek and
advance this source cursor; no other operation may concurrently use or close it.
This ownership differs from destination files, whose cursor is always preserved.

Opening must atomically pin the immutable representation matching the metadata
ETag, or return `ErrVersionChanged`. Metadata and file bytes must identify the
same snapshot. An open FD pins an inode, not immutable contents: prohibit
in-place writes/truncation even after source Close, since kernel network queues
may still reference file pages. Publish new inodes and replace/unlink old paths
rather than modifying old file contents. File stat checks detect invalid bounds but cannot prove version
identity or immutability. The adapter is responsible for that contract.

Go 1.26.6 `io.CopyBuffer` honors `WriterTo`, then `ReaderFrom`, before using its
buffer (`src/io/io.go:407`). Our length-limited reader hides `WriterTo` while
preserving the file's `SyscallConn`. For ordinary cleartext HTTP/1 TCP,
`net/http.response.ReadFrom` copies a small prefix, flushes framing, and delegates
to `TCPConn.ReadFrom` (`src/net/http/server.go:589`). Its sendfile path unwraps a
`LimitedReader` and accepts `syscall.Conn` (`src/net/sendfile.go:24`). This enables
platform sendfile without hijacking HTTP or replacing framing. Native Linux tests
instrument this dispatch and verify bytes bypass the userspace Read path.

It is therefore incorrect to describe all existing `RangeStore` copies as
buffered: a body already exposing an appropriate `SyscallConn` can reach Go's
fast path. The generic `Store` SectionReader/context wrapper hides that capability;
the explicit pin preserves it and makes bounds and cursor ownership deliberate.
Go's HTTP server over Unix sockets does not have TCP's `ReaderFrom` path; Unix
HTTP, TLS, HTTP/2, ResponseWriter wrappers hiding capabilities, and unsupported
platform/filesystem combinations can use bounded userspace copies instead.
Fast-path eligibility is not a guarantee of sendfile for every response.

Portable reads check request cancellation. Pinned transfers also set a write
deadline on cancellation through `http.ResponseController`, interrupting native
HTTP network writes without taking connection ownership. Wrappers must expose
`Unwrap`/deadline support or supply their own blocked-write cancellation. Regular
file I/O itself remains non-interruptible. Configure server write timeouts as for
other origin responses. The original source stays pinned until copying stops.
