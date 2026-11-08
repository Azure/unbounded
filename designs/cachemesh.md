# cachemesh: bounded-degree routing for a 100k-node cache

## Status

Implemented in `internal/cachemesh`. This document is the specification the
code was built against. The code is authoritative; when they disagree, fix
whichever is wrong and say so in the change.

## Scope

cachemesh is a Go library that makes routing decisions for a distributed,
groupcache-style cache spread across up to 100,000 processes. It decides:

- which member owns a key (and which members back it up),
- which neighbor a request should be sent to next,
- which neighbors a process should keep connections to,
- when a cached metadata value has expired,
- which members are in the cluster, and who coordinates changes to that set.

It does not move bytes, cache values, or open connections. The embedding
application (the first consumer is gantry, which will stripe image layers
across nodes and cache layer metadata separately) owns storage, transports,
caching, and the requests themselves. A separate design will cover gantry.

### Requirements

1. Not a full mesh. Every member holds at most `MaxNeighbors` (default 256)
   data-plane neighbors, at any cluster size up to 100k.
2. Content-addressed blobs never expire. Metadata keyspaces expire with a
   configurable TTL of at least 5 minutes, measured with a rolling cluster
   epoch, so cached copies held anywhere expire within a bounded window.
3. No member holds a connection to the Kubernetes API server in steady state.
   A small number of members poll one Lease to elect a leader.
4. Transport-agnostic: users bring gRPC, HTTP, or anything else.
5. Health signals (circuit breakers, outlier detection, latency tracking)
   steer traffic away from bad members to protect tail latency.
6. Frequent membership changes do not disrupt traffic or tail latency.
7. Minimal per-request cost: routing a request allocates nothing, takes no
   locks, and does not read the clock.

## Prior art

| Source | Adopted | Rejected |
|---|---|---|
| groupcache | Read-through owner model; owner-fill deduplication (left to the caller) | Global registries, crc32 ring, random hot-cache fill, origin load on any peer error |
| galaxycache, groupcache-go | Caller-supplied transports; expiry carried with values; asking the previous owner after ownership moves | A client per peer (N connections), broadcast removal |
| Facebook memcache (NSDI'13) | Leases for fills (left to the caller); cold-cluster warmup | A separate gutter pool; backups in the owner's cell play that role |
| Kelips, HyperX, Pastry/PAST | Bounded-degree grid; converging routes; caching on the route | Gossip-only convergence (estimated over an hour at 10^5 nodes) |
| Slicer, Centrifuge, KRaft | Leader-assigned ownership table; versioned deltas; snapshot fallback | A separate distribution tier; the grid distributes |
| Rapid, Lifeguard | Removal only when several observers agree; batched changes | Flat SWIM gossip (HashiCorp caps pools near 5k nodes) |
| Envoy, gRPC A50, Finagle | Outlier-detection defaults; peak-EWMA; single-probe half-open; panic threshold | Per-request clock reads for windows |
| Bazel REAPI | Blob (CAS) versus metadata (AC) split; a salt or generation for mass invalidation | |
| Spegel | Lesson: per-pod Kubernetes leader election broke near 1000 pods | |
| Google, Twitter, Uber subsetting | Deterministic, churn-minimizing neighbor choice | Subsetting itself; it assumes any replica can serve, but a cache needs the owner |

Rejected alternatives for placement:

- Rendezvous hashing over all members costs O(N) per lookup at 100k.
- Skeleton (hierarchical) rendezvous moves several times more keys per join.
- A hash ring with virtual nodes needs about 100x memory for usable balance.
- Jump, Anchor, and Memento hashing depend on update order or dense indexes.
- A leader-assigned partition table gives exact balance, slow start,
  draining, and O(1) lookups, and the leader already exists.

Rejected alternative for the wire format: protobuf. The repository's protobuf
workflow needs `protoc-gen-go` on `PATH`, and the data-plane header must be
fixed-size and allocation-free. The control messages use a small versioned
binary codec with fuzz tests instead.

## Architecture

```
          +-------------------------- cachemesh.Mesh ---------------------------+
          |                                                                     |
 Route ---+--> atomic.Pointer[route.State] --> view, health, epoch (lock-free)   |
 Forward  |                                                                     |
 Observe -+--> health.Tracker (atomics)                                         |
          |                                                                     |
 ServeControl --> inbox --> node.Node (single goroutine, deterministic) --> outbox --> ControlTransport
          |                    |  elect.Elector --> LeaseOp --> kubelease.Store  |
          |                    |  leader.Leader (when elected) --> deltas         |
          +---------------------------------------------------------------------+
```

The control plane is a deterministic, single-threaded state machine
(`node.Node`) with no I/O, no goroutines, no wall clock, and no global
randomness. Everything it needs comes in through method arguments (time,
messages, lease results, a seeded random source) and everything it does comes
out through an outbox. `cachemesh.Mesh` is a thin concurrent runtime around it.
The deterministic simulation test suite (`internal/dst`) drives thousands of
`node.Node` instances directly on a simulated clock and network.

Rules every package under `internal/cachemesh` follows so that simulation is
deterministic:

- Never read the wall clock or monotonic clock. Time is passed in as
  `int64` nanoseconds of a node-local monotonic clock (`mono`), or a
  `time.Time` for wall-clock comparisons with Lease timestamps.
- Never use the global `math/rand`. Use the injected `*rand.Rand`
  (`math/rand/v2`, PCG source).
- Never let Go map iteration order affect outputs. Sort keys, or iterate over
  slices.
- No goroutines, timers, or channels outside `cachemesh.Mesh` and
  `kubelease`.

### Packages

| Package | Purpose | Imports |
|---|---|---|
| `internal/cachemesh` | Public API (`Mesh`, `Config`, aliases of route types), concurrent runtime | all internal |
| `internal/cachemesh/kubelease` | `elect.Store` over client-go `coordination/v1` Leases | `internal/elect` |
| `internal/cachemesh/memtransport` | In-process `ControlTransport` and `LeaseStore` for tests and examples | `internal/cachemesh` |
| `internal/cachemesh/internal/hashing` | Key hashing, mixing, rendezvous scores | `zeebo/xxh3` |
| `internal/cachemesh/internal/topology` | Grid shapes, cells, neighbor cells, next-hop cells | `hashing` |
| `internal/cachemesh/internal/placement` | Partition table (chunked copy-on-write), leader rebalancer | `hashing` |
| `internal/cachemesh/internal/view` | Immutable membership view, deltas, apply, validation, digests, snapshots, delta log, interning | `hashing`, `topology`, `placement` |
| `internal/cachemesh/internal/epoch` | Rolling cluster epoch tracker, stamps | none |
| `internal/cachemesh/internal/health` | Per-neighbor breakers, outlier detection, peak-EWMA, hedge budget | none |
| `internal/cachemesh/internal/route` | Hot-path routing state, `Decision`, `Header` | `view`, `health`, `epoch`, `topology`, `placement`, `hashing` |
| `internal/cachemesh/internal/wire` | Control messages and binary codec | `view` |
| `internal/cachemesh/internal/elect` | Lease election state machine (candidates, lottery, fencing) | `hashing` |
| `internal/cachemesh/internal/leader` | Leader logic: admission, suspicion, removal, rebalance, cells, epochs | `view`, `placement`, `topology` |
| `internal/cachemesh/internal/node` | Per-member control-plane state machine | all of the above |
| `internal/cachemesh/internal/dst` | Deterministic simulator and the DST suite | `node` and below |

## Identity and hashing

- `MemberID` is a string supplied by the application, unique per member (for
  example the Kubernetes node name). `Incarnation` is a `uint64` chosen at
  process start (random, nonzero); a restart with the same ID gets a new one.
- `Member.Key = hashing.MemberKey(id, incarnation)` is a 64-bit hash used in
  every rendezvous score. Scores never use slot numbers, so they do not change
  when slots are reused.
- `Slot` is a dense `uint32` index assigned by the leader for the life of a
  member record. `NoSlot = ^uint32(0)`. A freed slot is not reused for
  `SlotReuseEpochs` (default 16) epochs, which exceeds the handoff window.
- `hashing.KeyHash(seed uint64, key []byte) uint64` is `xxh3.HashSeed`.
  A keyspace seed is `hashing.Seed(name)`; each keyspace hashes into the same
  partition table with a different seed, so blob and metadata placement are
  independent.
- `hashing.Partition(h uint64, bits uint8) uint32` takes the top `bits` bits.
- `hashing.Mix64` is the SplitMix64 finalizer. `hashing.Score(a, b uint64)`
  is `Mix64(a ^ Mix64(b))`; larger wins, ties broken by the larger `a`.
- No floating point in any value that two members must agree on.

## Topology: the grid

The leader places every member in one cell of a D-dimensional grid with
dimension sizes `s[0..D-1]` (mixed radix; `Cells = prod(s)`). Two cells are
adjacent if their coordinates differ in exactly one dimension. A member's
neighbors are every other member in its own cell or an adjacent cell.

```
neighbors ~= occ * (1 + sum(s[i] - 1)) - 1      where occ = members per cell
```

`topology.Choose(n, cellSize, maxNeighbors)` returns the shape with the
smallest D (1..`MaxDims`=8) such that `ceil(n/cells) * (1 + sum(s[i]-1)) - 1
<= maxNeighbors`, with `cells = max(1, floor(n/cellSize))` spread as evenly as
possible across dimensions (sizes differ by at most one): every size starts
at `floor(cells^(1/D))` and sizes are raised by one from dimension 0 upward
until the product is at least `cells`, so occupancy never exceeds the
requested cell size by rounding. If no D fits, D=`MaxDims`. Small clusters get
D=1, a full mesh with one hop.

At 100k members with `CellSize`=4 and `MaxNeighbors`=256 the grid is D=4 with
about 190 neighbors and at most 4 hops.

| D | neighbors at 100k | max hops |
|---|---|---|
| 3 | ~340 | 3 |
| 4 | ~190 | 4 |
| 5 | ~135 | 5 |

### Next hop

To reach member `t` in cell `ct` from member `x` in cell `cx`:

1. If `x == t`, deliver locally.
2. If `cx == ct` or the cells differ in exactly one dimension, `t` is a
   neighbor: send directly.
3. Otherwise take the lowest dimension `i` where they differ. The next cell
   is `cx` with coordinate `i` replaced by `ct[i]`. Within that cell choose
   the member with the highest `Score(member.Key, partition)` that is usable
   (see health). If no member of that cell is usable or the cell is empty, try
   the next differing dimension in increasing order.
4. If every option fails, ignore health (panic) for this decision. If cells
   are empty, return `KindDirect` with the target itself: the caller may open
   a connection outside the budget. The simulator measures how often this
   happens; it must be zero in a settled cluster.

Because every member in the same row uses the same dimension order and the
same per-partition score, requests for one partition from about `s[0]*occ`
members converge on the same relay. A blob requested by all 100k members fans
out through a tree about 50 wide, and the caller can cache at each hop.

### Cell assignment and reshape (leader)

- A new member joins the least-occupied cell; ties go to the cell with the
  highest `Score(member.Key, cell)` (`Better` semantics).
- Each tick, if the most and least occupied cells differ by more than one,
  the leader moves up to `MaxCellMovesPerTick` (32) members from the fullest
  to the emptiest cells, preferring members in `Joining` state.
- Reshape (all members get new cells) only when the current shape violates a
  hard bound: estimated neighbors with the maximum occupancy exceed
  `MaxNeighbors`, or average occupancy falls below `max(1, CellSize/2)` with
  more than one cell, or rises above `4*CellSize`, and `Choose` would return
  a different shape (a reshape that cannot fix the bound is skipped). Reshape assigns cells in
  order of `Member.Key` round-robin. Removed neighbor connections linger for
  `NeighborLinger` (30s) so in-flight requests finish.

## Placement: the partition table

- `P = 2^PartitionBits` partitions (default 20 bits, 1,048,576). Every member
  of a cluster must use the same value; it is part of the view and a joiner
  with a different value is rejected.
- `placement.Table` maps partition to `Entry{Owner, Prev uint32; Moved
  uint32}` (`Moved` is the epoch of the last move). It is stored in 1024-entry
  chunks, copied on write, so a delta that moves k partitions copies at most k
  chunks. Each chunk keeps its entries as three columns (owners, previous
  owners, move epochs), so the owner lookup every request makes reads a dense
  4 MiB array at 2^20 partitions instead of 12 MiB of entries; `Prev` and
  `Moved` are read only for decisions whose final hop is the owner.
- **Backups** are not stored. They are the owner's cell-mates ranked by
  `Score(member.Key, partition)`. A failover to a backup therefore keeps the
  same route except for the last hop.
- **Handoff**: when a partition moves from a live owner, `Prev` is set and the
  route decision includes `PrevOwner` for `HandoffEpochs` (8) epochs so a new
  owner can read through from the old one before going to origin.

### Rebalancer (leader)

Each member has a weight 0..1000. `Joining` and `Draining` members have
weight 0; `Active` members ramp 250, 500, 1000 every `WarmupStep` (20s) after
admission. The target for member i is `floor(P*w_i/W)`; the remainder goes to
members in order of `Member.Key`. Each leader tick:

1. Mandatory: every partition owned by a removed member, or by a member with
   weight 0, gets a new owner. Prefer the old owner's best-ranked cell-mate
   whose load is under its target; otherwise the globally least-loaded member
   (ties by `Member.Key`). `Assign` always records the old owner in `Prev`;
  consumers ignore a `Prev` that is no longer a member (slots are not reused
  within the handoff window). A removed member's cell is known to the
  rebalancer only if the leader lists it (`placement.Member.Gone`).
2. Optional, up to `MaxMovesPerTick` (4096): move partitions from the most
   over-target members to the most under-target members, setting `Prev`.
3. Settled invariant: every assignable member's load is within one of its
   target.

If no member has weight > 0 (bootstrap), all members are treated as weight
1000.

## Membership view

`view.View` is immutable and shared. It contains:

- `Version{Term, Seq uint64}` (ordered by term, then seq) and a 32-byte
  `Digest`: `Digest(next) = sha256(Digest(base) || encode(delta))`. Same
  version with a different digest is a safety violation.
- `Leader` (slot) and `Term` metadata.
- Members indexed by slot (chunked copy-on-write); each
  `Member{ID, Incarnation, Key, Addr, Zone, State, Weight, Cell, Joined
  (mono-independent epoch number), Since (epoch of last state change)}`.
  `State` is `Joining`, `Active`, or `Draining`. Removed members are absent.
- Cell index: cell to member slots (sorted by `Member.Key`).
- `topology.Shape`, `PartitionBits`, `placement.Table`.
- Epoch anchor: `Epoch uint64` and `EpochPeriod time.Duration` as of this
  version.
- Keyspace generations: `map[seed]uint32`, bumped by invalidation.
- Free slots with the epoch they become reusable.

`view.Delta{Base, Version Version; Ops []Op}`. `Apply(base, delta)` requires
`base.Version == delta.Base` and returns a new view that shares all untouched
chunks. Ops:

| Op | Effect |
|---|---|
| `NewTerm{Term, Leader}` | Start of a leader term. Seq restarts at 1. |
| `AddMember{Slot, Member}` | Admit a member. |
| `RemoveMember{Slot}` | Remove a member; its slot is freed. |
| `SetState{Slot, State, Weight}` | State and weight change. |
| `SetCell{Slot, Cell}` | Move a member to another cell. |
| `Reshape{Shape, Cells []uint32}` | New grid; `Cells` indexed by slot. |
| `Assign{Parts, Owners []uint32}` | Partition moves; sets `Prev` and `Moved`. |
| `Epoch{Epoch, Period}` | Epoch anchor (heartbeat). |
| `Invalidate{Seed, Gen}` | Keyspace generation bump. |
| `Config{PartitionBits, ...}` | Cluster constants; only in the first delta of a cluster. |

Delta rules (`view.Apply`):

- `Delta{Base, Version, Digest, Ops}`. `Digest` is the digest of the
  resulting view, `sha256(Digest(base) || canonical encoding of the delta
  without its digest)`; the genesis digest is
  `sha256("cachemesh/view/v1/genesis")`. `Apply` fails with `ErrBase` if
  `base.Version != delta.Base`, `ErrVersion` unless `Version` is
  `{base.Term, base.Seq+1}` (or `{Term, 1}` when the first op is `NewTerm`
  with a larger term), and `ErrDigest` on a digest mismatch, which also
  detects a forked base.
- `NewTerm` is only valid as the first op. `Config` is only valid when the
  view has no config; it creates the table with every partition owned by
  `InitialOwner` (no 2^20-entry `Assign` at bootstrap) and a one-cell
  shape, so the bootstrap delta can admit members in cell 0 and a later
  `Reshape` sets the real grid. Member, cell, and partition ops require a
  config; `Epoch` and `Invalidate` do not.
- `AddMember` takes the next slot (`SlotCap`) or a free slot whose
  reuse epoch has passed; `Joined` and `Since` are set from the view epoch.
  `RemoveMember` frees the slot until `epoch + SlotReuseEpochs`. `Assign`
  owners must be members when the op runs.
- At the end of a delta the leader must be a member when the term is
  non-zero, and while the view has members no partition may be owned by a
  non-member (per-slot loads are maintained incrementally, so this check is
  O(1)). A removal therefore moves the member's partitions in the same
  delta, before or after the `RemoveMember` op.
- `Apply` copies only the storage chunks it touches (256 members, 1024
  loads, 256 cells, 1024 ID index entries per chunk) except for `Config` and
  `Reshape`, which rebuild the cell index.

`view.Validate(v)` checks every structural invariant (owners are members,
cells match members, occupancy counts, slot uniqueness, shape bounds). Tests
and the simulator call it on every view they build.

Every node keeps a `DeltaLog` of the last `DeltaLogSize` (512) deltas so it
can serve catch-up. `view.Interner` lets the simulator share identical views
(keyed by digest) across simulated nodes; production uses no interner.

`view.AppendSnapshot` encodes a whole view (members, free slots, and the
table as bit-packed columns; derived indexes are rebuilt and checked by
`DecodeSnapshot`); `view.Observers(v, slot)` and
`view.Subjects(v, slot)` give the monitoring relation (below).

## Rolling cluster epoch

- The leader owns `EpochPeriod` (default 15s) and publishes an `Epoch` op
  every period as a heartbeat.
- `epoch.Tracker` maps local monotonic time to an epoch:
  `current = anchorEpoch + floor((now - anchorMono) / period)`, re-anchored on
  heartbeats received. It never decreases. A member cut off from the
  leader keeps advancing on its own clock, so expiry is never suspended.
- An anchor never moves the phase backward: a heartbeat (or snapshot)
  whose epoch is at or below the epoch the tracker already computes, with
  the same period, is ignored. Heartbeats are applied up to a period after
  their epoch began (catch-up deltas and snapshots replay old `Epoch` ops),
  and re-anchoring on them delayed the member's epoch boundaries; a member
  that then became leader published its late phase, every member stalled
  to match, and each leader change stretched every lifetime by up to
  another period. Only an anchor ahead of the member re-anchors it.
- Pings and pongs carry the sender's epoch; a member behind a peer anchors
  forward to it (the tracker ignores epochs it already reaches). The
  cluster epoch therefore follows its most advanced member, and a leader
  whose epoch lags (a lottery winner that anchored from a stale snapshot)
  catches up within a few ping intervals instead of pulling every member
  that later anchors to its heartbeats behind it.
- A member counts epochs from its start (epoch 0 at `Start`) until the
  first heartbeat or snapshot anchors it. Anchoring at monotonic time 0
  would start it at `uptime / period`, ahead of the cluster for good.
- `Tracker.Current()` is an atomic load. The runtime calls
  `Tracker.Advance(now)` on every tick (default every 100ms); the request path
  never reads the clock.
- A keyspace with TTL `T` (zero means never expires, otherwise
  `T >= 5m`) stamps values at the owner with
  `Stamp{Expires: current + ceil(T/period) + 1, Gen: generation}`. Copies
  keep the owner's stamp. A member without a view finds only stamps that
  never expire fresh. `Fresh(stamp)` is `current < Expires && Gen ==
  generation`. A value therefore lives between `T` and
  `ceil(T/period)*period + period` on members whose clocks match the
  leader's, plus clock drift between heartbeats: `T + period` when `T` is a
  multiple of the period, and always less than `T + 2*period`.
- `Invalidate(keyspace)` asks the leader to bump the generation, which makes
  every existing stamp of that keyspace stale.
- A whole-cluster cold restart (see the node notes) bootstraps a new view
  whose epoch starts at 0 again. Every cached value died with its process,
  so this is safe today; stamps persisted across process restarts would need
  the epoch carried over (for example in the Lease).

## Health

Health is tracked only for neighbors (and, briefly, for lingering former
neighbors), so state is bounded by the connection budget. Health never changes
membership; it only filters routing choices and feeds monitoring.

`health.Tracker` holds one `Peer` per slot with atomic fields. Defaults follow
Envoy and gRPC A50:

- **Breaker**: `ConsecutiveFailures` (5) failures eject the peer for
  `BaseEjection` (30s) times the ejection count, capped at `MaxEjection`
  (300s), with up to 10% jitter. When the ejection ends the peer is `Probing`:
  exactly one request (claimed with a CAS) may go to it; success makes it
  `Healthy`, failure ejects it again. The ejection count decays by one per
  healthy interval.
- **Outlier detection** every `Interval` (10s): per-peer success and failure
  counters live in two buckets swapped by the tick, so requests never read
  the clock. Success-rate ejection (below mean - 1.9 stddev, at least 5 peers
  with 100 requests) and failure-percentage ejection (85% with 50 requests).
- **Peak-EWMA latency**: a slow observation is taken immediately, faster ones
  decay with a 10s time constant measured on the coarse tick clock. A peer
  whose EWMA exceeds `DegradedFactor` (3) times the median of its neighbors
  and `DegradedFloor` (10ms) is `Degraded`: used only if no `Healthy` member of
  the same cell is usable.
- **Overload** results (the peer shed load) never trip the breaker; they mark
  the peer busy for `BusyFor` (1s), which ranks it like `Degraded`.
- **Limits**: at most `MaxEjectionPercent` (10%) of tracked peers are ejected
  at once, and the last usable member of a cell is never ejected. If more than
  `PanicThreshold` (50%) of tracked peers are unusable, health filtering is
  disabled (`Panic`).
- **Hedging**: each decision carries an `Alternate` (the next usable member
  of the same next-hop cell) and `HedgeAfter = clamp(3 * EWMA(Next), 5ms, 1s)`.
  `TryHedge()` enforces a budget of `HedgeRatio` (5%) of routed requests per
  interval. Routed requests are counted in shards picked by the calling
  goroutine's stack address, so concurrent routers rarely share a counter
  line; `TryHedge` (called only for hedge candidates) sums the shards, so the
  shard choice never changes a result.

Implementation notes (`health` package): the per-tracker slot index is a small
copy-on-write open-addressing table, so memory is O(tracked peers), not
O(max slot); `Sync` replaces the tracked set with one rebuild (the node calls
it on every neighbor change) and `TrackMany` adds a batch the same way.
Untracked slots are `Healthy`. Breaker ejections triggered by
`Observe` take effect immediately but are timed (and jittered) at the next
`Tick`, so the random source stays on the control path. At least one peer may
always be ejected regardless of `MaxEjectionPercent`; the limit counters are
adjusted optimistically and recomputed every `Tick`. `Panic` is recomputed on
`Tick`, `Track`, and `Untrack`. The degraded median is over all tracked peers
with latency data. `HedgeAfter` is `HedgeMax` for a peer with no latency data. A peer with no
observations in a tick decays its EWMA toward the last median (never
upward), so an idle `Degraded` peer, which routing avoids, recovers instead of
staying `Degraded` forever. Latencies are capped at one hour.

`Observe` is written for many goroutines reporting on the same peer. A
`Success` writes one shared counter (the active bucket); the latency mean
that the EWMA decays toward is sampled (every success up to 64 per interval,
then one in 8, chosen by the bucket count so it is deterministic), and the
peak EWMA and the consecutive-failure counter are written only when they
change. A `Peer` is three 64-byte lines: fields routing reads, counters
`Observe` writes, and ejection and control fields. `AllHealthy` (no peer
ejected, probing, degraded at the last tick, or busy; or panic) lets routing
skip per-peer `Usable` lookups; it is maintained by counters that change only
on breaker transitions, so it is exact for a single goroutine and
conservative otherwise. Peers dropped from the tracked set are retired so
that a stale table cannot eject them.

## Monitoring and failure detection

Each member monitors a small, fixed set of subjects:

- its cell-mates, and
- for each dimension j and direction +1/-1, the members of the adjacent cell
  `c +/- e_j` (wrapping) whose rank within that cell, modulo that cell's
  occupancy, equals this member's rank modulo the same occupancy.

So each member has about `CellSize - 1 + 2D` observers spread over up to
`2D + 1` cells. Monitors ping each subject every `MonitorInterval` (2s) with
`PingTimeout` (1s); the ping also carries the sender's version and epoch, so
it doubles as anti-entropy. After `SuspectAfter` (3) consecutive failed pings
the monitor sends `Suspect` to the leader, repeated every `SuspectRefresh`
(5s) while it persists.

The leader removes a member once observers from at least `ObserverCells` (3,
clamped to the number of distinct cells of its observers that are not
themselves suspected) distinct cells have reported it within
`SuspicionWindow` (30s). Excluding suspected observers lets the leader remove
two failed members that observe each other. Removals are capped at
`max(1, N/1000)` per tick; if more than `DampingFraction` (10%) of members are
suspected at once, the cap drops to `max(1, N/10000)` per tick so a partition
cannot empty the cluster quickly.

## Leader and control plane

### Messages

Every control message carries the sender's `MemberRef{ID, Incarnation,
Slot}`, `Version`, `Digest`, `Epoch`, and the slot of the leader it knows
(`NoSlot` if none). All are encoded by `internal/wire`. Every message is a
request answered by one response; `Ack` and `Nack{Reason}` are the generic
responses.

| Message | Use |
|---|---|
| `Ping` / `Pong` | Monitoring, version anti-entropy. `Pong` may carry a leader address hint. |
| `Broadcast{Delta, Level}` | Delta dissemination along the grid tree. |
| `CatchUpRequest{Have, HaveDigest, MaxDeltas}` | Ask a peer for missed deltas. |
| `CatchUpResponse{Deltas}` or `{Snapshot}` | Deltas from the peer's log if it holds `Have`, otherwise a snapshot; empty if the peer has nothing newer. |
| `Routed{Origin, Target or ToLeader, TTL, Hops, Inner}` | Relay a message to a member (usually the leader) hop by hop over the grid. |
| `Join{Member, PartitionBits}` | Ask to be admitted; sent to a seed, which wraps it in `Routed` to the leader. |
| `Leave{Member}` | Graceful departure; routed to the leader. |
| `Suspect{Target, Observer}` | Monitoring report; routed to the leader. |
| `Invalidate{Seed}` | Keyspace generation bump; routed to the leader. |

Lost routed messages are retried by their senders (joins every `JoinRetry`
(2s) until admitted; leaves until `Draining` is observed; suspicions while the
failure persists).

### Dissemination

The leader applies its own delta and sends `Broadcast{Level: -1}` toward the
grid tree. A member receiving `Broadcast` at level `l` applies it (or queues
it if it is ahead of its view, and catches up from the sender if it is behind)
and then forwards, using the post-apply view, to one usable member of each
adjacent cell in every dimension `j > l` (`Level: j`), chosen by
`Score(member.Key, deltaSeq)`, and, if it is the first member of its cell to
receive it (`l != Local`), to its cell-mates with `Level: Local`. In the
fault-free case each member receives each delta exactly once in at most
`D + 1` hops.

Pings carry versions, so a member that misses a broadcast learns within a
monitor interval that a peer is ahead and sends `CatchUpRequest`.

### Joining

1. A starting member gets seed addresses from `Config.Seeds`, or from one
   cached Lease read (`resourceVersion=0`), whose annotations list the leader
   and up to 16 random seed members chosen by the leader.
2. It sends `Join` and `CatchUpRequest{Have: zero}` to a random seed. Until it
   appears in a view, its `Route` returns `KindGateway` through that seed, and
   it repeats catch-up from the seed every `JoinRetry`.
3. The leader admits up to `MaxJoinsPerTick` (256) per tick: slot, cell,
   `Joining`, weight 0. If another incarnation with the same ID exists it is
   removed in the same delta.
4. After `QuarantineFor` (10s) the member becomes `Active` and its weight
   ramps up, which moves partitions to it gradually.

### Leaving

`Mesh.Leave` sends `Leave`. The leader sets `Draining`, weight 0, which moves
all its partitions away with `Prev` pointing at it, and removes it after
`DrainFor` (30s). The member keeps serving until it observes its removal or
the caller stops it.

### Leader logic

The leader batches inputs and emits at most one delta per `LeaderTick` (1s),
plus a heartbeat `Epoch` op each epoch period. Each delta:
admissions, quarantine and warmup promotions, drains and drained removals,
suspicion removals (with damping), mandatory then optional partition moves,
cell balancing or reshape, epoch, invalidations. A new leader starts from its
own view, so no state is rebuilt from scratch; pending joins and suspicions
are simply retried by their senders.

Implementation notes (`leader` package):

- Within a delta, cell balancing or reshape comes before the partition moves
  (`Assign`), so the planner prefers the final cell-mates of removed owners.
  The full order is: `NewTerm`, bootstrap (`Config`, `AddMember(self)`,
  `Epoch`), admissions, promotions, drains and drained removals, suspicion
  removals, cells, `Assign`, `Epoch`, `Invalidate`.
- `Tick(now, epochNow, view, newTerm)` emits `NewTerm` itself when `newTerm`
  is nonzero. A leader that is not a member of its view (a lottery winner)
  admits itself as `Joining` in that first delta, removing a stale record of
  its own ID; outside a term start a non-member leader publishes nothing.
- Quarantine, warmup, and drain timers run on the leader's monotonic clock
  from when it set the member's state. For states set by an earlier leader
  the start is estimated as `now - (epochNow - Member.Since) * EpochPeriod`.
- The leader drains itself on `Leave` but never removes itself; the next
  leader removes it.
- The placement scan is skipped when the table and the planner inputs
  (slots, keys, weights, cells) are unchanged since a plan with no moves.
- One delta holds at most `view.MaxDeltaOps - 4096` member ops and 1024
  invalidations; further due work waits for the next tick.
- Input queues (joins, leaves, suspected targets, invalidations) are bounded
  by `MaxPending` (4096); suspicion reports per target by 64.

Implementation notes (`node` package):

- `Routed` messages are relayed straight to the leader address the relay
  knows (from its view, the Lease, or a `Pong` hint) rather than hop by hop
  over the grid; `TTL` (4) bounds relay chains while leaders change.
- A member with no `Config.Seeds` issues one cached Lease read at start.
  Until it is answered the elector is held as a follower, so a starting
  member makes exactly one Lease call. If it finds no Lease, or a released
  one without seeds, it bootstraps: it takes part in the lottery until it
  has a view (not as a candidate, so members that all start at once without
  seeds make about `LotteryBudgetQPS` Lease calls rather than one each per
  `RetryPeriod`; set `ExpectedClusterSize` near the real size, since a
  single member with the default of 1000 is eligible in about one round in
  50). A member without a view that wins the Lease and is neither a
  bootstrapper nor cold-starting releases it instead of publishing a second
  genesis. A released Lease keeps its seeds, so starters still find the
  cluster through it.
- A member that finds itself removed (for example after a partition) and is
  not leaving rejoins with the same incarnation. While not a member, every
  view it adopts adds the leader and a few random members to its seeds, a
  `Broadcast` sender is added as a seed, a seed whose catch-up fails is
  dropped (keeping at least one), and the Lease is re-read every 16 join
  attempts.
- The Lease keeps the cluster's seeds: a holder that knows none (a member
  without a view that won the lottery, which then declines) keeps the
  record's seeds when it acquires and releases, and a releasing leader
  lists the other members it knows. Seeds are cleared only when the last
  known member leaves. Clearing them on every release let a fresh member
  bootstrap a second cluster next to the live one.
- A leaving member never (re)joins. A member removed by a partition that
  is then asked to leave used to keep sending joins, and one reaching the
  leader after the process stopped re-admitted it. A join the leader had
  already queued can still admit a member whose process has stopped;
  failure detection removes it.
- Calls that time out together are failed in call ID order, so map
  iteration order never reaches the outbox.
- A starting member without seeds treats any Lease record with a holder or
  seeds as a live cluster and joins through it; it bootstraps only when the
  Lease is missing, or released without seeds. Judging the holder's liveness
  by wall clock alone here would let a skewed clock start a second cluster.
- Cold start (every member died, so the Lease names a dead leader and dead
  seeds). A member that is not in its view (it may hold none, or a stale
  one from before the restart) enters a cold start when
  all of these hold at one of its own Lease reads (the start read, or the
  re-read every 16 join attempts, with or without configured seeds): the
  record, or its absence, has the same UID and resourceVersion as a read at
  least `LeaseDuration` earlier on the member's monotonic clock; a held
  record is expired by wall time beyond `LeaseDuration +
  ClockSkewAllowance`; and no reply from a peer that is a member of its
  view arrived for `LeaseDuration` (peers that hold a view they are not in
  do not count, or restarted processes would hold each other back). A cold-starting member is no longer held as a follower,
  so it enters the lottery (bounding Lease reads at `LotteryBudgetQPS`); a
  lottery winner publishes a first view like a bootstrapper (or a new term
  of the stale view it holds, admitting itself) and the others
  discover it in the Lease and join. The unchanged-resourceVersion rule is
  the candidates' rule, so a joiner with a skewed wall clock that is cut off
  from a live cluster never cold-starts: the live leader's renewals keep
  changing the record. A `Discovered` live holder cancels a cold start.
- Higher terms. Every control message and delta carries a full 64-bit term.
  A member that sees a term above its view's records it
  (`Elector.ObserveTerm`) and catches up from the sender; a leader that sees
  a term above its own stops publishing at once and releases the Lease
  (`resign`), as it does when a view of a later term is applied or its view
  term no longer matches. The release record carries the highest term the
  member has seen, so the next holder starts above both lineages even if
  the Lease term annotation was lost (read as 0). Terms at or above
  `elect.MaxSaneTerm` (2^62) are garbage and ignored, so terms cannot
  overflow. `NoteVersion`, fed from data-plane headers whose term is
  truncated to 32 bits, may trigger a catch-up but never a step-down, so
  terms that differ only above bit 32 cannot cause a regression.
- A leaving member hands leadership off once: if a Lease acquire that was
  in flight when `Leave` was called succeeds (or it wins the lottery), it
  releases the Lease at once, which any candidate takes immediately. A
  member that is not in its view always declines. If it wins the Lease
  again, nobody else could take it, so it leads rather than leave the
  cluster leaderless (it may be the only live member). A leaving leader
  drains itself and releases once its partitions have moved; after a first
  hand-off it releases again only when another member became Active after
  it started leaving (so is known to be alive).
- Snapshot trust. A snapshot's version and digest are taken from its
  encoding, so a corrupt or forged one decodes. A snapshot served by a peer
  other than the leader of its view is *unconfirmed* until a delta applies on
  top of it (the digest chain then vouches for it). An unconfirmed or bad
  view is never served to others. When a delta whose base is the current
  version fails with `view.ErrDigest`, the member re-fetches from a source
  other than the peer that served the snapshot (the delta's sender, then
  the leader, the Lease holder, a seed), with a catch-up that does not wait
  behind one already in flight, and adopts a snapshot at the same version
  with a different digest from it. If a snapshot stays unconfirmed for
  `2*EpochPeriod + 2*CallTimeout` (a live leader heartbeats every epoch) it
  re-fetches the same way. A leader asked for a catch-up by a member that
  claims to be ahead of it in its own term answers with its snapshot, and
  an unconfirmed member adopts that lower version from the leader of its
  term (a forged snapshot claimed a version that was never published).
  This is the only case in which a view version decreases.
- Bounded state. Outbound calls awaiting a reply are capped at
  `MaxPendingCalls` (4096); further calls are dropped and their senders
  retry on their own timers. Seeds are capped at 32 (64 from the Lease),
  queued ahead deltas at `MaxAhead`, the delta log at `DeltaLogSize`, the
  leader's input queues at `MaxPending`; neighbors and subjects follow the
  view. `Node.Stats` reports every size.
- A joiner that has seeds stays a follower (the elector is told it has a
  view and has just heard from the leader), so it never enters the lottery;
  otherwise a joiner with a skewed wall clock takes a live leader's Lease and
  gives it back, forcing an election each time. A joiner that holds a
  configured view instead reports when it last applied a new version
  (heartbeats included): while it is in contact with a live cluster that
  stays recent, and if every member of its lineage is gone (all restarted
  with new incarnations before a leader could admit any) it enters the
  lottery after `NoLeaderTimeout` and the winner admits itself into the
  existing lineage, keeping its epoch and keyspace generations.
- When the Lease (as read by the elector) names a leader of a later term
  than the view, the member catches up from that leader. This heals two
  lineages left by a partition.
- A failed `Broadcast` forward is retried once with the next-ranked member
  of the same cell.
- Elector and health ticks run every `TickInterval` (1s).
- `node.Shared` (simulation only) shares interned views, encoded deltas,
  snapshots, and candidate lists across nodes in one process.

## Leader election

`elect.Elector` is a state machine. It emits `LeaseOp`s (`Get`, `GetCached`,
`Create`, `Update`) for the runtime to execute against an `elect.Store`, and
receives results. `kubelease.Store` implements `Store` with the typed
client-go `CoordinationV1().Leases(ns)`; the simulator implements it in
memory.

```go
type Record struct {
    Holder          string        // MemberID, empty if released
    Incarnation     uint64
    Term            uint64        // cachemesh term (annotation)
    Addr            string        // leader control address (annotation)
    Seeds           []string      // up to 16 seed addresses (annotation)
    LeaseDuration   time.Duration
    AcquireTime     time.Time
    RenewTime       time.Time
    Transitions     int32
    ResourceVersion string
    UID             string
}
```

States:

- **Follower** (almost every member): no API calls at all.
- **Candidate**: the member is among the top `Candidates` (3) by
  `Score(leaseSeed, member.Key)` over `Active` members admitted at least
  `MinCandidateAge` (60s) ago in its current view, or is the current leader.
  Every member computes the same list from its view. Candidates `Get` the
  Lease every `RetryPeriod` (2s, jittered); if the record has not changed for
  `LeaseDuration` (15s) of local time, they `Update` it with a
  resourceVersion check to take it.
- **Leader**: renews every `RetryPeriod`; if no renewal succeeds for
  `RenewDeadline` (10s) it stops publishing immediately and steps down.
  On acquisition the new term is `max(record.Term, view.Term) + 1`, written in
  the same compare-and-swap update that takes the Lease. Only after that write
  succeeds does the leader publish `NewTerm`.
- **Lottery**: when no leader has been heard from (no delta or heartbeat with
  a current term) for `NoLeaderTimeout` (30s plus jitter), or when there is no
  view at all, a member enters rounds of `LotteryRound` (1s). In round r it is
  eligible if `Score(member.Key, r) < p * 2^64` with
  `p = min(1, LotteryBudgetQPS * LotteryRound / N)`, where N is the larger
  of the member count of its last view and `ExpectedClusterSize`. An eligible member reads
  the Lease once: if a live leader holds it, the member records the leader's
  address and seeds and catches up from a seed; if it is expired by more than
  `LeaseDuration + ClockSkewAllowance` (5s) of wall time, the member tries to
  take it. This bounds API traffic during leader loss at about
  `LotteryBudgetQPS` (20/s) across the whole cluster. The floor on N keeps
  a member whose view is much smaller than the cluster (a fresh lineage
  after a cold restart) from reading every round.

`Elector.ObserveTerm` records terms seen outside the Lease (see the node
notes); the next acquisition uses `max(record.Term, view.Term, observed) +
1`, a record term at or above `MaxSaneTerm` reads as 0, and `Release` writes
the highest term seen into the released record.

A record held by the same MemberID with a different incarnation is treated
like any other holder (wait `LeaseDuration`), since IDs may collide; only a
record held by the elector's own incarnation, or a released or missing Lease,
is taken at once. An outstanding Lease operation whose result has not arrived
within `OpTimeout` (default `RetryPeriod`) is abandoned and its late result
ignored. A leader's deadline counts from the send time of its last successful
write, and the node calls `CanPublish(now)` before every publish, so a leader
paused past `RenewDeadline` never publishes on wake. `kubelease` preserves
labels, foreign annotations, and unused spec fields on update.

Fencing: every delta carries its term. Members reject deltas with a term lower
than their view's term. A leader that loses the Lease stops publishing before
any other member can acquire it (`RenewDeadline < LeaseDuration`).

## Data-plane API

```go
type Decision struct {
    Kind       Kind          // Self, Relay, Owner, Backup, Gateway, Direct, NoRoute
    Next       PeerRef       // member to send to (zero for Self/NoRoute)
    Owner      PeerRef       // owner of the partition in this member's view
    Alternate  PeerRef       // hedge or retry target, may be zero
    PrevOwner  PeerRef       // set during the handoff window when Next is final
    HedgeAfter time.Duration
    Header     Header        // updated header to send with the request
}

func (m *Mesh) Route(ks Keyspace, key []byte) Decision
func (m *Mesh) RouteHash(ks Keyspace, h uint64) Decision  // h = ks.Hash(key)
func (m *Mesh) RouteMany(ks Keyspace, keys [][]byte, out []Decision)
func (m *Mesh) Forward(h Header) (Decision, error)  // at a relay
func (m *Mesh) Observe(p PeerRef, r Result, latency time.Duration)
func (m *Mesh) TryHedge(h Header) bool
func (m *Mesh) Stamp(ks Keyspace) Stamp
func (m *Mesh) Fresh(ks Keyspace, s Stamp) bool
func (m *Mesh) Invalidate(ctx context.Context, ks Keyspace) error
func (m *Mesh) ServeControl(ctx context.Context, from Endpoint, msg []byte) ([]byte, error)
func (m *Mesh) Run(ctx context.Context) error
func (m *Mesh) Leave(ctx context.Context) error
```

`Header` is a fixed 32-byte value: format version, flags, hop count, TTL
(`D + 2`), partition, sender slot, the sender's term, seq, and epoch.
`Forward` rejects a header whose TTL is exhausted, notes when the sender's
version is ahead (the control plane then catches up from it), and otherwise
computes the next hop from the relay's own view. `Observe` reports outcomes:
`Success`, `Failure`, `Timeout`, `Overload`.

`ControlTransport.Send(ctx, addr Endpoint, msg []byte) ([]byte, error)`
(`Endpoint` is an alias of `string`) is the only control-plane transport
method. `NeighborWatcher.OnNeighbors(added, removed []PeerRef)` tells the
caller which data-plane connections to keep; removals are delayed by
`NeighborLinger`.

Per-request cost targets (benchmarked): `Route` on a 100k view under 100ns and
zero allocations; `Observe` under 50ns and zero allocations; `Fresh` a few ns.
`BenchmarkMesh` in `internal/cachemesh` measures them (`Route` over 1,024
keys, `ObserveParallel` with every goroutine reporting on one peer, the worst
case); `BenchmarkRouteWide` routes 1M distinct keys, where the owner lookup
and the owner's member record usually miss every cache level and the cost is
a few DRAM latencies, and routes from 16 different members in turn.

The route package keeps the per-request work small: when `health.AllHealthy`
holds it ranks candidates without per-peer `Usable` lookups, it keeps this
member's cell coordinates in the `State` so only the owner's cell is divided,
the table lookup reads only the owner column, and decisions are filled in
place field by field (`State.RouteTo`, `RouteHashTo`, `ForwardTo`, which the
public methods use): building a `PeerRef` or `Decision` value and copying it
stalls on store-to-load forwarding.

### Public API (`internal/cachemesh`)

Types the caller needs are aliases of the internal ones, so nothing is
converted on the hot path: `Decision`, `Kind` and the `Kind*` constants,
`PeerRef`, `Header` (with `HeaderSize`, the `Flag*` constants, `ParseHeader`,
`ErrTTLExhausted`, `ErrBadHeader`), `Result` (`Success`, `Failure`, `Timeout`,
`Overload`), `Stamp`, `Version`, `Slot`/`NoSlot`, and for election
`LeaseStore` (`elect.Store`), `LeaseRecord`, `ErrLeaseNotFound`,
`ErrLeaseConflict`, `ElectionConfig` (`elect.Config`), and `HealthConfig`
(`health.Config`).

`NewKeyspace(name, ttl)` returns a `Keyspace` value with `Name`, `TTL`,
`Seed` (`hashing.Seed(name)`), and `Hash(key)`. A zero TTL never expires;
otherwise the TTL must be at least `MinTTL` (5m). `MustKeyspace` panics on
error.

`Config` has the required `ID`, `Addr`, and `Transport`, then `Incarnation`
(zero picks a random nonzero value from `crypto/rand`), `Zone`, `Seeds`,
`Lease` (may be nil: the member then joins only through seeds and never
bootstraps or leads), `Neighbors`, `Logger` (`log/slog`), `Hooks`
(`OnView`, `OnLeadership`, `OnControlError`, `OnLeaseError`), and the common
cluster knobs `PartitionBits`, `CellSize`, `MaxNeighbors`, `EpochPeriod`, and
`ExpectedSize`. `ExpectedSize` sets `Election.ExpectedClusterSize` (setting
both to different values is an error). It is the lottery floor on N, so it
trades bootstrap latency for API server protection: the lone first member of
a new cluster waits about `ExpectedSize / LotteryBudgetQPS` seconds (about
50s at the default 1000, under a second at 20 or less), while members
without a view read the Lease at about `LotteryBudgetQPS * actual /
ExpectedSize` per second. Members with a view use its size, so only a
cluster-wide cold restart is exposed when the value is too small.
Advanced knobs live in `Config.Tuning`: node timers (ping, suspicion, join,
call timeout, linger, leader and elector ticks), `EpochAdvance` (100ms, the
longest the runtime waits between ticks, bounding epoch staleness),
`MaxInFlight` (64 concurrent control calls; excess calls fail at once with
`ErrTooManyCalls` and the protocol retries them), `LeaseTimeout` (default
`Election.OpTimeout`), and the nested `Election`, `Health`, and `Leader`
(`LeaderTuning`) settings. Zero values select defaults; `New` returns
validation errors.

Runtime (`Mesh`):

- `New` builds the `node.Node` and a `route.Env` holding the node's health
  and epoch trackers, a `route.Notes`, the self ID and incarnation, and the
  node's join seeds (other than its own address) as gateways
  (`PeerRef{Slot: NoSlot, ID: addr, Addr: addr}`). While the member is not
  in its view, the runtime compares the node's seed list (configured seeds,
  seeds and the leader address learned from the Lease, members of views
  seen while joining, minus dropped unresponsive ones) after every node call
  and rebuilds the `Env` and `State` when it changed, so a member configured
  with only a Lease routes through gateways while joining. A member with no
  seeds at all routes `KindDirect` once it has a view and `KindNoRoute`
  before.
- Every node method runs under one mutex. After each call the runtime
  rebuilds the `atomic.Pointer[route.State]` if the node's view or self slot
  changed, and tracks leadership and `Left`.
- `Run` starts the node, then loops: `Tick`, take a `Notes` entry and pass
  it to `NoteVersion` (widening the 32-bit header term against the view
  term), drain `Outbox` and `LeaseOps`, and sleep until
  `min(NextDeadline, EpochAdvance)` or until a reply, lease result, or
  inbound message wakes it. Each call runs in its own goroutine under
  `context.WithTimeout(Call.Timeout)` and reports to `OnReply` exactly once.
  Lease operations run one at a time on one goroutine with `LeaseTimeout`;
  without a `Lease` they fail at once with `ErrNoLease`. Local monotonic
  time is nanoseconds since `New` plus one; wall time is `time.Now()`.
- `Run` returns `ctx.Err()` on cancellation and nil once a `Leave` has
  completed; it may be called once (`ErrAlreadyRun`). `ServeControl` and
  `Leave` return `ErrNotRunning` unless `Run` is active.
- Neighbor slots from the node hook are mapped to `PeerRef`s with the view
  at the time of the change and remembered, so removals (after linger)
  report the same `PeerRef` that was added. A slot reused by another member
  is reported as a removal of the old peer and an addition of the new one.
  Events are queued and delivered in order by one goroutine, never under
  the Mesh lock. `OnView` and `OnLeadership` hooks go through the same
  queue (consecutive view events coalesce), so every hook may call Mesh
  methods. `OnControlError` runs on the failed call's goroutine and
  `OnLeaseError` on the Lease goroutine, neither under the lock.
- `ServeControl` rejects messages over `MaxControlMessageSize`
  (`wire.MaxMessageSize`) with `ErrMessageTooLarge`; other malformed input
  gets an encoded rejection.
- `Invalidate` queues a request per keyspace seed (at most
  `MaxPendingInvalidations`, 1024; beyond that `ErrTooManyInvalidations`),
  recording the keyspace generation of the current view as its base (or the
  first view's, if it has none yet). The run loop sends it to the leader
  every `JoinRetry` until a view shows a different generation, which
  completes every caller waiting on that seed. A canceled ctx returns
  `ctx.Err()` but leaves the request queued; waiters get `ErrNotRunning`
  when `Run` returns. Retries can bump a generation more than once, which
  only makes values stale earlier.
- Status accessors read the snapshot: `Version`, `Members`, `Leader`,
  `Self`, `Ready` (Active in the view), plus `Leading`, `Neighbors`,
  `Done`, `ID`, `Incarnation`, and `Addr`.

`internal/cachemesh/memtransport` provides a `Network` (per-address
`ControlTransport`s calling registered handlers such as
`Mesh.ServeControl`, with latency and jitter, drop rate, partition groups,
and `Unregister` to simulate a crash) and a `LeaseStore` (compare-and-swap
on `ResourceVersion`, create conflicts, not-found, injectable errors).

### Route package details

`route.State` is an immutable snapshot of one view and the self slot;
`route.NewState(v, self, env)` is O(1). `route.Env` (built once per member)
holds the shared `health.Tracker`, `epoch.Tracker`, `Notes`, the gateway
list, and optionally the self ID and incarnation, so a reused slot that now
holds another member is not taken as self. Choices the code makes beyond the
rules above:

- Ranking within a cell (relays and backups) is by usability class first
  (Healthy, then Degraded; Unusable excluded), then `Score(member.Key,
  partition)` with `Better` tie-breaks.
- Final hop: the owner gets the request when it is Healthy, Degraded with no
  Healthy backup, or when no backup is usable at all. Otherwise the best
  backup gets it (`KindBackup`, header flag `Backup`). If this member is
  itself the best backup, or it receives a `Backup`-flagged request while in
  the owner's cell, it serves the request (`KindSelf` with `Owner` not self
  and the `Backup` flag set), so backup hops never chain and a backup hop is
  always final. The receiver serves even when its own health view finds the
  owner usable: health views differ per member, and sending the request back
  to the owner the sender gave up on took D+1 hops and, with failovers,
  bounced between the owner and its backups until the TTL ran out.
- A caller that retries or hedges to `Alternate` sends
  `Decision.AlternateHeader()`: after a final hop (`KindOwner`,
  `KindBackup`) it adds the `Backup` flag, so the backup serves; after a
  relay hop it is the decision's header.
- Relay `Alternate` is the second best of the chosen cell, or the best of the
  next candidate cell. Health is ignored when no candidate cell has a usable
  member and when the tracker is in panic.
- Joining, Draining, and weight-0 members are routed to as owners and used
  as relays and backups like any other member.
- `PrevOwner` is set when the owner is the final hop (`KindSelf` as owner,
  `KindOwner`, `KindDirect`), `Prev` is a current member other than the
  owner, and `uint32(current epoch) - Moved` is in `[-HandoffEpochs,
  HandoffEpochs)` modulo 2^32 (the negative half covers a member whose epoch
  tracker lags the leader that stamped `Moved`).
- A non-member uses rendezvous over gateways
  (`Score(Mix64(KeyHashString(0, addr)), partition)`); without gateways it
  sends `KindDirect` to the owner if it has a configured view, otherwise
  `KindNoRoute`. Its TTL is `D + 3` (`MaxDims + 3` without a view) to cover
  the gateway hop.
- Header wire form is big-endian: format, flags, hops, TTL (1 byte each),
  partition, sender, term truncated to 32 bits (4 bytes each), seq, epoch
  (8 bytes each). Flags: `Hedge` (set by the caller, kept across hops),
  `Direct`, `Gateway`, `Backup`. `Forward` returns `ErrTTLExhausted` when the
  outgoing hop would make `Hops` reach `TTL`; serving locally is never
  refused. It never sends back to the header's sender unless that is the
  owner.

## Deterministic simulation testing

`internal/cachemesh/internal/dst` is the single simulation suite that
exercises every state of the library. It contains:

- a discrete-event scheduler (`int64` nanosecond clock, events ordered by time
  then sequence number, PCG sources derived from the run seed, one named
  stream per component so extra draws in one component do not perturb
  another),
- a network model: per-link latency with jitter, loss, duplication,
  reordering, slow members, and partitions between member groups,
- per-member clocks with offset and drift (the library only sees its own
  member's clock),
- an in-memory Lease server implementing `elect.Store` with
  compare-and-swap, latency, outages, and per-second QPS accounting,
- a process model: start, crash, restart with a new incarnation, graceful
  leave, pause (stop the world for one member),
- a data-plane workload: requests from random members for blob, striped-layer,
  and metadata keys with hot-key skew, routed hop by hop with `Route` and
  `Forward`, outcomes fed to `Observe`, optional hedging, and per-request
  latency recorded,
- invariant checkers and a state-coverage registry.

Safety invariants, checked continuously:

1. At most one member publishes deltas for any term; terms increase across
   acquisitions; the same version never has two digests. No two members may
   publish (`Leading` with `CanPublish` true) for different terms at the
   same instant while clocks are within the election's safety bounds (wall
   clocks within `ClockSkewAllowance` of each other, monotonic rates with
   `rmax/rmin < LeaseDuration/RenewDeadline`).
2. A member's view version never decreases and never accepts a lower term.
3. Every view passes `view.Validate`.
4. Each member's neighbor count stays within `MaxNeighbors` once cells are
   balanced, and within twice that during a reshape.
5. Every request terminates within its TTL; when all members on its path
   share a version, it visits no member twice, takes at most D hops plus one
   failover, and ends at the owner or a backup in the owner's cell.
   (Failovers do not add hops in the implementation, so the check is at
   most D hops.)
6. Epochs never decrease on a member; a stamp is never `Fresh` later than
   `T + period + drift slack` after it was issued. (The slack includes up
   to one period of epoch phase lag on members that anchored late; see the
   data-plane notes below.)
7. Lease API traffic stays within its budget: steady state at most
   `Candidates/RetryPeriod` plus the leader's renewals; during leader loss
   about `LotteryBudgetQPS`. Followers make no Lease calls in steady state,
   and a joiner makes one cached read at start and at most one per
   seed-refresh period after.

Liveness invariants, checked after faults heal and a settle period:

8. Exactly one leader; every live member converges to the same version.
9. The view's members are exactly the live processes.
10. Partition loads are within one of their targets; no cell is empty; the
    neighbor bound holds.
11. Sampled routes from every member reach the owner in at most D hops with no
    `Direct` decisions.

Scenarios are generated from the seed (swarm testing): cluster size, grid
parameters, and a random schedule drawn from crash, restart, mass join, scale
down, rolling restart, network partition and heal, leader crash, API server
outage, slow members, clock skew and drift, message loss and duplication,
graceful drain, keyspace invalidation, and lease conflicts. Every run is
executed twice and the event-trace hashes must match, which catches
nondeterminism in the library. A failing seed is printed with the command
that replays it. A coverage registry records states reached (member states,
elector states, health states, catch-up by delta and by snapshot, reshape,
handoff, panic, hedge, lottery, damping, and so on), and the suite fails if a
registered state is never reached across the default seed set.

Default `go test` runs a seed set that finishes in about a minute and includes
a 10k-member cluster, a scripted series of whole-cluster and partial
restarts, and two simulated hours of churn on a small cluster. A failure
prints a replay command: the subtest name for default-set cases,
`CACHEMESH_DST_SEED` for explicit seeds.

Implementation notes (control-plane suite, `dst.Cluster` and
`suite_test.go`): each member is a `node.Node` with its own `dst.Clock`,
ticked at its `NextDeadline`; outbox calls go through `Net.Call` and Lease
operations through `LeaseServer.Submit`. Large clusters start from a view
prebuilt by running the leader logic offline (`node.Config.Initial`), so the
first leader is elected over an existing view. The default set is 16 tiny
and 4 small generated scenarios and the targeted 40-member `slow` scenario
(each run twice and trace-compared), one 1,000-member and one
10,000-member scenario; only every fourth tiny/small
seed keeps `PartitionBits` 20, the rest use 16 to bound runtime. The default set also has
`restarts` (`GenerateRestarts`: a partial restart that kills the leader
and all but two members, then whole-cluster restarts with and without
seeds) and `longrun` (`GenerateLongRun`: two simulated hours of churn on
12 members, a fault every few minutes); both require convergence at
`Checkpoint` faults between their faults and finish in a few seconds.

Control-plane invariants (`dst/control.go`):

- Leader overlap (invariant 1) is checked whenever a member publishes. An
  overlap while clocks are outside the safety bounds is counted
  (`leader-overlap-excused`) instead of failing: generated `ClockSkew`
  faults step wall clocks by up to 60s, beyond `ClockSkewAllowance`, and a
  skewed lottery member can take a live Lease (fencing by term still
  holds). Generated `ClockDrift` stays within +-10%, inside the +-20% that
  the default durations tolerate, so drift alone never excuses an overlap.
- Lease calls are attributed per member and classified: a node's own seed
  reads must be at least 25s apart per process (16 join attempts of 2s on
  a clock up to 10% fast); elector calls in quiet periods (no fault within
  the last 90s) are at most `Candidates + 2` per second, at most
  `5*(Candidates+1) + Candidates + 1` per 10s window, made by at most
  `Candidates + 2` members, each of which was a candidate or the leader at
  one end of the window. In any second after cluster formation (30s after
  the first term) elector calls are at most `4*LotteryBudgetQPS +
  Candidates + 2`: one second can hold a lottery round's reads and the
  previous round's acquiring writes, a sum with mean `2*LotteryBudgetQPS`
  and standard deviation about 6, so the bound is more than six standard
  deviations above it.
- Term churn: after convergence, clusters of at most 512 members run 60s
  more and must start no new term, keep the same leader, and stay
  converged. A converged cluster has at most `2 + 4*faults` terms (plus one
  per `RetryPeriod` of injected Lease conflicts, each of which can fail a
  renewal and end a term) and a
  view sequence number no larger than the elapsed seconds.
- Bounded state: every 10s every member's `node.Stats` are checked against
  their caps (pending calls, ahead queue, seeds, delta log, leader queues),
  and no removed neighbor may linger past its expiry.
- Scenarios draw `ColdRestart` and `PartialRestart` (each in about one in
  eight) and the delta log size (default, 64, or 32, so partitioned
  members catch up by snapshot) from a separate random stream, so
  existing seeds keep their other faults. After a whole-cluster restart
  the epoch restarts at zero, so the liveness epoch check counts from the
  last bootstrap.
- `TestDSTSweep` runs a resumable sweep in parallel:
  `CACHEMESH_DST_SWEEP=tiny:1-300,small:1-150,restarts:1-40,longrun:1-30,medium:1-25`
  with `CACHEMESH_DST_SWEEP_OUT=<file>` (one line per case, each run twice
  and trace-compared; cases already in the file are skipped).

The data-plane workload plugs in through
`dst.Workload`. `CACHEMESH_DST_SEEDS`, `CACHEMESH_DST_SEED`, and
`CACHEMESH_DST_LARGE=1` (100k members) control longer runs.

Implementation notes (data-plane workload, `dst.DataPlane` in
`dataplane.go`): every `TestDST` case runs it from the end of the warmup.

- Routing state: each running member routes with its own `route.State`
  over a `route.Env` holding its node's `Health()` and `Epoch()` trackers,
  its own `route.Notes`, its configured seeds as gateways, and its ID and
  incarnation. The state is rebuilt (O(1)) when the member's view pointer
  or slot changed; members not in their view route through their seeds or
  `Direct`.
- Requests: keys come from `KeyGen` (blobs and striped layers with TTL 0,
  metadata in the keyspaces `meta/0` to `meta/3` with TTL 5m, Zipf 1.1).
  The cluster-wide rate is the member count clamped to 20..120 per second
  (100 for the 1,000- and 10,000-member cases). Half the requests come from
  up to four hot clients (heavy pullers) so that a client routes to each
  neighbor often enough for health to learn it; while a slow-member fault
  is active the clients issue as many requests again, so that their
  traffic to each neighbor outnumbers the re-probes of a Degraded peer
  (retried once its EWMA decays, about every 20s). Every 30s a herd of 160
  requests (320 for large cases) for the hottest blob arrives within 20ms,
  which overloads owners and relays. 0.2% of relayed requests are sent
  with a TTL of one hop, to exercise TTL exhaustion.
- Delivery: each hop is a `Net.Call` of kind `KindData` (60, above every
  control kind) carrying the 32-byte header. The receiver queues it
  (service time uniform in 0.5..1.5 times 300us, times the member's slow
  factor; more than 16 in flight is answered `Overload`), then calls
  `Forward` on its own state, acks, and serves (`KindSelf`) or sends on.
  The sender observes the ack latency as `Success`, or `Failure`,
  `Timeout` (hop timeout 250ms), or `Overload`, and then retries the
  `Alternate` once with `AlternateHeader`. A request abandoned by its
  sender is dropped by the receiver; a lost ack leaves two live branches,
  both tracked. The origin hedges to the `Alternate` (with `FlagHedge`)
  when its first hop is unanswered after `HedgeAfter` and `TryHedge`
  allows it; the first served branch wins. A receiver that sees a newer
  version in a header calls `node.NoteVersion` from `Notes.Take`, as
  `Mesh` does. Every terminal result goes into the trace hash.
- Phases: a request is steady when membership was settled at the last
  5-second check (a leader, every running member at its version and in it,
  no member paused) and no fault was active within 30s; otherwise it is a
  fault-phase request. Latency histograms are kept overall, per phase, for
  steady client requests (the tail baseline), and for judged slow windows.

Data-plane invariants (each failure names the seed and replay command):

- (4) Every 5s each member's `Neighbors()` count is at most `MaxNeighbors`
  (or the estimate of the best shape when no shape meets it) while its
  view's cell occupancies differ by at most one, and twice that otherwise.
- (5) Every request reaches a terminal state (served, failed, no route,
  TTL exhausted, or the origin's 5s deadline) and all its branches resolve
  within `(MaxDims+3) * 2 * hop timeout + HedgeMax`. A branch whose
  routers were all members at the same version visits no member twice,
  takes at most D hops (failovers do not add hops), never exhausts its
  TTL, and is served by the owner or a member of the owner's cell. No
  non-truncated request exhausts its TTL in steady state with no view
  change since a second before it started (TTL exhaustion while a
  partition move propagates, when the old and new owner each send to the
  other, is what the TTL is for). A deadline miss in steady state fails.
- (6) Epochs never decrease. Up to 256 sampled stamps of served metadata
  requests are checked every 5s on 8 random members: a stamp must not be
  `Fresh` on a member of its view later than `T + period + slack` after
  issue, with
  `slack = period + 2 * TickInterval + 1s + 2 * 0.05% of the age` (a member
  lags the issuer's epoch phase by at most the period of an anchor that
  arrived late, plus the leader's and its own tick, plus latency and
  drift). The issuer still finds it `Fresh` up to `T - period` after issue
  when no fault was active within 90s. A member whose view has a newer
  generation for the keyspace never finds an older stamp `Fresh`.
  Non-members are not held to the upper bound: one may have anchored its
  epoch from a stale lineage's snapshot (a restarted process that caught
  up from a partitioned peer) and lags until it hears the leader. A member
  without any view finds no expiring stamp fresh (`route.State.Fresh`).
- (11) After convergence, 8 random partitions per member (1 for the large
  cases) are walked with `RoutePartition` and `Forward` through every
  member's own state: at most D hops, no `Direct`, `Gateway`, or `NoRoute`,
  ending at the owner, or at a backup only when the deciding member finds
  the owner not `Healthy`.
- Ejections: `Ejected + Probing` never exceeds `MaxEjectionPercent` of the
  largest tracked count over the last 335s (the longest ejection), which
  is recorded on every neighbor change; peers ejected while more were
  tracked stay ejected when neighbors are removed.
- Hedges: each member's hedges are at most the hedge ratio times its
  routed decisions, plus one.
- Tail latency, judged on requests from clients that are members of their
  view, start at least 30s after a slow-member fault of at least 10x
  began, while membership is settled and slow members are the only
  data-plane fault (no other fault except clock skew or drift, API server
  outage, lease conflicts, or invalidation active within the last minute):
  outlier detection is relative to the median of a member's peers, so
  while most peers time out (partition, loss) a slow member is not an
  outlier, and crashed or paused peers take the few ejections
  `MaxEjectionPercent` allows. The requests that send a hop to a slow
  member are at most half of the expected count without protection (slow
  share of running members times the mean path length) when at least 20
  are expected; with at least 200 baseline and judged requests, p99 is at
  most the hop timeout plus three times the steady client p99 (at the 99th
  percentile a request pays at most one hop timeout), and p50 at most
  twice the steady p50 plus 1ms. The default set adds a targeted case
  (`slow`: 40 members, two 30x slow members for 150s, clock drift, an
  invalidation) so the check always runs.

Health variants rotate with the seed: seeds 2 mod 4 use
`MaxEjectionPercent` 60 and `PanicThreshold` 20 (with the defaults at most
10% can be ejected and panic needs more than 50%, so panic is
unreachable), and seeds 3 mod 4 use `HedgeRatio` 0.5 (the 5% default needs
about 20 routed requests per shard and interval before the first hedge,
more than simulated members route). The data-plane coverage states are the
seven decision kinds, the five health states, uneject, panic,
hedge sent, won, and denied, failover, TTL exhausted, handoff (`PrevOwner`
set), stamp expired and invalidated, note-version, overload, the route
check, and a judged tail window.

Known limits: health protection beyond the first hop depends on relays
seeing enough traffic to each neighbor, which simulated rates give only to
the hot clients, so p99 in slow windows is bounded by one hop timeout
rather than by a multiple of the baseline. An idle peer's EWMA decays only
toward the median, so after a burst of timeouts (message loss) a member's
median stays high until it observes its peers again.

## Defaults

| Setting | Default |
|---|---|
| `PartitionBits` | 20 |
| `MaxNeighbors` | 256 |
| `CellSize` | 4 |
| `EpochPeriod` | 15s |
| Minimum metadata TTL | 5m |
| `LeaderTick` | 1s |
| `MaxJoinsPerTick` | 256 |
| `QuarantineFor` / `WarmupStep` | 10s / 20s |
| `DrainFor` | 30s |
| `MaxMovesPerTick` | 4096 |
| `MaxCellMovesPerTick` | 32 |
| `HandoffEpochs` / `SlotReuseEpochs` | 8 / 16 |
| `MonitorInterval` / `PingTimeout` / `SuspectAfter` | 2s / 1s / 3 |
| `SuspicionWindow` / `ObserverCells` / `DampingFraction` | 30s / 3 / 10% |
| `NeighborLinger` | 30s |
| `DeltaLogSize` | 512 |
| `Candidates` / `MinCandidateAge` | 3 / 60s |
| `LeaseDuration` / `RenewDeadline` / `RetryPeriod` | 15s / 10s / 2s |
| `NoLeaderTimeout` / `LotteryRound` / `LotteryBudgetQPS` | 30s / 1s / 20 |
| Breaker | 5 failures, 30s base, 300s max, 10% max ejected, 50% panic |
| Hedge | 5% budget, `3 * EWMA` clamped to 5ms..1s |

## Open questions

- Zone-aware dimension ordering so hot keys cross zone boundaries once.
- Load-based rebalancing using owner-reported request rates.
- Snapshot chunking for very large views over transports with message limits.
