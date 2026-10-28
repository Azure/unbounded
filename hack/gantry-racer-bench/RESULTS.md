# Gantry vs Gantry + Racer: image pull benchmark

Date: 2026-10-28. Base: origin/main d017881b5, plus the fixes listed under "Changes made" (draft PR
https://github.com/Azure/unbounded/pull/1016).

## Setup

- AKS cluster `joolshev-scale-test`, 1497 worker nodes. Every node is 8 vCPU / 32 GiB with a 128 GB local OS disk.
  - 447 Standard_D8ads_v5 and 550 Standard_D8d_v5. Racer uses the 43 GiB local temp disk as a raw block device.
  - 500 Standard_D8ds_v6. These have no extra local disk, so Racer uses 6 GiB slab files per I/O worker on the
    OS disk.
- Origin: `distribution` 3.0.0 over plain HTTP on the 3 system nodes, one replica per node. Every image is pushed
  to all 3 replicas. (Some earlier runs used 1 replica; they are listed separately.)
- Images are random data, so they don't compress or dedupe. Every run uses a new image.
  - 40 GB: 40 layers of 1 GB (10^9 bytes; 37.3 GiB total).
  - 8 GiB: 8 layers of 1 GiB.
  - 1 GiB: 4 layers of 256 MiB.
  - Smoke: 2 layers of 64 MiB.
- Each run starts a DaemonSet that runs `crictl pull` on every worker at the same second (start skew under
  0.05s). The time reported is from that start until the pull finishes, so it includes the containerd unpack.
- containerd uses gantry first, then falls back to the origin directly (`certs.d/bench.registry.local/hosts.toml`).
- Same gantry image in both modes (main + retry/resume fix). Racer mode is on when ClusterCache `gantry` exists.
- Racer: one dataplane per node, RDMA off, tuned config (see "Racer config").

## Full results (3 origin replicas, 1497 nodes, all runs 1497/1497 succeeded)

| Image | Mode | Run | p50 | p90 | p95 | p99 | max | mean | Origin egress |
|---|---|---|---|---|---|---|---|---|---|
| 40 GB | Gantry P2P | 40g-c | 279.9s | 320.4s | 333.3s | 362.1s | 408.0s | 281.6s | 299.2 GiB |
| 40 GB | Gantry P2P | 40g-e | 280.0s | 327.2s | 343.6s | 369.6s | 404.9s | 284.3s | 299.2 GiB |
| 40 GB | Gantry + Racer | 40g-a | 233.3s | 278.3s | 296.1s | 323.3s | 352.8s | 234.6s | 61.7 GiB |
| 40 GB | Gantry + Racer | 40g-b | 228.2s | 273.7s | 290.3s | 312.6s | 328.6s | 230.9s | 62.5 GiB |
| 8 GiB | Gantry P2P | o3a | 75.9s | 81.2s | 82.6s | 85.0s | 96.3s | | 64.1 GiB |
| 8 GiB | Gantry P2P | o3b | 76.2s | 80.7s | 81.9s | 84.8s | 95.7s | | 64.1 GiB |
| 8 GiB | Gantry + Racer | o3c | 54.1s | 61.6s | 63.4s | 66.2s | 72.1s | | 12.5 GiB |
| 8 GiB | Gantry + Racer | o3d | 52.2s | 58.8s | 60.8s | 64.9s | 73.8s | | 12.9 GiB |
| 1 GiB | Gantry P2P | sa | 15.5s | 16.2s | 16.6s | 17.5s | 18.4s | | 8.0 GiB |
| 1 GiB | Gantry P2P | sb | 15.2s | 16.3s | 16.7s | 17.4s | 17.8s | | 8.0 GiB |
| 1 GiB | Gantry + Racer | sc | 14.6s | 16.7s | 17.1s | 17.7s | 18.7s | | 1.6 GiB |
| 1 GiB | Gantry + Racer | sd | 15.8s | 17.7s | 18.1s | 19.2s | 20.3s | | 1.6 GiB |
| 128 MiB | Gantry + Racer | smoke | 8.6s | 9.7s | 9.9s | 10.0s | 11.0s | | 0.2 GiB |

### Summary

| Image | Racer vs Gantry P2P, p50 | p99 | max | Origin egress |
|---|---|---|---|---|
| 40 GB | 18% faster (231s vs 280s) | 13% faster | 16% faster | 4.8x less (1.7x image vs 8.0x image) |
| 8 GiB | 30% faster (53s vs 76s) | 22% faster | 24% faster | 5x less (1.6x image vs 8.0x image) |
| 1 GiB | same (15s) | 5-10% slower | 5-10% slower | 5x less |

### 40 GB results by node pool (p50 / p90 / max)

| Run | D8ads_v5 (raw disk) | D8ds_v6 (slab on OS disk) | D8d_v5 (raw disk) |
|---|---|---|---|
| Gantry 40g-c | 282.6 / 302.9 / 342.5s | 265.5 / 328.9 / 368.6s | 284.7 / 325.3 / 408.0s |
| Gantry 40g-e | 282.5 / 303.6 / 336.6s | 269.3 / 332.2 / 380.3s | 284.9 / 336.5 / 404.9s |
| Racer 40g-a | 239.2 / 257.4 / 283.8s | 217.0 / 293.1 / 352.8s | 233.7 / 278.3 / 335.7s |
| Racer 40g-b | 233.6 / 250.3 / 268.6s | 208.2 / 285.5 / 325.9s | 227.0 / 276.3 / 328.6s |

D8ds_v6 nodes have the best p50 but the widest spread in both modes. (Inference: they have a faster NVMe OS disk,
but in Racer mode the slab cache shares that disk with containerd.)

### Same tests with a single origin replica (8 GiB)

| Mode | Runs | p50 | p95 | max | Origin egress |
|---|---|---|---|---|---|
| Gantry P2P | k, l | 110.2s, 94.8s | 116.7s, 100.2s | 123.4s, 108.4s | 64.2 GiB |
| Gantry + Racer | e, h, j | 54.6s, 56.4s, 55.7s | 62.8s, 65.2s, 64.3s | 74.5s, 75.0s, 77.4s | 12.3-12.5 GiB |

Gantry P2P depends on origin bandwidth. It fetches each layer from the origin about 8 times. On one origin node
(about 1.6 GB/s NIC), the first 35s of the gantry run moved only about 2 GB/s across the whole fleet. Racer
fetches each 16 MiB page from the origin about 1.5-1.7 times, so origin bandwidth barely affects it.

### Fetch only (Racer, 8 GiB, curl from the gantry mirror, no unpack, 1 origin)

- 3 concurrent blobs per node: p50 45.9s, max 59.7s.
- 8 concurrent blobs per node: p50 49.0s, max 55.0s.

Unpack adds about 6-8s for 8 GiB. More parallel streams per node don't help.

## Network (per-node NIC counters sampled every second)

| | Gantry P2P 8 GiB | Racer 8 GiB | Gantry P2P 40 GB | Racer 40 GB |
|---|---|---|---|---|
| Total bytes sent by all nodes | 13.9 TB | 27.1 TB | 64.9 TB | 130.4 TB |
| ... as a multiple of bytes delivered | 1.08x | 2.1x | 1.08x | 2.2x |
| Bytes sent per node, p50 / p99 / max | 7.0 / 36.1 / 50.1 GB | 17.3 / 45.4 / 56.6 GB | 39-41 / 104-112 / 141-148 GB | 86 / 141-144 / 165 GB |
| Peak fleet receive rate | 479 GB/s (10s) | 718-732 GB/s (10s) | 553-573 GB/s (30s) | 1109-1134 GB/s (30s) |
| Time until network goes quiet | ~70s | ~50s | ~240s | ~150-180s |

40 GB fleet receive rate per 30s window (GB/s):

| Window end | 30s | 60s | 90s | 120s | 150s | 180s | 210s | 240s |
|---|---|---|---|---|---|---|---|---|
| Gantry 40g-c | 4.6 | 29 | 296 | 569 | 553 | 476 | 78 | 8 |
| Gantry 40g-e | 4.7 | 25 | 282 | 552 | 573 | 481 | 82 | 14 |
| Racer 40g-a | 670 | 1125 | 1109 | 831 | 280 | 19 | 3 | 0 |
| Racer 40g-b | 631 | 1134 | 1114 | 964 | 171 | 16 | 0 | 0 |

With 40 GB, data transfer ends well before the pulls do: about 180s (Racer) and 240s (gantry), against p50 of
230s and 280s. The rest is containerd unpacking layers one at a time. Both modes pay that cost, so the gap
between them is smaller than for 8 GiB in relative terms.

## What limits each mode

**Gantry P2P**
- Startup is slow. All nodes pull the layers in the same order, and the first copies come from the origin. For
  40 GB the fleet moved only 5-30 GB/s in the first 60s, while Racer moved 630-1130 GB/s.
- Once peers have data, the fleet peaks at about 480-570 GB/s, and nodes send only what they receive (1.08x).

**Gantry + Racer**
- Each 16 MiB page has 3 owner nodes (`cmd/racer-dataplane/topology/src/placement.rs:10`, a protocol constant).
  Only owners serve a page to other nodes. A node that already holds the page does not serve it.
- With N pages and 1497 nodes, some nodes own several pages. Each owned page means about 8 GB of upload (40 GB:
  up to 165 GB per node), so these nodes run their NIC at line rate for most of the run and set the tail.
- Each node connects directly to at most 64 neighbors (`topology/src/lib.rs`, 32 rings). It reaches other owners
  through a relay node, so every byte crosses the network about 2.1-2.2 times.
- Fixing these needs design changes (non-owners serving pages, or fan-out through relays). No config knob changes
  them.

**Both:** containerd unpacks layers one at a time. With 40 GB images this is 50-100s of each pull.

## Changes made (draft PR #1016, branch `bench/gantry-racer`)

1. **Gantry: retry and resume Racer reads** (`internal/gantry/racer/racer.go`).
   - Before: when an owner is busy, Racer returns unavailable. Gantry turned that into a 503, or cut off a blob
     that was already streaming. containerd then fetched from the origin. With 8 GiB images and the default Racer
     config, only 34 of 1497 nodes finished in 15 minutes and the origin ran at full speed. The 128 MiB smoke test
     took 22.5s p50 and pulled 18 GiB from the origin.
   - Now gantry retries unavailable errors with backoff (150ms doubling to 2s, with jitter). It resumes a broken
     blob stream at the byte where it stopped, pinned to the same ETag.
   - New setting `racer_retry_budget` (`GANTRY_RACER_RETRY_BUDGET`, default 30s, 0 turns it off). The benchmark
     used 120s. In the 40 GB runs, retries were rare (0-4 per run).
   - New metric `gantry_racer_mirror_retries_total{stage}`.
2. **Racer: recover when the slab size changes** (`cmd/racer-dataplane/alloc/src/slab.rs`, `src/app.rs`).
   - Before: changing `RACER_SLAB_BYTES` made every slab-file node crash on start (`InvalidConfiguration`) until
     someone deleted the old files by hand.
   - Now slab files of the wrong size are recreated empty. Files of workers that no longer exist are deleted.
     Raw device mode is not changed.
3. **Benchmark tools** in `hack/gantry-racer-bench/` (`bench.sh`, `genimage`, `nicstats.py`, `scrape.sh`,
   `origin.yaml`, `diskprep.sh`). `bench.sh prune` was added for the 40 GB runs (see below).

## Racer config

Set in ConfigMap `unbounded-system/racer-dataplane-config`. Byte and count values are node-wide.

| Key | Value | Default | Why |
|---|---|---|---|
| `RACER_ALLOW_SMT` | true | false | Use all 8 vCPUs (5 I/O + 3 crypto threads) instead of 4 physical cores (3 + 1). |
| `RACER_ENABLE_RDMA` | false | auto | No RDMA on these VMs. |
| `RACER_SLAB_BYTES` | 6 GiB | 1 GiB | Larger cache on nodes without a local disk. |
| `RACER_CIPHERTEXT_BYTES` | 8 GiB | 128 MiB x workers | The default budget filled up and rejected peer requests. |
| `RACER_PLAINTEXT_BYTES` | 2 GiB | 64 MiB x workers | Same. |
| `RACER_DIRTY_BYTES` | 2 GiB | 160 MiB x workers | Same. |
| `RACER_REQUEST_CONTEXT_BYTES` | 512 MiB | 32 MiB x workers | Same. |
| `RACER_CLIENT_CONNECTIONS` | 2048 | 64 x workers | Outbound connection limit (15 per worker) was hit. |
| `RACER_PIPES` | 1024 | 16 x workers | More concurrent peer transfers. |
| `RACER_QUEUE_ENTRIES` | 4096 | 256 x workers | Same. |
| `RACER_WAITERS_PER_FLIGHT` | 2048 | 64 | Up to 1497 readers wait on the same page. |
| `RACER_PEER_INFLIGHT_MAX` | 1024 | 256 | More outbound requests. |
| `RACER_PEER_PER_NEIGHBOR_MAX` | 128 | 32 | Relays forward many requests to one owner. |
| `RACER_CONNECTIONS_PER_NEIGHBOR` | 4 | 2 | Less head-of-line blocking on 16 MiB pages. |
| `RACER_RELAY_TRANSFERS` | 256 | 16 x workers | More relayed transfers. |

D8ads_v5 and D8d_v5 nodes: `diskprep.sh` unmounts `/mnt` and wipes `/dev/sdb`. The node is then annotated with
`racer.unbounded-cloud.io/block-devices=^wwn-0x...$`.

## Other findings

- **Gantry P2P keeps pulled content on disk for an hour.** In `storage_mode: containerd`, gantry holds a
  containerd lease on every blob it ingests for `containerd_lease_ttl` (default 60m,
  `internal/gantry/containerdstore/lease.go:81`). Removing the image does not free the space, and kubelet image GC
  can't reclaim it. Two 40 GB gantry pulls in a row filled the 123 GiB OS disk: 9 nodes hit DiskPressure and
  kubelet evicted pods (including racer-dataplane). That run (40g-d) is not reported. `bench.sh prune` now deletes
  gantry leases before each run. (Inference, not checked: Racer mode does not create these leases, since its
  data lives in the Racer cache.)
- Gantry P2P needs a direct path from containerd to the registry. Its mirror returns 503 on purpose when no peer
  has a layer yet (`internal/gantry/mirror/mirror.go`). Without the fallback, 18 of 1497 nodes failed.
- Changing a Racer setting rolls the DaemonSet with `maxUnavailable: 1`, which takes more than 15 minutes on 1500
  nodes. The benchmark deleted the pods instead.
- During a full restart, 2 of 1500 Racer pods exited once with `fatal stage=run error=Unauthorized` and then
  recovered.
- Racer does not log request failures. Use `/debug/failures` on the metrics port (9090).

## Raw data

Per-run files (`<run>.tsv` per-node results and NIC samples, `<run>.log`, `<run>.{pre,post}.{gantry,racer}.prom`)
are in `tmp/gantry-racer-bench/` (not committed). Run names there: `racer3-40g-a`, `racer3-40g-b`, `gantry3-40g-c`,
`gantry3-40g-e`, `gantry3-8x1g-o3a`, `gantry3-8x1g-o3b`, `racer3-8x1g-o3c`, `racer3-8x1g-o3d`, `gantry3-1g-sa`,
`gantry3-1g-sb`, `racer3-1g-sc`, `racer3-1g-sd`, `racer3-smoke`; single origin: `gantry-8x1g-k`, `gantry-8x1g-l`,
`racer-8x1g-e`, `racer-8x1g-h`, `racer-8x1g-j`; fetch only: `racer-8x1g-f-curl`, `racer-8x1g-g-curl8`.
