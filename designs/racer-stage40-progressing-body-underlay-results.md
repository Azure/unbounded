# Stage40: progressing-body underlay qualification

## Decision

Rejected the already-built dataplane `16ab2b76169d50a2862b9e0ebf696f3440ccf162`
(`sha256:5942b1fe5280c32d9c099ce7106f598629dff87a1c02d3015e2a25860c071c28`).
One C2 window failed all-node positive goodput and the 586 GB/s retention floor.
C4 was therefore not attempted; neither was underlay C6. Restored the exact
original pod-network `5e1a4554` configuration and live C6.

## Exact measurement

UTC September 29, 2026, **15:41:12.678098 to 15:46:12.678098**, all 1500 nodes,
including all eleven historical MANA failure hosts:

| Gate or measurement | C2 result |
| --- | ---: |
| Fully verified goodput, decimal GB/s | 459.8761593191221 |
| Aggregate complete-image success | 99.74277118769612% |
| Positive verified nodes | 1499/1500 |
| Nodes below 99% individual success | 105 |
| Exact telemetry coverage and C2 gauge | Passed |
| Workload health changes / new warnings / node changes | 0 / 0 / 0 |
| Checked counter resets | 0 |
| Host eth0 TX / RX, Tb/s | 9.35703380125907 / 9.348809158434738 |
| Mean host / dataplane CPU, logical cores | 3.003938 / 1.800330 |

Goodput is `rate(racer_loadgen_verified_bytes_total[5m])/1e9`, not received
bytes or raw NIC throughput. The successful verified-pull accounting is at
`cmd/racer-loadgen/pull.go:185-198`; metric definition is at
`cmd/racer-loadgen/metrics.go:39`. The retained collector evaluates every query
at the fixed window end and checks exact expected node sets, control minima and
maxima, scrape continuity, resets, and workload/node health.

No replacement baseline benchmark, warmup window, tests, builds, budget changes,
NIC tuning, node exclusions, or application source changes were performed.
A post-rollout workload snapshot served only as the window's health reference.

## Remaining bottleneck

`aks-ddsv6-84072342-vmss0000bl` was the sole zero-goodput node and had 0% complete
image success. The other ten historical problem hosts achieved approximately
100% complete-image success, but only 0.017991-0.035685 GB/s each. Their successful
layer means ranged from 11.707 to 26.514 seconds. This is encouraging relative to
stage36, not a controlled causal proof of the image fix: these are separate runs.

The remaining observed failure is end-to-end delivery under peer/admission and
queue pressure, not a demonstrated hard physical NIC ceiling:

- `0000bl`'s retained 128-entry failure ring contains peer-head/checkout and
  candidate-exchange overloads, candidate exhaustion, unavailability, and
  canceled reads/bodies. Several other cohort rings are dominated by ciphertext
  admission rejections near the unchanged 1,610,612,736-byte limit.
- A separate approximately 342-second diagnostic interval recorded **33,027**
  `ens1` child-qdisc drops on `0000bl`, **10,656** on `00005f`, and **1,588** on
  `000066`. These are not exact five-minute benchmark counters. Independent MANA
  hardware TX was 4.424-5.061 Gbit/s across the cohort; physical RX/TX drop counters
  did not increase. Wire activity is not complete-image goodput.
- `00003z` still recorded partially received 16 MiB bodies ending in
  `DeadlineExceeded`, including one record whose original deadline equals its
  recorded current time. That is evidence of a remaining outer-deadline failure,
  not proof that the old progressing-body candidate-share failure persists.
- The rings are bounded samples captured after the window, not exhaustive
  counts or proof of a single causal bottleneck. Stage40 does not establish a
  safe higher global concurrency or a physical hardware cure.

Evidence: `tmp/racer-stage40-c2-{summary,raw,details,cohort-summary}.json`,
`tmp/racer-stage40-c2-per-node.csv`, and `tmp/racer-stage40-analysis.json`.
All 1500 per-node rows and all eleven diagnostic hosts are retained.

## Rollout and restoration

Resumed the existing `ops/racer-stage40` worktree after the initial drain and
stage27 guard verification. Reused those exact guards and boot gate. Paused the
operator while coordinating host-network 18082, diagnostics 19090, the image,
and Prometheus scrape port. Verified all 1500 runtime image IDs, membership hash,
NodeIP listeners, local readiness responses, and guard jumps before measuring.
Operator d3, controller 0c23, Gantry b8, loadgen 208, catalog512, and budgets were
preserved. Global configuration changes necessarily rolled the controller;
its image and final original spec were preserved.

Both transport transitions encountered pre-serving bootstrap `DeadlineExceeded`
and kubelet backoff. At C0, cleared only unready exit-1 backoff pods (9 during
activation, 100 plus 2 during restoration); sampled previous logs confirmed the
bootstrap timeout. No healthy pods were restarted by that recovery action.
These startup failures are outside the measurement window. A helper initially
mistook historical terminal operator pods for running pods and stopped before
configuration mutation; it was corrected to ignore terminal pods. Roll polling
was also corrected to require the observed DaemonSet generation.

The existing bounded affinity watcher was reused across both rolls. It remapped
and verified `00007r`'s underlay PID486911 and restored PID517112 away from CPUs6/7.
Final inspection at 16:05 UTC showed all dataplane and io_uring worker affinities
off those CPUs. This remains a per-process quarantine, not a persistent fix for
future unattended process replacements. Watcher pods were removed afterward.

Final audit at **16:05:17 UTC**:

- All three workload DaemonSets 1500/1500 Ready and Available; all 1500 nodes
  Ready with unchanged UIDs/boot IDs.
- Exact original workload and operator/controller specs, protected ConfigMap
  data, Prometheus configuration, and recorded identity metadata restored.
- All 1500 dataplanes running original digest
  `sha256:5c001d5d0345783b34346e15b30b633ab48e62ab1c83e74cbcbbb437d7de3fc2`.
- Pod endpoint membership hash independently recomputed and matched.
- All 1500 task-owned guards removed only after no underlay listeners remained;
  unrelated INPUT rules preserved. Guard ConfigMap/DaemonSet and watcher pods gone.
- Live C6 applied on all 1500; recovery observation: **1500 positive**,
  **568.4134563813668 GB/s**, **99.31134417641591%** success over the latest two
  minutes. This is a recovery check, not a second qualification benchmark.
- Controller has one serving Ready leader, not three Ready replicas. Original
  leader-only readiness behavior/status was not presented as 3/3 controller health.

Restoration evidence: `tmp/racer-stage40-final.json`,
`tmp/racer-stage40-recovery-audit-1790697917422812265.json`,
`tmp/racer-stage40-recovery-membership.json`, `tmp/racer-stage40-guard-removal.json`,
and `tmp/racer-stage40-affinity-diag-log-*.json`. Checkpoints are in
`tmp/racer-stage40-progress.md`. Unrelated parent design/protocol files were untouched.
