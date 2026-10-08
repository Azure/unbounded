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

Both modes use `pkg/racersdk`. Racer creates one pair of sockets per cache
under `/run/racer/<cache>/`, one for clients and one for the origin.

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
`--metadata-ttl` (default 30s). It refuses delete markers and objects with a
`Content-Encoding` other than identity, because the SDK has no way to pass
that header on.

**Pages.** Racer asks for pages of 16 MiB. The origin sends a ranged GET with
`If-Match` set to the ETag from the metadata. It then checks the reply: ETag,
length, `Content-Range`, content type, version id, and encoding must all
match. The body goes straight to the SDK, which fails the read if the body is
short or too long.

**Changed objects.** If the object changed between the HEAD and the GET, the
ETag no longer matches and the origin returns `ErrVersionMismatch`. It checks
this itself and does not rely on the store honoring `If-Match`. A pinned
object that has since been deleted is also a version mismatch, not "not
found".

**Errors.** S3 error codes and HTTP status codes are mapped to `racersdk`
errors (not found, forbidden, unauthorized, version mismatch, unavailable).
The error message is a fixed "S3 request failed". The upstream error is kept
as the wrapped cause, so `errors.Is` still works, but its text is not shown.

**Setup.**

- Credentials and region come from the normal AWS SDK config chain.
  `--region` is required. `--endpoint` and `--path-style` (default on)
  support S3-compatible stores.
- Checksums are sent only when S3 requires them. Some S3-compatible stores
  reject the optional ones.
- `--request-timeout` is capped at one minute, which is the SDK's own limit
  for an origin call.
- Before it creates its socket, the origin walks the path to
  `/run/racer/<cache>/origin` one directory at a time without following
  symlinks. It refuses any directory that is group- or world-writable or
  owned by another user. This stops another local user from swapping the
  socket path.
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
- Query: only `versionId` and `x-id` (`GetObject` or `HeadObject`). Anything
  else returns 501.
- Headers: `If-Match`, `If-None-Match`, and a single byte `Range` are
  supported. `If-Modified-Since`, `If-Unmodified-Since`, `If-Range`, and
  unknown `x-amz-*` headers return 501, so the client never gets a full body
  when it asked for something the sidecar cannot do. Signing headers such as
  `x-amz-date` and `x-amz-content-sha256` are ignored, so standard S3 SDKs
  work unchanged.

**Flow.**

1. Stat the object in Racer.
2. Check `If-Match` (strong compare, 412) and `If-None-Match` (weak compare,
   304).
3. Parse the range. Open and suffix ranges are supported and clamped to the
   size. A range that starts past the end returns 416.
4. HEAD returns 200 with headers, even for a ranged HEAD. This matches S3.
5. GET asks Racer for the range, pinned to the ETag from step 1. If the size
   or content type changed, it returns 502.
6. The body is written with `WriteTo`.

**Copying with splice.** For HTTP/1, `WriteTo` moves bytes from the Racer
socket to the TCP socket with `splice()`, so the data does not pass through
user space. This only works if the TCP connection still exposes
`io.ReaderFrom`. `netutil.LimitListener` wraps connections and hides it, so
`listener.go` has its own connection limit (128) that keeps it.

**Errors.** Errors use S3's XML shape with fixed text:
`<Error><Code>...</Code><Message>...</Message></Error>`. SDK errors map to
400, 403, 404, 412, 416, 503 (unavailable, canceled, timed out, or closed),
and 502 for anything else. Once a non-empty body has started, a failure
aborts the connection instead of adding an error to the end. The client then
sees a short read, not a body with XML stuck on the end. The SDK holds back
the last byte until Racer confirms the range is complete, so a broken read
can never look like a finished one.

**Timeouts and limits.**

- `--request-timeout` (default 5m) bounds each request. The write timeout is
  that plus 5s, so the handler's own deadline fires first and the client gets
  a clean error.
- Header read timeout is the smaller of 5s and the request timeout. Header
  size is capped at 32 KiB. Idle connections close after 30s.
- Shutdown waits up to 5s for open requests.

## Bucket allowlist

`--bucket` may be repeated in both modes. If set, other buckets are refused:
403 in the sidecar, `ErrForbidden` in the origin. If not set, all buckets are
allowed. The origin checks on its own too, so the rule holds for any client
of the cache, not only this sidecar.

## Packaging and tests

`images/racer-object/Containerfile` builds one image with the binary at
`/usr/local/bin/racer-object`. The pod picks the mode with its command.

Tests use a fake S3 server (`origin_test.go`) and `httptest` with
`racersdktest` (`sidecar_test.go`). `main_test.go` and `listener_test.go`
cover flags, the listen address, the directory checks, and the connection
limit.
