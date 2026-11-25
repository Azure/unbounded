# orca

Orca is an origin cache. It runs inside a datacenter, serves an S3-compatible HTTP API, and reads objects from cloud
blob storage (AWS S3, Azure Blob) on a cache miss, keeping them as fixed-size chunks in an in-datacenter store.
`designs/orca/design.md` describes the design, and `hack/orca/README.md` covers development.

## Layout

- `main.go` and `orca/` - the `orca` binary.
- `internal/` - private packages. `internal/app` wires them together, `internal/inttest` holds the integration tests,
  and `internal/manifests` tests the `deploy/orca` templates.
- `hack/orcadev/` - the `orcadev` tool for seeding, inspecting and benchmarking a running dev cluster.
- `deploy/orca/` - manifests. The operator does not manage them: `make orca-install` applies them.
- `images/orca/` - the container image.
- `hack/orca/` - dev cluster scripts, the example config and the integration test guide.

## Building and testing

- `go test ./cmd/orca/...` runs the unit tests.
- `make orca-inttest` runs the integration tests. They need Docker and the `integrationtest` build tag.
- `make orca` builds the binary after lint and tests, and `make orca-build` builds it without them. `make orcadev`
  builds the dev tool.
- `make orca-kind-up` creates a kind cluster with orca installed.
