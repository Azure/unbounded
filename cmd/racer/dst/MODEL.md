# Model fidelity

`Fidelity::Model` runs `racer_dst::model`: a byte-free interpreter over the real
`racer_core::read::worker::WorkerCore` machines. It is dispatch-only: every decision (where
to probe, when to reach origin, whom to shed, which flight to join) is made by the cores.
The interpreter completes each effect on the FIFO cost model (`resources`), priced from
`costs.toml`, and answers it with facts of the world, such as which node holds which page.

Model scenarios share the choice stream, the event engine, the cost table and the
determinism double-run with Full fidelity. They carry a `model::ModelSpec` (workload, faults,
limits, report checks) instead of a node factory.

## What is modeled

- **Nodes and workers.** N nodes with W real `WorkerCore`s each. A page's flight lives on the
  worker `owner_worker` maps it to; client page reads land on that worker. A client range read
  runs its `ReadWindow` on worker `node % W`, so pages owned by other workers cross as
  `WorkerMsg::Fetch`/`Fetched`.
- **Client.** A recoverable failure is retried up to 4 attempts with a linear backoff; read
  latency runs from the first attempt.
- **Range reads** (`Storm::range`): windows with client credits, probes of the node store,
  spill holds and fills, and buffered-page discards. A provider selection is answered as not
  started, so its pages are filled directly.
- **Metadata** (`ModelSpec::metadata`): each page read first refreshes its object through the
  worker's `MetadataCohort`; the refresh is priced as an origin HEAD and draws the origin error
  mix.
- **Origin errors** (`ModelSpec::origin_errors`): 412 maps to `VersionUnavailable`, 403 to
  `OriginForbidden`, 5xx to `Unavailable`.
- **Placement.** A real `topology::Placement` over shared `phantom::Phantom` rankings and real
  `topology::Membership` versions. Ranking CPU: a worker's first ranking of a page under a
  membership costs one `ranking_chunk_ns` per 256 members, later ones one chunk.
- **Membership versions** (`model::membership`): `ModelSpec::churn` and `random_churn` make
  nodes leave and rejoin. Changes publish in 250 ms batches, each node adopts a version up to
  `membership_lag` late and still resolves the 2 versions before it. A holder that does not
  resolve a request's version answers `StaleMembership`, and the requester gets its unused
  credits and links back. A node outside the membership it adopted takes no client reads
  (`reads_skipped_not_member`).
- **Flights and acquisitions.** Joins, leader election, handover and settlement run in the
  core's `FlightTable`; ranking, peer probes, validation, origin fetches and publication run in
  the core's `PageAcquisition`. Flight tables use production admission limits (64 flights of 64
  waiters per worker): past the entry cap a join is `Overloaded`, past a flight's waiter cap
  `FlightFull`.
- **Serve side.** `WorkerCore::serve`: dispatch, hot-page shedding over live body counters,
  copy-only answers and copy-only joins of a running flight. A body counts toward the page's
  hot-page fan-in from admission until its last byte lands. Shed requesters go to overflow
  helpers resolved by `Pool::helper` on the real mesh.
- **Visit order and silence.** A non-owner's ranking uses production `visit_order` and
  `demote_unanswered` over a node-wide `liveness::Silence`, fed by the core's `Effect::Silence`
  and by last-link timeouts, and cleared by silence pings. A ping is a peer request routed over
  the mesh with no remote attempts.
- **Mesh relays** (`Model::forward`, `hop`, `link`, `fail_hop`): every probe is routed by the
  real `topology::Paths` with the core's route budget. Each holder skips silent neighbors and
  its detours, signs, and the next holder verifies before it relays or serves. Replies and
  bodies travel the route back.
- **Relay transfers.** A relay reserves one `Relay` transfer on its worker before it forwards
  (16 per worker); none free fails the hop `Overloaded` (`relay_sheds`). The reservation is
  held until the answer, or for a page the body, has passed.
- **Route search slots.** 8 searches run at once per worker; a search past that fails
  `Overloaded` (`search_sheds`). A requester's own shed search is no evidence against the
  destination.
- **Peer crypto.** Every holder verifies a request on the crypto helper of the connection's
  worker before dispatching or relaying. Signing a request with n proofs costs
  `sign_ns + (n - 1) * sign_hop_ns`, verifying it `verify_ns + (n - 1) * verify_hop_ns`,
  signing a response `sign_response_ns` (`costs.toml` `[crypto]`). A worker's crypto queue
  holds at most `crypto_queue` jobs (512 by default); a verification past it fails `Overloaded`
  before any key work and the server answers the unsigned overload head (`verify_sheds`).
- **Link circuits** (production `LinkHealth`, one per worker): an open circuit fails a send
  `Unavailable` at once; a failed connect backs the circuit off with `route::link_backoff`, and
  route searches skip blocked neighbors. `ModelSpec::circuits` turns them off for ablation.
- **Peer connection slots** (`ModelSpec::connections`, the shared `core::connections` table):
  ingress slots per worker shared node-wide, with the unsigned overload head when every worker
  is full; outbound pools with a per-worker and a per-next-hop cap, checked out without waiting.
  `ConnectionConfig::legacy` keeps an older, tighter configuration for comparison.
- **Faults.** From the scenario schedule and the drawn `fault_schedule`: `Crash`/`Restart` take
  a node's peer server down and drop its stored pages; `Partition`/`Heal` cut a node pair;
  `SlowPeer` scales the node's service times. Site cuts (`ModelSpec` partitions), hung peer
  servers (`ModelSpec::hangs`: accept and never answer) and cache outages
  (`ModelSpec::outages`: stored pages lost, probes answered `Miss`) are Model faults.
- **Costs.** Ranking and dispatch on per-worker CPU servers, page transfers on per-node NIC
  transmit and receive servers (over the route, chunked), payload open on the crypto server,
  publication on the disk write server, origin fetches on the shared origin model (per-cache
  concurrency, shared bandwidth, latency), RTT from the per-site matrix.

## Invariants and checks

The interpreter counts these as `hard_failures`, and any hard failure fails the run:

- every effect id is answered at most once;
- every client read, serving read, body, range read and metadata refresh ends;
- every core is quiescent at the end (no flights, requests, acquisitions, windows or metadata
  requests) and no relay reservation is still held;
- no coverage name outside `model::coverage::VARIANTS` appears.

Scenario checks then run on the JSON report (`model::report`): delivered reads, the allowed
failure kinds and rates, origin requests per page, and per-scenario counters.

**Coverage.** `model::coverage` lists every core effect and serve outcome variant.
`make racer-dst` reruns the CI Model entries and fails when a variant is not exercised, unless
it is in `UNREACHABLE` (the core never emits it) or `NOT_YET` (with its reason). A `NOT_YET`
variant that the CI set does exercise also fails, so the list cannot go stale.

## Scenario families

Built-in scenarios live in `scenarios::catalog`; `dst/seeds/ci.txt` picks the seeds CI runs.

| Scenario | Checks |
|---|---|
| `model-concurrent-cold-readers` | 64 nodes read one cold object within 1 ms: every read delivered, readers join flights, origin traffic stays per owner |
| `model-cold-start-400`, `model-cold-start-2000` | every node reads one object within 10 ms: every read ends, at most 1% fail and only with `Overloaded`/`FlightFull`/`Unavailable`, each page reaches origin at most once per owner, pages published |
| `model-hot-page-storm-512` | owners shed a hot page to helpers, every read completes, helpers serve copies, origin once; peak fan-in bounded |
| `model-origin-forbidden` | a 403 fails only the leader that got it and re-elects a waiter |
| `model-range-readers`, `model-range-deadline` | range reads through `ReadWindow`: in-order delivery, second pass from disk; deadline discards held pages |
| `model-metadata-readers` | page reads behind `MetadataCohort` refreshes with origin 5xx |
| `model-faults` | origin 403/5xx mix and cache outages under page and range readers |
| `model-partition-heals` | a site cut then a heal: every read ends, reads after the heal complete |
| `model-hung-peers` | hung peers are found by silence and routed around; bounded failures |
| `model-slow-origin` | under a slow origin, owners above rank 0 do not duplicate rank 0's fetch |
| `model-membership-churn` | nodes leave and rejoin; stale memberships, refreshes and fallbacks; only transient failures |
| `model-node-faults` | drawn crashes, partitions and slow peers; every read ends, only transient failures |
| `model-ctl-*` | control plans (enrollment storm, failover, churn, lagging) on real `racer-authority` rules: single primary, monotonic terms and sequences, clients converge |
| `model-full-*` | a control plan co-stepped with the read path (`model::combined`) plus data-plane faults |

Larger runs and ablations are tests in `tests/model_scenarios.rs`, `tests/model.rs`,
`tests/control.rs` and `tests/combined.rs`; the large ones are `#[ignore]` and run nightly
(`.github/workflows/racer-nightly.yaml`). Topology-only mesh measurements are
`topology/tests/mesh_scenarios.rs`.

## Control plane

`model::control` runs a control plane on the dst engine in which every decision is a real
`racer-authority` rule: seat lottery and renewal, primary contention and election, seat
followers, client enrollment and long polls through seat admission and bearer authorization
(sharing one API token bucket), and the primary pass driven by the real `rules::shell::Shell`.
A state read and the compare-and-swap PUT each take one API round trip, so a concurrent write
is a `Conflict` that re-reads; `Shell::recheck` refuses a write on a stale Lease; a
non-monotonic plan aborts and fails closed.

`model::combined` co-steps it with the read path: committed memberships become Model
membership versions, each node adopts exactly the version its client applied (a node with none
skips its reads, `reads_skipped_not_enrolled`), and Pod crashes and restarts crash or restart
the node. The read load is Poisson arrivals over Zipf objects with a fraction of range reads.
`scenarios::DataFaults` adds slow nodes, origin error bursts, a site cut and, in
`full-cold-start`, a storm on one object. No read starts in the last `2 * read_timeout + 10 s`,
so every read ends.

## Origin repeats

`model::repeats` classifies every origin request for a page that was fetched before, against
the page's previous request: rank 0 moved, it failed, it is still in flight, its holder lost
the copy, or the holder still holds it (by cause). An owner above rank 0 records why its last
probe of rank 0 did not serve (`skips`), and a held repeat behind such a skip is its orphan.
The report's `origin_per_page` object and the `origin_repeats_*`/`origin_skips_*` counters carry
the result. An unexplained repeat is counted, not a hard failure, because range windows can
refetch pages a node already holds (the window fill path does not consult the node store the
way a page read does).

## Known divergences from production

- Not modeled: hedges, successor hints and orphan notices (so `Served::Defer` never happens),
  rejections (`Served::Reject`: probes always match), provider selection streams, prefetch,
  admission quotas, persistence shedding, memory tiers, the disk index and per-node memory
  accounting.
- One page tier: a crash drops all stored pages, where production keeps its last checkpoint. A
  crashed node's own client reads keep running; only its peer server and replies are gone.
- Model nodes share one clock; clock faults are ignored.
- A node that left the membership keeps serving peers and relaying.
- Route searches run synchronously against one `Paths` cache shared by every node (production
  keeps one per worker), so the Model has more cache hits.
- A link circuit records success on connect, not when the exchange completes; backoff jitter is
  a mix of the peer id, not SHA-256 of its name.
- Connections: a refused connection is seen at once rather than a round trip later; connection
  setup costs no time; an accepted connection's worker is `probe % workers`, not the listener's
  choice.
- A response is verified at `verify_ns` whatever its relays (not measured). Peer message heads
  cost latency only, not NIC time.
- The Model never cancels an origin request.
- Control plane: webhook trust and endpoints, CA keyring rotation (defaults only), DaemonSet
  deletion and the ready floor are not modeled.

## Running

- `make racer-dst` runs `dst/seeds/ci.txt` and `dst/seeds/regressions.txt`, each entry twice
  with trace hashes compared, then the coverage check (`DST_SEEDS`, `DST_STEPS`, `DST_FILTER`).
  A numeric `DST_SEEDS` still appends `regressions.txt` and runs the coverage check over the
  default set. `cargo test -p racer-dst --test world` runs both in CI.
- `make racer-dst-report ARGS='--scenario full-failover --nodes 200'` runs one scenario in
  release and writes its JSON report. Options (`cli`): `--scenario`, `--nodes`, `--seed`,
  `--duration`, `--config FILE.json`, `--out PATH`. Scenarios are the control plans
  (`enrollment-storm`, `failover`, `ctl-churn`, `lagging`), the combined `full-*` scenarios
  (default 16 nodes, 0.02 reads per node per second), the aliases in `cli::ALIASES`, and every
  catalog name. `--config` takes the keys in `cli::KEYS` and `cli::NODE_KEYS`; any other key is
  an error. The process exits 1 when a check or invariant fails (after writing the report) and
  2 on a usage error.
- `MODEL_NODES=50000 cargo test --release -p racer-dst --test model_scenarios model_memory --
  --ignored --nocapture` reports peak RSS per node of a Model cold start.
