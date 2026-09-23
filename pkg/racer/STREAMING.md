# Streaming SDK integration

## Request authorization and metadata

Create one `Client` for the cache UDS. Derive a request-local view with
`WithAuthorization(value) (*Client, error)`. Views share the HTTP and streaming
connection pools. An empty value removes authorization. Nonempty values must
be 1 through 65,536 printable ASCII bytes without boundary whitespace.
Neither client is mutated by deriving a view.

`view.Open(ctx, target)` performs HEAD and returns an immutable `Object`.
`Object.Metadata()` includes `Size`, `ETag`, optional `ContentType` (256 bytes),
and TTL. GET pages must match HEAD's version and content type, including absence.
Use `errors.As(err, &status)` with `var status *racer.HTTPError` to obtain
`StatusCode`, `WWWAuthenticate` (1,024 bytes), and `RetryAfter` (128 bytes).
Invalid/duplicate/oversized fields produce `ErrProtocol`; values are never
truncated. Authorization is never included in SDK error text.

## Sequential reads and explicit splice

- `object.Stream(ctx) (*Stream, error)` reads the whole pinned snapshot.
- `object.ReadRange(ctx, offset, length) (*Stream, error)` reads an exact interval.
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
The mirror owns the hijacked connection, request framing, and keep-alive loop.
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

Racer's ETag is a version identity, not necessarily SHA-256. Supply the OCI
digest explicitly. A partial range cannot establish the full object's digest;
verify a complete object separately when that assurance is required.

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
Stat(ctx context.Context, target string) (Metadata, error)
OpenRange(ctx context.Context, target, etag string, offset, length int64) (io.ReadCloser, error)
```

`OpenRange` runs once per accepted GET after conditional/range evaluation. A
registry adapter can issue one upstream Range request and return its validated,
version-pinned body directly. Copy-buffer reads never trigger extra registry
requests. Honor context cancellation, return exactly length bytes, and release
upstream resources on Close. An accepted empty GET has length zero. HEAD calls
only Stat. Both methods receive credentials through
`AuthorizationFromContext(ctx) string`.

Return `*HTTPError` to preserve upstream HTTP errors and bounded challenge/retry
fields. `ErrVersionChanged`, `fs.ErrNotExist`, and `fs.ErrPermission` also map to
412, 404, and 403. Origin aborts short streaming responses rather than completing
them successfully. Existing local-file adapters can keep using `Store`,
`Source` (`ReaderAt` plus `Close`), and `NewOrigin`.
