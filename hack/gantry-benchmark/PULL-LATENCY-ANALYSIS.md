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
That is worth watching as a headroom limit, but the delivery timeline below
shows it is not what makes the Gantry phase slower.

## The Gantry phase is slower because of its cold start, not contention

Peer fetch outcomes over the whole Gantry-cold phase, summed across 1000 nodes:

| Outcome | Count | Mean duration |
| --- | ---: | ---: |
| busy (HTTP 429) | 1,162,435 | 0.0015s |
| hit | 41,789 | n/a |
| stall | 31,339 | 60.0007s |
| notfound | 10,164 | n/a |
| unavailable | 4,668 | n/a |
| digest_mismatch, auth, protocol, server, local error | 0 | n/a |

Only 3.3% of attempts succeed, and there are 27.8 rejections per success. Those
headline numbers invite two wrong conclusions, so both are worth ruling out.

First, the rejections are almost entirely a startup transient. 99.6% of them
occur in the first six minutes and 56% in the second minute alone, falling to
zero by minute eleven. At 1.5ms each they cost about 1.7 seconds per node in
total.

Second, the stalls are not lost work. The 60.0007s mean is `PeerFetchTimeout`
firing, but `livePeerStream` streams through to the containerd-facing response
and records the verified byte offset, and re-selection resumes from that offset.
A stall costs a DHT lookup and a redial, not the delivered prefix. Stalls also
hold steady at roughly 3,700 per minute from minute four to minute ten, which is
exactly when delivery runs at peak rate.

What does explain the gap is the rate at which layer bytes reach nodes:

| Minute | Layer GB served, all nodes | MB/s per node | Cumulative |
| ---: | ---: | ---: | ---: |
| 0 | 0 | 0.0 | 0.0% |
| 1 | 936 | 15.6 | 2.2% |
| 2 | 2,220 | 37.0 | 7.3% |
| 3 | 4,306 | 71.8 | 17.4% |
| 4 | 4,953 | 82.6 | 28.9% |
| 5 to 9 | about 5,100 each | about 85 | 88.1% |
| 10 | 4,617 | 77.0 | 98.9% |
| 11 | 487 | 8.1 | 100.0% |

Baseline reaches its full network rate inside the first minute because ACR is
already at capacity. Gantry needs four minutes, because at the start of a cold
run almost no node holds the image and there is nothing to serve. Supply has to
be built before it can be consumed, and the 429 storm is the visible signature
of that shortage rather than a cost in its own right.

Once the swarm is warm, Gantry is the faster of the two. Node network receive
peaks near 350 MB/s during the Gantry phase against about 182 MB/s for baseline.

At the observed steady rate of about 5,087 GB per minute, all 42.95 TB would
move in 8.4 minutes. It took roughly 11, so the cascade ramp costs about 2.6
minutes. Byte delivery finishes at minute 11 while the phase runs to minute 18;
that closing stretch is the serialized unpack draining, which matches the 334s
of measured unpack work per node.

| Component | Time | Attribution |
| --- | ---: | --- |
| Cascade cold-start ramp | about 2.6 min | Gantry only |
| Steady-state delivery | about 8.4 min | Gantry faster than baseline |
| Serialized unpack tail | about 5.6 min | both phases |

Gantry's total penalty against baseline in this run was 1.7 minutes, less than
the ramp alone, because its steady-state throughput recovers part of the deficit.

### The ramp exists because every node pulls layers in the same order

containerd walks the manifest in order, so all 1000 nodes request layer
positions in the same sequence. The containerd journal records each
`layer unpacked` event with its digest, and the completions fall into strict
waves. Seconds are measured from the first event in the phase.

| Gantry-cold layer | events | first | median | last |
| --- | ---: | ---: | ---: | ---: |
| `6a98e6e4b146` | 2000 | 0 | 114 | 232 |
| `b407264bc620` | 2000 | 8 | 121 | 243 |
| `06e50d442e52` | 2000 | 15 | 129 | 255 |
| `8d171b1dbb8e` | 2000 | 21 | 138 | 266 |
| `c3e575abbc22` | 2000 | 28 | 147 | 275 |
| `5843544c2584` | 2000 | 36 | 155 | 285 |
| `1af94d827f87` | 2000 | 43 | 163 | 297 |

Each layer's first completion trails the previous one by roughly 7 seconds, and
the medians advance in the same order. Independent per-node layer selection
would instead produce overlapping distributions starting near zero. The 7 second
step also matches the serialized unpack cost, so the wavefront advances at about
the speed of one layer.

Extrapolating that step across 40 layers puts the first seed for the final layer
near 280 seconds, which is the same scale as the observed four minute ramp.
Until some node has worked through the preceding layers, the later positions
have no seeder anywhere in the swarm, so demand for them cannot be served at any
price.

Baseline is the control. It shows the same stagger, with first completions at 0,
6, 13, 22, 29, 36 and 44 seconds, because it uses the same manifest order. It
has no ramp at all, because ACR already holds every layer at t=0. Uniform
ordering is therefore harmless when supply exists and expensive only when supply
has to be built.

This also re-explains the 429 storm. The rejections are not diffuse contention:
1000 nodes want the same layer at the same moment, which saturates whichever few
nodes hold it. Concentrated demand on a narrow wavefront is the cause, and the
serve cap is what makes it visible.

The journal capture is truncated to 7 distinct layers per phase, and the 2000
events per layer indicate some line duplication in the capture, so the ordering
and the 7 second step are measured while the 40 layer figure is arithmetic.
Capturing the full journal, or timestamping per-digest mirror advertisements,
would close that gap.

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
4. The Gantry phase is slower than baseline because of its cold start. Delivery
   takes four minutes to reach full rate while baseline is there in one, costing
   about 2.6 minutes. Neither the 429 storm nor the 60s stalls are the cost:
   the first is a startup transient at 1.5ms each, and the second preserves the
   delivered prefix and occurs while delivery is at peak rate.
5. The ramp exists because every node walks the manifest in the same order, so
   the swarm seeds one layer position at a time instead of all 40 at once. Layer
   completions arrive in strict waves about 7 seconds apart. Baseline shows the
   same ordering and no ramp, which isolates ordering as costly only when supply
   must be built rather than already existing at the origin.
6. Once warm, Gantry delivers faster than pulling from the registry, peaking
   near 350 MB/s per node against about 182 MB/s for baseline.
7. `PeerFetchTimeout` is a total request deadline rather than a no-progress
   deadline, so the throughput a stream must sustain to survive it scales with
   layer size: 17.9 MB/s for a 1 GiB layer, 716 MB/s for a 40 GiB one. This did
   not dominate these runs, but it does not scale to larger layers. containerd's
   own `image_pull_progress_timeout` uses no-progress semantics by contrast.
8. Gantry's value on this workload is the 99.5% reduction in registry egress
   and origin pulls, not pod startup latency, which stays 15-22% above baseline.

## Suggested next experiments

- `max_concurrent_unpacks = 4` at `max_concurrent_downloads = 6`, changing one
  variable from the run above. Watch disk busy, which is already at 73.7% P95.
- Desynchronize layer acquisition order across nodes so the swarm seeds every
  layer position at once instead of advancing a single wavefront. With 1000
  nodes and 40 layers, starting nodes at staggered offsets would put a seed on
  every position within roughly one layer-time rather than 40. This targets the
  ramp, which is the entire Gantry penalty.
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

Per-layer completion order, which shows the wavefront:

```bash
grep -ao 'time=\\"[0-9T:.Z-]*\\" level=debug msg=\\"layer unpacked\\" duration=[0-9.a-z]* layer=\\"sha256:[0-9a-f]*' \
  "$r/gantry_cold-performance.json"
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

Peer fetch outcomes, DHT results, and layer bytes served use the same path with
capture names `gantry_peer_outcomes`, `gantry_peer_duration`,
`gantry_dht_outcomes`, `gantry_dht_duration`, and `gantry_mirror_bytes`. These
are counters, so a phase total is the last sample minus the first, summed across
pods; bin those per-sample increases by minute to recover the timelines above.
