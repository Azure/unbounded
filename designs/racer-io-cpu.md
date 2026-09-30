# Racer I/O CPU: bounded wipe, delivery, and signing optimizations

## Profile and scope (2026-09-30)

The recorded Parca request used context `joolshev-scale-test`, node
`aks-ddv5-17198779-vmss0000ad`, `comm="racer-dataplane"`, and
`thread_name=~"racer-io-.*"`. The merge window was **00:09:30Z-00:10:20Z** on
2026-09-30, report `REPORT_TYPE_PPROF`, with **80.53 sampled CPU seconds**.
Executable build ID: `8b41dffa48294bac5f9a94f054051d8f62769578`.
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
| `signature_base` | not used here | 3.37 s (4.18%) |

These samples motivated three narrow changes, not an architecture redesign:

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
- **Signing** (`8a4590d8`): `signature_base` computes validated canonical
  components once, then uses them for signature-input formatting and base lines
  (`cmd/racer-dataplane/src/security/signing.rs:230-270`). Previously it computed
  them once through `signature_input` and again for lines. The profile had a
  separate duplicate-components child of 0.74 s. This is local per-call reuse,
  not caching; validation order and canonical wire bytes are preserved.

## Local measurements, not cluster speedup

All three experiments used Rust 1.96.0 on the same unpinned AMD EPYC 9V74 host;
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

Signing measured **base construction only**, not cryptographic signing, MACs,
certificate verification, or transport. Release/all-feature runs used 2,000
calls per sample, discarded one warmup, and reported medians of five measured
samples per request/response case. One sequential before/after pair ran
unpinned, without host isolation; the large-head cases showed host variance.

| Canonical base case | Request before -> after (ns/op) | Response before -> after (ns/op) |
| --- | ---: | ---: |
| Small, 7 fields | 3,918 -> 2,911 (25.7% lower) | 3,536 -> 2,645 (25.2% lower) |
| 31 fields, fixture values 64 bytes | 23,921 -> 18,969 | 23,690 -> 18,570 |
| 31 fields, fixture values 4,096 bytes, approximately 100 KiB | 149,114 -> 85,463 | 156,081 -> 84,007 |

Harness and rejection matrix:
`cmd/racer-dataplane/src/security/signing.rs:430-543`. Exact RFC request/response
vectors and reordered/mixed-case fields passed before and after, along with
13 invalid-input classes. The independent release security suite passed 66
tests with three opt-in tests ignored. These construction gains cannot be
applied directly to the entire 4.18% cumulative stack or to fleet throughput.

## Stop decision

Kernel socket copies and page allocation/clearing remain. The profile review
attributes kernel clearing to socket-fragment allocation; it does not justify
changing unrelated disk/head pools. First pipe drains and final secure payload
wipes also remain. After these three measured changes, no further small,
well-supported hotspot was identified in the sampled I/O work. Remaining kernel
transport and unresolved libc allocation costs do not justify an architecture
redesign on this evidence. Stop this optimization pass. No deployment or
post-change cluster profile establishes a production gain.

## Combined validation and outstanding failures

The initial broad validation used delivery `012189e2` plus wipe cherry-pick
`0c2cad26` in the exclusive delivery worktree. Signing was subsequently added
as `3bbb1c26` (source `8a4590d8`) for focused combined security validation.
These local integration cherry-picks are not new source changes for the parent;
only the new documentation commit should be taken from this follow-up.

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
   staging. A stale staging-size expectation is the likely explanation; the
   exact same assertion also fails in the executed baseline below.
2. `read::fill::integration_tests::peer_copies::unusable_peer_copy_advances_to_alternate_or_authorized_origin`
   fails the copy-mode/four-link/strict-child-deadline assertion at
   `src/read/fill_peer_tests.rs:262-267`. Cause remains unresolved.

### Baseline attribution and final focused integration

An owned detached worktree at **`31fddc58`**, before either wipe or delivery
optimization, executed only those same two tests with all features and one
test thread. Both failed at the identical assertion lines with matching output:
**0 passed, 2 failed, 929 filtered**, 0.01 seconds after a 106-second build.
Thus these two failures are **confirmed baseline failures**, not newly introduced
by the optimizations. This does not identify the exact cause of the compound
peer-probe assertion. The baseline worktree was clean and removed without force.
It reused the delivery target directory sequentially, with no concurrent Cargo.

After signing integration, the focused combined security suite passed
**66 tests, 0 failed, 3 ignored**, 1.55 seconds after a 105-second rebuild.
The ignored tests were two explicit hardware CRC cases and the signature-base
benchmark. The broad suite and all prior benchmarks were not rerun.

Both commands were bounded separately by
`timeout --signal=TERM --kill-after=10s 300s`:

```sh
# From the detached baseline crate; reuse the absolute delivery target directory.
cargo test --offline --locked --all-features --lib -j 2 \
  --target-dir /home/azureuser/code/unbounded/.worktrees/racer-delivery-io/cmd/racer-dataplane/target \
  -- --test-threads=1 --nocapture \
  assembled_peer_io_rejects_oversize_and_admission_pressure_before_submission \
  unusable_peer_copy_advances_to_alternate_or_authorized_origin
# From the combined delivery worktree crate, after signing integration.
cargo test --offline --locked --all-features --lib -j 2 security:: \
  -- --test-threads=1 --nocapture
```

No out-of-scope source edits, relaxed assertions, or suppressed failures were
made. The broad suite remains non-green due to the baseline failures; any repair
belongs to separately coordinated work. Earlier validation passed 141
delivery-adjacent tests and release allocator observation for wiping.

## Evidence retained locally

- Original checkpoints: `tmp/racer-io-checkpoint.md`,
  `tmp/racer-io-wipe-checkpoint.md`, `tmp/racer-io-delivery-checkpoint.md`,
  `tmp/racer-io-signing-checkpoint.md`, `tmp/racer-io-integration-checkpoint.md`
  (commands and phase outcomes).
- Combined logs: `tmp/racer-io-integration-{lib,zeroization,check,failures,baseline,security}.log`.
- Parent archive destination for wipe raw measurements/codegen:
  `tmp/racer-io-evidence/wipe-signing/wipe-before.log`,
  `wipe-after.log`, `wipe-before-codegen.log`, `wipe-libc-codegen.log`,
  `wipe-after-address-probe.log`, and `wipe-after-dynamic.log` in that directory.
- Signing raw output in the same archive directory: `signing-before.log`,
  `signing-after.log`, and `signing-security.log`.
- Parent archive destination for delivery worktree temporary evidence:
  `tmp/racer-io-evidence/delivery/`.
- Delivery measurement samples and initial timeout/recovery are preserved in
  the original delivery checkpoint; its ignored loopback harness is committed.

The parent owns copying both worktrees' temporary evidence into these archive
destinations before cleanup; this report does not assert that archival has
already completed. Original signing/wipe logs reside under
`.worktrees/racer-io-cpu/tmp/` until that copy.

These ignored local artifacts are evidence pointers, not promised packaged
release assets. The source-linked contracts and summarized outcomes above are
the durable record. See [deployment requirements](../cmd/racer-dataplane/DEPLOYMENT.md)
for custom GNU runtime compatibility.
