# Racer throughput architecture: Phase 3

## Authority and scope

Implement the complete approved Phase 3 on `racer-throughput-architecture`.
The user-approved plan and `/home/azureuser/design.md:5-32,43-69` are authoritative.
One writer owns this worktree; parent review owns integration and merge.
Retain production aggregate budgets, wire compatibility, deterministic page
ownership, singleflight, and kernel/crypto completion fences.

## Inspected starting point

Paths below are relative to `cmd/racer-dataplane`.

- `src/app.rs:212-252,897-914` partitions resources across workers but starts peer
  acceptance only on the control worker. `src/app_caches.rs:147-171` similarly
  prepares client sockets only there. Streams retain ingress-local delivery pipes
  (`src/read/range_stream.rs:76-95`). Distribute sockets before constructing sessions.
- `src/client/listener.rs:285-362` repeatedly attempts nonblocking acceptance and
  uses `BTreeMap::iter().nth()`. Replace this with readiness-indexed acceptance.
- Phase 2 provides owned total-plus-role `ConnectionReservation`
  (`src/runtime/admission.rs:108-131`) and completion-owned readiness leases
  (`src/runtime/reactor.rs:694-720`). Preserve 32 sockets per worker at four pairs:
  22 ingress, eight outbound, two control.
- `src/read/dispatch.rs:490-503` seals and submits even same-owner range pages,
  although ordinary acquire already has a local branch (`:471-476`). Local range
  work needs an independent admitted context, not an uncharged raw Clone.
- `src/read/drivers.rs:66-83` polls blocked futures every turn;
  `src/read/flight.rs:915-938,1114-1145` sweeps waiters inside each outer entry.
  Completion calls a full sweep (`:1034`). Bound examined inner work and target
  cleanup while preserving deadline/cancellation and accepted-operation fences.
- `src/memory/cache.rs:44-108,111-160` scans the LRU for lookup/publication/reclaim;
  `src/runtime/admission.rs:253-254` scans every cache on each reservation.
- `src/http/io.rs:223-251` allocates the endpoint maximum on every receive;
  `src/http/codec.rs:134-198` allocates parser slots and decoded field storage
  without decoded representation admission. A wire cap alone is insufficient.

## Architecture and implementation actions

1. **Socket ownership and admission.** Keep one owner of canonical UDS paths,
   filesystem prepare/rollback, and the peer listener. Add bounded per-worker
   ingress queues carrying an owned accepted descriptor, target-worker ingress
   reservation, and client cache-generation retirement token (or peer kind).
   Select available workers round-robin and reserve target capacity *before*
   accept; a queued descriptor consumes that same target socket charge until
   installed, rejected, or fenced. Never transfer a session, Rc service graph,
   submitted socket operation, or buffer. Queue shutdown rejects new offers and
   drops queued descriptors; installed work drains on its exclusive reactor.
   Retired generations reject queued/idle clients while accepted responses retain
   their original rights. Control and diagnostics retain one owner.
2. **Readiness and backpressure.** Index real UDS listeners in a level-triggered
   readiness set; wait on its descriptor through `readiness_with_lease`, retaining
   the readiness owner through CQE cancellation. Bound ready events and accepts
   per turn. Do not poll every idle cache or reserve one socket per idle listener.
   Capacity exhaustion suspends acceptance until a target reservation is released.
   Keep deterministic simulated acceptance as an explicit backend with indexed
   traversal; cover real readiness in executable tests.
3. **Local range acquisition.** Add a scope-checked, pre-admitted local context
   owner to the credential service. It copies validated sensitive fields only
   after admission, zeroizes them on destruction, and retains its charge through
   local acquisition. `start_page` selects this only when the directory's installed
   owner matches; remote work retains the existing encrypted envelope/mailbox.
   Both paths conserve independently partitioned credits and original deadlines.
4. **Runnable and bounded work.** Use task-specific wakes for acquisition drivers
   so parked futures are not polled each service turn. Preserve worker wake
   propagation, child submission, simulation crash cleanup, and queue permits.
   Make flight completion cleanup targeted; use indexed/cursor-based waiter
   maintenance with explicit examined-entry limits and deadline wake behavior.
   Bound synchronous receive/parse/reclaim work by bytes or entries as applicable.
5. **Memory and admission indexes.** Replace linear page lookup/LRU movement with
   indexed page and version records and an ordered recency index. Reclaim stops
   when enough bytes are released and bounds examined busy entries. Maintain
   cache activity incrementally through reservation release notifications so
   admission no longer scans all classes of all caches. Completion-thread releases
   remain safe; no new quota bypass, revocation, or global budget increase.
6. **HTTP heads.** Grow owned receive staging incrementally only after the previous
   receive CQE fences. Admission precedes allocation, including overlap while
   growing. Count fields and compute checked decoded/parser storage bounds before
   allocating either; retain decoded charges for the decoded head's lifetime.
   Bound field count in a way that preserves legal maximum client and peer heads.
   Reuse bounded parser scratch or remove dynamically sized parser slots. Preserve
   exact opaque values, duplicates, framing rejection, and zeroization. Data-plane
   buffer pooling remains Phase 4; bounded parsing is completed here.

## Incremental commits and exit tests

Commit this design first, then tested implementation increments. Before every
commit inspect `git status`, `git diff`, and `git log --oneline -10`; stage intended
files only. Run `cargo fmt` for Rust; `make fmt` if Go changes are necessary.
Update prior baseline prose and assertions to require the approved behavior.
Never delete tests, permit new acceptance errors, or label ignores as passes.

Every test, benchmark, and script invocation must use external
`timeout --signal=TERM --kill-after=10s 300s ...` (shorter allowed), tool timeout
<=320000 ms; Go additionally uses `-timeout=5m`. Split suites. Investigate a
timeout as failure and clean up owned child processes; never retry unbounded.
Temporary files and build artifacts stay in this worktree.

- Focused tests: target connection ceilings, queued/installed ownership, cache
  removal/replacement, saturation/recovery, shutdown and CQE fences; local context
  independent admission/cancellation; blocked/woken driver counts; indexed LRU
  busy protection and churn; allocation-before-parse rejection and legal max heads.
- Actual Application tests at 1/2/4 pairs: aggregate accepted connections must use
  all worker partitions, reserve outbound/control progress, and recover after
  saturation. Verify simultaneous delivery uses aggregate pipe capacity.
- Phase 1 production executable strict gates: restarts, origin/memory/disk matrix,
  authenticated peer/failure, blocked control/publication, strict multicache disk,
  strict churn, plus readiness/cache-count/fan-in fairness and cancellation cleanup.
- All-feature Rust library groups (split DST if needed), production integration,
  client/origin conformance, and doctests. Phase 2 baseline was 733 library passes,
  seven ignores; 11 strict process passes; 13 production passes/two ignores;
  19 conformance passes/one ignore; 31 doctest passes.
- Release measurements using the actual Phase 1 executable harness, at 1/2/4
  pairs with fixed budgets and comparable parallel workloads. Report verified
  completed bytes, latency, errors, per-thread CPU and available ownership counters.
  Compare honestly with historical four-pair 32-page memory 301.36 MB/s and origin
  44.55 MB/s. Fixture or same-host CPU limits are observations, never a hardware
  scaling promise. Record RDMA/NUMA/NIC hardware gaps explicitly.

## Results

### Commits and delivered interfaces

- `45b97d08`: this design, committed before implementation.
- `7b491e57`: bounded pre-session ingress and readiness-indexed UDS acceptance.
- `00d652b8`: same-owner contexts, runnable acquisitions/commands, indexed cache
  lookup/LRU and incremental admission, targeted flight completion cleanup.
- `144eb79b`: incremental HTTP receive staging and pre-allocation decoded charges.
- `57e69642`: runnable clients, aggregate delivery/fairness gates, corrected DST
  corruption selection, ownership gauge, and integration documentation.

All source paths below are relative to `cmd/racer-dataplane`.

- `runtime::ingress::Ingress::reserve/pop/close`
  (`src/runtime/ingress.rs:86-136`) uses target admission and round-robin bounded
  queues. `ConnectionAdmission` shares only socket counters/stopped state;
  `Admission::reserve_connection(IngressConnection)` uses the same implementation
  (`src/runtime/admission.rs:189`). `ConnectionLease::from_reserved` installs the
  owned charge. No session or submitted operation crosses reactors. The single
  canonical listener owner remains responsible for filesystem transactions.
- `src/client/readiness.rs` uses level-triggered epoll with at most 64 ready events
  and one completion-owned `readiness_with_lease` wait. Weak listener references
  avoid delaying canonical-path cleanup. Simulation keeps indexed ordered
  traversal without host epoll; actual executable tests cover distributed ingress.
- `CredentialCrypto::local_context` (`src/security/credentials.rs:113`) validates,
  reserves, then copies independently zeroizing fields. Same-owner `start_page`
  (`src/read/dispatch.rs:491`) bypasses sealing/mailbox; Fill's retained acquisition
  driver also uses this admitted local context. Remote envelopes remain encrypted.
- Acquisition drivers, directory commands, and client operations use task-specific
  runnable flags. Clients track changing idle/operation deadlines and force one
  deadline/cancellation poll while retaining completion ownership. Flights use
  ordered incarnation/waiter cursors and deadline indexes; refresh handles at most
  64 expired waiters, and completion removes only its quiescent flight
  (`src/read/flight.rs:927,1042,1166`). Terminal cohort notification still visits its
  configured bounded waiter cohort; it does not sweep unrelated flights.
- Memory pages/versions are hash-indexed, recency is ordered, and reclaim examines
  at most 256 entries with a persistent cursor (`src/memory/cache.rs:55`). Busy
  prefixes cannot permanently hide later idle pages. Cache reservation records
  use weak indexed entries, an atomic active count, and completion-thread retirement
  notifications, draining at most 256 notifications per reservation.
- Receive staging starts at 4096 bytes, grows only after receive completion, and
  charges both allocations during growth (`src/http/io.rs:234,318`). Before decoding,
  checked field count and representation size are admitted (`:260`); the returned
  HeadCompletion retains that charge alongside the decoded value. Parser scratch
  for start-line validation is constant; field descriptors and exact strings are
  bounded by the precomputed charge. Legal maximum wire heads and maximum legal
  field counts remain supported (`src/http/codec.rs:112,355`). Sending still uses
  existing bounded maximum staging; pooled data buffers remain Phase 4.
- `racer_active_deliveries` is an ownership gauge on ReaderLease, including reactor
  retention. The new actual-Application gates exercise all 16 aggregate pipes at
  1/2/4 pairs and 128-cache acceptance with 64 partial heads
  (`tests/process/throughput.rs:558,619`).

### Exact final checks

Commands ran from `cmd/racer-dataplane`. Every test/measurement invocation used
`timeout --signal=TERM --kill-after=10s 300s`; tool limits were <=320000 ms. No
external timeout fired. No Go files changed. `cargo fmt` and `cargo fmt --check`
passed. No tests were removed or acceptance failures permitted.

| Command after the external timeout prefix | Final result |
| --- | --- |
| `cargo test --locked --all-features --lib -j 2 -- --test-threads=2 --quiet` | 739 passed, seven explicit ignores, zero failures; 56.82 s. Includes all three DST tests. |
| `cargo test --locked --all-features --test production_dataplane --test client_origin_conformance -j 2 -- --test-threads=2 --quiet` | Conformance 19 passed/one ignore (0.24 s); production 13 passed/two ignores (5.94 s). |
| `cargo test --locked --all-features --no-run -j 2` | All library, binary and integration targets compiled. |
| `cargo test --locked --all-features --doc -j 2 -- --test-threads=2` | Six ordinary and 25 compile-fail doctests passed. |
| `env CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_STRICT_BASELINE=1' cargo test --locked --test process_restart -j 2 -- --ignored --test-threads=1 --quiet` | All 13 selected process gates passed, zero failures/ignores; 108.14 s. Includes strict churn/disk, restarts, peers, blocked publication/control, aggregate ingress/delivery, and idle-cache fairness. |

Focused runs also passed: 28 client-listener tests; ten application integration
tests; seven directory-dispatch tests; 107 read tests before later additions;
target admission/queued-descriptor lifecycle; HTTP field-allocation rejection;
independent local sensitive contexts; busy-prefix reclamation; and parked-driver
poll counting. The final library run supersedes these intermediate counts.

Initial failures were investigated and corrected: strong readiness references
delayed socket unlink; simulation detection used the seeded-clock flag rather than
the concrete simulation backend; old HTTP allocation assertions omitted newly
retained decoded charges; direct test mutation of waiter deadlines needed to update
the deadline index; gated test futures failed to notify their stored wakers; old
poll-count assertions expected repeated polls of blocked futures. The tests now
assert quiet parking and explicit wake propagation as well as their original
deadline, completion, and cleanup guarantees. DST's corruption probe selected
retired-key/cache records and random nonoverlapping ranges; it now selects an
available record and requests that exact page. Exact replay and the original
required corruption coverage both pass.

### Release measurements

Every row uses actual Application listeners, default resource budgets, 16 MiB
pages, verified complete bodies, and zero response failures. Decimal MB/s.
JSON remains in worktree-local `target/throughput-results.jsonl`.

Measurement command, following the mandatory timeout prefix:

```sh
env CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_MODE=memory RACER_THROUGHPUT_PAIRS=1 RACER_THROUGHPUT_REQUESTS=128 RACER_THROUGHPUT_CONCURRENCY=16' cargo test --locked --release --test process_restart production_configured_measurement -j 2 -- --ignored --test-threads=1 --nocapture
```

The table lists exact substitutions for MODE/PAIRS/REQUESTS/CONCURRENCY.

| Build/workload | Pairs | Requests / concurrency | MB/s | p50 / p99 ms |
| --- | ---: | ---: | ---: | ---: |
| Final memory | 1 | 128 / 16 | 2341.70 | 107.30 / 143.08 |
| Final memory, first two-pair run | 2 | 128 / 16 | 85.73 | 549.82 / 12774.81 |
| Final memory, bounded investigation follow-up | 2 | 128 / 16 | 2520.84 | 100.25 / 131.05 |
| Final memory | 4 | 128 / 16 | 2540.28 | 97.44 / 141.96 |
| Final memory, Phase 1 parameters | 4 | 32 / 1 | 318.92 | 52.56 / 53.38 |
| Final origin, Phase 1 parameters | 4 | 32 / 1 | 44.86 | 371.13 / 387.34 |
| Before final client runnable refinement, memory | 1 | 32 / 4 | 1212.06 | 53.90 / 55.97 |
| Same intermediate build, memory | 2 | 32 / 4 | 1136.23 | 55.45 / 65.41 |
| Same intermediate build, memory | 4 | 32 / 4 | 1083.36 | 54.56 / 85.61 |
| Same intermediate build, origin | 1 | 32 / 4 | 161.87 | 403.60 / 512.75 |
| Same intermediate build, origin | 2 | 32 / 4 | 158.21 | 403.50 / 518.01 |
| Same intermediate build, origin | 4 | 32 / 4 | 161.81 | 397.41 / 457.77 |
| Same intermediate build, memory | 1 | 128 / 16 | 1901.82 | 135.45 / 176.08 |
| Same intermediate build, memory | 2 | 128 / 16 | 2359.15 | 105.56 / 144.88 |
| Same intermediate build, memory | 4 | 128 / 16 | 2664.23 | 94.79 / 125.78 |

For Phase 1-parameter rows the runner omitted REQUESTS/CONCURRENCY (defaults
32/1); origin also omitted MODE (default origin). Historical Phase 1 values were
301.36 MB/s memory and 44.55 MB/s origin. These are same-host observations, not
statistical regressions or hardware ceilings. The 16-client runs demonstrate
parallel memory goodput improvement while retaining all 16 pipes. Four-client
runs do not show throughput scaling, and origin remains fixture-limited.

The final two-pair outlier completed 128/128 responses over 25.05 s. It was not
discarded. Afterward `ps` showed no remaining cargo/rustc/dataplane processes and
host load averages were 3.59/4.71/6.14; a bounded follow-up completed in 0.852 s.
The cause is unproven. Therefore these measurements do not establish stable
production p99 performance on this shared host. CPU observations show useful
delivery work across all I/O workers; scheduling contention or fixture effects
remain possible explanations, not demonstrated causes.

### Handoff limits and integration

Parent owns review and merge. No dependencies, generated files, migrations, Go
sources, or external scratch files were changed. The worktree uses existing real
io_uring/O_DIRECT/root fixtures, with child guards stopping/reaping processes.
Production protocol and aggregate socket/pipe limits remain unchanged. Simulator
tests exercise the original concrete simulated socket backend; distributed
descriptor queues and epoll are covered by actual-Application process gates.

Hardware gaps: no RDMA/NIC throughput, NUMA isolation, multi-host saturation, or
large sustained persistence benchmark. Origin response goodput is not disk
persistence throughput (the final 32-page origin run published eight pages by
measurement end). Queue residence, exact allocation/copy counters and exact
resource peaks remain unavailable. The shared-host latency outlier above needs
isolated-host follow-up; it is not hidden by a weakened acceptance gate.
