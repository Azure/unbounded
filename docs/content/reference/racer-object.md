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
| `--cache` | `racer-object` | Racer ClusterCache name. Must match the socket mount paths. |
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
| `--max-concurrent-requests` | `512` | Page requests served to Racer at once, and the size of the idle upstream connection pool. Requests beyond the limit are rejected as overloaded. 1 to 65536. |

S3 and Azure flags are mutually exclusive. Setting `--region`, `--endpoint`, or
`--path-style` with `--provider=azure` is an error, and so is setting
`--account-url` or `--azure-credential` with `--provider=s3`.

### Sidecar Only

| Flag | Default | Meaning |
|------|---------|---------|
| `--api` | `s3` | Client-facing API: `s3` or `azure`. Independent of the origin's `--provider`. |
| `--listen` | `127.0.0.1:8080` | Loopback IP literal and port. Non-loopback addresses are rejected. |
| `--request-timeout` | `5m` | Request timeout. Note the different default from the origin. |
| `--max-connections` | `128` | Client connections accepted at once, and concurrent reads from Racer. 1 to 65536. |

## Upstream Providers

### S3

Set `--provider=s3` (the default) and a region, either with `--region` or through
the AWS config chain. For an S3-compatible store other than AWS, also set
`--endpoint`. Prefer HTTPS. Use `--path-style=false` only if the store requires
virtual-host addressing.

Credentials come from the AWS default credential chain: environment variables,
shared config, web identity, or instance and container credentials. Prefer
workload identity or another short-lived provider. The S3 example below reads
static keys from an optional Secret named `racer-object-aws` for quick tests only.

Grant the origin read access to the objects it serves, such as `s3:GetObject`, and
`s3:GetObjectVersion` if clients request versions.

### Azure Blob

Set `--provider=azure` and `--account-url`. Do not set the S3 flags or AWS
environment variables. Only block blobs are served.

`--azure-credential=default` uses `DefaultAzureCredential`, covering environment,
workload identity, and managed identity. With it, `--account-url` must use HTTPS.
Select a user-assigned identity with the `AZURE_CLIENT_ID` environment variable;
there is no flag for it. One option is to read that variable from an optional
Secret, for example `racer-object-azure`, with `envFrom`.

Setting a client ID does not attach an identity or grant access. The identity must
be attached to the origin pods and hold **Storage Blob Data Reader** at the account
or container scope, on every eligible node.

For workload identity you also need the pod label
`azure.workload.identity/use: "true"`, a service account with the
`azure.workload.identity/client-id` annotation, federated credential trust matching
the cluster issuer and service account subject, and `AZURE_TENANT_ID` plus
`AZURE_FEDERATED_TOKEN_FILE`. The example below configures none of this, and sets
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
ClusterCache. Start from the S3 example below and change the provider flags for
Azure Blob. Run only one origin DaemonSet per cache.

Requirements:

- **Placement.** Run exactly one origin per cache on every eligible Racer node,
  not only the nodes running application pods. Racer can select any eligible node
  to supply an origin read. The examples follow Racer's default Linux placement and
  skip nodes labeled `racer.unbounded-cloud.io/exclude`. Match any custom Racer
  selectors, affinity, and tolerations.
- **Socket mount.** Mount `/run/racer/<cache>/origin` from the host, writable,
  with hostPath type `Directory`. The Racer dataplane creates this directory,
  owned by UID/GID 65532 with mode `0700`, when it sets up the cache on the node.
  Until then the pod waits in `ContainerCreating`. The origin never creates it.
  See [Socket Mounts](#socket-mounts).
- **Security context.** Run as UID/GID 65532, the image's default user, with
  `runAsNonRoot: true`, no privilege escalation, all capabilities dropped,
  `RuntimeDefault` seccomp, and a read-only root filesystem. The origin refuses
  to start unless the origin directory is owned by its UID, so other UIDs,
  including 0, do not work.
- **Credentials.** Supply upstream credentials to the origin pods only. See
  [Upstream Providers](#upstream-providers).
- **Arguments.** Set `--cache`, `--namespace`, and `--bucket` to match the
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
        runAsUser: 65532
        runAsGroup: 65532
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: origin
          image: ghcr.io/azure/racer-object:vX.Y.Z
          args:
            - origin
            - --cache=racer-object
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
            type: Directory
```

For Azure Blob, replace the S3 arguments with `--provider=azure`,
`--account-url`, and optionally `--azure-credential`, and replace `envFrom` with
your Azure identity configuration.

A running origin pod does not prove upstream access; only a real object read does.
Changing a credentials Secret requires restarting the origin pods.

## Deploying the Sidecar

The sidecar runs in each application pod that reads objects. The usual way to add
it is automatic injection. You can also add it to the pod spec by hand.

Injection needs Kubernetes 1.29 or later, because it uses native sidecars. In
both cases the sidecar runs as the non-root UID/GID 65532 but mounts a hostPath,
so the pod's namespace must allow hostPath volumes under Pod Security Admission. A namespace that enforces the `baseline` or
`restricted` level rejects the pod. Admission checks the pod after injection.

### Automatic Injection

`racer-controller` runs a mutating admission webhook that adds the sidecar to
opted-in pods. The unbounded operator installs the
`MutatingWebhookConfiguration` named `racer-object-sidecar-injector` and manages
its CA bundle. The webhook:

- Handles only pod `CREATE`. Existing pods are not changed.
- Matches only pods with the label
  `racer.unbounded-cloud.io/object-sidecar: enabled`.
- Skips the racer-controller namespace.
- Skips pods that already have a container named `racer-object`.
- Uses `failurePolicy: Fail`. If the webhook is down or an annotation is invalid,
  the pod is not created.

Put the label and annotations on the pod. For a Deployment, StatefulSet, or Job,
put them on the pod template, not the workload object:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: racer-object-example
spec:
  selector:
    matchLabels:
      app: racer-object-example
  template:
    metadata:
      labels:
        app: racer-object-example
        racer.unbounded-cloud.io/object-sidecar: enabled
      annotations:
        racer.unbounded-cloud.io/object-namespace: example-store
        racer.unbounded-cloud.io/object-buckets: example-bucket,other-bucket
        # Optional:
        # racer.unbounded-cloud.io/object-api: azure
        # racer.unbounded-cloud.io/object-memory-limit: 256Mi
    spec:
      containers:
        - name: app
          image: curlimages/curl:8.20.0
          command: ["sleep", "infinity"]
```

Annotations, all with the prefix `racer.unbounded-cloud.io/`:

| Annotation | Default | Meaning |
|------------|---------|---------|
| `object-namespace` | none, required | Sets `--namespace`. Must match the origin. |
| `object-buckets` | none, required | Comma-separated list. Each entry sets one `--bucket`. Must match the origin. |
| `object-cache` | `racer-object` | Sets `--cache` and the socket mount path. |
| `object-api` | flag default (`s3`) | Sets `--api`. `s3` or `azure`. |
| `object-listen` | flag default (`127.0.0.1:8080`) | Sets `--listen`. Loopback IP literal and port. |
| `object-cpu-request` | not set | Sidecar CPU request. |
| `object-cpu-limit` | not set | Sidecar CPU limit. |
| `object-memory-request` | not set | Sidecar memory request. |
| `object-memory-limit` | not set | Sidecar memory limit. |

An annotation that is present with an empty value is rejected. Values are checked
with the same rules as the flags; see [Cache Identity](#cache-identity) for
namespace and bucket limits.

Resource annotations take Kubernetes quantities, such as `100m` or `128Mi`. Each
must be greater than zero, and a request must not exceed its limit. With none set,
the sidecar has no requests or limits. If you set only a limit, Kubernetes sets the
request to the same value. Native sidecar resources count toward pod scheduling and
the pod's QoS class.

The webhook makes these changes to the pod:

- Adds an init container named `racer-object` with `restartPolicy: Always`, placed
  first. It uses the `racer-object` image of the same release as
  `racer-controller`, and the args
  `sidecar --cache=... --namespace=... --bucket=...` plus `--api` and `--listen`
  when set.
- Sets the security context described below, with `RuntimeDefault` seccomp.
- Adds the volume `racer-object-client`: hostPath `/run/racer/<cache>/client`,
  type `Directory`, mounted read-only in the sidecar only.
- Sets the node selector `kubernetes.io/os: linux`, and adds a required node
  affinity term `racer.unbounded-cloud.io/exclude DoesNotExist` to every node
  selector term.
- Adds the annotation `racer.unbounded-cloud.io/object-sidecar-injected` with the
  sidecar image.

The webhook does not add tolerations. Add them yourself if Racer runs on tainted
nodes.

#### Troubleshooting Injection

When the webhook denies a pod, the reason appears in the error from
`kubectl apply`, or for workloads in the ReplicaSet, StatefulSet, or Job events
(`kubectl describe`). The message names the bad annotation, for example
`annotation racer.unbounded-cloud.io/object-buckets is required`.

Other denial causes:

- The pod already has a volume named `racer-object-client`.
- The pod has a `kubernetes.io/os` node selector other than `linux`.

If the pod is created without a sidecar, check that the label is on the pod (not
only on the Deployment), that the pod is not in the racer-controller namespace,
and that no container is already named `racer-object`.

### Manual Pod Spec

Add the sidecar as an extra container in each application pod that reads objects.

Requirements:

- **Placement.** The pod must run on a node running Racer. Use the same node
  selector, affinity, and tolerations as Racer.
- **Socket mount.** Mount `/run/racer/<cache>/client` from the host, read-only,
  with hostPath type `Directory`, into the sidecar container only. See
  [Socket Mounts](#socket-mounts).
- **Security context.** Run the sidecar container as UID/GID 65532 with
  `runAsNonRoot: true`, no privilege escalation, all capabilities dropped,
  `RuntimeDefault` seccomp, and a read-only root filesystem. The
  application containers can run as any user.
- **Arguments.** Set `--cache`, `--namespace`, and `--bucket` to match the
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
        - --cache=racer-object
        - --namespace=example-store
        - --bucket=example-bucket
        - --listen=127.0.0.1:8080
        # Add --api=azure for Azure Blob clients.
      securityContext:
        runAsUser: 65532
        runAsGroup: 65532
        runAsNonRoot: true
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
| Origin | `/run/racer/<cache>/origin` | `Directory` | Writable |
| Sidecar | `/run/racer/<cache>/client` | `Directory` | Read-only |

The sidecar runs as the non-root UID/GID 65532. Racer creates client sockets with
mode `0666`, so access is controlled by which directory the pod mounts, not by the
process UID. The origin also runs as UID/GID 65532. The Racer dataplane creates
the origin directory owned by UID/GID 65532 with mode `0700` and repairs its
owner and mode if they drift. The origin creates its socket there with mode
`0600`; the privileged dataplane can still connect to it. Both drop all capabilities, disable
privilege escalation, use the `RuntimeDefault` seccomp profile, and have read-only
root filesystems. The adapter
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

These cover adapter behavior through the Go SDK test bridge. The bridge does not cache data. They do not establish live multi-node
behavior, admission compatibility, or compatibility with every S3 provider or Azure
identity setup. Validate those in your own environment.
