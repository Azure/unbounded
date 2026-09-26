# Racer full-stack e2e test

Run from the repository root on a Linux Docker host with Go, Docker, kind,
kubectl, and make installed. Build the images first, outside the test timeout:

```sh
for component in unbounded-operator racer-controller racer-dataplane gantry; do
  docker build -f "images/$component/Containerfile" --build-arg VERSION=e2e \
    -t "docker.io/library/$component:e2e" . || break
done

make e2e-racer
```

The test uses these local images and creates an isolated kind cluster with one
worker. Setup and pull share a seven-minute deadline, leaving time for diagnostics
and cleanup within the ten-minute test timeout. Commands have a two-minute
deadline; workload waits are capped at 90 seconds. The host
must support Racer's io_uring and direct-I/O requirements. The kind nodes must
be able to reach HTTP fixtures on the Docker network's IPv4 host gateway.

The test deploys the real unbounded-operator using its rendered manifests and
creates a single `ClusterCache` named `gantry`. The operator and Racer controller
provision Racer's initialization, identity, configuration, and workloads. A
test Gantry DaemonSet shares the node's Racer socket directory and runs in Racer
mode with the same UID as the dataplane.

The acceptance check is a digest-pinned containerd image pull and unpack from an
empty content namespace. Its only registry endpoint is a recording proxy to
Gantry; there is no direct upstream fallback. The fixture includes a compressed
65 MiB incompressible layer spanning five Racer pages. The test verifies:

- Racer readiness before and after the pull.
- Complete manifest, config, and layer delivery with `Gantry-Mirrored: 1`.
- Origin requests for every object.
- Exact SHA-256 digests and sizes in containerd's content store.

This covers the single-node origin-backed path through Gantry, the Go SDK, the
deployed Rust dataplane, and Gantry's origin adapter.

## Iteration and diagnostics

- Rebuild changed components before rerunning the test; it always uses locally
  built `docker.io/library/*:e2e` images.
- `RACER_E2E_KEEP_CLUSTER=1` retains the cluster for debugging. Delete it with
  `kind delete cluster --name <name>` before another run on resource-limited hosts.
- Generated fixtures, kubeconfig, pod/event diagnostics, and port-forward logs
  stay in the reported `tmp/racer-e2e-*` directory, including after failures.
- Readiness checks recreate port-forward processes that exit while the remote
  listener is starting. Each attempt retains a `forward-<port>-<attempt>.log`;
  an HTTP 200 is required within the readiness deadline.
- The cluster is deleted by default. No existing cluster is used.

For local streaming regression iteration, explicitly select a retained e2e kind
cluster (the test rejects other context names):

```sh
RACER_E2E_KUBECONFIG=/absolute/path/to/retained/kubeconfig \
RACER_E2E_STREAM_CONCURRENCY=64 \
  timeout 600s go test -tags=e2e ./e2e/racer -run '^TestRetainedKindStream$' \
  -parallel=64 -count=1 -timeout=9m -v
```

This test updates the test Gantry origin ConfigMap and restarts its DaemonSet.
It creates a fresh five-page layer, verifies three complete sequential responses,
then verifies full-body SHA-256 and lengths for concurrent readers (default four,
maximum 64). It preserves the cluster and records diagnostics under `tmp/`.
Rebuild and load the dataplane image, then replace its local pod before testing a
new candidate; the test itself does not replace the dataplane or controller.

## Full eight-layer image regression

From the repository root, select a retained local e2e kind cluster explicitly:

```sh
timeout 900s python3 e2e/racer/full-image-kind.py \
  --kubeconfig /absolute/path/to/retained/kubeconfig --concurrency 64
```

The script builds a test-only probe image and runs one finite batch inside the
cluster. The data path is pod -> Gantry Service -> Racer -> in-cluster origin;
there is no port-forward. Each pull verifies the manifest, config, and all eight
64 MiB-base layers with 20% deterministic size jitter using the real loadgen
puller. The `benchmark-v1` image is 542,952,560 bytes including manifest and config.
Success requires every image, every layer, exact body-byte totals, and zero errors,
cancellations, or remaining in-flight pulls. Per-pull errors and final counts are
retained under `tmp/racer-full-image-*`.

Use a new `--seed` for an uncached image and `--direct-origin` for the control
batch. Each pull has a 90-second deadline, the probe has a 115-second test timeout,
and the pod has a 120-second active deadline. The script has a 900-second outer
deadline; build commands are bounded at 600 seconds and all other subprocesses
at 120 seconds or less. The Gantry ConfigMap is restored and temporary Kubernetes
resources are deleted in cleanup. The installed dataplane is selected separately,
so the same probe can compare the base and candidate images.
