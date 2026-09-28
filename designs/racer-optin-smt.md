# Racer opt-in SMT worker placement

Base: `cccae518`. This implements one runtime policy option.

## Contract

The source design's pinned thread-per-core model and default maximum of eight
threads remain the default. `RACER_ALLOW_SMT=true` explicitly permits using SMT
logical CPUs to run more complete reactor/crypto pairs. The parser defaults to
false and accepts only exact lowercase booleans (`cmd/racer-dataplane/src/config.rs:159-166`).

Physical-core sizing remains the default; SMT sizing counts unique allowed
logical CPUs. Both apply the thread cap and tightest CPU-time quota, flooring to
complete pairs with the existing positive-quota minimum of one pair. SMT rejects
fewer than two logical CPUs so no two roles share a logical CPU. The default's
single-CPU fallback is retained (`cmd/racer-dataplane/src/runtime/affinity.rs:90-135`).
Discovery still intersects online CPUs, process affinity, and ancestor cpusets
before planning (`cmd/racer-dataplane/src/runtime/affinity.rs:307-387`).

Placement reserves reactors before crypto, preferring distinct package/core
identities, NIC locality, and ascending CPU IDs. It reserves all available crypto
siblings before fallback can consume another pair's sibling. Fallback uses an
unused CPU, preferring the reactor's NUMA node. This handles asymmetric cpusets,
multi-package core IDs, missing siblings, and more than two threads per core
(`cmd/racer-dataplane/src/runtime/affinity.rs:227-301`).

The production startup path already partitions aggregate budgets and reduces the
pair count until every worker's progress floors fit, before constructing the
worker directory or starting threads (`cmd/racer-dataplane/src/app.rs:186-207,225-279`).
No budget or queue defaults are changed by the option.

For sibling pairs `(0,4)`, `(1,5)`, `(2,6)`, `(3,7)`, with uniform NIC locality,
all eight CPUs allowed, adequate quota, and adequate memory, default placement is
`(reactor, crypto) = (0,1), (2,3)`. Opt-in placement is
`(0,4), (1,5), (2,6), (3,7)`, verified by the affinity regression test.

With the supplied node budgets, four workers each receive:

| Dimension | Per worker |
| --- | --- |
| Plaintext / ciphertext | 1 GiB each |
| Relay transfers | 64 |
| Pipes | 32 |
| Client connections | 512 |
| Queue entries | 256 |
| Connections per neighbor | 16 (per-neighbor cap, not divided) |

These aggregate partitions are half the two-worker partitions. The startup
regression test checks both policies, reduction to two workers at the exact
plaintext progress floor, and failure below the single-worker floor.

## Verification

Run from the worktree root. Each Cargo command below used the shared project
target directory `/home/azureuser/code/unbounded/bin/racer-cargo` and an external
`timeout --signal=TERM --kill-after=10s 300s` bound.

- `cargo fmt --manifest-path cmd/racer-dataplane/Cargo.toml --check`: passed.
- `cargo clippy --locked --manifest-path cmd/racer-dataplane/Cargo.toml --target-dir /home/azureuser/code/unbounded/bin/racer-cargo --all-features --all-targets -j2 --message-format=short`:
  completed, with warnings in unchanged code and none on new SMT code.
- `cargo test --locked --manifest-path cmd/racer-dataplane/Cargo.toml --target-dir /home/azureuser/code/unbounded/bin/racer-cargo --all-features --lib -j2 <filter> -- --test-threads=2`:
  - `smt`: 5 passed, including exhaustive placement over 256 allowed CPU subsets.
  - `runtime::`: 100 passed, 1 explicitly ignored benchmark.
  - `config::`: 25 passed.
  - `app::native::`: 11 passed, 1 explicitly ignored hardware test.
- `git diff --check`: passed.

The initial runtime run failed two filesystem fixtures because this fresh
worktree lacked `cmd/racer-dataplane/target`. Creating the scratch directory,
as `make racer-rust-test` does, resolved both failures on the bounded rerun.

The setting is passed through the existing operator ConfigMap `envFrom`
(`internal/operator/components/racer/racer.go:267`); no generation is required.
Cluster A/B deployment and performance measurement belong to the parent task.
