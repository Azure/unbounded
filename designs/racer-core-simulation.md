# Racer core simulation

## Summary

Racer's decision logic (read path, peer routing, placement, caches, admission, and control-plane
steps) lives in a sans-IO crate, `racer-core`. The production dataplane and cluster control call
it. A discrete-event simulator, `racer-sim`, calls the same code for up to 50,000 nodes in one
process and one thread, and checks invariants on every run. Runs are deterministic: a scenario,
node count, and seed always give the same trace hash.

## Goals

- Run the same decision code as production, not a model of it.
- 50,000 nodes, 2 workers per node, one thread, at most 32 GiB RSS.
- Deterministic replay from a seed.
- Report latency, origin load, peer traffic, cache behavior, control-plane load, and memory.
- A small smoke run in `cargo test`. Large runs with `make racer-sim ARGS=...`.

## Non-goals

- Simulating payload bytes, crypto output, or real disk contents.
- Replacing the dataplane deterministic tests (`src/app/tests/dst.rs`).
- Changing production behavior through the simulator. Generic code has defaults that compile to
  the production types (for example `Placement<R: Rankings = Scored>`, `TickLru<.., G = Ungrouped>`).

## racer-core

Path: `cmd/racer-dataplane/core`, crate `racer-core`, lib `racer_core`.

Depends only on `topology`, `racer-object-wire`, `racer-control-wire`, `flow-control`, and `sha2`.

### Purity rules

- No I/O, clocks, threads, or OS randomness. Callers pass time in (`racer_core::time::Now`: a
  monotonic `Instant` plus `wall_ms`) and randomness as an explicit RNG (`time::SplitMix64`).
- `core/clippy.toml` disallows `Instant::now`, `SystemTime::now`, and `thread::spawn`.
- `core/tests/purity.rs` scans non-test code in `core/src/**/*.rs` and fails on `Instant::now`,
  `SystemTime::now`, `uring_runtime`, `environment::now`, `getrandom`, and `thread::spawn`.

### Modules

| Module | Contents |
| --- | --- |
| `admission` | `ResourceClass`, `Limits`, `Floors`, and `Rules` (progress and weighted admission) |
| `cache` | `TickLru`, `OrderedLru`, per-cache reclaim lanes (`Grouping`, `Lanes`), segment value and retention scoring |
| `liveness` | `Silence` (silent next hops and pings) and `Rank0Watch` (rank 0 owners that hang or fail) |
| `route` | Route budget rules shared by app and wire (`forward`, `relay`, `search_budget`, `validate`), `decide_hop`, relay plans, exchange outcomes, and response validity rules |
| `subscriptions` | Sans-IO subscription scheduler |
| `read` | Errors, worker map, peer responses, metadata refresh, serve and persist policy, range slots, candidate order and response classes |
| `read::budget` | `AcquisitionBudget`, `MAX_ROUTE_LINKS` (8), hot page shedding to helpers |
| `read::walk` | `Fill`: one read's acquisition as a sans-IO state machine (owner targets, the candidate walk, hedges, stale membership refresh, fallback and waits, origin, the post-412 walk, publication), driven by both the dataplane and the simulator |
| `read::hints` | Orphan copy notices and successor hints |
| `read::flight` | Singleflight step functions (join gates, wait/elect, publish, fail) shared by production `Flights` and the sim's `PageFlights` table |

### Production callers

- The dataplane (`cmd/racer-dataplane/src`) calls `racer_core` directly. For example, page fill
  acquisition and candidate resolution drive `racer_core::read::walk::Fill`
  (`CandidatePolicy::drive` in `src/read/candidates.rs`, `acquire_once` in `src/read/fill.rs`), and
  the simulator drives the same machine (`NodeWorld::drive` in `sim/src/node/world.rs`). Range reads use
  `racer_core::read` range slots (`src/read/candidates.rs:3909`), and the memory cache uses
  `racer_core::cache` with `ByCache` grouping (`src/memory.rs`).
- The wire hop limit comes from core: `peer-wire/src/lib.rs:505` sets
  `MAX_HOPS = racer_core::route::MAX_LINKS`.
- `racer-cluster-control` exposes pure step functions in `cluster-control/src/step/` (seat
  lottery and renewal, primary contention and plan pass, seat admission, authorization). Wall time and entropy are inputs: `now_micro` takes a
  `SystemTime` (`cluster-control/src/authority/election.rs:126`) and `new_key` takes an `Entropy`
  (`cluster-control/src/authority/keyring.rs:352`).

## Phantom caches

A phantom cache makes the same decisions as the real cache but stores no payload.

- Placement: `Placement<R: Rankings>` (`topology/src/placement.rs`). The simulator plugs in
  `Phantom` (`sim/src/phantom.rs`), a ranking backed by a shared memo keyed by membership and slot.
  CLOCK, pinning, and `Overloaded` behavior are the real code.
- Disk: real `Segments` and `SegmentClock` from `page-alloc` over `PhantomEntries`, a phantom entry
  index that exists only with the `simulation` feature.
- Memory: `TickLru` with a small value type and `Ungrouped` lanes.
- Routes: `Paths<S: Searches>` with `SharedRoutes` (`sim/src/phantom.rs`), a shared route memo.
- `sim/tests/phantom_memory.rs` measures the per-node heap cost of all three phantom caches.

## racer-sim

Path: `cmd/racer-dataplane/sim`, crate and binary `racer-sim`.

| Module | Role |
| --- | --- |
| `engine` | Event queue ordered by `(time, seq)`, `u64` ns time, per-role seeded RNG streams, trace hash |
| `resources` | FIFO servers for NIC send and receive (cut-through per chunk), disk, CPU workers, crypto helper, site RTT matrix, origin |
| `workload`, `faults` | Poisson, Zipf, range reads, cold-start storms, churn; partitions, crashes, slow nodes, hung peers, origin 412, 403, 5xx |
| `node` | Real node logic for every node (below) |
| `phantom` | Shared-memo `Phantom` rankings and `SharedRoutes` searches for every node's caches |
| `config` | One `Config` (with its `node` knobs) parsed from `--config` JSON and validated in one place |
| `control` (model) | `ControlPlane`: a whole-cluster model built from the `cluster-control` steps, with the write-drain cost model |
| `control` | `ControlSim`: the `ControlPlane` model plus a client model |
| `combined` | Control plane and data plane on one timeline |
| `report` | Histograms, counters, invariants, memory accounting, JSON output |

### Node model

Per worker (2 by default): a phantom `Placement`, a `PageFlights` table, a `TickLru` memory index,
and its own `flow_control::Quotas`. `WorkerMap::select` picks the worker for a page. Per node: a
`Paths` route cache, `flow_control::Circuits` link health, a phantom disk, and retention history
(`SecondSight`, `Heat`).

A read checks memory, then disk, then joins a flight. The leader drives the same `Fill` machine as
production: it performs each action (rank, probe, wait, origin, publish) as a scheduled event.
Where the simulator still differs from production, a temporary `Features::SIM_*` bit says so
(`SIM_DIVERGENCES` in `world.rs`). Probes go hop by hop and every hop runs `decide_hop`. Relays take a `Relay` quota
and search their own route. The destination applies the serve action. Before origin, the leader
checks origin authority. Admission uses `racer_core::admission::Rules`.

Differences from production:

- Smaller capacities to save memory: placement 256 entries per worker, route cache 128 entries.
  Flight, waiter, relay, and fill quota limits use production defaults. All are set under `"node"`
  in `--config`.
- One cache identity, so cache priority weights have no effect.
- Hedges are off by default (`node.hedge_slots` 0), as in production.
- Standalone scenarios batch membership churn every 250 ms and keep 2 older versions per node.
- The simulator does not model quota-driven idle eviction, per-cache fair-share reclaim, or
  client cancellation. Production tests cover these (`src/read/owner_contracts.rs`,
  `src/read/tests/`).

### Control model

`ControlSim` co-steps the `ControlPlane` model (`sim/src/control.rs`: lottery, seats, primary,
commits, built from the `cluster-control` steps) with one
client per Pod. Clients enroll through seat bootstrap slots and the bearer authorization API calls
against a rate-limited fake API server, then long poll snapshots with the real seat admission
steps (write slots, a bounded FIFO, delta or full bodies). Clients send the base hash they hold.
See the header of `sim/src/control.rs` for details.

### Combined mode

Pod `i` is data-plane node `i`. Before the data plane runs an event at time `t`, every control
event at or before `t` runs, control first. Each node uses exactly the membership version its
control client applied, so lagging clients cause real data-plane skew. A node whose client has
applied nothing does not read or accept peer links. Pod crash, restart, and removal map to the
node.

### Scenarios

| Scenario | What it runs |
| --- | --- |
| `smoke`, `caches-only`, `empty` | Small sanity runs; `empty` must produce no events |
| `zipf`, `storm`, `churn`, `faults` | Data plane with synthetic membership |
| `enrollment-storm`, `failover`, `ctl-churn`, `lagging` | Control plane only |
| `full-cold-start`, `full-failover`, `full-churn`, `full-lagging` | Combined, using the control plan of the matching control scenario plus a Zipf read load |

`sim/tests/contention_scenarios.rs` holds pressure tests ported from a former test-only model
(cold origin contention, warm fanout, shared NIC bandwidth, slow disk, partition recovery, seed
replay).

## Memory budget

32 GiB / 50,000 nodes is about 671 KiB per node. The target is at most 640 KiB per node, leaving
room for shared state (ranking and route memos, event heap, RTT matrix, report). To fit, per-node
state holds no payloads and no per-node copies of shareable data. The report lists accounted
bytes per component and peak RSS so the budget can be checked. Measured runs at 50,000 nodes stay
far below the limit; the cold-start storm is the largest, driven by the route memo and in-flight
state.

## Determinism and replay

- All time comes from the event clock. All randomness comes from `SplitMix64` streams derived from
  the seed, one per role. Shared hash maps use seeded hashers in simulation.
- Each recorded event feeds an order-sensitive trace hash. In combined mode `trace_hash` mixes
  both timelines and `data_trace_hash` is the data plane alone.
- Tests check that a replay gives the same hash and report: `lib.rs` `replay_is_deterministic`,
  `node::tests::same_seed_replays_and_seeds_differ`, `control` `replays_exactly_and_seed_matters`,
  `combined` `replays_exactly`, and `contention_scenarios::seed_matrix_replays_identical_reports`.

To reproduce a run, use the same scenario, node count, seed, duration, and config:

```sh
make racer-sim ARGS='--scenario full-churn --nodes 200 --seed 7 --duration 300s'
make racer-sim ARGS='--scenario zipf --nodes 50000 --config cfg.json --out zipf.json'
```

`--config` takes a JSON object of `Config` fields (unknown keys are errors), with `"node"` and
`"control"` sub-objects. `RACER_SIM_TIMEOUT` (default 7200s) bounds the make target. Setting
`RACER_SIM_TRACE_FAILED` adds per-probe and per-page diagnostics for failed reads.

## Invariants

Every run checks these. A failure is reported with time, node, and detail. The report API
supports marking an invariant relaxed with a reason; none is relaxed today.

Data plane (`sim/src/node/world.rs`):

- `budget_never_grows`: attempt plans, relays, and inherited budgets.
- `route_within_hop_limit`: at most 8 links.
- `relay_route_valid`.
- `only_owner_hits_origin`: rank, owner, and authority on the walk's membership version.
- `origin_within_scope` and `origin_for_live_waiter`: origin requests only while a waiter wants
  the page.
- `origin_repeat_explained`: repeat origin fetches of a page that another owner still holds must
  fall into a known class.
- `membership_version_monotonic` per node.
- `flight_leader_valid`.

Control plane (`sim/src/control.rs`):

- `ctl_single_primary`, `ctl_terms_monotonic`, `ctl_sequence_monotonic`.
- `ctl_client_membership_monotonic` per client.
- `ctl_client_converges`: after faults heal, every live client applies every committed sequence
  within a fixed window.

## Design decisions found through simulation

These are production rules that the simulator motivated. Code comments refer to them by name.

### Joining members

A new member is added to the membership only after it reports that it serves peer links
(`src/control/session.rs` `mark_serving`). Before that, a listed member would refuse every link
and reads toward it would fail. Joins are batched by a hold that grows with membership size
(`join_hold_secs` in `cluster-control/src/step/primary.rs:102`), so clients stay within one version
during large enrollments instead of falling back to full images.

### Hot page fanout

An owner with more than `HOT_PAGE_QUEUE_BYTES` of queued egress for one page sheds further readers
with `Overloaded`, and readers fetch from the page's helpers instead
(`core/src/read/budget.rs`). The limit is in bytes so it means the same backlog for 1 MiB
simulator pages and 16 MiB production pages. A page is sent by at most `REPLICAS` owners plus
`fanout_width` helpers.

### Route budget

App and wire share one rule: a route carries at most 8 links (`MAX_ROUTE_LINKS`,
`core/src/read/budget.rs`; `MAX_LINKS` in `racer_core::route`).

## Known limitations

These are bounded behaviors, not bugs, and are covered by tests or the invariants above.

- Origin requests per page can exceed one when rank 0 changes, a copy is lost, or origin returns
  5xx and the client retries. Repeats while another owner holds the page must be explained
  (`origin_repeat_explained`).
- A relay-signed `Overloaded` on the route to rank 0 can give an owner above rank 0 origin
  authority, allowing at most one overlapping origin request per such walk.
- A minimum-funded walk cannot fund both a helper probe (`HELPER_FLOOR_ATTEMPTS`) and an
  out-of-order rank 1 probe. When the helper misses, the read retries.
- Silence pings add peer messages under many probe timeouts. Each ping follows a timeout or a
  recheck.
