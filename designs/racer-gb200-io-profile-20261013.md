# Racer GB200 allocator and I/O investigation, October 13, 2026

## Conclusion

**Crypto is the largest measured userspace CPU cost. Buffer copies and wipes are
substantial I/O-worker costs. I/O scheduling cannot be ruled out, and these runs
do not establish an NVMe bandwidth ceiling.** No application implementation,
allocator replacement, security bypass, or broad refactor was made.

- Software Poly1305 plus NEON ChaCha account for **74.76-78.77%** of sampled
  userspace CPU. SIMD CRC adds **2.23-3.49%**.
- Identified copy/zeroing routines account for **8.48-11.83%** overall and
  **49.10-58.01% of named I/O-worker CPU**. Identified heap-management costs are
  below 1%; this is not a bound on kernel faults or every allocation consequence.
- Fixed helper assignment, whole-page execution, two-page client credits, and
  serial stages within each page can limit overlap. Crypto queues and block
  queues both grew from C8 to C16.
- Node 2426 has slower crypto service. A later CPPC sample suggests lower
  delivered CPU performance, but was **not taken under benchmark load**.

CPU evidence: `E/io-cpu-c16-report.md:41-63` and
`E/io-allocator-profile-report.md:41-110`. Queue evidence:
`E/io-profile-c16-report.md:28-54`. `E` denotes the evidence paths described below.

## Source, workload, and measurement scope

Deployed source SHA: **`948928abd15913ca2f489d3f7195920f64104acf`**.
The audit checkout was `racer-v2` at `e16fe7611`, with the same application code
per task provenance. Four GB200 nodes, suffixes 2424-2427, served a shared
**128 GiB catalog of 2,048 deterministic 64 MiB objects** through direct UDS.
SHA256 verification was enabled. Each node retained five raw data devices,
20 I/O workers, 10 crypto helpers, 4 GiB plaintext and 8 GiB ciphertext admission;
RDMA was disabled. Sources: `E/io-live-inventory-report.md:7-11` and
`E/io-pprof-enable-report.md:23-25`.

| Run | Concurrency per node | Aggregate verified GiB/s | Direct evidence |
|---|---:|---:|---|
| Baseline C8 | 8 | **10.0130** | `E/io-c8-baseline01/summary.json:197-199` |
| Baseline C16 | 16 | **14.1379** | `E/io-c16-baseline01/summary.json:197-199` |
| C16 with CPU capture | 16 | **14.6727** | `E/io-c16-cpu-profile01/summary.json:197-199` |

All three summaries record completion and C0 cleanup (`:1-4`). The baseline
thread/device samples cover aligned 60-second interiors; CPU profiles cover
30 seconds inside the later C16 run. The 120-second goodput is not an exact
denominator for those shorter samples. Profiling enablement caused an operator
rollout, and cache warmth/system state could vary. **The later 14.6727 result is
not a software speedup or a controlled estimate of profiling overhead.**
See `E/io-profile-c8-report.md:5-7`, `E/io-profile-c16-report.md:5-7`,
`E/io-cpu-c16-report.md:5-11`, and `E/io-pprof-enable-report.md:7-25`.

Exact deployed executable and mapped-library hashes matched all nodes. The
saved executable SHA256 is
`a3dce00d0d1d2c5b9ffee106296f01721a37bec58e3bb5a52b372ca519516020`.
Symbolization used that ELF, not a local rebuild. Profiles are userspace-only;
kernel CPU and off-CPU waits are absent (`E/io-cpu-c16-report.md:15-39`).

## Measured CPU costs, without double-counting

Percentages below use all sampled userspace CPU per node. Copy/zeroing columns
are disjoint leaf groups; the last column has a different denominator.

| Node | Poly1305 + ChaCha | CRC | Copy + zeroing | malloc/free/calloc flat | Copy + zeroing / named I/O CPU |
|---|---:|---:|---:|---:|---:|
| 2424 | 74.765% | 3.290% | 11.829% | 0.323% | 58.008% |
| 2425 | 75.270% | 3.486% | 11.324% | 0.223% | 57.437% |
| 2426 | 78.768% | 2.229% | 8.477% | 0.243% | 49.102% |
| 2427 | 76.522% | 3.107% | 10.438% | 0.155% | 54.693% |

Sources: `E/io-allocator-profile-report.md:45-54,88-95`; raw profiles,
symbolized reports, thread maps, and `qualification.json` are under
`E/io-cpu-c16-profile01/`. `E/io-allocator-profile-analyze.py` reproduces the
sample-weight classification.

Matched-libc disassembly tied sampled PCs to the `memcpy` resolver's bounded
copy implementation and two zeroing routines. One is called directly by
`explicit_bzero`; the other is selected by `memset`. Callers connect the copy
mainly to disk ciphertext extraction, and secure wiping to slab drop and payload
recycle. The disassembler's nearest `__xpg_strerror_r` label is **not** their
identity (`E/io-allocator-profile-report.md:19-39,60-69`).

The cumulative union of exact malloc/free/calloc/realloc-related frames is
**0.40-0.60%**, not extra CPU to add to their flat cost. Generic destructor,
crypto `process`, and libc cumulative totals overlap child costs. Hidden/inlined
allocation code remains incompletely attributed. After crypto, CRC, and the
identified copy/zeroing groups, **9.92-10.53%** remains for all other sampled
userspace work, including runtime and AEAD glue. That residual is not an
allocator estimate or a bound on fault service time
(`E/io-allocator-profile-report.md:71-110`).

C16 recorded 28,877-38,263 minor faults per node over the 60-second counter
sample, 2.27-3.75 times C8, with zero major-fault deltas. Fault counts do not
identify a function or measure its kernel servicing cost. Zero sampled
mprotect/munmap wrapper weight likewise does not prove zero calls or latency
(`E/io-profile-c16-report.md:24-26`;
`E/io-allocator-profile-report.md:52,110`).

## Specific allocator and pipeline costs

Source paths in this section are relative to `cmd/racer-dataplane/`.

1. **Disk decode copies a whole ciphertext page.** StoreReader reserves padded
   staging plus decoded ciphertext before issuing the read, roughly **32 MiB
   per full 16 MiB page**, then performs `to_vec()` after completion
   (`src/store.rs:200-221,257-261`; page size `object-wire/src/model.rs:150`).
   This bypasses payload recycler checkout. A decrypt later needs separate
   plaintext storage (`src/security.rs:523-536`).
2. **Small exact-size recyclers can miss under bursts.** Slab keeps one
   exact-size idle buffer (`alloc/src/slab.rs:622-636`). The payload recycler
   keeps at most two buffers per quota authority, shared across sizes/classes;
   checkout searches by exact capacity (`flow/src/lib.rs:407-456`). Plaintext
   is N bytes and ciphertext N+16, so idle ciphertext need not help plaintext
   checkout. Actual reuse/miss rates were not measured.
3. **Secure wiping is measured work, not an optional correctness shortcut.**
   Dirty staging is wiped across its full allocation before free/reuse
   (`alloc/src/lib.rs:505-550,652-673`). Final payload owners wipe full capacity
   before deciding whether to recycle (`flow/src/lib.rs:433-456,1155-1163`).
   Preserve those guarantees; do not remove wipes to improve a benchmark.
4. **No process-global allocator pool was found on this path.** Each I/O lane
   creates its own quota authority (`src/worker.rs:595-615`); cross-thread
   owners share that authority's recycler/counters. Slab uses local Rc/RefCell.
   Payload charge drops do not wake the admission waiter: policy enables only
   connection classes (`src/admission.rs:163-167`). Local contention remains
   possible, but a global mutex or payload wake storm is not supported.
5. **Whole-page crypto is fixed to helpers.** The plan assigns helpers by lane
   index modulo helper count (`runtime/src/group.rs:1353-1367`), with one
   endpoint pair per I/O worker (`src/worker.rs:564-570`). In this 20/10 plan,
   each helper serves two lane queues. It rotates those assigned endpoints,
   executing one complete synchronous page at a time, without cross-helper
   stealing (`runtime/src/group.rs:1048-1059,1189-1223`;
   `src/security.rs:614-620`). This permits local waiting even with spare
   aggregate CPU; it is not proof of queue-capacity exhaustion.
6. **Two credits limit overlap; stages within a page are serial.** SDK Get uses
   two page credits and ordered delivery (`pkg/racersdk/client.go:53-54,370-376`,
   repository-relative). Server window is capped by those credits
   (`src/read/range_stream.rs:703-711`). Full disk read/copy precedes plaintext
   admission, CRC/AEAD, publication, and delivery
   (`src/store.rs:219-261`; `src/read/fill.rs:1288-1318`). Different pages can
   overlap, but an ordered head or slow credit return can stall progress.
7. **UDS delivery has copy and backpressure costs.** Initial pipe staging uses
   `libc::write`, not vmsplice (`flow/src/pipe.rs:388-407`). A pipe-mode
   WouldBlock drains into owned scratch once before switching to immutable
   page views (`http/src/transfer.rs:119-185`). It is neither end-to-end
   zero-copy nor a new full-page userspace copy on every send.

Node budgets are partitioned by I/O-worker count (`src/app.rs:2406-2423`). The
earlier 4 GiB/node ciphertext overload is documented in
`designs/racer-gb200-benchmark-results-20261013.md:235-253`; this investigation
used the already-raised **8 GiB** budget. C8/C16 samples had zero new observed
overload/error deltas, so the old quota failure is **not the current diagnosis**
(`E/io-profile-c16-report.md:58-61`).

## Queueing remains unresolved

C8 to C16 crypto execution means stayed near 23.4 ms on 2424/2425, 24.3 ms on
2427, and **38.2 ms on 2426**. Accepted queue means grew from 7.1-7.7 to
13.6-16.2 ms on peers, and **14.3 to 38.8 ms on 2426**. Timers cover publication
to helper ownership and whole-job elapsed execution, not pre-admission wait or
completion-reap delay (`src/security.rs:1071-1094`;
`E/io-profile-c16-report.md:28-39`). No helper was continuously saturated on
the 60-second average; burst/local stalls remain possible (`:20-22`).

The application submits one full-extent read with forced `Flags::ASYNC`
(`runtime/src/reactor.rs:1166-1176`; `alloc/src/slab.rs:698-715`). The observed
devices have scheduler `none` and a **256 KiB maximum request size**. Thus a
roughly 16 MiB application read is not one block request; block queue depth
cannot be read as application read concurrency. The size limit and counters
support splitting, but exact split/merge timing was not traced
(`E/io-profile-c8-report.md:28-37`; `E/io-runtime-audit.md:45-46`).

Device-wide read rate rose about 9.70 to 13.62 GiB/s. For example, 2424/nvme5n1
read await rose **44.1 to 127.5 ms**, weighted queue **102.5 to 395.6**, and busy
time **70.8% to 96.2%**. These include block queueing, not just device service;
they are not NAND bandwidth or proof of an NVMe ceiling
(`E/io-profile-c16-report.md:41-54`).

Forced io-wq dispatch, block queueing, and cooperative application turns remain
separate candidates. SQEs queued during service work are submitted at the end
of that turn (`src/worker.rs:708-716`). A full-page copy/wipe can therefore
delay subsequent submission or completion handling. Low io-wq CPU, low host PSI,
and spare average worker CPU do not rule out those waits. No aligned
submit/issue/complete/resume trace was captured; schedstats was disabled
(`E/io-profile-c16-report.md:22,54,59`; `E/io-runtime-audit.md:28-34,51`).

## Node 2426: a strong lead, not a causal finding

Post-run CPPC feedback at **19:32:21-19:32:24Z** reported mean delivered
performance on CPUs 20-29 equivalent to **1,941 MHz on 2426 versus 3,204 MHz on
2424**; peers 2425/2427 were 3,211/3,110 MHz. This is firmware performance
feedback mapped to MHz, not an independent cycle measurement. The inverse
ratio **1.65** aligns with the earlier crypto execution-time ratio **1.63**.
But collection occurred after load, during restoration, so it does not prove
benchmark-time frequency or identify thermal/power capping
(`E/io-node-asymmetry.md:18,26-44`).

Matched executable/libraries and CPU features, plus profiles showing the same
crypto backends, weaken a per-node software-backend explanation. SIMD CRC was
observed, not a dominant CRC table fallback. Existing thermal/power visibility
does not identify the limiting mechanism (`E/io-node-asymmetry.md:46-58`;
`E/io-cpu-c16-report.md:52-56`). **Verify CPPC under the same authorized load
window next**, alongside completed jobs and CPU, before making a causal claim.

## Next discriminating experiments, not implemented

1. Confirm node 2426's delivered CPU performance under load using existing CPPC
   counters; preserve settings and compare same-window job cost. Do not change
   governors, power limits, or security settings based on the post-run snapshot.
2. Separate successful-request waits: crypto capacity-ready/submit/dequeue/reap,
   disk submit/block issue/complete/application resume, and pipe/credit waits.
   Use bounded traces or sampled stage timing, not CPU percentages as wait times.
3. If testing helper contention, propose **20 I/O plus 20 crypto helpers** with
   unchanged I/O shard IDs, device ownership, quotas, catalog, credits, and load.
   This requires a reviewed worker-policy change. Existing thread caps 40 or 60
   are **not** an isolated 20/20 switch: the current roughly 2:1 planner can also
   change I/O count, hence quotas and storage ownership
   (`runtime/src/group.rs:1353-1367`; `src/app.rs:2406-2423`).
4. Measure recycler misses and bytes copied/wiped per worker before changing
   pooling. Existing bounded benchmarks are `secure_payload_wipe_benchmark`,
   `memory_only_codec_benchmark`, and `loopback_backpressure_thread_cpu`; their
   limits and commands are in `E/io-allocator-audit.md`, section
   "Discriminators and reusable benchmarks". None measures the complete
   production disk-to-UDS path. Preserve CRC, AEAD ordering, cancellation
   fences, and secure wiping in any future experiment.

## Final state and evidence

Profiling was temporarily enabled through one guarded config key, then removed.
**Final verification completed at 19:32:31.910282Z:** C0, all four loadgens at
applied concurrency/in-flight 0/0, four dataplanes Ready, five raw devices each,
20 I/O/10 crypto workers, original 4/8 GiB budgets, and profiling flag absent.
Config data/binaryData matched the saved baseline. Controllers and origins
remained Ready and unchanged across restoration. No application source,
device preparation, host tuning, or implementation rollout was performed;
the profiling config changes did trigger operator-managed replacements
(`E/io-pprof-restore-report.md:3-11,30-44`).

Evidence `E` is archived under **original checkout
`tmp/gb200-io-profile-20261013/`**, preserving basenames and run directories.
Archive verification completed at **19:38:32.923051Z**: **1,174 files,
23 directories, 94,815,359 bytes**, with matching paths, SHA256 hashes, sizes,
and unchanged source inventory. Directories are private (0700), files 0600.
The receipt is `E/archive-receipt.json`; `E/SHA256SUMS` has SHA256
`915fa576ed0bb07296a5fc4c3210346391d08e62d35ad92c220f4770c86ae888`.
Raw profiles, exact ELF/libraries, identities, command records, and host
snapshots remain private and uncommitted; review before sharing.

Key groups are `io-c8-baseline01/`, `io-c16-baseline01/`,
`io-c16-cpu-profile01/`, `io-profile-{c8,c16}-01/`,
`io-cpu-c16-profile01/`, `io-node-asymmetry-evidence/`, and
`io-pprof-{enable,restore}-evidence/`. The `io-*-report.md` files and allocator,
runtime, pipeline, and crypto audits retain detailed citations and scope limits.
Earlier evidence stays under original `tmp/gb200-bench-20261013/`.
