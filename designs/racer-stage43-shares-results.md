# Stage43: owner shares measured, cohort regression, rolled back

## Decision

Completed the previously missing **one five-minute weighted C6 measurement**.
Restored all eleven original absent administrator overrides because cohort
verified goodput fell **32.689617%**, even though fleet goodput rose **0.656035%**
against the fresh baseline. **No C7. Retain pod-network5e1, C6, catalog512,
all1500 members and local clients, controller0c23, operator d3, loadgen208.**

This was an annotation-only operational experiment, not a security downgrade.
No new gauge, image, build, test, pod roll, NIC change, underlay change, reboot,
cache wipe, exclusion, or identity reset occurred.

**Publication verified; per-process hash not observable.** The bounded healthy
refresh/drain observation supports proceeding with this experiment, not a claim
of complete exact all-process acceptance proof. The missing gauge did not abort it.

## Fixed windows and fleet result

All times UTC on2026-09-29. Before any patch, saved a retrospective five-minute
query snapshot and all1500 per-node rows, including the exact stage41 cohort.
The preceding stage42 window is retained as a second comparison, not substituted
for the fresh baseline. No replacement baseline wait or repeated weighted run.

| Metric | Previous stage42 C6 | Fresh shares4 C6 | Weighted shares1 C6 |
| --- | ---: | ---: | ---: |
| Window start | 16:16:52.202930 | 16:30:58.524638 | 16:41:21.344716 |
| Window end | 16:21:52.202930 | 16:35:58.524638 | 16:46:21.344716 |
| Verified complete-image GB/s | 586.356641 | 586.074856 | 589.919711 |
| Complete-image success % | 99.382606 | 99.409107 | 99.661186 |
| Positive verified nodes | 1500 | 1500 | 1500 |
| Host eth0 TX Tb/s | 12.981963 | 12.983663 | 13.029153 |
| Host eth0 RX Tb/s | 12.026908 | 12.028386 | 12.071846 |
| Mean host busy logical cores | 6.078081 | 6.078251 | 6.108821 |

The weighted window passed aggregate>=99%, all1500 positive, exact node-set
coverage, constant C6 min/max, five continuously-up telemetry jobs, and zero
checked counter resets. All4500 workload fingerprints/readiness transitions
remained unchanged; no new workload warning events or Node UID/boot changes.
Periodic checks also verified all1500 dataplane-ready series and all three
unchanged Running controller processes with their serving Ready leader. This is
not a claim that all three controller replicas were Ready. Individual success
was below99% on185 nodes; those nodes were not omitted.

Fleet gain was +0.607663% against previous586.356641 GB/s. These sequential single
windows are an operational comparison, not repeated causal evidence or proof of
maximum throughput. The small fleet gain does not offset the target cohort loss.

## Exact eleven-node cohort

All names below have prefix `aks-ddsv6-84072342-vmss`. Each cell is
**fresh shares4 -> weighted shares1**. Node UIDs were checked against the stage41
forecast, not selected after seeing results. No cohort node was removed.

| Suffix | Verified GB/s | Success % | TX Gbit/s | RX Gbit/s | Host cores | DP cores |
| --- | --- | --- | --- | --- | --- | --- |
| 00000h | .103136 -> .074508 | 93.878 -> 97.059 | 1.957 -> 1.758 | 2.581 -> 2.689 | 5.039 -> 4.857 | 2.833 -> 2.845 |
| 00002i | .115718 -> .076161 | 98.113 -> 89.474 | 1.849 -> 1.684 | 2.359 -> 2.685 | 4.847 -> 4.803 | 2.708 -> 2.793 |
| 00003z | .100479 -> .073600 | 97.826 -> 97.059 | 1.963 -> 1.782 | 2.159 -> 2.582 | 4.695 -> 4.897 | 2.168 -> 2.811 |
| 00005f | .123142 -> .079711 | 100.000 -> 92.308 | 1.945 -> 1.788 | 2.178 -> 2.432 | 4.820 -> 4.962 | 2.566 -> 2.823 |
| 000066 | .113725 -> .075361 | 91.071 -> 100.000 | 1.933 -> 1.775 | 2.385 -> 2.578 | 4.936 -> 5.034 | 2.734 -> 2.904 |
| 00006d | .107273 -> .074915 | 92.308 -> 94.444 | 1.898 -> 1.787 | 2.560 -> 2.789 | 4.994 -> 5.010 | 2.811 -> 2.931 |
| 000078 | .115512 -> .071216 | 96.296 -> 88.889 | 1.903 -> 1.782 | 2.159 -> 2.224 | 4.777 -> 4.882 | 2.498 -> 2.664 |
| 0000b1 | .109790 -> .073772 | 96.078 -> 94.286 | 1.841 -> 1.759 | 2.538 -> 2.623 | 4.753 -> 4.936 | 2.649 -> 2.823 |
| 0000bl | .121767 -> .080332 | 96.491 -> 100.000 | 1.958 -> 1.769 | 2.280 -> 2.431 | 4.753 -> 4.758 | 2.566 -> 2.714 |
| 0000cp | .118682 -> .080476 | 96.364 -> 100.000 | 1.942 -> 1.677 | 2.217 -> 2.603 | 4.791 -> 4.670 | 2.537 -> 2.682 |
| 0000d9 | .112758 -> .075930 | 100.000 -> 97.143 | 1.944 -> 1.768 | 2.523 -> 2.798 | 4.998 -> 4.939 | 2.700 -> 2.773 |

| Cohort aggregate | Previous stage42 | Fresh baseline | Weighted |
| --- | ---: | ---: | ---: |
| Verified GB/s | 1.231572 | 1.241981 | 0.835982 |
| Success % (pull-weighted) | 98.042705 | 96.200345 | 95.408163 |
| Total TX Gbit/s | 21.056227 | 21.131517 | 19.329195 |
| Total RX Gbit/s | 25.944146 | 25.939342 | 28.435056 |
| Mean host cores | 4.871364 | 4.854663 | 4.886201 |
| Mean dataplane cores | 2.627322 | 2.615557 | 2.796672 |

Every cohort node's verified rate decreased. TX decreased while RX and mean
dataplane CPU increased; this is not usable receiver-capacity relief. Owner
shares affect HRW ranking, not relay adjacency (`topology/placement.rs:64-110`,
`topology/graph.rs:11-36` at5e1). These metrics do not isolate owner versus transit
bytes and do not establish a specific causal mechanism or immutable NIC ceiling.

Goodput is `rate(racer_loadgen_verified_bytes_total[5m])/1e9`, credited to fully
verified successful complete images. Success includes all success/error/canceled
pull rates. NIC counters are host eth0 and include other traffic; they are not
verified goodput. CPU units are busy logical cores. All units are decimal. The
exact queries, evaluation times, full rows, and distributions are saved.

## Publication and bounded observation

Inspected exact deployed DP `5e1a4554f8034fef676d5ca91315da80646e5b71`; its
`internal/racer` tree is identical to controller
`0c23d0e93338979d41b9f78568ea08f7587fa0bc` (empty tree diff).

- Fresh authenticated server dry-runs and UID/resourceVersion tests preceded
  exactly11 annotation writes. Applied by16:36:55; no enrollment/history fields
  were manually edited. Explicit administrator precedence is implemented in
  `internal/racer/membership.go:43-69` and `bootstrap.go:173-202`.
- At16:37:11 the independently reconstructed canonical1500-member hash matched
  membership5001/sequence5002:
  `fb886b9d454dbc854c5212195cf75c8323ff807c82f3324e88697597a59ba2f2`.
  Exact expected11 shares1/1489 shares4; UID/endpoints/rails otherwise unchanged.
- Controller long poll is30s (`internal/racer/wire/types.go:20`); newer state
  returns immediately and publication changes wake parked polls
  (`publications.go:288-341`). Authenticated delivery remains bounded
  (`server.go:434-513`). Existing test assertions confirm immediate old-cursor
  return and exact normal poll timeout (`publications_test.go:341-380,411-423`);
  read, not rerun.
- DP normal control turns are40s, actually driven by `app.rs:1486-1504` and
  `control/client.rs:461-464`. Ordinary transient retry jitter is1-30s
  (`client.rs:326-336`); prepared state retries independently of remote progress
  (`client.rs:469-479,745-803`). Cursor/hash/replay acceptance is in-memory and
  atomic (`control/snapshot.rs:117-126,170-229,234-303`).
- Observed health for at least200s after verified publication, exceeding a40s
  normal turn +30s ordinary retry +120s configured in-flight pull drain. This is
  not an absolute convergence bound under repeated failures. The measurement
  started only after observation completed at16:40:46. C6 never changed.
- Existing diagnostic routes expose health, ready, metrics and failure ring,
  not accepted membership (`telemetry/server.rs:295-300,367-383`). No accepted
  hash endpoint was available. `racer_checkpoint_sequence` was not used as
  membership evidence. **Publication verified; per-process hash not observable.**

Stage42's observability-only rejection (`racer-stage42-shares-results.md:43-50`)
is historical; the user explicitly authorized this qualified measurement without
new observability. No behavior/security contract was weakened to obtain it.

## Rollback and recovery

Retention gates were fixed before applying: aggregate success>=99%, all1500
positive, clean health/coverage, improved eleven-node goodput, fleet goodput
within1% of both fresh and prior baselines. C7 required passing those gates and
>1% fleet improvement over both. Cohort regression rejected shares and C7.

At16:47:35 restored exact original absent overrides using fresh Node UID,
resourceVersion and current-value=`1` tests. At16:47:52 controller publication
was all1500 shares4, membership5012/sequence5013, original hash
`1edebfa700248f4e50834169640843cef80a3f76956f2f8acbd4b4e45d5ca178` and original
content hash. Counters advanced normally; none were reset.

Another bounded200s refresh/drain observation passed. Recovery observation at
16:51:33: **99.415231% success,589.295937 GB/s,1500 positive** (recent2m, not
another benchmark). Final preservation at16:51:56 verified all1500 Ready Nodes
with original UIDs/boot IDs/labels, all4500 unchanged Ready workload processes,
healthy unchanged controller processes/serving leader, protected configs/specs,
identity UIDs/hashes, and cache UID `79f749cf-6c6c-4e26-928f-2057ca9b4279`.
Read-only quarantine inspection at16:51:57 confirmed `00007r` original PID517112,
all process/io_uring workers still off CPUs6/7. No affinity mutation.

## Evidence and scope

Compact phase log: `tmp/racer-stage43-progress.md`. Artifacts under
`tmp/racer-stage43-`: baseline/c6 `{plan,raw,summary,health-review}.json`,
full1500-row `{baseline,c6}-per-node.csv`, exact `comparison.json` (all eleven
rows for previous/fresh/weighted), preflight and per-node applied/rolledback
records, before/weighted/rollback membership, observation/window health records,
final-health/final-preservation, quarantine, and
`recovery-1790700693271748066.json`. Retained helper: `tmp/racer-stage43-ops.py`.
Raw evidence contains no persisted credentials. Baseline history is inferred
from telemetry/reset checks rather than a retrospective process watch; periodic
checks cannot exclude unsampled transients.

One documentation-only worktree/commit. No application changes or unrelated
worktrees touched. Parent untracked request-path documents preserved. No subagent
tool available. `~/design/AGENTS.md` absent; repository instructions and
`~/design.md` read. No formatter/build/test invocation for this operational report.
