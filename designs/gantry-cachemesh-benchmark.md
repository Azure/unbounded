# Gantry cachemesh benchmark: 40GB image to 142 nodes

Results of the gantry benchmark campaign run on 2026-11-09 on the
`joolshev-rdma` cluster. Every run below predates the later simplification
of `meshfetch`, which removed the 512MiB hot page tier, the containerd
content API fallback, the content store descriptor cache, and most
cachemesh config knobs (see "Code changes since these runs" at the end).
Sections describing earlier designs are marked historical. It compares gantry's default peer-to-peer mode with
the optional cachemesh content path (`designs/gantry-cachemesh.md`) when
every node pulls the same 40GB image at the same moment.

## Summary

| mode | runs | pull p50 | pull p99 | makespan | origin egress |
|---|---|---|---|---|---|
| A: default (DHT p2p) | 2 | 235-247s | 306-311s | 313-317s | 8.01x |
| B: cachemesh, library defaults (1-D direct grid) | 1 (final build) | 104.2s | 113.8s | 114.2s | 1.00x |
| C: cachemesh, `maxNeighbors: 32` (relay grid) | 3 (final build) | 90.1-91.9s | 96.0-97.4s | 96.2-98.9s | 1.00x |
| single node straight from origin (baseline) | 1 | 94.7s | | | 1x per node |

- Cachemesh with a 32-neighbor grid delivers the image to all 142 nodes in
  about 97s, 3.2x faster than default mode, with every byte read from the
  origin once. Every node finishes within about 10s of the others.
- The median node finishes about as fast as a single node pulling alone
  straight from the origin (94.7s). About 69s of that is containerd unpacking
  the layers, which gantry cannot speed up. Only the download part (about
  26s alone) has any room left.
- Two bugs in the cachemesh path caused duplicate origin reads (1.16x and
  1.33x egress). Both are fixed and covered by regression tests.
- Cachemesh filled its in-memory chunk cache, so the agent's RSS rose to the
  configured cache size (24GiB here) and stayed there while idle. The
  disk-backed chunk store that replaced it (see "Historical: disk-backed chunk store"
  below) keeps the same pull time (p50 about 91s, makespan 97-102s) with
  about 2.8GiB RSS at the peak and about 0.26GiB working set when idle.
- The final design (see "Containerd content store as the only disk tier")
  drops gantry's private chunk files: containerd's content store is the
  only disk copy. It keeps the pull time (p50 91.8-92.3s, makespan
  97.8-101.8s, 1.00x egress) with 0 bytes of gantry disk writes, about
  2.5GiB RSS at the peak and 0.26GiB working set 15s after the pull, and serves a second wave of nodes from the first
  wave's content stores with 0.00x origin egress.

## Setup

- Cluster: 142 Ready workers labeled `cluster-workload=rdma`
  (Standard_D64ds_v6: 64 CPU, 251GiB, 30Gb/s NIC (about 3.67GB/s), OS and
  containerd on `nvme0n1`). One worker, `joolshev-rdma-w0134`, was NotReady
  during cachemesh run B1, so that run measured 141 nodes. It came back Ready
  during default run A1, before the gantry pod on it was running and before
  its origin `hosts.toml` was reapplied, so it failed A1. After `prep.sh apply`
  for that node, every later run used all 142 nodes.
- Origin: `registry:3` on w0007 (not a pull target), plain HTTP at
  `10.224.16.40:5005`, NIC capped at about 3.7GB/s.
- Images: `bench/g40:<tag>`, 4 uncompressed layers of 10GiB each
  (42,949,683,200 bytes), with incompressible content unique to each tag. Every
  measured run used a tag that no node had pulled before. Each tag was
  generated just before its run and deleted afterwards.
- Harness: `hack/gantry-cluster-bench/run.sh` starts `crictl pull` on every
  node at a shared `START_AT` epoch and samples every second (NIC, disk, CPU,
  gantry and containerd CPU/RSS, origin NIC). It also snapshots every agent's
  `/metrics`.
- Between runs: undeploy and redeploy gantry, delete the `gantry-cachemesh`
  Lease, run `DROP_CACHES=1 clean.sh` (removes images, gantry containerd
  leases, and page cache; afterwards about 256GB free per node), wait for the
  rollout, and give cachemesh 5 minutes to settle (see the control-plane
  section).
- No `racer-loadgen` pods were running on any target node. Six such pods
  were stuck `Terminating` on NotReady nodes (w0003, w0020, w0052, w0063,
  w0093, w0108), none of which are targets. They were left alone.

### Gantry builds

All builds come from the `gantry-cm` worktree:

| image | content | used by |
|---|---|---|
| `gantry:cm1` | branch head at the start of the campaign | A1, A2, B1 |
| `gantry:cm2` | + owner-peek fix | B2, C1 |
| `gantry:cm3` (`sha256:5948df87...`) | + owned-chunk retention fix | B3, C2, C3, C4 |

Both fixes are in `internal/gantry/meshfetch`. The default mode does not
build a `meshfetch.Node` when `cachemesh.enabled` is false
(`cmd/gantry/main.go:211`), so A runs on cm1 measure the same default
path cm3 would. This is an inference from the code, since A was not rerun on
cm3.

### Configuration

Both modes use `values-default.yaml`. Agent limits are 32 CPU and 48Gi
(requests: 1 CPU, 1Gi), `peerFetchTimeout: 15m`,
`transferMaxConcurrentServes: 10`, and `maxUnavailable: 100%`. Cachemesh
adds `values-cachemesh.yaml`:

```yaml
gantry:
  cachemesh:
    enabled: true
    expectedSize: 142
    maxNeighbors: 0            # library default 256: 1-D, every agent a direct neighbor
    chunkCacheBytes: 25769803776   # 24GiB
    verifyDigest: false        # containerd verifies digests itself
```

Mode C adds `--set gantry.cachemesh.maxNeighbors=32`. Every other cachemesh
setting is the agent default: 64MiB chunks, lookahead 8, 64 concurrent fills,
512 concurrent serves, hedging on, and a 5m tag TTL.

## Results

### Run list

| run | mode | build | tag digest | nodes ok | p50 | p99 | makespan | origin |
|---|---|---|---|---|---|---|---|---|
| default-a-run3 | A | cm1 | `062e0165...` | 141/142 (w0134, see above) | 246.9 | 306.6 | 316.7 | 8.01x |
| default-a2-run5 | A | cm1 | `1c2d6af2...` | 142/142 | 234.6 | 311.4 | 312.7 | 8.01x |
| cm-b-run4 | B | cm1 | `5ecddde7...` | 141/141 | 114.9 | 124.9 | 127.7 | 1.16x |
| cm-b2-run6 | B | cm2 | `55b3c04c...` | 142/142 | 106.6 | 118.9 | 120.2 | 1.00x |
| cm-b3-run9 | B | cm3 | `14a75e22...` | 142/142 | 104.2 | 113.8 | 114.2 | 1.00x |
| cm-c-n32-run7 | C | cm2 | `5c636e8a...` | 142/142 | 90.6 | 102.5 | 103.8 | 1.33x |
| cm-c2-n32-run8 | C | cm3 | `9e22d04e...` | 142/142 | 91.9 | 97.4 | 98.9 | 1.00x |
| cm-c4-n32-run11 | C | cm3 | `ab2e8b70...` | 142/142 | 91.0 | 96.0 | 96.2 | 1.00x |
| cm-c3-n32-verify-run10 | C + `verifyDigest: true` | cm3 | `71c31aa3...` | 142/142 | 90.1 | 96.5 | 98.6 | 1.00x |

Times are in seconds, measured from the shared start to `crictl pull`
returning. Pull duration includes the containerd unpack.

### Completion curves

The table shows seconds from the shared start until the given share of
nodes had finished, taken from `completion.csv`:

| run | first | 10% | 50% | 90% | all |
|---|---|---|---|---|---|
| default-a-run3 | 207.6 | 218.6 | 246.9 | 262.1 | 316.7 |
| default-a2-run5 | 203.6 | 212.2 | 234.6 | 255.1 | 312.7 |
| cm-b-run4 | 101.4 | 105.7 | 114.8 | 120.9 | 127.7 |
| cm-b2-run6 | 84.8 | 97.9 | 106.6 | 112.7 | 120.2 |
| cm-b3-run9 | 90.7 | 96.4 | 104.2 | 110.1 | 114.2 |
| cm-c-n32-run7 | 87.7 | 88.6 | 90.6 | 96.4 | 103.8 |
| cm-c2-n32-run8 | 89.8 | 90.8 | 91.9 | 95.1 | 98.9 |
| cm-c3-n32-verify-run10 | 86.2 | 88.4 | 90.1 | 95.0 | 98.6 |

In default mode, even the first node needs over 200s, and the last few nodes
trail the median by 60-80s. In cachemesh mode C, the nodes finish close
together: 10% to 90% spans about 5s.

### Compare output

This is the output of
`go run ./hack/cmd/gantry-cluster-bench compare` with all-zero rows and
histogram, gauge, and lookup rows removed. Rates are per node.

| metric | default-a-run3 | default-a2-run5 | cm-b-run4 | cm-b3-run9 | cm-c-n32-run7 | cm-c2-n32-run8 | cm-c4-n32-run11 | cm-c3-n32-verify-run10 |
|---|---|---|---|---|---|---|---|---|
| succeeded / expected | 141 / 142 | 142 / 142 | 141 / 141 | 142 / 142 | 142 / 142 | 142 / 142 | 142 / 142 | 142 / 142 |
| makespan s | 316.7 | 312.7 | 127.7 | 114.2 | 103.8 | 98.9 | 96.2 | 98.6 |
| pull s p50 | 246.9 | 234.6 | 114.9 | 104.2 | 90.6 | 91.9 | 91.0 | 90.1 |
| pull s p99 | 306.6 | 311.4 | 124.9 | 113.8 | 102.5 | 97.4 | 96.0 | 96.5 |
| aggregate GB/s | 19.12 | 19.51 | 47.43 | 53.39 | 58.74 | 61.68 | 63.37 | 61.83 |
| per-node MB/s p50 | 174.0 | 182.9 | 373.8 | 412.1 | 473.7 | 467.1 | 471.7 | 476.5 |
| origin egress GB | 344.03 | 344.04 | 49.67 | 43.14 | 57.00 | 43.08 | 43.09 | 43.15 |
| origin egress x image | 8.01 | 8.01 | 1.16 | 1.00 | 1.33 | 1.00 | 1.00 | 1.00 |
| origin tx peak MB/s | 3704 | 3709 | 2861 | 2295 | 2563 | 2643 | 2567 | 2493 |
| NIC rx avg MB/s p50 | 174.8 | 183.7 | 378.8 | 414.9 | 490.5 | 477.1 | 484.2 | 490.6 |
| NIC rx peak MB/s p99 | 1008.3 | 1012.9 | 3134.8 | 3217.8 | 2677.7 | 2936.9 | 2955.2 | 2606.9 |
| NIC tx avg MB/s p50 | 1.0 | 1.1 | 403.6 | 427.4 | 511.3 | 492.2 | 496.9 | 510.9 |
| NIC tx peak MB/s p99 | 3100.3 | 3219.7 | 3629.7 | 3624.3 | 3619.8 | 3606.6 | 3630.1 | 3617.2 |
| disk write GB p50 | 65.5 | 66.5 | 66.5 | 66.8 | 67.0 | 66.9 | 66.9 | 66.9 |
| node CPU avg % p50 | 1.3 | 1.4 | 4.4 | 4.5 | 6.0 | 5.5 | 5.5 | 6.0 |
| gantry CPU avg cores p50 | 0.4 | 0.4 | 1.1 | 1.1 | 1.5 | 1.4 | 1.4 | 1.7 |
| gantry CPU peak cores p99 | 8.5 | 8.5 | 8.6 | 9.0 | 13.4 | 12.9 | 12.7 | 11.7 |
| gantry RSS peak MiB p50 | 124.2 | 135.4 | 24962.0 | 24934.4 | 25023.5 | 24980.6 | 24993.7 | 24985.1 |
| containerd CPU avg cores p50 | 0.4 | 0.4 | 0.8 | 0.8 | 1.0 | 0.9 | 1.0 | 1.0 |
| containerd RSS peak MiB p99 | 197.2 | 204.8 | 100.3 | 106.5 | 106.0 | 106.0 | 106.3 | 106.9 |
| `chunk_fills_total{ok,origin}` | | | 744 | 647 | 853 | 646 | 646 | 647 |
| `chunk_fills_total{ok,owner}` | | | 710 | 903 | 978 | 624 | 694 | 700 |
| `chunk_fills_total{error,owner}` | | | 97 | 1 | 0 | 0 | 0 | 1 |
| `chunk_serves_total{not_found}` | | | 97 | 1 | 0 | 0 | 0 | 1 |
| `chunk_serves_total{error}` | | | 1865 | 1725 | 4045 | 2553 | 2818 | 2679 |
| `chunk_evicted_bytes_total` | | | 2.44e12 | 2.46e12 | 2.62e12 | 2.57e12 | 2.59e12 | 2.61e12 |
| `hedges_total{sent}` / `{won}` / `{denied}` | | | 1870 / 948 / 1109 | 1728 / 845 / 835 | 4088 / 1503 / 2694 | 2556 / 871 / 2413 | 2820 / 1031 / 2726 | 2683 / 953 / 2620 |
| `gantry_origin_bytes_total{layer}` | 3.44e11 | 3.44e11 | 4.95e10 | 4.30e10 | 5.68e10 | 4.30e10 | 4.30e10 | 4.30e10 |
| `gantry_peer_fetch_bytes_total{layer}` | 5.80e12 | 5.80e12 | | | | | | |

The image is 640 chunks of 64MiB plus a small config blob, so 646-647 origin
fills means each chunk was read from the origin once. Most
`chunk_serves_total{error}` responses are hedge losers that were canceled
once the other copy won. No 429 or retry counter changed in default mode.

### Resource usage

- Gantry CPU: about 1.1 cores on average per node in mode B and 1.4-1.7 cores
  in mode C, with peaks of 9-13 cores. Default mode averages 0.4 cores
  because only the 8 origin-seeded nodes and the serving nodes do much work.
  The 32-core limit was not reached.
- Gantry RSS: cachemesh fills its 24GiB chunk cache on every node during a
  pull (peak about 24.4GiB) and keeps it afterwards. When measured idle after
  a 4GB smoke pull, the working set was about 4.3GiB per pod (`kubectl top`).
  Default mode peaks at 120-350MiB.
- Containerd: 0.8-1.0 cores on average under cachemesh compared with 0.4 in
  default mode, because the same work is done in about a third of the time.
  Containerd RSS stays near 100MiB.
- Node NIC: under cachemesh, every node both sends and receives about
  400-500MB/s on average, and tx peaks hit the 3.6GB/s NIC cap. In default
  mode, the few seed nodes saturate tx (p90 tx avg about 950MB/s) while most
  nodes send nothing.
- `verifyDigest: true` changed nothing measurable in mode C: p50 90.1s
  compared with 91.0-91.9s, and gantry CPU averaged 1.7 cores compared with
  1.4. At about 470MB/s per node, SHA-256 is far below its cap of about
  1.8GB/s per blob.

## Control plane (cachemesh)

Measured with `meshstate.sh`, which reads every agent's `gantry_cachemesh_*`
gauges through the API server proxy:

- Fresh rollout: Helm started at T0. All 142 pods were running within about
  10s. One leader (term 1) was elected at about T0+14s. All 142 agents
  reported `ready=1`, `members=142`, and the same view seq at T0+56s.
- Settling: after the rollout, the leader kept publishing deltas for about
  4.5 minutes while members stepped through warmup weights (10s quarantine,
  then a weight step every 20s) and partitions moved to them. After that it
  published one delta per epoch (every 15s, an epoch anchor op;
  `internal/cachemesh/internal/leader/leader.go:90,467-469`). A smoke pull
  during the settling window recorded `prev_owner` and `owner` fill errors.
  These are expected handoff misses, but because of them every measured run
  waited 5 minutes after the rollout.
- Leader failover: deleting the leader pod produced term 2 with a new leader
  on all 142 agents at the next sample, within 4s. The replacement pod
  rejoined, and all 142 agents were `ready=1` again 22s after the delete.
  Pulls after the failover (B1) succeeded on every node.
- Rolling restart (`kubectl rollout restart ds/gantry`, `maxUnavailable:
  100%`, idle): the old agents left at once, and for about 33s no member
  reported a leader. The new leader took the Lease at +39s (term 3), after
  waiting out the old record's LeaseDuration of 15s plus the bootstrap
  delay. All 142 agents were `ready=1` with an identical seq at +106s.
- Lease API usage: the `gantry-cachemesh` Lease changed 21 times in 60s, so
  the leader renews every 2-3s (RetryPeriod 2s with jitter). Default mode's
  64 `gantry-chair-NN` Leases, which exist in both modes, changed about once
  a minute each. By design, only the leader and up to 3 candidates call the
  Lease API; followers make no calls
  (`internal/cachemesh/internal/elect/elector.go:8-25`, `DefaultCandidates =
  3` at line 159). That works out to about 0.4-0.5 PUT/s plus at most about
  1.5 GET/s of candidate reads. This last part is an inference from the code:
  the managed API server exposes `apiserver_request_total` per replica and
  cannot attribute requests to a user agent, and the counter deltas over the
  10-minute idle windows were dominated by about 150 kubelet node Leases and
  could not be attributed.
- Idle overhead (`kubectl top`, 10-minute window, after a smoke pull): mode
  B averaged 7-9 millicores per agent and 4.3GiB working set, and default
  mode averaged 4.8 millicores and 76-80MiB. The memory is the retained chunk
  cache, not control-plane state.

## Bugs found and fixed

1. **A backup stand-in duplicated origin reads**
   (`meshfetch: let the owner fill a chunk a backup peeks for`). When a
   request is hedged to a backup in the owner's cell, the backup gets
   `KindSelf` and first peeks the owner. The owner answered only with a
   complete chunk or one it was already reading from the origin. If its own
   fill had not started yet, it returned 404, and the backup read the chunk
   from the origin itself. In B1 this caused 97 extra origin fills (744
   instead of 647, 1.16x egress). Now an owner peek (`peek=owner`) at a member
   that owns the chunk in its own view starts or joins the origin fill there
   and serves it. Handoff peeks (then `peek=1`, now `peek=local`) still
   never start a fill, which avoids the deadlock described at
   `Node.chunkForPeer` in `node.go`. The owner path never
   forwards, so it cannot loop. Test: `TestOwnerPeekStartsOneOriginFill`.
   Result: 1.00x egress in B2 and B3.
2. **Owners evicted their own chunks under cache pressure**
   (`meshfetch: evict chunks this agent owns after transit chunks`). Every
   agent caches every chunk it pulls or relays, so during a 40GB pull the
   24GiB cache turns over in about 20s. With a plain LRU, an owner evicted the
   chunks it owned while slower requesters still needed them, and then read
   them from the origin again. Debug logs from C1 showed 853 origin fills by
   owners, about 210 of them repeats of the same owner and chunk index about
   20s apart. That gave 1.33x egress. The relay grid made this worse because
   relays add transit chunks to every cache. Owned chunks now sit on their
   own LRU list and are evicted only when no other complete chunk is left or
   when they exceed half of the budget. That cache and its tests were later
   replaced by the lingering-chunk design: a lingering chunk this member
   owns is reclaimed after the ones it does not own (`store.evictLingering`,
   test `TestEvictLingeringPrefersNotOwned`), and the owner-peek test checks
   the owned marking. Result: 1.00x egress in C2, C3, and C4, and the C
   makespan improved from 103.8s to 96-99s.

No default-mode bugs were found. Its origin egress was exactly 8.01x in both
runs, which suggests that 8 nodes read the image from the origin and the rest
fetch from peers. That is an inference from the metrics; the default-mode
seeding code was not reviewed. Because of that, the origin NIC is pinned at 3.7GB/s for about 90s,
and the first node takes over 200s.

## Findings

- The main limit in default mode is distribution, not unpacking. The first
  node finishes after 204-208s, compared with about 95s for a single node
  pulling straight from the origin. Peer fan-out uses about 1GB/s of NIC per
  node (rx peak p99) and keeps seed tx near the NIC cap.
- Cachemesh brings the median node close to the unpack floor. Node rx
  averages 470-490MB/s over the whole pull, and the transfer runs at up to
  3GB/s while containerd lags behind it.
- In mode B, the 1-D grid makes every owner send each of its chunks directly
  to all 141 other nodes. Its tx stays at the NIC cap for longer, and
  completions spread over 18-24s. The 32-neighbor grid (C) relays chunks
  through intermediate nodes, which spreads owner egress. Its completions
  spread over about 7-10s, and the makespan is 15-18s shorter, even though it
  costs a little more CPU (1.4 compared with 1.1 cores on average).
- After the fixes, origin egress is exactly 1.00x in every cachemesh run, and
  origin tx peaks at about 2.3-2.6GB/s, below its NIC cap.

## Recommendations

- Run cachemesh with `maxNeighbors` around 32 rather than the library
  default of 256, which makes the grid 1-D up to about 256 agents and serves
  each chunk directly from its owner to every node. Gantry now defaults to
  32; the library keeps 256 for 100k-node clusters.
- `chunkCacheBytes` no longer exists. Keep the in-flight budget default
  (`inflightBytes`, 2GiB): with it the chunk store matched the memory-only
  speed with a 4Gi memory limit; see "Containerd content store as the only
  disk tier".
- `verifyDigest` costs nothing measurable at these rates. Keep the default
  (`true`) unless a node must serve one blob faster than about 1.8GB/s.
  These runs used `false`, and the one `true` run gave the same results.
- Wait for the mesh to settle (about 5 minutes after a full rollout of 142
  agents) before a large synchronized pull. Pulls during settling still work
  but may go through handoff read-throughs.

## Caveats

- There is one run of B on the final build. B1 and B2 ran on older builds,
  and their timing differences come mostly from the duplicate origin reads.
  A was run twice and matched within 4%. C was run three times on cm3 (once
  with `verifyDigest: true`) and matched within 3%.
- A ran on cm1. The default path is the same in cm3 by inference (see
  "Gantry builds").
- A1 and B1 each measured 141 nodes (see Setup). Per-node rates are
  unaffected.
- The 24GiB cache and 48Gi limit are generous. The pull duration includes
  the containerd unpack, about 69s of floor.
- Lease API request counts come from the code and from observed Lease
  resourceVersion changes, not from API server metrics.

## Historical: disk-backed chunk store

This design, with private chunk files and a hot page tier, has been
replaced; its knobs (`persist`, `memoryCacheBytes`, and others) no longer
exist. Later on 2026-11-09 the in-memory chunk cache was replaced with the
disk-backed store described in `designs/gantry-cachemesh.md` (chunk store
section): chunk files on disk, a small window of in-flight pages per fill, a
store-wide in-flight budget, a hot page tier, and page read coalescing. The
same 142 nodes pulled the same kind of 40GB image (4 x 10GiB layers, a fresh
tag per run) with `values-cachemesh.yaml`, which now leaves `maxNeighbors`
at the new gantry default of 32. The agent limits stayed at 32 CPU and 48Gi
except in the 4Gi run.

### Disk measurement

Before deciding what to write to disk, a node's OS disk (`nvme0n1`, shared
with containerd) was measured with `dd` on a temporary file under
`/var/lib` (deleted afterwards):

| test | throughput |
|---|---|
| write, 4MiB blocks, `oflag=direct`, 1 stream | 1.07GB/s |
| write, 4 parallel streams, `oflag=direct` | 4 x 268MB/s (1.07GB/s) |
| read, 4MiB blocks, `iflag=direct`, 1 stream | 1.8GB/s |
| read, 4 parallel streams, `iflag=direct` | 4 x 536MB/s (2.1GB/s) |

Containerd alone writes about 86GB to this disk per 40GB pull (content store
plus unpacked snapshot, counted to 60s after the pull). In the memory-only
runs that took about 91s, so containerd already ran the disk near its write
limit. Any extra disk traffic from gantry therefore slows the pull down.

### Builds and runs

All builds come from the `gantry-mem` worktree. Every run was a full
142-node pull, and all 142 nodes succeeded in every run.

| run | build | chunk store settings | p50 | p99 | makespan | origin |
|---|---|---|---|---|---|---|
| mem1-all-r1 | mem1 | `persist: all` | 144.0 | 168.3 | 169.7 | 1.00x |
| mem1-shared-r1 | mem1 | `persist: shared` | 120.8 | 151.3 | 151.9 | 1.00x |
| mem2-owned-r1 | mem2 | `persist: owned`, no in-flight budget | 143.4 | 149.0 | 149.7 | 1.01x |
| mem3-owned-r1 | mem3 | `owned`, 1GiB in-flight budget | deadlock | | | |
| mem4-owned-r1 | mem4 | `owned`, 1GiB budget (deadlock fixed) | 107.0 | 133.9 | 135.3 | 1.01x |
| mem5-owned-r1 | mem5 | + mmap pages, ghost admission | 111.4 | 137.5 | 140.2 | 1.01x |
| mem5-owned-2g-r1 | mem5 | 2GiB budget, 512MiB hot tier | 99.1 | 112.0 | 118.1 | 1.00x |
| mem6-default-r1 | mem6 | new defaults (same as above) | 101.3 | 112.2 | 114.8 | 1.01x |
| mem6-default-r2 | mem6 | new defaults | 101.7 | 112.2 | 118.0 | 1.00x |
| mem7-default-r1 | mem7 | + lingering relay chunks | 91.1 | 97.0 | 97.3 | 1.00x |
| mem8-default-r1 | mem8 | + 30s linger TTL | 91.9 | 98.5 | 102.0 | 1.00x |
| mem8-default-r2 | mem8 | same | 91.3 | 98.2 | 98.4 | 1.01x |
| mem8-limit4g-r1 | mem8 | same, agent memory limit 4Gi | 90.7 | 97.5 | 98.8 | 1.00x |
| mem9-default-r1 | mem9 | + 30s idle TTL for hot pages (final) | 91.9 | 98.7 | 99.3 | 1.00x |

Times are seconds from the shared start to `crictl pull` returning.

### Final build compared with memory-only mode C

Output of `gantry-cluster-bench compare` (rows picked). `cm-c4-n32-run11`
is memory-only mode C from the first campaign, with a 24GiB chunk cache.

| metric | cm-c4-n32-run11 | mem8-default-r1 | mem8-default-r2 | mem8-limit4g-r1 | mem9-default-r1 |
|---|---|---|---|---|---|
| makespan s | 96.2 | 102.0 | 98.4 | 98.8 | 99.3 |
| pull s p50 | 91.0 | 91.9 | 91.3 | 90.7 | 91.9 |
| pull s p99 | 96.0 | 98.5 | 98.2 | 97.5 | 98.7 |
| pull s max | 96.2 | 102.0 | 98.4 | 98.8 | 99.3 |
| origin egress x image | 1.00 | 1.00 | 1.01 | 1.00 | 1.00 |
| NIC rx avg MB/s p50 | 484.2 | 485.2 | 496.3 | 485.9 | 479.4 |
| NIC tx avg MB/s p50 | 496.9 | 521.0 | 519.2 | 513.6 | 504.7 |
| gantry CPU avg cores p50 | 1.4 | 0.8 | 0.9 | 0.8 | 0.8 |
| gantry CPU peak cores p99 | 12.7 | 4.9 | 5.0 | 5.3 | 4.7 |
| gantry RSS peak MiB p50 | 24993.7 | 2772.1 | 2782.0 | 2776.0 | 2784.8 |
| gantry RSS peak MiB p99 | 25033.7 | 3032.6 | 3034.9 | 3036.0 | 3041.3 |
| containerd CPU avg cores p50 | 1.0 | 0.9 | 0.9 | 1.0 | 0.9 |
| disk read GB p50 | n/a | 0.5 | 0.5 | 0.5 | 0.5 |
| gantry working set peak MiB p50 | n/a | 3267.3 | 3306.6 | 3259.1 | 3269.1 |
| gantry working set peak MiB p99 | n/a | 3574.7 | 3617.4 | 3500.7 | 3612.4 |
| gantry working set end MiB p50 | n/a | 514.6 | 513.7 | 518.0 | 255.5 |
| gantry page cache peak MiB p50 | n/a | 0.1 | 0.1 | 0.1 | 0.1 |

- Pull time matches memory-only mode C: p50 about 91s and makespan 97-102s,
  against 91s and 96-99s.
- Gantry memory dropped from about 24.4GiB RSS, held while idle, to about
  2.8GiB RSS (3.3GiB working set) at the peak, and to about 0.26GiB working
  set 60s after the pull with the final build (0.5GiB before hot pages got
  an idle TTL).
- The 4Gi memory limit run finished at the same speed with no restarts.
- Gantry used less CPU: 0.8-0.9 cores on average against 1.4, and peaks of
  about 5 cores against 13. Page buffers live outside the Go heap, so the
  garbage collector has much less to do.
- Gantry's own disk traffic is small: each agent writes only the chunks it
  owns (about 0.3GB) and read about 0.5GB from disk. Page cache in the agent
  cgroup stayed under 1MiB (`O_DIRECT`).
- Origin egress stayed at 1.00x.
- "disk write GB p50" is not comparable between the two: the new runs count
  60s after the pull, the old ones 15s. Both are almost all containerd.

### What did not work, and why

- **Writing every chunk to disk (`persist: all`)** made the pull 58% slower
  (p50 144s). Each node wrote about 42GB of chunks on top of containerd's
  86GB and read back about 71GB. The local blob stream lags behind its
  lookahead fills, so it read most chunks back from disk.
- **Writing only chunks peers ask for (`persist: shared`)** cut chunk writes
  to about 5GB per node but still read about 33GB back for later peers, and
  the pull was 33% slower (p50 121s).
- **Writing only owned chunks with no memory budget** removed nearly all
  extra disk traffic, but a fill whose window was full waited for its
  slowest reader. Relays were then held to the pace of their own containerd,
  which chained across the grid (p50 143s).
- **Adding a 1-2GiB in-flight budget** let fills that peers read run ahead
  of a lagging reader (p50 99-111s). But relays kept nothing once a fill
  finished, so peers that reached a chunk a few seconds later made the
  relay fetch it again: about 70k extra fills and 1.7x the bytes received
  per node.
- **Keeping peer-read chunks in the same budget** (lingering relay chunks)
  brought peer-initiated fills back to about 12k, as in mode C, and the p50
  to 91s. A 30s idle TTL then returned that memory after the pull.

### Bugs found

1. **Deadlock when a fill spilled after borrowing pages** (mem3). The chunk
   writer was fed through a channel sized to the base window, and pages
   were queued while holding the fill lock. A fill that had borrowed pages
   beyond its base window and then spilled queued more pages than the
   channel held, blocked under the lock, and the writer needed the same lock.
   Every pull on every node stopped (0/142 done after 13 minutes); a
   goroutine dump (`kill -QUIT`) showed the filler blocked in `queueLocked`.
   The writer now takes pages from the window in order and is woken by a
   non-blocking signal. The spill path and its test were removed with the
   chunk files.
2. **Idle memory held by lingering chunks** (mem7). Lingering relay chunks
   kept about 2.7GiB after the pull until something else needed the budget.
   Fixed with a 30s idle TTL for lingering chunks (mem8) and hot pages
   (mem9). Test: `TestLingeringChunkExpires`. The hot page tier was later
   removed.

### Caveats

- Mode C was not rerun with the final build in the same session; the
  comparison uses the earlier C runs (same cluster, image shape, and
  harness, but `TAIL_SECONDS` 15 instead of 60).
- Working set and page cache were not measured in the first campaign, only
  RSS.
- The runs used `verifyDigest: false`, like the first campaign.
- The defaults favor nodes whose disk is already busy during a pull. On
  nodes with faster disks, `persist: all` or `shared` keeps more chunks for
  later pulls; that was not measured.
- In every run the bench pod log of `joolshev-rdma-w0139` ends 50-80s
  before its pull does, so its "working set at end of tail" (2.7-3.2GiB, the
  max column) is a mid-pull value. On every other node the end-of-tail
  working set was at most about 780MiB.

## Containerd content store as the only disk tier

The disk-backed store above still kept a second copy of every owned chunk in
the agent's own files, next to containerd's copy of the same blob. The
final design removes the private chunk files: containerd's content store is
the only disk copy, and agents read it back (committed blobs and
in-progress ingests) to serve peers. See "Chunk store" in
`designs/gantry-cachemesh.md`. Measured on 2026-11-09 on the same cluster.

### Method

- Build `gantry:cd2` from the `gantry-cd` worktree, deployed with
  `values-default.yaml` plus `values-cachemesh.yaml` (agent defaults:
  `maxNeighbors` 32, 2GiB in-flight budget, 512MiB hot tier (since
  removed), `verifyDigest: false`). The chart mounts
  `/var/lib/containerd/io.containerd.content.v1.content` read-only; the
  agent logged `content_source: dir` (now `content_readable: true`). During a pull the agent (uid 65532)
  listed and read `ingest/<key>/{ref,total,data}` (directories 0755, files
  0644).
- Between runs: `undeploy.sh` (which deletes the namespace and the
  `gantry-cachemesh` Lease), `DROP_CACHES=1 clean.sh`, delete the previous
  image and garbage collect the registry, redeploy, generate a fresh
  never-pulled 40GB image (4 x 10GiB layers), wait 4 minutes, run.
- `run.sh` now also samples the gantry container's cgroup `io.stat` and the
  gantry process's `/proc/<pid>/io` every second (`IOSAMPLE`), to show that
  gantry writes nothing to disk.
- Two-wave test: after the same reset, the odd-numbered half of the 142
  sorted nodes (71) pulled, then 3 minutes later (after every 30s linger
  window and hot page TTL expired; the hot tier has since been removed) the
  even-numbered half pulled the same
  image. The second run's origin egress counts only the second wave.
- A 4-node smoke pull of a 4GB image ran first to check the mount, the
  fallback logging, and the I/O sampling.

### Results

| run | nodes | p50 | p90 | p99 | max | makespan | origin egress |
|---|---|---|---|---|---|---|---|
| cd-g40-r1 | 142 | 92.3 | 94.6 | 97.6 | 101.8 | 101.8 | 1.00x |
| cd-g40-r2 | 142 | 91.8 | 96.5 | 97.6 | 97.8 | 97.8 | 1.00x |
| cd-g40-r3 | 142 | 91.9 | 96.0 | 97.3 | 97.8 | 97.8 | 1.00x |
| cd-wave1-w1 | 71 (wave 1) | 91.5 | 95.1 | 96.4 | 96.4 | 96.4 | 1.00x |
| cd-wave1-w2 | 71 (wave 2, 3 min later) | 86.8 | 89.2 | 92.6 | 92.6 | 92.6 | 0.00x (875 bytes) |

Seconds from the shared start to `crictl pull` returning; all nodes
succeeded in every run.

Output of `gantry-cluster-bench compare` (rows picked):

| metric | cd-g40-r1 | cd-g40-r2 | cd-g40-r3 | cd-wave1-w1 | cd-wave1-w2 |
|---|---|---|---|---|---|
| NIC rx avg MB/s p50 | 473.9 | 475.3 | 474.9 | 474.3 | 500.3 |
| NIC rx peak MB/s p99 | 2522.4 | 2676.4 | 2647.1 | 2569.4 | 3543.8 |
| NIC tx avg MB/s p50 | 499.8 | 510.2 | 496.6 | 268.3 | 260.1 |
| NIC tx peak MB/s p99 | 3623.2 | 3623.9 | 3642.2 | 3638.8 | 3573.2 |
| node disk write GB p50 (incl. 15s tail) | 64.2 | 64.2 | 64.2 | 64.2 | 64.1 |
| node disk read GB p50 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 |
| gantry cgroup disk write MiB max | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 |
| gantry process disk write MiB max | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 |
| gantry cgroup disk read MiB p50 | 0.0 | 0.0 | 0.0 | 0.0 | 0.0 |
| gantry CPU avg cores p50 | 0.8 | 0.8 | 0.8 | 0.7 | 0.6 |
| gantry CPU peak cores p99 | 5.1 | 5.1 | 5.3 | 4.8 | 4.7 |
| gantry RSS peak MiB p50 | 2539.5 | 2536.2 | 2539.3 | 2528.0 | 2517.3 |
| gantry RSS peak MiB p99 | 2912.7 | 2823.4 | 2807.7 | 2823.6 | 2936.3 |
| gantry working set peak MiB p50 | 2961.9 | 2935.1 | 2957.2 | 2741.3 | 2806.1 |
| gantry working set peak MiB p99 | 3128.1 | 3062.7 | 3090.8 | 2918.8 | 3021.0 |
| gantry working set end MiB p50 | 258.9 | 263.2 | 266.1 | 257.5 | 249.4 |
| gantry page cache peak MiB p50 | 0.1 | 0.1 | 0.1 | 0.1 | 0.1 |
| containerd CPU avg cores p50 | 0.9 | 0.9 | 0.9 | 0.9 | 1.0 |
| origin chunk fills | 646 | 646 | 646 | 646 | 1 |
| local (content store) chunk fills | 3195 | 2140 | 2075 | 873 | 12054 |
| content store page reads (1MiB) | 64560 | 46595 | 41188 | 19525 | 289626 |
| holder reads ok / failed | 0 / 0 | 0 / 0 | 0 / 0 | 0 / 0 | 312 / 2 |
| peer chunk fills | 92550 | 92160 | 91847 | 51419 | 46116 |

The image has 644 chunks of 64MiB (plus the manifest and config), so 646
origin fills is one per chunk. Disk free on `/var/lib/containerd` was about
256GB on every node after each `clean.sh` and 169.6-170.0GB after each pull
(the `DISK` line of every bench pod): 86GB per node, the image in the
content store plus its unpacked snapshot, as in the direct-origin baseline,
and nothing else. The 64.2GB of node disk writes within 15s of the pull is
containerd alone (gantry wrote 0 bytes).

### Compared with the disk-backed store

| metric | disk-backed (mem8/mem9, 4 runs) | content store only (cd2, 3 runs) |
|---|---|---|
| pull p50 | 90.7-91.9s | 91.8-92.3s |
| pull p99 | 97.5-98.7s | 97.3-97.6s |
| makespan | 98.4-102.0s | 97.8-101.8s |
| origin egress | 1.00-1.01x | 1.00x |
| gantry disk writes per node | about 0.3GB (owned chunks) | 0 bytes |
| gantry disk reads per node | about 0.5GB | 0 bytes (page cache hits only) |
| gantry RSS peak p50 | 2772-2785MiB | 2536-2540MiB |
| gantry working set peak p50 | 3259-3307MiB | 2935-2962MiB |
| gantry working set at end of tail (p50) | 256MiB (mem9, 60s tail) | 259-266MiB (15s tail) |
| gantry CPU avg / peak p99 | 0.8-0.9 / 4.7-5.3 cores | 0.8 / 5.1-5.3 cores |

- Pull time is the same within noise: the p50 is up to 1s higher, the p99
  and makespan are the same or lower. The disk-backed baseline was not
  rerun: the image shape, cluster, harness, and the direct and default-mode
  numbers have not changed, and the new p50 sits within the old runs' spread
  plus about 0.4s.
- Gantry writes nothing to disk: the agent cgroup's `io.stat` and the
  process's `write_bytes` stayed at 0 on all 142 nodes in every run. Agents
  read 180-280GB per run from containerd's content stores, all of it from
  the page cache (node disk reads 0.0GB).
- Memory is lower than with the disk-backed store (about 2.5GiB RSS and
  2.9-3.0GiB working set at the peak, about 0.26GiB 15s after the pull).
  Lingering owned chunks replace the owned chunk files, and pages behind a
  fill's window leave memory as soon as containerd has written them.
- The second wave pulled with 0.00x origin egress (875 bytes: one manifest
  fill) and faster than the first (p50 86.8s against 91.5s): relays and
  owners that pulled in the first wave served from their content stores
  (12k local fills, 290k page reads), and owners that had not pulled read
  their chunks from first-wave requesters (312 holder reads, 2 misses).

### Bugs found

1. **Owned chunks dropped before peers asked.** With no chunk files, a
   fill that only the owner's own pull read was dropped when it finished,
   and a peer asking later made the owner pull the chunk from the origin a
   second time. Found by `TestFetchBlobStripesAcrossOwnersAndDedupesOrigin`
   under `-race` before the cluster runs. Owned chunks now linger in the
   in-flight budget like chunks peers read.
2. **Holder reads of the requester being served.** The smoke run showed 73
   failed holder reads in a single pull: the owner recorded a requester and
   then immediately asked it for the chunk it was itself requesting.
   Requesters recorded less than 20s ago are now skipped
   (`TestHoldersRecentFirstBounded`); the 142-node runs had no holder reads,
   and the second wave had 2 misses in 314.

### Caveats

- Every node in the main runs pulled the image, so the content store
  covered most reads behind fill windows. Owners that do not pull an image
  keep their chunks only in memory (30s unused); a much later pull then
  depends on holders that still have the blob, else the origin.
- containerd's GC ran with its defaults; the bench never deleted an image
  while another node was reading it. GC during a read shows up as a lost
  reader and a retry, covered by unit tests only.
- `serveLocalHit` (a local hit served to containerd from its own store over
  the content API) was not changed; it does not occur in these runs.
- The runs used `verifyDigest: false`, like the earlier campaigns.

### Re-verification on merged code

On 2026-11-09 the merged code was rebuilt as `gantry:final1` and rerun with
the same procedure, limits, and image shape: three 142-node cachemesh runs
(`fin-cm-r1..r3`), a two-wave run (`fin-wave-w1`, `fin-wave-w2`, 71 nodes
each, 3 minutes apart), and two same-day default-mode runs (`fin-def-r1`,
`fin-def-r2`). Every run used a fresh, never-pulled image. All nodes
succeeded in every run.

| run | mode | nodes | p50 | p90 | p99 | max / makespan | origin egress |
|---|---|---|---|---|---|---|---|
| fin-cm-r1 | cachemesh | 142 | 91.9 | 96.2 | 98.5 | 99.1 | 1.00x |
| fin-cm-r2 | cachemesh | 142 | 92.5 | 95.6 | 99.6 | 101.3 | 1.00x |
| fin-cm-r3 | cachemesh | 142 | 92.1 | 95.7 | 97.9 | 98.0 | 1.00x |
| fin-wave-w1 | cachemesh, wave 1 | 71 | 91.9 | 93.8 | 96.7 | 96.7 | 1.00x |
| fin-wave-w2 | cachemesh, wave 2 | 71 | 86.8 | 88.7 | 93.5 | 93.5 | 0.00x (1 origin fill) |
| fin-def-r1 | default | 142 | 248.4 | 268.9 | 323.4 | 334.4 | 8.01x |
| fin-def-r2 | default | 142 | 248.2 | 264.4 | 325.9 | 335.6 | 8.01x |

| metric (p50 across nodes unless noted) | cachemesh fin-cm-r1..r3 | earlier cd2 runs | default fin-def-r1..r2 |
|---|---|---|---|
| gantry RSS peak | 2534-2542MiB | 2536-2540MiB | 128-131MiB |
| gantry working set peak | 2933-2967MiB | 2935-2962MiB | 85-88MiB |
| gantry working set at end of tail | 257-264MiB | 259-266MiB | 83MiB |
| gantry CPU avg / peak p99 | 0.8 / 4.9-5.3 cores | 0.8 / 5.1-5.3 cores | 0.4 / 8.5-8.7 cores |
| gantry cgroup disk write, max node | 0.1MiB | 0.0MiB | 0.0MiB |
| gantry cgroup disk read | 0.0MiB | 0.0MiB | 0.0MiB |
| NIC rx avg | 472-477MB/s | 474-475MB/s | 174MB/s |
| node disk write (incl. 15s tail) | 64.1-64.2GB | 64.2GB | 64.0GB |
| origin chunk fills | 646-647 | 646 | n/a |

- The merged code reproduces the cd2 results: p50 91.9-92.5s (cd2
  91.8-92.3s), p99 97.9-99.6s (cd2 97.3-97.6s), makespan 98.0-101.3s (cd2
  97.8-101.8s), 1.00x origin egress. The direct single-node pull is 94.7s.
- Default mode is slower than in the earlier campaigns (p50 248s against
  235-247s, p99 323-326s against 306-311s, makespan 334-336s against
  313-317s) with the same 8.01x egress. Cachemesh is 2.7x faster on p50 and
  3.3x on makespan against the same-day baseline.
- Wave 2 matched the earlier result: 0.00x egress (one origin fill), 322
  holder reads ok and 2 failed, p50 86.8s.
- No extra copies: free disk on `/` went from 255.5-255.6GB after
  `clean.sh` to 169.6GB after each pull on every node that reports nvme0n1
  (86GB: content store blob plus snapshot), the same as default mode. On
  w0000, w0070 and w0149 after `fin-cm-r1`, `/var/lib/gantry` held only
  `libp2p/identity.key` (68 bytes), each of the 4 layer blobs existed as
  exactly one file on the root filesystem (in the containerd content
  store), and no other file over 5GB existed outside the content store and
  snapshots.
- Tooling anomaly, not a gantry bug: on w0139 the OS disk is `nvme1n1`, not
  `nvme0n1`, so `node.sh` reports 0 disk writes and about 174GB free for it.

## Cluster end state

- The gantry release is uninstalled, and the `gantry-system` namespace,
  including all `gantry-chair-*` and `gantry-cachemesh` Leases, is gone.
- `clean.sh` with `DROP_CACHES=1` ran on all 142 nodes (about 256GB free on
  each).
- In the origin registry, all bench images and the `cm1` and `cm2` gantry
  builds were deleted and garbage collected. `gantry:bench-base` and
  `gantry:cm3` remain (5.2GB used, 242GB free). The registry is still
  running.
- No bench DaemonSets remain in `gantry-bench`. `default/bench-hwprobe` and
  `default/bench-nodestat` predate the campaign and were not touched.
- The `certs.d/10.224.16.40:5005/hosts.toml` files from `prep.sh` remain on
  every node, and were reapplied on w0134 after its restart.
- Raw data is in `tmp/gantry-cluster-bench/` in the worktree (gitignored).

After the chunk store runs the cluster is in the same state. The `mem1` to
`mem9` gantry builds and every `bench/g40:mem*` image were deleted and
garbage collected. Chunk files lived in the agent pods' `emptyDir` and went
away with the pods. The `dd` test file on `joolshev-rdma-w0000` was removed
by the same pod that wrote it. Raw data for these runs is in
`tmp/gantry-cluster-bench/mem*` in the `gantry-mem` worktree (gitignored).

After the content store runs the cluster is again in the same state: the
gantry release, the `gantry-system` namespace, and the `gantry-cachemesh`
Lease are gone, `clean.sh` with `DROP_CACHES=1` ran on all 142 nodes (about
256GB free on each), and the `bench/g40:cdrun*` and `cdwave1` images, the
`bench/smoke:cdsmoke1` image, and the `gantry:cd1` build were deleted and
garbage collected. `gantry:cd2` (the measured build) remains next to
`gantry:bench-base` and `gantry:cm3`. Raw data is in
`tmp/gantry-cluster-bench/cd-*` in the `gantry-cd` worktree (gitignored).

## Code changes since these runs

After the runs above, `meshfetch` and its configuration were simplified
without a cluster rerun. In local benchmarks (`go test -bench .
./internal/gantry/meshfetch`) the end-to-end fetches were unchanged (cold
fetch +3%, peer fetches within noise), while serving from a warm content
store got slower: one reader of a 16MiB chunk 1.4ms to 2.0ms per chunk
(opening the file per page) and 32 concurrent readers about 15% slower
(16 to 23 content store reads per chunk without the hot tier). Both stay
well above per-node network rates (7.7GiB/s and 46GiB/s). The changes:

- The hot page tier (`memory_cache_bytes`, 512MiB in every run above) was
  removed. Concurrent reads of one page are still coalesced; a page is
  freed once its last reader is done.
- The containerd content API fallback was removed: a node without a
  readable `content_root` serves peers from memory only.
- `DirContent` opens, reads, and closes the blob per 1MiB page instead of
  caching descriptors.
- The agent no longer sets `GOMEMLIMIT` from the cgroup limit.
- Config is reduced to `enabled`, `namespace`, `chunk_size`,
  `expected_size`, `max_neighbors`, `content_root`, `inflight_bytes`,
  `max_concurrent_chunk_fills`, `cache_tags`, and `verify_digest`; the
  other values used in these runs are now fixed defaults.
- A blob stream no longer retries a failed fill; only a reader that lost
  bytes obtains a new one. Attempts per chunk no longer compound.
