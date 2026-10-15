# Racer GB200 benchmark results, October 13, 2026

## Summary

Four GB200 nodes served a shared synthetic object catalog through direct Racer
Unix-domain sockets (UDS). The final 128 GiB catalog run delivered **14.22335 GiB/s
SHA-verified client goodput** at 16 concurrent operations per node, with zero
client failures. Two preceding unverified runs delivered **14.63834 and
14.82004 GiB/s** of successful payload. These are different verification modes,
not three interchangeable verified results.

The smaller 8 GiB catalog reached **22.22478 and 22.36159 GiB/s verified goodput**
after increasing ciphertext admission from 4 to 8 GiB per node. Both had zero
client failures, but each had **two server-side `ClientWrite/Cancelled` records**.
They are not zero-server-error runs. The earlier C16 run with 4 GiB ciphertext
failed with disk-read admission overload and is not an accepted operating point.

All 20 selected data NVMe devices showed block-layer reads during a separate
sample inside the first large-catalog C16 run, totaling about **14.09 GiB/s**.
That is device-wide block I/O, not measured NAND bandwidth or application-only
traffic. This was **not a network, RDMA, GPU, maximum-capacity, or soak benchmark**.

## Build and environment

Source branch: `racer-v2`. All four application/operator images used source SHA
`948928abd15913ca2f489d3f7195920f64104acf`, published by the GitHub image workflow:

| Image repository under `ghcr.io/azure/` | Platforms | Successful workflow |
|---|---|---|
| `racer-controller` | linux/arm64 | [38431303910](https://github.com/Azure/unbounded/actions/runs/38431303910) |
| `racer-dataplane` | linux/arm64 | [38431308082](https://github.com/Azure/unbounded/actions/runs/38431308082) |
| `racer-loadgen` | linux/arm64 | [38431312678](https://github.com/Azure/unbounded/actions/runs/38431312678) |
| `unbounded-operator` | linux/amd64, linux/arm64 | [38435026788](https://github.com/Azure/unbounded/actions/runs/38435026788) |

Manual `images.yaml` dispatch uses the full workflow SHA as the tag, not the
branch name or `latest` (`.github/workflows/images.yaml:58-60,134-146`). The
operator index digest was
`sha256:411263a5fceff129f2050d8eed83025489ead0d942ada68e7b7015f534ba7dd6`.
Controller startup reported the SHA as version but unknown commit/build-time
fields; those log fields are not independent provenance evidence.

| Node | Architecture / CPUs | Selected data NVMe |
|---|---|---|
| `gb2002159d2424` | arm64 / 144 | nvme0n1, nvme2n1, nvme3n1, nvme4n1, nvme5n1 |
| `gb2002159d2425` | arm64 / 144 | nvme0n1, nvme2n1, nvme3n1, nvme4n1, nvme5n1 |
| `gb2002159d2426` | arm64 / 144 | nvme0n1, nvme1n1, nvme3n1, nvme4n1, nvme5n1 |
| `gb2002159d2427` | arm64 / 144 | nvme0n1, nvme1n1, nvme2n1, nvme3n1, nvme4n1 |

Each selected HFS3T8GEJWX132N device reported 3,840,755,982,336 bytes, about
3.84 TB decimal. Installed selected capacity was 19.204 TB per node and
76.815 TB total, not demonstrated usable cache capacity. Kubernetes-reported
allocatable RAM was roughly 957 GiB on nodes 2424-2426 and 1,693 GiB on 2427;
host-visible `/proc/meminfo` was not the same accounting boundary.

The visible host roots were systemd-nspawn environments. Physical-host mount
visibility was incomplete. After explicit authorization, preparation checked
exact WWIDs, sizes, visible mounts, swaps, descendants, holders, and exclusive
opens; zeroed only nonzero first/last MiB guards; and created 20 exact EUI aliases.
System disks, their partitions, and the root LVM device were excluded. No data
disk interior wipe or format was performed. Runtime startup reported five raw
devices per node, with no file-slab fallback. Raw selection checks and zero-guard
reads are implemented in `cmd/racer-dataplane/src/app/devices.rs:49-151,172-221`.

**Aliases are not reboot-persistent.** They were created in kubelet host `/dev`
tmpfs and verified inside Racer's `/host/dev` mount. Do not replay edge zeroing
after cache data exists. Exact WWID mapping and exclusions are in the archived
`gb200-hardware-checkpoint.md` and preparation script.

All 16 mlx5 InfiniBand ports were DOWN. MANA was physically active but not
established as an application-compatible RDMA rail. `RACER_ENABLE_RDMA=false`
was used; no HCA repair was attempted. Client access was direct UDS, not Gantry
or an HTTP network front end. Warm profiles were mainly local memory/disk reads.
These results do not measure HCA, peer-network, or RDMA throughput. GPUs were not
used by this workload.

## Deployment, tuning, and scope effects

The existing operator was upgraded from `v0.12.0`; there was no isolated
Racer-only mode. Three Racer controller replicas and exactly four dataplanes were
managed in `unbounded-system`, with `ClusterVolume/gb200-bench`. Workload overrides
restricted both Racer workloads to the four named nodes. Pod networking remained
enabled, with peer port 8082 and diagnostics/Prometheus on 9090.

| Setting | Initial | After diagnosed C16 failure |
|---|---:|---:|
| Plaintext admission per node | 4 GiB | 4 GiB |
| Ciphertext admission per node | 4 GiB | 8 GiB |
| Total userspace thread cap | 30 | 30 |
| Observed I/O / crypto workers | 20 / 10 | 20 / 10 |
| Dataplane requests | 16 CPU / 16 GiB | unchanged |
| RDMA | disabled | disabled |

Startup placed the 30 worker threads on CPUs 0-29, NUMA 0, not across both CPU
NUMA nodes. Other runtime/kernel threads are outside that worker count. Byte
budgets are node-wide and divided by I/O-worker count
(`cmd/racer-dataplane/src/app.rs:2396-2426`). Payload admission is not an RSS limit.
The later disk sample observed about 11.6-11.8 GiB dataplane RSS per node.

Only ciphertext admission changed in the tuning step, using UID/resourceVersion
and old-value guards. The operator's config hash triggered the dataplane rollout
(`internal/operator/components/racer/racer.go:263-279`). Runtime checks confirmed
8 GiB ciphertext, 4 GiB plaintext, the same worker plan, five devices, and fully
applied membership. The 512 MiB checkpoint-working-memory cap continued to warn
that oversized cuts would be skipped. Cache warmth was not assumed across that
rollout; a separate warm phase preceded the repeated C16 measurements.

The task also had effects outside Racer; **unrelated resources were not all
unchanged**:

- Existing Net, Machina, Gantry, and Token Refresher images were pinned to
  `v0.12.0`, including Net's init container. Existing ConfigMap payloads compared
  equal after upgrade, but image pins do not freeze templates or CRDs.
- The Machine CRD lost legacy `provider`, `providerID`, and `pxe` fields and
  associated validation rules. The Site CRD had descriptive-text changes.
  No pre-upgrade Machine-object snapshot was captured. Later reads and older
  managed-field entries showed no legacy ownership paths, but that does not
  prove historical values could not have been pruned. This limitation was
  reported and the inherent upgrade accepted; no blind schema rollback occurred.
- Gantry rolled because the new template removed an optional registry Secret
  mount. That Secret was absent. All eight Gantry pods recovered healthy.
- Prometheus was resized from an unschedulable 4 CPU/32 GiB request to
  250m CPU/1 GiB, with limits 1 CPU/2 GiB and `GOMEMLIMIT=1200MiB`. Its 16 GiB
  data emptyDir became disk-backed; scrape configuration stayed unchanged.
- Task-precreating `racer-config` initially blocked the operator's fresh-install
  guard. Only that task-owned default ConfigMap was deleted with exact UID/RV
  preconditions. The operator then created its own identity and config. No
  installation identity, volume, or durable counters were reset or adopted.

Evidence: `gb200-deployment-checkpoint.md`, `gb200-prometheus-checkpoint.md`,
`gb200-cipher8-checkpoint.md`, and before/after snapshots in the local archive.

## Workload and measurement method

Each node ran one direct-UDS loadgen/origin with deterministic 64 MiB objects,
seed `gb200-bench-20261013-v1`, and eight catalog-hash workers. C1/C2/C4/C8/C16
mean concurrent operations **per node**; C16 means 64 across four nodes.

- Small catalog: **128 x 64 MiB = 8 GiB shared unique data**.
- Large catalog: **2,048 x 64 MiB = 128 GiB shared unique data**.

Every origin served the same bytes for the same keys. These are not four disjoint
catalogs or 512 GiB of unique large-catalog data. Generated data is virtual, not
fully allocated in every origin. Object contents depend on index and seed, not
catalog count, so the large catalog retains the initial 128-object subset
(`cmd/racer-loadgen/blob.go:114-170,173-205`). Catalog size exceeding admission
budgets alone does not prove disk service or full catalog coverage.

Verified runs hash received payload with SHA256 and check size/digest before
success (`cmd/racer-loadgen/pull.go:405-425`). Unverified runs use successful
complete operations multiplied by 67,108,864 bytes; that is **successful
unverified payload**, not SHA-verified bytes. The harness keeps both counters
separate and rejects nonzero failure deltas (`gb200-loadgen-phase-v2.py:86-100`);
its offline assertions cover both modes and failure rejection
(`test_gb200_loadgen_phase_v2.py:61-82`).

The harness required exact nodes, Ready pods, expected arguments/images, stable
identities, and all four applied-concurrency gauges. It recorded monotonic
per-pod before/after intervals, checked failures across admission, measurement,
and drain, and restored C0 in cleanup. C1 initially targeted 60 seconds; subsequent
headline phases targeted 120 seconds. Actual boundaries, including scrape
overhead, are in each summary. All listed runs confirmed C0 cleanup, including
the failed C16. No long-duration soak or exhaustive concurrency search was run.

Latency figures below are linear-interpolated histogram-delta estimates, not
exact request ranks. No cluster percentile is formed by averaging node percentiles
(`hack/scripts/racer-s3-benchmark.py:78-97,143-150`).

## Aggregate results

Rates are GiB/s, where 1 GiB = 2^30 bytes. Small-catalog artifact IDs have prefix
`gb200-`; each ID refers to its `summary.json` in the archive.

| Catalog | Cipher GiB/node | Concurrency/node | Verification | Goodput | Result / artifact ID |
|---|---:|---:|---|---:|---|
| 8 GiB | 4 | 1 | SHA256 | 1.613272 | client pass; `c1-verified-01` |
| 8 GiB | 4 | 2 | SHA256 | 3.869381 | client pass; `c2-verified-01` |
| 8 GiB | 4 | 4 | SHA256 | 7.468512 | client pass; `c4-verified-01` |
| 8 GiB | 4 | 8 | SHA256 | 12.898109 | client pass; `c8-verified-01` |
| 8 GiB | 4 | 16 | SHA256 | 18.905973 | **FAILED**, not accepted; `c16-verified-01` |
| 8 GiB | 8 | 16 | SHA256 | 22.224785 | client pass, 2 server cancellations; `c16-cipher8-verified-01` |
| 8 GiB | 8 | 16 | SHA256 | 22.361590 | client pass, 2 server cancellations; `c16-cipher8-verified-02` |
| 128 GiB | 8 | 1 | off | 1.610291 | client pass; `128gib-c1-unverified-01` |
| 128 GiB | 8 | 4 | off | 5.059980 | client pass; `128gib-c4-unverified-01` |
| 128 GiB | 8 | 8 | off | 10.411808 | client pass; `128gib-c8-unverified-01` |
| 128 GiB | 8 | 16 | off | 14.638342 | client pass; `128gib-c16-unverified-01` |
| 128 GiB | 8 | 16 | off | 14.820041 | client pass; `128gib-c16-unverified-02` |
| 128 GiB | 8 | 16 | SHA256 | **14.223347** | client pass; `128gib-c16-verified-01` |

"Client pass" means positive completed work and zero client failure deltas in
the saved phase, not a claim that every server counter stayed zero. The final
verified measurement was 18:27:17.129-18:29:17.629Z. Catalog expansion, cache
warming, verification changes, and dataplane rollout make cross-row comparisons
more than a single-variable concurrency experiment.

### Per-node small-catalog C16 repeats, ciphertext 8 GiB

Node names below have prefix `gb2002159d`. Latencies are milliseconds.

| Run | Node | Verified GiB/s | p50 | p95 | p99 |
|---|---|---:|---:|---:|---:|
| 01 | 2424 | 4.898473 | 201.410 | 473.832 | 498.047 |
| 01 | 2425 | 6.019449 | 193.596 | 471.225 | 495.903 |
| 01 | 2426 | 4.444767 | 211.567 | 475.155 | 498.585 |
| 01 | 2427 | 6.862096 | 190.254 | 469.448 | 494.265 |
| 02 | 2424 | 4.819867 | 206.587 | 475.112 | 498.981 |
| 02 | 2425 | 6.260225 | 188.780 | 469.343 | 494.282 |
| 02 | 2426 | 4.399149 | 212.962 | 476.585 | 501.529 |
| 02 | 2427 | 6.882350 | 190.875 | 469.676 | 494.459 |

Sources: both `gb200-c16-cipher8-verified-*/summary.json`, `pods` results.

### Per-node large-catalog C16 runs

| Mode / run | Node | Payload GiB/s | p50 ms | p95 ms | p99 ms |
|---|---|---:|---:|---:|---:|
| Unverified 01 | 2424 | 3.760261 | 300.492 | 491.249 | 829.674 |
| Unverified 01 | 2425 | 3.986389 | 296.240 | 483.926 | 563.764 |
| Unverified 01 | 2426 | 3.014310 | 312.448 | 663.279 | 934.503 |
| Unverified 01 | 2427 | 3.877381 | 298.206 | 485.781 | 685.292 |
| Unverified 02 | 2424 | 3.813471 | 298.427 | 487.460 | 751.824 |
| Unverified 02 | 2425 | 4.143451 | 296.093 | 481.938 | 498.457 |
| Unverified 02 | 2426 | 2.977846 | 312.675 | 666.453 | 933.291 |
| Unverified 02 | 2427 | 3.885272 | 297.935 | 485.922 | 693.279 |
| Verified 01 | 2424 | 3.700962 | 300.969 | 489.345 | 796.314 |
| Verified 01 | 2425 | 3.903080 | 298.660 | 483.705 | 517.949 |
| Verified 01 | 2426 | 2.882667 | 311.573 | 629.800 | 925.960 |
| Verified 01 | 2427 | 3.736638 | 298.653 | 484.407 | 590.966 |

Sources: `gb200-128gib-c16-{unverified-01,unverified-02,verified-01}/summary.json`.
Node 2426 was consistently slower with higher tail latency. This is an observation,
not proof of a CPU, disk, or network bottleneck.

## Failure accounting

### Initial C16: confirmed admission overload

With 4 GiB ciphertext, C16 had 78 incomplete and 24 HTTP-status failures during
measurement: **102 / 36,554 operations, 0.279%**. Including stabilization and drain,
there were 105 failures. All four nodes were affected; cleanup succeeded.

Protected final-admission records retained all 105 failures, all
`DiskCiphertext/Overloaded`. Per-worker ciphertext quota was 214,748,364 bytes
(4 GiB / 20 workers), while a disk read requested 33,554,960 bytes with about
184-201 MB already charged. Same-request diagnostics connected an exhausted
reservation to a later slice failure after partial delivery. Before-first-slice
failure produces 503; a later failure truncates the subscription instead of
falsely reporting completion (`cmd/racer-dataplane/src/client.rs:360-387,461-477`).
The disk-read test distinguishes recovered reservations from exhausted terminal
failures (`src/read/tests/fill.rs:1612-1653`). No corruption was detected.

This result remains failed after later tuning. It must not be hidden behind
its 18.90597 GiB/s of successfully verified bytes. Full evidence is in
`gb200-c16-diagnosis-report.md` and protected diagnostic artifacts.

### Small-catalog repeats: zero client failures, server cancellations

The 8 GiB-ciphertext repeats had two `ClientWrite/Cancelled` records each:

- Run 01: node 2426 at 17:58:01.707Z; node 2425 at 17:58:49.044Z.
- Run 02: node 2426 at 17:59:50.817Z; node 2424 at 18:00:41.784Z.

These were inside measurement, not drain, and were not health probes. Final
admission, overload, and observed integrity-failure counters stayed zero. A
client-close/completion race is plausible: SDK closes after validating completion,
server marks success after awaiting send, and reactor cancellation can take
precedence over a completed kernel result. The saved records lack send-phase and
CQE detail, so that cause is **not proved**. Sources:
`pkg/racersdk/object.go:555-584`, `cmd/racer-dataplane/src/client.rs:157-175,571-577`,
`runtime/src/reactor.rs:641-658`, and `gb200-request-error-report.md`.

## Disk and Prometheus evidence

During first large-catalog unverified C16, a roughly 60-second sample at
18:19:17-18:20:17Z was wholly inside measurement. Exact-WWID device checks and
`/sys/block/*/stat` deltas showed positive reads on **all 20 data devices**:

| Node | Block-read MiB/s |
|---|---:|
| 2424 | 3,717.474 |
| 2425 | 3,950.471 |
| 2426 | 2,975.053 |
| 2427 | 3,781.948 |
| Sum | 14,424.946, about 14.09 GiB/s |

There were also device-wide writes, about 45.33 MiB/s aggregate. Their issuing
process was not established. The sampler only read sysfs/proc. Device/controller
caching remains possible; neither these counters nor application disk payload
prove NAND access. Source: `gb200-disk-profile-128gib-c16-01.md:19-56`.

Prometheus scraped every **60 seconds**. Fixed-end `[2m]` profiles usually selected
two real scrapes 60 seconds apart inside each run, extrapolating that one-minute
slope. Nodes and components had different sample intervals. Query steps of 15
seconds did not add measurement resolution. Earlier range points or the harness's
supplemental 180-second lookback can include C0, warmup, or adjacent phases.
Missing vectors are not zero. The direct per-pod counter intervals are the
headline throughput source; Prometheus corroborates the same counters rather
than supplying an independent integrity implementation.

| Large C16 profile | Direct payload GiB/s | Prometheus payload GiB/s | App disk-read GiB/s | Sampled source behavior |
|---|---:|---:|---:|---|
| Unverified 01 | 14.638342 | 14.596875 | 14.039323 | residual peer/publication activity |
| Unverified 02 | 14.820041 | 14.887500 | 14.315625 | one peer hit and 16 MiB publication on 2426 |
| Verified 01 | 14.223347 | 14.266667 | 13.716146 | zero origin/peer/publication deltas in sampled pairs |

The final verified profile also reports zero request-error and overload deltas
in its sampled pairs. That is not a full-boundary zero-server-error proof.
Sources: `gb200-profile-large-c16-report.md`,
`gb200-profile-large-c16-repeat-report.md`, and
`gb200-profile-large-verified-evidence.md:5-40`. Large-catalog repeatability is
more representative of disk service than the 8 GiB shared catalog, but neither
establishes maximum capacity or sustained reliability.

The final verified run was about 4.03% slower than the preceding unverified
repeat. Sampled loadgen CPU rose from 3.56 to 11.98 aggregate cores. This is
consistent with verification cost, not a controlled measurement of that cost:
the phases were sequential, a loadgen rollout intervened, and cache/system state
could change. Host CPU includes other workloads. The final profile does not
establish per-worker saturation or a device bandwidth ceiling
(`gb200-profile-large-verified-report.md:24-26`).

## Evidence, reproduction, and final state

Evidence is preserved in **`tmp/gb200-bench-20261013/` in the original repository**,
with the original basenames and run directories. The archive contains 2,528 files
and 22,016,828 bytes, excluding its receipt. File counts, byte counts, and SHA256
hashes match the worktree source; see `archive-receipt.json`. Raw logs and resource
snapshots are not committed. Review the private archive before sharing it.

Key evidence groups:

- `gb200-bench-checkpoint.md`, hardware/deployment/Prometheus checkpoints.
- Every result-table run directory: `summary.json`, command records, raw metric
  snapshots, and `checkpoint.jsonl` with exact mutation and cleanup commands.
- `gb200-c16-diagnosis-*`, `gb200-request-error-*`, `gb200-cipher8-*`.
- `gb200-profile-c4c8-*`, `gb200-profile-cipher8*`, `gb200-profile-large-*`,
  and both disk-profile reports/samplers.
- `gb200-loadgen-phase.py`, `gb200-loadgen-phase-v2.py`, their tests/readmes,
  original manifests, and catalog/verification patches.

Run from the repository root with the matching source SHA and authorized lab
access. The harness uses `tmp/` placement to find `hack/scripts/racer-s3-benchmark.py`;
do not execute it directly from the deeper archive without restoring that layout.
The two helper files can be restored from the archive:

```sh
timeout --signal=TERM --kill-after=10s 30s install -m 0644 \
  tmp/gb200-bench-20261013/gb200-loadgen-phase.py tmp/gb200-loadgen-phase.py
timeout --signal=TERM --kill-after=10s 30s install -m 0644 \
  tmp/gb200-bench-20261013/gb200-loadgen-phase-v2.py tmp/gb200-loadgen-phase-v2.py
```

The following **starts load** and is for a separately authorized reproduction,
not a health check. First confirm C0/drain, expected catalog and verification
arguments, all origins Ready, no mixed rollout, and the intended dataplane budget.
Use a new output directory, never overwrite saved evidence:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B tmp/gb200-loadgen-phase-v2.py \
  --catalog-blobs=2048 --seed=gb200-bench-20261013-v1 --verify=true \
  --concurrency=16 --seconds=120 --stabilize=0 --prom-window=180 \
  --output=tmp/gb200-repro-128gib-c16-verified-01
```

For a controlled sweep, run one phase at a time with concurrency 1, 2, 4, 8,
then 16, changing only that flag and the fresh output name. Inspect
`complete`, failure deltas, and `cleanup_confirmed` before proceeding; stop on
failure and diagnose. The small catalog requires matching live count128/verifytrue;
the large unverified comparison requires matching count2048/verifyfalse. Changing
catalog or verification requires its own C0-gated loadgen rollout, not a harness
flag that disagrees with the running pods. Do not reapply old whole manifests or
replay disk initialization.

The final verified summary records `cleanup_confirmed=true`. The separate saved
health collection completed **18:30:37.399Z** and independently confirmed control
C0 and direct applied-concurrency/in-flight gauges of **0/0 on all four loadgens**.
Four loadgen/origin pods, four dataplanes, and three controllers were Running and
Ready with zero restarts. Prometheus was Ready; all four nodes reported Ready
with no memory, disk, or PID pressure. Node-condition heartbeat timestamps lagged
the read, so this was not a simultaneous physical-host health probe.
Evidence: `gb200-profile-large-verified-health.json` and
`gb200-profile-large-verified-report.md:58-77`.

That is the last independently captured safe state, not a promise about later
cluster state. Leave origins
alive and load paused. No result here authorizes automatic load resumption or
concurrency escalation.
