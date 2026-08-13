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
on, and it holds data. It is disabled by default, enrollment is per node, and
the operator never uninstalls it automatically. See
[Removing racer](#removing-racer).
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
component: enabling it on any Site installs it, and individual nodes then opt in
with the enrollment label. `site init` sets the flag on the remote Site because
that is where racer's nodes live, the same as `--enable-storage`.

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

## 2. Enroll Nodes

Enabling the component does not put racer on any node. A node opts in with a
single label:

```bash
kubectl label node dc2-worker-1 racer.unbounded-cloud.io/enabled=true
```

The operator then allocates that node's identity and stamps it onto the Node
object:

```bash
kubectl get node dc2-worker-1 -o jsonpath='{.metadata.annotations}' | tr ',' '\n' | grep racer
```

| Annotation | Meaning |
|---|---|
| `racer.unbounded-cloud.io/node-id` | The node's racer id. Unique and never reused |
| `racer.unbounded-cloud.io/zone` | The racer zone the node belongs to: one catalog, one failure domain. Not the Kubernetes topology zone |
| `racer.unbounded-cloud.io/cohort` | Which of the three replicas of a trio this node can hold |

Placement is automatic. It reads the node's `topology.kubernetes.io/zone` label
and its site, and spreads a zone's cohorts across availability zones so that a
trio takes one node from each; a zone never spans two sites. A node that
declares no availability zone still gets placed, but loses the guarantee that
its trios span three of them, so label nodes with their real failure domain
before enrolling them. A zone needs members in all three cohorts before it can
serve a replicated volume.

To pin a node to a specific racer zone instead, set
`racer.unbounded-cloud.io/zone-name` on the Node before enrolling it. A named
zone always wins over automatic placement.

The operator adds its own `racer.unbounded-cloud.io/active` label, which is what
the DaemonSet selects on. Do not set or remove it by hand.

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

Un-enroll a node by removing the enrollment label:

```bash
kubectl label node dc2-worker-1 racer.unbounded-cloud.io/enabled-
```

This starts a decommission rather than stopping the node's pod. The node keeps
running racer while the operator steps it out of each catalog and moves the
groups it holds elsewhere, and only when the node holds nothing does the
operator remove the `active` label and let the pod go. Removing a node's disk
or deleting the node before that completes loses the replicas it held.

Disabling the component on every Site does not uninstall racer either, for the
same reason: the operator keeps reconciling an existing installation so that
turning the flag off cannot strand data. Deleting the DaemonSet, the
`racer-allocations` ConfigMap and the StorageClasses is a deliberate,
manual act.
