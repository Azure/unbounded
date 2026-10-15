# Racer second-sight retention: implementation and results

Date: 2026-10-05. All times below are UTC. Environment: `joolshev-scale-test`, namespace `unbounded-system`, 1,500 nodes. This report consolidates saved evidence; its preparation did not run tests, rebuild images, query or mutate the cluster, read Git history, or create commits. The parent owns integration and evidence archival.

## Findings and attribution

- The final C8 window delivered **5.316094 TiB/s of application RX**, with **505.0417 successful image pulls/s**, **23.7319 s mean completed latency**, and **zero newly completed application failures**. Internal request errors remained **0.382322/s**. This is not an error-free dataplane or verified-goodput result.
- Every node exported exactly **11,572,407,598 indexed payload bytes** throughout the observed final window. Nonowned-class disk reads were positive on every node; peer acquisitions and origin fills had zero rate. These are strong reuse observations, not proof of unique full-image coverage or independent payload verification.
- Final RX was **3.0900x** the original 1.720420 TiB/s baseline. **That ratio is not an isolated second-sight feature effect.** The original deployed `5ddb2b08888da08c7535197f492b7602bcbf87d3` substantially predates the implementation base. Saved direct source-snapshot comparison already found 164 dataplane files changed (+54,156/-19,808 lines), including runtime, allocator, HTTP, topology, identity, UDS and checkpoint changes, before second-sight integration (`tmp/deploy-research/REPORT.md:123-133`). No new ancestry/history analysis was performed for this report.
- The narrower **2.243224 -> 5.316094 TiB/s (2.3698x, +136.98%)** comparison is **partial second-sight versus fixed second-sight**, not feature-off versus feature-on. It measures the post-headroom-fix deployment and subsequent warmup under this workload. The saved build checkpoint limits the source change from `4e2a980dd` to `622f8d086` to the optional-reclamation fix and tests; restart/warmup state still differs (`BUILD-CHECKPOINT.md:80-104`).
- Cold owned-page eviction is validated by unit assertions, **not live disk-pressure testing**. Both measured second-sight windows had zero index/segment eviction rates and ample capacity. No live eviction-validation claim is warranted.
- The final image was **locally built and imported**, not confirmed published. Fresh nodes without that import cannot pull it until it is published. Publication is an outstanding operational requirement, not a completed result.

## Implementation semantics

Code citations refer to the reporting worktree, with the headroom fix present. Code is authoritative; historical checkpoint line numbers describe their recorded snapshots.

### Demand, admission and ownership

`RACER_ADMISSION_MODE` defaults to `second-sight`; `disabled` restores owned-only disk admission, not disabled resource admission (`cmd/racer-dataplane/src/config.rs:173-177`; `src/retention.rs:210-218`, where abbreviated `src/` paths in this section are under `cmd/racer-dataplane`). Production installs one shared policy per I/O worker for writer/index/eviction, before read assembly. History is divided among workers; heat capacity is the worker index capacity plus pending queue capacity (`src/app.rs:1060-1076`). The documented default history budget is 4 MiB/node and rotation is 60 seconds (`docs/content/reference/racer.md:72-74`).

Four rotating Bloom generations track **page history only**, approximately 180-240 seconds at the default period. An exact operation-local `Interest` latches the decision before insertion; retries using that interest cannot turn their own first sight into a second sight. Distinct interests, including concurrent followers, can qualify. Bloom false positives can admit extra pages; Bloom state is neither reader deduplication nor authorization (`src/retention.rs:13-29,107-129,185-219`).

Eligibility is current owned classification **or** enabled-and-previously-seen. Ownership is a cache-only lookup against the current published topology; missing or obsolete hints provide no bonus. A bounded resident refresh visits recovered/idle pages as well as foreground demand, without making victim scoring perform cold full-membership ranking (`src/read/candidates.rs:415-499`). Ownership does not pin data or grant origin authority.

Subscription lookup/acquisition probes do not themselves count as additional consuming readers. Assertions establish zero observations after probe/selected acquisition, then one observation per consuming notification and persistence after the second notification (`src/read/tests/fill.rs:562-601`). Memory reuse persists the original ciphertext/envelope without another origin acquisition (`:605-650`). Ready verified results can return without waiting for optional ownership refresh; the background path retains the original observation and may additionally grant only newly current owned eligibility (`src/read/fill.rs:1544-1587`).

Persistence is best effort after verification/metadata validation. Pending and indexed duplicates return **before** counting a persistence attempt. Enqueue acceptance is not durable publication. Dirty and exact aligned staging reservations remain authoritative; enqueue failure cannot change a valid delivered read into failure (`src/read/fill.rs:1592-1674`; `src/store.rs:463-522`). No extra download or encryption is introduced by optional persistence.

### Retention value and bounded reclamation

Resident heat is exact and bounded, saturates at three, and lazily decays by one per minute. Current ownership adds one to the score, a soft preference rather than immunity (`src/retention.rs:95-124,224-265`). Storage guards track pending/index residency, and restore constructs a validated replacement index (`src/store/catalog.rs:221-240,450-470`). History/heat are process-local hints, not checkpoint payload state.

- Index selection inspects at most 64 ordered candidates with a separate cursor, choosing lower score then older publication age without reordering checkpoint traversal (`src/store/catalog.rs:242-264`).
- Whole-segment value sums **live logical payload bytes times score**, not padding or last-read time. It examines at most 256 mappings per segment and conservatively bounds the unseen suffix. Foreground budgets are 64 segment visits and 256 mapping removals; active unpublished writes are excluded from victim selection (`src/store/catalog.rs:522-542,629-649,692-699`). There is no payload compaction.
- Full-queue replacement requires a strictly lower-value queued candidate, and exact staging is reserved before acceptance (`src/store.rs:471-521`). Owned persistence can make one bounded queued-optional reclamation attempt and retry; nonowned reservation failure does not discard queued writes to obtain its own quota (`src/read/fill.rs:1704-1734`).

The unit test actually removes the cold owned mapping while retaining the hot nonowned mapping, rather than merely asserting that owned data is accepted (`src/store/catalog.rs:864-889`). Mixed-segment assertions also remove an owned-containing colder segment while retaining the hot one (`:927-963`). These establish policy semantics, not live pressure behavior.

### Headroom starvation and corrective fix

The partial deployment plateaued near 3.043 GB indexed/node despite 122,459.668 persistence attempts/s and no accepted attempts, publications or pending writes. Four directly sampled nodes across three pools, five workers each and two samples/worker, all had ciphertext usage **832,348,976-882,680,672 bytes**. Each worker's hard limit was **1,288,490,188 bytes**, but its optional ceiling was **644,245,094 bytes** (`tmp/admission-diagnosis/report.md:43-72`).

The former gate compared **all retained ciphertext** to the half-budget optional ceiling before reclaiming idle memory or generic recycled buffers. Ordinary cache residency could therefore remain below the hard quota yet indefinitely above the optional ceiling. More disk capacity would not solve it. Live snapshots satisfy this sufficient rejection condition, but absent per-attempt rejection-reason telemetry prevents attributing every historical failure to that branch. The roughly 51-full-page residency explanation is supported by exact sampled usage, but owner attribution remains an inference (`tmp/admission-diagnosis/report.md:74-115`).

The fix preserves `min(limit, max(limit/2, one_page))`. It first releases idle recycler backing, computes the **optional**, not hard-limit, deficit, performs one idle-memory reclamation pass, releases newly recycled backing, and rechecks actual charged usage before reservation (`src/read/fill.rs:1676-1701`). It neither waits nor relaxes the quota. Arc-owner checks protect active data; the no-op queued-release callback prevents this pass from discarding queued writes (`src/memory.rs:675-720,738-778`). Candidate scans are bounded; subsequent independent interests can advance cursors. Regression assertions cover five full pages above half quota, recycler-only pressure, 256-entry scan progress with exactly one further eviction on the next attempt, and preservation of active/queued owners (`src/read/tests/fill.rs:4-229`).

The historical diagnosis's “not implemented” recommendation and earlier contract notes are time-local, not final status: the implementation handoff records completion at 21:27:36, and the current code contains the fix (`tmp/admission-diagnosis/implementation.md:3-26,53-76`).

## Validation record

These are saved results, not tests rerun while writing this report.

| Scope | Recorded result and qualification |
|---|---|
| Dataplane library, before the final headroom fix | **1,110 passed, 6 ignored**, 120.30 s; `cargo test --lib -- --test-threads=2` with debug=0/incremental=0 and external 300-second TERM bound (`CHECKPOINT.md:7`) |
| Read suite after headroom fix | **232 passed, 0 failed**, including all four added regressions; no later source edits in that handoff (`tmp/admission-diagnosis/implementation.md:53-72`) |
| Focused storage/allocator | Saved storage result 92 passed/1 ignored; allocator segment result 26 passed; optional headroom and delayed-publication/fence regressions recorded (`STORAGE-CHECKPOINT.md:55-68`) |
| Formatting/lint | Scoped Rust formatting and whitespace checks passed. Two introduced Clippy findings were fixed; root Clippy remained **103 library/149 test findings**, classified as baseline by diff-span/current-base inspection, not a separately compiled baseline. Allocator/topology strict Clippy passed (`LINT-CHECKPOINT.md:6-12`) |
| Required repository formatter | Earlier `make fmt VERSION=lint-check` failed because golangci-lint was built with Go 1.26 while source required Go 1.27. No Go edits or toolchain upgrade were made (`LINT-CHECKPOINT.md:8-12`) |
| Final artifact build | Native-RDMA-enabled Linux/amd64 release build and debug-info check passed; not hardware RDMA runtime certification (`BUILD-CHECKPOINT.md:98-105`) |

Do not describe the final image as having a fresh 1,110-pass full-suite run: that suite preceded the headroom correction. The post-fix evidence is the focused 232-test read suite and release build. Ignored tests and baseline/toolchain lint failures remain disclosed.

## Artifact provenance and rollout timeline

Final source revision: `622f8d086887fdc461dc8dcb1700e8fd40ffae8a`.

- Full deployed/local import tag: `ghcr.io/azure/racer-dataplane:second-sight-halfquota-622f8d086-20261005t214640z`.
- Local alias: `racer-dataplane:second-sight-halfquota-622f8d086-20261005t214640z`.
- Local image ID/exported manifest-list digest: `sha256:f86031a4bbbbd4ef7206d4b9f1d5f5ffd5a960fa5b5ebdb8b54b10ccde57036a`.
- Linux/amd64 platform manifest: `sha256:93b8b89fdef0af42f06a5c15edd31c5cb7340eb3b808897497cf760aa4c8ab36`.
- OCI revision/version were checked against explicit build arguments (`BUILD-CHECKPOINT.md:89-104`). These are **local artifact identities**, not confirmed remote publication digests.

| Time | Event and evidence |
|---|---|
| 19:07-20:20 | Implementation, review and pre-headroom-fix validation; worktree began at `1838de507` (`CHECKPOINT.md:3-8`). |
| 20:23-20:26 | Initial `4e2a980dd2eb2f47cc80f5e4762cd72026f4af63` image built. GHCR push authentication/authorization failed; both discovered GitHub credentials lacked `write:packages`; final tag check returned 404 (`BUILD-CHECKPOINT.md:16-64`). |
| 20:26:19-20:31:19 | Original deployed-image baseline. |
| 20:41-20:53 | Isolated new-image/old-peer canary failed, then parent patched back to old image and removed the node cap. Observation 20:42:59-20:47:29 had **0 new successful pulls, 1,879 HTTP-status failures**, only 39,459 received bytes and 2,112 internal request errors. Readiness remained true. PeerHead/Io and CandidateExchange/Io localized the failure chain but did **not** prove mixed-version incompatibility (`tmp/canary/outcome.md:21-61`; `tmp/second-sight-live/checkpoint.jsonl:11-20`). |
| 21:02-21:07 | Fleet C0 drain reached zero in-flight, override updated and 1,500 dataplanes replaced; all Ready by 21:07:17, C8 restored at 21:07:21 (`tmp/second-sight-live/checkpoint.jsonl:21-56`). |
| 21:11:30-21:16:30 | Stable partial-cache measurement on `second-sight-4e2a980dd`. |
| 21:27:36 | Headroom-fix handoff, 232 passing read tests. |
| 21:46:40-21:47:59 | Fix committed and unique final image built by the earlier authorized phase (`BUILD-CHECKPOINT.md:89-106`). |
| 21:55:09 | Final image import/digest/managed-label verification reached **1,500/1,500** nodes. Three prior verification/query timeouts were inspected before recovery; cause remained undetermined, no failures remained (`tmp/image-import-fix/HANDOFF.md:3-18`). |
| 21:57-22:03 | Fixed canary resumed. Indexed payload grew from 3,164,363,054 to 11,572,407,598 bytes by 22:00; 506 nonowned publications added 8,408,044,544 bytes. Observation had +64 successful pulls, zero new loadgen failures, **one internal request error**, and no additional restart (`tmp/fixed-canary/outcome.md:3-18`). |
| 22:03-22:08 | Fleet paused/drained for fixed rollout; 1,499 remaining pods replaced, all 1,500 Ready by 22:08:30; C8 restored at 22:08:36 (`tmp/second-sight-live/checkpoint.jsonl:70-105`). |
| 22:13:30-22:18:30 | Final fixed five-minute measurement. Capture completed 22:19:07 with 147 saved records and no capture errors (`tmp/final-fixed-results/summary.md:70-72`). |
| 22:21-22:27 | One-node C0 drain, restart/recovery and C8 observation; details below. |

**Publication follow-up, 22:35 UTC:** GitHub Actions run **37371027808** succeeded, but publishes the **older `4e2a980dd` source**, not the final correction. The final source `622f8d086887fdc461dc8dcb1700e8fd40ffae8a` was pushed to dedicated branch `racer-second-sight-622f8d086`; its amd64 image workflow [37383148602](https://github.com/Azure/unbounded/actions/runs/37383148602) remained queued, with the exact expected head SHA. No final registry publication is claimed. Its expected tag is `ghcr.io/azure/racer-dataplane:622f8d086887fdc461dc8dcb1700e8fd40ffae8a`, distinct from the locally imported tag. Local import allowed the existing fleet to run; it does not make the deployed tag available to fresh nodes. After successful publication, verify its digest and update the persistent override before relying on fresh-node pulls. The final guard at 22:35:50 confirmed all 1,500 dataplanes Ready on the intended imported image and all 1,500 loadgens at C8.

## Fixed-window comparison

All windows are 300 seconds, with 1,500 loadgens at C8, 12,000 endpoint in-flight operations, one 11-layer synthetic OCI image, layer concurrency 1, and `verify=false`. Effective range window was 2, overriding the ConfigMap's value 1; Gantry remained CPU4/P4. Nominal layer sizing/jitter and live inventory are retained in the evidence, not reconstructed as exact payload identity (`tmp/live-baseline/summary.md:27-43`; `tmp/deploy-research/REPORT.md:113-119`; `CHECKPOINT.md:6`).

| Measurement | Original: 20:26:19-20:31:19 | Partial: 21:11:30-21:16:30 | Fixed: 22:13:30-22:18:30 |
|---|---:|---:|---:|
| Application RX, TiB/s | 1.720420 | 2.243224 | **5.316094** |
| Mean RX/node, GiB/s | 1.174474 | 1.531374 | **3.629120** |
| Successful pulls/s | 162.7333 | 218.2000 | **505.0417** |
| Newly completed failures | 0 | 0 | **0** |
| Mean completed latency, s | 73.2530 | 55.2698 | **23.7319** |
| Interpolated p50 / p95, s | 89.9292 / 118.1973 | 48.2179 / 103.2777 | **22.3370 / 52.0814** |
| Plaintext probe hit fraction | 11.3320% | 9.1036% | **33.1545%** |
| Disk hits/s | 18.9708 | 5,581.8124 | **66,201.1182** |
| Peer acquisitions/s | 14,223.6618 | 13,568.2747 | **0** |
| Origin fills/s; synthetic origin B/s | 0; 0 | 0; 0 | **0; 0** |
| Internal request errors/s | 0.580111 | 0.649207 | **0.382322** |
| Peer decrypt corrupt rejections/s | 0.129327 | 0.170873 | **0** |
| Node CPU busy mean / p95 | 32.087 / 42.503% | 39.164 / 51.677% | **74.994 / 83.288%** |
| Loadgen process CPU, total cores | 560.583 | 732.773 | **1,634.484** |
| eth0 RX / TX, decimal GB/s | 483.900 / 484.513 | 457.802 / 458.327 | **0.031753 / 0.037169** |

Sources: each window's `summary.md`, exact PromQL/query JSON, and `tmp/final-fixed-results/comparison.json`. RX is consumer-delivered body bytes, including partial failed operations, **not physical wire traffic** (`cmd/racer-loadgen/pull.go:425-445`). Verified-byte credit requires a successful operation with verification enabled (`:249-261`); its measured rate was zero in all three windows. **No verified goodput was established.** Source hit events are neither exhaustive consumer-byte attribution nor an end-to-end cache-hit ratio. Completed latency excludes unfinished operations, and histogram quantiles interpolate coarse buckets (`cmd/racer-loadgen/metrics.go:33-45`).

Dataplane/Gantry matched five-minute process CPU is unavailable. Asynchronous metrics API mean cores/pod, ordered dataplane/loadgen/Gantry, were baseline **1.17391/0.37017/0.52469**, partial **1.46550/0.47718/0.67223**, final **2.97542/1.08459/1.49438**. Final sample endpoints were 22:17:44-22:18:07, with individual shorter windows; do not treat these as matched-window CPU measurements (`tmp/final-fixed-results/summary.md:64-68`).

### Index, disk reuse and pressure limits

Partial indexed sum was **4,565,224,261,000 bytes**; per-node min/mean/max **2,627,492,142 / 3,043,482,840.667 / 3,181,140,270**. Final sum was **17,358,611,397,000 bytes**, exactly one series on each of 1,500 distinct nodes, each **11,572,407,598 bytes** at start-offset and endpoint and all five observed in-window samples. Maximum indexed-sample age was 59.932 s (`tmp/final-fixed-results/summary.md:26-33`; `index-validation.json`).

| Storage measurement | Partial | Fixed |
|---|---:|---:|
| Nonowned-class payload reads, B/s | 91,476,821,454.430 | 1,095,513,823,504.794 |
| Owned-class payload reads, B/s | 579,969,800.855 | 2,194,189,983.094 |
| Publications; index evictions; segment evictions, both classes | All zero rate | All zero rate |
| Observations and qualifications, each /s | 162,311.334 | 352,412.066 |
| Persistence attempts / accepted, /s | 122,459.668 / 0 | 0 / 0 |
| Pending writes / payload bytes at endpoint | 0 / 0 | 0 / 0 |
| Heat entries | 277,096 | 1,047,000 |

The partial nonowned rate is **91.477 decimal GB/s, not 915 GB/s**. Final nonowned reads were positive on every node; owned reads on 1,080 nodes, zero on 420. These classes are **event-time cache-only ownership hints**, not immutable insertion classes or proof of actual nonownership. Missing hints classify as nonowned (`src/retention.rs:171-175,294-325`; reference documentation `:140-169`). Payload gauges count logical mapping ownership, not physical allocation or independent content verification. Disk read bytes are payload-path events, not physical device I/O; validation later in fill is a separate boundary.

Capacity was **16 GiB per worker**, five workers, hence **80 GiB logical slab capacity/node**, with exported effective payload capacity **63,921,192,960 bytes/node**, comfortably larger than this image. Sparse logical capacity is not reserved host free space (`tmp/admission-diagnosis/report.md:134-140`). The unchanged half-cap fix enabled admission; it did not enlarge disk capacity. Zero eviction in these windows therefore does not exercise cold-owned eviction under live pressure. The final zero attempt rate is consistent with pre-counter resident dedup, not continuing starvation.

### Coverage and health

All final RX and success counters advanced on all 1,500 nodes. Per-node RX min/p05/median/p95/max was **1.794/2.705/3.903/5.627/6.346 decimal GB/s**. Endpoint-minus-offset success deltas totaled **151,825**, min/median/max **47/103/168**; Prometheus extrapolated increase was **151,512.5069**. Neither is a synchronized completion log. Baseline success `increase[5m]` advanced on 1,499 nodes, but endpoint-offset advanced on all 1,500; retain that scrape-boundary distinction (`tmp/live-baseline/summary.md:29-34`).

Final RX, success, readiness and index series each had five samples/node; observed concurrency stayed 8 and readiness 1. All collected reset queries were zero. Dataplane/loadgen/node-exporter targets were up at all observed samples. Across all pod targets, 6,003/6,004 were continuously observed up and all 6,004 were up at endpoint; the unrelated transient was not attributed.

Final inventory had 1,500 Ready pods/component, zero terminating, **one cumulative dataplane restart**, and zero loadgen/Gantry restarts. The canary's prior exit was 0/Completed, current container start 21:57:18, before the final window. Prometheus restart metrics were absent, not zero. Failure series existed on two nodes with zero window increase, not 1,500 explicit zero-valued error series. Final CRC/AEAD and disk/retained/peer decrypt-corrupt rates were zero with all-node coverage; they do not prove every image byte was verified. Gantry origin-byte series were absent, not zero (`tmp/final-fixed-results/summary.md:53-62`).

## Restart recovery observation

The parent capped node `aks-ddv5-17198779-vmss0000ad` at C0 at 22:21:25, observed C0/zero in-flight at 22:22:13, replaced its dataplane at 22:22:23, and removed the cap at 22:23:02 (`tmp/second-sight-live/checkpoint.jsonl:106-115`). The saved nine-snapshot observation ran **22:23:36.782-22:27:37.485**, 240.702 seconds, at 30-second intervals (`tmp/recovery-final/outcome.md`).

- The initial direct sample had **698 heat entries**, **11,572,407,598 indexed bytes**, zero usage in all five worker ciphertext gauges, and zero request/disk-hit/peer-hit/publication counters. This corroborates recovered page-index residency with application caches initially cold, not 698 freshly verified image pages.
- Index bytes stayed exact and heat entries stayed 698 at endpoints. No new publications, published bytes or sampled index/segment evictions occurred.
- Successful pulls increased **1,740 -> 1,788 (+48)**; cumulative application errors stayed **3,490 -> 3,490**. Final concurrency/in-flight was 8/8. Those historical errors include earlier activity; “zero failures” here means zero new failures in this observation.
- Disk hits rose **0 -> 8,774**; nonowned disk payload reads rose by **144,503,060,374 bytes**, owned by **956,301,312 bytes**. Direct requests rose **0 -> 876** with zero request errors; endpoint disk-index misses/errors were zero.
- Peer hits, bootstraps and page checkout/auth/head/body counters stayed zero, **but peer admission accepted and peer verified responses each rose by 13**. Their cause was not investigated. Claiming zero peer activity would be false.
- New pod `racer-dataplane-x8jv6` stayed Ready on the expected image, restart count zero. Application memory hits rose to **13,966**; dataplane memory rose **10 MiB -> 9,221 MiB** and CPU **252m -> 3,263m** in asynchronous `kubectl top` samples. Caches warmed during the observation.

This demonstrates restart/index recovery followed by successful C8 reuse without new disk publication or peer-hit/page-fetch increments. It is **not a physical cold-disk benchmark**: no host page-cache/device-cache flush or reboot was established. The whole interval is not RAM-cold or disk-only, its exact C8 start is obscured by asynchronous direct/control/Prometheus sampling, and it establishes neither saturation nor throughput superiority. Verified bytes remained zero.

The exact measured index size **11,572,407,598** exceeds the earlier theoretical image calculation **11,572,407,586** by **12 bytes** (`designs/racer-large-image-benchmark-20261005.md:16`; `tmp/fixed-canary/outcome.md:8`). The difference is **unexplained**. Do not round it away, assign it to an invented metadata object, or equate aggregate bytes with verified unique-page coverage.

## Cleanup, evidence and remaining work

The user/parent reports approximately **108 GiB reclaimed** in the authorized build-artifact cleanup, with source, Git metadata, worktrees, notes and result evidence preserved. This is not 108 GiB of application cache eviction. The independently recorded worktree cleanup portions were **28.8944 GiB** of generated targets and **55.9951 GiB** of selected debug/release profiles; filesystem-wide free-space deltas differ because of concurrent activity (`WORKTREE-TARGET-CLEANUP-CHECKPOINT.md:51-67`; `WORKTREE-TARGET-CLEANUP-PHASE2-CHECKPOINT.md:23-35`). Preserve that distinction rather than attributing the entire parent total to either subphase. No cleanup was performed while preparing this report.

Primary evidence, relative to the existing worktree:

- `tmp/live-baseline/`: original fixed-window PromQL, raw responses, inventory, CPU and supplemental checkpoints.
- `tmp/fleet-results/`: partial-cache window, distributions, per-node progress and rejected/successful query records.
- `tmp/admission-diagnosis/`: raw per-worker quota snapshots, exact config, causal limitations, implementation handoff and post-fix test record.
- `tmp/final-fixed-results/`: final query set/raw responses, comparison, per-node progress, exact-index validation, inventory/review and checkpoints.
- `tmp/recovery-final/`: nine raw command snapshots, endpoint deltas, outcome and heartbeat journal.
- `tmp/canary/`, `tmp/fixed-canary/`, `tmp/second-sight-live/`, `tmp/image-import-fix/` and root `*-CHECKPOINT.md`: failed/fixed canaries, mutation timeline, import verification and build/test/cleanup provenance.

Evidence was archived to project-local `tmp/racer-second-sight-evidence-20261005.tar.gz`: **14,078,571 bytes**, SHA-256 **`243003ca0f0202a7006291297cfda59e4bfe97c183be3d0a3f5cc840a86056f8`**. Its 533 files include raw queries, inventories, implementation/review checkpoints, and operational scripts. Two reproducible image-export tar files and two Python bytecode files were excluded. Exact file coverage, per-file hashes, and gzip integrity were verified before cleanup. The archive preserves the report before this publication/archive follow-up; this committed document is authoritative for the follow-up.

Outstanding claims require separate evidence: publish the **final** image, run a matched-source disabled/enabled comparison to isolate feature effect, measure independently verified goodput, and exercise constrained live disk/index pressure before claiming live eviction validation. The unexplained 12-byte difference, internal errors and 13 recovery peer events remain open observations, not silently resolved findings.
