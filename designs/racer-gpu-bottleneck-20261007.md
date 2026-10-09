# Racer GPU bottleneck investigation, October 7, 2026

## Conclusion

The warm concurrency-one run spent most measured pull time in failed requests,
not useful transfers: **95.5% on gpu-07-03 and 89.1% on gpu-07-13**. This is a
request-progress problem to investigate before a capacity test. It is not evidence
that the hardware can deliver only the reported goodput.

There is a **proven local lost-wake defect** in native RDMA preparation and connect
submission under mailbox contention. The tested implementation matches the
deployed source in the relevant trees. However, **no evidence yet proves that
this defect caused the live canary deadlines**. The live failure records identify
the enclosing candidate exchange, not the operation stalled inside it.

The experimental fix was rejected and fully withdrawn. There is no approved fix
and no rollout. Keep load at C0 and keep the repaired raw disk fenced out until
the raw layout is durable and a reviewed progress fix is validated. Evidence and
limits supporting these decisions follow.

## Evidence map and scope

Paths below use these prefixes:

- `S/`: `cmd/racer-dataplane/`, source at baseline `d7403af96`.
- `E/`: original checkout's `tmp/racer-gpu-bottleneck-20261007-artifacts/`.
- `B/`: original checkout's older, unchanged
  `tmp/racer-gpu-benchmark-20261007-artifacts/`.

The deployed tag is `4bb2327cb5ef9a2ce317473f7b634f4b22893cd0`; the dataplane
digest and unchanged pod identities are recorded in
`E/live-bottleneck-evidence.md:13-27`. A direct comparison of that known revision
with baseline found no changes under `S/src`, `S/runtime`, or `S/verbs`
(`E/source-comparison.txt:1-3`). This is source equality for those trees, not a
claim that every build input or runtime schedule is identical. No git history
was read.

The read-only refresh was collected at 20:32-20:34 UTC while load was paused.
It is not a new benchmark or an active-window utilization sample
(`E/live-bottleneck-evidence.md:1-11,49-82`). This report adds no production code,
cluster changes, disk operations, or load changes.

## Where the measured time went

Warm before-to-drained loadgen histogram deltas:

| Node | Failed pulls | Failed pull seconds | Successful pulls | Successful pull seconds | Failed share of measured pull time |
|---|---:|---:|---:|---:|---:|
| gpu-07-03 | 2 | 60.046 | 37 | 2.858 | 95.5% |
| gpu-07-13 | 2 | 60.002 | 32 | 7.371 | 89.1% |

The share is `error duration / (error duration + success duration)`, not a share
of the full wall-clock interval. Sources:
`B/benchmark/warm-c1-before-lg-gpu-07-03.prom:166-167`,
`B/benchmark/warm-c1-drained-lg-gpu-07-03.prom:166-182`, and the corresponding
node13 before/drained files at `:166-182`. Node03 had no error series before this
run. Recomputed values are in `E/derived-measurements.txt:1-15`.

The four failed pulls each consumed about 30 seconds. Retained candidate records
are `Page/Exchange/Cancelled`; matching terminal records show `NextSlice` or
`FirstSlice` deadline failures, including partial deliveries of 16 or 32 MiB out
of 64 MiB (`E/live-bottleneck-evidence.md:94-105`). In code, `CandidateExchange`
records the result of the whole peer request, after cancellation handling. It
does not label prepare, connect, response, receive, or fence substages
(`S/src/read/candidates.rs:1183-1240`). Thus `CandidateExchange Cancelled` is an
observed outcome, not a diagnosis of the stalled substage. Remaining budget
`attempts=7, links=12` is not a count of seven observed attempts.

## Proven defect: mailbox contention can leave a child asleep

The implementation has a gap between returning `Pending` and arranging a retry:

1. `try_mailbox` returns `Pending` on `WouldBlock`, without registering a wake
   (`S/verbs/src/lib.rs:1922-1931`). Its comment explicitly requires timer polling.
2. Preparation can take that path in `QueuePairHandle::poll_new`; connect can
   take it in `poll_submit`, before a command is queued
   (`S/verbs/src/lib.rs:673-740,752-760`).
3. Their callers use `poll_scoped`, which registers cancellation and checks the
   scope but creates no retry timer (`S/src/rdma.rs:192-205,273-297`;
   `S/runtime/src/drivers.rs:456-472`). A registered driver/slot waiter alone does
   not guarantee notification when a contended mailbox is released.

The existing mailbox test explicitly polls again after releasing the lock, so
it tests retry behavior but not autonomous wake-driven progress
(`S/src/rdma.rs:1922-1971`). The preserved tests-only patch instead puts the
operation inside `FuturesUnordered`, forces one mailbox collision, then releases
the lock. For both preparation and connect:

- The child is polled once, and no command is queued.
- Eight native/session ticks do not repoll it.
- One explicit child wake causes a second poll and a successful handoff.
- The final assertion requires automatic completion and fails on baseline.

These assertions are in `E/native-lostwake-artifacts/tests-only.patch:51-100`.
Both tests compiled and reached that final assertion: exit 101, two failures,
no timeout (`E/native-lostwake-artifacts/baseline-test-output.txt:1-25` and
`baseline-status.txt:1-10` in the same directory). They are expected-liveness
regressions exposing a defect, not test infrastructure failures. The reproduction
uses simulated native resources and needs no RDMA hardware. Its command and
toolchain are in `E/native-lostwake-artifacts/README.md:19-34`.

This proves a software progress defect under the tested schedule. It does not
show that any particular live request hit that schedule. A request-correlated
substage trace is still missing.

## Other limits and competing explanations

| Evidence | What it supports, and what it does not |
|---|---|
| Warm disk completion means: reads 1.588/1.713 ms, writes 2.232/2.175 ms | No evidence here of sustained disk saturation explaining 30-second waits. These are completed-I/O averages, not tail latency or proof that every pending I/O completed. |
| Crypto page execution means 14.2-14.6 ms; encryption queue means 1.30/1.12 ms, decryption queues about 0.12 ms | No evidence here of a saturated crypto queue explaining the failures. These completed-operation means cannot locate a stalled operation. |
| Exact current dataplane and loadgen cgroups have cumulative `nr_throttled=0` and `throttled_usec=0`; relevant pods have not restarted | These container counters argue against container CPU quota throttling over their lifetimes, including the warm run. They do not rule out ancestor limits, host contention, or CPU saturation. Current CPU totals are not benchmark-window CPU rates. |
| Plaintext `Admission/Overloaded` records: 201,326,592 used of 214,748,364 bytes, requesting 16,777,216 more | Memory admission pressure occurred. Records lack request/attempt IDs, so they do not establish that it caused the failed requests. |
| Admission-final journals are empty with `coverage=fill_only` | Limited diagnostic coverage, not proof of no admission pressure or successful reclamation. |

Disk inputs are `B/benchmark/warm-c1-{before,drained}-gpu-07-{03,13}-hw.txt:129-136`
(excluding protected node03 nvme0n1); crypto inputs are corresponding
`warm-c1-{before,drained}-dp-gpu-07-{03,13}.prom:78-108`. The recomputation is
preserved in `E/preserve-bottleneck-evidence.py` and
`E/derived-measurements.txt:3-15`. Exact cgroup scope, identities, and limitations
are in `E/live-bottleneck-evidence.md:13-20,60-82`; memory records and coverage
are at `:90-105`. The initial host-root cgroup readings were rejected as wrongly
scoped, not attributed to these containers.

Plaintext reclamation uses a fixed clock cut and visits at most 256 entries per
quantum, retaining live owners (`S/src/memory.rs:464-524`). That bounded code
does not prove reclamation progressed for the failed request. Neither memory
pressure nor an empty final journal is causal proof.

There is also a native capacity limit: 2 GiB registered budget divided among ten
workers yields **six slots per worker, fewer than eight configured rails**.
The budget, division, charge, and slot calculation are at
`B/benchmark/final-config.json:24-29`, `S/src/app.rs:2360-2361`,
`S/src/rdma.rs:741-749`, and `S/src/app/native.rs:25-29`. The maximum-page charge
is 32 MiB + 8 KiB (`S/src/rdma.rs:1348-1352`). This limits native capacity; it
does not explain indefinite waiting. Without contention, exhausted slots return
`Overloaded`, not `Pending` (`S/verbs/src/lib.rs:716-720`).

Finally, this was C1 per node, a shared 8 GiB catalog, and a 16-CPU/thread cap
with ten I/O and five crypto workers. Most client bytes were served locally;
client goodput is not RDMA throughput. The prior report records the configuration,
workload, and counter accounting at
`designs/racer-gpu-benchmark-results-20261007.md:53-80,103-128`, backed by
`B/benchmark/final-config.json:24-29,47` and its cited raw snapshots. This was
neither an all-core nor a hardware-capacity test.

## Required remediation and validation

The proposed direction, not an approved implementation, is reliable,
operation-owned retry notification after mailbox unlock, or a bounded real timer
that wakes the actual waiting operation. Cover **all contextless mailbox paths**,
not only prepare/connect: buffer handoffs, bind/write/invalidate submission, and
ticket results also need an explicit progress contract. A parent/native tick is
not a substitute for waking a sleeping child.

Do not force-poll every task or add unconditional self-wakes. Require tests for
multiple waiters, held-lock contention, unrelated busy slots, cancellation,
close/registration races, poisoned locks, waiter saturation, cleanup, and no idle
wakes. Validate cross-thread races and bounded per-turn work as well as liveness.
Preserve cancellation and DMA completion fences.

The rejected prototype passed a subset (46 RDMA tests, 32 verbs unit tests,
20 public API tests, then three focused regressions), but review found missed
wakes, poisoned-lock handling gaps, dependence on later successful native ticks,
premature waiter-cap rejection, unbudgeted scanning, and uncovered handoffs.
Those passes do not establish correctness or acceptable performance
(`E/native-lostwake-checkpoint.md:68-96`;
`E/native-lostwake-artifacts/README.md:3-8`). All prototype and test edits were
withdrawn; patches remain as evidence only
(`E/native-lostwake-checkpoint.md:98-135`). Do not deploy the rejected patch.

Before any later load experiment, add bounded request/attempt-correlated state
for native prepare, submission, completion, receive, and fence waits. Application
gauges do not expose native mailbox state; the current endpoint router has no
dedicated native pending-operation view (`E/live-bottleneck-evidence.md:84-90`).
After review and safety approval, a fresh bounded C1 comparison must show request
progress and explain remaining failures before any concurrency or capacity sweep.

## Safety hold and preserved artifacts

The last read-only refresh found C0 and zero in-flight operations on both nodes,
six plus eight open raw disks, and no node03 nvme5n1 or protected nvme0n1 raw FD.
The repaired target remains excluded. The protected PV remains Bound/Retain with
unchanged UID/resourceVersion; protected-disk write/discard counters remain zero
(`E/live-bottleneck-evidence.md:11,29-47`). This is not a fresh disk-content audit.

Keep C0 and the raw-device fence. The earlier repair cleared false partition
state but did not fix the offset-zero cache layout; reserved prefix/suffix layout
and separately approved cache invalidation remain outstanding
(`designs/racer-gpu-benchmark-results-20261007.md:284-290`). Do not readmit the
disk, replay repair scripts, restart the benchmark, or roll out the prototype.
The parent operation retains responsibility for the paused state and resumption.

`E/` preserves the worktree's investigation notes, checkpoints, collector scripts,
saved live capture, derived measurements, test patch/output/status/checksums, and
the clearly marked rejected prototype. The build target is excluded. The known
six-record final collector transcript is retained verbatim and split into readable
records under `E/live-bottleneck-captures/`; other refresh observations are
preserved in the evidence note, not represented as newly collected raw captures.
Files are restricted local evidence (files 0600, new directories 0700), not commit
attachments. Copy hashes were verified; `E/SHA256SUMS` lists the preserved files,
and the native artifact's original checksums were also verified. No Secret
objects or credential files were collected or committed. The older `B/` evidence
remains unchanged.

Report validation: `GOTOOLCHAIN=go1.26.6 make fmt` passed with zero issues under
an external 300-second TERM timeout; it left source files unchanged.
`git diff --check` passed. The existing regression output was reviewed, not
rerun for this documentation-only change.
