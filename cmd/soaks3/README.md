# soaks3

`soaks3` is an S3 load generator for benchmarking an unbounded-storage cluster.
It works in two phases, exposed as two subcommands:

- `seed` generates deterministic test objects onto the local filesystem,
  mirroring the bucket key layout. The tree is uploaded to an origin bucket out
  of band.
- `run` drives read load against an unbounded-storage S3 frontend, requesting
  the same keys back through GET (optionally ranged) requests.

Object contents are generated deterministically from a `--seed`, so the same
key always holds the same bytes. `seed` writes a `manifest.json` describing the
data set (count, object size, key prefix, seed) that `run --manifest` reads to
auto-configure itself.

Read load selects keys using a Zipf (default) or uniform distribution. The
distribution is keyed by shared seeds so that the hot-key set is identical
across every `soaks3` instance in the cluster, making cache-churn and hot-key
behavior reproducible. The `run` subcommand exposes a Prometheus `/metrics`
endpoint and prints periodic progress and a final summary.

Run `soaks3 version` to print the binary version.

## Usage

### Seed a data set

```bash
# Generate 10GB of 1.25GB objects into ./data
soaks3 seed --out-dir ./data --object-size 1.25GB --total-size 10GB

# Or generate an explicit number of objects
soaks3 seed --out-dir ./data --object-size 4MiB --count 1000
```

Upload the contents of `--out-dir` (including `manifest.json`) to your origin
bucket out of band, then drive read load against the frontend.

### Drive read load

```bash
# Auto-configure count/object-size/key-prefix from the seed manifest
soaks3 run \
  --endpoint http://127.0.0.1:9000 \
  --bucket my-bucket \
  --manifest ./data/manifest.json \
  --concurrency 32 \
  --duration 5m

# Ranged GETs at a target aggregate rate
soaks3 run --manifest ./data/manifest.json --range-read --range-size 64KiB --rate 500
```

## Container image and in-cluster deployment

`soaks3` is developer tooling, so its image is never built for tagged releases.
The image is only published on demand via the `unbounded container images`
workflow (`.github/workflows/images.yaml`): trigger it manually
(`workflow_dispatch` with `image: soaks3`) or push an `images/soaks3/<version>`
tag. Both publish `ghcr.io/<owner>/soaks3:<tag>`.

Build the image locally with:

```bash
make image-soaks3-local          # build ghcr.io/azure/soaks3:<version>
make image-soaks3-push           # build and push
```

Deploy manifests live under `deploy/soaks3/` and are **not** applied by any
default deploy flow or bundled into `release-manifests`. Render them on demand
and apply by hand against a throwaway/dev cluster:

```bash
make soaks3-manifests SOAKS3_IMAGE=ghcr.io/azure/soaks3:dev
kubectl apply -f deploy/soaks3/rendered/
```

The rendered Deployment runs `soaks3 run` and exposes Prometheus metrics on
port `9300` (also fronted by a ClusterIP Service). Adjust the endpoint, bucket,
and data-set flags via the `--set Key=Value` knobs documented in
`deploy/soaks3/03-deployment.yaml.tmpl`.

