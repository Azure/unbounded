# Typed syscall argument handoff

At base a2a65379 the remaining boxed syscall arguments were:

- `runtime/reactor/filesystem.rs:234-257`: shared OpenHow input, captured after SQE construction.
- `runtime/reactor/filesystem.rs:280-301`: mutable statx output, captured after pointer extraction.
- `runtime/reactor.rs:1021-1036,1385-1446`: shared encoded sockaddr input, captured after SQE construction.

Paths are relative to `cmd/racer-dataplane/src/`. All three now use the private
`SyscallArg<T>` owner. It owns exactly one typed Vec element and exposes no resize
or pointee-reference API. Vec allocation preserves T's size/alignment, and its
raw-pointer accessors avoid reference materialization. Ownership still moves into
the existing reactor completion closure. The returned statx is extracted only
after successful completion. ABI types, flags, masks, lengths, quotas, errno
handling, and original/cancel CQE fences are unchanged.

The standard-library-only `syscall.rs` includes the actual production helper by
path. A repr(C), aligned aggregate stands in for each syscall pointee; there is
no FFI or kernel emulation. Box controls reproduce the old shared-input and
mutable-output derivation, followed by nested boxed completion ownership. The
fixed cases cover mutable output success/error and shared input cleanup.

Use an already prepared isolated toolchain/sysroot, with no installation:

```sh
timeout --signal=TERM --kill-after=10s 10s ls -ld tmp
timeout --signal=TERM --kill-after=10s 10s mkdir -p tmp/syscall-provenance/{rustup,cargo,cache,scratch,target}
timeout --signal=TERM --kill-after=10s 40s python3 hack/racer-box-provenance/syscall-run.py /home/azureuser/code/unbounded/tmp/racer-box-provenance-20261001/tmp/box-provenance
```

The old toolchain and sysroot are read only; all new logs/cache/temp paths are in
the current worktree's `tmp/syscall-provenance`. Each Miri invocation is bounded
to 10 seconds. The runner checks expected exit codes and UB diagnostics.

Observed with nightly-2025-11-21, rustc/Miri 1.93.0-nightly (53732d5e0 2025-11-20):

| Case | Stacked Borrows | Tree Borrows |
| --- | --- | --- |
| Box output | UB at write, tag removed by Unique retag | UB at completion reborrow, tag Disabled by foreign write |
| Box shared input | UB at read, tag removed by Unique retag | Pass |
| Production SyscallArg | Pass | Pass |

These models are experimental. The proof uses the actual owner but not the
production async reactor or actual syscall structs. Kernel integration tests
remain necessary for ABI/result/fence behavior. This item establishes no causal
link to AEAD rejects or historical CRC discrepancies.

## Production validation

On rustc 1.96.0 (ac68faa20 2026-05-25):

- `cargo check --locked --offline --all-targets` passed (default features).
- Dataplane `cargo fmt --all -- --check` and standalone fixture rustfmt passed.
- Focused library tests passed: **42 passed, 0 failed, 0 ignored** using filters
  `runtime::reactor::tests runtime::reactor::filesystem control::async_files`.
  These include real TCP/Unix connect, errno handling, original/cancel fences,
  abandoned open/stat lifetime and quota checks, and atomic file persistence.

Two earlier default-profile test builds exhausted bounded codegen attempts
without compiler errors or running tests. Validation then used environment-only
`CARGO_PROFILE_TEST_OPT_LEVEL=0 CARGO_PROFILE_TEST_DEBUG=0`; dependencies retain
their repository package profile. This completed compilation in 34.45 seconds
and tests in 1.86 seconds. No Cargo profile or dependency files were changed.
The optimized test profile was not validated to completion. Miri results from
the preceding phase were retained, not rerun.
