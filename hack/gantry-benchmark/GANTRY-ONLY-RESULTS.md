# Gantry Benchmark Results

A 1,000 node AKS cluster, one pod per node, all starting the same cold 40 GiB
image at once. Registry traffic is measured at the ACR Private Endpoint, peer
traffic and origin traffic from Gantry metrics, pod startup latency from AKS
audit logs.

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
		<td><strong><code>chair-https-112507</code></strong></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">871.4 s</td>
		<td align="right">382.2 GB</td><td align="right">352.2 GB</td>
		<td align="right">30.0 GB</td><td align="right">42.7 TB</td>
	</tr>
	<tr>
		<td><code>chair-https-040127</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">823.5 s</td>
		<td align="right">376.3 GB</td><td align="right">343.6 GB</td>
		<td align="right">32.6 GB</td><td align="right">42.6 TB</td>
	</tr>
	<tr>
		<td><code>chair-https-030517</code></td><td align="right">1,000</td>
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

Newest first. `chair-https-112507` = `run-20260908-112507-1fa73f1d`; it is the
third run of one build, after `040127` and `030517`. All runs use fail-open
containerd routing, so ACR traffic could in principle include pulls that
bypassed Gantry. Measured, it does not: see Delivery.

### Latency

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Run</th><th>P50</th><th>P95</th><th>P100</th></tr>
	<tr>
		<td><strong><code>chair-https-112507</code></strong></td>
		<td align="right">633.0 s</td><td align="right">714.8 s</td><td align="right">851.9 s</td>
	</tr>
	<tr>
		<td><code>chair-https-040127</code></td>
		<td align="right">635.0 s</td><td align="right">711.2 s</td><td align="right">796.2 s</td>
	</tr>
	<tr>
		<td><code>chair-https-030517</code></td>
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

### Delivery

Bytes containerd received, `chair-https-112507`, all 1,000 pods.

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Source</th><th>Bytes</th><th>Share</th></tr>
	<tr><td>Peer</td><td align="right">42,655,995,158,946</td><td align="right">99.31%</td></tr>
	<tr><td>Local cache</td><td align="right">297,452,470,450</td><td align="right">0.69%</td></tr>
	<tr><td>Origin</td><td align="right">0</td><td align="right">0.00%</td></tr>
	<tr><td>Total served</td><td align="right">42,953,447,629,396</td><td align="right"></td></tr>
	<tr><td>Required (1,000 x 40 GiB)</td><td align="right">42,949,672,960,000</td><td align="right">100.0088%</td></tr>
</table>

Origin-sourced serves 0, NF5 origin fallbacks 0. ACR traffic 382.2 GB against
352.2 GB of Gantry origin body bytes is a ratio of 1.0852; the prior two runs
measured 1.0912 and 1.0950. That gap is wire framing, not bypassed traffic.

### Log summary

3,038,521 records from 1,000 pods, `chair-https-112507`.

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Class</th><th>Count</th><th>Detail</th></tr>
	<tr><td>ERROR / FATAL / WARN</td><td align="right">0</td><td></td></tr>
	<tr><td>Peer fetch failed</td><td align="right">2,042,270</td><td>1,994,898 are HTTP 429 peer-busy backpressure; requester retries another provider</td></tr>
	<tr><td>Advertise provide failed</td><td align="right">767,387</td><td>all "no peer in table", all 11:20-11:24Z during rollout, none during the 11:41Z phase</td></tr>
	<tr><td>Chair call failed</td><td align="right">49,789</td><td>90% no-route; 49,200 / 586 / 3 across 11:41 / 11:42 / 11:43Z</td></tr>
	<tr><td>Cold-start exhausted</td><td align="right">1,123</td><td></td></tr>
	<tr><td>Origin fallback events</td><td align="right">0</td><td></td></tr>
</table>

`no route to host` appears on both data planes in the burst window: 45,352 on
the unchanged peer transfer port and 44,816 on the chair port. It is a
cluster-wide condition during the connection burst, not a property of either
transport.

## The design

Registry traffic scales with the image, not the cluster.

1. Peers serve each other; almost all traffic never reaches the registry.
2. A distributed index, keyed by content digest, answers "who has this layer".
3. On a cold pull the index is empty, so a seed cohort elected through the
   Kubernetes Lease API fetches from the registry. Every node computes the same
   ranked cohort per layer independently, so no coordination is needed.

Cohort size is 8, so expected registry traffic is 8 copies of the image at any
cluster size.

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th></th><th>Registry traffic, 40 GiB image</th></tr>
	<tr><td>Every node pulls directly</td><td align="right">42 TB</td></tr>
	<tr><td>Gantry, by design (8 seeds)</td><td align="right">343.6 GB</td></tr>
	<tr><td>Gantry, measured (3 runs)</td><td align="right">343.6 / 343.6 / 352.2 GB</td></tr>
	<tr><td>Copies per layer (3 runs)</td><td align="right">8.00 / 8.00 / 8.20</td></tr>
</table>

## How we got here

Three corrections took measured traffic from 39 copies per layer to 8.

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Copies per layer</th><th>What was wrong</th></tr>
	<tr>
		<td align="right"><strong>39.0</strong></td>
		<td>When a seed node did not answer in time, the system recruited a
		replacement. But the unresponsive node had usually already started
		fetching, so a failure <em>added</em> a fetcher instead of substituting
		one. Every timeout cost another copy from the registry.</td>
	</tr>
	<tr>
		<td align="right"><strong>13.4</strong></td>
		<td>Seed nodes were failing to answer about a third of the time. The
		cause was not load: the peer network was closing idle connections to
		stay under a connection limit sized for a public network rather than a
		datacenter cluster, and it could not tell an idle connection from one
		about to be reused.</td>
	</tr>
	<tr>
		<td align="right"><strong>8.7</strong></td>
		<td>Raising that limit fixed the symptom but only because the new limit
		was larger than the cluster. Seed coordination was moved onto its own
		channel, independent of the peer network's connection budget, so the
		limit could be set from the work a node actually does rather than from
		the number of nodes.</td>
	</tr>
	<tr>
		<td align="right"><strong>8.0</strong></td>
		<td>Design floor reached, and reproduced on a second run.</td>
	</tr>
</table>

Two findings from that work:

- Seed reliability is registry cost. A failed seed request adds a fetcher rather
  than replacing one, so its failure rate translates almost directly into copies
  pulled from the registry.
- The peer network tends toward a full mesh. Measured across 1,000 nodes, every
  node held an open connection to very nearly every other node, driven by index
  lookups rather than seeding. Bounding that is the open question for larger
  clusters.

## Notes

- ACR traffic is wire bytes at the Private Endpoint and runs about 9% above the
  payload Gantry accounts for.
- Latency is dominated by writing and unpacking 40 GiB per node, so it is
  largely insensitive to registry-traffic changes.
- Runs older than `chair-https-030517` are carried forward unchanged and their
  duration definitions were not re-verified.
