# Racer Go SDK

A standard-library-only Go package for Racer volume clients and HTTP origins.
The package name is `racer`; its import path is
`github.com/Azure/unbounded/pkg/racer`. It is part of the root Unbounded Go
module and uses the toolchain and dependency versions declared in `go.mod`.

## Client

```go
import racer "github.com/Azure/unbounded/pkg/racer"

client, err := racer.NewClient("/dev/racer/dataset/cache", racer.ClientOptions{
    Concurrency: 8, // also the default
})
if err != nil { return err }
defer client.CloseIdleConnections()

ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
defer cancel()

file, err := os.Create("weights.partial")
if err != nil { return err }
defer file.Close()

metadata, err := client.Download(ctx, "/models/weights?version=7", file)
if err != nil { return err }
if err := file.Truncate(metadata.Size); err != nil { return err }
// Close/sync and rename the file when appropriate for the application.
```

Every `Client.Download` issues a **separate HEAD**, waits for metadata, then
dispatches concurrent GETs for aligned 4 MiB pages, clipping the last page at EOF.
No GET is needed for an empty object. Each GET uses `If-Match` with the HEAD's
strong ETag. Unexpected statuses, changed validators, incorrect ranges/lengths,
encoded responses, and truncated bodies fail the operation. There is no automatic
retry with newer metadata that could combine versions.

Reuse the client: its dedicated HTTP/1.1 connection pool retains enough idle
connections for the configured parallelism. Every connection uses the configured
filesystem Unix socket; proxy environment variables are ignored and redirects
are disabled. `Timeout` optionally bounds each HTTP request, including its body.
`Header` is copied; protocol-owned headers cannot be overridden. HTTP uses
`Host: localhost`, independently of the socket path. TCP URLs and abstract Unix
sockets are not accepted.

For a `P2PCache` named `dataset`, mount the host directory `/dev/racer/dataset`
into the application at the same path. Mount the directory rather than either
socket inode so a restarted server's socket replacement remains visible. The
cache socket is created by Racer, and the local origin process creates `origin`.
Run the origin on every participating node, serving the same logical dataset.

### Metadata and random access

```go
metadata, err := client.Stat(ctx, "/object") // HEAD only, requires a checksum ETag

object, err := client.Open(ctx, "/object")  // HEAD and pin a representation
if err != nil { return err }
buf := make([]byte, 8<<20)
n, err := object.ReadAt(ctx, buf, 1234)       // parallel, page-boundary-split GETs
// ReadAt follows io.ReaderAt EOF/count semantics, with an explicit context.
// object.Download(ctx, file) reuses this snapshot without another HEAD.
```

Targets are **already-escaped origin-form path/query strings**. No path joining,
cleaning, query sorting, or escaping is performed. `/a%2Fb`, `/a/b`, query order,
repeated keys, and a trailing `?` can identify different cache entries. `Client`
and `Object` support concurrent calls; objects own no connection and need no Close.

Every representation must carry a strong ETag containing exactly 64 lowercase
hexadecimal checksum characters inside double quotes, including empty objects.
Compute the checksum over the complete object (for example SHA-256) at publication.
`Stat` and `Open` reject weak, missing, arbitrary, and noncanonical validators;
the former `AllowUnvalidated` option has been removed. The SDK validates the wire
encoding and pins versions; it does not recompute whole-object checksums on ranged reads.
`errors.Is(err, racer.ErrVersionChanged)` identifies HTTP 412 or a
changed response ETag; reopen explicitly to refresh metadata. `*racer.HTTPError`
exposes status codes and matches `fs.ErrNotExist`/`fs.ErrPermission`.

### Resource and failure behavior

- A transfer creates at most `Concurrency` workers, with dynamic page assignment.
  The limit is **per operation**; applications can bound simultaneous transfers.
- Downloads stream into concurrent, non-overlapping `io.WriterAt` offsets, using
  pooled 32 KiB scratch buffers per worker plus HTTP transport overhead. Memory
  does not grow with object size. `*os.File` is a suitable destination.
- Random reads write directly into disjoint portions of the caller's buffer,
  without page-sized intermediate allocations or per-page completion queues.
- The first failure cancels sibling requests. All workers exit before return.
  Context deadlines cover HEAD and payload work. Use deadlines for bounded body
  reads; the default transport also bounds dialing and response-header waits.
- Destinations can contain partial/out-of-order data on error. Download counts
  include partial writes; ReadAt reports the contiguous completed prefix. Later
  bytes in the supplied buffer may also have changed. Files are neither closed
  nor truncated by the SDK. Use a temporary file for atomic publication.
- WriterAt calls must honor their interface contract. Cancellation cannot interrupt
  a blocked destination WriteAt or a blocked origin ReaderAt implementation.

## Origin server

Implement two storage operations:

```go
type Store interface {
    Stat(context.Context, string) (racer.Metadata, error)
    Open(ctx context.Context, target string, etag string) (racer.Source, error)
}

type Source interface {
    io.ReaderAt
    io.Closer
}
```

`Stat` returns size, canonical checksum ETag, and optional `TTL *time.Duration` without reading
payloads. A nil TTL leaves freshness unspecified; a pointer to zero requests
immediate revalidation. Nonnegative TTLs are emitted as `Cache-Control: max-age=N`,
rounded down to whole seconds. Negative TTLs are invalid. On the client, `Stat`
parses the remaining TTL from Cache-Control and Age: `s-maxage` overrides
`max-age`, Age reduces freshness to a minimum of zero, and `no-cache`, `no-store`,
or `private` forces zero. Missing freshness directives produce a nil TTL;
malformed values and TTLs that overflow `time.Duration` produce `ErrProtocol`.

The handler calls `Stat` on each HEAD or GET request, validating GET
preconditions and ranges before calling `Open`. `Open` returns a **pinned immutable
snapshot** matching the ETag from `Stat`, or `ErrVersionChanged` if that version
is unavailable. Checking the ETag and acquiring the snapshot must be atomic with
respect to replacement. Invalid store metadata yields HTTP 500 before opening a source.
The source exposes the size reported by `Stat` for that version. The handler calls
`Close` exactly once on every exit after a successful open, including disconnected
clients; the store retains ownership of any source returned alongside an error.

Compute content hashes at publication, not on each HEAD. File stores can return
`*os.File` directly: open immutable version files and retain their descriptors until
`Source.Close`. An inode modified in place is not a snapshot. In-memory sources
can implement a no-op `Close`.

```go
origin, err := racer.NewOrigin(store)
if err != nil { return err }
listener, err := net.Listen("unix", "/dev/racer/dataset/origin")
if err != nil { return err }
defer listener.Close()
if err := os.Chmod("/dev/racer/dataset/origin", 0o660); err != nil { return err }
server := &http.Server{
    Handler:           origin,
    ReadHeaderTimeout: 5 * time.Second,
    IdleTimeout:       90 * time.Second,
}
return server.Serve(listener)
```

Provision the parent directory with a shared group and mode `2770`; the origin
process and Racer must belong to that group. Socket mode `0660` supports non-root
applications. The embedding application owns startup and shutdown, including
exclusive ownership and stale-socket recovery after an unclean exit. Never
blindly unlink a path that could belong to a running server.

See [example_test.go](example_test.go) for a complete immutable memory-store
implementation. Store methods must support concurrent calls and respect context
cancellation. Errors matching `fs.ErrNotExist`, `fs.ErrPermission`, or
`racer.ErrVersionChanged` map to 404, 403, and 412; other failures return 500 without
exposing backend details.

The handler and Racer volume listeners share an object-read API. Clients can
switch between them by changing only the Unix socket path:

| Request | Response |
| --- | --- |
| `HEAD /path?query` | 200, Content-Length, mandatory checksum ETag and optional Cache-Control; no payload reads; Range ignored |
| `GET /path?query` | Full object, 200 and Content-Length |
| Single bounded, open-ended or suffix byte Range | 206 with Content-Range and Content-Length; clipped to EOF |
| Unsatisfiable valid Range | Empty 416 with `Content-Range: bytes */size` |
| Malformed, duplicate or multipart Range | Ignored; full 200 |
| Failed strong `If-Match` (including lists) | Empty 412 before If-None-Match or Range processing |
| Matching weak `If-None-Match` | Empty 304; both conditional fields are syntax-checked before evaluation |
| Strong matching `If-Range` | Honor Range; otherwise send full 200 (dates are not evaluated) |

Targets retain their exact path/query bytes, including duplicate slashes, escape
case, encoded slashes, dot segments, query ordering/repeats and a trailing `?`.
Mount the handler directly, without a path-cleaning router. `/metadata` and `/page`
are ordinary object names; `X-Racer-Target` has no routing effect.

This is a breaking replacement of the old two-endpoint origin API: deploy the
origin and dataplane changes together. Racer sends HEAD and aligned page GETs to
the actual target, with `Host: localhost` and `Accept-Encoding: identity`.
There is no legacy fallback on errors.

Clients may omit `If-Match`. Conditional headers retain standard HTTP syntax:
lists, arbitrary opaque tags, weak comparisons for `If-None-Match`, and wildcards
matching any existing representation. These do not relax the representation ETag
contract. Racer always
computes CRC64 over incoming data and stores it for future anti-entropy checks;
origin-supplied `X-Racer-Crc64` headers are ignored. The origin handler
does not buffer whole pages. Source read failures abort the HTTP response rather than
finish a truncated success. No content compression or chunked encoding is used.
Metadata TTL determines how quickly new reads observe updates; immutable pages
are cached under versioned identities. Configure admission, server timeouts,
authentication middleware and graceful shutdown in the embedding application.

Transparency covers binary object reads, not arbitrary HTTP proxy behavior.
Racer reconstructs response headers and does not forward arbitrary client headers,
credentials, cookies or representation variants. Origin authentication must be
arranged separately. Fresh cached metadata can serve an older representation and
evaluate conditions differently from a direct read until its TTL expires; use
immutable targets or a freshness policy appropriate to migration requirements.

## Verification and benchmarks

From the repository root:

```sh
GOTOOLCHAIN=go1.26.6 go test -race ./pkg/racer/...
GOTOOLCHAIN=go1.26.6 go vet ./pkg/racer/...
GOTOOLCHAIN=go1.26.6 go test -run '^$' -bench . -benchmem ./pkg/racer
```

For real Rust dataplane interoperability, build `racer-dataplane` and run on a
Linux host supporting its io_uring/NUMA requirements:

```sh
RACER_DATAPLANE_BINARY=/absolute/path/to/racer-dataplane \
    GOTOOLCHAIN=go1.26.6 go test -race -run TestDataplaneInterop -v ./pkg/racer
```

The opt-in test starts an isolated single-node daemon with signing keys and an SDK origin,
verifies HEAD causes no page reads, checks multi-page cold downloads and warm
cache reuse, and reads across a page boundary. A shared conformance suite runs
against both Unix sockets, covering raw targets, full and ranged reads, empty objects,
conditions, SDK operations and version changes. It fails on daemon startup errors
when the binary is explicitly configured. The regular suite exercises wire
validation, concurrent page dispatch, cancellation, validators, EOF, source
cleanup and failure propagation without external processes.
