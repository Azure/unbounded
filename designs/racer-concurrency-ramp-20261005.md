# Warm-cache concurrency ramp, 2026-10-05

## 2026-10-05T16:24:41Z: baseline and before C4

User authorized increasing loadgen concurrency and identifying the bottleneck. Context joolshev-scale-test, namespace unbounded-system. All 1,500 loadgens, Gantry, and dataplanes Ready. One ~518 MiB image, layer concurrency 1, verification enabled. Global C2; no node caps. No application changes planned.

Baseline at ~16:20-16:23Z: 2.258 TiB/s verified; mean loadgen CPU 1.981 cores, Gantry 0.489, dataplane 0.733. Node busy p50 44.9%, p95 52.5%. Loadgen has no CPU limit, GOMAXPROCS 8 on 1,497 nodes and 16 on three. Gantry has a 2-core limit, but is far below it. Parca on its single covered node attributes 58.3% flat CPU to SHA-256 SHANI verification. This is a hotspot, not yet a proven saturation ceiling.

Next exact mutation: `kubectl --context=joolshev-scale-test -n unbounded-system patch cm racer-loadgen-control --type=json -p='[{"op":"test","path":"/data/concurrency","value":"2"},{"op":"replace","path":"/data/concurrency","value":"4"}]'`.

Each ramp phase deadline 5 minutes, heartbeat every 30 seconds, success all 1,500 apply target concurrency and remain Ready with no failures, followed by a clean two-minute steady-state rate window. Parent owns rollback to previous stable concurrency if health degrades; no automatic increases. Stop increasing on plateau and diagnose resource usage/profile. No caches will be reset or verification disabled. Error: none.

## 2026-10-05T16:29:30Z: C4 passed, before C8

C4 patch succeeded at 16:25:21Z. All 1,500 applied by 16:27:56Z; final 16:29:29Z two-minute rate 4.09675 TiB/s, 8,296 successful pulls/s, mean latency 0.72317s, 6,000 in flight, all loadgen scrapes and dataplane readiness 1,500. Plaintext misses and peer acquisitions zero. Pull result series only success. Mean CPU cores: loadgen 3.5853, Gantry 0.8561, dataplane 1.3137. Node busy p50 76.72%, p95 83.65%. No error.

Next exact mutation: `kubectl --context=joolshev-scale-test -n unbounded-system patch cm racer-loadgen-control --type=json -p='[{"op":"test","path":"/data/concurrency","value":"4"},{"op":"replace","path":"/data/concurrency","value":"8"}]'`.

C8 phase deadline 5 minutes, heartbeat 30 seconds using `timeout --signal=TERM --kill-after=10s 280s python3 -B ramp.py 9`. Same health/convergence gates; C4 is rollback target. CPU saturation is expected and is not by itself a health failure. A plateau triggers profiling, not another blind increase.

## 2026-10-05T16:34:20Z: C8 passed, saturation observed

All 1,500 C8 by 16:32:46Z; final two-minute rate 5.08280 TiB/s, 10,292.9 successful pulls/s, mean 1.16613s, 12,000 in flight. All loadgens up and dataplanes ready; zero plaintext misses/peer acquisitions; no failed pull result series. Mean CPU loadgen 4.6200, Gantry 1.0312, dataplane 1.6368 cores. Node busy p50 96.965%, p95 97.217%. C4->C8 gain only 24.1%, versus doubled concurrency, and latency increased 61.3%. This establishes node CPU saturation; profile the saturated interval to attribute work.

Next exact mutation: `kubectl --context=joolshev-scale-test -n unbounded-system patch cm racer-loadgen-control --type=json -p='[{"op":"test","path":"/data/concurrency","value":"8"},{"op":"replace","path":"/data/concurrency","value":"16"}]'`.

One bounded C16 confirmation, not an open-ended ramp: deadline 5 minutes, heartbeat 30 seconds, same health gates and two-minute rates. Roll back to C8 after measurement if gain is negligible or negative. Profiling runs read-only concurrently, using C8 steady interval 16:32:45-16:34:15Z. Error: none.

## 2026-10-05T16:38:53Z: C16 plateau, before return to C8

C16 fully applied by 16:37:19Z. Final two-minute verified rate 5.11396 TiB/s, 10,349.75 success/s, mean latency 2.31756s, 24,000 in flight; all 1,500 scrapes/readiness healthy, zero plaintext misses/peer acquisitions, success-only pull results. CPU means loadgen 4.7367, Gantry 0.9520, dataplane 1.6716; node busy p50 97.475%, p95 97.823%. Doubling C8 yields only 0.61% more throughput and 98.7% greater latency.

Next exact mutation: `kubectl --context=joolshev-scale-test -n unbounded-system patch cm racer-loadgen-control --type=json -p='[{"op":"test","path":"/data/concurrency","value":"16"},{"op":"replace","path":"/data/concurrency","value":"8"}]'`. Deadline five minutes, heartbeat every 30 seconds; success all 1,500 C8 with healthy steady throughput, no new failures or restarts. Error: none.

### Bottleneck evidence

Read-only Parca C8 interval 16:32:45-16:34:15Z covers only node `aks-ddv5-17198779-vmss0000ad`. Loadgen sampled 450.474 CPU seconds: SHA-256 `blockSHANI` 292.421s flat (64.91%), kernel bucket 32.84%. Gantry sampled 68.526 CPU seconds: kernel 81.87%, `internal/poll.Splice` cumulative 76.11%. Dataplane sampled 142.421s: kernel 79.08%. Kernel samples collapse into an address-zero bucket and its symbol is unstable: do not attribute to individual kernel functions. This supports verification plus kernel/socket work, not a specific kernel-copy optimization claim.

Code matches inline verification (`cmd/racer-loadgen/pull.go:420-435`) and SDK Unix-socket forwarding via `io.ReaderFrom` (`pkg/racersdk/http_stream.go:279-295`). Checkout is not asserted identical to deployed images.

Fleet CPU at 16:34:15Z: user 50.49%, system 34.74%, softirq 10.61%, idle 4.17%, I/O wait 0.00008%, steal zero. 1,497 nodes have eight logical CPUs, three have sixteen. Profiled node 97.18% busy; its loadgen CPU 4.865 cores is at ~51st percentile but throughput 3.017 GiB/s is only ~14th percentile. Profile is representative of saturation, not all CPU architectures.

Raw cAdvisor on D8d_v5 node above and D8ads_v5 `aks-adsv5-13731677-vmss00006u`: Gantry CFS throttled periods/seconds remain zero with advancing total periods. During 16:36:20-16:37:20 transition/confirmation, both nodes consume ~7.77/8 cores; CPU PSI some 57.12%/60.82%, Gantry PSI some 19.08%/18.18%, loadgen 40.70%/43.56%. PSI is wall time with runnable work waiting, not CPU utilization. These observations establish shared CPU contention, not a Gantry two-core quota ceiling.

Source of metrics: Prometheus via monitoring/prometheus:9090 Kubernetes service proxy; two-minute rates required for complete fleet coverage. Pod CPU means from metrics.k8s.io roughly one-minute windows, not perfectly aligned. Parca via parca/parca-profile-store:7070 proxy `/api/profiles/query`, mode MODE_MERGE, report REPORT_TYPE_TOP, selector `parca_agent:samples:count:cpu:nanoseconds:delta{container="racer-loadgen"}` (substitute gantry/dataplane). cAdvisor via `/api/v1/nodes/<node>/proxy/metrics/cadvisor`; its container CPU/PSI/throttling metrics are not currently scraped into Prometheus.

## 2026-10-05T16:41:00Z: user authorized unverified benchmark

The C16->C8 patch succeeded; monitoring was interrupted by user request, not a command timeout. Live inspection at 16:40:01Z confirmed CM C8 and the read-only monitor still running under its timeout. User requested disabling loadgen SHA-256 and explicitly authorized deleting all old loadgen pods for fast replacement; availability is not required. No code/image change is needed: existing `--verify=false` bypasses per-read SHA-256 (`pull.go:420-453`) and comparison (`398-406`). Size/status checks remain. Verified-byte counter then stops, received-byte counter includes partial failures and must be paired with success/error metrics. Startup catalog hashes remain.

Next commands: patch CM C8->C0 with JSON test/replace, then observe all 1,500 applied zero and in-flight zero, deadline 5 minutes/30-second heartbeat. Set DS OnDelete before replacing args; change only `--verify=true` to `--verify=false`; enumerate existing pods then delete those exact names with wait=false, preserving replacement pods. Restore RollingUpdate maxUnavailable=10%,maxSurge=0 after patch/deletion. Parent retains responsibility for resuming C8 after all replacements are healthy. Error: none.

## 2026-10-05T16:44:53Z: drained, before replacement

All 1,500 readers applied C0, total in-flight zero. Dataplanes 1,500 ready, no peer work or misses. Next `timeout --signal=TERM --kill-after=10s 280s python3 -B rollout.py apply`, using guarded exact-args patch and deletion of pre-enumerated names, followed by `rollout.py observe`. Mutation deadline five minutes, per-batch heartbeat (100 pod names, five concurrent commands), readiness deadline five minutes with 30-second heartbeat. Error: none.

## 2026-10-05T16:47:05Z: replacement complete, before unverified C8

All 1,500 old pods explicitly deleted. All 1,500 replacements Ready with `--verify=false`, zero restarts, no terminating pods. Original RollingUpdate strategy restored. C0 and zero in-flight remain; Prometheus has stale old pod series (2,405 C0 entries), so do not treat that as distinct live readers or use transition rates. Catalog and origin identity unchanged; Gantry/dataplane pods and caches unchanged.

Next exact mutation: `kubectl --context=joolshev-scale-test -n unbounded-system patch cm racer-loadgen-control --type=json -p='[{"op":"test","path":"/data/concurrency","value":"0"},{"op":"replace","path":"/data/concurrency","value":"8"}]'`. Deadline five minutes, 30-second heartbeat using ramp.py; require live 1,500 C8 and a clean two-minute rate before comparing. Received bytes are unverified; verify counter must remain zero. Error: none.

## 2026-10-05T16:51:29Z: unverified C8 passed, before C16

All 1,500 C8 by 16:49:55Z. Final two-minute received 7.38730 TiB/s, verified zero, success 14,958.55/s, mean latency 0.80215s, in-flight 12,000; plaintext misses/peer acquisitions zero, no failed pull result series. Node busy p50 73.517%, p95 75.428%. CPU means loadgen 1.5973, Gantry 1.8492, dataplane 1.9611. Per-pull hashing removal increased delivery 45.3% at C8 and reduced loadgen CPU ~65.4%. Gantry now approaches two-core quota; this is a hypothesis until checked via cAdvisor/ramp.

Next exact mutation: `kubectl --context=joolshev-scale-test -n unbounded-system patch cm racer-loadgen-control --type=json -p='[{"op":"test","path":"/data/concurrency","value":"8"},{"op":"replace","path":"/data/concurrency","value":"16"}]'`. Deadline five minutes, heartbeat 30 seconds; same convergence/health/rate gates. Read-only concurrent profiling of 16:49:55-16:51:25Z and cAdvisor throttling. Error: none.

## 2026-10-05T16:56:31Z: unverified C16 plateau, before final C8

All 1,500 C16 applied by 16:54:57Z. Final two-minute received rate 7.40078 TiB/s, verified zero, success 14,985.3/s, mean latency 1.60136s, 24,000 in flight. Plaintext misses/peer acquisitions zero, success-only results, all dataplanes ready. Mean CPU loadgen 1.5982, Gantry 1.8616, dataplane 1.9898; node busy p50 74.185%, p95 76.175%. Gain over no-hash C8 only 0.18%, latency doubles.

Next exact mutation: `kubectl --context=joolshev-scale-test -n unbounded-system patch cm racer-loadgen-control --type=json -p='[{"op":"test","path":"/data/concurrency","value":"16"},{"op":"replace","path":"/data/concurrency","value":"8"}]'`. Final validation deadline five minutes, 30-second heartbeat; all 1,500 C8 healthy with steady received rate, zero verified rate, no failures or restarts. Error: none.

### Remaining bottleneck after disabling hashing

No-hash C8 Parca window 16:49:55-16:51:25Z, same single profiled node: no SHA-256 frames in loadgen/Gantry/dataplane TOP reports. Loadgen 130.684 CPU seconds, kernel flat 87.68%, readBody cumulative 88.72%; Gantry 166.105 seconds, kernel flat 91.35%, splice cumulative 91.98%; dataplane 189.947 seconds, kernel flat 78.17%. Kernel symbols remain unreliable; only aggregate/ancestry attribution used. This confirms per-pull hashing removal and a kernel/socket-forwarding-dominated path.

Both sampled Gantry processes have GOMAXPROCS2 (direct runtime metric), CPU quota 200000/100000=2, and corresponding pod limits/env. Two cAdvisor fetches at ~16:52:51 and 16:53:53Z overlap C8->C16. D8d_v5 Gantry: 1.814 cores, zero throttling in 593 added periods; node 5.866/8 cores. D8ads_v5 Gantry: 1.946 cores, 308/634 periods throttled (48.58%), 3.784s throttled over 63.386s (5.97%); node 5.527/8 cores. Throttled periods are not lost-throughput percentage. These prove quota is an active constraint on the sampled D8ads_v5 node, but not the sampled D8d_v5 node. Fleet-wide quota dominance is not established; GOMAXPROCS2/serialization/backpressure remain hypotheses elsewhere.

Fleet C8 CPU at 16:51:25Z: user 12.37%, system 44.04%, softirq 16.08%, idle 27.51%, I/O wait 0.00077%, steal zero. The previous shared-node CPU saturation is relieved. More loadgen concurrency no longer helps. Next useful experiment is a controlled Gantry CPU-quota/GOMAXPROCS increase, separating those variables, not further loadgen concurrency. No Gantry settings were changed in this task.

## 2026-10-05T17:01:11Z: final validation passed

All 1,500 loadgens Ready, unverified, C8, zero restarts and no terminating pods. Two-minute received rate 7.38195 TiB/s, verified zero, 14,947.4 successful pulls/s, mean latency 0.80273s; 12,000 in flight. All dataplanes ready, zero plaintext misses and peer acquisitions, no failed pull result series. Final CPU means loadgen 1.5927, Gantry 1.8543, dataplane 1.9578. Load remains running at unverified C8. No application code, image, Gantry settings, or dataplane settings changed; no image build/push necessary.

Next phase: remove transient operational scripts, run bounded `make fmt`, review diff, commit this record in the dedicated worktree, cherry-pick to original branch, remove worktree. Deadline five minutes per command, heartbeat on completion. No application tests needed for this operational-record-only change. Error: none.

Formatting attempt: gofumpt completed; golangci-lint panicked because it was built with Go 1.26 and encountered Go 1.27 source. This environment/toolchain failure prevents a successful `make fmt`; no application changes were intended. Next: verify no tracked source modifications, commit only this operational record, and integrate. Live benchmark is complete and unaffected.
