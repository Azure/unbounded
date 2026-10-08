---
title: "Read S3 Objects with Racer"
weight: 10
description: "Use a pod-local HTTP sidecar to read S3-compatible objects through Racer."
---

`racer-object` is a read-only S3 adapter with two commands:

- `origin` runs on every eligible Racer node and reads an S3-compatible upstream.
- `sidecar` serves the application on pod loopback, using Racer's client socket.

The read path is application -> sidecar -> Racer mesh -> origin -> upstream.
Racer owns the cache. Neither command keeps a separate cache or bypasses Racer
when a cache read fails.

## Supported Requests

Use path-style URLs: `http://127.0.0.1:8080/BUCKET/KEY`.

- Object `GET` and `HEAD`, including empty objects.
- One byte range: `bytes=0-1023`, `bytes=1024-`, or `bytes=-1024`.
- `If-Match` and `If-None-Match` ETag conditions.
- An optional `versionId` query parameter.

This is not a full S3 endpoint. It does not support listing, writes, multipart
operations or multi-range reads, date conditions, `If-Range`, custom object
metadata, response-header overrides, or presigned URLs. Only size, ETag, content
type, range headers, and an explicitly requested version ID are exposed as object
response metadata. Use an SDK's path-style addressing option if you use one.
Clients must preserve the exact key path, including repeated slashes and dot
segments; `curl --path-as-is` does this.

Requests for owner checks, requester-pays, SSE-C, or optional checksums return
`501 NotImplemented`. For AWS SDK clients, set
`AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED` (or the SDK's equivalent option)
so reads do not request optional checksums. In Go, set
`s3.Options.ResponseChecksumValidation` to
`aws.ResponseChecksumValidationWhenRequired`. Normal SigV4 headers are accepted.
Objects with a non-identity `Content-Encoding`, such as gzip, are rejected because
Racer's metadata cannot preserve that header. Store unencoded bytes for this adapter.

The upstream must return strong quoted ETags and keep bytes stable for a given
ETag. An ETag need not be an MD5 hash. Page reads use `If-Match` to avoid mixing
versions. Mutable names may stay stale during the metadata TTL (30 seconds by
default). An overwritten version may become unavailable during a read. Use
`versionId` when the upstream supports versioning and you need a specific version.

## Trust and Credentials

**There is no per-client IAM authorization.** The origin uses the AWS SDK default
credential chain. Application SigV4 headers are ignored, not verified or forwarded.
Unsigned requests work; SDKs that require credentials can use non-secret
placeholders. Presigned query parameters are rejected.

Every process in the application pod can read the sidecar's exposed data. Keep the
listener on loopback; do not add a Service, host networking, or an external proxy.
Permission to mount a cache's client socket grants access to cached data. Origin
checks cannot authorize cache hits. Separate trust domains with separate caches,
upstream namespaces, socket mount permissions, and upstream IAM policies. A bucket
allowlist narrows the adapter's scope but is not a per-user cache access policy.

`--namespace` is a required, stable identifier for the upstream store and trust
domain, **not a Kubernetes namespace**. It is part of each cache key. Use the same
value on all origins and sidecars for that store. Do not reuse it for a different
account or endpoint in the same cache. Requests cannot choose origin credentials
or an upstream endpoint.

## 1. Build and Prepare

Install the operator and prepare Racer as described in the
[Racer guide]({{< relref "guides/racer" >}}). You need Linux nodes, a working
dataplane, permission to create ClusterCaches, and permission for narrowly scoped
hostPath mounts and UID 0 adapter containers. The adapters do not need privileged
mode. The Racer dataplane has separate security requirements.

From a source checkout:

```bash
make racer-object-build VERSION=dev
make image-racer-object-local VERSION=dev
```

The binary is `bin/racer-object`. The image includes CA certificates and version
metadata. It uses the repository's existing AWS SDK dependencies. Push the image
to your own registry or load it onto your test nodes, then replace
`racer-object:dev` in both example manifests with your image, preferably by digest.
The local build alone does not make the image available to cluster nodes.

The examples are in
[`deploy/racer-object`](https://github.com/Azure/unbounded/tree/main/deploy/racer-object).
They use `example-store`, `example-bucket`, and `us-east-1` as placeholders. Edit
them before applying. Commands below use the existing `unbounded-system`
Kubernetes namespace; use your chosen workload namespace if different.

## 2. Create the Cache and Configure the Origin

```bash
kubectl apply -f deploy/racer-object/cache.yaml
kubectl get ccache racer-object
```

ClusterCache is cluster-scoped, has no `spec`, and has no
readiness status. Creating it triggers operator-managed Racer installation. It is
not a PVC or durable storage. Wait for the controller and dataplane to be ready
using the Racer guide's checks before starting consumers.

Configure upstream access on **every** origin. Prefer workload identity or another
short-lived AWS credential provider. Set the service account, projected token,
environment variables, and mounts required by your provider; the example disables
automatic Kubernetes API token mounting and does not configure a cloud identity.
Grant only the required object read permissions, including version reads if used.

For an environment-based credential setup, the origin has an optional
`secretRef` named `racer-object-aws`. Create it in the workload namespace from an
existing protected environment file outside source control:

```bash
kubectl -n unbounded-system create secret generic racer-object-aws \
  --from-env-file="$AWS_ENV_FILE"
```

The file can contain `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and, for temporary
credentials, `AWS_SESSION_TOKEN`. Do not put credentials in manifests, command
flags, images, or source control. Rotate them through your normal secret process;
environment-based changes require restarting origin pods. If the Secret is absent,
the AWS default credential chain must obtain credentials another way.

Origin options:

| Flag | Default or requirement |
|------|------------------------|
| `--cache` | `racer-object`; match the ClusterCache name and mount paths |
| `--namespace` | Required stable upstream identifier |
| `--bucket` | Optional, repeatable allowlist; omitted means no bucket restriction |
| `--region` | Optional; falls back to AWS configuration when omitted |
| `--endpoint` | Optional S3-compatible endpoint; omitted uses AWS endpoint resolution |
| `--path-style` | `true`; use `false` if the upstream requires virtual-host addressing |
| `--metadata-ttl` | `30s` |
| `--request-timeout` | `1m` |

For a non-AWS upstream, set `--endpoint` to its HTTPS URL and configure the region
and credentials it expects. The URL must not contain credentials, a path prefix,
query parameters, or a fragment. The endpoint is configured only on the origin.

```bash
kubectl -n unbounded-system apply -f deploy/racer-object/origin.yaml
kubectl -n unbounded-system rollout status daemonset/racer-object-origin --timeout=5m
```

Run exactly one origin per cache on every eligible Racer origin-serving node,
not just on nodes hosting application pods. The example follows Racer's default
Linux placement and excludes nodes with the `racer.unbounded-cloud.io/exclude`
label. Match any custom Racer selectors, affinity, and tolerations, including all
eligible nodes in mixed-network installations. A running origin pod alone does
not prove upstream access; check a real object read.

## 3. Start the Sidecar and Read an Object

```bash
kubectl -n unbounded-system apply -f deploy/racer-object/sidecar-pod.yaml
kubectl -n unbounded-system wait --for=condition=Ready pod/racer-object-example --timeout=5m
kubectl -n unbounded-system exec racer-object-example -c app -- \
  curl --fail --show-error --path-as-is --max-time 300 \
  http://127.0.0.1:8080/example-bucket/example-key
kubectl -n unbounded-system exec racer-object-example -c app -- \
  curl --fail --show-error --path-as-is --max-time 300 --head \
  http://127.0.0.1:8080/example-bucket/example-key
kubectl -n unbounded-system exec racer-object-example -c app -- \
  curl --fail --show-error --path-as-is --max-time 300 -H 'Range: bytes=0-1023' \
  http://127.0.0.1:8080/example-bucket/example-key
```

Replace `example-key` with an existing object. For conditions, pass an upstream
ETag including its quotes, for example `-H 'If-Match: "ETAG"'`. For a versioned
read, append `?versionId=VERSION_ID`, URL-encoding the version ID. The example
application has no AWS credentials and no hostPath mount.

The sidecar shares `--cache`, `--namespace`, and repeatable `--bucket` options with
the origin. The `--volume` flag is not accepted. Its listener defaults to
`--listen=127.0.0.1:8080` and rejects non-loopback addresses. Its default request
timeout is `--request-timeout=5m`.
Place application pods only on nodes running Racer. The examples have no HTTP
readiness probe: Kubernetes probes target the pod IP, not pod loopback. Pod
readiness therefore does not establish cache or upstream readiness.

### Socket Mounts and Security

| Container | Host path and container mount path | Type | Access |
|-----------|------------------------------------|------|--------|
| Origin | `/run/racer/racer-object/origin` | `DirectoryOrCreate` | Writable |
| Sidecar | `/run/racer/racer-object/client` | `Directory` | Read-only |

Both adapter containers run as UID/GID 0 for Racer's root-owned `0600` sockets.
They drop all capabilities, disable privilege escalation, use `RuntimeDefault`
seccomp, and have read-only root filesystems and resource requests/limits. The
origin needs only its socket directory writable. A read-only client mount still
allows socket connections; it does not remove cache read permission. Mount only
these cache-specific directories, never all of `/run/racer`. Keep their ancestors
trusted and free of symlinks. Never remove a live socket to bypass another owner.

## Checks and Limits

```bash
timeout --signal=TERM --kill-after=10s 300s make racer-object-test VERSION=dev
```

The focused tests cover adapter behavior through the Go SDK test bridge and check
the example deployment contracts. The bridge does not cache data. These tests do
not establish live multi-node mesh reuse, Kubernetes admission compatibility, or
compatibility with every S3 provider. Validate those in your own environment.

If a read fails, check the origin logs, upstream credentials and permissions,
matching cache/namespace/bucket settings, and both socket paths. A missing client
directory means the consumer must wait for Racer on that node. `403` can indicate
an allowlist or upstream permission failure; `412` can indicate an ETag mismatch
or a version that is no longer available. A stream error can leave partial output;
discard it rather than treating it as a complete object.
