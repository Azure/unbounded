# unbounded-storage

`unbounded-storage` is one of two Rust crates in this repository; the
rest of the project is Go. The other Rust crate is `unbounded-s3`
(see `cmd/unbounded-s3/AGENTS.md`), which depends on `unbounded-storage`
as a library. The conventions below intentionally diverge from
the top-level `AGENTS.md` where they need to. Read this file before
touching anything under `cmd/unbounded-storage/`.

## Crate layout

- Each subsystem is a directory module:
  - `src/<area>/mod.rs` declares its private and public child modules
    and re-exports the public surface (`pub use ...`).
  - Internal submodules (`free_list`, `inflight`, `types`, `traits`,
    ...) are declared without `pub` and only their selected types are
    re-exported from `mod.rs`.
  - Public-facing submodules (`pool`, `stream`, ...) may be `pub mod`
    when their types are referenced by path elsewhere.
- Inline unit tests live at the bottom of the `src/` file that defines
  the construct under test, inside a `#[cfg(test)] mod tests { ... }`
  block.
- Module integration tests live next to the code as
  `src/<area>/tests.rs`, gated behind `#[cfg(test)] mod tests;` in
  `mod.rs`.
- Integration / DST tests live under `tests/` (see below).
- `target/` is gitignored.
- There is no `clippy.toml` or `rustfmt.toml`; use defaults.

## File headers

Every `.rs` file - source and test - starts with:

```
// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.
```

Add it to any new file.

## Code style

These rules are about keeping the crate legible for the next human
or agent that opens a file cold. They are deliberately stricter than
`rustfmt`/`clippy` defaults because those tools do not enforce
structure.

- **File length.** Keep `.rs` files under 1500 lines. If a file is
  approaching that limit, that is a strong signal it is doing too
  much; split it along concept boundaries rather than mechanical
  ones (e.g. by trait or subsystem, not "types A-M vs N-Z").
- **Comments.** Avoid commentary that restates what the code already
  says. Reserve comments for invariants, non-obvious reasoning,
  cross-references, and intentional deviations. Doc comments on
  public items are encouraged; running narration inside function
  bodies is not.
- **Declaration order.** Order items within a file so a human reader
  encounters concepts in the order they need to understand them:
  primary type(s) first, then their `impl` blocks, then closely
  related helper types, then free functions, then tests. Inside an
  `impl`, group constructors before mutators before accessors. The
  goal is that someone skimming top-to-bottom builds a mental model
  without having to jump around.
- **Separation of concerns per file.** Every file should have a
  short, human-legible answer to "what is this file about?". If you
  cannot summarize a file's responsibility in one sentence, it
  probably needs to be split. Conversely, do not shatter a single
  concept across many tiny files just to keep file counts down per
  type - a module with twenty one-screen files is as hard to
  navigate as a single 3000-line file. Aim for a small number of
  files per module, each with a distinct concern (for example
  `types.rs`, `traits.rs`, `free_list.rs`, `inflight.rs`), and
  resist adding a new file unless it carves out a concern the
  existing files genuinely do not own.

When in doubt, optimize for the reader who has never seen this code
before.

## Build and test

Use the top-level Makefile targets, not raw `cargo` invocations, so CI
and local runs stay aligned:

- `make unbounded-storage` - format, lint (currently implicit via
  `cargo`), test, then release build. This is the default "did I break
  it" target.
- `make unbounded-storage-test` -
  `cargo test --manifest-path cmd/unbounded-storage/Cargo.toml --locked --all-targets`.
  `--all-targets` is required because the proptest-driven DST tests live
  in `tests/` and would otherwise be skipped from some invocations.
- `make unbounded-storage-build` - release build only, no tests.

Always pass `--locked` for any direct `cargo` use; the lockfile is the
source of truth.

### libfabric dependency

The `fabric` module links against libfabric (the C `shim.c` is compiled
against its headers by `build.rs` via pkg-config, and the binary loads
`libfabric.so` at runtime). The fabric uses the native `tcp` RDM
provider, which only exists in libfabric 2.0+ (the experimental `net`
provider was merged into `tcp` and removed there). Distro packages are
usually too old, so the Makefile builds a pinned release from source:

- `make libfabric` downloads and installs the pinned `LIBFABRIC_VERSION`
  (see the Makefile default) under `tmp/libfabric/<version>/`. It is a
  no-op once built; remove `tmp/libfabric` to force a rebuild.
- The `unbounded-storage-*` targets depend on it and export
  `LIBFABRIC_PKG_CONFIG_PATH` and `LD_LIBRARY_PATH` for `cargo`
  automatically, so prefer the Makefile targets over raw `cargo`. If you
  must run `cargo` directly, point it at the pinned install yourself.

## Testing patterns

There are three distinct in-process test styles in this crate plus an
out-of-process end-to-end smoke test, and they are not interchangeable.
Pick the one that matches what you are testing.

### 1. Inline unit tests (bottom of `src/<area>/<file>.rs`)

Used for specific, targeted unit tests of constructs declared in the
same file. Reach for these when you want to pin down the behavior of
one type, function, or small helper in isolation, without exercising
the surrounding subsystem.

Conventions:

- Tests live in a `#[cfg(test)] mod tests { ... }` block at the bottom
  of the file that declares the construct under test. Do not put
  cross-file or cross-module tests here; that is what `tests.rs` is
  for.
- Keep these tests synchronous where possible. If the construct is
  async, use the same noop-waker `block_on` pattern as the module
  integration tests rather than pulling in an async runtime.
- Scope tests narrowly to the file's public and private surface. If a
  test needs types or behavior from sibling files in the same module,
  promote it to `src/<area>/tests.rs`.

### 2. Module integration tests (`src/<area>/tests.rs`)

Used for broader exercises of a subsystem that span multiple files
within the module, against hand-written mocks where deterministic
scheduling is not required. Focus on user-facing scenarios: drive the
module through its public surface the way a caller would and assert
on observable outcomes.

Conventions:

- The module is private (`mod tests;`) and gated with `#[cfg(test)]`
  in the parent `mod.rs`.
- The test file defines its own minimal `block_on` (and friends like
  `block_on_two`) built on a noop `RawWaker`/`RawWakerVTable`. Do not
  pull in `tokio` or any other async runtime; the crate is runtime
  agnostic by design.
- `block_on` loops include a spin counter with a generous bound
  (`< 1_000_000`) and `assert!` on no-progress to fail loudly instead
  of hanging.
- Mocks are heap-backed and live in the same file as the tests.
- Tests in this style are not deterministic across schedules - they
  validate behavior under a single, fixed polling order. Use the DST
  harness when you need schedule coverage.

### 3. Deterministic Simulation Testing (`tests/`)

The integration test target wires every subsystem into a single
deterministic, seeded executor and drives randomized workloads through
proptest. This is the primary correctness gate for concurrent code.

Layout:

- `tests/dst.rs` - the only file at the root of the integration test
  target. It exists solely to declare `mod blockstore; mod bufferpool;
  mod framework;` so all of the modules below compile as a single
  test binary. Add new top-level DST areas here.
- `tests/framework/` - generic, project-agnostic DST primitives.
  - `executor.rs` - the single-threaded executor: seeded
    `ChaCha8Rng`, thread-local `SimState`, a ready queue from which
    one task is picked uniformly at random on every step, a
    `step_budget` for liveness, and an explicit `Deadlock` error when
    no task is ready while tasks remain alive. Public surface:
    `Executor`, `SimState`, `RunError`, `with_sim`, `yield_once`,
    `yield_n`. **Do not** add subsystem-specific knobs here - the
    framework only knows about scheduling and the PRNG; latency, fault
    rates, cache-hit rates, etc. belong in per-area mock configs.
- `tests/<area>/` - per-subsystem DST plug-in. Each contains:
  - `mod.rs` - module declarations and any `#![allow(...)]` attributes
    needed for the test build (e.g. `arc_with_non_send_sync` because
    production types are `Send + Sync` but the single-threaded mocks
    are intentionally `!Send`).
  - `mocks.rs` - DST-aware implementations of the subsystem's traits.
    Mocks must pull all randomness from `framework::executor::with_sim`
    and route "I/O latency" through `yield_n` with a count drawn from
    the same PRNG. Per-area knobs (delay bound, fault rate, hit rate,
    corruption rate, ...) live on a local `*SimCfg` struct held behind
    `Rc`, not on `SimState`.
  - `workload.rs` (or equivalent) - the workload model
    (`Workload`/`ClientSpec`/...), the proptest strategy
    (`workload_strategy()`), and a `run`/`run_workload` driver that
    constructs the executor, installs the mocks, spawns tasks, runs to
    completion, and returns a `RunReport`.
  - `oracle.rs` (when relevant) - reference model tracked alongside
    the system under test. The oracle constrains what successful reads
    must observe; it does not constrain what the system may evict or
    drop.
  - `tests.rs` - the proptest entrypoints. A single `proptest!` block
    is preferred per area; inside, one `#[test]` function calls
    `run_workload` once and then dispatches to a series of small
    `assert_<invariant>` helpers that return
    `Result<(), TestCaseError>`. Each invariant is documented with a
    short comment naming what it covers.
  - `recovery.rs` (or other explicit-state files) - hand-rolled
    scenarios that pin down specific behaviors the proptest only
    covers end-to-end. Use these when a particular sequence is too
    rare for proptest to reliably exercise or when the failure mode is
    easier to read as a scripted test.
  - `tests.proptest-regressions` - generated by proptest on failure;
    commit it so the regression case is replayed on every run.

Required properties of any DST mock or workload code:

- All randomness flows through `framework::executor::with_sim` so the
  `(seed, workload)` pair fully determines the run. Do not call
  `rand::thread_rng()` or use `SystemTime` from inside a mock.
- Every async I/O point in a mock yields a random number of times via
  `yield_n` so the executor can interleave it with other tasks. Mocks
  that complete synchronously hide bugs.
- `run_workload`-style drivers must return the underlying `RunError`
  (or convert it into a panic at the call site) so a deadlock or
  step-budget exhaustion is surfaced as a test failure, not a hang.
- `ProptestConfig { cases: 256, .. }` is the current default; bump it
  locally or via `PROPTEST_CASES` for soak runs but keep the committed
  value modest so CI stays fast.
- Prefer many small invariants over one giant assertion. Each
  invariant function takes `&RunReport` and returns
  `Result<(), TestCaseError>`; this keeps shrinking output legible.

### Re-running a specific DST failure

A DST run is fully determined by `(seed, Workload)`, so you can
iterate on one failing case without re-running the whole suite:

- Proptest writes shrunk failures to `tests/<area>/tests.proptest-regressions`
  and replays them before any novel cases. To iterate on just those,
  filter by the failing test name and cap novel generation, e.g.:

  ```
  PROPTEST_CASES=1 cargo test --manifest-path cmd/unbounded-storage/Cargo.toml \
      --locked --all-targets --test dst \
      bufferpool::tests::invariant_single_flight_per_page
  ```

  The persisted regression(s) still replay; `PROPTEST_CASES=1`
  suppresses the rest of the sweep.

- For a one-off seed (e.g. from a failure not yet in the regressions
  file, or to bisect under a debugger), promote it to a hand-rolled
  `#[test] fn regression_*` next to the proptest that calls
  `run_workload(seed, w)` with the seed and `Workload` literal from
  the shrunk output. See `regression_freelist_deadlock_under_faults`
  in `tests/bufferpool/tests.rs` for the pattern. Keep the test
  committed once the bug is fixed so the case is a permanent guard.

### 4. End-to-end smoke test (`hack/smoke-storage.py`)

The smoke test is the only test that exercises the real, fully linked
binary across a process boundary. It brings up two `unbounded-storage`
processes on loopback, wires them together over the real libfabric `tcp`
fabric (file-backed disks, HTTP frontends, and a stub origin), then
fetches an object through both frontends so the second fetch is served
cross-node over a fabric RPC. It is the gate that catches integration
breakage the in-process Rust tests cannot: FFI/ABI mismatches against
the installed libfabric, provider negotiation, real socket addressing,
and the lazy-connect retry paths.

How to run it:

```
make unbounded-storage-build      # builds libfabric + the release binary
sudo -E env "PATH=$PATH" \
  "LD_LIBRARY_PATH=$PWD/tmp/libfabric/<version>/lib" \
  python3 hack/smoke-storage.py
```

- `sudo` is required because the processes pin io_uring buffers and the
  harness raises `RLIMIT_MEMLOCK` (needs `CAP_SYS_RESOURCE`). `sudo`
  strips `LD_*` even with `-E`, so the libfabric runtime path must be
  re-applied explicitly via `env` as shown.
- Substitute the pinned `LIBFABRIC_VERSION` for `<version>`.

When to run it:

- After any change to the `fabric` module (`src/fabric/**`, especially
  `shim.c`, `ffi.rs`, provider/addressing/connection logic), to
  `src/main.rs` shard wiring, or to the libfabric version.
- After changing `hack/smoke-storage.py` itself.

CI runs it automatically via `.github/workflows/smoke-storage.yaml` on
changes under `cmd/unbounded-storage/**`, `hack/smoke-storage.py`, or the
`Makefile`. The unit/module/DST tests do not cover the FFI boundary
against a real provider, so do not treat a green `make unbounded-storage`
as sufficient for fabric-affecting changes; run the smoke test too.

### Adding a new subsystem

1. Add `pub mod <area>;` to `src/lib.rs` and create `src/<area>/mod.rs`
   with private submodules and a curated public re-export list.
2. For targeted tests of an individual construct, add a
   `#[cfg(test)] mod tests { ... }` block at the bottom of the file
   that defines it.
3. For user-facing scenarios that span multiple files in the module,
   add `#[cfg(test)] mod tests;` to `mod.rs` and a `tests.rs` sibling
   using the noop-waker `block_on` pattern.
4. For concurrent behavior, add `tests/<area>/` with `mod.rs`,
   `mocks.rs`, a workload module, optional `oracle.rs`, and `tests.rs`,
   then register it in `tests/dst.rs`.
5. Reuse `tests/framework/` as-is. If you find yourself wanting to add
   a subsystem-specific knob there, put it in the per-area `*SimCfg`
   instead.
