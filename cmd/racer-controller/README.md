# Racer Controller

`racer-controller` is the Go control plane for Racer. It publishes member and
cache topology, issues peer credentials, rotates keys, and serves the HTTPS
control API. Kubernetes leader election selects the writer; synchronized
replicas can serve validated state.

This component includes the Racer API types, controller packages, deployment
templates, and image definition. The dataplane, SDK, and operator integration are
separate work. The workload builder in `internal/racer/members` is retained, but
this controller does not create or reconcile dataplane workloads.

## Build and test

Run from the repository root with the repository's Go and golangci-lint tools:

```sh
timeout --signal=TERM --kill-after=10s 300s make racer-controller-build
timeout --signal=TERM --kill-after=10s 300s make racer-test
```

The build writes `bin/racer-controller` with build metadata. `racer-test` runs
lint and race tests for the API, controller, and deployment packages.
`make racer-controller` combines those checks and the build.

For local API-server integration tests, use:

```sh
timeout --signal=TERM --kill-after=10s 300s make racer-envtest-ci
```

This target provisions pinned envtest assets. To use existing repository-local
assets, run `make racer-envtest KUBEBUILDER_ASSETS=<assets-directory>` under the
same timeout. These tests cover initialization, recovery, leader election, and
HTTPS serving, not a running dataplane. Go test targets use `-timeout=5m`.

## Run

The binary takes no arguments or subcommands, including no `initialize` command.
Configuration comes from environment variables. `RACER_CLUSTER_ID` must be a
permanent UUID. The deployment supplies Pod identity, serving TLS, replication
trust, and a dedicated projected replication token.

Every replica validates durable installation state before starting the manager.
Successful recovery alone does not grant serving readiness. See the
[standalone deployment guide](../../deploy/racer/README.md) for prerequisites,
installation order, and state that must be retained.
