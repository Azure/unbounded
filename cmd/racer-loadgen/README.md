# racer-loadgen

Generate deterministic blobs and repeatedly consume them through either Gantry's
registry API or Racer's client Unix domain socket (UDS). Both backends use the
same catalog selection, worker admission, pacing, deadlines, integrity checking,
diagnostics, and metrics. Payloads are generated on demand, not retained in a
load-generator content cache.

## Workloads and backends

For independent raw blobs through Gantry:

```sh
racer-loadgen --backend=gantry --target=http://127.0.0.1:5000 \
  --namespace=loadgen.invalid --catalog-blobs=128 --blob-bytes=67108864 \
  --concurrency=8 --seed=benchmark-v1
```

Configure Gantry's upstream for `loadgen.invalid` to reach the synthetic HTTP
origin on `--listen` (default `:8080`). To measure the HTTP origin without Gantry,
point `--target` at that origin instead.

For the same blobs directly through Racer, without Gantry:

```sh
racer-loadgen --backend=uds --cache=racer-loadgen \
  --catalog-blobs=128 --blob-bytes=67108864 \
  --concurrency=8 --seed=benchmark-v1
```

Direct mode uses `pkg/racersdk` against
`/run/racer/racer-loadgen/client/socket` and serves the synthetic origin at
`/run/racer/racer-loadgen/origin/socket`. It starts no HTTP registry and ignores
`--target`, `--namespace`, and `--listen`. Metrics and probes still use
`--metrics-listen` (default `:9090`). An operational Racer dataplane and cache
configuration are prerequisites; loadgen does not start the dataplane.

Unit tests use `pkg/racersdk/racersdktest.NewClient` with a noncaching local
daemon and real SDK origin validation over temporary Unix sockets. They always
call the returned cleanup function; closing only the client leaves servers
running. These tests are not evidence of real Racer compatibility or performance.

Use a dedicated cache, not a cache whose origin socket Gantry already owns. The
origin directory must be owned by loadgen's UID and must not be group/world
writable. Existing directory permissions are not changed; missing directories
are created if permitted. The SDK provides exclusive ownership, stale-socket
recovery, and shutdown cleanup. See the
[standalone Kubernetes example](../../deploy/racer-loadgen/direct/README.md).

Every node eligible to supply origin data must have a running origin with the
same seed and catalog parameters. There is no upload step: Racer retrieves
synthetic data from the origin on demand. Blob keys are the SHA-256 digest bytes;
ETags are the quoted `sha256:<hex>` digest. Initial reads are unpinned, like Gantry's
ordinary full-object reads. Direct mode also checks returned metadata against the
expected digest and size.

### Catalog and operation units

- `--catalog-blobs=N` creates 1-512 independent raw objects. Each is exactly
  `--blob-bytes` bytes (default 64 MiB), without tar framing or jitter. One worker
  operation reads one blob. Very small objects can produce duplicate content;
  startup rejects duplicate catalogs rather than reporting a false distinct count.
- Without `--catalog-blobs`, existing OCI invocations remain compatible. Each
  catalog entry becomes a blob batch: ordered manifest and config, then concurrent
  layer blobs. This workload also works directly over UDS.
- `--catalog-images`, `--layers`, `--layer-bytes`, and `--jitter` retain their OCI
  meanings and cannot be explicitly combined with `--catalog-blobs`.
  `--blob-bytes` requires `--catalog-blobs`.
- `--concurrency` counts admitted operations, not individual requests.
  `--blob-concurrency` bounds parallel blobs within a batch (default 4).
  `--layer-concurrency` is a compatibility alias; specify only one. Independent
  single-blob operations always use one request.
- `--profile=shuffle` traverses the catalog in shuffled passes; `--profile=zipf`
  samples by stable catalog rank with `--zipf-exponent` controlling skew.
- Live `--concurrency-file` and node caps apply to the same admission loop for
  both backends. Reducing concurrency drains admitted operations under their
  original deadline. Zero pauses reads while keeping the origin available.

`--pull-timeout` covers a complete operation. `--interval` and `--retry-delay`
control per-worker pacing. `--duration` bounds the load phase. `--startup-timeout`
bounds catalog hashing and origin startup; UDS origin readiness also has a
10-second bound. `/readyz` means the catalog and origin are ready, not that the
dataplane has successfully acquired data. The UDS readiness probe talks directly
to the origin and does not warm measured objects in Racer.

## Memory and measurement

Direct mode uses the SDK's ordered reader with one page credit. Budget up to
**16 MiB of SDK page storage per active read**, plus protocol, socket, origin,
verification, and runtime allocations. Startup logs the configured page-storage
bound without silently reducing requested concurrency. Eight single-blob workers
can therefore require 128 MiB of page storage alone. Defaults of 64 workers can
require 1 GiB for raw blobs, or 4 GiB for OCI batches with four concurrent layers.
Live-control mode reports the maximum configurable worker count, not merely the
initial setting. This is a buffer bound, not an RSS measurement.

Metric names remain compatible:

- `racer_loadgen_pulls_total`, pull duration, and in-flight metrics describe
  operations: one raw blob or one complete OCI batch, according to workload.
- Request metrics describe individual blob acquisitions. The bounded `kind`
  label is `blob` for raw objects, or `manifest`, `config`, and `layer` for OCI.
- `racer_loadgen_verified_bytes_total` credits only completely successful,
  verified operations. Failed batches and `--verify=false` receive no credit.
- `racer_loadgen_received_bytes_total` counts bytes delivered to the consumer,
  including failed operations. It is **not wire traffic**: the SDK may discard
  incomplete or invalid pages before delivery.
- UDS origin metrics count callback operations and bytes generated for the SDK,
  not HTTP socket writes. They are not network throughput counters.

Existing image-oriented dashboards should not be interpreted as image-pull
measurements for raw-blob jobs. Keep backend/workload jobs distinguishable through
deployment labels. A warmed read does not guarantee permanent cache residency;
changing the seed changes content identities, whereas restarting loadgen with
the same seed does not make Racer's cache cold.

`--verify=true` is the default. [`--diagnose-integrity`](DIAGNOSTICS.md) uses the
same deterministic oracle on both backends. Tests cover the shared SDK and Gantry
adapters, including a noncaching SDK fake and temporary UDS readiness servers;
they do not establish real Racer cache-hit behavior or throughput.
