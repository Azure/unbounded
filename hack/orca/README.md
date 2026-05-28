<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Orca Dev Quickstart

This is the single coherent entrypoint for installing Orca into a
Kubernetes cluster and driving it for development. The default install
runs Orca with an in-cluster Azurite Azure-Blob origin and an
in-cluster LocalStack S3 cachestore - zero real cloud credentials
required.

Orca is a read-only, S3-compatible HTTP origin cache that fronts cloud
blob storage from within an on-prem datacenter. For the design and
rationale, see [`designs/orca/brief.md`](../../designs/orca/brief.md)
and [`designs/orca/design.md`](../../designs/orca/design.md).

## Prerequisites

- `kubectl` and `go`.
- For kind clusters: `kind` and `podman` (or `docker`).
- For non-kind clusters (AKS, EKS, k3d, ...): a kubectl context that
  can reach the cluster, plus a container registry the cluster can
  pull from.
- Optional: `jq` for parsing the JSON bench output.

## 1. Build

```bash
make orca-build orcadev
```

Produces `bin/orca` (the daemon) and `bin/orcadev` (the dev tool you
will use to seed data and drive scenarios). The rest of this
quickstart uses `bin/orcadev`.

## 2. Install Orca into a cluster

Pick the path that matches your situation.

### Path A: I don't have a cluster

The repo ships a kind cluster spec tuned for Orca (1 control plane +
3 workers, matching Orca's default replica count and required
pod-anti-affinity).

```bash
make orca-kind-up
# == ./hack/orca/kind-up.sh && ./hack/orca/setup-orca.sh --build --kind-load
```

This creates the `orca-dev` kind cluster, builds the orca container
image locally, side-loads it into the kind nodes, deploys Azurite +
LocalStack, creates the orca-credentials Secret, and applies the orca
manifests. Takes ~2 minutes on a warm host.

### Path B: I have a cluster

Push the orca image to a registry the cluster can pull from, then
install:

```bash
make image-orca-local ORCA_IMAGE=my-registry.io/orca:dev
$CONTAINER_ENGINE push my-registry.io/orca:dev

./hack/orca/setup-orca.sh \
    --context my-cluster \
    --image my-registry.io/orca:dev
```

The setup script is cluster-agnostic. It applies vanilla
`kubectl apply -f` against the chosen context and waits for the
emulator init hooks plus the orca rollout to settle.

On clusters with fewer than 3 schedulable nodes, the default install
relaxes Orca's pod anti-affinity from `required` to `preferred` so
the rollout still completes. Kind installs keep the strict
`required` anti-affinity (mirrors production topology). Pass
`--replicas N` to override the default of 3.

## 3. Verify the install

```bash
kubectl --context kind-orca-dev -n unbounded-kube get pods
# NAME                          READY   STATUS    RESTARTS   AGE
# azurite-...                   2/2     Running   0          1m   (sidecar = container-ensurer)
# localstack-...                1/1     Running   0          1m
# orca-...                      1/1     Running   0          50s   (x3)
```

## 4. Seed some data

`bin/orcadev` auto-opens port-forwards to `svc/orca`, `svc/azurite`,
and `svc/localstack` as needed, so no manual `kubectl port-forward`
is required. The same `orcadev` invocation works on kind, AKS, EKS,
k3d, anything reachable via kubectl.

```bash
# 5 x 10 MiB random blobs named synth1 ... synth5
bin/orcadev upload --generate --count 5 --size 10MiB

# Single 100 MiB random blob named big1
bin/orcadev upload --generate --count 1 --size 100MiB --name big

# A real file from disk
bin/orcadev upload --file ~/data.tar.gz

# Reproducible content (same --seed -> byte-identical blobs)
bin/orcadev upload --generate --count 3 --size 10MiB --seed 42

# What's in the origin?
bin/orcadev list

# Clean up
bin/orcadev delete --prefix synth --yes
```

Per-blob ceiling: 1 GiB unless `--force`. orcadev streams uploads in
chunks so very large blobs do not buffer in host memory.

## 5. Run a roundtrip (correctness check)

```bash
dd if=/dev/urandom of=/tmp/orca-test.bin bs=1M count=10 status=none
bin/orcadev roundtrip --file /tmp/orca-test.bin
```

orcadev uploads the file, fetches it back via orca, and compares a
streaming SHA-256 of the source against a streaming SHA-256 of the
response. Exit code 0 on PASS, 1 on mismatch (suitable for CI).

Useful flags:

- `--repeat 3`: issue three sequential GETs (first cold, rest warm).
- `--cleanup`: delete the uploaded blob after the run.
- `--dump-diff`: print a side-by-side hex dump of the first differing
  bytes when the checksums disagree.

## 6. Run a canned scenario

Scenarios are one-line invocations that string together upload +
fetch + verify against a specific behaviour:

```bash
bin/orcadev scenario cold-warm        # cold-vs-warm GET ratio
bin/orcadev scenario range-stress     # concurrent ranges, all bytes verified
bin/orcadev scenario empty-object     # zero-byte regression check
bin/orcadev scenario etag-change      # mid-stream etag rotation
```

Each prints PASS or FAIL with per-step timings. `--json-out PATH`
writes a machine-parseable result for CI; `--keep-data` skips
end-of-run cleanup so you can inspect the post-mortem state.

## 7. Run a benchmark

Seed an object to bench against, then drive parallel GETs:

```bash
bin/orcadev upload --file /tmp/orca-test.bin
bin/orcadev bench \
    --key orca-test.bin \
    --duration 30s \
    --concurrency 16 \
    --range-size 1MiB \
    --read-pattern random
```

Prints a human summary on stdout (requests, throughput,
min/p50/p90/p99/max latency). Use `--output json` to switch the
stdout payload to JSON, or `--json-out PATH` to keep human text on
stdout and persist JSON for cross-run comparison:

```bash
# Baseline
bin/orcadev bench --key orca-test.bin \
    --duration 30s --concurrency 16 \
    --json-out /tmp/run-a.json --label baseline

# Iterate on code (make orca-reset), then re-bench
bin/orcadev bench --key orca-test.bin \
    --duration 30s --concurrency 16 \
    --json-out /tmp/run-b.json --label after-fix

# Compare with jq
jq -r '"\(.label)\tMiB/s=\(.results.throughput_bytes_per_second/1048576|floor)\tp99ms=\(.results.latency_ns.p99/1000000)"' \
    /tmp/run-*.json
```

The JSON envelope is versioned (`schema_version: 1`) and includes a
log-spaced latency histogram. See `bin/orcadev bench --help` for
tuning knobs.

## 8. Inspect the cachestore while iterating

After a roundtrip or bench you can see what landed in the cache and
force a cold state before the next experiment:

```bash
bin/orcadev cache list
bin/orcadev cache inspect --bucket orca-test --key orca-test.bin

# Force a cold-cache state for orca-test.bin before the next bench
bin/orcadev cache clear --object orca-test/orca-test.bin --yes
```

`cache inspect` answers the "did my fix actually populate the cache?"
question: it HEADs the origin for size + etag, computes the canonical
chunk paths, then HEADs each path in the cachestore.

In `awss3` mode the origin bucket is `orca-origin`; substitute that
for `orca-test` in the examples above.

## 9. Iterate / reset / tear down

```bash
# After editing Go source on kind:
make orca-reset
# Rebuilds the image, side-loads into kind, rolling-restarts. ~30-60s.

# Tail logs from all orca replicas:
make -C hack/orca logs

# Tear down the kind cluster:
make orca-kind-down       # or: make orca-down

# Or uninstall just Orca + emulators from a non-kind cluster, leaving
# the namespace and any unrelated resources in it intact:
./hack/orca/setup-orca.sh --context my-cluster --uninstall

# Also remove the namespace (DESTRUCTIVE - removes every resource in
# the namespace, including ones this script did not create):
./hack/orca/setup-orca.sh --context my-cluster --uninstall --delete-namespace
```

## 10. Watch the per-chunk debug trace

For deep cache-behavior inspection, install with debug logging:

```bash
./hack/orca/setup-orca.sh --context kind-orca-dev --log-level debug
make -C hack/orca logs | jq 'select(.chunk.bucket=="orca-test")'
```

On a cold fill the per-chunk trace shows the full pipeline
(`metadata_singleflight_leader`, `coordinator_selected`,
`origin_get_range_attempt`, `cachestore_put_chunk`, `commit_success`,
`chunkcatalog_record_insert`). On a warm hit only
`chunkcatalog_lookup_hit` and `cachestore_get_chunk` fire - no origin
call, no commit.

## Cheat-sheet

| Verb | Effect |
|---|---|
| `make orca-build orcadev` | Build the orca daemon + orcadev tool. |
| `make orca-kind-up` | Create kind cluster + install Orca (`= orca-up`). |
| `make orca-kind-down` | Delete the kind cluster (`= orca-down`). |
| `make orca-install` | Install Orca into the current kubectl context. |
| `make orca-reset` | Rebuild image + side-load + rollout-restart on kind. |
| `make -C hack/orca status` | `kubectl get pods` in the orca namespace. |
| `make -C hack/orca logs` | Tail logs from all orca replicas. |
| `make -C hack/orca port-forward` | Foreground forward localhost:8443 -> svc/orca. |
| `bin/orcadev upload --generate --count N --size S` | Synthesise + upload N random blobs. |
| `bin/orcadev upload --file PATH` | Upload one real file. |
| `bin/orcadev list` | Enumerate origin objects. |
| `bin/orcadev delete [--prefix P] --yes` | Bulk delete origin objects. |
| `bin/orcadev roundtrip --file PATH` | Upload + GET via orca + verify SHA-256. |
| `bin/orcadev cache list` | Enumerate cachestore chunks. |
| `bin/orcadev cache inspect --bucket B --key K` | Per-chunk presence for one object. |
| `bin/orcadev cache clear ...` | Delete chunks. |
| `bin/orcadev bench --key K [tuning flags]` | Parallel-GET throughput / latency. |
| `bin/orcadev scenario NAME` | Canned end-to-end scenario. |
| `./hack/orca/setup-orca.sh --uninstall` | Remove Orca + emulators from a cluster. |

## Troubleshooting

### `setup-orca.sh` exits with "LocalStack init-hook did not create orca-cache"

LocalStack startup takes longer than the 60-second budget. Check its
logs and re-run setup-orca.sh (it is idempotent):

```bash
kubectl --context kind-orca-dev -n unbounded-kube logs deploy/localstack | tail
./hack/orca/setup-orca.sh
```

If you see "License activation failed" with exit code 55, you are on
LocalStack's Pro-only `latest` tag. The dev install pins
`localstack/localstack:3.8` specifically to avoid this; if you have
overridden the image, switch to a community-tier tag.

### Orca pods CrashLoopBackOff with "NoSuchBucket: orca-cache"

The LocalStack pod restarted (OOM, eviction, ...) and its in-memory
state was wiped. The init-hook ConfigMap re-creates the buckets on
every LocalStack start, so the clean recovery is to re-run
setup-orca.sh which also waits until the buckets exist:

```bash
./hack/orca/setup-orca.sh
```

### Orca pods CrashLoopBackOff with "config invalid: ..."

Most common in real-Azure mode when one of `AZURE_STORAGE_ACCOUNT`,
`AZURE_STORAGE_KEY`, `AZURE_CONTAINER` is empty.

```bash
kubectl --context kind-orca-dev -n unbounded-kube logs deploy/orca | head
# Fix the env vars, then:
./hack/orca/setup-orca.sh
```

### `bin/orcadev` reports "OriginUnreachable" or 502 from manual GETs

The blob doesn't exist in the origin. Seed it:

```bash
bin/orcadev upload --generate --count 1 --size 10MiB --name orca-test
```

### `kind load` fails with "tag not found"

The kind-load path tags the image as `ghcr.io/azure/orca:dev`. If
you overrode `VERSION` and got a slash in the tag (git describe can
produce e.g. `images/agent-ubuntu2404-nvidia/v...-dirty`), the OCI
tag is invalid. Stick with the default; the dev tag is intentionally
slash-free.

### orcadev says "auto port-forward: ..." even though kubectl is running locally

orcadev probes the configured localhost ports first; if anything is
bound (your own `make port-forward`, a sibling orcadev, or a stale
foreground forward), it reuses that. The "auto port-forward: ..."
message means the probe failed and orcadev opened its own forward
for the duration of the run. Both are fine.

## Advanced

### Real Azure

Set the standard Azure env vars before invoking setup-orca.sh:

```bash
export AZURE_STORAGE_ACCOUNT=myaccount
export AZURE_STORAGE_KEY=...
export AZURE_CONTAINER=my-container
./hack/orca/setup-orca.sh
```

The endpoint is computed as `https://<account>.blob.core.windows.net/`
and authentication uses `AZURE_STORAGE_KEY`. The in-cluster Azurite +
LocalStack are still deployed (they are inexpensive) but Orca ignores
them and talks to real Azure for the origin. The cachestore stays on
the in-cluster LocalStack.

To upload to real Azure with orcadev, pass the matching overrides:

```bash
bin/orcadev upload \
    --origin-driver azureblob \
    --origin-account "$AZURE_STORAGE_ACCOUNT" \
    --origin-account-key "$AZURE_STORAGE_KEY" \
    --origin-bucket "$AZURE_CONTAINER" \
    --origin-endpoint "https://${AZURE_STORAGE_ACCOUNT}.blob.core.windows.net/" \
    --file ./my-file
```

### awss3 origin mode (LocalStack as origin)

```bash
./hack/orca/setup-orca.sh --origin awss3
bin/orcadev --origin-driver awss3 upload --generate --count 5 --size 10MiB
```

Both the cachestore and origin point at the same in-cluster LocalStack
on different buckets (`orca-cache` and `orca-origin`).

### Custom Orca config

`./hack/orca/setup-orca.sh` only exposes the handful of knobs
developers actually need. For a custom Orca config (production-shape
auth, mTLS, custom chunk sizes, multi-origin, ...) see the
fully-annotated reference at
[`config.example.yaml`](./config.example.yaml). The file is
maintained by hand alongside the schema in
`internal/orca/config/config.go` and is exercised by a config-package
test on every CI run, so it cannot silently drift.

### Go-only integration tests (no Kubernetes cluster)

For Go-level behavior coverage (chunked fetch, dedup, peer fallback)
the integration suite under `internal/orca/inttest/` runs against
testcontainers-managed LocalStack + Azurite and finishes in ~30s with
no Kubernetes setup at all:

```bash
make orca-inttest    # requires Docker
```

See [`inttest.md`](./inttest.md) for the harness layout, the
production-code seams it depends on, and how to add a scenario.
