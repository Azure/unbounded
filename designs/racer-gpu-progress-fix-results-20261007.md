# Racer progress fix and bounded GPU-node remeasurement

## Result

The native wait progress defect was fixed and deployed. The reviewed tests establish
wake-driven progress for the tested schedules; they do **not** establish that the
old live requests failed for that reason. The new deployment completed C1 and warm
C4 without client errors using isolated file-backed storage. C16 failed under both
2 GiB and 8 GiB plaintext budgets. C8 with 8 GiB plaintext failed after a partial
window. The remaining admission failure has not been attributed to one exact gate.

**The goal of measuring absolute capacity across all available disks and RDMA
resources was not met.** The highest completed zero-error point was warm C4 with
the **2 GiB plaintext budget**, not a maximum sustainable rate:

- node03: 1,787.660 MiB/s verified client goodput.
- node13: 1,910.258 MiB/s.
- Sum: **3,697.918 MiB/s, or 3.611 GiB/s**, not 3.7 GiB/s.

The last read-only refresh at 23:03 UTC confirmed both loadgens at applied C0 and
zero in-flight work. No later load or configuration change was made for this report.

## Fix, validation, and deployment identity

The earlier investigation reproduced a software lost wake under a controlled
schedule: native ticks alone did not wake the sleeping child. That result is
separate from the missing request-correlated proof for the old live incident
(`designs/racer-gpu-bottleneck-20261007.md:83-104`).

The deployed fix retains one operation-owned kernel alarm for each pending native
wait, using the serving worker's reactor. It polls the native operation first,
allocates no alarm for an immediate result, retains the alarm across unrelated
wakes, and bounds the retry interval to one millisecond and the request deadline
(`cmd/racer-dataplane/src/rdma/retry.rs:40-107`). Required fences have a separate
policy; alarm failure cannot claim successful DMA fencing (`:109-145`). The tests
assert immediate-result behavior and exercise retained alarms, wake-gated native
paths, cancellation, admission exhaustion, and kernel completion ownership.

Independent review led to corrections and release-gate tests before approval.
Recorded final validation was **1,108 application tests passed, 6 ignored**, plus
**52 verbs tests passed** (32 unit and 20 public API), all-target RDMA clippy with
warnings denied, Cargo formatting, and required Go formatting. These recorded
results were reviewed, not rerun for this report. See the preserved
`progress-fix-checkpoint.md:19-28,38-74` for commands, assertions, review corrections,
and remaining limits. The parent supplied final independent-review approval.

| Item | Verified value |
| --- | --- |
| Reviewed fix commit | `42730780d324ba77bedf5305460a6e9ad4606060` |
| Integrated build source | `b04d28f91d775519c820027a73291b8c3cbd4fd4` |
| Successful image run | [37694995366](https://github.com/Azure/unbounded/actions/runs/37694995366) |
| Dataplane image | `ghcr.io/azure/racer-dataplane@sha256:466ea41053da31dc11fb0857bad75aea82dbb24958f6e77f8822fe73b6f8c316` |
| Platform | linux/amd64 |

The build's push log and digest output matched that source. Only the dataplane
image changed; controllers, operator, and loadgen retained the old `4bb2327...`
images. ClusterVolume identity and socket paths were preserved. No concurrent
ClusterCache rename or CRD migration was deployed.

## Why storage isolation was required

The raw planner still starts each device at offset zero and binds offsets into its
checkpoint layout digest (`cmd/racer-dataplane/src/app/devices.rs:250-282`). Its
open checks do not perform a signature check (`:90-178`). The earlier false
partition collision was not durably fixed by the approved single-disk repair.
Restarting into raw mode could accept the other 14 selected disks and expose the
same hazard. The repaired node03 nvme5n1, serial S64HNN0XA09741, stayed fenced.

The supported operator override atomically combined the reviewed image with:

```text
RACER_DEVICE_DIRECTORY=/racer-no-raw-devices
RACER_SLAB_DIRECTORY=/var/lib/racer/slabs/progress-fix-20261007
RACER_SLAB_BYTES=1073741824
RACER_SEGMENT_BYTES=67108864
RACER_FREE_SEGMENT_RESERVE=2
```

Missing-root discovery falls back before any device open (`app/devices.rs:47-85`);
file slabs and checkpoints use the separate configured directory
(`src/app.rs:1192-1219,1264-1277`). Runtime absence, file-backed startup logs,
canonical non-symlink storage on ext4 `/dev/sda2`, ten 1 GiB slabs per node, and
**zero raw block-device FDs** were verified before load. Old raw-cache checkpoint
hashes and NVMe write/discard counters were checked throughout.

No NVMe cache disk was used by these measurements. No disk was initialized,
migrated, wiped, or readmitted. The node03 model-cache nvme0n1 and its Bound/Retain
PV were unchanged. Identity stayed at `/var/lib/racer/identity/private`; original
raw cache bytes and top-level checkpoints were left intact.

## Workload and accounting

Both nodes used the same 128-blob, 64 MiB/blob synthetic catalog, seed
`gpu-progress-fileslab-20261007-cold-01`, shuffle selection, UDS clients, and full
verification. Each node had ten I/O and five crypto workers, a 16-CPU limit, and
64 GiB memory limit. This was not an all-core or all-device test.

C1 began with a new seed. The applied-to-end window includes warming after initial
activation, not exclusively cold reads. C4 continued the same caches. The later
plaintext change required a normal rollout, clearing in-memory state while
preserving file slabs and the seed. Comparisons across that rollout have this
cache-state difference in addition to the budget change.

Each phase had a 60-second target, external TERM timeout, 15-second command
heartbeats, storage checks, immediate error abort, and finally-C0 with independent
parent fallback. Actual intervals include scrape overhead. Per-node goodput uses
that node's monotonic scrape-midpoint interval. Percentiles below are histogram
estimates, not exact samples. Client verified bytes, application disk payload,
and whole-port network counters are different measures.

## Completed zero-error measurements

Both completed phases used **2 GiB node-wide plaintext**. Errors remained zero
through the before-to-drained checks, not just the displayed observation window.

| Phase / node | Metric interval s | Verified MiB | Verified MiB/s | Successful pulls | Errors | p50 / p95 / p99 ms |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| C1 / node03 | 64.713 | 47,616 | 735.807 | 744 | 0 | 62 / 361 / 478 |
| C1 / node13 | 64.665 | 50,688 | 783.858 | 792 | 0 | 59 / 249 / 461 |
| Warm C4 / node03 | 64.692 | 115,648 | 1,787.660 | 1,807 | 0 | 247 / 475 / 495 |
| Warm C4 / node13 | 64.695 | 123,584 | 1,910.258 | 1,931 | 0 | 225 / 472 / 494 |

Wall observation spans were 65.532s and 65.621s. C1 returned to affirmed C0/drain
at 22:35:55 UTC; C4 at 22:41:09 UTC. Full before-to-drained C1 totals were
56,448/60,288 MiB, including activation and stopping tails.

### Storage, CPU, and transport in completed windows

| Phase / node | File payload published bytes | File payload read bytes | DP cores | LG cores | Whole-port TX bytes | Whole-port RX bytes |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| C1 / node03 | 16,508,780,544 | 31,406,948,352 | ~0.990 | 0.421 | 2,463,789,636 | 3,541,202,784 |
| C1 / node13 | 16,039,018,496 | 35,097,935,872 | ~1.031 | 0.449 | 3,541,202,784 | 2,463,789,636 |
| C4 / node03 | 25,098,715,136 | 76,906,758,144 | 2.089 | 1.052 | 5,166,611,952 | 4,790,372,304 |
| C4 / node13 | 26,658,996,224 | 81,067,507,712 | 2.143 | 1.135 | 4,807,483,688 | 5,200,825,132 |

C4 file-payload published/read rates were 369.86/1,133.31 MiB/s on node03 and
392.41/1,193.28 on node13. These are application payload counters, not physical
device I/O. Whole-port totals cover mlx5_00 through mlx5_07, excluding bond aliases,
with four-byte data units converted to bytes. They are not exclusive Racer traffic
or RDMA capacity. Client goodput largely reflects local service, not wire traffic.

C1 process CPU used ticks with verified CLK_TCK=100 and nearby metric intervals.
Its initially collected root-cgroup counters were rejected for container
attribution. The node03 broader-root memory-event increment was not treated as
a Racer OOM. C4 and later phases collected the exact nested container scope and
process-adjacent timestamps. C4 exact-container CPU usage was 135.779/139.637s;
throttling and memory-limit/OOM events stayed zero. Memory.current ended at
7,891,619,840/7,842,742,272 bytes, below unchanged 64 GiB limits.

## Failed higher-concurrency experiments

| Experiment | Outcome | New failed pulls node03 / node13 | Valid 60s window? |
| --- | --- | --- | --- |
| C16, plaintext 2 GiB | Abort during activation | 11 incomplete / 14 incomplete + 4 HTTP-status | No |
| C16, plaintext 8 GiB | Abort during activation | 8 incomplete + 9 HTTP-status / 9 incomplete + 2 HTTP-status | No |
| C8, plaintext 8 GiB | Abort after about 37s observation | 2 incomplete / 0 | No |

The one-variable plaintext change used UID/resourceVersion/full-old-data CAS on
`racer-dataplane-config`, changing only 2147483648 to 8589934592. No explicit env
masked the ConfigMap. Runtime environment, ten I/O/five crypto workers, unchanged
override data and all other budgets were verified after normal rollout at C0.
Fresh pre-change exact-container usage was about 8.07/8.14 GB, leaving over 60 GB
headroom per container. The experiment did not make C16 error-free.

C16 at 2 GiB showed plaintext admission rejections at 201,326,592 bytes used of
214,748,364, requesting another 16,777,216, plus pipe contention. At 8 GiB,
retained tails instead included pipe, ciphertext, and dirty-ciphertext pressure.
All terminal failures were `Overloaded` at first-slice or partial-delivery
boundaries, not proof of recurrence of the old lost wake. Exact-container CPU
throttling and memory-limit/OOM events stayed zero.

The C8 partial interval is preserved, not promoted to a completed result:

| Node | Partial interval s | Verified MiB/s | Successes | New errors | p50 / p95 / p99 ms |
| --- | ---: | ---: | ---: | ---: | --- |
| node03 | 37.112 | 4,995.94 | 2,897 | 2 | 69 / 450 / 784 |
| node13 | 37.064 | 6,509.79 | 3,770 | 0 | 66 / 404 / 487 |

The two new node03 terminal pairs failed after 48 MiB of 64 MiB. Earlier node03
entries and all node13 terminal entries were retained C16 history, not new C8
errors. C8 before-to-drained exact-container memory ended at
19,327,631,360/18,887,335,936 bytes, with no throttling or OOM events. No throughput
plateau was measured: error aborts and unequal runtime state prevent that claim.

## Unresolved admission gates and next work

General admission rings lack request IDs, while terminal records carry
`facts=unknown`. Protected fill-final and candidate-final remained empty under
their stated limited coverage. Empty scoped journals are not proof of no pressure.
No exact resource-to-failed-request mapping was established.

Current defaults are 64 flights, 256 queue entries, and 16 pipes node-wide
(`cmd/racer-dataplane/src/config.rs:224-229`). Divided among ten I/O workers, these
give six flights, 25 queue entries, and one pipe per worker. These are possible
limits, not proven rejection sites; division is implemented in
`cmd/racer-dataplane/src/app.rs:2331-2357`. Multiple consumers, retained buffers, and
client release-readiness can compete with native work.

The client path prefers an available slice, but propagates auxiliary readiness
errors when the slice is pending (`src/client.rs:280-295,369-398`). Its tests assert
that behavior, including Overloaded (`src/client/tests.rs:3213-3225`). This is an
alternative for some NextSlice failures, not proof of their cause; initial
FirstSlice acquisition uses a different path (`src/client.rs:328-342`).

The new one-millisecond retry alarms also use ordinary reactor admission
(`src/rdma/retry.rs:49-56`). Kernel admission-exhaustion tests confirm failure and
fence behavior, not cost-free capacity. There is **no evidence that these alarms
caused the live rejections**. No reactor-named metrics were exported to isolate
their live occupancy or cost.

The next useful step is bounded, **request-correlated protected diagnostics** for
client readiness, pipe/flight/queue admission, buffer ownership, and retry-alarm
admission, preserving the original failure and resource facts. Do not keep raising
budgets blindly. Further experiments need separate authorization and must retain
C0 and storage gates. Fixing liveness did not establish error-free high-concurrency
admission or meet the all-disk/RDMA capacity goal.

## Final safe state and evidence preservation

At the final 23:03 UTC check: both nodes were Ready with ten matching workers,
both loadgens applied C0/inflight0, approved image unchanged, plaintext budget
8 GiB, and isolated file slabs active. Node/installation/ClusterVolume identities,
the repaired-disk fence, model PV, raw-cache bytes, and original checkpoint hashes
were preserved. No cleanup, rollback, or further load followed.

Restricted local evidence is preserved at
`tmp/racer-gpu-progress-fix-20261007-artifacts/` in the original workspace. `SHA256SUMS`
records file hashes; directories are 0700 and files 0600. The archive includes all
assigned-worktree ops scripts/snapshots/reports/checkpoints, progress review and
validation checkpoint, build checkpoint, and the rejected mailbox primitive.
Build targets are excluded. `rejected-mailbox-primitive.rs` is **rejected and not
deployed**, not the retained-alarm implementation. No private identity or token
contents were collected.

`ops-deploy-2227-dp-frozen.json` is a preserved **invalid partial JSON artifact**
from local ENOSPC, not successful evidence. The dataplane rollout had completed;
the seed mutation had not yet been sent. Later read-only checks and separately
named complete freeze snapshots recovered the evidence without replaying the
dataplane mutation. The parent's cleanup was limited to the completed build target;
its exact invocation was not supplied to this operational phase.

Primary evidence families in the archive:

- `progress-fix-checkpoint.md`, `build-checkpoint.md`, `ops-fresh-2223-report.md`:
  reviewed code validation, source integration, successful build and digest.
- `ops-deploy-2227-*`, `ops-seed-2231-*`, `ops-plaintext-2249-*`:
  exact guarded configuration changes, rollout state, memory and storage checks.
- `ops-cold-c1-01-*`, `ops-warm-c4-01-*`: complete measured windows and C0 proofs.
- `ops-warm-c16-01-*`, `ops-plaintext-c16-01-*`, `ops-plaintext-c8-01-*`:
  aborted intervals, post-C0 journals, final state and configuration snapshots.
- `ops-checkpoint.md`: exact commands, mutation results, deadlines, errors,
  recovery decisions, and handoff ownership.

The earlier raw benchmark, bottleneck, and approved disk-recovery archives remain
separate and unchanged. This report supersedes neither their historical failures
nor their raw-layout safety warning.
