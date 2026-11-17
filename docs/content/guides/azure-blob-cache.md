---
title: "Cache Azure Blob Storage with Racer"
weight: 10
description: "Serve objects from a public or private Azure Blob Storage container to pods at local read latency with a Racer ClusterCache and the racer-object sidecar."
---

Racer turns a cluster into a read-through cache for object storage. You point a
`ClusterCache` at one Azure Blob Storage container, and the Racer dataplane that
runs on every eligible node fetches objects on demand and keeps them close to
the pods that read them. Applications do not need an Azure SDK change, a
credential, or a network path to the storage account. They read from a loopback
endpoint in their own pod, and the sidecar forwards the request to the cache.

This guide walks through the whole path: preparing the cluster, creating a
cache over a public container, switching to a private container with a managed
identity, opting an application pod into the injected sidecar, verifying reads,
and diagnosing the failures you are most likely to see.

The mental model is worth keeping in mind. A `ClusterCache` is a name and, at
creation, an origin. Its Kubernetes UID is the cache identity, so deleting and
recreating a cache starts empty even if it points at the same container. The
cache is not a volume and not durable storage. It is a best-effort copy of
origin data, and the origin remains the source of truth.

## Before You Start

{{< callout type="warning" >}}
Racer and the `racer-object` sidecar are pre-release. Resource fields, flags, and
on-disk formats can change between releases. Treat cached data as disposable and
only cache objects that you can fetch again from the container.
{{< /callout >}}

### Cluster and Client Requirements

You need a working Unbounded installation and a way to reach the API server:

- Kubernetes 1.30 or later. The injected sidecar is a native sidecar, an init
  container with `restartPolicy: Always`, which requires Kubernetes 1.29 or
  later.
- A kubeconfig with permission to install cluster components and create
  cluster-scoped resources.
- The `kubectl unbounded` plugin. Install it from the release artifacts:

  ```bash
  curl -sSfL https://get.unbounded.cloud/kubectl-unbounded-linux-amd64.tar.gz | tar -xzC /usr/local/bin
  kubectl unbounded --help
  ```

  Use the `kubectl-unbounded-mac-arm64.tar.gz` artifact on macOS Apple Silicon.
  The platform table in [Install on an Existing Cluster]({{< relref "guides/existing-cluster" >}})
  lists both.

- The Unbounded operator, installed into the cluster. The default install
  timeout is already five minutes, so the explicit flag documents intent rather
  than changing behavior:

  ```bash
  kubectl unbounded install --timeout=5m
  ```

  `install` waits for the operator rollout and for the required CRDs to become
  established. Racer itself is not installed here: the operator installs it on
  demand once a `ClusterCache` exists.

- Permission to create `clustercaches` in the `racer.unbounded-cloud.io` API
  group. The resource is cluster-scoped, so the grant must be a `ClusterRole`
  bound with a `ClusterRoleBinding`, not a namespaced `Role`.

- A namespace for your application pods. This namespace must allow `hostPath`
  volumes, because the webhook mounts the cache socket from the node. Pod
  Security Admission `baseline` and `restricted` both forbid `hostPath`
  volumes, so use a dedicated namespace labeled
  `pod-security.kubernetes.io/enforce: privileged` or a namespace with no Pod
  Security Admission enforcement.

{{< callout type="important" >}}
The sidecar is not injected into the Racer system namespace, which defaults to
`unbounded-system`. Put workloads in their own namespace. The exclusion is part
of the admission configuration so the dataplane can always start. That namespace
also has to allow privileged pods, because the dataplane mounts host devices.
{{< /callout >}}

### Node Requirements

Racer runs as a DaemonSet named `racer` in the system namespace. Every node that
should store or serve cache data needs to satisfy the following:

- Linux. The pod template requires `kubernetes.io/os=linux`.
- Access to host devices. The dataplane mounts `/dev` and runs privileged,
  because its storage and RDMA paths use host device access.
- Local disk for the page cache. Slabs live under `/var/lib/racer/slabs` by
  default, mounted from the host, and the seeded budget is 4 GiB per I/O worker.
- The label `racer.unbounded-cloud.io/exclude` must be absent. Nodes carrying
  that label are removed from dataplane placement and from cache membership.

The dataplane DaemonSet itself is created only after the first `ClusterCache`
exists. On a cluster that already runs Gantry, Racer may also be used as the
Gantry backend, but that path is configured separately and is not required for
this guide.

### Azure Requirements

On the Azure side you need:

- A storage account and a block blob container. The container name must be 3 to
  63 characters of lowercase letters, digits, and single hyphens, which is the
  same rule the `containerURL` field enforces.
- The container URL, for example
  `https://myaccount.blob.core.windows.net/models`. The last path segment is the
  container name.
- For a private container, an Azure identity that can read blobs in that
  container. `Storage Blob Data Reader` at container scope is enough; the cache
  never writes to the origin.
- Network reachability from every Racer node to the storage account, including
  any firewall or private endpoint rules on the account.

## 1. Create the Cache

The smallest useful cache points at a public container. The public MNIST
dataset lives in the `mnist` container of the `azureopendatastorage` account, and
the Racer e2e suite uses it as an anonymous origin that needs no credential at
all.

Save the following manifest as `mnist-cache.yaml`:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: mnist
spec:
  origin:
    azure:
      containerURL: https://azureopendatastorage.blob.core.windows.net/mnist
```

Apply it and inspect the object:

```bash
kubectl apply -f mnist-cache.yaml
kubectl get ccache mnist
```

`ccache` is the short name of the `clustercaches` resource. The object has no
status field, so a successful `get` tells you only that the API server stored it.
The work happens in the operator and the dataplane.

### What the Fields Mean

`metadata.name` is not just a label. The cache name is part of the socket paths
the dataplane creates, `/run/racer/<name>/client/socket` and
`/run/racer/<name>/origin/socket`, so it is limited to 82 characters overall and
63 characters per label. Longer names would not fit a Unix socket address.
Names must be lowercase DNS subdomains, and the webhook enforces the same rule
when a pod references a cache.

`spec.origin.azure.containerURL` identifies the account and one container. It
must be an `http` or `https` URL, and it must not contain credentials, a query
string, or a fragment. The path must end in a valid container name. Azurite-style
URLs may carry one extra path segment for the account, for example
`http://127.0.0.1:10000/devstoreaccount1/models`, which is how local testing
against an emulator works.

The origin is immutable in two ways. `containerURL` cannot be changed, and
whether `spec.origin` is set at all cannot change after creation. Cache entries
are keyed by the `ClusterCache` UID, and the dataplane does not record which
account or container a cached page came from. Allowing the source to change
would serve bytes fetched elsewhere. The credential is not part of the identity,
so you can change it in place. If you need a different container, create a new
`ClusterCache`.

### Optional Origin and Cache Settings

You can tune the origin and the cache without changing the container:

| Field | Default | Meaning |
| --- | --- | --- |
| `spec.origin.metadataTTL` | `30s` | How long object metadata (ETag, size, expiry) is reused before revalidation. Must not be negative. |
| `spec.origin.requestTimeout` | `1m` | Bound on each upstream request. Must be positive. |
| `spec.origin.maxConcurrentRequests` | `512` | Concurrent upstream requests per dataplane node for this cache. Allowed range 1 to 65536. |
| `spec.priority` | `500` | Relative share of node cache capacity. Allowed range 0 to 1000, changeable at any time. |
| `spec.passthrough` | unset | Proxies reads without caching. `route` is `Peers` (default) or `Local`. |
| `spec.nodeSelector` | unset | Restricts which Racer nodes store, serve, and read the cache. Client pods are pinned to matching nodes. |

`spec.priority` matters when several caches share a node. Disk shares are
weighted across published caches, plus caches that still hold data on the node,
and memory shares are weighted across caches that are actively using memory. A
priority of 0 gets no weighted disk share while positive priorities compete, but
it keeps a working allowance. If every counted cache is 0, they share equally.

`spec.passthrough` is for data you read once and never reuse. It turns off
caching, so every read revalidates with the origin. Prefetch requests are
rejected with `409 passthrough`, and switching a cache to passthrough drops the
data previously cached for it. Since passthrough still weights in-flight memory
use, it does not make a cache free.

### Wait for the Dataplane

Creating the cache causes the operator to install the Racer dataplane. Wait for
the DaemonSet rollout:

```bash
kubectl -n unbounded-system rollout status daemonset/racer --timeout=10m
kubectl -n unbounded-system get pods -l app.kubernetes.io/name=racer -o wide
```

If your cluster has at least one `Site`, the operator also publishes a
`RacerReady` condition on each `Site`:

```bash
kubectl get sites -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .status.conditions[?(@.type=="RacerReady")]}{.status}{"\t"}{.reason}{"\n"}{end}{end}'
```

Without a `Site` the components still reconcile, but there is nowhere to publish
conditions, so the DaemonSet and pod state are the authoritative checks.

### Confirm the Control Seats

Every dataplane pod runs the control plane, and a small number of seats are
elected through Kubernetes Leases. The operator creates the Leases
`racer-primary` and `racer-seat-0` through `racer-seat-15` in the system
namespace, and marks them with `racer.unbounded-cloud.io/control: "true"`. The
number of active seats comes from `RACER_CONTROL_SEATS` in the `racer-settings`
ConfigMap: `auto` selects 1 to 16 based on cluster size, and a fixed value must
be between 1 and 16.

Check that a seat holds the primary Lease:

```bash
kubectl -n unbounded-system get lease racer-primary \
  -o jsonpath='{.spec.holderIdentity}{"\n"}'
kubectl -n unbounded-system get leases -l racer.unbounded-cloud.io/control=true
```

The holder identity is the pod name. A seat also records its nonce in an
annotation, so a restarted process on the same pod is distinguishable from its
predecessor. If `racer-primary` has no holder, the webhook has no backend, and
opted-in pod creation fails closed until a seat is elected.

### Private Containers

Anonymous access is the exception. Most real containers require an identity, and
the field you use depends on where that identity lives. The dataplane reads the
origin itself on each node, so the credential is a property of the cache, not of
the application pod. Application pods never see it.

First grant read access. The assignment can be scoped to the container, which is
the least privilege option:

```bash
ACCOUNT=myaccount
CONTAINER=models
IDENTITY_CLIENT_ID=00000000-0000-0000-0000-000000000000

az role assignment create \
  --assignee "$IDENTITY_CLIENT_ID" \
  --role "Storage Blob Data Reader" \
  --scope "/subscriptions/<subscription-id>/resourceGroups/<resource-group>/providers/Microsoft.Storage/storageAccounts/$ACCOUNT/blobServices/default/containers/$CONTAINER"
```

Replace the subscription, resource group, and client ID placeholders with your
values. `Storage Blob Data Reader` allows reads and metadata reads but no writes,
which matches what the cache does.

#### Node Managed Identity

If the cluster nodes already have a managed identity with access to the account,
select the default credential. A system-assigned identity needs no fields:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
spec:
  origin:
    azure:
      containerURL: https://myaccount.blob.core.windows.net/models
      credential:
        default: {}
```

When several user-assigned identities are attached to a node, name the one to
use. The dataplane reaches the Azure Instance Metadata Service at
`169.254.169.254:80`, so nodes must be able to reach IMDS, which is normal on
Azure but not on clusters running outside Azure:

```yaml
spec:
  origin:
    azure:
      containerURL: https://myaccount.blob.core.windows.net/models
      credential:
        default:
          clientID: 00000000-0000-0000-0000-000000000000
```

`credential.default` uses the node identity through IMDS. It does not read
Kubernetes Secrets. The older `secretRef` field still exists in the schema only
so that stored caches which set it fail closed instead of being silently
rewritten into a managed identity; a cache that sets it is rejected with
`secretRef is no longer supported; use clientID or workloadIdentity`.

#### Microsoft Entra Workload ID

Workload identity avoids depending on the node identity. The dataplane
ServiceAccount is named `racer` in the system namespace, and its token is
projected into every dataplane pod at
`/var/run/secrets/azure/tokens/azure-identity-token` with the audience
`api://AzureADTokenExchange`. The operator always projects that volume, so
enabling workload identity for a cache does not roll the dataplane.

Create the federated credential on a user-assigned identity. The subject must
name the Racer ServiceAccount in the namespace where the operator is installed:

```bash
az identity federated-credential create \
  --name racer-blob \
  --identity-name racer-blob-identity \
  --resource-group <resource-group> \
  --issuer "$(az aks show -n <cluster> -g <resource-group> --query oidcIssuerProfile.issuerUrl -o tsv)" \
  --subject "system:serviceaccount:unbounded-system:racer" \
  --audiences "api://AzureADTokenExchange"
```

Then reference the identity in the cache:

```yaml
spec:
  origin:
    azure:
      containerURL: https://myaccount.blob.core.windows.net/models
      credential:
        workloadIdentity:
          clientID: 00000000-0000-0000-0000-000000000000
          tenantID: 00000000-0000-0000-0000-000000000000
```

`clientID` is required. `tenantID` is optional: when it is omitted, the
dataplane learns the tenant from the storage account's `WWW-Authenticate` bearer
challenge and uses that tenant for the token exchange. Setting it explicitly
avoids one extra round trip on the first read and is useful when the account
returns an unexpected tenant.

Exactly one of `default` or `workloadIdentity` must be set. Setting both, or
setting `credential` with neither, is rejected. The container URL must use
`https` whenever a credential is configured, because a bearer token is sent with
each request.

### Upgrading From Per-Cache Origins

Earlier operator versions ran one origin DaemonSet and ServiceAccount per cache,
named `racer-origin-<cache>`. Current versions read the origin in-process in the
dataplane, so those objects are only pruned. The operator waits until every
dataplane pod runs the native-origin template before deleting them, because
nodes still on the old dataplane read through their origin sockets. If you see
`racer-origin-*` objects after an upgrade, the rollout is still in progress; no
manual cleanup is needed. A cache whose origin is invalid is reported in the
`RacerReady` condition instead of being silently served.

## 2. Deploy an Application

An application opts in with one label and two annotations on its pod template.
The label is what selects the pod for mutation; the annotations tell the webhook
which cache to use and which client API to serve.

The following pod reads a public MNIST file into its own container and prints
its checksum. Save it as `fetch-blob.yaml`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: fetch-blob
  labels:
    app: fetch-blob
    racer.unbounded-cloud.io/object-sidecar: enabled
  annotations:
    racer.unbounded-cloud.io/object-cache: mnist
    racer.unbounded-cloud.io/object-api: azure
spec:
  restartPolicy: Never
  containers:
    - name: app
      image: curlimages/curl:8.11.1
      command:
        - sh
        - -c
        - |
          set -eu
          url=http://127.0.0.1:8080/mnist/t10k-labels-idx1-ubyte.gz
          time curl -fsS -o /tmp/t10k.gz "$url"
          echo "f7ae60f92e00ec6debd23a6088c31dbd2371eca3ffa0defaefb259924204aec6  /tmp/t10k.gz" | sha256sum -c -
          time curl -fsS -o /dev/null "$url"
```

Apply it:

```bash
kubectl create namespace blob-demo
kubectl -n blob-demo apply -f fetch-blob.yaml
```

### Label and Annotation Reference

| Key | Kind | Required | Value |
| --- | --- | --- | --- |
| `racer.unbounded-cloud.io/object-sidecar` | label | yes | `enabled` to inject the sidecar |
| `racer.unbounded-cloud.io/object-cache` | annotation | yes | Name of an existing `ClusterCache` |
| `racer.unbounded-cloud.io/object-api` | annotation | no | `s3` (default) or `azure` |
| `racer.unbounded-cloud.io/object-listen` | annotation | no | Loopback IP and port, default `127.0.0.1:8080` |
| `racer.unbounded-cloud.io/object-cpu-request` | annotation | no | CPU request for the sidecar |
| `racer.unbounded-cloud.io/object-cpu-limit` | annotation | no | CPU limit for the sidecar |
| `racer.unbounded-cloud.io/object-memory-request` | annotation | no | Memory request for the sidecar |
| `racer.unbounded-cloud.io/object-memory-limit` | annotation | no | Memory limit for the sidecar |
| `racer.unbounded-cloud.io/object-sidecar-injected` | annotation | output | Set to the sidecar image after injection |

The cache annotation has no default. A pod that opts in without naming a cache is
rejected rather than attached to an unrelated cache. The API annotation is
validated against `s3` and `azure`; any other value fails admission.

The `object-listen` value must be a loopback IP literal and a port from 1 to
65535. Do not bind a routable address. The sidecar has no authentication and no
TLS, so a listener on the pod IP or on all interfaces would expose the cache
keyspace to anything that can reach the pod.

### What Injection Adds

When the pod is created, the Racer webhook looks up the named cache and applies a
JSON patch. It adds an init container named `racer-object` at the front of the
init container list, a `hostPath` volume named `racer-object-client`, and an
annotation recording the injected image. The container runs as UID and GID
65532, drops all capabilities, uses the `RuntimeDefault` seccomp profile, and
mounts the cache's client directory read-only at
`/run/racer/<cache>/client`.

The init container uses `restartPolicy: Always`, so it is a native sidecar: it
starts before the application containers and keeps running for the life of the
pod. It has no readiness probe. Pod readiness therefore reflects the application
containers, not the cache, which is why the first read may still need a retry.

The webhook also pins the pod to viable nodes. It adds
`kubernetes.io/os=linux` to the node selector and adds the cache's
`spec.nodeSelector`, when set, to the pod's required node affinity. It always
adds a requirement that the node does not carry
`racer.unbounded-cloud.io/exclude`, so a client pod never lands on a node that
cannot serve the cache.

### Placement and Existing Workloads

Put the label and annotations under the pod template, not on the Deployment,
StatefulSet, or Job metadata. Admission sees pods, not controllers, so a label on
the controller has no effect.

After you change the template, existing pods are not mutated retroactively.
Deployments replace their pods on a template change. StatefulSets replace pods as
they are deleted or rolled, and a Job only picks up the new template on a new
Job. For a bare pod, delete and recreate it.

If the webhook cannot reach a seat, it fails closed. `failurePolicy: Fail` means
a pod that opts in is denied when the webhook is unavailable, rather than
admitted without its cache mount. During a Racer outage, new opted-in pods do not
start; already running pods keep working through their existing socket.

### Opting In Without the Object API

If your application already speaks the Racer client protocol, skip the sidecar
and mount the socket directly. Label the pod
`racer.unbounded-cloud.io/client-socket: enabled` and set
`racer.unbounded-cloud.io/client-cache` to the cache name. The webhook mounts
`/run/racer/<cache>/client` read-only into each init and application container
and sets `RACER_SOCKET` to the socket path. Use
`racer.unbounded-cloud.io/client-containers` with a comma-separated list when
only some containers should receive the mount.

The [racer-object reference]({{< relref "reference/racer-object" >}}) covers the
manual sidecar variant, the socket protocol, and the full request rules.

## 3. Read a Blob

Wait for the pod to finish, then read the logs:

```bash
kubectl -n blob-demo wait --for=jsonpath='{.status.phase}'=Succeeded pod/fetch-blob --timeout=5m
kubectl -n blob-demo logs fetch-blob -c app
```

A successful run ends with `/tmp/t10k.gz: OK` from `sha256sum -c`. That checks
the bytes against the published digest, which is the right verification for a
cache: the cache promises to return the origin's bytes, and the digest is how you
prove it did.

The pod finishing tells you the reads worked, not that the cache was warm when
the first one started. The sidecar has no readiness probe, so a pod can be
running before the dataplane on its node has published the cache socket. The
first read then fails with a `503`, and a bounded retry succeeds once the socket
exists.

### First Read and Cached Read

An unconditional `GET` goes straight to the cache with the client's range. On a
cold cache, the dataplane asks an owner node to fetch page-aligned ranges from
the origin, encrypts and stores them, and streams them back. Racer pages are
16 MiB, so a small object such as this 1.6 MB MNIST file still causes a 16 MiB
page fetch on its first read.

A second read of the same object is served from memory, from local disk, or from
a peer that already holds the page. Nothing is revalidated with the origin on a
cache hit. The example pod reads the same blob twice and prints `time` for each
read, so the two `real` values in the log show the cold and warm paths on the
same node:

```text
real    <cold read time>
/tmp/t10k.gz: OK
real    <warm read time>
```

Your numbers will differ with the node, the cache state, and how many peers hold
the page. A warm read is bounded by the loopback copy, not by the storage
account.

### Retrying a Cold Read

The e2e suite uses a bounded retry loop instead of sleeping, and it is a good
pattern for init containers and startup scripts:

```bash
n=0
until curl -fsS -o /tmp/t10k.gz http://127.0.0.1:8080/mnist/t10k-labels-idx1-ubyte.gz; do
  n=$((n + 1))
  [ "$n" -lt 60 ] || exit 1
  sleep 2
done
```

The sidecar returns `503` when it cannot reach the Racer socket, which is the
normal failure while the dataplane or the cache's client socket is still coming
up. Retrying with a cap distinguishes that transient state from a real error
such as `403` or `404`.

### URL Shape

The read path is `http://127.0.0.1:8080/<CONTAINER>/<BLOB>`. The container
segment must match the cache's `containerURL`, because the cache serves exactly
one container. A request naming another container is rejected by the origin with
`403`, even if the sidecar would happily forward the path.

```bash
# Correct: the cache's container, then the blob name, slashes preserved.
curl -fsS http://127.0.0.1:8080/mnist/t10k-labels-idx1-ubyte.gz -o /tmp/t10k.gz

# Wrong container: 403 from the origin.
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/other/t10k-labels-idx1-ubyte.gz
```

Blob names keep their slashes. For a container named `models`, the blob
`llama/config.json` is requested at `/models/llama/config.json`. Do not collapse
repeated slashes or resolve `..` segments on the client; the sidecar preserves the
path as sent.

### The Local Endpoint Has No Credentials

The sidecar holds no upstream credentials and performs no per-client
authorization. Any process in the pod that can reach `127.0.0.1:8080` can read
any blob in the cache's container. That is deliberate: the pod is the trust
boundary, and the socket mount is what grants access. It also means the listener
must stay on loopback.

The same property applies to the mounted socket. A container that can connect to
`/run/racer/<cache>/client/socket` can read the whole cache keyspace. Do not
mount that directory into containers you do not trust, and do not share a
namespace across trust domains when the pods can reach each other's loopback
interfaces.

## Using the Azure SDK

Because the sidecar serves the Azure Blob client API, existing Azure SDK code
works after one change: point the client at the loopback endpoint and remove the
credential. The service URL is the sidecar address, and the container and blob
names are passed to the operation as usual.

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

func main() {
	ctx := context.Background()

	client, err := azblob.NewClientWithNoCredential("http://127.0.0.1:8080", nil)
	if err != nil {
		log.Fatal(err)
	}

	response, err := client.DownloadStream(ctx, "mnist", "t10k-labels-idx1-ubyte.gz", nil)
	if err != nil {
		log.Fatal(err)
	}
	defer response.Body.Close()

	data, err := io.ReadAll(response.Body)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("read %d bytes\n", len(data))
}
```

Use `NewClientWithNoCredential`, not a shared key or token credential. The
sidecar ignores `Authorization` headers, so sending one adds bytes and no
security. It also rejects presigned URLs and SAS query parameters.

`DownloadStream` is the supported read path for a single blob. It returns a
streaming body, and the sidecar's large-body path moves bytes from the Racer
socket to the client socket without copying them through user space. Byte ranges
and the `If-Match` and `If-None-Match` conditions are supported. An
`If-None-Match` that matches returns `304`, and an `If-Match` that does not match
returns `412`.

Listing, uploading, deleting, copying, and metadata mutation are not supported.
The cache is read-only and serves single objects, so `NewListBlobsFlatPager`,
`UploadStream`, and similar calls fail with `501`. Options that change how bytes
are validated, such as `RangeGetContentMD5`, are also unsupported and return
`501` rather than silently ignoring the request.

## Performance and Capacity

The object cache is fast when the workload reads the same pages repeatedly and
slow when every read is unique. Size and tune with that in mind.

### Page Granularity

The dataplane caches 16 MiB pages and fetches whole pages from the origin.
Reading a 100 KB object pulls and stores a 16 MiB page, so a workload built from
many tiny objects wastes most of each page and pays one origin fill per page. A
few large objects, or archives read as one stream, use the page budget far
better.

Reads are page-aligned and the last page of an object may be shorter, so a byte
range near the end of a large blob still fetches that page from its owner. Range
reads do not reduce the amount cached.

### Node Capacity

Each dataplane worker owns a slab budget. The `racer-settings` ConfigMap seeds
`RACER_SLAB_BYTES_PER_WORKER` at 4 GiB, `RACER_SEGMENT_BYTES` at 1 GiB, and
`RACER_SHARES` at 4. The number of I/O workers follows the node's eligible
physical cores unless you cap it, so a large node also gets a large total cache
budget. To bound memory and disk, cap threads with `RACER_MAX_THREADS`, set an
explicit `RACER_IO_WORKERS` and `RACER_CRYPTO_HELPERS` pair, or set node-wide
byte budgets such as `RACER_PLAINTEXT_BYTES`.

The `racer-settings` ConfigMap is administrator payload. The operator creates it
once, preserves your edits, and rolls the DaemonSet when its hash changes. Any
`RACER_*` key the dataplane does not recognize fails the operator plan, so a typo
surfaces as a reconcile error instead of a crash loop.

When several caches share a node, `spec.priority` decides who keeps data. Set a
higher priority for the dataset that must stay warm and a lower one, or 0, for a
cache that is read occasionally. Priority is advisory: a cache that exceeds its
share is evicted first, within bounded scans, but data is never guaranteed to
stay.

`spec.nodeSelector` restricts a cache to a pool of nodes. Changing it moves the
cache without migration, so pages are refilled from the origin. That is useful
for keeping a large dataset on storage-dense nodes, but expect a cold period
after the change. The admission webhook also restricts client pods to the
matching nodes, so a client pod cannot land on a node that cannot serve it.

### Origin Concurrency and Freshness

Each dataplane node keeps at most `spec.origin.maxConcurrentRequests` upstream
requests in flight for the cache, 512 by default. Raise it when many distinct
pages are read at once and the storage account can absorb the load; lower it when
the account throttles. `spec.origin.requestTimeout` bounds each upstream request
at one minute by default and surfaces a timeout as a read error rather than
hanging the client.

Object metadata is reused for `spec.origin.metadataTTL`, 30 seconds by default.
Lower it when the container is overwritten frequently and clients must see new
content promptly, at the cost of more `HEAD` requests. Raise it when objects are
immutable, which is the common case for models and datasets.

The sidecar itself has its own limits. `--workers` defaults to the usable CPU
count capped at 4, `--max-connections` defaults to 128, and the request timeout
defaults to 5 minutes. Those defaults are fine for most pods. Annotation-based
resource requests and limits are the right lever for multi-tenant clusters, since
the injected container has no resources of its own.

### Measuring Hit Rates

The dataplane serves Prometheus metrics from its diagnostics listener, which
binds `127.0.0.1:9090` inside the pod by default. Read it through the API server
pod proxy, which is how the e2e suite reaches the diagnostics port:

```bash
POD=$(kubectl -n unbounded-system get pods -l app.kubernetes.io/name=racer \
  -o jsonpath='{.items[0].metadata.name}')
kubectl get --raw "/api/v1/namespaces/unbounded-system/pods/$POD:9090/proxy/metrics" \
  | grep -E '^racer_(memory|disk|peer)_hits_total|^racer_origin_fills_total|^racer_requests_total'
```

`racer_memory_hits_total`, `racer_disk_hits_total`, and `racer_peer_hits_total`
count served pages by source. `racer_origin_fills_total` counts origin fills, so
the ratio of fills to requests is your cold-read rate. `racer_cache_disk_resident_bytes`
and `racer_cache_disk_share_bytes` show how much of a cache's disk share is in
use. If fills stay high after warmup, check page granularity, cache priority, and
whether the client pods are spread across nodes that each read a different slice
of the data.

## If Something Fails

Start with the layer that failed. Admission problems show up when the pod is
created, scheduling problems show up as a pending pod, container startup problems
show up in `describe` and events, and read problems show up as HTTP status codes
from the sidecar.

### The Pod Is Rejected at Creation

An admission denial comes back from `kubectl apply` and is also recorded on the
controller. For a Deployment, inspect the ReplicaSet events; for a StatefulSet,
the pod events; for a Job, the Job status and pod events:

```bash
kubectl -n blob-demo describe pod fetch-blob
kubectl -n blob-demo get events --sort-by=.lastTimestamp
```

The message names the rule that failed. The common ones are:

| Message | Cause |
| --- | --- |
| `annotation racer.unbounded-cloud.io/object-cache is required and must name the ClusterCache to bind to` | The label is present without the cache annotation. |
| `annotation racer.unbounded-cloud.io/object-api must be s3 or azure` | A typo in the API annotation. |
| `annotation racer.unbounded-cloud.io/object-listen must be a loopback IP literal and port (1-65535)` | A routable address or a bad port. |
| `pod already defines a volume named "racer-object-client"` | The pod spec already uses the volume name the webhook needs. |
| `pod spec.hostNetwork is true, which the racer-object sidecar does not support...` | Host networking would expose the loopback listener. Remove `hostNetwork`. |
| `pod spec.os.name ... conflicts with the racer-object sidecar, which requires linux` | The pod requests a non-Linux OS. |
| `annotation racer.unbounded-cloud.io/object-namespace is no longer supported` | A pre-release annotation that scoped a cache to a bucket. The cache origin selects the bucket now. |

The webhook denies a pod whose cache does not exist, and denies with a retryable
`503` when it cannot look the cache up. A `503` denial is worth retrying; a
message about the pod spec is not.

A pod that is admitted but stuck in `ContainerCreating` usually has a missing
socket directory. The injected volume uses `hostPath` type `Directory`, so the
kubelet refuses to start the container if `/run/racer/<cache>/client` does not
exist on the node. That directory is created by the dataplane for each published
cache, so check that the dataplane pod on that node is running and that the cache
name matches:

```bash
kubectl -n unbounded-system get pods -l app.kubernetes.io/name=racer -o wide
kubectl -n unbounded-system logs <racer-pod> -c dataplane --tail=100 \
  | grep 'stage=client-endpoint'
```

### Application and Sidecar Logs

The application container logs its own errors, and the sidecar logs the Racer
protocol failures. Read both:

```bash
kubectl -n blob-demo logs fetch-blob -c app
kubectl -n blob-demo logs fetch-blob -c racer-object
kubectl -n blob-demo get pod fetch-blob \
  -o jsonpath='{.metadata.annotations.racer\.unbounded-cloud\.io/object-sidecar-injected}{"\n"}'
```

The injected annotation holds the sidecar image. If it is empty, the pod was not
mutated. Check that the label value is exactly `enabled`, that the pod is not in
the Racer system namespace, and that the webhook configuration exists:

```bash
kubectl get mutatingwebhookconfiguration racer-object-sidecar-injector
```

For dataplane-side failures, read the dataplane container. Its logs use
`stage=` fields, and origin and socket problems appear there rather than in the
application:

```bash
kubectl -n unbounded-system logs <racer-pod> -c dataplane --tail=200
```

If Racer is not ready at all, confirm that a primary seat exists and that the
DaemonSet rolled out after the cache was created. An invalid origin is reported
in the `RacerReady` condition, not by the `ClusterCache`, which has no status.

### Read Status Codes

The Azure API returns XML error bodies with a `Code` and a `Message`, and the same
code in the `x-ms-error-code` response header. Use `curl -i` when you need the
code, since `-f` hides the body.

| Status | Code | Likely cause |
| --- | --- | --- |
| `400` | `InvalidInput` | Malformed path, missing blob segment, unsupported query shape, or a malformed conditional header. |
| `403` | `AuthorizationFailure` | The path container does not match the cache, the identity lacks the role, or the token was issued for the wrong tenant. |
| `404` | `BlobNotFound` | The blob does not exist in the container, or the requested `versionid` does not. |
| `405` | `UnsupportedHttpVerb` | A method other than `GET` or `HEAD` for a single blob. |
| `412` | `ConditionNotMet` | `If-Match` did not match the current ETag. |
| `416` | `InvalidRange` | The range starts beyond the object size. |
| `431` | `InvalidInput` | The request head exceeds 36 KiB. |
| `501` | `NotImplemented` | An operation or header the cache does not support, such as listing, writes, snapshots, leases, or checksum validation. |
| `502` | `BadGateway` | The sidecar received a malformed response from Racer. |
| `503` | `ServerBusy` | The cache is not ready, the socket is missing, or a request was rejected under load. Retry. |

`403` deserves a note. A cache serves exactly one container, so a request for any
other container in the same account is a `403` even when the identity could read
it directly. Check the first path segment against `spec.origin.azure.containerURL`
before you look at Azure RBAC. If the path is right, verify the role assignment
and its scope, and remember that Azure role assignments can take several minutes
to propagate. For workload identity, confirm the federated credential subject is
`system:serviceaccount:<namespace>:racer` with the namespace where the operator
installed Racer, and that the audience is `api://AzureADTokenExchange`.

### Cache Creation Is Rejected

The CRD validates the origin at admission. A cache is rejected when `origin` is
present without `azure`, when the container URL is not `https` with a credential,
when the URL carries a query or credentials, when both credential modes or
neither are set, when `metadataTTL` is negative, when `requestTimeout` is not
positive, or when `maxConcurrentRequests` is outside 1 to 65536.

Editing `containerURL` on an existing cache fails with an immutability error, and
adding or removing `spec.origin` fails with `spec.origin cannot be added or
removed; create a new ClusterCache to change the origin`. Both are intentional.
The cache identity is the UID, so a new source needs a new object and a new,
empty cache.

## Clean Up

Delete the application namespace first, then the cache. Deleting the namespace
stops the client pods; deleting the cache removes it from the dataplane, and the
operator retains the Racer installation for future caches.

```bash
kubectl delete namespace blob-demo
kubectl delete ccache mnist
```

Deleting a `ClusterCache` does not delete data from the Azure container, and it
does not remove the Racer DaemonSet. The operator keeps an existing installation
when no caches remain, so the next cache you create starts serving without a
reinstall of the dataplane.

Recreating a cache does not reuse cached data. The new object has a new
Kubernetes UID, and cached pages are keyed by that UID, so the first read of each
object fetches from the origin again. If you need a clean measurement, delete and
recreate the cache rather than deleting blobs or restarting pods.

## Next Steps

- Read [Racer Distributed Cache]({{< relref "concepts/racer" >}}) for how pages,
  owners, and reads work.
- Use the [ClusterCache reference]({{< relref "reference/racer" >}}) for the
  resource fields and their defaults.
- See the [racer-object reference]({{< relref "reference/racer-object" >}}) for
  flags, request limits, the S3 API, and the security model.
- Load models through the cache with
  [Load vLLM Models from Racer]({{< relref "guides/racer-vllm" >}}).
- If you have not installed the operator yet, start with
  [Install on an Existing Cluster]({{< relref "guides/existing-cluster" >}}).
