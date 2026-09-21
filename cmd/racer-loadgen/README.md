# Racer load generator

Each process runs a deterministic synthetic HTTP origin, full-object download
workers using the [Racer Go SDK](../../pkg/racer/README.md), and Prometheus metrics
on one listener. Bytes are generated without origin disk storage; downloaded bytes
are counted and discarded. Each download performs HEAD followed by aligned 4 MiB
page GETs (the last page is clipped at EOF).

## Local smoke test

From the repository root, using the root module's Go toolchain:

```sh
GOTOOLCHAIN=go1.26.6 go run ./cmd/racer-loadgen \
  -endpoint=http://127.0.0.1:8080 \
  -footprint=32MiB -object-size=8MiB -duration=15s
```

This reads directly from the same process's origin. While it runs, inspect
`http://127.0.0.1:8080/healthz` and `http://127.0.0.1:8080/metrics`.
Startup logs include configuration; shutdown logs include final totals.

## Flags and workload

| Flag | Default | Meaning |
| --- | --- | --- |
| `-endpoint` | `http://racer-loadgen-volume.racer-system.svc` | Download base URL; the default uses port 80. |
| `-listen` | `:8080` | Origin, `/healthz`, and `/metrics` listener. |
| `-footprint` | `512GB` | Global logical dataset size. |
| `-object-size` | `1GB` | Size of every object. |
| `-exponent` | `1` | Rank sampling weight proportional to `rank^-exponent`. |
| `-seed` | random | Optional signed int64 sampling seed. |
| `-concurrency` | `4` | Concurrent full-object downloads per process. |
| `-page-concurrency` | `8` | Concurrent page GETs per object. |
| `-timeout` | `5m` | Deadline for an entire object, including HEAD. |
| `-ttl` | `1h` | Origin metadata freshness. |
| `-duration` | `0` | Run indefinitely; a positive duration ends the run. |

Sizes accept decimal units (`GB` = 1,000,000,000 bytes) and binary units such as
`GiB`. Footprint and object size must be positive and exactly divisible, with at
most 1,000,000 objects. The sampler builds an O(object count) cumulative
distribution and uses binary search per selection. All finite nonnegative
exponents are supported: zero is uniform, one is Zipf-like, and larger values
concentrate reads on the hottest ranks.

The default is exactly **512,000,000,000 logical bytes in 512 objects**, shared
across all nodes, not 512 GB per node. Object paths include dataset geometry and a
format version; equal geometry gives identical paths and bytes on every process.
The sampling seed does not change content. Keep footprint and object size equal
across origin pods. A geometry change selects a different namespace; old cache
entries can remain until eviction. Logical footprint is distinct from physical
cache occupancy: page/slab allocation rounding and metadata affect on-disk space,
and configured cache capacity limits the resident subset.

There are no automatic data retries. A worker waits one second after a failed
download before selecting again and logs its error (at most once per worker per
second). In-flight operations canceled at shutdown count
as errors, including when a finite run ends.

Example flag sets (append to the local command or use as container args):

```sh
# Uniform churn: choose a footprint larger than the effective cache capacity.
-footprint=512GB -object-size=1GB -exponent=0

# A concentrated hot set over the same shared dataset.
-footprint=512GB -object-size=1GB -exponent=2

# Standalone only: fixed seed and bounded run (completion order can still vary).
-seed=42 -duration=10m
```

Keep `-duration=0` in a DaemonSet: Kubernetes restarts containers that exit,
including successful finite runs. Remove the workload to end a cluster run.

## Build and test

The SDK and load generator share the root Go module. From the repository root:

```sh
GOTOOLCHAIN=go1.26.6 go build -o bin/racer-loadgen ./cmd/racer-loadgen
GOTOOLCHAIN=go1.26.6 go test -race ./pkg/racer/... ./cmd/racer-loadgen/...
```

## Cluster configuration

Set `-endpoint` to the Racer volume Service in your deployment namespace.
The default retains the standalone `racer-system` Service address; override it
when using a different namespace. Configure the volume's origin to reach the
generator listener on port 8080. Match node selectors, pod labels, Service
selectors, and the volume Service's `racer.unbounded-cloud.io/universe` annotation
to the participating dataplane nodes.

For node-local traffic, configure both volume and origin Services with
`internalTrafficPolicy: Local` and provide a ready generator pod on every
participating dataplane node. Origin health is independent of download success,
so readiness does not create an origin/dataplane startup cycle. Initial download
failures are possible while the controller and dataplane reconcile.

Inspect a controller-allocated volume listener port with:

```sh
kubectl -n "$NAMESPACE" get service racer-loadgen-volume \
  -o jsonpath='{.metadata.annotations.racer\.unbounded-cloud\.io/allocated-port}{"\n"}'
```

Tune resources and `GOMAXPROCS` alongside object/page concurrency to prevent the
generator from becoming the bottleneck, and reserve enough node capacity for the
dataplane. Defaults allow up to 32 concurrent page GETs per process. Watch CPU
throttling, memory, and network utilization; origin byte generation also consumes
CPU. The first HEAD for each object hashes its synthetic contents with SHA-256;
subsequent HEADs reuse the checksum. This cold metadata cost is part of warmup.
ETags are strong quoted lowercase content checksums, consistent across replicas.
Memory is bounded by worker buffers and per-object sampler/checksum state rather
than footprint bytes. The generator needs no data volume.

## Prometheus

Configure Prometheus to scrape `/metrics` on port 8080 on each generator pod,
rather than a load-balanced Service address. Metrics are process-local
counters/histograms:

- `racer_loadgen_received_bytes_total`: bytes written to the discard destination
  during downloads, including bytes from failed downloads; excludes HEAD and wire
  overhead.
- `racer_loadgen_downloads_total{result="success"|"error"}`: completed attempts.
- `racer_loadgen_download_duration_seconds{result="success"|"error"}`: object
  duration histogram.

Aggregate received throughput in bytes/second:

```promql
sum(rate(racer_loadgen_received_bytes_total[1m]))
```

Successful full-object p95 latency in seconds:

```promql
histogram_quantile(0.95,
  sum by (le) (rate(racer_loadgen_download_duration_seconds_bucket{result="success"}[5m]))
)
```

Error fraction (multiply by 100 for percent):

```promql
sum(rate(racer_loadgen_downloads_total{result="error"}[5m]))
/
sum(rate(racer_loadgen_downloads_total[5m]))
```

Scope queries to the intended scrape job/namespace when running multiple tests.
Throughput includes partial failed attempts; use the error fraction alongside it.
