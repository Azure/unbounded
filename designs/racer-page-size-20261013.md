# Racer page-size experiment: 4 MiB versus 16 MiB

**2026-10-13. Both six-window legs complete; no qualified steady-state
winner. Original 16 MiB images and C16 restored.**

This report uses saved measurements, an independent offline audit and the
parent's restoration handoff, not new live calls. `P` means the checkpoint at
`/home/azureuser/code/unbounded/tmp/racer-page-size-checkpoint.md` as read at
17:54 UTC. Other `tmp/` citations refer to `.worktrees/racer-page-size`.
Commit-qualified source citations name exact snapshots, not this worktree's
Rust sources. Local evidence archive location:
`tmp/racer-page-size-evidence-20261013.tar.gz`, with verification receipt
`tmp/racer-page-size-evidence-20261013.receipt.json` in the original repository.
The receipt must report `verified: true` before either worktree is removed.
Archive paths `.worktrees/racer-page-size/tmp/` and
`.worktrees/racer-page-size-dataplane/tmp/` preserve the two worktrees' evidence.

## Question and controls

The request is to measure object tail latency with 4 MiB pages instead of
16 MiB, keeping 1 GiB objects, C16 and two SDK page credits (`P:70-74`).

| Geometry | 16 MiB baseline | 4 MiB candidate |
| --- | ---: | ---: |
| Pages per 1 GiB object | 64 | 256 |
| SDK page credits | 2 | 2 |
| Initial SDK byte credits | 32 MiB | 8 MiB |

The SDK fixes `pageCredits=2` and sends `pageCredits * PageSize` bytes
(`pkg/racersdk/client.go:45-55,370-379`). The candidate wire constant is 4 MiB
(`pkg/racersdk/internal/wire/wire.go:24-34`); the Rust candidate agrees
(`011a6b143:cmd/racer-dataplane/object-wire/src/model.rs:150,275-299`).
**The smaller byte window is a confound, not a fixed-byte-window comparison.**
Page count, placement units and cache packing also change; equal budgets do
not imply equal effective capacity (`tmp/page-contract-report.md:49-53,67-71`).

Shared controls: direct UDS, 16,384 objects, seed `zipf-balanced-v2`,
Zipf 1.1, one blob per operation, two-minute pull timeout, C16 after startup,
unchanged budgets and RX1024/TX256. Startup uses fallback concurrency zero.
Sources: `tmp/page-live-report.md:21-26,34`,
`tmp/page-config-b16-01.racer-loadgen.merge.json:10-28`, `P:47`.
Observed membership implied HTTP transport, not a hardware-level RDMA proof;
native slot capacity was preserved (`tmp/page-rollout-plan.md:11-14`,
`tmp/page-review-checkpoint.md:18`).

## Separate source and image identities

Main/`racer-v2` remains untouched at
`948928abd15913ca2f489d3f7195920f64104acf` (`948928abd`). Experimental changes
stay on the separate branches; no cherry-pick (`P:59-60,72,137`).

| Component | Baseline image source | Candidate branch | Candidate build SHA | Build run |
| --- | --- | --- | --- | --- |
| Loadgen/Go SDK | `55f74010e0e3e78f59660fb0807b445f05f0ff48` | [ops/racer-page-size](https://github.com/Azure/unbounded/tree/ops/racer-page-size) | `c543afe81a4efe0e3e2da298184eb632db095c56` | `38420615953` |
| Dataplane | `f9a088a22a29cf8549e1945af833f508a14199d1` | [ops/racer-page-size-dataplane](https://github.com/Azure/unbounded/tree/ops/racer-page-size-dataplane) | `011a6b143fe5af73979c5c4cd9fe643946f2dbb7` | `38420661614` |

The Go branch starts at `948928abd`, whose checked loadgen/SDK/build inputs
differ from deployed `55f74010e` only by an SDK credit regression test. The
Rust branch starts at deployed `f9a088a22`, not the newer main snapshot:
that newer snapshot includes unrelated changes and checkpoint v4 versus the
deployed v3. These are separate source baselines, not one shared build SHA.
Sources: `tmp/page-sdk-checkpoint.md:5-7`,
`tmp/page-live-report.md:61-71`, `tmp/page-review-checkpoint.md:17,80-86`.
The Go input comparison was also checked locally for this draft.

Image references use `ghcr.io/azure/racer-loadgen` and
`ghcr.io/azure/racer-dataplane`, respectively:

| Component | Leg | Immutable digest |
| --- | --- | --- |
| Loadgen | 16 MiB | `sha256:a7b5b9e0e0babe3b42c935fba4d32d1c59de22138fbdb0e519c5a2097beb3031` |
| Dataplane | 16 MiB | `sha256:a5ee91a9ec1a913cfe1d5e26a7a6235c9eac04969e52747594b66e6b35108a9c` |
| Loadgen | 4 MiB, linux/amd64 | `sha256:012b8318cd8650548136b97992b570557e4db6cb031fdb475f4d1714f21840e8` |
| Dataplane | 4 MiB, linux/amd64 | `sha256:89064233cf83e1ecb3685c4146bc41cc9817713e303e222b1f675ee185cb0a8e` |

Baseline identities are saved observations (`tmp/page-live-report.md:18-19`).
Candidate build success and digests are parent-recorded (`P:45,52-54`), not
registry provenance independently checked by this report author.

## Approved cold resets, not cache-preserving rollback

The user approved benchmark cache invalidation and repopulation (`P:59`).
Each leg uses a fresh checkpoint directory on the same approved raw devices:

- Baseline: `/var/lib/racer/slabs/page-experiment-20261013-b16-01`.
- Candidate: `/var/lib/racer/slabs/page-experiment-20261013-c4-01`.
- Restoration: `/var/lib/racer/slabs/page-experiment-20261013-restored16-01`
  (`tmp/page-config-r16-01.summary.json:4`, `P:137,146-149`).

Paths must be absent on all 100 hosts before use. Do not reuse old checkpoint
directories after writes: fresh allocation can overwrite their raw extents.
No wipe, reformat or checkpoint-file deletion is required. Restoration means
baseline images plus another cold reset and rewarming, not restoration of the
old warm cache (`tmp/page-rollout-plan.md:28-48,156-158`).

This is a compatibility boundary: page lengths derive from the local constant
and page identity has no size field
(`011a6b143:cmd/racer-dataplane/object-wire/src/model.rs:139-150,479-498`).
Envelope validation rejects plaintext above that constant; failed record
parsing invalidates mappings (`f9a088a22:cmd/racer-dataplane/src/model.rs:34-43`,
`f9a088a22:cmd/racer-dataplane/src/store.rs:231-237`; deployed-source confirmation at
`tmp/page-contract-report.md:114-124`). Homogeneous dataplanes and SDK
client/origins, with verified all-stop barriers between legs, are required.

## Six paired windows: exploratory cold-start results

Baseline T0 was 15:54 UTC; candidate T0 was 16:52 UTC after confirmed fleet
C16 (`P:3-6,114-116`). After five minutes of initial warmup, each leg supplied
six disjoint five-minute windows at ages 5-35 minutes: baseline 15:59-16:29,
candidate 16:57-17:27. These are **nonsteady observations**, not accepted
steady measurement windows. Counts and error increases are Prometheus
estimates. Each pair below is **16 MiB / 4 MiB**.

| Window | Successful reads/s | Successful >10 s share | Histogram p99.9, s | DP request-error increase |
| --- | ---: | ---: | ---: | ---: |
| 1 | 470.242 / 437.838 | 0.307466% / 0.240767% | 23.495 / 21.693 | 1.0953 / 3.3670 |
| 2 | 494.879 / 450.738 | 0.353622% / 0.171016% | 24.344 / 18.305 | 2.3822 / 8.3851 |
| 3 | 514.158 / 465.350 | 0.356570% / 0.090434% | 24.391 / 9.998 | 0 / 6.9397 |
| 4 | 523.938 / 479.121 | 0.371387% / 0.055657% | 24.615 / 9.993 | 0 / 4.6058 |
| 5 | 540.729 / 486.387 | 0.362936% / 0.024843% | 24.489 / 9.987 | 1.1562 / 3.3281 |
| 6 | 548.317 / 493.054 | 0.341956% / 0.009296% | 24.151 / 9.984 | 0 / 7.8025 |

Sources: `tmp/finalanalysis.md:20-31`; independent raw-data checks in
`tmp/page-final-independent-audit.json:4-14,112-123,221-229,327-335,433-443,541-549`
and `:669-681,779-794,892-907,1005-1018,1116-1128,1226-1242`.
The audit recomputes counts and histogram quantiles and checks agreement with
the comparison (`tmp/page_final_audit.py:17-42,65-75`); it was not rerun here.

### All six windows pooled, 1,800 seconds per leg

| Measure | 16 MiB | 4 MiB |
| --- | ---: | ---: |
| Successful reads | 927,678.75 | 843,746.25 |
| Successful reads/s | 515.377 | 468.748 |
| Successful >10 s count | 3,243.75 | 803.75 |
| Successful >10 s share | 0.349663% | 0.095260% |
| Histogram p99.9 | 24.280 s | 9.999 s |
| Histogram median | 2.901 s | 3.601 s |
| Origin/received byte ratio | 0.474048% | 0.458308% |

The candidate's successful >10 s share fell **72.76%**, but successful
throughput fell **9.05%**, beyond the unchanged 3% loss guardrail. Every paired
window lost more than 3% throughput. The median worsened despite the smaller
tail. Quantiles pool bucket counts, not node/window percentiles; coarse
interpolation near the 10 s boundary is not precise latency evidence.
Sources: `tmp/page-exploratory-06.json:9865-9975`, `tmp/finalanalysis.md:24-49`.
No duration-sum metric was saved, so there is **no measured arithmetic mean**;
the median is not a substitute (`tmp/finalanalysis.md:51-61`).

### Why neither leg qualifies

Both legs exhausted the unchanged six-window stability budget. The final
three throughput max/min ratios passed 1.05: baseline 1.046531, candidate
1.029081. Both origin-ratio spans failed 0.001: baseline 0.001370, candidate
0.001248. Baseline window 5 and all six candidate windows also failed the
zero-new-DP-errors gate (`tmp/page-baseline-stability-06.json:1-17,448-454`,
`tmp/page-candidate-stability-06.json:1-25`, `tmp/finalanalysis.md:13-18`).
No accepted steady measurement followed; the last two windows cannot be
relabeled as accepted measurements.

The parent chose the exploratory equal-age scope **before candidate results**
(`P:87-92`). No budget or threshold was relaxed. The comparison explicitly
sets `accepted_winner=false`; its empty `qualification_failures` list is not
steady-state approval (`tmp/page-exploratory-06.json:1-8,9933-9975`).

## Errors, cache state and limits

DP request-error increases totaled **4.634 on 4 nodes** for baseline and
**34.428 on 28 nodes** for candidate, about 7.43 times higher. **Cause is
unknown.** These are request-lifecycle non-success counts, not necessarily
failed 1 GiB operations or corruption. There are no cause labels, so neither
page size nor a retry/cancellation mechanism is established as the cause
(`tmp/page-final-independent-audit.json:648-664,1341-1380`,
`tmp/page-window01-assessment.md:36-45`). Returned integrity counters had full
100-node coverage and zero increases; this is not full-object verification.
Absent loadgen failure-reason series remain unknown (`tmp/finalanalysis.md:63-73`).

Both caches kept filling. Indexed payload rose from 36.362 to 64.312 TB for
baseline and 36.759 to 61.740 TB for candidate (decimal). Effective payload
capacity was 212.374 versus 265.468 TB, 25% higher for candidate. Similar final
origin ratios do not prove equal cache contents or stability. The **32 MiB
versus 8 MiB two-credit window** remains a confound alongside packing and
placement changes (`tmp/finalanalysis.md:75-89`).

The saved windows report stable identities, sampled C16/health, zero
restarts and no returned resets. These are not continuous-readiness proofs.
DP process and cAdvisor CPU/RSS are absent, not zero or evidence of equal
resource use (`tmp/finalanalysis.md:91`). The earlier old-warm smoke is not a
matched baseline (`tmp/page-metrics-report.md:44-73`).

**Conclusion:** 4 MiB reduced successful long-tail share during equal-age
cold-cache warmup, but lost throughput and showed more unclassified DP
errors. This is an exploratory tradeoff, not a qualified steady-state winner.
The parent restored the original 16 MiB images and C16 load.

## Final state: original 16 MiB images and C16 restored

The parent completed C0 drain, all-stop barriers and original-image patches
before the restored16 cold gate (`P:138-149`). All 100 original-image
dataplanes passed: 300 raw devices, 21 workers per node, zero indexed/used/
pending payload (`tmp/page-cold-r16-01.json:1181-1209`). The fresh directory is
`/var/lib/racer/slabs/page-experiment-20261013-restored16-01`; **old indices
were not reused**. This restores cold cache operation, not the discarded
warm cache (`tmp/page-config-r16-01.summary.json:4`, `P:137,149`).

Loadgen/catalog startup began at 17:39 UTC under C0. After readiness and two
C0 health passes (`P:156-158`), the saved applied receipt confirmed all 100
loadgens at C16 through both direct and Prometheus checks at **17:53:16 UTC**,
with stable UIDs (`tmp/page-r16-applied16-01.json:4-5,5428-5440`).

Final health at **17:53:24-38 UTC** passed for all 200 pods with stable UIDs
and consistent membership 97. Both original image digests listed above are
active (`tmp/page-r16-final-health.json:4,12-57,36036-36064`). The experiment's
stop gates are removed and **no C0 recovery remains pending** (`P:159-161`).
The fresh `restored16-01` cache is warming; the discarded warm cache was not
recovered. This is restoration of running load, not proof of steady cache
state or a new accepted performance window.

One operational difference remains: the loadgen CLI fallback is still
`--concurrency=0`, while the control file supplies 16. Effective C16 is
confirmed above; the original startup fallback was 16. The retained zero
fallback was used for safe startup in both legs
(`tmp/page-config-r16-01.racer-loadgen.merge.json:20-21`,
`tmp/page-live-report.md:21-22`, `tmp/page-rollout-plan.md:98`). This report
performed no operational steps.

## Verification and tests: limited evidence, not all green

Both measurement legs use `verify=false`; the chosen protocol has no separate
live SHA smoke (`P:47`, `tmp/page-rollout-plan.md:118`). The unverified path
drains bytes without hashing, while verification creates a SHA-256 hasher
(`cmd/racer-loadgen/pull.go:428-475`). Successful reads, readiness and live
error counters do **not** establish end-to-end content verification or
verified goodput.

Recorded test results, not rerun for this draft:

- Go SDK, loadgen, Gantry and racerobject suites and scoped formatting passed
  after fixing one stale range fixture (`tmp/page-sdk-checkpoint.md:41-62`).
  The 1 GiB wire test checks 256 page frames plus completion and old geometry
  rejection, not a payload digest
  (`pkg/racersdk/internal/wire/page_size_test.go:12-56`).
- Rust all-target compile, focused wire/conformance, actual Go SDK interop,
  selected production paths and interrupted/periodic restart checks passed,
  as recorded in `tmp/page-review-checkpoint.md:23-24` and `P:53`.
- **Two known failures remain:** adaptive-peer idle-retirement assertion and
  graceful-restart key-ID assertion. Review records unchanged,
  geometry-independent source, not successful baseline runtime retests.
  Graceful offline reopen was not reached
  (`tmp/page-review-checkpoint.md:25-26`).

Full-suite success, full 1 GiB live digest verification, native RDMA hardware
validation and end-to-end old-checkpoint reopen are not established.

## Unchanged acceptance rule and remaining evidence

The original steady-state plan requires five minutes of initial C16 warmup,
then at most six disjoint five-minute windows to seek three consecutive
healthy windows:
origin/received bytes <=1%, throughput max/min <=1.05, and origin-ratio span
<=0.001. Only after passing, collect two measurement windows per leg. Require
matching identities, complete fresh coverage, no observed resets and no new
errors. Both legs failed, so there is no accepted steady-state comparison
(`P:47,137`, `tmp/page-rollout-plan.md:138-154`).

Primary metric: pooled successful-object >10 s share. Secondary: coarse
histogram p99.9. The predeclared screen is >=20% relative tail reduction with
<=3% throughput loss, not a statistical significance claim. Histograms have
10 s and 30 s bounds, and successful-tail queries exclude failed/unfinished
operations (`cmd/racer-loadgen/metrics.go:33-43`,
`tmp/page_metrics.py:46-57,112-118`). Report failures separately.

| Evidence item | Status |
| --- | --- |
| Both C16 T0 anchors and six-window legs | Recorded; both stability budgets failed |
| Exploratory comparison and independent audit | Complete; no qualified steady winner |
| DP error cause and missing resource data | Unresolved; no causal/resource-equivalence claim |
| Original images and fresh restored16 cold gate | Passed; old warm cache not restored |
| Final loadgen readiness, health and C16 receipt | Passed at 17:53 UTC; no C0 recovery pending |
| Evidence archive and manifest | Require verified receipt at the location above before cleanup |
