# Racer initialized buffer checkout

## Scope and safety

One item based directly on `aba97ad02367259fe2a609d7256795ca22a2fb7f`, in
`tmp/racer-buffer-checkout`. No parent integration, push, deployment, or cluster
operation. Read the supplied `~/design.md`, repository `AGENTS.md`, and
`tmp/racer-stage15-results.md`. The latter's sampled reactor recycling/copy costs
are motivation, not evidence of a fleet-wide bottleneck or this patch's gain.

- `cmd/racer-dataplane/src/runtime/admission.rs:85-126`: retain the deployed
  `clear` plus `Vec::zeroize` full-capacity secure wipe, before every rejection
  and deallocation path. Only after the wipe and retention checks, set the
  vector's length to its initialized capacity. The locked zeroize 1.9.0
  implementation (`src/lib.rs:520-537`) writes the entire spare capacity;
  its `MaybeUninit` slice implementation (`src/lib.rs:425-447`) writes zeros,
  not merely an optimization barrier. Zero is valid initialized `u8` data.
  No allocation or mutation intervenes before `set_len`. No uninitialized
  slice is constructed. This unsafe step depends on that full-capacity wipe
  contract; a future dependency or wipe change must preserve it.
- `cmd/racer-dataplane/src/runtime/admission.rs:127-148`: exact-capacity
  checkout returns those initialized bytes without another `resize` zero-fill.
  Fresh allocations still use fallible reserve and safe zero-initialization.
  Retention stays capped at two slots with the original admission, cache
  accounting, stop, contention, size, and pressure-reclamation checks.
- `cmd/racer-dataplane/src/peer/transfer.rs:25-47`: both `WireBuffer`
  constructors use reservation checkout rather than fresh `vec![0; length]`.
  Bounds/class checks, boxed stable backing, `into_parts` ownership transfer,
  crypto ownership, and I/O completion fences are unchanged. Successful page
  owners still perform the existing final recycle. This does not add recycling
  to abandoned WireBuffers or change their existing destructor behavior.

## Single before/after microbenchmark pair

Rust 1.96.0, release profile, all features, AMD EPYC 9V74 VM. One sequential
before run and one after run, not CPU-pinned or isolated from host activity.
Baseline is exact base production code plus the new ignored wire benchmark.
Both revisions use identical harnesses: 128 operations/sample, one warmup
sample discarded, five measured samples per case, one test thread. Each cycle
includes admission, checkout, full payload fill, black-box observation, secure
recycle, and reservation drop. Wire cases explicitly transfer `into_parts` to
recycle, modeling the successful final payload-owner path, not network I/O.

Median microseconds per operation (negative change means less time):

| Path | Size | Before | After | Change |
| --- | --- | ---: | ---: | ---: |
| Wire new | 1 MiB | 166.754 | 155.887 | -6.52% |
| Wire reserved | 1 MiB | 167.013 | 155.193 | -7.08% |
| Wire new | 16 MiB | 2681.950 | 2513.447 | -6.28% |
| Wire reserved | 16 MiB | 2691.406 | 2676.697 | -0.55% |
| Wire new | 16 MiB+16 | 2689.800 | 2578.818 | -4.13% |
| Wire reserved | 16 MiB+16 | 2693.700 | 2625.794 | -2.52% |
| Reservation pooled | 1 MiB | 167.196 | 172.461 | +3.15% |
| Reservation pooled | 16 MiB | 2733.433 | 2609.747 | -4.52% |
| Reservation pooled | 16 MiB+16 | 2746.073 | 2547.715 | -7.22% |
| Non-pooling control | 1 MiB | 168.309 | 186.693 | +10.92% |
| Non-pooling control | 16 MiB | 2735.669 | 2755.838 | +0.74% |
| Non-pooling control | 16 MiB+16 | 2736.758 | 2775.449 | +1.41% |

The 1 MiB pooled regression and unchanged-path control drift limit attribution.
The reserved 16 MiB after samples ranged from 2556.223 to 2782.632 us. These
observations support removal of redundant work, not a reliable percentage
prediction for production. No fleet, NIC, crypto, or end-to-end gain claim.
No benchmark reruns were performed.

Command, once per revision (all commands wrapped in
`timeout --signal=TERM --kill-after=10s 300s`):

```sh
cargo test --locked --release --manifest-path cmd/racer-dataplane/Cargo.toml \
  --all-features --lib -j2 -- --ignored --nocapture --test-threads=1 \
  payload_recycle_benchmark wire_checkout_benchmark
```

Raw output is retained in the worktree's ignored
`before-buffer-benchmark.log` and `after-buffer-benchmark.log`.

## Focused regression evidence

- All-feature test-profile library selection: **20 passed**, two opt-in
  benchmarks ignored. Includes four new tests for spare-capacity initialization,
  exact geometry/charge checks, two-slot retention/pressure reclamation, both
  wire constructors, cross-cache/class reuse, stable pointer/charge transfer,
  and rejection. Existing assertions cover truncated tails, cache fairness,
  foreign reservation rejection, immutable final-owner lifetime, actual HTTP
  fragmentation/truncation, and both cancellation-completion orders.
- Existing `tests/payload_zeroization.rs`, all-feature **release** integration:
  **1 passed**, unchanged. Allocator-observed full-capacity wiping covers
  plaintext/ciphertext, truncation, retained/small/full/stopped/destroyed pools,
  cross-thread final ownership, and failed/canceled crypto outputs.
- `cargo fmt --check`, `git diff --check`, and scoped `make fmt` with
  `GOTOOLCHAIN=go1.26.6 GO_PACKAGE_PATTERNS=./internal/unbounded/...` passed;
  no Go source changes. No broad suite repeated.

```sh
cargo test --locked --manifest-path cmd/racer-dataplane/Cargo.toml \
  --all-features --lib -j2 -- --test-threads=2 \
  runtime::admission::tests memory::pool::tests peer::transfer::tests \
  received_page_keeps_one_charge real_http_ciphertext_fragmentation \
  immutable_ciphertext_send_shares abandoned_resources_wait_for_both_fences \
  scheduled_short_io_disconnect
cargo test --locked --release --manifest-path cmd/racer-dataplane/Cargo.toml \
  --all-features --test payload_zeroization -j2 -- --test-threads=1
```

Logs: `focused-buffer-tests.log` and `payload-zeroization-tests.log`, ignored
within the worktree. No sanitizer/Miri, allocator-failure injection, full-stack
load, or fleet validation was performed. No subagent facility was available;
no delegation occurred.
