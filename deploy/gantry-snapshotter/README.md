# gantry-snapshotter deployment

A containerd snapshotter that reads container image layers straight out of
[RACER](../../cmd/racer/README.md) instead of downloading and unpacking them
on every node. Each layer is converted to an uncompressed EROFS image
**once per cluster**; every other node maps it with a device-mapper linear
target and mounts it read-only as an overlayfs lowerdir. The writable
container layer stays on local disk.

The full rationale, on-disk formats and failure analysis live in
[designs/gantry-snapshotter-design.md](../../designs/gantry-snapshotter-design.md).
This directory is only the deployment.

## Files

| File | Purpose |
| --- | --- |
| `serviceaccount.yaml.tmpl` | Namespace, ServiceAccount, RBAC and the PriorityClass |
| `runtimeclass.yaml.tmpl` | The `gantry-bootstrap` RuntimeClass the agent runs under |
| `daemonset.yaml.tmpl` | The node agent |
| `containerd-config.toml` | Stanzas to merge into `/etc/containerd/config.toml` |

Templates are rendered with `make gantry-snapshotter-manifests`, which writes
plain YAML into `rendered/`. Override the namespace and image with
`GANTRY_SNAPSHOTTER_NAMESPACE` and `GANTRY_SNAPSHOTTER_IMAGE`.

## Prerequisites

Per node:

- A kernel with `CONFIG_EROFS_FS`, `CONFIG_BLK_DEV_DM` and `CONFIG_OVERLAY_FS`.
- `racer` and `racer-ctrl` running, with the operator-rendered image device
  description at `/run/racer/image-devices.json`.
- containerd 2.x, with a `gantry-bootstrap` runtime handler pinned to
  overlayfs. This is not optional; see "Why the agent runs under its own
  RuntimeClass" below.

In the image: `mkfs.erofs` (erofs-utils) and `dmsetup` (lvm2) on `PATH`.

Cluster-wide, the operator must have provisioned the image volume: one
IMMUTABLE_4M extent per segment plus a small OCC catalog extent, exported as
block devices on every node. Without it the agent still runs, every lookup
misses, and containerd unpacks locally exactly as it does today.

## Install

Three phases, in this order. The middle one is the only one that can be done
from the API server, and the two containerd edits are deliberately separate:
`snapshotter = "gantry"` is the CRI default for every pod on the node, and the
agent is a pod.

**1. Install the bootstrap runtime handler on every node.** Merge phase 1 of
[`containerd-config.toml`](containerd-config.toml) into
`/etc/containerd/config.toml` and restart containerd. This adds a
`gantry-bootstrap` runtime handler pinned to overlayfs and registers the proxy
plugin. Nothing selects either yet, so it changes no pod's behaviour.

```sh
systemctl restart containerd
ctr plugin ls | grep gantry
```

**2. Apply the manifests.** The RuntimeClass has to exist before the DaemonSet
that selects it, and the handler from phase 1 has to exist before either, or
the pods stay Pending.

```sh
make gantry-snapshotter-manifests
kubectl apply -f deploy/gantry-snapshotter/rendered/serviceaccount.yaml
kubectl apply -f deploy/gantry-snapshotter/rendered/runtimeclass.yaml
kubectl apply -f deploy/gantry-snapshotter/rendered/daemonset.yaml
kubectl -n unbounded-system rollout status ds/gantry-snapshotter
```

**3. Point CRI at the snapshotter.** Only once the socket exists on the node.
Merge phase 2 and restart containerd:

```sh
ls -l /run/gantry-snapshotter/snapshotter.sock
systemctl restart containerd
```

Verify it works end to end:

```sh
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
# 1. Remove the phase 2 stanzas and restart containerd on every node.
# 2. Then, and only then:
kubectl delete -f deploy/gantry-snapshotter/rendered/daemonset.yaml
kubectl delete -f deploy/gantry-snapshotter/rendered/runtimeclass.yaml
# 3. Finally, remove the phase 1 stanzas.
```

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
other node will publish it. Persistent `not enough room on the work
filesystem` means the nodes are too small for the images, not that the
snapshotter is broken.
