# Racer saturation results: October 1, 2026

## Outcome

**GOAL NOT MET.** The campaign reached a stable workload plateau with all 1,500
nodes Ready and making verified progress, not demonstrated full NIC saturation.
The latest full five-minute window delivered **7,721.833455 Gbps verified image
goodput**. Aligned accelerated-VF counters reported **16,298.011 Gbps TX**, about
87% of the fleet's nominal 18,750 Gbps egress reference. Neither a physical
resource ceiling nor provider shaping enforcement was established. Internal
AEAD rejections continue; their cause remains unresolved.

This is a historical measurement report, not a new benchmark or cluster change.
The parent operator retains responsibility for the running C10 workload and
pause/recovery. No cluster mutation was performed to produce this document.

## Workload and measurement

- Context `joolshev-scale-test`, namespace `unbounded-system`; 1,500 participating
  nodes across the Adsv5, Ddv5, and Ddsv6 pools.
- Catalog: 512 images, eight layers per image, nominal 64 MiB layers with size
  jitter; SHA-256 verification enabled (`verify=true`). All origins and clients
  remained represented. This is image-pull traffic, not an isolated NIC test.
- Final applied concurrency: exactly 1,489 nodes at C10 and eleven impaired
  nodes capped at C1, totaling 14,901 in-flight pulls in the recorded snapshot.
- Latest full window ends **2026-10-01T16:23:21.883393Z**. Rates use Prometheus
  `rate(...[5m])`, in decimal Gbps (`bytes/s * 8 / 1e9`). Distribution statistics
  describe per-node five-minute rates, not instantaneous peaks or request latency.

| Measurement | Fleet sum (Gbps) | Mean/node | p05 | Median | p95 | Min | Max |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Verified image goodput | 7,721.833455 | 5.147889 | 3.755765 | 5.249458 | 6.238905 | 0.324103 | 7.230979 |
| Accelerated VF TX | 16,298.011 | 10.865340 | 8.569348 | 11.236333 | 12.206456 | 3.687902 | 12.771585 |

Aligned VF RX totaled **16,283.458 Gbps**. Synthetic `eth0` TX totaled
**15,585.686988 Gbps**, and RX 15,581.027461 Gbps. `eth0` is **not a physical NIC
measurement**. The approximately 4.57% VF-TX excess over synthetic TX is
consistent with segmentation/header accounting differences; it is not extra
independent delivered traffic. Never add VF and `eth0`, or TX and RX, to claim
useful throughput. Forwarding, protocol overhead, and failed work also separate
network traffic from verified application goodput.

The aligned VF selector was `device=~"ens.*|enP.*"`. Exactly one TX and one RX
series per node were confirmed for all 1,500 nodes before aggregation. Of these,
249 nodes had TX >=11.875 Gbps (95% of 12.5), and only six had TX >=12.5 Gbps.
The nominal 12.5 Gbps VM egress maximum is not guaranteed bandwidth or a measured
per-node policing threshold. The aggregate ratio and a few rates above that
reference do not prove fleet saturation or the presence/absence of a policer.

All 1,500 nodes had positive verified goodput, loadgen/dataplane scrape availability,
and `racer_ready=1`. Success rate was **1,800.741650 pulls/s**; non-success rate
was **0.954167 pulls/s**. The fleet error fraction was
**0.0005295936515**, approximately **0.05296%**, calculated as errors divided by
successes plus errors, not an average of node error percentages. Node CPU nonidle
median/p95 were 74.84%/82.94%; aggregate CPU is not evidence that every critical
worker has headroom. Container-resource CPU series were missing for all nodes in
this report and must not be interpreted as zero usage.

### Accounting contract and reproducible queries

`cmd/racer-loadgen/pull.go:185-213` credits verified bytes only after a complete
successful image pull with verification enabled, including manifest, config, and
all layers. Size and SHA-256 checks are at `pull.go:310-334`; received bytes are
counted even before a later failure (`pull.go:337-360`). Thus received bytes are
not verified goodput. The failure/unverified assertions at
`cmd/racer-loadgen/pull_test.go:215-252` explicitly require zero verified credit.

Evaluate these expressions at the window-end timestamp above, using
`monitoring/prometheus:9090`. Keep the node set identical across expressions.

```promql
sum by (node) (rate(racer_loadgen_verified_bytes_total{
  job="kubernetes-pods",app_kubernetes_io_name="racer-loadgen",
  namespace="unbounded-system",node!="",node=~".+"}[5m])) * 8 / 1e9

max by (node) (rate(node_network_transmit_bytes_total{
  job="node-exporter",node!="",node=~".+",device="eth0"}[5m])) * 8 / 1e9

sum by (node) (rate(node_network_transmit_bytes_total{
  job="node-exporter",node!="",node=~".+",device=~"ens.*|enP.*"}[5m])) * 8 / 1e9

count by (node) (node_network_transmit_bytes_total{
  job="node-exporter",node!="",node=~".+",device=~"ens.*|enP.*"})
```

Substitute `receive` for `transmit` to measure RX and validate its series count.
The final count expression must return 1 on each of the expected 1,500 nodes;
do not silently deduplicate or sum extra devices. Sum the per-node rates for
fleet totals. Use the same loadgen labels with
`rate(racer_loadgen_pulls_total{...,result="success"}[5m])` and
`result!="success"` for success/error rates. The ellipsis denotes the explicit
loadgen selector above, not literal PromQL. The saved helper report zero-fills
absent error series against the verified-byte node set, while separately
checking scrape coverage. Check `racer_loadgen_applied_concurrency`,
`racer_loadgen_in_flight`, `racer_ready`, and `up` per node rather than relying
only on sums.

## Changes and experiments

These were sequential controlled phases where possible, not a single randomized
experiment. Restarts, warmup, image changes, and unequal observation windows
limit causal attribution; a correctness fix is not automatically a throughput win.

| Change or experiment | Observed result and disposition |
| --- | --- |
| Owner-share balancing | Matched C8, same-image, opaque-off baseline rose from 6,657.761 to 7,607.221 Gbps verified (+14.26%). All 1,500 remained Ready/positive. Baseline owner-hotspot local goodput mean improved from 2.652 to 4.854 Gbps. Retained. |
| Eleven per-node C1 caps | Restored verified progress on all eleven impaired nodes without removing clients/origins. The caps baseline had 6,663.629 Gbps and 0.06779% errors versus 6,555.237 Gbps and 0.19043% previously. Retained; later excessive global concurrency could still impair them. |
| Gantry TCP `ReaderFrom` | Connection limiting had hidden the TCP fast path. After the fix, host sampling observed actual UDS-to-pipe and pipe-to-TCP kernel splice (610/748 samples, previously zero splice in 902). Matched C12 goodput changed only +0.20%, 7,729.245 to 7,744.516 Gbps; error fraction fell from 0.12974% to 0.07768%. Retained, without claiming a large fleet throughput win. |
| Higher concurrency | Weighted C12 gave only +1.60% over C8 with more errors. Post-splice C16 delivered 7,739.802 Gbps, no useful gain over C12, with 0.613919% errors and eleven zero-progress nodes. Rejected in favor of the stable C10 hold. |
| Opaque relay, through 300 nodes | Actual opaque traffic was observed, but the 300-node trial gained only 0.34%, below its 3% gate. Head time fell 121.359 to 102.478 ms while body time rose 71.868 to 91.043 ms: combined time stayed about 193.5 ms. Expansion reversed; all opaque relay ultimately disabled. |
| Larger memory budgets | Plaintext/ciphertext 6/9 GiB versus 4/6 GiB yielded 5,997.043 versus 6,050.191 Gbps, with worse errors. Restored 4/6 GiB; no memory-ceiling claim. |
| Additional page credit | Client window 3/server 2 versus 2/2 failed the useful-gain/no-error-regression gate. Restored 2/2. |
| TSO canary | Negative; original settings restored and verified. No fleet-wide offload recommendation follows. |
| RX checksum canaries | No fresh pair AEAD recurrence in the guarded baselines, so no changed stage ran. Inconclusive, not evidence for or against RX offload. Receiver-wide proposal was retired without execution or merge; no receiver-wide host mutation occurred. |
| Peer TCP_NODELAY | Enabled and verified on sampled accepted/outbound sockets. No established throughput gain or head/body-latency improvement; restart confounding remains. Currently true. |
| Buffer and syscall provenance | Isolated Miri reproduced Box handoff aliasing; Vec-backed IoBuffer and syscall-argument fixes addressed the demonstrated model/lifetime issue. They did not establish the production AEAD root cause. |

Other retained regression-backed fixes included auxiliary readiness polling and
preserving unrelated payload buffers on quota failures. Neither was established
as the cause of the campaign's performance plateau. The weighted model fit
baseline TX well (r=0.998684), but its fixed-demand constraints were not measured
hardware limits; higher achieved demand exceeded some modeled directional bounds.

## Integrity and unresolved diagnostics

The parent-reported **16:25 UTC** integrity observation is separate from the
16:23:21 throughput window: AEAD rejection counter total **312**, with an
extrapolated five-minute increase **132.628** across **76 nodes**, matching the
peer-fill corruption count. Fractional `increase()` is Prometheus extrapolation,
not a literal fractional event. CRC rejection, disk-fill corruption, and
retained-fill corruption counters were zero. No end-digest-failure series was
present; absence of that series is not an independent proof of zero corruption.
Successful verified image accounting remains stronger than raw received-byte
accounting, but internal rejects and pull errors remain real failures.

Relevant diagnostic counters are `racer_crypto_decrypt_aead_rejected_total`,
`racer_crypto_decrypt_crc_rejected_total`, and
`racer_fill_decrypt_{peer,disk,retained}_corrupt_total`; compare sums, per-node
`increase(...[5m])`, scrape coverage, and counter resets. Loadgen end-digest
diagnostics use `racer_loadgen_pull_failures_total{reason="digest_mismatch"}`.
These counters count attempts, not unique corrupt records or corrupt client
deliveries, and locate the rejection rather than its origin; see
the integrity-counter contract beside `Metrics` in
`cmd/racer-dataplane/src/telemetry.rs`. This current source reference does not
refresh the campaign observations above.
Sender/receiver CRC disagreement narrowed a boundary in one joined acquisition,
but did not attribute the cause to the kernel, NIC, network, or allocator.

Provider diagnostics were blocked by the absence of `az` and an existing usable
management context in the inspected environment, **not an observed authorization
denial**. The official VMSS/NIC metric catalogs expose traffic bytes, packets,
and flows, not the shaping-enforcement counters needed to establish policing.
See Microsoft's [VMSS metrics](https://learn.microsoft.com/en-us/azure/azure-monitor/reference/supported-metrics/microsoft-compute-virtualmachinescalesets-metrics)
and [NIC metrics](https://learn.microsoft.com/en-us/azure/azure-monitor/reference/supported-metrics/microsoft-network-networkinterfaces-metrics).
Provider enforcement evidence and discriminating AEAD diagnostics remain open
requirements. Do not relabel the workload plateau as a physical-resource ceiling.

## Final deployed state and recovery

All campaign commits were pushed on `racer-v2`; images were built through
`.github/workflows/images.yaml`. These are full source-commit image tags, not OCI
manifest digests:

| Component | Image tag | Deployment scope |
| --- | --- | --- |
| Racer dataplane | `1a37bd2e5718813b9eed7bfe1b7eb6dfc6a7a99a` | All 1,500 |
| Gantry | `dca35b1a4280c4bb33b9026d8f4fa5f7a4b2c8a3` | All 1,500 |
| Racer loadgen, cap-aware | `245441dc7595b28215b534a1ca1e29323e8e1b61` | Eleven updated canaries |
| Racer loadgen, previous | `23d3549d2b9f6e2e91fc09346fdf963b01ced883` | 1,489 retained pods |
| Operator and Racer controller | `c330224c1b07e36fd3188d30642ab0de6757837d` | Existing controller deployments |

The loadgen DaemonSet is intentionally **OnDelete**: eleven cap-aware canaries
were replaced, not an incomplete fleet rollout. Dataplane strategy is
**RollingUpdate, maxUnavailable 10%, maxSurge 0**. Opaque relay is off;
`RACER_PEER_TCP_NODELAY=true`; plaintext/ciphertext budgets are 4/6 GiB; client
and server page windows are 2/2. Weighted shares use candidate-map SHA-256
`65ac1f3576631495585aebb42374fef3bedeea6ba82179f31ed0b57b9dc9ba47`.
The planner artifact's `applied:false` describes its offline generation, not
live state: subsequent guarded application and fleet membership attestation
confirmed all 1,500 workers on the published candidate membership.

**Pause command, for the parent operator only; not executed for this report:**

```sh
timeout --signal=TERM --kill-after=10s 300s \
  kubectl --context joolshev-scale-test -n unbounded-system \
  patch cm racer-loadgen-control --type=merge \
  -p '{"data":{"concurrency":"0"}}'
```

C0 is asynchronous, not an instant drain. Confirm all 1,500 nodes have applied
zero and zero in-flight pulls, active requests, fills, peer exchanges, pending
disk writes, deliveries, and relay usage, with fresh complete scrape coverage.
The conservative gate uses 255-second history for 60-second scrapes, at least
four samples, freshness checks, and loadgen/dataplane `up` coverage; see
`hack/scripts/racer-shares-apply.py:28-37`, `:128-141`, and `:247-251`.
Do not roll images or alter membership based only on a successful patch or one
zero-valued sample. The parent owns safe recovery and any subsequent resumption.

## Evidence provenance and limits

Original local evidence was read under `/home/azureuser/code/unbounded/`:

- `tmp/racer-saturation-c10-syscall-steady.json`: timestamp and exact queries at
  lines 3-22; 1,500-node rate/readiness/error summaries at lines 22528-22636.
- `tmp/racer-saturation-20261001-checkpoint.md`: timestamped phase decisions,
  comparison windows, build/rollout checkpoints, and negative experiments.
- `tmp/racer-weight-plan-artifacts/plan.json`: candidate map and rollback data;
  `tmp/racer-membership-candidate-final.json`: post-application attestation.
- `tmp/loadgen-caps-245441dc/`, `tmp/racer-syscall-rollout-drain-ready/`, and
  `tmp/rx-canary-6o-7n-run.jsonl`: rollout, drain, and inconclusive-canary evidence.

These `tmp/` artifacts are ignored, local, and **not durable public proof**.
The VF alignment, 16:25 integrity results, and management-context inspection are
parent-supplied observations, not independently repeated by this documentation
task. The key queries, values, accounting contract, deployment identities, and
limitations are preserved here so the conclusion does not depend solely on
artifact filenames. Reproduction requires retained historical Prometheus data
and the same workload/membership; present-day queries cannot recreate this window.
