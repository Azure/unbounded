# Racer Dataplane

This workspace introduces the `uring-runtime` crate for the replacement Racer
dataplane. The root binary remains an empty placeholder; the replacement
dataplane and topology crate are not included.

The runtime provides explicitly driven, worker-local io_uring execution, with
optional `simulation` and `test-util` features. Its default feature set is empty.

From the repository root, use `make racer-dataplane-check` to check formatting
and Clippy, `make racer-dataplane-build` to build the workspace, and
`make racer-dataplane-test` to run all-feature unit, integration, and documentation
tests. `make racer-runtime-test` tests the runtime without optional features and
requires a Linux host that permits io_uring. CI requires native io_uring coverage
for both test targets.
