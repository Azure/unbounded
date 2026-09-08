# Gantry Benchmark Results

Gantry-only runs measure what a 1,000 node AKS cluster pulls from Azure
Container Registry when every node starts the same cold 40 GiB image at once.
Each run generates a fresh random image, pushes it to a private registry reached
over a Private Endpoint, and runs a Kubernetes Job with exactly one pod per node.
Registry traffic is measured at the Private Endpoint, peer traffic from Gantry
metrics, and pod startup latency from AKS audit logs.

### Image

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Size</th><th>Layers</th></tr>
	<tr><td align="right">40 GiB</td><td align="right">40</td></tr>
</table>

### Traffic

<table border="1" cellspacing="0" cellpadding="6">
	<tr>
		<th>Run</th><th>Nodes</th><th>Completed</th><th>Duration</th>
		<th>ACR traffic</th><th>Gantry origin traffic</th><th>ACR minus Gantry origin</th><th>Peer traffic</th>
	</tr>
	<tr>
		<td><strong><code>chair-https-030517</code></strong></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">785.6 s</td>
		<td align="right">375.0 GB</td><td align="right">343.6 GB</td>
		<td align="right">31.3 GB</td><td align="right">42.6 TB</td>
	</tr>
	<tr>
		<td><code>fixed-runtime-155811</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">851.9 s</td>
		<td align="right">1.4 TB</td><td align="right">1.3 TB</td>
		<td align="right">119.0 GB</td><td align="right">42.0 TB</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-232309</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">761.2 s</td>
		<td align="right">601.0 GB</td><td align="right">552.0 GB</td>
		<td align="right">49.0 GB</td><td align="right">42.4 TB</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-165929</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">1,166.0 s</td>
		<td align="right">1.0 TB</td><td align="right">494.0 GB</td>
		<td align="right">555.2 GB</td><td align="right">42.0 TB</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-151506</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">927.1 s</td>
		<td align="right">1.7 TB</td><td align="right">525.1 GB</td>
		<td align="right">1.2 TB</td><td align="right">41.3 TB</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-142520</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">798.5 s</td>
		<td align="right">729.1 GB</td><td align="right">513.3 GB</td>
		<td align="right">215.8 GB</td><td align="right">42.1 TB</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-133121</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">827.4 s</td>
		<td align="right">561.3 GB</td><td align="right">486.5 GB</td>
		<td align="right">74.9 GB</td><td align="right">42.1 TB</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-053922</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">911.0 s</td>
		<td align="right">1.4 TB</td><td align="right">497.2 GB</td>
		<td align="right">925.1 GB</td><td align="right">41.6 TB</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-045041</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">933.7 s</td>
		<td align="right">1.0 TB</td><td align="right">597.1 GB</td>
		<td align="right">446.3 GB</td><td align="right">42.0 TB</td>
	</tr>
</table>

Rows are newest first. **`chair-https-030517`** is the most recent run and the
one analyzed in detail below; its full identifier is
`run-20260908-030517-cb337b67`. Every run used fail-open containerd routing,
where the registry remains the default server and containerd can reach it
directly if Gantry fails. ACR traffic under fail-open can therefore include
direct pulls that bypass Gantry, so the accounting below tests for exactly that.

### Latency

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Run</th><th>P50</th><th>P95</th><th>P100</th></tr>
	<tr>
		<td><strong><code>chair-https-030517</code></strong></td>
		<td align="right">640.3 s</td><td align="right">712.0 s</td><td align="right">763.7 s</td>
	</tr>
	<tr>
		<td><code>fixed-runtime-155811</code></td>
		<td align="right">671.0 s</td><td align="right">741.4 s</td><td align="right">831.2 s</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-232309</code></td>
		<td align="right">619.2 s</td><td align="right">703.1 s</td><td align="right">735.5 s</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-165929</code></td>
		<td align="right">635.6 s</td><td align="right">709.5 s</td><td align="right">1,140.1 s</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-151506</code></td>
		<td align="right">637.7 s</td><td align="right">713.2 s</td><td align="right">902.7 s</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-142520</code></td>
		<td align="right">629.0 s</td><td align="right">699.5 s</td><td align="right">770.3 s</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-133121</code></td>
		<td align="right">628.8 s</td><td align="right">704.8 s</td><td align="right">800.3 s</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-053922</code></td>
		<td align="right">635.1 s</td><td align="right">706.1 s</td><td align="right">886.3 s</td>
	</tr>
	<tr>
		<td><code>lease-rendezvous-045041</code></td>
		<td align="right">637.9 s</td><td align="right">708.9 s</td><td align="right">909.9 s</td>
	</tr>
</table>

## Measured accounting, `chair-https-030517`

Full run identifier `run-20260908-030517-cb337b67`. Gantry image
`sha256:5401cfcf924f3de29ddcf2849d3304e506ebe62b2c99f11475f1ec931765cd99`,
workload image
`sha256:f1178c220af2487884c4343c2ff627f168d5d4d21652a515582ef2bd7595e3fa`.
All 1,000 pods completed, none failed; the harness exited 0.

### Origin copies per layer

The design seeds each layer from `chairs.SeedCount` = 8 agents, so the floor for
a 40 GiB / 40 layer image is 8 x 40 GiB = 343.6 GB of registry traffic. This run
reached that floor:

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Quantity</th><th>Value</th></tr>
	<tr><td>Gantry origin bytes</td><td align="right">343,627,582,304</td></tr>
	<tr><td>Bytes per 1 GiB layer copy</td><td align="right">1,073,741,824</td></tr>
	<tr><td>Layer copies</td><td align="right">320.0</td></tr>
	<tr><td><strong>Copies per layer</strong></td><td align="right"><strong>8.0007</strong></td></tr>
	<tr><td>Successful origin layer pulls</td><td align="right">328 (= 8 x 41 children)</td></tr>
	<tr><td>Origin fallbacks (NF5)</td><td align="right">0</td></tr>
</table>

For comparison, the same measurement on earlier runs: 39.00 copies per layer
before the cold-start seed cohort was fixed, 13.39 after that fix alone, and
8.72 once chair calls stopped failing. The remaining gap closed when the chair
RPC moved off libp2p.

### Registry traffic

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Component</th><th>Bytes</th></tr>
	<tr><td>Private Endpoint <code>PEBytesIn</code></td><td align="right">374,965,644,547</td></tr>
	<tr><td>Gantry origin body bytes</td><td align="right">343,627,582,304</td></tr>
	<tr><td>Difference</td><td align="right">31,338,062,243</td></tr>
	<tr><td>Ratio</td><td align="right">1.0912</td></tr>
	<tr><td>ACR pull events</td><td align="right">8</td></tr>
</table>

The two counters measure different things: Gantry counts decoded HTTP body
bytes for fetches it performs, `PEBytesIn` counts wire bytes for all endpoint
traffic. The 1.0912 ratio matches the 1.0913 measured on `fixed-runtime-155811`,
so the difference is framing overhead rather than traffic that bypassed Gantry.
Do not carry this ratio to a different endpoint; it is per-endpoint.

### Delivery to containerd

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Source</th><th>Bytes</th></tr>
	<tr><td>Peer traffic</td><td align="right">42,620,558,167,817</td></tr>
	<tr><td>Peer fetch hits</td><td align="right">41,679</td></tr>
	<tr><td>Origin fallbacks</td><td align="right">0</td></tr>
</table>

### What changed in this run

The cold-start `please_pull` RPC moved from a libp2p stream to a dedicated
HTTPS listener on port 5002, authenticated by pinning the TLS certificate to the
chair's libp2p peer ID as published in its Lease. That decouples chair calls
from the libp2p connection budget, which in turn allowed the connection-manager
watermarks to drop from 8192/6144 to 900/600.

The watermark matters because DHT lookups open a connection per peer walked and
nothing reclaims them, so the swarm tends toward a full mesh. Measured directly
across all 1,000 pods, open libp2p connections fell from a p50 of 999 (a
complete mesh, every agent connected to every other) to a p50 of 841 with the
smaller watermark, while chairs contacted per resolve stayed at exactly 8.00,
meaning no resolve had to walk past its first-choice cohort. Chair pods and
non-chair pods held statistically identical connection counts (1,000.4 vs
998.1), confirming the mesh is built by DHT traffic rather than chair fanout;
with only 64 chairs in the fleet, at most 6.4% of connections can be
chair-attributable.

### Open item

29.7% of chair calls failed, dominated by `dial tcp <ip>:5002: connect: no route
to host`. These were confined to the first minute of the phase, which began at
03:20:07 Z: 1,535 of 1,582 sampled failures fall in minute 03:20, after which
the counters stopped moving entirely. All 64 chairs were affected roughly
evenly, no Gantry pod restarted, and every chair Lease address resolved to a
live pod, so a specific unhealthy chair and stale Lease addresses are both
excluded. The cause is not established. One untested possibility is that the
libp2p transport previously masked the same underlying condition, since it can
reach a peer through the DHT and peerstore when a recorded address does not
work, whereas the HTTPS client dials `Holder.TransferAddr` with no fallback.

The failures did not affect delivery: chairs contacted per resolve stayed at
8.00, copies per layer reached the design floor, and origin fallbacks were zero.

## Measured accounting, `fixed-runtime-155811`

Full run identifier `run-20260904-155811-ef5dd034`. Gantry image
`sha256:a40d88d05691de2500619868fa85012d914ece6548d300ba45b9a61c516450a6`,
workload image
`sha256:25f46aaa060219b0d3f758f8936d144f082782aacf328607f32d66e2164f0093`.
All 1,000 pods completed, none failed.

### Registry traffic

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Component</th><th>Bytes</th></tr>
	<tr><td>Observed <code>PEBytesIn</code>, wire</td><td align="right">1,422,029,922,123</td></tr>
	<tr><td>Gantry origin counter, decoded body</td><td align="right">1,303,056,256,847</td></tr>
	<tr><td>Difference</td><td align="right">118,973,665,276</td></tr>
	<tr><td>Ratio</td><td align="right">1.0913</td></tr>
</table>

Without Gantry, 1,000 nodes pulling this image directly would transfer at least
42,949,672,960,000 B. Measured registry traffic was 1,422,029,922,123 B, a
**96.69% reduction**. That comparison uses payload bytes for the counterfactual
and wire bytes for the measurement, so it understates the true reduction.

### Delivery to containerd

`gantry_mirror_bytes_served_total` counts bytes the mirror wrote to the local
containerd client, labeled by the path that supplied them.

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Source</th><th>Layer bytes</th></tr>
	<tr><td>peer</td><td align="right">41,995,578,797,957</td></tr>
	<tr><td>cache</td><td align="right">957,861,385,340</td></tr>
	<tr><td>origin</td><td align="right">0</td></tr>
	<tr><td>Total served</td><td align="right">42,953,440,183,297</td></tr>
	<tr><td>Required, 1,000 x 42,949,672,960</td><td align="right">42,949,672,960,000</td></tr>
</table>

Gantry delivered 100.0088% of the layer bytes the workload required, so no layer
payload reached containerd by any other transport. **Direct-to-registry payload
leakage in this run is 0 bytes.**

The manifest path closes the same way. Gantry fetched the manifest from the
registry on exactly 8 pods, 7,476 B each for 59,808 B total, matching the 8
registry pull events recorded for the window. The mirror then served the manifest
over HTTP to 992 pods, 7,416,192 B total, which is exactly 992 x 7,476 B. The
remaining 8 pods are those same registry fetchers: Gantry writes into
containerd's own content store, so on those nodes the manifest was already local
and containerd never issued an HTTP request for it. Those 8 pods still received a
complete image through Gantry. `gantry-kkggb`, for example, was served
13,959,863,237 B from cache plus 28,993,577,081 B from peers, totaling
42,953,440,318 B.

Layer seeding was not confined to those 8 pods. 64 of the 1,000 pods recorded
non-zero `gantry_origin_bytes_total{kind="layer"}`.

The 118,973,665,276 B difference between `PEBytesIn` and Gantry's origin counter
is overhead on the same connections rather than a separate transfer. Per-minute
readings put 1,421,562,675,101 B, or 99.97% of all registry traffic, inside
16:15 to 16:22, which is exactly the interval in which Gantry's origin counter
advanced. Once that counter stops, the endpoint records only 467,247,022 B more,
trailing to zero by 16:26. The two measurements count different things:
`PEBytesIn` counts wire bytes arriving at the endpoint, including TCP and IP
framing, TLS records, any retransmission, and bytes an aborted fetch left unread,
while the Gantry counter counts decoded response-body bytes it actually read.

The internal composition of that 118,973,665,276 B was not measured. The 1.0913
ratio is specific to this endpoint and run, and no calibration constant from any
other endpoint is used anywhere in this document. What is established is the
destination of the payload, not the breakdown of the overhead: containerd
received no layer bytes from outside Gantry. The retained phase evidence
separately recorded 0 `timeout awaiting response headers` messages, 0 containerd
pull-cancel events, and 0 `p2p_origin_fallback_total`.

### Sources

Prometheus figures are instant queries evaluated at `2026-09-04T16:33:00Z`, the
close of the phase telemetry window, with selector
`{namespace="gantry-system",gantry_benchmark="true"}`. All counters read 0 at the
`2026-09-04T16:15:00Z` window open, so the end value is the phase delta. Artifact
figures are fields of `gantry-cold.json` for this run.

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Figure</th><th>Source</th></tr>
	<tr><td>ACR traffic 1,422,029,922,123 B</td><td>Azure Monitor <code>PEBytesIn</code>, Total, PT1M, 18 points, private endpoint <code>vapa-gantry-branch-benchmark-gantry-acr-pe</code>; independently equals <code>azure.private_endpoint.bytes_from_acr</code></td></tr>
	<tr><td>Per-minute 99.97% inside 16:15 to 16:22, 467,247,022 B after</td><td>Same <code>PEBytesIn</code> series read per bucket, against <code>sum(gantry_origin_bytes_total)</code> as a 60 s <code>query_range</code> over the window</td></tr>
	<tr><td>Gantry origin 1,303,056,256,847 B</td><td><code>sum(gantry_origin_bytes_total)</code>; independently equals <code>gantry.origin_bytes</code></td></tr>
	<tr><td>Origin split 1,303,056,197,039 layer / 59,808 manifest / 0 config</td><td><code>sum by (kind) (gantry_origin_bytes_total)</code></td></tr>
	<tr><td>Served by source, layer</td><td><code>sum by (source) (gantry_mirror_bytes_served_total{kind="layer"})</code></td></tr>
	<tr><td>Served all kinds 42,953,447,599,489 B</td><td><code>sum(gantry_mirror_bytes_served_total)</code></td></tr>
	<tr><td>Required 42,949,672,960,000 B</td><td>1,000 nodes x <code>image_size_mib</code> 40960 MiB from benchmark state</td></tr>
	<tr><td>64 pods with origin layer bytes, 8 with origin manifest bytes</td><td><code>count(count by (pod) (gantry_origin_bytes_total{kind=...} > 0))</code></td></tr>
	<tr><td>992 pods with all 40 layers completed</td><td><code>count(count by (pod) (gantry_layer_download_completed_timestamp_seconds > 0) == 40)</code></td></tr>
	<tr><td>1,000 pods in fleet</td><td><code>count(count by (pod) (up{job="gantry-benchmark/gantry-benchmark-agent"}))</code></td></tr>
	<tr><td>0 origin fallbacks</td><td><code>sum(p2p_origin_fallback_total)</code>; independently equals <code>gantry.origin_fallbacks</code></td></tr>
	<tr><td>8 registry pull events</td><td><code>azure.acr.successful_pull_count</code>, source <code>ContainerRegistryRepositoryEvents</code></td></tr>
	<tr><td>Peer traffic 41,995,585,932,722 B</td><td><code>gantry_peer.total</code>, source <code>gantry_peer_serve_bytes_total</code></td></tr>
	<tr><td>P50 670.964 s, P95 741.364 s, P100 831.249 s</td><td><code>azure.audit.pod_startup_latency</code>, source <code>AKSAuditAdmin</code></td></tr>
	<tr><td>Duration 851.9 s</td><td><code>job.phase_finished_at</code> minus <code>job.phase_started_at</code></td></tr>
	<tr><td>1,000/1,000 completed</td><td><code>job.pods</code> length; Job reported 0 failed</td></tr>
</table>

Rows other than `fixed-runtime-155811` are prior recorded measurements carried
forward unchanged. Their duration definitions were not re-verified against this
run's, so treat cross-run duration differences with care.
