# Racer I/O CPU: bounded wipe and delivery optimizations

## Profile and scope (2026-09-30)

The recorded Parca request used context `joolshev-scale-test`, node
`aks-ddv5-17198779-vmss0000ad`, `comm="racer-dataplane"`, and
`thread_name=~"racer-io-.*"`. The merge window was **00:09:30Z-00:10:20Z** on
2026-09-30, report `REPORT_TYPE_PPROF`, with **80.53 sampled CPU seconds**.
Profile identifier: `8b41dffa48294bac5f9a94f054051d8f62769578`.
The saved request and profile are
`tmp/parca-recovery-io-cpu-initial.{json,pprof}`; no cluster operation was performed
for the combined validation below.

| Attribution | Flat CPU | Cumulative CPU |
| --- | ---: | ---: |
| `Reservation::recycle` | 17.84% | 19.35% |
| `clear_page_erms` | 9.93% | not used here |
| `_copy_to_iter` | 10.39% | not used here |
| `_copy_from_iter` | 7.84% | not used here |
| `OwnedBuffer` drop | 1.76% | not used here |

These samples motivated two narrow changes, not a delivery architecture redesign:

- **Wipe** (`eb956f29`, integrated on parent as `d6d89ed9`): Linux GNU/musl
  `wipe_payload` calls non-elidable `libc::explicit_bzero` across the allocation's
  entire capacity, including truncated/spare bytes. Other targets retain
  `clear` plus `Vec::zeroize`. Zero capacity skips FFI. Wiping still precedes
  every rejection/retention path; two-slot admission and initialized checkout
  remain unchanged (`cmd/racer-dataplane/src/runtime/admission.rs:83-157`).
- **Delivery** (`012189e2`, integrated on parent as `c6e1e6d8`): after the first
  pipe drain, a backpressured send owns an immutable `VerifiedPage` subrange,
  avoiding another staging allocation, zero-fill, payload copy, and staging
  wipe. The reactor still owns the full reader/page/pipe/connection lease through
  cancellation and completion; short sends advance only the accepted cursor
  (`cmd/racer-dataplane/src/memory/delivery.rs:36-70,277-338`,
  `cmd/racer-dataplane/src/runtime/reactor.rs:367-380,844-877`). No mutable
  receive capability or userspace-page splice was introduced.

## Local measurements, not cluster speedup

Both experiments used Rust 1.96.0 on the same unpinned AMD EPYC 9V74 host;
the wipe checkpoint records glibc 2.39 and allowed CPUs 0-47. Host activity was
not isolated. Measurements were completed before integration and **not rerun**
for this report. Do not add their percentages or extrapolate them to node CPU,
NIC throughput, or end-to-end latency.

| Workload | Before median | After median | Interpretation |
| --- | ---: | ---: | --- |
| 16 MiB pooled payload lifecycle, release/all features | 2,551,317 ns/op | 368,704 ns/op | Includes fill, admission, checkout, recycle, and drop; not wipe alone |
| TCP backpressured delivery, 512 MiB/sample, optimized test profile | 215.976 ms | 152.136 ms | Sender-thread CPU, 29.6% lower |

The wipe lifecycle used five measured samples. Its separate alternating
same-binary fill+wipe comparison measured 16 MiB medians of 2,520,472 ns for
scalar zeroize and 369,324 ns for explicit wiping. Release disassembly showed
the old byte-store loop replaced by `explicit_bzero@GLIBC_2.25`, before pool
checks and deallocation. Allocator-observed tests check the full allocation
before free, including failed/canceled crypto output and cross-thread owners
(`cmd/racer-dataplane/tests/payload_zeroization.rs:70-173,218-340`).

Delivery used one warmup and three measured samples, each 1,024 deliveries of
512 KiB, real TCP loopback, 4 KiB `SO_SNDBUF`, and receiver quick ACK/pacing.
`CLOCK_THREAD_CPUTIME_ID` surrounded sender future/reactor turns, excluding
receiver reads, validation, pacing, and work on other threads. Exact bytes and
HTTP framing were checked. Before CPU samples: 216.417, 215.976, 209.637 ms;
after: 152.136, 161.728, 143.223 ms. Every measured sample had 1,024 pipe drains
and 10,240 pending turns. Harness:
`cmd/racer-dataplane/src/memory/delivery.rs:700-807`.
An initial compile-plus-benchmark command timed out before fixture tuning;
those incomplete samples are excluded, as recorded in the delivery checkpoint.

Kernel socket copies and page allocation/clearing remain. The profile review
attributes kernel clearing to socket-fragment allocation; it does not justify
changing unrelated disk/head pools. First pipe drains and final secure payload
wipes also remain. Signing canonicalization is a **separate candidate under
measurement**, not a completed improvement in this report. No deployment or
post-change cluster profile establishes a production gain.

## Combined validation and outstanding failures

Combined source state: delivery `012189e2` plus wipe cherry-pick `0c2cad26` in
the exclusive delivery worktree. The parent's equivalent source commits are
already integrated; only this documentation commit should be cherry-picked.

Commands ran from `cmd/racer-dataplane`, each separately wrapped with
`timeout --signal=TERM --kill-after=10s 300s` and logged with `pipefail`/`tee`:

```sh
cargo test --offline --locked --all-features --lib -j 2 -- --test-threads=2 --nocapture
cargo test --offline --locked --all-features --test payload_zeroization -j 2 -- --test-threads=1 --nocapture
cargo check --offline --locked --all-features --all-targets -j 2
```

- Library: **916 passed, 2 failed, 18 ignored**, 105.52 seconds after a
  109-second build. This is **not a green combined suite**.
- Allocator-observed zeroization integration: **1 passed**, no skips.
- All-feature/all-target compile check: **passed**.
- Real io_uring was exercised: the mandatory real-ring listener retry test
  passed (`src/runtime/reactor.rs:2285-2329`), as did delivery short-send and
  cancellation tests. The uncaptured-output log contains no io_uring-unavailable
  messages; the kernel helper would report ENOSYS/EPERM/EACCES skips
  (`src/runtime/reactor.rs:1464-1482`). The 18 ignored benchmark/hardware tests
  were not exercised, and no RDMA hardware validation is claimed.

Both failures reproduced in a single-threaded, two-test selection (60-second
external bound; 0 passed, 2 failed, 934 filtered, 0.01 seconds):

1. `app::peer_tests::assembled_peer_io_rejects_oversize_and_admission_pressure_before_submission`
   expects `Overloaded`, receives `Ok(())` at `src/app_peer_tests.rs:385`.
   The test budgets just below two maximum heads (`:355`), whereas
   `src/http/io.rs:440-462` reserves one maximum scratch plus actual encoded
   staging. A stale expectation is an inference, not a baseline execution result.
2. `read::fill::integration_tests::peer_copies::unusable_peer_copy_advances_to_alternate_or_authorized_origin`
   fails the copy-mode/four-link/strict-child-deadline assertion at
   `src/read/fill_peer_tests.rs:262-267`. Cause remains unresolved.

Those test files, HTTP head code, and candidate policy are unchanged from
`31fddc58`; this alone does not prove the failures predate these optimizations.
No source edits, relaxed assertions, or suppressed failures were made during
integration. Parent coordination is required before repairing either contract.
Earlier focused validation passed 141 delivery-adjacent tests; the wipe phase
also passed release allocator observation. Neither substitutes for the broad
suite's unresolved failures.

## Evidence retained locally

- Original checkpoints: `tmp/racer-io-checkpoint.md`,
  `tmp/racer-io-wipe-checkpoint.md`, `tmp/racer-io-delivery-checkpoint.md`,
  `tmp/racer-io-integration-checkpoint.md` (commands and phase outcomes).
- Combined logs: `tmp/racer-io-integration-{lib,zeroization,check,failures}.log`.
- Wipe raw measurements/codegen: `.worktrees/racer-io-cpu/tmp/wipe-before.log`,
  `wipe-after.log`, `wipe-before-codegen.log`, `wipe-libc-codegen.log`,
  `wipe-after-address-probe.log`, and `wipe-after-dynamic.log` in that directory.
- Delivery measurement samples and initial timeout/recovery are preserved in
  the original delivery checkpoint; its ignored loopback harness is committed.

These ignored local artifacts are evidence pointers, not promised packaged
release assets. The source-linked contracts and summarized outcomes above are
the durable record. See [deployment requirements](../cmd/racer-dataplane/DEPLOYMENT.md)
for custom GNU runtime compatibility.
