# Stage37: packetization, TCP progress, and candidate deadlines

## Decision

Keep pod-network C6. No application/NIC/fleet settings changed. No deadline increase,
new image, ring/reset/reboot, or loadgen C1 experiment. Investigated exact dataplane
`f19e21a7a0e8fa963c596b92a5d696e4e31194a8`, not the parent branch's protocol.

**Best supported mechanism:** overlay segmentation consumes many more guest TX
submission events per byte; congestion then gives individual flows small,
variable congestion windows despite continuous useful receive progress. Underlay
TSO removes that particular queue pressure, but does not guarantee complete page
delivery within Racer's fixed, recursively divided candidate deadlines. These are
two distinct effects, not one proven fixed platform bandwidth cap.

The actionable application hypothesis is **discard/retry amplification when a
whole-page body exceeds its candidate time share while still making progress**.
The code permits this; the origin trace demonstrates the relevant slow-progress
shape. It is NOT yet proven to explain the underlay failures: their saved rings
contain neither body offsets nor last-progress timestamps. No production code
change or fabricated failing regression is justified by that missing correlation.

## Actual packet-rate relationships

Recomputed from saved raw ethtool counters, not the prior prose. Stage35 baseline
is 20s pod C6; stage36 is approximately 310s underlay C2, not a matched A/B.

| Counter rate, 00005f | Pod C6 | Underlay C2 |
|---|---:|---:|
| MANA hardware TX Gbit/s | 1.92535 | 4.61663 |
| Hardware unicast TX packets/s | 192,878 | 431,797 |
| Sum driver tx_N_packets/s | 192,882 | 431,793 |
| Sum short/long packet-format events/s | 192,881 | 61,894 |
| TSO packets/s | 1.60 | 21,710 |
| TSO bytes / hardware TX bytes | 0.00192% | 94.8543% |
| Queue stops/s | 32,987 | 0.468 |

Across all eleven underlay nodes: hardware TX 4.298-4.617 Gbit/s,
413,419-433,519 unicast packets/s, format events 60,216-75,943/s, TSO byte
fraction 94.65-94.85%, and zero observed qdisc drops. Control00000i's saved pod
baseline is 6.715 Gbit/s, 576,671 packets/s, 576,669 format events/s, no qdisc drops.

This contradicts both a mode-independent 2-Gbit/s byte cap and a mode-independent
approximately 190-kpps **wire-packet** cap. Packet-format events diverge sharply
from tx_N_packets under TSO; treating tx_N_packets as WQEs would erase the key
distinction. A submission/completion-service constraint relieved by TSO is
consistent with the evidence, not a measured immutable WQE cap or proven vendor
defect. The old CQ evidence remains useful: timely NAPI, mostly pending sends
without a ready CQE, not proof of the cause of limited completion supply.

Upstream Linux v6.8 `mana_start_xmit` increments format counters once before the
GSO branch, and its GSO branch increments TSO counters. This is interpretive
context only, not an assertion that upstream matches installed Ubuntu exactly:
<https://github.com/torvalds/linux/blob/v6.8/drivers/net/ethernet/microsoft/mana/mana_en.c>.
The saved counter relationships above stand independently of that interpretation.

Evidence: `tmp/racer-stage37-saved-rates.json`, recomputed from
`racer-stage35-baseline.json` and `racer-stage36-c2-{start,end}-cohort.json`;
prior CQ probe data `racer-stage30-{baseline-trace-v2,reset-trace,readiness}.json`.
Current offload settings remain TSO/GSO/GRO enabled, tunnel segmentation disabled;
no offload was changed. Underlay C2 and overlay C6 differ in offered work too.

## Two bounded existing-origin streams, no Racer in the payload path

September 29, 2026, starts 13:27:58 and 13:29:25 UTC. Exactly two 64-MiB HTTP206
range bodies, one in each direction, no retry. Both returned 67,108,864 bytes and
the same range SHA256 `6691e76e3c3a7519808c3d7c135bf9be290c93b9ff9fcd7402c401555710fad7`.
That checks matching range content, not the full blob digest. Existing origin
PodIP:8080, host-namespace receivers; source SNAT tuples were recorded. No new
listeners/pods, packet payload capture, authentication capture, or tracing hooks.

| Sender -> receiver | 00005f -> 00000i | 00000i -> 00005f |
|---|---:|---:|
| Header seconds | 0.00670 | 0.00779 |
| Complete body/GET seconds | 35.473 | 15.388 |
| Body MB/s | 1.892 | 4.363 |
| Maximum observed read1 gap | 66.47 ms | 13.98 ms |
| Maximum one-second delivered-payload bin | 29.43 Mbit/s | 53.01 Mbit/s |
| Sender cwnd sample range | 9-39 MSS | 9-44 MSS |
| Sender RTT sample range | 8.917-21.086 ms | 6.134-10.450 ms |
| Last sampled retransmitted bytes | 204,330 | 528,200 |

The slow direction crossed each 16-MiB boundary at 10.751, 15.789, 22.296 and
35.473 seconds. Therefore a 10-second whole-page attempt could fail despite
millisecond-scale receive progress. This is an illustration using the origin
path, NOT a replay of a signed Racer page or an underlay performance prediction.

Exact-tuple sender `ss -tinm` samples show persistent notsent data (38,920-236,300
bytes on the bad sender), MSS1390, and advertised snd_wnd rising to536,192 bytes.
The congestion window was only12.5-54.2KiB. Receiver queues were generally empty;
read1 continuously consumed data. In reverse, advertised window reached2,364,928
bytes, receive buffer5,045,017 bytes, and receiver socket drop count reached1.
Both directions show out-of-order traffic and retransmission. Congestion/ACK-path
behavior is supported; receiver zero-window or application refusal to read is not.
The bad node participates in ACK egress even when it is the receiver. Reverse
slowness therefore does not prove a separate receive-bandwidth cap. These samples
do not timestamp individual ACKs or establish whether ACK loss, data loss, or
reordering dominates; no packet-header pcap was collected.

Both hosts and dataplane/origin namespaces have tcp_rmem4096/131072/6291456,
tcp_wmem4096/16384/4194304, window_scaling1, moderate_rcvbuf1, cubic,
tcp_limit_output_bytes1048576, tcp_notsent_lowat4294967295. Host rmem_max1048576
and wmem_max212992 match. Do not mistake those explicit socket-option caps for
the autotuning maxima: the observed receive buffer already exceeds rmem_max.
The populated send queues and larger advertised windows do not support a blind
sysctl/window increase. No such change was made.

Each direction had parallel85s observations on both nodes: `ss` about every0.4s,
MANA per-queue bytes/packets and IRQ34-41 per-CPU timestamps about every10 samples,
qdiscs and softnet snapshots. All eight TX queues progressed. Bad-node aggregate
TX remained1.888-1.900Gbit/s, stops32,223-32,497/s and59,570/59,761 ens1 drops;
control6.638-6.645Gbit/s and zero drops. These are whole-node observations, not
the transfer's throughput. The diagnostic streams are small shares of that load.
First-window bad-node TX queue rates were23,162-23,697 packets/s across all eight;
control63,044-86,670. Data-vector IRQ totals were92,624/s versus114,348/s, with
per-CPU deltas retained. IRQs include receive work: they are not TX completion
counts and do not prove a notification-rate cap.
Maximum sampled Racer TCP delivery estimates were213Mbit/s bad versus10.26Gbit/s
control, but such instantaneous estimates are not sustained per-flow limits.

Evidence: `tmp/racer-stage37-{streams,analysis,socket-summary,settings}.json`.
Raw streams retain every positive read's cumulative byte count and monotonic time;
analysis retains exact-tuple socket samples, per-queue deltas and interrupt deltas.
Previous stage32 single/three-flow results (13.876s versus13.732/13.723/13.749s
per16MiB) support competing flow shares, not too few connections. No extra
parallel-stream sweep was repeated.

## Exact f19 deadline and topology contract

Paths below are under `cmd/racer-dataplane/src`, at the exact deployed revision.

1. `read/candidates.rs:394-400` divides remaining acquisition time by remaining
   opportunities, including local origin after predecessor probes. That deadline
   is used to seal credentials and sign route state (`:416-434`). A fresh30s
   noncandidate request with three opportunities gets at most approximately10s
   for the first complete exchange, including routing/checkout/head/body.
2. Each forwarded relay retains that deadline, not a fresh share
   (`peer/relay.rs:100-114`). Normal route ceiling4 and failure ceiling8 are
   separate from acquisition credits (`topology/paths.rs:44-45`). A destination
   candidate's predecessor **acquisition** can divide inherited time again;
   pure transit does not. `read/serve.rs:54-69` inherits signed attempts, remaining
   links minus final incoming link, and the tighter deadline.
3. `peer/transfer.rs:378-393` repeatedly reads the admitted body with the same
   scope; positive bytes update only offset. `http/io.rs:539-589` checks that scope
   and submits receive with it. No per-chunk deadline renewal exists. The candidate
   polls cancellation and drains accepted work before fallback
   (`read/candidates.rs:436-455`), then rejects late success (`:487-494`).
4. Request scope can only narrow route/credential deadlines (`peer.rs:61-81`);
   signed forwarding rejects a deadline increase (`topology/paths.rs:64-78`).
   A requester-only "extend on progress" edit cannot make remote relays honor a
   longer already-signed deadline. That is not a safe one-line fix.
5. Client delivery is different: later distinct pages get new fixed child
   allowances (8 attempts/16 links, `read/range_stream.rs:31-59`); already admitted
   pages do not renew. Observed client writes use progress-based delivery
   (`client/response.rs:179-219`). Do not substitute the claim that every entire
   layer has one absolute30s deadline. The default request/page timeout is30s
   (`config.rs:207`), not a per-chunk timeout.

Tests explicitly assert share expiry, fencing, late-success discard and conserved
credits (`read/candidate_timeout_tests.rs:199-242`), origin opportunity after
stalled predecessors (`:244-283`), and original deadline exhaustion (`:341-367`).
They model stalled metadata exchanges, not a continuously progressing page body.
Prose agrees with this fixed-share policy (`read/INTEGRATION.md:66-71`) and
separates client progress from admitted-page deadlines (`client/INTEGRATION.md:
107-113`). No contradictory parent protocol was substituted. No tests/builds
were run for this evidence-only task.

## Why local C1 is not a peer-serving fix

Deployed loadgen208 has one scalar integer file (`cmd/racer-loadgen/concurrency.go:
25-61`), parks local pull worker slots (`:106-143`), and polls that file (`:146-194`).
There is no existing exact-node limit map. Local C controls neither inbound Racer
peer admission nor topology relay demand (`peer/relay.rs:76-85`). Even C0 keeps
origins serving (`208:cmd/racer-loadgen/pull.go:102-135`). C1 might reduce local
receive/ACK and acquisition contention; it cannot guarantee reduced independent
remote page/relay work. No C1 sufficiency claim follows from C2 failure.

Stage36 00005f actually completed extrapolated17.5 successful layers while having
zero complete images,21.25 layer errors and60 canceled layers. These fractional
counts are Prometheus extrapolation, not literal requests. Its final128-event ring
contains61 PeerReceiveBody deadlines,55 PeerRelay deadlines and7 CandidateExchange
deadlines. Events overlap request chains and are not unique failure counts. This
is not a dead receiver: all-or-nothing image credit plus sibling cancellation
amplifies individual page/layer failures (`208:pull.go:156-219`).
Evidence: `tmp/racer-stage37-{ring-analysis,bad-node-detail}.json`.

## Minimal next discriminating experiment

Do not lower local C or increase timeouts as the presumed fix. On an already
authorized future underlay canary, correlate one failed signed page at both ends:
request/attempt ID, route endpoint, page bytes expected/received, original and
candidate deadlines, first/last positive body time, and sender/receiver TCP tuple.
Use bounded high-level telemetry without credential/body capture. Capture whether
expiration has (a) ongoing application receive progress, (b) no TCP payload despite
pending sender work, or (c) TCP receive backlog while the application is not reading.
The existing failure ring's Detail::None cannot distinguish them.

If (a) is confirmed, the smallest policy experiment is **separate no-progress
fallback from a fixed original page hard deadline**, with a signed hard ceiling
established before send, rather than renewing an already-signed scope. Before
any rollout, add a failing real-body regression at exact f19: valid signed page,
regular positive body completions, total duration just beyond the first share
but before the original page deadline. Assert one validated completed page rather
than discard/reacquisition. Pair with stalled-first fallback, parent cancellation,
hard-deadline trickle termination, fenced cleanup and unchanged credit/hop limits.
The current stalled-metadata test is not that regression. No policy change is
implemented until this missing page-level evidence justifies it.

If (b), prioritize per-flow data/ACK timing and submission-event service, not
another ring resize/reset. If (c), investigate receiver/relay scheduling and bounded
admission. Stage36's high TSO/zero-qdisc-drop result makes these discriminators
more useful than a generic vendor escalation or a blind timeout increase.

## Preservation

Final13:33:37UTC: all three1500-node fleets Ready/Available; C6 on1500, all1500
positive in the2m check. DS/deployment specs and control data unchanged. Target/
control pod UIDs and restart counts unchanged. 00007r dataplane PID324280,
start1637035, all dataplane and io_uring helper threads singleton off CPUs6/7;
loadgen affinity was not part of that quarantine and was not changed.
No diagnostic process persists, no host files/settings written, no packet auth
payload retained. Initial sysctl collection failed on host-only core keys inside
the pod namespace; corrected once without a traffic retry. No subagent tool was
available. Read repository AGENTS.md and ~/design.md; ~/designAGENTS was absent.

Evidence: `tmp/racer-stage37-{before-health,final-health,preservation,quarantine}.json`.
Collector retained under `tmp/racer-stage37-diagnose.py`; all commands used bounded
TERM with kill-after10s. One documentation commit; no application code change.
