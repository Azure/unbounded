# Racer GPU plaintext reuse investigation

Date: October 9, 2026, UTC.

## Outcome and scope

Source commit `5464b41dda3a1f192e60536c91c864da0499ab5f` fixes a bounded memory-reclaim cursor that advanced past entries it had not examined. Independent review approved the fix and ignored trace test. The change is limited to `cmd/racer-dataplane/src/memory.rs`: production 12 added/14 removed lines, plus 474 test lines. **The fix is not deployed, and no live performance gain is established.** Parent audit and integration into the original branch are next; no integration hash exists yet.

This iteration made no image deployment, load activation, worker-count change, memory-budget change, or new CPU capture. The report phase read saved receipts only. Existing operational incidents remain recorded, not cleared by local tests.

Below, `DP/` means `cmd/racer-dataplane/`, with source lines at the source commit above. Private `tmp/` receipts are preserved under the archive described at the end.

## What the code does

Page acquisition routes to a stable worker owner (`DP/src/read/dispatch.rs:448-475,851-870`). Verified plaintext lookup precedes flight admission and returns its existing bundle without decrypting (`DP/src/read/fill.rs:717-734`). On a local miss, pending ciphertext or disk acquisition leads to plaintext reservation, decrypt, and publication (`:1174-1267`). Crypto returns verified plaintext with the original ciphertext, and Fill constructs a complete bundle (`DP/src/security.rs:582-598`; `DP/src/read/fill.rs:1586-1615`). Memory publication replaces ciphertext-only residency with that bundle; publication may be skipped on expected admission/availability errors without failing the read (`DP/src/memory.rs:605-647`; `DP/src/read/fill.rs:1879-1882`).

The memory hit counter mixes verified-plaintext hits, ciphertext copy-only hits, pending copies, and some pending-copy decrypt results (`DP/src/read/fill.rs:720-734,1050-1064,1232-1265`). It is not a count of decrypts avoided. Plaintext hit/miss counters count executed presence probes, not distinct keys or requests (`DP/src/telemetry.rs:2438-2472`). Crypto success counters are reaped engine completions, including abandoned waiters (`DP/src/security.rs:1071-1118`). Flights disappear when waiters and retained operations are both absent; they are not a permanent second plaintext cache (`DP/src/read/flight.rs:1720-1722,1791-1807`).

The hot-read assertions prove copy preservation, shared delivery, and particular counter values, not a plaintext latency ceiling under churn. One test counts a copy-only hit and a later plaintext acquisition as two memory hits; its disk-ciphertext promotion keeps that count unchanged (`DP/src/read/tests/hot_reads.rs:1522-1571`). The mixed-reader test asserts one origin call, not a decrypt-count or latency limit (`:768-774`).

## Why quota coupling matters

Saved configuration has 8 GiB plaintext and 8 GiB ciphertext per node, partitioned across 10 I/O workers, with five crypto workers. Partitioning divides both limits by I/O-worker count (`DP/src/app.rs:2388-2405`). Each worker therefore has about 819.2 MiB per class: a ceiling of 51 full 16-MiB plaintext pages before other ownership and overhead. This is not 51 whole 256-MiB objects.

A verified bundle retains both allocations. Either class can cause the whole bundle to be evicted, and both payload owners must be exclusive before eviction (`DP/src/memory.rs:702-745,800-802`). Ciphertext-only entries are considered first for ciphertext pressure. Disk reads reserve staging plus decoded ciphertext together (`DP/src/store.rs:197-215`). Recycled buffers can retain charges after payload release (`DP/flow/src/lib.rs:431-456`). Thus increasing only plaintext is a poor isolated intervention: ciphertext can still limit plaintext residency.

Saved worker usage was 838,862,096 of 858,993,459 ciphertext bytes, about **97.6564%**. Usage includes allocations such as pooled/staging backing; it is not a verified-bundle count. The saved admission diagnostics show a 33,554,960-byte request at that usage. References: `tmp/ops-readonly-1356-report-v2.json:58-62,117-121,186-188,283-285`. The code's disk staging requirement supports this pressure mechanism, but aggregate occupancy does not identify each evicted page.

## Cursor defect and narrow fix

Before the fix, candidate selection advanced the cursor to the last entry in a detached batch of up to 256, even if reclamation stopped after its first victim. With idle A/B/C, reclaiming A could advance past C; inserting D then made D the next victim ahead of older B/C. Touching B could likewise make it the next victim ahead of older C.

The fixed selector is nonmutating and returns `(tick, PageId)` receipts (`DP/src/memory.rs:437-446`). Capacity eviction advances through examined entries (`:636-645`). Byte-target consumers check whether the target is already met, then advance before cache/busy filters and callbacks (`:679-745`). Ciphertext-only key-order reclamation follows the same rule (`:764-797`). Zero-target calls and ciphertext-only sufficiency do not advance the paired cursor.

This preserves bounded scan progress, not strict global LRU. The cooperative fixed-cut plaintext reclaimer is unchanged (`:466-519`). Callbacks still execute under the existing mutable entries borrow (`:717-730`); this change does not solve callback reentry or destruction concerns. Ciphertext cursor advancement adds a `PageId` clone per examined entry (`:787`), bounded by the 256-entry scan. No new queue, allocation framework, crypto algorithm, or wiping exception was added.

## Local validation

The baseline was `5356a0b67339933eab24b36ec1a89d83efb00958`. Before changing production code, six narrow tests produced **two passing controls and four expected failures**: publication and touch chose the wrong victim under both plaintext and ciphertext pressure. After the fix, all six passed. Added tests cover capacity eviction, zero targets, ciphertext sufficiency, busy/cache-filter/callback advancement, 256-entry progress, insertion during key-order traversal, and empty/max-clock wrap (`DP/src/memory.rs:953-1287`).

Validation used Rust 1.96, offline/locked Cargo, eight build jobs, opt-level 0, debug info off, and a task-private target:

- **32 memory tests passed.**
- **42 tests reported success before the broader reclaim filter hit its 75-second bound, plus one later isolated verified candidate pass. This was not one successful broad-suite run.**
- The isolated full-page test passed in **114.62 seconds**, after **16.64 seconds** of explicit candidate compilation, inside a 280-second external TERM bound. CPU counters advanced at about one core. The test performs 28 full-page acquisitions; its fixture has a 600-second request deadline (`DP/src/read/tests/fill.rs:2377-2456,3301-3328`). Earlier 75/90-second timeouts were retained, not rewritten as passes.
- One earlier extended pass was rejected as candidate evidence: Cargo did not rebuild after shared private-target baseline/Clippy use, and the executable reported 1,171 filtered tests instead of the candidate's 1,183. Cleaning only this task's dataplane package artifacts and explicitly rebuilding produced the verified pass. Both logs remain preserved.
- Strict library/test Clippy passed with warnings denied. Rust formatting and diff checks passed.
- Required `GOTOOLCHAIN=go1.26.9 make fmt` ran once and exited 2 on **11 unrelated Gantry SA1019 HTTP/2 deprecations**. It made no unrelated source changes. Repository-wide formatting/lint is not green; the report phase did not rerun it.

Exact commands, CPU samples, exit codes, and hashes are in `tmp/plaintext-source-checkpoint.md`, `tmp/plaintext-extended-test.log` (rejected provenance), and `tmp/plaintext-extended-candidate-test.log` (accepted).

## Actual-cache trace comparison

The permanent ignored test `memory::tests::cache::reclaim_trace_experiment` uses actual `MemoryCache`, quotas, reclamation, and publication, not a theoretical LRU model (`DP/src/memory.rs:816-951`). It creates 512 distinct object keys with synthetic 8-byte plaintext and 24-byte ciphertext, quotas of 408/1,224 bytes, and metadata capacity 512. There is one lookup per request; hit owners are dropped immediately. A miss reserves ciphertext then plaintext, reclaims only on overload, retries, and publishes without double reservation. Each step checks accounting and the 51-page ceiling; final cleanup checks both quotas return to zero.

Both source versions ran the identical harness: 2,048 warmup requests followed by 20,000 measured requests. Hot/churn repeats four cyclic hot-16 requests then one cyclic tail-496 request. Zipf uses exponent 1.2, xorshift64 seed `0x6a09e667f3bcc909`, and a finite CDF. Sequential cycles all 512 keys. No assertion requires an improvement.

| Workload | Baseline hits | Fixed hits | Baseline refills | Fixed refills | Fixed minus baseline |
|---|---:|---:|---:|---:|---:|
| Hot16 + churn | 1,424 | 16,000 | 18,576 | 4,000 | -14,576 |
| Zipf 512, exponent 1.2 | 13,987 | 14,277 | 6,013 | 5,723 | -290 |
| Sequential 512 | 1,981 | 0 | 18,019 | 20,000 | +1,981 |

Measured reclamations equal refills in every row. Baseline/fixed warmup refills were 1,904/425, 676/580, and 1,898/2,048 respectively. Both exact test runs passed all invariants and cleanup checks.

Zipf had **4.82288% fewer refills**, not measured decrypts or live throughput improvement. Sequential had **1,981 more refills, about 11% worse**: the baseline's skipped entries happened to survive across scans. That accidental retention is an observed effect, not evidence of an intentional policy. Results are workload-dependent.

The full warmup-plus-measurement rank streams, encoded as little-endian u64 values, had identical SHA-256 hashes on both versions:

```text
hot16_churn   b60d78fb0d3ef6292a6cb6a866801030c3964ae0314c5787b4de485246c79e08
zipf512_1.2   9eb38e59aca2b1ccc6a798fcb44a05b7bb183cc728f4230f81814c093f3f3334
sequential512 27fd2bf7b187c0a5c31aaed781cb9b498e7f258cda30f16f811f78aa2dba564a
```

Tiny payloads do not exercise full-page AEAD, disk staging, the recycler's 1-MiB threshold, multiple workers, or active-reader lifetimes. The comparison proves avoidable refills in these traces, not their production frequency or cost.

## Prior GPU evidence, not a new measurement

The [CPU report](racer-gpu-pprof-results-20261009.md) found about 75.5% of sampled user CPU on crypto workers, with roughly 47% XChaCha20 AVX2, 18% Poly1305, and 5% CRC64 flat cost. Individual crypto workers averaged about 0.65-0.82 user cores, not sustained saturation. **Profiles were valid; the whole measurement remained unqualified** (`designs/racer-gpu-pprof-results-20261009.md:7-11,75-84,96-102`).

The saved corrected C8 window gives:

| Quantity | gpu-07-03 | gpu-07-13 |
|---|---:|---:|
| Plaintext probe hit fraction | 58.6485% | 55.6999% |
| Successful decrypt completions | 16,474 | 16,776 |
| Disk-read 16-MiB page equivalents | 16,489 | 16,767 |
| Delivered page equivalents | 41,696 | 39,136 |
| Decrypt completions / delivered equivalents | 39.51% | 42.866% |

Source: predecessor archive `worktree-tmp/ops-pprof-corrected-c8-0302-run-window.json:39-61,90,226-239,268,417,580`. Probe fractions are hits/(hits+misses). Disk equivalents divide read payload bytes by 16 MiB; delivered equivalents are successful unverified pulls times 16 pages. They are aggregate ratios, not distinct-key evidence or a per-operation attribution. Scrapes/completions are not atomic. Client `verify=false` disables client content hashing, not dataplane CRC/AEAD (`DP/src/security.rs:494-533`; `cmd/racer-loadgen/pull.go:428-447`).

## Saved operational state

The independent read-only receipt at **14:05:37.561142Z** reports **RacerSafety PASS**, both generators at C0, and both indexes at 128 GiB. Final C0 collection was at **14:05:35.931181Z** (`tmp/ops-readonly-1400-resume-final-C0.json:280`). It did not authorize activation. The earlier 13:59 report's BLOCKED status remains preserved; the later full guard adds the missing checks rather than changing that old receipt.

The full guard records 16 device identities, 7+8 unique raw-device FDs, 30 approved edge guards, four frozen checkpoint hashes per node, and protected PV Bound/Retain. Write/discard baselines remained unchanged. The deployed dataplane image stayed `ghcr.io/azure/racer-dataplane@sha256:eca33ff671fd2def5556ee4ff58b1e3e5d173638ef55f9d3c639748b3f8c4fa7`; the cursor fix was not deployed. References: `tmp/ops-readonly-1400-resume-summary.json:1-14,107-142`, `tmp/ops-readonly-1400-final-report.json:140-145,204-207`, and `tmp/ops-readonly-1400-resume-pods.json`.

Shared incidents remain separate from RacerSafety: monitoring restart counts were 221/195 with OOMKilled history; node13's missing-GPU condition dates to October 6. The protected device accumulated 88 reads/202,752 bytes since the 03:18 reference, with no further delta from the 13:58 initial capture and no write/discard change. Read source is unresolved; no Racer causality is assigned. The two historical decrypt failures also remain unresolved. Scope is visible nested kube1 resources, not an outer-host health claim (`tmp/ops-readonly-1400-final-report.json:4-46,74-88,119-133,230-257`; `tmp/ops-readonly-1400-resume-summary.json:138-142`).

## Next step and deferred copy work

After parent audit/integration and separate operational approval, compare one bounded C8 fixed-image run against the old image with identical settings, normal warmup, and controlled offered load. Measure decrypt work and disk-read work per delivered page, plaintext probe fractions, failures, latency, and throughput. Retain safety guards and compare equivalent windows. Do not add another profile unless these measurements leave a specific CPU question. A live comparison is required before any production impact claim.

Disk decode still copies with `.to_vec()` (`DP/src/store.rs:254-258`), bypassing the existing charged-buffer recycler lookup (`DP/flow/src/lib.rs:407-428`). This is a separate possible copy/allocation investigation, not implemented here. On a recycler miss, using `buffer()` initializes bytes to zero before a copy, which could add work and regress performance. Secure wiping remains required and unchanged (`:431-456`). Do not fold this into the cursor result.

## Evidence preservation

Private archive: `tmp/racer-gpu-plaintext-reuse-20261009-artifacts` in the original repository, outside the review worktree. It preserves worktree tmp receipts/scripts/logs and the report, excluding the task's build target. Directories are 0700; files are 0600. `MANIFEST.json` and `SHA256SUMS` identify copies. `EXTERNAL_REFERENCES.json` records original archive paths and manifest hashes without recursively copying imports.

The source evidence archive `tmp/racer-plaintext-20261009-evidence` was independently verified and remains referenced in place. Its manifest SHA-256 is `daff3d051fdb747f0a90dbdc95d61acab2b7601c131a1acab184cb78132cf387`. It retains both extended-test logs, including rejected wrong-build evidence, and baseline harness patch SHA-256 `8094f10790ae3e973f4a5b994c90cddaca055e180c5dc36fc66efbdbab5980f7`.

The temporary baseline overlay commit `f5a3e50d2839090af6de1e8259335c3fdba3fcef` exists only for evidence-preserving worktree cleanup. **Do not cherry-pick it.** The baseline worktree was removed normally; the candidate source retains the ignored test. No credentials or private operational JSON belong in the report commit. **Do not replay archived scripts:** prior operations are already applied, canceled, or consumed, and this report authorizes no live action.
