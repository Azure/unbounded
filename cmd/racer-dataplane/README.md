# Racer Dataplane

Placeholder for the replacement Racer dataplane implementation.

The previous Racer implementation has been removed. The root executable remains
a placeholder while supporting libraries are introduced as workspace members.
List only present crates in `Cargo.toml` and regenerate the root `Cargo.lock` when
adding a member. Keep the root package independent of those libraries until the
replacement application is introduced.

From the repository root, validate the current partial workspace with:

```sh
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 fmt --manifest-path cmd/racer-dataplane/Cargo.toml --all --check
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 check --locked --manifest-path cmd/racer-dataplane/Cargo.toml --workspace --all-targets --all-features -j 2
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 test --locked --manifest-path cmd/racer-dataplane/Cargo.toml --workspace --all-features -j 2 -- --test-threads=1
```

CI also checks and tests default features separately. Crates that require isolated
feature selection or hardware validation should add those checks with their code.
After adding dependencies, run locked `cargo fetch` and `make notice` so NOTICE
covers direct non-development dependencies of every declared member.
