# Streaming SDK integration

## Request origin data and metadata

Create one `Client` for the cache UDS. Derive a request-local view with
`WithOriginData(value []byte) (*Client, error)`. Views share the HTTP and streaming
connection pools and copy the input. Nil or empty input removes origin data.
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

## Sequential reads and explicit splice

- `object.Stream(ctx) (*Stream, error)` reads the whole pinned snapshot without
  verifying a content digest.
- `object.ReadRange(ctx, offset, length) (*Stream, error)` reads an exact pinned
  interval without verifying a content digest.
- `object.StreamVerified(ctx, expectedSHA256 [32]byte) (*Stream, error)` reads
  the whole object and verifies an independently supplied SHA-256 digest.
- `stream.Prepare() error` opens and validates the first GET response headers
  before the caller commits downstream HTTP headers. It consumes no payload.
- `stream.Read`, `stream.WriteTo`, `stream.Close`, and `stream.Stats` expose
  consumption, cancellation, and observed syscall traffic.

Streams fetch aligned 64 MiB pages sequentially with `If-Match`. Payload scratch
space is bounded independently of page/object size: a pooled 32 KiB buffer,
8 KiB socket/header buffers, and, on Linux, one pipe or two for verification.
There are no per-page payload allocations. HTTP request/response metadata still
allocates. Close every stream, including ones abandoned after Prepare.

`WriteTo` explicitly calls `splice(2)` for concrete `*net.TCPConn` and
`*net.UnixConn` destinations on Linux. The header reader's buffered payload
prefix is drained first. `Stats().SpliceCalls`/`SpliceBytes` measure actual
syscalls/forwarded bytes; `TeeCalls`/`TeeBytes` measure verification duplication.
Read-ahead bytes and portable copies appear in `BufferedBytes`.

For a Gantry HTTP/1 mirror, prepare the stream, hijack the response connection,
write status and headers (including exact Content-Length and Content-Type),
flush the hijacker's buffered writer, then call `stream.WriteTo(conn)`.
The mirror owns the hijacked connection and request framing. Gantry serves one
response per connection with `Connection: close`.
Close that connection on any transfer/verification error. Do not pass the
buffered writer or `http.ResponseWriter` when splice is required. TLS writers
and non-Linux platforms use bounded userspace copies.

Verified splice duplicates pipe buffers with `tee(2)` and hashes a bounded
userspace copy. Forwarding remains socket-to-pipe-to-socket splice. The final
payload byte is withheld by `WriteTo` until SHA-256 succeeds. A mismatch returns
`ErrDigestMismatch`, leaving a framed HTTP body incomplete. Already delivered
bytes cannot be recalled. For empty objects call Prepare before headers so a
wrong empty-object digest is rejected before response completion. `Read` follows
normal Go reader semantics and may return final bytes alongside a digest error;
callers using Read must inspect that error.

Racer's ETag is a version identity, not necessarily SHA-256. To use
`StreamVerified`, supply the expected digest independently. A partial range cannot
establish the full object's digest; verify a complete object separately when that
assurance is required.

### Gantry's integrity boundary

Gantry uses `Stream` for full Racer responses and `ReadRange` for single ranges.
Its former SHA-256 verification tee is removed; `StreamVerified` remains available
to SDK callers. Gantry checks the metadata ETag against the requested OCI digest,
but matching metadata does not prove that the payload hashes to that digest.
Containerd's expected OCI digest check at commit determines whether downloaded
image content is accepted. HTTP completion and Gantry's
`gantry_racer_stream_total{outcome="completed"}` report forwarding only. The former
`verified` outcome is replaced by `completed`; Racer forwarding no longer emits
`digest_mismatch`. Gantry's `gantry_racer_tee_calls_total` and
`gantry_racer_tee_bytes_total` remain exported and are expected to be zero.

Racer retains CRC64/ECMA-182 validation at peer transfer admission and background
disk scrubbing. Origin admission computes a CRC over received bytes, not an OCI
SHA-256 proof; incorrect origin bytes can have a consistent CRC. File-backed
local hits are not rehashed in the foreground. Version pinning and these CRC
checks therefore do not replace end-to-end content validation. Generic SDK/HTTP
consumers must validate content themselves, using `StreamVerified` or their own
check against an independently trusted expected digest before accepting it.

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
