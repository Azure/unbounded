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
dataplane uses `/var/lib/racer/identity`, `/var/lib/racer/slabs`, and `/run/racer`,
and mounts host `/dev` at `/host/dev` for optional raw cache devices.
RDMA is optional; its device and network requirements are in the
[reference]({{< relref "reference/racer" >}}#rdma).

Examples use the default operator namespace, `unbounded-system`. Substitute your
installation namespace if different. You need permission to create cluster-scoped
ClusterVolumes and deploy workloads with the required mounts.

## 2. Create a Cache Volume

Save this as `models-volume.yaml`, then apply it:

```yaml
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterVolume
metadata:
  name: models
spec:
  type: Cache
```

```bash
kubectl apply -f models-volume.yaml
kubectl get cvol models
kubectl -n unbounded-system get deployment,daemonset
kubectl -n unbounded-system rollout status deployment/racer-controller --timeout=5m
kubectl -n unbounded-system rollout status daemonset/racer-dataplane --timeout=5m
```

Any ClusterVolume triggers operator-managed Racer installation. Allow reconciliation
to create the workloads before checking rollout status. `spec.type` is required
and immutable; `Cache` is the only supported value and only Cache volumes enter
the cache catalog. ClusterVolume has **no `status`** and does not configure an
origin or capacity. It is not a PVC and does not promise durable storage.
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
budgets are **node-wide** and divided among I/O workers. In file mode,
`RACER_SLAB_BYTES` is **per I/O worker**, so automatic worker sizing affects total
disk capacity. Raw-device capacity is instead divided among workers. Budgets are
not RSS limits: leave room for indexes, checkpoint scratch, TLS, allocators, and
filesystem cache. See [configuration]({{< relref "reference/racer" >}}#dataplane-configuration)
for defaults and constraints, and use
[workload overrides]({{< relref "reference/workload-overrides" >}}) for scheduling
or Kubernetes resource requests.

### Use Dedicated Raw Cache Devices

**This destroys existing data on selected devices.** Racer does not check for
filesystem signatures. It skips devices with child partitions, active holders,
read-only state, visible mounts, or a failed exclusive open, but those checks
cannot identify every disk that contains valuable data. Partitions are allowed;
an unmounted OS partition can be overwritten if your regex matches it.

Inspect the target node's `/dev/disk/by-id` entries and their resolved devices
first. Confirm that every match is dedicated to disposable cache data and is not
an OS disk, partition, or disk needed by another workload. Match basenames, not
full paths. Use both anchors because matching is otherwise unanchored.

For example, **only after replacing this illustrative vendor/model and serial
with your verified device ID**, annotate the Node:

```bash
kubectl annotate node NODE \
  'racer.unbounded-cloud.io/block-devices=^nvme-Samsung_SSD_970_EVO_Plus_1TB_S4EWNX0M123456A$' \
  --overwrite
```

The anchored example does not match a `-part1` suffix; it is not a safety check.
Keep the regex within 1,024 bytes and use syntax accepted by both Go and Rust.
Restart the affected dataplane Pod to apply an annotation change or removal.
The annotation alone does not trigger a rollout and renewal does not change
live storage. Check startup logs for `raw-device storage` or fallback/skip warnings;
no usable matches or insufficient capacity falls back to ordinary slab files.

Device mode uses all whole segments across selected devices, split among I/O
workers, subject to hard caps. Subsegment tails are lost; free-segment reserves,
headers, AEAD tags, and alignment reduce usable payload space.
`RACER_SLAB_BYTES` does not limit device capacity. Page-index and checkpoint
budgets can grow automatically, increasing memory use. Review the
[capacity and memory limits]({{< relref "reference/racer" >}}#raw-device-storage)
before selecting large devices.

The read-only `/host/dev` mount protects directory entries, not raw-device
contents. Writes are intentional. Keep `/var/lib/racer/slabs` writable: it still
stores checkpoints. Storage-mode and incompatible layout changes start cold;
cached data is not migrated between slab files and devices.

## 3. Connect Your Application

Run exactly **one origin owner per volume per node**, on **every eligible node
that can be selected to supply origin reads**, not just on the requesting node.
Use matching placement/tolerations for Racer and the adapters, typically a
DaemonSet. Every adapter must resolve the same key/version to the same bytes.
Consumers must run on nodes with Racer. The SDK does not create a volume or
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

The Go SDK is `github.com/Azure/unbounded/pkg/racersdk`. Its
[package documentation](https://pkg.go.dev/github.com/Azure/unbounded/pkg/racersdk)
is the SDK reference: it covers both read paths, origin contracts, errors, and
testing. These snippets also use the standard `context`, `errors`, `io`,
`strings`, and `time` packages.

Create one client at startup and reuse it for every request. `NewClient`
validates configuration without dialing, so it is not a readiness check.

```go
client, err := racersdk.NewClient(racersdk.ClientConfig{Volume: "models"})
if err != nil {
    return err
}
defer client.Close()
```

```go
func readHello(ctx context.Context, client *racersdk.Client, dst io.Writer) (err error) {
    key, err := racersdk.ParseKey(strings.Repeat("01", 32))
    if err != nil {
        return err
    }
    obj, err := client.Get(ctx, racersdk.Request{Key: key})
    if err != nil {
        return err
    }
    defer func() { err = errors.Join(err, obj.Close()) }()
    _, err = io.Copy(dst, obj)
    return err
}
```

An `Object` has two read paths. `Read` copies bytes into your buffer, which is
what you need to hash, parse, or transform them. `WriteTo` forwards bytes straight
to the destination without a copy through your memory, using `splice(2)` when the
destination is a file, socket, or plain-text HTTP/1 response. `io.Copy` uses
`WriteTo` automatically. Both paths withhold the final byte until Racer confirms
the whole object arrived intact. Always close each `Object`, including after
partial reads; a copy error means the destination may contain incomplete data.

### Supply an Origin

This tiny immutable fixture serves `hello` for the same key used above. Call
`serveModels` from the origin process with a shutdown context; it blocks until
cancellation or failure. Treat `context.Canceled` as expected during shutdown and
report other returned errors.

```go
func serveModels(ctx context.Context) error {
    key, err := racersdk.ParseKey(strings.Repeat("01", 32))
    if err != nil {
        return err
    }
    const data = "hello"
    m := racersdk.Metadata{
        Size: int64(len(data)), ETag: `"hello-v1"`,
        ContentType: "text/plain", ExpiresAt: time.UnixMilli(1_900_000_000_000),
    }
    origin := func(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
        switch {
        case r.Key != key:
            return racersdk.Metadata{}, nil, racersdk.ErrNotFound
        case r.ETag != "" && r.ETag != m.ETag:
            return racersdk.Metadata{}, nil, racersdk.ErrVersionMismatch
        case r.Head:
            return m, nil, nil
        }
        start := min(r.Offset, m.Size)
        end := min(r.Offset+r.Length, m.Size)
        return m, io.NopCloser(strings.NewReader(data[start:end])), nil
    }
    return racersdk.ServeOrigin(ctx, racersdk.OriginConfig{Volume: "models"}, origin)
}
```

In a real adapter, select metadata and bytes atomically from an immutable version;
do not stat a mutable name and then open potentially different bytes. Propagate
cancellation into backend I/O, and ensure body `Close` promptly unblocks `Read`.
The SDK owns returned bodies, even on errors, and rejects bodies that do not match
the requested range. Keep metadata consistent and choose expiry deliberately.

For local adapter tests, import `github.com/Azure/unbounded/pkg/racersdk/racersdktest`
and call `racersdktest.NewClient(t, origin)`. It stops its fake Racer when the
test ends. The helper uses private temporary Unix sockets and real SDK origin
validation, but does not cache data or establish compatibility with the real
Racer runtime. Use a short `TMPDIR` so each socket path fits the 107-byte Unix
socket limit.

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
state. Deleting and recreating a ClusterVolume creates a new cache identity, even
with the same name. Follow release-specific recovery instructions rather than
resetting durable state independently.

Checkpoint v3 discards older checkpoint formats in both file and device modes.
Expect a cold cache when upgrading from an older format or changing to an
incompatible device layout, worker count, or storage geometry. Recovery does not
scan payloads to rebuild the cache. Ensure origins can serve the resulting misses;
do not delete identity state to address a cold cache.

### Upgrade from ClusterCache

This is a breaking API, SDK, and CLI change, not an in-place resource conversion.

1. Stop consumers and origin adapters before upgrading. Save the old resource
   names and any metadata you need to recreate them.
2. Upgrade the operator and Racer components together. Replace each old manifest
   with `kind: ClusterVolume` and required `spec.type: Cache`, then apply it.
   New objects receive new Kubernetes UIDs. **Old cache identities and their cached
   data are not reused**, even when names and socket paths match.
3. Update applications to the current SDK and set `ClientConfig.Volume` and
   `OriginConfig.Volume`. Change racer-object and racer-loadgen arguments to
   `--volume`; there is no `--cache` alias. Keep the existing
   `/run/racer/<name>/client/socket` and `/run/racer/<name>/origin/socket` paths.
   Wire fields such as `CacheID` and runtime cache behavior are unchanged.
4. Wait for controller/dataplane readiness, restart updated origins and consumers,
   and verify reads against the authoritative origin. Preserve installation
   identity, credentials, and version counters.
5. Remove the old CRD manually only after saving what you need and completing the
   transition. The operator does **not** clean up the old CRD or its objects.

**Destructive cleanup:** the following command deletes the old CRD **and all
ClusterCache objects in the cluster**. It is not a migration or a rollback step.
Do not run it until you accept that loss:

```bash
kubectl delete crd clustercaches.racer.unbounded-cloud.io
```
