# Racer secure aligned-buffer zeroing: deployment and measurement

## Change and source control

Implemented secure bulk erasure of aligned storage with Linux GNU/musl
`explicit_bzero`, retaining the `zeroize` fallback elsewhere and under Miri.
An allocation-owned clean bit skips only proven redundant erasure: fresh zeroed
storage, already-erased rejected pool returns, and clean idle destruction.
Every mutable exposure marks dirty before returning bytes, including kernel-write
submission through `IoBuffer::bytes_mut`. Erasure covers the full layout, including
padding, before accounting guards are released.

Source: `cmd/racer-dataplane/alloc/src/lib.rs:408-498,586-620`.
Runtime read/receive pointer derivation:
`cmd/racer-dataplane/runtime/src/reactor.rs:982-1027`.
No cipher, CRC, AEAD, quota, transport-lifetime, or disk-format changes.
Regular flow payloads already use bulk erasure and were not changed.

Original-branch commits:
- `c5bb99081`: implementation (worktree commit `b73c737e7`).
- `f9047d9c4`: additional kernel-read/fence assertions (worktree `5cdec7c27`).

Current source had substantial unrelated refactors relative to deployed
`622f8d086887fdc461dc8dcb1700e8fd40ffae8a`. To avoid deploying those in this
experiment, candidate **`41ea7720fc7d552a7875b5b87c8768f910014247`** is exactly
that deployed tree plus the one-file implementation change. Compared source trees,
not Git history. Original branch externally advanced during execution; both fixes
cherry-picked cleanly and allocator tests passed on the integrated source.

## Validation

All commands had external TERM timeouts of at most 300 seconds, with a 10-second
kill grace. Rust tests used `CARGO_PROFILE_TEST_DEBUG=0 CARGO_INCREMENTAL=0`.

- Current source allocator all-features: 54 unit, 12 workflow, 4 compile-fail docs.
- Controlled candidate allocator no-default-features: 54 unit, 2 workflow,
  4 compile-fail docs; `--nocapture` showed no real-I/O capability skips.
- Controlled candidate store: 96 passed, 1 explicit benchmark ignored.
- Controlled candidate read: 232 passed.
- Final-owner payload full-capacity erasure integration test: passed.
- Strengthened workflow assertions: all 12 passed; parent integration allocator
  suite also passed 54 + 12 + 4.
- Scoped rustfmt and `git diff --check` passed. `make fmt` was attempted with
  explicit version variables, but the preexisting Go 1.26-built golangci-lint
  panicked on Go 1.27 source. It left no Go changes. This is not a lint pass.

New assertions cover padded capacity, clean reuse, dirtying before kernel writes,
occupied/borrowed/dropped pool rejection with exactly one wipe, reentrant and
unwinding retained guards, successful and short reads, and abandoned reads whose
storage cannot return to the pool until completion. See
`alloc/src/lib.rs:658-706,888-1022` and
`alloc/tests/workflows.rs:459-518,575-626` under `cmd/racer-dataplane/`.

## Build, publication, and rollout

Measured local native-RDMA image:
`ghcr.io/azure/racer-dataplane:secure-zeroing-41ea7720f`.

- OCI index digest:
  `sha256:b9960c690a3a6a9120b0a569e24e39b154203c7043fa26465999fccd3dcea50d`.
- amd64 manifest:
  `sha256:647f9d1a5af3ff8bea35a48f871e2bcbf836bd3f172c0d53bdeb530c9c628e35`.
- Imported and verified digest plus CRI-managed label on all 1500 nodes.
- Local tag is not claimed to be registry-published.

Dedicated branch `perf/racer-zeroing-deployed` was pushed. [Actions run
37387739487](https://github.com/Azure/unbounded/actions/runs/37387739487)
succeeded and published the full-SHA tag
`ghcr.io/azure/racer-dataplane:41ea7720fc7d552a7875b5b87c8768f910014247`:

- Registry index:
  `sha256:7134e6bfb1ef3b41b29fdf8259729f519c0189dbca4904dd4f95fc167f1d701e`.
- Registry amd64 manifest:
  `sha256:3276e593dd47694ff7f0ba34147e15765d1156f8a48cff4e3a30c3a12b796318`.

These are separate builds. The fleet remains on the measured local artifact,
not the registry build. A new node without the imported local tag cannot pull it;
the published full-SHA image is available for a separately controlled transition.

Canary `aks-ddv5-17198779-vmss0000ad` was capped at C0 and drained, then its pod
image alone was patched. Actual candidate readiness and full index were verified
before removing the cap; progress resumed without application/integrity errors.
Fleet pause reached **1500 C0 / zero in-flight at 23:35:54Z**. The persistent
`unbounded-component-overrides` image was patched, the operator's OnDelete template
converged, and 1499 old-image pods were deleted in bounded batches. All 1500 actual
candidate containers were Ready by 23:37:30Z. Prometheus old-pod staleness was
allowed to clear before resuming; exactly 1500 Ready/full-index series were
confirmed at 23:39:34Z. All 1500 applied C8 by **23:42:25Z**.

Final structural audit found the reused recursive override helper also changed
the unused `racer-dataplane-podnet` entry. No such DaemonSet exists. That entry was
restored; the final semantic diff is only the intended named dataplane image.
No slab or identity files were removed or resized. The canary retains its old
controller-revision label, so DS updated count is 1499; actual-image inventory
independently verifies all 1500 candidate containers.

## Matched measurements

Unchanged workload: 1500 nodes, C8, one catalog image, 11 layers of nominal 1 GiB
with jitter 0.2, seed `benchmark-v1`, layer concurrency 1, loadgen verification
off. Cache CRC and AEAD remain on. Gantry CPU quota/GOMAXPROCS remain 4.

Before: **23:11:18-23:16:18Z**. After: **23:42:38-23:47:38Z** on 2026-10-05.
Fleet Prometheus rates have five samples per node per window; all 1500 nodes
advanced successful pulls and bytes, C8 throughout, Ready throughout, zero counter
resets, and exactly 11,572,407,598 indexed payload bytes on every node throughout.

| Fleet metric | Before | After | Observed change |
|---|---:|---:|---:|
| Received TiB/s | 4.7923 | 5.6061 | +16.98% |
| Successful pulls/s | 455.16 | 532.10 | +16.90% |
| Mean pull seconds | 26.349 | 22.488 | -14.66% |
| Application errors/s | 0 | 0 | unchanged |
| Internal request errors/s | 0.3133 | 0.2881 | nonzero in both |
| Disk source hits/s | 47,761.52 | 65,390.05 | different mix |
| Peer hits/s, origin fills/s | 0 | 0 | unchanged |

CRC/AEAD rejection and disk/retained/peer corruption rates were zero in both.
Source-hit counters are conditional lookup events, not client hit ratios.

### CPU per GiB, sampled node only

Parca profiles for the same node and exact five-minute windows have 300 consecutive
nonzero one-second points each; coverage sums exactly equal raw pprof CPU totals.
Function identities use raw pprof IDs and Name/SystemName fallback, not zero
addresses. Raw profile duration headers are not used. Denominator is the same
node's `rate(racer_loadgen_received_bytes_total[5m]) * 300`, so it is a
Prometheus-extrapolated byte estimate, not an exact byte-fenced experiment.

| Sampled process | Before CPU s | After CPU s | Before CPU s/GiB | After CPU s/GiB |
|---|---:|---:|---:|---:|
| Dataplane | 643.053 | 943.474 | 0.86918 | 0.88901 |
| Gantry | 271.211 | 397.474 | 0.36658 | 0.37453 |
| Loadgen | 217.632 | 311.053 | 0.29416 | 0.29310 |
| Sum | 1131.895 | 1652.000 | 1.52992 | 1.55664 |

Matched node work: 739.837 GiB before, 1061.257 GiB after. **Dataplane CPU/GiB
increased 2.28%; the three-process sum increased 1.75%. This does not establish
an overall CPU-efficiency improvement.** The prominent volatile `zeroize<u8>`
frames disappear after the fix; `explicit_bzero` cumulative CPU is 32.579 seconds
before and 78.000 after, now covering both ordinary and aligned bulk erasure.
Do not add overlapping cumulative frames or infer a speedup from their shares.

The original 90-second after profile had only one byte-counter sample because
Prometheus scrapes every minute. Its CPU/GiB was rejected; five-minute historical
profiles were collected instead. Both raw captures remain in the archive.

This is one before/after observation, not a randomized crossover. Restart changes
memory residency, while disk index persistence does not guarantee an identical
read-source mix. Disk events increased more than delivered bytes, and the sampled
node's work changed more than fleet throughput. Runtime variation, sampling,
scrape extrapolation, unresolved libc symbols, and cache state limit causal claims.
The targeted wipe optimization is validated functionally and visible in profiles;
the measured throughput increase cannot all be credited to it.

## Copied UDS investigation: separate follow-up

The production adapter retains page backing in `PageSendRange`:
`src/http.rs:360-379,527-539`. The delivery engine stages with pipe write/splice,
then switches to copying after unsupported splice or backpressure:
`http/src/transfer.rs:114-184`. Backpressure already uses owning immutable views
after the first drain; that is not a fresh userspace payload copy per send.
Pipe staging itself uses `write`, not vmsplice (`flow/src/pipe.rs:369-390`).
These relevant source files match the deployed source in the controlled tree
comparison. Existing tests assert accepted-cursor fallback and exact bytes
(`http/src/transfer.rs:500-530`) and completion-owned backing on abandonment
(`http/src/transfer.rs:651` onward).

No clear bounded change was established that removes the dominant kernel UDS
copy/page allocation while preserving those ownership guarantees. Merely choosing
direct copying earlier would still leave that copy and requires a separate
fallback-frequency and controlled transfer experiment. No shared-memory redesign,
unsafe vmsplice, or speculative UDS change was included.

## Final state and evidence

Final live audit at **23:50:24Z**: all 1500 actual candidate containers Ready,
all 1500 C8 with empty caps, full indexes, successful-pull progress on all nodes,
zero application/CRC/AEAD errors over the preceding five minutes.
Parent-owned checkpoints: `tmp/racer-zeroing-checkpoint.md`.
Evidence archive: `tmp/racer-zeroing-evidence-20261005.tar.gz` (3,391,935 bytes,
465 files), SHA-256
`44cabd6dad4af75c553fa4d449d25e3925f1a321d5129db85f07ac342f4a087a`.
All archive members were verified against their SHA-256 manifest. The archived
report is the pre-seal version without this checksum paragraph. Includes raw Prometheus responses,
90-second and five-minute profiles, coverage, import checkpoints, live audit,
comparison scripts, and override scope-correction evidence. Excludes build caches
and the large image tar. Worktrees are removed only after archive verification.
