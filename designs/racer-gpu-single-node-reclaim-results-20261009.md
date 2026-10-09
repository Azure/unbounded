# Single-node reclaim measurement, October 9, 2026

## Result and stop condition

The reclaim fix is deployed on **gpu-07-03**. One old-image C8 window, one candidate first-load C8 window, and one candidate warm C16 window completed. Candidate C32 hit a **hard stop: generic decrypt failures increased from0 to9**. Further load is blocked. No C64, repeat C32, profiling session, key change, memory tuning, or admission-allowance change followed.

Candidate first-load C8 delivered9.27% more successful unverified bytes per second than old first-load C8 in this pair. Normalized decrypt and disk-read work fell about10.49%. This is a descriptive result, not a steady-state, deterministic, or causal performance claim. Both windows include RAM warming. The same LG process continued its RNG stream, and the timing samples are not simultaneous across components.

## Deployed state and recovery constraints

- Source: `bb196c96753dac84f36ea86abdc11036a35a2af2`, already integrated before this work. No production source changed here.
- Build: [37947250639](https://github.com/Azure/unbounded/actions/runs/37947250639), linux/amd64.
- Candidate image: `ghcr.io/azure/racer-dataplane@sha256:352eb2f9b2859ffbf69496b1d27f8e807b70a4dfd34a7c5e9f80a496b722691a`.
- amd64 manifest: `sha256:56813113ce8922c746283b602c91678cba4f3b44d03a6f506c1b06b5d8685289`.
- Previous image: `ghcr.io/azure/racer-dataplane@sha256:eca33ff671fd2def5556ee4ff58b1e3e5d173638ef55f9d3c639748b3f8c4fa7`.
- Current DP: `racer-dataplane-pp46z`, UID `5ee72a5c-a18c-4390-8c2f-0027af5f97c0`. Current LG: `racer-loadgen-vkw7l`, UID `0edd2211-3092-4677-bf70-f06493a2e912`.
- Both supported DP overrides require node03. Node13 has `racer.unbounded-cloud.io/exclude=true`. LG DaemonSet also requires node03 and mounts `racer-bench-single03-control`, UID `579e8466-6810-4de2-9cb3-43a6bf79f724`.
- **Original `racer-bench-control`, UID `dc2df38f-7fc8-4444-b19e-800079832384`, must stay0 indefinitely while an old node13 LG may exist.** Never use it for single-node load. The new control is also0.
- Last C0/drain proof: **17:18:39Z**. Node03 index remained **137,438,953,472 bytes (128 GiB)** in the post-failure capture at17:19:27Z.
- Singleton membership: sequence5, membership version4, hash `92e0916d997f032685d1718d002f55927a1ad51be67798601df8d56501ea4900`. Node03's public admitted-member annotation was hashed and matched a stable version ConfigMap bracket and all10 DP workers. No Secret or raw authenticated snapshot was read.

Node13 stopped renewing its Lease at14:45:40.428890Z and posting node status at14:45:43Z. Ready became Unknown at14:46:47Z. This predates the measurement work. Its later storage and process state are unverified. Do not auto-unexclude it if it returns. Restore labels, affinity, membership, origins, and control wiring only under a separate C0 plan. The operational changes are reversible, but reversal is not authorized by this report.

The user accepted downtime and approved normal-grace UID-preconditioned deletion of old node03 DP/LG pods when normal RollingUpdate stalled with a terminating node13 pod. No force deletion, node13 manual deletion, host restart, disk repair, or checkpoint reset was used. The candidate rollout changed only the two supported image fields. Isolation changes were separately recorded.

## Fixed workload and safety scope

Zipf catalog512 blobs x256 MiB, exponent1.2, seed `gpu-zipf-20261008-128g-v1`, UDS backend, `verify=false`, one blob per pull. Successful delivery is successful-pull delta x268,435,456 bytes, not received partial bytes and not content-verified delivery.

Unchanged DP: flights64, max threads16, actual10 IO/5 crypto workers, plaintext8 GiB, ciphertext8 GiB, registered2 GiB, dirty1 GiB, request contexts256 MiB. DP memory limit64 GiB, requests4 CPU/8 GiB. LG memory limit4 GiB, requests1 CPU/512 MiB. No CPU quota at self or visible cgroup ancestors (`max 100000`). Profiling opt-in stayed true, but no profile session ran.

Completed local guards cover **8 physical devices, 7 approved raw FD device identities, and14 zero endpoint hashes** on node03, plus original frozen checkpoint hashes, protected PV UID/spec/Bound/Retain, protected write/discard baseline, and exact resource/env/source checks. They do not claim global16-device/30-edge coverage or outer-host visibility. Node13's historical baseline remains unchanged and unverified.

Protected node03nvme0 serial `S64HNN0XA11543` was never payload-read. Original write/discard counters remained unchanged. Unknown reads were preserved: +88 reads/+202,752 bytes between03:18 and14:05, then +16/+36,864 by15:56. Later saved counters remain evidence, not an all-zero-counter claim. Historical monitoring OOM counts, including node03=222 and node13=196, remain separate incidents with no Racer cause assigned. The user waived aborting solely on unrelated monitoring restart availability; integrity and local resource checks were not waived.

## Attempts and measurement windows

Saved files show **four old-baseline activation attempts, three failed and one completed**, not four failed baselines:

| Prefix suffix | Outcome |
|---|---|
| 1643 | C8 did not project within the original25s budget; C0 restored; no workload progress. |
| 1649 | Wrapper allowlist rejected a valid refresh; C0 restored; no workload progress. |
| 1652 | JSON Patch tried to add a child of an absent annotations map; API rejected it; C0 restored; no applied window. |
| 1653 | UID/RV-tested whole annotations map preserved existing annotations; projection refresh worked; first completed old C8 window. |

The earlier stage4 Task cancellation was an interrupted isolation observation, not a benchmark attempt or deadlock proof. Its acknowledged CAS/delete were inspected and never replayed.

The target was60s. Sequential metric/CPU reads make the actual applied-to-end sample intervals longer. Rates use per-pod monotonic midpoints. UTC labels below are sample labels, not exact common boundaries.

| Run | Applied UTC | End UTC | LG seconds | Window successes/errors | Whole attempt successes |
|---|---|---|---:|---:|---:|
| Old C8 | 16:54:25.885 | 16:55:30.318 | 64.459486 | 2,652 /0 | 2,897 |
| Candidate C8 | 17:01:35.395 | 17:02:39.938 | 64.509481 | 2,900 /0 | 3,130 |
| Candidate C16 | 17:06:23.783 | 17:07:30.746 | 66.995984 | 3,174 /3 | 3,607 |

Old C8 had85 successes before the applied sample; candidate C8 had75. Whole-attempt counts include activation and drain. Old C8 half-window rates changed9.8158->10.8372 GiB/s (+10.41%); candidate C8 changed11.0453->11.4647 (+3.80%). These do not pass a flat-window stability screen. No separate steady-state window was run.

## First-load C8 comparison

| Metric | Old | Candidate |
|---|---:|---:|
| Successful unverified GiB | 663 | 725 |
| GiB/s | 10.285530 | 11.238658 |
| Plaintext lookup hits/(hits+misses) | 57.853990% | 62.469954% |
| Decrypt successes/(successful pulls x16 pages) | 0.406815611 | 0.364159483 |
| Disk payload bytes/(successful pulls x256 MiB) | 0.406744910 | 0.364073276 |
| Mean decrypt queue, ms | 13.567 | 12.659 |
| Mean decrypt execution, ms | 12.896 | 12.928 |
| Mean successful pull, ms | 194.234 | 177.899 |
| Estimated p50/p95/p99, ms | 129.4 /467.0 /497.0 | 99.5 /460.5 /492.8 |

Relative throughput: **+9.2667%**. Plaintext lookup hit fraction: **+4.615964 percentage points**. Decrypt work/page: **-10.4854%**. Disk bytes/delivered byte: **-10.4910%**. The lookup denominator is not memory-hit plus disk-hit counters. DP/LG intervals differ slightly; counters are not atomic. Percentiles are linear estimates from coarse histogram bucket deltas, not precise tail measurements. C8 CPU seconds/GiB is unavailable because applied/end CPU snapshots were not collected. No estimate is invented.

## C16 continuation, not clean warm-cache scaling

C16 delivered793.5 GiB at11.843993 GiB/s, with3 incomplete pulls out of3,177 completions (0.09443%), within the approved5% after32/cap500 policy. Integrity/CRC/AEAD counters stayed0. Plaintext lookup hit fraction60.11268%; decrypt/page0.276268; disk bytes/delivered byte0.276229. Mean decrypt queue/execution21.240/13.068 ms. Estimated successful-pull p50/p95/p99345.7/883.3/976.7 ms.

The applied-to-end window is3,174 successes/3 errors over66.995984s. Before-to-end is3,271/3 because97 successes preceded the applied sample. Whole attempt through drain is3,607 successes/3 errors. These are different denominators; only the applied-to-end window produces the793.5 GiB and11.843993 GiB/s values above.

Adjacent exact-container CPU samples spanned66.972848s: DP574.939146 CPU-seconds,0.724561 CPU-seconds/GiB,8.58466 mean cores; LG157.162930 CPU-seconds,0.198063 CPU-seconds/GiB,2.34667 mean cores. These are visible container counters, not physical-node CPU attribution or a matched old C8 CPU comparison.

Keyring generation changed **4 at candidate C8 drain to5 before C16**. It stayed5 in C16 samples. Verified applied->end deltas: origin fills333->5,527 (**+5,194**), successful encryptions333->5,527 (**+5,194**), index misses339->5,527 (**+5,188**), disk publications293->5,520 (**+5,227**). They are real interval deltas, not a cumulative reporting error, but they do not prove removal of any particular stored-page key. Index size remained128GiB and recorded index/segment eviction deltas were0.

Stored-page availability is key-dependent: `cmd/racer-dataplane/src/store.rs:417-421` checks cache/key ID availability. Thus key availability filtering is a plausible source-backed explanation for misses while mappings remain counted. Removal of the exact keys for these pages was not captured and is not proved. No Secrets or key material were inspected. C16 is not a clean concurrency-only comparison to C8.

Three unique C16 failed requests joined gate, expiry, and terminal records:

| UTC17:07 | Requested page | Expiry overshoot | Queue/limit | Sent before failure |
|---|---:|---:|---:|---:|
| 28.347 | 3 | 69 us | 4/25 | 48 MiB |
| 28.498 | 10 | 78 us | 6/25 | 160 MiB |
| 28.507 | 15 | 55 us | 5/25 | 240 MiB |

All three expiry snapshots were worker7, with all6/6 retained entries observed in DecryptCall. Gate44 AllowanceExpired increased3; gate52 Cancelled increased1. The recorded EntryCap cause is a prior observation, not proof of terminal capacity. DecryptCall identifies a driver-call stage, not backend execution or hardware saturation. Gate/terminal/expiry rings were unchanged in the later C0 capture. Candidate-final and fill-only admission-final rings were empty. The general ring retained128 of224,315 events, including112 Ciphertext and16 Pipe rows; those unjoined rows do not prove the terminal cause.

## C32 hard failure

One C32 attempt ran. Before/applied/mid samples had generic decrypt failures0 and keyring generation5. The end hard check detected a change; immediate cleanup restored C0 before further diagnostics. Post-failure capture: **generic decrypt failures9**, CRC rejected0, AEAD rejected0, corruption counters0, AEAD ring empty, keyring generation5, index128GiB. Only the AEAD ring was empty; other failure rings were populated. Sampled generation did not change during this attempt; do not blame an observed rotation. Stable generation is not proof that every stored-page key remained usable.

Total attempt accounting:3,778 successful pulls and39 errors,39/3,817 = **1.021745%**, below5%. The **hard integrity policy overrides that availability budget**, so C32 failed. **No qualified60s throughput or CPU efficiency result is reported.** An actual recording bug caused the evidence gap: the wrapper called `hard()` before saving the triggering end60 sample, then a second `hard()` exception in `finally` suppressed drained/result records and obscured the original exception. Missing trigger/end/drained CPU and rings are not reconstructed measurements. C0 proof was saved successfully. Mid expiry ring already had22 total/16 retained; final44/16 with28 overwritten. Final terminal ring82/64 with18 overwritten. Gate45/45 retained. These gaps prevent complete event history. Before any future authorized run, persist each raw snapshot before evaluating its guard and preserve the original exception through cleanup. That is a future recording fix, not permission for more load now.

The generic counter is broader than CRC/AEAD corruption. `cmd/racer-dataplane/src/security.rs:1071-1118` records accepted executed completion outcomes on I/O dequeue, even for abandoned waiters; any outcome other than Completed increments generic failure. Thus9 means nine non-Completed reaped decrypt outcomes, **not nine proved AEAD failures or corrupted pages**. Separate rejection counters require recorded rejection facts (`security.rs:1120-1128`). `runtime/src/offload.rs:1-4,21-22,43-84` retains accepted work/credit through completion; dropping a waiter is not the same as canceling its accepted job. Zero CRC/AEAD proves neither corruption nor benign cancellation. The9 failures remain unresolved. Earlier previous-image decrypt2 remain separate unresolved history.

Next investigation should capture/classify the actual error enum and CryptoId across admission, queue, execution, completion consumption/reaping and abandoned waiters. Do not change crypto algorithms, relax100ms allowance, reset counters, or claim a capacity limit to explain this result. No further load until the hard failure is understood and a separate plan is approved.

## Evidence and validation

Private archive: `tmp/racer-gpu-single-node-reclaim-20261009-artifacts`, with SHA256SUMS, manifest and external archive references. It contains new worktree tmp evidence and this report; prior archives are read-only references, not recursively copied. Earlier blocked-phase archive `tmp/racer-reclaim-measure-20261009-artifacts` remains separate and unchanged.

The original archive is immutable. `final-audit-addendum/` contains this qualification update and its own manifest/seal, referencing the original seal. Raw measurements and the original manifest are not rewritten.

Primary evidence prefixes inside `worktree-tmp/`:

- `ops-isolate03-*`: exact intents/CAS responses, UID deletes, fixed scope, completed API-effect chains, singleton proof and local guards.
- `ops-candidate-deploy-1657-*`: image-only intent/response, expected sources, resolved candidate UID/image, post guard.
- `ops-bench03-old-C8-1653-*`, `ops-bench03-candidate-C8-1700-*`: successful windows and reports.
- `ops-bench03-candidate-C16-1705-*`, `ops-c16-forensics-1710-*`: warm C16, CPU and request-ID joins.
- `ops-bench03-candidate-C32-1715-*`: hard-stop snapshots, ring gaps, C0 and post-failure guards.
- `ops-bench03-candidate-firstload-comparison.json`: normalized C8 comparison.
- `reclaim-build-checkpoint.md`: completed build provenance. `ops-results-audit.py` independently recomputes counts and subdivisions.

Local fake tests covered isolation CAS preservation, singleton canonical hashing, recovery-first ordering, current pod defaults, and benchmark control/cleanup. They did not prevent the three documented baseline wrapper/projection failures; live artifacts are authoritative. Build success is not blanket lint/test success. The requested Go1.26.9 `make fmt` was run once: gofumpt completed, but golangci-lint stopped because another golangci-lint process held its lock (`parallel golangci-lint is running`). No tracked Go changes resulted. This invocation did not reach or reproduce the previously known11 Gantry SA1019 findings. Those remain unresolved prior findings, not a green validation result. Full command/output are archived. This report-only commit does not fix unrelated production code.

The worktree is retained for parent audit. No push, cherry-pick, or removal is part of this reporting phase.
