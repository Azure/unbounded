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

Cluster-wide, the operator must have provisioned the image volume: a small OCC
catalog extent followed by a run of IMMUTABLE_4M extents, exported as one block
device on every node. Without it the agent still runs, every lookup misses, and
containerd unpacks locally exactly as it does today.

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
      size: 32Gi
      extentSize: 8Gi
      catalogSize: 256Mi
```

`size` is the usable capacity for layer bytes and must be a whole number of
`extentSize` extents. `extentSize` is the unit of reclamation: the cleaner
copies the layers still in use out of one extent and then has the whole extent
collected, because racer can only collect a whole extent at a time.

The operator creates one `gantry-image` PersistentVolume, waits for racer's
allocator to stamp a composition on it, and applies the manifests. It reports
`GantrySnapshotterReady` on the Site, staying `ImageVolumePending` until the
volume has been placed. The volume is `Retain` and is never resized: a device's
extent list and an extent's page count are both frozen once the device exists,
so changing `size` or `extentSize` after the fact is reported and ignored.
Reclamation, not growth, is what keeps the volume usable.

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
while `/run/gantry-snapshotter/snapshotter.sock` on that node actually answers.
Doing both at once deadlocks the node permanently after the first reboot,
because the default snapshotter applies to the agent's own pod and the socket is
on tmpfs.

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

### Why node-config keeps running: phase 2 is a lease, not an install

containerd unpacks **every** image through the CRI images plugin's default
snapshotter. The runtime handler a pod names has no say in that. So
`runtimeClassName: gantry-bootstrap` keeps the agent's sandbox startable, but it
does not let the agent's own image be pulled. A node sitting on phase 2 with no
socket cannot pull anything at all - including the replacement agent that would
fix it. That is a node-wide, self-sustaining deadlock, and an ordinary upgrade
walks straight into it: the old agent pod is deleted, its socket goes, and the
new one needs a pull.

So node-config does not install phase 2 and stop. It holds it:

- Every `--interval` (15s) it dials the socket. Dials, not stats, because a
  killed agent leaves a socket file that exists and refuses connections.
- While the socket answers, phase 2 is applied and kept applied.
- Once it has been silent for `--revert-after` (90s), phase 2 is removed and
  containerd is restarted. The node is back on overlayfs and can pull again.
  The grace period is what keeps an agent restart from churning the node.
- When node-config itself is terminated it removes phase 2 on the way out,
  before its own replacement needs an image. This is why its
  `terminationGracePeriodSeconds` is 180: it has to rewrite the file and wait
  for containerd to come back.
- Phase 1 is never removed. It is inert, and it is what lets the agent start.

`--keep-default-on-exit` suppresses the shutdown revert. It exists for
debugging; do not use it on a cluster you care about.

### Rescuing a node by hand

If a node is wedged anyway - node-config was SIGKILLed while the agent was also
gone - every pull on it fails with `dial
unix:///run/gantry-snapshotter/snapshotter.sock`. Recovery needs a pod that
needs no pull:

```sh
# A privileged pod pinned to the node, using the bootstrap handler and an image
# tag that is already resident there, so nothing has to be unpacked.
kubectl run rescue --image=<a tag already on that node> --image-pull-policy=IfNotPresent \
  --overrides='{"spec":{"nodeName":"<node>","hostPID":true,
    "runtimeClassName":"gantry-bootstrap",
    "containers":[{"name":"rescue","image":"<same tag>","command":["sleep","3600"],
      "securityContext":{"privileged":true}}]}}'

# Then, on the host, drop phase 2 and restart containerd.
kubectl exec rescue -- nsenter -t 1 -m -u -i -n -p -- \
  sh -c "cp /etc/containerd/config.toml.gantry-orig /etc/containerd/config.toml && \
         systemctl restart containerd"
```

node-config will re-apply both phases on its next pass once the agent is back.

### Why the agent runs under its own RuntimeClass

The socket at `/run/gantry-snapshotter/snapshotter.sock` is on tmpfs, so it does
not survive a reboot. If the agent's pod were created through the default CRI
snapshotter, then after a reboot kubelet would ask an absent socket to create
the pod that creates that socket, and the node could never start a pod again.
`runtimeClassName: gantry-bootstrap` selects a containerd handler pinned to
overlayfs, which breaks the cycle. Do not remove it, and do not give that
handler `snapshotter = "gantry"`.

It covers sandbox creation only. Image pulls are covered by the watchdog above.

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
| `--devices` | `/run/racer/image-devices.json` | Re-read on a timer; the agent picks up reclaimed segments without a restart |
| `--map-root` | `/var/lib/gantry-snapshotter/l` | Where layer mounts appear. Keep it short and under the same parent as `--root`, or deep images run out of mount options |
| `--work-headroom` | `4Gi` (DaemonSet sets `8Gi`) | Free bytes on the work filesystem an ingest refuses to spend. Raise it above the kubelet's `nodefs` eviction threshold on nodes with small root disks |
| `--format-catalog` | `false` | The DaemonSet sets it to `true`. Formatting is a compare-and-swap on block zero, so every node may safely try |
| `--adopt-segments` | `true` | Register newly visible segments and open one for writing if none is open |
| `--watermark-blocks` | `8` | Drain-gate table size, set at format time. Eight blocks covers a thousand nodes in 32 KiB; a node with no slot is a node the cleaner cannot wait for |
| `--watermark-grace` | `10m` | How long a node's watermark stands before the cleaner treats it as belonging to a node that is gone. Too short trims pages out from under a slow node; too long lets one decommissioned node stall reclamation |
| `--clean` | `true` | Runs the reclaim cycle. Turning it off means a sealed segment never comes back |
| `--clean-interval` | `1m` | One segment advances one step per pass, so a full cycle takes several |
| `--clean-low-water` | `0.25` | Free fraction of the image volume below which the cleaner starts picking victims |
| `--clean-max-live` | `0.5` | Live fraction above which a sealed segment is not worth copying out of |
| `--members-selector` | unset | Enables the peer view that ranks ingest work. Unset means every node ingests every layer |
| `--election-step` | `30s` | Delay per rendezvous rank. Must exceed one EROFS build plus one segment write, or the deduplication is lost |
| `--ingest-workers` | `1` | Ingest is off the container start path; making it fast buys nothing and competes with the pods the node is trying to run |
| `--skip-verify` | `false` | RACER's 4 MiB pages carry no data checksum, so the writer reads its blob back before publishing it. Turn this off only if you have measured that it matters |
| `--conflict-errnos` | unset | The errno the image device reports for a failed optimistic write. Defaults to `EAGAIN,EBUSY`, which is what RACER reports today |
| `--metrics-addr` | unset | The DaemonSet sets `:9096`, which serves `/metrics` and `/healthz` |
| `--pprof` | `false` | Adds `/debug/pprof` to the metrics listener. That listener is on the pod network, so leave it off outside an investigation |

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

**Segments fill and roll by themselves.** Blobs are appended into one segment
at a time. The first reservation that does not fit seals that segment and moves
appends to the lowest empty one that can hold the blob, logging `segment full,
ingest rolled to the next one`. No restart or reconfiguration is involved, and
nodes rolling at the same instant converge on the same segment. A steady roll
rate is normal; what matters is whether reclamation is keeping up with it.

**Reclamation is what gives a sealed segment back.** Once free space falls
below `--clean-low-water`, one node per segment, chosen by rendezvous hashing,
walks a sealed segment through four states, one step per pass:

- *Cleaning* copies every layer still in use out of the victim and republishes
  its catalog record at a higher generation. Readers take the highest
  generation for a layer, so a node that has not caught up keeps resolving the
  old location, which is still live.
- *Draining* waits until every node's watermark has passed that generation,
  then discards the victim's whole byte range. A discarded IMMUTABLE_4M page
  reads back as zeroes rather than an error, so this wait is not optional: it
  is the difference between reclaiming space and silently corrupting a mount
  another node is still reading.
- *Empty* happens when the operator sees the extent holds no live pages,
  advances its tombstone epoch, and racer-ctrl republishes the device map with
  the new epoch. That is the only signal that the space is actually back.

`gantry_snapshotter_clean_cycles_total{phase}` shows where cycles are ending.
Cycles piling up in `waiting` mean a node is not publishing a watermark: check
that it has `--node-name` set and that its `Cleanup` is running. Once no
segment can hold a layer, ingest logs `catalog: full` and every new layer goes
back to being unpacked on every node, which costs throughput and nothing else.

## Resetting the layer store

The catalog is the index for every blob in every segment, so it cannot be
reformatted in place: `Format` refuses a device that already holds a catalog,
and it refuses one that still holds records even if the superblock has been
cleared by hand. Clearing the superblock and letting a node reformat on top of
the leftover records produces a catalog that reports zero records and then
collides with the residue on its first append.

To start over, replace the image volume and let the operator rebuild it on
fresh extents. This is the last resort: reclamation handles a full volume on
its own, and this does not.

```sh
# Nothing may hold the block devices while the volumes go away.
kubectl -n unbounded-system scale deploy/unbounded-operator --replicas=0
kubectl -n unbounded-system patch ds/gantry-snapshotter --type=merge \
  -p '{"spec":{"template":{"spec":{"nodeSelector":{"gantry-snapshotter-quiesced":"true"}}}}}'
kubectl -n unbounded-system rollout status ds/gantry-snapshotter

kubectl delete pv -l app.kubernetes.io/name=gantry-snapshotter --wait=false

# The operator drops racer's extent finalizer, recreates the volumes, and
# restores the DaemonSet's node selector on its own.
kubectl -n unbounded-system scale deploy/unbounded-operator --replicas=1
kubectl get pv -l app.kubernetes.io/name=gantry-snapshotter \
  -o custom-columns=NAME:.metadata.name,COMP:'.metadata.annotations.racer\.unbounded-cloud\.io/composition'
```

Every layer already in the old segments is discarded. Nodes go back to
unpacking locally until the images are pulled again, which costs throughput and
nothing else.
