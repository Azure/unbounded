# racer-loadgen

Generate deterministic blobs and repeatedly consume them through Gantry's
registry API, Racer's client Unix domain socket (UDS), or an S3 HTTP
endpoint. All backends use the
same catalog selection, worker admission, pacing, deadlines, integrity checking,
diagnostics, and metrics. Payloads are generated on demand, not retained in a
load-generator content cache.

## Workloads and backends

### S3 HTTP benchmark and synthetic origin

S3 mode performs an unsigned, path-style **HTTP GET for every operation**, not
an SDK shortcut. Point the client directly at the synthetic origin for a baseline,
or use a compatible object adapter. The `internal/racerobject` library provides
an SDK origin and HTTP sidecar handler; this extraction does not include a
`racer-object` executable.

Origin-only process (no Garage provisioning or uploads required):

```sh
racer-loadgen --backend=s3 --s3-origin --listen=:8080 --concurrency=0 \
  --bucket=benchmark --object-count=128 --object-bytes=67108864 \
  --seed=benchmark-v1 --startup-timeout=4m --metrics-listen=:9090
```

Client process with an origin or compatible adapter on localhost:

```sh
racer-loadgen --backend=s3 --endpoint=http://127.0.0.1:8080 \
  --bucket=benchmark --object-count=128 --object-bytes=67108864 \
  --seed=benchmark-v1 --concurrency=8 --profile=shuffle \
  --pull-timeout=2m --startup-timeout=4m --metrics-listen=:9090
```

S3 mode defaults to bucket `benchmark`, 128 objects of 64 MiB, and endpoint
`http://127.0.0.1:8080`. Objects have keys `object-000000` through
`object-000127` for this example. Count is bounded to 1-512; size must be positive.
Bytes use the existing deterministic raw-blob generator, without OCI metadata or
tar framing. Each process hashes its whole catalog at startup using bounded
scratch space, not an in-memory object cache. The example hashes 8 GiB on each
pod before readiness; allow CPU and a sufficient startup probe budget.
Very small payloads may collide; duplicate-content catalogs are rejected.

Keep **seed, count, size, and bucket identical** on origin and client pods.
Opt-in integrity verification compares downloaded SHA-256 and size against the local
deterministic catalog, independently of the upstream's ETag format.
The default `--verify=false` skips hashing during reads, but still checks body size and
does not skip startup catalog hashing. An external S3 store can be used only if
these exact keys and payloads are populated separately and unsigned reads are
allowed; loadgen neither uploads objects nor signs requests.

Without `--s3-origin`, S3 loadgen starts only its metrics/probe listener,
leaving port 8080 free for the endpoint. For a remote origin, change `--endpoint`
to its URL. Compare runs with identical concurrency, catalog, profile,
verification, and resource limits. Report network topology differences rather
than attributing all differences to caching.

The synthetic origin implements object HEAD/GET, one closed/open/suffix byte
range, strong quoted ETags, If-Match/If-None-Match (including wildcard conditions),
and S3 XML errors. Racer-object's pinned HEAD and ranged GET requests are tested
with the real AWS HTTP client. ETags are quoted `sha256:<hex>`, not MD5.
This is an **unauthenticated, read-only test fixture**, not a general S3 server:
no listing, writes, versioning, date conditions, If-Range, or multipart reads.
Keep it on a trusted benchmark network; it accepts signed requests without
validating signatures. `x-id=GetObject` and `x-id=HeadObject` are supported SDK
query plumbing; version reads fail rather than silently returning current data.

Existing `--profile=shuffle|zipf`, `--zipf-exponent`, `--concurrency-file`, node
caps, `--interval`, `--retry-delay`, `--duration`, and `--diagnose-integrity` work
unchanged. One operation is one full object GET; per-blob concurrency is fixed to
one. S3 mode rejects OCI sizing/transport flags instead of silently ignoring them;
use `--endpoint`, not `--target`, and configure the upstream namespace on
racer-object, not loadgen.

Metrics retain their existing names and the `kind="blob"` label. Successful
operations appear in `racer_loadgen_pulls_total{result="success"}`; verified goodput
is `rate(racer_loadgen_verified_bytes_total[1m])`, in bytes/s, and failures are
classified in `racer_loadgen_pull_failures_total`. Use request/pull duration
histograms for latency and `racer_loadgen_origin_bytes_total` on origin pods for
generated body bytes. No per-key metric labels are added. `/readyz` indicates
catalog readiness, not sidecar/dataplane health, and performs no warmup GET.
The process can exit normally after `--duration` even if reads failed: evaluate
the success, failure, and byte counters, not just its exit code. A finite run
stops the metrics listener at completion, so scrape during the run.

Verification consumes CPU, and synthetic origins spend CPU regenerating data;
neither result represents persistent-storage performance. Restarting with the
same seed does not make the cache cold. Unlike the digest-keyed UDS mode, S3 names
stay stable across seed changes: use a new racer-object namespace for an isolated
cold run to avoid metadata-TTL staleness. The local adapter integration test uses
a noncaching SDK test daemon and does not establish real cache-hit behavior,
cluster compatibility, or throughput. Size the sidecar and dataplane separately;
the loadgen's streaming buffer does not account for their page storage.

### Gantry and direct SDK modes

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
racer-loadgen --backend=uds --volume=racer-loadgen \
  --catalog-blobs=128 --blob-bytes=67108864 \
  --concurrency=8 --seed=benchmark-v1
```

Direct mode uses `pkg/racersdk` against
`/run/racer/racer-loadgen/client/socket` and serves the synthetic origin at
`/run/racer/racer-loadgen/origin/socket`. It starts no HTTP registry and ignores
`--target`, `--namespace`, and `--listen`. Metrics and probes still use
`--metrics-listen` (default `:9090`). An operational Racer dataplane and cache
configuration are prerequisites; loadgen does not start the dataplane.

`--volume` is retained for command-line compatibility. Its value is the cache
name passed to the SDK's `ClientConfig.Cache` and `OriginConfig.Cache` fields.
It does not select a separate volume API. There is no `--cache` flag.

Unit tests use `pkg/racersdk/racersdktest.NewClient` with a noncaching local
daemon and real SDK origin validation over temporary Unix sockets. The helper
registers cleanup with the test. These tests are not evidence of real Racer
compatibility or performance.

Use a dedicated cache, not a cache whose origin socket Gantry already owns. The
origin directory must be owned by loadgen's UID and must not be group/world
writable. Existing directory permissions are not changed; missing directories
are created if permitted. The SDK provides exclusive ownership, stale-socket
recovery, and shutdown cleanup.

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

By default, reads drain with `io.Copy` to an open `/dev/null` file reused for the
puller's lifetime. Direct UDS reads preserve the SDK object's `WriteTo` method,
allowing the zero-copy splice path rather than copying whole objects into Go
memory. `--verify=true` uses a 32 KiB read buffer to hash each object, so bytes
are copied into Go memory. Protocol, socket, origin, and runtime allocations
still contribute to RSS; there is no fixed page-storage budget per read.

Metric names remain compatible:

- `racer_loadgen_pulls_total`, pull duration, and in-flight metrics describe
  operations: one raw blob or one complete OCI batch, according to workload.
- Request metrics describe individual blob acquisitions. The bounded `kind`
  label is `blob` for raw objects, or `manifest`, `config`, and `layer` for OCI.
- `racer_loadgen_verified_bytes_total` credits only completely successful,
  verified operations. Failed batches and the default `--verify=false` receive
  no credit; enable `--verify` when measuring verified goodput.
- `racer_loadgen_received_bytes_total` counts bytes delivered to the consumer,
  including failed operations. Drain bytes are counted when `io.Copy` returns,
  including on failure. This is **not wire traffic**; SDK framing is excluded.
- UDS origin metrics count callback operations and bytes generated for the SDK,
  not HTTP socket writes. They are not network throughput counters.

Existing image-oriented dashboards should not be interpreted as image-pull
measurements for raw-blob jobs. Keep backend/workload jobs distinguishable through
deployment labels. A warmed read does not guarantee permanent cache residency;
changing the seed changes content identities, whereas restarting loadgen with
the same seed does not make Racer's cache cold.

`--verify=false` is the default. Use `--verify` to hash on the copy path;
[`--diagnose-integrity`](DIAGNOSTICS.md) also requires `--verify` and uses the
same deterministic oracle on both backends. Tests cover the shared SDK and Gantry
adapters, including a noncaching SDK fake and temporary UDS readiness servers;
they do not establish real Racer cache-hit behavior or throughput.
