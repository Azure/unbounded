---
title: "Pool Node Storage with Racer"
weight: 9
description: "Deploy and operate racer, the distributed block device, with unbounded-operator."
---

Racer pools the local storage of a site's nodes into replicated block devices
that Kubernetes consumes as raw block PersistentVolumes. Each volume is split
into extents, each extent is replicated across a trio of nodes drawn from
separate failure domains, and the device is exported on the consuming node
through `ublk`, with remote replicas reached over NVMe-oF.

Racer is deployed by `unbounded-operator` as a cluster component. The operator
is the single writer of everything that has to be agreed across machines: node
identity, zone and cohort placement, catalog membership, and the extent layout
of every volume. The node agent (`racer-ctrl`) only renders that state into the
one config file the dataplane reads, and publishes what it observes back onto
its own Node object.

{{< callout type="warning" >}}
Racer takes over a store file and creates block devices on every node it runs
on, and it holds data. It is disabled by default, enabling it for a Site puts it
on every node in that Site, and the operator never uninstalls it automatically.
See [Removing racer](#removing-racer).
{{< /callout >}}

## Node Prerequisites

An init container named `preflight` verifies these before the dataplane starts,
so an unmet prerequisite is a legible `CrashLoopBackOff` on that container
rather than a later failure inside racer:

| Requirement | Why |
|---|---|
| `ublk_drv` loaded and `/dev/ublk-control` openable | The exported block device is a ublk device |
| `ublks_max` at least 256 (`modprobe ublk_drv ublks_max=256`, or `/etc/modprobe.d`) | racer's per-node export budget. The kernel default is lower and would refuse devices the first time a volume is staged |
| `/var/lib/racer` on a filesystem honoring `O_DIRECT` and `RWF_DSYNC` | racer manages its own cache, so the page cache must not lie to it about durability. tmpfs and overlayfs do not qualify |
| `nvmet` available under `/sys/kernel/config` | Only when the NVMe-oF fabric is in use, which is any multi-node universe |

The store file (`/var/lib/racer/store.img`) is created and grown by the node
agent as extents land on the node, so it needs free space on the host
filesystem rather than a preallocated size.

## 1. Enable Racer for a Site

At site creation time, pass `--enable-racer`. Racer is a cluster-scoped
component, but it only runs on the nodes of the Sites that ask for it: enabling
it on a Site puts racer on every node carrying that Site's
`unbounded-cloud.io/site` label. `site init` sets the flag on the remote Site
because that is where racer's nodes live, the same as `--enable-storage`.

```bash
kubectl unbounded site init \
  --name dc2 \
  --cluster-node-cidr 10.224.0.0/16 \
  --cluster-pod-cidr 10.244.0.0/16 \
  --node-cidr 10.200.0.0/24 \
  --pod-cidr 10.201.0.0/24 \
  --enable-racer
```

For an existing Site, set the component directly:

```bash
kubectl patch site dc2 --type merge \
  -p '{"spec":{"components":{"racer":{"enabled":true}}}}'
```

The operator installs the `racer` DaemonSet, the CSI driver, the admission
policies that keep volume geometry immutable, and creates a default
StorageClass named `racer` the first time racer is enabled.

## 2. Node Membership

There is nothing to enroll. Every node in a Site that enables racer runs the
agent, and a node becomes a member of the racer cluster once that agent starts:
it announces itself with `racer.unbounded-cloud.io/agent`, which it can only
write after `preflight` has accepted the node, and the operator allocates
identity to nodes that have announced. A node whose store or kernel is not ready
never announces, so it never enters a catalog.

The operator stamps the identity it allocated onto the Node object:

```bash
kubectl get node dc2-worker-1 -o jsonpath='{.metadata.annotations}' | tr ',' '\n' | grep racer
```

| Annotation | Meaning |
|---|---|
| `racer.unbounded-cloud.io/node-id` | The node's racer id. Unique and never reused |
| `racer.unbounded-cloud.io/zone` | The racer zone the node belongs to: one catalog, one failure domain. Not the Kubernetes topology zone |
| `racer.unbounded-cloud.io/cohort` | Which of the three replicas of a trio this node can hold |
| `racer.unbounded-cloud.io/agent` | Written by the node agent itself. Its presence is what makes the node eligible for an identity |

Placement is automatic. It reads the node's `topology.kubernetes.io/zone` label
and its site, and spreads a zone's cohorts across availability zones so that a
trio takes one node from each; a zone never spans two sites. A node that
declares no availability zone still gets placed, but loses the guarantee that
its trios span three of them, so label nodes with their real failure domain
before racer reaches them. A zone needs members in all three cohorts before it
can serve a replicated volume.

To pin a node to a specific racer zone instead, set
`racer.unbounded-cloud.io/zone-name` on the Node before the operator places it.
A named zone always wins over automatic placement.

To keep racer off one node in an otherwise racer-enabled Site, label it out:

```bash
kubectl label node dc2-worker-1 racer.unbounded-cloud.io/enabled=false
```

Only the exact value `false` excludes a node. A leftover
`racer.unbounded-cloud.io/enabled=true` from an earlier release does nothing and
can be removed.

The operator adds its own `racer.unbounded-cloud.io/active` label, which is what
the DaemonSet selects on. It tracks the nodes racer belongs on plus the nodes
that still hold data, which is why it lags a decommission. Do not set or remove
it by hand.

## 3. Create a Volume

Racer runs the CSI Node service only, so there is no dynamic provisioner.
Volumes are static PersistentVolumes an administrator (or a higher-level
controller) writes, and the operator allocates extents onto them. The
StorageClass uses `WaitForFirstConsumer`, so the scheduler picks the zone.

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: db-0
spec:
  capacity:
    storage: 64Gi
  accessModes:
    - ReadWriteOnce
  volumeMode: Block
  storageClassName: racer
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: racer.unbounded-cloud.io
    volumeHandle: db-0
    volumeAttributes:
      mutableBytes: 8Gi
      mutableKind: LWW
```

The volume attributes are the volume's geometry, and geometry is frozen at
creation. An admission policy rejects updates to them:

| Attribute | Default | Meaning |
|---|---|---|
| `mutableBytes` | `0` | Size of the mutable head. The remainder of the capacity is an immutable tail |
| `mutableKind` | `LWW` | `LWW` (last writer wins) or `OCC` (optimistic concurrency) |
| `immutablePageSize` | `4Mi` | `4Ki` or `4Mi`. 4 MiB pages are cheaper per byte stored |

The mutable head sits at device offset zero and the immutable tail runs from
there to the end of the device, so both `mutableBytes` and the remaining
capacity must be a multiple of `immutablePageSize`. A layout that would leave
the tail's pages straddling a boundary is rejected rather than rounded.

Volumes are raw block devices; `volumeMode: Block` is required on both the
PersistentVolume and the claim that binds to it.

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: db-0
spec:
  accessModes:
    - ReadWriteOnce
  volumeMode: Block
  storageClassName: racer
  volumeName: db-0
  resources:
    requests:
      storage: 64Gi
```

Capacity cannot be changed after the fact: the StorageClass sets
`allowVolumeExpansion: false` because an extent's size is fixed by the address
space it was allocated in.

## 4. Share a Volume Across Many Pods

Nothing binds an extent to one node. The same volume can be exported from every
node in the cluster at once, and racer's per-page consensus keeps the media
coherent while it is. Declare that by asking for `ReadWriteMany` on both the
PersistentVolume and the claim:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: shared-0
spec:
  accessModes:
    - ReadWriteMany
  volumeMode: Block
  storageClassName: racer
  volumeName: shared-0
  resources:
    requests:
      storage: 64Gi
```

A claim is not a per-pod object. Any number of pods may reference one, and the
CSIDriver sets `attachRequired: false`, so there are no VolumeAttachment objects
either. A DaemonSet spanning ten thousand nodes therefore costs one
PersistentVolume and one claim, not ten thousand of each. Per pod the cost is a
single `NodePublishVolume`: a `stat` and a bind mount, entirely node-local.
Nothing on the pod start path talks to the apiserver.

### What you are responsible for

The volume is a raw block device, so there is no filesystem in the middle to
arbitrate between writers, and the driver does not pretend otherwise. Four
things are yours to get right.

**Use `O_DIRECT`.** This is the one that bites. Racer makes the *media*
coherent; the kernel above it is not. Each node keeps its own page cache over
its own `/dev/ublkb<minor>`, and those caches know nothing about each other. A
reader on one node can serve stale bytes indefinitely after another node has
written, with no error and no eventual repair. Open the device with `O_DIRECT`
on every node that shares it.

**Do not expect fencing.** There is no reservation mechanism, no SCSI Persistent
Reservation equivalent, and no way to evict a writer. A partitioned or hung pod
that still holds the device can still write to it.

**Pick the extent kind deliberately.** `mutableKind` is the real concurrency
primitive and it is frozen at creation:

| Kind | Concurrent writers |
|---|---|
| `OCC` | A cluster-wide compare-and-swap. The guard is the version this node last read, and acceptors keep no read state, so the check is genuinely cluster-wide. This is the one to build on |
| `LWW` | A write always lands. Two writers to one page silently lose one of them |
| immutable tail | Write-once per tombstone epoch. Safe by construction, because a second write is refused rather than reordered |

**Do not put an ordinary filesystem on it.** ext4 or XFS mounted read-write from
two nodes will corrupt, quickly, whatever the access mode says. A shared volume
wants an application that understands shared storage, or a filesystem that does.

### First use of a universe is slower

The first volume a node exports from a given universe makes that node join it.
The join publishes a bootstrap configuration and then a full one, and attaches
an NVMe-oF controller for every node in the zone's membership plus every foreign
zone's gateway. That happens inside `NodeStageVolume` and is bounded by
`RACER_STAGE_TIMEOUT` (two minutes by default), so the first pod on a node can
take appreciably longer to start than the rest.

Every later volume in the same universe costs nothing on the wire. On a large
rollout the join is the dominant cost, and it is paid once per node, not once
per pod.

## Checking Status

The operator publishes a `RacerReady` condition on each Site:

```bash
kubectl get site dc2 -o jsonpath='{.status.conditions[?(@.type=="RacerReady")]}'
```

A `Sequencing` reason is not a failure. It means the operator has made a change
and is waiting for the nodes to report that they have applied it before taking
the next step, and it will retry on its own. Membership replacement, extent
migration, tombstone collection and decommission all advance this way, gated on
what the node agents publish rather than on a timer.

## Removing Racer

Take a node out by labelling it out:

```bash
kubectl label node dc2-worker-1 racer.unbounded-cloud.io/enabled=false
```

This starts a decommission rather than stopping the node's pod. The node keeps
running racer while the operator steps it out of each catalog and moves the
groups it holds elsewhere, and only when the node holds nothing does the
operator remove the `active` label and let the pod go. Removing a node's disk
or deleting the node before that completes loses the replicas it held.

Disabling racer on a Site asks the same thing of every node in it at once. A
zone never spans two Sites, so those nodes have only each other to hand their
groups to: the decommission finishes only for what the surviving members of each
zone can absorb, and blocks, visibly, for anything they cannot. Emptying a Site
means moving its volumes off first.

Disabling the component on every Site does not uninstall racer, and does not
decommission anything: with nowhere left to move data to, a decommission could
never finish, so the operator freezes the cluster exactly as it is and keeps
reconciling it. Deleting the DaemonSet, the `racer-allocations` ConfigMap and
the StorageClasses is a deliberate, manual act.
