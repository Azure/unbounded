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

## Delivered scope

Implementation commits: `628de6fa` (executable gates and oracle), `3c414650`
(cold-path evidence, churn/configured workloads, and blocked publication).
The plan was committed first as `ffc8fa21`. All work is confined to this
worktree; the parent retains merge ownership.

Files delivered:

- `tests/process/measurement.rs`: bounded HTTP completion oracle and strict
  acceptance accounting, including malformed, corrupt, truncated, empty, and
  unsuccessful responses. No partial bytes count toward goodput.
- `tests/process/throughput.rs`: production-profile workloads, JSON reporting,
  process CPU/RSS/socket observations, actual peer processes, expected baseline
  diagnostics, and configurable measurement parameters.
- `tests/process/control.rs`: reusable multicache/controller publication fixture,
  separate node enrollment bindings under one CA/key domain, and blocked polls.
- `tests/process_restart.rs`: reusable actual executable startup profile and
  variable-length/distinct-key origin. Production-profile budgets are defaults.
  The preexisting restart-only 1 MiB request-context setting was stale against
  `src/app.rs:245-246` and `src/peer/wire.rs:85-89`; it is now 16 MiB. Both
  existing restart tests pass without removing assertions.
- `tests/process/README.md`: exact commands, prerequisites, test semantics,
  measurement limitations, and configurable later-phase gates.

The acceptance matrix covers 1/2/4 pairs; memory, cold disk, origin, and peer
paths; full pages, small objects, long values, short ranges, multicache traffic,
fan-in, and slow readers. Separate diagnostics cover 512-key distinct churn,
connection saturation, and full-page multicache disk failure. Real control
response blocking and failed listener publication preserve last-good traffic
and recover. Two actual Applications exercise authenticated TCP transfer and
fallback after the preferred peer is killed without changing membership.

## Commands and outcomes

Run from `cmd/racer-dataplane/` unless specified otherwise. All commands below
were actually executed on this Linux host, with working sudo/mount namespaces,
io_uring, O_DIRECT, and CPU capacity for four pairs.

| Command | Outcome |
| --- | --- |
| `cargo fmt` and `cargo fmt --check` | Passed. |
| `cargo test --locked --test process_restart measurement -j 2` | Two oracle tests passed; the matching privileged configured-measurement test was explicitly ignored. |
| `cargo test --locked --all-features --no-run -j 2` | All library, binary, and integration targets compiled. |
| `CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n' cargo test --locked --test process_restart -j 2 -- --ignored --test-threads=1` | Eleven selected privileged tests passed, zero ignored in this selected run, including existing restart tests and the final matrix. |
| `cargo test --locked --all-features --lib app::caches::tests -j 2` | Two publication/rendezvous tests passed. |
| `cargo test --locked --all-features --lib app::health::tests -j 2` | Readiness progress test passed. |
| `CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_STRICT_BASELINE=1' cargo test --locked --test process_restart production_multicache_disk_baseline -j 2 -- --ignored --test-threads=1` | Intentionally failed: zero complete responses, one HTTP 503, zero goodput, one server overload. Confirms strict gate rejects the baseline. |
| `CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n' cargo test --locked --release --test process_restart production_configured_measurement -j 2 -- --ignored --test-threads=1 --nocapture` | Passed: four pairs, 32 distinct full-page origin reads, 32/32 complete, 536870912 verified bytes, 44.55 MB/s, p50 376.21 ms, p99 404.07 ms. |
| `CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_MODE=memory' cargo test --locked --release --test process_restart production_configured_measurement -j 2 -- --ignored --test-threads=1 --nocapture` | Passed: four pairs, 32 shared full-page memory reads, 32/32 complete, 536870912 verified bytes, 301.36 MB/s, p50 54.98 ms, p99 61.49 ms; 32 memory hits, zero origin fills. |

An initial cold build exceeded the tool's 120-second command timeout; the retry
completed. Intermediate fixture failures were investigated rather than counted
as passing: Linux includes kernel `iou-wrk-*` tasks in `/proc/PID/task`, so exact
pair verification uses named userspace roles; stale restart context limits failed
startup; long distinct disk preloads omitted persistence; and four-pair churn
intermittently overloaded. The final selected suite above passed after separating
diagnostics from the zero-failure acceptance matrix and tightening path evidence.

## Baseline discoveries and exact limits

1. Full-page multicache disk: four pairs give 64 MiB ciphertext per worker.
   With a second cache's short tail retained on worker zero, a recovered full
   page returns HTTP 503, one overload, zero disk hits, and zero origin fills.
   The tail remains readable. This is a production Application reproduction of
   the reserve/staging overlap identified in the inspected baseline above.
2. Ingress: 32 partial heads occupy worker-zero acceptance at four pairs despite
   the aggregate 128-connection budget. The excess request times out at the
   diagnostic's 200 ms bound while readiness remains 200. Releasing held heads
   restores successful reads. The diagnostic tests this baseline, not improved
   ingress; later phases must update its expected contract when ownership changes.
3. Churn: 512 distinct 113-byte objects across two caches and two clients passed
   at some runs but produced four HTTP 503s at four pairs in another observed run
   (508/512 complete). The diagnostic permits only categorized HTTP 503s, checks
   server-counter consistency, and offers strict zero-failure mode. No claim of
   reliable high-concurrency small-object progress is made.
4. Persistence: even sequential successful long-value reads can omit dirty
   publication. An observed four-pair preload had 12 origin fills but only ten
   disk publications with no pending writes. Page-at-a-time preload also exposed
   omission under accumulated residency. The ordinary long disk case therefore
   explicitly measures one recovered long object transitioning to memory; cold
   distinct single-page disk cases require a disk hit for every measured page.
   Configurable larger distinct disk workloads retain the hard persistence gate.
   The release origin run published eight pages by measurement end for 32 fills;
   response goodput is not persistence throughput.
5. Implemented counters are client completion/error/truncation/content evidence,
   existing aggregate request/hit/overload/write counters, per-thread schedstat
   execution nanoseconds, and before/after RSS/socket FD observations. No
   per-worker reservation-failure, queue-residence, reactor-lag, allocation/copy,
   or ranking-miss counters were added. These are explicitly unavailable in JSON.
   Socket FDs include listeners/control/origin/peer sockets and are not categorized
   connections or peaks. CPU snapshots include small measurement overhead.
6. The bounded fixtures and byte oracle can limit measured goodput. No CPU
   isolation, NUMA hardware scaling, NIC/RDMA throughput, multi-hop peer cluster,
   exhaustive Cartesian workload matrix, or full 100k-node throughput was tested.
   Peer coverage is full-page cold-to-warm transfer and failure fallback on two
   local processes; it is not sustained cold-peer goodput. All production source
   files and budgets remain unchanged. No migration, dependency, or test removal
   was introduced.

Reusable later-phase commands and environment knobs are in
`tests/process/README.md`. Machine-readable measurements from the final runs are
retained in the gitignored worktree-local
`cmd/racer-dataplane/target/throughput-results.jsonl`; they contain no credentials.
Ordinary test ignores are never represented as passed hardware measurements.
