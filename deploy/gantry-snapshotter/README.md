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
- containerd 2.x.

In the image: `mkfs.erofs` (erofs-utils) and `dmsetup` (lvm2) on `PATH`.

Cluster-wide, the operator must have provisioned the image volume: one
IMMUTABLE_4M extent per segment plus a small OCC catalog extent, exported as
block devices on every node. Without it the agent still runs, every lookup
misses, and containerd unpacks locally exactly as it does today.

## Install

Order matters. containerd configured for a snapshotter whose socket is absent
refuses to create containers, so the agent goes first.

```sh
make gantry-snapshotter-manifests
kubectl apply -f deploy/gantry-snapshotter/rendered/serviceaccount.yaml
kubectl apply -f deploy/gantry-snapshotter/rendered/daemonset.yaml
kubectl -n unbounded-system rollout status ds/gantry-snapshotter
```

Confirm the socket exists on a node, then merge `containerd-config.toml` into
`/etc/containerd/config.toml` and restart containerd:

```sh
ls -l /run/gantry-snapshotter/snapshotter.sock
systemctl restart containerd
```

Verify containerd sees the plugin and that it works end to end:

```sh
ctr plugin ls | grep gantry
ctr -n k8s.io image pull --snapshotter gantry docker.io/library/alpine:latest
ctr -n k8s.io snapshot --snapshotter gantry ls
```

## Uninstall

Reverse order, for the same reason:

```sh
# 1. Remove the containerd stanzas and restart containerd on every node.
# 2. Then, and only then:
kubectl delete -f deploy/gantry-snapshotter/rendered/daemonset.yaml
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
inside the agent's mount namespace at `/run/gantry-snapshotter/l/<name>` and
must propagate to the host, because runc assembles the container rootfs
there. If containers start with layers missing rather than failing outright,
check that `mountPropagation: Bidirectional` survived your overlay.

**Layer mappings outlive the containers that used them.** The agent leaves
device-mapper targets and EROFS mounts in place so the next container that
needs the layer starts with a single overlay mount. They are swept by the
periodic cleanup, and only when the agent can account for every live
snapshot; a mapping leaked for one sweep is much cheaper than unmapping a
layer a running container is reading.
