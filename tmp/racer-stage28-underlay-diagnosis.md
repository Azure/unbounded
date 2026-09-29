# Stage28: underlay zero-goodput diagnosis

Date: September 29, 2026. Original branch: racer-v2. Investigation worktree based
on deployed dataplane f19e21a7a0e8fa963c596b92a5d696e4e31194a8.

## Result and confidence

**A real transport bottleneck below Racer is established. The exact initiating
cause of the Stage27 eleven-node failure is not yet proven. Do not label this
an application cancellation regression or a proven MTU/flow-limit defect.**

The eleven zero-goodput nodes are exactly the eleven highest cumulative MANA VF
transmit-qdisc drop counts among all 500 ddsv6 nodes. All eleven still show active
queue stops, backlog, and drops in the recovered pod-network configuration.
A read from the existing synthetic origin, bypassing Gantry and Racer entirely,
reproduces very slow bulk TCP on a suspect node. This is a substantially stronger
lead than the old failure-ring deadlines. No application patch is justified yet.

The original 684.634460 GB/s, 98.859972% host-network result remains rejected, not
discarded as unpromising. Restoring reliability remains the path to retaining
underlay. No fleet rollout, configuration change, firewall change, NIC setting,
identity/storage operation, new application image, or acceptance benchmark was
performed in Stage28. No source tests were rerun. No subagent tool was available.

## Exact application failure mechanism that the evidence DOES establish

1. All eleven had zero completed verified **images**, not zero received bytes.
   Fixed-time historical queries show 0.189 to 8.681 GB received per node during
   the window. Manifest/config requests mostly succeeded. Nine nodes also
   completed individual layers; 00005f/0000bl completed no layers.
2. For example, 00000h had extrapolated increases of 60 manifest successes,
   60 config successes, 47.5 layer successes, 60 layer errors, and 180 layer
   cancellations. Its 60 image errors and zero image successes are therefore
   consistent with one failed layer canceling three siblings, not a dead local
   service. Fractional values are Prometheus extrapolation, not literal counts.
3. Deployed loadgen208 `cmd/racer-loadgen/pull.go:156-219` credits verified bytes
   only after the complete image succeeds, and uses errgroup cancellation for
   its four layer workers. Body bytes are counted even on failure at :295-330.
   Do not substitute the worktree's loadgen implementation: f19 has other
   loadgen changes and is NOT the deployed loadgen revision.
4. The f19 receive failure stage is precise: `peer/transfer.rs:283-319` has
   completed checkout, session handshake, request send, and response-head read;
   :362-386 has admitted the ciphertext body before `PeerReceiveBody` can fail.
   Thus those events are not missing UDS, service endpoint lookup, initial DNS,
   handshake failure, or ciphertext admission failure for that same attempt.
5. Candidate scopes are independent of caller cancellation, share remaining
   time among opportunities, and fence canceled I/O before returning
   (`read/candidates.rs:387-453`). Small candidate subdeadlines can expire before
   the 30-second application request, well before the loadgen's two-minute cap.
6. The strongest retained request chain is on 00005f:
   request `86b447f4562010e296493bf603e8e61e`, sequences22830-22839:
   body DeadlineExceeded on workers3/2 at1790679561349ms; final signed
   CandidateResponse Unavailable and CandidateExhausted with attempts0/links0
   at1790679561417ms; FirstSlice Cancelled, sent0/expected55758848, followed by
   cancellation of the remaining peer attempt. 0000b1 request
   `39255cfe37c2f09023923a94844b1156`, sequences20134-20139, similarly progresses
   body deadline -> final Unavailable -> PageAcquire page1 -> FirstSlice
   Unavailable. These are request failures followed by cleanup, not proof that
   cancellation killed permanent serving.
7. Rings cover only approximately 2.7-7.3 seconds per suspect, collected after
   the measured window while C0 was propagating. They do not identify remote
   endpoints, bytes remaining, kernel TCP state, or every failed request.

Source paths in points4-5 are relative to `cmd/racer-dataplane/src` at f19.
Evidence: `tmp/racer-stage28-{historical,requests,artifact-analysis}.json` and
`tmp/racer-stage27-failure-evidence.json`.

## Eleven-node comparison

Stage27 fixed five-minute window. CPU is logical cores, TX/RX is eth0 Gb/s.
Received GB is extrapolated loadgen response-body bytes, including failed pulls.
VF drops are later cumulative ens1 qdisc counts from the 500-node survey, NOT
claimed as Stage27 deltas.

| ddsv6 suffix | TX | RX | host CPU | DP CPU | received GB | VF drops, millions |
|---|---:|---:|---:|---:|---:|---:|
| 00000h | 3.921 | 3.496 | 4.102 | 2.895 | 8.065 | 39.491 |
| 00002i | 4.101 | 3.097 | 3.934 | 2.731 | 6.305 | 40.618 |
| 00003z | 4.109 | 2.531 | 3.356 | 2.278 | 3.984 | 42.179 |
| 00005f | 4.344 | 1.384 | 2.633 | 1.613 | 0.210 | 46.922 |
| 000066 | 4.143 | 2.704 | 3.656 | 2.487 | 3.351 | 41.572 |
| 00006d | 4.054 | 3.425 | 4.054 | 2.833 | 7.599 | 34.229 |
| 000078 | 4.272 | 2.507 | 3.497 | 2.347 | 3.782 | 41.945 |
| 0000b1 | 4.027 | 3.555 | 4.144 | 2.924 | 7.553 | 34.084 |
| 0000bl | 4.442 | 1.326 | 2.565 | 1.496 | 0.189 | 48.663 |
| 0000cp | 4.242 | 2.599 | 3.537 | 2.395 | 3.796 | 46.633 |
| 0000d9 | 4.057 | 3.529 | 4.093 | 2.903 | 8.681 | 46.673 |

Mean eleven: TX4.156/RX2.741, host3.597/DP2.446, Gantry0.0164/loadgen0.0226.
Mean other489 ddsv6: TX9.298/RX9.357, host4.386/DP2.895,
Gantry0.3126/loadgen0.4979, verified0.46964 GB/s per node.
Other pool means: adsv5 TX9.371/RX9.194, host5.289/DP3.118;
ddv5 TX9.330/RX9.423, host4.703/DP2.965.
The zero nodes are not the highest application CPU consumers.

## Network evidence, including a Racer-independent reproduction

- Read-only survey of all500 ddsv6: ens1 drops rank1-11 matches the exact zero
  set, with34.084-48.663 million drops. Rank12 is0000a0 at19.581 million.
  Healthy adjacent control00000i has37 drops. A second control000023 has
  1.776 million, so absolute drops alone are not a binary diagnosis.
- All500 report identical features (TSO/GSO/GRO on; tunnel segmentation off)
  and identical current ring sizes RX1024/TX256, maxima8192/16384.
- In one five-second **post-rollback** queue observation, all eleven accumulated
  154,956-177,759 stop_queue increments and1,303-5,731 qdisc drops, while
  sending1.108-1.313 GB and retaining roughly2-3 MB of qdisc backlog.
  Controls00000i/000023 sent4.183/4.865 GB with2,387/6,936 stops, zero drops,
  and zero ending backlog. 0000a0 is a partially affected positive control:
  2.530 GB,243,962 stops,8,693 drops. These observations do not alter load.
- Existing origin HTTP range GETs, one at a time, capped at16MiB each:
  healthy00000i receiving from suspect00005f:12.305s;
  reverse:1.012s; controls00000i <-000023:0.0347s and reverse0.0242s.
  All returned206 and exactly16,777,216 bytes. No Racer or Gantry code runs in
  this path, and no new listener or pod was created.
- Moving the client into the existing loadgen pod network namespace, with
  namespace-only nsenter, did not remove the effect:00000i <-00005f13.530s,
  reverse3.786s. This checks a different path, not another benchmark window.
- One sender-side socket observation during a slow range transfer showed
  rtt14.166ms, cwnd19, ssthresh18, bytes_retrans15290, total retrans11,
  delivery_rate11,600,704bps, notsent214060, snd_wnd748288, pmtu1442/mss1390.
  The transfer completed8.033s. A prior socket-filter attempt selected the
  NodeIP rather than the actual SNAT source and returned no sockets; it was
  corrected, not treated as evidence of no TCP traffic. Actual source at the
  origin was10.242.252.1, not the receiver NodeIP.
- eth0 NIC error counters do NOT cover VF fq_codel drops. Even historical
  ens1 netdev transmit_drop_total was zero during Stage27. qdisc counters must
  be collected separately. The original 'no NIC errors' observation cannot
  exclude this loss mechanism.
- Small DF probes of1442-byte and1500-byte IPv4 packets succeeded both ways
  between000023 and each suspect. Current clocks are PHC synchronized within
  microseconds. All sampled eth0/ens1 MTUs1500, overlay1442. These probes
  weaken a persistent MTU black hole, but do not prove Stage27 PMTU behavior.
- Current conntrack has thousands of entries, below262144. Historical conntrack,
  TCP retransmission/timeout, and timex Prometheus queries returned no series.
  Missing telemetry is not a zero counter. No Azure fabric flow-limit metric
  was collected; Azure flow exhaustion is neither established nor excluded.
- Queue RSS indirection is balanced0-7, eight channels, irqbalance active,
  cubic, netdev_budget300/usecs2000. Some current IRQ assignments collide,
  but this is not uniform across the eleven. No matching kernel warnings in
  the bounded Stage27 journal interval. No unsupported ethtool coalescing
  setting was changed.

Artifacts: `racer-stage28-{hosts,path-probes,qdisc,vf-survey,vf-summary,
queue-detail,queue-summary,vf-config,vf-history,vf-history-summary,
origin-transport,namespace-transport,socket-trace}.json` under tmp.
Upstream Linux v6.8 mana_en.c was inspected for interpretation only:
mana_start_xmit increments stop_queue when SQ room falls below MAX_TX_WQE_SIZE;
mana_poll_tx_cq wakes after completions restore room. This is not proof that the
running Ubuntu6.8.0-1067-azure driver is byte-identical or has a particular bug.

## Endpoint, origin, membership, and cancellation checks

- Both Services are InternalTrafficPolicy Local. Current EndpointSlices contain
  exactly one ready/serving/nonterminating Gantry endpoint and one origin
  endpoint on every suspect. Gantry and loadgen were NOT hostNetwork in Stage27;
  only DP changed. Their local HTTP/UDS paths did not become NodeIP peer traffic.
  Manifest/config successes in the historical window rule out a permanently
  missing local Gantry endpoint as the all-zero explanation. Current slices
  alone do not prove every historical kube-proxy rule.
- All eleven currently have socket objects at the configured client and origin
  paths. Eight had successful origin206 requests/fills during the old window;
  the other three had zero fills, not evidence of absent origin sockets.
- Controller0c23 `internal/racer/membership.go:108-157` selects an owned,
  nonterminating PodIP, ignores readiness, and appends configured PeerPort.
  d3 `internal/racer/workload.go:240-244` switches hostNetwork and DNS policy;
  it does not replace client/origin UDS. Stage27 reconstructed all1500
  NodeIP:18082 endpoints matched the committed membership hash. The eleven
  exact IPs are retained in `racer-stage28-metrics-detail.json`.
- f19 `peer.rs:43-57` derives the endpoint from the operation's retained
  membership lease; `http/pool.rs:456-508` keys connection buckets by endpoint.
  An IP change selects a different bucket. Old membership leases can finish
  on old endpoints; every DP process was also replaced during the rollout.
  No artifact establishes a stale pool as the persistent eleven-node cause.
  Per-process accepted sequence was not exposed, so no stronger claim is made.
- f19 already includes independent acquisition driver cancellation:
  `read/fill.rs:382-387,459-469`, `read/metadata.rs:512-538`;
  attempts independently cancel at `read/candidates.rs:399-400,436-453`.
  Existing `read/fill_tests.rs:321-405` asserts parent scope remains usable,
  driver queue drains, and a later acquisition succeeds for both abandoned
  metadata and page acquisition. Read, not rerun. A cloned ingress scope still
  shares cancellation (`peer/server.rs:283-288`), but the inspected driver and
  candidate paths do not cancel that parent. Do not infer a new permanent-scope
  defect from FirstSlice Cancelled after CandidateExhausted.
- All twelve retained guard samples have zero rejects, including all eleven
  zeros. This excludes rejection at those local guards only, not remote loss.

## Next repair/validation decision

The precise proven target is the **MANA ens1 transmit-queue bottleneck**, not
Racer request timeout, metadata budgets, or missing service wiring. A fleet
repair is NOT yet justified: the queue problem is proven, its originating
driver/platform cause is not. In particular, identical TX256 on healthy and
bad nodes means 'increase every ring' is not an established repair.

Before the next underlay roll, parent should authorize a single-node network
maintenance canary on00005f, with00000i as control. Preserve current settings,
capture per-queue completion/stop/drop deltas and TCP_INFO on the same bounded
origin transfer, then test only one reversible change. A TX-ring increase to
1024 is a candidate, NOT a proven fix; ethtool ring reconfiguration may rebuild
queues and transiently interrupt networking, so it was not performed here.
If a same-value queue rebuild alone clears the pathology, distinguish reset
effect from capacity effect before encoding a permanent configuration.
Do not flush conntrack, disable the VF, alter IRQ affinity fleet-wide, enlarge
Racer deadlines, or widen the old guard as a speculative remedy.

An approved minimal secured underlay canary must capture both ends of the exact
Racer TCP connection (tuple, retransmissions, cwnd, send/receive queues, body
offset, request/attempt IDs), plus ens1 qdisc/driver deltas **while failing**.
This closes the remaining link between Stage27 candidate expiry and the
independently reproduced queue pathology. If transport is healthy but body
progress stops, instrument f19 sender/relay progress in a separate focused
patch rather than deploying the parent's newer protocol.

Only after that correction passes should parent authorize a fleet underlay
transition and one prospective C6 acceptance window. Retain the existing
success/coverage requirements; neither waive the1% gate nor abandon underlay.

## Exit state

11:30:53UTC read-only audit: all three benchmark DaemonSets1500/1500
Ready/Available/updated,1500 positive two-minute verified rates,1500 loadgen
targets, only applied-concurrency value6. DP remains pod-network f19,
operator d3/controller0c23 unchanged. Origins remain running. No host-network
listener or guard was installed. No test/build repetition. Parent's concurrent
request-path documents were untouched. `racer-stage28-final-audit.json` records
the audit. Investigation complete as bounded evidence collection; exact
Stage27 root-cause attribution and a validated repair remain open.
