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
- Node and network faults from the scenario's schedule (`Scenario::faults` and, when
  `fault_mix.max_faults` is set, the drawn `fault_schedule`): `Crash`/`Restart` take a node's
  peer server down (racer-sim `link`, `hop`: a link to it fails after the 100 ms connect
  timeout, a request at it is lost, a reply from or through it is lost); `Partition`/`Heal`
  cut a node pair (links fail, replies over the cut are lost, racer-sim `send_reply`);
  `SlowPeer` scales the node's service times (`Fabric::slow`, as racer-sim `Fault::Slow`).
  Hung peer servers (`ModelSpec::hangs`, racer-sim `Fault::Hang`) accept requests as
  destination or relay and never answer; pings to them time out.
- Cache outages (`ModelSpec::outages`): the node loses its stored pages and answers peer
  probes with `Miss` (`ServeFacts::cache_available` false) until the outage ends.
- Page store: published pages per node, bounded by `ModelSpec::stored_pages` with
  least-recently-used eviction (0 is unbounded).
- Placement: a real `topology::Placement` over `Phantom` rankings (copied from
  racer-sim's `phantom.rs`) and real `topology::Membership` versions.
- Membership versions (`src/model/membership.rs`, racer-sim `Versions` in standalone mode):
  `ModelSpec::churn` and `random_churn` make nodes leave and rejoin; changes publish in
  250 ms batches, each node adopts a version up to `membership_lag` (100 ms) late and
  resolves the 2 versions before it (`RETAINED`). Rankings carry their version: the core's
  `plan`, `mesh` and `helper_of` closures read the version their node adopted, `Rank` answers
  `Verify`, `Latest`, `Previous` (non-members as `NOT_MEMBER`, `recent` within
  `PREVIOUS_MEMBERSHIP_WITHIN`) and `Fallback` under the named version, and `Refresh` reports
  the adopted version and the recent previous one. A holder that does not resolve a
  request's version answers `StaleMembership` (`decide_hop`), and the requester gets the
  unused remote credits and links back (production `request_traced`,
  `stale_unused_links`). Routes, neighbors, columns and helpers map node ids to the
  version's member positions. A node outside the membership it adopted takes no client
  reads (`reads_skipped_not_member`; production `/readyz`).
- Page flights: joins, leader election, handover and settlement run in the core's
  `FlightTable`, including on owners that serve peer `Acquire` probes.
- Acquisition walks: ranking, peer probes, validation, origin fetches and publication
  run in the core's `PageAcquisition`.
- Serve side: `WorkerCore::serve` (dispatch table, hot-page shedding over live body
  counters, copy-only answers, successor deferral). A body counts as streaming from
  its first byte until its last byte lands.
- Visit order: a non-owner's Rank is answered with production `visit_order` (`Spread` over
  `replica_score`, mesh neighbors first) and `demote_unanswered` over a node-wide
  `liveness::Silence`, fed by the core's `Effect::Silence` (production `CandidatePolicy::apply`
  into `LinkHealth`; racer-sim `apply`) and by last-link timeouts, and cleared by silence
  pings (`poll_pings`).
- Flight tables use production admission limits (`admission::Limits::default`: 64 flights of 64
  waiters per worker, as racer-sim's `NodeConfig`). A join past the entry cap is answered
  `Overloaded`; a join past a flight's waiter cap is answered `FlightFull`
  (`core/src/read/flight.rs` `JoinReject::WaiterCap`), which proves the flight is fetching.
- Ranking CPU: a worker's first ranking of a page under a membership costs one
  `ranking_chunk_ns` per 256 members (production `topology/src/placement.rs` `rank_async`,
  `WORK_QUANTUM`; racer-sim `spawn`), later ones one chunk (the worker's placement cache).
- Overflow helpers: the core resolves a shed requester's helper with `mesh_helper`
  over `Pool::helper` of the real membership mesh.
- Mesh relays (`Model::forward`, `hop`, `link`, `fail_hop`; racer-sim `forward`/`hop`/
  `link`/`fail_hop`; production `src/peer/forwarding.rs`): every probe is routed by the real
  `topology::Paths` with the core's route budget (`route::search_budget`, `decide_hop`,
  `relay_failure`). Each holder skips silent neighbors and its `liveness::Detours` (fed by
  the core's timed-out `Effect::Silence`), signs, and the next holder verifies before it
  relays or serves. Replies and page bodies travel the route back. A last link still
  unanswered when the requester gives up is a silence timeout for its sender
  (`LINK_TIMEOUTS`), which can start pings. The serving read's inherited budget counts the
  route's links.
- Relay transfers: a relay reserves one `ResourceClass::Relay` transfer on its worker before
  it forwards (production `src/peer/server.rs:540`, limit `relay_transfers` 16 per worker,
  `src/worker.rs:265`, `core/src/admission.rs:145`); none free fails the hop `Overloaded`
  (`relay_sheds`). The reservation is held until the answer has been written back upstream
  (production `send_http_response` clears `relay.reservation`), or for a page until the body
  has passed (racer-sim `send_reply`, `held`; Model `reply`, `release_relays_at`). Both
  fidelities used to release a non-page answer's relays when the destination decided it. The
  run fails if any is still held.
- Route search slots: production builds its `Paths` with `with_limits(.., active_path_searches)`
  (`src/app.rs:1320-1333`, `RACER_ACTIVE_PATH_SEARCHES_PER_WORKER` default 8,
  `src/worker.rs:261`) and no wait queue, so a search past 8 running on the worker fails
  `Overloaded` (`topology/src/paths.rs` `route`, `queue_limit` 0). The Model holds a slot until
  the search's CPU work is done (`search_sheds`). racer-sim's `search_queue` now defaults to 0
  as well (`sim/src/config.rs` `NodeConfig::default`). A requester's own shed search fails the
  probe `Overloaded` (production `request_network`, `src/peer.rs:919-931`), not `Unavailable`:
  it is no evidence against the destination (Model `fail_hop`, racer-sim `Reply::Overloaded`).
- Peer crypto costs (peer wire v2): signing a request with n proofs costs `sign_ns + (n - 1) *
  sign_hop_ns`, verifying it `verify_ns + (n - 1) * verify_hop_ns`, signing a response
  `sign_response_ns` (racer-sim `Costs::sign_request`/`verify`, `sim/src/config.rs`; Model
  `costs.toml` `[crypto]`, `Costs::sign_request`/`verify`). Values come from the ignored
  `peer_crypto_cost_benchmark` (`src/peer/tests/opaque.rs`, release): 64, 76 and 114 us to sign
  a direct request, a relayed request and a response; 73 and 160 us to verify one and two
  proofs. They were 20 us per sign and 40 us per verify. A response is verified at
  `verify_ns` whatever its relays (an inference; it was not measured). Head bytes are not
  charged: peer messages are latency only (`Fabric::message`), and a v2 head (2,075 B direct,
  3,208 B relayed) is small next to a 16 MiB page.
- Request verification: every holder verifies the request on the crypto helper of the
  connection's worker and dispatches or relays only after (production `src/peer/server.rs:515-518`,
  `:792`; `src/worker.rs` `WorkerPair`). racer-sim does the same since this commit (`hop`,
  `Msg::verified`, per-worker `crypto_free`); before, it charged one helper per node and went on.
- Crypto queue bound: a worker's crypto client admits `queue_entries` port slots plus as many
  waiters (`src/security.rs` `CryptoClient::new`, `runtime/src/offload.rs`
  `AdmissionQueue::enter`), `queue_entries` 256 per worker (`core/src/admission.rs:138`,
  per worker after `Config::scale_limits`), so 512 jobs. A request verification past it fails
  `Overloaded` before any key work. Production used to return that error from
  `serve_connection`, which closes the connection (the previous holder read `Io`: a relay
  signed `Unavailable`, a requester counted a transport failure). It now answers the unsigned
  overload head (`peer_wire::overloaded_head`, `racer_core::route::unverified_failure`), which
  the previous holder reads as `Overloaded` (`src/peer/transport.rs`). Both fidelities count
  every crypto job a worker's helper holds (`Fabric::crypto_outstanding`) and shed a
  verification at `crypto_queue` (racer-sim `NodeConfig::crypto_queue`, Model
  `ModelSpec::crypto_queue`, both `Limits::crypto_submissions`, `core/src/admission.rs`) through the same table (`verify_sheds`,
  racer-sim `verify_sheds_dest`/`verify_sheds_relay`). The shed is not silence: the last-hop
  record is dropped. Before this, both fidelities queued verifications without bound.
- Pings: a silence ping is a peer request routed over the mesh (production
  `CandidatePolicy::ping` through `request_traced`, `src/read/candidates.rs:2488-2531`;
  racer-sim `launch_control`) on `NOTICE_LINKS` links with no remote attempts; the
  requester's own ping may cross the silent link it tests and detours at least once. The
  destination answers `Miss` at once; `ping_verdict` classifies the end.
- Costs: ranking and dispatch on per-worker CPU servers, page transfers on per-node
  NIC transmit and receive servers (over the route, chunked), route searches on the
  holder's worker, payload open on the
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

## Peer connection slots

Production semantics (cited from `cmd/racer/src`; the shared table is
`core/src/connections.rs`, opt-in: Model `ModelSpec::connections`, racer-sim
`NodeConfig::connections`, off by default so existing scenarios keep their transport):

- Ingress: 45 `IngressConnection` slots per worker (`core/src/admission.rs` `Rules::limit`:
  `client_connections` 64 less 3 control and 16 outbound), shared node-wide: the listener
  reserves a slot on any worker before `accept` (`src/peer/server.rs:255-292`,
  `AcceptMode::Distributed`, chosen at `src/app.rs:1569-1589`; `flow-core/src/admission.rs`
  `Handoff::reserve`). With every worker full it stops accepting (`Poll::Pending` at
  `server.rs:270`); new connections wait in the kernel accept queue (std `TcpListener::bind`
  backlog 128, `io/src/ring/fd.rs:118-121`; an inference for hosts whose `somaxconn` is
  lower). The sender's connect completes and it waits until its attempt times out; past the
  backlog the handshake is dropped and it hears nothing. Neither is an `Overloaded` answer.
- A connection serves one exchange at a time and stays open between them: the server waits
  `request_timeout` (30 s, `src/config.rs:2262`) for the next head (`server.rs:451`
  `header_scope`), the sender pools it idle for 30 s (`src/http.rs` `pool_with_limits`). An
  idle connection keeps its ingress slot.
- Outbound: each worker's HTTP pool holds at most 15 ordinary connections, busy or idle
  (`OrdinaryOutboundConnection`, `src/admission.rs:431-457`), and 2 busy per next hop
  (`connections_per_neighbor`, `src/app.rs:1346-1350`, `src/http.rs:425-428`). Peer
  exchanges check out without waiting (`src/peer/transport.rs:1296` `checkout_with_state`;
  `http/src/connection.rs:1702-1704` per-endpoint cap, `:1796-1802` clears idle then fails):
  past either cap the exchange fails `Overloaded` at once.

Cold start (16-page object read by every node in 10 ms, failed reads, origin requests, read
latency p50 / p99). "off" is the previous transport with worker signing; "prod" production
connection semantics; "evict" and "refuse" proposed fixes (not production: a full listener
closes its longest-idle inbound connection, and answers the overload head when nothing is
idle); "wide" adds 1,024 ingress slots per node. Release builds, `tmp/parity/run.sh`:

| Nodes | Seed | Mode | Model failed | Model origin, p50/p99 | racer-sim failed | racer-sim origin, p50/p99 |
|---|---|---|---|---|---|---|
| 2,000 | 8 | off | 1 (0.00%) | 16, 163 ms/2.13 s | 1 (0.00%) | 16, 150 ms/2.01 s |
| 2,000 | 8 | prod | 2,034 (6.4%) | 24, 2.11/12.1 s | 3,421 (10.7%) | 26, 2.01/8.05 s |
| 2,000 | 3 | prod | 1,880 (5.9%) | 24, 2.10/12.1 s | 3,295 (10.3%) | 21, 2.01/8.05 s |
| 2,000 | 8 | evict | 533 (1.7%) | 16, 2.09/6.13 s | 168 (0.5%) | 18, 2.01/5.91 s |
| 2,000 | 8 | evict+refuse | 510 (1.6%) | 16, 149 ms/2.06 s | 284 (0.9%) | 16, 134/301 ms |
| 10,000 | 8 | off | 22 (0.01%) | 16, 248 ms/2.18 s | 42 (0.03%) | 16, 218 ms/2.15 s |
| 10,000 | 8 | prod | 55,968 (35.0%) | 28, 4.09/10.1 s | 58,959 (36.9%) | 31, 4.03/8.05 s |
| 10,000 | 3 | prod | 55,451 (34.7%) | 29, 4.11/10.1 s | 60,683 (37.9%) | 30, 4.03/8.05 s |
| 10,000 | 8 | evict | 24,492 (15.3%) | 22 | 31,102 (19.4%) | 20 |
| 10,000 | 8 | evict+refuse | 14,376 (9.0%) | 16, 275 ms/2.19 s | 21,557 (13.5%) | 16, 268 ms/2.15 s |
| 10,000 | 8 | wide | - | - | 1,626 (1.0%) | 16, 251 ms/2.15 s |
| 50,000 | 8 | off | 31,338 (3.9%) | 16, 469 ms/2.40 s | 15,396 (1.9%) | 16, 402 ms/2.42 s |
| 50,000 | 8 | prod | 589,542 (73.7%) | 46, 2.35/12.1 s | 594,189 (74.3%) | 37, 4.03/8.05 s |
| 50,000 | 3 | prod | 591,552 (73.9%) | 44, 2.47/10.3 s | 604,347 (75.5%) | 36, 4.03/8.05 s |
| 50,000 | 8 | evict | 492,456 (61.6%) | 34 | 509,754 (63.7%) | 28 |
| 50,000 | 8 | evict+refuse | 425,685 (53.2%) | 16, 293 ms/2.12 s | 469,263 (58.7%) | 16, 335 ms/2.15 s |
| 50,000 | 8 | wide | - | - | 50,483 (6.3%) | 16, 369 ms/2.15 s |

Findings:

- The fidelities agree within 1.3x everywhere with production semantics, and both say
  production fails the cold start badly: 6 to 11% at 2,000, 35 to 38% at 10,000, 74 to 76%
  at 50,000 (both seeds). Without connection slots the same fidelities fail 0.00 to 3.9%.
- Bottleneck, in order: hot owners' and relays' ingress (90 slots per 2-worker node) with
  connections queued in the accept queue past their 2 s attempts (50,000, seed 8: 1.6 to
  1.9 M queued, 185 to 279 K abandoned in the queue, 385 to 438 K dropped by the full
  backlog), outbound per-hop and pool refusals (4.3 to 4.4 M `Overloaded` links), then route
  search sheds. Idle pinning is a large share at 2,000 (eviction cuts failures 4 to 20x) and a
  smaller one at scale. Raising ingress to 1,024 per node with eviction and refusal brings
  racer-sim to 1.0% at 10,000 and 6.3% at 50,000: the cap, not crypto, now bounds the cold
  start, and the crypto queue no longer sheds at all.
- Proposed production fixes (network side, not implemented): close idle inbound peer
  connections when ingress is full (or a short server-side keepalive for peer connections);
  answer the unsigned overload head instead of leaving connections in the accept queue; size
  peer ingress separately from `client_connections` (a peer exchange holds little memory) or
  multiplex exchanges per connection; let peer checkout wait briefly (`checkout_wait`) instead
  of failing at the 2-per-hop cap; give route searches a queue.
- Not modeled: the accept queue is admitted on the next event after a slot frees (no
  dedicated timer); a refused connection is seen at once rather than one round trip later;
  connection setup costs no time; the server's worker per connection is the Model's
  `conn_worker`, not the listener's choice.

## Not modeled yet

- Hedges (`Hedge` answers `NotStarted`; `FillConfig::hedge` is off), successor hint notes
  and orphan notices (`Hints` answers no hints, `Batch` answers no copy), so successor
  deferral (`Served::Defer`) never happens.
- Membership changes come from scenario churn, or in combined scenarios from the control
  plane (see Control plane below). A node that left keeps serving peers and relaying.
- Link circuits (`Circuits`). Route searches run synchronously against one shared `Paths`
  cache (production keeps one per worker), so the Model has more cache hits and fewer
  searches needing a slot.
- A crash keeps the node's cores and stored pages (production keeps the disk; racer-sim
  drops memory state and in-flight reads). A crashed node's own client reads keep running;
  only its peer server and its replies are gone. Clock faults (`ClockSkew`, `ClockRate`,
  `ClockJump`) are ignored: Model nodes share one clock.
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
| `model-partition-heals` | `reads_recover_after_partition_heals` (6) | at least one storm's reads complete (Model: also every read ends, links crossed the cut) |
| `model-hung-peers` | `hung_relays_are_routed_around`, `hung_peers_are_found_and_recovered_peers_are_used_again` (`GRID`, silence on; 5) | requests are lost to hung peers and found by silence; racer-sim's bounds are at most 2 failures in the first storm and 1 in the second, the Model checks at most 3 in total (its peers hang until 40 s rather than for good, so the run drains) |

| Scenario | Metric | racer-sim | Model |
|---|---|---|---|
| cold readers, 64 nodes | completed / failed | 1024 / 0 | 1024 / 0 |
| | origin requests, origin/page | 16, 1 | 16, 1 |
| | flight joins | 998 | 1008 |
| | latency ms | 58 / 117 / 131 | 67 / 109 / 120 |
| cold start, 400 nodes | completed / failed | 6400 / 0 | 6400 / 0 |
| | origin requests, origin/page | 16, 1 | 16, 1 |
| | route unreachable | 7 | 8 |
| | relays | 5012 | see Scale |
| | flight joins | 6434 | 10336 |
| | sheds / helper copies | 604 / 3821 | 681 / 3896 |
| | latency ms | 92 / 184 / 2097 | 111 / 2049 / 2097 |
| cold start, 2000 nodes | completed / failed | 31997 / 3 (`FlightFull`) | 31998 / 2 (`FlightFull`) |
| | origin requests, origin/page | 16, 1 | 17, 2 |
| | flight joins | 32322 | 60927 |
| | sheds / helper copies | 6053 / 26056 | 4549 / 25587 |
| | latency ms | 150 / 2147 / 2218 | 172 / 2154 / 2193 |
| hot-page storm, 512 nodes | completed / failed | 513 / 0 | 513 / 0 |
| | origin requests | 1 | 1 |
| | sheds / helper probes / helper copies | 446 / 498 / 315 | 444 / 454 / 308 |
| | flight joins | 616 | 606 |
| | latency ms | 1879 / 3221 / 3307 | 1399 / 3163 / 3225 |
| origin 403, 64 nodes | completed / failed | 1004 / 20 | 1007 / 17 |
| | origin requests, origin/page | 36, 6 | 33, 12 |
| | latency ms | 83 / 218 / 232 | 75 / 402 / 410 |
| | flights reelected | 21 (`flight_reelected`) | 19 |
| partition heals, 32 nodes | completed / failed | at least 512 (racer-sim's check) | 1008 / 16 |
| | link failures | not recorded | 191 |
| hung peers, 200 nodes (`GRID`) | failed | at most 3 (racer-sim's bounds) | 0 of 12800 |
| | requests lost to hangs / silence timeouts / pings | not recorded | 691 / 505 / 1452 (seed 1) |

Tolerances. The Model passes every racer-sim check above, with these documented
differences:

- Origin requests: at most racer-sim's (exact on cold readers, the hot page and cold start
  400), except cold start 2000: 17 against 16, within `REPLICAS` per page as racer-sim's
  check requires. Both reach origin once per page at 400 nodes (see Scale). Every row was
  re-measured after racer-sim adopted production's search slots and request verification.
- Completed and failed reads: equal, except cold start 2000 (2 against 3 `FlightFull`) and origin 403 (fewer `OriginForbidden`
  failures; the 403 draw order differs because the Model's origin draws come from the
  DST `Choices` stream).
- Latency: p50 within 1.5x of racer-sim; p99 within 1.1x except where a few reads wait out
  one 2 s attempt (the cold start 400 maximum). Treat Model tail latency as an upper bound,
  not a match.
- Sheds, flight joins and helper copies: within an order of magnitude, not closer. The
  Model counts a join for every client or serving read that finds a running flight on its
  worker, including retries; racer-sim's counter may count differently (inference, not
  yet verified). On cold start 400 most
  helper copies come from flight-full `Overloaded` answers rather than hot-page sheds.
- Origin/page under 403s: an interpreter draw-order difference, not a core decision
  difference. In both racer-sim and the Model every 403 fails exactly its leader and re-elects
  one waiter (`origin_forbidden_fails_only_the_leader` asserts one `OriginForbidden` failure
  per 403 and at least as many re-elections; a leader shed by every owner re-elects too, 2 of
  19 here), so origin requests are always pages + 403s
  (racer-sim 16 + 20 = 36, Model 16 + 17 = 33). The per-page maximum is the longest run of
  403 draws on one page; over Model seeds 1..20 it ranges 3..12 (mean 6), and seed 11 happens
  to be the 12 tail.

## Scale

Tolerance for the scale cold start (seed 8, every node reads one 16-page object in 10 ms):
Model failures within 2x of racer-sim's and the same order of magnitude, origin requests at
most racer-sim's.

Since the overflow helper spends its own grant (`core/src/read/walk.rs` `helper_grant`), both
fidelities changed, because racer-sim drives the same racer-core walk. Before and after, both
at seed 8 (racer-sim from `NodeWorld` with the `cold_start` test's config, release):

| Nodes | racer-sim before | racer-sim after | Model before | Model after |
|---|---|---|---|---|
| 400 | 0 failed, 30 origin | 0 failed, 30 origin | 0 failed, 36 origin | 0 failed, 36 origin, p50 104 ms / p99 167 ms |
| 2,000 | 0 failed, 43 origin | 0 failed, 43 origin | 2 failed, 43 origin | 2 failed (`Overloaded`), 43 origin, p50 128 ms / p99 2.12 s |
| 10,000 | 180 failed, 47 origin | 44 failed, 47 origin, 11,432 pings | 682 failed, 25 origin | 95 failed (16,029 relay sheds), 25 origin, 8,773 pings, p50 295 ms / p99 2.23 s, 91,004 B/node |
| 50,000 | 24,742 failed (3.1%), 47 origin | 1,094 failed (0.14%), 35 origin, 97,468 pings | 56,145 failed (7.0%), 21 origin | 30,697 failed (3.8%; 1,030,672 relay sheds), 21 origin, 35,011 pings, p50 0.96 s / p99 2.69 s, 92,488 B/node |

At 10,000 the Model now fails 2.2x racer-sim's reads (was 3.8x). At 50,000 the gap grew to
28x (was 2.3x): racer-sim gained more from the helper fix than the Model did. The cause was
racer-sim's divergence from production, found below (50,000-node gap).

Current parity, peer wire v2 costs and crypto queue 512 (seed 8, failed reads / origin
requests, relay sheds, latency p50 / p99; other seeds and depths under Crypto queue depth
below):

| Nodes | racer-sim | Model | relay sheds (racer-sim / Model) | latency p50 / p99 (racer-sim / Model) |
|---|---|---|---|---|
| 400 | 0 / 16 | 0 / 16 | 21 / 20 | 101 ms, 0.20 s / 114 ms, 0.24 s |
| 2,000 | 0 / 16 | 1 / 16 | 2,629 / 2,500 | 185 ms, 2.01 s / 197 ms, 2.13 s |
| 10,000 | 582 / 16 (0.36%) | 17 / 16 (0.01%) | 32,599 / 20,485 | 403 ms, 2.68 s / 416 ms, 2.48 s |
| 50,000 | 74,077 / 16 (9.3%) | 6,575 / 16 (0.82%) | 234,649 / 199,964 | 805 ms, 2.68 s / 757 ms, 2.84 s |

The Model now fails fewer reads than racer-sim at 10,000 and 50,000, by more than the 2x
tolerance (seed 3 at 50,000: 1.7% against 7.9%). Both fidelities share the crypto queue rule;
the gap is in how many requests each lets reach a hot owner at once, which production caps
(see Crypto queue depth below).

Earlier parity, before the v2 costs (seed 8, failed reads / origin requests, racer-sim release binary on
`tmp/parity/cold<N>.json`, Model `model_memory_per_node` with `MODEL_NODES`). racer-sim now has
production's search slots (no queue) and request verification (see Modeled):

| Nodes | racer-sim | Model | Model / racer-sim failures | relay sheds (racer-sim / Model) | latency p50 / p99 (racer-sim / Model) |
|---|---|---|---|---|---|
| 400 | 0 / 16 | 0 / 16 | equal | 12 / not recorded | 92 ms, 184 ms / 111 ms, 2.05 s |
| 2,000 | 3 / 16 (`FlightFull`) | 2 / 17 (`FlightFull`) | 0.7x | 787 / 863 | 150 ms, 2.15 s / 172 ms, 2.15 s |
| 10,000 | 204 / 16 | 118 / 16 | 0.6x | 25,023 / 15,768 | 268 ms, 2.15 s / 303 ms, 2.23 s |
| 50,000 | 56,073 / 16 (7.0%) | 36,933 / 16 (4.6%) | 0.7x | 994,407 / 1,094,694 | 1.07 s, 2.68 s / 1.12 s, 2.90 s |

Within tolerance at every size: failures within 2x and the same order of magnitude, origin
requests equal (cold start 2000: 17 against 16, within `REPLICAS` per page). The cold start's
own 1% failure check fails at 50,000 for both fidelities, so it is a production result (see
the open question below), not a Model artifact.

Since rank 0 answers `FlightFull` and the Model charges full first rankings (see below), seed
8, failed reads / origin requests. "racer-sim, no queue" sets `search_queue` to 0, as in
production (see Route search slots):

| Nodes | racer-sim | racer-sim, no queue | Model, `FlightFull` | Model, `FlightFull` and ranking CPU |
|---|---|---|---|---|
| 400 | 0 / 16 | not run | 0 / 16 | 0 / 16 (p50 111 ms, p99 2.05 s) |
| 2,000 | 2 / 16 (`FlightFull`) | not run | 3 / 16 | 2 / 17 (`FlightFull`) |
| 10,000 | 68 / 16 | 90 / 16 | 97 / 16 | 118 / 16 |
| 50,000 | 1,123 / 16 | 4,343 / 19 | 31,128 / 16 | 36,933 / 16 |

Within tolerance at 400, 2,000 and 10,000 (1.7x racer-sim, 1.3x racer-sim without the
queue). At 50,000 the Model fails 33x racer-sim and 8.5x racer-sim without the queue. Superseded
by Current parity above.

`FlightFull`. A flight at its waiter cap now answers `FlightFull`
(`core/src/read/flight.rs` `JoinReject::WaiterCap`, `join_copy_existing`; production
`src/read/flight.rs` `join_prepared`, through `peer_error` in `src/read.rs`), wire outcome
`flight-full` (`src/peer/protocol.rs`), HTTP 503 for clients (`src/client.rs`). The entry
cap, quotas and admission still answer `Overloaded`: none of them proves a fetch. The walk
takes a signed `FlightFull` as `Outcome::Fetching` (`core/src/read/walk.rs`): like a shed
(helper, busy evidence), and when it answers an owner's page probe of rank 0 it sets
`rank0_unanswered`, so the owner's walk ends `Exhausted` and its callers retry and join rank 0's
flight, as after a timeout of rank 0. Unlike a timeout this holds in every rank-0 watch state,
including `Overdue`: the watch tracks silence, and the answer disproves it; the flight ends
on its own deadline, which bounds the wait. Unsigned (relayed) `FlightFull` stays a relay
`Overloaded`. Cold start 400 went from 36 (Model) and 30 (racer-sim) origin requests to 16 in
both, and 2,000 from 43 to 16 or 17. The cost is tail latency at 400 nodes (Model p99 167 ms
to 2.05 s): a few readers whose owner gets `FlightFull` from rank 0 wait out an attempt and
retry instead of reading the owner's own origin copy.

Ranking CPU (production-faithful, adopted). Production ranks with `rank_async`, 256 members
per poll (`topology/src/placement.rs:35`, `:793-810`), in a placement cache per worker
(`src/app.rs:1343` `Placement::with_memory_budget`). racer-sim charges one `ranking_chunk_ns`
per poll (`sim/src/node/world.rs` `spawn`), 157 M polls at 50,000 nodes. The Model charged one
chunk per ranking; it now charges a worker's first ranking of a page in full
(`ranking_polls`). This was a lead for the 50,000-node gap and did not close it.

50,000-node gap (closed). The earlier comparison was against a racer-sim that diverged
from production in two ways, and the Model was the faithful side in both:

- Search queue. Production builds `Paths` with `with_limits` (`src/app.rs:1327`,
  `src/topology.rs:984-993`) and never calls `with_queue`, so `queue_limit` is 0
  (`topology/src/paths.rs:479`) and a search past the 8 slots fails `Overloaded`
  (`paths.rs:612-614`). racer-sim queued 64. It now defaults to 0. A requester's own shed
  search reached both fidelities' walks as a transport `Unavailable`, which counts against
  rank 0 and let owners above rank 0 overlap rank 0's origin fetch (racer-sim's
  `owners_above_rank_zero_do_not_overlap_a_slow_origin_fetch` failed once the queue was off).
  Production returns `Error::Overloaded` (`src/peer.rs:919-931` through `observer.result`),
  which `transport_outcome` (`core/src/read/errors.rs`) reads as `Overloaded`; both fidelities
  now do the same.
- Request verification (the root cause). Production verifies each request before it serves or
  relays it (`src/peer/server.rs:515-518`, `:792`), on the crypto helper paired with the
  connection's worker (`src/worker.rs` `WorkerPair`). A relay reserves its `Relay` transfer
  after its own verification (`server.rs:539-546`) and keeps it until the answer is written back
  (`:778`), so its hold includes the next holders' verification queues. The Model waits
  (`Model::hop`, `verified`). racer-sim charged `verify_ns` on one helper per node and dispatched
  at once (`hop`), so the destination answered, and the relays released their transfers, before
  the destination's verification queue drained. Relay holds on non-page answers
  (`relay_hold_answer_ns`, new) at 50,000: racer-sim p50 11.5 ms, p90 42 ms before; Model mostly
  64 to 256 ms (`relay_hold:answered:*`); racer-sim p50 75 ms, p90 252 ms after. With verification
  waited on one helper per node racer-sim failed 123,903 reads (1,438,866 relay sheds); with one
  helper per worker, as production and the Model, 56,073.

Relay sheds stay at the first relay in both: hot owners' verification queues hold every relay
toward them, and 16 `Relay` transfers per worker (`src/worker.rs:265`) run out. That queue was
unbounded in both fidelities, which production's is not (see Crypto queue bound in Modeled).

Crypto queue bound (production-faithful, adopted). Every number in the tables above queued
request verifications without bound. Production sheds a verification past 512 crypto jobs per
worker, and it used to answer that shed by closing the connection. Seed 8, cold start, failed
reads / origin requests (most per page), relay sheds, verification sheds, latency p50 / p99:

| Nodes | Fidelity | Before: bound, connection closed | After: bound, unsigned overload head, helper |
|---|---|---|---|
| 2,000 | racer-sim | 3 / 16 (1), 2,723, 0, 150 ms / 2.15 s | 0 / 16 (1), 2,601, 0, 134 ms / 2.15 s |
| 2,000 | Model | 2 / 17, 2,550, 0, 167 ms / 2.15 s | 2 / 16, 2,510, 0, 171 ms / 2.16 s |
| 10,000 | racer-sim | 470 / 31 (3), 59,045, 76,145, 251 ms / 2.15 s | 465 / 16 (1), 31,569, 54,297, 301 ms / 2.15 s |
| 10,000 | Model | 224 / 33, 37,479, 94,068, 257 ms / 2.20 s | 177 / 16, 20,813, 64,567, 315 ms / 2.22 s |
| 50,000 | racer-sim | 631,635 (79%) / 46 (3), 1,420,841, 3,737,271, 301 ms / 603 ms | 47,915 (6.0%) / 16 (1), 235,532, 1,073,883, 603 ms / 2.42 s |
| 50,000 | Model | 648,773 (81%) / 44, 1,546,084, 4,424,300, 354 ms / 639 ms | 18,615 (2.3%) / 16, 222,487, 962,087, 712 ms / 2.53 s |

Both columns hold relays until the answer is written back. "Before" is production before this
change: a 50,000-node cold start collapses, because a closed connection reads as a dead link
(signed `Unavailable` from a relay, a transport failure at a requester), owners above rank 0
reach origin, and readers retry the same hot owners. "After" is the fix:

- The holder answers a verification shed with the unsigned overload head
  (`src/peer/server.rs` `serve_connection`, `racer_core::route::unverified_failure`). It costs
  no key work and carries nothing from the request; it says only that the holder is busy, which
  closing the connection says as well. Every other verification failure still closes.
- The sender reads it as `Overloaded` (`src/peer/transport.rs`): a relay signs its own
  `Overloaded` (`route::relay_failure`), a requester sees a transport `Overloaded`.
- A reader's walk asks its overflow helper after any `Overloaded`, signed or not
  (`core/src/read/walk.rs` `complete`), not only after the destination's signed shed: the helper
  is the way around a busy hop as well as a busy owner. A forged unsigned overload can only send
  a reader to its helper, which dropping the connection could force as well.

Other candidates at 50,000 unless noted (seed 8, all with the bound 512, the unsigned head, the
helper change and relays held to the written answer unless noted):

| Candidate | Model failed reads | racer-sim failed reads |
|---|---|---|
| Unsigned head without the helper change (relays released early) | 716,558 | 714,562 (bound 128) |
| Shed a relay before verifying when its `Relay` quota is full (unbounded queue, no helper change, relays released early) | not run | 208 at 10,000 (204 without) |
| Owner sheds a hot page before verifying, unsigned (relays released early) | 18,831 (22,978 without), 11,257 unbounded (1,722 without) | not run |
| `Relay` transfers 32 per worker | 27,681 | 42,617 |
| Crypto bound 1,024 (relays released early) | 17,748 | not run |
| Crypto bound 2,048 | 6,212 (0.78%), p50 805 ms / p99 2.59 s | 18,120 (2.3%), p50 671 ms / p99 2.42 s |
| Unbounded verification queue (relays released early) | 1,722 | not run |

Pre-verification relay admission does not help: relay verifications wait about 40 us (racer-sim
`verify_wait_relay_ns` p90 at 10,000) against owners' tens of ms (`verify_wait_dest_ns`).
Shedding hot pages before verifying loses the signed shed's evidence and does worse. More relay
transfers move the queue to the owners. A deeper crypto queue is the remaining lever: at 2,048
jobs per worker the Model passes the 1% check and racer-sim fails 2.3%.

Open question (production). With the fix a 50,000-node cold start still fails 2.3% (Model) to
6.0% (racer-sim) of reads, and the cold start's 1% check fails in both. The crypto capacity is
`queue_entries` (`src/worker.rs:618` passes it to `security::try_pair`), which also sizes flight
operation queues (`src/read/flight.rs:675`), `ControlProgress`, the store queue and peer
admission (`src/app.rs:480`); raising it, or giving the crypto pair its own limit, changes those
too or touches `src/worker.rs` beyond one line, so it is not done here. Operators can raise it
with `RACER_QUEUE_ENTRIES`. racer-sim fails 2.6x the Model at 50,000 with the fix (outside the
2x tolerance); relay holds to the written answer moved racer-sim from 25,207 to 47,915 and the
Model from 22,978 to 18,615, so the gap is in how long relays hold, not verification. Answered with the v2 costs below.

Crypto queue depth with peer wire v2 costs (decided: keep 512). Every row below uses the v2
costs (see Peer crypto costs in Modeled). Cold start, failed reads (share of all reads),
origin requests / most per page (racer-sim only), client read latency, relay sheds,
verification sheds and peak RSS per node. racer-sim from its release binary with
`"node": {"crypto_queue": <depth>}`; the Model from `model_memory_per_node` with
`MODEL_CRYPTO_QUEUE` and `MODEL_SEED`:

| Nodes | Seed | Depth | Fidelity | failed (%) | origin / most per page | p50 / p99 | relay sheds | verify sheds | peak B/node |
|---|---|---|---|---|---|---|---|---|---|
| 10,000 | 3 | 512 | model | 55 (0.03%) | 16 / - | 462 ms / 2.76 s | 21,213 | 106,354 | 89,165 |
| 10,000 | 3 | 512 | sim | 37 (0.02%) | 16 / 1 | 403 ms / 2.42 s | 31,081 | 78,006 | 144,877 |
| 10,000 | 3 | 1024 | model | 26 (0.02%) | 16 / - | 544 ms / 2.87 s | 23,422 | 31,516 | 88,892 |
| 10,000 | 3 | 1024 | sim | 25 (0.02%) | 16 / 1 | 503 ms / 2.42 s | 33,517 | 24,865 | 144,425 |
| 10,000 | 3 | 2048 | model | 26 (0.02%) | 16 / - | 610 ms / 2.76 s | 23,398 | 0 | 88,436 |
| 10,000 | 3 | 2048 | sim | 26 (0.02%) | 16 / 1 | 537 ms / 2.15 s | 33,443 | 0 | 130,276 |
| 10,000 | 8 | 64 | model | 154 (0.10%) | 16 / - | 357 ms / 2.43 s | 12,025 | 208,228 | 90,279 |
| 10,000 | 8 | 64 | sim | 558 (0.35%) | 16 / 1 | 336 ms / 2.15 s | 20,052 | 209,130 | 145,537 |
| 10,000 | 8 | 512 | model | 17 (0.01%) | 16 / - | 416 ms / 2.48 s | 20,485 | 85,816 | 89,137 |
| 10,000 | 8 | 512 | sim | 582 (0.36%) | 16 / 1 | 403 ms / 2.68 s | 32,599 | 95,026 | 144,830 |
| 10,000 | 8 | 1024 | model | 16 (0.01%) | 16 / - | 503 ms / 2.49 s | 23,368 | 30,573 | 88,659 |
| 10,000 | 8 | 1024 | sim | 37 (0.02%) | 16 / 1 | 503 ms / 2.42 s | 33,491 | 24,967 | 144,527 |
| 10,000 | 8 | 2048 | model | 16 (0.01%) | 16 / - | 560 ms / 2.36 s | 23,391 | 0 | 88,073 |
| 10,000 | 8 | 2048 | sim | 23 (0.01%) | 16 / 1 | 537 ms / 2.68 s | 33,498 | 0 | 144,012 |
| 50,000 | 3 | 512 | model | 13,357 (1.67%) | 16 / - | 946 ms / 2.93 s | 202,242 | 925,853 | 91,069 |
| 50,000 | 3 | 512 | sim | 63,354 (7.92%) | 16 / 1 | 872 ms / 2.95 s | 236,859 | 1,348,636 | 137,659 |
| 50,000 | 3 | 1024 | model | 5,569 (0.70%) | 16 / - | 1054 ms / 3.14 s | 221,589 | 657,770 | 91,203 |
| 50,000 | 3 | 1024 | sim | 78,053 (9.76%) | 16 / 1 | 940 ms / 3.22 s | 279,221 | 1,175,956 | 137,288 |
| 50,000 | 3 | 2048 | model | 13,975 (1.75%) | 16 / - | 1481 ms / 4.54 s | 290,987 | 625,946 | 90,983 |
| 50,000 | 3 | 2048 | sim | 34,132 (4.27%) | 16 / 1 | 1208 ms / 4.29 s | 337,006 | 609,865 | 136,867 |
| 50,000 | 8 | 64 | model | 48,394 (6.05%) | 16 / - | 622 ms / 2.59 s | 171,961 | 1,824,671 | 104,636 |
| 50,000 | 8 | 64 | sim | 100,763 (12.60%) | 16 / 1 | 604 ms / 2.42 s | 189,300 | 2,257,055 | 166,936 |
| 50,000 | 8 | 512 | model | 6,575 (0.82%) | 16 / - | 757 ms / 2.84 s | 199,964 | 801,822 | 90,116 |
| 50,000 | 8 | 512 | sim | 74,077 (9.26%) | 16 / 1 | 805 ms / 2.68 s | 234,649 | 1,401,652 | 137,732 |
| 50,000 | 8 | 1024 | model | 12,625 (1.58%) | 16 / - | 1013 ms / 3.29 s | 223,533 | 715,771 | 90,855 |
| 50,000 | 8 | 1024 | sim | 56,027 (7.00%) | 16 / 1 | 1007 ms / 3.22 s | 266,702 | 982,175 | 137,374 |
| 50,000 | 8 | 2048 | model | 10,366 (1.30%) | 16 / - | 1424 ms / 4.46 s | 298,377 | 534,842 | 90,707 |
| 50,000 | 8 | 2048 | sim | 34,374 (4.30%) | 16 / 1 | 1342 ms / 4.29 s | 319,325 | 561,505 | 137,534 |

- No depth brings both fidelities under 1% at 50,000. The Model stays between 0.7% and 1.8%
  and is not monotonic in depth; racer-sim fails 7.0 to 9.8% at 512
  and 1,024 and 4.3% at 2,048. A deeper queue raises p50 from about 0.8 s to 1.4 s and p99 from 2.7 s to 4.5 s.
  At 10,000 every depth from 512 up fails at most 0.36%.
- Memory does not depend on the depth. Each byte a queued verification pins is charged to
  `RequestContext`: the receive buffer and decoded head (`http/src/connection.rs:444`, `:710`
  through `HttpContext::charge`, `src/http.rs:79-83`) and `3 * head_bytes` held through
  verification (`src/peer/server.rs:461-468`). That class is 32 MiB per worker
  (`core/src/admission.rs` `request_context_bytes`), so a full queue pins at most 32 MiB per
  worker and `32 MiB * workers` per node at 512, 1,024 or 2,048. A v2 head costs about 13 KB
  this way (3,208 B relayed: buffer plus 3 copies), a head at `MAX_ENVELOPE_HEAD` (629,452 B)
  up to about 3.1 MB, so about 10 of those fill the budget. Past it the reservation fails
  `Overloaded` and the connection closes (`server.rs:468`). Without that cap, 2,048 such heads
  would pin about 6.4 GB per worker.
- Production cannot fill even 512. Inbound peer connections are capped at `IngressConnection`,
  45 per worker by default (`core/src/admission.rs` `Rules::limit`, `client_connections` 64
  minus 3 control and 16 outbound), by the listener (`src/peer/server.rs:252-255`, `:295`) or
  the ingress handoff (`src/admission.rs` `reserve_ingress`). A connection serves one exchange
  at a time (`server.rs:315-322`). So a worker holds at most 45 inbound and 16 outbound peer
  crypto jobs (`Rules::peer_crypto_jobs`, 61), well under `Limits::crypto_submissions` (512);
  the test `crypto_queue_holds_every_peer_exchange_of_a_worker` pins this. Raising the depth
  would change nothing in production and would also raise flight queues, `ControlProgress`,
  pipe waiters and the store queue, which share `queue_entries`.
- Remaining failure cause (a fidelity gap, not fixed here). Neither fidelity models the ingress
  cap: both let any number of requests reach a hot owner's worker at once and shed them only at
  the crypto queue (racer-sim `hop`, Model `Model::hop`). The 1.4 M (racer-sim) and 0.8 M
  (Model) verification sheds at 50,000 mean hundreds of requests at once on hot owners'
  workers, which production would hold at 45 connections. Past that production does not
  accept (`server.rs:295-297`) and the sender waits for its attempt to time out, not for an
  unsigned overload. The depth-64 rows, closest to that cap, fail 6.1% (Model) and 12.6%
  (racer-sim). Production at 50,000 may therefore fail more than either fidelity reports. The
  next step is to model the ingress cap and its accept wait in both fidelities.
- Signing placement (changed). Production signs requests and responses inline on the I/O
  worker (`src/peer.rs` `sign_request_to`, `append_request`; `src/peer/server.rs`
  `sign_response` calls); only verification goes to the crypto helper (`src/peer/forwarding.rs`
  `verify_request_async`). Both fidelities now charge signing to the worker CPU (Model
  `link`/`sign`, racer-sim `link`/`send_reply`, through `Fabric::cpu_from`, which charges a
  worker from now rather than booking it ahead of input that is ready later).
  The Model's 8.8% failures with worker signing had two causes. (1) A Model bug: the
  destination booked its worker CPU at the future end of verification (`fabric.cpu(ready, ..)`
  in `hop`), and `reserve` keeps one free time per server, so the worker sat idle until then
  and every signature or search queued behind it. Fixed: the worker takes the request on
  `Ev::Verified` (test `a_request_waiting_for_verification_leaves_its_worker_free`).
  (2) Requesters' route search sheds: 8 search slots per worker and no queue (production
  `Paths::with_limits`, `src/app.rs`); a 32-page burst per worker holds the slots across the
  signatures queued on the same worker. The Model hid these: `search_sheds` counted only
  sheds after a search's first poll, but almost all happen on the first (about 15,600 per
  `model-hung-peers` run, now counted). `model-hung-peers` now fails 8 to 30 of 12,800 reads
  over seeds 1 to 8; its bound is 64. Proposed production fix: give `Paths` a small queue
  (`Paths::with_queue`) so a burst waits for a slot rather than failing.

Earlier leads, kept for the record: read attempts failed at similar rates but the Model's
failures clustered (four attempts within a second); per-node route caches, the destination not
waiting for verification (in the Model only, with one queue still coupling verify and sign),
and the ranking cost did not close the gap.

Crypto helpers (production-faithful, adopted). Production pairs each worker with its own
crypto helper (`src/worker.rs:124-130` `WorkerPair { io, crypto }`); racer-sim had one per
node and now has one per worker too. The Model now has one per worker (`resources.rs` `Fabric::crypto`), and a connection's
worker is `probe % workers` (racer-sim `hop`; an inference about production's listener). The
Model and production also wait for request verification before dispatch
(`src/peer/forwarding.rs:368` `verify_request_async`); racer-sim charged `verify_ns` and went
on until it adopted the wait (see 50,000-node gap).

Fixed: helper detours starved the remaining owners. Before the fix, the extra 10,000-node
failures were walks `OOver HUnav OFail OFail` (`MODEL_DIAG=1`, `walk:*`). Owner 0 shed the
reader (`Outcome::Shed`). The helper was funded from the walk's own 8-attempt, 16-link page
budget (`min(remaining, HELPER_ATTEMPTS)` attempts and `min(remaining, HELPER_LINKS)` links).
It then failed at once with `Unavailable` and came back empty. Owners 1 and 2 had no credits
left, so they failed to plan with `Unavailable` (0 attempts) or `HopBudgetExhausted` (0 links).
At 2,000 nodes there were 1,647 such walks and 4,879 unsent owner probes. After the fix there
are none and 504 unsent probes. The helper now spends its own grant, which is dropped after
the probe, so a walk asks at most one helper and the walk's budget never grows.

Pings. Production records a probe's silence evidence through `CandidatePolicy::apply`
(`src/read/candidates.rs:1546-1580`) with the helper as the destination
(`ProbeDest::Helper`, `:1486`, `:1563`), so a helper probe's timeout counts against the helper
(`needed` = `NO_PING`, `core/src/read/walk.rs:2184-2195`) and a later last-link timeout of the
same peer reaches `LINK_TIMEOUTS` (2, `src/peer.rs:1146`) and pings. `WorkerCore` (used only by
the Model) dropped that evidence because the acquisition asks for a helper without a
destination; it now names the resolved helper (`core/src/read/worker.rs` `feed`). At 10,000:
2,082 helper probe timeouts all yield silence evidence, 1,651 pings (racer-sim 3,986).

Cold start 400 origin requests (36 against 30, before `FlightFull`). Ranks 1 and 2 went to
origin after rank 0 answered their delegated `Acquire` with its flight's waiter cap, a signed
`Overloaded` the walk took as a shed. Fixed by `FlightFull` (above).

Slow origin (port in progress, `scenarios::slow_origin`, not in the catalog). racer-sim's
`relay_shed_rank0_probes_do_not_overlap_a_slow_origin_fetch` (seed 3) asserts one origin
request per page. The Model (seed 3) asks origin 144 times for 64 pages (max 3 per page) and
fails 141 of 3,072 reads. `MODEL_DIAG=1`: rank 0 fetches all 64 pages; ranks 1 and 2 fetch 80
times after rank 0 answered their probe `Miss` (`origin:rankSome(1):OMiss`). Those probes are
copy-only (`owner_probe:copy_onlytrue:attempts0`, 101 of 262 owner-to-owner probes): the owner
serving a reader's request did not have the attempts to delegate (`walk.rs:657-661` needs
`left + 2`), so it probed rank 0 copy-only (`walk.rs:674-677`), got a miss while rank 0 was still
fetching, and ended `End::Origin`. Open question for the next session: whether racer-sim funds
owner serving reads differently (reader `owner_funding`, retries' budgets) or this is a Model
divergence; compare the inherited `route.attempts` at owners with racer-sim's `serve`.

Earlier leads:

- Shared route cache (refuted). A cache entry's routes depend only on its `PathKey`
  (`topology/src/paths.rs:165-180`), so the node that searched does not change which relays
  carry a request. Per-node caches of production's 128 entries (`src/worker.rs:262`) gave 130
  failures at 10,000 (against 134) and 18 KB more per node, so the Model keeps one cache.
- Search latency (refuted for now). No Model search found its worker's 8 slots busy at 10,000
  or 50,000. racer-sim's queued searches come from its 64-entry queue, which production does
  not have (see Route search slots); without it racer-sim fails 3.9x more reads at 50,000.
- Relay hold (matches production): `src/peer/server.rs:540-560` reserves the `Relay` transfer
  and awaits `forward_inner`; `:778` clears it after the relayed body is written.
- Relay hold breakdown (`MODEL_DIAG=1`, `relay_hold:*`, `relay_shed:*`): every shed is at the
  first relay (`hop1`) of a probe, never a ping.

What closed the 10,000-node gap earlier: mesh relays (494 to 79 failures), signature costs
(production `verify_request_async`, `sign_response`), visit order, production flight limits
and silence demotion.

Flight failure semantics (checked, no change): when a leader's walk fails, production
fails every waiter with the leader's error unless it is an origin rejection or a
handed-over deadline/cancellation, which re-elect (`core/src/read/flight.rs` `fail`).

Not ported yet: `slow_disk_sheds_persistence...` (persistence shedding),
`relays_hold_their_reservation_only_while_the_body_streams` and `relay_shed_rank0_probes...`
(the quota now exists; the scenarios are not ported), the full-mesh and watch-off variants
of the hung-peer test, quota tests in `contention_scenarios.rs`, `sim/tests/mesh_scenarios.rs`
(topology-only mesh metrics) and `sim/tests/phantom_memory.rs` (racer-sim memory accounting).

## Model-only scenarios

| Scenario | What it covers |
|---|---|
| `model-range-readers` | range reads through `ReadWindow` with credits, spill holds, selections, worker fetches; a second pass reads every page from disk |
| `model-metadata-readers` | page reads behind `MetadataCohort` refreshes with 10% origin 5xx; refreshes coalesce per cohort |
| `model-faults` | origin 403/5xx mix, four cache outages, page and range readers; failed windows discard buffered pages |
| `model-membership-churn` | port of racer-sim's `churn` scenario (`sim/src/lib.rs` `scenario_schedules`: a tenth of the nodes leave and rejoin a quarter of the run later) on 200 nodes reading a new object every 250 ms; see below |
| `model-node-faults` | 64 nodes read twelve objects, one a second, under a drawn schedule of up to 16 crash/restarts, node-pair partitions and slow peers; every read ends and failures are only `Unavailable`/`Overloaded` (seeds 1..6: none fail) |

### Membership churn

racer-sim `--scenario churn --nodes 200 --seed 1` (2 s, its Poisson workload) against the
Model's `model-membership-churn` seed 1 (200 nodes, 8 storms of one 16-page object, 2 s of
churn). The workloads differ, so only the membership counters compare in kind:

| Metric | racer-sim | Model |
|---|---|---|
| reads completed / failed | 11,801 / 12 (`Unavailable`) | 24,960 / 0 |
| reads skipped on non-members | 166 | 640 |
| membership changes / publications | 37 / 8 | 40 / 9 |
| `StaleMembership` replies | 516 | 1,472 |
| stale waits (`Sleep`) | 7 | 11 |
| links refunded | 2,833 | 8,066 |

The Model's CI churn seeds exercise `Acquisition::Refresh`, `Acquisition::Sleep` and
`Acquisition::Batch` (handoff targets under the previous membership).

## Memory

`model_memory_per_node` in `tests/model_parity.rs` (ignored; run alone in release with
`MODEL_NODES=<n>`) reports the peak RSS of a Model cold start divided by the node count.
The racer-sim baseline is 103,189 B/node for `full-cold-start` at 50,000 nodes (peak RSS).

| Nodes | Peak RSS | Per node | Wall (release) | Notes |
|---|---|---|---|---|
| 2,000 | 154,730,000 B | 77,365 B | under 1 s | measured before signature costs |
| 10,000 | 889,640,000 B | 88,964 B | 13 s | with relays, 0.05% fail (see Scale) |
| 50,000 | 4,280,800,000 B | 85,616 B | 100 s | with relays, 0.15% fail (see Scale) |
| 10,000 | 930,942,976 B | 93,094 B | 22 s | per-worker crypto helpers, relay quotas, routed pings |
| 50,000 | 4,673,978,368 B | 93,479 B | 201 s | per-worker crypto helpers, relay quotas, routed pings |

At 50,000 nodes the Model uses about 83% of racer-sim's per-node peak (85,616 B against
103,189 B) and runs in minutes where racer-sim's `full-cold-start` takes 39 min. The
workloads differ: racer-sim's `full-cold-start` runs the full control plane, and the Model
runs the contention cold start (every node reads one 16-page object).

## Control plane

`model/control.rs` is racer-sim's `ControlSim` (`sim/src/control.rs`) moved onto the dst
engine, with the primary pass replaced: every decision is a real `racer-cluster-control`
rule. Each Pod runs the seat lottery and renewal (`rules::seat::{Lottery, SeatRenew}`),
primary contention (`rules::primary::Contend`, `rules::election`), and a seat follower that
installs records through `rules::seat::{admit, superseded, need_image}`. Each Pod's client
enrolls and long-polls through seat admission (`rules::admission::{start_poll, WriteQueue,
body_for}`) and bearer authorization (`rules::authz::{AuthCache, ApiBudget}`); all
authorization API calls share one Kubernetes API token bucket. The primary pass is
`rules::primary::plan_pass` and `discovery`, driven by the real `rules::shell::Shell` as
`primary.rs` `reconcile` drives it (cluster-control/src/primary.rs:545-723):

- `Shell::begin` asks for a state read. The read takes one API round trip
  (`api_latency_ns`); the shell checks it against the process high water (rollback or wipe
  fails closed) and the term fence (`Writer::observe`, with the live Lease UID only when
  `Shell::needs_live_uid`).
- The plan is fed back as `Event::Planned`; a changed record must be monotonic over the read
  and the Lease fresh, else the pass aborts.
- Right before the PUT, `Shell::recheck` refuses a write once the Lease went stale
  (primary.rs:682-684). The PUT is a compare-and-swap on the resourceVersion the read
  returned and takes another round trip; a write in between answers `Conflict` and the shell
  re-reads and re-plans, at most `MAX_WRITE_ATTEMPTS` reads per pass.
- `Settle` installs the record into the primary's own follower (fails closed on rollback),
  installs the plan's image, and publishes discovery on the primary Lease.
- An `Abort` with `StateError::fails_closed` ends the term and drops the served state
  (`seat::fail_closed`, cluster-control/src/seat.rs:695), as `lead` does
  (primary.rs:195-208). The primary Lease renewal feeds `Shell::renewed`, and
  `ctl_single_primary` counts Pods whose shell is `fresh`.

Divergences from racer-sim, where production decides (racer-sim is wrong in each):

| racer-sim (`sim/src/control.rs` `reconcile`) | Model and production |
|---|---|
| Reads and the PUT are atomic in one event: no write can land between them | One API round trip each; a concurrent write is a CAS `Conflict` and the pass re-reads (shell.rs:262-266) |
| No freshness recheck before the PUT | `Shell::recheck` refuses a stale term (shell.rs:198-204, primary.rs:684) |
| A non-monotonic plan is skipped silently and the term keeps serving | `Abort(Rollback)`, fail closed (shell.rs:250-254, primary.rs:195-208) |
| `ctl_single_primary` counts the seat Lease freshness of primaries | Counts primaries whose shell is fresh (Lease freshness from the last renewal start, state.rs:289) |
| A seat that becomes primary plans from the old primary's global image | Plans from the image its follower copied (seat.rs:824), so its first commit is a delta when it held the previous image |

New counters: `ctl_passes`, `ctl_put_conflicts`, `ctl_put_refused_stale`, `ctl_pass_aborts`,
`ctl_fail_closed`. Unit tests in `model/control.rs` cover a CAS conflict that re-reads and
commits, a stale Lease that refuses the PUT and fails closed, and a rolled-back record that
fails closed; racer-sim's 21 control tests run unchanged against the Model.

Not modeled (as racer-sim): webhook trust and endpoints, the CA keyring rotation schedule
(`rules::keyring::Policy` defaults only), DaemonSet deletion, and the ready floor
(`discovery` gets no previous sequence, primary.rs:744).

### Combined scenarios

`model/combined.rs` co-steps `ControlSim` with the read-path Model (racer-sim
`combined.rs`): control events at or before a read-path event run first; committed
memberships become `Versions::external` versions, each node adopts exactly the version its
client applied (none: its reads count as `reads_skipped_not_enrolled` and it is not a member
of anything it can resolve), and Pod crashes, restarts and removals crash or restart the
node. The read load is racer-sim's: Poisson arrivals (`Workload::rate`), Zipf objects (4096,
s = 1), uniform first page, 10% 8-page ranges. Data-plane faults are racer-sim's
`DataFaults::plan` defaults: slow nodes (every 100th, 8x), origin error bursts
(`OriginBurst`), a cut between sites 0 and 1 (`SiteCut`), and in `full-cold-start` a storm
on object 0 by every node. The report adds racer-sim's `combined` object
(`hard_failures`, `control_invariants`, `control_metrics`, `control`,
`membership_skew_at_end`) plus `control_memory` and `versions_retained`.

Differences from racer-sim's combined mode: range reads are issued as concurrent page
reads, not through a `ReadWindow`; no read starts in the last `2 * read_timeout + 10 s` so
every read ends (the Model checks that; racer-sim does not); the data-plane report is the
Model's (`read_latency_ns`, Model counters), not racer-sim's node report.

### Port status

| racer-sim scenario | Measures | Invariants | racer-dst | Status |
|---|---|---|---|---|
| `enrollment-storm` | every client enrolls through seat admission and authorization within the spread; API QPS peak, 429s, enroll time, client lag | single primary, terms monotonic, sequence monotonic, client membership monotonic, client converges | `model-ctl-enrollment-storm-200` (CI), `scenarios::control(plan, n)` | ported |
| `failover` | primary crash, two seat crashes, primary crash without restart: failover time, new terms, clients re-home | as above | `model-ctl-failover-200` | ported |
| `ctl-churn` | 1/16 of Pods at start, 8 growth waves, 2 shrinks: membership versions, delta and full replies | as above | `model-ctl-churn-320` | ported |
| `lagging` | 10% of clients partitioned, 5% slow, a shrink and a seat crash while they lag; catch-up lag | as above | `model-ctl-lagging-200` | ported |
| `full-cold-start` | enrollment storm plus reads; reads skipped before enrollment; storm on object 0 after enrollment | control invariants plus read-path hard failures | `model-full-cold-start-200` | ported |
| `full-failover` | failover plan plus reads, site cut, origin 5xx bursts | as above | `model-full-failover-200` | ported |
| `full-churn` | ctl-churn plus reads and an origin 412 burst | as above | `model-full-churn-320` | ported |
| `full-lagging` | lagging plus reads and slow nodes | as above | `model-full-lagging-300` | ported |

Full fidelity counterparts: the `authority-*` scenarios (`authority.rs`) run three real
authorities; enrolling clients against them are not modeled yet (see Gaps).

### Parity

racer-sim release binary against `cargo test --release -p racer-dst --test control`
(`control_report`, `combined_report`, ignored) at the same seed and size. Control-only
scenarios at racer-sim defaults (seed 1, and seed 2 where noted):

| Scenario | Metric | racer-sim | Model |
|---|---|---|---|
| enrollment-storm 200 | enrolled, all enrolled at | 200, 4.98 s | 200, 5.00 s |
| | client replies (full) | 471 (271) | 489 (289) |
| | enroll p50/p99 ms | 2684 / 4832 | 2684 / 4832 |
| failover 200 | enrolled, terms, failovers | 199, 2 failovers | 199, 2 failovers |
| | failover p50/max s (seed 1) | 26.5 / 91.5 | 24.3 / 96.7 |
| | failover p50/max s (seed 2) | 23.0 / 69.3 | 27.8 / 90.1 |
| | full / delta replies | 243 / 399 | 242 / 399 |
| ctl-churn 320 | enrolled, membership version | 304, 21 | 304, 21 |
| | client replies | 3745 | 3743 |
| lagging 200 | enrolled, unreachable polls (seed 1, 2) | 198, 190, 104 | 198, 142, 184 |
| | client lag p99 ms | 77,309 | 77,309 |

| enrollment-storm 10,000 | all enrolled at, enroll p50/p99 s | 44.4, 14.0 / 30.1 | 32.4, 15.0 / 30.1 |
| | API requests, 429s, replies | 30,449, 371, 41,571 | 30,374, 286, 41,481 |
| failover 10,000 | failover p50/max s | 27.9 / 27.9 | 28.6 / 33.5 |
| | API requests, 429s, replies | 30,433, 326, 62,390 | 30,458, 348, 62,370 |
| ctl-churn 10,000 | enrolled, membership version, replies | 9,500, 21, 116,007 | 9,500, 21, 116,102 |
| lagging 10,000 | unreachable polls, client lag p99/max s | 8,136, 8.6 / 79.0 | 7,644, 8.6 / 79.0 |
| | wall (release), peak RSS | 22.1 s, 163 MB | 11.7 s, 172 MB |

Every control run has zero hard failures in both. Combined scenarios at racer-sim's test
sizes (seed 1):

| Scenario | Metric | racer-sim | Model |
|---|---|---|---|
| full-cold-start 200, 120 s, 0.05/s | reads completed / failed | 4823 / 0 | 4450 / 0 |
| | skipped not enrolled / not member | 35 / 113 | 34 / 104 |
| | origin requests, origin/page | 1123, 1 | 918, 2 |
| full-failover 200, 300 s, 0.2/s | reads completed / failed | 17123 / 10 | 15043 / 91 |
| | skipped not enrolled / not member | 226 / 474 | 236 / 487 |
| | origin requests, origin/page | 9149, 6 | 7923, 7 |
| full-churn 320, 400 s, 0.1/s | reads completed / failed | 13969 / 0 | 12802 / 2 |
| | skipped not enrolled | 3046 | 3001 |
| | origin requests | 6627 | 6643 |
| full-lagging 300, 600 s, 0.2/s | reads completed / failed | 52651 / 4 | 49670 / 0 |
| | origin requests | 21055 | 16614 |

| full-cold-start 10,000, 400 s, 0.02/s | reads completed / failed | 275,251 / 0 | 266,048 / 0 |
| | skipped not enrolled / not member | 3,051 / 1,164 | 4,568 / 1,718 |
| | origin requests, origin/page | 27,623, 2 | 26,899, 4 |
| | read latency p50/p99/max ms | 42 / 168 / 3,472 | 47 / 132 / 3,496 |
| | wall (release), peak RSS | 155 s, 1.02 GB | 142 s, 0.66 GB |
| full-failover 10,000, 900 s, 0.02/s | reads completed / failed | 268,752 / 187 | 258,424 / 1,462 |
| | origin requests, origin/page | 44,199, 6 | 44,715, 8 |
| | read latency p50/p99/max ms | 42 / 218 / 3,551 | 47 / 157 / 3,473 |
| | wall (release), peak RSS | 316 s, 1.15 GB | 268 s, 0.64 GB |

Membership skew at the end, enrolled counts and membership versions match exactly in every
combined run. Reads completed are 3-12% lower in the Model because its read load stops
before the end (see above). Within tolerance except `full-failover` read failures (91 against 10, all `Unavailable`).
Ablation (`CTL_NO_CUT=1`, `CTL_NO_BURSTS=1` on `combined_report`): without the site cut the
Model fails 5 reads, without the origin bursts 90, so the excess comes from the cut between
sites 0 and 1. Both harnesses fail a hop that crosses the cut (racer-sim
`sim/src/node/world.rs:3464-3472`, Model `Model::cut`); racer-sim also opens link circuits
after failures (`world.rs:3458-3462`), which the Model does not model (see Not modeled yet).
That circuits steer racer-sim's later requests around the cut is an inference, not traced.
Open; see Gaps.

### Gaps blocking racer-sim deletion

- `full-failover` read failures differ by 9x at 200 nodes and 8x at 10,000, from the site
  cut (above); needs link circuits in the Model or a traced explanation.
- No Full counterpart with enrolling clients against real authorities.
- The Model has no racer-sim node report fields (`peer_requests_*`, `fills_failed`,
  `origin_repeats_*` classification used by racer-sim's `failover_repeats_are_skips_or_their_orphans`),
  so those racer-sim combined tests have no Model counterpart.
- `--config` overrides (`"control"`, `"combined_faults"`) are not parsed; scenarios take
  `ControlSpec::overrides` and spec fields instead.
