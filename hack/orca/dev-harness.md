<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Orca Dev Harness

A local end-to-end harness for the Orca origin cache. Stands up a Kind
cluster with three Orca replicas, an in-cluster LocalStack as the
cachestore, and an in-cluster origin (LocalStack S3 by default; Azurite
when `ORIGIN_DRIVER=azureblob`). Both default paths run with zero real
cloud credentials. The harness can also be flipped to point at a real
Azure Blob storage account.

This document covers a single workstation. For the production
architecture and design rationale, see `design/orca/`. For Go-level
integration tests that exercise the same code paths without Kubernetes
(via testcontainers-managed LocalStack and Azurite), see
[inttest.md](./inttest.md). The two harnesses are complementary: this
one validates the K8s deployment shape (manifests, headless DNS, image
build/load); the integration tests cover the Go runtime behavior.

## Origin modes

| `ORIGIN_DRIVER` value | Origin backend | Driver path exercised | Creds needed |
| --------------------- | -------------- | --------------------- | ------------ |
| `awss3` (default)     | LocalStack S3 (in-cluster) | `internal/orca/origin/awss3` | None |
| `azureblob` (Azurite) | Azurite (in-cluster) | `internal/orca/origin/azureblob` | None (well-known dev key) |
| `azureblob` (real Azure) | Azure Blob Storage | `internal/orca/origin/azureblob` | Account + key in `.env` |

The cachestore is always in-cluster LocalStack S3 (different bucket
from the awss3 origin).

## What you get

- A Kind cluster named `orca-dev` with one control plane and three
  worker nodes (one per Orca replica via required pod-anti-affinity).
- LocalStack 3.8 running in the cluster as the S3-compatible
  cachestore (and origin in `awss3` mode). Community tier (`latest`
  is Pro-only and exits with code 55 "License activation failed").
- Azurite (Microsoft's official Azure Storage emulator) deployed on
  demand when `ORIGIN_DRIVER=azureblob`. Runs from
  `mcr.microsoft.com/azure-storage/azurite`.
- Buckets/containers pre-created by init Jobs:
  - `orca-cache` (S3) - cachestore (versioning unset; Orca's
    versioningGate rejects Enabled and Suspended).
  - `orca-origin` (S3) - origin (used when `ORIGIN_DRIVER=awss3`).
  - `orca-test` (Azure container) - origin (used when `ORIGIN_DRIVER=azureblob`).
- Three Orca replicas. mTLS between peers and bearer auth for
  clients are both disabled in dev (`cluster.internal_tls.enabled=false`,
  `server.auth.enabled=false`).
- Helper scripts (seed sample blobs, GET, LIST, clear cache, tail logs).

## Prerequisites

- `kind` (https://kind.sigs.k8s.io/), `kubectl`, `podman` (or `docker`).
- `go` toolchain (for `go run ./hack/cmd/render-manifests`).
- Optional (Azure mode only): a real Azure Storage account + container
  + account key.

No real cloud credentials are required for the default flow.

## One-time setup

```bash
cp hack/orca/.env.example hack/orca/.env
# Default values work; only edit if you want Azure mode.
```

`.env` is git-ignored. The default `ORIGIN_DRIVER=awss3` runs entirely
on the in-cluster LocalStack.

The `.env` file drives the dev-harness manifest renderer. For an
annotated reference of every field Orca's runtime config YAML
accepts (defaults, valid ranges, env-var fallbacks, prod-vs-dev
nuances), see [`config.example.yaml`](./config.example.yaml).

## Bring it up

```bash
make -C hack/orca up
```

This runs, in order:

1. `kind-create` - create the `orca-dev` cluster (idempotent).
2. `image` - build `ghcr.io/azure/orca:dev` via `make image-orca-local`.
3. `kind-load` - save the image to a tar and `kind load image-archive`.
4. `render` - render `deploy/orca/*.yaml.tmpl` with values from `.env`.
5. `render-dev` - render `deploy/orca/dev/*.yaml.tmpl` (LocalStack, Azurite, init Jobs).
6. `deploy-localstack` - apply the namespace, LocalStack, wait until
   ready, run the bucket-init Job (creates `orca-cache` + `orca-origin`),
   wait for completion.
7. `deploy-azurite-maybe` - if `ORIGIN_DRIVER=azureblob`, deploy
   Azurite + run its container-init Job. Skipped for `awss3`.
8. `deploy-credentials` - create the `orca-credentials` Secret.
9. `deploy-orca` - apply RBAC, ConfigMap, Services, Deployment.
10. `wait-ready` - block until all 3 replicas are Ready.

When this finishes you should see something like:

```
$ make -C hack/orca status
NAME                                READY   STATUS    RESTARTS   AGE
azurite-...                         1/1     Running   0          1m   (only in azureblob mode)
localstack-...                      1/1     Running   0          1m
orca-azurite-container-init-...     0/1     Completed 0          1m   (only in azureblob mode)
orca-buckets-init-...               0/1     Completed 0          1m
orca-7c5d4f9b8c-...                 1/1     Running   0          50s
orca-7c5d4f9b8c-...                 1/1     Running   0          50s
orca-7c5d4f9b8c-...                 1/1     Running   0          50s
```

## Switching origins

Edit `hack/orca/.env`, change `ORIGIN_DRIVER`, then:

```bash
make -C hack/orca down
make -C hack/orca up
```

Or, to keep the cluster but reconfigure Orca and pull in any newly
needed backends:

```bash
$EDITOR hack/orca/.env
make -C hack/orca deploy        # idempotent; brings up Azurite if needed
make -C hack/orca reset         # rolling-restart Orca with new ConfigMap
```

## Seed sample data

The dev harness ships a multi-purpose Go tool, `hack/cmd/orcadev`,
that populates the origin (Azurite or LocalStack S3 or real Azure)
with synthetic or operator-supplied content, drives roundtrip
correctness checks through orca, inspects the cachestore, and runs
throughput benchmarks + canned scenarios. For the canonical recipe
(NodePort 30100 for Azurite, NodePort 30200 for LocalStack S3, the
seed subcommands wrapped as Make targets) see
[quickstart.md - Step 3](./quickstart.md#step-3---seed-the-origin).

For real Azure storage, the `dev-azure` Make target invokes
`orcadev upload` against your account using credentials from `.env`:

```bash
make -C hack/orca dev-azure FILE=/path/to/local-file
```

This replaces the legacy `seed-azure.sh` script (retired). Required
in `.env`: `AZURE_STORAGE_ACCOUNT`, `AZURE_STORAGE_KEY`,
`AZURE_CONTAINER`. The endpoint is computed as
`https://<account>.blob.core.windows.net/`.

For seeding into the in-cluster LocalStack S3 origin (the default
`awss3` mode), `orcadev` speaks S3 natively via the NodePort 30200:

```bash
make -C hack/orca data-upload FILE=/path/to/local-file
make -C hack/orca data-generate ARGS='--size 10MiB --count 5'
```

## Exercise the cache

See [quickstart.md - Steps 4-5](./quickstart.md#step-4---port-forward-the-orca-edge)
for the port-forward + `curl` walkthrough. For SHA-256 roundtrip
verification, parallel-GET benchmarks (with JSON output for
cross-run comparison), canned end-to-end scenarios, and cachestore
inspection via the `orcadev` tool, see
[quickstart.md - Step 8](./quickstart.md#step-8---roundtrip-benchmarks-and-scenarios).

The cluster-wide deduplication, singleflight collapse, and
warm-cache behavior are verified deterministically by
`make orca-inttest` against testcontainers; this Kind harness is
for validating the Kubernetes deployment shape (manifests, image,
headless DNS, RBAC, init-Job ordering) and for ad-hoc operator
exploration.

## See cluster-wide deduplication in action

The integration test `TestSingleflightCollapse` (under
`internal/orca/inttest/`) deterministically asserts this behavior
with byte-exact body checks and a `CountingOrigin` decorator. To
reproduce manually against this harness, fire concurrent GETs of a
fresh blob and tail the logs:

```bash
make -C hack/orca logs
```

You should see exactly one chunk-fill per chunk-key across the
cluster (coordinator selected by rendezvous-hash). Replicas that
received the client request but are not the coordinator forward via
`/internal/fill`. Once a chunk is committed to the cachestore,
subsequent GETs (and joiners that arrived during the fill) read from
cache.

## Switching to Azure mode (real Azure)

Edit `hack/orca/.env` and set:

```
ORIGIN_DRIVER=azureblob
ORIGIN_ID=azureblob-real
AZURE_STORAGE_ACCOUNT=<your-account>
AZURE_STORAGE_KEY=<your-key>
AZURE_CONTAINER=<your-container>
AZUREBLOB_ENDPOINT=                # leave blank for real Azure
```

Then:

```bash
make -C hack/orca deploy                          # idempotent
make -C hack/orca dev-azure FILE=/path/to/file    # uploads via orcadev -> real Azure
make -C hack/orca reset
```

The `dev-azure` target uses `hack/cmd/orcadev` under the hood,
constructing the endpoint as `https://<account>.blob.core.windows.net/`
and authenticating with `AZURE_STORAGE_KEY`. Pass `ARGS='--name foo'`
to override the destination blob name.

## Reset / iterate

```bash
# Rebuild the image and rolling-restart the deployment:
make -C hack/orca reset

# Tear down the whole Kind cluster:
make -C hack/orca down
```

To clear the cachestore bucket between manual experiments, exec into
the LocalStack pod or run a one-off `aws s3 rm s3://orca-cache --recursive`
job; the prior canned script was retired alongside the seeding helpers.

## Logging

The Orca pods default to info-level structured JSON logging. Set
`LOG_LEVEL=debug` in `hack/orca/.env` (then `make -C hack/orca deploy
&& make -C hack/orca reset`) for persistent per-chunk debug tracing,
or `kubectl set env deployment/orca ORCA_LOG_LEVEL=debug` for a
one-off runtime override. See
[quickstart.md - Step 6](./quickstart.md#step-6---watch-the-per-chunk-debug-trace)
for the structured-log shape and `jq` filter examples.

## Troubleshooting

### `localstack` deployment never goes Ready

Check the LocalStack pod's logs:

```bash
kubectl --context kind-orca-dev -n unbounded-kube logs deploy/localstack
```

If you see "License activation failed" with exit code 55, you're on the
Pro-only `latest` tag. The dev harness pins `localstack/localstack:3.8`
specifically to avoid this.

### `azurite` deployment never goes Ready (azureblob mode)

Check the Azurite logs:

```bash
kubectl --context kind-orca-dev -n unbounded-kube logs deploy/azurite
```

Most commonly the readiness probe is failing because Azurite was
launched with `--blobHost 127.0.0.1` (default) instead of `0.0.0.0`.
The harness's manifest already passes the right flag; if you've
overridden `AzuriteImage` to a custom build, ensure it accepts the
flag.

### `orca-buckets-init` Job fails

The Job waits up to 120 seconds for LocalStack readiness, then creates
both `orca-cache` and `orca-origin` and verifies cachestore versioning
is unset. Failures are typically LocalStack startup taking longer than
that on a slow disk; rerun the Job:

```bash
kubectl --context kind-orca-dev -n unbounded-kube delete job orca-buckets-init --ignore-not-found
make -C hack/orca deploy-localstack
```

### Orca pods CrashLoopBackOff with "config invalid: ..."

Check what's missing:

```bash
kubectl --context kind-orca-dev -n unbounded-kube logs deploy/orca | head
```

Common causes:
- In Azure mode, an empty `AZURE_STORAGE_ACCOUNT`/`AZURE_CONTAINER`
  (rendered into the ConfigMap).
- A missing `orca-credentials` Secret.

Fix:

```bash
$EDITOR hack/orca/.env
make -C hack/orca render        # re-render ConfigMap from .env
make -C hack/orca deploy-credentials
kubectl --context kind-orca-dev -n unbounded-kube apply -f deploy/orca/rendered/03-config.yaml
make -C hack/orca reset
```

### "OriginUnreachable" or 502 from manual GETs

In awss3 (default) mode:
- The bucket name in the URL must match `ORIGIN_AWSS3_BUCKET` (default
  `orca-origin`).
- Seed the bucket manually with `kubectl run orca-seed --rm -it
  --image=amazon/aws-cli:latest -- ...`.

In Azure mode:
- Account key wrong or revoked. Re-run `make -C hack/orca deploy-credentials && make -C hack/orca reset`.
- The blob doesn't exist in `$AZURE_CONTAINER`. Run `make -C hack/orca dev-azure`.

### kind load fails with "tag not found"

The `make image` target tags the image as `ghcr.io/azure/orca:dev` (the
default `ORCA_VERSION=dev`). If you overrode `VERSION` and got a slash
in the tag (git describe can produce e.g.
`images/agent-ubuntu2404-nvidia/v...-dirty`), the OCI tag is invalid.
Stick with `ORCA_VERSION=dev` for the dev harness.

## What this harness does NOT cover

- `cachestore/posixfs` and `cachestore/localfs` drivers (deferred; v1
  prototype has only `cachestore/s3`).
- Production auth (bearer tokens, mTLS edge, internal mTLS). All three
  are disabled by config in dev.
- Edge rate limiting and dynamic per-replica origin caps (see s15
  deferred-optimizations in `design/orca/design.md`).
- Mid-stream origin resume; if origin stalls after first byte the
  client sees a truncated body. Acceptable for the prototype.
- Crash recovery / unowned-key sweep (post-MVP).

For more on what's in vs out of scope, see `design/orca/design.md`
(in particular the
[Deferred / future work](../../designs/orca/design.md#15-deferred--future-work)
section).
