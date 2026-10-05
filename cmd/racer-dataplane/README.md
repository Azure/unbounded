# Racer Dataplane

Cargo scaffolding for the replacement Racer dataplane. The root package contains
library and no-op binary placeholders, not an implemented dataplane.

The `topology` workspace member provides deterministic membership graphs,
weighted placement, and bounded route searches. Its public API and usage examples
are documented in `topology/src/lib.rs`.

The runtime, native adapters, controller integration, and deployable binary are
not included yet. Topology does not require io_uring, RDMA libraries, or elevated
privileges.

## Development

CI uses Rust 1.96.0 with the rustfmt and Clippy components. From the repository
root:

```sh
make racer-dataplane-fmt
make racer-dataplane-check
make racer-dataplane-build
make racer-dataplane-test
```

The check target verifies formatting and runs Clippy with warnings denied. The
test target includes unit tests, integration tests, and documentation examples.
Each Cargo build, check, or test command is bounded to five minutes. Artifacts go
to `bin/racer-cargo`; override `RACER_CARGO_TARGET_DIR` to change that location.
Use `RACER_CARGO='cargo +1.96.0'` to select the CI toolchain explicitly and
`RACER_TEST_ARGS` to pass arguments to the Rust test harness.
