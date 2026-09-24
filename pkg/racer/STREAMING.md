# Streaming SDK integration

## Request origin data and metadata

Create one `Client` for the cache UDS. Derive a request-local view with
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
Use `errors.As(err, &status)` with `var status *racer.HTTPError` to obtain
`StatusCode`, `WWWAuthenticate` (1,024 bytes), and `RetryAfter` (128 bytes).
Invalid/duplicate/oversized fields produce `ErrProtocol`; values are never
truncated. Origin data is never included in SDK error text.

## Independent resource controls

`ClientOptions` separates three limits:

| Option | Zero/default | Scope |
| --- | --- | --- |
| `Concurrency` | 8 | Page workers per `Download` or `ReadAt` operation. Sequential streams issue one page request at a time. |
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

There is no prefetch option yet. The internal admission control provides a
nonblocking `tryAcquire` for future one-page speculation: skip prefetch when no
permit is immediately available, and transfer the acquired permit to the request
without acquiring again. Speculation must never wait for capacity while holding
a foreground response's permit.

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

`Object.StreamVerified`, `racer.ErrDigestMismatch`, and
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

Return `*HTTPError` to preserve upstream HTTP errors and bounded challenge/retry
fields. `ErrVersionChanged`, `fs.ErrNotExist`, and `fs.ErrPermission` also map to
412, 404, and 403. Origin aborts short streaming responses rather than completing
them successfully. Existing local-file adapters can keep using `Store`,
`Source` (`ReaderAt` plus `Close`), and `NewOrigin`. `Store.Stat` has the same
signature above and `Store.Open` accepts
`Open(ctx context.Context, target, etag string, originData []byte) (Source, error)`.
