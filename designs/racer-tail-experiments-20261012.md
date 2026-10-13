# Racer tail experiments: October 12-13, 2026

**Both experiments are complete and rejected under the stated acceptance rule.**
The final recorded state is healthy C16 with baseline images, RX1024, and no
pending ring guards. This report uses saved evidence through the October 13
00:38Z parent checkpoint, not fresh live checks. Times below are UTC.

## Decision and method

Reject one-page SDK delivery for this workload. Both matched windows lost
throughput and had a larger >10s tail. Production source is back to two page
credits, the baseline loadgen image is restored, and the useful credit regression
test remains. RX2048 sharply reduced measured no-WQE discards but did not meet
the repeatable tail target. All 20 treatment nodes were restored to RX1024;
RX4096 was not tried. This does not establish that RX2048 has no benefit.
Sources: `tmp/onepage-comparison.json:56-64`, `pkg/racersdk/client.go:53-54`,
`tmp/ring2048-decision.md:3-5,41-47`, and restoration evidence below.

The predefined acceptance rule was a repeatable **at least 20% relative reduction
in successful completed-read share above 10 seconds**, with **no more than 3%
throughput loss**, unchanged C16, and no new error, integrity, resource, or
readiness regression. This rule predates outcomes; it was not adjusted to accept
either experiment. Source: original checkout
`tmp/racer-tail-experiments-checkpoint.md`, October 12 21:36Z section.

The one-page comparison used the same 100 nodes, C16 per loadgen, five minutes of
warmup after confirmed application of C16, and two disjoint five-minute windows
per version. Queries used pinned end times after at least 60 seconds of scrape
lag. Baseline ran first, candidate second; these are matched measurement
protocols, not randomized or concurrent controls. Only the loadgen image changed;
the dataplane image remained `f9a088a22`, and rings were unchanged. Baseline
loadgen was `55f74010e`; candidate was `07a30db74`. Sources:
`tmp/onepage-comparison.json:3-8,67-69`; original checkout
`tmp/racer-tail-experiments-checkpoint.md`, October 12 21:52Z, 22:05:47Z,
22:09Z, and 22:44Z sections.

The primary tail measure is `(successful count - successful le10 bucket) /
successful count`. Pooled results sum counts and buckets, rather than averaging
node ratios or percentiles. Throughput is successful completed operations per
second. Prometheus increases are estimates, hence fractional event counts.
The implementation selects `result="success"`; failed, canceled, and unfinished
operations are not part of these latency distributions
(`tmp/experiment_metrics.py:145-177`, `cmd/racer-loadgen/pull.go:259-282,314-343`).

## Completed one-page results

| Version/window | Time | Successful reads/s | >10s share | Approx. >10s count | p99.9, seconds |
| --- | --- | ---: | ---: | ---: | ---: |
| Two-page baseline 01 | 22:14-22:19 | 636.538 | 0.161027% | 307.50 | 17.580 |
| Two-page baseline 02 | 22:19-22:24 | 637.675 | 0.138524% | 265.00 | 15.562 |
| One-page candidate 01 | 22:49-22:54 | 496.646 | 0.544486% | 811.25 | 26.327 |
| One-page candidate 02 | 22:54-22:59 | 499.771 | 0.531910% | 797.50 | 26.240 |

Exact values: `tmp/onepage-comparison.json:10-54`. Raw count and le10 evidence:
`tmp/baseline-window-01.json:13-20,97-104`,
`tmp/baseline-window-02.json:13-20,97-104`,
`tmp/candidate-window-01.json:13-20,97-104`, and
`tmp/candidate-window-02.json:13-20,97-104`.

Combined over 600 seconds per version:

- Throughput: **637.106 -> 498.208 reads/s, down 21.80%**.
- >10s share: **0.149766% -> 0.538178%, up 259.35%** relative, or 0.388412
  percentage points. Approximate counts: 572.5/382,263.75 -> 1,608.75/298,925.
- Pooled p99: 9.744 -> 9.882 seconds. Pooled p99.9: 16.646 -> 26.284 seconds,
  up 57.90%. These are histogram interpolations, not exact observed percentiles.
- Each candidate window separately failed both performance targets: throughput
  fell 21.98% and 21.63%; >10s share rose 238.13% and 283.98%.

Source: `tmp/onepage-comparison.json:56-64`. The repeated regression supports
rejection in this setup, not a universal causal claim about one-page credits.
Sequential timing, restarts, cache state, and traversal drift remain possible
confounders. Adjacent windows are not independent randomized trials.

### Guardrails and measurement limits

- All four windows return C16 and in-flight 16 on 100 nodes, fleet mean in-flight
  1,600, 100 ready/live dataplanes, and 21 workers per node. Saved loaded-health
  snapshots pass health, membership, and stable-UID checks, with both applications
  100/100 ready and zero restarts. These snapshots precede the windows; neither
  endpoint gauges nor means prove uninterrupted health or load.
- Dataplane request-error increases were baseline 0/1.25 and candidate 0/0.
  Only `success` loadgen outcome series were returned; error/canceled and failure
  reasons were absent, not measured zero. Failure series are created on events
  (`cmd/racer-loadgen/failures.go:122-128`), and the success test explicitly expects
  no failure series (`cmd/racer-loadgen/pull_test.go:154-157`). Absence is consistent
  with no observed failures after restart, not proof of complete error telemetry.
- Six corruption/CRC/AEAD queries returned zero increases on all 100 nodes.
  **Verified-byte rate was zero: no end-to-end SHA-256 verification is claimed.**
  That counter is credited only for successful operations with verification
  enabled (`cmd/racer-loadgen/pull.go:265-276`).
- Loadgen CPU fell from about 160.6 to 123.4 fleet cores alongside throughput.
  CPU seconds per successful read changed only from about 0.252 to 0.248; median
  RSS fell about 3 MiB. No aggregate CPU or available-memory pressure regression
  was visible, but this does not offset the performance loss.
- Process metrics cover loadgen, not dataplane. Host CPU includes other work;
  memory gauges are endpoints, not peaks. Disk and network-drop rates are missing,
  not zero. Layered network interfaces may double-count traffic.

Artifact evidence: `tmp/onepage-comparison.json:66-81`. Histograms have a wide
10-30 second bucket, so the direct >10s share is stronger evidence than precise
p99.9 decimals. Received bytes are application-consumed body bytes, including
failed operations, not wire traffic or verified goodput. Request counts are blob
acquisitions, not SDK pages (`cmd/racer-loadgen/metrics.go:33-43`).

### Reversal and retained test

Current source sets `pageCredits = 2` and uses it for both page and byte credit
headers (`pkg/racersdk/client.go:53-54,370-375`). No production one-page change
remains. `TestGetPageCredits` remains: it checks headers for two pages and
`2*PageSize` bytes, sends both boundary pages, checks each exact credit release
before completion, and requires `abc` through both Read and WriteTo. It does
**not** assert that a third page blocks (`pkg/racersdk/client_test.go:166-225`).
The recorded scoped formatter and full race-enabled SDK suite passed after
restoration (`tmp/onepage-checkpoint.md:66-79`); this documentation pass did not
rerun them.

The parent records baseline image `55f74010e` restored, all 100 loadgens ready,
two C0 health gates passed, and C16 confirmed on all 100 direct and Prometheus
observations at 23:24:48. Rings were 1024 with no pending rollback timers at the
23:25 checkpoint. Sources: original checkout
`tmp/racer-tail-experiments-checkpoint.md`, October 12 23:21Z/23:25Z sections;
saved receipts
`tmp/restored-ready-health-02.json:12-34,35080` and
`tmp/restored-applied-16-confirm.json:5440`.

## Completed RX2048 safety canary

Under drained C0, node `aks-ddsv6-10576182-vmss00000g` had RX changed from 1024 to
2048 with a host-local 180-second automatic rollback to 1024. The setter returned
success. The timer fired at 23:09:09; rollback returned success and the getter
showed RX1024/TX256. The parent verified channels32 and both links UP, then full
application C0 health. The recorded service had finished with no PID, queued job,
or future expiry; transient units remained. Sources:
`tmp/ring-canary-arm.jsonl:1-5`, `tmp/ring-canary-status.jsonl:1-5`,
`tmp/ring-canary-logs.jsonl:4-5`, `tmp/canary-restored-health.json:1-34,35080`, and
original checkout `tmp/racer-tail-experiments-checkpoint.md`, October 12
23:06Z/23:12Z sections.

This validates the successful setter/reversal path on one drained node, not tail
improvement, failure-path recovery, or safety of a fleet rollout. The helper
receipts explicitly do not certify application health; the separate health gate
does that. Independent ARM/serial recovery was not verified. The parent accepted
the residual one-node isolation risk; a local timer cannot guarantee recovery
from a stuck kernel call or failed device reattach. Exact installed setter source
was unavailable, and synthetic-network topology does not establish RX failover.
Sources: `tmp/ring-recovery.md:7-14,77-89`, `tmp/ring-checkpoint.md:58-78`, and
original checkout `tmp/racer-tail-experiments-checkpoint.md`, October 12 23:06Z
section.

## Completed controlled RX2048 experiment

### Assignment and strict acceptance rule

Twenty treatment/control pairs were selected from 100 eligible nodes, with seed
`20261012`, and saved at October 12 23:41:11 before intervention. The code ranks
nodes by baseline window 02's >10s share, makes adjacent pairs, samples 20 pairs,
and randomly assigns one treatment per pair. It does not match on throughput or
NIC loss (`tmp/experiment_control.py:200-229`, `tmp/ring-assignment.json:1-9`).
The assignment test checks deterministic output, 40 unique nodes, and one node
per arm in each pair (`tmp/experiment_control_test.py:47-58`).
All 20 nodes in each arm remained in the primary comparison. Controls stayed at
RX1024 in the saved host samples; treatment used RX2048, with TX256 and 32 channels
unchanged (`tmp/ring2048-decision.md:9-11`, `tmp/ring2048-nic-review.md:19-23`).

The restored two-page loadgen image stayed fixed. Baseline warmup was 23:25-23:30;
baseline windows were 23:30-23:35 and 23:35-23:40. Treatment followed a C0 drain,
five guarded four-node batches, health gates, confirmed C16, and warmup
23:51-23:56. Treatment windows were 23:56-00:01 and 00:01-00:06. Queries waited
at least 60 seconds after each end. Source: original checkout
`tmp/racer-tail-experiments-checkpoint.md`, October 12 23:25Z, 23:42Z, 23:48Z,
and 23:51Z sections.

The analyzer's strict screen requires **each of at least two windows** to show
at least 20% reduction in both raw treatment tail and control-adjusted tail.
For successful-read and received-byte throughput, treatment, control, and the
control-adjusted ratio must each remain at least 0.97 of baseline. Passing this
screen only permits guardrail review, not automatic acceptance
(`tmp/ring_analysis.py:225-260`). Tests explicitly reject shared control
improvement, a 4% throughput loss in either arm, and a single-window trial;
zero-baseline tail ratios stay undefined (`tmp/ring_analysis_test.py:38-51`).

Here, control-adjusted reduction means
`1 - (T_after/T_before)/(C_after/C_before)`. Combined tails pool counts, not
window or node percentages (`tmp/ring_analysis.py:60-88,137-150`).

### Application results

| Comparison | Treatment >10s share, before -> after | Control >10s share, before -> after | Raw treatment reduction | Adjusted reduction |
| --- | ---: | ---: | ---: | ---: |
| Window 01 | 0.144998% -> 0.118309% | 0.209246% -> 0.173862% | 18.41% | **1.80%: fail** |
| Window 02 | 0.170183% -> 0.124517% | 0.165992% -> 0.171402% | 26.83% | **29.14%: pass** |
| Combined 600 seconds | 0.157612% -> 0.121415% | 0.187784% -> 0.172635% | 22.97% | **16.21%: below target** |

| Window/arm | Approx. successes, before -> after | Approx. >10s count, before -> after | Reads/s, before -> after |
| --- | ---: | ---: | ---: |
| 01 treatment | 38,793.75 -> 39,092.50 | 56.25 -> 46.25 | 129.3125 -> 130.3083 |
| 01 control | 38,232.50 -> 38,105.00 | 80.00 -> 66.25 | 127.4417 -> 127.0167 |
| 02 treatment | 38,928.75 -> 39,151.25 | 66.25 -> 48.75 | 129.7625 -> 130.5042 |
| 02 control | 37,652.50 -> 37,922.50 | 62.50 -> 65.00 | 125.5083 -> 126.4083 |

Both throughput checks passed. Adjusted changes were +1.1073% and -0.1445%,
combined +0.4820%; success and byte-throughput changes agree. Combined treatment
p99.9 moved 17.3106 -> 13.5276 seconds, control 19.3495 -> 18.4149 seconds, an
adjusted reduction of 17.89%. Wide-bucket interpolation limits this precision.
Sources: `tmp/ring2048-analysis.json:2513-2635,5146-5268`,
`tmp/ring2048-decision.md:13-30`. The saved analyzer reports
`performance_screen_pass=false` (`tmp/ring2048-analysis.json:315776-315834`).

Window 01's control improvement explains most of its apparent treatment gain.
Window 02 is encouraging, but neither repeatability nor the combined adjusted
20% target was established. RX2048 was rejected under the stated acceptance rule,
**not shown to have no benefit** (`tmp/ring2048-decision.md:3-5,41-47`).

### NIC mechanism, resources, and coverage

| Short sample | Treatment no-WQE discards | Treatment discards/received unicast | Control discards/received unicast | Adjusted reduction |
| --- | ---: | ---: | ---: | ---: |
| Baseline | 85,410 | 0.028090% | 0.026798% | Not applicable |
| RX2048 sample 01 | 973 | 0.000319% | 0.027358% | 98.89% |
| RX2048 sample 02 | 3,025 | 0.001001% | 0.025599% | 96.27% |

Raw normalized treatment reductions were 98.86% and 96.44%, with similar receive
exposure; all 20 treatment nodes improved in both samples. This is strong evidence
of improvement in the measured no-WQE mechanism, not elimination of all loss or
proof of whole-read latency benefit. Residual discards remained concentrated on
some nodes (`tmp/ring2048-nic-review.md:27-44`).
No-WQE/received-unicast is a traffic scale ratio, not a verified total-loss
probability; it does not include every source of packet loss.

Each host sample spans only about 10.02-10.05 seconds; the 40 hosts were collected
in batches over about 43 seconds, not simultaneously or across a full application
window. NIC, TCP, CPU, and disk families validated in all three samples. Softnet
was valid on 39/40, 39/40, and 40/40 nodes. Two counter decreases were consistent
with wraps but were not silently corrected or counted as zero. NIC subtraction
stayed within each stable sample epoch, never across a ring reset
(`tmp/ring2048-nic-review.md:13-25`). The sampler marks decreased counters invalid,
as its reset test requires (`tmp/ring_sample.py:90-98`,
`tmp/ring_sample_test.py:26-30`).

- **Memory:** short host samples show about **175-192 MiB less available memory
  per treatment node relative to control changes**. Slab rose about 16 MiB on
  treatment nodes. This suggests a larger resource footprint, but does not
  isolate driver allocations from cache/workload drift or measure peak allocation.
  Window-end Prometheus snapshots show a smaller 131-136 MiB differential at
  different times. The minimum sampled treatment available memory was about
  114,876 MiB; no immediate exhaustion was visible. Neither result predicts a
  fixed linear cost for RX4096 (`tmp/ring2048-nic-review.md:64-70`,
  `tmp/ring2048-decision.md:37`).
- **CPU/TCP:** no large CPU or loadgen RSS regression was visible. Local outgoing
  retransmission ratios and softnet time_squeeze did not improve comparably to
  no-WQE. Local RX loss normally drives remote sender recovery; these counters
  provide no flow-to-read attribution (`tmp/ring2048-nic-review.md:48-62`,
  `tmp/ring2048-decision.md:36`).
- **Load and integrity:** selected endpoint observations had C16, in-flight 16,
  ready/live 1, and 21 workers. Full-fleet saved loaded-health checks passed with
  stable UIDs/membership and zero restarts. Six integrity-counter families had
  zero increases; post-treatment dataplane request-error increases were zero.
  These are discrete observations, not continuous safety proof
  (`tmp/ring2048-decision.md:34-35`).
- **Missing metrics:** loadgen error/canceled and failure-reason series were
  absent, not measured zero. Prometheus disk/drop rates and dataplane process
  CPU/RSS were unavailable. Short host samples do not replace full-window or peak
  coverage. Verified-byte rate was zero, so **no end-to-end SHA-256 payload
  verification is claimed** (`tmp/ring2048-decision.md:35-39`,
  `tmp/ring2048-analysis.json:315777-315840`, `cmd/racer-loadgen/pull.go:265-276`).

### Limits and why RX4096 was not tried

Matching on the second baseline window did not remain balanced in the first.
The treatment arm already had 2.42% more combined throughput; one pair differed
by roughly 39-42%. Sparse tails left only 15 and 14 defined pair tail ratios.
No nodes were removed from the pooled application comparison, and no rematching
followed outcomes (`tmp/ring-baseline-review.md:22-28`,
`tmp/ring2048-analysis.json:2627-2635,5260-5268`).

The analysis provides descriptive ratios, not causal confidence intervals or
significance. Shared peer traffic can affect controls; nodes, pairs, and adjacent
windows are not independent trials. Cache/load history and time drift remain
possible confounders. A mixed-fleet result is not a full-fleet estimate. These
limits do not justify ignoring controls or selecting only the favorable window
(`tmp/ring2048-analysis.json:315835-315841`, `tmp/ring-baseline-review.md:49-53`).

RX4096 was not tried because RX2048 had already reduced the measured no-WQE
mechanism by about 96-99%, while a repeatable 20% tail gain was not established.
No evidence tied the small residual to slow reads. A deeper ring would add a new
memory/queueing question without a demonstrated remaining bottleneck. The parent
chose reversal rather than automatic escalation. This is not proof that RX4096
could never help
(`tmp/ring2048-decision.md:41-47`; original checkout
`tmp/racer-tail-experiments-checkpoint.md`, October 13 00:09Z/00:38Z sections).

## Restored state and completed reversal

All 20 treatment nodes were restored to RX1024. The final saved inventory covers
those nodes plus the separate canary: **21/21 RX1024, TX256, channels32, both
links UP/LOWER_UP**. No ring-guard jobs or processes remained. Treatment hosts had
no loaded guard units; the canary retained only completed inert units with no
PID, queued job, or future expiry. The inventory spans 00:21:58-00:22:31 and
includes a targeted retry after parser errors, not one simultaneous snapshot
(`tmp/ring-final-state-summary.md:3-16`).

After restoration, all 100 loadgens were confirmed at C16 by both direct and
Prometheus observations at 00:23, with stable UIDs and no missing/bad Prometheus
nodes (`tmp/ring-restored-applied-16-confirm.json:1-5,5428-5440`). Host inventory
and application confirmation are separate evidence, not interchangeable checks.

### One reversal window, not repeated efficacy

The final RX1024 application window was October 13 **00:28-00:33**. It covers the
same 100-node fleet and fixed 20+20 cohorts. Baseline and RX2048 each pool 600
seconds; reversal covers 300 seconds. Rates and shares account for that unequal
exposure (`tmp/ring-reversal-analysis.json:3-14`).

| Scope | Baseline >10s share | RX2048-phase >10s share | Reversal >10s share | Reversal reads/s |
| --- | ---: | ---: | ---: | ---: |
| Fleet, 100 nodes | 0.150523% | 0.166373% | 0.161284% | 643.275 |
| Former treatment, 20 nodes | 0.157612% | 0.121415% | 0.099968% | 129.208 |
| Controls, 20 nodes | 0.187784% | 0.172635% | 0.105377% | 126.529 |

Fleet reversal had approximately 192,982.52 successful completions and 311.25
above 10 seconds, with pooled p99.9 of 17.600 seconds. The other 60 nodes supplied
232.5 of those tail completions: selected-cohort improvement was not fleet-wide
improvement (`tmp/ring-reversal-analysis.json:18-32,66`).

Against pooled RX2048, former-treatment tails improved 17.66% in absolute terms,
but controls improved 38.96%. The control-adjusted reversal tail therefore
**worsened 34.89%**, with adjusted throughput down only 0.78%. Against original
baseline, adjusted tail worsened 13.03% and adjusted throughput fell 0.30%.
No >3% throughput regression appeared in these comparisons
(`tmp/ring-reversal-analysis.json:35-45`).

This is **consistent with losing some relative RX2048 benefit**, not proof of
zero benefit. Both restored arms had better absolute tails than their original
baseline, showing substantial time/state drift. One reversal cannot establish
repeatability or repair the original 1.80%/29.14% adjusted-window result. Both
interventions remain rejected under the original rule; RX4096 was not tried
(`tmp/ring-reversal-analysis.md:44-54`).

### NIC rebound was measured before the window

The reversal NIC sample ran **00:27:06.835-00:27:49.967**, entirely **before**
the 00:28-00:33 application window. The earlier parent schedule proposed a
during-window sample; the saved artifact shows otherwise. It is not concurrent
evidence for that window (`tmp/ring-reversal-nic-review.md:3-17`; original
checkout `tmp/racer-tail-experiments-checkpoint.md`, October 13 00:23Z/00:38Z
sections).

All 40 sampled hosts showed RX1024/TX256 and 32 channels. Former-treatment
no-WQE/received-unicast rebounded to **0.029034%**, near its **0.028090%** baseline;
controls were **0.026156%**. All NIC families validated; one softnet decrease
remained excluded. The 1024 -> 2048 -> 1024 pattern strengthens attribution of
the discard change to ring depth, not read-tail causation. Much of the added
memory footprint also receded, without isolating driver allocation from workload
drift (`tmp/ring-reversal-nic-review.md:15-34,42`).

### Final loaded health

The 00:33:16-00:33:30 health observation passed for all 200 pods, with 100 nodes
per application, stable UIDs, consistent membership 85, zero restarts, and no
errors. `drain_pass=false` reflects active C16, not failed health: the helper
checks health separately from zero-activity drain conditions
(`tmp/final-loaded-health.json:35985-36014`,
`tmp/ring-reversal-analysis.json:48-55`, `tmp/rollout_observe.py:183-212`).

Final window endpoints showed C16/in-flight16, ready/live1, and 21 workers per
node. Returned request-error and six integrity-counter increases were zero.
Missing error labels and disk/drop metrics remain missing; verified-byte rate
remains zero. These observations do not establish continuous safety, payload
SHA-256 verification, or peak-resource bounds
(`tmp/ring-reversal-analysis.json:48-55,68`). The parent's October 13 00:38Z
checkpoint records all experiments complete, rings restored, no pending guards,
and no recovery pending.

## Evidence retention

Paths under `tmp/` refer to the experiment worktree unless marked "original
checkout". After report finalization, the parent plans to create original
checkout `tmp/racer-tail-experiments-evidence-20261013.tar.gz`. This report does
not assert that the archive already exists.

For later lookup, check original checkout
`tmp/racer-tail-experiments-archive-receipt.json` for `verified=true` and the
archive hash. The planned archive preserves `tmp/...` artifact paths and this
report; the root checkpoint is stored as `original-checkpoint.md`, with sections
identified by timestamp. `manifest.json` lists member sizes and hashes. These
paths and verification steps follow `tmp/archive_experiments.py:19-25,66-91,179-199`.
Input hashes are also recorded in `tmp/ring2048-analysis.json:315843-315877`;
artifact hashes do not imply workload payload verification.
