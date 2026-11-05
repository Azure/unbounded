# Model fidelity: what is modeled

`Fidelity::Model` runs `racer_dst::model`: a byte-free interpreter over the real
`racer_core::read::worker::WorkerCore` machines. It is dispatch-only: every decision
(where to probe, when to reach origin, whom to shed, which flight to join) is made by
the cores. The interpreter completes each effect on the FIFO cost model
(`src/resources.rs`) priced from `costs.toml`, and answers it with facts of the world,
such as which node holds which page.

## Modeled

- N nodes with W real `WorkerCore`s each. A page's flight lives on the worker that
  `owner_worker` maps it to; client reads land on that worker.
- Placement: a real `topology::Placement` over `Phantom` rankings (copied from
  racer-sim's `phantom.rs`) and a real `topology::Membership`. Member positions are
  node ids.
- Page flights: joins, leader election, handover and settlement run in the core's
  `FlightTable`, including on owners that serve peer `Acquire` probes.
- Acquisition walks: ranking, peer probes, validation, origin fetches and publication
  run in the core's `PageAcquisition`.
- Serve side: `WorkerCore::serve` (dispatch table, hot-page shedding over live body
  counters, copy-only answers, successor deferral). A body counts as streaming from
  its first byte until its last byte lands.
- Overflow helpers: the core resolves a shed requester's helper with `mesh_helper`
  over `Pool::helper` of the real membership mesh.
- Costs: ranking and dispatch on per-worker CPU servers, page transfers on per-node
  NIC transmit and receive servers (direct route, chunked), payload open on the
  crypto server, publication on the disk write server, origin fetches on the shared
  origin model (per-cache concurrency, shared bandwidth, latency), RTT from the
  per-site matrix.
- Invariants: every effect id is answered at most once, every client read ends, every
  serving read is answered, every body ends, and every core is quiescent (no flights,
  requests or acquisitions) at the end. Violations count as `hard_failures`. Scenario
  checks run on the JSON report.

## Not modeled yet

- Range windows (`ReadWindow`), spill holds and provider selections; the interpreter
  reports a hard failure if a core emits a window effect.
- Metadata cohorts and refresh traffic; every read uses version 1 of one membership.
- Hedges, successor hint notes and orphan notices: `Hints` answers with no hints,
  `Hedge` answers `NotStarted`, `Batch` answers with no copy.
- Mesh relays and route failures: probes and transfers go directly between nodes.
- Faults (crash, partition, slow peers), membership changes, origin errors and
  timeouts. A serving read inherits half the client read timeout instead of the
  requester's remaining budget.
- Memory and disk tiers beyond a per-node set of published pages: no eviction, no
  admission quotas, no persistence shedding.
- Prefetch (`OwnerBudget` is owned by each core but no prefetch traffic runs).

## Ported scenarios

| racer-dst scenario | racer-sim source | Checks |
|---|---|---|
| `model-concurrent-cold-readers` | `concurrent_cold_readers_join_flights` in `sim/tests/contention_scenarios.rs` | `hard_failures == 0`, every read delivered, flight joins happen, origin requests at most `pages * REPLICAS`, at most `REPLICAS * 2` origin requests per page, origin requests under 1/8 of reads |

The remaining racer-sim scenarios (`contention_scenarios.rs`, `mesh_scenarios.rs`,
`phantom_memory.rs`) still run in racer-sim. Porting them needs the items above, mainly
faults, mesh relays, memory tiers and windows.
