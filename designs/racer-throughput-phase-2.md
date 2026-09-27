# Racer throughput architecture: Phase 2

## Authority and acceptance

This phase implements the approved progress and bounded-hot-path changes on
`racer-throughput-architecture`. The user-approved scope and canonical
`/home/azureuser/design.md:5-32,43-69` authorize cross-package changes: bounded
worker-local resources, shared fills, alternate routes, rotation, and circuit
breakers. Phase 1 is committed at `62ff37b5`. Parent integration owns merging.
No dependencies, destructive migrations, or test removals are planned.

## Inspected evidence

Paths below are relative to `cmd/racer-dataplane`.

- `src/read/fill.rs:487-513` reserves a generic network fill before local lookup;
  `src/store/reader.rs:110-155` separately owns padded staging and decoded bytes.
  `src/runtime/admission.rs:123-133` permits only a single-page fairness floor.
  These conflict with the required complete progress working set. Resolve in
  favor of source-specific admission, retaining global bounds.
- `src/memory/pool.rs:63-104` retains upper-bound reservations after exact-sized
  allocation. Its test at lines 231-248 explicitly expects an eight-byte charge
  for three bytes. Preserve lifetime/provenance assertions but require exact
  allocated-capacity accounting instead.
- `src/control/client.rs:362-365` bypasses all remote progress while publication
  is pending; lines 464-487 clone and validate the full publication repeatedly.
  Resolve in favor of independent bounded duties and prepare-once publication.
- Phase 1's strict disk diagnostic intentionally fails
  (`designs/racer-throughput-phase-1.md:145,159-163`). Convert its permanent
  assertion to successful verified disk progress, keeping the strict command.

## Reviewable implementation sequence

Mandatory execution and handoff rule (including all future agents): every test
command uses external `timeout --signal=TERM --kill-after=10s 300s ...`, or a
shorter reasonable bound; Bash tool timeout is at most 320000 ms. Go also uses
`-timeout=5m`. Split suites into bounded groups, including scripts/benchmarks and
`make fmt` if it invokes testing. Investigate any timeout as a failure and ensure
child process cleanup; never retry unbounded. The previous Phase 2 invocation
was canceled during a stuck test; its uncommitted implementation is preserved
and reviewed by the replacement phase owner.

1. Commit this plan before source changes.
2. Admission: select pending/disk/network sources before reserving; atomically
   acquire each source's overlapping plaintext, staging, decoded ciphertext and
   optional dirty obligations before asynchronous I/O. Size the bounded fairness
   floor for the complete working set, without increasing global budgets.
   Shrink only uniquely owned reservations to actual live allocation capacity.
   Verify rollback, invalid shrink, multiple-cache progress, and small objects.
3. Connections: provide explicit ingress/outbound/control classes and bounded
   reserved progress capacity. Transfer charge ownership to socket/kernel
   operation lifetime, including cancellation. Expose this API for Phase 3's
   ingress distribution.
4. Failure progress: share worker-local LinkHealth between routing and actual
   immediate-hop transfers; report transport outcomes, bound half-open probes,
   prune retired members, and keep application misses/401/403 separate. Preserve
   request deadline, attempt/hop accounting and cancellation fences. Apply bounded
   endpoint breakers to origin and control. Add actual failed-hop alternate-route
   coverage rather than only candidate fallback.
5. Control: service projection reload, renewal, pending installation and polling
   independently with bounded turns/backoff. Retain immutable prepared pending
   state; newer authenticated publications may supersede blocked ones. Test key
   rotation and supersession while listener preparation is blocked.
6. Security/hot paths: validate immutable identity/trust epochs outside locks;
   retain signature/session checks; cache exact peer chains against trust epoch
   and time validity with bounded capacity. Use indexed key lookup where useful.
   Avoid committed cache-definition clones and poll health periodically.
7. Peer pools: binary-search membership endpoints, autonomously expire idle
   connections with bounded per-turn work. Correct related 24-hour activation
   cadence if the control implementation requires it; run `make fmt` for Go.
8. Verification and final documentation: `cargo fmt`, applicable focused tests,
   all-feature compilation/tests, explicitly selected real io_uring executable
   suite, strict multicache disk gate and strict small-object churn. Record exact
   commands/results, proven properties, Phase 3 APIs, and concrete blockers.

Before each incremental commit inspect `git status`, `git diff`, and
`git log --oneline -10`; stage only intended files. All scratch/build artifacts
stay in this worktree. Final status must be clean. Tests that formerly encoded
baseline failures must assert the fixes. Hardware/prerequisite failures are
reported as failures or blockers, never successful acceptance.

## Results

Implementation commits:

- `d0703fc8`: predecessor's source-specific disk admission and exact allocation charges.
- `3b330e16`: reviewed connection partitions, endpoint circuits, routing observation,
  indexed endpoint lookup, and autonomous pool expiration.
- `a28ec206`: 24-hour activation cadence including preparation and corrected
  overlapping-generation capacity reservation.
- `23e1b454`: independent control duties, immutable prepared publication,
  validated security epochs, periodic health, and regression coverage.

### Delivered behavior and Phase 3 interfaces

Paths below are relative to `cmd/racer-dataplane` unless prefixed `internal/`.

- `src/read/fill.rs:487-539` chooses pending/disk before the network reservation;
  `src/store/reader.rs:109-117,156-160` reserves simultaneous padded staging and
  decoded ciphertext before disk submission. Plaintext is reserved before that
  asynchronous work. `src/runtime/admission.rs:182-194` funds the complete disk
  fairness floor while enforcing the original global ceiling. `Reservation::split`
  and `shrink` (`:52-82`) transfer/release uniquely owned charges; BufferPool uses
  actual allocation capacity. Tiny payloads no longer retain full-page charges.
- `Admission::reserve_connection(role)` (`src/runtime/admission.rs:116-131`)
  returns an owned total-plus-role reservation. At four production pairs each
  worker's unchanged 32 sockets split into 22 ingress, eight outbound, two control
  (`:168-175`). `ConnectionLease::from_accepted` charges ingress; HttpPool charges
  outbound; TLS control charges control. `Reactor::readiness_with_lease`
  (`src/runtime/reactor.rs:697-721`) retains the charge with the descriptor through
  CQE fences. Phase 3 must use these ownership interfaces for distributed ingress.
- Requester shares Paths' LinkHealth and reports immediate-hop transport outcomes
  (`src/peer/requester.rs:149-173`). Signed application misses/401/403 do not open
  circuits. Owned half-open probes remain exclusive while alive; routing prunes
  former neighbors. Origin and control use the same bounded circuit policy.
  Actual refused-TCP coverage proves the failed immediate edge is excluded and
  selects A-B-C within the original link budget; real two-Application coverage
  proves failed-peer fallback. No new attempts, hops, or deadlines are minted.
- `src/control/client.rs:350-617` drives projection, renewal, polling and pending
  installation independently. Projection/renewal ticks continue during a held
  poll; renewal has its own retry deadline. Polling uses the latest prepared
  cursor so blocked state can be superseded. Pending installation keeps an Rc
  of prepared state, without revalidation/full DTO cloning each service turn.
  `SnapshotStore::prepare/publish_prepared` rechecks current replay/capacity at
  commit and retains immutable membership leases. The new real-TLS integration
  test blocks lifecycle staging, reloads generation two keys, supersedes sequence
  two with three, then restores actual staging and commits only sequence three.
- Identity validity is captured on validation, checked against runtime wall time
  on sign, and cached against the immutable roots epoch. Chain verification is
  outside the keyring lock; refresh checks identity/root pointer equality before
  publishing validation. Key lookup is indexed. Certificates caches at most 64
  exact wire-bounded chains (4 MiB), against root identity, checked time and expiry;
  every message signature is still verified. Tests reject altered signatures,
  changed chains, expired cache entries and replacement trust.
- `src/app_caches.rs:113-130` skips committed/prepared generations before cloning
  definitions. `src/app.rs:1108-1115` observes health every 100 ms. HttpPool's
  ordered cursor expires at most the tick's endpoint budget without checkout or
  waiters (`src/http/pool.rs:498-527`). `src/peer.rs:54` uses Membership's indexed
  lookup. Go activation now schedules preparation at interval minus preparation
  (`internal/racer/rotation.go:171,448-452`), preserving persisted transitions and
  overlap lifetimes; catalog capacity uses the actual activation interval.

### Bounded verification

All commands below were executed in this invocation. Rust commands ran in
`cmd/racer-dataplane`; Go/make commands ran at this worktree's root. Every test
used external TERM/KILL timeout and a tool limit at or below 320000 ms. No external
test timeout fired in this invocation.
Final `ps -C cargo,rustc,racer-dataplane,process_restart-8ab1fa715cd1d141,racer_dataplane-9a60c9fce09db663 -o pid,ppid,stat,comm`
returned no matching processes after executable verification. Fixture guards
stopped/reaped child dataplanes and cleaned their scratch directories.

| Exact command | Result |
| --- | --- |
| `cargo fmt` and `cargo fmt --check` | Passed. |
| `timeout --signal=TERM --kill-after=10s 300s cargo test --locked --all-features --lib -j 2 -- --test-threads=2 --quiet` | Final: 733 passed, seven explicit hardware/benchmark ignores, zero failures; includes all three DST tests. |
| `timeout --signal=TERM --kill-after=10s 300s cargo test --locked --all-features --lib -j 2 -- --test-threads=2 --skip app::dst:: --quiet` | 730 passed, seven ignores after final behavioral changes. |
| `timeout --signal=TERM --kill-after=10s 300s cargo test --locked --all-features --lib app::dst:: -j 2 -- --test-threads=1 --quiet` | Three passed, including exact replay/coverage. |
| `timeout --signal=TERM --kill-after=10s 300s cargo test --locked --all-features --no-run -j 2` | All library, binary, integration targets compiled. |
| `timeout --signal=TERM --kill-after=10s 300s cargo test --locked --all-features --test production_dataplane --test client_origin_conformance -j 2 -- --test-threads=2 --quiet` | Conformance: 19 passed, one ignore. Production initially found three stale allocation/breaker expectations, fixed below. |
| `timeout --signal=TERM --kill-after=10s 300s cargo test --locked --all-features --test production_dataplane -j 2 -- --test-threads=2 --quiet` | Final: 13 passed, two explicit ignores. |
| `timeout --signal=TERM --kill-after=10s 300s cargo test --locked --all-features --doc -j 2 -- --test-threads=2` | Six ordinary and 25 compile-fail contracts passed. |
| `timeout --signal=TERM --kill-after=10s 300s env CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_STRICT_BASELINE=1' cargo test --locked --test process_restart -j 2 -- --ignored --test-threads=1` | Final executable run: all 11 passed, zero ignored/failures, 94.41 seconds test runtime. Includes strict disk, strict 512-object churn at 1/2/4 pairs, restarts, traffic matrix, peer failure and blocked publication. |
| `timeout --signal=TERM --kill-after=10s 300s env CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n' cargo test --locked --test process_restart production_multicache_disk_baseline -j 2 -- --ignored --test-threads=1` | Passed strict permanent disk assertions. |
| `timeout --signal=TERM --kill-after=10s 300s env CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_STRICT_BASELINE=1' cargo test --locked --test process_restart production_churn_diagnostic -j 2 -- --ignored --test-threads=1` | Passed, no allowed overloads in strict mode. |
| `timeout --signal=TERM --kill-after=10s 300s env GOTOOLCHAIN=go1.26.6 make fmt GO_PACKAGE_DIRS=./internal/racer GO_PACKAGE_PATTERNS=./internal/racer/...` | Passed, zero lint issues. |
| `timeout --signal=TERM --kill-after=10s 300s env GOTOOLCHAIN=go1.26.6 go test -timeout=5m ./internal/racer -count=1` | Passed full controller package, 53.666 seconds. |

Initial focused groups also ran with the required wrapper: control (35 passed),
HttpPool (nine passed), peer (30 passed, one ignore), security (56 then 57 passed),
and application integration (ten tests, ultimately covered by the full passing
library group). Early failures were investigated, not counted as acceptance.

### Stuck-test diagnosis and corrected failures

The canceled invocation's precise stuck test/process was not retained in the
handoff, so identifying that exact historical process is not possible. A concrete
predecessor defect was reproduced: its partition used a fixture's neighbor limit
equal to total capacity and left zero ingress. Accepted sockets failed Overloaded
in connection/session tests. Fixtures with waiting peers could then stall. The
partition now reserves a bounded fraction rather than consuming all ingress;
the eight-link fixture explicitly funds both ends of eight nodes' sockets. All
relevant socket/session/fence tests pass. This is a plausible historical hang
cause, not a claim that the canceled process was observed.

Other resolved failures: fixed-page simulator assertions incorrectly charged
three-byte buffers as 16 MiB; pressure fixtures now use real full pages where
byte pressure is required and assert exact short-page charges elsewhere. Origin
fault probes wait past breaker backoff; DST selects eligible current-version
records before requiring corruption coverage. Control renewal tests exposed
cancellation of renewal by fast polls and expired-identity retry storms; the
independent duties retain first-turn completion and renewal retry deadlines.
One malformed cargo command placed `-j` after `--` and was rejected before tests.
Initial Go formatting failed because the installed linter was built with Go 1.26
while ambient Go was 1.27; pinning the module's 1.26.6 toolchain fixed it. Initial
rotation tests caught preparation-equals-interval startup and changed overlap
counts; both are covered by the passing full controller suite.

### Handoff and remaining scope

Mandatory for parent and every future agent: never run tests without external
`timeout --signal=TERM --kill-after=10s 300s` (or shorter), tool timeout <=320000 ms,
and Go `-timeout=5m`. Include scripts/benchmarks and make targets that invoke tests.
Split larger groups; investigate timeout failures and clean up children. Never
retry unbounded. This instruction is also recorded in worktree AGENTS.md.

Phase 2 acceptance is complete at unchanged production caps. Phase 3 still owns
ingress distribution; worker-zero's admitted-ingress ceiling is now 22 at four
pairs, with outbound/control progress reserved. The alternate-route unit test
uses an actual failed TCP connect and verifies route selection, not a successful
three-process relay transfer. Existing real peer/relay and deterministic generated
tests cover transport and relay correctness separately. RDMA hardware performance,
NUMA throughput, new telemetry, larger sustained cold-disk persistence workloads,
and later-phase scheduling/storage architecture are not claimed here. Existing
on-disk rotation schedules complete without destructive migration before adopting
the new cadence. Parent owns eventual integration/merge.
