# Racer dataplane DST campaigns

**The bounded dataplane DST implementation is complete and locally verified
within the boundaries below.** The current manifest has **34 required cells**.
`dst/artifacts/composition-v3-integration` passed all 34 plus eight mixed samples:
**42/42 outcome/witness gates and fresh-process exact replays**, with nine
certified lifecycle rounds including a three-round case
(`dst/artifacts/composition-v3-integration/campaign-result.json:1-11`, `:82-89`,
`:1073-1113`). The final PR baseline passed
**120 executions across all five groups** in `dst/artifacts/lifecycle-final-baseline`.

All three focused generated-lifecycle tests passed. The allocator `model_tests`
run passed 25 tests with one permanent opt-in ignored in 8.57 seconds; size
reduction accepted three freshly replayed candidates. Parent independently verified
54 Python tests passing in 1.481 seconds with
`timeout 20s python3 -B -m unittest discover -s dst`. Rust formatting checks passed
for all eight remaining Rust files, and the diff check passed. Nightly selects
templates-v2 and composition-v3; YAML/`bash -n` checks passed, but hosted execution
is unclaimed. Earlier failures and the repository-wide linter blocker remain
recorded. See [acceptance boundaries and verification](MIGRATION.md#acceptance-status-and-boundaries).

## Prefix exploration and reduction

```sh
python3 dst/run.py explore dst/artifacts/campaign/status-mutant --choices 8 \
  --seed 71 --timeout 90 --artifacts dst/artifacts/prefix-exploration
python3 dst/run.py reduce dst/artifacts/prefix-exploration/record \
  --timeout 90 --artifacts dst/artifacts/prefix-reduction
```

`explore` first exactly replays the source, retains its executable and up to 1024
initial branching choices, and changes only the scheduler seed. Each prefix
choice validates the enabled count and ordered identity fingerprint, including
preceding singleton history. The continuation uses the new seed; the resulting
execution writes a new complete journal and must freshly exact-replay. A shorter
execution that cannot consume the prefix is rejected. This command requires an
adapter binary supporting `schedule_prefix`; older retained binaries reject the
new input field. Prefixes are bounded separately from the diagnostic tail.

The reducer proposes shorter prefix lengths before other dimensions. Acceptance
still requires the same named failure, ordered repeated path witnesses, and a
fresh exact replay of the candidate journal. It does not splice or relabel the
source transcript. The 1024-choice adapter bound keeps the resolved input/header
within the journal's bounded record size.

Nightly selection can prioritize gaps from a retained campaign:

```sh
python3 dst/run.py campaign --tier nightly --samples 24 --timeout 900 \
  --coverage-from dst/artifacts/previous-campaign --artifacts dst/artifacts/guided
```

The optional `witness-weighted-fair-v1` policy assigns each passing template a
base weight of one, adds one for each missing required transition kind, and adds
one if no prior run passed its gate and exact replay. Prior sampled cells map back
to their template identities. Deterministic weighted fair selection retains
nonzero allocation for covered templates; a finite sample budget may still leave
some templates unsampled. The required matrix always runs in full. The resolved
manifest records weights and hashes of both prior inputs, which are copied into
the new bundle. These diagnostic coverage records guide selection; they are not
a substitute for the new run's witness gates or exact replay. Without
`--coverage-from`, the default `templates-v2` sampler remains round-robin.
The opt-in `composition-v2` sampler described below generates bounded action
workloads. The new `composition-v3` alternates those preserved inputs with
generated lifecycle actors. Neither accepts template coverage weights.

Run from the repository root:

```sh
python3 dst/run.py run --profile pr --artifacts dst/artifacts/pr
python3 dst/run.py run --scenario simulator-contracts --seed 19
python3 dst/run.py report dst/artifacts/pr
timeout --signal=KILL 10s python3 -m unittest discover -s dst
```

The runner builds the attached library tests once with two compiler jobs, executes
one suite at a time with one test thread, and applies per-build/per-suite host
deadlines. A verified cgroup-v2 ancestor must cap memory at 23,000,000,000 bytes
and disable swap. Otherwise the runner enters a user systemd scope with those
limits. This bounds the aggregate process tree below 24 GB, including subprocesses.

`scenarios/baseline.json` owns the initial selector, owner, tier, and execution
contract inventory. Each run records all discovered library selectors, including
ignored tests as opt-in, and validates required regression selectors. Hybrid
negotiation tests remain in the native tier. The native tier requests io_uring;
this is not yet a provider, envtest, binary-test, or deployment campaign.

Artifacts include the resolved manifest, build identity and log, tracked dirty
patch and untracked-file list, per-suite logs, inventory, and incremental results.
The baseline wrapper records seeds but does not claim exact replay of arbitrary
legacy or hybrid fixtures. Its result counts are suite test executions, not unique
coverage cells: generated tests also occur in owning suites. Product assertion
failures are currently coarse libtest failures; the wrapper does not infer an
oracle ID from a panic string. A killed runner leaves `complete: false`.

## Verified baseline

The final local baseline in `dst/artifacts/lifecycle-final-baseline` passed all
five groups: 38 simulator contracts (19.73s), 9 generated lifecycles (15.41s),
43 managed cluster (83.29s), 15 targeted cluster (10.35s), and 15 causal-routing
executions (43.03s), totaling 120. These are observed suite times and executions,
not production throughput, distinct coverage cells, or universal exact replay
(`result.json:60-104`, `:162-177`, `:235-284`, `:342-363`, `:421-442`). The earlier
114-execution run remains in `dst/artifacts/parallel-complete-baseline`.

The separate entire allocator `model_tests` run passed 25 tests, with the one
permanent opt-in long campaign ignored, in 8.57 seconds (parent integration
report). This covers the ordinary model/parity suite, not the ignored extended
seed sweep (`cmd/racer-dataplane/tests/storage/recovery.rs:1693-1700`).

At `cc0a20cd`, all five PR groups passed under the memory cap and deadlines:
15 simulator contracts, generated lifecycles, managed cluster, targeted cluster,
and causal routing (63 test executions total). The full-page latency test is a
required HTTP/RDMA regression, superseding the old ignored-503 description.
Confirmation/replacement regressions are explicitly required in the inventory;
their native execution is separate from these managed-group results.

The scope is dataplane infrastructure. The production Subscriber/controller
decision-core extraction and cross-language stepped bridge are deferred by user
direction. Legacy fixtures, scale suites, and native requirements must remain
visible while managed artifacts are introduced incrementally.

## Complete journals

`python3 dst/run.py run --scenario artifact --seed 19 --artifacts dst/artifacts/example`
records the exact `runtime::dst::artifact_campaign` adapter. Replay in a new process
with `python3 dst/run.py replay dst/artifacts/example`. The bundle retains the libtest
binary and verifies its SHA-256; moving the bundle preserves replayability on a
compatible host. `--input path.json` accepts a resolved scenario, including six
independent seeds and serialized workload actions. Edit these inputs only for a
new semantic run, never for exact replay.

The versioned hash-chained JSON-lines journal streams all branches, environmental
timing and entropy observations independently of the 64-entry diagnostic tail.
Terminal validation includes the complete trace digest, trailing singleton history,
choice count, outcome, and disk digests. Exact mode rejects unexpected EOF and
trailing records. A killed run without a terminal record is incomplete and cannot
pass exact replay. Existing `World::replay` remains explicitly bounded prefix
exploration with seeded continuation; it is not this exact mode.

Unclassified Rust panics are simulator failures, not named product-oracle evidence.
Only this dedicated adapter currently emits complete journals. Baseline selectors
remain classified by their existing contracts.

### Cooperative overlap and named failures

`python3 dst/run.py run --scenario overlap-reconfigure-restart --seed 19`
records a two-node HTTP cell with two simultaneous request-phase stalls,
nonblocking topology publication, caller cancellation, process restart,
independent healthy traffic, and cold recovery probes. Fault steps never run a
nested scheduler. Admissions are bounded to three live callers on the traffic
node; fault completion has a virtual-time budget. Required witnesses fail the
cell if the gates or live publication do not execute. Namespace-changing reload,
RDMA overlap, and shared-listener workers require separate cells.

Typed invocation, response, cancellation, process-loss, publication, and fault
observations are streamed into complete journals. Cancellation records caller
retirement intent; terminal ring ownership is still checked by the existing
teardown invariants. The older string observations remain diagnostic and retain
their existing dependency/routing checks.

The test-only `SuccessfulGetStatus` mutant changes an actual production GET
response accessor. `dst/scenarios/response-status-mutant.json` is an intentional
failure fixture for `run --scenario artifact --input ...`; it must fail with
`response.status`. Its unmutated control passes in the owning-module test.
`replay` succeeds only when the recorded failure and complete journal match.
This first mutant checks response semantics; it does not certify ownership,
durability, or session-fence oracle sensitivity.

## Environment policies

Resolved inputs may set `socket_capacity` in bytes. Each socket direction has an
independent bounded receive queue; SEND waits for space and splice reports
backpressure before consuming source pages. The default preserves the existing
fixtures' unbounded queues. `WallOffset(node, milliseconds)` changes only that
node's wall clock, leaving monotonic request deadlines unchanged.
`CrashSectors(node, sectors)` selects arbitrary atomic 512-byte dirty sectors,
including punched holes, for the next crash. Completed sync barriers remain
durable. Legacy `Restart` retains its zero-prefix policy.

`dst/scenarios/environment.json` exercises these policies through the production
cluster and supports fresh-process exact replay. Model conformance independently
checks non-prefix byte images, barrier preservation, directional backpressure,
and wall/monotonic separation. This cell does not claim a dirty-checkpoint overlap
witness or production authentication-expiry coverage.

One-shot `POLL_ADD` is now modeled for socket/pipe readiness. Socket polls wait
for requested IN/OUT/RDHUP readiness and always report ERR/HUP when present;
pipe readiness follows queue occupancy. Poll effects and CQ delivery remain
separate, including when readiness disappears before delivery. Unsupported
masks/multishot flags and descriptor kinds fail explicitly. Pipe endpoint
closure remains outside this model (`cmd/racer-dataplane/tests/support/simulation.rs:1274`).
Four focused contracts passed in parent integration: bounded-socket wake/delayed
CQ, pipe-pressure wake, FIN/reset/close readiness, and rejection/cancellation
(`cmd/racer-dataplane/tests/support/simulation_contracts.rs:177`, `:266`, `:339`, `:455`).

## Reduction and witness reports

`report` includes typed transition counts, distinct armed/effective/released
faults, peak simultaneous effective faults, and publication during two effective
faults. These are observations from the journal, not inferred from scenario
names. The report is diagnostic; `replay` performs checksum and terminal
validation. A terminal record alone does not prove journal integrity.

```sh
python3 dst/run.py reduce dst/artifacts/failure --artifacts dst/artifacts/reduced \
  --timeout 300 --max-candidates 64
```

Reduction first verifies the original exact replay, then tries coarse-to-fine
action deletion, actor removal, smaller node counts, HTTP-only transport, fixed
phase ordering, FIFO callbacks, smaller socket capacities, shorter peer-notification
delays, smaller turn counts and wall offsets, fewer crash-persisted sectors, and
smaller `/sized/N/...` objects with fresh recordings. Domain seeds
and mutant identity stay fixed. The adapter validates every proposal, including
node references and actor topology requirements. It accepts a candidate only if it reaches
the same named product oracle, preserves the source's typed path witnesses, and
its new complete journal passes a separate exact replay. Witnesses retain node,
worker, incarnation, response status, mutant identity, and observed fault target.
The `causal-subsequence-sized-target-v2` policy retains every `Invoke` referenced
by a later witness. Request IDs normalize in involved-admission order, preserving
method, target, available range fields, overlap cohort membership/order, and links
to response, cancellation, and process loss. Unreferenced admissions may disappear;
canceling a different caller cannot substitute for the original cancellation.
Ticks are omitted and fault IDs normalize to their armed scope. The original observations
must occur as an ordered subsequence, including every repetition; candidates may
add observations between them. This conservative contract can retain incidental path
observations, but cannot drop a recorded mutant activation. A size reduction is
the only authorized target substitution: one smaller `/sized/N` prefix is applied
consistently to requests, gates, and durability actions without changing the key
suffix, methods, ranges, endpoints, seeds, or other configuration, or merging
objects. Input validation authorizes that mapping before it is applied to typed
witness target fields; cause strings and all other fields remain exact. Short
canonical gate keys must retain their owner. Only typed observations present in
the source can be required.
Invalid scenarios, arbitrary panics, unrelated oracle failures,
timeouts, and passing candidates are rejected. `reduction.json` records every
attempt and dimension, the versioned witness policy and ordered requirements,
witness preservation, original and remaining inputs, the
accepted bundle, and budget exhaustion. Duplicate proposals are not rerun.
Executables are hard-linked inside candidate bundles to limit disk growth, so source and destination must
share a filesystem. Each bundle retains its executable hash.

Arbitrary actor-internal reduction remains outside the supported dimensions;
schedule-prefix lengths and sized workload objects can now shrink. Exact replay
is never reused as a shrinking mode. Node-count proposals do
not rewrite caller identities or weaken the source's path requirements. An
unchanged minimum is a valid result.

Implementation: `dst/run.py:1263` (proposals), `dst/run.py:1368` (size mapping),
`dst/run.py:1424` (causal signatures), and `dst/run.py:1641` (acceptance).
`dst/test_runner.py:516` asserts rejection of changed causes, caller links, oracle
IDs, and failed replay. Artifact-backed reduction is now verified:
`dst/artifacts/size-reduction-source` intentionally fails `response.status`, and
`dst/artifacts/size-reduction-result/reduction.json:187-243` records three actions
reduced to one GET and a 4096-byte object reduced to zero bytes. The retained
causal `Invoke`/`Response` link and mutant activation remain required (`:295-333`).
Candidates 0000, 0002, and 0004 each passed a fresh exact replay (their
`replay-result.json:54-57`); candidates that removed the failure were rejected.
This fixture establishes real size/action reduction with caller identity
preservation. It has no overlap cohort, so cohort-order/cancellation sensitivity
remains the separately tested runner contract. The earlier Python run passed 49
tests; parent independently verified the latest 54 tests passing in 1.481 seconds
with `timeout 20s python3 -B -m unittest discover -s dst`. Runtime lifecycle/campaign
verification has also passed.

`dst/scenarios/reduction-multidimensional.json` is an intentional named ownership
failure with removable setup. Recording it with `run --scenario artifact --input`
and reducing the resulting bundle exercises action, node-count, phase-policy,
and delay simplification. Removing its required actor produces a passing run and
must be rejected by the reducer.

## Shared-process simulator identities

Simulator workers now have `(node, incarnation, worker)` identities. Entropy,
callbacks, and scheduler fingerprints distinguish workers; replay protection is
shared by all workers in a process incarnation. Restart fences all its workers.
Listener groups select among current members through a journaled scheduler
choice. Closing one member preserves other members and replacement incarnations.
The conformance suite exercises admission to both workers, shared nonce rejection,
restart fencing, and old-listener cleanup after replacement. The existing cluster
takeover fixture still uses explicit worker endpoints and separate machine slots;
it does not yet claim shared-process cluster coverage. Model version 2 records
these identity and listener changes. Older bundles replay with their retained
executable.

## Required artifact campaigns

```sh
python3 dst/run.py campaign --tier pr --artifacts dst/artifacts/required
python3 dst/run.py campaign --tier nightly --seed 71 --samples 8 \
  --timeout 600 --artifacts dst/artifacts/nightly
```

`scenarios/campaign.json` defines 34 required cells: requests, permuted
RDMA phases, RDMA recovery, delayed peer failures, checkpoint crash,
simultaneous faults with live publication, namespace publication, environment
policies, and paired controls and mutants for status, namespace authority,
checkpoint barrier ordering, flight cancellation accounting, local attribution,
session confirmation admission, and zero-copy retirement. The required generated
lifecycle cell joins the HTTP reset, half-close, and in-flight wall-expiry cells
(`dst/scenarios/campaign.json:4-7`). All 34 passed their gates and exact replay in
`dst/artifacts/composition-v3-integration`, together with eight mixed samples.
Every cell requires a complete fresh-process exact
replay and observed transition minima. Each mutant also requires its control to
pass and the exact named oracle to fail. Missing witnesses are `unexercised` and
fail the campaign. `campaign-result.json` retains record outcomes separately from
gate results; an expected mutant failure is never presented as a passing product
execution. The resolved campaign manifest records all sampled seeds.

Default nightly sampling (`--sampler templates-v2`) cycles through all
expected-pass scenario templates in stable ID order,
excluding paired controls and intentional mutants, which still run in the required
matrix. With at least twenty samples the current twenty templates each receive a
new seed. Fixture-based samples replace all six named seed domains using the
versioned `dst/nightly/v2` SHA-256 derivation; generated scenarios receive a new
root seed and the Rust adapter resolves its named domains. Both the template ID
and resolved inputs are retained. Sampling preserves each template's topology,
actor constraints, and witness minima. It does not count unimplemented overlap cells.
The runner builds once, hard-links its retained binary among cell bundles, and
runs sequentially under the same aggregate memory cap and a campaign host
deadline. Run the baseline PR groups separately for conformance and regressions;
artifact campaign gates supplement those groups. Native and scale coverage remain
separate from these managed campaign results.

### Bounded generated composition

```sh
python3 dst/run.py campaign --tier nightly --sampler composition-v2 \
  --seed 19 --samples 8 --timeout 600 --artifacts dst/artifacts/composition
```

This opt-in sampler appends generated cells to the full required matrix. It
records its version, generation seed/index, six resolved named seeds, bounds,
window dimensions, and complete action inputs (`dst/run.py:871`). It samples
2-4 nodes, HTTP/RDMA, 4/16/64 KiB socket queues, Fixed/Permuted phases, and
Fifo/ReadyBatch callbacks. Each input has 1-4 fault windows, at most six live
request admissions and 80 actions, and 1-4 speculative turns per window followed
by an explicit `AwaitOverlap` coordinator barrier bounded to 500 ticks.

Each window holds a cold exact target on a direct canonical-owner peer edge,
admits at least two GETs and one HEAD, awaits real gate interception, steps a
bounded wall offset, waits at `AwaitOverlap` for the entire accepted/live cohort,
then cancels one caller, restores the wall offset, and releases. The barrier
advances only normal coordinator turns: no fabricated acceptance, forced
readiness, drain, or deadline renewal (`cmd/racer-dataplane/tests/runtime/cluster.rs:2352`).
There is no drain inside the held window. Release retains the adapter's settle
and cold-owner healing probes; mixed-key GET/HEAD traffic then drains before the
next window. Object lengths sample zero through 4 MiB + 1, including page
boundaries; held targets are at most 4096 bytes and larger objects use short
boundary ranges to fit the response destination.

Even a short external range can require a full upstream cache page. The
`page-fill-deadline-floor-v1` capacity policy therefore resolves queues to at
least 64 KiB whenever a selected object is 4 MiB - 1 bytes or larger. Smaller
objects retain 4/16/64 KiB sampling. This keeps the success profile feasible with
the unchanged production deadline and modeled 1-3 ms I/O delays, while a full
page still spans 64 queue-sized chunks. It does not relax response/error oracles.
The manifest retains sampled capacity, resolved floor, and configuration-only
pressure ratios; those ratios are not evidence of executed backpressure
(`dst/run.py:941`, `cmd/racer-dataplane/tests/support/simulation.rs:1066`).

Certification requires one `action_fault_overlap` per window, effective/released
faults and cancellation, plus successful GET and HEAD counts. Rust emits
`ActionFaultOverlap` only after a held peer `Request` gate is hit and the entire
unambiguous post-arm target cohort contains at least two accepted, still-live
callers in the source incarnation. Admissions alone, refusal gates, origin gates,
ambiguous `/` accepts, and a partly retired cohort cannot certify it. Acceptance
state resets on release (`tests/runtime/cluster.rs:2756`, relative to
`cmd/racer-dataplane/`). The runner checks live request IDs, exact armed target,
effective fault, distinct endpoints, and boundary/phase before counting it once
per gate (`dst/run.py:312`). Missing evidence is `unexercised`, even when many
actions executed. `AwaitGate` or a fixed turn count alone cannot establish the
full-cohort barrier.

This is a bounded generated grammar, not arbitrary actor composition. It does
not generate reload, restart, storage faults, or simultaneous independent action
gates; authored required actors retain those declared overlaps. Every accepted
cell still requires fresh-process exact replay. The scheduled workflow now
configures shard 0 with `templates-v2` and shard 1 with `composition-v3`
(`.github/workflows/racer-dst-nightly.yaml:25-33`, `:62`). Parent integration reports
workflow parsing and `bash -n` passing; hosted execution has not been observed.
The earlier v2 verification at root
seed 19 passed eight generated cells and the then-required 33 cells in
`dst/artifacts/composition-budgeted`. The earlier `composition-integration`
(33/41 exercised; five unexercised and three simulator failures) and
`composition-accepted` (38/41 exercised; three simulator failures) bundles remain
retained failures, not passes superseded in place. See their `campaign-result.json`
files and the [verification snapshot](MIGRATION.md#verification-snapshot).

### Generated lifecycle composition (locally verified)

```sh
python3 dst/run.py campaign --tier nightly --sampler composition-v3 \
  --seed 19 --samples 8 --timeout 600 --artifacts dst/artifacts/lifecycle
```

`composition-v3` appends a bounded mix to the required matrix: even sample slots
retain the exact `composition-v2` action input at index slot/2; odd slots derive
six world seed streams and a separate actor seed for `generated_lifecycle`.
Eight samples therefore contain four v2 action cells and four lifecycle cells.
Inputs select `generated_lifecycle: {"seed": 71, "rounds": 3}` as an optional
actor, require exactly two HTTP-only nodes and 1-3 rounds, and reject mixing with
another actor. The sampler varies 4/16/64 KiB queues and phase/callback policies;
it does not accept prior template weights (`dst/run.py:1080-1119`,
`cmd/racer-dataplane/tests/runtime/cluster.rs:98-139`). Follow-up actions still
run after the actor, while generated lifecycle samples supply an empty list.

Each seeded round varies the crash node, first admission source, 2-3 held callers
per node, 257/4095/4096-byte objects, and non-prefix persistence stride. Setup
first witnesses an independently durable object. The cooperative actor then holds
two distinct opposite-direction peer Request gates and requires every GET/HEAD
cohort member to be accepted and remain live. It publishes topology revisions on
both production workers without changing the cache namespace, observes actual
activation, and completes independent healthy traffic while both faults remain
effective. Data sync is held; a real data-written checkpoint, at least three
dirty sectors, and a 32-tick observation window precede the selected non-prefix
crash. The crash window has a 2500-tick bound and advances only through normal
coordinator turns (`cmd/racer-dataplane/tests/runtime/actors.rs:70-278`, `:281-365`).

At the crash cut, surviving callers are explicitly canceled and crashed callers
are recorded as process losses. After restart and gate release, the retained GET
must recover through local disk/splice with the restarted owner's origin disabled
and no retained-target origin execution anywhere. The other node's unrelated
origin work is permitted. Origin service is then restored and two cold peer
probes must reach their original owners with exact successful responses. Setup,
post-crash probes, and inter-round boundaries may drain/quiesce; the held overlap
window does not (`cmd/racer-dataplane/tests/runtime/actors.rs:239-461`).

Storage is explicitly bounded at **128/192/256 MiB per node for 1/2/3 rounds**:
`(16 + 16 * rounds) * 4 MiB` sparse slab geometry reserves checkpoint-retained
payload extents plus index/recovery headroom while sync is stalled. The actor
asserts the budget on both nodes, records `disk_bytes` in `LifecycleRoundPlanned`,
and checks that restart preserves it. Sparse geometry is not eagerly allocated
resident memory (`cmd/racer-dataplane/tests/runtime/cluster.rs:1721-1730`,
`cmd/racer-dataplane/tests/runtime/actors.rs:281-307`, `:366-370`). An earlier
three-round attempt exhausted the old six-extent disk; that remains a failed
attempt reported by parent integration. The revised budget subsequently passed
the lifecycle tests and v3 campaign, including the three-round sampled cell
(`dst/artifacts/composition-v3-integration/campaign-result.json:1073-1113`).

Per-round gates require ordered causal evidence, not just transition totals:
accepted live cohorts tied to effective faults and Invokes, actual publication,
healthy responses, dirty crash, correct cancellation/process loss, a retained
GET in the restarted incarnation, released faults, and both cold recoveries.
`lifecycle_round_certified` must equal the requested rounds, and every cell must
freshly exact-replay (`dst/run.py:399-540`). The three focused lifecycle tests
(input validation, overlap/recovery, and barrier-order mutant) passed and are
included in the final 120-execution baseline. The v3 campaign passed 42/42 gates
and exact replays with nine certified rounds: one in the required cell and eight
across four generated lifecycle cells. The required manifest now includes the
lifecycle cell, and nightly shard 1 selects v3 (`dst/scenarios/campaign.json:4`,
`.github/workflows/racer-dst-nightly.yaml:25-33`). This closes the scoped bounded
generated-overlap implementation and local verification gate; it does not claim
exhaustive combinations or hosted execution.

`report <campaign-directory>` reports planned, feasible, attempted, and exercised
cells, aggregate typed transitions, and each unmet cell's missing transition
counts and replay status. Feasible means the named adapter and input file are
available; it does not certify runtime prerequisites. Exercised means the entire
per-cell gate passed, including expected outcome, witnesses, paired control when
required, and exact replay. Unattempted cells remain in the denominator after a
deadline or infrastructure failure. Aggregate witnesses never satisfy another
cell's missing obligations.

The existing Racer CI job runs both the bounded PR baseline and required artifact
matrix. Each runner process tree executes inside a system service capped at
23,000,000,000 bytes with no swap and a 600-second outer deadline. The matrix has
its own 540-second deadline. CI uploads a tar archive even after failure; this
preserves executable permissions and shared executable hard links for replay.
The native kernel and cross-language CI steps continue separately in that job.

`.github/workflows/racer-dst-nightly.yaml` is configured for daily and manual runs.
Two independent jobs each repeat the required matrix and add 24 sampled cells:
shard 0 samples `templates-v2`, and shard 1 generates mixed `composition-v3` workloads.
Root seeds derive from the workflow run ID and shard; rerunning an attempt retains
the same seeds, while a new run gets a new sweep. Each job has its own 23 GB,
no-swap process-tree cap, 900-second campaign budget, and 960-second service
deadline. Jobs retain their bundles independently even if the other shard fails.
The workflow uploads hard-link-preserving archives for seven days and reports
coverage gaps in its summary. Failure reduction remains an explicit bounded
command against the retained bundle.

## Required native kernel capability

The native baseline command, `python3 dst/run.py run --profile native --artifacts
<directory>`, first runs the exact production io_uring kernel contract with
`RACER_REQUIRE_URING=1`. `capabilities.json` records the kernel, architecture,
allowed CPUs, locked-memory limits, available scratch bytes, and probe result.
A ring setup failure or environmental skip fails this required tier as
`infrastructure_failure`; later contract assertion failures remain product
failures. Unattempted suites remain in the planned denominator. Provider coverage
is explicitly `not_requested`: this probe establishes kernel execution, not an
RDMA device, Soft-RoCE, or cross-language integration result.
The empty-selector native fallback excludes tests assigned to other inventory
suites, including the managed PR groups. Parent libtest totals are checked after
any nested subprocess summaries; a passing child cannot stand in for its parent.

## Live namespace overlap

`run --scenario overlap-namespace` holds an owner-to-origin request while three
GET callers and one HEAD caller share a cold key. Production flight joining and
all four old-revision server acceptances must execute before publication. Both
nodes activate a new cache generation while the gate remains held; one caller
cancels, the three survivors retain their admission times and must complete with
the existing strict response oracle. A later same-key GET must fetch from the
origin again, proving the old flight did not populate the new namespace. The PR
artifact campaign requires these witnesses and a fresh-process exact replay.

This fixture checks namespace separation through independent origin-hit counts;
the existing namespace lifecycle suite also checks differing backend bytes under
equal ETags. Admission timestamps are retained by the caller deadline checks;
they are distinct from server acceptance observations.

The test-only `StaleNamespaceSelection` mutant selects a still-draining generation
for a fresh unrouted request at the production generation-selection boundary.
The `namespace.authority` oracle independently expects revision 2 after witnessed
activation and checks actual server acceptance. Its paired campaign requires
activation, the targeted mutant transition, four successful responses, the named
authority failure, and exact replay. This checks a configuration-selection fence;
process and session fences require their own negative controls.

For disk-constrained builds, set `CARGO_INCREMENTAL=0 CARGO_PROFILE_TEST_DEBUG=0
CARGO_PROFILE_DEV_DEBUG=0` on the runner. These overrides are recorded in build
metadata; debug assertions remain enabled.

## Dirty checkpoint crash

`overlap-checkpoint-crash` first witnesses metadata and payload durability for
one object. It holds subsequent simulated sync effects, observes the production
allocator reaching its data-written checkpoint transition, and crashes with
dirty sectors present. The selected persistence set omits the lowest dirty
sector while retaining later sectors, so it cannot be an address-ordered prefix.
After restart, both origins are disabled; the retained object must return exact
bytes through file-backed splice. Typed durability, crash, and recovery witnesses
are mandatory in the campaign and exact replay.

The sync hold is a disk-scoped environmental fault cleared by crash. It does not
change direct setup sync calls. This cell checks recovery of an explicitly
witnessed object; the separate allocator recovery suite retains its independent
both-root and exhaustive root-sector checks.

The `SkipCheckpointDataSync` negative control bypasses the real allocator's
data-sync transition before it submits a sync ticket. The crash actor observes
completed checkpoint-root writes and rejects any while data sync remains held,
using the named `durability.barrier-order` oracle. It leaves a bounded 32-tick
observation window before the nonprefix crash. The paired control must recover
successfully; the mutant must activate, fail this exact oracle, and reproduce in
a fresh process. This tests barrier ordering rather than declaring an ordinary
GET durable or expecting every partial checkpoint to corrupt recovered bytes.

## Flight cancellation negative control

The flight cancellation pair directly acquires two production `NetworkFlight`
leases from a cluster worker's real pool, elects a producer, and parks a joiner.
Dropping the producer must retire its lease and permit survivor takeover. The
test-only `SkipCanceledFlightAccounting` mutant skips the consumer decrement
on an unfinished shared flight while still releasing Rust ownership and wakers.
The independent pool snapshot compares live strong references with declared
consumer leases and reports `ownership.flight-leases` on disagreement.

Both cases require joined-flight and producer-cancellation witnesses and exact
fresh-process replay; the negative case additionally requires mutant activation
and the named failure. This is component cancellation coverage inside the cluster
adapter, not evidence that an HTTP client cancellation has retired a server task
or its kernel I/O. The live HTTP overlap and terminal resource checks remain
separate coverage.

## Local attribution negative control

The attribution pair submits five captured local outcomes to the production
`Origin::error` adapter: local pressure, caller deadline, cancellation, breaker
rejection, and a connection failure before initiation. Each must leave the real
breaker able to admit another request. The control then submits an initiated
connection failure and requires rejection, ensuring the adapter still records
remote failure and retires its permit.

`LocalFailureAsRemote` changes the adapter's local-outcome permit retirement into
a failure completion. The `attribution.local-health` oracle checks subsequent
admission rather than reimplementing the adapter's classification. The campaign
requires typed input witnesses, mutant activation, a passing paired control,
the exact named failure, and fresh-process replay. This is component attribution
coverage with captured attempt evidence; existing full-stack attribution tests
remain responsible for verifying where transport evidence originates.

## Unconfirmed session negative control

The confirmation pair uses the production authenticated handshake and simulated
NIC. After Ready, the responder has received no Confirm and must reject an
application request with `WouldBlock`. The test-only
`UnconfirmedSessionAdmission` mutant bypasses the awaiting-confirmation request
guard. `session.confirmation-admission` detects the resulting premature admission.
The paired control then delivers Confirm and ACK, admits an application request,
verifies its remote metadata, and shuts down both transports with no retained
resources. Campaign gates require the before/after witnesses, mutant activation,
the named failure, and fresh-process replay.

This is component session-admission coverage. Confirmation loss, queue pressure,
and reload overlap are separate paths; the existing confirmation regressions
remain required and are not replaced by this pair.

## Premature zero-copy retirement negative control

The zero-copy pair drives the production request completion transition with a
successful, failed, or canceled SEND_ZC primary completion carrying `MORE`.
An abandoned request must retain its real pool buffer until the notification.
`PrematureZcRetirement` reports the primary completion as terminal, causing the
fixture to retire that request early. The independent `ownership.zc-notification`
oracle attempts to allocate the only pool slot and detects premature reuse.
The passing control also checks preservation of the primary result, ownership
through the terminal transition, and complete pool recovery after retirement.

This component fixture submits no kernel SQE, so the mutant cannot cause unsafe
DMA access. The campaign requires all three primary/notification pairs for the
control, activation and the named failure for the mutant, and exact replay of
both. Existing real-ring and managed SEND_ZC regressions remain separate coverage.

## Permuted cluster phases

Resolved inputs can select `phase_policy: "Permuted"` (the default is `"Fixed"`).
Each turn chooses a journaled permutation of CQ delivery, RDMA effects, and ready
workers, and chooses among ready workers by stable identity. Each phase runs once
per turn, bounding scheduler starvation. CQ delivery snapshots only completions
queued before that turn: a new effect cannot manufacture a same-turn completion,
regardless of the chosen order. Driver readiness and existing per-QP ordering,
pool ownership, byte, routing, and deadline checks still apply.

The required `permuted-rdma` cell records actual RDMA read effects and successful
responses and must replay in a fresh process. This is bounded phase permutation,
not a unified event scheduler: timer advancement remains at the start of a turn
and peer-failure notification uses the separately configured delay policy.

## RDMA corruption and session recovery

The required `rdma-recovery` cell uses eight production dataplane nodes and
permuted phases. A real READ destination is corrupted while independent local
traffic is admitted. Strict response bytes remain checked. The cell requires
same-edge HTTP fallback with no owner-candidate advancement, retirement of the
failed authenticated QP, authentication of a different QP on that edge, and a
fresh READ initiated and served by the replacement endpoints. Typed corruption,
fallback, and replacement-read witnesses gate fresh-process replay.

Recovery begins after quiescence and the existing cooldown interval. This cell
does not claim reload overlap or delayed peer-failure detection; those policies
are separate from this corruption and replacement path.

## Delayed peer-failure notifications

Resolved inputs can set `peer_failure_delay` to 0 through 1000 virtual ticks.
Zero preserves immediate notification. A positive delay queues failed reciprocal
QP notifications, including notifications caused by process reboot, independently
of the current live-pair registry. Each queued notification retains the exact QP
and destination process incarnation. Duplicate notifications for that session
are coalesced; delivery never precedes its recorded due tick, and an incarnation
change discards the notification without touching the replacement process.
Quiescence requires this queue to drain.

The required `delayed-peer-failure` cell repeats corruption, same-edge fallback,
and authenticated replacement with a 17-tick delay. Scheduled and delivered
notifications are typed journal witnesses and the complete run must replay.
The focused contract also covers reboot before delivery and duplicate scheduling.
This models delayed failure notification, not delayed detection of the original
corrupt data by the receiving production state machine.

## Pending storage versions

The simulator disk has an opt-in, bounded pending-version policy for storage
conformance and the checkpoint actor. `track_versions(limit)` must start at a
clean durability boundary and permits at most 65,536 pending sector versions.
Each write records its resulting sector value, including prior partial writes; punch records a
hole. `crash_versions` chooses an ordered prefix independently for each sector.
Zero or omission retains its durable value. Successful sync commits the latest
values and clears pending history, so subsequent crashes cannot select a value
older than that barrier. Invalid selections reject before mutation, and budget
exhaustion is an infrastructure error rather than silent history truncation.

Contracts enumerate every combination of two sectors with three overlapping
writes, check partial-write composition, old hole/new write outcomes, sync
floors, invalid selections, and budget recovery. Mutable file-backed splice
pages retain their existing live references. Default cluster fixtures still use
the selected-sector/prefix policy. Additive allocator transcript adapters now
compare logical effects and serialized physical operations; their limits and
retirement gates are documented in [MIGRATION.md](MIGRATION.md).

`Disk::persist_sectors` propagates selected dirty sectors' latest bytes or holes
without completing a sync or changing live page references. It validates unique,
in-range indices and rejects armed crash policies before mutation. Propagated
sectors establish new durable floors and retire only their own pending versions;
later writes start new histories above those floors
(`cmd/racer-dataplane/tests/support/simulation.rs:1818`). The opt-in logical
partial-sync parity profile uses this seam for failed alternating-sector syncs.
This is explicit partial propagation, not a successful sync completion.

The required `checkpoint-versions` cell enables tracking after the actor's real
durability witness. While the next checkpoint's data sync is held, it selects
alternating durable floors and middle pending prefixes. `SectorVersionCrash`
records the concrete sector/version pairs and pending-version count. Selection
only arms the policy; the normal reboot retires the old driver before applying
the crash. An intervening sync is rejected as harness misuse. Recovery must still
serve the durably witnessed object from disk with both origins disabled, and the
entire journal must replay in a fresh process. The independent sector contracts
cover older overlapping versions even when a cluster seed produces only one
pending version per selected sector.

## Confirmation and namespace reload overlap

The required `confirmation-reload` cell starts real authenticated negotiation
without prewarming sessions. Its link policy holds posted Confirm/ConfirmAck
SEND effects (kinds 5/6), retaining the normal DMA and completion ownership.
Only after an actual post hits the hold does the actor publish a new namespace
on both nodes. It waits for both runtime generations to activate while the hold
remains armed, then releases confirmation, establishes current sessions, and
requires a successful cold GET with a real RDMA read. Typed observations gate
each transition and the complete journal must replay in a fresh process.

This cell exercises delayed confirmation across reload. Confirmation loss and
queue pressure retain their separate component regressions. The hold does not
fabricate a completion or declare an unconfirmed session usable.

## Explicit actor selection and follow-up workloads

Resolved inputs reject unknown fields and multiple actor flags. Each actor owns
its documented concurrent workload and fault composition; `checkpoint_versions`
is a checkpoint policy modifier and requires that actor. This prevents a second
requested actor from being silently skipped by dispatch priority.

Supplied `actions` execute in order after the selected actor returns. Generated
actor inputs contain an empty action list. Each completed action emits
`ActionExecuted`; admission and response observations still verify the actual
request path. The required `actor-follow-up` cell performs namespace overlap
followed by an additional GET and drain, requiring both action observations and
five responses before fresh-process replay passes. Follow-up actions are
sequential, not evidence of overlap with the earlier actor. Reduction excludes
action indices from its path signature so deleting an irrelevant action does
not pin the original numbering.

## Production workers in one process

The required `shared-workers` cell runs two production drivers with worker IDs
0 and 1 in process 0, using the same incarnation, publication source, crypto pool,
NUMA buffer/flight registry, and reuse-port listener address. Their caches and
disks are worker-local. Only worker 0 owns the origin listener. Eight cold callers
must be accepted across both workers while a peer request is held. The shared
flight must join, both workers must activate the same publication with all eight
callers still live, and every response must pass the independent byte oracle.
The owner serves exactly one metadata response and one page for these callers.

Worker 1 then shuts down without restarting the process. A fresh request must
complete through the remaining listener. Worker acceptance, shared activation,
retirement, and response witnesses gate exact replay. Legacy `add_worker`
fixtures retain their separate-machine identities; this cell explicitly models
shared process resources rather than changing those fixtures' routing contracts.

## Ready callback scheduling

`callback_policy: "ReadyBatch"` selects from at most 64 due compute callbacks
in stable due-time/identity order. Every surviving callback in that batch runs
before later ready work can enter the next batch. Choices include callback IDs,
due times, and process/worker incarnations in their enabled-set fingerprint.
Callbacks remain nonrecursive, retired processes are fenced, and callbacks that
are not due cannot execute. Process-local shutdown drains keep their explicit
retirement order. The default policy remains `Fifo`.

The required `ready-callbacks` cell combines this policy with cross-domain phase
permutations, production requests, and an observed RDMA read. Fresh-process replay
checks each dispatch observation and the terminal outcome. A conformance test
also verifies the batch boundary when a callback creates more ready work.

## Routing oracle capabilities

`tests/runtime/oracles.rs` declares five routing obligations independently:
healthy recovery, HTTP graph membership, HTTP rank, RDMA rank, and origin
placement. Canonical artifact campaigns enable all five independent checks.
The targeted runtime/security fixture constructor declares a reason, named
replacement, scope, and independence level for each altered obligation. Those
declarations are typed history records, included in the semantic digest and
complete journal. Changing fixture resource sizing cannot silently change the
oracle selection.

Existing targeted assertions remain at their owning call sites. A test-owned
`Placement`/`RouteScope` model now independently derives owner hashing, shortest
digit-word paths, noncontiguous co-location shortcuts, physical hops, worker
aliases, and origin placement without production Routing/Cursor/Topology helpers
(`cmd/racer-dataplane/tests/runtime/oracles.rs:9`). Selected scopes cover B03
stopped-owner, idle-close, relay/local-failure and co-located candidate-cap cases,
all-local admission, and shared-NUMA takeover. B02 exported placements also use
the model but remain opt-in with `B02_EXPORT`
(`cmd/racer-dataplane/tests/runtime/scenarios.rs:111`, `:378`, `:604`, `:749`, `:1636`).

Cold scopes require origin, logical-arrival, and physical-hop witnesses; cached
scopes forbid origin and peer work, and failed scopes forbid origin access.
Declared candidates obey the existing three-attempt cap. Candidate transitions
consume a reachable predecessor within one observation node/worker/incarnation.
Observations lack request/cursor IDs: concurrent chains are matched existentially
and transports against the declared route set, not paired per request. Lost
scope markers fail closed (`cmd/racer-dataplane/tests/runtime/oracles.rs:208`).
These checks strengthen selected scopes, not every custom fixture or every
oracle-capability replacement. Physical RDMA evidence elsewhere remains labeled
as such; independent logical checks do not imply complete-journal migration.
Byte, dependency, deadline, and resource checks have no disabling capability in
this interface. A conformance test verifies that replacing HTTP rank leaves the
other four canonical obligations enabled and rejects undocumented replacements.

See [migration boundaries and retirement gates](MIGRATION.md) for retained
fixtures, outstanding model limits, and the verification snapshot.

## Scale and format gate

```sh
python3 dst/run.py run --profile scale --artifacts dst/artifacts/scale
```

`scenarios/scale.json` fixes the measured topology at 16 nodes and explicitly
opts in to two exact ignored selectors. It also runs current-binary rejection
of an incompatible `RACERS03` slab without modification and bounded,
versioned peer-descriptor checks. No old executable or automatic migration is
implied. The retained libtest executable, hash, resolved manifest, logs, wall
time, memory ceiling, and cumulative child high-water RSS are recorded. RSS is
not an aggregate process-tree peak; the cgroup enforces the aggregate ceiling.

The initial wave failure exposed a stale fixture contract: production peer
metadata uses a storage-free HTTP exchange (`Provider::start_metadata`), while
payloads use negotiated RDMA. With the approved fixture correction, each cold
relay hop must perform exactly one HTTP metadata exchange, no HTTP payload
exchange, and at least one payload RDMA read. Independent route, byte, deadline,
and ownership checks remain active. The historical scale snapshot records all
four gates passing at 16 nodes and seed 19. This does not claim a successful 1024-node run, deployment coverage, or
complete-journal replay for these legacy entries.

## Wall-clock authentication and monotonic replay retention

The `wall-authentication` required cell calls production signing, verification,
and nonce admission directly. An unchanged signed request is accepted at receiver
offsets of plus/minus 60 seconds and rejected at plus/minus 61 seconds. Every wall
step leaves monotonic time unchanged. After nonce admission, plus/minus one-day
wall steps cannot retire it; it remains rejected at 120 monotonic seconds and
expires at 121. The stale signed request is still rejected after that expiry.
Typed boundary observations and a complete fresh-process replay gate the cell.
This component contract does not claim concurrent HTTP request or key-rotation
coverage.

The separate required `http-wall-expiry` actor drives signed page requests
through production HTTP client/server handling. Before authentication, a real
request-send gate holds the request while receiver wall offsets of +62 and -62
seconds cross the validity window. Each case requires unsigned empty 400,
unchanged cache-admission metrics, and no origin request. Restoring the offset
must let the exact same signature and nonce succeed with verified response
signature and exact bytes. A third gate holds the origin request after actual
authentication: a +62-second step must still permit its signed successful
response. Independent healthy traffic must complete during each fault; wall
changes preserve monotonic time and the actor's original bounded deadlines
(`cmd/racer-dataplane/tests/runtime/actors.rs:900`). Typed expiry, same-nonce
recovery, and admitted-flight witnesses gate exact replay. Key rotation and
delayed Subscriber/control delivery remain outside this actor's boundary.

## Directional FIN and reset policy

The `stream-policies` environment cell half-closes a stream with queued bytes:
the receiver drains those bytes before EOF, reverse traffic remains writable,
and new writes from the closed direction fail with `EPIPE`. Reset discards queued
bytes in both directions and retains `ECONNRESET` for reads and `EPIPE` for writes
until descriptor retirement. This persistent-error behavior is an explicit
simulator policy, not a claim that every kernel consumes errors identically.
Rejected splice operations preserve their pipe bytes and page references.
The required cell records both transitions and exact-replays the contract in a
fresh process. It exercises the environment directly; HTTP reset recovery is
covered by separate required actors below.

The `http-stream-reset` and `http-stream-half-close` actors use two HTTP-only
nodes and an external simulated socket connected to the production listener.
They require server acceptance and a hit owner-to-origin request gate before
resetting the connection or half-closing the caller's write direction. Independent healthy
traffic must complete while that gate remains held. After release, FIN requires
an exact 200 response, one correct Content-Length, independent payload bytes,
and EOF. Reset requires server connection retirement while the reset descriptor
is still alive, rather than obtaining cleanup by dropping the caller. Both
require retirement within the original monotonic budget, quiescence, and a
successful same-target request on a new connection
(`cmd/racer-dataplane/tests/runtime/actors.rs:645`, `:676`).

These are full production HTTP recovery paths above simulated streams, not
native TCP validation or every post-header disconnect phase. Their required
typed in-flight, healthy-progress, retirement, and recovery witnesses and fresh
exact replay passed in `dst/artifacts/composition-budgeted`. Parent integration
also reports all three focused HTTP actor tests passing; the full five-group PR
baseline has since passed 120 executions in `dst/artifacts/lifecycle-final-baseline`.

## Shared-process crash

`shared-process-crash.json` extends the shared-worker actor with a process crash
while eight joined callers remain held after both workers activate publication.
Both production drivers retire before any further coordinator turn, the old
shared pool must recover every lease and slot, and all eight callers are recorded
as process losses. The new incarnation must bind the listener and serve a fresh
request through the independent response oracle. Worker-local disks retain their
completed barriers and discard uncommitted sectors. Recovery reconstructs both
workers in the new incarnation. Two eight-request batches must be accepted by
both listener members, share one metadata/page fetch per batch, and select the
expected revisions. Both workers acknowledge a new publication while the first
batch remains held. Retiring worker 1 must leave the listener usable.

Publication subscriptions last for the process lifetime. After verifying worker
retirement, both shared-worker fixtures explicitly restart into a single-worker
process before returning to follow-up actions. The crash fixture then reloads
configuration and serves another independently checked request; this avoids
treating listener retirement as publication unsubscription.

## Host disk admission

DST execution requires at least 10 GiB available on each checked storage
destination before work starts. Use `--min-free-disk-bytes` to set a different
positive threshold and repeat `--disk-path` for additional storage destinations,
including Cargo config-file overrides. The runner checks artifact storage, Rust
scratch storage, and build destinations inferred from environment variables.
Evidence is retained in `disk-capacity.jsonl`; failed admission is an
`infrastructure_failure` and does not count as an attempted test.

This is admission headroom, not a disk reservation. Post-execution low space
alone does not reclassify assertions. Completed execution outcomes remain counted
when diagnostic writes fail, with the reporting failure recorded separately.
The runner does not automatically clean up artifacts. If storage cannot retain
even the failure result, structured evidence is emitted to stderr.
