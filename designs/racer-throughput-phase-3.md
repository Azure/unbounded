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

Implementation and exact verification results will be recorded as work completes.
