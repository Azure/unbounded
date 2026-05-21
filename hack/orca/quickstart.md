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
# 5 x 10 MiB random blobs named synth-0 ... synth-4
make -C hack/orca data-generate ARGS='--size 10MiB --count 5'

# Or a single 100 MiB blob named big-0
make -C hack/orca data-generate ARGS='--size 100MiB --count 1 --prefix big-'

# Or upload a real file from disk
make -C hack/orca data-upload FILE=~/data.tar.gz

# Reproducible content (same --seed -> byte-identical blobs)
make -C hack/orca data-generate ARGS='--size 10MiB --count 3 --seed 42'

# Inspect / clean up
make -C hack/orca data-list
make -C hack/orca data-delete PREFIX=synth- ARGS='--yes'
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
curl -v http://localhost:8443/orca-test/synth-0 -o /dev/null

# Second hit: warm cache. catalog hit -> cachestore_get_chunk.
curl -v http://localhost:8443/orca-test/synth-0 -o /dev/null
```

For the bigger blob, you can watch chunked streaming behaviour by
running the GET against `big-0` (12 chunks at the default 8 MiB
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

## Step 8 - Tear down

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
