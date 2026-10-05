# Racer consumer-local cache test, 2026-10-05

## Checkpoint 2026-10-05T15:59:27Z: before pause

- User authorized updating the live loadgen configuration to test near-100% local cache reuse.
- Context: `joolshev-scale-test`; namespace: `unbounded-system`.
- Last successful state: 1,500 dataplanes ready; 581 old loadgens ready, 919 new loadgens startup-invalid because node caps require a concurrency file. Global concurrency 2, empty node caps. No mutation yet; error: none.
- Intended workload: one legacy-compatible image, eight nominal 64 MiB layers, jitter 0.2, seed benchmark-v1, shuffle, layer concurrency 1. About 0.506 GiB of payload, versus 4 GiB node-wide plaintext admission. Retain existing image, endpoints, mounts, security settings, and node-cap controls.
- Restore `--concurrency-file=/etc/loadgen-control/concurrency`; use startup fallback `--concurrency=0`; resume by writing global control value 2 only after convergence.
- Compatibility: image zero retains existing bytes; old readers must drain before shrinking origin catalogs. No dataplane/cache reset is planned.
- Next commands: change DaemonSet update strategy to OnDelete with resource-version precondition, then change control concurrency from 2 to 0 with value precondition.
- Pause phase deadline: 5 minutes, heartbeat every 30 seconds. Success: all 581 surviving readers freshly report applied concurrency and in-flight zero; unavailable readers accounted for by pod state. Diagnose any timeout instead of repeating unchanged waits.
- Parent owns pause, rollout, recovery, and resume. No unrelated controller or workload changes are authorized.

## Checkpoint 2026-10-05T16:02:49Z: pause complete, before canary

- Commands completed: `kubectl --context=joolshev-scale-test -n unbounded-system patch ds racer-loadgen --type=json` changed updateStrategy to OnDelete; corresponding ConfigMap JSON patch changed concurrency 2 to 0.
- Verified 581 fresh loadgen series, max sample age under 60s, applied concurrency 0 and in-flight 0. Dataplane requests/fills/deliveries all 0; 1,500 ready. No errors.
- Next: replace only the loadgen args while OnDelete, then delete the known crashloop canary `racer-loadgen-22447` normally. Observe its node until replacement Ready with desired args and C0.
- Canary deadline 5 minutes; heartbeat 30s; success Ready, no restart, C0. After success restore original RollingUpdate 10% strategy for full rollout while C0; bounded 5-minute observation phases with progress diagnosis.

## Checkpoint 2026-10-05T16:04:00Z: canary passed, before fleet rollout

- Completed `python3 configure_local_cache.py` with resourceVersion precondition and `kubectl ... delete pod racer-loadgen-22447 --wait=false`.
- Replacement `racer-loadgen-2s6lx` Ready, zero restarts, desired one-image args, existing image digest unchanged. Logs show catalog image zero and origin ready; direct pod metrics confirm applied concurrency 0 and in-flight 0. Error: none.
- Next exact mutation: `kubectl --context=joolshev-scale-test -n unbounded-system patch ds racer-loadgen --type=json -p='[{"op":"test","path":"/spec/updateStrategy/type","value":"OnDelete"},{"op":"replace","path":"/spec/updateStrategy","value":{"type":"RollingUpdate","rollingUpdate":{"maxSurge":0,"maxUnavailable":"10%"}}}]'`.
- Fleet rollout phase deadline 5 minutes, heartbeat every 30 seconds using `python3 observe_local_cache.py 8`. Success is all 1,500 updated and Ready, all load paused. Parent retains recovery and resume responsibility.

## Checkpoint 2026-10-05T16:09:00Z: fleet passed, before resume

- RollingUpdate restored and completed: 1,500 updated/Ready, one identical argument set, zero container restarts. Ready local Gantry and origin EndpointSlices cover all 1,500 nodes.
- Prometheus stale old-pod series cleared: exactly 1,500 fresh loadgen series, C0 and in-flight 0. Dataplane requests/fills/deliveries 0 and ready 1,500. Error: none.
- Next exact mutation: `kubectl --context=joolshev-scale-test -n unbounded-system patch cm racer-loadgen-control --type=json -p='[{"op":"test","path":"/data/concurrency","value":"0"},{"op":"replace","path":"/data/concurrency","value":"2"}]'`.
- Resume phase deadline 5 minutes, heartbeat every 30 seconds. Success all 1,500 apply C2, verified throughput advances with no pull errors, peer work falls after warmup. Measure a clean five-minute interval after propagation. If error rates or readiness regress, parent will pause via the same live control and diagnose.

## Checkpoint 2026-10-05T16:13:10Z: resumed and warm

- ConfigMap C2 patch completed. All 1,500 loadgens applied C2, total concurrency/in-flight 3,000, ready 1,500 with no restarts observed. Dataplanes remained ready 1,500.
- Latest two-minute window: verified 2,225.89 GiB/s, plaintext lookup ratio 1, peer acquisitions 0/s, origin fills 0/s. Error-result series absent, consistent with counters created only when used; final check will include all non-success results and failure counters.
- Next: observe to 16:17:15Z to obtain a complete five-minute window after all readers applied C2; heartbeat every 30s. Deadline 5 minutes, success stable coverage, no errors and local-cache-dominated delivery. No cluster mutation planned.
- Repository-only phase: `timeout --signal=TERM --kill-after=10s 300s make fmt`, inspect resulting diff, then commit only this operational record and integrate into original racer-v2 branch. Deadline 5 minutes; success formatting exits 0 without unrelated source changes. Scripts are temporary and will not be committed.
- Error: none. Load remains running, not paused.

## Checkpoint 2026-10-05T16:17:37Z: verified steady-state success

Fixed Prometheus query time `1791217057.2874534`, five-minute rate window:

| Measurement | Result |
| --- | --- |
| Ready loadgens / dataplanes | 1,500 / 1,500 |
| Loadgens at C2 / advancing verified bytes | 1,500 / 1,500 |
| Verified delivery | 2,432,538,213,398.93 B/s, about 2.21 TiB/s |
| Successful image pulls | 4,480.20/s |
| Mean complete-image latency | 0.6696s |
| Plaintext lookup hits / misses | 170,245.89/s / 0/s |
| Plaintext conditional lookup hit ratio | 100% |
| Peer acquisitions / origin fills / disk hits | 0/s / 0/s / 0/s |
| Origin delivered bytes / corrupt misses | 0 B/s / 0/s |
| Non-success pulls / failure-counter series | None present |
| Verified-byte / plaintext-hit counter resets | 0 / 0 |
| p95 worker ciphertext charge / limit | 71.33% (not cache occupancy) |

- Results establish the intended warm-memory baseline. This is not a Zipf or large-working-set test. Source/lookup metrics are not generally an exhaustive client-hit partition, but zero misses/peer/origin work corroborate local reuse here.
- Throughput is consumer-verified application delivery, not NIC throughput. The previous ~320 GiB/s measurement used only 581 readers and a ~256 GiB catalog; aggregate improvement is not a controlled cache-only speedup.
- No cache flush, dataplane restart, image change, node-cap exception, or Gantry reconfiguration was performed. All 1,500 generators remain running at C2. Original RollingUpdate 10% policy restored.
- `make fmt` ran gofumpt successfully, then golangci-lint panicked because its Go 1.26 build cannot analyze a file requiring Go 1.27. This is an environment/tool compatibility failure, not a live rollout failure. `git diff --stat` confirmed no tracked source modifications. No application code changed, so no application tests were run.
- Next repository-only phase: inspect and commit this record, cherry-pick into original `racer-v2`, remove temporary scripts and worktree. Deadline 5 minutes, success original worktree clean and record retained; no further cluster mutations.
