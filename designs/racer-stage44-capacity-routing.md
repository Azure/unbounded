# Stage44: capacity-weighted shortest-hop routing

## Decision and boundary

Implement a narrowly scoped, default-off `RACER_ROUTING_ALGORITHM=4` using
existing authenticated membership shares. The offline model supports meaningful
transit relief at unchanged shortest distance, not a fourfold speedup or proof of
live capacity. **No image build, push, deployment, annotation change, cluster
command, NIC canary, or affinity change was performed.**

Base is exactly `5e1a4554f8034fef676d5ca91315da80646e5b71`, not its parent,
the failed 16ab image, or concurrent parent changes. Work used the dedicated
`tmp/racer-stage44-capacity-routing` worktree. No subagent tool was available.
`~/design/AGENTS.md` is absent; read repository AGENTS.md and `~/design.md:70-74`.
Parent request-path documents were not edited.

## Source findings before modeling

- `cmd/racer-dataplane/src/topology/graph.rs:26-36`: graph depends on sorted UID
  positions/count, not shares. Degree remains at most 36.
- `topology/equal_cost.rs:93-137`: v3 finds all shortest eligible first hops with
  visited-node and failed source-edge filtering. V4 reuses this unchanged search.
- `topology/paths.rs:305-335` at the base: v3 uses request/attempt/current/destination
  SHA256 and uniform index selection. It is not capacity-aware.
- `topology/membership.rs:16-22,72-81` and `placement.rs:64-110`: positive member
  shares already affect HRW ownership. Source of authority remains the accepted
  controller snapshot, not a receiver-provided load number.
- `internal/racer/membership.go:43-69` and `bootstrap.go:152-207` at the base:
  administrator Node shares override authenticated enrollment proposals. Snapshot
  delivery is authenticated HTTPS/mTLS, not a new separately signed envelope.
- `security/protocol.rs:175-191`: no routing/topology-v3 header exists here.
  The peer wire/profile is already v4, separately from routing selection. Runtime
  algorithm selection is not negotiated; coordinate all participating processes.
  The selected next receiver remains signed and recomputation is checked.

Stage41 correctly predicted owner relief but essentially no transit relief.
Stage43 measured cohort goodput 1.241981 -> 0.835982 GB/s (-32.6896%), fleet
+0.6560%, then restored all eleven shares. That outcome is not evidence for
another owner-only trial. Historical reports describe intent/evidence; deployed
source above defines the contract. Stage41's recommendation to separate future
relay capacity is valid for independent tuning. This task explicitly permits
evaluating reuse of shares: v4 documents the coupling rather than claiming a
separate capability field exists.

## Actual UID/catalog expectation

Inputs are retained stage43 membership4993, Node UID inventory and all1500
baseline rows, stage16 verified 512-image catalog, and stage41's eleven UIDs/cache
identity. Exact input SHA256s are in `racer-stage44-model.json:11-16`. Membership
UID set/order matches stage41. The model recomputes catalog ownership with the
deployed slot framing and Q32.32 HRW comparison, checking every cohort before/
after owner byte count against stage41. No hardcoded host IDs occur in source.

All 18,417 pages / 18,249 slots / 274,439,398,400 layer bytes are represented.
Each consumer's measured verified rate supplies its demand, spread over a complete
catalog pass. Payload flows return along request paths. Destination-rooted BFS
propagates exact expected flow in descending distance using eligible-hop weight
over total eligible weight. Local hits contribute no wire flow; constrained nodes
remain owners, destinations, sources and eligible transit nodes. Every node is
included even when slow. All2,250,000 endpoint-pair distances are evaluated:
1500 local, 53,658 one-link, 898,642 two-link, 1,296,200 three-link. None needs
more than three links here; the general normal four-link ceiling is preserved.

This is primary-hit payload expectation, not replay. It omits cache retention,
coalescing, alternate-owner hits, failed/partial/retried transfers, metadata and
admission/queue dynamics. Raw modeled cohort TX87.173/RX62.665 Gbit/s is much
larger than measured21.132/25.939: absolute modeled wire demand must not be
presented as actual NIC traffic or an achievable fleet throughput.

At measured baseline fleet586.074856 GB/s:

| Weights | Cohort transit | Cohort TX | Cohort RX | Healthy max TX / RX | Healthy CV TX / RX |
| --- | ---: | ---: | ---: | --- | --- |
| Uniform routing/owners | 52.736 | 87.173 | 62.665 | 11.743 / 10.464 | .147990 / .112218 |
| Owners only4:1 | 52.809 | 65.109 | 62.742 | 11.756 / 10.469 | .148293 / .113428 |
| Routing only4:1 | 35.467 | 69.904 | 45.396 | 11.756 / 10.484 | .147982 / .112872 |
| Both4:1 | 35.523 | 47.823 | 45.456 | 11.770 / 10.489 | .148335 / .114138 |
| Both, relay weight approaches zero | 28.573 | 40.873 | 38.506 | 11.775 / 10.497 | .148370 / .114426 |

All rates are Gbit/s, summed only for the explicit cohort columns. Maxima are
maxima of expected loads, not expectations of finite-flow maxima. Routing-only
reduces cohort transit32.75%; both reduces32.64%. Every constrained node benefits.
Mean links2.551666730 is exactly unchanged by routing-only; owner movement changes
it to2.551657954 for owner-only and both. Uniform-demand and recovered-demand
scenarios are also included in the JSON, rather than assuming everyone currently
pulls at the mean.

### All eleven nodes

Each cell is `transit; TX/RX`, expected Gbit/s at actual baseline demand.
Names have prefix `aks-ddsv6-84072342-vmss`; full names/UIDs are in the JSON.

| Suffix | Uniform | Routing only | Both |
| --- | --- | --- | --- |
| 00000h | 5.284; 7.602/6.109 | 3.638; 5.956/4.462 | 3.666; 4.525/4.491 |
| 00002i | 4.200; 6.795/5.125 | 2.862; 5.458/3.787 | 2.871; 3.466/3.797 |
| 00003z | 4.158; 7.412/4.961 | 2.793; 6.048/3.596 | 2.767; 3.844/3.571 |
| 00005f | 4.280; 8.592/5.264 | 2.828; 7.140/3.812 | 2.846; 4.281/3.831 |
| 000066 | 5.224; 8.618/6.133 | 3.541; 6.935/4.450 | 3.558; 5.164/4.468 |
| 00006d | 5.245; 7.725/6.103 | 3.468; 5.948/4.326 | 3.477; 4.460/4.335 |
| 000078 | 4.413; 7.877/5.336 | 2.967; 6.432/3.891 | 2.983; 4.800/3.906 |
| 0000b1 | 5.296; 7.588/6.174 | 3.539; 5.831/4.417 | 3.540; 4.687/4.418 |
| 0000bl | 5.024; 9.678/5.997 | 3.429; 8.083/4.402 | 3.456; 5.175/4.430 |
| 0000cp | 4.291; 7.658/5.240 | 2.878; 6.245/3.827 | 2.899; 3.472/3.849 |
| 0000d9 | 5.322; 7.628/6.224 | 3.524; 5.830/4.426 | 3.460; 3.949/4.362 |

### Healthy-node burden and finite-flow maxima

Worst individual healthy relative TX increase:1.416% routing-only at ddv5/0000af
(baseline host5.407 cores);7.622% both at ddsv6/00002d (5.483 cores). Worst RX
increase1.150% routing-only and1.484% both. The largest absolute expected TX stays
adsv5/0000ap; RX stays ddsv6/00006h. There is no new bottleneck in this payload
expectation, but CPU averages and measured NIC rates do not prove capacity slack.

`racer-stage44-simulation.json` supplements the expectation with eight reproducible
finite-flow samples, each64 byte-weighted owner samples per consumer, actual UIDs,
the specified SHA256 selector, and reselection at every hop. All1500 participate.
Mean sample maxima TX/RX: uniform12.448/11.065, routing-only12.293/11.379,
both12.405/11.281 Gbit/s. Largest observed seed maxima:13.345/11.375,
12.692/12.250,12.755/11.766 respectively. Mean cohort transit53.593,
35.792,35.993 supports the analytical relief. These are finite Monte Carlo
estimates for64 samples, not confidence bounds or modeled live queue saturation.
Do not hide the RX maximum increase or claim zero risk elsewhere.

## Demand feedback and useful ceiling

Hold all healthy consumers at their measured demand; interpolate each constrained
consumer from its own baseline toward the healthy mean. At full recovery, both
weights require cohort RX70.236 rather than45.456 Gbit/s, while transit changes
only35.523 ->35.680. Receiver demand consumes the freed capacity.

Use a deliberately explicit residual fit per node: `alpha=(measured_RX-local)/
modeled_transit`, `beta=(measured_TX-alpha*transit)/modeled_owner`. Predict
`RX=local+alpha*new_transit`, `TX=beta*new_owner+alpha*new_transit`. This preserves
local useful demand rather than scaling it down with NIC traffic. Assume no other
overhead and unchanged cache/coalescing behavior; use baseline observed rates as
operating caps, **not proven NIC limits**. It is an optimistic sensitivity only.

- Routing-only admits a common18.24% recovery toward healthy mean before the
  first cohort RX cap: cohort1.241981 ->1.803450 GB/s (+45.2%).
- Both admits18.03%:1.796935 GB/s (+44.7%). Its larger TX relief adds almost no
  receiver benefit in this fit. Fleet gain only about0.555 GB/s (+0.095%) if
  healthy rates do not change.
- The pointwise near-zero relay-weight limit gives2.024629 GB/s (+63.0%), not
  full recovery. It is the limit of this immediate-next-hop weighting family,
  **not a global shortest-path optimization bound**; downstream-aware tie choice
  may avoid nodes even before they become immediate candidates.
- All11 have positive modeled recovery headroom; both per-node recovery fractions
  span18.03-25.15%. Fully recovering all11 would require fitted per-node RX roughly
  3.94-4.36 Gbit/s, above baseline observed2.16-2.58. No4x claim is warranted.

This supports a useful structural primitive, not a live rollout decision. If the
goal is substantially more than partial cohort recovery, the next real option is
authenticated **separate relay capacity** plus downstream-aware shortest-path
costs/reservations that account for mandatory transit and preserve receiver
headroom. That needs a versioned control contract and a new model; another NIC
canary, local concurrency reduction, or owner-only change cannot provide it.

## Implementation and validation

Public specification and independent vectors: `cmd/racer-dataplane/src/topology/
ALGORITHM_V4.md`. Reuse shares with no new keys or wire fields; default2 and v3
vectors unchanged. Selection uses SHA256 domain separation and unbiased integer
rejection mapping; positive u32 weights, u64 total, at most64 draws with deadline
checks, fail closed on exhaustion. Existing alternative cache remains bounded,
weak-leased and request-independent. Low-capacity destinations/local clients are
never excluded. Graph/search/budget/signing algorithms are unchanged.

Focused validation (all external TERM, kill-after10s, <=300s):
- Paths tests19 passed, including independent v4 vectors, proportional/uniform
  frequency, integer rejection, u32 max, actual production search/reselection for
  all1500 synthetic endpoints, shortest-distance oracle, changed membership,
  cache eviction, failed links, visited nodes, exhausted budgets and deadlines.
- Signed receiver wire/recompute tests2 passed (v3 and weighted v4).
- Supported runtime configuration test1 passed.
- Offline model tests3 passed, including actual1500 all-pairs distances,
  conservation, all11 transit relief and all1500 positive receiver demand.
- Rust formatting passed. Initial max-u32 test expected index0 incorrectly;
  exact arithmetic gives5. Corrected that assertion, then the focused suite passed.
- Replaced a placement cutoff proximity optimization with an exact cost-tie
  check, reran only placement, and checked identical checkpoint contents.
- Required scoped `make fmt GO_PACKAGE_DIRS=internal/racer
  GO_PACKAGE_PATTERNS=./internal/racer/...` ran gofumpt, then installed lint
  panicked (built with Go1.26; input requires Go1.27). No Go source changed.
  No broad application suite or unrelated toolchain replacement was attempted.

Reproducibility: model script has separate bounded `placement` and `routing`
phases. Supply `--membership`, `--cohort`, `--inventory`, `--baseline`, `--catalog`
using the exact filenames/hashes above; write placement to a checkpoint, then run
`routing --checkpoint ... --output ...`. The simulation takes that checkpoint and
an output path. The report script takes checkpoint/full-result and optionally a
compact-output path. Raw checkpoints/full1500-row results remain in parent tmp;
compact all-cohort summaries and seed maxima are committed alongside this report.
