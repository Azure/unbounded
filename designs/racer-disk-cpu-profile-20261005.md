# Racer warm-disk CPU profile, 2026-10-05

## Scope and workload context

Observation only: no source, configuration, or load changes. The run label is
10.78 GiB, warm disk, C8, 1500 nodes, **noverify=true**, meaning loadgen
**`--verify=false`**, not disabled cache CRC or AEAD authentication. The later
capture verifies that argv on all three sampled nodes, mounted concurrency `8`,
empty per-node caps, and 1500 Ready/Available instances of each component
(`tmp/kernel/report.md:15-18`; raw `*-pre.stdout:1`, `*-controls.stdout:1`,
`daemonsets-post.stdout`). DP image identifies source **`622f8d086`**. The run
label is not a fresh measurement of cache occupancy.

Parent-supplied Prometheus context: five-minute rates evaluated at **22:46:17Z**
show RX **5.12517 TiB/s**, all 1500 at C8, zero peer traffic, **62,252.65 disk/s**,
**487.479 successes/s**, and **0 errors/s**. Fleet CPU: idle **28.35%**, iowait
**0.0538%**, system **32.04%**, softirq **10.94%**, user **26.42%**. These are
handoff values, not independently queried here or measurements over the exact
90-second profile window. They must not be used to normalize this single-node
CPU profile into CPU/GiB.

## Single-node Parca window

Node `aks-ddv5-17198779-vmss0000ad`, **22:44:47-22:46:17Z**, 90 seconds.
Each container query has 90 consecutive nonzero one-second points; range sums
equal raw profile CPU totals exactly (`tmp/parca/window.json:2-7`,
`tmp/parca/coverage.json:2-45`). This verifies returned coverage, not capture of
every CPU sample or fleet representativeness.

| Container | Sampled CPU seconds | Average cores |
|---|---:|---:|
| Dataplane (DP) | 291.105 | **3.235** |
| Gantry | 121.158 | **1.346** |
| Loadgen | 93.895 | **1.043** |

DP mapping attribution is **61.87% userspace / 38.13% kernel**. Unresolved
leaves remain **16.33% of total DP CPU**, mostly userspace; mapping attribution
does not resolve those symbols (`tmp/parca/report.md:17-25`).

- **Crypto/CRC union: 29.00%** of DP CPU: XChaCha20-Poly1305 decrypt 25.9447%
  cumulative and CRC64 3.0555%. AVX2 ChaCha20/Poly1305 and PCLMUL CRC are already
  active, not proposed acceleration (`tmp/parca/focused-evidence.txt:2-16,59-72,115-122`;
  `tmp/parca/final-metrics.json:20-24`).
- **UDS send: 29.87% cumulative**. DP `_copy_from_iter` is 13.289% flat and
  `clear_page_erms` 9.022% flat; most of those samples are under Unix-stream
  send copying and socket-buffer page allocation, not disk page-cache reads
  (`tmp/parca/report.md:29-38,56-61`).
- **Userspace wipe-context union: 12.75%**. `explicit_bzero` contributes 5.9483%
  cumulative; zeroize-frame union is 6.7980%. The apparent **6.60% `Charge` drop**
  leaf always has inline zeroization context and is **not a measurement of
  admission-charge bookkeeping cost** (`tmp/parca/final-metrics.json:2-12,32-34`;
  `tmp/parca/report.md:63-70`).
- Gantry splice syscall is **81.15% cumulative**; loadgen `_copy_to_iter` is
  **38.96% flat**, in TCP receive-to-userspace copying
  (`tmp/parca/report.md:72-76`).

These are CPU shares, not CPU/GiB or predicted speedups. Flat symbols,
cumulative ancestors, and sample unions have different meanings: **do not add
these numbers as independent cost buckets**. In particular, runtime polling,
syscall, allocator, and UDS ancestors overlap useful descendant work. The
exploratory broad allocator regex is invalid as an allocator-cost estimate
(`tmp/parca/report.md:60-70`).

## Later three-cohort kernel cross-check

Separate capture envelope **22:52:33-22:53:08Z**, not the Parca window. BCC
requested 30 seconds at 49 Hz on one ddv5, adsv5, and ddsv6 node; startup/output
are included in the envelope, and exact first/last sample times are unavailable
(`tmp/kernel/report.md:7,94-97`). Percentages below use **all samples for each
process TGID**, including empty/missed stacks, not only resolved kernel samples.

| CPU attribution | ddv5 | adsv5 | ddsv6 |
|---|---:|---:|---:|
| DP UDS send, inclusive | 24.67% | 27.92% | 39.80% |
| DP copy-from-iter, flat | 10.78% | 10.01% | 14.75% |
| DP clear-page, flat | 7.19% | 7.04% | 11.42% |
| DP io-wq worker stack, inclusive | 1.45% | 1.32% | 0.84% |
| Gantry splice syscall, inclusive | 78.36% | 82.82% | 80.05% |
| Loadgen read syscall, inclusive | 79.74% | 75.15% | 79.90% |

All three reproduce the DP-send / Gantry-splice / loadgen-receive path
(`tmp/kernel/report.md:52-73`). DP io-wq ownership is established by TGID and
task membership, not thread-name guessing (`tmp/kernel/attribution.md:3-25`).
Empty `-K` stacks are not proof of userspace residency; missed stacks remain
separate. CPU_CLOCK sampling and IRQ attribution can bias leaves, and unlock
symbols do not prove lock contention (`tmp/kernel/report.md:24-48`).

Host snapshots show idle 31.43-38.40%, CPU PSI some 26.68-36.85%, and small
iowait/I/O PSI (`tmp/kernel/report.md:84-100`). Direct-I/O CPU is visible but
small in both captures. **Neither perf/BCC CPU samples nor low iowait prove
disk latency is not limiting**; there is no off-CPU or disk-latency measurement.
One short capture per cohort is not a controlled hardware comparison.

## Exact-source interpretation and follow-up

The following source facts are from the parent's exact-`622f8d086` inspection,
not an inference from current HEAD. Paths are under `cmd/racer-dataplane/`:

- `src/store.rs:196-258`: `DiskStoreReader` copies the aligned disk record into
  owned ciphertext. `src/read/fill.rs:1168-1195` and `src/security.rs:498-537`
  retain CRC and AEAD processing on this path despite loadgen noverify.
- Final-owner wipe/recycle behavior is in `flow/src/lib.rs:321-335,419-449`;
  aligned allocation/drop behavior is in `alloc/src/lib.rs:450-456,545-565`.
  Pool rejection can potentially lead to a second wipe, but its frequency and
  CPU cost are **unquantified**. Do not count the full wipe union as removable.
- Actual `Charge` drop at `flow/src/lib.rs:452-465` is small bookkeeping. The
  6.60% displayed profile leaf has inline wipe attribution, not evidence that
  charging itself consumes that share.

Candidate investigations are redundant copies and wipe ownership, preserving
authentication and final-owner cleanup. No optimization is established here.
Resolve matching-build libc symbols and obtain equal-work CPU seconds/GiB plus
disk latency/off-CPU evidence before ranking changes or claiming a bottleneck.

## Evidence archive and validation

Archive, relative to the original project root:
`tmp/racer-disk-cpu-evidence-20261005.tar.gz`.

SHA-256:
`8464b167b3c77e3d46f3bc91372a452b75a57732889660f5e1a4e2e4095dacc9`

**11,042,213 compressed bytes**, containing **130 files / 366,938,238 source
bytes** under `tmp/parca` and `tmp/kernel`. Includes raw profiles/responses,
queries, coverage, full stacks, analysis, scripts, and capture checkpoints.
No compiled binaries were present; binary pprof evidence is intentionally kept.
`gzip -t` passed; exact member coverage and every archived file's SHA-256 were
checked against its original. The archive is local evidence, not committed.

This commit is documentation only. No application tests or `make fmt` rerun:
the earlier Go 1.26/1.27 toolchain blocker is carried forward from the parent
handoff. No new workload reads, load changes, builds, or history reads were
performed in this final record phase. Parent owns integration and worktree
cleanup.
