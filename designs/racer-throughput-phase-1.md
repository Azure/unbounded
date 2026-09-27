# Racer throughput architecture: Phase 1

## Authority and scope

This is the implementation plan for Phase 1 only, on branch
`racer-throughput-architecture`. The approved user instructions and the canonical
`/home/azureuser/design.md` supersede older repository design prose. That design
requires bounded pinned I/O/crypto workers, 16 MiB pages, shared fills, real
client/origin UDS, peer HTTP, and progress under failures (lines 5-32, 43-69).
Phase 1 establishes executable progress and measurement gates. It does not change
admission policy, ingress ownership, storage format, or scheduling policy.

## Inspected baseline

All paths below are relative to `cmd/racer-dataplane/`.

- `src/main.rs:15-21` runs `Application`; `src/app.rs:174-199` discovers CPUs,
  partitions limits, bootstraps credentials, and runs the worker group.
- Defaults are aggregate budgets (`src/config.rs:174-194`), divided across
  workers (`src/app.rs:212-252`). Four pairs have 32 connection charges, four
  pipes, and four relay charges per worker.
- Client preparation and TCP peer listening belong to the control worker
  (`src/app_caches.rs:138-167`, `src/app.rs:895-911`). A production ingress test
  must connect to those listeners, not distribute accepted descriptors itself.
- Acquisition reserves speculative ciphertext before disk lookup
  (`src/read/fill.rs:480-513`); disk staging overlaps decoded ciphertext
  (`src/store/reader.rs:110-155`). The fair-share floor is one page plus tag
  (`src/runtime/admission.rs:123-133`). Two active caches at four pairs expose
  a full-page disk progress deficit despite passing startup validation.
- Service turns prepare caches, poll control, refresh snapshots, and observe
  health (`src/app.rs:1034-1108`). Health parses credentials
  (`src/app_health.rs:83-102`). These costs belong in executable measurements.
- The previous benchmark enlarges budgets and injects round-robin connections
  (`tests/hotpath/bench.rs:108-143,523-530`); acceptance only requires some
  successful reads (`:591`). It remains a component probe, not this gate.
- Real restart tests verify bytes, pinning, disk hits, origin exclusion, and
  shutdown (`tests/process_restart.rs:487-645`). Their fixture and real TLS
  controller (`tests/process/control.rs`) are the reuse point. Application
  integration and DST fixtures were inspected for publication and ownership
  semantics (`src/app_integration_tests.rs:132-340`, `src/app_dst_tests.rs`).
- Existing exported telemetry has fixed aggregate counters/gauges
  (`src/telemetry/metrics.rs:8-107`), not per-worker utilization, queue residence,
  allocation/copy bytes, or ranking misses. Do not relabel those as implemented.

## Implementation sequence and gates

1. Commit this plan before implementation.
2. Add reusable measurement/oracle code with ordinary tests. Count attempted and
   completed responses, completed verified payload bytes, HTTP errors, malformed
   replies, transport failures, and truncations. Partial bytes never contribute
   to goodput. Strict mode requires every scheduled request to finish correctly,
   zero errors/truncations, positive elapsed time, and path evidence. Test the
   oracle on complete, empty, error, truncated, and corrupt input.
3. Extend the existing executable fixture with explicit production-default
   configuration. Keep the original restart profile available. Support exact
   1/2/4 pairs and verify the actual thread count rather than silently accepting
   CPU/quota downsizing. Use worktree-local scratch and child mount namespaces.
   Never override production resource budgets for throughput cases.
4. Add real-listener workloads for memory, recovered disk, origin, and peer
   paths; small objects, full pages, long values, and short ranges; one/multiple
   caches, shared fan-in, distinct-key churn, and slow readers. Exercise the
   control lifecycle during traffic and bounded failure scenarios: saturated
   connections, failed neighbors, and blocked publication. Prove selected paths
   with counter deltas and origin-call evidence, not workload names alone.
5. Report JSON results with completed-response goodput and latency, exact workload
   parameters, process RSS and per-thread CPU observations, aggregate production
   counter deltas, and an explicit list of unavailable instrumentation. Add
   low-overhead measurement-only production counters only where necessary and
   justified; do not alter later-phase product behavior.
6. Establish separate expected-baseline diagnostics for full-page multicache
   disk admission and worker-zero ingress. They must prove the limitation,
   report the observed failure, and test recovery where possible. Ordinary
   acceptance remains zero-failure. An opt-in strict version of a diagnostic
   must fail on the baseline, providing the later-phase improvement gate.
7. Format with `cargo fmt`, compile applicable targets, run oracle tests and
   focused existing lifecycle tests, then explicitly select privileged baseline
   tests if the host supports them. Missing root/mount namespaces, io_uring,
   O_DIRECT, CPU capacity, or hardware is a prerequisite failure or an explicit
   ignore, never a passing benchmark. RDMA hardware throughput is not inferred
   from HTTP or simulated tests.
8. Commit tested increments. Before every commit inspect status, diff, and the
   last ten commits; stage only intended files. Finish with exact delivered
   scope, commands/outcomes, discovered baselines, and remaining gaps here.

## Measurement semantics

Timing includes real connection establishment and response consumption, excludes
fixture construction and preload, and includes slow-reader delays when selected.
Every measured response checks status, framing, pin, range, exact length, and
deterministic content. Report failures alongside goodput, even in diagnostic mode.
Latency percentiles describe complete responses only and are absent when none
complete. CPU utilization is sampled OS CPU time divided by wall time, not a
claim about useful work. RSS is observed process memory, not admission accounting.
Sampling maxima are labeled sampled, not exact peaks. Queue residence, reactor
lag, reservation failures, allocation/copy bytes, connections, and ranking misses
must be explicitly distinguished as implemented, approximated, or unavailable.

## Validation and completion record

Pending implementation. This section will be replaced with actual commands,
results, exact scope, and limitations before the final Phase 1 commit.
