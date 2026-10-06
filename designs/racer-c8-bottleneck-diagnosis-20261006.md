# Racer C8 after secure zeroing: bounded read-only diagnosis

## Decision

**Investigate root-disk service on v5 first, not crypto-worker count or Gantry's
CPU quota.** Both sampled v5 hosts sustained approximately 515 MB/s physical reads
in two consecutive 60-second intervals, with 57-61 ms read await and about 30
outstanding reads, while host CPU was only 51-55%. Decrypted bytes and logical
disk payload reads independently track that physical read rate. This is stronger
limiter evidence than a CPU hotspot alone, but it is **not causal proof** of a
disk bandwidth cap. No Azure disk/burst-credit telemetry or controlled perturbation
was collected. Do not claim a fleet-wide hardware limit from three hosts.

No image, workload, C8, resource setting, slab, cache, or runtime code was changed.
No C16 experiment was run. The measured image remains
`ghcr.io/azure/racer-dataplane:secure-zeroing-41ea7720f`, whose previously verified
OCI index is `sha256:b9960c690a3a6a9120b0a569e24e39b154203c7043fa26465999fccd3dcea50d`.
The separately published full-SHA registry build was not substituted. This audit
checked actual container image/readiness, not a fresh all-node OCI import audit.

## Scope and evidence convention

Fresh fixed window: **2026-10-06 13:48:13-13:53:13 UTC**. Three procfs snapshots
span approximately 13:51:13-13:53:13, overlapping the same window. Parca is only
available for `aks-ddv5-17198779-vmss0000ad`; no other-node profiles are implied.

Archive-relative evidence paths below identify exact JSON keys; JSON files retain
queries, evaluation timestamps, and raw responses. Deployed source excerpts are
from tree `41ea7720fc7d552a7875b5b87c8768f910014247`, exported without reading Git
history. Source citations in this report refer to `cmd/racer-dataplane/` under that
tree, archived in `deployed/`, not to later refactors on the original branch.
Prior intent/results: `designs/racer-secure-zeroing-results-20261005.md:97-154`.

Collectors used only Kubernetes GET and read-only exec into existing privileged
net pods. Every command had an external TERM timeout of at most 300 seconds and
10-second kill grace. No delegation tool was available; independent collection
ran concurrently with bounded commands. One host-path lookup failed, then only
that lookup was corrected using the actual volume mapping; completed collection
was not replayed. Initial and final audits, commands, and checkpoints are archived.

## Matched workload and health

Unchanged one-image, 11-layer nominal 1 GiB/jitter 0.2/seed `benchmark-v1` workload,
layer concurrency 1, verification off, 4 GiB plaintext and 6 GiB ciphertext per
node, max 8 runtime threads, 16 GiB slab per worker, Gantry quota/GOMAXPROCS 4.
The explicit pod environment sets `RACER_RANGE_WINDOW_PAGES=2`, overriding the
ConfigMap's value 1; this is not the loadgen's layer concurrency setting.
Evidence: `workload-ds.json`, `sample-pods.json`, `dataplane-config.json`.

| Fleet observation | Fresh window |
|---|---:|
| Received TiB/s | 4.84407 |
| Successful pulls/s | 460.533 |
| Mean pull seconds | 26.0745 |
| Nodes advancing successful pulls | 1500 |
| Applied concurrency min/max | 8 / 8 on all 1500 |
| Ready minimum | 1 on all 1500 |
| Indexed bytes min/max per node | 11,572,407,598 |
| Byte samples per node / byte counter resets | 5 / 0 |
| Application error rate | 0 |
| CRC/AEAD and disk/retained/peer corruption rates | 0 |
| Internal request errors/s | 0.29583 |
| Disk source events/s | 47,156.06 |
| Peer source events/s / origin fills/s | 0 / 0 |

Evidence: `prom-results.json` keys `rx`, `pulls`, `pull-mean`, `progress`,
`concurrency-{min,max}`, `ready-min`, `index-{min,max}`, `rx-{samples,resets}`, and
`rate-racer_*`; `correlation.json` retains counter-reset and fleet metric coverage.
Largest endpoint byte/readiness sample ages were 59.95/59.96 seconds, consistent
with one-minute scraping. The fresh rate is lower than the immediate post-rollout
5.6061 TiB/s; this is not a new code regression experiment. Cache/source behavior
and disk service have changed over the intervening hours.

## CPU costs, not automatically bottlenecks

All three raw profiles have **300 consecutive nonzero one-second bins** whose
sums exactly match their pprof CPU totals. Raw duration headers are not used.
The existing `hack/cmd/racer-profile-summary` groups raw IDs and falls back from
Name to SystemName, never merging zero addresses (`main.go:146-203`). There are
no TID labels in these profiles; thread evidence below comes from procfs instead.

Denominator: same node's Prometheus five-minute byte rate times 300 =
**740.94465 GiB**, an extrapolated estimate, not exact byte-fenced work.

| Process | CPU seconds | CPU s/GiB |
|---|---:|---:|
| Dataplane | 621.684 | 0.83904 |
| Gantry | 282.000 | 0.38060 |
| Loadgen | 231.105 | 0.31191 |
| Sum | 1134.789 | 1.53154 |

The immediate post-rollout dataplane figure was 0.88901 CPU s/GiB, but the changed
source mix prevents attributing this difference to an efficiency change.

| Sampled CPU stack category | CPU s | Process share | CPU s/GiB |
|---|---:|---:|---:|
| DP ChaCha20/Poly1305/CRC64 union | 172.632 | 27.77% | 0.23299 |
| DP `unix_stream_sendmsg` | 194.947 | 31.36% | 0.26311 |
| DP `_copy_from_iter` | 78.000 | 12.55% | 0.10527 |
| DP `clear_page_erms` | 64.105 | 10.31% | 0.08652 |
| DP `explicit_bzero` | 47.421 | 7.63% | 0.06400 |
| Gantry `__x64_sys_splice` | 234.263 | 83.07% | 0.31617 |
| Loadgen `_copy_to_iter` | 86.316 | 37.35% | 0.11649 |

These are per-sample cumulative unions, **not additive categories**; copy/clear
overlap the UDS path. Crypto union matches source filenames containing `chacha20`,
`poly1305`, or `crc64fast`. The archived `union.go` specifies every selector.
Unresolved libc leaf CPU is 113.368 seconds (18.24% of DP CPU); it is not labeled
memcpy, and can include descendants of identified callers such as `explicit_bzero`.
The remaining union of `zeroize`-named frames is only 0.0526 seconds; this is not
the old dominant aligned-byte wipe loop. Evidence: `profiles/*.raw.pprof`,
`profiles/*.summary.json`, `coverage-verified.json`, `findings.json:profiles`.

Direct-delivery byte rate is about 93.3% of this node's received-byte rate and
pipe-drain events are 158.4/s. These are not exact route fractions or fallback
probabilities. Deployed `http/src/transfer.rs:114-184` starts with pipe staging,
switches after unsupported splice or backpressure, and reports direct sends,
including completion sends. This supports the observed copied UDS path but does
not establish its critical-path cost. No speculative UDS implementation is proposed.

## Headroom and disk correlation

One host per cohort; each row contains the two consecutive 60-second readings.
MB means decimal megabytes. Await includes block-layer queue/service time, not
application page-read latency; large reads can split into multiple block requests.

| Cohort / node suffix | Physical read MB/s | Read await ms | Average outstanding reads approximately | Host busy % | CPU PSI some % |
|---|---|---|---|---|---|
| ddv5 / `0000ad` | 514.7 / 515.1 | 57.4 / 58.9 | 30.1 / 31.2 | 51.8 / 50.9 | 23.7 / 23.1 |
| adsv5 / `00006u` | 515.2 / 514.7 | 60.5 / 59.6 | 31.1 / 30.5 | 54.6 / 53.8 | 27.4 / 26.6 |
| ddsv6 / `000000` | 519.8 / 536.0 | 8.0 / 8.2 | 16.2 / 17.3 | 69.1 / 69.5 | 37.0 / 36.3 |

The outstanding values use weighted I/O milliseconds / elapsed milliseconds;
writes are only about 0.05 MB/s. v5 read request rates are 510-527/s with roughly
1 MB requests; v6 is 2033-2107/s with roughly 256 KB requests. Do not compare IOPS
alone. Physical `sda` and partition `sda1` report very different busy ticks, so
`%util` is not used as a saturation verdict. Slabs are on ext4 root backing:
v5 device 8:1, v6 259:1, **not the idle v5 `sdb` temporary disk**. HostPath is
`/var/lib/racer-disk-benchmark-20261005/slabs`. Evidence: `backing-*.json`,
`host-*.json`, `analysis.json:hosts`; extraction formulas are in `analyze.py`.

| Cohort | Delivered GiB/s | Logical disk payload MB/s | Decrypted MB/s | Plain hits/s | Plain misses/s | Disk events/s | Decrypt execution / queue mean ms |
|---|---:|---:|---:|---:|---:|---:|---:|
| ddv5 | 2.470 | 514.87 | 514.94 | 37.26 | 124.77 | 31.05 | 20.24 / 1.86 |
| adsv5 | 2.487 | 514.95 | 514.95 | 37.45 | 124.07 | 31.07 | 19.04 / 1.96 |
| ddsv6 | 5.428 | 527.19 | 527.12 | 129.15 | 221.45 | 31.64 | 18.19 / 0.69 |

Evidence: `correlation.json`, `findings.json:cohorts`. The v6 node delivers more
than twice as much data for similar disk/decrypt bandwidth, with substantially
more plaintext hit events. Source counters are conditional lookup events, not
client hit ratios. Deployed `src/read/fill.rs:703-775` checks plaintext before
joining a shared flight; followers can receive a completed shared result without
another disk hit. `fill.rs:1125-1195` counts disk hits after local acquisition and
decryption. Coalescing can explain work amplification, but **its factor and wait
time were not measured**: the available metric inventory has no flight-wait or
disk-read-latency histogram. Missing metrics are not zeros. The approximate 5.15x
delivered/decrypted byte ratio on ddv5 is not a cache-hit percentage.

Queue means measure accepted submission to crypto dequeue, exclude permit wait,
and are recorded on I/O completion consumption (`src/security.rs:1031-1110,
1231-1265`). Execution is elapsed wall time, not CPU time. Tests explicitly
exclude pre-admission and delayed completion reaping and assert exact shared-worker
times (`security.rs:2485-2504,2519-2544,2680-2703`). These means cannot exclude a
tail-latency or admission problem. They do not indicate a saturated crypto pool.

Fresh procfs thread evidence confirms five I/O threads and three crypto threads,
pinned to CPUs 0/2/4/6/1 and 3/5/7 respectively on all three nodes. No dataplane
thread exceeds 38.7% of a core; v5 maxima are 31.7% and 28.3%. Individual crypto
threads use roughly 12-28%; total crypto demand is well below three cores.
Per-core host busy ranges are 46.5-58.8% (ddv5), 50.4-61.9% (adsv5), and
67.0-74.6% (ddsv6). Scheduler wait is real, but no fully occupied individual core
was observed. Point-in-time wchan includes I/O poll and crypto futex waits; these
are snapshots, not an off-CPU distribution.

All sampled process/pod/ancestor cgroup CPU throttled deltas are zero. Dataplane
and loadgen have unlimited CPU quota; Gantry has 400000/100000 and consumes about
1.0, 1.2, and 1.7 cores across the three hosts. Effective cpusets are 0-7. No
non-root cgroup `io.max` limit was present. Root `cpu.max`/`io.max` files are absent,
as recorded, not fabricated zeros. IO PSI is tiny and iowait below 0.12%, but
asynchronous disk-dependent work can still be latency/bandwidth limited. Low IO
PSI is **not** evidence against that hypothesis.

## Next experiment, only with separate authorization

First obtain read-only Azure OS-disk/VM bandwidth, IOPS, and burst-credit telemetry
for the two v5 hosts. Approximately equal 515 MB/s plateaus and greatly increased
await versus the earlier 9 ms observation suggest a backing-service ceiling or
lost burst headroom; neither explanation is verified here.

For causal confirmation, run a reversible paired canary experiment that varies
only backing-disk service capacity at fixed C8/image/catalog/cache budgets, with
matched fresh profiles, block stats, source bytes, and cache/coalescing evidence.
Predeclare equal warm-state criteria; merely relocating or restarting changes
cache residency and is not a clean test. If faster disk service raises unique
page acquisition and delivered bytes while CPU demand rises into existing
headroom, the disk hypothesis gains causal support. If it does not, distinguish
disk-latency exposure/read-window scheduling from UDS delivery and CPU scheduling.
Do not increase concurrency, disable integrity, change crypto workers, or rewrite
transport based solely on these profiles. No such experiment was run here.

## Validation and preservation

No implementation or tests changed. Analysis assertions verified profile coverage,
sample sums, and timestamp continuity. JSON/raw evidence and analysis scripts are
preserved outside Git; only this report is committed. Full build/test/format suites
are not relevant to a Markdown-only diagnosis and were not run. `git diff --check`
is the scoped report check. Final audit and archive seal are recorded below.

Final live audit at **14:01:38 UTC**: all 1500 actual candidate containers Ready,
all 1500 C8 with empty caps and full indexes, successful-pull progress on all
1500, zero application/CRC/AEAD error rates in the preceding five minutes.
The audit and fixed-window invariants passed explicit assertions, including
1500-series coverage for the collected crypto/source/corruption counters.
DaemonSet updated count remains 1499 because the prior canary has an old revision
label; actual image inventory independently verifies all 1500 containers.

Evidence archive: `tmp/racer-c8-diagnosis-evidence-20261006.tar.gz`, 1,897,888
bytes, 311 members. SHA-256:
`2c33e06eed50ba7e7406f96c138fe7284ad57417f2c5ded0d9c21f94a5038fd1`.
Every archived evidence member was verified against its SHA-256 manifest. The
archived report is the pre-seal version, without this paragraph. No build caches
or tool binaries are included. Parent checkpoint: `tmp/racer-bottleneck-checkpoint.md`.
