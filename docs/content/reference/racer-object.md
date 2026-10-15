---
title: "racer-object Reference"
weight: 8
description: "Flags, deployment requirements, supported requests, and the security model for the racer-object adapter."
---

{{< callout type="warning" >}}
Racer is pre-release. Its APIs, flags, and on-disk formats may change without a
migration path. Cached data is disposable; Racer is not durable storage. Start
with the [racer-object guide]({{< relref "guides/racer-object" >}}).
{{< /callout >}}

`racer-object` is a read-only object storage adapter for Racer. It reads from S3,
S3-compatible stores, and Azure Blob Storage, and serves either an S3 or an Azure
Blob API to applications. For a working setup, start with
[Cache Object Storage with Racer]({{< relref "guides/racer-object" >}}).

The binary has two commands:

- `origin` runs on every eligible Racer node and reads the upstream store.
- `sidecar` runs in the application pod and serves the read API on pod loopback.

The read path is application -> sidecar -> Racer -> origin -> upstream. Racer owns
the cache. Neither command keeps a separate cache, and neither falls back to the
upstream when a cache read fails.

The origin provider (`--provider`) and the sidecar API (`--api`) are independent.
Both default to `s3`, and any combination works. In this page, "bucket" means an S3
bucket or an Azure container, and "key" means an S3 object key or an Azure blob
name.

Unbounded releases publish the image as `ghcr.io/azure/racer-object:vX.Y.Z`; use
the same [release](https://github.com/Azure/unbounded/releases) tag as your
operator's `racer-controller` and `racer-dataplane` images. Contributors can build
from source with `make racer-object` (binary at `bin/racer-object`) or
`make image-racer-object-local`.

## Flags

### Both Commands

| Flag | Default | Meaning |
|------|---------|---------|
| `--volume` | `racer-object` | Racer ClusterCache name. Must match the socket mount paths. |
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

## Upstream Providers

### S3

Set `--provider=s3` (the default) and a region, either with `--region` or through
the AWS config chain. For an S3-compatible store other than AWS, also set
`--endpoint`. Prefer HTTPS. Use `--path-style=false` only if the store requires
virtual-host addressing.

Credentials come from the AWS default credential chain: environment variables,
shared config, web identity, or instance and container credentials. Prefer
workload identity or another short-lived provider. The example `origin.yaml` reads
static keys from an optional Secret named `racer-object-aws` for quick tests only.

Grant the origin read access to the objects it serves, such as `s3:GetObject`, and
`s3:GetObjectVersion` if clients request versions.

### Azure Blob

Set `--provider=azure` and `--account-url`. Do not set the S3 flags or AWS
environment variables. Only block blobs are served.

`--azure-credential=default` uses `DefaultAzureCredential`, covering environment,
workload identity, and managed identity. With it, `--account-url` must use HTTPS.
Select a user-assigned identity with the `AZURE_CLIENT_ID` environment variable;
there is no flag for it. The example `origin-azure.yaml` reads that variable from
an optional Secret named `racer-object-azure`.

Setting a client ID does not attach an identity or grant access. The identity must
be attached to the origin pods and hold **Storage Blob Data Reader** at the account
or container scope, on every eligible node.

For workload identity you also need the pod label
`azure.workload.identity/use: "true"`, a service account with the
`azure.workload.identity/client-id` annotation, federated credential trust matching
the cluster issuer and service account subject, and `AZURE_TENANT_ID` plus
`AZURE_FEDERATED_TOKEN_FILE`. The example configures none of this, and sets
`automountServiceAccountToken: false`.

SAS tokens and shared keys are not supported. Use `--azure-credential=anonymous`
only for blobs that allow public reads. It controls origin-to-Azure access only,
not sidecar access.

### Upstream Requirements

The upstream must return strong quoted ETags and keep bytes stable for a given
ETag. The ETag does not have to be an MD5 hash. Page reads use `If-Match` so a
read cannot mix versions.

Objects with a non-identity `Content-Encoding`, such as gzip, are rejected,
because Racer's metadata cannot carry that header. Store unencoded bytes.

Mutable object names can stay stale for the length of `--metadata-ttl`. An
overwritten version may become unavailable mid-read. Request a specific version
when you need an exact one and the upstream supports versioning.

## Deploying the Origin

Run the origin as a DaemonSet in the Racer installation namespace, one per
ClusterCache. Start from
[`deploy/racer-object/origin.yaml`](https://github.com/Azure/unbounded/tree/main/deploy/racer-object/origin.yaml)
for S3 or
[`deploy/racer-object/origin-azure.yaml`](https://github.com/Azure/unbounded/tree/main/deploy/racer-object/origin-azure.yaml)
for Azure Blob. Both define the DaemonSet `racer-object-origin`, so apply only one
per cache.

Requirements:

- **Placement.** Run exactly one origin per cache on every eligible Racer node,
  not only the nodes running application pods. Racer can select any eligible node
  to supply an origin read. The examples follow Racer's default Linux placement and
  skip nodes labeled `racer.unbounded-cloud.io/exclude`. Match any custom Racer
  selectors, affinity, and tolerations.
- **Socket mount.** Mount `/run/racer/<cache>/origin` from the host, writable,
  with hostPath type `DirectoryOrCreate`. See [Socket Mounts](#socket-mounts).
- **Security context.** Run as UID/GID 0, with no privilege escalation, all
  capabilities dropped, `RuntimeDefault` seccomp, and a read-only root filesystem.
- **Credentials.** Supply upstream credentials to the origin pods only. See
  [Upstream Providers](#upstream-providers).
- **Arguments.** Set `--volume`, `--namespace`, and `--bucket` to match the
  sidecars, plus the provider flags.

The essential parts of the S3 example:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: racer-object-origin
spec:
  template:
    spec:
      automountServiceAccountToken: false
      nodeSelector:
        kubernetes.io/os: linux
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                  - key: racer.unbounded-cloud.io/exclude
                    operator: DoesNotExist
      securityContext:
        runAsUser: 0
        runAsGroup: 0
        runAsNonRoot: false
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: origin
          image: ghcr.io/azure/racer-object:vX.Y.Z
          args:
            - origin
            - --volume=racer-object
            - --namespace=example-store
            - --bucket=example-bucket
            - --region=us-east-1
          envFrom:
            - secretRef:
                name: racer-object-aws
                optional: true
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
          volumeMounts:
            - name: origin
              mountPath: /run/racer/racer-object/origin
      volumes:
        - name: origin
          hostPath:
            path: /run/racer/racer-object/origin
            type: DirectoryOrCreate
```

For Azure Blob, replace the S3 arguments with `--provider=azure`,
`--account-url`, and optionally `--azure-credential`, and replace `envFrom` with
your Azure identity configuration.

A running origin pod does not prove upstream access; only a real object read does.
Changing a credentials Secret requires restarting the origin pods.

## Deploying the Sidecar

Add the sidecar as an extra container in each application pod that reads objects.
Start from
[`deploy/racer-object/sidecar-pod.yaml`](https://github.com/Azure/unbounded/tree/main/deploy/racer-object/sidecar-pod.yaml).

Requirements:

- **Placement.** The pod must run on a node running Racer. Use the same node
  selector, affinity, and tolerations as Racer.
- **Socket mount.** Mount `/run/racer/<cache>/client` from the host, read-only,
  with hostPath type `Directory`, into the sidecar container only. See
  [Socket Mounts](#socket-mounts).
- **Security context.** Run the sidecar container as UID/GID 0, with no privilege
  escalation, all capabilities dropped, and a read-only root filesystem. The
  application containers can run as any user.
- **Arguments.** Set `--volume`, `--namespace`, and `--bucket` to match the
  origin. Add `--api=azure` if the application uses the Azure Blob API.
- **No credentials.** Do not give the sidecar or the application upstream
  credentials.
- **No probes or Services.** Do not add an HTTP readiness probe or expose the
  listener. See [Placement and Readiness](#placement-and-readiness).

The application reaches the sidecar at `http://127.0.0.1:8080`. The essential
parts of the example:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: racer-object-example
spec:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: racer.unbounded-cloud.io/exclude
                operator: DoesNotExist
  containers:
    - name: app
      image: curlimages/curl:8.20.0
      command: ["sleep", "infinity"]
    - name: sidecar
      image: ghcr.io/azure/racer-object:vX.Y.Z
      args:
        - sidecar
        - --volume=racer-object
        - --namespace=example-store
        - --bucket=example-bucket
        - --listen=127.0.0.1:8080
        # Add --api=azure for Azure Blob clients.
      securityContext:
        runAsUser: 0
        runAsGroup: 0
        runAsNonRoot: false
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ALL]
      volumeMounts:
        - name: client
          mountPath: /run/racer/racer-object/client
          readOnly: true
  volumes:
    - name: client
      hostPath:
        path: /run/racer/racer-object/client
        type: Directory
```

The same container works in a Deployment, StatefulSet, or Job pod template. On
Kubernetes versions with native sidecar support, you can also run it as an init
container with `restartPolicy: Always` so it starts before the application.

## Cache Identity

The cache key is the SHA-256 of canonical JSON holding the schema version,
`--namespace`, bucket, key, and version ID. It does **not** include the region,
endpoint, provider, or account URL.

That has one hard rule: change `--namespace` whenever you point the adapter at a
different account, provider, or endpoint. Otherwise Racer can serve bytes cached
from the old store. Do not reuse an old namespace for a different store in the
same cache. Requests cannot select origin credentials or an upstream endpoint.

Limits: namespace up to 256 bytes of valid UTF-8 with no spaces or control
characters, bucket up to 255 characters from `[a-zA-Z0-9._-]` and not `.` or `..`,
key 1 to 1024 bytes, version ID up to 1024 bytes, and 8 KiB for the whole
canonical key.

## Client APIs

Both APIs use the URL form `http://127.0.0.1:8080/BUCKET/KEY`, with no account
name, and are read-only. Both support:

- `GET` and `HEAD` on a single object, including empty objects.
- One byte range: `bytes=0-1023`, `bytes=1024-`, or `bytes=-1024`.
- `If-Match` and `If-None-Match` with quoted ETags.
- An optional version ID query parameter.

Neither supports listing, writes, multi-range reads, date conditions, `If-Range`,
or presigned and SAS URLs. Unsupported headers and query parameters return `501`
rather than being ignored. Responses omit `Last-Modified` and user metadata.

Clients must preserve the exact key path, including repeated slashes and dot
segments. `curl --path-as-is` does this.

### S3 API

The default, `--api=s3`. URLs are path-style.

- The version query parameter is `versionId`.
- Not supported: multipart operations, custom object metadata, and
  response-header overrides. Unknown `x-amz-*` headers, owner checks,
  requester-pays, SSE-C, and optional checksums return `501`.
- Responses carry only `ETag`, `Content-Length`, `Content-Type`, `Accept-Ranges`,
  range headers, and `x-amz-version-id` when a version was requested.

SDK clients need path-style addressing, placeholder credentials, and
`AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED`, or in Go
`s3.Options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired`.

### Azure Blob API

Set `sidecar --api=azure`.

- The version query parameter is a lowercase `versionid`. A positive integer
  `timeout` is also accepted. Any other query parameter returns `501`.
- `x-ms-range` wins over `Range`. `HEAD` ignores both.
- Not supported: page and append blobs, snapshots, leases, customer-provided keys,
  and checksum requests such as `x-ms-range-get-content-md5`. The only accepted
  `x-ms-*` request headers are `x-ms-range`, `x-ms-version`, `x-ms-date`,
  `x-ms-client-request-id`, `x-ms-return-client-request-id`, and
  `x-ms-useragent`.
- The `$root`, `$web`, and `$logs` containers are not accepted.
- Responses report `x-ms-blob-type: BlockBlob` and `x-ms-version-id` when a
  version was requested.

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

### Error Codes

Errors are XML bodies with a `Code` and `Message`. The Azure API also sets the
`x-ms-error-code` header. The common ones:

| Status | S3 code | Azure code | Usual cause |
|--------|---------|------------|-------------|
| `400` | `InvalidArgument` | `InvalidInput` | A malformed path or range, including a listing request or a multi-range read. |
| `403` | `AccessDenied` | `AuthorizationFailure` | Bucket not in `--bucket`, `--namespace` differs between origin and sidecar, or the origin cannot read the object. |
| `404` | `NoSuchKey` | `BlobNotFound` | No such object upstream. |
| `405` | `MethodNotAllowed` | `UnsupportedHttpVerb` | Anything other than `GET` or `HEAD`. |
| `412` | `PreconditionFailed` | `ConditionNotMet` | ETag mismatch, or the version is gone. |
| `416` | `InvalidRange` | `InvalidRange` | The range starts past the end of the object, or asks for a suffix of an empty object. |
| `501` | `NotImplemented` | `NotImplemented` | An unsupported header or query parameter. |
| `502` | `BadGateway` | `BadGateway` | An unexpected error from Racer, or the object changed between metadata and data reads. |
| `503` | `ServiceUnavailable` | `ServerBusy` | Racer unavailable on this node, or the request timed out. |

A stream can fail after headers are sent. That leaves partial output. Discard it
rather than treating it as a complete object.

## Security Model

**There is no per-client authorization.** The origin holds the upstream
credentials. Authorization headers sent to the sidecar, including SigV4 and Azure
credentials, are ignored. They are not verified and not forwarded. Unsigned
requests work. SDKs that insist on credentials can use placeholders. Presigned and
SAS query parameters are rejected. Do not send real credentials to the sidecar.

Consequences:

- Every process in the application pod can read everything the sidecar exposes.
- Keep the listener on loopback. No Service, no host networking, no external proxy.
- Permission to mount a cache's client socket grants access to its cached data.
  Origin-side checks cannot authorize a cache hit.
- Separate trust domains with separate caches, namespaces, socket mount
  permissions, and upstream access policies.
- A `--bucket` allowlist narrows what the adapter fetches. It is not a per-user
  policy.

Keep credentials out of flags, images, and source control. Grant the origin only
the object read permissions it needs, including version reads if you use them.

## Socket Mounts

| Container | Host path and container mount path | hostPath type | Access |
|-----------|------------------------------------|---------------|--------|
| Origin | `/run/racer/<cache>/origin` | `DirectoryOrCreate` | Writable |
| Sidecar | `/run/racer/<cache>/client` | `Directory` | Read-only |

Both containers run as UID/GID 0 because Racer's sockets are root-owned and mode
`0600`. They still drop all capabilities, disable privilege escalation, use the
`RuntimeDefault` seccomp profile, and have read-only root filesystems. The adapter
does not need privileged mode.

Mount only these cache-specific directories, never all of `/run/racer`. Keep
their ancestors trusted and free of symlinks. A read-only client mount still
allows socket connections; it does not remove read access to cached data. Never
delete a live socket to take over from another owner.

## Placement and Readiness

Origins must cover every eligible Racer node. Application pods must run on nodes
running Racer.

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
