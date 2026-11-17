---
title: "racer-object Reference"
weight: 8
description: "Reference for the racer-object sidecar, covering its commands and flags, object addressing and storage layout, read operations, response headers, integrity, concurrency, authentication, and script usage."
---

## Overview

`racer-object` is a small sidecar that presents a Racer cache as a read-only S3 or Azure Blob endpoint. It accepts HTTP/1.1 requests on a loopback address, translates each object request into a request on the cache's Racer client socket, and streams the selected bytes back to the caller. The cache does the data movement; the sidecar translates protocols and enforces the read-only request rules.

The mental model is one sidecar per pod, bound to exactly one `ClusterCache`. With the built-in Azure Blob origin, the bucket segment of a request names the cache's container, and the key segment names a blob in that container. The sidecar itself holds no object data, no upstream credentials, and no durable state. Every read is satisfied through Racer, which serves it from memory, local disk, a peer node, or the origin, depending on where the pages currently live.

Use `racer-object` when an application already speaks the S3 or Azure Blob client API and you do not want to change it. If the application can mount a Unix socket and link the Go SDK, read through the Racer client socket directly instead. If the application needs durable storage outside Racer, use the storage provider's native API. The sidecar is an adapter between a cache and an object client, not a second cache and not a storage service.

| Property | Value |
| --- | --- |
| Binary | `racer-object` |
| Rust crate | `cmd/racer-object`, package name `racer-object`, library name `racer_object` |
| Container image | `ghcr.io/azure/racer-object:<version>` by default |
| Client APIs | S3 and Azure Blob, selected with `--api` |
| Listener | `127.0.0.1:8080` by default, loopback only |
| Cache connection | `/run/racer/<cache>/client/socket` |
| Racer page size | 16 MiB |
| Supported methods | `GET`, `HEAD`, and `OPTIONS *` |
| Request authentication | None; the listener is trusted and pod-local |
| Upstream credentials | None; they belong to the `ClusterCache` origin |

## Where It Fits in Racer

A `ClusterCache` is a cluster-scoped custom resource that names a cache. Racer runs a dataplane on each node, and every cache has two Unix sockets: an origin socket for the process that fills cache misses, and a client socket for readers. The `racer-object` sidecar is a client. It only needs the client socket, which the pod mounts read-only at `/run/racer/<cache>/client`.

The request path looks like this:

```text
application container (same pod)
  -> http://127.0.0.1:8080/<bucket>/<key>
    -> racer-object sidecar
      -> /run/racer/<cache>/client/socket
        -> Racer dataplane on this node
          -> local memory or disk, a peer node, or the origin container
```

Because the sidecar and the application share a pod network namespace, the application reaches the sidecar on loopback. Nothing outside the pod needs to reach it. For a cache with an Azure Blob origin, the Racer dataplane reads the configured container directly, so a cache miss does not pass through the sidecar and does not need storage credentials in the application pod.

The admission webhook can inject the sidecar for you. A pod labeled `racer.unbounded-cloud.io/object-sidecar: enabled` and annotated with `racer.unbounded-cloud.io/object-cache: <cache>` gets a native sidecar named `racer-object`, a `hostPath` volume for the cache's client directory, and the matching node affinity. See [Azure Blob cache]({{< relref "guides/azure-blob-cache" >}}) for the complete walkthrough and [Racer concepts]({{< relref "concepts/racer" >}}) for the cache architecture.

The sidecar is deliberately narrow. It reads one cache, exposes one bucket namespace, and supports reads only. It does not authenticate callers, sign upstream requests, retry against a second store, or write objects. Those omissions are what let it run as an unprivileged, read-only container with no storage secrets.

## Build and Image

The sidecar is a standalone Rust crate with its own Cargo workspace, so it does not share a workspace or dependency graph with the Racer dataplane crate. The repository Makefile wraps formatting checks, Clippy, tests, and a release build.

| Target | What it does |
| --- | --- |
| `make racer-object` | Checks formatting, runs Clippy, runs the crate tests, and builds `bin/racer-object` |
| `make racer-object-build` | Builds the release binary into `bin/racer-object` without tests |
| `make racer-object-test` | Runs `cargo fmt --check`, Clippy with warnings denied, and the crate tests |
| `make image-racer-object-local` | Builds the container image locally and tags it |

The direct Cargo commands are useful when you are working only on the crate:

```bash
cargo build --release --locked --manifest-path cmd/racer-object/Cargo.toml
cargo test --locked --manifest-path cmd/racer-object/Cargo.toml
cargo clippy --locked --manifest-path cmd/racer-object/Cargo.toml --all-targets -- -D warnings
```

`--version` reports the crate version plus build metadata. The Makefile passes three compile-time environment variables to Cargo, and the image build passes the same variables as build arguments:

| Variable | Reported as | Default when unset |
| --- | --- | --- |
| `RACER_OBJECT_VERSION` | Version | `dev` |
| `RACER_OBJECT_GIT_COMMIT` | Commit | `unknown` |
| `RACER_OBJECT_BUILD_TIME` | Build time | `unknown` |

The output has the form `<version> (commit: <commit>, built: <time>)`. These values are read at compile time with `option_env!`; they are not runtime configuration and the binary reads no environment variables while serving.

`make image-racer-object-local` tags the image as both `racer-object:<version>` and `$(CONTAINER_REGISTRY)/racer-object:<version>`. `CONTAINER_REGISTRY` defaults to `ghcr.io/azure`, so the published name is `ghcr.io/azure/racer-object`. The runtime image contains the binary at `/unbounded/bin/racer-object`, sets it as the entrypoint, includes CA certificates, and runs as UID and GID 65532.

## Invocation

The binary has one real command, `sidecar`, plus help and version shortcuts. The command word is required even when all settings are supplied as flags.

| Command | Behavior |
| --- | --- |
| `racer-object --help` or `racer-object -h` | Prints short usage and exits 0 |
| `racer-object --version` | Prints the version string and exits 0 |
| `racer-object sidecar --help` or `racer-object sidecar -h` | Prints the full flag usage and exits 0 |
| `racer-object sidecar [flags]` | Starts the sidecar and serves until a signal arrives |

Running `racer-object` with no arguments fails with `racer-object: expected sidecar (use --help)`. An unknown command, including the removed `origin` command, fails with `racer-object: unknown command (use --help)`. Older deployments sometimes passed `origin`; that command no longer exists, and a pod spec that still uses it exits instead of starting a different mode.

Flags accept all four common spellings: `--flag=value`, `--flag value`, `-flag=value`, and `-flag value`. A single dash is not an abbreviation for a short option; it is accepted for compatibility with earlier manifests. Parsing stops at the first token that is not a flag, at a bare `-`, or at `--`. If any positional arguments remain after flag parsing, startup fails with `racer-object: unexpected positional arguments`.

Repeating a flag is allowed, and the last value wins. This matters for generated pod specs that append overrides to a base argument list. Boolean values for `--version` accept `1`, `t`, `T`, `true`, `TRUE`, `True`, `0`, `f`, `F`, `false`, `FALSE`, and `False`.

Invalid input is reported on standard error as `racer-object: <message>`, and the process exits with status 1. Error messages identify the category, such as `invalid flags (use --help)` or `--cache is required`, without echoing the supplied value. That keeps a mistyped secret out of container logs even when someone accidentally passes one as a flag value.

On `SIGTERM` or `SIGINT`, the sidecar stops accepting connections and gives in-flight requests up to 5 seconds to finish. It then exits. The container runtime may send `SIGKILL` after its own grace period, so keep the pod termination grace period above the sidecar's 5 second drain if you care about completing large reads during a rollout.

## Flags

All flags below belong to the `sidecar` command. The top-level `--help` and `--version` forms accept no additional flags, and there is no configuration file.

| Flag | Type | Default | Meaning |
| --- | --- | --- | --- |
| `--cache` | string | none, required | `ClusterCache` name whose client socket the sidecar reads |
| `--api` | string | `s3` | Client API flavor, either `s3` or `azure` |
| `--listen` | string | `127.0.0.1:8080` | Loopback IP literal and port for the object API |
| `--debug-listen` | string | empty, disabled | Optional diagnostics address serving `/healthz` |
| `--request-timeout` | duration | `5m0s` | Maximum time for one request, including its body |
| `--max-connections` | integer | `128` | Concurrent client connections and each Racer request lane, 1 to 65536 |
| `--workers` | integer | usable CPUs, at most 4 | Serving threads, 1 to 256, each with its own event loop |
| `--version` | boolean | false | Prints the version and exits without serving |

### Cache selection

`--cache` is required and must be a valid `ClusterCache` name. Names are lowercase DNS subdomains: dot-separated labels of lowercase letters, digits, and hyphens, with no empty label and no leading or trailing hyphen. Each label is limited to 63 characters. The full name is also limited so that `/run/racer/<cache>/origin/socket` fits inside a Unix socket address, which effectively caps the name at 82 characters.

The name must match the `ClusterCache` exactly. The sidecar derives the client socket path from the name and does not discover caches, so a typo produces connection failures rather than a helpful lookup error. There is no default cache.

### API flavor and listener

`--api=s3` serves S3-style paths, query parameters, conditions, and error codes. `--api=azure` serves Azure Blob-style paths, query parameters, headers, and error codes, and reports the Azure service version `2026-02-06` on responses. The two flavors share the same object identity and read path; they differ in the accepted request shape and the response headers.

`--listen` takes an IP literal and a port, for example `127.0.0.1:8080`, `127.1.2.3:9000`, or `[::1]:8080`. DNS names such as `localhost` are rejected, as are wildcard addresses such as `0.0.0.0:8080` and `[::]:8080`. IPv4-mapped IPv6 loopback such as `[::ffff:127.0.0.1]:8080` is accepted. The binary permits port 0, which asks the kernel for an ephemeral port, but the admission webhook requires a port from 1 to 65535 because a pod cannot discover an ephemeral port from an injected argument.

The loopback restriction is a security boundary. The sidecar has no authentication and no TLS, so binding a routable address would expose the cache keyspace to anything that can reach the pod. The webhook also rejects `hostNetwork: true`, because a loopback listener in the host network namespace is reachable from other host-network workloads.

### Diagnostics

`--debug-listen` is empty by default, which disables the diagnostics listener. When set to an address such as `127.0.0.1:6060`, the sidecar serves only `GET /healthz` and `HEAD /healthz`, both with status 200 and an empty body. Any other path returns 404, and any other method on `/healthz` returns 405.

A health response only means the diagnostics listener is accepting connections. It does not mean the cache socket exists, the cache is published, or the dataplane is ready. The listener has no authentication and no TLS. An address beginning with a colon, such as `:6060`, binds every interface, so use a loopback address unless you deliberately want network access. A bind failure stops startup.

Profile the process with `perf` rather than expecting pprof endpoints. Release builds keep line tables and the container image builds with frame pointers, so stack sampling works. The debug listener is not a profiling endpoint.

### Timeouts, connections, and workers

`--request-timeout` uses Go duration syntax: a sequence of decimal numbers with optional fractions and a unit, such as `300ms`, `1.5s`, `5m`, or `1h2m3.5s`. Supported units are `ns`, `us`, `µs`, `ms`, `s`, `m`, and `h`. A bare number is invalid, as are negative values and zero. The default is 5 minutes. The timeout covers the whole request, including waiting for Racer and streaming the response body.

The same value also bounds one response write and sets the request-head read timeout, capped at 5 seconds. A very small request timeout therefore also shortens how long a client has to send its headers. A timeout that expires before the response is committed produces a 503; a timeout after the head is committed closes the connection so a client cannot mistake a truncated body for a complete one.

`--max-connections` bounds two things at once: the number of accepted client connections, and the number of concurrent Racer requests in each of two internal lanes, one for metadata and one for bodies. The default is 128, and the accepted range is 1 to 65536. Excess connections wait in the kernel backlog rather than being accepted and queued in the process. The limit is global across workers, not per worker.

`--workers` starts N operating-system threads, each with its own single-threaded Tokio runtime and its own idle Racer connection pool. The default is the number of usable CPUs, including cgroup quota and affinity, capped at 4. The accepted range is 1 to 256. The first worker accepts every connection and hands it to the worker currently serving the fewest connections, so a handful of long-lived connections cannot pin all work to one thread. There is no work stealing. With `--workers=1`, the binary runs a plain single-threaded runtime for the smallest footprint.

## Deployment

The usual deployment path is the Racer admission webhook. It injects a native sidecar, which Kubernetes models as an init container with `restartPolicy: Always`. The sidecar starts before the application containers and keeps running for the life of the pod. It has no readiness probe, so pod readiness reflects the application, not the cache. A first read can still fail with 503 while the node dataplane is publishing the cache socket; a bounded retry handles that case.

A pod opts in with a label and names its cache with an annotation. The webhook reads the following keys from the pod template:

| Key | Kind | Required | Value |
| --- | --- | --- | --- |
| `racer.unbounded-cloud.io/object-sidecar` | label | yes | `enabled` |
| `racer.unbounded-cloud.io/object-cache` | annotation | yes | Name of an existing `ClusterCache` |
| `racer.unbounded-cloud.io/object-api` | annotation | no | `s3` (default) or `azure` |
| `racer.unbounded-cloud.io/object-listen` | annotation | no | Loopback IP and port from 1 to 65535, default `127.0.0.1:8080` |
| `racer.unbounded-cloud.io/object-cpu-request` | annotation | no | CPU request for the sidecar |
| `racer.unbounded-cloud.io/object-cpu-limit` | annotation | no | CPU limit for the sidecar |
| `racer.unbounded-cloud.io/object-memory-request` | annotation | no | Memory request for the sidecar |
| `racer.unbounded-cloud.io/object-memory-limit` | annotation | no | Memory limit for the sidecar |
| `racer.unbounded-cloud.io/object-sidecar-injected` | annotation | output | Set to the injected image after admission |

There is no default cache. A pod that opts in without `object-cache` is denied instead of being attached to an unrelated cache. The API annotation is validated against `s3` and `azure`; any other value fails admission. The listen annotation must be a loopback IP literal with a port from 1 to 65535, which is stricter than the binary's own parser because an injected pod cannot learn an ephemeral port.

The webhook adds a container named `racer-object` at the front of `spec.initContainers`, a `hostPath` volume named `racer-object-client`, and an annotation recording the image. The volume points at `/run/racer/<cache>/client` on the host and is mounted read-only at the same path. The container runs as UID and GID 65532, has `allowPrivilegeEscalation: false`, a read-only root filesystem, all capabilities dropped, and the `RuntimeDefault` seccomp profile.

Admission also places the pod. It adds `kubernetes.io/os: linux` to the node selector, requires that the node does not carry `racer.unbounded-cloud.io/exclude`, and ANDs the cache's `spec.nodeSelector` into every nonempty required node affinity term. A pod with `hostNetwork: true` is denied, as is a pod whose OS or node selector conflicts with Linux. Admission fails closed: if the webhook cannot reach a Racer control seat, pods that opt in are denied rather than admitted without their cache mount.

Two older annotations, `racer.unbounded-cloud.io/object-namespace` and `racer.unbounded-cloud.io/object-buckets`, are rejected. A cache with an Azure Blob origin serves exactly the container named by that origin, so there is no per-pod bucket scope to configure.

If you manage the pod yourself, add the sidecar directly. The following manifest is the manual equivalent for a cache named `my-cache`; replace the cache name in all three places and replace the image placeholder with a released tag:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: fetch-blob
spec:
  initContainers:
    - name: racer-object
      image: ghcr.io/azure/racer-object:<version>
      args:
        - sidecar
        - --cache=my-cache
        - --api=s3
        - --listen=127.0.0.1:8080
      restartPolicy: Always
      securityContext:
        runAsUser: 65532
        runAsGroup: 65532
        runAsNonRoot: true
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
        seccompProfile:
          type: RuntimeDefault
      volumeMounts:
        - name: racer-object-client
          mountPath: /run/racer/my-cache/client
          readOnly: true
  containers:
    - name: app
      image: curlimages/curl:latest
      command: ["sleep", "infinity"]
  volumes:
    - name: racer-object-client
      hostPath:
        path: /run/racer/my-cache/client
        type: Directory
```

The mount must be the client directory, not the origin directory. The client socket is the read path; the origin socket speaks a different protocol and is meant for the process that fills cache misses. The webhook does not add a readiness probe, and a manual sidecar does not need one either; treat a 503 as retryable while the cache is being published.

## Object Storage Layout

`racer-object` uses the same two-level namespace as S3 and Azure Blob: a bucket and a key. The sidecar is bound to one cache, so the bucket is not a way to select a different cache. With the built-in Azure Blob origin, the bucket names the container that the cache reads, and the key names a blob in that container. A custom origin receives the same canonical metadata and can define its own mapping from bucket and key to storage.

### Request path grammar

A read targets `/<bucket>/<key>`, optionally followed by a version query:

```text
http://127.0.0.1:8080/models/llama/Llama-3.1-8B-Instruct/config.json
                       |      |
                       |      +-- key: llama/Llama-3.1-8B-Instruct/config.json
                       +--------- bucket: models
```

The path is split at the first literal slash after the leading slash, before either part is percent-decoded. This ordering means an escaped slash belongs to the key. `/models/a%2Fb` names the key `a/b`, while `/models/a/b` names the same key through two literal separators. Escapes are decoded exactly once. Dot segments and repeated slashes are never cleaned or collapsed, so `/models/./a` names the key `./a`, not `a`.

A path with a bucket but no key is rejected with 400, not 404. So is an empty bucket, an empty key, a missing leading slash, or an invalid percent escape. The parser also accepts absolute-form request targets, which is useful for proxies that send a full URI instead of an origin-form path.

The query string is flavor-specific:

| API | Version parameter | Other allowed parameter | Rejected |
| --- | --- | --- | --- |
| S3 | `versionId=<id>` | `x-id=GetObject` for GET, `x-id=HeadObject` for HEAD | Another name returns 501; a repeated name or an empty value returns 400 |
| Azure | `versionid=<id>` | `timeout=<positive decimal seconds>` | Another name returns 501; a repeated name or an empty value returns 400 |

S3 parameter names are case-sensitive, so `versionid` is not `versionId`. Azure's `timeout` value is validated as a positive decimal number but has no effect on the sidecar's own timeout; use `--request-timeout` for that. When a query mixes a malformed parameter, which would be 400, with an unsupported one, which would be 501, the sidecar returns 400 because malformed input takes precedence. A repeated parameter is malformed even when both values are identical.

### Bucket and key rules

The parser applies the following limits before any request reaches the cache:

| Field | Rule |
| --- | --- |
| Bucket | 1 to 255 bytes, not `.` or `..`, only ASCII letters, digits, `.`, `-`, and `_` |
| Key | 1 to 1024 bytes, valid UTF-8, never empty after decoding |
| Version ID | 0 to 1024 bytes, valid UTF-8 |
| Canonical metadata | At most 8 KiB |
| Control characters | DEL (`0x7f`) is rejected in the canonical metadata; C0 controls are escaped in JSON |

The parser accepts uppercase letters in a bucket because legacy S3-compatible names may contain them, but Azure container names are lowercase. With the Azure Blob origin, a bucket that does not match the container name is not redirected to another container. The dataplane serves exactly the one container configured in the cache's origin and rejects a bucket that does not name it.

Keys are opaque strings to Racer. The sidecar does not normalize Unicode, resolve `..`, collapse `//`, or strip a trailing slash. If two clients disagree about a path, they are reading two different objects. Keep keys stable and clean at the producer, and remember that a URL encoder may turn a slash into `%2F`; the sidecar decodes it back to a slash in the key, so both spellings address the same object as long as the split point is the same.

### Object identity and the Racer key

Racer identifies an object by a 32-byte key, which the sidecar computes as the SHA-256 of a canonical JSON document. The document has a fixed field order and is built as follows:

```json
{"schema":1,"bucket":"models","key":"llama/config.json"}
```

When a version ID is present, the document gains a `versionId` field:

```json
{"schema":1,"bucket":"models","key":"llama/config.json","versionId":"2026-11-01T12:00:00Z"}
```

The exact bytes are hashed, without a trailing newline. The canonical encoder escapes `"`, `\`, backspace, form feed, newline, carriage return, and tab with their short JSON escapes, escapes other C0 controls, `<`, `>`, and `&` as `\u00xx`, and escapes U+2028 and U+2029. The resulting digest is written as 64 lowercase hexadecimal characters, which is the key the sidecar sends to Racer. You can reproduce the identity for a simple key with standard tools:

```bash
printf '%s' '{"schema":1,"bucket":"models","key":"llama/config.json"}' | sha256sum
```

The same canonical document is sent to Racer in the `Racer-Metadata` header, unchanged. Racer does not interpret it; it passes it to the origin on a cache miss, and the Azure origin uses the bucket and key to locate the blob. The in-tree Go package `internal/racerobject` builds the same document and the same key byte for byte, so tooling on either side of the cache agrees on identity.

This key is a metadata digest, not a content digest. Two objects with the same bytes but different bucket or key names have different Racer keys. A versioned read hashes the version ID as well, so it addresses a distinct key, while an unversioned read hashes only the bucket and key and lets Racer choose the current version.

### The upstream container

The cache's origin is configured in `ClusterCache.spec.origin`. Only the Azure provider is supported today:

| Origin field | Meaning |
| --- | --- |
| `spec.origin.azure.containerURL` | Blob container URL, for example `https://account.blob.core.windows.net/models`; the last path segment is the container name |
| `spec.origin.azure.credential.default` | Node managed identity through the Azure Instance Metadata Service |
| `spec.origin.azure.credential.workloadIdentity` | Microsoft Entra Workload ID with a federated token for the Racer dataplane ServiceAccount |
| `spec.origin.metadataTTL` | How long object metadata is reused before revalidation; 30s when unset |
| `spec.origin.requestTimeout` | Bound on each upstream request; 1m when unset |
| `spec.origin.maxConcurrentRequests` | Concurrent upstream requests per dataplane node; 512 when unset |

The container URL is immutable. Cache entries are keyed by the `ClusterCache` UID, not by the container URL, so pointing a new cache at the same container starts with an empty key space. The whole container keyspace is visible through the cache; there is no per-pod bucket scope beyond the container itself.

The sidecar is not involved in any of this configuration. It does not read the `ClusterCache` object, does not open a storage connection, and does not need the credential. That is also why the sidecar image contains no Azure SDK and no storage secrets.

### Upstream requirements

The built-in origin is a client for Azure Blob Storage. If you run a custom origin, it must meet the same contract, because Racer caches pages and trusts the origin to keep versions coherent.

| Requirement | Why it matters |
| --- | --- |
| Block blob storage | Racer reads whole pages and byte ranges from block blobs. The sidecar reports `BlockBlob` to Azure clients because that is the object model it serves. |
| Strong quoted ETags | Racer and the sidecar require tags of the form `"..."`. An unquoted or weak tag is a protocol violation and becomes a 502. |
| Immutable bytes per ETag | Pages are cached against a version. If the origin reuses an ETag for different bytes, a read can mix pages from two contents. |
| Stable failure behavior | A failed upstream read must fail the read. It must not return a different version or a partial object as if it were the requested one. |
| `If-Match` on ranged reads | Racer pins a read to one ETag and sends `If-Match` with page requests. The origin must honor it so a page read cannot cross versions. Racer also rechecks the returned version when a service ignores the header. |
| Identity content encoding | Racer rejects responses with `Content-Encoding`, `Transfer-Encoding`, `Trailer`, `Upgrade`, or `Expect`. Serve the raw bytes; do not compress on the wire. `racer-object` never adds a content encoding of its own. |
| A bounded metadata window | Racer reuses object metadata for up to `spec.origin.metadataTTL`, 30s by default. A write is not visible until that metadata is revalidated. |
| Exact version reads | When a client asks for a version ID, the origin must return that version or report it as unavailable. It must not substitute the current version. |

These requirements explain several behaviors that otherwise look surprising. A write made directly to the container can take up to the metadata window to appear through the cache. A compressed origin response becomes a 502 rather than a decoded object. A reused ETag can corrupt a read even though the sidecar validated every protocol field, because the device that violated the contract was the origin, not the cache.

### Pages and cache entries

Racer stores and transfers objects in 16 MiB pages. The last page of an object may be shorter. A page is the unit in which Racer caches and transfers an object. On a cache miss, a read that asks for a small range still causes the dataplane to fetch and cache the page or pages that cover that range, which is why the first read of a small object can transfer a full 16 MiB page from the origin.

Each version of an object has its own ETag, and the bytes behind one ETag never change. Racer may serve different versions of the same key on two separate reads, because the current version can change between them. A single read, however, is pinned to one version from start to finish. The sidecar relies on that guarantee when it forwards a range to Racer and when it pins a version for a conditional request.

`racer-object` does not add another cache layer. It keeps a small response buffer per connection, a 64 KiB buffer for small bodies, and a 256 KiB fallback buffer for a large body when a splice pipe cannot be created. The bytes in memory are transient, and the durable copy, if any, remains in the cache's normal storage hierarchy or in the origin.

## Read Operations

The sidecar supports three request shapes: an unconditional `GET`, a `HEAD`, and a conditional `GET`. It also answers `OPTIONS *` with an empty 200 so discovery probes do not fail. Every other method is rejected before any object lookup.

### Unconditional GET

An unconditional `GET` without `If-Match` or `If-None-Match` is the fast path. The sidecar sends one subscription to Racer with the client's range and does not send a metadata request first. Racer resolves the range against the current version, and the sidecar independently resolves the same range from the size Racer reports in the subscription response. If the two selections disagree, the sidecar returns 502 instead of serving bytes from a different window. This cross-check catches a broken or misbehaving Racer without trusting it blindly.

The sidecar then streams the body. Bodies up to 64 KiB are read into memory and validated through completion before any response byte is sent, so a Racer failure in that window still produces a proper error status. Larger bodies send the head first, then forward pages, and hold back the final byte until Racer confirms that the read completed. If Racer fails after the head is sent, the connection closes early, which leaves the client with a body shorter than `Content-Length` and therefore an error rather than a silently truncated success.

### HEAD

A `HEAD` request asks Racer for metadata with a `HEAD /v1/objects/<key>` request. It does not read object content. The response carries the selected length, ETag, content type, and, when applicable, the version header. The body is always empty.

For Azure, a `HEAD` is the Get Blob Properties operation. Azure clients expect properties to describe the whole blob, so the sidecar ignores `Range` and `x-ms-range` on an Azure `HEAD` and reports the full size. If you need the size of a range, use a `GET` with a range and read `Content-Range`.

Error responses to `HEAD` keep the error `Content-Length` but carry no body, as HTTP requires. A client must not try to read a body from a `HEAD` response, even when the status is an error and `Content-Length` is nonzero.

### Ranges

A read can select one byte range. The sidecar accepts a single `Range` header in the standard forms:

| Form | Example | Result |
| --- | --- | --- |
| Closed | `bytes=0-4095` | First 4096 bytes, status 206 |
| Open ended | `bytes=4096-` | From byte 4096 to the end, status 206 |
| Suffix | `bytes=-4096` | Last 4096 bytes, status 206 |
| No range | omitted | Whole object, status 200 |

Only one range is supported. Two `Range` headers, a comma-separated list, a non-`bytes` unit, a missing dash, a negative number, or a range whose end precedes its start returns 400. A suffix length of zero, or any range whose start is at or beyond the object size, returns 416. When the sidecar resolved the size itself, the 416 carries `Content-Range: bytes */<size>`; a 416 discovered after a pinned read has no size to report and omits that header.

For Azure, `x-ms-range` takes precedence over `Range` on a `GET`. On an Azure `HEAD`, both are ignored as described above. A start offset at or beyond the object size is not clamped to the last byte; it returns 416. An end offset beyond the size is clamped to the last byte, as standard HTTP range semantics require, so `bytes=0-999999` on a ten-byte object returns all ten bytes.

### Conditional requests

The sidecar supports `If-Match` and `If-None-Match` with strong or weak entity tags and with the `*` wildcard. Multiple header lines are joined with commas, empty list elements are ignored, and a tag must be a quoted string. A bare token such as `If-Match: v1` is malformed and returns 400. A tag containing a control character is also malformed.

| Header | Comparison | Match result |
| --- | --- | --- |
| `If-Match` | Strong; a weak candidate never matches | Continue the read |
| `If-Match` | Strong; no candidate matches | 412 Precondition Failed |
| `If-None-Match` | Weak; weakness is ignored | 304 Not Modified, no body |
| `If-None-Match` | Weak; no candidate matches | Continue the read |
| Either | `*` | Matches any existing version |

Conditional requests take the metadata path: the sidecar asks Racer for the current metadata, evaluates `If-Match` first, then `If-None-Match`, and only then issues a content read pinned to the ETag that the metadata request returned. This ordering means a request with both headers can fail with 412 before the 304 rule is considered. A matching `If-None-Match` returns 304 with the ETag and the Azure headers when applicable, and never reads the body. The same evaluation applies to `HEAD`, so a conditional `HEAD` is a metadata probe that returns 304 while the version still matches.

Three conditional headers are not supported: `If-Modified-Since`, `If-Unmodified-Since`, and `If-Range`. They are rejected with 501 rather than ignored, because silently ignoring a cache validator would change the meaning of the response. Use ETags for conditional reads.

### Versioned reads

A version query pins the identity to a specific version. For S3, use `?versionId=<id>` and, if your client sends it, `?x-id=GetObject` on a GET or `?x-id=HeadObject` on a HEAD. Any other `x-id` value returns 501. For Azure, use `?versionid=<id>`; `?timeout=<seconds>` is accepted and validated but does not change the sidecar timeout.

A successful versioned read reports the version back to the client. S3 responses use `X-Amz-Version-Id`; Azure responses use `X-Ms-Version-Id`. A version that Racer reports as unavailable produces 412, the same status as a failed ETag precondition, which tells the client that the requested version cannot be served rather than that the whole object is missing.

Version IDs are opaque to the sidecar. They are hashed into the object identity and forwarded in the canonical metadata, so the same version string must be used consistently. An empty version value is malformed and returns 400; omit the parameter to read the current version instead.

### Unsupported operations

The sidecar is read-only, and it enforces that before any lookup:

| Request | Response |
| --- | --- |
| `PUT`, `POST`, `DELETE`, `PATCH`, or another method other than GET and HEAD (except `OPTIONS *`) | 405 with `Allow: GET, HEAD` |
| `OPTIONS *` | 200 with an empty body |
| `OPTIONS /bucket/key` | 405, because only the asterisk form is handled |
| Multipart or upload query parameters, such as `partNumber` or `uploads` | 501 |
| Presigned query parameters, such as `X-Amz-Signature` or Azure `sig` | 501 |
| Conditional headers other than `If-Match` and `If-None-Match` | 501 |
| Unknown `x-amz-*` or `x-ms-*` headers | 501 |

Rejecting unknown provider headers is deliberate. A header that the sidecar does not understand could change authorization or read semantics, and dropping it silently would be worse than refusing the request. A client that needs to upload, copy, or delete objects must use the storage provider directly, outside the cache.

## Response Headers

Successful responses use a small, predictable set of headers. Header names are sent with canonical MIME capitalization, but HTTP header names are case-insensitive on the wire.

| Header | When it appears | Meaning |
| --- | --- | --- |
| `Etag` | Every success and 304 | Strong quoted entity tag of the selected version |
| `Accept-Ranges` | Every 200 and 206 | Always `bytes` |
| `Content-Length` | Every 200 and 206 | Number of bytes in the selected range |
| `Content-Type` | Every 200 and 206 | Object content type, or `application/octet-stream` when the origin gave none |
| `Content-Range` | 206 | `bytes <first>-<last>/<size>` |
| `X-Amz-Version-Id` | S3 response with a version ID | The requested or selected version |
| `X-Ms-Version` | Every Azure response | Service version `2026-02-06` |
| `X-Ms-Blob-Type` | Every Azure 200, 206, and 304 | Always `BlockBlob` |
| `X-Ms-Version-Id` | Azure response with a version ID | The requested or selected version |
| `Date` | Every response | Current time in IMF-fixdate form |
| `Connection` | When not the HTTP/1.1 default | `close` or `keep-alive` |

A 304 response carries `Etag` and, for Azure, `X-Ms-Version` and the blob headers, but no content headers. Error responses use `Content-Type: application/xml`, a body of the form `<Error><Code>...</Code><Message>...</Message></Error>`, and `Content-Length` of that body. Azure errors also carry `X-Ms-Version` and `X-Ms-Error-Code`. A 405 carries `Allow: GET, HEAD`, and a 416 carries `Content-Range: bytes */<size>` when the sidecar knows the size.

The message in an error body is always the standard HTTP reason phrase for the status. It never includes request data, upstream data, or credentials, so an error body is safe to log. The `Code` value is the only API-specific part: S3 and Azure use different names for the same condition.

## Download Operations

A download is a `GET` for a whole object or a range. The sidecar streams the response instead of buffering the object, so memory use does not grow with object size. A 1 GiB read does not allocate 1 GiB in the sidecar; it holds a 64 KiB buffer for a small body, a pipe (or a 256 KiB fallback buffer) for a large body, and per-connection protocol state.

The response always carries an exact `Content-Length`. For a whole-object read it is the object size; for a range it is the number of selected bytes. A client can therefore detect a short transfer without knowing the object size in advance. The sidecar holds back the final byte until Racer confirms completion, so a connection that ends early always ends with fewer bytes than promised. Do not disable length checks in the client, and do not treat EOF alone as success.

A download can be resumed by issuing a new range request from the last received offset. The sidecar does not remember previous requests and does not issue resume tokens; the range is the only state. If the object changed between the two requests, the second range can describe a different version. Take the ETag from the first response and send it as `If-Match` on the resumed request to keep both halves on one version. A mismatch returns 412 instead of stitching two versions together.

Parallel downloads are independent connections. Each one consumes a connection slot for its lifetime, and each one holds a Racer read permit while its body streams. Increasing parallelism beyond `--max-connections` does not increase throughput; it only fills the queue and eventually produces 503s. For large objects, measure with a modest number of streams first, because the origin and the cache cluster often saturate before the loopback sidecar does.

Small reads have a different cost profile from large ones. Racer caches 16 MiB pages, so reading a 4 KiB range of a cold object still fetches the page that covers it from the origin. Ten thousand small reads spread across many pages can move far more data from the origin than their total response size suggests. If a workload reads many small objects, keep them in a small number of pages where possible, and warm the cache with a prefetch or a full read before the latency-sensitive phase.

## Upload Operations

There are no upload operations. The sidecar rejects `PUT`, `POST`, `DELETE`, and every method other than `GET`, `HEAD`, and the bare `OPTIONS *` probe with 405 and `Allow: GET, HEAD`. It has no code path that writes to Racer or to the origin, no multipart upload support, no copy support, and no delete support. Adding a write would require bypassing the cache's read-only object identity and would give an unauthenticated loopback listener the ability to mutate durable storage.

Upload objects with the storage provider's own tools, directly to the container that the cache reads. For an Azure Blob origin, the Azure CLI is one option:

```bash
az storage blob upload \
  --account-name <account> \
  --container-name <container> \
  --name llama/config.json \
  --file ./config.json \
  --auth-mode login
```

The account, container, and file are placeholders; the blob name is the key that the sidecar will use. The upload does not pass through Racer, and the sidecar cannot see the new object until the cache revalidates its metadata. The `spec.origin.metadataTTL` value controls that window, with a default of 30 seconds. During the window, a read can still return the previous version. After revalidation, a read returns the new version and a new ETag.

An overwrite creates a new version with a new ETag. Racer pins each read to one ETag, so a reader that started before the overwrite finishes on the old version, and a reader that starts afterward sees the new one. This is safe as long as the origin never changes the bytes behind an existing ETag. If the origin reuses an ETag for different content, the cache can mix pages from two different contents under one identity, and no amount of sidecar validation can detect it. Configure the origin so that each ETag identifies immutable bytes.

Deleting a blob is also done at the provider. A cached page can continue to satisfy reads until the metadata is revalidated and the cache learns that the object is gone. Do not treat the sidecar as a deletion barrier. If a workload requires an immediate guarantee that no reader can see an object after deletion, coordinate that in the application; the cache is not a transactional store.

Uploads have no checksum handshake with the sidecar. If you publish a digest alongside the object, record it before the upload and use it on the reader side as described in the next section. The cache returns the origin's bytes, and the published digest is how you prove that it did.

## Integrity and Checksums

`racer-object` does not implement S3 checksum headers, `Content-MD5`, `x-amz-checksum-*`, or Azure content MD5 validation. Those headers are not part of the read path, and an unknown `x-amz-checksum-*` header is rejected with 501 because all unknown `x-amz-*` headers are rejected. A client that requires a checksum must compute it over the received bytes and compare it with a digest obtained from the producer.

Integrity in this system has three layers:

1. The origin guarantees that each ETag identifies immutable bytes. Racer relies on this to cache pages safely.
2. Racer pins a read to one ETag and validates each page against the version that the read selected. A short, long, or mismatched response is rejected rather than cached.
3. The sidecar validates the protocol exchange and the completion signal before it lets the client see a complete body.

The sidecar's own checks are deliberately strict. It validates that Racer's ETag is a strong quoted tag of a bounded length, that the expiry value is a canonical decimal, and that the content type is a well-formed media type. On a stat-then-read path it checks that the pinned subscription reports the same size and content type as the metadata request that selected the pin. It checks that the first byte and end offset of the subscription match the range it resolved. On an unconditional read it checks that Racer selected exactly the range the sidecar would have selected. A disagreement is a 502, not a partial success.

The final-byte rule is the most visible integrity behavior. For a body larger than 64 KiB, the sidecar forwards pages as they arrive but withholds the last byte until Racer sends its completion frame and accepts the final credit. If the Racer socket closes or the read is cut short, the client receives one byte fewer than `Content-Length`. HTTP clients treat that as a premature EOF. The sidecar closes the connection rather than sending a clean end-of-body, so a truncated transfer cannot be mistaken for a successful one. A page that fails Racer's own authentication never reaches the sidecar as valid content; the dataplane reports the failure through the client socket.

A checksum comparison at the application layer is still worthwhile:

```bash
curl -fsS http://127.0.0.1:8080/models/llama/config.json -o config.json
printf '%s  %s\n' '<sha256>' config.json | sha256sum -c -
```

Replace `<sha256>` with the digest published by the producer. The comparison proves that the bytes the application received are the bytes that were published, which covers the origin, the cache, the sidecar, and the client. It does not prove that the ETag is a content hash; ETags are opaque version identifiers and should not be compared with `sha256sum` output.

For multi-file artifacts, pin the set as well as the bytes. Fetch the manifest first, record the ETag of every file it names, then read each file with `If-Match` set to the ETag you recorded for that file. If any file changed in between, the read fails with 412 instead of producing a mixed set. The vLLM loader uses this pattern; see [Racer with vLLM]({{< relref "guides/racer-vllm" >}}) for a worked example.

## Concurrency and Flow Control

Concurrency is bounded at four places: accepted client connections, Racer metadata requests, Racer body subscriptions, and pages in flight per subscription. The limits are global across workers, so adding workers does not multiply the number of connections a pod can open to the cache.

| Limit | Default | Scope |
| --- | --- | --- |
| Accepted client connections | `--max-connections`, 128 | Process-wide semaphore |
| Concurrent Racer metadata requests | `--max-connections` | Process-wide lane |
| Concurrent Racer body subscriptions | `--max-connections` | Process-wide lane |
| Pages in flight per subscription | 2 | Per read, 32 MiB of byte credit |
| Idle Racer metadata connections | 4 | Process-wide, shared by all workers |
| Request head size | 36 KiB | Per request, with at most 256 headers |
| Small body threshold | 64 KiB | Per response |
| Splice pipe | 1 MiB when the kernel allows | Per large body |
| Userspace copy buffer fallback | 256 KiB | Per large body when no pipe is available |

The accept loop takes a connection slot before accepting, so a burst beyond the limit waits in the kernel backlog instead of being accepted and queued in user space. This keeps memory bounded and gives clients a normal TCP backpressure signal. If a client waits too long, its own timeout expires; the sidecar does not send a 503 for a connection it never accepted.

Each read has a page credit window. The subscription starts with two page credits and 32 MiB of byte credit, and the sidecar returns a credit only after it has forwarded a page. Racer can therefore run at most two 16 MiB pages ahead of the client. A slow client reduces the credit return rate, which slows the origin and peer fetches for that read. A fast client keeps the window full. This is flow control, not a fixed throughput limit, but it does bound a single read's ability to pull an unlimited amount of data ahead of the consumer.

Metadata requests use pooled keep-alive connections to the Racer socket. At most four idle metadata connections are kept across the whole process, regardless of worker count. A pooled connection is discarded after 90 seconds idle or after a maximum age of 300 seconds, with the actual expiry rotated between 75 and 100 percent of the maximum so that all workers do not reconnect at once after a Racer restart. A stale pooled connection is retried once on a fresh connection when nothing was received, which turns a benign Racer restart into a successful request instead of a 503.

Large bodies take the zero-copy path when possible. The sidecar creates a pipe and uses `splice(2)` to move bytes from the Racer Unix socket to the client TCP socket without copying them through user space. The pipe is sized at 1 MiB when the kernel permits it. If a pipe cannot be created, the sidecar falls back to a 256 KiB userspace buffer. Both paths use the same completion and final-byte rules, so the integrity behavior does not depend on which path ran.

The sidecar watches the client socket while it waits for Racer. If the client disconnects or half-closes, the read is abandoned, the Racer subscription is released, and the connection slot is returned. A disconnected client cannot hold a cache read open indefinitely.

### Timeouts

Timeouts are layered. The client-visible timeout is `--request-timeout`; the Racer-side timeouts are internal and fixed.

| Timeout | Value | Applies to |
| --- | --- | --- |
| Sidecar request timeout | `--request-timeout`, default 5m | One client request, including its body |
| Sidecar request-head read | Minimum of request timeout and 5s | Reading request headers |
| Sidecar idle connection | 30s | Waiting for the first byte of a later request |
| Racer queue wait | 5s | Waiting for a metadata or body lane permit |
| Racer dial | 5s | Connecting to the client socket |
| Racer response head | 60s | Reading a metadata or subscription response head |
| Racer body read, credit write, or splice wait | 60s | One body operation |
| Racer idle metadata connection | 90s | Pooled connection reuse |
| Racer maximum connection age | 300s | Forced rotation |
| Shutdown drain | 5s | In-flight requests after SIGTERM or SIGINT |
| Close linger | 500ms | Draining unread request bytes before close |

If the request timeout expires before the response head is committed, the client gets 503. If it expires after the head is committed, the sidecar closes the connection. The second case is not a 503 because the status line has already been sent; a short body is the only honest signal.

## Authentication and Trust

`racer-object` does not authenticate or authorize client requests. There is no TLS, no token check, no signature verification, and no per-object access control. The `Authorization` header is accepted and ignored. S3 signing headers and Azure credential headers are accepted where listed below, but they are not verified, and the sidecar does not forward them to Racer or to the origin.

This is a deliberate trust model. The listener is loopback-only, and the intended unit of isolation is the pod. Every container in the pod shares the network namespace and can reach `127.0.0.1:8080`, so every container in the pod can read the cache's whole container. Do not co-locate the sidecar with containers you do not trust with that data. A Kubernetes NetworkPolicy cannot protect a loopback listener, and a sidecar port is not a security boundary between containers in one pod.

Access to the cache itself is controlled by the mount. The webhook mounts `/run/racer/<cache>/client` read-only, and a container that cannot mount that directory cannot open the socket. The socket is accessible to the sidecar's UID, and a read-only mount still permits `connect(2)` because the kernel checks permission on the socket inode, not on the mount's read-only flag. The sidecar runs as UID and GID 65532 with a read-only root filesystem, no capabilities, and the `RuntimeDefault` seccomp profile.

Provider headers are handled by an allowlist. On the S3 path, the sidecar accepts the following credential and plumbing headers but ignores their contents:

| Header | Why it is accepted |
| --- | --- |
| `x-amz-date` | Client signing timestamp |
| `x-amz-content-sha256` | Client payload hash declaration |
| `x-amz-security-token` | Temporary session token |
| `x-amz-region-set` | Region set for SigV4a |
| `x-amz-s3session-token` | S3 session token |
| `x-amz-user-agent` | SDK telemetry |

Any other `x-amz-*` header is rejected with 501. On the Azure path, the accepted provider headers are `x-ms-range`, `x-ms-version`, `x-ms-date`, `x-ms-client-request-id`, `x-ms-return-client-request-id`, and `x-ms-useragent`. Any other `x-ms-*` header, such as a lease ID, is rejected with 501. A signed query string is also rejected, because the sidecar does not verify signatures and a presigned URL would otherwise look like an authorization decision that was never made.

Upstream authentication belongs to the cache, not the sidecar. The `ClusterCache` origin supports an omitted credential for anonymous access, a default credential that uses the node's managed identity through the Azure Instance Metadata Service, and a workload identity credential that exchanges a projected ServiceAccount token for a Microsoft Entra token. When a credential is configured, the container URL must use HTTPS. The cache's origin credential is not mounted into the sidecar or passed to the application pod.

The practical rules are:

- Bind only to a loopback address. The webhook and the CLI both enforce this for the object listener.
- Do not enable `hostNetwork`. The webhook denies it, and a manual pod should not do it either.
- Do not expose the diagnostics listener on a routable address unless you intend to. It has no authentication and no TLS.
- Treat the pod as the trust boundary. If two workloads need different access, put them in different pods with different cache mounts.

## Package APIs

The deployed sidecar is the Rust binary, but the repository also contains the library that the binary is built from and an in-tree Go handler that implements the same object rules. Use the binary when you need an HTTP endpoint. Use the Go packages when you are writing Go code that reads the cache directly or when you are extending the repository's tooling.

### The Rust crate

`cmd/racer-object` is a standalone crate with a library target named `racer_object` and a binary target named `racer-object`. It is marked `publish = false`, so it is not a crates.io dependency. The library exposes the modules that the binary composes:

| Module | Responsibility |
| --- | --- |
| `cli` | Argument parsing, usage text, version string, cache and listen validation |
| `object` | Request target, bucket and key rules, object identity, query, conditions, ranges, error codes |
| `racer` | Racer client-socket protocol, metadata requests, subscriptions, credits, page frames |
| `server` | HTTP/1.1 parsing, request flow, worker handoff, response heads |
| `splice` | Zero-copy body forwarding through a pipe |
| `http` | Response head construction, dates, XML error responses |
| `debug` | Optional `/healthz` listener |
| `lib` | Socket path derivation, listener binding, single-worker and multi-worker entry points |

The library's public surface is not a stability promise; the command line and the HTTP API are. If you embed the crate, pin the revision and expect internal modules to change. The Go handler is the better choice for most in-tree integrations because it shares the repository's API types and error conventions.

### The Go handler package

`internal/racerobject` builds the same object identity and serves the same read-only API through a `net/http` handler. It is internal to the module, so only code in this repository can import it. The deployed binary does not use it. Repository tests use the handler as an S3 endpoint, and operator code uses the URL and bucket validators.

| Function | Purpose |
| --- | --- |
| `NewRequest(bucket, key, versionID)` | Builds the canonical metadata and its SHA-256 key as a `racersdk.Request` |
| `ValidateBucket(bucket)` | Checks a bucket name against the parser's rules |
| `NewSidecar(client, config)` | Returns an `http.Handler` that serves the S3 or Azure read API |
| `ValidateAzureAccountURL(value, credential)` | Validates an Azure account URL for the origin configuration |
| `ParseAzureContainerURL(value, credential)` | Splits an account URL into the account URL and container name |

`NewSidecar` takes a `racersdk.Client` and a `SidecarConfig{API: "s3"}` or `SidecarConfig{API: "azure"}`. Install the returned handler directly. Do not put it behind a path-cleaning mux, because cleaning `..` or collapsing slashes would change object identities. Bind the listener to loopback and treat every caller as trusted, exactly as for the binary.

The Go handler always asks Racer for metadata before it reads content, including for an unconditional `GET`. The Rust binary has a one-subscription fast path for that case. The response semantics, error codes, range rules, and header set are otherwise the same, which is why repository tests can use the Go handler as a reference endpoint while production uses the binary.

### Reading the cache from Go

The public Go package is `pkg/racersdk`. It talks to the client socket directly and does not involve HTTP. Use it when you control the application and can link Go code, because it avoids the loopback hop and gives you the object as an `io.Reader` or an `io.WriterTo`.

```go
package main

import (
	"context"
	"io"
	"os"

	"github.com/Azure/unbounded/internal/racerobject"
	"github.com/Azure/unbounded/pkg/racersdk"
)

func main() {
	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: "models"})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	request, err := racerobject.NewRequest("models", "llama/config.json", "")
	if err != nil {
		panic(err)
	}

	object, err := client.Get(context.Background(), request)
	if err != nil {
		panic(err)
	}
	defer object.Close()

	if _, err := io.Copy(os.Stdout, object); err != nil {
		panic(err)
	}
}
```

`io.Copy` uses `Object.WriteTo`, which is the no-copy path for a destination such as a file or a network connection. If you need to inspect the bytes, call `Object.Read` instead and pass buffers of at least 32 KiB. `Client.Stat` returns metadata without reading content, and `ReadOptions` can set `Offset`, `Length`, `ETag`, and `SmallObject`. `Client.Prefetch` warms pages without returning content, which is useful before a latency-sensitive phase.

Outside this repository, you cannot import `internal/racerobject`. Compute the same canonical JSON and SHA-256 yourself, or vendor the small function, and pass the JSON string as `Request.Metadata`. The bytes must match exactly, including field order, because the key is a hash of the bytes, not of a parsed object.

## Using It from Scripts

Any HTTP client works. The examples below assume the default listener inside a pod, where the application container shares the network namespace with the sidecar. Set `endpoint` to the actual listen address if you changed `--listen`.

### Readiness and retry

A new pod can reach the sidecar before the node dataplane has published the cache socket. Once the listener is up, the sidecar replies 503 during that window; before the listener is up, the connection is refused. Retry both conditions, but do not retry 400, 404, 412, or 501; those are not transient.

```bash
#!/usr/bin/env bash
set -euo pipefail

endpoint=http://127.0.0.1:8080
bucket=models
key=llama/config.json

status=
for attempt in $(seq 1 30); do
  status=$(curl -sS -o /dev/null -w '%{http_code}' -I \
    "$endpoint/$bucket/$key" || true)
  case "$status" in
    200) break ;;
    000 | "") sleep 1 ;;
    503) sleep 1 ;;
    *) echo "unexpected status $status" >&2; exit 1 ;;
  esac
done
test "$status" = 200
```

The loop makes at most 30 attempts and sleeps one second after a connection failure or a 503. A curl failure is captured as status `000` and retried, so the script also handles the short window before the listener is bound. A 503 on the final attempt fails the `test` and exits. Adjust the bound to the startup behavior of your nodes; the sidecar itself does not need a readiness probe, and the pod can be running before the cache is ready.

### Download and verify

```bash
curl -fsS "$endpoint/$bucket/$key" -o config.json
printf '%s  %s\n' '<sha256>' config.json | sha256sum -c -
```

`-f` makes curl fail on a 4xx or 5xx response instead of writing the XML error body to the output file. Replace `<sha256>` with the digest published by the producer; the command prints `config.json: OK` only when the downloaded bytes match. For an atomic update, download to a temporary file in the destination directory, verify it, and rename it into place.

### Read metadata

```bash
curl -fsSI "$endpoint/$bucket/$key"
```

`-I` sends `HEAD`, which reads metadata only. The output includes `Content-Length`, `Etag`, `Content-Type`, and an `X-Amz-Version-Id` or `X-Ms-Version-Id` when a version was selected. Use this to discover the size before deciding how to split a range or how many parallel readers to start.

### Read a range

```bash
curl -fsS -H 'Range: bytes=0-4095' \
  "$endpoint/$bucket/$key" -o first-4k.bin
curl -fsS -D tail-headers.txt -H 'Range: bytes=-4096' \
  "$endpoint/$bucket/$key" -o last-4k.bin
```

The first request asks for the first 4096 bytes. The second asks for the last 4096 bytes and writes the response headers to `tail-headers.txt`, where `Content-Range` shows the exact selection. The sidecar does not accept multiple ranges in one request; issue one request per range instead.

### Conditional read

```bash
etag=$(curl -fsSI "$endpoint/$bucket/$key" \
  | awk -F': ' 'tolower($1)=="etag" {print $2}' \
  | tr -d '\r')
curl -fsS -H "If-None-Match: $etag" \
  -o /dev/null -w '%{http_code}\n' "$endpoint/$bucket/$key"
```

If nothing changed, the status is 304 and the body is empty. A changed object returns 200 and the full body. Use `If-Match` instead when you need the read to fail on change:

```bash
curl -fsS -H "If-Match: $etag" "$endpoint/$bucket/$key" -o config.json
```

A mismatch returns 412 and curl exits nonzero with `-f`. That is the intended signal to re-read metadata and retry with the new ETag.

### Versioned read

```bash
curl -fsS "$endpoint/$bucket/$key?versionId=<version-id>" -o config.json
```

Replace `<version-id>` with a value reported by a previous response or by the storage provider. For the Azure API, use `?versionid=<version-id>` instead. If Racer reports the version unavailable, the sidecar returns 412. The version query and an `If-Match` header are independent controls; use one or the other unless you have a reason to combine them.

### Error body

```bash
status=$(curl -sS -o error.xml -w '%{http_code}' "$endpoint/$bucket/missing-key")
echo "$status"
cat error.xml
```

This prints the status and the XML error code without failing the shell. It is useful in a probe or a diagnostic job where you want to distinguish a missing object (404), a bad request (400), and an unavailable cache (503).

### AWS CLI

The AWS CLI works against the S3 flavor. It insists on credentials even though the sidecar ignores the signature, so provide dummy values:

```bash
AWS_ACCESS_KEY_ID=unused AWS_SECRET_ACCESS_KEY=unused \
aws --endpoint-url http://127.0.0.1:8080 --region us-east-1 \
  s3api get-object --bucket models --key llama/config.json config.json
```

`head-object` and `--version-id` work as well. Do not enable checksum mode on a `get-object`; the sidecar does not implement the checksum response fields, and an unknown `x-amz-checksum-*` header is rejected with 501. The same endpoint rejects presigned URLs, so generate them elsewhere or use curl without a signature.

### Azure Blob with curl

The Azure flavor accepts the standard blob path and reports the Azure service version. No authentication header is needed:

```bash
curl -fsS -H 'x-ms-version: 2026-02-06' \
  "$endpoint/$bucket/$key" -o config.json
```

Use `-H 'x-ms-range: bytes=0-4095'` for a range. Do not send a shared access signature in the query string; the sidecar rejects it with 501. Use the S3 flavor only if your client is an S3 client; the two flavors share the same underlying bytes.

## Error Reference

Every failure has an HTTP status and, except for a `HEAD` response, an XML body with an API-specific code. The message is the standard reason phrase. This table maps the statuses to their S3 and Azure codes and to the usual causes.

| Status | S3 code | Azure code | Typical cause |
| --- | --- | --- | --- |
| 400 | `InvalidArgument` | `InvalidInput` | Malformed path, missing key, invalid bucket or key, bad query, bad condition, bad range |
| 403 | `AccessDenied` | `AuthorizationFailure` | Racer reported unauthorized or forbidden, usually an origin credential or permission problem |
| 404 | `NoSuchKey` | `BlobNotFound` | The origin has no blob at that bucket and key |
| 405 | `MethodNotAllowed` | `UnsupportedHttpVerb` | A method other than GET, HEAD, or the bare `OPTIONS *` probe |
| 412 | `PreconditionFailed` | `ConditionNotMet` | `If-Match` did not match, or the requested version is no longer available |
| 416 | `InvalidRange` | `InvalidRange` | Range starts at or beyond the object size, or a zero-length suffix |
| 431 | `RequestHeaderSectionTooLarge` | `InvalidInput` | Request head exceeds 36 KiB or 256 headers |
| 500 | `InternalError` | `InternalError` | Unexpected internal failure |
| 501 | `NotImplemented` | `NotImplemented` | Unsupported query, header, or operation |
| 502 | `BadGateway` | `BadGateway` | Racer protocol violation or a cross-check mismatch |
| 503 | `ServiceUnavailable` | `ServerBusy` | Racer unreachable, overloaded, timed out, or the request queue is full |

A 304 is not an error and has no XML body. A 412 for a conditional read and a 412 for an unavailable version look the same to the client; re-read metadata to learn the current ETag before deciding whether to retry.

## Troubleshooting

| Symptom | Likely cause | What to do |
| --- | --- | --- |
| Connection refused on the listen address | Sidecar not started, wrong `--listen`, or the client is in a different network namespace | Check the container logs and the pod's listener; the sidecar and client must share the pod network |
| 503 at pod startup, then success | Cache socket not published yet, or the dataplane is restarting | Retry with a bounded backoff; check that the cache exists and its dataplane is ready |
| Persistent 503 | Racer unreachable, queue full, or request timeout too small | Check the cache, raise `--max-connections` or `--request-timeout`, and reduce client parallelism |
| 404 for a blob you know exists | Wrong key, an object that the origin has not published yet, or a bucket the cache does not serve | Confirm the bucket equals the origin container and that the key has no accidental leading or trailing slash |
| 400 with no obvious path problem | Missing key, empty value, invalid escape, or a malformed condition | Print the exact request target; a path with a bucket but no key is 400, not 404 |
| 501 from an SDK | The SDK sent an unsupported header or query, such as checksum mode or a presigned URL | Disable that feature or use curl against the documented read API |
| 412 on every read | The ETag changed between metadata and content, or the version is gone | Re-read metadata and retry with the new ETag; check whether another writer is replacing the object |
| 403 from every read | The origin credential lacks permission, or the container URL is wrong | Check the `ClusterCache` origin, its identity, and its container URL |
| 502 | Racer returned a malformed response or a range that did not match | Check the dataplane and origin; a 502 is a bug or a misbehaving upstream, not a client mistake |
| Short body and a client EOF error | Racer failed or the request timed out after the head was sent | Retry the read; inspect dataplane logs for the underlying failure |
| 431 | Cookies or tracing headers made the request head too large | Reduce headers; the limit is 36 KiB and 256 headers |
| 405 on an upload | The sidecar is read-only by design | Upload to the origin directly, outside the cache |

When you report a problem, include the request method, path without secrets, status, error code, sidecar flags, cache name, and whether the same object reads successfully through the Racer client socket. That distinguishes a sidecar translation issue from a cache or origin issue.

## Related

- [Racer concepts]({{< relref "concepts/racer" >}}) explains caches, dataplane nodes, origins, and the socket layout.
- [ClusterCache reference]({{< relref "reference/racer" >}}) documents the resource, its origin, and its capacity settings.
- [Azure Blob cache]({{< relref "guides/azure-blob-cache" >}}) walks through admission, a first read, and verification.
- [Racer with vLLM]({{< relref "guides/racer-vllm" >}}) shows ETag pinning across a multi-file model.
- [Operations]({{< relref "guides/operations" >}}) covers day-two checks for Racer and the workloads that depend on it.
