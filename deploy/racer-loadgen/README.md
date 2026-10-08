# Racer load generator deployment

The base example serves a synthetic OCI registry and reads it through an existing
Gantry installation. The [direct overlay](direct/README.md) reads a dedicated
ClusterCache through the Racer SDK instead. Use only one variant per namespace.
Neither variant installs Gantry or Racer.

## Build

From the repository root:

```sh
make racer-loadgen-build
make image-racer-loadgen-local VERSION=dev
make racer-loadgen-manifest-test
```

Load the image onto your test nodes, or publish it to your own registry and edit
the chosen kustomization's `images` entry. `racer-loadgen:dev` is a local build
tag, not a published image. See [CLI usage](../../cmd/racer-loadgen/README.md)
for workload flags and metrics. The image uses the same Go version as go.mod.

## Base prerequisites

- Create the `unbounded-system` namespace before applying the example.
- Run Gantry on every selected Linux node, in the same namespace. Its pods must
  have `app.kubernetes.io/name: gantry` and `app.kubernetes.io/component: agent`
  labels and serve the mirror on port 5000.
- Add this entry to Gantry's `upstream_registries` configuration, preserving any
  other entries you need, then roll out that configuration:

  ```yaml
  - name: loadgen.invalid
    endpoint: http://racer-loadgen-origin:8080
  ```

  For the Gantry Helm chart, the corresponding list is
  `gantry.upstreamRegistries`. This example does not change Gantry's configuration.
- Match the DaemonSet's placement to Gantry. Both Services use
  `internalTrafficPolicy: Local`, so a node without a ready local backend cannot
  use them. Allow pod traffic to Gantry port 5000 and the origin port 8080.
- Keep the seed, image version, and catalog settings identical on all nodes.
  These defaults generate continuous load. Tune concurrency and resource requests
  for your environment before applying them.

## Render and apply the base

```sh
kubectl kustomize deploy/racer-loadgen
kubectl apply -k deploy/racer-loadgen
kubectl -n unbounded-system rollout status daemonset/racer-loadgen --timeout=5m
```

Review the rendered output before applying. Metrics and health endpoints use
port 9090; the pods carry Prometheus scrape annotations. Readiness confirms the
catalog and origin are ready, not that pulls through Gantry succeed. Check load
metrics and logs for failures.

The offline manifest test renders both variants and checks resource scope,
arguments, security, socket mounts, probes, and Service routing. It does not
validate a live cluster, storage setup, or throughput.

The optional S3 deployment is deferred. This package does not include or reference
a racer-object executable or cluster-specific benchmark campaigns.
