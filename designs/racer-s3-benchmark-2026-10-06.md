# Racer S3 benchmark: October 6, 2026

## Result and scope

The fixed Racer object sidecar delivered **561.560813 GiB/s of SHA-256-verified
object content across 128 nodes at concurrency 8 per node**, with **1,079,645
successful reads and zero failure increments in the nominal 120-second warm
measurement**. Independent Prometheus scrapes measured **561.190955 GiB/s**.
This is aggregate application delivery from node-local memory cache, not
network-wire throughput, origin capacity, or a cold-cache benchmark.

There were **six incomplete startup deliveries on four nodes before that window**.
They closely match six dataplane mid-stream `Overloaded` terminations. Per-cache
Flight admission exhaustion is strongly suspected, but the quota-to-request
causal join is not proven. These failures were detected, not silently accepted,
and were not fixed by this work. A clean warm window does not make the entire
startup error-free.

The task used a simple iterative sequence: direct baseline, sidecar baseline,
profile one bottleneck, apply a narrow listener fix, and ramp to 16 then 128
consumers. It did not expand into a new object-store deployment or dataplane
admission redesign. The origin is a synthetic read-only S3 fixture inside
`racer-loadgen`, **not Garage** or a production S3 service.

## Configuration and measurement

- Azure `Standard_D8ds_v6` nodes, 8 vCPUs each. Loadgen, sidecar, and Racer
  dataplane share the node CPU budget; these are not isolated proxy-capacity tests.
- Catalog: 16 objects of 64 MiB (67,108,864 bytes), 1 GiB logical working set,
  bucket `benchmark`, seed `benchmark-v1`, shuffle profile. SHA-256 verification
  remained enabled in every reported run.
- Direct path: consumer HTTP GET to `http://racer-loadgen-origin:8080`, using
  the node-local origin Service. Sidecar path: HTTP GET to
  `http://127.0.0.1:8080`, then Racer's local client socket and dataplane.
- Consumer placement: `racer-s3-benchmark=enabled`; concurrency is per consumer,
  not fleet-wide. C8 on 128 nodes means 1,024 configured workers.
- The consumer manifest pins catalog, endpoint, verification, and control-file
  arguments at `deploy/racer-loadgen/s3/consumer.yaml:44-60`; placement is at
  lines 25-35. CPU requests have no CPU limits (lines 92-97 and 122-127).
- The S3 client makes ordinary GETs (`cmd/racer-loadgen/s3.go:105-126`). The
  fixture accepts only GET/HEAD and generates content through its catalog reader
  (lines 130-139 and 183-225); it is not an authenticated writable object store.
- Verified bytes are credited only after a completely successful verified batch
  (`cmd/racer-loadgen/pull.go:244-267`). They are distinct from received bytes,
  which can include incomplete or failed operations.

The sampler takes before/after per-pod counters and sums each pod's byte delta
divided by its monotonic scrape-midpoint interval. GiB means 2^30 bytes. The
durations below are requested sampling durations, not a claim of perfectly
simultaneous fleet boundaries; total sampler elapsed times were 121.88 seconds
for 16 nodes and 125.07 seconds for 128 nodes, including collection overhead.
Both fleet aggregates were complete, with all selected pods valid and no sampler
errors. Gauges are endpoint snapshots, not interval averages. Two samples cannot
establish second-by-second stability or detect every possible counter reset.

## Single-node results

All rows below had zero measured failure increments. The initial direct C1
sample taken before ConfigMap propagation was still C0 and is excluded.

| Path / version | Concurrency | Requested seconds | Verified GiB/s |
| --- | ---: | ---: | ---: |
| Direct synthetic origin | 1 | 45 | 1.105760915 |
| Direct synthetic origin | 8 | 60 | 4.526892645 |
| Unfixed sidecar | 1 | 60 | 1.216214605 |
| Unfixed sidecar | 8 | 60 | 4.308540036 |
| Unfixed sidecar | 32 | 60 | 4.235374555 |
| Unfixed sidecar, immediate pre-fix repeat | 8 | 60 | 4.306202245 |
| Fixed sidecar | 8 | 60 | 4.509739296 |
| Fixed sidecar | 32 | 60 | 4.541319762 |

The fixed C8 result is **4.7% above the immediately preceding C8 repeat**; fixed
C32 is **7.2% above the unfixed C32 run**. These are observed sequential-run
differences, not formal statistical confidence intervals or universal speedup
guarantees. C32 did not materially outperform C8 after the fix, so the fleet
ramp used C8. The direct baseline exercises a different path and is not an
isolated upper bound on Racer capacity.

At the unfixed C32 plateau, the node used 7.899 of 8 cores: loadgen 4.253,
sidecar 1.716, and dataplane 1.804 cores in the recorded CPU snapshot. Prometheus
showed memory hits and zero disk, peer, or origin activity in the stable window.
This supports CPU contention as the immediate plateau explanation, rather than
a demonstrated network-bandwidth ceiling.

### Profile-guided fix

The original `netutil.LimitListener` wrapper hid the accepted TCP connection's
`io.ReaderFrom` method, preventing the HTTP Unix-to-TCP splice path. The narrow
replacement retains that method when the underlying connection supports it
(`cmd/racer-object/listener.go:49-54,75-82`), retains slot admission and releases
each slot once on close (lines 26-45 and 62-72), and is actually used by the HTTP
server with the existing **128-connection limit**
(`cmd/racer-object/main.go:416`). It does not remove the connection bound.

Two 30-second C8 CPU profiles recorded **49.57 CPU-seconds before** and
**23.09 CPU-seconds after**, a **53.4% reduction in sampled sidecar CPU**. The
pre-fix profile was dominated by read/write syscall copying; after the fix,
`net.spliceFrom` accounted for 82.85% of the profile. This confirms the intended
fast path was active; it does not imply a 53.4% fleet throughput increase.
The local synthetic microbenchmark's throughput ranges overlapped, so no local
microbenchmark speedup is claimed.

The final-byte/Complete protocol was untouched. The SDK still withholds the last
byte until the Complete frame validates (`pkg/racersdk/value.go:427-484`), and
the sidecar aborts a failed committed response rather than appending XML or
returning a successful EOF (`internal/racerobject/sidecar.go:164-170`). Regression
assertions exercise the real SDK, Unix sockets, HTTP, underlying TCP ReaderFrom,
payload integrity, and keepalive (`cmd/racer-object/listener_test.go:65-165`).
Limiter lifecycle assertions cover admission, release, deadlines, and closure
(lines 227-324).

## Fleet results and independent check

| Fixed sidecar, C8 per node | Requested seconds | Verified GiB/s | Successful reads | Failure delta | Prometheus GiB/s |
| --- | ---: | ---: | ---: | ---: | ---: |
| 16 nodes | 120 | 68.888375343 | 132,450 | 0 | 68.845833 |
| 128 nodes | 120 | 561.560813020 | 1,079,645 | 0 | 561.190955 |

For 128 nodes, per-node verified delivery was **3.7364 minimum, 4.4645 median,
and 4.8022 maximum GiB/s**. These are within-run node differences, not repeat-run
error bars.

The independent 128-node query used fixed evaluation time
**2026-10-06T15:21:15Z**, with `[2m]` covering `(15:19:15Z, 15:21:15Z]`:

```promql
sum(rate(racer_loadgen_verified_bytes_total{job="kubernetes-pods",namespace="unbounded-system",app_kubernetes_io_name="racer-s3-loadgen"}[2m])) / 1073741824
```

It returned 561.1909548886018 GiB/s, 0.0659% below the sampler. All 128 consumers
had positive delivery, C8 at every available window scrape, and `up=1`; verified
byte and failure counters had zero observed resets. Error and incomplete-failure
increases were zero. There were exactly two verified-byte samples per consumer
in `[2m]`, so Prometheus extrapolation corroborates the aggregate, not exact
sampler boundaries or an exact event count. Its extrapolated successful-pull
increase was 1,077,492.63, not the sampler's 1,079,645 reads.

Node-matched dataplane coverage was 128, with 35,906.58 memory hits/s and zero
disk hits, peer hits, origin fills, or request errors per second. All 1,500
dataplanes had origin-fill coverage with zero fills, and all 1,500 origin pods
had byte-rate coverage with zero origin GiB/s. The independent 16-node check at
15:16:18Z likewise reported memory-only delivery. Thus the reported scale result
measures repeated local memory-cache delivery, not cross-node transfer scaling.

## Startup failures: disclosed and unresolved

Six `reason=incomplete` failures occurred across four new consumers during C8
activation. The diagnostic terminal ring matched the node/count distribution:

| Node suffix | UTC terminal times | Dataplane sent / expected MiB |
| --- | --- | --- |
| `00s` | 15:18:45.081 | 32 / 64 |
| `01t` | 15:18:51.783 | 48 / 64 |
| `02d` | 15:18:55.270, 15:18:55.278 | 48 / 64 each |
| `02t` | 15:18:58.340, 15:18:58.518 | 32 / 64, 48 / 64 |

Each distinct dataplane request had paired `NextSlice error=Overloaded` and
`ClientWrite error=Overloaded` records. These pairs are one termination each,
not twelve failures. First terminal events preceded consumer warnings by about
3-12 ms. Consumer logs lack shared request IDs, so the cross-layer match is
strong correlation rather than a literal end-to-end trace join. Dataplane sent
bytes are not a measurement of bytes received by the consumer.

Immediately preceding admission events reported Flight global usage 6/12 and
cache-local usage **6/6**, requesting one additional slot. These records lacked
request IDs, and workers differed across admission and delivery events.
Therefore **cache-local Flight saturation is strongly suspected, not proven as
the exact causal resource for each request**. Startup counters showed peer
acquisition on these nodes, no origin fills, and no disk-hit increments; the
stable-window zero peer rate must not be retroactively applied to startup.

The failures predated the sampler start at 15:19:08.946161Z and the independent
Prometheus window. No silent corruption was observed: HTTP 200 headers did not
turn partial bodies into verified successes. The short-200 test explicitly
requires an incomplete failure and zero verified bytes
(`cmd/racer-loadgen/s3_test.go:204-243`). This agrees with the guide's requirement
to discard partial output (`docs/content/guides/racer-object.md:210-215`).

No dataplane change or claim of a startup fix is included. If zero-error cold
activation is required, the next bounded experiment should correlate a cold-page
Flight rejection with the same request/page as NextSlice under live-equivalent
quotas, then compare abrupt C8 with a lower-concurrency warmup. Deliberate verified
catalog warmup is a proposed mitigation, not a tested guarantee. Do not suppress
the failures or raise quotas blindly to make this benchmark appear clean.

## Deployment identity and reproducibility

Integration note: while these measurements ran, the original `racer-v2` branch
advanced to the ClusterVolume API. Final integration preserves that concurrent
work and adapts the new deployment base to `ClusterVolume`, `spec.type: Cache`,
and `--volume`. The images and live cluster measured below still use the earlier
ClusterCache API. The final integrated source is tested but was not rebuilt or
deployed for these measurements. Do not apply the current base to the measured
legacy cluster without its corresponding controller/dataplane API migration.
The overlay below records historical image identity, not an API migration recipe.

The old 1,500-pod `DaemonSet/racer-loadgen` was replaced with the synthetic origin
plus `racer-object origin` adapter. `ClusterCache/gantry` was deleted after old
clients were removed; the Gantry DaemonSet remains idle. The new cache is
`ClusterCache/racer-object`. A separate `DaemonSet/racer-s3-loadgen` reached 128
labeled consumer nodes. The final sidecar debug listener was disabled before
the fleet measurements.

Actual published image tags:

| Role | Image tag |
| --- | --- |
| Synthetic origin and HTTP consumer | `ghcr.io/azure/racer-loadgen:66231a5fc3df56285fffb6e266cf3f88d2e566dc` |
| Origin adapter, unchanged during fix/ramp | `ghcr.io/azure/racer-object:66231a5fc3df56285fffb6e266cf3f88d2e566dc` |
| Fixed sidecar | `ghcr.io/azure/racer-object:cd0aae96acf7d5505209aad7c1a2efc283f84398` |

Image workflow runs: [loadgen 37480225309](https://github.com/Azure/unbounded/actions/runs/37480225309),
[baseline object 37480230361](https://github.com/Azure/unbounded/actions/runs/37480230361),
and [fixed object 37484636635](https://github.com/Azure/unbounded/actions/runs/37484636635).
The immediate pre-fix profile/repeat used the temporary profiling-enabled image
`74ca85e65` rather than the original baseline image. The inspected dataplane
diagnostic pod used `ghcr.io/azure/racer-dataplane:secure-zeroing-41ea7720f`;
current source is not a proven byte-for-byte reconstruction of that deployed
dataplane build.

To preserve the actual split in a future local `deploy/racer-loadgen/s3-run/`
overlay, use this recipe rather than assigning the fixed object image globally:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../s3
images:
  - name: ghcr.io/azure/racer-loadgen
    newTag: 66231a5fc3df56285fffb6e266cf3f88d2e566dc
patches:
  - target:
      kind: DaemonSet
      name: racer-loadgen
    patch: |-
      apiVersion: apps/v1
      kind: DaemonSet
      metadata:
        name: racer-loadgen
      spec:
        template:
          spec:
            containers:
              - name: origin
                image: ghcr.io/azure/racer-object:66231a5fc3df56285fffb6e266cf3f88d2e566dc
  - target:
      kind: DaemonSet
      name: racer-s3-loadgen
    patch: |-
      apiVersion: apps/v1
      kind: DaemonSet
      metadata:
        name: racer-s3-loadgen
      spec:
        template:
          spec:
            containers:
              - name: sidecar
                image: ghcr.io/azure/racer-object:cd0aae96acf7d5505209aad7c1a2efc283f84398
```

Deploying the same fixed object image for both adapters is an optional future
configuration, **not what ran here**. The catalog and cache-key namespace remain
identical in either case. Follow the [deployment README](../deploy/racer-loadgen/s3/README.md)
for prerequisites, rendering, C0 checks, and a controlled ramp. Reapplying the
base resets the control ConfigMap to C0; do not treat reapplication as resume.

Final sampling at **15:29:29Z** verified all **128 consumers at C0**, with zero
in-flight reads and zero byte deltas. All 128 consumer pods and 1,500 origin pods
were Ready. Only `ClusterCache/racer-object` remained. The deployment is retained
with load paused and sidecar profiling disabled.

For an explicitly authorized later resume in the intended cluster context:

```sh
timeout --signal=TERM --kill-after=10s 300s kubectl patch cm racer-s3-loadgen-control -n unbounded-system --type=merge -p '{"data":{"concurrency":"8"}}'
```

Allow up to about 90 seconds for ConfigMap projection in this environment; a
successful patch is not evidence that consumers applied it. Inspect applied
concurrency, in-flight reads, verification, and failures on all selected pods.
Retain the parent-owned C0 recovery procedure if any check fails.

## Validation and evidence retained

Prior implementation validation passed all seven focused Go packages/subpackages,
14 sampler tests, eight deployment tests, and focused race tests, as recorded
in the benchmark checkpoint. This documentation-only chunk does not rerun them
or run Go formatters; its validation is documentation diff review.

Operational evidence was read from the original workspace's
`tmp/racer-s3-benchmark-checkpoint.md` (phase results and image actions),
`tmp/racer-s3-prometheus-128.md` (fixed-time queries, coverage, and limitations),
and `tmp/racer-s3-startup-diagnosis.md` (terminal/admission evidence). Raw sampler
artifacts were collected in the benchmark worktree's `tmp/s3-*/summary.json`, including
`s3-direct-c1-active`, `s3-direct-c8`, `s3-sidecar-c1`, `s3-sidecar-c8`,
`s3-sidecar-c32`, `s3-sidecar-prefix-c8`, `s3-sidecar-fixed-c8`,
`s3-sidecar-fixed-c32`, `s3-sidecar-fixed-16nodes-c8`, and
`s3-sidecar-fixed-128nodes-c8`. Profiles are
`tmp/s3-sidecar-c8-cpu.pprof` and `tmp/s3-sidecar-fixed-c8-cpu.pprof`.
These temporary artifacts are preserved in the original workspace at
`tmp/racer-s3-benchmark-artifacts.tar.gz` before worktree cleanup, not committed.
The core results and caveats above are retained here so the report does not
depend on temporary files to be read.
