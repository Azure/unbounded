# cachemesh protocol

## Status

Implemented in `internal/cachemesh` (routing library) and
`internal/gantry/meshfetch` (the gantry data path built on it). Gantry's use
is optional and off by default. The code is authoritative; when this
document and the code disagree, fix whichever is wrong and say so in the
change.

## Scope

cachemesh is a Go library that makes routing decisions for a distributed,
groupcache-style cache spread across up to 100,000 processes. It decides:

- which member owns a key (and which members back it up),
- which neighbor a request should be sent to next,
- which neighbors a process should keep connections to,
- when a cached metadata value has expired,
- which members are in the cluster, and who coordinates changes to that set.

The library does not move bytes, cache values, or open connections. The
embedding application owns storage, transports, caching, and the requests
themselves. Gantry is the first user: it stripes image blobs into chunks
owned by individual agents and caches tag resolutions. The second half of
this document ([Gantry data path](#gantry-data-path)) specifies that use.

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

Design choices: placement is a leader-assigned partition table rather than
rendezvous over all members (O(N) per lookup), a hash ring (memory for
balance), or jump-style hashing (depends on update order). The leader
already exists, and a table gives exact balance, slow start, draining, and
O(1) lookups. Membership changes are leader-ordered deltas spread over the
grid rather than flat gossip, which converges too slowly at 10^5 members.

## Architecture

```
          +---------------------------- cachemesh.Mesh -----------------------------+
          |                                                                         |
 Route ---+--> atomic.Pointer[route.State] --> view, health, epoch (lock-free)      |
 Forward  |                                                                         |
 Observe -+--> route.HealthTracker (atomics)                                        |
          |                                                                         |
 ServeControl --> inbox --> control.Node (single goroutine, deterministic) --> outbox --> ControlTransport
          |                    |  control.Elector --> LeaseOp --> kubelease.Store   |
          |                    |  control.Leader (when elected) --> deltas          |
          |                    |  control.Router --> route.State                    |
          +-------------------------------------------------------------------------+
```

The control plane is a deterministic, single-threaded state machine
(`control.Node`) with no I/O, no goroutines, no wall clock, and no global
randomness. Everything it needs comes in through method arguments (time,
messages, lease results, a seeded random source) and everything it does comes
out through an outbox. `cachemesh.Mesh` is a thin concurrent runtime around it.
The deterministic simulation test suite (`internal/dst`) drives thousands of
`control.Node` instances directly on a simulated clock and network.

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
| `internal/cachemesh` | Public API (`Mesh`, `Config`, aliases of internal types), concurrent runtime | `control`, `route`, `view` |
| `internal/cachemesh/kubelease` | `control.Store` over client-go `coordination/v1` Leases | `internal/control` |
| `internal/cachemesh/memtransport` | In-process `ControlTransport` and `LeaseStore` for tests and examples | `internal/cachemesh` |
| `internal/cachemesh/internal/view` | Immutable membership view, deltas, apply, validation, digests, snapshots, delta log, interning; the grid (shapes, cells, next hops, `Choose`, cell assignment), the partition `Table` (chunked copy-on-write), hashing (key hashing, mixing, rendezvous scores), and the canonical binary encoding | `zeebo/xxh3` |
| `internal/cachemesh/internal/route` | Hot path: routing `State`, `Decision`, `Header`, `Notes`, the per-neighbor `HealthTracker` (breakers, outlier detection, peak-EWMA, hedge budget), and the `EpochTracker` and stamps | `view` |
| `internal/cachemesh/internal/control` | Control plane: the per-member `Node` state machine, Lease election (`Elector`, `Store`), the `Leader` (admission, suspicion, removal, rebalance with the partition `Planner`, cells, epochs), wire messages, and the `Router` that keeps a member's `route.State` current | `route`, `view` |
| `internal/cachemesh/internal/dst` | Deterministic simulator and the DST suite | `control` and below |
| `internal/cachemesh/internal/view/viewtest` | View builders for tests | `view` |
| `internal/gantry/meshfetch` | Gantry chunk and tag transport, chunk store, holder directory | `internal/cachemesh` |
| `internal/gantry/mirror` (`cachemesh.go`) | Mirror integration: serve blobs and tags through the mesh | `meshfetch` interfaces |

## Identity and hashing

- `MemberID` is a string supplied by the application, unique per member (for
  example the Kubernetes node name). `Incarnation` is a `uint64` chosen at
  process start (random, nonzero); a restart with the same ID gets a new one.
- `Member.Key = view.MemberKey(id, incarnation)` is a 64-bit hash used in
  every rendezvous score. Scores never use slot numbers, so they do not change
  when slots are reused.
- `Slot` is a dense `uint32` index assigned by the leader for the life of a
  member record. `NoSlot = ^uint32(0)`. A freed slot is not reused for
  `SlotReuseEpochs` (default 16) epochs, which exceeds the handoff window.
- `view.KeyHash(seed uint64, key []byte) uint64` is `xxh3.HashSeed`.
  A keyspace seed is `view.Seed(name)`; each keyspace hashes into the same
  partition table with a different seed, so blob and metadata placement are
  independent.
- `view.Partition(h uint64, bits uint8) uint32` takes the top `bits` bits.
- `view.Mix64` is the SplitMix64 finalizer. `view.Score(a, b uint64)`
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

`view.Choose(n, cellSize, maxNeighbors)` returns the shape with the
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
- `view.Table` maps partition to `Entry{Owner, Prev uint32; Moved
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
  rebalancer only if the leader lists it (`control.PlanMember.Gone`).
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
- `view.Shape`, `PartitionBits`, `view.Table`.
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
  every period as a heartbeat. The period is fixed for a lineage: the first
  `Epoch` op sets it, and later leaders republish the view's period even when
  their own `EpochPeriod` differs. Stamps count epochs, so a period change
  would rescale every live stamp (a longer period would keep values fresh
  past their TTL). A new `EpochPeriod` takes effect only in a new lineage
  (a whole-cluster cold restart).
- `route.EpochTracker` maps local monotonic time to an epoch:
  `current = anchorEpoch + floor((now - anchorMono) / period)`, re-anchored on
  heartbeats received. It never decreases. A member cut off from the
  leader keeps advancing on its own clock, so expiry is never suspended.
- An anchor never moves the phase backward: a heartbeat (or snapshot)
  whose epoch is at or below the epoch the tracker already computes, with
  the same period, is ignored. Heartbeats are applied up to a period after
  their epoch began (catch-up deltas and snapshots replay old `Epoch` ops),
  so re-anchoring on them would delay epoch boundaries and stretch value
  lifetimes. Only an anchor ahead of the member re-anchors it.
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

`route.HealthTracker` holds one `Peer` per slot with atomic fields. Defaults follow
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

Implementation notes (`route.HealthTracker`): the per-tracker slot index is a small
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
`2D + 1` cells. Monitors ping each subject every `PingInterval` (2s) with
`PingTimeout` (1s); the ping also carries the sender's version and epoch, so
it doubles as anti-entropy. After `SuspectAfter` (3) consecutive failed pings
the monitor sends `Suspect` to the leader, repeated every `SuspectRefresh`
(5s) while it persists.

The leader removes a member once observers from at least `ObserverCells` (3,
clamped to the number of distinct cells of its observers that are not
themselves suspected) distinct cells have reported it within
`SuspicionWindow` (30s). Excluding suspected observers lets the leader remove
two failed members that observe each other. Removals are capped at
`max(1, N/1000)` per tick; if more than `DampingPercent` (10%) of members are
suspected at once, the cap drops to `max(1, N/10000)` per tick so a partition
cannot empty the cluster quickly.

## Leader and control plane

### Messages

Every control message carries the sender's `MemberRef{ID, Incarnation,
Slot}`, `Version`, `Digest`, `Epoch`, and the slot of the leader it knows
(`NoSlot` if none). All are encoded by `internal/control`. Every message is a
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

### Wire format

Control messages use a small versioned binary codec
(`internal/cachemesh/internal/control/wire.go`), not protobuf: the codec is
allocation-free for the frequent kinds and is fuzz tested. Every message is
an envelope followed by a kind-specific body:

```
u8      format version (FormatVersion = 2)
u8      kind
ref     From          (string ID, uvarint Incarnation, u32 Slot)
string  FromAddr
uvarint Version.Term, uvarint Version.Seq
[32]    Digest
uvarint Epoch
u32     LeaderSlot    (NoSlot if unknown)
...     body
```

- A message with any other format version is rejected (`ErrVersion`).
  There is no negotiation: every member of a cluster must run the same
  format.
- `FromAddr` is the sender's advertised control address (`Config.Addr`).
  Receivers dial it, never the transport-level source address, which may be
  an ephemeral port.
- Kinds: `Ping` 1, `Pong` 2, `Ack` 3, `Nack` 4, `Broadcast` 5,
  `CatchUpRequest` 6, `CatchUpResponse` 7, `Routed` 8, `Join` 9, `Leave` 10,
  `Suspect` 11, `Invalidate` 12.
- `Nack` reasons: `TTLExhausted` 1, `NoRoute` 2, `NotLeader` 3, `Busy` 4,
  `BadRequest` 5.
- `Broadcast.Level` is a dimension index, `-1` (sent by the leader), or `-2`
  (cell-mate fan-out).
- Deltas and snapshots are opaque byte strings in the view package's
  canonical encoding.
- Limits: a whole message at most `MaxMessageSize` (64MiB, snapshots
  included), a `Routed` inner message at most 64KiB, at most 4096 deltas per
  `CatchUpResponse`.

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

Implementation notes (`control.Leader`):

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

Implementation notes (`control.Node`):

- `Routed` messages are relayed straight to the leader address the relay
  knows (from its view, the Lease, or a `Pong` hint) rather than hop by hop
  over the grid; `TTL` (4) bounds relay chains while leaders change.
- A member with no `Config.Seeds` issues one cached Lease read at start.
  Until it is answered the elector is held as a follower, so a starting
  member makes exactly one Lease call. If it finds no Lease, or a released
  one without seeds, it bootstraps: it takes part in the lottery until it
  has a view (not as a candidate, so members that all start at once without
  seeds make about `LotteryBudgetQPS` Lease calls rather than one each per
  `RetryPeriod`; set `ExpectedSize` near the real size, since a
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
  known member leaves, so a fresh member cannot bootstrap a second cluster
  next to the live one.
- A leaving member that has been in a view never rejoins; if it was
  already removed when `Leave` was called, it has left at once (it would
  otherwise wait forever for a view showing a removal that already
  happened). A join the leader had already queued can still admit a member whose
  process has stopped; failure detection removes it. A process asked to
  leave before it ever joined still joins: its admission replaces a stale
  record of its earlier incarnation, which nothing else removes (the new
  process answers pings at the same address), and the leave then drains
  it.
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
  Messages are not authenticated, so a term in a message is a claim that
  only the Lease can confirm (see Lease corroboration below). A member that
  sees a term above its view's records it (`Elector.ObserveTerm`), so its
  next acquisition and any release start above it. A higher term in a
  message never makes a leader step down: a leader stops publishing only
  when its own Lease renewals fail (`RenewDeadline`, or a conflicting
  write), or when it applies a view of a later term that the Lease shows,
  and then it releases the Lease (`resign`) as before. The release record
  carries the highest term the member has seen, so the next holder starts
  above both lineages even if the Lease term annotation was lost (read as
  0). Terms at or above `control.MaxSaneTerm` (2^62), or more than
  `control.MaxTermLead` (2^32) above the highest term the member has read
  from or written to the Lease (`Elector.LeaseTerm`), are garbage and
  ignored, so messages cannot push terms toward overflow: each real
  acquisition raises the Lease term by at most `MaxTermLead`.
  `NoteVersion`, fed from data-plane headers whose term is truncated to
  32 bits, may trigger a catch-up but never a step-down, so terms that
  differ only above bit 32 cannot cause a regression.
- Lease corroboration. The Lease is the only source of truth for who leads
  at which term. A member's view is *backed* when the member leads its
  term, or when the member's last Lease read named the view's leader (ID and
  incarnation) at the view's term. A member with a backed view does not
  adopt a view (delta or snapshot) of a later term unless its last Lease
  read shows that term, and does not catch up from the peer that claimed
  it; it reads the Lease instead (`Elector.Verify`, a consistent `Get`, at
  most once per `LeaseDuration`, never by the leader, whose renewals
  already check the Lease). If the read names a later term, the member
  catches up from the address in the Lease. A member whose view is not
  backed (most followers, which never read the Lease) catches up from the
  sender as before, but what it adopts is checked by the snapshot trust
  rules below. When such a member receives a delta of a term below its
  view's, two terms are live and one may be forged, so it also reads the
  Lease; if the Lease contradicts the view (another leader at the view's
  term, or a lower term), it re-fetches from the Lease holder and adopts
  the holder's snapshot even at a lower version. A leader asked for a
  catch-up by a member that claims to be ahead of it (in its own term or a
  later one) answers with its snapshot. Once rolled back, the member's view
  is backed, so later forgeries of a higher term are refused. Followers
  that never see a conflict make no Lease calls. Forging deltas within the
  current term is not prevented here; that needs transport authentication.
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
  encoding, so a corrupt or forged one decodes, and the leader it names and
  the responder's identity are both claims of the responder. A snapshot is
  trusted only when the member's last Lease read names the snapshot's
  leader at the snapshot's term and that leader served it. Any other
  snapshot, including one from a responder that claims to lead it without
  the Lease saying so, is *unconfirmed* until a delta applies on
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
  term (a forged snapshot claimed a version that was never published),
  unless its view is backed by the Lease. This, and the Lease rollback
  above, are the only cases in which a view version decreases.
- Bounded state. Outbound calls awaiting a reply are capped at
  `MaxPendingCalls` (4096); further calls are dropped and their senders
  retry on their own timers. Seeds are capped at 32 (64 from the Lease),
  queued ahead deltas at `MaxAhead`, the delta log at `DeltaLogSize`, the
  leader's input queues at `MaxPending`; neighbors and subjects follow the
  view. `Node.Stats` reports every size.
- A joiner that has seeds and holds no configured view stays a follower
  (the elector is told it has a view and has just heard from the leader),
  so it enters the lottery only through a cold start (above); otherwise a joiner with a skewed wall clock takes a live leader's Lease and
  gives it back, forcing an election each time. A joiner that holds a
  configured view instead reports when it last applied a new version
  (heartbeats included): while it is in contact with a live cluster that
  stays recent, and if every member of its lineage is gone (all restarted
  with new incarnations before a leader could admit any) it enters the
  lottery after `NoLeaderTimeout` and the winner admits itself into the
  existing lineage, keeping its epoch and keyspace generations.
- When the Lease (as read by the elector) names a leader of a later term
  than the view, the member catches up from that leader. This heals two
  lineages left by a partition. Requests routed to the leader (`Join`,
  `Leave`, `Suspect`, `Invalidate`) go to the Lease holder's address when
  the Lease does not back the member's view.
- A failed `Broadcast` forward is retried once with the next-ranked member
  of the same cell.
- Elector and health ticks run every `TickInterval` (1s).
- `control.Shared` (simulation only) shares interned views, encoded deltas,
  snapshots, and candidate lists across nodes in one process.

## Leader election

`control.Elector` is a state machine. It emits `LeaseOp`s (`Get`, `GetCached`,
`Create`, `Update`) for the runtime to execute against an `control.Store`, and
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

- **Follower** (almost every member): no API calls, except a rate-limited
  `Verify` read when a peer's claim conflicts with its view (see Lease
  corroboration).
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
  of the member count of its last view and `ExpectedSize`. An eligible member reads
  the Lease once: if a live leader holds it, the member records the leader's
  address and seeds and catches up from a seed; if it is expired by more than
  `LeaseDuration + ClockSkewAllowance` (5s) of wall time, the member tries to
  take it. This bounds API traffic during leader loss at about
  `LotteryBudgetQPS` (20/s) across the whole cluster. The floor on N keeps
  a member whose view is much smaller than the cluster (a fresh lineage
  after a cold restart) from reading every round.

`Elector.ObserveTerm` records terms seen outside the Lease (see the node
notes; the node drops terms more than `MaxTermLead` above
`Elector.LeaseTerm`, the highest term read from or written to the Lease);
the next acquisition uses `max(record.Term, view.Term, observed) + 1`, a
record term at or above `MaxSaneTerm` reads as 0, and `Release` writes the
highest term seen into the released record. `Elector.Verify` issues one
consistent read outside the candidate and lottery states; its result only
updates the known holder (a released, missing, or expired Lease names
none) and never starts an acquisition.

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
than their view's term, and members whose view the Lease backs reject
deltas of a later term than the Lease shows. A leader that loses the Lease stops publishing before
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

The route package keeps the per-request work small: when `route.HealthTracker.AllHealthy`
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
`LeaseStore` (`control.Store`), `LeaseRecord`, `ErrLeaseNotFound`,
`ErrLeaseConflict`, `ElectionConfig` (`control.ElectionConfig`), and `HealthConfig`
(`route.HealthConfig`).

`NewKeyspace(name, ttl)` returns a `Keyspace` value with `Name`, `TTL`,
`Seed` (`view.Seed(name)`), and `Hash(key)`. A zero TTL never expires;
otherwise the TTL must be at least `MinTTL` (5m). `MustKeyspace` panics on
error.

`Config` has the required `ID`, `Addr`, and `Transport`, then `Incarnation`
(zero picks a random nonzero value from `crypto/rand`), `Zone`, `Seeds`,
`Lease` (may be nil: the member then joins only through seeds and never
bootstraps or leads), `Neighbors`, `Logger` (`log/slog`), `Hooks`
(`OnView`, `OnLeadership`, `OnControlError`, `OnLeaseError`), and the common
cluster knobs `PartitionBits`, `CellSize`, `MaxNeighbors`, `EpochPeriod`, and
`ExpectedSize`. `ExpectedSize` is the lottery floor on N, so it
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
`Election.OpTimeout`), the leader knobs (admission, quarantine, warmup,
drain, suspicion, move limits, slot reuse, handoff), and the nested
`Election` and `Health` settings. `Tuning` is an alias of `control.Tuning`
and is embedded in the node configuration as is. Zero values select defaults; `New` returns
validation errors.

Runtime (`Mesh`):

- `New` builds the `control.Node` and a `control.Router`, which keeps a
  `route.Env` holding the node's health
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
  queue, so every hook may call Mesh methods. The queue is bounded: since
  the last leadership event it holds at most one neighbor event, the net
  change since the last delivered call (a peer added and removed again
  meanwhile is never reported), and one view event, the latest, after it. `OnControlError` runs on the failed call's goroutine and
  `OnLeaseError` on the Lease goroutine, neither under the lock.
- `ServeControl` rejects messages over `MaxControlMessageSize`
  (`control.MaxMessageSize`) with `ErrMessageTooLarge`; other malformed input
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
holds the shared `route.HealthTracker`, `route.EpochTracker`, `Notes`, the gateway
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
  to the owner the sender gave up on can bounce between the owner and its
  backups until the TTL runs out.
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

## Gantry data path

Gantry uses cachemesh to move image content between agents. The library
only decides routes; gantry owns the HTTP transport, the chunk store,
coalescing, retries, and metrics. Code: `internal/gantry/meshfetch`,
`internal/gantry/mirror/cachemesh.go`, `internal/gantry/origin/tag.go`,
`internal/gantry/config/cachemesh.go`, `internal/gantry/transfer`
(`WithExtraRoutes`), and `cmd/gantry/agent_cachemesh.go`.

Goals:

- Pull each chunk of a blob from the origin registry once per cluster, not
  once per node.
- Spread the load of a large image over every node: each node owns some
  chunks and serves them to the others.
- Stream bytes through every hop as they arrive, with bounded memory.
- Leave the default (DHT peer-to-peer) path unchanged when disabled, and use
  it as the fallback when the mesh cannot serve a request.

### Configuration

All settings live under `cachemesh:` in the agent config
(`internal/gantry/config/cachemesh.go`). Nothing changes unless `enabled` is
true.

| YAML key | Env (`GANTRY_` prefix) | Default | Meaning |
|---|---|---|---|
| `enabled` | `CACHEMESH_ENABLED` | `false` | Turn the path on. |
| `namespace` | `CACHEMESH_NAMESPACE` | `chair_namespace` | Namespace of the leader Lease (named `gantry-cachemesh`). |
| `peer_key_file` | `CACHEMESH_PEER_KEY_FILE` | none (required) | Absolute path of the shared peer keys, one per line, at least 32 bytes each; the first signs, all verify. See Security. |
| `chunk_size` | `CACHEMESH_CHUNK_SIZE` | 64MiB | Stripe size; power of two, 1MiB to 1GiB, same on every node. |
| `expected_size` | `CACHEMESH_EXPECTED_SIZE` | 0 (library: 1000) | Expected node count. |
| `max_neighbors` | `CACHEMESH_MAX_NEIGHBORS` | 32 | Data-plane neighbors per node; at least 1. |
| `content_root` | `CACHEMESH_CONTENT_ROOT` | `/var/lib/containerd/io.containerd.content.v1.content` | containerd content store directory, mounted read-only; the only disk tier. Empty or unreadable: serve peers from memory only. |
| `inflight_bytes` | `CACHEMESH_INFLIGHT_BYTES` | 2GiB | Memory fills borrow for lagging readers, recent relay chunks, and owned chunks. |
| `max_concurrent_chunk_fills` | `CACHEMESH_MAX_CONCURRENT_CHUNK_FILLS` | 64 | Chunks fetched at once. |
| `cache_tags` | `CACHEMESH_CACHE_TAGS` | `false` | Serve anonymous tag requests from the mesh. Trusts every peer-key holder to resolve tags honestly. |
| `verify_digest` | `CACHEMESH_VERIFY_DIGEST` | `true` | Hash full blobs before the last byte. |

Validation requires a Lease namespace and `pod_ip`. The Racer backend
ignores the block. Unknown keys fail at load time (`KnownFields`), and the
chart schema rejects unknown values.

The rest are fixed defaults in `meshfetch` (`tuning` in
`internal/gantry/meshfetch/node.go`): 1MiB pages, a fill window of 4 pages,
512 concurrent chunk responses to peers (more get 429), 8 chunks of one
blob fetched ahead, 5m per fill attempt, 30s without progress fails an
attempt, 4 attempts per chunk, hedging on, 30s linger for unused chunks in
memory, and a 5m tag TTL (`cachemesh.MinTTL`). The grid cell size and
partition count use the cachemesh library defaults.

Grid shape: gantry defaults `max_neighbors` to 32, unlike the library
default of 256, which is sized for 100k members. With a few hundred nodes
and 256 neighbors, every node is a neighbor of every other node, so the
owner of a popular chunk serves every node itself. With 32 the grid has
more than one dimension, so requests relay through intermediate nodes that
cache the chunk on the way and spread the fan-out. Set 256 for a fully
direct grid.

The Helm chart exposes the same knobs as `gantry.cachemesh.*` (camelCase,
except `namespace`). Zero values are omitted, so the agent defaults apply.
It renders the block, and the `GANTRY_NODE_NAME` env var, only when
`gantry.cachemesh.enabled` is true. Example:

```yaml
gantry:
  cachemesh:
    enabled: true
    expectedSize: 150
resources:
  agent:
    limits:
      memory: 8Gi
```

### Membership

Each agent creates one `cachemesh.Mesh`:

- ID: the Kubernetes node name (`GANTRY_NODE_NAME`), else `POD_NAME`, else
  the hostname.
- Addr: `<pod IP>:<transfer port>`. Control and data requests share the
  transfer listener (default port 5001).
- Lease: `kubelease.Store` on the leader Lease. The agent Role already allows
  get, list, create, and update on Leases.
- Control transport: HTTP/1.1 `POST /cachemesh/control`, body bounded by
  `cachemesh.MaxControlMessageSize`.

`Run` starts after the transfer listener is up. Agent readiness does not wait
for the mesh; the mirror simply declines to use it until `Mesh.Ready()`. On
shutdown the agent calls `Leave` (5s budget) before the transfer listener
stops, so other members take over its partitions while it can still serve.

### Chunks and keys

A blob of size S is split into `ceil(S / chunk_size)` chunks; the last one
may be short. Chunk keys live in the immutable keyspace `gantry-blob`
(TTL 0). The routing key is the kind byte (0 blob, 1 manifest), the digest
string, and the chunk index as 8 big-endian bytes (`chunkKey.routeKey` in
`store.go`), so chunks of one layer land on different
owners.

Manifests by digest use the same path with the manifest kind. They are small
and fit in one chunk; the owner keeps the upstream Content-Type.

The owner fills a chunk with a bounded `PullRange` request
(`origin.Client.PullRange`). The first response tells every hop the blob
size, so there is no separate HEAD request.

### Peer HTTP API

All three routes are registered on the transfer listener with
`transfer.WithExtraRoutes`, outside the otelhttp wrapper. Every request
carries the `Gantry-Cachemesh-Auth` signature (see Security); a request
without a valid one gets 401 before any other processing.

`POST /cachemesh/control`: body is one encoded control message, at most
`cachemesh.MaxControlMessageSize`; the response body is the encoded reply
(`application/octet-stream`). 503 when the mesh is not running, 400 for a
bad message.

`GET /cachemesh/chunk` query parameters:

| Param | Meaning |
|---|---|
| `r` | Origin registry. |
| `n` | Repository. |
| `d` | Blob digest. |
| `k` | Kind: 0 blob, 1 manifest. |
| `i` | Chunk index. |
| `cs` | Sender's chunk size; must equal the receiver's, else 400. |
| `o` | Optional byte offset within the chunk (resume). |
| `peek` | Optional. `local`: serve only from memory or the content store. `owner`: as `local`, but an owner in its own view also starts its origin fill and serves it. |

Request headers: `Gantry-Cachemesh-Route` carries the 32-byte route header
(base64url); `Gantry-Cachemesh-Requester` carries the member address of the
node whose own containerd pull started the request, and relays forward it.

Response: 200 with `Gantry-Blob-Size`, `Content-Length` of the rest of the
chunk, and the upstream `Content-Type` when known. When the upstream sent
none, the header is omitted (not sniffed) so the requester also records an
empty type and the mirror applies its own fallback. Errors: 400 bad request,
404 (peek miss), 416 offset beyond chunk, 429 shed (more than 512
concurrent responses), 503 no route, 508 TTL exhausted or loop, 502 other
fill errors. After the status line is sent, a failure aborts the response
so the peer sees a short body.

`GET /cachemesh/meta?r=<registry>&n=<repo>&t=<tag>` with the route header
returns the manifest bytes with `Content-Type` (omitted when the upstream
sent none, as above), `Docker-Content-Digest`, `Gantry-Stamp-Expires`, and
`Gantry-Stamp-Gen`.

### Request flow

On a mirror GET for a digest that is not in the local containerd store (and
on open-ended `Range: bytes=N-` requests, which containerd uses to resume):

1. If the mesh is not ready, the mirror uses its default path.
2. The node routes chunk `N / chunk_size` with `Mesh.Route` and waits for
   the blob size. If that fails, nothing has been written and the mirror uses
   its default path (peers, then origin).
3. The mirror writes the status line and headers (200, or 206 with
   `Content-Range`).
4. A producer keeps 8 later chunks in flight; the writer copies chunks to
   containerd in order as their bytes arrive.
5. A fill retries its sources up to 4 times in all, resuming at the byte it
   reached (`Node.route` in `fetch.go`). If the writer's reader loses bytes
   the fill no longer holds (`errLost`), it obtains a new reader, which may
   start a new fill, and continues from the same byte, up to 4 times
   (`Node.copyChunk`). A failed fill is not retried again by the writer.
   After that the response is aborted (`http.ErrAbortHandler`) and
   containerd retries with a Range request, which goes through the same
   path.

Every node handles a chunk request the same way, whether it came from its
own mirror or from a peer:

- If the chunk is in memory (a fill in progress, or a complete chunk
  lingering), stream it from there. Concurrent requests for one chunk share
  one fill.
- Otherwise start a fill. If the local containerd content store holds the
  whole chunk (a committed blob, or an ingest containerd is still writing),
  the fill reads it from there. Otherwise the fill is routed. The decision comes from `Route` (local request) or
  `Forward` with the request header (peer request):
  - `KindSelf`: this node owns the chunk (or stands in as a backup). It first
    reads through from `PrevOwner` during a handoff, and from the owner when
    acting as a backup, using a "peek" request that only returns chunks the
    other node has complete or is reading from the origin. A backup's peek
    (`peek=owner`) additionally makes a node that is the owner in its own
    view start its origin fill and serve it, so a hedge to a backup does not
    read the chunk from the origin a second time; a handoff peek
    (`peek=local`) never starts a fill, since the previous owner's own fill
    may be waiting on the new owner. Every peek is also served from the
    peer's content store when it holds the whole chunk. The owner then asks
    recent requesters of the chunk (the holder directory, below), also with
    `peek=local`. Otherwise it pulls the chunk from the origin. Peeks wait
    2s for response headers, holder peeks 1s.
  - Relay, owner, backup, gateway, direct: send to `Next` with the decision
    header. After `HedgeAfter`, if `TryHedge` allows, send a copy with
    `FlagHedge` to `Alternate` using `AlternateHeader`; the first response
    wins. On failure, retry to `Alternate`, then route again.
  - No route: fail.
- Every peer outcome is reported with `Observe` (latency to headers).
  429 and 503 count as `Overload`; stalls as `Timeout`.
- `ErrTTLExhausted`: serve only from memory or the content store, else 508.

### Chunk store

Gantry keeps no copy of image data on disk. The only disk copy is
containerd's own content store: containerd writes every blob while it
pulls through the gantry mirror, and the snapshotter unpacks from that same
blob. `meshfetch` (`internal/gantry/meshfetch/store.go`, `content.go`, and the
holder directory in `fetch.go`) holds chunks in bounded memory and
reads the content store back to serve peers.

#### Fills and the in-flight window

A fill is one chunk being fetched or already complete. The filler reads the
chunk into a window of 4 page buffers of 1MiB.
Readers (the local blob stream and peers) that keep up copy bytes out of
those pages as they arrive, so every hop is cut-through and a hot fan-out
reads each byte from the network once.

A page leaves the window once no reader needs it from memory. A reader needs
memory from `max(position, local floor)` on, where the local floor is the
end of the whole pages of the chunk that the content store held when last
asked (`fill.needLocked`, `fill.localFloorLocked` in `store.go`). So a page
is dropped when every reader has passed it, or when containerd has written
it and lagging readers can read it back. A reader that falls behind the
window reads the page from the content store through the page cache below;
if the content store does not have it either, the reader gets `errLost`, the
local blob stream starts a new fill from the same byte, and a peer request
that needs bytes a fill has dropped starts a new fill.

The fill asks the content store how much of its chunk it holds
(`fill.refreshLocal`) at most every 10ms, and only when it matters: when its
window is full and a reader holds it back, every 40ms while it waits, and
when a reader finds its page gone.

When the window is full and a reader the content store does not cover still
needs the oldest page, the fill:

1. borrows another page from the store-wide in-flight budget
   (`inflight_bytes`) if peers read the fill, so a slow reader does not
   slow the others down;
2. otherwise waits for the slowest reader. A fill that only the local blob
   stream reads always does this: nobody else is waiting, and containerd
   writes those bytes right after the reader passes them. A reader that
   holds the fill back for 30s (the stall timeout) is dropped with `errLost`.

With the content store covering everything the local pull has written,
the only readers that can still exhaust the budget are peers of a chunk
this node is not pulling itself, and those pace the fill.

#### Lingering and owned chunks

A fill that a peer has read, or that this node fills as the chunk's owner,
keeps every page in memory while the in-flight budget allows, unless the
content store already holds the chunk: instead of dropping a page that all
readers have passed, it borrows a new one. When such a fill completes with
the whole chunk in memory, it stays in the map on a memory LRU list, so
peers that ask later (in the relay grid, nodes reach a chunk a few seconds
apart) join it instead of fetching it again. These chunks give their pages
back:

- when a fill needs to borrow and the budget is used up (least recently used
  first, chunks this node does not own before owned ones), and
- after 30s without use, so an idle agent returns the memory.

Owned chunks linger because the owner's own pull reaches a given chunk at
an arbitrary time; dropping it at once would make a later peer request pull
it from the origin again. Each node owns about `image size / nodes` of an
image.

A complete chunk that left memory is served again from the content store:
a new fill reads it (`Node.fillFromLocal`), and concurrent requests share
that fill.

#### Reading the content store

`ContentSource` (`content.go`) is implemented by `DirContent`, which reads
the content root directly. Committed blobs are at `blobs/sha256/<hex>`.
In-progress downloads are at `ingest/<key>/data`, where `<key>` is a hash of
the namespace and ingest ref, so the directory is listed (at most every
100ms, on a miss) and each `ref` file is parsed for the digest (CRI uses
`layer-sha256:<hex>`); the readable length is the size of `data`, capped at
`total`. containerd writes ingests sequentially with plain `write` calls, so
bytes below the file size are complete and in the page cache. A blob is
opened as committed, else as its ingest, else as committed again in case
containerd renamed it in between. On commit containerd renames `data` into
`blobs/`, so an open descriptor stays valid. Descriptors are shared per
blob with a reference count and closed 2s after their last use by a timer,
so a blob that containerd garbage collects does not keep its space for
long (opening the file for every 1MiB page read made serving from the
content store about a third slower). A short read (an ingest restarted
with a truncate, or a deleted blob) drops the descriptor and loses the
reader, which retries elsewhere. A permission error disables the source with one warning.
Digests are validated as 64 lowercase hex characters before they become a
path.

When `content_root` is empty or unreadable (`cachemeshContent` in
`cmd/gantry/agent_cachemesh.go`), the node runs without a content source:
it serves peers from memory only, and every fill goes to peers or the
origin.

Reads are buffered (`pread`), not `O_DIRECT`, to hit the page cache that
containerd has just filled. Those pages belong to containerd's cgroup, so
they do not count against the agent's memory.

#### Holder directory

Chunk requests made for a node's own containerd pull carry that node's
member address in `Gantry-Cachemesh-Requester`; relays forward it. Every
node remembers the last four requesters of each chunk (`holders` in
`fetch.go`; at most
32k chunks, entries expire after 6h). When the owner has the chunk neither
in memory nor in its own content store, it asks up to two of those
requesters with `peek=local`, which they serve only from memory or their
content store, before it goes to the origin. Requesters recorded less than
20s ago are skipped: they are most likely still pulling the chunk,
typically in the very request the owner is filling. A requester that no
longer has the blob answers 404 quickly (1s header timeout).

#### Page coalescing

Content store reads go through a page cache keyed by (digest, absolute page
index), so readers of different fills of the same chunk share pages.
Concurrent readers of the same page share one read (singleflight). Each
entry is a reference-counted page buffer. When its last reader is done the
page joins a ring of the 32 most recently released pages, so readers of a
fan-out that trail a few pages behind share the read too; a page pushed
out of the ring is freed. Evicting a digest (failed verification) frees
its pages.

#### Memory bound

Chunk memory is page buffers only:

- in-flight windows: 4 x 1MiB per fill, at most
  `max_concurrent_chunk_fills` fills (256MiB with the defaults);
- borrowed pages, including lingering and owned chunks: at most
  `inflight_bytes`;
- one page per active reader while it writes that page to its socket;
- the recent page ring: 32 pages (32MiB).

With the defaults that is about 2.3GiB plus the pages being sent. ThePage buffers are anonymous `mmap` mappings outside the Go heap: they do
not raise the garbage collector's heap goal, and `munmap` returns them to
the system as soon as they are freed (an idle pool keeps at most 64MiB).

### Integrity

Chunks cannot be checked one by one: OCI only gives the digest of the whole
blob. Containerd checks the full digest when it commits. With
`verify_digest`, the serving node also hashes the full blob (offset 0 only)
and holds back the last byte until the hash matches. On a mismatch it drops
every cached chunk of the blob and aborts the response, so a bad chunk from
a peer or the origin cannot be committed and is not served again from this
node's cache. It also stops reading that blob from the content store for
10 minutes: the bad bytes may still sit in containerd's ingest. Other nodes
that cached the bad chunk find out on their own verification.

The content store serves ingests before containerd has verified them. The
bytes there are exactly what this node's gantry served to containerd, so
they are as trustworthy as the chunks in memory; containerd still verifies
the full digest before commit on every node.

### Tags and metadata

Tag resolutions use a separate keyspace, `gantry-meta`, with a 5m TTL
(`cachemesh.MinTTL`). The routing key is `<registry>/<repo>:<tag>`.

- The owner resolves a tag with one origin GET
  (`origin.Client.ManifestByTag`), stamps it with `Mesh.Stamp`, and caches
  the manifest bytes, media type, and digest.
- Other nodes fetch through `GET /cachemesh/meta?r=&n=&t=` with the route
  header, check that the body matches `Docker-Content-Digest`, and cache the
  entry with the owner's stamp (`Gantry-Stamp-Expires`, `Gantry-Stamp-Gen`).
  The owner's `Expires` is clamped to at most one epoch past what this node
  would stamp now, so a skewed or misbehaving peer cannot keep a binding
  cached past the keyspace TTL; the slack tolerates an owner one epoch ahead.
- Every holder checks `Mesh.Fresh` before using a copy, including a
  resolution it just received from a peer or joined in flight; a stale one
  is rejected and not cached, and the mirror falls back. One resolution per
  key runs at a time on each node.
- A resolution runs on the node's context, so it outlives a requester that
  gives up. Each node runs at most 64 (`maxMetaFetches`) at once; a new key
  beyond that is shed (`429` to a peer), and the mirror falls back.
- Each node keeps at most 4096 entries and 64 MiB of manifest bodies
  (`metaCacheBytes`). An insert that would exceed either bound drops expired
  entries first, then arbitrary ones; a body larger than the byte budget is
  served but not cached.

The mirror answers a tag request from this cache only when `cache_tags` is
on, the request has no `Authorization` header, and the registry does not
require authentication (`AuthenticationChallenge`). Otherwise it keeps the
existing 503, and containerd goes to the origin. The requester's check only
probes the registry root (or is skipped when credentials are configured), so
it does not show that the repository itself is public. The metadata owner
therefore resolves every tag anonymously
(`registryauth.WithAnonymous`): no delegated authorization, no configured
credentials or token obtained with them, and no URL user information; an
endpoint with a query string is refused. A private repository then fails at
the owner, the requester declines, and containerd pulls the tag with its own
credentials. Nodes with shared credentials do not use them for mesh tag
resolution. A tag cannot be verified
against content, so a node that lies about a tag can point other nodes at
the wrong image for up to the TTL (see the tag-binding note in
`designs/gantry-detailed-design.md`). Manifests by digest are safe because
containerd verifies them. Because one compromised member could redirect a
tag for the whole cluster, `cache_tags` is off by default; turn it on only
when every holder of the peer key is trusted to that degree.

### Interaction with the default path

- Local hits, HEAD requests, and the DHT, cold-start, and origin paths are
  unchanged. The mesh runs after the local-store miss and before the peer
  lookup.
- Bytes served through the mesh count in
  `gantry_mirror_bytes_served_total{source="cachemesh"}` and the completion
  hooks, so the stream-commit tracker and the advertiser still see them.
  Completed blobs are announced in the DHT as before.
- With cachemesh on, the layer prefetcher only tracks layer progress; it no
  longer asks chairs to seed layers from the origin, which would duplicate
  the mesh's origin pulls.

### Transport

Data requests use a dedicated HTTP/1.1 client (`meshfetch.NewDataClient`):
one TCP connection per concurrent chunk avoids HTTP/2 flow-control windows
capping a single stream. Chunk traffic creates no tracing spans.

Metrics are in the `gantry_cachemesh_*` family (`internal/gantry/metrics`);
fill sources are labeled `origin`, `peer`, `prev_owner`, `owner`, `local`,
and `holder`.

### Security

Every request to `/cachemesh/control`, `/cachemesh/chunk`, and
`/cachemesh/meta` is authenticated with a shared key
(`internal/gantry/meshfetch/auth.go`). `/v2/` on the transfer port is
unchanged.

- Signing: the sender adds
  `Gantry-Cachemesh-Auth: v1 t=<unix seconds> s=<base64url MAC>`. The MAC is
  HMAC-SHA256 over a domain prefix with the version, the method, the escaped
  URL path, the raw query, the timestamp, and the SHA-256 of the body (of
  the empty body for GET).
- Verification: each handler verifies before it parses the request, touches
  mesh state, records a holder hint, or starts a fill. The control handler
  reads the body under its size bound first, since the MAC covers it.
  Missing, malformed, stale (more than 2 minutes from the receiver's clock),
  or unmatched signatures get 401. Comparison is constant time.
- Keys: `cachemesh.peer_key_file` holds one key per line, each at least 32
  bytes. The first key signs and every key verifies, so a key rotates in
  three rollouts: add it second, move it first, drop the old one. The agent
  reads the file at startup and refuses to start without valid keys; config
  validation requires the path when cachemesh is enabled. There is no
  unauthenticated mode. The Helm chart generates the key into a Secret
  (kept across upgrades) or uses an existing one.

This stops pods without the key from joining or disrupting the control
plane (forged joins, leaves, suspicions, or leader snapshots), from making
owners pull from the origin, and from registering holder addresses that
peers would later fetch from.

Residual risk:

- Any holder of the key, such as a compromised Gantry pod, is a trusted
  member: it can do all of the above.
- Owners pull with the agent's own registry identity (configured
  credentials or anonymous), never with a requester's delegated
  credentials. An authenticated peer can therefore obtain blobs that only
  another node's pod was allowed to pull, and can trigger origin fills
  with that identity.
- Responses are not signed, and the transfer port is plaintext (h2c and
  HTTP/1.1) by design, so an attacker on the network path can read and
  alter traffic. Chunk integrity rests on digest verification (see
  Integrity). A captured request can be replayed within the skew window.

NetworkPolicy limiting port 5001 to Gantry agents remains recommended as
defense in depth.

### Limitations

- Routing loops can form while nodes see different views. A loop between two
  nodes is refused at once (the receiver checks whether its in-progress fill
  reads from the sender); longer loops end when the stall timeout fires.
- A backup that stands in for a slow owner reads through from the owner with
  `peek=owner`, and an owner without the chunk starts its own origin fill
  and serves it. The backup still goes to the origin when the owner does
  not answer (or does not see itself as the owner), so a chunk may be
  pulled twice.
- HEAD requests on a local miss still go to the origin from every node.
- Owners pull from the origin with the agent's origin client, which uses
  HTTP/2 for HTTPS registries; many concurrent chunk pulls from one owner
  share a connection.
- The chunk size must match on every node; a mixed rollout refuses requests
  between nodes with different sizes, and the mirror falls back.
- Reading the content store relies on containerd's on-disk layout
  (`blobs/sha256/<hex>`, `ingest/<key>/{ref,total,data}`, rename on commit,
  sequential ingest writes), verified against containerd 2.1. A layout
  change makes the directory reader find nothing (or a permission error
  disables it), and cachemesh falls back to memory and the origin; it does
  not serve wrong bytes unless a file under a blob's path holds other data.
- The agent must be able to read the content root: the chart mounts it
  read-only, and the non-root agent relies on containerd's default
  permissions (directories 0755, blobs 0444, ingest files 0644).
- containerd may garbage collect a blob at any time (immediately, with the
  default GC settings, once no image or lease references it). Readers then
  get short reads and retry elsewhere; descriptors are closed after 2s idle
  so the space is reclaimed.
- A relay or owner that is not pulling an image keeps its chunks only in
  memory, for at most 30s unused and while the in-flight budget has room.
  After that its share is served from requesters' content stores (holder
  directory) or, if none still has the blob, from the origin.
- The holder directory only knows requesters whose requests reached this
  node: with relays, a coalesced relay fill carries one requester. It is
  lost on restart.
- Content store reads are synchronous, one page at a time per reader. There
  is no read-ahead.

## Deterministic simulation testing

`internal/cachemesh/internal/dst` drives many `control.Node` instances on a
simulated clock and network (latency, loss, duplication, reordering,
partitions, slow members, per-member clock offset and drift), with an
in-memory Lease server and a process model (crash, restart with a new
incarnation, leave, pause). A data-plane workload routes requests hop by hop
with `Route` and `Forward` and feeds outcomes to `Observe`. Scenarios are
generated from a seed; selected cases run twice and must produce the same
event-trace hash.

Invariants it checks:

- Safety: at most one member publishes per term; terms increase; one digest
  per version; view versions never decrease (except the snapshot-trust case
  above); every view passes `view.Validate`; the neighbor bound holds once
  cells are balanced (twice the bound during a reshape); every request ends
  within its TTL; epochs never decrease and a stamp is never `Fresh` later
  than `T + period` plus drift slack; Lease traffic stays within budget.
- Liveness after faults heal: one leader, every live member at the same
  version, members are exactly the live processes, loads within one of
  target, sampled routes reach the owner in at most D hops.
- Forgery: `TestDSTForgery` adds a peer that cannot write the Lease but
  sends pings that claim later (and absurd) terms, `NewTerm` broadcasts
  led by members the Lease does not name, and forged catch-up snapshots.
  The leader and its term must survive the attack, a leader crash during
  it must still fail over, and every member must converge (every forged
  view it adopted rolled back). Forged views are exempt from the
  one-digest-per-version and no-regression checks, since they were never
  published.

`go test ./internal/cachemesh/internal/dst` runs the default seed set. It
is skipped under `-short` and `-race`. `CACHEMESH_DST_SEED`,
`CACHEMESH_DST_SEEDS`, and `CACHEMESH_DST_LARGE=1` select longer runs; a
failure prints its replay command.

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
| `PingInterval` / `PingTimeout` / `SuspectAfter` | 2s / 1s / 3 |
| `SuspicionWindow` / `ObserverCells` / `DampingPercent` | 30s / 3 / 10% |
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