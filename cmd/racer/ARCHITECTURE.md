# Architecture

Racer is a Linux-only userspace distributed block dataplane. A process exports
one ublk device per configured block device and one sparse ublk "fabric" device
per universe, used by that universe's peers. It stores authoritative pages in one fixed-length local file opened
with `O_DIRECT`, replicates page registers through fixed three-member consensus
groups, and uses whatever of that file the slabs did not claim as a cooperative
read cache.

The process does not establish its peer network. An external control plane
publishes each node's fabric ublk device through NVMe-oF and attaches remote
fabric namespaces as local block-device paths. Racer communicates with a peer
by issuing block I/O to such a path. Its only native network listener is the
metrics HTTP endpoint, bound at `METRICS_ADDR` (`:9090` by default).

## Executables and Configuration

`racer` has one command: `serve` starts the dataplane and watches the binary
configuration for whole-file replacements. The store is a single file whose path
comes from the environment, `RACER_STORE`, defaulting to
`/var/lib/racer/store.img`; the path belongs to the process rather than to a
generation, so it is deliberately not in the configuration. At startup the file
is created if it is missing, along with any parent directories, and reserved out
to the configured `size_bytes`. A file already longer than that is refused
rather than truncated, so a store never shrinks. A blank store is then formatted
automatically. An existing valid layout is left untouched, and invalid nonblank
superblocks are rejected rather than overwritten.

`racer-bench` contains a checksum/copy benchmark and an end-to-end load driver.
The configuration schema is `proto/config.proto`; `build.rs` generates Prost
types, requiring `protoc` at build time.

The checked configuration describes:

- A generation.
- The local node: id, zone, and cohort, plus its store - size and rate ceilings.
  Nodes are otherwise alike; there is no gateway role.
- Universes. A universe is one flat, sparse address space counted in 4 KiB
  blocks, and everything about placement inside it: its own epoch, its own
  balanced catalog of three distinct acceptors per group, its own remote-zone
  entries, its own peers, and its own extents. A global page address is
  `universe:26 | lba:38`, so a universe spans 1 PiB and universe id 0 is
  reserved to keep a zero address meaning "free slot" on disk. An address's
  16,384-way hash slot folds over that universe's catalog to name its group, so
  a catalog is the whole of the placement map for one universe and says nothing
  about any other.
- Extents. An extent is a range of a universe's address space: a base block, a
  length in pages, LWW, OCC, Immutable, or Immutable-4M semantics, its home and
  next zones, its own tombstone epoch, so one extent can reclaim while another
  lags, and its cache admission policy. The kind carries the page size, and
  4 MiB exists only as immutable, which is why a 4 MiB extent's base is
  1024-block aligned and each of its pages spans 1024 blocks. Extents may not
  overlap, and their ids are unique across every universe the node holds, which
  is what lets a seal name one with a bare 32-bit id.
- Devices. A device is a local ublk block device: an ordered list of whole
  extents, concatenated. A device may not mix the two page sizes, but it is
  otherwise free, so two hosts may map the same extents in different orders and
  combinations and no page's address moves when they do.
- Runtime policy. Cache admission is per extent, not here.

An address hashes to a fixed slot, which names its consensus group within its
own universe. Each node has complete group information for its zone in each
universe it belongs to, and entry nodes for that universe's other zones, rather
than a global connection graph.

A universe is also the security boundary. The control plane publishes one fabric
namespace per universe and attaches it only to that universe's members, so a
node that was never given the namespace cannot address the universe at all.
Nothing on the wire names a universe: the namespace a frame arrives on is the
universe, and the receiver rebuilds every address, group and extent id from its
own side of the link.

The nodes of a zone are homogeneous. Each universe's catalog must give every
node it names the same number of groups, so every node holds the same share of
that universe's zone and sizes its store for the sum of those shares: per
universe, the zone's pages, times three replicas, divided by the number of
nodes. There is no way to declare one node larger than
another. A node the catalog does not name holds nothing; it is either a spare
about to join, or a member being decommissioned, or a node that only routes.

The watcher uses inotify on the configuration's parent directory and reacts to
close-write and rename-into-place. Generations must increase; an extent's
tombstone epoch may not decrease, though it may jump by any amount; the store's
size may rise but never fall; a surviving extent keeps its universe, its base,
its length and its kind, and a surviving device keeps its ordered list of
extents; each universe's catalog keeps its length for the life of the zone,
since that length is what folds a slot onto a group; and catalog membership
moves one node at a time, as migration changes do. Parse, validation, and build failures leave the
previous runtime configuration active and increment a metric. Reconciliation can
still fail after publication and partially apply a generation.

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

The runtime has 256 exported-device slots, shared between the fabric device each
universe consumes and the block devices. Two queues of depth 16 per device give
32 tags each, so the registered request buffers come to 8192 slots and the pool
brings the total to 9306, inside the kernel's 16384 registered-buffer ceiling.
The kernel imposes a second, lower limit of its own: `ublk_drv.ublks_max`
defaults to 64, so exporting more than that needs the parameter raised, and a
failed ADD_DEV names it rather than reporting a bare errno. Queues are
distributed across physical-core workers. For each request a worker fetches the
ublk descriptor, invokes `Server::handle`, commits its result, and rearms the
tag. At high operation-slab occupancy it delays completion and tag reuse,
applying block-layer backpressure. The sparse fabric device has a logical size
of 4 EiB to provide an LBA address space for protocol frames.

A device maps a request to a page by walking its extents, so a request that
lands outside every extent is refused. Extent geometry then prevents a request
from crossing an allocator page:

- A 4 KiB request is copied between the ublk guest buffer and a registered
  pool buffer so Racer can checksum it.
- A 4 MiB request uses its registered guest buffer directly. Writes must cover
  exactly one page. Reads may be partial, including remote and local-cache
  reads; remote admission and repair still transfer whole pages, though a
  transport may deliver one in pieces.
- Reads of holes return zeroes. Huge holes are filled from a format-time zero
  region because opaque guest buffers cannot be cleared directly.
- Discard becomes a consensus trim.

Fabric requests enter through a device key tagged with their universe, which is
the only place a universe is ever named. Their LBA is decoded, validated against
that universe alone, routed if necessary, and dispatched to consensus, allocator, cache, snapshot,
healing, migration, or liveness operations.

## Persistent Layout and Allocator

On the first `serve` of a blank store, formatting fixes all offsets and
capacities. Four CRC32C-protected 4 KiB superblocks contain geometry plus
bounded consensus promises and migration seals. A promise is a universe, a
group index within it, and a term; a seal is an extent id and a term, since
extent ids are unique across universes. The format version is 5, and earlier
versions are refused rather than reinterpreted: nothing in the bytes
distinguishes the layouts, so an older store has to be reformatted. The
remaining regions are:

1. A 4 MiB zero page.
2. A/B copies of 4 KiB-page metadata.
3. A/B copies of 4 MiB-page metadata.
4. Out-of-place small and huge data slabs, with the huge slab aligned.

Everything past the last of those, rounded up to 4 MiB, is the tail, and the tail
is the cache. It is not recorded in the superblock and is not a region in the
same sense as the others. It is derived at every start from where the slabs end
and how long the file is, which is sound only because the cache is volatile:
nothing points at a cache page across a restart, so the cache may sit wherever
the layout is not looking this boot.

Capacity comes from one number, the configured `size_bytes`, and includes spare
data slots. It is checked whenever `serve` starts. The share a node is sized for
is the zone's mean rather than a declared ceiling, so the overprovision above it,
five percent plus a per-class floor, is also what absorbs the variance in how
many pages actually hash into the groups a node holds. A configuration that has
outgrown the layout is satisfied by appending a growth run per class: a fresh run of
metadata blocks and the data slots they name, placed past the end of everything
already written, recorded in a growth table in the superblock, and never moving a
byte that already exists. A growth run therefore lands in what was tail, and the
next start simply derives a shorter one. The file is reserved out to `size_bytes`
first, with
`fallocate` where the file system supports it and a plain extension where it does
not, so the space a growth run lands in is space the store already owns. If the
appended runs would still not fit within `size_bytes`, `serve` refuses to
start and names the shortfall. Growth happens only at startup, before shards are
sized; a reload that asks for more publishes the shortfall as
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
encode unwritten/live/tombstone within the extent's tombstone epoch, so an
immutable trim remains persisted until epoch reclamation.

## Consensus and Page Semantics

`paxos` implements a CASPaxos-like register per page. A register is
`(version, ballot)`; a ballot packs a 30-bit term and a two-bit member index.
Normal groups have three members and require two responses. A configuration
with no peer links is treated as single-node mode and requires one.

LWW writes read/guard the current version and retry bounded conflicts. OCC
writes require a bounded, volatile prior-read record on the client-facing
node. Immutable writes derive their version from the extent's tombstone epoch.
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

A `fabric::Link` is an `O_DIRECT` local block-device handle plus the universe
and peer it reaches; a peer shared by two universes publishes two namespaces and
is two links. Each operation has a linked two-second timeout, or a
quarter-second one when it puts a 4 MiB guest buffer on the wire. The protocol
encodes RPC metadata in the LBA rather than a packet header: opcode, hop/cache
flags, member addressee, and page offset, with the offset spanning the whole of
a universe. Nothing names the universe, because the namespace already does. Small frames separate payload and trailer;
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

Routing has two tiers, both inside one universe. Local-zone traffic resolves
directly or through another member; traffic for another zone selects one of that
zone's three entry nodes by address. A frame has a two-forwarding-hop budget and
never leaves the universe it arrived on. Relays reuse the same registered data
buffer.

## Cooperative Cache

The cache is advisory and divided per core. A periodically halved count-min
sketch estimates popularity, and the estimate drives a bounded replica width;
rendezvous ranking selects nested replicas without a directory. CLOCK
replacement manages the slots the cache currently holds.

Admission is per extent. `Extent.cache_admit` is zero to keep an extent out of
the cache entirely, one to admit on first sight, or a threshold in `2..=15` that
the sketch estimate must reach first. It is decided where the signal is, on the
node computing the width, which for a 4 KiB page is a group member rather than
the node that would do the caching: the nodes that cache a small page are by
construction not in its group, so their own sketches never see the reads that
matter. A width of zero is the veto, and every downstream placement check
already honours it, so the decision reaches the caching node in the reply it was
already going to send.

Admission reserves no capacity and ranks no extents. Priority is global: when
CLOCK finds a candidate victim, the incumbent keeps its slot if the sketch makes
it strictly hotter than the arriving page, and the admission ends rather than
sweeping on. Ties go to the arrival, so a scan churns only entries as cold as
itself. That contest is what lets one extent admit everything without its scan
sweeping the rest of the cache away, and it is what keeps the hottest key first
regardless of which extent it came from. Lowering an extent to zero stops
admission immediately but strands nothing: what is already resident stays until
it loses a contest, which it will as soon as its estimate decays.

Media comes in 4 MiB chunks carved from the tail. A chunk is the unit of
everything: what a class is given, what one class takes from the other, and what
the allocator lends. Each class is striped only over the cores its lookups can
reach, which is the same core mapping the allocator uses, so a chunk placed
anywhere else would be unreachable rather than merely cold. Core zero holds the
chunks no class has taken and once a second moves media toward whichever class
is both evicting and earning more confirmed hits per byte, drawing from that
pool first, then from free 4 MiB slab slots the allocator will lend, then from
the other class. A borrowed slab slot is given back synchronously, on the core
that owns it, as soon as the allocator runs low; the allocator counts a loan as
free, so lending never moves its own watermarks.

The tail is media, not memory, so what bounds the cache is DRAM:
`policy.cache_index_bytes` caps resident slot records at 48 bytes each. That
makes the 4 KiB class a thousand times more expensive per byte of media than the
4 MiB class, and the cap therefore binds the small class first. It is not an
admission check: the cache sizes itself to fit and grows toward the cap rather
than being refused for exceeding it.

Writes are in-place and non-durable, and cache metadata is not recovered after
restart. A hit carries its claimed register, which consensus metadata must
confirm; stale entries are discarded. Huge immutable admission also requires a
quorum-confirmed version. Cache bytes have no checksum, so register confirmation
establishes identity and freshness but cannot detect silent cache-media
corruption. An extent whose `cache_admit` is zero is never cached at all.

## Healing and Migration

Under normal allocator pressure each core starts at most one detached healing
sweep per second, drawn round-robin from every group of every universe the node
belongs to. For each group and page class, peers compare 512-bucket
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

`Node::attach` declaratively builds a dataplane: the backing store, one ublk
device per configured device, one fabric device per universe, one peer link per
universe and peer, and process-lifetime allocator/Paxos/cache/healing objects. Reconciliation registers new fixed files on all workers, stops and
drains removed devices, publishes a new raw configuration pointer, starts new
ublk devices, waits for every old per-worker configuration guard to drain, then
unregisters and deletes unreferenced resources. Stable paths and device keys
reuse handles and ublk identities; an existing device cannot change size, which
is why a device's list of extents is frozen once it exists.

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
rows. Per-extent live-page and tombstone counts are published as
`racer_extent_live_pages` and `racer_extent_tombstones`, labelled by universe
and extent, alongside `racer_universes`, `racer_devices`, and `racer_extents`.
Cache counters carry a `class` label of `small` or `huge`, and
`racer_cache_bytes`, `racer_cache_borrowed_bytes`, `racer_cache_tail_bytes`, and
`racer_cache_unused_bytes` report how the tail is currently divided.
`racer_cache_reject_total` splits refused admissions by `reason`: `policy` for
an extent that asked for a higher threshold than the page has reached, `victim`
for a page that lost the contest to a hotter incumbent. The metrics
server is blocking, handles one HTTP/1.1 connection at a time,
and exposes unauthenticated plaintext `GET /metrics` with five-second
socket timeouts.

Racer trusts the local configuration, external NVMe-oF control plane, Linux
ublk/io_uring implementation, direct-I/O alignment, and the durability semantics
of the file system and device under its store. Configuration is unsigned and can
select writable peer device paths; the store's own path comes from the process
environment rather than from a generation. The
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
  slot changes immutable. It does not prevent an extent from being unmapped and
  its pages orphaned in the store until a restart. A reload that outgrows the store is accepted and runs
  short until a restart grows it.
- Empty peer links select quorum one. Promise changes can remain only in memory,
  and observed higher ballot terms are not persisted before a crash.
- Huge authoritative and all cached data lack data checksums.
- The cache takes whatever the slabs did not, so a store sized close to its
  configuration caches almost nothing, and a growth run at the next start takes
  media back from the cache without warning.

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
when kernel facilities are absent. Cross-zone routing, production
migration, NVMe status translation, and real NVMe-oF are not
covered end to end.

Both suites run from `cmd/racer`:

```
timeout 15m cargo test --locked --all-targets
timeout 15m cargo test --locked --all-targets --features sim
```

The wrapper is deliberate. A simulation or a model check that wedges does not
fail, it hangs, and `libtest` has no per-test deadline, so an outer clock is
the only thing that turns a deadlock into a report. CI applies the same cap per
step. The individual tests already bound themselves from the inside: the
simulations stop after `PATIENCE` scheduler steps and the ublk integration test
carries explicit deadlines, so a timeout at this level means something is stuck
below those guards.

`tests/dst.rs` sweeps `SEEDS` linearizability seeds in parallel. When one
fails, replay it alone with `DST_SEED=<seed>`; the run is deterministic.

Tests build under `[profile.test]` with optimizations on and debug assertions
left enabled. The simulations drive whole clusters in one process and the model
checkers enumerate millions of states, which is more than an order of magnitude
of wall clock for a few seconds of extra codegen.
