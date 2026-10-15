---
title: "racer-object Reference"
weight: 8
description: "Flags, supported requests, Azure Blob support, and the security model for the racer-object adapter."
---

`racer-object` is a read-only S3 and Azure Blob adapter for Racer. For a working
setup, start with [Cache an S3 Bucket with Racer]({{< relref "guides/racer-s3" >}}).

The binary has two commands:

- `origin` runs on every eligible Racer node and reads the upstream store.
- `sidecar` runs in the application pod and serves the read API on pod loopback.

The read path is application -> sidecar -> Racer -> origin -> upstream. Racer owns
the cache. Neither command keeps a separate cache, and neither falls back to the
upstream when a cache read fails.

Build it with `make racer-object` (binary at `bin/racer-object`) or
`make image-racer-object-local`.

## Flags

### Both Commands

| Flag | Default | Meaning |
|------|---------|---------|
| `--volume` | `racer-object` | Racer ClusterVolume name. Must match the socket mount paths. |
| `--namespace` | none, required | Stable identifier for the upstream store. Not a Kubernetes namespace. |
| `--bucket` | empty | Allowed bucket or Azure container. Repeatable. Empty allows all. |
| `--debug-listen` | empty | Optional pprof and `/healthz` address. Unauthenticated and unencrypted. Restrict access. |
| `--version` | | Print version and exit. |

### Origin Only

| Flag | Default | Meaning |
|------|---------|---------|
| `--provider` | `s3` | `s3` or `azure`. |
| `--region` | empty | S3 only. Falls back to the AWS config chain. A region is required one way or the other. |
| `--endpoint` | empty | S3 only. HTTP or HTTPS origin for an S3-compatible store. No path, query, fragment, or credentials. |
| `--path-style` | `true` | S3 only. Set `false` for virtual-host addressing. |
| `--account-url` | empty | Azure only, required. For example `https://ACCOUNT.blob.core.windows.net`. |
| `--azure-credential` | `default` | Azure only. `default` or `anonymous`. |
| `--metadata-ttl` | `30s` | How long upstream metadata stays valid. Zero expires immediately. |
| `--request-timeout` | `1m` | Upstream request timeout. Must be positive. |

S3 and Azure flags are mutually exclusive. Setting `--region`, `--endpoint`, or
`--path-style` with `--provider=azure` is an error, and so is setting
`--account-url` or `--azure-credential` with `--provider=s3`.

### Sidecar Only

| Flag | Default | Meaning |
|------|---------|---------|
| `--api` | `s3` | Client-facing API: `s3` or `azure`. Independent of the origin's `--provider`. |
| `--listen` | `127.0.0.1:8080` | Loopback IP literal and port. Non-loopback addresses are rejected. |
| `--request-timeout` | `5m` | Request timeout. Note the different default from the origin. |

The sidecar accepts at most 128 concurrent connections.

There is no `--cache` flag. Older deployments that used it must move to `--volume`.

## Cache Identity

The cache key is the SHA-256 of canonical JSON holding the schema version,
`--namespace`, bucket, key, and version ID. It does **not** include the region,
endpoint, provider, or account URL.

That has one hard rule: change `--namespace` whenever you point the adapter at a
different account, provider, or endpoint. Otherwise Racer can serve bytes cached
from the old store. Do not reuse an old namespace for a different store in the
same volume. Requests cannot select origin credentials or an upstream endpoint.

Limits: namespace up to 256 bytes of valid UTF-8 with no spaces or control
characters, bucket up to 255 characters from `[a-zA-Z0-9._-]` and not `.` or `..`,
key 1 to 1024 bytes, version ID up to 1024 bytes, and 8 KiB for the whole
canonical key.

## Supported S3 Requests

URLs are path-style: `http://127.0.0.1:8080/BUCKET/KEY`.

Supported:

- `GET` and `HEAD` on a single object, including empty objects.
- One byte range: `bytes=0-1023`, `bytes=1024-`, or `bytes=-1024`.
- `If-Match` and `If-None-Match` with quoted ETags.
- An optional `versionId` query parameter.

Not supported: listing, any write, multipart operations, multi-range reads, date
conditions, `If-Range`, custom object metadata, response-header overrides, and
presigned URLs. Unknown `x-amz-*` headers return `501` rather than being ignored.
Owner checks, requester-pays, SSE-C, and optional checksums also return `501`.

Responses carry only `ETag`, `Content-Length`, `Content-Type`, `Accept-Ranges`,
range headers, and `x-amz-version-id` when a version was requested. There is no
`Last-Modified` and no user metadata.

Clients must preserve the exact key path, including repeated slashes and dot
segments. `curl --path-as-is` does this. SDK clients need path-style addressing
and `AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED`, or in Go
`s3.Options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired`.

### Upstream Requirements

The upstream must return strong quoted ETags and keep bytes stable for a given
ETag. The ETag does not have to be an MD5 hash. Page reads use `If-Match` so a
read cannot mix versions.

Objects with a non-identity `Content-Encoding`, such as gzip, are rejected,
because Racer's metadata cannot carry that header. Store unencoded bytes.

Mutable object names can stay stale for the length of `--metadata-ttl`. An
overwritten version may become unavailable mid-read. Use `versionId` when you need
an exact version and the upstream supports versioning.

### Error Codes

Errors are S3-style XML. The common ones:

| Status | Usual cause |
|--------|-------------|
| `400 InvalidArgument` | A malformed path or range, including a listing request or a multi-range read. |
| `403 AccessDenied` | Bucket not in `--bucket`, `--namespace` differs between origin and sidecar, or the origin cannot read the object. |
| `404 NoSuchKey` | No such object upstream. |
| `405 MethodNotAllowed` | Anything other than `GET` or `HEAD`. |
| `412 PreconditionFailed` | ETag mismatch, or the version is gone. |
| `501 NotImplemented` | An unsupported header or query parameter. |
| `503 ServiceUnavailable` | Racer unavailable on this node, or the request timed out. |

A stream can fail after headers are sent. That leaves partial output. Discard it
rather than treating it as a complete object.

## Azure Blob Support

The origin provider and the sidecar API are separate choices. Both default to
`s3`. An Azure origin works behind the default S3 sidecar API: the bucket is the
container and the key is the blob name.

### Azure Origin

Use `deploy/racer-object/origin-azure.yaml`. It replaces the same DaemonSet as
`origin.yaml`, so apply only one. Set `--provider=azure`, an `--account-url`, a
stable `--namespace`, and the containers you allow with `--bucket`. Do not set the
S3 flags or AWS environment variables.

`--azure-credential=default` uses `DefaultAzureCredential`, covering environment,
workload identity, and managed identity. With it, `--account-url` must use HTTPS.
Select a user-assigned identity with the `AZURE_CLIENT_ID` environment variable;
there is no flag for it. The example reads that variable from an optional Secret
named `racer-object-azure`.

Setting a client ID does not attach an identity or grant access. The identity must
be attached to the origin pods and hold **Storage Blob Data Reader** at the account
or container scope, on every eligible node.

For workload identity you also need the pod label
`azure.workload.identity/use: "true"`, a service account with the
`azure.workload.identity/client-id` annotation, federated credential trust matching
the cluster issuer and service account subject, and `AZURE_TENANT_ID` plus
`AZURE_FEDERATED_TOKEN_FILE`. The example configures none of this.

SAS tokens and shared keys are not supported. Use `--azure-credential=anonymous`
only for blobs that allow public reads. It controls origin-to-Azure access only,
not sidecar access.

### Azure Blob API

Set `sidecar --api=azure`. URLs are `http://127.0.0.1:8080/CONTAINER/BLOB`, with no
account name in the path.

Supported: block blob `GET` and `HEAD`, empty blobs, `If-Match` and `If-None-Match`,
a lowercase `versionid` query parameter, and one byte range on `GET`. `x-ms-range`
wins over `Range`. `HEAD` ignores both.

Not supported: listing, writes, page and append blobs, snapshots, multi-range reads,
date conditions, `If-Range`, leases, customer-provided keys, and checksum requests
such as `x-ms-range-get-content-md5`. Unsupported `x-ms-*` headers are rejected, not
ignored. The `$root`, `$web`, and `$logs` containers are not accepted. Responses
omit `Last-Modified` and user metadata.

For the Go SDK, use an anonymous client against the local address:

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

Do not use the Azure account URL or an Azurite-style `/ACCOUNT` path here.

## Security Model

**There is no per-client authorization.** The origin holds the upstream
credentials. Authorization headers sent to the sidecar, including SigV4 and Azure
credentials, are ignored. They are not verified and not forwarded. Unsigned
requests work. SDKs that insist on credentials can use placeholders. Presigned and
SAS query parameters are rejected. Do not send real credentials to the sidecar.

Consequences:

- Every process in the application pod can read everything the sidecar exposes.
- Keep the listener on loopback. No Service, no host networking, no external proxy.
- Permission to mount a volume's client socket grants access to its cached data.
  Origin-side checks cannot authorize a cache hit.
- Separate trust domains with separate volumes, namespaces, socket mount
  permissions, and upstream access policies.
- A `--bucket` allowlist narrows what the adapter fetches. It is not a per-user
  policy.

Keep credentials out of flags, images, and source control. Grant the origin only
the object read permissions it needs, including version reads if you use them.

## Socket Mounts

| Container | Host path and container mount path | hostPath type | Access |
|-----------|------------------------------------|---------------|--------|
| Origin | `/run/racer/<volume>/origin` | `DirectoryOrCreate` | Writable |
| Sidecar | `/run/racer/<volume>/client` | `Directory` | Read-only |

Both containers run as UID/GID 0 because Racer's sockets are root-owned and mode
`0600`. They still drop all capabilities, disable privilege escalation, use the
`RuntimeDefault` seccomp profile, and have read-only root filesystems. The adapter
does not need privileged mode.

Mount only these volume-specific directories, never all of `/run/racer`. Keep
their ancestors trusted and free of symlinks. A read-only client mount still
allows socket connections; it does not remove read access to cached data. Never
delete a live socket to take over from another owner.

## Placement

Run exactly one origin per volume on every eligible Racer node, not only the nodes
running application pods. Racer can select any eligible node to supply an origin
read. The example DaemonSet follows Racer's default Linux placement and skips
nodes labeled `racer.unbounded-cloud.io/exclude`. Match any custom Racer selectors,
affinity, and tolerations.

Application pods must run on nodes running Racer.

The example pods have no HTTP readiness probe, on purpose: Kubernetes probes target
the pod IP, and the sidecar listens on pod loopback. Pod readiness therefore says
nothing about cache or upstream readiness. `--debug-listen` serves `/healthz`, but
that only reports that the diagnostic listener is up.

## Tests

```bash
make racer-object-test
```

These cover adapter behavior through the Go SDK test bridge and check the example
manifests. The bridge does not cache data. They do not establish live multi-node
behavior, admission compatibility, or compatibility with every S3 provider or Azure
identity setup. Validate those in your own environment.
