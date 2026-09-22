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
# Complete journals

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
action deletion with fresh recordings. It accepts a candidate only if it reaches
the same named product oracle and its new complete journal passes a separate
exact replay. Invalid scenarios, arbitrary panics, unrelated oracle failures,
timeouts, and passing candidates are rejected. `reduction.json` records every
attempt, the accepted bundle, and budget exhaustion. Executables are hard-linked
inside candidate bundles to limit disk growth, so source and destination must
share a filesystem. Each bundle retains its executable hash.

Current reduction supports action lists. Actor parameters, topology, fault
parameters, and schedule-prefix minimization remain separate work; exact replay
is never reused as a shrinking mode. An unchanged minimum is a valid result.
# Shared-process simulator identities

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
