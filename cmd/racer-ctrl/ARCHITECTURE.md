# Architecture

`racer-ctrl` is the node half of the racer control plane. It runs as a sidecar
next to the `racer` dataplane in the same pod.

racer is a dataplane and nothing else. It has no discovery, no membership
protocol, no NVMe-oF and no Kubernetes. Everything it does is a function of one
config file it is handed, and the only thing it reports is a Prometheus
endpoint. racer-ctrl writes that file, manages the fabric racer refuses to know
about, republishes racer's metrics so the operator can act on them, and serves
the CSI node service.

Code lives in `internal/racerctrl` (pure logic, shared with the operator),
`internal/racerctrl/node` (the node agent), and `internal/racerctrl/csi` (the
CSI driver). `cmd/racer-ctrl` is the process.

## Division of labour

There are exactly two writers of racer state, and no value has two.

The unbounded-operator owns everything cluster-scoped: node ids, cohorts and
zones; universe ids, epochs, catalog size and gateway lists; extent allocation
and volume placement; per-zone catalog membership. racer-ctrl owns exactly one
file (this node's config) and five annotations (this node's own status). It
never allocates a cluster-scoped identifier and never writes another node's
object.

There are no CRDs. A StorageClass whose provisioner is
`racer.unbounded-cloud.io` is a universe, a PersistentVolume naming that driver
is a volume, and a Node's annotations are its identity and its status. Catalog
membership is the one thing too large for an annotation, so it lives in one
ConfigMap per universe per zone, carrying the topology epoch it was published
at so a catalog and its epoch can never be read apart.

The operator finalizes every universe StorageClass. Deleting one freezes new
volume placement and membership changes but keeps the class and its membership
readable while its existing volumes are retired. Once no volumes remain, the
operator removes the membership ConfigMaps and releases the class finalizer.
If a class is removed without that protection, deleting orphan PVs have their
unusable collection finalizer released and the operator reports the data-loss
condition rather than leaving the PVs stuck forever.

The node writes `store-bytes`, `devices`, `fabric`, `health`, `live` and
`applied` on its own Node object. It reads everything else.

## Process

No flags. Everything is an environment variable so the DaemonSet manifest is
the single place a knob is set: `NODE_NAME` (required), `POD_NAMESPACE`,
`RACER_CONFIG_DIR` (default `/etc/racer`), `RACER_STORE`, `RACER_METRICS_URL`,
`CSI_ENDPOINT`, `RACER_NVMET_ROOT`, `RACER_FABRIC_ADDR`, `RACER_FABRIC_PORT`,
`RACER_RDMA_PORT`, `RACER_NQN_PREFIX`, `RACER_STAGE_TIMEOUT`,
`RACER_SKIP_PREFLIGHT`, `RACER_LOG_LEVEL`, `RACER_DEVICE_ID_BASE`,
`RACER_IMAGE_DEVICES`.

`RACER_DEVICE_ID_BASE` moves the floor of the ublk minor window this node
allocates from. It exists because minors are global to the kernel, not to the
container: one node is normally one kernel and the default floor of 1 is right,
but when several agents share a kernel a fixed floor puts all of them on the
same first minor. Setting it to a number picks that floor explicitly. Setting
it to `auto` derives the floor from the node id the operator allocated, which
is unique cluster-wide, so agents that share a kernel never contend. `auto` is
a testing arrangement rather than a production one: a derived window for a node
id in the thousands runs past what the driver accepts.

Two subcommands. `preflight` checks host prerequisites and exits; it is a
separate entry point so it can run as an init container, where a node that
cannot run racer says so once and loudly instead of crash-looping later. `run`
starts the agent and the CSI server as two goroutines that share a context and
a fate: a CSI server with no agent would accept stages it can never satisfy,
and an agent with no CSI server would render configs no pod can reach.

Preflight checks that `/dev/ublk-control` opens, that `ublks_max` is at least
256 (racer's export budget; the module default of 64 fails at the worst
moment), that the store's filesystem honours `O_DIRECT` and `RWF_DSYNC`, and,
when a fabric address is set, that nvmet configfs is mounted.

## Reconcile loop

Informers watch Nodes, PersistentVolumes and StorageClasses cluster-wide.
Membership ConfigMaps are watched separately, narrowed to the operator's
namespace and this node's zone label, and started lazily because the zone is
only known once the operator has stamped it. Every event triggers a reconcile,
coalesced by a 200 ms debounce and floored at one per second.

A reconcile folds the cached objects into a `ClusterState`, finds this node's
identity in it, assigns fabric minors, drives the fabric, derives and installs
the config, and patches any status annotation that changed. Identity fields are
re-read from the Node every pass; status fields come from memory, because a
volume staged a millisecond ago has not reached the informer cache yet and
reading it back would un-export a device the kubelet is waiting on.

Objects that are not ready (a Node with no id, a class with no universe id, a
volume with no composition) are skipped, not rejected: they are normal
intermediate states, and refusing to render would stall the zone behind the
slowest object. A failed reconcile leaves the previously published config in
place.

## Derivation

`racerctrl.Derive` is a pure function: cluster state and this node's identity
in, one `NodeConfig` out, no I/O and no clock. Every node runs the same code
over the same annotations, so all of them reach the same answer without talking
to each other. Determinism is load-bearing rather than tidy, so every list is
sorted before it is emitted; two nodes disagreeing about a catalog would be
indistinguishable from a split brain.

A node joins a universe if its zone's catalog names it, if its zone's draining
list names it, or if it exports one of the universe's volumes and so has to
route for it. The draining case is the one that is not obvious: a node the
catalog has just stopped naming has to keep deriving that universe, with itself
absent from the catalog, because that is the configuration that tells racer the
groups it holds are no longer its own. Stop deriving it and the node keeps the
configuration that still names it, sheds nothing, and holds its registers until
the process ends.

For each joined universe the derivation emits: the catalog, which is the zone's
published catalog when there is one and otherwise one built from the zone's
membership by a rotation that keeps cohorts balanced; the epoch that catalog was
published at, taken from the same object so a freshly read catalog can never be
paired with an epoch bumped for another zone's change; the other zones with
their gateways (its own zone is absent, since racer reaches that through the
catalog); the peers it holds fabric attachments to; and the extents it either
stores (homed in its zone, or migrating into it) or routes for (behind a device
it exports), sorted by `base_lba` because racer resolves addresses by binary
search. Devices map each local ublk minor to its volume's extent ids. Store size
is computed from the node's share of the zone's pages using layout constants
transcribed from `cmd/racer/src/layout.rs`, and `Policy.max_index_bytes` is
sized to the pages actually carried; leaving it at the schema default would be a
silent cap.

**Bootstrap.** A peer link is an NVMe-oF namespace backed by
`/dev/ublkb<fabric_device_id>`, and racer only creates that device once it has
accepted a config naming the universe, so a first config demanding peers could
never be accepted. The first config for a universe therefore carries only its
id, epoch, catalog and fabric device id: it holds no data and answers no reads,
and exists only to bring the fabric device up. Once attachments land the next
derivation publishes the universe in full. A universe that has once been
published in full is never demoted back; it renders degraded when a peer drops,
which is what racer expects of a live group.

## Publication

The config is marshalled deterministically and, if byte-identical to what is
already installed, dropped. Otherwise it is validated twice before anything
touches the filesystem: `Validate` is a close port of racer's own `validate()`,
so a config the dataplane would reject never reaches the watched directory, and
`ValidateTransition` checks the rules that span two generations (generation
strictly increases, node id and cohort never change, store size never shrinks,
extent geometry is frozen, membership moves by at most one node per slot).

Installation is a temp file in the destination directory, fsync, chmod 0600,
then `rename(2)`. racer watches the parent directory and reloads on any event
touching the file, so a partial write is a config racer will read.

The generation is adopted from the file left by an earlier run, because racer
may not have restarted with the agent and would reject a counter that started
over. The facts that generation carried are adopted with it, so a restarted
agent restates what it has installed rather than reporting that it has installed
nothing.

Device bindings are adopted too, from a `bindings.json` written next to the
config. The ublk minor a volume is exported on is the only piece of node state
that is not derivable from the cluster: the config records that device 4 carries
extents 9 and 10, but not that device 4 is `pv-abc`. Keeping that only in memory
would mean a racer-ctrl restart publishes a config with no devices and takes
exports away from pods that are still writing, on a node where racer itself
never restarted; the kubelet only replays `NodeStageVolume` after a *node*
restart, which is both the wrong trigger and too late. The file sits in the same
emptyDir as the config, whose lifetime is exactly right - as long as the pod,
which is as long as racer's exports - and racer's watch matches on the config
file's name, so a sibling file does not provoke a reload. Bindings restored this
way are pruned once, on the first reconcile, against the volumes the cluster
actually has.

Every publication advances the generation by one. Membership moves one group
slot at a time and a group keeps quorum across a generation, so there is no
longer any change too wide to publish as a single legal transition.

**What a generation carried.** racer reports only which generation is in force,
never what is in it. Every sequenced operation, though, waits on a fact the
control plane put in a configuration: a catalog at an epoch, an extent pointed
at a new zone, a tombstone epoch advanced. So the agent reads those facts back
out of the file it has just installed and publishes them as the `applied`
annotation: generation N carried these epochs and these mid-flight extents. A
sequencer can then tell a node that is acting on a change from one that never
heard of it. Only extents in the middle of an operation are carried; everything
else is at rest and listing it would spend the whole annotation budget saying
nothing.

## Device ids

A device id is a ublk minor, so the path is `/dev/ublkb<id>`. One node-local
space of 1..256 covers both kinds of export: one id per staged volume and one
per joined universe for that universe's fabric namespace. Ids are lowest-free,
since minors are genuinely reclaimed and a reused one refers to nothing
persistent. Making the id the minor is what makes paths stable by construction:
a reload that does not change a binding cannot change a path, so racer's open
file descriptors survive.

## Fabric

Each node publishes one nvmet subsystem per universe it joins, named
`<prefix>.u<universe>.n<node>`, carrying a single namespace backed by that
universe's local fabric ublk device, and attaches the matching namespace from
every peer it must reach: its catalog peers plus every foreign zone's gateways.
The resulting local device path becomes `Peer.device`.

Attachment is the entire security boundary. The fabric offers no
authentication, encryption, peer identity or replay protection, so anything
that can attach a universe's namespace can read and write every page in it.
Every subsystem sets `attr_allow_any_host=0` and carries an explicit
`allowed_hosts` list of exactly that universe's members' host NQNs
(`<prefix>.host.n<node>`), including this node's own, since racer reads its own
fabric device through the fabric like any peer's. Stale entries are revoked in
the same pass that adds new ones.

Every node publishes a TCP port (4420 by default). A node that also declares
`rdma-addr` publishes an RDMA port (4421) and links its subsystems into both.
A retained subsystem is unlinked from any port that is no longer live, so
withdrawing or moving an RDMA address does not leave an old listener behind.
A peer is dialled over RDMA only when both ends declare the same `fabric-id`
and the peer has advertised an RDMA address, which it does only once its port
is actually listening; everything else is TCP. A broken RDMA setup therefore
costs latency, not connectivity.

An initiator controller is matched by host identity, subsystem NQN, transport,
target address and service port. Transport or endpoint changes replace the
controller because the kernel's reconnect parameters are fixed when it is
created; matching by NQN alone would keep retrying an address the peer no longer
serves.

The plan is whole-state: anything published or attached that the plan does not
name is torn down, restricted to subsystems and controllers carrying our NQN
prefix so unrelated NVMe-oF storage on the node is left alone. Reconciliation
is tolerant: a link that fails is logged and omitted, the rest of the plan is
still applied, and the config renders without that peer. Refusing to render
until every link was up would let one unreachable machine stall a zone.

## Metrics

racer has no status file and no API, so the scrape is the only feedback channel
in the system. Every 15 seconds the agent reads racer's loopback Prometheus
endpoint (unauthenticated plaintext, one connection at a time, hence keep-alives
off and an interval in seconds), parses it with a small hand-rolled parser that
skips anything it does not recognise, and digests it into node-wide health
(loaded generation, rejected configs, groups replaying or shedding, unbacked
pages, unavailable groups, pressured cores, gateway fallbacks) and per-extent
live pages and tombstones. Those are published as the `health` and `live`
annotations, which is what the operator's sequenced operations are gated on.

A failed scrape leaves the previous values in place, which is the safe
direction: stale nonzero values block a destructive sequence, they never
unblock one. A change in the digest triggers a reconcile.

## CSI

Identity and Node services only. There is no Controller service and no
external-provisioner: a racer volume's extents are allocated out of a
cluster-wide space and replicated by a catalog whose membership is a sequenced
operation, all of which belongs to the operator, so a `CreateVolume` RPC could
only forward and wait. The operator stamps the allocation onto the
PersistentVolume, and the node service does the one thing that must happen on
the node.

Volumes are raw block only: no filesystem mode, no mkfs, no mount-utils.
`NodeStageVolume` assigns a minor, triggers a reconcile, and polls for
`/dev/ublkb<minor>` to appear within `RACER_STAGE_TIMEOUT` (2 minutes by
default); nothing is written at the staging target path, because for a raw
block volume the kubelet creates that path as a directory before it calls us.
`NodePublishVolume` bind-mounts the device onto the kubelet's target, which
keeps the pod's view to one device node rather than `/dev`. Unstage drops the
binding and lets the next render withdraw the export.

`STAGE_UNSTAGE_VOLUME` is the only capability. `EXPAND_VOLUME` is absent
because an extent's size is frozen for its life: the address space around it is
already allocated and the slot hash that picks a page's group is a function of
the address. `MaxVolumesPerNode` is half the 256-export budget, leaving room for
fabric devices. The node reports its zone as topology
`racer.unbounded-cloud.io/zone` so the scheduler keeps pods near their data, and
`Probe` reports ready once the operator has assigned this node an id.
`NodeGetVolumeStats` is unimplemented; racer's own metrics say far more.

### Access modes

Every access mode is accepted, multi-node included. Nothing binds an extent to
one device or one node, a node joins a universe by exporting one of its volumes,
and per-page consensus keeps the media coherent however many nodes are mapping
it. The usual reason to refuse `MULTI_NODE_MULTI_WRITER` is a filesystem that
assumes it is the only writer, and there is no filesystem here: the pod gets the
device.

What the driver cannot supply, and therefore documents rather than pretends to:

- **Page cache coherence.** Racer makes the media coherent. The kernel above it
  does not participate: each node has its own page cache over its own
  `/dev/ublkb<minor>`, so a reader can serve stale bytes indefinitely after
  another node writes, with no error and no eventual repair. Shared consumers
  must use `O_DIRECT`.
- **Fencing.** There are no reservations, no SCSI Persistent Reservation
  equivalent, and no way to evict a writer that still holds the device.
- **Write arbitration.** That is the extent kind's job, and the kind is frozen at
  creation. `OCC` is a cluster-wide compare-and-swap and is the primitive worth
  building on; `LWW` lands every write and silently loses one of two racing
  writers; an immutable extent is write-once per tombstone epoch and refuses the
  second write rather than reordering it.

The scale consequence is the point. A claim is not a per-pod object, and
`attachRequired: false` means there are no VolumeAttachment objects either, so a
DaemonSet spanning a very large cluster costs one PersistentVolume and one
claim. Per pod the cost is a single `NodePublishVolume`, which is a `stat` and a
bind mount. Nothing on the pod start path talks to the apiserver.

The first volume a node exports from a universe is the expensive one: joining
publishes a bootstrap configuration and then a full one, and attaches an NVMe-oF
controller per node in the zone's membership plus every foreign zone's gateway,
all inside `NodeStageVolume`. Later volumes in that universe cost nothing on the
wire, so the join is paid once per node rather than once per pod.

### Concurrency

Stage, unstage, publish and unpublish take a per-volume lock, not a driver-wide
one. The distinction matters because `NodeStageVolume` waits for a device to
appear and that wait is bounded by `RACER_STAGE_TIMEOUT` rather than by anything
the caller controls; under a single mutex one slow stage would park every other
call on the node behind it, including the per-pod publishes of volumes that were
already staged. The agent already serialises its own binding table and
`AssignDeviceID` is idempotent, so what the driver still needs to guard is the
publish sequence against concurrent kubelet retries, and that is per volume by
nature. Lock entries are reference-counted and dropped when the last holder
leaves, so a node that has churned through many volumes does not accumulate them.

## Deployment

One DaemonSet (`deploy/racer/`) with a `preflight` init container, and
`racer-ctrl`, `racer` and the node-driver-registrar as containers. All are
privileged and share `/dev`, `/sys`, configfs and the store hostPath. The
config directory is an emptyDir shared between racer-ctrl (writer) and racer
(reader), deliberately not a hostPath: the config is a pure function of cluster
state and this node's identity, so there is nothing worth surviving the pod.
