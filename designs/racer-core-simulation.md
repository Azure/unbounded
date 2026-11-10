# Racer core simulation

## Summary

Racer's decision logic (read path, peer routing, placement, caches, admission, and control-plane
steps) lives in a sans-IO crate, `racer-core`. The production dataplane and cluster control call
it. The deterministic simulation harness, `racer-dst` (`cmd/racer/dst`), calls the same code: at
Full fidelity it runs the real application on simulated I/O, and at Model fidelity it drives the
real `racer-core` machines for up to 50,000 nodes in one process and one thread. Every run checks
invariants. Runs are deterministic: a scenario and a choice stream (seed) always give the same
trace hash.

## Goals

- Run the same decision code as production, not a model of it.
- 50,000 nodes, 2 workers per node, one thread, at most 32 GiB RSS.
- Deterministic replay from a seed.
- Report latency, origin load, peer traffic, cache behavior, control-plane load, and memory.
- Small runs in `cargo test -p racer-dst` and `make racer-dst`. Large runs with
  `make racer-dst-report ARGS=...`.

## Non-goals

- Simulating payload bytes, crypto output, or real disk contents.
- Replacing the dataplane deterministic tests (`src/app/tests/dst.rs`).
- Changing production behavior through the simulation. Generic code has defaults that compile to
  the production types (for example `Placement<R: Rankings = Scored>`, `TickLru<.., G = Ungrouped>`).

## racer-core

Path: `cmd/racer/core`, crate `racer-core`, lib `racer_core`.

Depends only on `topology`, `racer-wire`, `racer-flow-core` (re-exported as `racer_core::flow`),
and `sha2`.

### Purity rules

- No I/O, clocks, threads, or OS randomness. Callers pass time in (a monotonic `Instant`, and
  wall time where a rule needs it) and randomness as an explicit RNG (`rng::SplitMix64`).
- `core/clippy.toml` disallows `Instant::now`, `SystemTime::now`, and `thread::spawn`.
- `core/tests/purity.rs` scans non-test code in `core/src/**/*.rs` and fails on `Instant::now`,
  `SystemTime::now`, `racer_io`, `environment::now`, `getrandom`, and `thread::spawn`.

### Modules

| Module | Contents |
| --- | --- |
| `admission` | `ResourceClass`, `Limits`, `Floors`, and `Rules` (progress and weighted admission) |
| `cache` | `TickLru`, `OrderedLru`, per-cache reclaim lanes (`Grouping`, `Lanes`), segment value and retention scoring |
| `prefetch` | Prefetch priorities, owner peer-operation pages and result records, the owner fill budget (`OwnerBudget`), retry backoff |
| `rng` | `SplitMix64`, the explicit deterministic RNG |
| `liveness` | `Silence` (silent next hops and pings) and `Rank0Watch` (rank 0 owners that hang or fail) |
| `route` | Route budget rules shared by app and wire (`forward`, `relay`, `search_budget`, `validate`), `decide_hop`, relay plans, exchange outcomes, and response validity rules |
| `subscriptions` | Sans-IO subscription scheduler |
| `read` | Errors, worker map, peer responses, metadata refresh, serve and persist policy, range slots, candidate order and response classes |
| `read::budget` | `AcquisitionBudget`, route link limits, hot page shedding (`shed_hot_page`) and helper fanout |
| `read::walk` | `Fill`: one read's acquisition as a sans-IO state machine (owner targets, the candidate walk, hedges, stale membership refresh, fallback and waits, origin, the post-412 walk, publication), driven by both the dataplane and racer-dst |
| `read::acquisition` | `PageAcquisition`: the walk plus the rankings, memberships and answers the dataplane keeps beside it, as effects (`Effect::Io`, `Report`, `Done`) and inputs |
| `read::effect` | `EffectId`s that pair each machine effect with its answer |
| `read::errors` | The retry/terminal table over the closed `Error` enum |
| `read::window` | `ReadWindow`: one range read's window of pages (probes, fills, spill holds) |
| `read::serve` | `ServeDecision`: per-page streaming counters, hot page shedding and the serve dispatch |
| `read::cohort` | `MetadataCohort`: metadata refresh cohorts on a flight table |
| `read::hints` | Orphan copy notices and successor hints |
| `read::flight` | Singleflight step functions (join gates, wait/elect, handover, publish, fail) used by production `src/read/flight.rs` and by `read::worker` |
| `read::worker` | `WorkerCore`: one worker's flights, acquisitions and serve decisions as events in and effects out, which racer-dst's Model interprets |

### Production callers

- The dataplane (`cmd/racer/src`) calls `racer_core` directly. Page fills drive
  `racer_core::read::acquisition::PageAcquisition`, which wraps `racer_core::read::walk::Fill`
  (built by `CandidatePolicy::acquisition` in `src/read/candidates.rs`, driven from `acquire_once`
  in `src/read/fill.rs`). racer-dst's Model drives the same machine through
  `WorkerCore` (`core/src/read/worker.rs`, interpreted by `dst/src/model/mod.rs`). Range reads use `racer_core::read::window::ReadWindow`
  (`src/read/range_stream.rs`), and metadata refreshes use `racer_core::read::cohort`
  (`src/read/metadata.rs`). The memory cache uses `racer_core::cache` with `ByCache` grouping
  (`src/memory.rs`). Peer serving decides and sheds hot pages with `racer_core::read::serve::ServeDecision`
  (`src/peer/server.rs`), as does `WorkerCore::serve`.
- Production and `WorkerCore` use the same ping and copy offer caps (`MAX_PINGS_IN_FLIGHT`,
  `MAX_OFFERS_IN_FLIGHT` in `racer_core::liveness`).
- The wire hop limit and the route budget share one constant: `peer-wire/src/lib.rs` sets
  `MAX_HOPS = racer_wire::route::MAX_LINKS`.
- `racer-authority` exposes pure rule functions in `authority/src/rules/` (seat
  lottery and renewal, primary contention and plan pass, seat admission, authorization). Wall
  time and entropy are inputs: `now_micro` takes a
  `SystemTime` (`authority/src/rules/election.rs:126`) and `new_key` takes an `Entropy`
  (`authority/src/rules/keyring.rs` `new_key`).

## racer-dst

Path: `cmd/racer/dst`, crate and binary `racer-dst`. One engine, one choice stream, one cost
model (`dst/costs.toml`) and one invariant set serve two fidelities (`dst/src/lib.rs`).

| Module | Role |
| --- | --- |
| `engine` | Virtual time in `u64` ns, event queue with deterministic ties, trace hash |
| `choices` | The choice stream: seeded PRNG, fuzzer bytes, record and replay, shrinking, seed files |
| `costs`, `resources` | The cost table and FIFO servers for NIC, disk, CPU, crypto, origin and the site RTT matrix |
| `world`, `fixture` | Full fidelity: nodes, roles, the one-thread scheduler and faults over simulated `racer-io` |
| `authority` | Full fidelity control plane: real `racer-authority` authorities against the fake API |
| `model` | Model fidelity: a byte-free interpreter over real `WorkerCore`s, `topology::Placement` with `Phantom` rankings, the control plane (`model/control.rs`, driving `authority` rules) and combined runs |
| `invariants`, `determinism` | Invariant registration, double-run trace comparison and failure shrinking |
| `scenario`, `scenarios`, `cli` | Scenario builder, the catalog run by `make racer-dst`, and the one-scenario command line |

### Full fidelity

Real application code runs on the simulated driver. `dst/FULL.md` describes the control plane
world (real authorities against `simulation::FakeKube`, built with racer-authority's `sim`
feature), its fault model and invariants.

### Model fidelity

Interpreters are dispatch-only: every decision comes from `racer-core` (`WorkerCore`, the walk,
flights, serve, liveness, route budgets) and `topology`; the interpreter prices each effect with
`resources` and completes it as an engine event. `dst/MODEL.md` lists what is modeled, what is
not, the scenario families and their checks, and the accepted divergences from production.

### Phantom caches

A phantom cache makes the same decisions as the real cache but stores no payload. Placement is
`Placement<R: Rankings>` (`topology/src/placement.rs`) with `Phantom` rankings backed by a shared
memo keyed by membership and slot (`dst/src/model/phantom.rs`); CLOCK, pinning and `Overloaded`
behavior are the real code.

### Scenarios and command line

`make racer-dst` runs `dst/seeds/ci.txt` (each entry twice, comparing trace hashes); the catalog
is `dst/src/scenarios.rs`. `make racer-dst-report ARGS=...` runs one scenario and writes its JSON
report:

```sh
make racer-dst-report ARGS='--scenario full-churn --nodes 200 --seed 7 --duration 300s'
make racer-dst-report ARGS='--scenario full-cold-start --nodes 50000 --config cfg.json --out r.json'
```

Names are the control plans (`enrollment-storm`, `failover`, `ctl-churn`, `lagging`), the
combined scenarios (`full-cold-start`, `full-failover`, `full-churn`, `full-lagging`), short
aliases mapped to Model scenarios (`dst/src/cli.rs` `ALIASES`) and every catalog name.
`--config` takes a JSON object of the keys in `dst/src/cli.rs` `KEYS` and `NODE_KEYS` (see
`dst/MODEL.md`, Running); unknown keys are errors. `RACER_DST_REPORT_TIMEOUT` (default 7200s) bounds the make target.

### Mesh v2 metrics

Topology-only mesh measurements (peers per node, route links by transport, edges changed by
churn, selector pools, failover rebuilds) are `topology/tests/mesh_scenarios.rs`; the hot page
storm with mesh helpers is `model-hot-page-storm-512` (and the ignored 2,048-node test in
`dst/tests/model_scenarios.rs`).

## Memory budget

32 GiB / 50,000 nodes is about 671 KiB per node. Model per-node state holds no payloads and no
per-node copies of shareable data. `dst/tests/model_scenarios.rs` `model_memory_per_node` measures it
(`dst/MODEL.md`, Running, has the command). Measured peak RSS: `full-cold-start` at 50,000 nodes peaks at
3.10 GB (62 KB per node).

## Determinism and replay

- All time comes from the engine clock. All randomness comes from the choice stream
  (`dst/src/choices.rs`); `dst/tests/no_random_state.rs` rejects randomly seeded hash maps.
- Each recorded event feeds an order-sensitive trace hash. `make racer-dst` runs every seed entry
  twice and fails when the hashes differ; `dst/tests/model.rs`
  `model_runs_are_deterministic_and_seed_dependent` and `dst/tests/combined.rs` `replays_exactly`
  check the Model.

To reproduce a run, use the same scenario, node count, seed, duration and config with
`make racer-dst-report`, or a seed file entry with `DST_SEEDS`.

## Invariants

Every run checks its scenario's invariants and checks (`dst/src/invariants.rs`, `ModelSpec::check`
in `dst/src/scenarios.rs`); a failure is reported with time, node and detail. The Model report
classifies every repeated origin request (`dst/src/model/repeats.rs`) and counts unexplained
repeats, which the ported tests require to be zero.

Control plane (`dst/src/model/control.rs`):

- `ctl_single_primary`, `ctl_terms_monotonic`, `ctl_sequence_monotonic`.
- `ctl_client_membership_monotonic` per client.
- `ctl_client_converges`: after faults heal, every live client applies every committed sequence
  within a fixed window.

## Design decisions found through simulation

These are production rules that simulation (first racer-sim, now racer-dst) motivated. Code
comments refer to them by name.

### Joining members

A new member is added to the membership only after it reports that it serves peer links
(`src/control/session.rs` `mark_serving`). Before that, a listed member would refuse every link
and reads toward it would fail. Joins are batched by a hold that grows with membership size
(`join_hold_secs` in `authority/src/rules/primary.rs`), so clients stay within one version
during large enrollments instead of falling back to full images.

### Hot page fanout

An owner with more than `HOT_PAGE_QUEUE_BYTES` of queued egress for one page sheds further readers
with `Overloaded`, and readers fetch from the page's helpers instead
(`core/src/read/budget.rs`). The limit is in bytes so it means the same backlog for 1 MiB
simulated pages and 16 MiB production pages. A page is sent by at most `REPLICAS` owners plus
`fanout_width` helpers.

### Route budget

App and wire share one rule: a route carries at most 8 links (`racer_wire::route::MAX_LINKS`,
also `MAX_ROUTE_LINKS` in `core/src/read/budget.rs`).

## Known limitations

These are bounded behaviors, not bugs, and are covered by tests or the invariants above.

- Origin requests per page can exceed one when rank 0 changes, a copy is lost, or origin returns
  5xx and the client retries. Repeats while another owner holds the page are classified
  (`dst/src/model/repeats.rs`).
- A relay-signed `Overloaded` on the route to rank 0 can give an owner above rank 0 origin
  authority, allowing at most one overlapping origin request per such walk.
- A helper probe spends its own grant (`walk::helper_grant`: `HELPER_ATTEMPTS`, `HELPER_LINKS`),
  never the walk's, once per walk, so a helper that fails still leaves the remaining owners
  funded. A page read that asks a helper may therefore send up to `HELPER_ATTEMPTS` attempts and
  `HELPER_LINKS` links beyond its page budget.
- Silence pings add peer messages under many probe timeouts. Each ping follows a timeout or a
  recheck, and a node has at most `MAX_PINGS_IN_FLIGHT` pings and `MAX_OFFERS_IN_FLIGHT` copy
  offers in flight, as in production.
