# Model fidelity: what is modeled

`Fidelity::Model` runs `racer_dst::model`: a byte-free interpreter over the real
`racer_core::read::worker::WorkerCore` machines. It is dispatch-only: every decision
(where to probe, when to reach origin, whom to shed, which flight to join) is made by
the cores. The interpreter completes each effect on the FIFO cost model
(`src/resources.rs`) priced from `costs.toml`, and answers it with facts of the world,
such as which node holds which page.

## Modeled

- N nodes with W real `WorkerCore`s each. A page's flight lives on the worker that
  `owner_worker` maps it to; client page reads land on that worker. A client range read
  runs its `ReadWindow` on worker `node % W`, so pages owned by other workers cross as
  `WorkerMsg::Fetch`/`Fetched`.
- Client: racer-sim's retry rule (`resolve` in `sim/src/node/world.rs`): a recoverable
  failure is retried up to 4 attempts with a linear backoff; latency runs from the first
  attempt.
- Range reads (`Storm::range`): windows with client credits (`window_pages`), probes of
  the node store, spill holds and spill fills, and buffered-page discards. A provider
  selection is answered as not started, so its pages are filled directly.
- Metadata (`ModelSpec::metadata`): each page read first refreshes its object through
  the worker's `MetadataCohort`; the refresh is priced as an origin HEAD and draws the
  origin error mix.
- Origin errors (`ModelSpec::origin_errors`): racer-sim's mapping (412 to
  `VersionUnavailable`, 403 to `OriginForbidden`, 5xx to `Unavailable`).
- Cache outages (`ModelSpec::outages`): the node loses its stored pages and answers peer
  probes with `Miss` (`ServeFacts::cache_available` false) until the outage ends.
- Page store: published pages per node, bounded by `ModelSpec::stored_pages` with
  least-recently-used eviction (0 is unbounded).
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
  serving read is answered, every body ends, every range read and metadata refresh ends,
  and every core is quiescent (no flights, requests, acquisitions, windows or metadata
  requests) at the end. Violations count as `hard_failures`. Scenario checks run on the
  JSON report.
- Coverage parity (plan section 9.2): `src/model/coverage.rs` lists every core effect and
  serve outcome variant. `make racer-dst` reruns the CI Model entries once and fails when
  a variant is not exercised, unless it is unreachable by construction or listed in
  `NOT_YET` with its reason. A `NOT_YET` variant that the CI set does exercise also fails,
  so the list cannot go stale.

## Not modeled yet

- Hedges (`Hedge` answers `NotStarted`; `FillConfig::hedge` is off), successor hint notes
  and orphan notices (`Hints` answers no hints, `Batch` answers no copy), so successor
  deferral (`Served::Defer`) never happens.
- Membership changes: every read uses version 1 of one membership, so `Refresh` and the
  stale-membership `Sleep` never run.
- Mesh relays and route failures (racer-sim's `SharedRoutes` in `sim/src/phantom.rs`):
  probes and transfers go directly between nodes. Relay quotas and relay sheds are absent.
- The production 2 s per-attempt cap is implemented (`ModelSpec::attempt_timeout`,
  `model::ATTEMPT_TIMEOUT`) but off by default (`NO_ATTEMPT_CAP`: an attempt ends at its
  planned overall deadline). With the cap the Model fails more reads than racer-sim (see
  Scale) and fails one read of `model-hot-page-storm-512`, so it stays off until that gap is
  understood. Probes are otherwise planned as production does: `plan_attempt` on the read's
  request budget, serving reads on `inherited_budget` within a 5 s serve scope, abandoned
  exchanges stop their transfer.
- Node crash/restart, partitions and slow peers. Cache outages are the only node fault.
- Admission quotas, persistence shedding (`persist_no_headroom`), memory tiers and the
  DiskIndex; per-node memory accounting (`memory.accounted_bytes_per_node`).
- Provider selection streams (`Offered`), prefetch traffic.
- Rejections (`Served::Reject`): Model probes always match the object and operation.

## Ported scenarios

Every racer-sim number below is from the racer-sim release binary at this commit
(`racer-sim --scenario empty --config <json> --seed <seed>`, configs equal to the test's
`Config`), and every Model number from `cargo test -p racer-dst --test model_parity` (or
`--test model`) with the same seed. Latency is the client read latency in ms
(p50/p99/max). "Origin/page" is the most origin requests for one page.

| racer-dst scenario | racer-sim source (seed) | Checks (racer-sim's) |
|---|---|---|
| `model-concurrent-cold-readers` | `concurrent_cold_readers_join_flights` (2) | every read delivered, flight joins, origin at most `pages * REPLICAS`, at most `2 * REPLICAS` per page, origin under 1/8 of reads |
| `model-cold-start-400`, `model-cold-start-2000` | `cold_start_completes_with_bounded_origin_traffic`, `two_thousand_nodes_cold_start` (8) | every read ends, failures only `Overloaded`/`Unavailable` and at most 1%, no `HopBudgetExhausted`, origin/page in `1..=REPLICAS`, pages published |
| `model-hot-page-storm-512` | `hot_page_readers_fan_out_through_helpers`, `mesh_hot_page_readers_fan_out_through_column_helpers` (5) | every read completes, owners shed, helpers serve copies, origin once |
| `model-origin-forbidden` | `origin_forbidden_reelects_a_waiter` (11) | 403s fail reads with `OriginForbidden`, the rest complete |

| Scenario | Metric | racer-sim | Model |
|---|---|---|---|
| cold readers, 64 nodes | completed / failed | 1024 / 0 | 1024 / 0 |
| | origin requests, origin/page | 16, 1 | 16, 1 |
| | flight joins | 1008 | 1008 |
| | latency ms | 59 / 109 / 121 | 77 / 148 / 153 |
| cold start, 400 nodes | completed / failed | 6400 / 0 | 6400 / 0 |
| | origin requests, origin/page | 30, 3 | 30, 3 |
| | flight joins | 6061 | 8638 |
| | sheds / helper copies | 91 / 3332 | 0 / 2250 |
| | latency ms | 92 / 185 / 220 | 167 / 457 / 5093 |
| cold start, 2000 nodes | completed / failed | 32000 / 0 | 31959 / 41 (33 `Unavailable`, 8 `Overloaded`) |
| | origin requests, origin/page | 43, 3 | 40, 3 |
| | flight joins | 30204 | 66227 |
| | sheds / helper copies | 1536 / 27734 | 6229 / 26890 |
| | latency ms | 126 / 2013 / 2125 | 244 / 10214 / 15326 |
| hot-page storm, 512 nodes | completed / failed | 513 / 0 | 513 / 0 |
| | origin requests | 1 | 1 |
| | sheds / helper probes / helper copies | 300 / 496 / 324 | 848 / 847 / 441 |
| | flight joins | 614 | 841 |
| | latency ms | 1879 / 2953 / 3192 | 5812 / 7097 / 7247 |
| origin 403, 64 nodes | completed / failed | 1004 / 20 | 1007 / 17 |
| | origin requests, origin/page | 36, 6 | 33, 12 |
| | latency ms | 92 / 218 / 225 | 82 / 409 / 418 |
| | flights reelected | 20 (`flight_reelected`) | 17 |

Tolerances. The Model passes every racer-sim check above, with these documented
differences:

- Origin requests: within 10% of racer-sim on every scenario (exact on cold readers,
  cold start 400 and the hot page).
- Completed and failed reads: equal, except cold start 2000 (41 of 32000 failures, inside racer-sim's own 1% bound) and origin 403 (fewer `OriginForbidden`
  failures; the 403 draw order differs because the Model's origin draws come from the
  DST `Choices` stream).
- Latency: the Model is slower in the tail (p99 up to 5x on cold start 2000, where reads
  wait out helper chains up to the 5 s serve scope; see Scale). The 2 s attempt cap is off
  by default and there are no relays. Treat Model latency as an upper bound, not a match.
- Sheds, flight joins and helper copies: within an order of magnitude, not closer. The
  Model counts a join for every client or serving read that finds a running flight on its
  worker, including retries; racer-sim's counter may count differently (inference, not
  yet verified). On cold start 400 the Model
  never sheds (its helper copies come from flight-full `Overloaded` answers), because
  without relays owners never stream enough bodies at once to cross the hot-page threshold.
- Origin/page under 403s: an interpreter draw-order difference, not a core decision
  difference. In both racer-sim and the Model every 403 fails exactly its leader and re-elects
  one waiter (`flights_reelected` equals `origin_403`, asserted in
  `origin_forbidden_fails_only_the_leader`), so origin requests are always pages + 403s
  (racer-sim 16 + 20 = 36, Model 16 + 17 = 33). The per-page maximum is the longest run of
  403 draws on one page; over Model seeds 1..20 it ranges 3..12 (mean 6), and seed 11 happens
  to be the 12 tail.

## Scale

Root cause of the old 16% (10k) and 63% (50k) cold-start failures, with evidence from the
`attempt_errors` breakdown (`MODEL_DIAG=1` adds probe, serve and send breakdowns): serving reads
that failed were always answered `Unavailable`. 433,028 of them failed `Overloaded` (their owner
shed them or its flight was full), so the requester saw `Unavailable` instead of a shed and never
asked its helper; attempts failed within a second and retries burned out. The Model now answers
a failed serving read with production `peer_error` (racer-sim `err_reply`); at 10k that alone
took failures from 24,938 to 15. Two more interpreter facts were wrong and are fixed: owners
never exempted their column mates (`ServeView::column_mate` was always false) and serving reads
ran on a fixed budget instead of the probe's inherited one.

With faithful inherited budgets the numbers below are worse than with the old fixed serving
budget, and worse than racer-sim (cold start, seed 8, `racer-sim --scenario empty` with the
same storm; Model `model_memory_per_node`, release, no attempt cap):

| Nodes | racer-sim failed | Model failed | Model wall | Model peak RSS per node |
|---|---|---|---|---|
| 2,000 | 0 of 32,000 | 41 (0.13%) | under 1 s run | 75,216 B |
| 10,000 | 180 (0.11%), 5,139 probe timeouts | 3,944 (2.5%); 10,707 (6.7%) with the 2 s cap | 4 s run | 79,405 B |
| 50,000 | 24,742 (3.1%), 16,379 probe timeouts; 8 min 45 s, 8,041,764 KiB RSS | 67,712 (8.5%) | 2 min 20 s | 82,272 B |

The remaining gap is inferred, not proven: with the 2 s cap every Model timeout is a reader's
probe to its overflow helper (`timeout:reader->helper`), and at the moment it fires the helper is
itself waiting on a probe to another non-owner (32,147 of 53,014) or an owner (16,054): helper
flights chain through further helpers when owners shed column mates past the exempt cap, and
serving reads that join such a flight wait out the chain. racer-sim reports 75k fast
`helper_misses` where the Model's helpers wait. Next steps: relays (racer-sim routes through the
delegate, which is also the helper), and comparing racer-sim's helper serve path line by line.

Not ported yet (they need the gaps above): `slow_disk_sheds_persistence...` (persistence
shedding), `reads_recover_after_partition_heals` (partitions), the relay, hung-peer and
quota tests in `contention_scenarios.rs`, `sim/tests/mesh_scenarios.rs` (relays and
routes) and `sim/tests/phantom_memory.rs` (racer-sim memory accounting; see the memory
section).

## Model-only scenarios

| Scenario | What it covers |
|---|---|
| `model-range-readers` | range reads through `ReadWindow` with credits, spill holds, selections, worker fetches; a second pass reads every page from disk |
| `model-metadata-readers` | page reads behind `MetadataCohort` refreshes with 10% origin 5xx; refreshes coalesce per cohort |
| `model-faults` | origin 403/5xx mix, four cache outages, page and range readers; failed windows discard buffered pages |

## Memory

`model_memory_per_node` in `tests/model_parity.rs` (ignored; run alone in release with
`MODEL_NODES=<n>`) reports the peak RSS of a Model cold start divided by the node count.
The racer-sim baseline is 103,189 B/node for `full-cold-start` at 50,000 nodes (peak RSS).

| Nodes | Peak RSS | Per node | Wall (release) | Notes |
|---|---|---|---|---|
| 2,000 | 150,433,792 B | 75,216 B | under 1 s | 0.13% of reads fail |
| 10,000 | 794,058,752 B | 79,405 B | 4 s | 2.5% fail (see Scale) |
| 50,000 | 4,113,645,568 B | 82,272 B | 2 min 20 s | 8.5% fail (see Scale) |

At 50,000 nodes the Model uses about 80% of racer-sim's per-node peak (82,272 B against
103,189 B) and runs in minutes where racer-sim's `full-cold-start` takes 39 min. The
workloads differ: racer-sim's `full-cold-start` runs the full control plane, and the Model
runs the contention cold start (every node reads one 16-page object). The failure rate at
this size is a parity gap, not a memory result.
