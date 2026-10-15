# Racer GPU-node 128 GiB Zipf results

## Outcome and scope

This round exercised **memory, all 15 approved NVMe devices, and RDMA peer traffic**
on `gpu-07-03` and `gpu-07-13`. It met the tier-participation goal, not a hardware
capacity or maximum-performance claim. Removing CPU quotas did not remove the
worker-count or admission constraints. The exact next bottleneck remains unknown.

The best completed measured window was the C8 thread-diagnostic run:
**21,224.598 MiB/s, or 20.727147 GiB/s aggregate**, with counted response failures.
The only completed zero-error point was C1: **4.531652 GiB/s**. C16 crossed the 5%
failure gate and stopped; its partial **22.551045 GiB/s** is not a valid steady
point. No later load or tuning followed the diagnostic C8.

All rates below are **successful unverified payload delivery**, not verified
goodput. Client content hashes were disabled. Exact size and protocol completion
still gate success; no partial failed RX bytes receive successful-delivery credit.
This round continues the [guarded NVMe report](racer-gpu-nvme-results-20261008.md).
Catalog, selection, verification, CPU limits, process starts, and cache history
changed, so the reports are not a controlled single-variable comparison.

## Workload and retained safeguards

| Setting | Value on both nodes |
|---|---|
| Catalog | 512 generic blobs x 268,435,456 bytes = 128 GiB |
| Selection | `--profile=zipf --zipf-exponent=1.2` |
| Seed | `gpu-zipf-20261008-128g-v1` |
| Client verification | `--verify=false --diagnose-integrity=false` |
| Transport | UDS client and synthetic origin, `racer-bench` volume |
| LG image | `ghcr.io/azure/racer-loadgen:4bb2327cb5ef9a2ce317473f7b634f4b22893cd0` |
| DP image | `ghcr.io/azure/racer-dataplane@sha256:b338c73d95bdebbb981caaf774c477ed73980d56ec26680974cc7691d767f7c4` |
| CPU limits | Removed for DP and LG; actual self and visible ancestors `max 100000` |
| Memory limits | DP 64 GiB; LG 4 GiB, retained |
| Requests | DP 4 CPU/8Gi; LG 1 CPU/512Mi, retained |
| Threads | `RACER_MAX_THREADS=16`; final 10 I/O + 5 crypto per DP |
| Cache budgets | Plaintext 8 GiB, ciphertext 8 GiB, registered 2 GiB |
| Raw cache | Same `/host/dev`, `/var/lib/racer/slabs/nvme-v2-20261008` |
| Runtime control | Duration default 0; concurrency file, C0 between phases |
| Startup bound | Explicit `--startup-timeout=4m`; pull timeout stayed 2m |

Memory usage was checked at samples against 75% of the exact container limits,
with max/OOM event checks. These are not continuous guarantees. Nodes and shared
workloads were checked for health and identity changes. Control-plane resources
were not tuned. Visible ancestor checks establish no observed CFS quota in that
hierarchy, not unlimited physical CPU or knowledge of hidden outer ancestors.

The DP CPU field was owned by the operator. Its override omitted only CPU while
retaining memory and requests; LG CPU removal and workload args used one atomic
UID/RV/full-spec CAS. DP rollout occurred first at C0, then LG rollout. The exact
deployed operator revision's resource-generation block, override merge, and SSA
path were reviewed; the resource render test produced no CPU limit. Evidence:
`ops-zipf-plan-1704-*`, DP rollout receipts, and `ops-zipf-lg-observe-1725-*`.

The new synthetic catalog still generated and hashed the whole 128 GiB at startup
despite download verification being disabled. `cmd/racer-loadgen/blob.go:59-108`
uses a 128 KiB scratch buffer and sequentially hashes every generated blob. LG
node13 started generation at 17:22:02.477Z and reported origin Ready at
17:23:12.566Z; node03 at 17:23:13.534Z and 17:24:23.575Z. Both were below 4m.
Observation returned a partial checkpoint once, then resumed read-only without
reapplying. Origins started at C0 before any pull.

## Unexpected old-index observation during DP restart

Removing the DP quota restarted the processes. Before restart, the exported
`racer_disk_indexed_payload_bytes` gauge was 8,589,934,592 on each node. Afterward
it was 0 on both. HTTP 200, the exact unlabeled gauge family, and its zero value
were captured: this was not a missing metric or a 404 parsed as zero.

Current raw checkpoint sequences continued (1231/1243 in the later read-only
capture). The same `checkpoint.0/.1` names remained, but each file's size changed:

| Node | Before each file | After each file | Difference |
|---|---:|---:|---:|
| gpu-07-03 | 10,167,601 bytes | 10,040,113 bytes | 127,488 bytes |
| gpu-07-13 | 11,598,376 bytes | 11,470,888 bytes | 127,488 bytes |

127,488 = 512 x 249 is a structural hypothesis consistent with a page-record
change, **not decoded proof** of which records were removed. No pre-restart binary
checkpoint backup was captured for this restart. File hashes/sizes/mtimes and
metrics exist, but the before/after payloads were not decoded in this operational
phase. Logs did not show incompatible-checkpoint skips or fallback, but absence
of those messages does not prove recovery. Cause and old-content survival remain
unresolved. The parent explicitly accepted fresh-seed testing with that caveat.

The indexed gauge aggregates retention snapshots, rather than decoding disk
checkpoints (`src/telemetry.rs:1576-1585,1761-1783` under `cmd/racer-dataplane`).
Checkpoint sequence updates on publication (`src/app/recovery.rs:363-376`). Do
not infer data loss or successful recovery from either alone. Evidence:
`ops-zipf-dp-post-1715-recovery-evidence.json`, its report, and the saved pre-rollout
checkpoint inventory. No clear, erase, repair, format, identity reset, or checkpoint
deletion occurred this round. Current raw checkpoint publication was allowed;
the **old** top-level/file-slab checkpoints remained frozen and unchanged.

## Measurement protocol and acceptance

Each load phase required a full read-only preflight receipt, private and hashed,
valid for 60s. A separate command checked source UID/RV/payload, complete exact four
DP/LG pods, starts/container IDs, shared workloads, API C0, and unchanged LG counters
concurrently before activation. C1 additionally required the new seed's pull/RX
counters to be zero. Continuations accepted historical counters only if unchanged
from their new receipt. Consumed receipts were never replayed.

External commands were TERM-bounded at 300s; the reviewed execution protocol used
an absolute internal 290s, quick gates at most 20s, active work at most 110s, and
at least 150s reserved for cleanup. Mandatory cleanup patched UID-guarded C0 first,
then queried known LG metrics concurrently. Full DP/raw/resource checks ran in a
separate read-only post-audit after applied0/in-flight0 was affirmed. No costly
diagnostics preceded C0. The parent retained an independent fallback.

C1 and the first C8 used strict zero errors. The parent then separately authorized
new counted-policy phases: per-node failure ratio below 5% after 32 completions,
absolute cap 500, budgeting only `incomplete` and `http_status`. Unknown or
size-unexpected labels and observed DP crypto/CRC/AEAD failures remained hard
stops. No-progress was 30s of successful-delivery progress. Sampled gates may see
multiple errors at once; a strict stop does not mean exactly one error occurred.

Successful bytes = successful-pull delta x 268,435,456. The genuine verified-byte
counter stayed 0. `cmd/racer-loadgen/pull.go:265-276,390-436` shows the distinction:
verified credit requires Verify=true; verification-disabled reads still check
protocol/size but received bytes can include partial failures. Histograms provide
estimated percentiles, not exact event timings.

## Client results

Intervals are observed, not nominal 60s. Completed rows show runner wall-window
time; partial rows show node03/node13 metric intervals. Rates always use each
node's metric midpoint interval. Thread sampling has a separate interval below.

| Phase | Result | Observed seconds | Node03 MiB/s | Node13 MiB/s | Aggregate GiB/s | Window errors03/13 | Success p95 ms03/13 |
|---|---|---:|---:|---:|---:|---:|---:|
| C1 strict, cold-seed start | Completed zero errors | 73.291 | 2,332.236 | 2,308.176 | **4.531652** | 0/0 | 455.282/456.612 |
| C8 strict continuation | Aborted | 18.289/18.362 | 9,938.241 | 10,316.808 | 19.780321 partial | 8/1 | 482.005/486.423 |
| C8 counted continuation | Completed with failures | 73.420 | 10,528.355 | 10,266.036 | 20.307022 | 20/18 | 472.447/475.271 |
| C16 counted continuation | Aborted at ratio gate | 55.615/55.599 | 11,774.661 | 11,317.609 | 22.551045 partial | 134/135 | 825.232/806.705 |
| C8 counted, thread diagnostic | Completed with failures | 74.488 | 10,682.669 | 10,541.929 | **20.727147** | 11/8 | 466.840/468.858 |

The first seed was unpulled at activation, but it warmed during the C1 activation
and window. Later runs continued that cache history; they were not reset cold.
The diagnostic C8 is the best completed observation in this round, not proof of
a maximum sustainable rate or an improvement caused by instrumentation.

All-phase success/error counts, including propagation and drain:

| Phase | Activation03;13 | Window03;13 | Drain03;13 | Total03;13 |
|---|---|---|---|---|
| C1 strict | 30/0;11/0 | 673/0;666/0 | 157/0;142/0 | 860/0;819/0 |
| C8 strict | 121/0;61/0 | 710/8;740/1 | 578/3;615/3 | 1409/11;1416/4 |
| C8 counted | 115/0;52/0 | 3023/20;2949/18 | 631/3;655/1 | 3769/23;3656/19 |
| C16 counted | 108/3;35/0 | 2558/134;2458/135 | 675/43;729/36 | 3341/180;3222/171 |
| C8 diagnostic | 103/0;47/0 | 3109/11;3063/8 | 651/1;707/2 | 3863/12;3817/10 |

C16 total failure ratios reached 5.1122%/5.0398%; cap500 was not reached. Its total
reasons were incomplete175/162 and HTTP5/9. Counted C8 total ratios were
0.6065%/0.5170%; diagnostic C8 0.3097%/0.2613%. Partial RX remained separately
reported and uncredited in every phase. Highest zero-error remains C1.

## Disk and peer participation

All 15 approved devices had measured physical reads and writes; protected node03
nvme0 had no I/O delta. FDs covered exactly 7+8 **distinct devices**, not 15 total
descriptors. All 30 endpoint regions matched the SHA256 of 1 MiB of zeros at checks.
Per-device bytes, I/Os and rates are retained in each phase's analysis/report.
Physical counters include guard probes and readahead; no capacity or uniform
striping conclusion follows.

| Phase | DP published MiB/s03/13 | DP read MiB/s03/13 | Physical read MiB/s03/13 | Physical write MiB/s03/13 |
|---|---:|---:|---:|---:|
| C1 | 499.140/481.779 | 526.653/544.573 | Per-device evidence retained | Per-device evidence retained |
| C8 strict partial | 945.770/1045.067 | 3034.492/3078.546 | Per-device evidence retained | Per-device evidence retained |
| C8 counted | 389.454/360.969 | 3732.140/3772.655 | Per-device evidence retained | Per-device evidence retained |
| C16 partial | 139.935/190.524 | 4693.002/4401.911 | 4685.768/4413.334 | 135.253/178.396 |
| C8 diagnostic | 56.515/104.026 | 4227.043/4101.284 | 4226.361/4110.359 | 57.171/93.684 |

Memory/disk/peer hit deltas occurred on both nodes in every measured interval.
For diagnostic C8 they were 29513/19664/107 on node03 and 29207/19085/174 on node13.
These counts are not byte fractions. Hardware RDMA writes corroborate peer-path
activity; they are not exclusive Racer attribution or fabric saturation.

RDMA tables select only `mlx5_00` through `mlx5_07`, not all captured ports. Data
counter units were converted x4 to bytes. Other physical/bond port counters remain
individual records; no alias-unsafe all-port sum was made.

| Phase | Selected TX/RX bytes03 | Selected TX/RX bytes13 | RX write requests03/13 |
|---|---:|---:|---:|
| C1 | 2874187996/2891289792 | 2891289792/2874187996 | 692393/688296 |
| C8 strict partial | 1334404920/1129183300 | 1129183300/1334404920 | 270402/319566 |
| C8 counted | 3472998360/3592710932 | 3609815924/3473001556 | 860370/831691 |
| C16 partial | 2258176760/1676715628 | 1676712432/2241071768 | 401506/536707 |
| C8 diagnostic | 1505486428/1248959488 | 1248959488/1505486428 | 299081/360536 |

C16 selected TX/RX rates were 38.755/28.776 MiB/s on node03 and 28.763/38.444 on
node13; diagnostic C8 was 19.283/15.997 and 16.000/19.286. Selected RX read requests
and TX discards were zero. Fabric capacity was not established by this round;
increasing client throughput is not itself RDMA throughput.

## CPU, thread and queue observations

CPU-unlimited is not thread-unlimited. With node client_connections 128 divided
among workers, each worker needs at least 12 to retain three control slots:
`src/admission.rs:134-137`, `src/app.rs:2349,2406-2409`. This gives an I/O ceiling
of floor(128/12)=10 under that constraint. Startup logs explicitly report
`limiting_resource=control_connections`, final10I/O/5crypto. Reduction retains up
to ceil(I/O/2) crypto assignments per NUMA group (`src/worker.rs:159-177`). Raising
MAX_THREADS alone would not remove this binding floor. No thread setting changed.

| Phase | DP CPU cores03/13 | LG CPU cores03/13 | Encrypt queue mean ms03/13 | Decrypt queue mean ms03/13 |
|---|---:|---:|---:|---:|
| C1 | 1.651/1.647 | 0.424/0.413 | Not summarized | Not summarized |
| C8 strict partial | 6.711/7.151 | 1.674/1.846 | 9.838/11.873 | 10.709/10.359 |
| C8 counted | 6.809/6.894 | 1.589/1.600 | 9.968/10.734 | 11.478/11.209 |
| C16 partial | 8.224/7.973 | 1.809/1.839 | 17.988/18.429 | 21.431/19.855 |
| C8 diagnostic | 6.906/6.946 | 1.497/1.551 | 10.487/10.151 | 12.233/12.141 |

All observed CFS throttling periods/time were zero. DP sampled memory was about
20 GiB in the later windows, LG below 0.06 GiB, with max/OOM event counters zero.
LG CPU combines origin generation and client work; no separate origin CPU
attribution was measured. Origin request p95 was about 9.5ms. Queue means use raw
sum/count deltas; no absent crypto-pool busy gauge was invented.

The final C8 added exactly two bounded per-DP thread snapshots inside the existing
phase budget, not perf/BPF or host changes. Calls took 1.06-1.11s. TID/starttime/comm,
affinity, pod UID and container identity were matched before comparison. About 25
Linux tasks were visible, including 15 user workers and io_uring workers; not 25
user threads. The main `racer-dataplane` task is included with the I/O workers.

Thread interval was **74.49s** from remote uptime; endpoints were about 12.46-12.48s
later than corresponding DP metric midpoints. These shifted intervals are not the
same metric window. CPU cores use tick deltas/CLK_TCK100/own elapsed seconds.
Scheduler run and wait percentages divide by own wall interval; scheduler wait
is runnable delay, not blocked I/O or crypto queue time.

Usable crypto threads ran at about **0.65-0.74 cores each**, with scheduler wait
about **0.036-0.076% of wall time**. Matched I/O workers ran at about 0.32-0.38 cores.
This does not show continuous one-core saturation or strong scheduler starvation
for those matched rows; bursts and other constraints remain possible.

End snapshots were partial. Node03 I/O6 and transient io_uring records lacked a
usable pair; node13 crypto11 was unavailable. They were not assigned zero CPU.
Matched-only group sums cannot be treated as whole-process totals. Raw endpoints,
all usable rows, exclusions, offsets and scheduler denominators are retained in
`ops-zipf-thread-c8-1822-thread-analysis.json` and the phase report.

## What remains unproven

Recovered terminal journals show `Overloaded` at client boundaries, including
partial NextSlice delivery. Generic admission rings contain Ciphertext and Pipe
pressure. But terminal coverage is `client_boundaries_resource_unknown`, generic
rows lack client request IDs, multiple rows can describe one request, and bounded
rings overwrite events. This is insufficient to assign each failure to a precise
resource or claim an 8->16GiB ciphertext change would fix it.

The next proposed work is **narrow request-linked flight/reactor-submission
attribution before tuning**, with bounded diagnostics and unchanged workload.
It should distinguish which admission or submission path produced the final
client error. This report does not recommend or implement a speculative budget
increase, MAX_THREADS increase, broader instrumentation, or another run. Earlier
scratch-report budget suggestions are unexecuted hypotheses, not validated fixes.

## Final state and evidence preservation

Final C0 PATCH completed **18:24:51.015Z**; both LGs applied0/in-flight0 at
**18:24:55.333Z**. Separate full post-audit completed **18:25:44Z**. Sources, pod
identities, matching10/10 membership, node/shared health, exact resource limits,
7+8 distinct-device coverage/full16 inventory, all30 zero-region hashes, protected
PV/device counters and frozen old checkpoint hashes passed. The report-only phase
made no later cluster calls.

The deployment was left with both CPU limits absent, visible ancestors `max
100000`, MAX_THREADS16, budgets8/8/2GiB, memory64GiDP/4GiLG, requests unchanged,
Zipf128GiB/verifyfalse args, and concurrency0. No CPU/thread/cache-budget change
was made after the initial approved rollouts. No claim of client hash integrity
or old-index recovery is made.

Evidence prefixes in chronological order:

- `ops-zipf-cold-c1-1750`
- `ops-zipf-warm-c8-1757` (strict abort)
- `ops-zipf-warm-c8-counted-1803`
- `ops-zipf-warm-c16-counted-1809` (ratio abort)
- `ops-zipf-thread-c8-1822`

Each retains samples, all-phase accounting, result/policy, C0 receipt, full
post-audit, analysis and report; later phases also retain terminal journals.
Local diagnostic save/allowlist errors after successful audits were recorded and
only missing evidence was collected, not a repeated load or repair. The archived
source-validation caveats from the prior NVMe report still apply; this benchmark
does not make the unresolved broad tests green.

All round artifacts and hash-verified import dependencies are preserved under the
original repository's `tmp/racer-gpu-zipf-20261008-artifacts/`, with source paths,
hashes and exclusions in `preservation-manifest.json` and an outer `SHA256SUMS`.
Directories are 0700 and files 0600. Build caches are excluded. Original NVMe and
sealed engine archives remain immutable; copied dependency paths are recorded
for offline reproduction. Archived operational tools and consumed receipts are
**DONE - DO NOT REPLAY**. No secret objects or credentials are requested or
committed. Only this report is committed; raw/private evidence remains local.
