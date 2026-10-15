---
title: "Cache Object Storage with Racer"
weight: 10
description: "Read objects from an S3 bucket or an Azure Blob container through a pod-local Racer endpoint."
aliases:
  - /guides/racer-s3/
  - /guides/racer/
---

{{< callout type="warning" >}}
Racer is pre-release. Its APIs, flags, and on-disk formats may change without a
migration path. Cached data is disposable; Racer is not durable storage. See the
[Racer reference]({{< relref "reference/racer" >}}) for upgrade notes.
{{< /callout >}}

`racer-object` lets pods read an S3 bucket or an Azure Blob container through the
Racer cache. It is one binary with two commands:

- `origin` runs on every Racer node and reads your bucket or container with your
  credentials.
- `sidecar` runs next to your application and serves a read-only object endpoint
  on pod loopback.

The read path is application -> sidecar -> Racer -> origin -> object store. Racer
holds the cached bytes. Neither command keeps its own cache.

You make two independent choices:

| Choice | Flag | Values |
|--------|------|--------|
| Where the objects live | `origin --provider` | `s3` (default) or `azure` |
| Which API your application speaks | `sidecar --api` | `s3` (default) or `azure` |

Any combination works. For example, an application that uses an AWS SDK can read
an Azure container through `--provider=azure` and the default `--api=s3`. In this
guide, "bucket" means an S3 bucket or an Azure container, and "key" means an S3
object key or an Azure blob name.

This page is the shortest working path. For every flag, the exact request support,
the security model, and how to add the origin and sidecar to your own workloads,
see the [racer-object reference]({{< relref "reference/racer-object" >}}).

## Before You Start

- A cluster with the Unbounded operator. Install the `kubectl unbounded` plugin
  (see the [CLI reference]({{< relref "reference/cli" >}})), then run
  `kubectl unbounded install --timeout=5m`.
- Linux nodes that meet the Racer
  [node requirements]({{< relref "reference/racer" >}}#node-requirements):
  working `io_uring`, local disk, and a privileged root dataplane with hostPath
  mounts.
- One of:
  - **S3**: a bucket, its region, and credentials that can read the objects you
    want. Any S3-compatible store works if it meets the
    [upstream requirements]({{< relref "reference/racer-object" >}}#upstream-requirements).
  - **Azure Blob**: a storage account, a container of block blobs, and an Azure
    identity with **Storage Blob Data Reader** on that account or container.
- Permission to create cluster-scoped ClusterCaches, and to run pods with
  hostPath mounts and UID 0 containers. The adapter does not need privileged mode.

Examples use the default operator namespace, `unbounded-system`. Substitute your
installation namespace if different.

### Image

Unbounded releases publish the image as `ghcr.io/azure/racer-object:vX.Y.Z`.
Find the tag on the
[Unbounded releases page](https://github.com/Azure/unbounded/releases). Use the
same release tag as your operator's `racer-controller` and `racer-dataplane`
images. Replace `vX.Y.Z` in the YAML below with that tag.

## 1. Create the Cache

```bash
kubectl apply -f - <<'EOF'
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: racer-object
EOF
kubectl get ccache racer-object
```

The first ClusterCache makes the operator install Racer. ClusterCache has only
metadata: no `spec` or `status`, no origin, and no capacity. It is not a PVC.
Deleting and recreating it creates a new, empty cache. Wait for the workloads:

```bash
kubectl -n unbounded-system rollout status deployment/racer-controller --timeout=5m
kubectl -n unbounded-system rollout status daemonset/racer-dataplane --timeout=5m
```

If the controller or DaemonSet does not exist yet, wait a few seconds for the
operator and retry.

Racer runs on every Linux node without the `racer.unbounded-cloud.io/exclude`
label. The label's presence excludes a node, whatever its value. To change
placement weight, cache size, or use dedicated raw disks, see
[node labels]({{< relref "reference/racer" >}}#node-labels-and-annotations),
[dataplane configuration]({{< relref "reference/racer" >}}#dataplane-configuration),
and [raw-device storage]({{< relref "reference/racer" >}}#raw-device-storage).
Raw-device mode erases the selected disks.

## 2. Deploy the Origin

The origin is the only component that holds upstream credentials. It runs as a
DaemonSet because Racer can ask any Racer node for an origin read, not just the
node running your application. Keep its node selector and affinity the same as
Racer's.

### S3

The origin uses the AWS default credential chain. Prefer workload identity or
another short-lived credential provider. For a quick test with static keys,
store them in a Secret. The file may contain `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, and `AWS_SESSION_TOKEN`:

```bash
kubectl -n unbounded-system create secret generic racer-object-aws \
  --from-env-file="$AWS_ENV_FILE"
```

Set `--namespace`, `--bucket`, and `--region`, then apply:

```bash
kubectl -n unbounded-system apply -f - <<'EOF'
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: racer-object-origin
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: racer-object-origin
  template:
    metadata:
      labels:
        app.kubernetes.io/name: racer-object-origin
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
            type: DirectoryOrCreate
EOF
```

| Argument | Set it to |
|----------|-----------|
| `--cache` | The ClusterCache name. |
| `--namespace` | A stable name for this store. See the warning below. |
| `--bucket` | Your bucket name. Repeat the flag for more buckets. |
| `--region` | Your bucket's region. |

For an S3-compatible store that is not AWS, also add `--endpoint` with its URL.
Prefer HTTPS. Clients never choose the endpoint.

### Azure Blob

The origin uses `DefaultAzureCredential`, which covers environment credentials,
workload identity, and managed identity. Attach an identity to the origin pods the
way your cluster requires, and grant it **Storage Blob Data Reader**. The YAML
below does not attach one for you.

Use the S3 DaemonSet above with these changes:

- Replace the `args` with:

  ```yaml
  args:
    - origin
    - --cache=racer-object
    - --provider=azure
    - --namespace=example-azure-store
    - --bucket=example-container
    - --account-url=https://ACCOUNT.blob.core.windows.net
  ```

- Replace `envFrom` with this `env` entry if you need to select a user-assigned
  identity:

  ```yaml
  env:
    - name: AZURE_CLIENT_ID
      valueFrom:
        secretKeyRef:
          name: racer-object-azure
          key: AZURE_CLIENT_ID
          optional: true
  ```

  Create the Secret with:

  ```bash
  kubectl -n unbounded-system create secret generic racer-object-azure \
    --from-literal=AZURE_CLIENT_ID="$AZURE_CLIENT_ID"
  ```

A client ID only selects an identity. It does not attach one or grant access.
`--account-url` must use HTTPS. For blobs that allow anonymous public reads, add
`--azure-credential=anonymous`. SAS tokens and shared keys are not supported.

### Wait for the Origin

```bash
kubectl -n unbounded-system rollout status daemonset/racer-object-origin --timeout=5m
```

A running origin pod does not prove upstream access; only a real object read
does.

Never put credentials in manifests, flags, images, or source control. Changing a
credentials Secret requires restarting the origin pods.

{{< callout type="warning" >}}
`--namespace` is **not** a Kubernetes namespace. It is a stable identifier for
the upstream store, and it is part of every cache key. Use the same value on the
origin and the sidecar. Change it whenever you point the adapter at a different
account, provider, or endpoint, or Racer will serve bytes cached from the old
store.
{{< /callout >}}

`--bucket` is an allowlist. If you omit it, every bucket or container is allowed.
It limits what the adapter will fetch; it is not a per-user access policy.

## 3. Deploy the Sidecar and Read an Object

This example pod has a `curl` container as the application and a `racer-object`
sidecar. Set `--namespace` and `--bucket` to the same values as the origin. For
an application that uses the Azure Blob API, add `--api=azure`.

```bash
kubectl -n unbounded-system apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: racer-object-example
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
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: app
      image: curlimages/curl:8.20.0
      command: ["sleep", "infinity"]
      securityContext:
        runAsNonRoot: true
        runAsUser: 100
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ALL]
    - name: sidecar
      image: ghcr.io/azure/racer-object:vX.Y.Z
      args:
        - sidecar
        - --cache=racer-object
        - --namespace=example-store
        - --bucket=example-bucket
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
EOF
```

The sidecar listens on `127.0.0.1:8080` by default. Read an object:

```bash
kubectl -n unbounded-system wait --for=condition=Ready pod/racer-object-example --timeout=5m
kubectl -n unbounded-system exec racer-object-example -c app -- \
  curl --fail --show-error --path-as-is --max-time 300 \
  http://127.0.0.1:8080/example-bucket/example-key
```

Replace `example-bucket` and `example-key` with a real bucket or container and a
real key or blob name. Both APIs use the same URL form,
`http://127.0.0.1:8080/BUCKET/KEY`, with no account name. Use `curl --path-as-is`
so the exact key path is preserved. Add `--head` for metadata only, or
`-H 'Range: bytes=0-1023'` for a byte range.

The example application container has no credentials and no Racer socket. Only
the sidecar container mounts the socket, and only the origin pods hold upstream
credentials. To add the sidecar to your own workload, see
[Deploying the Sidecar]({{< relref "reference/racer-object" >}}#deploying-the-sidecar).

{{< callout type="warning" >}}
The sidecar does not authenticate callers. Any `Authorization` header is ignored,
not checked. Every process in the pod can read anything the sidecar exposes. Keep
it on loopback: never put it behind a Service or host networking.
{{< /callout >}}

## Using an SDK

Point the SDK at `http://127.0.0.1:8080`. Never give it real credentials.

**AWS SDKs** (`--api=s3`): use path-style addressing and any placeholder
credentials. Also turn off optional checksum validation, which the sidecar
rejects with `501 NotImplemented`:

- Any SDK: set `AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED` on the application.
- Go: set `s3.Options.ResponseChecksumValidation` to
  `aws.ResponseChecksumValidationWhenRequired`.

**Azure SDKs** (`--api=azure`): create an anonymous client with
`http://127.0.0.1:8080` as the service URL. Do not use the storage account URL or
an Azurite-style `/ACCOUNT` path.

Both APIs are a read-only subset. `GET` and `HEAD` on a single object work.
Listing and writes do not. See
[Client APIs]({{< relref "reference/racer-object" >}}#client-apis) for the full
list.

## If a Read Fails

Start with the origin logs:

```bash
kubectl -n unbounded-system logs daemonset/racer-object-origin
```

Common causes:

- `403`: the bucket or container is not in `--bucket`, `--namespace` differs
  between the origin and the sidecar, or the origin credentials cannot read the
  object.
- `404`: no such object upstream.
- `412`: the object changed during the read, or the requested ETag no longer matches.
- `501`: the client sent a header or query parameter the sidecar does not support,
  such as an SDK checksum request.
- `503`: Racer is not ready on that node. Application pods must run on nodes
  running Racer.

The [error code table]({{< relref "reference/racer-object" >}}#error-codes) lists
the S3 and Azure error names for each status.

Pod readiness does not prove the cache or the upstream store is reachable. The
examples have no HTTP readiness probe because Kubernetes probes target the pod IP,
not pod loopback.

## Next Steps

- [racer-object reference]({{< relref "reference/racer-object" >}}): all flags,
  deployment requirements, request support, and the security model.
- [Racer reference]({{< relref "reference/racer" >}}): cache sizing, storage, and
  metrics.
