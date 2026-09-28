# Racer payload recycle zeroization

## Scope and evidence

Base: `43f251da159615c2832d4e726d4654e011fc7882`. One production change:
clear the length of the exclusively owned `Vec<u8>` before its existing
`zeroize()` call in `cmd/racer-dataplane/src/runtime/admission.rs:85-92`.

The existing saved Parca PPROF was decoded locally with:

```sh
timeout --signal=TERM --kill-after=10s 30s go tool pprof -top -nodecount=25 \
  tmp/parca-recovery-targeted-final-racer-io-1.pprof
```

Node `aks-ddv5-17198779-vmss0000ad`, requested window
2026-09-28 10:30:40-10:31:40 UTC: 59.3158 sampled CPU seconds, with
`Reservation::recycle` 24.31% flat / 27.24% cumulative. The saved request/range
timestamps are authoritative; exported pprof time metadata is incorrect.
Parent artifact `tmp/parca-recovery-results.md:161-178` records the four threads:
reactor 0 at 98.51% of a core, reactor 1 at 98.86%, crypto threads at 2.98% each.
These are observations of the stage-5 workload, not an optimization A/B.

The decoded reactor-1 profile also has `_copy_to_iter` 10.03% flat,
`_copy_from_iter` 5.50%, and `clear_page_erms` 5.32%. Those kernel copy/allocation
paths are separate costs; this change only addresses the recycle hotspot.
No application configuration, collector configuration, image, deployment, or
cluster workload was changed during this work.

## Why the wipe remains secure

The locked zeroize 1.9.0 implementation's `Vec<Z>::zeroize` first zeroizes
initialized elements, clears the vector, then zeroizes its entire spare capacity
(`zeroize-1.9.0/src/lib.rs:520-537`). Its documented guarantee is that the entire
capacity is zeroed. See the [locked dependency source](https://docs.rs/zeroize/1.9.0/src/zeroize/lib.rs.html#520-537).
The spare-capacity implementation uses volatile byte writes and an optimization
barrier (`:434-447`). Thus an initialized `Vec<u8>` prefix was wiped twice.

`Vec<u8>::clear()` has no element destructors, allocations, callbacks, or payload
copies. It makes the redundant element pass empty. `zeroize()` still securely
wipes every capacity byte, including truncated and uninitialized spare tails,
before any pool eligibility check or deallocation (`admission.rs:91-109`).
There is no new unsafe production code and no ordinary memset substitution.
The full-capacity guarantee, rather than a length-only slice wipe, is essential.

Final plaintext staging, verified plaintext, and ciphertext owners all call
recycle (`src/memory/pool.rs:34-78`). Their ownership and reservation transfer
are preserved. The existing two-slot bound, charge retention, exact-capacity
reuse, stop recheck, and allocation initialization remain in
`admission.rs:93-143`; the existing reuse test still checks pointer identity,
zero bytes, and quota release. Idle page retention and completion ownership
match `designs/racer-throughput-phase-4.md:91-99`.

CRC, AEAD, and I/O/crypto thread handoffs remain governed by
`src/security/aead.rs:171-250` and `src/runtime/crypto.rs:59-78`. The optimization
needs no new queue, asynchronous cleanup, or ownership transfer. It preserves
the thread-per-core and retained-buffer design in `/home/azureuser/design.md`.

## Correctness regressions

- `runtime::admission::tests::recycled_truncated_capacity_is_zero_before_cross_cache_and_class_reuse`
  examines idle backing bytes before allocation can overwrite them, for lengths
  0, 1, capacity minus 16, and full capacity. It then reuses the same allocation
  across cache IDs and ciphertext/plaintext classes and checks charge transfer
  and final reclamation.
- `tests/payload_zeroization.rs` observes a watched allocation immediately before
  `System.dealloc`, while the allocation is still valid. It verifies the complete
  initialized capacity, including a nonzero truncated ciphertext tail. It covers
  plaintext and ciphertext final drops on another thread, retained-pool reclaim,
  too-small rejection, full-pool rejection, stopped admission, and destroyed
  admission. A ciphertext clone remains readable until final ownership release.
  The test passes on the base implementation and the optimized implementation.

## Same-workload local benchmark

Host: AMD EPYC 9V74, Linux x86-64, rustc/cargo 1.96.0, release profile, all features,
system allocator, CPU 2 affinity. No other test suite ran during either benchmark.
The benchmark was first added and executed against unchanged base production
code, then executed again after the one-line production change. Its code and
command were identical in both runs:

```sh
timeout --signal=TERM --kill-after=10s 300s taskset -c 2 cargo test \
  --locked --all-features --release --lib payload_recycle_benchmark -- \
  --ignored --exact runtime::admission::tests::payload_recycle_benchmark \
  --nocapture --test-threads=1
```

Each case has 128 warmup operations and five samples of 128 operations each.
Every operation reserves capacity, obtains an initialized buffer, fills it with
nonzero bytes, recycles it, and drops the reservation. Retained cases exercise
reuse; stopped-admission cases use completion admission and exercise fresh
allocation and non-pooling destruction. Times include all of this work.

| Bytes | Retain | Base median ns/op | Changed median ns/op | Reduction |
| ---: | :---: | ---: | ---: | ---: |
| 1,048,576 | yes | 455,250 | 167,191 | 63.3% |
| 1,048,576 | no | 455,110 | 166,692 | 63.4% |
| 16,777,216 | yes | 7,417,624 | 2,731,172 | 63.2% |
| 16,777,216 | no | 7,495,124 | 2,749,009 | 63.3% |
| 16,777,232 | yes | 7,436,168 | 2,748,079 | 63.0% |
| 16,777,232 | no | 7,355,964 | 2,740,957 | 62.7% |

Raw samples in table order, nanoseconds/operation:

```text
base:
455939 454997 455250 454732 457326
453916 455110 456156 455707 454699
7464951 7401702 7417624 7398746 7418894
7511313 7495124 7474322 7501017 7448315
7480236 7444136 7310489 7436168 7428660
7340663 7414696 7320591 7355964 7399603
changed:
168968 170017 166982 167191 166557
166649 166692 166723 166391 167260
2712734 2723088 2731172 2745950 2733578
2740850 2730201 2749009 2749853 2775592
2730402 2749234 2748079 2761506 2746225
2744814 2740957 2851985 2733236 2736543
```

Safe claim: about 63% lower elapsed time for these local buffer lifecycle cases.
The live profile selects the optimization target; it does not establish how much
of live recycle time is redundant wiping. No cluster throughput, latency, or
reactor CPU reduction is claimed. This change has not been deployed.

## Validation

Every test/benchmark command used external
`timeout --signal=TERM --kill-after=10s 300s`; no timeout fired. Rust commands
ran in `cmd/racer-dataplane`, with `--locked --all-features` and compilation
parallelism `-j 8`. Full suites ran once in bounded groups after focused checks.

| Check | Result |
| --- | --- |
| Release admission group | 7 passed, benchmark explicitly ignored |
| Release allocator-observation regression, base and changed | Passed both |
| `cargo check --all-targets` | Passed |
| Library/binary suite, excluding DST/contention group | 748 + 2 passed, 8 explicit library ignores |
| Library DST/contention group, serial | 43 passed, verified 16 GiB/no-swap cgroup |
| Ordinary production/conformance/zeroization/process integration | 15 + 19 + 1 + 2 passed; 2 + 1 + 0 + 15 explicit ignores |
| Doctests | 32 passed, including 26 compile-fail contracts |
| Explicit privileged process suite, strict baseline | 15 passed, verified 16 GiB/no-swap cgroup |
| `cargo fmt --check`, `git diff --check` | Passed |
| Scoped `make fmt`, `GOTOOLCHAIN=go1.26.6` | Passed, zero issues; three unrelated blank-line autofixes reverted |

The privileged suite used the existing memory-safe wrapper and
`CARGO_TARGET_X86_64_UNKNOWN_LINUX_GNU_RUNNER='sudo -n env RACER_THROUGHPUT_STRICT_BASELINE=1'`.
It executes local applications with fixture-owned state, not the live cluster.
Initial scoped Go formatting attempts failed due to the ambient Go 1.27/linter
toolchain mismatch; selecting the compatible installed Go 1.26.6 resolved it.
