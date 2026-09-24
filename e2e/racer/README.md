# Racer kind smoke test

```sh
make e2e-racer-build
make e2e-racer
```

One test creates a local kind cluster with one control-plane node and one worker.
The shipping operator deploys one Racer control plane and dataplane for one Site
and ClusterCache. After cache activation, a Go SDK pod serves an origin and checks
exact cold-read bytes, warm reuse without additional origin payload fetches, and
a range crossing a page boundary. There are no separate native Racer e2e suites.

## Requirements

- Linux with usable io_uring, 4 KiB pages, at least two physical cores on one NUMA
  node, and enough memory for kind, the dataplane, and image builds.
- Docker access, kind, kubectl, and Go matching `go.mod`.
- An ext4 checkout or an ext4 directory selected by `RACER_E2E_DIR`. The test
  bind-mounts this storage into the worker for real slab files.

The build target builds the control-plane, dataplane, operator, and SDK fixture
images. The test target uses those prebuilt images and has a five-minute timeout,
including cleanup. Individual operations have shorter deadlines and log progress.
Set `RACER_E2E_IMAGE_TAG` to select a tag other than the default `racer-e2e`; all
four local images must have that tag. CI uses `ci` with cached image builds.
`RACER_E2E_NODE_IMAGE` overrides the default `kindest/node:v1.33.1`.

The cluster is cleaned up automatically; images are retained for repeated runs. Failure
diagnostics remain under `e2e/racer/.artifacts/` (or `RACER_E2E_DIR`), including
resources, events, and kind logs. Missing prerequisites fail the test.

Use `make e2e-racer-compile` to check compilation without starting a cluster.
Normal Go and Rust unit/component tests remain separate from this smoke test.
