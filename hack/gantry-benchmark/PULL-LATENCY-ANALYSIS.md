# Pod Startup Latency Analysis

Where the ~750-1100 seconds of per-pod startup time actually goes on a
1000-node AKS cluster pulling a 40 GiB, 40-layer image, and which containerd
settings move it.

Source runs, both Canada Central, `Standard_D8s_v3`, Kubernetes 1.35, Azure CNI
Overlay, containerd 2.x, images pulled over ACR Private Endpoints:

| Run | containerd `max_concurrent_downloads` |
| --- | --- |
| `run-20260806-185139-9995e167` | 3 (containerd default) |
| `run-20260806-205719-51c38730` | 6 (benchmark drop-in) |

`max_concurrent_unpacks` was 1 (the containerd default) in both.

Measurements come from AKS audit timestamps, containerd debug journals captured
by the node-observer DaemonSet on all 1000 nodes, and Prometheus node metrics.
Percentiles are nearest-rank, matching the benchmark runner.

## Scheduling is not the story

| Component | Baseline | Gantry cold |
| --- | ---: | ---: |
| Scheduling, create to bind (P50) | 2.162s | 1.836s |
| Post-bind, bind to container started (P50) | 757.726s | 921.710s |
| Total pod startup (P50) | 759.746s | 923.473s |

Scheduling is 0.3% of startup. Effectively all wall time is the image pull, so
the rest of this document decomposes post-bind.

## Post-bind splits into byte-waiting and unpacking

containerd logs one `layer unpacked` event per layer and one `image unpacked`
event per image, both with durations. The image event covers the whole pull, so
the difference between it and the sum of its layer events is time containerd
spent waiting for bytes rather than unpacking them.

Per-layer unpack cost is stable across every phase and both runs: mean 8.331s
to 8.404s over 14,000 samples per phase. Forty layers therefore cost roughly
333s of unpack work per node, and that figure does not depend on where the bytes
came from.

| Run / phase | Image unpack (P50) | Unpack work | Waiting for bytes |
| --- | ---: | ---: | ---: |
| downloads=3, baseline | 931.1s | 333.5s (35.8%) | 597.6s (64.2%) |
| downloads=3, Gantry cold | 1098.0s | 336.2s (30.6%) | 761.8s (69.4%) |
| downloads=6, baseline | 753.3s | 333.2s (44.2%) | 420.0s (55.8%) |
| downloads=6, Gantry cold | 917.3s | 334.0s (36.4%) | 583.3s (63.6%) |

## Raising download concurrency to 6 is a 15-20% win

| Metric | downloads=3 | downloads=6 | Change |
| --- | ---: | ---: | ---: |
| Baseline P50 | 939.368s | 759.746s | 19.1% faster |
| Baseline P95 | 1091.173s | 867.850s | 20.5% faster |
| Gantry P50 | 1105.766s | 923.473s | 16.5% faster |
| Gantry P95 | 1180.970s | 994.468s | 15.8% faster |

The gain comes entirely out of byte-waiting. Baseline waiting fell from 597.6s
to 420.0s while unpack work stayed at 333s.

## Unpacking is serialized, and that floor is now dominant

containerd's transfer plugin defaults to `max_concurrent_unpacks = 1`
(`plugins/transfer/plugin.go`), and `core/transfer/local/transfer.go` only
builds the unpack semaphore when the value exceeds 1. Without that semaphore,
`Unpacker.supportParallel` returns false at its first branch and every layer is
unpacked sequentially.

Both runs confirm this empirically: all 2000 `image unpacked` events in each run
carry `parallel=false`, and the journals contain no "snapshotter does not
support rebase capability" message. The rebase check was never reached, so the
only thing disabling parallel unpack was the concurrency setting.

This matters because overlayfs does advertise the `rebase` capability required
for parallel unpack whenever containerd is not running inside a user namespace
(`plugins/snapshots/overlay/plugin/plugin.go`), which is the case on AKS. The
capability is available and unused.

As delivery gets faster the serialized floor becomes proportionally larger. On
baseline it moved from 35.8% of the pull at 3 concurrent downloads to 44.2% at
6. Raising download concurrency further without also raising unpack concurrency
has diminishing returns.

## Nothing is resource-saturated except Gantry's CPU

| Resource | downloads=3 baseline | downloads=6 baseline | downloads=3 Gantry | downloads=6 Gantry |
| --- | ---: | ---: | ---: | ---: |
| NIC utilization (P95) | 1.26% | 1.71% | 8.89% | 11.67% |
| CPU busy (mean) | 23.5% | 28.1% | 50.2% | 53.7% |
| CPU busy (P95) | 49.4% | 53.4% | 85.2% | 90.2% |
| Disk busy (P95) | 69.3% | 73.7% | 64.9% | 64.7% |

The NIC is roughly 98% idle in every configuration, so these pulls are
concurrency-limited rather than bandwidth-limited. Disk is the second-most
loaded resource and is the one to watch when raising unpack concurrency,
because unpacking is write-heavy.

Gantry CPU is the exception. Gantry nodes pull layers, unpack them, and serve
roughly 42 TB to peers, which puts them at 90.2% P95 CPU on 8 vCPU at 6
concurrent downloads. Baseline nodes, doing only the first two, sit at 53.4%.

That headroom difference explains why raising concurrency helped baseline more
than Gantry (20.5% versus 15.8% at P95) and why the Gantry-to-baseline P95 ratio
worsened from 1.0823 to 1.1459. Feeding more concurrent streams to a node with
no spare CPU does not help it.

## Byte reduction is unaffected and remains the headline

| Run | ACR bytes | Byte reduction | Pulls B/G | Peer bytes served | Fallbacks |
| --- | ---: | ---: | ---: | ---: | ---: |
| downloads=3 | 45.008 TB to 154.787 GB | 99.656% | 1000 / 2 | 42.817 TB | 0 |
| downloads=6 | 47.165 TB to 256.512 GB | 99.456% | 1000 / 4 | 42.734 TB | 0 |

Both runs reported `FAIL` only because they were configured with a maximum
Gantry-to-baseline P95 ratio of 1.0. Every other sample in `RESULTS.md` used
3.0, which both runs pass. No pod in either run reported `ErrImagePull` or
`ImagePullBackOff`, and neither run recorded an origin fallback.

## Conclusions

1. Pod startup on this workload is almost entirely image pull. Scheduling and
   container creation are negligible.
2. `max_concurrent_downloads = 6` is worth taking. It cut P95 by 15-20% for
   both phases at negligible resource cost.
3. `max_concurrent_unpacks = 1` leaves roughly 333s of strictly serialized work
   per node, now 36-44% of the pull. Overlayfs supports the `rebase` capability
   needed to parallelize it, so this is the largest untested lever.
4. Gantry's constraint is node CPU, not network. At 90.2% P95 CPU it cannot
   convert extra download concurrency into speed, so raising concurrency alone
   widens the gap against baseline rather than closing it.
5. Gantry's value on this workload is the 99.5% reduction in registry egress
   and origin pulls, not pod startup latency, which stays 15-22% above baseline.

## Suggested next experiments

- `max_concurrent_unpacks = 4` at `max_concurrent_downloads = 6`, changing one
  variable from the run above. Watch disk busy, which is already at 73.7% P95.
- A larger node SKU or a smaller peer-serving fan-out for the Gantry phase, to
  test whether Gantry latency is CPU-bound as the data suggests.
- Do not raise `max_concurrent_downloads` past 6 until unpack concurrency is
  addressed, since byte-waiting is no longer the majority of baseline pull time.

## Reproducing

Per-layer and per-image unpack durations, with the parallel flag:

```bash
r=/var/lib/gantry-benchmark/artifacts/<run-id>
grep -ao 'msg=\\"layer unpacked\\" duration=[0-9.]*[a-z]*' "$r/baseline-performance.json"
grep -ao 'msg=\\"image unpacked\\"[^|]\{0,400\}' "$r/baseline-performance.json"
grep -ao 'parallel=[a-z]*' "$r/baseline-performance.json" | sort | uniq -c
```

Audit-derived latency decomposition:

```bash
jq -c '{startup: .baseline.azure.audit.pod_startup_latency,
        scheduling: .baseline.azure.audit.scheduling_latency,
        post_bind: .baseline.azure.audit.post_bind_startup_latency}' "$r/comparison.json"
```

Node resource utilization is under `.prometheus[] | select(.name == "<capture>")
| .response.data.result` in the phase performance artifacts, using capture names
`node_cpu_busy_ratio`, `node_disk_busy_ratio`, and
`node_network_receive_utilization_ratio`.
