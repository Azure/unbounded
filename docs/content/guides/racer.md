---
title: "Cache Objects with Racer"
weight: 9
description: "Deploy Racer and connect applications to a shared object cache with the Go SDK."
---

Racer caches application objects across Kubernetes nodes. Your application supplies
an origin adapter; Racer handles cache placement and peer reads. Read the
[concepts]({{< relref "concepts/racer" >}}) first. For container images, use
[Gantry's Racer backend]({{< relref "guides/gantry" >}}#use-racer-as-the-backend)
instead of writing an adapter.

## 1. Prepare the Cluster

Install the `kubectl unbounded` plugin and a matching Unbounded release; see the
[CLI reference]({{< relref "reference/cli" >}}). Bootstrap the operator:

```bash
kubectl unbounded install --timeout=5m
```

Use Linux nodes with working `io_uring` support, sufficient local disk, and
permission for the dataplane's privileged UID 0 container and hostPath mounts.
Kernel or container security policy must not block `io_uring`. Do not disable
cluster-wide admission protections; authorize these workloads narrowly. The
dataplane uses `/var/lib/racer/identity`, `/var/lib/racer/slabs`, and `/run/racer`.
RDMA is optional; its device and network requirements are in the
[reference]({{< relref "reference/racer" >}}#rdma).

Examples use the default operator namespace, `unbounded-system`. Substitute your
installation namespace if different. You need permission to create cluster-scoped
ClusterCaches and deploy workloads with the required mounts.

## 2. Create a Cache

Save this as `models-cache.yaml`, then apply it:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: models
```

```bash
kubectl apply -f models-cache.yaml
kubectl get clustercache models
kubectl -n unbounded-system get deployment,daemonset
kubectl -n unbounded-system rollout status deployment/racer-controller --timeout=5m
kubectl -n unbounded-system rollout status daemonset/racer-dataplane --timeout=5m
```

The first cache triggers operator-managed Racer installation. Allow reconciliation
to create the workloads before checking rollout status. ClusterCache has **no
`spec` or `status`**: it names a cache, not its origin, capacity, or readiness.
Check the workloads and diagnostics instead. If mixed networking is configured,
also verify `daemonset/racer-dataplane-podnet`.

### Placement and Capacity

To exclude a node, add the exclusion label. **Presence** excludes it, even when
the value is `false`; remove the label to make it eligible again:

```bash
kubectl label node NODE racer.unbounded-cloud.io/exclude=true
kubectl label node NODE racer.unbounded-cloud.io/exclude-
kubectl annotate node NODE racer.unbounded-cloud.io/shares=8 --overwrite
```

The `shares` annotation is a positive placement weight, not a memory or storage
limit. It overrides the enrolled weight (default 4).

Tune the `racer-dataplane-config` ConfigMap rather than editing managed Pods.
Administrator data is preserved, and edits automatically trigger a managed
dataplane rollout. For example, increase the node-wide plaintext admission budget:

```bash
kubectl -n unbounded-system patch configmap racer-dataplane-config --type=merge \
  -p '{"data":{"RACER_PLAINTEXT_BYTES":"536870912"}}'
```

Observe the new rollout before repeating the rollout-status check. Byte admission
budgets are **node-wide** and divided among I/O workers; `RACER_SLAB_BYTES` and
segment geometry are **per I/O worker**. Automatic worker sizing affects total
disk capacity. Budgets are not RSS limits: leave room for TLS, allocators, and
filesystem cache. See [configuration]({{< relref "reference/racer" >}}#dataplane-configuration)
for defaults and constraints, and use
[workload overrides]({{< relref "reference/workload-overrides" >}}) for scheduling
or Kubernetes resource requests.

## 3. Connect Your Application

Run exactly **one origin owner per cache per node**, on **every eligible node
that can be selected to supply origin reads**, not just on the requesting node.
Use matching placement/tolerations for Racer and the adapters, typically a
DaemonSet. Every adapter must resolve the same key/version to the same bytes.
Consumers must run on nodes with Racer. The SDK does not install a cache or
fall back directly to an origin when Racer is unavailable.

### Mount Only the Required Socket Directories

| Role | Host directory mounted at the same container path | Access |
|------|--------------------------------------------------|--------|
| Consumer only | `/run/racer/models/client` | Read-only mount; connect to `socket` |
| Origin only | `/run/racer/models/origin` | Writable mount; creates `socket` |
| Combined consumer and origin | Both directories above | Keep client read-only and origin writable |

Use hostPath `Directory` for the dataplane-created client directory. Provision the
origin directory before `ServeOrigin` (for example, root-owned hostPath
`DirectoryOrCreate`). Its ancestors must be real, trusted directories, not
symlinks or writable by untrusted users. Run the origin as UID/GID 0 to match the
root dataplane's access to its default `0600` socket. An origin need not itself
be privileged. Do not mount all of `/run/racer` into consumers: mount permission
grants cache access. Never delete a live socket to bypass another owner.

### Read with the Go SDK

Import `github.com/Azure/unbounded/pkg/racersdk`. These function snippets also use
the standard `context`, `errors`, `io`, `strings`, and `time` packages. At startup,
call `newModelsClient`, handle its error, and reuse the returned client across
requests. At shutdown, call `client.Close()` and handle its error.

```go
func newModelsClient() (*racersdk.Client, error) {
    cache, err := racersdk.ParseCacheName("models")
    if err != nil {
        return nil, err
    }
    return racersdk.NewClient(racersdk.ClientConfig{Cache: cache})
}

func readHello(ctx context.Context, client *racersdk.Client, dst io.Writer) (err error) {
    key, err := racersdk.ParseKey(strings.Repeat("01", 32))
    if err != nil {
        return err
    }
    ctx, cancel := context.WithTimeout(ctx, time.Minute)
    defer cancel() // Keep the context alive through the last read.
    value, err := client.Get(ctx, racersdk.Request{Key: key})
    if err != nil {
        return err
    }
    defer func() { err = errors.Join(err, value.Close()) }()
    _, err = io.Copy(dst, value)
    return err
}
```

`NewClient` validates configuration without dialing. A successful constructor is
not a readiness check. Always close each `Value`, including after partial reads;
a copy error means the destination may contain incomplete data.

### Supply an Origin

This tiny immutable fixture serves `hello` for the same key used above. It handles
HEAD, bootstrap, pinned reads, exact ranges, metadata, and millisecond-aligned
expiry. Call `serveModels` from the origin process with a shutdown context; it
blocks until cancellation or failure. Treat `context.Canceled` as expected during
shutdown and report other returned errors.

```go
func serveModels(ctx context.Context) error {
    cache, err := racersdk.ParseCacheName("models")
    if err != nil {
        return err
    }
    key, err := racersdk.ParseKey(strings.Repeat("01", 32))
    if err != nil {
        return err
    }
    tag, err := racersdk.ParseETag(`"hello-v1"`)
    if err != nil {
        return err
    }
    const data = "hello"
    m := racersdk.Metadata{
        Size: racersdk.ByteLength(len(data)), ETag: tag,
        ContentType: "text/plain", ExpiresAt: time.UnixMilli(1_900_000_000_000),
    }
    origin := racersdk.Origin(func(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
        if err := ctx.Err(); err != nil {
            return racersdk.Metadata{}, nil, err
        }
        if r.Key() != key {
            return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorNotFound, nil)
        }
        if pin, ok := r.Pin(); ok && pin != tag {
            return racersdk.Metadata{}, nil, racersdk.NewOriginError(racersdk.ErrorVersionUnavailable, nil)
        }
        if r.Operation() == racersdk.OperationHead ||
            (r.Operation() == racersdk.OperationBootstrap && m.Size == 0) {
            return m, nil, nil
        }
        page, _ := r.Range() // Validated by the SDK; absent only for HEAD.
        first, last, err := page.Resolve(m.Size)
        if err != nil {
            return m, nil, err // Preserve metadata for a 416 response.
        }
        return m, io.NopCloser(strings.NewReader(data[int(first):int(last)+1])), nil
    })
    return racersdk.ServeOrigin(ctx, racersdk.OriginConfig{Cache: cache}, origin)
}
```

In a real adapter, select metadata and bytes atomically from an immutable version;
do not stat a mutable name and then open potentially different bytes. Propagate
cancellation into backend I/O, and ensure body `Close` promptly unblocks `Read`.
The SDK owns returned bodies, even on errors. Keep metadata consistent and choose
expiry deliberately. The [SDK examples](https://github.com/Azure/unbounded/blob/main/pkg/racersdk/example_test.go)
cover startup/shutdown and version/range behavior; the
[API reference]({{< relref "reference/racer" >}}#go-sdk) covers options, classified
errors, and opt-in owned stale-socket recovery.

For local adapter tests, import `github.com/Azure/unbounded/pkg/racersdk/racersdktest`
and call `racersdktest.NewClient(origin)`. Register its returned cleanup function
with `t.Cleanup`; `Client.Close` alone does not stop the test servers. This helper
uses private temporary Unix sockets and real SDK origin validation, but does not
cache data or establish compatibility with the real Racer runtime. Use a short
`TMPDIR` so each socket path fits the 107-byte Unix socket limit.

## 4. Verify Reads and Operate Safely

Start origins before issuing reads. Read the fixture from consumers on two nodes
and verify that both receive `hello`. Find a dataplane Pod's IP and inspect its logs:

```bash
kubectl -n unbounded-system get pods -o wide
kubectl -n unbounded-system logs POD -c dataplane
```

From a workload with network access to that Pod IP, check
`curl -fsS http://POD_IP:9090/readyz` and `curl -fsS http://POD_IP:9090/metrics`.
Managed diagnostics bind to the Pod IP, not loopback. A successful scrape alone does not prove
readiness or origin availability. Watch request errors, overloads, and per-tier
lookup counters. Use `/debug/membership` and `/debug/failures` for diagnostics;
restrict access to this listener. See the [reference]({{< relref "reference/racer" >}})
for ports, metrics, and network configuration.

**Upgrade and recovery:** keep the operator, controller, dataplane, SDK, and origin
adapters on coordinated compatible versions. Preserve durable cluster identity,
version counters, credentials, and node identity state; do not delete them to
force a fresh start. Disposable cached bytes are not disposable installation
state. Deleting and recreating a ClusterCache creates a new cache identity, even
with the same name. Follow release-specific recovery instructions rather than
resetting durable state independently.
