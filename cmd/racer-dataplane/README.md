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

The `uring-runtime` library provides explicitly driven worker-local I/O, pinned
thread groups, and bounded handoffs. Its `simulation` and `test-util` features are
opt-in. Validate the production path on a Linux host with io_uring available:

```sh
RUNTIME_REQUIRE_IO_URING=1 timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 test --locked --manifest-path cmd/racer-dataplane/Cargo.toml -p uring-runtime --no-default-features -j 2 -- --test-threads=1
```

CI also runs strict Clippy with all features and no default features. Pure runtime
ownership tests run under pinned Miri with its default isolation and checks:

```sh
timeout --signal=TERM --kill-after=10s 300s rustup toolchain install nightly-2025-11-21 --profile minimal --component miri,rust-src
timeout --signal=TERM --kill-after=10s 300s cargo +nightly-2025-11-21 miri setup
timeout --signal=TERM --kill-after=10s 300s python3 hack/scripts/runtime-miri.py channel
```

Run the `offload`, `scheduler`, and `memory` groups separately with the same script.
Each group verifies that every exact allowlisted test actually ran.

The `page-alloc` library provides worker-local aligned buffers, lease-fenced slab
I/O, and bounded segment reclamation. Test it separately from runtime/workspace
tests so feature unification cannot enable simulation in its production gate:

```sh
PAGE_ALLOC_REQUIRE_REAL_IO=1 timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 test --locked --manifest-path cmd/racer-dataplane/Cargo.toml -p page-alloc --no-default-features -j 2 -- --test-threads=1
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 test --locked --manifest-path cmd/racer-dataplane/Cargo.toml -p page-alloc --no-default-features --features simulation -j 2 -- --test-threads=1
```

The production gate requires real io_uring and direct-I/O support: capability
skips fail when `PAGE_ALLOC_REQUIRE_REAL_IO=1`. CI also checks all targets and
runs strict Clippy for each allocator mode.

The `flow-control` library provides policy-driven quotas, charged buffers, bounded
kernel pipes, adaptive admission, and worker-local coalescing. Application policy
and real operation completion remain the caller's responsibility. Its crate docs
describe ownership and admission contracts; generate them with:

```sh
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 doc --locked --manifest-path cmd/racer-dataplane/Cargo.toml -p flow-control --no-deps
```

Validate flow separately so workspace feature unification cannot enable simulation
in its production gate:

```sh
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 test --locked --manifest-path cmd/racer-dataplane/Cargo.toml -p flow-control --no-default-features -j 2 -- --test-threads=1
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 test --locked --manifest-path cmd/racer-dataplane/Cargo.toml -p flow-control --no-default-features --features simulation -j 2 -- --test-threads=1
```

The flow production tests require Linux kernel pipes, Unix sockets, and splice.
They exercise real I/O and fail rather than skipping unsupported operations.
Simulation adds simulated and mixed-descriptor contracts without removing the
real pipe tests. CI checks all targets and runs strict Clippy in each mode.

The `http1` library provides strict fixed-length HTTP/1.1 heads, exclusive
connections and bounded pools, immutable-body delivery, and opaque-body relay.
Callers own admission, scheduling, authorization, and exchange finalization.
Its crate docs describe framing and completion-ownership contracts.

Validate HTTP independently so workspace feature unification cannot enable runtime
simulation. HTTP's only optional feature is `test-util`; both isolated modes use
real Unix sockets and io_uring, with no capability skips:

```sh
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 test --locked --manifest-path cmd/racer-dataplane/Cargo.toml -p http1 --no-default-features -j 2 -- --test-threads=1
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 test --locked --manifest-path cmd/racer-dataplane/Cargo.toml -p http1 --all-features -j 2 -- --test-threads=1
timeout --signal=TERM --kill-after=10s 300s cargo +1.96.0 doc --locked --manifest-path cmd/racer-dataplane/Cargo.toml -p http1 --no-deps --all-features
```

Tests cover parser rejection, sequential upload/fetch exchanges, body bounds,
read-ahead, cancellation fences, and transfer fallback. Transfer fixtures script
pipe outcomes while checking actual Unix socket bytes. CI checks all targets and
runs strict Clippy separately for production and all HTTP features.
