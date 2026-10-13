---
title: "Read S3 Objects and Azure Blobs with Racer"
weight: 10
description: "Use a pod-local HTTP sidecar to read S3-compatible objects and Azure block blobs through Racer."
---

`racer-object` is a read-only S3 and Azure Blob adapter with two commands:

- `origin` runs on every eligible Racer node and reads an S3-compatible or Azure Blob upstream.
- `sidecar` serves the S3 or Azure Blob read API on pod loopback, using Racer's client socket.

The read path is application -> sidecar -> Racer mesh -> origin -> upstream.
Racer owns the cache. Neither command keeps a separate cache or bypasses Racer
when a cache read fails.

## Supported Requests

The origin provider and sidecar API are separate choices. Both default to `s3`.
An Azure origin also works with the default S3 sidecar API: the bucket is the
container and the key is the blob name.

### S3 API (Default)

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

### Azure Blob API

Set `sidecar --api=azure`. Use `http://127.0.0.1:8080/CONTAINER/BLOB`, with no
account name in the path. The API supports block blob `GET` and `HEAD`, empty
blobs, `If-Match` and `If-None-Match` ETag conditions, and an optional lowercase
`versionid` query parameter. `GET` supports one closed, open-ended, or suffix byte
range. `x-ms-range` takes precedence over `Range`; `HEAD` ignores both range headers.

This is not a full Azure Storage endpoint. It does not support listing, writes,
page or append blobs, snapshots, multi-range reads, date conditions, `If-Range`,
leases, customer-provided keys (CPK), or optional checksum requests such as
`x-ms-range-get-content-md5` and `x-ms-range-get-content-crc64`. Unsupported
`x-ms-*` feature headers are rejected, not silently ignored. The existing name
validator excludes the special `$root`, `$web`, and `$logs` containers. Responses
do not expose `Last-Modified` or user metadata. Store unencoded bytes; non-identity
`Content-Encoding` is rejected. The same strong ETag and metadata TTL limits apply
as for S3. Use `versionid` for a specific Azure blob version.

## Trust and Credentials

**There is no per-client authorization.** The origin holds upstream credentials.
S3 origins use the AWS SDK default credential chain. Azure origins use
`DefaultAzureCredential` by default, or anonymous access when explicitly selected.
Application authorization headers, including SigV4 and Azure credentials, are
ignored, not verified or forwarded. Unsigned requests work; use an anonymous local
Azure SDK client. S3 SDKs that require credentials can use non-secret placeholders.
Presigned and SAS query parameters are rejected. Do not send real credentials to
the sidecar.

Every process in the application pod can read the sidecar's exposed data. Keep the
listener on loopback; do not add a Service, host networking, or an external proxy.
Permission to mount a volume's client socket grants access to cached data. Origin
checks cannot authorize cache hits. Separate trust domains with separate volumes,
upstream namespaces, socket mount permissions, and upstream access policies. A bucket
allowlist narrows the adapter's scope but is not a per-user cache access policy.

`--namespace` is a required, stable identifier for the upstream store and trust
domain, **not a Kubernetes namespace**. It is part of each cache key. Use the same
value on all origins and sidecars for that store. Cache keys keep the schema 1
format; the namespace identifies the upstream, not the API. **Change the namespace
when changing the account, provider, or endpoint.** Do not reuse an old namespace
for a different store in the same volume. Requests cannot choose origin credentials
or an upstream endpoint.

## 1. Build and Prepare

Install the operator and prepare Racer as described in the
[Racer guide]({{< relref "guides/racer" >}}). You need Linux nodes, a working
dataplane, permission to create ClusterVolumes, and permission for narrowly scoped
hostPath mounts and UID 0 adapter containers. The adapters do not need privileged
mode. The Racer dataplane has separate security requirements.

From a source checkout:

```bash
make racer-object-build VERSION=dev
make image-racer-object-local VERSION=dev
```

The binary is `bin/racer-object`. The image includes CA certificates and version
metadata. It uses the AWS and Azure SDKs. Push the image
to your own registry or load it onto your test nodes, then replace
`racer-object:dev` in the origin and sidecar manifests with your image, preferably by digest.
The local build alone does not make the image available to cluster nodes.

The examples are in
[`deploy/racer-object`](https://github.com/Azure/unbounded/tree/main/deploy/racer-object).
They use placeholder namespaces, buckets or containers, regions, and account URLs.
Edit them before applying. Commands below use the existing `unbounded-system`
Kubernetes namespace; use your chosen workload namespace if different.

## 2. Create the Cache Volume and Configure the Origin

```bash
kubectl apply -f deploy/racer-object/volume.yaml
kubectl get cvol racer-object
```

ClusterVolume is cluster-scoped, requires immutable `spec.type: Cache`, and has no
readiness status. Creating it triggers operator-managed Racer installation. It is
not a PVC or durable storage. Wait for the controller and dataplane to be ready
using the Racer guide's checks before starting consumers. For an existing
ClusterCache deployment, follow the [breaking-change upgrade steps]({{< relref "guides/racer" >}}#upgrade-from-clustercache).

Choose **one** origin manifest for each volume: `origin.yaml` for S3 or
`origin-azure.yaml` for Azure Blob. They update the same DaemonSet; do not apply
both. When switching providers, also change the namespace on origin and sidecar.

### S3 Origin

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

### Origin Options

| Flag | Default or requirement |
|------|------------------------|
| `--volume` | `racer-object`; match the ClusterVolume name and mount paths |
| `--namespace` | Required stable upstream identifier |
| `--provider` | `s3`; select `azure` for Azure Blob |
| `--bucket` | Optional, repeatable bucket or Azure container allowlist; omitted means no restriction |
| `--region` | S3 only; falls back to AWS configuration when omitted |
| `--endpoint` | S3 only; optional compatible endpoint; omitted uses AWS endpoint resolution |
| `--path-style` | S3 only; `true`; use `false` for virtual-host addressing |
| `--account-url` | Required for Azure, e.g. `https://ACCOUNT.blob.core.windows.net` |
| `--azure-credential` | Azure only; `default` or `anonymous`, default `default` |
| `--metadata-ttl` | `30s` |
| `--request-timeout` | `1m` |

For a non-AWS S3 upstream, set `--endpoint` to its HTTPS URL and configure the region
and credentials it expects. The URL must not contain credentials, a path prefix,
query parameters, or a fragment. The endpoint is configured only on the origin.

```bash
kubectl -n unbounded-system apply -f deploy/racer-object/origin.yaml
kubectl -n unbounded-system rollout status daemonset/racer-object-origin --timeout=5m
```

### Azure Blob Origin

In `origin-azure.yaml`, set `--provider=azure`, replace
`--account-url=https://ACCOUNT.blob.core.windows.net`, and choose a stable
`--namespace` and a container with `--bucket`. The account URL must not contain
credentials, an account path, query parameters, or a fragment. Do not use the S3
`--region`, `--endpoint`, or `--path-style` flags or AWS environment variables.
Azure origin authentication does not support SAS or shared keys.

`--azure-credential=default` uses `DefaultAzureCredential`, including environment,
workload identity, and managed identity credentials. Select a user-assigned
identity through `AZURE_CLIENT_ID`; there is no dedicated client ID flag. The
example reads this variable from the optional `AZURE_CLIENT_ID` key in the optional
`racer-object-azure` Secret. Setting a client ID does not attach an identity or
grant access. The selected identity must actually be attached or configured for
the origin and have **Storage Blob Data Reader** at the required account or
container scope on every eligible node.

For workload identity, configure the real pod labels, service account and its
annotations, federated identity trust, and projected token file and mounts. With
the Azure Workload Identity webhook, this includes the pod label
`azure.workload.identity/use: "true"`, a `serviceAccountName`, and that service
account's `azure.workload.identity/client-id` annotation. The federation must match
the cluster issuer and service account subject. The SDK needs `AZURE_TENANT_ID`,
`AZURE_CLIENT_ID`, and `AZURE_FEDERATED_TOKEN_FILE` pointing to the projected token.
Environment variables alone do not enable workload identity. The example disables
automatic Kubernetes API token mounting and does not configure this federation.
For managed identity, ensure the intended identity is attached and reachable from
each origin pod. Keep credentials out of flags, images, and source control.

Use `--azure-credential=anonymous` only for blobs that allow public reads. This
controls origin-to-Azure access, not sidecar authentication.

```bash
kubectl -n unbounded-system apply -f deploy/racer-object/origin-azure.yaml
kubectl -n unbounded-system rollout status daemonset/racer-object-origin --timeout=5m
```

### Origin Placement

Run exactly one origin per volume on every eligible Racer origin-serving node,
not just on nodes hosting application pods. The example follows Racer's default
Linux placement and excludes nodes with the `racer.unbounded-cloud.io/exclude`
label. Match any custom Racer selectors, affinity, and tolerations, including all
eligible nodes in mixed-network installations. A running origin pod alone does
not prove upstream access; check a real object read.

## 3. Start the Sidecar and Read an Object

For S3, the example below works as written after replacing the placeholders.
For an Azure origin, first edit `sidecar-pod.yaml` to use the same namespace and
container allowlist as the origin. Keep the default S3 API for S3 clients, or add
`--api=azure` for Azure Blob clients. Do not add upstream credentials to the app or
sidecar. Apply the edited pod manifest, not both API configurations at once.

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

The sidecar shares `--volume`, `--namespace`, and repeatable `--bucket` options with
the origin. The old `--cache` flag is not accepted. Its listener defaults to
`--listen=127.0.0.1:8080` and rejects non-loopback addresses. `--api` defaults to
`s3` and also accepts `azure`. Its default request timeout is `--request-timeout=5m`.
Place application pods only on nodes running Racer. The examples have no HTTP
readiness probe: Kubernetes probes target the pod IP, not pod loopback. Pod
readiness therefore does not establish cache or upstream readiness.

### Read Through the Azure API

With `--api=azure`, use the container and blob names without an account path:

```bash
kubectl -n unbounded-system exec racer-object-example -c app -- \
  curl --fail --show-error --path-as-is --max-time 300 \
  -H 'x-ms-range: bytes=0-1023' \
  http://127.0.0.1:8080/example-container/example-blob
```

Use `--head` for blob properties or append `?versionid=VERSION_ID` for a versioned
read, URL-encoding the version ID. ETag conditions use quoted upstream ETags.
For the Go Azure Blob SDK, create an anonymous client with the local service URL:

```go
client, err := azblob.NewClientWithNoCredential("http://127.0.0.1:8080", nil)
if err != nil {
    return err
}
response, err := client.DownloadStream(ctx, "example-container", "example-blob", nil)
if err != nil {
    return err
}
defer response.Body.Close()
```

Import `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob`. Read and close the
response body in your application. Do not use the Azure account URL or an
Azurite-style `/ACCOUNT` path for this local client.

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
these volume-specific directories, never all of `/run/racer`. Keep their ancestors
trusted and free of symlinks. Never remove a live socket to bypass another owner.

## Checks and Limits

```bash
timeout --signal=TERM 300s go test -timeout=4m ./cmd/racer-object/... ./internal/racerobject/... ./deploy/racer-object/...
```

The focused tests cover adapter behavior through the Go SDK test bridge and check
the example deployment contracts. The bridge does not cache data. These tests do
not establish live multi-node mesh reuse, Kubernetes admission compatibility, or
compatibility with every S3 provider or live Azure identity setup. Validate those
in your own environment.

If a read fails, check the origin logs, upstream credentials and permissions,
matching volume/namespace/bucket settings, and both socket paths. A missing client
directory means the consumer must wait for Racer on that node. `403` can indicate
an allowlist or upstream permission failure; `412` can indicate an ETag mismatch
or a version that is no longer available. A stream error can leave partial output;
discard it rather than treating it as a complete object.
