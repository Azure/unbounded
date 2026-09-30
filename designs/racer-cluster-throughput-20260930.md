# Racer cluster throughput, September 30, 2026

## Result and scope

On the existing **1,500-node `joolshev-scale-test` cluster**, the sustained
**10:46:10-10:54:10 UTC** C10 window delivered **695.829 GB/s of verified
complete-image goodput**, or **5.566632 Tbps**. That is **463.886 MB/s
(approximately 464 MB/s), 3.711 Gbps of usable throughput per node on average**.
All rates use decimal units; GiB below denotes binary memory capacity. The Tbps
figures here are conversions of the displayed three-decimal GB/s figures.

This is the **best observed throughput/error tradeoff among the session's tested
settings**, not a global optimum, full NIC saturation, or a claim that the work
is 100% done. Eleven known impaired nodes remain included in every fleet total.
**C10 was left active**, with no load shutdown or cluster changes by this
documentation task. This reports the recorded session exit, not a new live audit.

Final settings: concurrency 10 per loadgen, Racer/Gantry page windows 1,
reactor queue 4096, relay limit 256, ciphertext capacity 6 GiB, and host-network
dataplanes on all 1,500 nodes. Dataplane image:

```text
ghcr.io/azure/racer-dataplane@sha256:109ed5da340add037e4ce5e8859d90ae2d9a7a4cbdf017c7caab76e4edfd3db8
```

SHA-256 verification remained enabled. Verified bytes are credited only for a
successful complete image, including manifest, config, and layers, not partial
or failed downloads (`cmd/racer-loadgen/pull.go:177-204,293-307,321-337`, unchanged
between the deployed loadgen's source `d738fa9f` and documentation base
`f2f2f392`). This preserves the design's no-corrupt-data requirement; no
correctness check was disabled for throughput. Removed operational guard
machinery is not part of this result or its reproduction.

## Sustained measurement

| Pool | Nodes | Verified GB/s | Mean verified MB/s/node | Mean eth0 TX Gbps/node |
| --- | ---: | ---: | ---: | ---: |
| adsv5 | 447 | 204.887 | 458.360 | 9.548 |
| ddv5 | 550 | 261.597 | 475.630 | 9.516 |
| ddsv6 | 500 | 227.925 | 455.850 | 9.333 |
| system | 3 | 1.420 | 473.500 | 8.868 |
| **Fleet** | **1,500** | **695.829** | **463.886** | **9.463** |

- Host eth0 TX: **1,774.358 GB/s = 14.194864 Tbps**, averaging **9.463 Gbps/node**.
  RX: **1,772.460 GB/s**. TX and RX are separate directions, not additive useful
  throughput; host traffic includes forwarding and other overhead.
- Average TX is **75.706%** of the nominal **12.5 Gbps/node egress maximum**.
  That maximum is not a guaranteed allocation or a demonstrated achievable
  rate. Only **161/1,500 nodes** averaged at least **11.25 Gbps**, or 90% of it.
- Endpoint counter differences: **623,481 successful pulls, 7,191 errors**;
  `errors / (successes + errors)` = **1.140212%** (1.140% rounded), with **870
  overload events**. Overloads are not an additional pull-error count.
- The impaired eleven ddsv6 nodes produced **41 successes, 1,375 errors, 399
  overloads**; **six had zero verified goodput**. They are not recovered.
- Full 1,500-node coverage for goodput, RX, TX, overload/source metrics and each
  component's CPU rates; eight goodput samples per node; applied-concurrency
  minimum and maximum both 10 throughout. Five monitored jobs each reported
  1,500 up targets, including window minima. No measured counter resets;
  maximum sample age 59.994 seconds.
- The 10:54:24 UTC inventory found 1,500 Ready dataplane/Gantry/loadgen pods
  each, three Ready controllers and one Ready current-image operator; expected
  running digests matched, with zero reported restarts/deletions. Nine older,
  non-Ready operator-prefix pods remained unclassified after a bounded follow-up
  timed out. This is not an assertion that every operator-prefix pod was healthy.

Peer reception events plus host traffic establish nonlocal peer activity.
Source-event counts are **not byte-share percentages**. Zero eth0 netdev
error/drop counters do not exclude qdisc or fabric loss. The JSON retains pool
percentiles, source rates, CPU rates, and the impaired-node breakdown.

## Selected trials, not controlled causal comparisons

| Setting | UTC window | Verified GB/s | Pull error fraction |
| --- | --- | ---: | ---: |
| C1, old `d738fa9f` build, earlier mixed network placement | 07:15:15-07:17:15 | 211.211 | 0.285% |
| C12, final `109ed5da` image | 10:15:35-10:17:35 | 703.292 | 3.289% |
| C14, final image | 10:39:55-10:41:55 | 695.551 | 14.279% |
| **C10, final image, sustained** | **10:46:10-10:54:10** | **695.829** | **1.140%** |

C12 was faster but less reliable; C14 added traffic/errors without useful gain.
C1 is historical context, not an isolated measure of a code fix. Short trials
used `rate[2m]`; the final result uses `rate[8m]`. Sequential trials, cache state,
different durations and earlier image/configuration changes limit causal claims.

Rejected trials were rolled back: ciphertext 8 GiB to 6 GiB; paired page windows
3 to 2 (later reduced to the retained 1); one-node MANA TX ring 1024 to 256;
relay limit 288 to 256. The ring trial eliminated queue-stop increments but
worsened that node's verified goodput, so stop counts alone were not a recovery
signal. Higher C16 on the older image was also rejected, not treated as a direct
comparison to the final build. These trials do not prove a thread bottleneck.
The recorded four I/O/four crypto thread arrangement (`SESSION-CHECKPOINT.md:158`)
and aggregate CPU rates do not establish a per-thread ceiling or justify a
speculative thread split.

The eleven-node platform problem remains unresolved. Recovery requires Azure
access and approval for preservation of ephemeral data before potentially
disruptive platform actions; no usable Azure authentication was available to
this session. No platform recovery, destructive action, or new protocol is
claimed or prescribed here.

## Fix and image provenance

The integrated fixes, with source references at `f2f2f392`, are:

- `a3cddc2e2a4f66536c7a3740a819754a92fade93`: explicit controller UID for the
  projected token (`deploy/racer/controller.yaml.tmpl:24-28,81-88`). Applied as
  an authoritative deployment override; the controller image remained the
  earlier `d738fa9f` build, not a newly built token-fix image.
- `27558690886714746129be865d1365ec6a3de9ba`: reserve fallback time for slow peer
  bodies without extending the signed deadline
  (`cmd/racer-dataplane/src/read/candidates.rs:479-510`).
  [Dataplane image workflow 36691940953](https://github.com/Azure/unbounded/actions/runs/36691940953).
- `f2f2f3929d4073945720a033fcf7a534214497e4`: cancel materialized relay work on
  upstream disconnect, awaiting cleanup before reuse
  (`cmd/racer-dataplane/src/peer/server.rs:728-783`).
  [Final dataplane image workflow 36700154407](https://github.com/Azure/unbounded/actions/runs/36700154407).

Both dataplane workflows succeeded for `linux/amd64`. Initial `d738fa9f` image
workflows also succeeded:
[operator 36677778011](https://github.com/Azure/unbounded/actions/runs/36677778011),
[controller 36677780668](https://github.com/Azure/unbounded/actions/runs/36677780668),
[dataplane 36677783653](https://github.com/Azure/unbounded/actions/runs/36677783653),
[Gantry 36677786728](https://github.com/Azure/unbounded/actions/runs/36677786728),
[loadgen 36677788975](https://github.com/Azure/unbounded/actions/runs/36677788975).
Full running component digests are in the sustained JSON's `health` object.

Recorded validation: nine peer-server and 26 reactor focused tests passed.
The integrated Rust library run at `f2f2f392` finished with **925 passed, two
failed, 13 ignored**. The two failures were classified from unchanged code and
parent history as preexisting fixture/implementation mismatches: admission
staging size (`app_peer_tests.rs:385`) and signed route deadline expectations
(`read/fill_peer_tests.rs:262-271`). No separate baseline suite was run to prove
that classification. **The broader suite was not green.** This documentation
does not edit tests/dependencies or rerun that suite.

## Reproducing the calculation

Primary evidence was read from `.worktrees/racer-cluster-session/` before
archiving: `tmp/sustained-c10-20260930T105410Z.json` (settings/coverage/totals/pools/
health/method), `tmp/sustained-c10-summary.md`, and
`tmp/sustained-c10-checkpoint.md`. Trial evidence: `SESSION-CHECKPOINT.md:67-69`
(C1), `tmp/relay-fix-measurement-20260930T101735Z.json`, and
`tmp/relay-fix-c14-20260930T104155Z.json`. Test evidence:
`tmp/relay-suite-checkpoint.md:15-35`. These are session artifacts, not assumed
permanent files in a fresh checkout; retain them in the session archive.

Collector: `tmp/racer-hostnet-c4-measure.py:20-92` constructs queries and
`:95-124` evaluates them at a fixed time. The actual collection began
10:54:10.000108 UTC and ended 10:54:12.821 UTC. Use the retained Prometheus data
with evaluation time **`2026-09-30T10:54:10Z`**, not the current time:

```promql
# Fleet verified GB/s and host TX GB/s, respectively.
sum(rate(racer_loadgen_verified_bytes_total{job="gantry-bench-client"}[8m])) / 1e9
sum(rate(node_network_transmit_bytes_total{job="node-exporter",device="eth0"}[8m])) / 1e9

# Coverage: these first two counts must each be 1500, including zero-goodput nodes.
count(rate(racer_loadgen_verified_bytes_total{job="gantry-bench-client"}[8m]))
count(rate(node_network_transmit_bytes_total{job="node-exporter",device="eth0"}[8m]))
# Sample count 8 and concurrency min/max 10 must each cover all 1500 nodes.
count_values("samples", count_over_time(racer_loadgen_verified_bytes_total{job="gantry-bench-client"}[8m]))
count_values("minimum", min by(node)(min_over_time(racer_loadgen_applied_concurrency{job="gantry-bench-client"}[8m])))
count_values("maximum", max by(node)(max_over_time(racer_loadgen_applied_concurrency{job="gantry-bench-client"}[8m])))
count by(job)(min_over_time(up{job=~"gantry-bench-.*|node-exporter"}[8m]) == 1)

# Pull statistics: endpoint differences by result, not extrapolated increase().
sum by(result)(racer_loadgen_pulls_total{job="gantry-bench-client"})
- (sum by(result)(racer_loadgen_pulls_total{job="gantry-bench-client"} offset 8m)
   or (0 * sum by(result)(racer_loadgen_pulls_total{job="gantry-bench-client"})))
sum(resets(racer_loadgen_pulls_total{job="gantry-bench-client"}[8m]))
sum(resets(racer_loadgen_verified_bytes_total{job="gantry-bench-client"}[8m]))
sum(racer_overloads_total{job="gantry-bench-dataplane"})
- sum(racer_overloads_total{job="gantry-bench-dataplane"} offset 8m)

# Nodes averaging at least 90% of nominal maximum egress.
count(sum by(node)(rate(node_network_transmit_bytes_total{job="node-exporter",device="eth0"}[8m])) * 8 / 1e9 >= 11.25)
```

Pool aggregation uses `label_replace(...,"pool","$1","node","aks-([^-]+)-.*")`
then `sum by(pool)`. Divide verified GB/s by node count and multiply by 1,000
for mean MB/s; divide pool TX GB/s by node count and multiply by eight for Gbps.
Counter endpoints come from staggered scrapes, so the integer differences are
approximate event-window counts, not synchronized event timestamps. Check
coverage and resets rather than treating missing series as zeros.

The recorded read-only collection command, from the operational worktree with
the intended Kubernetes context selected, was:

```sh
timeout --signal=TERM --kill-after=10s 30s \
  python3 tmp/racer-hostnet-c4-measure.py 2026-09-30T10:54:10Z 8m
```

It queries the Prometheus service API through Kubernetes; it does not start a
new load run. Reproduction requires retained historical series and the session
collector, not a fresh cluster rollout or a promise of identical performance.
