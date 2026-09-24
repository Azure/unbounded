# Racer load generator

Test-only load generator with two workload modes. The default `-mode=racer`
provides a synthetic origin and full-object download workers using the Racer Go
SDK over Unix sockets. `-mode=container-image` provides a fake HTTP registry and/or
simulated image pulls through Gantry. Downloaded bytes are counted and discarded.

## Build and try it

From the repository root:

```sh
make racer-loadgen-build
mkdir -p "$PWD/tmp/loadgen"
./bin/racer-loadgen \
  -endpoint="$PWD/tmp/loadgen/origin" -origin-socket="$PWD/tmp/loadgen/origin" \
  -listen=127.0.0.1:8080 -footprint=32MiB -object-size=8MiB -duration=15s
```

This smoke test downloads directly from its own origin. During the run,
`http://127.0.0.1:8080/metrics` exposes Prometheus metrics and `/healthz` reports
process health. Shutdown logs include byte and download totals.

## Use with a cache

Point `-endpoint` at an existing Racer client socket and `-origin-socket` at the
origin socket configured for that cache. The origin socket's parent directory
must exist and be writable. Keep `-footprint` and `-object-size` identical across
origins sharing the dataset; footprint must be an exact multiple of object size.

Use `./bin/racer-loadgen -h` for all flags and defaults, including concurrency,
Zipf sampling, and timeouts. Omit `-duration` to run until interrupted.

## Container-image mode

Use separate processes with `-role=registry` and `-role=load`, or one process per
host with `-role=both`. The registry serves digest-addressed OCI manifests/configs
and deterministic opaque layer bytes with real SHA-256 digests. Layers are not
tar archives: this measures manifest-driven download/verification throughput,
not containerd image commit, unpack, or container startup.

The separate puller discovers descriptors at the registry's `/loadgen/catalog`;
combined mode uses its own prepared catalog without an HTTP catalog request.
Every manifest, config, and layer GET then goes to `-gantry-endpoint` using a
digest and `?ns=<registry-namespace>`. It verifies size and SHA-256, discards
layers with bounded memory, and fetches layers concurrently. It never follows
redirects or fetches payload directly from the registry. There is no tag lookup,
client-side blob cache, cross-image deduplication, or authentication workload.
The registry is anonymous HTTP; the client accepts HTTP or HTTPS origin URLs.

### Local Gantry benchmark

Start a registry (eight images, four 64 MiB layers each):

```sh
./bin/racer-loadgen -mode=container-image -role=registry \
  -listen=127.0.0.1:18080 -registry-listen=127.0.0.1:18081 \
  -footprint=2GiB -object-size=64MiB -layers-per-image=4
```

Configure the existing Gantry deployment's upstream list with this entry, using
an endpoint reachable from **every** Gantry origin node (loopback works only for
a single-node local setup):

```yaml
upstream_registries:
  - name: image-fixture.test
    endpoint: http://127.0.0.1:18081
```

For operator-managed Gantry, select a dedicated user-managed P2PCache through
`unbounded-cloud.io/gantry-backing: "true"`, following the
[Gantry guide](../../docs/content/guides/gantry.md#operator-managed-enablement).
The operator validates full node coverage and generates the backend arguments;
editing `content_backend` in its ConfigMap does not select Racer.

For a standalone Gantry process, set `content_backend: racer` and
`racer_cache_name: gantry` in its YAML, or pass `--content-backend=racer` and
`--racer-cache-name=gantry`, and provide the matching Racer cache and socket
mounts. Without that standalone setting, Gantry uses direct distribution.

Keep other required Gantry configuration and upstream entries. Gantry owns its
Racer origin socket; the loadgen registry does not replace it. Wait for Gantry
and Racer readiness, then start the puller:

```sh
./bin/racer-loadgen -mode=container-image -role=load \
  -listen=127.0.0.1:18082 -registry-url=http://127.0.0.1:18081 \
  -registry-namespace=image-fixture.test -gantry-endpoint=http://127.0.0.1:5000 \
  -concurrency=4 -layer-concurrency=3 -exponent=1 -seed=42 -duration=60s
```

All roles expose `/healthz`, `/readyz`, and `/metrics` on `-listen`. Health is
process health. Registry readiness requires hashing the entire dataset first;
memory retains descriptors and metadata, not layer bodies. The separate `load` role retries
catalog discovery until successful or interrupted. Its readiness means catalog
discovery succeeded, not that Gantry pulls are succeeding. `-duration` starts
after preparation/discovery, so hashing is outside the measurement interval.
At shutdown, in-flight canceled pulls count as errors and consumed bytes remain
counted. Final pull and byte totals are logged before exit; normal duration
expiry does not make the process exit nonzero when individual pulls failed.

### Combined per-host benchmark

Run an identical registry and client on every participating Gantry/Racer node:

```sh
./bin/racer-loadgen -mode=container-image -role=both \
  -listen=:18082 -registry-listen=:18081 \
  -footprint=80GB -object-size=1GB -layers-per-image=80 \
  -registry-namespace=image-fixture.test -gantry-endpoint=http://127.0.0.1:5000 \
  -concurrency=8 -layer-concurrency=8 -timeout=30m -gantry-ready-timeout=10m
```

This is one deterministic 80 GB image with 80 unique 1 GB layers per node.
Keep all dataset flags and the registry namespace identical across nodes.
Each process hashes the full dataset at startup and generates bodies on demand;
it does not retain 80 GB in memory. Do not set `-registry-url` in combined mode.
Every client payload still goes through its configured Gantry endpoint, including
when that process also hosts the registry. Gantry's origin fetches can reach the
registry independently of the client's readiness.

Management and registry listeners start before hashing. The registry returns
503 while preparing, then serves immediately. Only after preparation does the
client probe Gantry's `/v2/` startup gate, requiring HTTP 200 and the
`Docker-Distribution-API-Version: registry/2.0` header. Probes have a five-second
deadline and a one-second retry delay, bounded overall by
`-gantry-ready-timeout`. Expiry exits nonzero; interruption cancels hashing,
probes, or pulls and shuts down both listeners. Combined `/readyz` becomes 200
and `-duration` starts only after this gate opens. This is initial readiness,
not continuous health or proof of Racer acceleration; subsequent fallback and
pull failures remain visible in Gantry and client metrics.

### Dataset and concurrency

| Flag | Meaning in image mode |
| --- | --- |
| `-footprint` | Registry's total unique layer bytes (default 512 GB); metadata is additional |
| `-object-size` | Bytes per layer (default 1 GB) |
| `-layers-per-image` | Layers per image (default 4, maximum 1024) |
| `-concurrency` | Concurrent image pulls per process (default 4) |
| `-layer-concurrency` | Concurrent layer GETs per image (default 3) |
| `-exponent` | Zipf image-popularity exponent (default 1; 0 is uniform) |
| `-seed` | Sampling seed; random if omitted, printed in readiness log |
| `-timeout` | Whole-image deadline, catalog-request deadline, and registry response write timeout (default 5m) |
| `-gantry-ready-timeout` | Combined mode's bounded Gantry startup wait after local hashing (default 10m) |

Image count is `footprint / object-size / layers-per-image`; both divisions must
be exact. There are at most one million layers and 100,000 images. Layers are
unique across images. Identity depends on footprint, layer size, and layer
index; changing the sampling seed preserves content and warm cache keys.
The puller takes the dataset from the catalog; dataset flags apply to the
registry. Racer-only `-endpoint`, `-origin-socket`, `-page-concurrency`, and `-ttl`
do not configure the image data path. Gantry/Racer controls its own page fetching
and metadata TTL. The fixture serves HEAD, full GET, and byte-range GET with
digest/length/media-type metadata; it does not implement general registry push,
tag, listing, or authorization APIs.

### Metrics and cold/warm comparisons

Scrape the puller's `:18082/metrics` and registry's `:18080/metrics` in the separate
local example. In combined mode scrape only `:18082/metrics`, once per pod; it
exposes both independent metric families. Gantry fallback metrics remain on
Gantry's metrics endpoint. Image metrics are separate from the default Racer
download metrics:

- `racer_loadgen_image_pulls_total{result="success|error"}`: whole-image attempts.
- `racer_loadgen_image_pull_duration_seconds{result}`: verified image latency.
- `racer_loadgen_image_objects_total{kind="manifest|config|layer",result}`:
  per-object attempts.
- `racer_loadgen_image_received_bytes_total`: consumed payload bytes, including
  failed attempts; excludes catalog, HTTP headers, and unread error bodies.
- `racer_loadgen_registry_requests_total{kind="manifest|blob",request="head|full|range"}`
  and `racer_loadgen_registry_sent_bytes_total` with the same labels: origin
  traffic for known objects (blobs include configs). Range error bodies, if any,
  are included in sent bytes.

Use `rate(racer_loadgen_image_received_bytes_total[1m])` for bytes/sec and
`rate(racer_loadgen_image_pulls_total{result="success"}[1m])` for images/sec.
For p95 latency:

```promql
histogram_quantile(0.95,
  sum by (le) (rate(racer_loadgen_image_pull_duration_seconds_bucket{result="success"}[1m])))
```

Record a cold window with fresh cache keys, then a warm window with the same
dataset and namespace. To create fresh keys without deleting caches, use a new
Gantry upstream name and matching `-registry-namespace`, or change the dataset
footprint. Restarting the puller or changing its seed does not make the cache
cold. Compare upstream range bytes and Racer cache hits in both windows. A
working set larger than available slab capacity will continue evicting pages.

Successful HTTP pulls alone do not prove acceleration. Check
`gantry_racer_stream_total{outcome="completed"}`, positive splice byte counters,
zero `gantry_racer_tee_calls_total` / `gantry_racer_tee_bytes_total`,
`gantry_mirror_bytes_served_total{source="racer"}`, and
`gantry_racer_fallback_total`. Use Racer peer-page request and kTLS sendfile
counters when measuring multi-node transport. Gantry's completion metric reports
forwarding, not OCI digest acceptance; the loadgen client performs its own SHA-256
validation. Client SHA-256 and synthetic origin generation consume CPU; record
their CPU limits and utilization alongside
Gantry/Racer throughput. The client remains closed-loop with one-second backoff
after errors, so throughput includes backpressure and retry delays.

### Kubernetes example

Build the example's image tag:

```sh
make image-racer-loadgen-local CONTAINER_ENGINE=docker RACER_LOADGEN_IMAGE=racer-loadgen:local
```

Import `racer-loadgen:local` into your cluster's nodes (for kind, use
`kind load docker-image racer-loadgen:local --name <cluster>`), then use
[`e2e/racer/examples/container-image-loadgen.yaml`](../../e2e/racer/examples/container-image-loadgen.yaml).
It supplies a combined host-network DaemonSet on Site `racer-a`, with low resource
requests and no CPU/memory limits or explicit `GOMAXPROCS`/`GOMEMLIMIT` caps.
Change the Site selector and image tag as needed. Gantry runs in the pod network,
so its loopback is not the node's loopback. Configure this upstream on every
participating Gantry node, retaining other entries:

```yaml
upstream_registries:
  - name: image-fixture.test
    endpoint: http://image-loadgen-local-registry.unbounded-system.svc.cluster.local:18081
```

The example includes a ClusterIP Service with `internalTrafficPolicy: Local`
selecting the host-network loadgen pods on port 18081. All Gantry instances use
the same ordinary Service DNS name; no Gantry image or environment change is
needed. The fixture binds all host interfaces so the Service can reach its
registry port. Without a local endpoint, requests fail rather than routing to
another node's registry.

The Service sets `publishNotReadyAddresses: true` so registry traffic can reach
the pod before combined client readiness passes Gantry's startup gate. This
avoids a readiness cycle; it does not bypass registry preparation. Registry
requests return 503 while hashing, then succeed even while management `/readyz`
is still waiting for Gantry. Keep the Service selector independent of role and
readiness so it selects both registry-only staging pods and combined pods.

The example disables generic annotation scraping to avoid duplicate targets
from its two declared ports. Add one dedicated Prometheus job (or equivalent
PodMonitor selecting only `management`):

```yaml
- job_name: image-loadgen
  kubernetes_sd_configs:
    - role: pod
      namespaces:
        names: [unbounded-system]
      selectors:
        - role: pod
          label: app=image-loadgen
          field: status.phase=Running
  relabel_configs:
    - source_labels: [__meta_kubernetes_pod_container_name, __meta_kubernetes_pod_container_port_name]
      action: keep
      regex: loadgen;management
    - source_labels: [__meta_kubernetes_pod_node_name]
      target_label: node
```

Then apply the example and observe preparation and pull metrics:

```sh
kubectl apply -f e2e/racer/examples/container-image-loadgen.yaml
kubectl -n unbounded-system logs <image-loadgen-pod> --tail=30
kubectl -n unbounded-system get pods -l app=image-loadgen -o wide
```

Ports 18081 and 18082 must be free on participating nodes. The puller uses host
networking to reach Gantry's node-local `127.0.0.1:5000`. Ensure every Gantry/Racer
origin node has a prepared registry before measuring fleet throughput. This workload
uses Gantry's existing P2PCache and requires no loadgen socket mounts or extra
P2PCache. Keep the default unbounded duration for the DaemonSet; a finite duration
causes Kubernetes to restart it. Delete the example to stop the benchmark:

```sh
kubectl delete -f e2e/racer/examples/container-image-loadgen.yaml
```

For automated real Gantry/Racer verification, run `make e2e-gantry-racer-build`
and `make e2e-gantry-racer` as described in the [native test guide](../../e2e/racer/README.md).
