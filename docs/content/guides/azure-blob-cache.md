---
title: "Cache Azure Blob Storage with Racer"
weight: 10
description: "Read blobs from an Azure Blob container through the Racer cache."
---

{{< callout type="warning" >}}
Racer is pre-release. Its APIs, flags, and on-disk formats may change without a
migration path. Cached data is disposable; Racer is not durable storage. See the
[Racer reference]({{< relref "reference/racer" >}}) for upgrade notes.
{{< /callout >}}

This guide sets up a Racer cache in front of an Azure Blob container. Your pods
then read blobs from a local endpoint at `http://127.0.0.1:8080`. The first read
of a blob fetches it from Azure. Later reads come from the cache.

## Before You Start

- A cluster with the Unbounded operator. Install the `kubectl unbounded` plugin
  (see the [CLI reference]({{< relref "reference/cli" >}})), then run
  `kubectl unbounded install --timeout=5m`.
- Kubernetes 1.30 or later.
- Linux nodes that meet the Racer
  [node requirements]({{< relref "reference/racer" >}}#node-requirements).
- An Azure storage account with a container of block blobs. The example below
  reads a public container, so you can skip this to try it out.
- Permission to create ClusterCaches.
- An application namespace that allows hostPath volumes. Pod Security Admission
  at the `baseline` or `restricted` level rejects the pods in this guide.

Examples use the default operator namespace, `unbounded-system`. Use your own
installation namespace if different.

## 1. Create the Cache

This example reads the public `mnist` container from Azure Open Datasets:

```bash
kubectl apply -f - <<'EOF'
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: racer-object
spec:
  origin:
    bucket: mnist
    azure:
      accountURL: https://azureopendatastorage.blob.core.windows.net
EOF
```

| Field | Set it to |
|-------|-----------|
| `metadata.name` | Keep `racer-object`. It is the default cache name for pods. |
| `spec.origin.bucket` | The one container pods may read. You cannot change it later. |
| `spec.origin.azure.accountURL` | Your blob service URL, such as `https://ACCOUNT.blob.core.windows.net`. You cannot change it later. |

To read another container or account, create another ClusterCache.

Wait for the cache to start:

```bash
kubectl -n unbounded-system rollout status deployment/racer-controller --timeout=5m
kubectl -n unbounded-system rollout status daemonset/racer-dataplane --timeout=5m
kubectl -n unbounded-system rollout status daemonset/racer-origin-racer-object --timeout=5m
```

If a command reports that the workload does not exist, wait a few seconds and
run it again.

### Private Containers

For a private container, give an Azure identity the **Storage Blob Data Reader**
role on the account or container. Then add a `credential` to
`spec.origin.azure`. Pick one of the options below. SAS tokens and shared keys
are not supported.

**Workload identity.** Use this if the cluster runs the
[Azure Workload Identity](https://azure.github.io/azure-workload-identity/)
webhook:

```yaml
    azure:
      accountURL: https://ACCOUNT.blob.core.windows.net
      credential:
        workloadIdentity:
          clientID: 00000000-0000-0000-0000-000000000000
```

Then let the identity trust the cache's ServiceAccount, `racer-origin-racer-object`:

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

**Service principal.** Store its credentials in a Secret:

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

For all fields, see
[Managed Azure origin]({{< relref "reference/racer" >}}#managed-azure-origin).

## 2. Deploy an Application

To use the cache, add one label and one annotation to your pod:

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
    racer.unbounded-cloud.io/object-api: azure
spec:
  containers:
    - name: app
      image: curlimages/curl:8.20.0
      command: ["sleep", "infinity"]
EOF
```

| Annotation | Set it to |
|------------|-----------|
| `object-api` | `azure`, so pods use the Azure Blob API. |

The pod uses the `racer-object` cache by default. To use another cache, set
`racer.unbounded-cloud.io/object-cache` to its name.

For a Deployment, StatefulSet, or Job, put the label and annotations on the pod
template. Changes apply only to new pods, so restart existing workloads after you
add them. Do not use the `unbounded-system` namespace.

## 3. Read a Blob

Read a blob from the example container:

```bash
kubectl -n racer-object-example wait --for=condition=Ready pod/racer-object-example --timeout=5m
kubectl -n racer-object-example exec racer-object-example -c app -- sh -c \
  'curl --fail --show-error --path-as-is --max-time 300 -o /tmp/obj \
    http://127.0.0.1:8080/mnist/t10k-labels-idx1-ubyte.gz && sha256sum /tmp/obj'
```

The output starts with `f7ae60f92e00ec6debd23a6088c31dbd2371eca3ffa0defaefb259924204aec6`.
Run it again. The second read comes from the cache. If the first read returns
`503`, the cache is still starting on that node. Wait a few seconds and retry.

The URL form is `http://127.0.0.1:8080/CONTAINER/BLOB`, with no account name.

{{< callout type="warning" >}}
The local endpoint does not check credentials. Any process in the pod can read
any blob in the cache's container. Do not expose it outside the pod.
{{< /callout >}}

## Using the Azure SDK

Create an anonymous client with `http://127.0.0.1:8080` as the service URL. Do
not give it real credentials. In Go:

```go
client, err := azblob.NewClientWithNoCredential("http://127.0.0.1:8080", nil)
if err != nil {
    return err
}
response, err := client.DownloadStream(ctx, "mnist", "t10k-labels-idx1-ubyte.gz", nil)
if err != nil {
    return err
}
defer response.Body.Close()
```

Only reads of single blobs work. Listing and writes do not. See
[Client APIs]({{< relref "reference/racer-object" >}}#client-apis) for details.

## If Something Fails

**The pod is not created.** The error from `kubectl apply` gives the reason. For
a workload, run `kubectl describe` on the ReplicaSet, StatefulSet, or Job.

**A read fails.** Check the logs:

```bash
kubectl -n racer-object-example logs racer-object-example -c racer-object
kubectl -n unbounded-system logs daemonset/racer-origin-racer-object
```

Common errors:

- `403`: the container in the URL is not `spec.origin.bucket`, or the identity
  cannot read the blob.
- `404`: the blob does not exist.
- `501`: the client sent a header or option that is not supported.
- `503`: the cache is not ready on that node.

## Clean Up

```bash
kubectl delete namespace racer-object-example
kubectl delete clustercache racer-object
```

If you recreate the ClusterCache, it starts empty.

## Next Steps

- [racer-object reference]({{< relref "reference/racer-object" >}}): all
  options, including S3 support.
- [Racer reference]({{< relref "reference/racer" >}}): ClusterCache fields, cache
  sizing, and metrics.
