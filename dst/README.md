# Racer dataplane DST campaigns

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
phase ordering, shorter peer-notification delays, smaller turn counts and wall
offsets, and fewer crash-persisted sectors with fresh recordings. Domain seeds
and mutant identity stay fixed. The adapter validates every proposal, including
node references and actor topology requirements. It accepts a candidate only if it reaches
the same named product oracle, preserves the source's typed path witnesses, and
its new complete journal passes a separate exact replay. Witnesses retain node,
worker, incarnation, response status, mutant identity, and observed fault target.
Ticks and allocated request/fault IDs are normalized; repeated identical witnesses
need only occur once. This conservative contract can retain incidental path
observations, but cannot drop a recorded mutant activation or substitute another
fault target. Only typed observations present in the source can be required.
Invalid scenarios, arbitrary panics, unrelated oracle failures,
timeouts, and passing candidates are rejected. `reduction.json` records every
attempt and dimension, witness preservation, original and remaining inputs, the
accepted bundle, and budget exhaustion. Duplicate proposals are not rerun.
Executables are hard-linked inside candidate bundles to limit disk growth, so source and destination must
share a filesystem. Each bundle retains its executable hash.

Actor internals, object sizes, and schedule-prefix minimization remain separate
work; exact replay is never reused as a shrinking mode. Node-count proposals do
not rewrite caller identities or weaken the source's path requirements. An
unchanged minimum is a valid result.

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

`scenarios/campaign.json` defines twenty-three required cells: requests, permuted
RDMA phases, RDMA recovery, delayed peer failures, checkpoint crash,
simultaneous faults with live publication, namespace publication, environment
policies, and paired controls and mutants for status, namespace authority,
checkpoint barrier ordering, flight cancellation accounting, local attribution,
session confirmation admission, and zero-copy retirement.
Every cell requires a complete fresh-process exact
replay and observed transition minima. Each mutant also requires its control to
pass and the exact named oracle to fail. Missing witnesses are `unexercised` and
fail the campaign. `campaign-result.json` retains record outcomes separately from
gate results; an expected mutant failure is never presented as a passing product
execution. The resolved campaign manifest records all sampled seeds.

Nightly sampling extends the two implemented generated cells with bounded,
deterministically derived seeds. It does not count unimplemented overlap cells.
The runner builds once, hard-links its retained binary among cell bundles, and
runs sequentially under the same aggregate memory cap and a campaign host
deadline. Run the baseline PR groups separately for conformance and regressions;
artifact campaign gates supplement those groups. Native and scale coverage remain
separate from these managed campaign results.

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
the selected-sector/prefix policy; allocator-fixture vocabulary unification and
shared operation transcripts remain separate work.

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
