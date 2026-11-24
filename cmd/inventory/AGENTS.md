# inventory

Collects hardware inventory from bare-metal nodes and stores it in PostgreSQL. `designs/inventory.md` describes the data
flow, the gRPC API and the database schema.

## Layout

- `inventory-agent/`, `inventory-aggregator/`, `inventory-inspector/`, `inventory-viewer/` - one directory per binary.
- `internal/` - one package per binary. The viewer's web pages are embedded from `internal/viewer/static/`.
- `api/inventory/v1` - the gRPC API the agent publishes to the aggregator with. Run `make generate` after changing it.
- `deploy/inventory/` - manifests for the aggregator, inspector and viewer. The operator does not manage them:
  `make inventory-manifests` renders them for `kubectl apply`.
- `images/inventory/` - container images for the aggregator, inspector and viewer.

## Things to know

- The agent runs once, as root, directly on a host, and keeps a local SQLite database. It ships as a released binary,
  not an image, and nothing schedules it.
- The aggregator, inspector and viewer connect to PostgreSQL, which is provisioned separately.

## Building and testing

- `go test ./cmd/inventory/...` runs the tests.
- `make inventory-all` builds all four binaries after lint and tests. `make inventory-<binary>-build` builds one
  without them.
- `make image-inventory-all-local` builds the three images.
