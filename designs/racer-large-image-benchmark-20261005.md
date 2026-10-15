# Racer larger-than-memory image benchmark, October 5, 2026

## Configuration and outcome

Context `joolshev-scale-test`, namespace `unbounded-system`, 1,500 nodes.
The workload was expanded from one 0.505664 GiB image to one **10.777644 GiB
image**, with **16 GiB per-worker slab files**. The final workload remains at
concurrency 8, layer concurrency 1, and `--verify=false`. Gantry remains at
CPU limit 4 and GOMAXPROCS 4. No application code or container images changed.

| Setting | Before | After |
|---|---|---|
| Catalog | One image | One image |
| Layers / nominal layer bytes | 8 / 67,108,864 | 11 / 1,073,741,824 |
| Seed / jitter / profile | benchmark-v1 / 0.2 / shuffle | Unchanged |
| Logical image bytes | 542,952,548 | 11,572,407,586 |
| Plaintext / ciphertext budgets per node | 4 / 6 GiB | Unchanged |
| Slab size per I/O worker | 1 GiB | 16 GiB |
| Five-worker logical slab capacity per node | 5 GiB | 80 GiB |
| Whole-image pull timeout | 2 minutes | 5 minutes |

The larger image exceeds even the combined nominal 10 GiB cache budgets.
Those budgets are separate resource pools, not an additive plaintext-content
capacity. Image sizing is metadata-only calculation from
`cmd/racer-loadgen/image.go:77-99,177-197`; startup still generates and hashes
the catalog independently of per-pull verification.

Each worker receives the full slab size (`cmd/racer-dataplane/src/app.rs:938-944`).
The catalog needs 698 pages including manifest/config. Three full-page records
fit each 64 MiB segment. A 16 GiB slab, excluding two reserve segments, has
762 full-page slots, sufficient for the entire image in one file even though
placement does not actually put the entire image on every node. Capacity logic:
`cmd/racer-dataplane/src/store/checkpoint.rs:589-604`; capacity assertion:
`cmd/racer-dataplane/src/store/tests.rs:688-697`.

## Storage safety and authoritative changes

- `racer-dataplane-config.data.RACER_SLAB_BYTES`: `17179869184`.
- Active `racer-dataplane` entry in
  `unbounded-component-overrides.data["racer-v2.yaml"]`: `reset-slabs` hostPath
  changed to `/var/lib/racer-disk-benchmark-20261005/slabs`.
- The identity directory and original slab directory
  `/var/lib/racer-reset-20261002-f85/slabs` were retained unchanged.
- Live loadgen DaemonSet args changed to the table above. Startup concurrency
  remains zero with projected ConfigMap control and node caps intact.

Existing nonempty slabs reject a mismatched size rather than resizing
(`cmd/racer-dataplane/alloc/src/slab.rs:267-274`, preservation test `:913-925`).
A new directory avoids truncation. Rollback must restore **both size and path**
before replacing dataplanes; restoring just the size would fail again.

All 1,500 nodes passed a read-only available-space check at 18:18:07 UTC.
Minimum available space was 97,550,442,496 bytes (90.851 GiB), on
`aks-ddsv6-84072342-vmss0000dj`. Fully allocating the new 80 GiB capacity there
would leave about 10.851 GiB at that snapshot, before other growth. Slabs are
OS-root-backed ext4, not a separately mounted temporary data disk. Sparse
files do not reserve physical capacity; keep free-space monitoring in place.

Post-rollout checks covered all 1,500 pod paths and one physical node from
each of four cohorts. Every sampled node had five 16 GiB files, initially
zero allocated blocks, five I/O workers, three crypto workers, unchanged
4/6 GiB budgets, and no startup errors. STATX_DIOALIGN reported 4-byte memory
alignment and 512-byte offset/length alignment on all sampled files.

## Parent-owned rollout checkpoints (UTC)

Each mutating command and its result was saved in `checkpoint.jsonl` with
timestamps, phase bounds, and next actions. Commands used external TERM
timeouts with a 10-second kill grace; deletion batches were explicitly
enumerated and used `--wait=false`, 100 names per batch, five concurrent batches.

1. 18:16:14: small-image baseline, all 1,500 C8.
2. 18:18:43: control C8 -> C0. At 18:21:17 all 1,500 had applied C0 and
   aggregate in-flight was zero.
3. 18:21:34: changed slab ConfigMap, persistent override, and paused loadgen
   template. Dataplane remained OnDelete; loadgen temporarily used OnDelete.
4. A broad string replacement briefly changed the unused podnet override too;
   restored that entry at 18:21:46. No podnet DaemonSet exists. Active dataplane
   identity and intended new slab path were verified before deletion.
5. Initial canary deletion rejected unsupported `-o json` before deleting
   anything. Live state was checked; corrected to `-o name`. Canary
   `racer-dataplane-286c2` became Ready with zero restarts and five I/O workers.
6. 18:22:51-18:23:46: replaced the remaining 1,499 dataplanes. All were Ready
   by the 18:25:14 snapshot.
7. 18:24:11-18:25:06: replaced all 1,500 loadgens while C0 remained active.
8. 18:28:14-18:28:30: all components Ready, zero restarts/terminating pods;
   identical large-image args on all readers; Gantry and origin local endpoint
   coverage both 1,500 nodes; all readers C0 with zero in-flight. Restored
   loadgen RollingUpdate with maxUnavailable 10%, maxSurge 0.
9. 18:28:40: resumed C8; all 1,500 observed at C8 by 18:31:14. Cold startup
   and convergence were excluded from the final measurement.
10. 18:39:00 endpoint: all components healthy, load unchanged at C8.

## Benchmark results

Baseline is the two-minute rate ending approximately 18:16:14; large-image
measurement is the fixed five-minute window **18:34:00-18:39:00 UTC**. Both
use 1,500 nodes, C8, layer concurrency 1, and disabled SHA verification.

| Metric | Small, memory-resident image | Larger-than-memory image |
|---|---:|---:|
| Application received throughput | 9.2248 TiB/s | **1.71844 TiB/s** |
| Mean received throughput per node | 6.297 GiB/s | **1.17312 GiB/s** |
| Successful image pulls/s | 18,680.89 | **160.97** |
| Mean completed-image latency | 0.6424 s | **74.69 s** |
| Node CPU busy, median | 91.38% | **30.93%** |
| Completed pull errors in measurement | 0 | **0** |

Received throughput decreased approximately **81.4%**. Pull rate and latency
are for differently sized images, so do not interpret their ratios as pure
transport regressions. RX counts streamed bytes, including failed/partial
operations, not verified goodput or physical NIC throughput. Completed-image
accounting can cross the measurement boundary. Verified-byte rate is zero.

Live latency buckets bracket both p50 and p95 in **(60,120] seconds**;
interpolated estimates are approximately 89.7 and 118.0 seconds, respectively,
but the wide bucket makes those estimates coarse. Mean latency excludes
unfinished operations. All RX counters advanced; no queried counter resets.
Success endpoint differences advanced on all nodes, while `increase[5m]`
was positive for 1,499: one slow node's increment straddled the first scrape
boundary. That node continued receiving at 0.317 GiB/s with eight in flight.

| Large-image source or error event | Rate/s |
|---|---:|
| Plaintext lookup hits / misses | 14,420.55 / 113,768.68 |
| Peer acquisitions | **14,218.66** |
| Disk hits | **19.10** |
| Origin fills / origin bytes | **0 / 0 B/s** |
| Disk publications / dirty discards | 0 / 0 |
| Internal request errors | 0.5661 |
| Peer-decrypt corrupt rejections | 0.10135 |

This is a **peer-acquisition and local-flight-reuse benchmark, not a disk
saturation benchmark**. Noncandidate readers do not enqueue new disk copies
(`cmd/racer-dataplane/src/read/fill.rs:1149-1173`). Larger slabs change capacity,
not placement policy. Total publications were 2,094 across 1,104 nodes;
2,094 = 3 x 698 is consistent with three dispersed copies of the catalog,
but publication counters do not prove current distinct replica inventory.

Peer acquisitions at the 16 MiB page upper bound account for approximately
one eighth of delivered bytes; plaintext misses are also about eight times
peer acquisitions. This is consistent with C8 readers sharing page flights.
Flight completion bypasses a fresh source event (`read/fill.rs:634-664`).
The 11.25% plaintext probe hit fraction is not an exhaustive consumer hit ratio.

### Errors and resource limits

Eight incomplete-layer pull errors occurred on one loadgen during warmup,
before the measurement and before all readers applied C8. Exact warning at
18:29:36.225: `reason=incomplete kind=layer http_status=200`. Rate-limited
logs do not individually timestamp all eight. No new pull errors occurred
in the measured five minutes. Internal request errors and peer-corrupt
rejections remained nonzero; their underlying causes and any relationship
to warmup truncation were not established. With verification disabled,
successful pulls do not prove end-to-end content integrity.

Dataplane/Gantry/loadgen CPU averaged approximately 1.167/0.518/0.372 cores
per pod in the asynchronous metrics-server snapshot. Node-busy p95 was
42.58%; system plus softirq was 66.16% of nonidle CPU. There is no evidence
of broad CPU exhaustion, Gantry's 4-core quota saturation, or disk saturation.
Aggregate eth0 RX/TX was 484.14/484.43 decimal GB/s; accelerated interfaces
were not double-counted. Physical NIC saturation and a unique throughput
bottleneck remain unproven.

## Evidence and validation

Raw checkpoints, configuration snapshots, all-node capacity results, physical
slab checks, fixed-time PromQL responses, resource snapshots, and bounded logs
are archived under the project-local `tmp/racer-large-image-evidence-20261005.tar.gz`.
They are operational artifacts, not committed application source. The archive
includes `tmp/analysis/final-report.md` for detailed coverage and caveats.
No application tests or image build were needed for these live configuration
changes. `make fmt` ran gofumpt, then the installed golangci-lint panicked
because it was built with Go 1.26 and encountered Go 1.27 code. No tracked
source changes resulted. The expanded workload remains running.
