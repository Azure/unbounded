# Stage36: lower-concurrency secured underlay

## Decision

Rejected underlay C2. Do not increase globally to C4 or repeat C6. Restore the
accepted pod-network C6 configuration. No image, application, NIC, or protocol fix
was deployed. All 1,500 nodes were included, including the eleven slow hosts.

Single exact Prometheus window, September 29, 2026:
13:02:54.559301 through 13:07:54.559301 UTC.

| Metric | Underlay C2 |
|---|---:|
| Fully verified complete-image goodput | 549.464107 GB/s |
| Full-image success | 99.905379% |
| Positive verified nodes | 1,495 / 1,500 |
| Nodes below 99% individual success | 68 |
| Host eth0 TX / RX | 11.172836 / 11.159920 Tb/s |
| Mean host / dataplane CPU | 3.582828 / 2.186286 logical cores |
| Dataplane request errors / overloads | 2.583492 / 0.061272 per second |
| NIC error increase | 0 |

Full expected-node metric coverage, C2 minimum/maximum on every node, zero counter
resets, no workload health changes, no new warnings or node identity/boot changes.
This point is 6.46% below the historical accepted 587.395362 GB/s pod C6 result,
and fails the all-positive requirement. Aggregate success hides severe cohort
failure; it does not qualify this configuration.

Evidence: `tmp/racer-stage36-c2-{plan,raw,summary,health-review,details}.json`,
`c2-per-node.csv`, compact start/end workload health, and `c2-compact.json`.
Byte credit requires complete verified images (`cmd/racer-loadgen/metrics.go:39`),
not NIC traffic or partially successful bodies.

## Eleven-node cohort, none excluded

All names have prefix `aks-ddsv6-84072342-vmss`.

| Suffix | Verified MB/s | Full success % | Mean successful layer/body seconds |
|---|---:|---:|---:|
| 00000h | 8.639 | 28.571 | 20.729 |
| 00002i | 13.451 | 54.545 | 21.183 |
| 00003z | 2.233 | 7.692 | 19.488 |
| 00005f | 0 | 0 | 15.643 |
| 000066 | 0 | 0 | 24.941 |
| 00006d | 6.804 | 27.273 | 22.677 |
| 000078 | 0 | 0 | 23.985 |
| 0000b1 | 13.353 | 50.000 | 21.183 |
| 0000bl | 0 | 0 | 23.611 |
| 0000cp | 0 | 0 | 21.798 |
| 0000d9 | 6.430 | 23.077 | 19.755 |

Host eth0 TX was 4.078-4.413 Gbit/s. Independently collected MANA hardware TX
was 4.298-4.617 Gbit/s over approximately 310 seconds, not the exact Prometheus
window. Thus the earlier approximately 2 Gbit/s observation is not a fixed
ceiling in this configuration. More transmitted bytes did not produce reliable
complete images. Do not equate NIC TX to local verified receive goodput.

All eleven had zero observed VF error/drop increments, zero ens1 leaf-qdisc drop
increments, and zero firewall rejects. Queue-stop increments ranged 0-1,746.
Bounded failure rings show PeerReceiveBody/PeerRelay DeadlineExceeded, candidate
deadlines, some Unavailable and admission overload. These are overwriteable
samples, not complete unique failure counts. Successful-body means exclude
failures; full histograms and error counts are retained in fixed-time details.
The evidence does not establish a physical NIC cure or a single root cause.

## Exact requirements for the next minimal loadgen control fix

The measured requirement is **less local offered work than C2 with layer
concurrency 4 on each of these eleven nodes**, not a proven safe C1 setting.
No positive integer image concurrency below 2 other than 1 exists; C1 is the
next bounded candidate, not validated here. Local reduction may be insufficient
because these nodes also serve remote peer traffic. If C1 fails, reduce local
layer admission (4 to 1) or introduce bounded pacing before considering any
global increase. Do not increase deadlines or count partial images as success.

Minimal implementation contract:

1. Keep the existing scalar concurrency file and backward-compatible CLI fallback.
   Add an optional separate node-limit file keyed by exact Downward API node name.
   Effective concurrency is `min(global, node_limit)`. Global zero must always
   drain all nodes. No node selectors, membership exclusion, new protocol, or
   per-node DaemonSet/template-hash rollout is needed for subsequent limit changes.
2. Initial experimental limits: the eleven exact names above each cap 1; other
   1,489 nodes retain global control. Keep all 1,500 in metrics and the denominator.
   This is a proposed experiment map, NOT an accepted production setting.
3. Missing entries use global control; malformed/unreadable updates retain the
   last known-good local limit. Validate finite integers and bound document size.
   A bad document must never reset a constrained node to unrestricted concurrency.
4. Reuse bounded worker slots and parking: current `concurrency.go:64-104` creates
   slots lazily, and `106-143` lets already admitted pulls finish under their
   existing timeout. Export global, node-cap and effective limits separately;
   existing applied gauge must report the effective value, with in-flight gauge
   retained. Keep origins running.
5. If automatic adaptation is added, decide locally from completed verified-image
   goodput, full-pull failure/timeout counts and body latency, not NIC throughput
   or fleet aggregate success. Use bounded multiplicative decrease, minimum 1,
   hysteresis and a complete pull-timeout drain before evaluating a reduction.
   Do not treat zero attempts as 100% success or grow on partial-byte progress.
6. Qualification remains one exact five-minute settled window: all expected nodes
   positive, aggregate success >=99%, visible per-node failures, clean health,
   and total verified goodput above accepted 587.395362 GB/s. Do not run C4 globally
   until constrained nodes have demonstrated successful complete images.
7. Focused future tests should cover exact-node/default selection, global-zero
   precedence, invalid/oversize documents, retained safe limits, rapid updates,
   draining without overlapping workers, and visible zero-goodput/failure nodes.

The current parser only accepts one integer (`concurrency.go:25-61`); current
polling uses one global file (`146-194`). No adaptive/per-node support is claimed
to exist in deployed image 208. This task stops at an actionable contract rather
than building an unqualified new image.

## Security and operational scope

Reused exact stage27 IPv4 NodeIP-only listeners, 1,500-IP peer set, diagnostic
local/Prometheus source restrictions and fail-closed init gate. Verified all
1,500 guards before activation and all 1,500 live listeners afterward. No broad
ACCEPT, wildcard listener, chain/conntrack flush, NIC tuning, reboot, disk or
secret operation. Operator d3, dataplane f19, controller 0c23, loadgen 208 and
Gantry pins remained unchanged.

100% dataplane transitions ran at C0. Bootstrap DeadlineExceeded retries were
outside measurement. Compact API health avoids repeated huge Pod JSON captures.
The existing bounded 00007r watcher was renewed with overlap across transitions
and measurement; application/helper affinities remain singleton off CPUs 6/7.
No builds or tests were repeated. Full incremental recovery is recorded in
`tmp/racer-stage36-progress.md`.

Final recovery at 13:24 UTC: all three 1,500-node workload fleets Ready/Available,
C6 applied on all 1,500, and all 1,500 positive in the two-minute recovery check
(570.634278 GB/s, 99.239166% success). This is not another acceptance benchmark.
Reconstructed pod membership matched committed membership4936/sequence4937.
Original complete workload/deployment specs and ConfigMap/Prometheus data matched
the pre-stage snapshots exactly. All 1,500 task firewall guards were removed only
after listener absence checks; preexisting INPUT rules were preserved. Final
00007r PID324280/start1637035 and every application/helper thread remained off6/7;
watcher pods were cleaned up, retaining runtime affinity. Controller availability
remains its preexisting one-leader-ready state, not a new 3/3 HA claim.
