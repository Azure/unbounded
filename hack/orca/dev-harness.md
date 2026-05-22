<!-- Copyright (c) Microsoft Corporation. Licensed under the MIT License. -->

# Orca Dev Harness

A local end-to-end harness for the Orca origin cache. Stands up a Kind
cluster with three Orca replicas, an in-cluster LocalStack as the
cachestore, and an in-cluster origin (Azurite by default; LocalStack
S3 when `ORIGIN_DRIVER=awss3`). Both default paths run with zero real
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
| `azureblob` (default; Azurite) | Azurite (in-cluster) | `internal/orca/origin/azureblob` | None (well-known dev key) |
| `awss3` (opt-in)      | LocalStack S3 (in-cluster) | `internal/orca/origin/awss3` | None |
| `azureblob` (real Azure) | Azure Blob Storage | `internal/orca/origin/azureblob` | Account + key in `.env` |

The cachestore is always in-cluster LocalStack S3 (a different bucket
from the awss3 origin, in awss3 mode).

## What you get

- A Kind cluster named `orca-dev` with one control plane and three
  worker nodes (one per Orca replica via required pod-anti-affinity).
- LocalStack 3.8 running in the cluster as the S3-compatible
  cachestore (and origin in `awss3` mode). Community tier (`latest`
  is Pro-only and exits with code 55 "License activation failed").
  Always-on regardless of `ORIGIN_DRIVER`.
- Azurite (Microsoft's official Azure Storage emulator) running in
  the cluster as the default azureblob origin. Image
  `mcr.microsoft.com/azure-storage/azurite`. Always-on regardless
  of `ORIGIN_DRIVER` so switching modes via `.env` requires no
  redeploy.
- Buckets/containers self-healing on every emulator start:
  - `orca-cache` (S3) - cachestore (versioning unset; Orca's
    versioningGate rejects Enabled and Suspended). Created by the
    `localstack-init-buckets` ConfigMap mounted into LocalStack's
    `/etc/localstack/init/ready.d/`.
  - `orca-origin` (S3) - origin (used when `ORIGIN_DRIVER=awss3`).
    Created by the same LocalStack init hook.
  - `orca-test` (Azure container) - origin (used when `ORIGIN_DRIVER=azureblob`, the default).
    Created by the `container-ensurer` sidecar that runs alongside
    Azurite in the same Pod and loops every 30 seconds.
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
# Default values work; only edit if you want awss3 mode or real Azure.
```

`.env` is git-ignored. The default `ORIGIN_DRIVER=azureblob` runs
entirely on the in-cluster Azurite emulator (well-known dev key,
zero credentials).

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
5. `render-dev` - render `deploy/orca/dev/*.yaml.tmpl` (LocalStack
   Deployment + init-hook ConfigMap, Azurite Deployment with its
   container-ensurer sidecar).
6. `deploy-localstack` - apply the namespace and LocalStack, wait
   until Ready, then poll until LocalStack's init-hook has created
   `orca-cache` and `orca-origin` (the hook fires on every container
   start so this is also the clean recovery path; see "Recovery"
   below).
7. `deploy-azurite` - apply Azurite (always; it's the default
   origin), wait until Ready, then poll until its in-pod
   `container-ensurer` sidecar has created `orca-test`.
8. `deploy-credentials` - create the `orca-credentials` Secret.
9. `deploy-orca` - apply RBAC, ConfigMap, Services, Deployment.
10. `wait-ready` - block until all 3 replicas are Ready.

When this finishes you should see something like:

```
$ make -C hack/orca status
NAME                                READY   STATUS    RESTARTS   AGE
azurite-...                         2/2     Running   0          1m   (2/2 includes container-ensurer sidecar)
localstack-...                      1/1     Running   0          1m
orca-7c5d4f9b8c-...                 1/1     Running   0          50s
orca-7c5d4f9b8c-...                 1/1     Running   0          50s
orca-7c5d4f9b8c-...                 1/1     Running   0          50s
```

## Switching origins

The default origin is azureblob (Azurite). To switch to the awss3
(LocalStack) origin path, edit `hack/orca/.env`:

```
ORIGIN_DRIVER=awss3
ORIGIN_ID=awss3-localstack
```

Then either bounce the cluster cleanly:

```bash
make -C hack/orca down
make -C hack/orca up
```

Or keep the cluster and reconfigure Orca in place (Azurite +
LocalStack are both already deployed; switching modes is just a
ConfigMap change):

```bash
$EDITOR hack/orca/.env
make -C hack/orca deploy        # idempotent re-render + apply
make -C hack/orca reset         # rolling-restart Orca with new ConfigMap
```

When switching modes, remember that orcadev's defaults match the
default `.env` (azureblob). After switching to awss3 you'll either
update `defaultGlobalFlags` (don't; it's the dev default), pass
`--origin-driver=awss3 --origin-endpoint=http://localhost:30200
--origin-bucket=orca-origin` to each orcadev invocation, OR keep
your own pointer to a `--config` YAML that captures the awss3
coordinates.

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
make -C hack/orca data-random NAME=orca-test SIZE=10MiB
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

## Recovery

LocalStack and Azurite run with ephemeral storage (`emptyDir`,
`PERSISTENCE=0`). When their pods restart for any reason (OOM,
eviction, manual delete, kind node restart) they come back with
empty state. Orca's cachestore startup probe will then fail with
`NoSuchBucket` on the next restart, and operators using `azureblob`
mode will hit a similar "container does not exist" condition.

The harness handles this transparently:

- **LocalStack** runs a ConfigMap-mounted init hook under
  `/etc/localstack/init/ready.d/init-buckets.sh`. LocalStack rescans
  that directory on every container start; the script idempotently
  creates `orca-cache` and `orca-origin` and verifies cachestore
  versioning is unset.
- **Azurite** runs a `container-ensurer` sidecar in the same Pod.
  The sidecar loops every 30 seconds calling
  `az storage container create` idempotently. Within ~30 seconds of
  Azurite coming back up, the container is recreated.

If orca pods are still crash-looping after an emulator restart -
e.g. because orca raced the init hook on the very first start - the
clean recovery target is:

```bash
make -C hack/orca deploy-localstack       # awss3 / cachestore
make -C hack/orca deploy-azurite          # azureblob mode only
```

Both targets are idempotent: re-applying the Deployment is a no-op,
and the readiness poll confirms the bucket / container exists
before returning. A full `make orca-down && make orca-up` works too
but is heavier.

> Note: `deploy-azurite` only succeeds if the Azurite Deployment is
> already running (i.e. you initially brought the cluster up with
> `ORIGIN_DRIVER=azureblob`). If you switched modes after bringing
> the cluster up, run `make -C hack/orca up` (which calls
> `deploy-azurite-maybe`) to deploy Azurite first.

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

### Orca pods CrashLoopBackOff with "NoSuchBucket: orca-cache"

LocalStack pod restarted (OOM, eviction, etc.) and its in-memory
state was wiped. The init-hook ConfigMap re-creates the buckets on
every LocalStack start, so the clean recovery is to bounce
LocalStack so the hook re-fires, OR re-run the deploy-localstack
target which also waits until the buckets exist:

```bash
make -C hack/orca deploy-localstack
# or, equivalently:
kubectl --context kind-orca-dev -n unbounded-kube rollout restart deploy/localstack
kubectl --context kind-orca-dev -n unbounded-kube rollout restart deploy/orca
```

If the buckets still aren't appearing, inspect the LocalStack
container logs for `init:` lines emitted by the init script - they
will surface any malformed bucket name or unexpected
get-bucket-versioning state.

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

In azureblob (default) mode:
- The blob doesn't exist in `$AZURE_CONTAINER` (default `orca-test`).
  Seed it with `make -C hack/orca data-upload FILE=...` or
  `make -C hack/orca data-random NAME=... SIZE=...`.
- For real-Azure mode, account key wrong or revoked. Re-run
  `make -C hack/orca deploy-credentials && make -C hack/orca reset`.

In awss3 mode (opt-in via `.env`):
- The bucket name in the URL must match `ORIGIN_AWSS3_BUCKET`
  (default `orca-origin`).
- Pass `--origin-driver=awss3 --origin-endpoint=http://localhost:30200
  --origin-bucket=orca-origin` to orcadev invocations so they seed
  the correct backend, OR use `kubectl exec` against LocalStack.

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
