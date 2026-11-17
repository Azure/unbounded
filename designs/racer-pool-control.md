# Racer buffer pool sizing

## Summary

Racer keeps three kinds of reusable buffers: each worker's page buffer pool
(`page_alloc::buf::Pool`, one per worker), each worker's disk-write staging
slab pool, and the node's registered RDMA receive pool (one per rail, owned by
the native thread). All three are sized by the same controller,
`page_alloc::Provision` (`cmd/racer/alloc/src/demand.rs`). It holds enough
capacity for recent demand, grows at once when demand rises, and gives
surplus back only after demand has stayed lower for a hold time derived from
how expensive the capacity is to get back. Only the actions differ per pool:
mapping and unmapping memory for host pools, registering and deregistering
blocks for RDMA.

## Problem

Before this change the pools traded spikes against idle memory badly:

- The page buffer pool kept every buffer a spike left behind, up to its
  admission budget, and trimmed only after a whole second with no checkout at
  all. A trickle of traffic after a spike kept the spike's memory forever.
- Misses mapped and faulted fresh memory on the request path, and the trim
  rule freed half of the idle buffers at a time, so bursty load kept mapping
  and unmapping.
- The ciphertext reclaimer and encryption output shed every idle buffer when
  admission ran short, although idle buffers are charged to the `pool` class,
  not to ciphertext, so shedding freed no ciphertext admission and forced the
  next take to map again.
- The RDMA pool registered and pinned the whole `RACER_REGISTERED_BYTES`
  budget at activation and never gave any back.

## Goals and non-goals

Goals:

- Absorb spikes: capacity follows a rise without waiting for a window, and
  a shortfall (a take that found nothing ready) grows the pool at once.
- Low idle baseline: after demand falls, capacity returns to the floor, even
  under a trickle of traffic.
- Little churn: demand that recurs within the hold time causes no release and
  re-acquire; expensive units (registered memory) are held longer than cheap
  ones (mapped memory).
- One algorithm for every pool, deterministic, integer only, cheap on the
  take path.
- Memory pressure overrides demand.

Non-goals:

- Prefilling host buffers ahead of demand on helper threads. Host misses
  still map on the request path; the controller only makes them rarer.
- Changing GPU device pools, which are registered once per round, or the
  per-grant registration that registration-mode devices (MANA) already do.
- New settings. Every constant is internal.

## Why not a plain PID controller

A PID loop on pool size fits poorly. Demand is bursty and heavy-tailed, the
plant is trivial (capacity is whatever was acquired), the costs are
asymmetric (a shortfall stalls a request, surplus only costs memory), and
an integral term winds up during long idle periods and then overshoots. The
controller keeps the useful parts of PID semantics and replaces the rest with
estimators from prior art:

- Proportional: the target tracks the recent peak directly, the way
  tcmalloc's subrelease keeps the recent demand peak.
- Integral: a correction for unobserved demand (shortfalls) that grows by one
  unit per shortfall and leaks away over the hold time, with conditional
  integration as anti-windup: it is frozen while the pool is saturated or
  under memory pressure and capped at the ceiling.
- Derivative: a look-ahead of the recent rise times the acquire latency,
  only while demand rises two samples in a row, capped at two units.

Other prior art: TCP's retransmit timer (mean plus a multiple of the mean
deviation), jemalloc's dirty and muzzy decay (warm then cold then unmapped),
Linux per-CPU page list high-watermark tuning, and ski rental (hold an idle
resource until holding it has cost as much as reacquiring it).

## Controller

Each pool reports to its `Provision`:

- `observe(in_use)` whenever the in-use count changes or is sampled: items
  checked out, plus requests waiting for one where the pool can see them.
- `shortfall(now, items)` when a take found nothing ready.
- `acquired(now, units, took)` with the measured time to acquire a unit.
- `tick(now, capacity, pending, pressure) -> Plan` from a periodic path, at
  least every `SAMPLE_PERIOD` (100 ms) while the pool is not idle.

The hold time `H` is 1000 times the smoothed time to acquire one unit,
clamped to 1 s to 300 s, so time spent reacquiring released capacity stays
near 0.1% of the time it is held. A 16 MiB host buffer (2 ms to map and fault
in) holds for 2 s; an RDMA block that takes 100 ms to register holds for 100 s.

Demand is recorded as the peak in-use level of windows `H / 8` long. The
target is the largest of:

- the recent peak, the largest window peak over the last `H`;
- the typical level, a smoothed mean of window peaks plus two smoothed mean
  deviations, capped at the largest peak of the last `4 x H`, so demand that
  recurs a little slower than `H` keeps its capacity without ever exceeding
  what was recently used;

plus the look-ahead and the correction, clamped between the floor and the
ceiling. The plan grows when the target exceeds capacity and nothing is
pending, rounded up to whole units. It releases half the surplus, in whole
units, only once capacity exceeds the target by a unit, at most once per
`H / 8`, and never within `H / 8` of growth or a shortfall.

Memory pressure changes the rules. `Elevated` shortens `H` to a quarter,
freezes the correction, and caps the target one unit above in-use.
`Critical` caps it at in-use and releases all surplus at once.

## Host page buffer pool

`buf::Pool` keeps one `Provision` per capacity class, with a unit of one
buffer, floor 0, and the class's budget as the ceiling. Each class counts
buffers out with an atomic, incremented on take and decremented when a
buffer returns from any thread, and the owner observes it on take and on
`Pool::maintain`. `BufferPool::maintain_idle_buffers` in
`cmd/racer/src/memory.rs` calls `Pool::maintain` from the worker's pool
maintenance on every pass.

Surplus is cooled rather than freed: `MADV_DONTNEED` returns a mapping's
pages to the kernel, its idle charge is released, and the mapping stays in a
cold list. Takes reuse warm buffers first, then cold ones; a cold take faults
the pages back in, counts as a cold hit, and reports a shortfall so the target
rises. Cold buffers unused for `COLD_HOLDS` (4) hold times are unmapped. Heap
buffers (under 2 MiB) are freed at once, since they cannot be cooled.
Returns are accepted while the class is under its idle depth and the `pool`
admission class accepts the charge; under `Critical` pressure they are
refused.

`top_up` no longer sheds idle buffers on a refused ciphertext reservation, and
the ciphertext reclaimer no longer sheds them after reclaiming cache, since
neither freed ciphertext admission. Origin fills reserve the padded capacity
up front through the reclaiming path instead, so encryption output rarely has
anything to top up.

## Staging slab pool

The slab pool has one `Provision` in 64 KiB granules over checked-out plus
idle storage, floor 0, with acquire cost of 20 us. It keeps every return that
fits the 128 MiB budget and frees surplus oldest first on
`Slab::maintain_idle`, in whole buffers. Without pressure it skips a buffer
whose removal would leave less than the target, so a large buffer stays while
it backs the target and goes once returns of other sizes do. `Elevated`
refuses returns beyond the spare target, so it frees whole buffers even below
the target, which is at most one granule above in-use; otherwise an idle
buffer no current request can reuse would stay. `Critical` frees every idle
buffer. Idle slab buffers keep their primary charge, so there is no cold
stage.

## RDMA registered pool

Registered memory is pinned, so it cannot be cooled with `MADV_DONTNEED`;
the only way to shrink is to deregister a whole region. Each rail's host pool
is therefore a list of blocks, each its own registered region with its own
free list (`verbs/src/pool.rs`). A block is an eighth of the rail's share of
`RACER_REGISTERED_BYTES`, 2 to 32 buffers of 18 MiB
(`rdma::links::block_buffers`). Activation registers the first block, which
is the floor and stays for the round; the rest of the budget is the ceiling.

The native owner ticks one `Provision` per rail (`verbs/src/sizing.rs`) in
every pass with the unit of one block. In-use counts buffers not free plus
grant requests parked on the rail that a free buffer would serve: each
ready link's live requests up to its free grant slots, so requests held back
by their link's grant cap do not grow the pool. A take that found the rail
empty reports a shortfall, at most once per sample; a link checks its grant
cap before taking, so a capped request never reports one. Growth first
reactivates a draining block, which costs nothing, and otherwise reserves the
block's `Registered` admission charge through `Growth::charge` and starts
`Registering::host` on a helper thread, one block in flight per rail. The
owner keeps serving; when the registration finishes, it adds the block,
reports the measured time, and serves parked grants again. A refused charge
marks the rail saturated (freezing the correction) and a failed registration
logs; either way growth retries after 1 s, and until then the rail reports no
room to grow.

Release marks the newest active block draining: it hands out no more buffers
and the links free its memory windows as grants return, since a window stays
tied to the first region it binds. Takes prefer the oldest active block, so
draining blocks empty quickly. Once every buffer of a draining block is free
and no window holds its region, `Pool::reap` drops it, which deregisters it on
the owner thread and releases its charge; a failed deregistration leaks the
region and quarantines its charge, as before. Shutdown waits for block
registrations in flight.

`Client::free_buffers` counts the buffers a rail may still register as free,
so the read path's ahead-grant reserve sees the room growth would fill
rather than only the current registration, and does not hold back requests
that growth would serve. The room is published with the sizing on every
owner pass, and follows what `Provision::tick` may still grow to: up to the
ceiling under `Normal`; under `Elevated`, which caps the target one block
above the observed demand, a block already registering plus the blocks up to
that cap; and none under `Critical`, which never grows, while growth waits to
retry, or once the rail is closing or lost. Otherwise ahead pages could
take the buffers the reserve keeps for window heads, counting on growth that
pressure has disabled. `Client::set_pool_pressure` wakes the owner, so the
room follows a pressure change on the next pass. Grants per link are still
sized from the ceiling.

## Pressure

The sampling worker computes the node pressure level; every worker applies
transitions in `apply_pressure` (`cmd/racer/src/app.rs`), which sets the
pressure of its page buffer and staging pools and, through
`Links::set_pool_pressure`, of the node's RDMA pools (idempotent across
workers). A rising transition still reclaims idle buffers and staging at once.

## Telemetry

Host pools export `racer_buffer_pool_*` and `racer_staging_pool_*`: entries,
warm, in-use, target, and cold bytes, and hits, misses, cold hits, rejects,
trimmed, and unpooled counters. RDMA pools export
`racer_rdma_pool_{registered,capacity,target,in_use}_bytes` (registered
counts draining blocks until they are deregistered; capacity counts only the
active blocks that sizing compares with the target), block and draining block
counts, registration and deregistration counters, and the slowest rail's
registration and hold times.

## Testing

`alloc/src/demand.rs` tests the controller on synthetic demand: spikes return
to the floor, a trickle after a spike does not pin it, spikes within `H` keep
capacity and further apart release between, steady and noisy demand do not
churn, rising demand grows ahead, shortfall correction rises and decays, and
pressure, saturation, the ceiling, the floor, and irregular time. `buf.rs`,
`lib.rs`, `memory.rs`, and app tests cover the host pools end to end, and
`verbs/tests/links.rs` covers RDMA block growth, draining, pressure, refused
charges, and shutdown with registrations held in flight, in the verbs
simulation.
