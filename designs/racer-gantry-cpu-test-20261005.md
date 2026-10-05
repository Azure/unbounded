# Gantry CPU isolation experiment, 2026-10-05

## 17:05Z checkpoint: baseline, before pause

User requested continuing the previous experiment, specifically separating Gantry CPU quota from runtime parallelism. Context joolshev-scale-test, namespace unbounded-system; loadgen C8, verify=false, one warm image. Prior stable received throughput ~7.38 TiB/s. Gantry CPU quota2/GOMAXPROCS2; image7697139 unchanged; all1,500 ready, OnDelete. Dataplane remains unchanged.

Active operator owns Gantry. Change existing `unbounded-component-overrides` key `gantry-image-benchmark.yaml`, not just DS, preserving all other keys/fields. Implementation repairs drift (`internal/operator/component/env.go:344-355`); override CM watched (`internal/operator/reconciler.go:705-715`). Live operator1/1 confirms it is active.

Plan: baseline snapshot; CM loadgen C8->C0 and drain; override CPU2->4 leaving GOMAXPROCS2; wait DS convergence, delete all pre-enumerated Gantry pods as authorized for this test cluster; verify replacement pods and local endpoints; resume C8 and measure. Then repeat for GOMAXPROCS4 with quota4 if healthy. Each command/phase deadline five minutes, heartbeat every30s (deletion per100pods), external TERM timeout kill-after10s. Parent owns pause/resume/recovery. No caches reset. Rollback prior measured settings if errors or regression. Error:none.

## 17:08:14Z: drained, before quota-only rollout

Baseline17:05:38: received7.38384TiB/s, success14951.77/s, mean.80253s, nodebusy median73.34%; CPU means Gantry1.8545, loadgen1.5909, dataplane1.9588. All1500 C8/ready, zero misses/peers/failures. `experiment.py control 8 0` succeeded17:05:39; all1500 C0, inflight0 at17:08:14. Next exact commands `experiment.py override 2 2 4 2`, wait DS template4/2, then `experiment.py replace 4 2` and `experiment.py observe 4 2`. Each phase bound5min, heartbeats30s or per100pod batch. Success all1500 quota4/P2 Ready and endpoint coverage1500. Error:none.

## 17:10:02Z: quota-only rollout complete, before resume

Override patch succeeded17:08:30, operator converged in10s. All1500 old Gantry pods deleted, replacements quota4/P2 Ready with zero restarts; DS generation35 updated1500, local endpoint node coverage1500. C0 remains. Next `experiment.py control 0 8` then `experiment.py monitor 9` (external timeout280s), heartbeat30s; require all1500 C8 and clean2min rates. No error.

## 17:17Z: quota-only complete, before runtime-parallelism phase

Resume17:10:18; all1500 C8 observed17:12:53. Gantry restart caused transient cache misses/peer fills despite no dataplane restart; excluded warmup. Clean17:14:26 received7.416315TiB/s (+0.44%), success15019.35/s, mean.79897s, nodebusy median73.576%; CPU means Gantry1.8563/loadgen1.6045/dataplane1.9672. Verified0, misses0, peers0, onlysuccess results, all1500ready.

Read-only raw cAdvisor17:15:42-17:16:42 on ddv5...0000ad (`gantry-htjlq`) and adsv5...00006u (`gantry-2r6w2`): quota400000/period100000; both container and parent cgroups zero throttled periods/duration across advancing counters. Direct metrics confirm runtimeP2, process CPU1.810/1.983cores. Cached cAdvisor CPU divided by scrape spacing differs from process rates; do not use it as precise CPU rate. Local loadgen received advances5.859/4.754GB/s. Quota relaxation removed sampled throttle, but did not materially raise fleet throughput.

Next `experiment.py control 8 0`, `monitor 6`, require all1500C0/inflight0; `override 4 2 4 4`, waittemplate then `replace 4 4`/`observe 4 4`. Each phase5min, heartbeats30s, parentowns recovery. Error:none.

## 17:20:00Z: second drain complete

C0 patch17:17:25; all1500C0/inflight0 confirmed17:20:00. Next execute planned override/replace4/4, bounded5min, same image/cache/config otherwise. Error:none.

## 17:22:13Z: runtime-parallelism rollout complete, before resume

Override patch17:20:13; all1500 new4/4 GantryReady17:22:12, zero restarts, endpoints1500, generation36. Next `control 0 8` and `monitor 9`, bound5min, heartbeat30s, then clean-window measurements. Error:none.

## 17:30Z: P4 C8 result, before C16 check

Resumed17:22:23; all1500C8 at17:24:58. Clean17:26:31 received9.27544TiB/s (+25.1% versus quota4/P2), success18783.27/s, mean.638855s, nodebusy median91.4875% p9592.9043%; CPUmeans Gantry2.5494/loadgen1.7657/dataplane2.5640. All1500up/ready, verified0, misses0, peers0, onlysuccess pulls.

Parca17:25-17:26:30 sampled CPU seconds Gantry212.158 (kernel89.08%), DP235.263(kernel79.60%), loadgen156.053(kernel88.33%). Container-only queries, prior-known single-node scope not independently revalidated this phase. Kernel address-zero symbolization still unreliable; no specific kernel function inference. Gantry payload/HTTPReadFrom ancestry, loadgenreadBody dominate cumulative. Agent used90s Prom CPU mode query (partial fleet coverage caveat), so rely on parent's2min nodebusy for fleet claims.

RawcAdvisor17:27-17:29:42 confirms quota4, zero Gantry throttling bothnodes, directprocessP4. Internal-timestamp CPUrates Gantry2.429/2.636, DP2.686/2.393, loadgen1.736/1.873 cores on ddv5/adsv5. Agent anticipated concurrent C16 ramp, but parent had not issued it: these samples remained C8. Next `control 8 16` then `monitor 9`,5min deadline/30s heartbeat; compare throughput versus latency then retain best setting. Error:none.

## 17:36Z: C16 complete, before final C8 restoration

C16 patched17:31:32, all1500applied17:34:08. Clean17:35:41 received9.77604TiB/s, success19813.60/s, mean1.21122s, nodebusy median95.9792% p9597.1042%. CPUmeans Gantry2.6090/loadgen1.8702/DP2.7735. All1500up/ready, onlysuccess, verified0/misses0/peers0. C16 adds5.4% versus C8 with89.6% more latency. Shared host CPU streaming costs now favored over quota ceiling; exact kernel operation not resolved.

Next `control 16 8`/`monitor 9`,5min bound30sheartbeat; retain Gantryquota4/P4 C8 as throughput/latency knee. Finalgate all1500 C8 and cleanrate, endpoints/readiness, no new failures. Noerror. No more live mutations planned after restoration.

## 17:40Z: final live state verified

Restored C8 at17:35:59; all1500 applied by17:38:35. Clean17:39:37 received9.258TiB/s, mean.64004s;17:40:08 received9.20675TiB/s, mean.64363s. All1500scraped/ready; onlysuccess pull results, zero plaintextmiss/peer rates, verified0. At17:40:21 all1500 Gantry4/4 Ready, zero restarts, no terminating pods, local endpoints1500; loadgen/dataplane DaemonSets1500/1500Ready. Final load remains C8, verify=false, Gantryquota4/P4. The override is persistent in the active operator ConfigMap; CPU request remains1, memory limit2Gi, image unchanged, OnDelete preserved. No application code/image changes.

| Gantry quota | GOMAXPROCS | Loadgen C | Received TiB/s | Mean pull seconds | Median node CPU busy |
| --- | --- | --- | --- | --- | --- |
| 2 | 2 | 8 | 7.384 | 0.803 | 73.3% |
| 4 | 2 | 8 | 7.416 | 0.799 | 73.6% |
| 4 | 4 | 8 | 9.275 | 0.639 | 91.5% |
| 4 | 4 | 16 | 9.776 | 1.211 | 96.0% |

Increasing quota alone did not materially improve throughput. Increasing runtime parallelism at quota4 did. This is not a full factorial experiment: P4 at quota2 was not tested, and no claim is made that quota4 is unnecessary for the P4 improvement. C8 retains approximately95% of measured peak with much lower latency. Remaining evidence favors shared host CPU in kernel/socket streaming paths, not cache misses or sampled Gantry quota throttling. No specific kernel function or NIC bandwidth bottleneck was established.

Rates use Prometheus2min windows via `/api/v1/namespaces/monitoring/services/prometheus:9090/proxy/api/v1/query`, principally `sum(rate(racer_loadgen_received_bytes_total[2m]))/1024^4`, `sum by(result)(rate(racer_loadgen_pulls_total[2m]))`, pull duration sum/count rates, and `quantile(0.5,1-avg by(node)(rate(node_cpu_seconds_total{mode="idle"}[2m])))`. Queries within each snapshot are near-simultaneous, not a single fixed evaluation time. Metrics-server CPU is a separate roughly1min sample. Received throughput is unverified application bytes, not NIC traffic or verified goodput.

Temporary `experiment.py` executed `kubectl --context=joolshev-scale-test -n unbounded-system` operations: guarded JSONPatch test/replace of `/data/concurrency`; guarded test/replace of `/data/gantry-image-benchmark.yaml` after changing only quota/GOMAXPROCS in parsed YAML; delete pre-enumerated old pod names with `--wait=false --ignore-not-found=true`, batches100/fiveparallel. Helper removed before commit; this document retains checkpoints/results. No other override keys changed.

Validation: live experimental checks passed. `make fmt` attempted with external180s TERM timeout: gofumpt completed; golangci-lint panicked because it was built with Go1.26 and encountered Go1.27 source. No tracked source changes resulted. No application tests run because only this operational record is retained. Next integrate record with commit/cherry-pick to racer-v2 and remove detached worktree; each git command bounded30s. Formatting-tool version mismatch is the only recorded error.
