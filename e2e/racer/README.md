# Racer full-stack e2e test

Run from the repository root on a Linux Docker host with Go, Docker, kind,
kubectl, and make installed. Build the images first, outside the test timeout:

```sh
for component in unbounded-operator racer-controller racer-dataplane gantry; do
  docker build -f "images/$component/Containerfile" --build-arg VERSION=e2e \
    -t "docker.io/library/$component:e2e" . || break
done

go test -tags=e2e ./e2e/racer -run TestOperatorImagePull -count=1 -v -timeout=10m
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
layer larger than Racer's 16 MiB page size. The test verifies:

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
