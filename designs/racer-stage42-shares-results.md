# Stage42: guarded owner-share trial rolled back

## Decision

Applied the existing authenticated administrator Node annotation shares4 -> shares1 on exactly the eleven stage41 Nodes, then restored their exact original absent overrides. **Retain pod-network5e1, live C6, catalog512, all1500 members and clients.** No C7 or C8. This is a convergence-gate rejection, not evidence that owner weighting improves or worsens steady-state performance.

The requested weighted five-minute measurement was **not armed**. Controller publication was proven; exact dataplane acceptance was not observable through the deployed metrics/control interface. No before/after cohort, NIC, or CPU improvement is claimed.

## One fresh prechange snapshot

Retrospective existing-steady Prometheus window: **2026-09-29T16:16:52.202930+00:00 to 2026-09-29T16:21:52.202930+00:00**. No replacement baseline wait. All queries used the fixed end time and retained full1500-node rows.

| Metric | Prechange shares4/C6 |
| --- | ---: |
| Verified complete-image GB/s | 586.3566408716164 |
| Complete-image success % | 99.38260622958126 |
| Positive verified nodes | 1500 |
| Host eth0 TX Tb/s | 12.981962601421339 |
| Host eth0 RX Tb/s | 12.02690802361494 |
| Mean host busy logical cores | 6.078081084281063 |
| Mean dataplane logical cores | 3.5980471736221444 |

Exact expected node sets, five continuously-up telemetry jobs, constant C6 min/max, and checked zero counter resets passed. Retrospective telemetry supports continuity; the snapshot does not supply a prospective pod watch over the preceding five minutes. NIC bytes include other host traffic and are not verified goodput.

### Eleven-node prechange cohort, no exclusions

| ddsv6 suffix | Verified GB/s | Image success % | TX Gbit/s | RX Gbit/s | Host cores | DP cores |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 00000h | 0.109605 | 100.000000 | 1.941437 | 2.609935 | 5.050000 | 2.847281 |
| 00002i | 0.116062 | 100.000000 | 1.888165 | 2.437690 | 4.989542 | 2.441223 |
| 00003z | 0.101365 | 97.826087 | 1.888366 | 2.153103 | 4.726292 | 2.556985 |
| 00005f | 0.114162 | 100.000000 | 1.910903 | 2.070365 | 4.695000 | 2.467224 |
| 000066 | 0.119229 | 100.000000 | 1.842607 | 2.356899 | 4.858458 | 2.672602 |
| 00006d | 0.106631 | 96.000000 | 1.982102 | 2.598731 | 5.159833 | 2.851220 |
| 000078 | 0.106039 | 96.000000 | 1.906655 | 2.196928 | 4.812833 | 2.535539 |
| 0000b1 | 0.107982 | 96.000000 | 1.938915 | 2.604174 | 4.876542 | 2.736427 |
| 0000bl | 0.123709 | 98.214286 | 1.874787 | 2.192421 | 4.679875 | 2.499285 |
| 0000cp | 0.122347 | 100.000000 | 1.934063 | 2.216915 | 4.780583 | 2.562920 |
| 0000d9 | 0.104440 | 94.000000 | 1.948226 | 2.506986 | 4.956042 | 2.729840 |

Cohort total verified goodput: **1.2315718219333334 GB/s**. Every cohort node was positive. Weighted equivalents were not measured.

## Application, gate failure, and exact restoration

- Refreshed `racer-capacity-preflight.py`; all11 UID/resourceVersion-guarded patches passed authenticated server admission dry-run. Applied only the shares annotation at16:22 UTC. No enrollment/history fields were edited by the operator of this trial.
- Initial membership read raced normal asynchronous publication and failed its hash assertion. The next read at16:23:37 matched the controller: membership4982, sequence4983, hash `fb886b9d454dbc854c5212195cf75c8323ff807c82f3324e88697597a59ba2f2`. Exact members were11 shares1/1489 shares4; all UID/endpoints/rails were unchanged.
- Operational collector mistake: attempted to compare `racer_checkpoint_sequence` against controller sequence4983. Its values represent storage checkpoints, so that bounded poll failed and supplied no membership evidence. No convergence success was recorded. The metric name mapping is `cmd/racer-dataplane/src/telemetry/metrics.rs:176-215`; storage publication updates it in `app.rs:1307`.
- The deployed metrics expose readiness, not accepted membership hash/version. The diagnostic routes are health, ready, metrics, and failure ring (`telemetry/server.rs:295-300,367-383`); the control snapshot is in-memory (`control/snapshot.rs:35-44,117-126`). Readiness alone cannot prove adoption of a particular publication. Do not add observability or redeploy binaries during this authorized annotation-only operation.
- Consequently no performance arm/drain/C7 promotion followed. At16:27:24 all11 overrides were removed using exact Node UID, fresh resourceVersion, and current-value=`1` tests. Every original override was absent. No identity reset, exclusion, cache wipe, pod replacement, NIC setting, or underlay change.
- At16:28:10 the controller independently matched the original membership hash `1edebfa700248f4e50834169640843cef80a3f76956f2f8acbd4b4e45d5ca178` and content hash, now membership4993/sequence4994. All1500 shares4. Counters advanced normally, not reset. Exact all-DP membership-version convergence remains unproven after rollback too.

## Final health and recovery

The first rollback recovery observation was not accepted as full application health: its recent2m complete-image success was78.767732%, despite1500 positive nodes and unchanged/Ready workloads. After one bounded120-second recovery wait, the final recent2m observation at **16:31:39 UTC** was **99.471365% success,586.207274 GB/s,1500/1500 positive**. This is a recovery check, not the missing weighted benchmark or a new baseline.

Final audit preserved all4500 workload pod UIDs/container fingerprints/restart counts/readiness transitions, DaemonSet specs, protected ConfigMaps/deployment specs, identity UIDs/hashes, all1500 Node UIDs/boot IDs/labels, and cache UID `79f749cf-6c6c-4e26-928f-2057ca9b4279`. All three workload DaemonSets1500 Ready/Available; dataplane ready metric1500; C6 on all1500. Controller has a serving Ready leader, not three Ready replicas. Read-only inspection at16:29:06 verified original `00007r` PID517112 and its process/io_uring thread affinities still off CPUs6/7.

## Exact remaining structural constraint

Owner shares change weighted HRW ranking (`topology/placement.rs:64-68,100-110`), not radix18 graph adjacency (`topology/graph.rs:11-36`). They cannot reserve receiver RX capacity or directly reduce transit imposed by other sources. The retained stage41 model predicts cohort relay traffic of4.502-6.058 Gbit/s per node at586 GB/s, despite owner relief; this is conditional model output, not measured stage42 traffic or a proven hardware ceiling (`designs/racer-stage41-capacity-weights.md:148-194`). This trial does not solve or newly prove the transit bottleneck.

A future capacity-aware relay design, not just another owner-HRW weight, must separately authorize UID-bound transit capacity, incorporate it into route selection/admission, preserve all-node receiver membership/local sockets and signed hop/deadline budgets, and define mixed-version/capacity-change behavior. It also needs an externally verifiable accepted-publication hash/version per dataplane before claiming fleet convergence. This is a future contract requirement, not an implemented design or throughput promise. Do not overload shares as graph exclusion or assume zero shares is receiver-only.

## Evidence and scope

`tmp/racer-stage42-progress.md` records each phase. `tmp/racer-stage42-` artifacts include baseline `{plan,raw,summary,health-review}.json`, full1500-row `baseline-per-node.csv`, preflight, per-node applied/rolledback patches, before/weighted/rollback membership records, final preservation and quarantine records, and `recovery-1790699499096977241.json`. Raw snapshots contain no persisted credentials.

No application source/config files changed, no build/test reruns, no formatters, no repeated performance window, and no parent protocol/request-path documents touched. No subagent tool was available. `~/design/AGENTS.md` was absent; repository instructions and `~/design.md` were read. One documentation-only commit.
