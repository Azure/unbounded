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
| `internal/cachemesh/memtransport` | In-process `ControlTransport` for tests and examples | `internal/cachemesh` |
| `internal/cachemesh/internal/hashing` | Key hashing, mixing, rendezvous scores | `zeebo/xxh3` |
| `internal/cachemesh/internal/topology` | Grid shapes, cells, neighbor cells, next-hop cells | `hashing` |
| `internal/cachemesh/internal/placement` | Partition table (chunked copy-on-write), leader rebalancer | `hashing` |
| `internal/cachemesh/internal/view` | Immutable membership view, deltas, apply, validation, digests, snapshots, delta log, interning | `hashing`, `topology`, `placement` |
| `internal/cachemesh/internal/epoch` | Rolling cluster epoch tracker, stamps | none |
| `internal/cachemesh/internal/health` | Per-neighbor breakers, outlier detection, peak-EWMA, hedge budget | none |
| `internal/cachemesh/internal/route` | Hot-path routing state, `Decision`, `Header` | `view`, `health`, `epoch`, `topology`, `hashing` |
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
possible across dimensions (sizes differ by at most one). Small clusters get
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

- A new member joins the least-occupied cell; ties go to the lowest
  `Score(member.Key, cell)` winner.
- Each tick, if the most and least occupied cells differ by more than one,
  the leader moves up to `MaxCellMovesPerTick` (32) members from the fullest
  to the emptiest cells, preferring members in `Joining` state.
- Reshape (all members get new cells) only when the current shape violates a
  hard bound: estimated neighbors with the maximum occupancy exceed
  `MaxNeighbors`, or average occupancy falls below `max(1, CellSize/2)` with
  more than one cell, or rises above `4*CellSize`. Reshape assigns cells in
  order of `Member.Key` round-robin. Removed neighbor connections linger for
  `NeighborLinger` (30s) so in-flight requests finish.

## Placement: the partition table

- `P = 2^PartitionBits` partitions (default 20 bits, 1,048,576). Every member
  of a cluster must use the same value; it is part of the view and a joiner
  with a different value is rejected.
- `placement.Table` maps partition to `Entry{Owner, Prev uint32; Moved
  uint32}` (`Moved` is the epoch of the last move). It is stored in 1024-entry
  chunks, copied on write, so a delta that moves k partitions copies at most k
  chunks.
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
   (ties by `Member.Key`). If the previous owner is still a member, set `Prev`.
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

`view.Validate(v)` checks every structural invariant (owners are members,
cells match members, occupancy counts, slot uniqueness, shape bounds). Tests
and the simulator call it on every view they build.

Every node keeps a `DeltaLog` of the last `DeltaLogSize` (512) deltas so it
can serve catch-up. `view.Interner` lets the simulator share identical views
(keyed by digest) across simulated nodes; production uses no interner.

`view.Snapshot` encodes a whole view; `view.Observers(v, slot)` and
`view.Subjects(v, slot)` give the monitoring relation (below).

## Rolling cluster epoch

- The leader owns `EpochPeriod` (default 15s) and publishes an `Epoch` op
  every period as a heartbeat.
- `epoch.Tracker` maps local monotonic time to an epoch:
  `current = anchorEpoch + floor((now - anchorMono) / period)`, re-anchored on
  every heartbeat received. It never decreases. A member cut off from the
  leader keeps advancing on its own clock, so expiry is never suspended.
- `Tracker.Current()` is an atomic load. The runtime calls
  `Tracker.Advance(now)` on every tick (default every 100ms); the request path
  never reads the clock.
- A keyspace with TTL `T` (zero means never expires, otherwise
  `T >= 5m`) stamps values at the owner with
  `Stamp{Expires: current + ceil(T/period) + 1, Gen: generation}`. Copies
  keep the owner's stamp. `Fresh(stamp)` is `current < Expires && Gen ==
  generation`. A value therefore lives between `T` and
  `ceil(T/period)*period + period` on members whose clocks match the
  leader's, plus clock drift between heartbeats: `T + period` when `T` is a
  multiple of the period, and always less than `T + 2*period`.
- `Invalidate(keyspace)` asks the leader to bump the generation, which makes
  every existing stamp of that keyspace stale.

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
  interval using sharded counters.

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
clamped to the number of distinct observer cells that exist) distinct cells
have reported it within `SuspicionWindow` (30s). Removals are capped at
`max(1, N/1000)` per tick; if more than `DampingFraction` (10%) of members are
suspected at once, the cap drops to `max(1, N/10000)` per tick so a partition
cannot empty the cluster quickly.

## Leader and control plane

### Messages

Every control message carries the sender's `MemberRef{ID, Incarnation,
Slot}`, `Version`, and `Epoch`. All are encoded by `internal/wire`.

| Message | Use |
|---|---|
| `Ping` / `Pong` | Monitoring, version anti-entropy. |
| `Broadcast{Delta, Level}` | Delta dissemination along the grid tree. |
| `CatchUpRequest{Have, HaveDigest}` | Ask a peer for missed deltas. |
| `CatchUpResponse{Deltas}` or `{Snapshot}` | Deltas from the peer's log if it holds `Have`, otherwise a snapshot. |
| `Routed{Target, TTL, Payload}` | Relay a message to a member (usually the leader) hop by hop over the grid. |
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
  `p = min(1, LotteryBudgetQPS * LotteryRound / N)`, where N is the member
  count of its last view or `ExpectedClusterSize`. An eligible member reads
  the Lease once: if a live leader holds it, the member records the leader's
  address and seeds and catches up from a seed; if it is expired by more than
  `LeaseDuration + ClockSkewAllowance` (5s) of wall time, the member tries to
  take it. This bounds API traffic during leader loss at about
  `LotteryBudgetQPS` (20/s) across the whole cluster.

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
func (m *Mesh) RouteHash(ks Keyspace, h uint64) Decision
func (m *Mesh) RouteMany(ks Keyspace, keys [][]byte, out []Decision)
func (m *Mesh) Forward(h Header) (Decision, error)  // at a relay
func (m *Mesh) Observe(p PeerRef, r Result, latency time.Duration)
func (m *Mesh) TryHedge(h Header) bool
func (m *Mesh) Stamp(ks Keyspace) Stamp
func (m *Mesh) Fresh(ks Keyspace, s Stamp) bool
func (m *Mesh) Invalidate(ks Keyspace)
func (m *Mesh) ServeControl(ctx context.Context, from string, msg []byte) ([]byte, error)
func (m *Mesh) Run(ctx context.Context) error
func (m *Mesh) Leave(ctx context.Context) error
```

`Header` is a fixed 32-byte value: format version, flags, hop count, TTL
(`D + 2`), partition, sender slot, the sender's term, seq, and epoch.
`Forward` rejects a header whose TTL is exhausted, notes when the sender's
version is ahead (the control plane then catches up from it), and otherwise
computes the next hop from the relay's own view. `Observe` reports outcomes:
`Success`, `Failure`, `Timeout`, `Overload`.

`ControlTransport.Send(ctx, to Endpoint, msg []byte) ([]byte, error)` is the
only control-plane transport method. `NeighborWatcher.OnNeighbors(added,
removed []PeerRef)` tells the caller which data-plane connections to keep;
removals are delayed by `NeighborLinger`.

Per-request cost targets (benchmarked): `Route` on a 100k view under 100ns and
zero allocations; `Observe` under 50ns and zero allocations; `Fresh` a few ns.

## Deterministic simulation testing

`internal/cachemesh/internal/dst` is the single simulation suite that
exercises every state of the library. It contains:

- a discrete-event scheduler (`int64` nanosecond clock, events ordered by time
  then sequence number, one seeded PCG source per run),
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
   acquisitions; the same version never has two digests.
2. A member's view version never decreases and never accepts a lower term.
3. Every view passes `view.Validate`.
4. Each member's neighbor count stays within `MaxNeighbors` once cells are
   balanced, and within twice that during a reshape.
5. Every request terminates within its TTL; when all members on its path
   share a version, it visits no member twice, takes at most D hops plus one
   failover, and ends at the owner or a backup in the owner's cell.
6. Epochs never decrease on a member; a stamp is never `Fresh` later than
   `T + period + drift slack` after it was issued.
7. Lease API traffic stays within its budget: steady state at most
   `Candidates/RetryPeriod` plus the leader's renewals; during leader loss
   about `LotteryBudgetQPS`.

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
a 10k-member cluster. `CACHEMESH_DST_SEEDS`, `CACHEMESH_DST_SEED`, and
`CACHEMESH_DST_LARGE=1` (100k members) control longer runs.

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
