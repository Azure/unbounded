# Opt-in Box ownership-handoff diagnostic

This standalone, dependency-free Rust reduction tests aliasing models, not the
kernel or production dataplane. It is intentionally outside Cargo's production
tests because some cases must fail under Miri. Do not execute its invalid cases
as a native binary. It makes no cluster or load changes.

## Production correspondence

Paths below are relative to `cmd/racer-dataplane/src/` at base `5dacdaf1`:

- `peer/transport.rs:1075-1112`: `WireBuffer` owns a `Box<[u8]>` and exposes a slice.
- `peer/transport.rs:1497-1524`: receive loop transfers and recovers that owner.
- `http/connection.rs:84-105,562-580`: `BufferRange` slices the backing storage and
  transfers it to the reactor when there is no read-ahead data.
- `runtime/reactor.rs:692-725,747-761`: derive a raw receive pointer, then move the
  `InFlight` owner into the completion closure.
- `runtime/reactor.rs:628-668`: move that closure inside a boxed closure and retain
  it in the entry before publishing the SQE.

The reduction keeps a live allocation throughout, uses the same slice accessor
for both Box and Vec, moves nested owners into a boxed completion closure, then
performs a saved-pointer access before calling completion. Vec is a control, not
a proposed production fix. The read cases also derive their pointers from a
mutable slice; they are not an exact reduction of production's shared send accessor.
FDs, leases, admission, async state, CQEs, and io_uring are deliberately omitted.

## Isolated provisioning and execution

Run from the assigned worktree root. All generated files stay below its `tmp/`.
The existing rustup executable is used only as a program; its homes are overridden.
Verify the worktree path and parent directories before creating anything.

```sh
timeout --signal=TERM --kill-after=10s 10s ls -ld .
timeout --signal=TERM --kill-after=10s 10s mkdir -p tmp/box-provenance/{rustup,cargo,miri-cache,scratch,target}
timeout --signal=TERM --kill-after=10s 120s env \
  RUSTUP_HOME="$PWD/tmp/box-provenance/rustup" \
  CARGO_HOME="$PWD/tmp/box-provenance/cargo" \
  XDG_CACHE_HOME="$PWD/tmp/box-provenance/miri-cache" \
  TMPDIR="$PWD/tmp/box-provenance/scratch" \
  RUSTUP_AUTO_INSTALL=0 \
  /home/azureuser/.cargo/bin/rustup toolchain install nightly-2025-11-21 \
  --profile minimal --component miri,rust-src --no-self-update
timeout --signal=TERM --kill-after=10s 150s python3 hack/racer-box-provenance/run.py
```

The runner does not install a toolchain. Its first setup builds a Miri sysroot
and can download standard-library build dependencies into the isolated Cargo
home. Each command is printed; each case has a 10-second bound, setup has a
90-second bound. The outer timeout bounds the entire run and its children.
Logs and `results.txt` remain in `tmp/box-provenance/`. Unexpected exit codes,
missing diagnostics, and changes to the recorded matrix fail the runner.

## Observed results, 2026-10-01

Exact toolchain: `nightly-2025-11-21-x86_64-unknown-linux-gnu`.
Miri reports `rustc 1.93.0-nightly (53732d5e0 2025-11-20)`.
Edition 2024; default Stacked Borrows or explicit `-Zmiri-tree-borrows`.

| Case | Stacked Borrows | Tree Borrows |
| --- | --- | --- |
| Box, write after handoff | UB, exit 1 | UB, exit 1 |
| Box, read after handoff | UB, exit 1 | Pass |
| Box, write before handoff | Pass | Pass |
| Vec, write after handoff | Pass | Pass |
| Vec, read after handoff | Pass | Pass |

Stacked Borrows write diagnostic:

```text
Undefined Behavior: attempting a write access using <1818> at alloc788[0x1], but that tag does not exist in the borrow stack for this location
<1818> was created by a SharedReadWrite retag at offsets [0x1..0x2]
<1818> was later invalidated at offsets [0x0..0x3] by a Unique retag (of a reference/box inside this compound value)
```

The invalidation is attributed to `submit(move || { ... })`; the rejected access
is `ptr.write(7)`. The read case gives the analogous read-access diagnostic.

Tree Borrows write diagnostic:

```text
Undefined Behavior: reborrow through <1769> at alloc788[0x1] is forbidden
the accessed tag <1769> has state Disabled which forbids this reborrow (acting as a child read access)
the accessed tag <1769> later transitioned to Disabled due to a foreign write access at offsets [0x1..0x2]
```

Tree Borrows attributes the disabling write to `ptr.write(7)` and reports the
violation later, when invoking the boxed completion closure (`move || finish()`).
Tag/allocation numbers are diagnostic details, not stable test expectations.

## Interpretation and limits

The receive-like ordering is a reproduced aliasing violation under **both tested
models**, despite stable allocation and retained ownership. This is stronger
evidence than documentation alone. Both models remain experimental, as the
diagnostics explicitly state; this does not establish a model-independent Rust
language ruling or actual production compiler miscompilation.

This fixture is not an execution of production code, does not model kernel
pointer exposure, and does not validate all cancellation or completion behavior.
The next narrower integration step is a Miri-compatible production simulator
receive using `WireBuffer` and `BufferRange`; the simulator dereferences its saved
pointer at `runtime/reactor/simulation.rs:1453-1459`. Keep unrelated unsupported
FFI outside that test. The original diagnostic commit contains no production fix;
the subsequent backing change is described below.

There is no demonstrated causal link to AEAD failures or the historical
matched-envelope send/receive CRC discrepancy.

## Fixed movable backing follow-up

Production movable IoBuffer storage now uses private Vec backing in WireBuffer,
OwnedBuffer, PlaintextBuffer, filesystem Buffer, and telemetry Buffer. The reactor
test Buffer follows the same rule. BufferRange delegates; AlignedBuffer and its
test View retain their existing managed allocation. Constructor normalization
through `into_boxed_slice().into_vec()` preserves the previous exact capacity
before pointers are derived. Plaintext converts to Box only at completed export.
Slice zeroization preserves pooled lengths; no in-flight resize API was added.

`lifecycle.rs` additionally checks success, error, canceled completion, and an
abandoned reply using the fixed Vec handoff shape. Both tested models pass. Its
ordinary fill models destructor access, not cryptographic zeroization; production
continues using zeroize or reservation recycling. The test models the point after
CQE fencing, not the CQE protocol itself. Existing production reactor tests cover
that protocol. The production-buffer subrange regression uses actual constructors
for HTTP, wire, plaintext, and filesystem buffers.

Validation of the backing change:

- The focused production group (reactor tests, HTTP connections, memory pool,
  wire checkout, telemetry, and filesystem buffers) passed 116 tests with zero
  failures; two preexisting opt-in benchmarks were ignored.
- `cargo fmt --all -- --check` passed for the dataplane; standalone fixtures also
  passed rustfmt checking.
- `cargo check --locked --offline --all-targets --features rdma,subscription-interop`
  passed. Optional heap-profiling was not enabled.
- The Miri lifecycle passes use the exact pinned version above. Production tests
  and target checks use rustc 1.96.0 (ac68faa20 2026-05-25).
