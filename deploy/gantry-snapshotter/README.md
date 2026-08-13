# gantry-snapshotter deployment

A containerd snapshotter that reads container image layers straight out of
[RACER](../../cmd/racer/README.md) instead of downloading and unpacking them
on every node. Each layer is converted to an uncompressed EROFS image
**once per cluster**; every other node maps it with a device-mapper linear
target and mounts it read-only as an overlayfs lowerdir. The writable
container layer stays on local disk.

## Prerequisites

Per node:

- A kernel with `CONFIG_EROFS_FS`, `CONFIG_BLK_DEV_DM` and `CONFIG_OVERLAY_FS`,
  and the `erofs`, `dm_mod` and `ublk_drv` modules loaded.
- `racer` and `racer-ctrl` running, with the image device description
  racer-ctrl publishes at `/run/racer/image-devices.json`.
- containerd 2.x, with a `gantry-bootstrap` runtime handler pinned to
  overlayfs. This is not optional; see "Why the agent runs under its own
  RuntimeClass" below. The `gantry-snapshotter-node-config` DaemonSet installs
  it; see "Install".

In the image: `mkfs.erofs` (erofs-utils) and `dmsetup` (lvm2) on `PATH`.

Cluster-wide, the operator must have provisioned the image volume: one
IMMUTABLE_4M extent per segment plus a small OCC catalog extent, exported as
block devices on every node. Without it the agent still runs, every lookup
misses, and containerd unpacks locally exactly as it does today.

## Install

Enable the component on the Site and the operator does the rest:

```yaml
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: default
spec:
  components:
    racer:
      enabled: true
    gantrySnapshotter:
      enabled: true
      segments: 4
      segmentSize: 8Gi
      catalogSize: 256Mi
```

The operator creates one `gantry-image-segment-<n>` PersistentVolume per
segment plus `gantry-image-catalog`, waits for racer's allocator to stamp a
composition on each, and applies the manifests. It reports
`GantrySnapshotterReady` on the Site, staying `ImageVolumesPending` until every
image volume has been placed. The image volumes are `Retain` and are never
resized: the extent page count is frozen when the volume is allocated, so
changing `segmentSize` after the fact is reported and ignored.

Watch it come up:

```sh
kubectl get site default -o jsonpath='{.status.conditions}' | jq
kubectl get pv -l app.kubernetes.io/name=gantry-snapshotter
kubectl -n unbounded-system rollout status ds/gantry-snapshotter-node-config
kubectl -n unbounded-system rollout status ds/gantry-snapshotter
```

### Manual install

The same manifests can be applied by hand. Order matters: the RuntimeClass has
to exist before the DaemonSet that selects it, and the containerd handler has
to exist before either or the pods stay Pending. The rendered files are
numbered in the order they must be applied.

```sh
make gantry-snapshotter-manifests
kubectl apply -f deploy/gantry-snapshotter/rendered/00-serviceaccount.yaml
kubectl apply -f deploy/gantry-snapshotter/rendered/01-runtimeclass.yaml
kubectl apply -f deploy/gantry-snapshotter/rendered/02-node-config.yaml
kubectl apply -f deploy/gantry-snapshotter/rendered/03-daemonset.yaml
kubectl -n unbounded-system rollout status ds/gantry-snapshotter
```

### What node-config does, and why it is two phases

`gantry-snapshotter-node-config` merges this repository's stanzas into the
node's `/etc/containerd/config.toml` and restarts containerd. It merges into the
parsed document rather than appending text, because that file belongs to
whatever installed the node: on AKS it pins the sandbox image, the registry
config path and runc's `SystemdCgroup`, and all of that has to survive. The
original is kept once at `/etc/containerd/config.toml.gantry-orig`.

It does it in two passes, and the split is deliberate.
[`containerd-config.toml`](containerd-config.toml) shows what each one writes.

**Phase 1** registers the proxy plugin and adds a `gantry-bootstrap` runtime
handler pinned to overlayfs, copied from the node's real default handler so it
keeps its cgroup driver and runc binary. Nothing selects either yet, so it
changes no pod's behaviour. It is safe before the agent exists: containerd dials
a proxy plugin lazily.

**Phase 2** makes `gantry` the CRI default snapshotter, and is applied only
after `/run/gantry-snapshotter/snapshotter.sock` exists on that node. Doing both
at once deadlocks the node permanently after the first reboot, because the
default snapshotter applies to the agent's own pod and the socket is on tmpfs.

The node-config pod deliberately carries no `runtimeClassName`, because it is
what creates the handler that class names. After a reboot of a fully configured
node it cannot start until the agent is serving again - the agent can, because
it uses `gantry-bootstrap` - and there is nothing for it to do until then
anyway.

Verify it took:

```sh
ctr plugin ls | grep gantry
ctr -n k8s.io image pull --snapshotter gantry docker.io/library/alpine:latest
ctr -n k8s.io snapshot --snapshotter gantry ls
```

### Why the agent runs under its own RuntimeClass

The socket at `/run/gantry-snapshotter/snapshotter.sock` is on tmpfs, so it does
not survive a reboot. If the agent's pod were created through the default CRI
snapshotter, then after a reboot kubelet would ask an absent socket to create
the pod that creates that socket, and the node could never start a pod again.
`runtimeClassName: gantry-bootstrap` selects a containerd handler pinned to
overlayfs, which breaks the cycle. Do not remove it, and do not give that
handler `snapshotter = "gantry"`.

## Uninstall

Reverse order, for the same reason:

```sh
# 1. Remove the phase 2 stanzas and restart containerd on every node. The
#    original config is at /etc/containerd/config.toml.gantry-orig.
# 2. Then, and only then:
kubectl delete -f deploy/gantry-snapshotter/rendered/03-daemonset.yaml
kubectl delete -f deploy/gantry-snapshotter/rendered/02-node-config.yaml
kubectl delete -f deploy/gantry-snapshotter/rendered/01-runtimeclass.yaml
# 3. Finally, remove the phase 1 stanzas.
```

Setting `gantrySnapshotter.enabled: false` on the Site does not uninstall
anything, in the same way that disabling racer does not. The image volume holds
the only copy of a converted layer, and the containerd edits have to come off a
node before the pods do.

Snapshots created by this snapshotter are not readable by overlayfs, so any
pod still running on a rootfs it provided must be recreated after the
switch back.

## Configuration

Every flag has a `GANTRY_SNAPSHOTTER_*` environment fallback, so the DaemonSet
can be tuned without changing its `args`. The ones worth knowing:

| Flag | Default | Notes |
| --- | --- | --- |
| `--devices` | `/run/racer/image-devices.json` | Re-read on a timer; the agent picks up new segments without a restart |
| `--map-root` | `/var/lib/gantry-snapshotter/l` | Where layer mounts appear. Keep it short and under the same parent as `--root`, or deep images run out of mount options |
| `--work-headroom` | `4Gi` (DaemonSet sets `8Gi`) | Free bytes on the work filesystem an ingest refuses to spend. Raise it above the kubelet's `nodefs` eviction threshold on nodes with small root disks |
| `--format-catalog` | `false` | The DaemonSet sets it to `true`. Formatting is a compare-and-swap on block zero, so every node may safely try |
| `--adopt-segments` | `true` | Register newly visible segments and open one for writing if none is open |
| `--members-selector` | unset | Enables the peer view that ranks ingest work. Unset means every node ingests every layer |
| `--election-step` | `30s` | Delay per rendezvous rank. Must exceed one EROFS build plus one segment write, or the deduplication is lost |
| `--ingest-workers` | `1` | Ingest is off the container start path; making it fast buys nothing and competes with the pods the node is trying to run |
| `--skip-verify` | `false` | RACER's 4 MiB pages carry no data checksum, so the writer reads its blob back before publishing it. Turn this off only if you have measured that it matters |
| `--conflict-errnos` | unset | The errno the image device reports for a failed optimistic write. See the design doc's open questions |
| `--metrics-addr` | unset | The DaemonSet sets `:9096`, which also serves `/healthz` and `/debug/pprof` |

## Operating notes

**Everything degrades to "unpack locally".** A missing device file, an
unformatted catalog, a detached image volume, an unreachable containerd, an
unsynced peer informer: each of these turns lookups into misses and ingest
into a no-op. None of them fails a container start. When throughput looks
wrong, check the logs for `catalog unavailable` before assuming a data
problem.

**Mount propagation is the usual misconfiguration.** Layer mounts are made
inside the agent's mount namespace at `/var/lib/gantry-snapshotter/l/<name>`
and must propagate to the host, because runc assembles the container rootfs
there. If containers start with layers missing rather than failing outright,
check that `mountPropagation: Bidirectional` survived your overlay.

**An overlay's options have to fit in one page.** The kernel copies mount
data into a single page, so the joined option string, and with it the whole
`lowerdir=` list, is capped at 4095 bytes. containerd buys room back by
chdir'ing to the longest common prefix of the lowerdirs, which only helps if
the mapped layers and the locally unpacked ones share a parent. That is why
`--map-root` defaults to a directory beside `--root` rather than somewhere
under `/run`, and why the agent logs a warning at startup if you point them
at unrelated paths. A stack that would overflow anyway is refused with a
failed precondition rather than mounted with the deepest layers silently
truncated.

**Layer mappings outlive the containers that used them.** The agent leaves
device-mapper targets and EROFS mounts in place so the next container that
needs the layer starts with a single overlay mount. They are swept by the
periodic cleanup, and only when the agent can account for every live
snapshot; a mapping leaked for one sweep is much cheaper than unmapping a
layer a running container is reading.

**Ingest needs disk and gives it up first.** Converting a layer writes the
uncompressed tarball and then the EROFS image beside it under `--work-dir`,
so a layer costs about twice its own size on the node's root filesystem while
it is being built. The agent statfs's that filesystem before it fetches
anything and refuses the layer if the conversion would eat into
`--work-headroom`. A refusal is logged and is not a failure: the container
that triggered it is already running on a locally unpacked layer, and some
other node will publish it. Persistent `not enough room on the work filesystem` means the nodes are too small for the
images, not that the snapshotter is broken.

**The liveness probe asks the agent a real question.** `/healthz` dials the
snapshotter's own socket and requests a snapshot that cannot exist, expecting a
not-found. A daemon whose listener has stopped accepting, whose gRPC handlers
are all blocked, or whose metadata store is holding a transaction nobody will
commit fails that request, and three failures kill the pod. That is the right
outcome: a wedged agent leaves every pod on the node in `ContainerCreating`
with nothing to show for it, and a restart either clears the wedge or degrades
the node to local unpack, both of which beat a black hole. `/healthz` needs
`--metrics-addr` to be set; without it there is no probe at all.

**The agent says so when containerd is not passing layer annotations.**
`disable_snapshot_annotations` defaults to true on Linux, and with it on the
agent still serves every container correctly while publishing absolutely
nothing: it never learns which blob a layer came from, so no layer it unpacks
can ever be converted, and every other node keeps paying full price. There is
no way to detect this other than noticing the label is missing, so the first
image layer committed without one logs a warning naming the setting. It is
logged once per process, not once per layer.

**Segments fill and roll by themselves, and a roll is a capacity warning.**
Blobs are appended into one segment at a time. The first reservation that does
not fit seals that segment and moves appends to the lowest empty one that can
hold the blob, logging `segment full, ingest rolled to the next one`. No
restart or reconfiguration is involved, and nodes rolling at the same instant
converge on the same segment. What the message means is that a chunk of the
image volume has stopped accepting writes until a cleaner reclaims it, so a
node logging it regularly is a cluster running out of image volume: provision
more segments before the last one seals. Once no segment can hold a layer,
ingest logs `catalog: full` and every new layer goes back to being unpacked on
every node, which costs throughput and nothing else.
