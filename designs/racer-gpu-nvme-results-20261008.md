# Racer GPU-node guarded NVMe migration and results

## Result and limits of the measurement

On October 8, 2026, the user approved a cold raw-cache migration on
`gpu-07-03` and `gpu-07-13`, followed by separately authorized benchmark phases.
Seven approved NVMe devices on node03 and eight on node13 were active. Node03
`nvme0n1`, serial `S64HNN0XA11543`, remained protected model-cache storage.

**The highest completed zero-error concurrency was C24 with 32-CPU loadgens:**
34,473.217 MiB/s, or **33.665251 GiB/s aggregate**. C32 reached 36,404.020 MiB/s,
or **35.550801 GiB/s**, but had two counted failures on node03. It is not a
zero-error result. Testing stopped after C32; no C48/C64 or further limit change
was performed. Both loadgens were confirmed C0 with zero in-flight work at
**16:35:15 UTC**.

These are warm/hot-cache client results, **not NVMe or RDMA capacity measurements**.
The shared 8 GiB catalog fits the configured plaintext budget. All 15 approved
devices were opened and received writes during C1, but later warm windows read
from only four physical disks per node and had no writes. Traffic on the selected
RDMA hardware ports was nearly idle in those windows. Neither opening all disks
nor high client goodput proves all-disk striping, disk saturation, or fabric capacity.

This continues the [file-slab concurrency report](racer-gpu-concurrency-results-20261007.md)
and [progress-fix report](racer-gpu-progress-fix-results-20261007.md). The storage
layout, image, cache history, and later LG quotas differ. These runs are not a
controlled causal comparison of raw NVMe versus file slabs.

## Source and validation provenance

| Item | Recorded value |
|---|---|
| Integrated source | `3c49c066ea57dac8768f60189fc67b942138f0aa` |
| Source worktree patch | `c79ee1d794eb2d4b90a20320f549393b1162b44f` |
| Successful amd64 build | [37796550021](https://github.com/Azure/unbounded/actions/runs/37796550021) |
| Immutable image | `ghcr.io/azure/racer-dataplane@sha256:b338c73d95bdebbb981caaf774c477ed73980d56ec26680974cc7691d767f7c4` |
| New slab/checkpoint directory | `/var/lib/racer/slabs/nvme-v2-20261008` |
| Raw device root | `/host/dev` |

The build checkpoint records matching Actions head SHA, amd64 build input, and
successful pushed index digest. This is Actions-log/metadata verification, not a
separate registry pull (`nvme-build-checkpoint.md:67-76` in the evidence archive).

Validation was **partial, not all green**:

- Thirteen focused device tests and the actual legacy/v2 recovery/reset test passed.
- The privileged disposable-loop test wrote/read all 11 segments across both
  workers, checked both guards after writes and restart, and passed. Test-owned
  loops were removed. These were local disposable loops, not the production disks.
- Production-only RDMA Clippy and Rust formatting passed. The Go formatting/lint
  retry passed with zero issues.
- Nine broad failures and an isolated relay timeout remain unresolved as a broad
  gate. Only one exact lifecycle EACCES failure was reproduced on baseline
  `13a17a9a3`; that does not classify the other failures as baseline or harmless.
- All-target Clippy remained blocked by unchanged `manual_noop_waker` in
  `src/read/tests.rs:498`. No unrelated suppression or fix was made.

Evidence: `source-nvme-checkpoint.md:12-19,23-28`,
`source-failure-report.md:3-23,33-44,70-79`, `source-isolation-results.json`, and
`isolate-*.log`. The later update at the top of the failure report supersedes its
earlier statements that loop execution and baseline comparison had not occurred.
Successful image publication does not resolve these test failures.

## Migration: completed operations, not replay instructions

The production discovery code opens an exclusive direct-I/O FD, validates the
device identity and unused state, and reads both guards. It does not erase them
(`cmd/racer-dataplane/src/app/devices.rs:91-149,171-194`). Each guard is exactly
1 MiB; capacity excludes both ends. Placement starts after the prefix and remains
before the suffix. The `racer-raw-layout-v2` digest binds the guard sizes and
mapping (`devices.rs:22,152-168,308-345`). Nonzero or short guard reads fail.
Discovery can skip devices or fall back to files (`devices.rs:71-86`), so the
operational runner separately required raw FDs covering exactly the approved
7+8 distinct devices. Duplicate descriptors were present; 7+8 is a device count,
not a descriptor count. Every FD path/device-number pair was validated before
deduplicating device coverage (`ops-raw-adapter.py:69-75`).

Two nonzero prefixes required separate user-approved clears. **Both are DONE.
DO NOT REPLAY the repair tools or automatically restore a backup.**

| Target | Approved write | Verified outcome | Durable original-prefix SHA256 |
|---|---|---|---|
| node03 `nvme3n1`, `S64HNN0XA09744` | One `pwrite`, offset 0, 1,048,576 bytes | Full return, fsync, zero readback; suffix unchanged | `1311a132a1df14269da894e009e947ca521bed31304498c18c2f7d76285423d2` |
| node13 `nvme6n1`, `S64HNN0XA11494` | One `pwrite`, offset 0, 1,048,576 bytes | Full return, fsync, zero readback; suffix unchanged | `664b871cce31375b2ef05c0646651c340650b378c2f8ccca7b167e128ec32b3a` |

Backups, manifests, directories, and parent directories were fsynced before the
write ACK, with renewed C0/identity/usage gates. Backups were private: files 0600,
directories 0700. The helper retained the same exclusive FD and rechecked identity,
geometry, reserve time, and readback. Exit was confirmed before diagnostic-pod
deletion. Kernel write-I/O splitting is not an extra helper `pwrite`.

There were **no other clears in this migration**, no suffix writes, format,
partition reread, discard, or automatic erase. Counter comparisons covered all
16 physical devices; all other devices' counters remained unchanged during each
clear. The previously repaired node03 device with serial09741 was not cleared
again. Protected node03 nvme0 payload was never read. Visible-process checks had
an outer-host visibility limit; the record does not claim omniscient opener
detection. See `ops-node03-clear-report.md`, `ops-node13-clear-report.md`, their
verified JSON/counter snapshots, and the two backup directories below.

Separate simultaneous read-only exclusive probes then validated all seven/eight
approved devices; all 30 endpoint regions matched the SHA256 of 1 MiB of zeros.
Activation used the new image and new checkpoint directory. Old top-level and
file-slab checkpoint trees were kept;
their hashes were frozen after the old DP exited, not reset to hide changes.
The user approved cold node-wide raw caches; no old cache identity was reused.
Evidence: `ops-all7-node03-report.md`, `ops-all8-node13-report.md`,
`ops-activation-1529-*`, and `ops-raw-cli-review.md`.

## Workload and resource changes

The seed stayed `gpu-progress-fileslab-20261007-cold-01`: shuffle selection of
128 blobs x 64 MiB, an 8 GiB catalog, UDS clients and origins on both nodes, SHA256
verification enabled. The DP stayed at 16 CPU/64 GiB with 10 I/O and 5 crypto workers,
plaintext/ciphertext/registered budgets 8/8/2 GiB, queues 256, flights 64, and configured
threads 16. Source guards compared the budget ConfigMap and full intended override.

The user authorized LG CPU-limit-only rollouts **8 -> 16 -> 32**. Requests stayed
1 CPU/512Mi and memory limit 4Gi; image, seed, origin settings, and all other DS spec
fields stayed unchanged. Each patch tested fresh UID/RV/full old spec. The DS was
manually managed by kubectl, with no owner reference or operator spec manager.
Each rollout restarted LGs/origins at C0, but DP UIDs/restarts and warm state were
retained. Node headroom was 253.34 allocatable CPUs and 5.107 effective requests per
node; this is scheduling headroom, not measured idle CPU.

The canceled CPU8 C24 task has **no observable workload execution**: no C24
artifacts/active runner, unchanged LG UIDs, and zero success/error/verified-byte
deltas since the C16 drain. This proves no measured workload advancement, not that
no control write could ever have been attempted; cancellation is not a diagnosed
deadlock. It is excluded from the results table. Evidence:
`ops-lg-cpu-review-1601-interrupted-c24-audit.json` and runner-process receipts.

## Completed windows

All phases targeted 60s; collection produced observed windows 66.55-66.70s. Rates
use per-node metric midpoint intervals; aggregate GiB/s sums the two rates and
divides by 1024. Percentiles are histogram estimates. Errors below are window
counts, not hidden by the permitted error budget.

| Phase | LG CPU limit | Node03 MiB/s | Node13 MiB/s | Aggregate GiB/s | Errors03/13 | Success p95 ms03/13 |
|---|---:|---:|---:|---:|---:|---:|
| Raw cold-start C1 | 8 | 1,578.523 | 1,590.793 | 3.095035 | 0/0 | 95.761/95.759 |
| Warm C8 | 8 | 11,540.190 | 11,236.444 | 22.242807 | 0/0 | 95.635/95.683 |
| Warm C16 | 8 | 11,981.963 | 11,731.920 | 23.158089 | 0/0 | 413.073/412.694 |
| Warm C16 | 16 | 16,167.726 | 15,673.679 | 31.095122 | 0/0 | 96.908/96.974 |
| Warm C24 | 16 | 16,525.864 | 16,044.217 | 31.806720 | 1/0 | 419.645/438.045 |
| Warm C24 | 32 | 17,397.729 | 17,075.488 | **33.665251** | **0/0** | 361.515/382.575 |
| Warm C32 | 32 | 18,356.373 | 18,047.647 | 35.550801 | **2/0** | 474.846/475.932 |

Evidence prefixes, in table order: `ops-raw-cold-c1-01`, `ops-raw-warm-c8-01`,
`ops-raw-warm-c16-01`, `ops-raw-cpu16-warm-c16-01`,
`ops-raw-cpu16-warm-c24-01`, `ops-raw-cpu32-warm-c24-01`,
`ops-raw-cpu32-warm-c32-01`. Each has analysis, window, result, raw samples, source,
drain, final-control, and report files. All-phase counts are retained, not just
the steady window. C32 activation/window/drain successes were 661/19102/2935 on
node03 and 236/18784/3278 on node13; only the window had failures 2/0. Total counts
were 22698/2 and 22298/0. CPU32/C24 totals were 21495/0 and 21091/0.

## Error policy and resource observations

C1 used strict zero-error acceptance. Later runs explicitly allowed per-node
counted failures below 5% after 32 completed attempts, with an absolute cap 500.
Only `http_status` and `incomplete` were budgeted. They are unverified failed
responses, not proven clean overloads. Integrity/unknown failures always stop;
no-progress 30s, sampled 75% memory checks against exact container limits, OOM
events, UID/readiness, and storage failures also stop. Cleanup applies C0 and
drains LGs before DP evidence; the parent retained
an independent fallback. See sealed `ops-curve.py:62-140` and raw guard adapters.
Verified bytes are credited only after successful verified completion
(`cmd/racer-loadgen/pull.go:251-282`); partial failed bytes get no goodput credit.

CPU16/C24 had one node03 `incomplete`. CPU32/C32 had one `http_status` and one
`incomplete`, window ratio 2/19104 = 0.010469%, total 2/22700 = 0.008811%. Node13 had none.
No hard integrity or unknown failure was observed. C32 error p50/p95/p99 estimates
100/460/492ms interpolate only two events and are not exact timings.

CPU8/C16 used about 7.99 LG cores with 659/665 and 660/666 throttled periods. Raising
LG CPU removed that quota constraint at C16. CPU16/C24 again used 15.981/15.954
cores with 614/666 and 621/667 throttled periods (15.485084/17.509343 throttled seconds).
CPU32/C24 used 17.902/17.904 cores without throttling. At CPU32/C32:

| Container | Cores total/user/system | Throttled periods/total | End memory MiB |
|---|---:|---:|---:|
| LG03 | 22.485/8.864/13.621 | 0/667 | 40.141 |
| LG13 | 22.437/8.704/13.733 | 0/666 | 34.625 |
| DP03 | 8.541/1.950/6.592 | 0/666 | 18,618.156 |
| DP13 | 8.868/1.966/6.902 | 0/667 | 18,594.266 |

Throttled time was zero in all four C32 containers; sampled memory-event counters
were zero and the sampled 75% memory gates passed. This is not a continuous
memory-usage guarantee between samples. C32 gained 5.601% over CPU32/C24, but success
median latency rose from 62.596/64.238ms to 248.459/259.318ms. This does not establish the
next bottleneck or justify more limits automatically.

## Disk and fabric evidence

C1 started with cold raw layout/checkpoints, but initial fills preceded the applied
window and repeated access warmed the catalog. All 15 disks had positive physical
writes. Later warm windows had zero writes/discards and reads on node03 nvme2/4/5/6
and node13 nvme0/1/4/6. The other approved devices stayed open but had zero physical
read deltas. Every per-device byte/I/O count is in the phase analysis/report files.

At C32, DP disk-payload reads were 1163.633/1146.236 MiB/s, about 1.1 GiB/s per node,
with zero published payload. This is far below client goodput. Physical counters
include endpoint guard reads and readahead; cached guards may add no physical
read. At each guard check, all 30 endpoint regions matched the SHA256 of 1 MiB of
zeros; the digest itself is not zero. Protected node03 nvme0 had zero I/O delta.

The RDMA table totals select only hardware ports under **mlx5_00 through mlx5_07**,
not every captured port (`ops-cpu16-report.py:39-42`). C32 selected-port TX/RX were
13,824 bytes each per node, with zero selected-port discards and RX read/write
requests. The same analysis also captured node03 `ibp210s0f0` with **48,960 TX bytes
and 48,960 RX bytes**, excluded from that selected sum
(`ops-raw-cpu32-warm-c32-01-analysis.json:793-802`). Do not read the table as an
all-port total. Data counters were converted from four-byte units. These counters
include background traffic, not exclusive Racer wire bytes. The selected-port
and additional captured traffic remain near idle; no sustained NVMe or RDMA
capacity claim follows.

## Final state and preservation

C32 C0 CAS completed 16:35:09.732Z; both LGs drained 16:35:15.138Z; post-drain raw
snapshot 16:35:21.741Z; final control snapshot 16:35:26.714Z. DP membership remained
fully applied 10/10 with matching hashes. The final deployment was left with
LG 32CPU/4Gi, requests 1CPU/512Mi, DP 16CPU/64Gi, raw FDs covering 7+8 distinct
devices, and all 30 endpoint regions matching the SHA256 of 1 MiB of zeros,
protected PV/device unchanged, and frozen old checkpoints intact. No later cluster
audit is implied by this report-only phase.

Evidence is preserved outside the worktree at
`tmp/racer-gpu-nvme-20261008-artifacts/` in the original repository:

- `worktree-tmp/`: all operational scripts, receipts, tests, samples, reports,
  checkpoints, source-test logs, and both private raw-prefix backups. The backup
  directories are `ops-prefix-backup-gpu-07-03-1791471594` and
  `ops-prefix-backup-gpu-07-13-1791472107`. Probe-only receipts are retained too.
- `dependencies/`: copies of the sealed concurrency and progress-fix archives.
  `ops-raw-curve.py` imports the first archive's `ops-curve.py`, which imports the
  second archive's `ops-c1.py`. Both original manifest hashes and every listed
  file were verified before copying. Originals were not changed.
- `source-snapshots/`: cited code and reports; `parent-checkpoints/`: parent record.
- `preservation-manifest.json`: original source paths, destination paths, sizes,
  SHA256 values, exclusions, and import requirements. `SHA256SUMS` seals preserved
  files. Directories 0700, files 0600, including backups; build targets and unrelated
  caches are excluded. Final count and seal hashes are returned in the handoff.

**DONE - DO NOT REPLAY** labels apply to archived repair, rollout, and load tools.
Preservation is not permission to restore raw bytes or rerun a clear. Scripts keep
their original absolute import paths: offline reproduction needs those verified
archives or a separately reviewed path rebind, not mutation of the sealed originals.
Raw backups are private evidence, not files to commit. Only this report is committed.
