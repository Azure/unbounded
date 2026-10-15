# Racer GPU benchmark: aborted canaries

## Result

Initial cold activation opened all 15 approved raw disks: seven on node03 and
eight on node13. The warm run and later live inspection had only six plus eight;
node03 skipped one disk with unexpected kernel child partitions. Historical
writes on all 15 do not establish 15 active disks. Both GPU dataplanes deployed,
and inter-node RDMA write activity was measured. A reliable throughput
sweep did **not** complete: both concurrency-one canaries hit client deadlines.
Each was stopped automatically, paused, and drained. No higher-concurrency runs
were attempted after the second failure.

At 18:45:26 UTC on October 7, 2026, both load generators reported applied
concurrency zero and zero in-flight operations. Origins remain running at zero
load. Operator, three controllers, two dataplanes, and two origins are Ready.
All nodes remain Ready. gpu-07-03 nvme0n1 still has the protected
`tau-model-cache` UUID, and its PV remains Bound. Its kernel disk statistics
show zero writes. Every approved disk has recorded historical writes. The
18:53 UTC read-only followup confirmed concurrency zero and zero in-flight
operations. At that point the reduced raw-device set was a high-risk stopping
condition, with C0 retained pending user approval. The scoped recovery below was
later approved and completed; load remains paused.

## Builds and deployment

All four images use tag `4bb2327cb5ef9a2ce317473f7b634f4b22893cd0` under
`ghcr.io/azure`. Only original `racer-v2` was pushed or used for image builds.

| Image | GitHub Actions run | Registry digest |
|---|---|---|
| racer-dataplane | [37665144114](https://github.com/Azure/unbounded/actions/runs/37665144114) | `sha256:cee310f151d16b61adbda8f5ef8c157b878745d60c6e7dd2e6c4a948642f154c` |
| racer-loadgen | [37665148287](https://github.com/Azure/unbounded/actions/runs/37665148287) | `sha256:005684b357934411c05605c0b5c726ec43012d357bd4a8902cfd24b931043cab` |
| racer-controller | [37665152916](https://github.com/Azure/unbounded/actions/runs/37665152916) | `sha256:f48332276acdeaecf772e896cbe03efc5b4f7bb8f3ee4d24cf08754326f63bf4` |
| unbounded-operator | [37665157987](https://github.com/Azure/unbounded/actions/runs/37665157987) | `sha256:56eeb1b35b3329d869815181a7725e3d62ae941571ea6aa703c60e0ec5b9cc42` |

All runs succeeded. The operator required a second attempt after Ubuntu's
package server failed to deliver ca-certificates; its Go build had succeeded.
Registry HEAD checks confirmed all images before the matching upgrade.

The updated operator needed PDB list/watch permissions for its cache. The
vanilla RBAC template and regression assertion were fixed in original commit
`1c4236c76`, tested, and applied. An operator restart after the RBAC correction
allowed the controller rollout to finish. This bootstrap-manifest fix did not
require changing image tags. See the session checkpoint for earlier deployment
failures and recovery.

Racer is restricted to gpu-07-03 and gpu-07-13 by exact node-name affinity and
GPU taint tolerations. Both AKS nodes are excluded from membership. The shared
site is `racer-gpu-fabric-07`; explicit NIC mappings use mlx5_00 through mlx5_07,
port 1, rails 0 through 7. Host-network peer/diagnostic ports are 8082/9090.

Dataplane requests are 4 CPUs and 8 GiB; limits are 16 CPUs and 64 GiB. The
16-thread cap results in 10 I/O and five crypto workers, currently on NUMA node
0. This is not an all-core or balanced-NUMA saturation setup. Initial cold
startup logs confirmed seven raw disks on node03 and eight on node13, with no
slab-file fallback (`gpu-07-03-startup.log:15` and `gpu-07-13-startup.log:15`
under the artifact directory). The warm startup had six plus eight; later live
logs and unique-device descriptors confirmed that reduced set
(`benchmark/post-warm-dp-gpu-07-03.log:14-18` and
`read-only-followup-1854.txt:13-35`). Checkpoint scratch is capped at 512 MiB;
oversized cuts can be skipped.

## Workload and measurement

Both nodes run direct UDS loadgen against ClusterVolume `racer-bench` with:

- 128 blobs of 67,108,864 bytes each: an 8 GiB shared catalog.
- Seed `gpu-benchmark-20261007-cold-01`, shuffle selection, SHA-256 verification.
- One blob acquisition at a time per worker, one worker per node in both runs.
- Pull timeout two minutes; the observed internal failures occurred near 30 seconds.
- Projected ConfigMap concurrency control; origins stay alive during pauses.
- Loadgen limits 8 CPUs/4 GiB per node; no GPU resources requested.

Each run planned a 60-second window after both nodes applied concurrency one.
The runner checked errors/readiness/pressure about every 15 seconds and stopped
on the first observed load error. The tables use the **before-to-drained**
interval, including control propagation and drain time, since the normal end
snapshot was not reached. They are aborted-canary accounting, not steady-state
capacity estimates or successful 60-second runs.

| Run | Node | Interval seconds | Verified GiB | Verified MiB/s | Successful blobs | Failed blobs | Mean successful blob latency ms |
|---|---|---:|---:|---:|---:|---:|---:|
| Cold-start/fill, aborted | gpu-07-03 | 69.668 | 42.375 | 622.84 | 678 | 0 | 62.01 |
| Cold-start/fill, aborted | gpu-07-13 | 69.668 | 1.3125 | 19.29 | 21 | 2 | 86.00 |
| Warm/recovered, aborted | gpu-07-03 | 74.699 | 2.3125 | 31.70 | 37 | 2 | 77.23 |
| Warm/recovered, aborted | gpu-07-13 | 74.699 | 2.0000 | 27.42 | 32 | 2 | 230.35 |

Aggregate verified goodput was 642.13 MiB/s in the first accounting interval and
59.12 MiB/s in the second. These rates include local cache hits and must not be
reported as RDMA throughput. Mean latency excludes failed blobs, which each
consumed roughly 30 seconds; raw histograms include both result classes.

The first pass began with an unused seed/catalog but warmed during the run.
Before the second pass, load was paused/drained and the supported ConfigMap
changed ciphertext budget from 2 to 8 GiB and registered budget from 1 to 2 GiB.
Plaintext stayed at 2 GiB. The resulting managed dataplane rollout preserved raw
cache contents and identity; no cache reset or disk wipe occurred. Node03
skipped the disk with child partitions and incompatible checkpoint sequences
252 and 251. Recovery, the reduced disk set, and memory-cache loss mean the
second pass is not a pure warm-memory steady state.

## RDMA and disk evidence

| Run | Node | IB TX bytes | IB RX bytes | RX RDMA write counter delta | Raw disk read bytes | Raw disk write bytes |
|---|---|---:|---:|---:|---:|---:|
| Cold-start/fill | gpu-07-03 | 34,225,592 | 34,225,592 | 8,194 | 30,954,908,160 | 8,590,196,736 |
| Cold-start/fill | gpu-07-13 | 34,225,592 | 34,225,592 | 8,194 | 0 | 1,476,440,064 |
| Warm/recovered | gpu-07-03 | 51,360,760 | 171,073,332 | 40,970 | 16,777,728 | 2,566,992,384 |
| Warm/recovered | gpu-07-13 | 171,073,332 | 51,360,760 | 12,291 | 318,776,832 | 2,181,104,640 |

Each sender's TX delta exactly matches the other node's RX delta in these
snapshots. RX RDMA-write counters increased; RX RDMA-read counters stayed zero,
consistent with Racer's `IBV_WR_RDMA_WRITE` implementation
(`cmd/racer-dataplane/verbs/native/verbs.c:241-251`). Request CQE errors and
transport-retries-exceeded counters stayed zero. This establishes inter-node
RDMA write activity during the workload, beyond membership or open-device
evidence. It does not establish that every successful blob traveled over RDMA.
Counters cover entire ports and include protocol overhead and possible unrelated
traffic. No per-process RDMA payload-byte counter exists in this build.

Do not add RX and TX to estimate aggregate throughput: that double-counts traffic.
Summed TX rates were about 0.94 MiB/s and 2.84 MiB/s for the two full accounting
intervals. Client goodput was much higher, demonstrating local service dominates.
There is no exact exported HTTP-fallback byte counter either. HTTP page timing
counts are enabled only for selected direct HTTP Page operations
(`src/peer.rs:45-67` under `cmd/racer-dataplane`), not all fallbacks, so absence
of that count cannot prove absence of fallback.

| Run | Node | Disk read operations | Mean read ms | Disk write operations | Mean write ms |
|---|---|---:|---:|---:|---:|
| Cold-start/fill | gpu-07-03 | 50,215 | 1.863 | 14,232 | 2.182 |
| Cold-start/fill | gpu-07-13 | 0 | n/a | 1,250 | 2.141 |
| Warm/recovered | gpu-07-03 | 17 | 1.588 | 2,953 | 2.232 |
| Warm/recovered | gpu-07-13 | 307 | 1.713 | 2,122 | 2.175 |

These are kernel block-I/O averages, not application latency distributions.
All 15 approved disks had historical writes by final inspection, but only 14
remained open in the warm run and later live inspection. Disk bytes include
encrypted record overhead.

### Dataplane payload accounting

These are before-to-drained counter deltas, not rates. Published/read bytes use
`racer_disk_class_{published,read}_payload_bytes_total`, all with
`classification="owned"`; both `nonowned` counters stayed zero. Requests and
errors use `racer_requests_total` and `racer_request_errors_total`.

| Run | Node | Interval seconds | Published payload bytes | Read payload bytes | Request errors | Requests |
|---|---|---:|---:|---:|---:|---:|
| Cold-start/fill | gpu-07-03 | 69.6681872 | 8,589,934,592 | 30,953,963,520 | 0 | 678 |
| Cold-start/fill | gpu-07-13 | 69.6681872 | 1,476,395,008 | 0 | 2 | 23 |
| Warm/recovered | gpu-07-03 | 74.6992865 | 2,566,914,048 | 16,777,216 | 2 | 39 |
| Warm/recovered | gpu-07-13 | 74.6992865 | 2,181,038,080 | 318,767,104 | 2 | 34 |

Sources: artifact `benchmark/{cold,warm}-c1-{before,drained}.json:2` for times
and matching `*-dp-gpu-07-{03,13}.prom:34-36,219-220,234-235` for counters.
These application disk payload bytes are not kernel block-I/O bytes, RDMA
bytes, or verified client goodput. They do not measure link throughput.

## Formulas and source contracts

- Verified goodput: delta `racer_loadgen_verified_bytes_total` / interval / 2^20.
  Only successful fully verified batches count (`cmd/racer-loadgen/metrics.go:35-46`).
- Mean successful blob latency: delta success
  `racer_loadgen_pull_duration_seconds_sum` / delta success count x 1,000.
- IB bytes: sum across mlx5_00 through mlx5_07 of delta `port_xmit_data` or
  `port_rcv_data` x 4. Linux documents these as octets divided by four:
  [sysfs InfiniBand ABI](https://github.com/torvalds/linux/blob/v6.8/Documentation/ABI/stable/sysfs-class-infiniband).
- Disk bytes: delta sectors x 512 from `/sys/block/nvmeNn1/stat`, excluding
  node03 nvme0n1. Mean I/O ms: delta read/write milliseconds / delta completed
  read/write operations, summed over approved disks. No counter resets occurred
  inside either interval; dataplane metric resets between runs get new baselines.
- `racer_disk_class_{published,read}_payload_bytes_total` distinguish application
  payload from block I/O (`cmd/racer-dataplane/src/telemetry.rs:1588-1594`).

## Blocker and safe final state

### Unexpected kernel partitions

Node03 `/dev/nvme5n1`, serial `S64HNN0XA09741`, EUI
`3634483058a097410025384e00000001`, was skipped because it had child partitions.
The kernel reported `nvme5n1: AHDI p2 p3 p4`; sysfs showed three overlapping
partition ranges. The raw first sector starts with Racer's `RCRPAGE1` version-4
header, and bytes at Atari partition-entry offsets reproduce the sysfs starts
and sizes (`read-only-followup-1854.txt:37-74`). This is strong evidence of false
Atari/AHDI detection of raw cache data, not proof of the exact scan trigger.
The running distribution's parser patches were not audited. There is no proof
that this anomaly caused the canary deadline failures.

The code rejects devices with child partitions
(`cmd/racer-dataplane/src/app/devices.rs:158-169`), starts raw placements at
offset zero (`:250-282`), and defines the observed header in
`cmd/racer-dataplane/src/store.rs:964-968`. Empty holders and no child mounts in
the dataplane namespace do not establish that there are no outer-host users.
Do not repair partitions, reset or wipe cache, force a partition reread, or
bypass device safety checks without explicit user approval. Keep C0 and zero
in-flight operations while this high-risk condition remains unresolved.

### Canary deadlines

First-run node13 failures were incomplete delivery and HTTP 503 after internal
deadlines. Node03's failure journal contained 6,726 observations dominated by
ciphertext admission pressure: 184,550,048 bytes used of 214,748,364 per worker,
requesting another 33,554,960 bytes. After the budget change, both nodes still
had two failed blobs; protected terminal records show 16/32 MiB partial delivery
followed by `NextSlice`, `ClientWrite`, or `FirstSlice` deadline failures.
Plaintext admission pressure was then recorded at 201,326,592 of 214,748,364
bytes per worker while requesting 16,777,216 bytes.

These are observed pressure and deadline facts, not proof that every deadline
has the same root cause. There were no reported verification mismatches or
node-pressure conditions. A higher-concurrency sweep would conceal the blocker,
so it was not run. No completed steady-state or saturation result is claimed.

Final ConfigMap concurrency is zero; both origins remain Ready and drained.
The dataplanes remain Ready with the larger ciphertext/registered budgets and
64 GiB limit. Final RSS was about 6.1 GiB each. The 18:53 UTC followup confirmed
C0, zero in-flight operations, and the protected PV still Bound/Retain with
zero protected-disk writes (`read-only-followup-1854.txt:124-151`). Resume only
after user-approved recovery of the disk anomaly and diagnosis of the
concurrency-one deadlines, with a fresh baseline and an explicit bounded phase.
Do not wipe raw cache, remove installation identity, or touch the excluded disk.

## Reproduction and artifacts

Artifacts are preserved inside the project at
`tmp/racer-gpu-benchmark-20261007-artifacts/`. This directory contains session
scripts, loadgen manifest, full checkpoints, rendered operator manifests, and
`benchmark/` with before/applied/drained JSON, raw Prometheus snapshots, per-rail
hardware counters, final resource state, and post-warm protected failure logs.
`read-only-followup-1854.txt` records the later six-plus-eight live disk set,
partition anomaly, and C0 safety checks. It made no remote changes.
No Secret objects or private identity contents were collected.

The bounded runner commands were:

```sh
timeout --signal=TERM --kill-after=10s 280s python3 tmp/bench.py phase cold-c1 1 60
timeout --signal=TERM --kill-after=10s 280s python3 tmp/bench.py phase warm-c1 1 60
```

The runner writes C0 in a finally block and verifies drain. Both commands exited
nonzero on measured load errors, not timeout. Their before-to-drained intervals
are reported above. Do not replay them unchanged as a recovery step.

## User-approved scoped disk recovery

The user approved recovery of **only** gpu-07-03 `/dev/nvme5n1`, serial
`S64HNN0XA09741`, EUI `3634483058a097410025384e00000001`, capacity
3,840,755,982,336 bytes, diskseq 15. At 19:26:03 UTC, an optimistic
resourceVersion/UID/old-value-tested patch removed only this target from the
Node's `racer.unbounded-cloud.io/block-devices` selector. All other annotations
were unchanged. The six other selected disks remained selected. No dataplane
was restarted or rolled.

At **19:28:53 UTC on October 7, 2026**, the approved repair completed:

- Fresh checks confirmed target identity, C0 and zero in-flight operations,
  the selector exclusion, and no visible target/child mounts, swaps, holders,
  or other raw-device FDs. One `O_RDWR | O_EXCL | O_CLOEXEC` whole-device FD
  was held throughout; a second exclusive claim returned `EBUSY`. The accepted
  limitation remains that unseen nonexclusive openers cannot be ruled out.
- Original first and last 1 MiB regions were backed up locally, hashed, and
  fsynced with mode `0600` before the write gate was released. The same open FD
  was rechecked before writing.
- Two full-length `pwrite` calls zeroed exactly `[0, 1048576)` and
  `[3840754933760, 3840755982336)`: **2,097,152 bytes written**. Of those,
  1,044,469 byte values changed; the last 1 MiB was already zero. `fsync`
  succeeded, and both full regions read back as zeros.
- Exactly one same-FD `BLKRRPART` (`0x125f`) returned success. No child
  partitions remained. Target identity and diskseq 15 were unchanged. There
  was no forced retry, discard, other-disk reread, reboot, or cache readmission.

Cleanup completed at **19:29:01.395899 UTC**: the bounded diagnostic pod was
deleted. The final read-only check confirmed **six plus eight active raw
disks**, with no node03 target or protected-disk raw FD. Both load generators
remained at C0 with zero in-flight operations. Dataplane UIDs, containers,
images, startup times, readiness, and zero restart counts were unchanged.
Protected node03 `nvme0n1` retained UUID
`d55cd903-d42e-4625-82a3-d5a8c5b2bab3`, zero write/discard counters, and its
unchanged Bound/Retain PV `glm-model-cache-gpu-07-03`.

The target **stays fenced out**. This repair removed the false kernel partition
state; it is not a durable raw-format fix or a secure erase of the whole cache.
Do not readmit it to the offset-zero writer or resume load. A durable reserved
prefix/suffix layout, cache invalidation requiring separate approval, and
diagnosis of the concurrency-one (C1) deadlines remain outstanding. C0 remains owned by
the parent operation. The earlier anomaly and aborted benchmark results above
are historical evidence and are not replaced by this recovery.

Recovery evidence is preserved separately at
`tmp/racer-gpu-disk-recovery-20261007-artifacts/`. Sources:
`fence-phase-checkpoint.txt` (19:26:05 and 19:26:34 entries),
`repair-target-checkpoint.txt:23-56`, and the timestamped
`repair-target-journal.jsonl`. The exact executed script is `repair-target.py`;
**do not replay it**. Original endpoint artifacts are restricted local data,
not committed report attachments:

| Original artifact | SHA-256 |
|---|---|
| `repair-original-0.bin` | `14c9f72bd3fc7725f6ec2d2f5edb60965f0b41f634d9379c26176d893df8eff6` |
| `repair-original-3840754933760.bin` | `30e14955ebf1352266dc2ff8067e68104607e750abb9d3b36582b8af909fcb58` |
