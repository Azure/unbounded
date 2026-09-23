# DST migration and retained boundaries

The artifact adapter is the complete-journal boundary. Baseline, native, and
scale runners retain executable/source metadata and logs, but do not promote
every selected test to exact replay. The inventory in each run identifies the
actual selectors. Passing the required artifact matrix establishes its declared
cells, not parity with all legacy fixtures.

**Historical validation record, before the mTLS integration:** the bounded
dataplane implementation was locally verified within the declared scope. The
passing runs and acceptance conclusions recorded below apply to their retained
binaries; they do not validate the merged TLS replacement contracts. Current
authentication behavior is summarized below and in the
[DST authentication boundary](README.md#tls-membership-and-monotonic-channel-expiry).
The recorded manifest had **34 required cells**
(`dst/scenarios/campaign.json:4-37`). `dst/artifacts/composition-v3-integration`
passed all 34 plus eight mixed samples: 42 outcome/witness gates and 42 fresh
exact replays, with nine certified lifecycle rounds including a three-round case.
`dst/artifacts/lifecycle-final-baseline` passed 120 executions across five groups.
All three focused lifecycle tests passed. The allocator `model_tests` run passed
25 tests with one permanent opt-in ignored in 8.57 seconds; size reduction accepted
three freshly replayed candidates. Parent independently verified all 54 Python
tests passing in 1.481 seconds. Rust formatting checks passed for all eight
remaining Rust files, and the diff check passed. Nightly selects
templates-v2/composition-v3 and has YAML/`bash -n` validation reported. Hosted
execution and repository-wide lint success remain unclaimed.

## Retained fixtures and retirement gates

| Fixture | Owner | Reason retained | Required evidence before retirement |
| --- | --- | --- | --- |
| `tests/storage/recovery.rs` allocator `Sim` and `Model` | dataplane/storage | Independent logical snapshots, both retained checkpoint roots, and finite crash cases exceed the cluster actor's durable-object witness. Logical and serialized physical parity adapters are additive. | Verify the adapters, preserve both-root and exhaustive finite checks, and establish parity for each remaining legacy contract before replacing its environment. |
| `tests/runtime/scenarios.rs` targeted fixtures | dataplane/runtime | Custom placement, physical workers, and fault histories have assertions beyond the canonical artifact topology. | Port each scenario with its original path and resource assertions, exact success/failure replay, and an explicit independent replacement for every changed routing obligation. |
| `simulation::World` unmanaged progression | dataplane/environment | Component and hybrid tests still use explicit progression or direct completion calls. | Classify each caller, preserve effect/completion separation, and demonstrate managed adapter parity before deleting progression helpers. |
| `tests/security/negotiation.rs` native/hybrid regressions | dataplane/negotiation | Queue pressure, authenticated replacement, stale messages, and real I/O have coverage beyond the confirmation actor and admission mutant. | Retain required selectors; migrate only with equivalent wire, capacity, identity, and ownership assertions. Native I/O validation remains a separate tier. |
| Ignored large-cluster selectors in `tests/runtime/cluster.rs` | dataplane/runtime | The bounded scale profile runs 16 real modeled nodes, not the default 1024-node topology. These entries lack complete journals. | Measure the larger topology within its declared resource budget and add streaming replay before advertising large-scale exact replay. |
| Native io_uring, provider, and real-thread fixtures | dataplane/platform | Simulation cannot execute kernel/provider ordering or host-thread behavior. | Keep capability-gated native checks. The runner currently requires io_uring; hardware/provider and deployment coverage must be requested and reported separately. |
| Subscriber and Go controller integration | dataplane/control | Excluded from this implementation by user direction. | A separately approved control/data-boundary project is required; prepared configuration publication is not controller decision coverage. |

No retained test or engine is removed by this migration. All `tests/` source
paths here are relative to `cmd/racer-dataplane/`. In particular, allocator
`assert_recovery` visits every retained checkpoint and compares it with the
independent snapshot map; a successful cluster GET cannot replace that check.

## Allocator parity domains

The additive adapter in `tests/storage/recovery_parity.rs` captures logical
Job write/sync effects before legacy execution and rebuilds independent `Disk`
instances for crash probes. It compares bytes, slot presence and generations,
and every retained root against workload snapshots through OS-backed and
simulated production recovery (`tests/storage/recovery_parity.rs:179`). Its
assertions include all 256 torn magic-page sector
masks, failed write prefixes, failed sync with no/all propagation, and final-sync
effect durability before completion collection. An older-root-loss sensitivity
check retains the valid newest root while detecting the missing predecessor.
The default `LatestOnly` profile still rejects alternating partial sync.

The opt-in `PartialSync` profile captures failed `Alternating` sync effects and
independently selects even dirty sector indices through `Disk::persist_sectors`
(`tests/storage/recovery_parity.rs:13-95`). Tests assert failed alternating
propagation at both checkpoint barriers, crash probes before/after delivery and
collection, preservation of the durable predecessor, later failed-prefix writes,
out-of-order completion delivery, and a subsequent successful barrier
(`tests/storage/recovery_parity.rs:489`, `:557`). Scratch pages outside retained
roots make partial propagation observably different from None/All. The poisoned
allocator is not resumed; the later-write suffix checks Storage/Disk effects.
This logical profile still excludes physical punch refinement and historical
sector selection. `persist_sectors` establishes selected-sector durable floors
without changing live pages or completing held syncs; validation, holes, version
rebasing, and budget reclamation have focused assertions
(`tests/support/simulation.rs:1818`, `tests/support/simulation_contracts.rs:543`).

`tests/storage/recovery_physical_parity.rs` separately captures submitted inputs
around production `RingIo` and runs one outstanding operation (`max_io = 1`).
Its transcript separates submit, effect, CQ delivery, allocator collection, and
publication for punch, value/page writes, and both sync barriers
(`tests/storage/recovery_physical_parity.rs:163`, `:279`, `:353`). Independently
computed images feed the retained logical snapshot oracle; both recovered roots,
generations, and durable invalid-slot cleanup are compared (`:415`). Assertions
cover selected crash cuts, before-effect failures and error-after-full-effect
completions, historical punch-then-write sector prefixes, and buffer/extent
ownership through abandoned writes until process death (`:475`, `:555`, `:598`,
`:644`). This is serialized physical refinement, not parity for all concurrent
allocator schedules or an artifact-journal adapter. It does not replace the
logical profile's exhaustive root-page enumeration or partial-write cases.

These adapters narrow the retirement gap; they do not authorize deleting `Sim`,
`Model`, or any original finite tests. Parent integration reports the four new
Disk contracts and two partial-sync parity tests passing, and subsequently the
entire `allocator::model_tests` suite: **25 passed, one permanent opt-in ignored,
8.57 seconds**. The ignored test is the long extended seeded checkpoint/crash
campaign (`tests/storage/recovery.rs:1693-1700`), not a required failure hidden
as a pass. This separate allocator evidence covers the non-ignored logical and
physical parity assertions. It does not imply every possible allocator schedule
or the extended sweep passed. Runtime lifecycle verification comes from the
separate focused tests, v3 campaign, and final baseline. The five-group PR baseline
itself does not select allocator recovery
(`dst/scenarios/baseline.json:6-12`).

## Generated lifecycle and composition-v3 (locally verified)

Optional `generated_lifecycle` accepts a seed and 1-3 rounds, requires two
HTTP-only nodes, and counts as the single selected actor
(`tests/runtime/cluster.rs:98-139`). `composition-v3` alternates unchanged v2
action samples in even slots with lifecycle samples in odd slots. Lifecycle
samples derive six world seeds plus an independent actor seed, vary rounds,
4/16/64 KiB queues, and phase/callback policies, and contain no follow-up actions
(`dst/run.py:1080-1119`). It is a bounded explicit composition, not arbitrary
actor mixing.

Per round, seeded choices select the crash node, first admission source, 2-3
held callers per node, 257/4095/4096-byte objects, and persistence stride. After
durable setup, two independent opposite-direction Request gates must hit with
both GET/HEAD cohorts accepted and live. The cooperative actor publishes real
topology revisions on both workers, waits for activation, and checks healthy
traffic while both faults remain effective. It preserves the cache namespace.
A held data-sync barrier must intersect a data-written checkpoint and at least
three dirty sectors, with a 32-tick observation window before non-prefix crash.
The 2500-tick crash-window bound uses normal coordinator turns; setup and recovery
may drain/quiesce outside that window (`tests/runtime/actors.rs:70-365`).

The crash cut explicitly cancels the surviving cohort and loses the crashed
cohort. Recovery checks the original durable object's exact bytes through local
disk/splice with the restarted owner's origin disabled and no origin fetch of
that target, then restores origin service and checks cold peer recovery in both
directions (`tests/runtime/actors.rs:239-461`). The runner certifies ordered
per-round cohort/publication/healthy/crash/retirement/incarnation/durable/cold
evidence and requires `lifecycle_round_certified` for every requested round,
alongside fresh exact replay (`dst/run.py:399-540`).

The actor declares **128/192/256 MiB sparse slabs per node for 1/2/3 rounds**,
using `(16 + 16 * rounds) * 4 MiB` for live/checkpoint-retained extents and index/
recovery headroom. Both nodes must have that geometry; `LifecycleRoundPlanned`
records it and restart must preserve it (`tests/runtime/cluster.rs:1721-1730`,
`tests/runtime/actors.rs:281-307`, `:366-370`). Parent integration reports an
earlier three-round failure from exhausting the old six-extent disk. That failed
attempt remains recorded. The revised budget passed the focused lifecycle tests
and v3 campaign, including `generated-v3-0005` with three certified rounds and a
fresh exact replay (`dst/artifacts/composition-v3-integration/campaign-result.json:1073-1113`).

## Remaining model and campaign limits

- Actor templates compose their documented concurrent operations. Inputs select
  one template plus follow-up actions; arbitrary actor combinations are rejected.
  Default `templates-v2` sampling varies independent seeds across twenty expected-pass
  templates, optionally weighted by prior witnesses (`dst/run.py:800`). Opt-in
  `composition-v2` instead generates 1-4 held-peer windows on 2-4 nodes, at most
  six live admissions and 80 actions, varied object boundaries, GET/HEAD,
  cancellation, wall offsets, queues, transport and scheduling policies
  (`dst/run.py:871`). Each window requires a real `ActionFaultOverlap`: the full
  unambiguous cohort must be accepted and live at a hit peer Request gate
  (`tests/runtime/cluster.rs:2756`, `dst/run.py:312`). An explicit `AwaitOverlap`
  barrier advances normal coordinator turns for at most 500 ticks before
  cancellation, requiring the entire cohort to remain accepted and live without
  renewing deadlines (`tests/runtime/cluster.rs:2352`). Page-boundary objects
  (4 MiB - 1 bytes and larger) require a queue of at least 64 KiB so upstream
  page fills fit the unchanged deadline under modeled 1-3 ms I/O delays. Smaller
  objects retain 4/16/64 KiB sampling; recorded pressure ratios describe
  configuration, not witnessed backpressure (`dst/run.py:941`). Release still settles and
  heals before subsequent windows. The new v3 lifecycle implements bounded
  reload/independent-gate/dirty-crash/recovery overlap separately; it does not
  widen v2's action grammar. The required lifecycle cell and mixed v3 samples now
  have passing local witness/replay evidence; no full cross product is claimed.
- Cross-domain phase permutations retain bounded fairness and next-turn CQ
  delivery. Ready compute callbacks can use bounded batch permutations; default
  callbacks and delayed peer notifications retain turn/FIFO ordering. This is not
  enumeration of all enabled kernel events.
- Shared-worker coverage exercises common listeners, publication, flight ownership,
  and a whole-process crash with joined requests accepted on both workers. Recovery
  reconstructs both workers and verifies shared fetches and publication activation.
  Follow-up actions run after an explicit single-worker process restart because
  retiring a listener does not remove its process-lifetime publication subscription.
- Wall offsets and bounded directional queues have conformance and integration
  fixtures. Required full HTTP reset/FIN actors now check accepted in-flight
  requests, healthy progress, server retirement and new-connection recovery;
  FIN also checks exact response framing/bytes (`tests/runtime/actors.rs:676`).
  The current `http-wall-expiry` actor checks TLS Pod-membership rejection across
  +/-62-second wall steps, same-descriptor recovery on a fresh connection after
  membership restoration, and completion after a post-admission wall jump
  (`tests/runtime/actors.rs:894-1078`). The component authentication cell checks
  membership revocation and monotonic channel expiry (`tests/security/negotiation.rs:121-177`);
  shared-worker contracts check common modeled TLS identity and incarnation fencing
  (`tests/support/simulation_contracts.rs:1036-1134`). These replace request-signature
  windows and nonce retention. Simulated identities do not establish native
  certificate validity or encrypted-record correctness. Historically, all three
  pre-mTLS HTTP tests and their required campaign/replay gates passed; that record
  does not cover the replacements. One-shot socket/pipe `POLL_ADD` models readiness
  and separate effect/CQ delivery, with four focused tests passing; multishot and pipe endpoint
  closure remain excluded (`tests/support/simulation.rs:1274`,
  `tests/support/simulation_contracts.rs:177`). These results do not claim native TCP,
  key rotation, delayed control delivery, or every disconnect phase.
- Selected custom routing scopes now use an independent logical `Placement` model
  for B03 co-location/fallback, all-local admission, candidate caps, and shared-NUMA
  takeover; B02 exported-placement scopes remain opt-in with `B02_EXPORT`
  (`tests/runtime/scenarios.rs:111`, `:378`, `:604`, `:749`, `:1636`). The model
  derives paths and physical hops independently, checks cold/cached/failed origin
  contracts, and bounds candidate chains within each worker/incarnation
  (`tests/runtime/oracles.rs:39`, `:254`). Transport observations lack cursor IDs;
  checks match the declared route set and existential candidate chains, not
  per-request transport causality. Generic fixture declarations still do not
  establish independent replacement coverage for every custom topology, or
  complete-journal replay of those legacy fixtures.
- The reducer preserves named failure identity and typed path witnesses as an
  ordered subsequence, including repetitions. Its current policy retains involved
  Invokes, stable normalized caller links, overlap cohort membership/order, and
  terminal response/cancel links (`dst/run.py:1424`). `/sized/N` reductions apply
  one input-validated size decrease consistently across target references and
  typed witnesses without merging keys or changing ranges, methods, endpoints,
  causes, or seeds (`dst/run.py:1368`). Socket capacity, callback policy, and
  dependent checkpoint/shared-worker modifiers are reducible too. Arbitrary
  actor internals and a complete causal graph are not claimed. Every accepted
  candidate still needs the same named failure, preserved witnesses, a fresh
  journal and exact replay (`dst/run.py:1641`). Lifecycle proposals can remove the
  actor or reduce rounds while preserving its seed; causal request/fault lists
  remain part of the witness signature. This support is not an observed passing
  lifecycle-reduction run.
- All six initial semantic mutant families have paired controls and exact replay.
  Some exercise production components directly; their evidence is not relabeled
  as end-to-end HTTP, kernel, or deployment coverage.

## Acceptance status and boundaries

Re-review of the production-path actor, causal certification, retained results,
and original bounded exit gates supports local completion of this dataplane
increment (`tests/runtime/actors.rs:70-461`, `dst/run.py:399-540`,
`/tmp/DST_IMPROVEMENTS.md:382-406`). The generated overlap now witnesses traffic,
two independent effective faults, real topology activation, healthy progress,
dirty checkpoint crash, process restart, and durable/cold recovery. All 1-3-round
bounds are represented in passing focused/campaign evidence. The required
manifest includes a lifecycle cell, and nightly shard 1 selects v3
(`dst/scenarios/campaign.json:4`, `.github/workflows/racer-dst-nightly.yaml:25-33`).
This is bounded implementation/local-verification completion, not completion of
the separately deferred control-plane bridge or universal simulation coverage.

### Actual remaining checks and conditional claims

- **Repository tooling:** parent independently verified all 54 Python tests,
  Rust formatting for all eight remaining Rust files, and the diff check. The
  repeated `make fmt` attempt under a 90-second cap still panicked because the
  golangci-lint binary was built with Go 1.26 while loading Go 1.27 code; it made
  no Go edits. Repository-wide formatting/lint success remains blocked on a
  compatible toolchain. This is the remaining local tooling blocker, not a
  pending DST implementation, Python, baseline, or replay check.
- **Hosted enforcement:** templates-v2/composition-v3 wiring and YAML/`bash -n`
  checks are in place (`.github/workflows/racer-dst-nightly.yaml:25-33`, `:62`).
  Actual hosted jobs must execute before claiming hosted acceptance. This is an
  execution boundary, not missing sampler implementation or local replay evidence.
- **Preserve tier-specific evidence:** the allocator suite's ordinary tests now
  have passing evidence; its permanent opt-in sweep is not newly mandatory.
  Native/provider/API/deployment execution remains required only in the declared
  tiers, not inferred from managed results (`/tmp/DST_IMPROVEMENTS.md:343-346`).
- **Conditional retirement gates:** preserve the retained fixtures and explicit
  blockers above. Equivalent path/resource assertions and storage parity are
  required before replacing a legacy mechanism, not a demand to migrate/delete
  every mechanism in this increment (`/tmp/DST_IMPROVEMENTS.md:221`, `:390`, `:404`).

### Optional extensions or separately deferred scope

Unrestricted actor/configuration cross products, instruction-level exhaustive
scheduling, partial-order reduction, a complete per-request causal graph, and
arbitrary actor-internal shrinking are not new acceptance blockers. Neither a
mandatory 1024-node campaign nor an artifact-backed reduction for every witness
shape follows from the original bounded gates. The verified size fixture has
causal invocation/response evidence; cohort substitution sensitivity is checked
separately by the runner tests. Larger scale or B02 export-dependent claims need
their own budgets/prerequisites only when exercised. Subscriber/controller
extraction remains deferred by user direction, rather than silently counted as
implemented or added to this dataplane increment.

## Verification snapshot

**Latest verified results: v3 campaign 42/42 gated and freshly exact-replayed,
nine certified lifecycle rounds; final PR baseline 120 executions/five groups;
three focused lifecycle tests passed; allocator model_tests 25 passed/one
permanent opt-in ignored; size reduction three accepted fresh replays. Parent
independently verified all 54 Python tests in 1.481 seconds, Rust formatting for
all eight remaining Rust files, and the diff check.**
Parent integration owns builds, tests, and replay verification. This documentation
update read retained results and ran no builds or tests. Hosted-CI execution has
not been observed here.

| Verification scope | Result | Evidence |
| --- | --- | --- |
| Required + `composition-v3`, root seed 19 | 34 required + 8 mixed samples; 42 planned/attempted/exercised/gated, no gaps; all 42 fresh exact replays passed; 9 certified lifecycle rounds | `dst/artifacts/composition-v3-integration/campaign-result.json:1-11`, `:82-89`, `:900-1185` |
| Final PR baseline | 120 executions passed: 38 + 9 + 43 + 15 + 15; measured suite times 19.73/15.41/83.29/10.35/43.03s | `dst/artifacts/lifecycle-final-baseline/result.json:60-104`, `:162-177`, `:235-284`, `:342-363`, `:421-442` |
| Generated lifecycle focused tests | 3 passed: input bounds/mixing, overlap/recovery, sync-mutant barrier detection; included in final baseline | Parent integration report; selectors at `dst/artifacts/lifecycle-final-baseline/result.json:168-170`; assertions in `tests/runtime/actors.rs:465-501`, `tests/runtime/cluster.rs:343-389` |
| Earlier required + `composition-v2`, root seed 19 | 33 required + 8 samples; 41 gated and exact-replayed, no gaps | `dst/artifacts/composition-budgeted/campaign-result.json:1-11`, `:80`; generated replay results `:851-1094` |
| New Disk propagation contracts | 4 passed | Parent integration report; assertions in `tests/support/simulation_contracts.rs:543`, `:594`, `:667`, `:706` |
| Opt-in logical partial-sync parity | 2 passed | Parent integration report; `tests/storage/recovery_parity.rs:489`, `:557` |
| Independent routing model | 4 passed | Parent integration report; `tests/runtime/oracles.rs:418`, `:446`, `:478`, `:657` |
| Full HTTP actors | 3 passed; corresponding required cells gated and exact-replayed | Parent integration report; `tests/runtime/actors.rs:1043-1075`; `dst/artifacts/composition-budgeted/campaign-result.json:83-172` |
| POLL_ADD contracts | 4 passed | Parent integration report; `tests/support/simulation_contracts.rs:177`, `:266`, `:339`, `:455` |
| Retained PR baseline, before lifecycle additions | 114 executions passed across all five groups: 38 + 6 + 40 + 15 + 15 | `dst/artifacts/parallel-complete-baseline/result.json:103-105`, `:173-175`, `:277-279`, `:356-358`, `:435-437` |
| Entire allocator `model_tests` | 25 passed, 1 permanent opt-in ignored; 8.57 seconds | Parent integration report; ignored extended sweep declared at `tests/storage/recovery.rs:1693-1700` |
| Artifact-backed size/action reduction | Intentional `response.status` preserved; 3 actions to 1 GET, 4096 to 0 bytes, 3 accepted fresh exact replays | `dst/artifacts/size-reduction-source/semantic.json:1`; `dst/artifacts/size-reduction-result/reduction.json:187-243`, `:295-333`; candidates 0000/0002/0004 `replay-result.json:54-57` |
| Python runner | Parent independently verified 54 passed in 1.481 seconds; earlier 49-pass result retained as historical | Parent report: `timeout 20s python3 -B -m unittest discover -s dst` |
| Rust formatting and diff | Checks passed for all eight remaining Rust files; diff check passed | Parent independent verification report |
| Repository-wide `make fmt` | Retried under a 90-second cap; Go 1.26-built golangci-lint panicked on Go 1.27 code; no Go edits | Parent report; tooling blocker remains unresolved |
| Generated lifecycle sparse budget | 128/192/256 MiB-per-node budget verified by focused tests and campaign, including three-round case | `tests/runtime/cluster.rs:1721-1730`; `dst/artifacts/composition-v3-integration/campaign-result.json:1073-1113` |
| Nightly workflow wiring | Shard 0 `templates-v2`, shard 1 `composition-v3`; YAML and `bash -n` validated locally, hosted execution unclaimed | `.github/workflows/racer-dst-nightly.yaml:25-33`, `:62`; parent integration syntax-check report |

Passing campaign gates include expected named mutant failures and their controls;
they are not 42 successful product executions. Per-cell `record_outcome` remains
separate from the gate result.

The size-reduction source is an intentional mutant failure, not an unexpected
product regression. Its accepted result retains the normalized caller's `Invoke`
and status-201 `Response`, along with mutant activation, under
`causal-subsequence-sized-target-v2`. All three accepted candidates have passing
fresh replay results despite expected libtest exit 101. The two candidates that
removed the failure returned product pass and were rejected by reduction
(`dst/artifacts/size-reduction-result/reduction.json:40-74`, `:112-146`). This
fixture verifies size/action reduction and causal caller linkage; it contains
no overlap cohort and is not presented as an end-to-end cohort-reduction run.

Earlier unsuccessful composition runs remain retained and classified:

| Retained run | Actual result | Evidence |
| --- | --- | --- |
| `dst/artifacts/composition-integration` | 33/41 exercised; 5 unexercised for missing overlap witnesses, 3 simulator failures | `campaign-result.json:3-16`, `:101-105` |
| `dst/artifacts/composition-accepted` | 38/41 exercised; 3 simulator failures | `campaign-result.json:3-49` |
| Earlier three-round generated lifecycle | Failed with old six-extent disk exhaustion; later revised-budget run passed, but this failed attempt remains recorded | Parent integration report; replacement budget in `tests/runtime/cluster.rs:1721-1730`; later evidence in `dst/artifacts/composition-v3-integration/campaign-result.json:1073-1113` |

The accepted-cohort barrier, modeled POLL_ADD readiness, and capacity/deadline
constraint describe the verified v2 implementation. Its later successful bundle
does not relabel the earlier v2 failures. Likewise, the passing revised-budget
lifecycle run does not relabel or remove its earlier disk-exhaustion failure.

### Historical snapshots

Runs use a 23,000,000,000-byte process-tree cgroup limit, no swap, and explicit
build/suite/campaign deadlines. Retained artifacts are ignored local outputs.

| Tier | Historical verified result | Artifact directory |
| --- | --- | --- |
| PR baseline | 94 test executions passed across five groups | `dst/artifacts/stream-baseline` |
| Required artifacts | 30 cells passed their outcome/witness gates and fresh-process exact replay | `dst/artifacts/ready-callbacks` |
| Nightly sampler | 26 required plus 13 sampled cells passed and exact-replayed | `dst/artifacts/nightly-matrix` |
| Native | Required kernel capability, negotiation, and remaining library: 376 test executions passed | `dst/artifacts/native-capabilities-owned` |
| Bounded scale/formats | Four gates passed at 16 nodes, seed 19 | `dst/artifacts/scale-contract-correction` |

These previously recorded runs were performed at their respective incremental
revisions; each bundle retains its executable hash and source metadata. Hosted workflow syntax
was checked locally; execution on GitHub runners has not been observed here.
At that earlier snapshot Rust formatting passed. Parent's latest Rust formatting
and diff checks also passed. The latest repository-wide `make fmt` retry under a
90-second cap reproduced the Go 1.26-built golangci-lint panic while loading
Go 1.27 code, with no Go edits; repository-wide lint success remains unclaimed.
