# Gantry vs Gantry + Racer: image pull benchmark

Date: 2026-10-28. Base: origin/main d017881b5, plus the fixes listed below.

## Setup

- AKS cluster `joolshev-scale-test`, 1497 worker nodes. Every node is 8 vCPU / 32 GiB.
  - 447 Standard_D8ads_v5 and 550 Standard_D8d_v5. Racer uses the 43 GiB local temp disk as a raw block device.
  - 500 Standard_D8ds_v6. These have no extra local disk, so Racer uses 6 GiB slab files per I/O worker on the
    OS disk.
- Origin: `distribution` 3.0.0 over plain HTTP on the 3 system nodes, one replica per node. Every image is pushed
  to all 3 replicas (`origin.yaml`, `bench.sh gen`).
- Images are random data, so they don't compress or dedupe:
  - 8 GiB: 8 layers of 1 GiB.
  - 1 GiB: 4 layers of 256 MiB.
  - Smoke: 2 layers of 64 MiB.
  - Every run uses a new image.
- Each run starts a DaemonSet that pulls with `crictl pull` on every worker at the same second. The time reported
  is from that start until the pull finishes, so it includes the containerd unpack.
- containerd uses gantry first, then falls back to the origin directly (`certs.d/bench.registry.local/hosts.toml`).
- Racer runs use one dataplane per node. RDMA is off. Racer settings are in "Racer config" below.

## Results (3 origin replicas)

| Image | Mode | Run | p50 | p90 | p95 | p99 | max | Origin egress |
|---|---|---|---|---|---|---|---|---|
| 8 GiB | Gantry P2P | o3a | 75.9s | 81.2s | 82.6s | 85.0s | 96.3s | 64.1 GiB |
| 8 GiB | Gantry P2P | o3b | 76.2s | 80.7s | 81.9s | 84.8s | 95.7s | 64.1 GiB |
| 8 GiB | Gantry + Racer | o3c | 54.1s | 61.6s | 63.4s | 66.2s | 72.1s | 12.5 GiB |
| 8 GiB | Gantry + Racer | o3d | 52.2s | 58.8s | 60.8s | 64.9s | 73.8s | 12.9 GiB |
| 1 GiB | Gantry P2P | sa | 15.5s | 16.2s | 16.6s | 17.5s | 18.4s | 8.0 GiB |
| 1 GiB | Gantry P2P | sb | 15.2s | 16.3s | 16.7s | 17.4s | 17.8s | 8.0 GiB |
| 1 GiB | Gantry + Racer | sc | 14.6s | 16.7s | 17.1s | 17.7s | 18.7s | 1.6 GiB |
| 1 GiB | Gantry + Racer | sd | 15.8s | 17.7s | 18.1s | 19.2s | 20.3s | 1.6 GiB |
| 128 MiB | Gantry + Racer | smoke | 8.6s | 9.7s | 9.9s | 10.0s | 11.0s | 0.2 GiB |

All runs: 1497 of 1497 nodes succeeded. The results were close across node pools. In the 8 GiB Racer runs the
D8ads_v5 pool was 3-5s slower than the other two at p50.

### Same tests with a single origin replica (8 GiB)

| Mode | Runs | p50 | p95 | max | Origin egress |
|---|---|---|---|---|---|
| Gantry P2P | k, l | 110.2s, 94.8s | 116.7s, 100.2s | 123.4s, 108.4s | 64.2 GiB |
| Gantry + Racer | e, h, j | 54.6s, 56.4s, 55.7s | 62.8s, 65.2s, 64.3s | 74.5s, 75.0s, 77.4s | 12.3-12.5 GiB |

Gantry P2P depends on origin bandwidth. It fetches each layer from the origin about 8 times. On one origin node
(about 1.6 GB/s NIC), the first 35s of the gantry run moved only about 2 GB/s across the whole fleet. Racer
fetches each 16 MiB page from the origin about 1.5 times, so origin bandwidth barely affects it.

### Fetch only (Racer, 8 GiB, curl from the gantry mirror, no unpack)

- 3 concurrent blobs per node: p50 45.9s, max 59.7s.
- 8 concurrent blobs per node: p50 49.0s, max 55.0s.

Unpack adds about 6-8s. More parallel streams per node don't help.

### Network (8 GiB, per-node NIC counters sampled every second)

| | Gantry P2P (o3a) | Gantry + Racer (o3c, o3d) |
|---|---|---|
| Total bytes sent by all nodes | 13.9 TB (1.08x the bytes delivered) | 27.1 TB (2.1x) |
| Bytes sent per node, p50 / p99 / max | 7.0 / 36.1 / 50.1 GB | 17.3 / 45.4 / 56.6 GB |
| Peak fleet receive rate | 479 GB/s | 718-732 GB/s |
| Time until network goes quiet | ~70s | ~50s |

## What limits each mode

**Gantry P2P**
- Startup is slow. All nodes pull the layers in the same order, and the first copies come from the origin. With 3
  origin replicas the fleet receive rate was 4 GB/s at 10s, 70 GB/s at 30s, and 480 GB/s at 50s.
- Once peers have data, the fleet peaks at about 480 GB/s.

**Gantry + Racer**
- Each 16 MiB page has 3 owner nodes (`cmd/racer-dataplane/topology/src/placement.rs:10`, a protocol constant).
  Only owners serve a page to other nodes. A node that already holds the page does not serve it.
- With 512 pages and 1497 nodes, some nodes own several pages. Each owned page means about 8 GB of upload, so
  these nodes run their NIC at line rate (about 1.6 GB/s) for most of the run and set the tail.
- Each node connects directly to at most 64 neighbors (`topology/src/lib.rs`, 32 rings). It reaches other owners
  through a relay node, so every byte crosses the network about 2.1 times.
- Fixing these needs design changes (non-owners serving pages, or fan-out through relays). No config knob changes
  them.

## Changes made (draft PR, branch `bench/gantry-racer`)

1. **Gantry: retry and resume Racer reads** (`internal/gantry/racer/racer.go`).
   - Before: when an owner is busy, Racer returns unavailable. Gantry turned that into a 503, or cut off a blob
     that was already streaming. containerd then fetched from the origin. With 8 GiB images and the default Racer
     config, only 34 of 1497 nodes finished in 15 minutes and the origin ran at full speed. The 128 MiB smoke test
     took 22.5s p50 and pulled 18 GiB from the origin.
   - Now gantry retries unavailable errors with backoff (150ms doubling to 2s, with jitter). It resumes a broken
     blob stream at the byte where it stopped, pinned to the same ETag.
   - New setting `racer_retry_budget` (`GANTRY_RACER_RETRY_BUDGET`, default 30s, 0 turns it off). The benchmark
     used 120s.
   - New metric `gantry_racer_mirror_retries_total{stage}`.
2. **Racer: recover when the slab size changes** (`cmd/racer-dataplane/alloc/src/slab.rs`, `src/app.rs`).
   - Before: changing `RACER_SLAB_BYTES` made every slab-file node crash on start (`InvalidConfiguration`) until
     someone deleted the old files by hand.
   - Now slab files of the wrong size are recreated empty. Files of workers that no longer exist are deleted.
     Raw device mode is not changed.
3. **Benchmark tools** in this directory.

## Racer config

These values are set in ConfigMap `unbounded-system/racer-dataplane-config`. Byte and count values are
node-wide.

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

- Gantry P2P needs a direct path from containerd to the registry. Its mirror returns 503 on purpose when no peer
  has a layer yet (`internal/gantry/mirror/mirror.go`). Without the fallback, 18 of 1497 nodes failed.
- Changing a Racer setting rolls the DaemonSet with `maxUnavailable: 1`, which takes more than 15 minutes on 1500
  nodes. The benchmark deleted the pods instead.
- During a full restart, 2 of 1500 Racer pods exited once with `fatal stage=run error=Unauthorized` and then
  recovered.
- Racer does not log request failures. Use `/debug/failures` on the metrics port (9090).

## Running it

```sh
export BENCH_CONTEXT=$(kubectl config current-context)
kubectl apply -f origin.yaml
./bench.sh prep                      # hosts.toml on every worker
./bench.sh diskprep                  # optional: free local disks for Racer
ref=$(./bench.sh gen g8x1g-a 8 1073741824)
NIC_SECS=100 ./bench.sh run gantry-a "$ref" crictl
python3 nicstats.py ../../tmp/gantry-racer-bench/gantry-a.tsv 10
```

Racer mode is on when a ClusterCache named `gantry` exists. Delete it to go back to gantry P2P. To keep the Racer
DaemonSet while doing that, first create another ClusterCache, for example `bench-hold`.
