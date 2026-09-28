# Racer crypto measurement foundation

## Scope and reproduction

This change measures the existing CRC and XChaCha20-Poly1305 implementation. It
does not replace either algorithm or change the wire profile. The only AEAD
production edits are two clock hooks and a mutable permit binding in
`cmd/racer-dataplane/src/security/aead.rs:171-179,310-318`.

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
Keep the wrapper's cgroup limit diagnostics with the output. No extra dependency
or standalone observability framework is introduced.

Primitive buffers, cipher, AAD, and tags are prepared before the clock starts.
Decryption includes copying ciphertext into preallocated scratch (named
`decrypt_copy`); bad-tag also includes that copy. Both still perform the complete
AEAD operation including authentication, not a stream-cipher proxy. Repeated
encryption mutates the same preallocated input. Fixed primitive nonces are for
measurement only; the page engine uses its real fresh-nonce path.

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
collected or inferred. CRC/AEAD optimization agents must rerun this harness after
integration to establish their own results.

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

## Validation

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

Remaining limitations: no full-node throughput claim, no pinned/NUMA study,
no allocation count or hardware cycle/cache-miss counters, no sampled histograms,
and no Grace hardware validation. Broad integration/doctest/all-feature validation
belongs to parent integration with the independent CRC and AEAD changes.
