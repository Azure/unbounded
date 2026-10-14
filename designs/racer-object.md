# Racer object gateway (`racer-object`)

## Summary

`racer-object` lets normal programs read S3 objects through a Racer cache.
Programs talk plain HTTP in the S3 style. They do not need to know about
Racer's Unix socket API.

One binary has two modes:

- `racer-object origin` fills the cache. Racer calls it on a cache miss. It
  reads the object from S3 (or an S3-compatible store) and hands the bytes to
  Racer.
- `racer-object sidecar` serves the cache. It listens on a loopback HTTP port
  next to the app, asks Racer for the object, and streams it back.

```text
app --HTTP--> sidecar --client socket--> Racer --origin socket--> origin --> S3
```

Both modes use `pkg/racersdk`. Racer serves the client socket under
`/run/racer/<cache>/`. The origin process uses `racersdk.ServeOrigin` to
create and serve `/run/racer/<cache>/origin/socket`; Racer connects to it.
The SDK creates the origin socket with mode `0600`, owned by the origin's
effective user. Racer must run as that user or have permission to bypass
the socket's file permissions.

Code:

- `cmd/racer-object/main.go`: flags, setup, and process lifetime.
- `cmd/racer-object/listener.go`: the sidecar's connection limit.
- `internal/racer/object/origin.go`: cache keys and the S3 origin.
- `internal/racer/object/sidecar.go`: the HTTP handler.

## Cache keys

Each object is named by a small JSON document:

```json
{"schema":1,"namespace":"...","bucket":"...","key":"...","versionId":"..."}
```

The Racer key is the SHA-256 hash of that JSON. The JSON itself travels with
the request as metadata, so the origin knows what to fetch.

- `--namespace` names the upstream store. It is required and must match in
  the origin and the sidecar. Two stores with different data must use
  different namespaces, or they would share cache entries.
- Object keys are used as given. No path cleaning, no case folding, no
  unescaping beyond the single URL decode in the sidecar. `a/../b` and `b`
  are different objects, as they are in S3.
- The origin rebuilds the JSON from its parts and checks that both the bytes
  and the hash match what Racer sent. This means one object has exactly one
  key, and a client cannot ask the origin to fetch something under a key
  that belongs to a different object.
- Limits: namespace up to 256 bytes with no spaces or control characters,
  bucket names use `[A-Za-z0-9._-]` up to 255 bytes, object keys are valid
  UTF-8 up to 1024 bytes.

## Origin

On a miss Racer first asks for metadata, then for pages.

**Metadata.** The origin sends S3 a HEAD request. It returns the size, the
strong ETag, the content type, and an expiry time of now plus
`--metadata-ttl` (default 30s). At expiry Racer must stop serving that version
from cache; the TTL is not just an admission hint. Zero expires metadata
immediately. Supported content types are normalized for the SDK metadata
format; missing or unsupported values become `application/octet-stream`.
The raw HEAD and GET content types must still match.
Delete markers are reported as not found. S3 itself answers a delete marker
with an error: 404 when it is the current version, and 405
`MethodNotAllowed` when its version id is requested explicitly; the origin
maps both to not found. A successful reply flagged as a delete marker is
refused as an invalid response. The origin also refuses objects with a
`Content-Encoding` other than identity, because the SDK has no way to pass
that header on.

**Pages.** Racer asks for pages of 16 MiB. The origin sends a ranged GET with
`If-Match` set to the ETag from the metadata. When the request names no
version but HEAD returned a version id, the GET is pinned to that version id,
so an overwrite with identical bytes cannot fail the version check. It then checks the reply: ETag,
length, `Content-Range`, content type, version id, and encoding must all
match. The body goes straight to the SDK, which fails the read if the body is
short or too long.

**Changed objects.** If the object changed between the HEAD and the GET, the
ETag no longer matches. If Racer pinned an ETag, the origin returns
`ErrVersionMismatch`. If Racer did not pin one (a bootstrap read), the ETag
came from the origin's own HEAD, so the origin returns `ErrUnavailable`
instead: a retry sees the new version, and Racer never gets a 412 for an ETag
it did not ask for. The origin checks this itself and does not rely on the
store honoring `If-Match`. A pinned object that has since been deleted is also
a version mismatch, not "not found".

**Errors.** S3 error codes and HTTP status codes are mapped to `racersdk`
errors (not found, forbidden, unauthorized, version mismatch, unavailable).
Network operation and DNS failures map to unavailable when no API or HTTP
status classification applies. Cancellation and deadlines take precedence.
The error message is a fixed "S3 request failed". The upstream error is kept
as the wrapped cause, so `errors.Is` still works, but its text is not shown.

**Setup.**

- Credentials and region come from the normal AWS SDK config chain.
  `--region` overrides the configured region. A region must be available
  through `--region` or AWS configuration, such as `AWS_REGION` or
  `AWS_DEFAULT_REGION`. `--endpoint` and `--path-style` (default on) support
  S3-compatible stores. The effective endpoint, including one from AWS
  environment or shared configuration, must be an HTTP(S) origin without
  credentials, a path prefix, query, or fragment. `--endpoint` takes precedence.
- The origin uses the AWS SDK's HTTP transport so SDK transport settings,
  including configured CA bundles, still apply. Its idle connection pool
  has room for 64 page requests and four metadata requests.
- Checksums are sent only when S3 requires them. Some S3-compatible stores
  reject the optional ones.
- `--request-timeout` is capped at one minute, which is the SDK's own limit
  for an origin call.
- Before it creates its socket, the origin walks the path to
  `/run/racer/<cache>/origin` one directory at a time without following
  symlinks. Missing directories are created with mode `0755`, subject to
  umask; existing permissions are not changed. It refuses any directory
  that is group- or world-writable.
  Ancestors must be owned by root or the process's effective UID; the final
  `origin` directory must be owned by the effective UID. This stops another
  local user from swapping the socket path.
- The origin does no caching of its own. Racer owns all caching.

## Sidecar

The sidecar is a small, read-only S3 endpoint for one app.

**Listening.** `--listen` must be a loopback IP literal (default
`127.0.0.1:8080`). The sidecar does not check SigV4 signatures, and it does
not forward them. Access control is the origin's job and the bucket
allowlist's. Keeping the port on loopback means only local processes can
reach it.

**Requests.** Only GET and HEAD are allowed, using path-style URLs
(`/bucket/key`).

- The path is split into bucket and key before it is decoded, and decoded
  once.
- Query: only `versionId` and a method-matching `x-id` (`GetObject` or
  `HeadObject`). Malformed encoding, empty values, and repeated parameters
  return 400; for example, `?acl` and `?versionId=` return 400.
  Well-formed unsupported options, such as `?partNumber=1`, return 501.
- Headers: `If-Match`, `If-None-Match`, and a single byte `Range` are
  supported. A single `x-amz-checksum-mode: ENABLED` is accepted for GET and
  HEAD as a request for available checksums. The sidecar has no S3 checksum
  metadata, so it returns no checksum headers, including for ranges and empty
  objects. The default AWS SDK accepts this response without checksum
  validation. Empty, repeated, or unknown checksum modes return 501.
  `If-Modified-Since`, `If-Unmodified-Since`, `If-Range`, and unknown `x-amz-*`
  headers still return 501, including owner checks, SSE-C, and requester-pays.
  Signing headers such as `x-amz-date` and `x-amz-content-sha256` are ignored.

**Flow.**

1. Stat the object in Racer.
2. Check `If-Match` (strong compare, 412) and `If-None-Match` (weak compare,
   304).
3. Parse the range. Open and suffix ranges are supported and clamped to the
   size. The `bytes` unit is case-insensitive. Unknown range units are
   ignored, so the full object is selected. Malformed byte ranges and
   multiple ranges return 400. A range that starts past the end returns 416.
4. HEAD returns 200 with headers, even for a ranged HEAD. This matches S3.
5. GET asks Racer for the range, pinned to the ETag from step 1. If the size
   or content type changed, it returns 502. An internal pin failure returns
   503, not 412: only a failed client `If-Match` condition returns 412.
6. The body is written with `WriteTo`.

**Copying with splice.** For HTTP/1, `WriteTo` moves bytes from the Racer
socket to the TCP socket with `splice()`, so the data does not pass through
user space. This only works if the TCP connection still exposes
`io.ReaderFrom`. `netutil.LimitListener` wraps connections and hides it, so
`listener.go` has its own connection limit (128) that keeps it.

**Errors.** Application errors use S3's XML shape with fixed text:
`<Error><Code>...</Code><Message>...</Message></Error>`. SDK errors map to
400, 403, 404, 416, 503 (internal pin failure, unavailable, canceled, timed
out, or closed), and 502 for anything else. Once a non-empty body has started, a failure
aborts the connection instead of adding an error to the end. The client then
sees a short read, not a body with XML stuck on the end. The SDK holds back
the last byte until Racer confirms the range is complete, so a broken read
can never look like a finished one.

Go's HTTP parser can reject a request before the application runs, including
malformed HTTP and headers beyond its bounded parser limit. Those responses
use Go's error format, not S3 XML, and may not follow the application's
body-free HEAD error behavior. Raising the parser limit only moves this
boundary; it does not make every parser error an application XML error.

**Timeouts and limits.**

- `--request-timeout` (default 5m) bounds each request. The write timeout is
  that plus 5s, so the handler's own deadline fires first and the client gets
  a clean error.
- Header read timeout is the smaller of 5s and the request timeout. Header
  size is capped at 32 KiB. Idle connections close after 30s.
- Shutdown stops accepting connections and lets open requests drain for up
  to 5s without canceling them first. After that, it cancels remaining
  requests and closes their connections.

## Bucket allowlist

`--bucket` may be repeated in both modes. If set, other buckets are refused:
403 in the sidecar, `ErrForbidden` in the origin. If not set, all buckets are
allowed. The origin checks on its own too, so the rule holds for any client
of the cache, not only this sidecar.

## Packaging and tests

`images/racer-object/Containerfile` builds one image with the binary at
`/usr/local/bin/racer-object`. The pod picks the mode with its command.
Publication is experimental: use the generic image workflow with a manual
dispatch for `racer-object` or an `images/racer-object/<version>` tag. This
does not add the image to the regular release or nightly publication sets.

Tests use a fake S3 server (`origin_test.go`) and `httptest` with
`racersdktest` (`sidecar_test.go`). `main_test.go` and `listener_test.go`
cover flags, the listen address, the directory checks, and the connection
limit.
