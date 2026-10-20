---
title: "Cache Object Storage with Racer"
weight: 10
description: "Read objects from an Azure Blob container or an S3 bucket through a pod-local Racer endpoint."
---

{{< callout type="warning" >}}
Racer is pre-release. Its APIs, flags, and on-disk formats may change without a
migration path. Cached data is disposable; Racer is not durable storage. See the
[Racer reference]({{< relref "reference/racer" >}}) for upgrade notes.
{{< /callout >}}

`racer-object` lets pods read an Azure Blob container or an S3 bucket through the
Racer cache. It has two parts:

- The **origin** runs on every Racer node and reads your store with your
  credentials. For Azure Blob, the operator runs it for you from the ClusterCache.
  For S3, you run it yourself.
- The **sidecar** runs in your application pod and serves a read-only object
  endpoint on pod loopback. A webhook adds it to pods that have a label.

The read path is application -> sidecar -> Racer -> origin -> object store. Racer
holds the cached bytes. Neither the origin nor the sidecar keeps its own cache.

You make two independent choices:

| Choice | Where you set it | Values |
|--------|------------------|--------|
| Where the objects live | ClusterCache `spec.origin`, or your own origin | Azure Blob or S3 |
| Which API your application speaks | Pod annotation `racer.unbounded-cloud.io/object-api` | `s3` (default) or `azure` |

Any combination works. For example, an application that uses an AWS SDK can read
an Azure container. In this guide, "bucket" means an S3 bucket or an Azure
container, and "key" means an S3 object key or an Azure blob name.

This page is the shortest working path. For every option, the exact request
support, and the security model, see the
[racer-object reference]({{< relref "reference/racer-object" >}}) and the
[Racer reference]({{< relref "reference/racer" >}}).

## Before You Start

- A cluster with the Unbounded operator. Install the `kubectl unbounded` plugin
  (see the [CLI reference]({{< relref "reference/cli" >}})), then run
  `kubectl unbounded install --timeout=5m`.
- Kubernetes 1.29 or later. The sidecar is a native sidecar.
- Linux nodes that meet the Racer
  [node requirements]({{< relref "reference/racer" >}}#node-requirements):
  working `io_uring`, local disk, and a privileged root dataplane with hostPath
  mounts.
- One of:
  - **Azure Blob**: a storage account and a container of block blobs. For a
    private container, an Azure identity with **Storage Blob Data Reader** on
    that account or container. The example below reads a public container and
    needs no identity.
  - **S3**: a bucket, its region, and credentials that can read the objects you
    want. Any S3-compatible store works if it meets the
    [upstream requirements]({{< relref "reference/racer-object" >}}#upstream-requirements).
- Permission to create cluster-scoped ClusterCaches.
- An application namespace that allows hostPath volumes. The sidecar mounts the
  Racer socket from the node, so Pod Security Admission at the `baseline` or
  `restricted` level rejects the pod.

Examples use the default operator namespace, `unbounded-system`. Substitute your
installation namespace if different.

## 1. Create the Cache

A ClusterCache is a named cache. The first one makes the operator install Racer.
It is not a PVC and has no capacity setting. Deleting and recreating it creates a
new, empty cache.

This guide names the cache `racer-object`, the default cache name for the
sidecar. If you pick another name, set the `object-cache` annotation in
[step 2](#2-deploy-an-application).

### Azure Blob

Set `spec.origin` and the operator runs the origin for you. This example reads
the public `mnist` container from Azure Open Datasets:

```bash
kubectl apply -f - <<'EOF'
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: racer-object
spec:
  origin:
    buckets: [mnist]
    azure:
      accountURL: https://azureopendatastorage.blob.core.windows.net
EOF
```

| Field | Set it to |
|-------|-----------|
| `buckets` | The containers the origin may read. Reads from any other container are refused. |
| `azure.accountURL` | Your blob service URL, for example `https://ACCOUNT.blob.core.windows.net`. |
| `azure.credential` | How the origin signs in. Omit it for public containers. See below. |

Wait for Racer and the origin:

```bash
kubectl -n unbounded-system rollout status deployment/racer-controller --timeout=5m
kubectl -n unbounded-system rollout status daemonset/racer-dataplane --timeout=5m
kubectl -n unbounded-system rollout status daemonset/racer-origin-racer-object --timeout=5m
```

If a workload does not exist yet, wait a few seconds for the operator and retry.
The origin DaemonSet is named `racer-origin-<cache>`. It runs on the same nodes
as `racer-dataplane`. A running origin pod does not prove upstream access; only a
real object read does.

#### Private Containers

Grant an Azure identity **Storage Blob Data Reader** on the account or container,
then add one `credential` to `spec.origin.azure`. With a credential,
`accountURL` must use `https`. SAS tokens and shared keys are not supported.

**Workload identity.** Use this when the cluster runs the
[Azure Workload Identity](https://azure.github.io/azure-workload-identity/)
webhook:

```yaml
    azure:
      accountURL: https://ACCOUNT.blob.core.windows.net
      credential:
        workloadIdentity:
          clientID: 00000000-0000-0000-0000-000000000000
```

The operator creates the ServiceAccount `racer-origin-<cache>` in
`unbounded-system` and sets the workload identity annotation and pod label. You
add a federated credential to the identity that trusts that ServiceAccount:

```bash
az identity federated-credential create \
  --name racer-origin-racer-object \
  --identity-name "$IDENTITY_NAME" \
  --resource-group "$RESOURCE_GROUP" \
  --issuer "$CLUSTER_OIDC_ISSUER" \
  --subject system:serviceaccount:unbounded-system:racer-origin-racer-object \
  --audience api://AzureADTokenExchange
```

**Managed identity on the nodes.** Attach a user-assigned identity to every
Racer node, then select it:

```yaml
      credential:
        default:
          clientID: 00000000-0000-0000-0000-000000000000
```

**Service principal.** Store its credentials in a Secret in `unbounded-system`.
Each key becomes an environment variable of the origin:

```bash
kubectl -n unbounded-system create secret generic racer-object-azure \
  --from-literal=AZURE_TENANT_ID="$AZURE_TENANT_ID" \
  --from-literal=AZURE_CLIENT_ID="$AZURE_CLIENT_ID" \
  --from-literal=AZURE_CLIENT_SECRET="$AZURE_CLIENT_SECRET"
```

```yaml
      credential:
        default:
          secretRef:
            name: racer-object-azure
```

The operator restarts the origin pods when `spec.origin` or this Secret changes.
For all fields, see
[Managed Azure origin]({{< relref "reference/racer" >}}#managed-azure-origin).

### S3

The operator does not run S3 origins. Create the cache without `spec.origin`, and
deploy the origin yourself:

```bash
kubectl apply -f - <<'EOF'
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: racer-object
EOF
kubectl -n unbounded-system rollout status deployment/racer-controller --timeout=5m
kubectl -n unbounded-system rollout status daemonset/racer-dataplane --timeout=5m
```

The origin is the only component that holds upstream credentials. It runs as a
DaemonSet because Racer can ask any Racer node for an origin read, not just the
node running your application. Keep its node selector and affinity the same as
Racer's.

The origin uses the AWS default credential chain. Prefer workload identity or
another short-lived credential provider. For a quick test with static keys,
store them in a Secret. The file may contain `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, and `AWS_SESSION_TOKEN`:

```bash
kubectl -n unbounded-system create secret generic racer-object-aws \
  --from-env-file="$AWS_ENV_FILE"
```

Unbounded releases publish the origin image as
`ghcr.io/azure/racer-object:vX.Y.Z`. Use the same
[release](https://github.com/Azure/unbounded/releases) tag as your operator's
`racer-controller` and `racer-dataplane` images. Replace `vX.Y.Z` below with that
tag. Set `--namespace`, `--bucket`, and `--region`, then apply:

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
EOF
kubectl -n unbounded-system rollout status daemonset/racer-object-origin --timeout=5m
```

| Argument | Set it to |
|----------|-----------|
| `--cache` | The ClusterCache name. |
| `--namespace` | A stable name for this store. See the warning below. |
| `--bucket` | Your bucket name. Repeat the flag for more buckets. |
| `--region` | Your bucket's region. |

For an S3-compatible store that is not AWS, also add `--endpoint` with its URL.
Prefer HTTPS. Never put credentials in manifests, flags, images, or source
control. Changing the Secret requires restarting the origin pods.

The Racer dataplane creates `/run/racer/racer-object/origin` on each node. Until
then the origin pods wait in `ContainerCreating`.

{{< callout type="warning" >}}
`--namespace` is **not** a Kubernetes namespace. It is a stable name for the
upstream store, and it is part of every cache key. Change it whenever you point
the origin at a different account, provider, or endpoint, or Racer will serve
bytes cached from the old store.
{{< /callout >}}

To run an Azure origin yourself instead of using `spec.origin`, see
[Deploying the Origin]({{< relref "reference/racer-object" >}}#deploying-the-origin).

## 2. Deploy an Application

Add the label `racer.unbounded-cloud.io/object-sidecar: enabled` to a pod and the
webhook adds the sidecar when the pod is created. Two annotations tell the
sidecar which store and buckets to read:

| Annotation | Azure Blob with `spec.origin` | S3 |
|------------|-------------------------------|----|
| `racer.unbounded-cloud.io/object-namespace` | The account URL with a lowercase scheme and host, no default port, and no trailing slash | The origin's `--namespace` |
| `racer.unbounded-cloud.io/object-buckets` | Some or all of `spec.origin.buckets`, comma-separated | Some or all of the origin's `--bucket` values |

The webhook skips pods in `unbounded-system`, so use another namespace. This pod
reads the Azure example from step 1:

```bash
kubectl create namespace racer-object-example
kubectl -n racer-object-example apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: racer-object-example
  labels:
    racer.unbounded-cloud.io/object-sidecar: enabled
  annotations:
    racer.unbounded-cloud.io/object-namespace: https://azureopendatastorage.blob.core.windows.net
    racer.unbounded-cloud.io/object-buckets: mnist
spec:
  containers:
    - name: app
      image: curlimages/curl:8.20.0
      command: ["sleep", "infinity"]
EOF
```

For the S3 example, use `example-store` and `example-bucket` instead.

Other annotations you may need:

| Annotation | Use it to |
|------------|-----------|
| `racer.unbounded-cloud.io/object-api` | Set `azure` if the application uses the Azure Blob API. |
| `racer.unbounded-cloud.io/object-cache` | Name a cache other than `racer-object`. |
| `racer.unbounded-cloud.io/object-listen` | Move the listener off `127.0.0.1:8080`. |

For a Deployment, StatefulSet, or Job, put the label and annotations on the pod
template, not the workload object. The webhook only changes new pods. Restart
existing workloads after you add or change them.

The webhook adds an init container named `racer-object` that runs for the life of
the pod, mounts the Racer socket into it only, and limits the pod to Linux nodes
that are not excluded from Racer. It does not copy any other Racer node selector,
affinity, or tolerations. If you limit where `racer-dataplane` runs, give your pod
the same limits, or it can land on a node without Racer and fail to start. Check that it ran:

```bash
kubectl -n racer-object-example get pod racer-object-example \
  -o jsonpath='{.spec.initContainers[*].name}{"\n"}'
```

The output includes `racer-object`. For resource limits and the full list of
annotations, see
[Automatic Injection]({{< relref "reference/racer-object" >}}#automatic-injection).

## 3. Read an Object

The sidecar listens on `127.0.0.1:8080`. Read a blob from the example container:

```bash
kubectl -n racer-object-example wait --for=condition=Ready pod/racer-object-example --timeout=5m
kubectl -n racer-object-example exec racer-object-example -c app -- sh -c \
  'curl --fail --show-error --path-as-is --max-time 300 -o /tmp/obj \
    http://127.0.0.1:8080/mnist/t10k-labels-idx1-ubyte.gz && sha256sum /tmp/obj'
```

The output starts with `f7ae60f92e00ec6debd23a6088c31dbd2371eca3ffa0defaefb259924204aec6`.
Run it again. The second read comes from the cache. If the first read returns
`503`, Racer is still starting on that node. Wait a few seconds and retry.

This checksum is for the public Azure example only. If you use S3 or your own
Azure container, replace `mnist` with a bucket or container in `object-buckets`
and use a key that exists. Any other bucket returns `403`.

Both APIs use the same URL form, `http://127.0.0.1:8080/BUCKET/KEY`, with no
account name. Use `curl --path-as-is` so the exact key path is preserved. Add
`--head` for metadata only, or `-H 'Range: bytes=0-1023'` for a byte range.

The application container has no credentials and no Racer socket. Only the
sidecar mounts the socket, and only the origin pods hold upstream credentials.

{{< callout type="warning" >}}
The sidecar does not authenticate callers. Any `Authorization` header is ignored,
not checked. Every process in the pod can read anything the sidecar exposes. Keep
it on loopback: never put it behind a Service or host networking.
{{< /callout >}}

## Using an SDK

Point the SDK at `http://127.0.0.1:8080`. Never give it real credentials.

**AWS SDKs** (`object-api` unset or `s3`): use path-style addressing and any
placeholder credentials. Also turn off optional checksum validation, which the
sidecar rejects with `501 NotImplemented`:

- Any SDK: set `AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED` on the application.
- Go: set `s3.Options.ResponseChecksumValidation` to
  `aws.ResponseChecksumValidationWhenRequired`.

**Azure SDKs** (`object-api: azure`): create an anonymous client with
`http://127.0.0.1:8080` as the service URL. Do not use the storage account URL or
an Azurite-style `/ACCOUNT` path.

Both APIs are a read-only subset. `GET` and `HEAD` on a single object work.
Listing and writes do not. See
[Client APIs]({{< relref "reference/racer-object" >}}#client-apis) for the full
list.

## If Something Fails

**The origin DaemonSet does not appear.** The API server rejects most invalid
`spec.origin` values when you apply them. Otherwise, the operator reports
`OriginInvalid` for a spec it cannot use, or `OriginPending` while
`racer-dataplane` does not exist yet, in the `RacerReady` condition of each Site.

**The pod is not created.** The webhook rejected it. The reason is in the error
from `kubectl apply`, or for a workload, in the ReplicaSet, StatefulSet, or Job
events (`kubectl describe`). For example:
`annotation racer.unbounded-cloud.io/object-buckets is required`.

**The pod has no sidecar.** Check that the label is on the pod, not only on the
Deployment, and that the pod is not in `unbounded-system`.

**A read fails.** Check the sidecar and origin logs:

```bash
kubectl -n racer-object-example logs racer-object-example -c racer-object
kubectl -n unbounded-system logs daemonset/racer-origin-racer-object
```

For S3, the origin is `daemonset/racer-object-origin`. Common causes:

- `403`: the bucket is not allowed by the origin, the `object-namespace`
  annotation does not match the origin, or the origin credentials cannot read the
  object.
- `404`: no such object upstream.
- `412`: the object changed during the read, or the requested ETag no longer matches.
- `501`: the client sent a header or query parameter the sidecar does not support,
  such as an SDK checksum request.
- `503`: Racer is not ready on that node.

The [error code table]({{< relref "reference/racer-object" >}}#error-codes) lists
the S3 and Azure error names for each status.

Pod readiness does not prove the cache or the upstream store is reachable. Do not
add an HTTP readiness probe for the sidecar: Kubernetes probes target the pod IP,
not pod loopback.

## Change or Remove the Cache

- Editing `spec.origin` restarts the managed origin pods. Application pods keep
  running.
- Changing `accountURL` starts a new set of cache entries. Update the
  `object-namespace` annotation on your pods to match, then restart them.
- Removing `spec.origin` removes the managed origin. Cache misses fail until you
  run an origin yourself.
- Deleting the ClusterCache removes its managed origin. Recreating it starts an
  empty cache.

Clean up the example:

```bash
kubectl delete namespace racer-object-example
kubectl delete clustercache racer-object
```

For S3, also delete `daemonset/racer-object-origin` and the `racer-object-aws`
Secret in `unbounded-system`.

## Next Steps

- [racer-object reference]({{< relref "reference/racer-object" >}}): all flags,
  annotations, request support, and the security model.
- [Racer reference]({{< relref "reference/racer" >}}): ClusterCache fields, cache
  sizing, storage, and metrics.
- [Racer concepts]({{< relref "concepts/racer" >}}): how Racer stores and shares
  pages.
