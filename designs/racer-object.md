# Racer object

## Scope

Provide a read-only S3 caching mesh with one Go binary and two subcommands.
Racer owns caching, page placement, peer reads, and retries. This adapter does not
keep its own cache or bypass Racer when a cache read fails.

```
S3 client -> pod loopback sidecar -> Racer client UDS -> Racer mesh
    -> node origin UDS -> S3-compatible upstream
```

The sidecar uses the client UDS, not the origin UDS directly. The latter serves
cache misses and metadata requests from Racer.

## Interface

- `racer-object origin`: serve the SDK origin socket for one cache and one
  configured upstream namespace. Use the AWS SDK default credential chain, an
  optional S3 endpoint, and optional path-style upstream addressing.
- `racer-object sidecar`: serve HTTP on `127.0.0.1:8080` by default. Refuse
  non-loopback addresses. Use the SDK client socket for the same cache.
- Support path-style GET and HEAD of objects, optional `versionId`, single byte
  ranges, and ETag conditions. Reject unsupported operations explicitly.
- Do not claim full S3 API coverage. Listing, writes, multipart operations,
  arbitrary object metadata, date conditions, and client IAM authentication are
  outside this read-only adapter.
- Reject semantic headers that cannot be honored, including owner checks, SSE-C,
  requester-pays, and optional checksum requests. Reject non-identity object
  encodings because the SDK cannot carry their Content-Encoding header.

## Identity and consistency

Hash a canonical versioned JSON tuple of namespace, bucket, exact key, and optional
version ID into a Racer key. Carry the same tuple as adapter metadata. The origin
checks the namespace, bucket allowlist, and recomputed key. No request can choose
an upstream endpoint or credentials.

Use the upstream strong quoted ETag as the Racer version. HEAD selects metadata;
page GET uses `If-Match` and any explicit version ID. Validate the returned range,
length, and ETag. Fail rather than mix bytes from different versions. An S3 ETag
need not be an MD5 digest. An upstream must keep bytes stable for a given ETag.

Metadata has a configurable TTL, default 30 seconds. Mutable names can remain
stale during that interval. Already admitted reads stay pinned. An overwritten
version may become unavailable rather than silently switching to newer bytes.
Explicit object version IDs avoid that ambiguity when the upstream supports them.

The SDK requires full-object size even for partial origin responses and owns all
callback bodies (`pkg/racersdk/origin.go:22-30`). It supports only size, ETag,
expiry, and content type as object metadata (`pkg/racersdk/types.go:206-223`).
Its metadata snapshot option pins a ranged subscription to the selected version
(`pkg/racersdk/types.go:178-203`).

## Security and deployment

Use one origin DaemonSet pod on every Racer origin-serving node. Mount only that
cache's origin directory writable. Sidecars mount only its client directory,
read-only. The Racer operator and a ClusterCache must already be installed.
Use the SDK's stale-socket recovery and bounded server resources.

The origin uses its own upstream identity, not application SigV4 headers.
Applications may use unsigned requests or SDK placeholder credentials. Incoming
signatures do not establish identity. Every process in the application pod can
read the exposed data. Access to the cache socket grants access to cached data;
origin checks do not authorize cache hits. Isolate trust domains with separate
caches, namespaces, socket mounts, and upstream IAM policies. An optional bucket
allowlist narrows the exposed upstream scope.

## Verification

Test canonical identity, strict metadata validation, origin HEAD and conditional
page GET, changed versions, upstream failures, range framing, empty objects, and
HTTP GET/HEAD/ranges/conditions/errors. Exercise the real Go SDK protocol with
its test bridge. That bridge does not cache, so these tests do not prove live
multi-node cache reuse. Provide deployable examples and document that distinction.
