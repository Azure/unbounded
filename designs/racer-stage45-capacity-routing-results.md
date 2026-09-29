# Stage45: exact minimal capacity routing rollout and rejection

## Decision

**Reject the combined v4/shares1 experiment and restore pod-network5e1/v3,
uniform shares4, C6.** One clean five-minute window passed aggregate success,
all1500-positive, telemetry and health gates, but the eleven-node cohort lost
15.355983% verified goodput. No C7, underlay trial, NIC tuning or reboot.

| Metric | Stage43 uniform v3 baseline | Stage45 v4 + eleven shares1 |
| --- | ---: | ---: |
| Fleet verified GB/s | 586.074856 | 591.481548 |
| Fleet complete-image success % | 99.409107 | 99.718341 |
| Positive nodes | 1500 | 1500 |
| Cohort verified GB/s | 1.241981 | 1.051263 |
| Cohort host eth0 TX Gbit/s | 21.131517 | 19.193837 |
| Cohort host eth0 RX Gbit/s | 25.939342 | 26.479629 |
| Cohort mean host busy logical cores | 4.854663 | 4.606178 |
| Cohort mean dataplane cores | 2.615557 | 2.539785 |

Fleet goodput increased0.922526%, satisfying the no-more-than2% regression gate.
Material cohort improvement was fixed as >5% before collecting; the observed
decline fails even a weaker positive-gain gate. Every cohort node lost goodput.
The v4 result is better than stage43's owner-only0.835982GB/s, but that is not
sufficient to beat the original uniform-shares baseline or retain this change.

## Exact publication and deployment

- Release commit `3fc4194ef01075dfbaee4933c67152b3033070f2`, direct parent
  `5e1a4554f8034fef676d5ca91315da80646e5b71`.
- Dedicated remote branch `release/racer-stage45-capacity-routing` points to
  that exact commit. Clean parent integration `a0dfaf8c` was pushed to
  `racer-v2`; its concurrent protocol work and request-path documents were not
  deployed or edited.
- [images.yaml run36603365291](https://github.com/Azure/unbounded/actions/runs/36603365291)
  completed successfully at the exact release SHA, image `racer-dataplane`,
  platform `linux/amd64`. Registry inspection verified the platform manifest
  and index digest:
  `sha256:f455bd6b211a4fae140ad8ddeb0a5888375d829c2eea6a1edb3049745bdd689c`.
- All1500 running imageIDs matched that digest and all1500 pod specs had explicit
  `RACER_ROUTING_ALGORITHM=4`. The authoritative
  `unbounded-component-overrides/racer-v2.yaml` supplied both image and env;
  merely changing the envFrom ConfigMap would not override the explicit env3.
- C0 reached all1500 and zero in-flight pulls before the100% coordinated rollout.
  No loadgen or Gantry process was restarted. Their readiness conditions changed
  transiently during the dataplane outage, so outside-window preservation uses
  process fingerprints rather than claiming unchanged readiness timestamps.
- Full fleet rollout converged naturally. Startup retries occurred before the
  measurement; an attempted backoff cleanup selected zero pods and deleted none.
  The00007r affinity watcher ran across process replacements and verified new
  PID620518 off CPUs6/7 before load. It waits for the complete runtime thread
  topology before moving threads, so it is not a bootstrap-time hard cpuset.
- Exactly the stage41 eleven UID-bound Nodes received supported administrator
  `racer.unbounded-cloud.io/shares=1`; remaining1489 retained4. A stale batch
  resourceVersion dry-run failed before any mutation. Each Node was then read,
  server-dry-run and applied immediately with UID/resourceVersion guards intact.
- Weighted publication membership5029/sequence5030 independently matched the
  complete1500 UID/endpoints/rails and shares, hash
  `eee553669ef13786271209f6cee5f654224c317f64e4d6f5d90e53e78322bb7c`.
  A200-second refresh/drain allowance elapsed at C0 before C6 resumed. Per-process
  accepted membership hashes are not observable; publication plus elapsed normal
  refresh bounds is the explicit qualifier, not exact acceptance proof.

Pod networking, ports8082/9090, catalog512, full membership/local clients, loadgen
verification and2-minute pull timeout, all budgets/resources/probes, controller,
operator, Gantry and loadgen images remained unchanged. No host-network guard was
installed. No identity/disk/secret settings were changed. No source tests were
repeated; the release's recorded22 Rust and4 focused Python checks were reused.

## One measured window

UTC2026-09-29 **17:39:47.815493 to17:44:47.815493**, C6 on every node throughout.
This was the only candidate five-minute window. All five telemetry jobs had exact
1500-node coverage and continuously-up series; applied min/max were6 everywhere.
The retained collector marked the window eligible: zero relevant counter resets,
no workload fingerprint/readiness changes, new warning events, node UID/boot
changes or unhealthy endpoints. All4500 workload pods were Ready at collection.
Fleet host eth0 TX/RX were13.055980/12.096008Tbit/s.

Verified goodput credits only successful complete SHA256-verified images,
including manifest/config, not received bytes or NIC bytes. Units are decimal.
Host eth0 includes other traffic. CPU is busy logical-core usage. Individual-node
success need not reach99%; the specified gate is aggregate success plus positive
goodput everywhere and clean health. Periodic checks cannot exclude events too
brief to appear in API or scrape observations.

## All eleven: observed networking and CPU versus model

Names use prefix `aks-ddsv6-84072342-vmss`. Cells are baseline -> v4.

| Suffix | Verified GB/s | TX Gbit/s | RX Gbit/s | Host cores | DP cores | Modeled transit Gbit/s, uniform -> both |
| --- | --- | --- | --- | --- | --- | --- |
| 00000h | .103136 -> .088849 | 1.956812 -> 1.759637 | 2.581492 -> 2.533009 | 5.038916 -> 4.696167 | 2.833266 -> 2.658139 | 5.284 -> 3.666 |
| 00002i | .115718 -> .093240 | 1.848676 -> 1.713032 | 2.358990 -> 2.552247 | 4.846667 -> 4.583292 | 2.708220 -> 2.255024 | 4.200 -> 2.871 |
| 00003z | .100479 -> .095564 | 1.962650 -> 1.789441 | 2.158542 -> 2.367607 | 4.694708 -> 4.675042 | 2.167595 -> 2.614816 | 4.158 -> 2.767 |
| 00005f | .123142 -> .092400 | 1.945379 -> 1.695728 | 2.178173 -> 2.135492 | 4.820500 -> 4.505625 | 2.565878 -> 2.464655 | 4.280 -> 2.846 |
| 000066 | .113725 -> .091113 | 1.932614 -> 1.761664 | 2.385115 -> 2.300807 | 4.935667 -> 4.569042 | 2.734065 -> 2.545106 | 5.224 -> 3.558 |
| 00006d | .107273 -> .094342 | 1.898182 -> 1.775397 | 2.560449 -> 2.614977 | 4.993583 -> 4.828708 | 2.811187 -> 2.805119 | 5.245 -> 3.477 |
| 000078 | .115512 -> .096240 | 1.902910 -> 1.789973 | 2.158637 -> 2.080807 | 4.776667 -> 4.503667 | 2.498402 -> 2.420996 | 4.413 -> 2.983 |
| 0000b1 | .109790 -> .096528 | 1.840816 -> 1.702641 | 2.537812 -> 2.442458 | 4.752625 -> 4.654667 | 2.649360 -> 2.521002 | 5.296 -> 3.540 |
| 0000bl | .121767 -> .102522 | 1.958064 -> 1.676375 | 2.280465 -> 2.193580 | 4.753000 -> 4.414250 | 2.566166 -> 2.442450 | 5.024 -> 3.456 |
| 0000cp | .118682 -> .102254 | 1.941545 -> 1.752847 | 2.216977 -> 2.525663 | 4.791083 -> 4.549875 | 2.536880 -> 2.529338 | 4.291 -> 2.899 |
| 0000d9 | .112758 -> .098212 | 1.943867 -> 1.777101 | 2.522691 -> 2.732981 | 4.997875 -> 4.687625 | 2.700107 -> 2.680991 | 5.322 -> 3.460 |

Stage44's primary-hit model predicted cohort transit52.736 ->35.523Gbit/s,
TX87.173 ->47.823 and RX62.665 ->45.456. Those are expected payload flows,
not measured host traffic. Here measured TX and host CPU decreased on all11,
but aggregate RX increased while useful goodput decreased. There is no direct
transit byte measurement to establish that the modeled32.64% transit reduction
occurred. Lower TX alone cannot establish receiver headroom or NIC saturation.
The optimistic modeled1.797GB/s cohort sensitivity was not realized.

The implementation weights eligible shortest next hops from authenticated shares
(`cmd/racer-dataplane/src/topology/paths.rs:311-390` at release), and shares also
affect ownership. V4 has a different hash domain from v3 even with uniform shares.
This is intentionally the combined routing/ownership experiment, not an isolated
shares comparison. Sequential windows, a process rollout and changed cache state
also limit causal attribution. The structural model omits retry/coalescing/cache
and queue dynamics. The negative live retention result takes precedence over the
model. A separate underlay proposal was conditional on a successful pod result;
that condition is unmet. Diagnose the model/live discrepancy before another trial.

## Evidence and rollback

Raw ignored artifacts are under `tmp/racer-stage45-`: `publication.json`,
`activation.json`, `runtime-verification.json`, `applied-*.json`,
`weighted-membership.json`, `c6-{plan,raw,summary,health-review}.json`,
`c6-per-node.csv` (all1500), `comparison.json`, health snapshots and affinity logs.
`tmp/racer-stage45-progress.md` records every phase. Baseline rows are retained in
`tmp/racer-stage43-baseline-per-node.csv`; model data in
`designs/racer-stage44-model.json`. No credentials are stored in these reports.

All11 administrator annotations were removed with UID/resourceVersion/current
value guards. C0 then drained to zero in-flight before restoring the exact
original operator override for5e1/v3 with a100% rollout and renewed00007r watcher.
Rollback startup hit the same exit1 `DeadlineExceeded` backoff as earlier fleet
rollouts.87 still-unready backoff pods were recreated at C0 after inspecting
representative previous logs; no healthy pod was selected and no budgets changed.
All1500 recovered by17:57:34. Uniform shares4 publication5057/sequence5058 was
independently verified, with current pod endpoints. C6 resumed at17:58:16.

Final audit at18:01:37 verified all4500 Ready, all1500 applied C6 and positive
verified throughput, all1500 runtime imageIDs at the original5e1 digest
`sha256:5c001d5d0345783b34346e15b30b633ab48e62ab1c83e74cbcbbb437d7de3fc2`,
exact original workload specs and protected ConfigMap/deployment settings,
original node UIDs/bootIDs/labels, unchanged Gantry/loadgen/controller processes,
identity hashes and cache UID. The recovery-only two-minute observation was
583.736260GB/s and99.308500% success, not an additional benchmark. Rollback
PID654095 and its io_uring workers remain off CPUs6/7; temporary affinity pods
were removed, without restoring the quarantined CPUs.
