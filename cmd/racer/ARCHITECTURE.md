# Architecture

Racer is a Linux-only userspace distributed block dataplane. A process exports
one ublk device per configured volume and one sparse ublk "fabric" device used
by peers. It stores authoritative pages in a local file or block device opened
with `O_DIRECT`, replicates page registers through fixed three-member consensus
groups, and can use separate on-device regions as a cooperative read cache.

The process does not establish its peer network. An external control plane
publishes each node's fabric ublk device through NVMe-oF and attaches remote
fabric namespaces as local block-device paths. Racer communicates with a peer
by issuing block I/O to such a path. Its only native network listener is the
metrics HTTP endpoint, bound at `METRICS_ADDR` (`:9090` by default).

## Executables and Configuration

`racer` has one command: `serve` starts the dataplane and watches the binary
configuration for whole-file replacements. At startup it formats a blank
backing device automatically. An existing valid layout is left untouched, and
invalid nonblank superblocks are rejected rather than overwritten.

`racer-bench` contains a checksum/copy benchmark and an end-to-end load driver.
The configuration schema is `proto/config.proto`; `build.rs` generates Prost
types, requiring `protoc` at build time.

The checked configuration describes:

- A generation.
- The local node: id, site, zone, and cohort. Its device — path, cache sizes,
  and rate ceilings — plus peers. A peer may name the foreign site it lives in,
  making that link a site crossing, and may name the sites it will carry traffic
  to on our behalf. Nodes are otherwise alike; there is no gateway role.
- A topology epoch, a balanced catalog of three distinct acceptors per group,
  and remote-zone entries. An address's 16,384-way hash slot folds over the
  catalog to name its group, so the catalog is the whole of the placement map.
- Volumes and ordered extents. A global page address is a 32-bit volume ID plus
  a 32-bit page offset; the volume ID's high byte is its site. A six-bit volume
  slot is used on the fabric. Extents specify LWW, OCC, Immutable, or
  Immutable-4M semantics and their home and next zones. The kind carries the
  page size: 4 MiB exists only as immutable, and a volume's extents must all
  name the same size. Each volume also carries its own tombstone epoch, so one
  volume can reclaim while another lags.
- Runtime and cache policy.

An address hashes to a fixed slot, which names its consensus group. Each node
has complete group information for its zone and entry nodes for other zones,
rather than a global connection graph. Reaching another site takes only a peer
that says it can, so no node holds the far site's shape.

The nodes of a zone are homogeneous. The catalog must give every node it names
the same number of groups, so every node holds the same share of the zone and
sizes its device for that same share: the zone's pages, times three replicas,
divided by the number of nodes. There is no way to declare one node larger than
another. A node the catalog does not name holds nothing; it is either a spare
about to join, or a member being decommissioned, or a node that only routes.

The watcher uses inotify on the configuration's parent directory and reacts to
close-write and rename-into-place. Generations must increase; a volume's
tombstone epoch may not decrease, though it may jump by any amount; existing
volume slots and extent shapes cannot change; the catalog keeps its length for
the life of the zone, since that length is what folds a slot onto a group; and
catalog membership moves one node at a time, as migration changes do. Parse,
validation, and build failures leave the previous runtime
configuration active and increment a metric. Reconciliation can still fail
after publication and partially apply a generation.

## Process and Runtime

`runtime` is a process singleton with one `racer-ctl` control thread and one
`racer-wN` worker per physical core selected from the process affinity mask.
Workers attempt to pin themselves; affinity failures are ignored. SMT siblings
are folded, and the control thread attempts to use the first worker's sibling.
The main thread waits for signals, `racer-cfg` watches the configuration, and a
`racer-metrics` thread serves metrics. Initial allocator scanning
temporarily uses additional threads.

The control thread owns resource reconciliation, worker broadcasts, and all
live configuration versions. Each worker owns:

- An io_uring with registered files and buffers.
- A scratch-buffer pool and async-operation slab.
- A cooperative ready queue and fixed future slot for every ublk queue/tag.
- Its ublk queues and tag state.
- Its current configuration pointer and per-version guard counts.
- Cross-core request, task, and result slots.

The guards delay reclamation of resources referenced by old `Dataplane`s. They
do not snapshot subsystem configuration: process-lifetime allocator, consensus,
and cache objects have independently replaced live views.

Allocator, consensus, cache, and healing state is split into arrays indexed by
worker. Mutable hot-path state is worker-local (`Cell`/`RefCell`), not protected
by shared locks. `runtime::on_core` moves inline closures through an N-by-N
matrix of SPSC rings. Full rings spill to a local outbox; io_uring `MSG_RING`
wakes a sleeping destination. Dropping the result future does not prevent the
destination closure from running.

Workers reap completions, run cross-core and ready work, process control and
periodic work, flush submissions/completions, then spin, yield, or park.
Detached maintenance uses the same cooperative executor. A worker panic aborts
the process.

Kernel I/O is not cancelled when its Rust future is dropped. Its operation slot
and owned pool buffer remain detached until all completions arrive. An opaque
ublk guest buffer cannot be retained that way, because it belongs to the kernel
for one request, so operations naming one are counted instead and its request
is not answered while the count stands. Quorum operations drop unresolved legs
after deciding an outcome, except where a leg carries a guest buffer: those are
settled, on a shorter deadline, so an abandoned leg can never read pages the
kernel has already rebound to another request.

## ublk and Request Handling

The runtime has 60 exported-device slots, one of which the fabric consumes, so
at most 59 volumes are usable even though validation accepts 60. Queues are
distributed across physical-core workers. For each request a worker fetches the
ublk descriptor, invokes `Server::handle`, commits its result, and rearms the
tag. At high operation-slab occupancy it delays completion and tag reuse,
applying block-layer backpressure. The sparse fabric device has a logical size
of 4 EiB to provide an LBA address space for protocol frames.

Volume geometry prevents a request from crossing an allocator page:

- A 4 KiB request is copied between the ublk guest buffer and a registered
  pool buffer so Racer can checksum it.
- A 4 MiB request uses its registered guest buffer directly. Writes must cover
  exactly one page. Reads may be partial, including remote and local-cache
  reads; remote admission and repair still transfer whole pages, though a
  transport may deliver one in pieces.
- Reads of holes return zeroes. Huge holes are filled from a format-time zero
  region because opaque guest buffers cannot be cleared directly.
- Discard becomes a consensus trim.

Fabric requests enter through device key zero. Their LBA is decoded, validated,
routed if necessary, and dispatched to consensus, allocator, cache, snapshot,
healing, migration, or liveness operations.

## Persistent Layout and Allocator

On the first `serve` of a blank device, formatting fixes all offsets and
capacities. Four CRC32C-protected 4 KiB superblocks contain geometry plus
bounded consensus promises and migration seals. The remaining regions are:

1. A 4 MiB zero page.
2. A/B copies of 4 KiB-page metadata.
3. A/B copies of 4 MiB-page metadata.
4. Out-of-place small and huge data slabs, with the huge slab aligned.
5. Separate fixed-size small and huge cache regions.

Capacity includes spare data slots and is checked whenever `serve` starts. The
share a node is sized for is the zone's mean rather than a declared ceiling, so
the overprovision above it, five percent plus a per-class floor, is also what
absorbs the variance in how many pages actually hash into the groups a node
holds. A configuration that has outgrown the device is satisfied by appending an
extent per class: a fresh run of metadata blocks and the data slots they name,
placed past the end of everything already written, recorded in a growth table in
the superblock, and never moving a byte that already exists. Regular files are
extended in place; a block device must be enlarged by the operator first, and
`serve` refuses to start naming the shortfall. Growth happens only at startup,
before shards are sized; a reload that asks for more publishes the shortfall as
`racer_alloc_unbacked_pages` and runs short until the next restart.

A metadata block is held wholly in memory and alternately
rewritten to its A/B
locations. Entries persist address, version, ballot, state, flags, and tombstone
epoch; small entries also persist a seeded data CRC. On restart, the valid copy
with the highest generation wins. Two bad copies quarantine the whole metadata
block. Parallel startup scans rebuild all indices and free lists without a
journal; duplicate addresses are resolved by register order.

Metadata blocks are striped across per-core allocator shards. A write:

1. Reserves an out-of-place slot on the address-owning core under a register
   guard.
2. Writes page data durably with `RWF_DSYNC` on the request core.
3. Stages the new entry on the owner core.
4. Group-commits the containing metadata block.
5. Releases the displaced slot only after metadata is durable.

The proposer-local consensus leg remains pending until a quorum succeeds; a
failed round abandons its reservation without advancing the local register.
Snapshot cursors defer reuse of referenced slots. Allocator watermarks throttle
writes and suppress cache admission and healing.

Small authoritative reads verify CRC32C seeded by address and version. Huge
pages have no data checksum. Mutable trim releases storage. Immutable versions
encode unwritten/live/tombstone within the volume's tombstone epoch, so an
immutable trim remains persisted until epoch reclamation.

## Consensus and Page Semantics

`paxos` implements a CASPaxos-like register per page. A register is
`(version, ballot)`; a ballot packs a 30-bit term and a two-bit member index.
Normal groups have three members and require two responses. A configuration
with no peer links is treated as single-node mode and requires one.

LWW writes read/guard the current version and retry bounded conflicts. OCC
writes require a bounded, volatile prior-read record on the client-facing
node. Immutable writes derive their version from the volume's tombstone epoch.
Requests
originating on a nonmember are handed to a group member.

A write concurrently sends guarded accepts to members, including a pending
local allocation. Quorum commits the local leg; rejection or ambiguity can
trigger prepare, term advancement, and repair. Ordinary reads fetch local data
before gathering peer metadata; cache and huge-page paths can overlap those
steps. Two exact matching registers identify a chosen value; otherwise Racer
fetches the matching value or repairs the group. A local authoritative 4 MiB
immutable hit can avoid a read round.

Terms, in-flight keys, and replay state are owned by the group's worker; seals
are copied into every worker. Explicit promise and seal snapshots rewrite all
superblock copies, but records beyond 128 terms or 96 seals are silently
omitted, and an observed higher ballot updates its promise only in memory. Wipe
detection is reactive: a node can participate after restart until healing
compares its empty state with a peer. Once marked for replay it cannot propose
or count toward accept quorum until anti-entropy restores pages and promises;
rejoin requires direct links to both peers. Apply-if-newer learning and extent
sealing support repair and migration.

## Fabric and Routing

A `fabric::Link` is an `O_DIRECT` local block-device handle plus a peer ID. Each
operation has a linked two-second timeout, or a quarter-second one when it puts
a 4 MiB guest buffer on the wire. The protocol encodes RPC metadata in
the LBA rather than a packet header: opcode, hop/cache flags, member addressee,
six-bit volume slot, and page offset. Small frames separate payload and trailer;
huge frames reserve a contiguous 4 MiB payload range. A transport splits a huge
command at its transfer limit, so the target sees consecutive pieces of one
frame. Partial huge reads are served by offset; huge writes reserve a slot on
the first piece, land each piece in it from the core that received it, and only
become an entry once every block is durable, because a huge page carries no
checksum to detect a half-written one.

Read-like commands return payload and/or trailer; imperative commands are
durable block writes. Operations cover get and metadata, prepare, accept, trim,
learn, seal, Merkle digest, snapshot open/next, term, and ping. Decoding and
dispatch validate range, alignment, transfer shape, direction, page class, and
address. NVMe status preserves only stale (`EREMOTEIO`), absent (`ENODATA`),
unsupported (`EOPNOTSUPP`), and space (`ENOSPC`); other failures collapse to
transport `EIO`.

Local-zone traffic resolves directly or through another member. Cross-zone
traffic selects one of three entry nodes by address. Cross-site traffic takes
our own crossing when we hold one, and is otherwise handed to a peer that does,
picked by address so the load spreads. A frame normally has a two-forwarding-hop
budget; the single site crossing leaves it at three, so the far site funds its
own hops. Relays reuse the same registered data buffer.

## Cooperative Cache

The cache is advisory and divided per core. A periodically halved count-min
sketch estimates popularity. A configured target request rate determines a
bounded replica width; rendezvous ranking selects nested replicas without a
directory. TinyLFU-style admission and CLOCK replacement manage fixed
on-device slots.

Writes are in-place and non-durable, and cache metadata is not recovered after
restart. A hit carries its claimed register, which consensus metadata must
confirm; stale entries are discarded. Huge immutable admission also requires a
quorum-confirmed version. Cache bytes have no checksum, so register confirmation
establishes identity and freshness but cannot detect silent cache-media
corruption. A zero target rate disables caching.

## Healing and Migration

Under normal allocator pressure each core starts at most one detached healing
sweep per second. For each group and page class, peers compare 512-bucket
non-cryptographic XOR digests. Differing buckets are enumerated through bounded
snapshot cursors with a 30-second idle expiry; cursors return only address and
register, and page bytes are reconciled through ordinary Paxos repair. Cursor
count, buckets, and repairs are budgeted, with larger budgets during replay.

A node whose digests are empty where its peers' are not is replaying, and is
excluded from quorum until it has caught up. That covers both a member wiped and
restarted under its own id and a node the catalog has just named for the first
time. Membership moves by replacement, one node at a time: the joining node
inherits the departing node's groups and replays them, and the departing node,
which the new configuration no longer names, walks what it still holds, confirms
the new members have each version, then drops its registers and frees the slots.
Since the member count is unchanged, no node's share moves. There is no way to
give one node more of the zone than another, so there is nothing else to
rebalance.

For migration, a source asks directly linked catalog nodes to seal, ignores
failed sends, persists its own seal, then repeatedly walks the extent and sends
apply-if-newer learns toward the target zone. There is no quorum barrier proving
all source acceptors sealed and no durable gate proving the destination drained
the source. The external control plane completes handover by publishing a
configuration that changes the home zone and clears the next zone.

## Reload and Shutdown

`Node::attach` declaratively builds a dataplane: backing disk, volume devices,
fabric device, peer links, and process-lifetime allocator/Paxos/cache/healing
objects. Reconciliation registers new fixed files on all workers, stops and
drains removed devices, publishes a new raw configuration pointer, starts new
ublk devices, waits for every old per-worker configuration guard to drain, then
unregisters and deletes unreferenced resources. Stable paths and volume keys
reuse handles and ublk identities; an existing volume cannot change size.

Subsystem link/topology views are installed during dataplane construction,
before the worker configuration-pointer broadcast. Old guarded requests can
therefore observe new subsystem views, and reload has no single atomic cutover
across both layers. Build failures are rolled back, but failures starting
devices after publication are not. The first backing store and format geometry
remain attached to the process-lifetime allocator across reloads.

On SIGINT or SIGTERM, the control thread stops all ublk devices, drains queues
and old configurations, broadcasts worker shutdown, deletes devices, and joins
control and worker threads. Watcher/metrics threads and domain objects are
process-lifetime and are not explicitly joined or reclaimed.

## Observability, Trust, and Limits

Workers periodically publish per-core counters to atomic rows; scrapes sum the
rows. The metrics server is blocking, handles one HTTP/1.1 connection at a time,
and exposes unauthenticated plaintext `GET /metrics` with five-second
socket timeouts.

Racer trusts the local configuration, external NVMe-oF control plane, Linux
ublk/io_uring implementation, direct-I/O alignment, and device durability
semantics. Configuration is unsigned and can select writable device paths. The
fabric has no authentication, authorization, encryption, peer identity, or
replay protection; access to its namespace permits consensus and control
operations. Anti-entropy digests are collision-prone hints, not integrity
proofs.

Important implementation boundaries are:

- Linux must provide `/dev/ublk-control`, the required ublk features, io_uring
  fixed files/buffers, linked timeouts, `MSG_RING`, deferred task running,
  `O_DIRECT`, `RWF_DSYNC`, inotify, affinity, and SMT topology information.
- `lease_ms`, `clock_drift_ms`, and configured SMT policy are parsed but do not
  affect the dataplane. Placement epochs are reload checks, not request-routing
  guards. Topology epoch accompanies small writes but is not enforced by
  receivers.
- Reload validation does not make all identity, peer, topology-epoch, or hash
  slot changes immutable. A reload that outgrows the device is accepted and runs
  short until a restart grows it.
- Empty peer links select quorum one. Promise changes can remain only in memory,
  and observed higher ballot terms are not persisted before a crash.
- Huge authoritative and all cached data lack data checksums.

## Verification Architecture

The `sim` feature routes production disk submission and runtime sleep/clock
paths through an in-memory deterministic event queue while retaining request
handling, allocator, Paxos, cache, healing, hop rings, operation slabs, and
worker stepping. It does not run production threads or ublk. Worker shells
still construct io_uring instances, and a few subsystem paths use host time.

`tests/dst.rs` drives this stack through crashes, restarts, partitions, dropped
frames, disk errors, corruption, straggling links, transport-split huge writes,
and seeded linearizability checks.
`tests/paxos_model.rs` model-checks abstract consensus, replay, handover, and
anti-entropy; `alloc/shard.rs` model-checks the actual allocator transitions.
Privileged integration tests exercise real ublk with multiple processes,
reload, intra-zone routing, cache, restart, and wiped-member replay, but skip
when kernel facilities are absent. Cross-zone/site routing, production
migration, NVMe status translation, and real NVMe-oF are not
covered end to end.
