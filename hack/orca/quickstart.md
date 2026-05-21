<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Orca Dev Cluster Quickstart

End-to-end recipe to stand up a local Kind cluster with Orca pointed
at an in-cluster Azurite origin and a LocalStack S3 cachestore, then
seed data and exercise the cache with debug-level traces.

For the longer reference (every Make target, troubleshooting,
prerequisites, switching origin modes), see [dev-harness.md](./dev-harness.md).

## Prerequisites

- `kind`, `kubectl`, `podman` (or `docker`).
- `go` toolchain (used to build the orca image and run the
  `hack/cmd/orcadev` tool).

## Step 1 - One-time setup

Copy the example env file and edit it for Azurite-with-debug:

```bash
cp hack/orca/.env.example hack/orca/.env
$EDITOR hack/orca/.env
```

Set:

```
ORIGIN_DRIVER=azureblob
ORIGIN_ID=azureblob-azurite
AZURE_CONTAINER=orca-test
LOG_LEVEL=debug
```

Leave `AZURE_STORAGE_ACCOUNT`, `AZURE_STORAGE_KEY`, and
`AZUREBLOB_ENDPOINT` blank - the harness auto-selects
`devstoreaccount1` + the well-known Azurite dev key + the in-cluster
Azurite Service URL.

## Step 2 - Bring up the cluster

```bash
make orca-up
```

Single command. Builds the orca image, creates the Kind cluster,
loads the image, deploys LocalStack + Azurite + Orca, waits until
all three Orca replicas are Ready. Orca pods start with
`logging.level: debug` so the per-chunk trace is live from the very
first request.

Expected pods after bring-up:

```bash
make -C hack/orca status
# azurite-...                        1/1 Running
# localstack-...                     1/1 Running
# orca-azurite-container-init-...    0/1 Completed
# orca-buckets-init-...              0/1 Completed
# orca-...                           1/1 Running   (x3)
```

## Step 3 - Seed the origin

Both Azurite (NodePort 30100) and LocalStack S3 (NodePort 30200) are
exposed to the host via Kind's `extraPortMappings`, so no
`kubectl port-forward` is needed for the dev tool. Pick the driver
that matches your `.env` ORIGIN_DRIVER value.

```bash
# 5 x 10 MiB random blobs named synth1 ... synth5
make -C hack/orca data-generate ARGS='--size 10MiB --count 5'

# Single random blob (use this for benchmarks; named "orca-test1")
make -C hack/orca data-random NAME=orca-test SIZE=10MiB

# Or a single 100 MiB blob named big1
make -C hack/orca data-generate ARGS='--size 100MiB --count 1 --name big'

# Or upload a real file from disk
make -C hack/orca data-upload FILE=~/data.tar.gz

# Reproducible content (same --seed -> byte-identical blobs)
make -C hack/orca data-generate ARGS='--size 10MiB --count 3 --seed 42'

# Inspect / clean up
make -C hack/orca data-list
make -C hack/orca data-delete PREFIX=synth ARGS='--yes'
```

Per-blob ceiling: 1 GiB unless `--force`. Cumulative-bytes warning at
1 GiB. orcadev uses chunked uploads, so very large blobs do not
buffer in host memory.

## Step 4 - Port-forward the Orca edge

In a separate terminal:

```bash
make -C hack/orca port-forward
# Forwarding from 127.0.0.1:8443 -> 8443
```

Leave this running.

## Step 5 - Drive the cache

```bash
# First hit: cold fill. Triggers origin GetRange, cachestore PutChunk.
curl -v http://localhost:8443/orca-test/synth1 -o /dev/null

# Second hit: warm cache. catalog hit -> cachestore_get_chunk.
curl -v http://localhost:8443/orca-test/synth1 -o /dev/null
```

For the bigger blob, you can watch chunked streaming behaviour by
running the GET against `big1` (12 chunks at the default 8 MiB
chunk size) and tailing the logs in parallel.

## Step 6 - Watch the per-chunk debug trace

```bash
# Filter to one bucket
make -C hack/orca logs | jq 'select(.chunk.bucket=="orca-test")'

# Filter to one source file (e.g. just fetch coordinator decisions)
make -C hack/orca logs | jq 'select(.source.file | endswith("fetch.go"))'

# Or just the firehose
make -C hack/orca logs
```

On a cold fill you should see a sequence like:

```
edge_request                       (server.EdgeHandler)
head_object                        (fetch.Coordinator)
metadata_singleflight_leader       (metadata.Cache)
azureblob_head_request / _response (origin/azureblob)
metadata_record                    (metadata.Cache)
edge_get_plan                      (server.EdgeHandler)
get_chunk                          (fetch.Coordinator)
chunkcatalog_lookup_miss           (chunkcatalog.Catalog)
cachestore_stat_result present:false (cachestore/s3)
coordinator_selected               (cluster.Cluster)
fill_local_lead OR peer_fill_attempt (fetch.Coordinator)
origin_slot_acquired               (fetch.Coordinator.runFill)
origin_get_range_attempt           (fetch.fetchWithRetry)
azureblob_get_range_request / _response (origin/azureblob)
origin_body_received bytes=N       (fetch.runFill)
cachestore_put_chunk -> _success   (cachestore/s3)
commit_success                     (fetch.runFill)
chunkcatalog_record_insert         (chunkcatalog.Catalog)
edge_get_complete                  (server.EdgeHandler)
```

On a warm hit only `chunkcatalog_lookup_hit` and
`cachestore_get_chunk` fire - no origin call, no commit.

## Step 7 - Iterate

```bash
# After editing Go source:
make orca-reset
# Rebuilds image, side-loads into Kind, rolling-restarts. ~30-60s.

# After editing a manifest template or .env:
make -C hack/orca deploy        # re-render + apply (idempotent)
make -C hack/orca reset         # bounce to pick up new ConfigMap

# Clear the cachestore between experiments (forces every chunk back
# to the cold-fill path on next GET):
kubectl --context kind-orca-dev -n unbounded-kube exec deploy/localstack -- \
  awslocal s3 rm s3://orca-cache --recursive

# Clear the origin between experiments:
make -C hack/orca data-delete ARGS='--yes'
```

## Step 8 - Roundtrip, benchmarks, and scenarios

The previous steps stand up the cluster and let you drive it with
`curl` by hand. This step uses the `orcadev` tool to do the same
work in one command: SHA-256-verified roundtrips, parallel
throughput benchmarks, and canned end-to-end scenarios.

> The edge port-forward to `svc/orca:8443` is auto-managed: orcadev
> probes `localhost:8443` and spawns a short-lived
> `kubectl port-forward` if nothing is bound. If you already have
> `make -C hack/orca port-forward` running from Step 4 (or any
> other binder on that port), orcadev detects it and reuses it. To
> disable the auto-forward, pass `ARGS='--auto-port-forward=false'`.

### 8a - Roundtrip (correctness check)

```bash
dd if=/dev/urandom of=/tmp/orca-test.bin bs=1M count=10 status=none
make -C hack/orca roundtrip FILE=/tmp/orca-test.bin
```

orcadev uploads the file, fetches it back via orca, and compares a
streaming SHA-256 of the source bytes against a streaming SHA-256 of
the response. Exit code 0 on PASS, 1 on mismatch (suitable for CI).

Useful flags via `ARGS=`:

- `--repeat 3` to issue three sequential GETs (first cold, rest
  warm).
- `--cleanup` to delete the uploaded blob after the run.
- `--dump-diff` to print a side-by-side hex dump of the first
  differing bytes when the checksums disagree.

A failure with `--dump-diff` looks like:

```text
MISMATCH
  source sha256:   3a7bd9...e21f
  received sha256: 9a1c0c...4a8e
  first difference at offset 1024 (0x400)

  offset 0x0 (0):
             SOURCE                                          | RECEIVED
    00000400  aa bb cc dd ee ff 00 11 22 33 44 55 66 77 88 99  ........"3DUfw.. | aa bb cc dd ee ff 00 11 22 33 44 55 66 77 88 00  ........"3DUfw..
    ...
```

### 8b - Benchmarks

Seed an object to bench against (or reuse the roundtrip blob), then
run the benchmark:

```bash
make -C hack/orca data-upload FILE=/tmp/orca-test.bin
make -C hack/orca bench KEY=orca-test.bin \
  ARGS='--duration 30s --concurrency 16 --range-size 1MiB --read-pattern random'
```

orcadev prints a human summary on stdout (requests, throughput,
min/p50/p90/p99/max latency). Use `--output json` to switch the
stdout payload to JSON, or `--json-out PATH` to keep human text on
stdout and persist JSON to a file for comparison across runs:

```bash
# Capture a baseline
make -C hack/orca bench KEY=orca-test.bin \
  ARGS='--duration 30s --concurrency 16 --json-out /tmp/run-a.json --label baseline'

# Iterate on code (make orca-reset), then re-bench
make -C hack/orca bench KEY=orca-test.bin \
  ARGS='--duration 30s --concurrency 16 --json-out /tmp/run-b.json --label after-fix'

# Compare with jq
jq -r '"\(.label)\tMiB/s=\(.results.throughput_bytes_per_second/1048576|floor)\tp99ms=\(.results.latency_ns.p99/1000000)"' \
  /tmp/run-*.json
```

The JSON envelope is versioned (`schema_version: 1`) and includes a
log-spaced latency histogram in `latency_histogram` (configurable
bounds and bucket count). See `go run ./hack/cmd/orcadev bench --help`
for tuning knobs.

### 8c - Scenarios

Canned end-to-end scenarios are one-line invocations that string
together upload + fetch + verify against a specific behaviour:

```bash
make -C hack/orca scenario NAME=cold-warm       # cold-vs-warm GET ratio
make -C hack/orca scenario NAME=range-stress    # concurrent ranges, all bytes verified
make -C hack/orca scenario NAME=empty-object    # zero-byte regression check
make -C hack/orca scenario NAME=etag-change     # mid-stream etag rotation
```

Each prints PASS or FAIL with per-step timings. `ARGS='--json-out
PATH'` writes a machine-parseable result for CI; `ARGS='--keep-data'`
skips end-of-run cleanup so you can inspect the post-mortem state.

### 8d - Cache inspection while iterating

After a roundtrip or bench you can see what landed in the cache, and
force a cold state before the next experiment:

```bash
make -C hack/orca cache-list
make -C hack/orca cache-inspect BUCKET=orca-origin KEY=orca-test.bin

# Force a cold-cache state for orca-test.bin before the next bench
make -C hack/orca cache-clear ARGS='--object orca-origin/orca-test.bin --yes'
```

`cache-inspect` answers the "did my fix actually populate the
cache?" question: it HEADs the origin for size + etag, computes the
canonical chunk paths via `internal/orca/chunk`, then HEADs each
path in the cachestore.

## Step 9 - Tear down

```bash
make orca-down
```

Deletes the Kind cluster (and everything in it).

## Cheat-sheet of common helpers

| Verb | Effect |
|---|---|
| `make orca-up` | Full bring-up (idempotent). |
| `make orca-reset` | Rebuild image + kind-load + rolling-restart Orca. |
| `make orca-down` | Delete the Kind cluster. |
| `make -C hack/orca status` | `kubectl get pods -o wide` in the namespace. |
| `make -C hack/orca logs` | Tail all Orca pods. |
| `make -C hack/orca port-forward` | localhost:8443 -> edge service. |
| `make -C hack/orca data-generate ARGS='...'` | Synthetic content. |
| `make -C hack/orca data-random NAME=foo [SIZE=10MiB]` | One random blob `foo1` of SIZE. |
| `make -C hack/orca data-upload FILE=...` | Upload a real file. |
| `make -C hack/orca data-list` | What's in the origin. |
| `make -C hack/orca data-delete [PREFIX=...]` | Remove origin objects. |
| `make -C hack/orca roundtrip FILE=...` | Upload + fetch via orca + verify checksum. |
| `make -C hack/orca cache-list` | Enumerate cachestore chunks. |
| `make -C hack/orca cache-inspect BUCKET=b KEY=k` | Per-chunk presence for one object. |
| `make -C hack/orca bench KEY=...` | Parallel GET throughput benchmark. |
| `make -C hack/orca scenario NAME=...` | Canned end-to-end scenario. |

## Alternative: integration tests (no Kind cluster)

If you don't need to inspect the K8s deployment shape, the Go-level
integration suite under `internal/orca/inttest/` covers chunked
fetch + dedup + peer fallback against testcontainers-managed
LocalStack + Azurite. Much faster, no Kind setup:

```bash
make orca-inttest    # ~15-20s, requires Docker
```

## Reference: every config knob

For an annotated, top-to-bottom reference of every field Orca's
config YAML accepts (defaults, valid ranges, environment-variable
fallbacks, production vs dev nuances), see
[`config.example.yaml`](./config.example.yaml). The file is
maintained by hand alongside the schema in
`internal/orca/config/config.go` and is exercised by a config-package
test that re-loads it on every CI run, so it cannot silently drift
out of sync with the parser. Copy it as a starting point for a
custom Orca config (`orca -config /path/to/yours.yaml`).
