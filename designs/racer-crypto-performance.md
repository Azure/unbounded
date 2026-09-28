# Racer integrated CRC and AEAD measurements

## Scope and reproduction

The integrated implementation uses accelerated CRC-64/XZ and RustCrypto's
detached immutable-input XChaCha20-Poly1305 API. Record v4 is the sole supported
record format; old CRC variants and v1-v3 records are deliberately rejected,
not migrated or tried as fallback (`cmd/racer-dataplane/src/store/format.rs:19-20`
and `RecordCodec::parse`). Page AAD, 24-byte nonces, and ciphertext-plus-16-byte-tag
remain unchanged (`cmd/racer-dataplane/src/security/aead.rs:44-68,201-229`).
Production execution hooks still surround the full page-engine operation
(`cmd/racer-dataplane/src/security/aead.rs:175,320`).

From the repository root, run the explicitly ignored test:

```sh
timeout --signal=TERM --kill-after=10s 300s env \
  CARGO_BUILD_JOBS=2 RUST_TEST_THREADS=2 \
  bash hack/scripts/memory-safe-run.sh -- \
  cargo test --locked --release \
    --manifest-path cmd/racer-dataplane/Cargo.toml --lib \
    runtime::crypto::measurement::crypto_measurement -- \
    --ignored --exact --nocapture --test-threads=2
```

The test has no workload environment knobs. It rejects debug builds. Its fixed
matrix is 63 bytes, 4,095 bytes, 1 MiB, and 16 MiB; reused and rotating sources;
CRC, complete detached AEAD encryption/decryption, and bad-tag failure; direct
page-engine lifecycle and real two-thread handoff at a maximum of eight admitted
operations. Each output line prefixed `CRYPTO_MEASURE ` contains a JSON object.
`CRYPTO_PROVENANCE ` includes architecture, CPU model/features, compiler, revision,
dirty state, Cargo.lock hash, caller affinity, profile, and ambient RUSTFLAGS.
Keep the wrapper's cgroup limit diagnostics with the output. Measurement adds no
dependency or standalone observability framework; crypto dependencies are listed
below.

Primitive buffers, cipher, AAD, and tags are prepared before the clock starts.
Decryption includes copying ciphertext into preallocated scratch (named
`decrypt_copy`); bad-tag also includes that copy. Both still perform the complete
AEAD operation including authentication, not a stream-cipher proxy. Repeated
encryption mutates the same preallocated input. Fixed primitive nonces are for
measurement only; the page engine uses its real fresh-nonce path. Migration from
`AeadInPlace` to `AeadInOut` preserves these original 80 cases: the primitive
`encrypt` still aliases input/output, and `decrypt_copy` and `bad_tag` still copy
before an aliased detached operation. They are not mislabeled as the new
production out-of-place path.

For the separate 24-case primitive supplement, use the same command with the
exact test name `runtime::crypto::measurement::crypto_measurement_inout`.
`encrypt_inout`, `decrypt_inout`, and `bad_tag_inout` borrow immutable input and
write to one preallocated scratch buffer, with **no preliminary payload copy**.
Input rotation and iteration counts match the original matrix, but the write
working set differs from aliased encryption. Neither primitive matrix includes
allocation, clearing, or final zeroization in its clock. Do not subtract these
separately run timings to claim an isolated copy cost.

Lifecycle timing includes input allocation/copy, admission, key leasing, permit
reservation, real `PageCryptoEngine::process`, CRC, output destruction/recycling,
completion drain, thread join, and final recycler reclamation. Fixture construction
and fixture-source destruction are excluded. Engine execution itself is not
cleanup-inclusive: production output leases intentionally outlive that interval.
Paired execution moves owned jobs through the actual SPSC ports on a second OS
thread; it is bounded batches of eight, not a serial submit/wait disguised as
concurrency. This is not the full worker scheduler or a network/storage benchmark.

The rotating source span is 128 MiB, larger than the measured host's aggregate
96 MiB L3. Primitive cases traverse the entire span. Lifecycle 1/16 MiB cases also
traverse it. To keep the default bounded, small lifecycle cases sample up to 4,096
positions with approximately 32 KiB stride across that span: they **do not touch
128 MiB of payload**, and should not be described as a fully cache-cold small-page
sweep. `source_span_bytes` deliberately describes span, not measured cache misses.
Ciphertext fixtures include tag/allocator overhead beyond that span. On a host
with LLC >=128 MiB the default is not cache-exceeding; change the fixed constant
and record that revision for that experiment. Threads are unpinned, so this does
not establish NUMA-local peak throughput.

## Baseline captured before attribution hooks

Captured 2026-09-28 at base revision
`e130ae8a638e9e21a6202341f8346806bac11467` with only the uncommitted harness/helper
changes. Rust 1.96.0 (`ac68faa20`, LLVM 22.1.2), release/default features, empty
RUSTFLAGS, Linux x86_64, AMD EPYC 9V74 virtual machine, 48 logical CPUs, one NUMA
node, 96 MiB aggregate L3, PCLMUL/AVX2 present. The fail-closed wrapper reported
16 GiB memory.max and zero swap. Cargo jobs and test threads were both two.
The existing repository build cache was used. The test passed in 32.47 seconds;
all 80 cases completed with zero unexpected failures. Authentication failures
equaled the iteration count in every `bad_tag` case.

Selected raw baseline values below are **total milliseconds**, not per-operation
latencies. CPU is CLOCK_PROCESS_CPUTIME_ID summed across both benchmark threads.
All 16 MiB rows processed eight operations (128 MiB payload). These are single
observations on a shared VM, not confidence intervals or a claimed speedup.

| Size | Input | Layer | Operation | Iterations | Elapsed ms | CPU ms |
|---|---|---|---|---:|---:|---:|
| 63 B | reused | primitive | CRC | 266305 | 23.232613 | 23.232023 |
| 63 B | reused | primitive | encrypt | 266305 | 481.325828 | 481.305302 |
| 4095 B | reused | primitive | CRC | 4097 | 16.156909 | 16.157192 |
| 4095 B | reused | primitive | encrypt | 4097 | 20.569770 | 20.570454 |
| 1 MiB | rotating | primitive | CRC | 128 | 128.863919 | 128.839862 |
| 1 MiB | rotating | primitive | encrypt | 128 | 94.916533 | 94.915454 |
| 1 MiB | rotating | primitive | decrypt_copy | 128 | 105.567962 | 105.565597 |
| 1 MiB | rotating | engine | encrypt | 128 | 279.582454 | 279.550304 |
| 1 MiB | rotating | engine | decrypt | 128 | 272.171141 | 272.147408 |
| 1 MiB | rotating | paired | encrypt | 128 | 239.604745 | 282.043899 |
| 1 MiB | rotating | paired | decrypt | 128 | 249.004419 | 334.409638 |
| 16 MiB | reused | primitive | CRC | 8 | 134.684546 | 134.658665 |
| 16 MiB | reused | primitive | encrypt | 8 | 107.492998 | 107.492439 |
| 16 MiB | reused | primitive | decrypt_copy | 8 | 105.150597 | 105.090997 |
| 16 MiB | reused | engine | encrypt | 8 | 289.669897 | 289.614700 |
| 16 MiB | reused | engine | decrypt | 8 | 290.681624 | 290.644414 |
| 16 MiB | reused | paired | encrypt | 8 | 280.694677 | 334.704956 |
| 16 MiB | reused | paired | decrypt | 8 | 240.238357 | 280.145569 |
| 16 MiB | rotating | primitive | CRC | 8 | 128.177403 | 128.175280 |
| 16 MiB | rotating | primitive | encrypt | 8 | 97.148222 | 97.139606 |
| 16 MiB | rotating | primitive | decrypt_copy | 8 | 102.385358 | 102.383628 |
| 16 MiB | rotating | primitive | bad_tag | 8 | 51.198908 | 51.197853 |
| 16 MiB | rotating | engine | encrypt | 8 | 281.829181 | 281.801406 |
| 16 MiB | rotating | engine | decrypt | 8 | 345.262537 | 345.245373 |
| 16 MiB | rotating | engine | bad_tag | 8 | 390.911228 | 390.780050 |
| 16 MiB | rotating | paired | encrypt | 8 | 269.657166 | 367.685707 |
| 16 MiB | rotating | paired | decrypt | 8 | 256.469964 | 354.402010 |
| 16 MiB | rotating | paired | bad_tag | 8 | 242.902719 | 287.784340 |

Inference: CRC is material compared with complete AEAD on this host; lifecycle
cost is not explained by primitive AEAD alone. These observations do not isolate
allocation, zeroization, CRC, scheduling, or copying from one another, and should
not be subtracted to claim individual stage costs. No Grace/ARM numbers were
collected or inferred. The final integrated run is below.

## Final integrated x86 run

Captured 2026-09-28 at clean revision
`151f88c2fd8310d40216a5c583a8fcbe2eb35ca1`, after integrating CRC and AEAD plus
the harness API migration. Cargo.lock SHA-256:
`434d4e320144aa9cf2d04d15c4bf652dfaac5b7269521c677d457758b90c1a33`.
Same Rust 1.96.0/LLVM 22.1.2, AMD EPYC 9V74 x86_64 VM, release/default features,
empty RUSTFLAGS, allowed CPUs 0-47 and memory node 0. The wrapper again verified
16 GiB memory.max and zero swap; jobs/tests were two. All 80 cases passed in
31.17 seconds, with bad-tag failure counts exactly equal to iterations and no
unexpected failures. The supplement passed all 24 cases in 24.08 seconds.
These are sequential single runs on a shared unpinned VM, not medians or a
controlled statistical study. Compiler/CPU provenance matches the baseline;
code, dependencies, checksum variant, and instrumentation differ.

Exact paired observations for every row retained in the baseline table follow.
All times are **total milliseconds**; iteration counts are unchanged from above.

| Size | Input | Layer | Operation | Baseline elapsed | Optimized elapsed | Baseline CPU | Optimized CPU |
|---|---|---|---|---:|---:|---:|---:|
| 63 B | reused | primitive | CRC | 23.232613 | 12.262418 | 23.232023 | 12.262759 |
| 63 B | reused | primitive | encrypt | 481.325828 | 517.370860 | 481.305302 | 517.302888 |
| 4095 B | reused | primitive | CRC | 16.156909 | 1.411271 | 16.157192 | 1.410888 |
| 4095 B | reused | primitive | encrypt | 20.569770 | 20.371971 | 20.570454 | 20.372281 |
| 1 MiB | rotating | primitive | CRC | 128.863919 | 9.828897 | 128.839862 | 9.829938 |
| 1 MiB | rotating | primitive | encrypt | 94.916533 | 96.977257 | 94.915454 | 96.976776 |
| 1 MiB | rotating | primitive | decrypt_copy | 105.567962 | 102.396216 | 105.565597 | 102.393883 |
| 1 MiB | rotating | engine | encrypt | 279.582454 | 157.328876 | 279.550304 | 157.324800 |
| 1 MiB | rotating | engine | decrypt | 272.171141 | 156.297464 | 272.147408 | 156.135700 |
| 1 MiB | rotating | paired | encrypt | 239.604745 | 177.247173 | 282.043899 | 287.209017 |
| 1 MiB | rotating | paired | decrypt | 249.004419 | 126.100748 | 334.409638 | 210.127637 |
| 16 MiB | reused | primitive | CRC | 134.684546 | 9.336654 | 134.658665 | 9.336568 |
| 16 MiB | reused | primitive | encrypt | 107.492998 | 94.808773 | 107.492439 | 94.455715 |
| 16 MiB | reused | primitive | decrypt_copy | 105.150597 | 100.155774 | 105.090997 | 100.141238 |
| 16 MiB | reused | engine | encrypt | 289.669897 | 154.206554 | 289.614700 | 154.187408 |
| 16 MiB | reused | engine | decrypt | 290.681624 | 151.247246 | 290.644414 | 151.242419 |
| 16 MiB | reused | paired | encrypt | 280.694677 | 153.646108 | 334.704956 | 221.454470 |
| 16 MiB | reused | paired | decrypt | 240.238357 | 114.211542 | 280.145569 | 156.765601 |
| 16 MiB | rotating | primitive | CRC | 128.177403 | 9.279798 | 128.175280 | 9.279404 |
| 16 MiB | rotating | primitive | encrypt | 97.148222 | 94.824838 | 97.139606 | 94.824280 |
| 16 MiB | rotating | primitive | decrypt_copy | 102.385358 | 100.320262 | 102.383628 | 100.293504 |
| 16 MiB | rotating | primitive | bad_tag | 51.198908 | 43.174932 | 51.197853 | 43.173867 |
| 16 MiB | rotating | engine | encrypt | 281.829181 | 159.158919 | 281.801406 | 159.154168 |
| 16 MiB | rotating | engine | decrypt | 345.262537 | 238.975201 | 345.245373 | 238.950908 |
| 16 MiB | rotating | engine | bad_tag | 390.911228 | 254.463862 | 390.780050 | 254.420391 |
| 16 MiB | rotating | paired | encrypt | 269.657166 | 151.131482 | 367.685707 | 253.090923 |
| 16 MiB | rotating | paired | decrypt | 256.469964 | 154.815373 | 354.402010 | 261.504006 |
| 16 MiB | rotating | paired | bad_tag | 242.902719 | 140.723851 | 287.784340 | 214.236400 |

The 16 MiB rotating CRC elapsed ratio is about 13.8x; paired encrypt/decrypt
elapsed ratios are about 1.78x/1.66x. Those are ratios of these observations, not
guaranteed deployment speedups. This changes the CRC variant as explicitly
approved, so it is not a bit-identical CRC implementation comparison. The large
CRC reduction coexists with roughly unchanged primitive AEAD and substantial
lifecycle gains. The 63-byte primitive encryption observation regressed, and
1 MiB rotating paired encryption CPU increased despite lower wall time. There is
no evidence here for an across-the-board primitive or CPU improvement, nor an
isolated benefit attributable solely to eliminating the page copy.

Selected exact **out-of-place supplement** results (same counts as primitives
above, milliseconds):

| Size | Input | Operation | Elapsed | CPU |
|---|---|---|---:|---:|
| 63 B | reused | encrypt_inout | 508.975562 | 508.850709 |
| 63 B | reused | decrypt_inout | 507.093103 | 506.911992 |
| 4095 B | reused | encrypt_inout | 21.489353 | 21.489635 |
| 4095 B | reused | decrypt_inout | 21.397815 | 21.398750 |
| 1 MiB | rotating | encrypt_inout | 96.851919 | 96.842106 |
| 1 MiB | rotating | decrypt_inout | 109.673745 | 109.647438 |
| 1 MiB | rotating | bad_tag_inout | 54.762535 | 54.761467 |
| 16 MiB | reused | encrypt_inout | 128.952041 | 128.923277 |
| 16 MiB | reused | decrypt_inout | 115.840879 | 115.838511 |
| 16 MiB | rotating | encrypt_inout | 106.497702 | 106.495504 |
| 16 MiB | rotating | decrypt_inout | 109.172247 | 109.169241 |
| 16 MiB | rotating | bad_tag_inout | 50.663180 | 50.662541 |

### Paired full-client attribution overhead

The original 80-case matrix uses low-level ports and does not aggregate counters.
It is still not an overhead comparison. A separate test now closes that gap:
`runtime::crypto::measurement::crypto_attribution_overhead`. It runs the same
optimized CRC/AEAD in both modes through **actual `CryptoClient::execute` and
`CryptoClient::poll_budgeted` completion reap**, including all eight counter
updates, not a surrogate atomic loop
(`cmd/racer-dataplane/src/runtime/crypto_measurement.rs:429-578`,
`cmd/racer-dataplane/src/runtime/crypto.rs:642-646`).

The disable flag exists only under `cfg(test)`, is set on the I/O port before
constructing the client, and follows each permit across threads. It skips input
direction/byte bookkeeping, submission/dequeue/execution attribution clocks,
duration conversion/storage, and the metrics borrow/aggregation at reap. Production
`measurement_enabled()` is unconditionally true with no disable field or runtime
configuration (`runtime/crypto.rs:111-145,213-224,376-378,413-415`). Both modes retain
the same message sizes/default field initialization and test-only branches; this
is an incremental **active attribution** cost measurement, not an ABI-size or
compile-out/code-layout experiment. Deadline checks, nonce generation, crypto,
admission, waiter handling, and ownership fences are unchanged. The clock hook
still selects `runtime::environment::now`, preserving virtual-time attribution.

Workload: reused source, batches of up to eight operations, 32,768 operations per
63-byte sample or 16 operations (256 MiB) per 16 MiB sample, success paths in both
directions. Input admission/allocation/copy, key leasing, queue handoff, crypto,
real client completion reap and result delivery, sampled output checks, output
destruction/recycling, thread join, and recycler reclamation are timed. Fixture
construction, thread spawn, final quota/counter assertions, and fixture destruction
are outside timing. Each sample asserts exact success/bytes/execution-count/queue-
count counters when enabled, all eight counters zero when disabled, no waiters or
permits remaining, and zero payload quota. One warmup per mode precedes eight
paired samples; order is off/on for even pairs and on/off for odd pairs
(`runtime/crypto_measurement.rs:596-639`). This is the full crypto-client lifecycle,
not the complete production worker scheduler, a scrape-concurrency benchmark, or
an error-path performance study.

Reproduction (select CPUs from the current allowed set on other machines):

```sh
timeout --signal=TERM --kill-after=10s 300s env \
  CARGO_BUILD_JOBS=2 RUST_TEST_THREADS=2 \
  bash hack/scripts/memory-safe-run.sh -- taskset -c 2,4 \
  cargo test --locked --release \
    --manifest-path cmd/racer-dataplane/Cargo.toml --lib \
    runtime::crypto::measurement::crypto_attribution_overhead -- \
    --ignored --exact --nocapture --test-threads=2
```

Captured 2026-09-28 on base `2688f3e0bc5fc8d65c57b46978cdfd398fcdd3a5`
plus the test-hook/harness patch committed as `fdf13ce6` (dirty=true during the
measurement). Same optimized Cargo.lock hash,
Rust 1.96.0/LLVM 22.1.2, empty RUSTFLAGS, default features, and EPYC 9V74 VM as the
integrated run. The wrapper verified 16 GiB memory.max and zero swap. Process
affinity was restricted to **2,4**, distinct physical cores 1 and 2, NUMA node 0,
within the actual allowed 0-47 set. Both threads inherit this two-CPU mask; roles
are not individually pinned and CPUs are not isolated from other host work.
All 64 measured samples passed in 17.41 seconds including warmups/setup.
`CRYPTO_ATTRIBUTION` emits raw elapsed/process-CPU nanoseconds and workload fields;
`CRYPTO_ATTRIBUTION_RATIOS` emits each paired on/off ratio. Process CPU sums both
threads. The retained raw samples below are total **milliseconds** (six decimals).

| Size/op | Pair | Off elapsed | On elapsed | Off CPU | On CPU |
|---|---:|---:|---:|---:|---:|
| 63 B encrypt | 0 | 213.423745 | 220.203797 | 307.714514 | 318.277823 |
| 63 B encrypt | 1 | 213.548913 | 217.643093 | 308.425609 | 316.091927 |
| 63 B encrypt | 2 | 213.361311 | 216.052723 | 307.118876 | 312.348504 |
| 63 B encrypt | 3 | 209.376127 | 218.355297 | 306.348552 | 316.029142 |
| 63 B encrypt | 4 | 212.283384 | 218.738326 | 308.961875 | 316.931059 |
| 63 B encrypt | 5 | 213.489544 | 217.443050 | 307.491389 | 315.433552 |
| 63 B encrypt | 6 | 213.672771 | 215.542292 | 308.534988 | 311.041618 |
| 63 B encrypt | 7 | 206.334206 | 217.695882 | 297.806607 | 312.306584 |
| 63 B decrypt | 0 | 179.246537 | 177.297747 | 259.156922 | 264.276599 |
| 63 B decrypt | 1 | 178.172246 | 179.672821 | 262.298831 | 264.727383 |
| 63 B decrypt | 2 | 178.588665 | 179.640713 | 262.443534 | 265.992693 |
| 63 B decrypt | 3 | 179.232468 | 180.382794 | 262.540571 | 267.848707 |
| 63 B decrypt | 4 | 179.307050 | 180.155120 | 261.745317 | 264.712550 |
| 63 B decrypt | 5 | 184.584065 | 186.207215 | 270.214869 | 271.909008 |
| 63 B decrypt | 6 | 182.341079 | 185.025611 | 267.859216 | 273.956681 |
| 63 B decrypt | 7 | 179.503246 | 180.734465 | 263.465608 | 267.794528 |
| 16 MiB encrypt | 0 | 297.039271 | 288.762118 | 462.313298 | 456.608030 |
| 16 MiB encrypt | 1 | 299.007702 | 299.224780 | 462.756810 | 485.067444 |
| 16 MiB encrypt | 2 | 288.002111 | 298.532034 | 454.783915 | 483.464483 |
| 16 MiB encrypt | 3 | 299.422653 | 296.043628 | 485.375469 | 459.252791 |
| 16 MiB encrypt | 4 | 296.583649 | 299.224970 | 459.371221 | 483.805057 |
| 16 MiB encrypt | 5 | 299.307315 | 294.947013 | 484.090840 | 457.483859 |
| 16 MiB encrypt | 6 | 295.840086 | 281.561779 | 459.775191 | 444.305730 |
| 16 MiB encrypt | 7 | 295.001365 | 299.052560 | 458.624567 | 486.369201 |
| 16 MiB decrypt | 0 | 296.324907 | 297.074244 | 463.407052 | 463.854791 |
| 16 MiB decrypt | 1 | 295.752954 | 296.153704 | 459.189986 | 462.873153 |
| 16 MiB decrypt | 2 | 295.906090 | 294.929792 | 459.139175 | 457.207195 |
| 16 MiB decrypt | 3 | 295.614895 | 295.455525 | 457.767629 | 458.169607 |
| 16 MiB decrypt | 4 | 295.396091 | 295.603633 | 459.353859 | 457.757171 |
| 16 MiB decrypt | 5 | 296.078395 | 298.195125 | 459.758855 | 460.555973 |
| 16 MiB decrypt | 6 | 297.280615 | 297.543791 | 458.961860 | 461.850006 |
| 16 MiB decrypt | 7 | 267.160276 | 270.421653 | 393.156377 | 412.986942 |

Summary of **paired on/off ratios**, not ratios of pooled means:

| Size/op | Median elapsed ratio | Elapsed min-max | Median CPU ratio | CPU min-max |
|---|---:|---:|---:|---:|
| 63 B encrypt | 1.024790 | 1.008749-1.055064 | 1.025811 | 1.008124-1.048689 |
| 63 B decrypt | 1.006639 | 0.989128-1.014723 | 1.014977 | 1.006270-1.022764 |
| 16 MiB encrypt | 0.994720 | 0.951736-1.036562 | 1.017936 | 0.945037-1.063064 |
| 16 MiB decrypt | 1.001120 | 0.996701-1.012208 | 1.001350 | 0.995792-1.050439 |

Small-page median deltas here are +2.48%/+0.66% elapsed and +2.58%/+1.50% CPU
for encrypt/decrypt. Large-page deltas are dominated by run variation at this
sample count; in particular the apparent encryption elapsed reduction is **not
evidence of a speedup**. Eight pairs on a shared VM do not establish significance,
an upper bound, or deployment-wide overhead. This does establish a reproducible
same-crypto comparison including real aggregation, unlike subtracting the old
baseline from the optimized implementation. No Grace numbers are implied.

Focused acceptance checks: crypto tests 17 passed/three benchmarks ignored,
AEAD tests nine passed, release non-test `cargo check --lib` passed. The new
toggle/cleanup test runs both modes and directions under a simulated environment;
existing exact virtual-duration/cancellation/abandonment tests still pass. No
blanket broad suite was repeated. Changed Rust files and `crc64.rs` passed rustfmt;
`crc64.rs` already matched the formatter and required no edit. Bounded scoped
`make fmt` ran gofumpt, then reproduced the existing golangci-lint Go 1.26 versus
source Go 1.27 panic; no Go changes resulted.

## Dependencies, dispatch, and security review

The resolved manifest preserves **both** `crc64fast` and the AEAD feature
additions (`cmd/racer-dataplane/Cargo.toml:15-28`). Cargo, not a hand edit,
reconciled Cargo.lock by adding CRC to the AEAD lock; subsequent checks/tests use
`--locked`. Relevant locked releases and `cargo metadata` license expressions:

| Crate | Version | License choice |
|---|---|---|
| crc64fast | 1.1.0 | MIT OR Apache-2.0 |
| chacha20poly1305 | 0.11.0 | Apache-2.0 OR MIT |
| chacha20 | 0.10.2 | MIT OR Apache-2.0 |
| poly1305 | 0.9.1 | Apache-2.0 OR MIT |
| cipher | 0.5.2 | MIT OR Apache-2.0 |
| aead | 0.6.1 | MIT OR Apache-2.0 |
| inout | 0.2.2 | MIT OR Apache-2.0 |

The dependency tree confirms `chacha20poly1305` enables `alloc` and `zeroize`
without default features. Explicit `cipher/zeroize` and `poly1305/zeroize`
feature unification is intentional: the released AEAD feature does not alone
enable both buffered-keystream wiping and Poly1305 state wiping. Do not remove
these apparently unused direct dependencies without rechecking the release
feature graph. This review is not an independent cryptographic audit or a
claim that every compiler register/temporary is scrubbed.

Released source, not upstream main-branch promises, determines CPU support:

- `crc64fast` 1.1.0 `src/pclmulqdq/mod.rs:72-98` selects SIMD after architecture
  detection and otherwise uses tables. It folds aligned 128-byte blocks and
  handles short prefixes/tails with tables. Its AArch64 implementation detects
  PMULL and NEON; x86 requires PCLMULQDQ, SSE2, SSE4.1. The explicit x86 test
  passed here. The PMULL test is a separate ignored hardware gate, not silently
  passed on x86 (`cmd/racer-dataplane/src/security/crc64.rs:96-125`).
- `chacha20` 0.10.2 `src/lib.rs:239-297` runtime-dispatches x86 AVX2/SSE2; AVX-512
  is gated by an additional `chacha20_avx512` cfg and was **not enabled** here,
  even though this VM advertises AVX-512. AArch64 NEON is selected at compile time
  with `target_arch=aarch64,target_feature=neon`, not a new project runtime
  dispatch. Without that feature the portable backend is used.
- `poly1305` 0.9.1 `src/backend.rs:3-15` exposes the AVX2/autodetect backend only
  on x86/x86_64 and the soft backend otherwise. There is **no released ARM NEON
  Poly1305 backend** in this locked version. NEON ChaCha does not imply NEON for
  the complete AEAD. No forced target-cpu or unstable feature is enabled.

No ARM/Grace machine, GPU, ARM cross-build, or PMULL execution was measured in
this integration. The repository delegates CPU dispatch to maintained crates;
it adds no custom SIMD, GPU crypto, or changed cryptographic algorithm.

Correctness review verified immutable input until the final scope check,
initialized admitted output, exact plaintext-length decryption allocation, and
retained original ciphertext on success/failure
(`cmd/racer-dataplane/src/security/aead.rs:217-270,302-316`). In the locked AEAD,
`src/cipher.rs:75-98` authenticates ciphertext/tag before writing plaintext.
Independent libsodium boundary/full-page vectors and sentinel-output tamper
tests exercise that contract (`aead.rs:494-681`). The allocator-observing
integration test checks complete output wiping on authentication failure and
cancellation injected at output allocation, with charges retained until the
completion is dropped (`cmd/racer-dataplane/tests/payload_zeroization.rs:311-339`).
CRC remains accidental-corruption detection, never a substitute for AEAD.

## Minimal production attribution

Sixteen fixed label-free series are added, eight per direction:

- `racer_crypto_{encrypt,decrypt}_started_total`
- `racer_crypto_{encrypt,decrypt}_success_total`
- `racer_crypto_{encrypt,decrypt}_failure_total`
- `racer_crypto_{encrypt,decrypt}_success_bytes_total` (plaintext bytes)
- `racer_crypto_{encrypt,decrypt}_execution_nanoseconds_{count,sum}`
- `racer_crypto_{encrypt,decrypt}_queue_nanoseconds_{count,sum}`

These names describe two separately enumerated fixed sets, not dynamic labels.
All increments and cross-worker sums saturate at u64::MAX. The existing aligned
I/O writer shard is reused: the crypto thread writes only timing fields in its
owned permit, never the shared counter shard. No metric update allocates, locks,
registers a name, or clones an Arc. The client installs its metrics handle once
at application assembly. Relaxed scrapes are not transactional snapshots.

Aggregation occurs **once on I/O completion dequeue**, before waiter delivery or
abandonment handling. Thus `started` means execution observed through a reaped
completion; executing work and an engine crash without completion are not counted.
A success later abandoned/canceled by its caller is still crypto success. A scope
canceled before engine processing counts as an executed failure, not success
bytes. Submission rejection and retries do not count; retrying completion
publication cannot count twice. Direct calls to `process` and low-level ports
outside the production CryptoClient do not publish metrics.

Queue residence begins immediately before the **successful** SPSC send attempt
and ends at crypto dequeue. Failed attempts are unobserved and their timestamp
is overwritten on retry. Reservation/admission and input allocation are excluded;
the tiny publication-call cost is included. Execution starts at `process` entry
and ends after constructing the outcome, including CRC and failure checks but
excluding completion publication, I/O wait, and output destruction. Both clocks
use `runtime::environment::now`, so DST never accesses host time for attribution.
Measurements stay inside the existing permit, preserving every input/key/quota
and abandoned-completion ownership fence.

## Measurement-foundation validation (before integration)

- Focused release crypto tests: 16 passed, including real success, validation/CRC
  failure, cancellation, abandonment, completion-publication retry, exact virtual
  queue/execution sums, admission-delay exclusion, and duration saturation.
- Bounded release library suite: 818 passed, 5 ignored, no failures; 55.13 seconds
  test execution. Includes generated datapath DST, saturation/aggregation,
  concurrent scrape tests, and worst-case diagnostic response bounds.
- Cargo formatting applied; unrelated preexisting integration-test module-order
  formatting is excluded from this change.
- Final harness smoke rerun after provenance and sampled-output validation changes:
  all 80 cases passed in 33.13 seconds. This is not a controlled attribution-overhead
  comparison (low-level benchmark ports bypass I/O counter aggregation).
- Required root `make fmt` was attempted under the bound. gofumpt completed;
  golangci-lint refused because another agent held its global parallel-run lock.
  No Go changes are included.

## Final integration validation

All commands had external `timeout --signal=TERM --kill-after=10s 300s` bounds.
Rust test commands used the fail-closed memory-safe wrapper, Cargo jobs two and
test threads two. Broad validation ran once in bounded groups, with all features:

- `cargo check --locked --all-features --all-targets`: passed without warnings.
- Focused release security: 65 passed, two hardware gates ignored; crypto:
  16 passed, two benchmark tests ignored; store: 64 passed, one benchmark ignored.
- Explicit x86 CRC hardware gate: one passed.
- Full release/all-feature library: **821 passed, 14 ignored**, no failures;
  test execution 58.35 seconds. Ignored tests include benchmarks and native RDMA
  hardware gates; the 80/24-case crypto matrices were run separately as above.
- All integration targets: client/origin conformance 19 passed/one ignored;
  payload zeroization one passed; process restart three passed/16 ignored;
  production dataplane 16 passed/one ignored. The privileged restart/throughput,
  Go SDK bridge, and brd benchmarks remained ignored, not claimed as executed.
- Doctests: 32 passed, including 26 compile-fail contracts. Binary tests: two passed.
- Changed Rust files formatted with rustfmt (edition 2024, child traversal
  disabled when checking the complete changed-file set); no unrelated changes.
- Required bounded `make fmt` attempted once in integration. gofumpt completed;
  golangci-lint panicked because it was built with Go 1.26 while a source file
  required Go 1.27. No Go changes resulted. This is an environment limitation,
  not a passing root-format/lint result.

Remaining limitations: no full-node throughput claim, isolated-core/per-role
pinned NUMA study, allocation count, hardware
cycle/cache-miss counters, sampled histograms, or Grace/GPU hardware validation.
