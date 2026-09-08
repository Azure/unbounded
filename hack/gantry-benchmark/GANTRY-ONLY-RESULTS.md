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
		<td><strong><code>chair-https-040127</code></strong></td><td align="right">1,000</td>
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

Rows are newest first. **`chair-https-040127`** is the most recent run; its full
identifier is `run-20260908-040127-dfc1f9a6`. It repeats `chair-https-030517` on
the same build: the two agree to within 32 bytes of registry payload. Every run
used fail-open containerd routing, where the registry remains the default server
and containerd can reach it directly if Gantry fails, so ACR traffic can in
principle include pulls that bypassed Gantry. In the two most recent runs it did
not: measured delivery to containerd came entirely from peers and local cache.

### Latency

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Run</th><th>P50</th><th>P95</th><th>P100</th></tr>
	<tr>
		<td><strong><code>chair-https-040127</code></strong></td>
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

## The design

A cold 40 GiB image on 1,000 nodes is 42 TB if every node pulls from the
registry. Gantry's goal is to make registry traffic depend on the size of the
image rather than the size of the cluster.

It does that by having a small number of nodes fetch each layer from the
registry and every other node fetch from a peer:

1. **Peers serve each other.** A node that holds a layer serves it to any node
   that asks. Almost all traffic is peer to peer, and it never touches the
   registry.
2. **A distributed index answers "who has this layer".** Nodes publish the
   layers they hold, keyed by content digest, and look each other up through it.
   No node holds the whole index and no node is a bottleneck.
3. **A seed cohort covers the cold case.** On the very first pull nobody has the
   layer, so the index is empty. A small set of nodes, elected through the
   Kubernetes Lease API, is responsible for fetching from the registry. For any
   given layer, every node independently computes the *same* ranked list of
   which of those nodes should seed it, so they all ask the same few nodes
   without needing to coordinate.

The seed cohort size is the design's central number. It is set to **8**, so the
expected registry traffic for one image is 8 copies, whether the cluster has
1,000 nodes or 100,000.

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th></th><th>Registry traffic for a 40 GiB image</th></tr>
	<tr><td>Every node pulls directly</td><td align="right">42 TB</td></tr>
	<tr><td>Gantry, by design (8 seeds)</td><td align="right">343.6 GB</td></tr>
	<tr><td>Gantry, measured</td><td align="right">343.6 GB</td></tr>
</table>

That floor has been reproduced. Two runs of the same build fetched 328 layer
copies each, 8 per layer, and their registry payloads differ by 32 bytes out of
343.6 GB. Neither fell back to the registry for delivery.

## How we got here

The design was right from the start; reaching its floor took three corrections,
each found by measuring rather than reasoning. Registry traffic is quoted as
copies of each layer, where 8 is the target.

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

Two findings from that process are worth keeping:

**Seed reliability is the same thing as registry cost.** Nothing else moved
registry traffic materially. Because a failed seed request adds a fetcher rather
than replacing one, the failure rate of that one request translates almost
directly into copies pulled from the registry.

**The peer network tended toward a full mesh.** Measured across all 1,000 nodes,
every node had an open connection to very nearly every other node. This was not
caused by the seeding traffic; it came from ordinary index lookups, each of which
opens connections that nothing later reclaims. That behavior does not scale, and
bounding it is the main open question for much larger clusters.

## Reading these numbers

- **ACR traffic** is measured at the registry's private endpoint and counts
  wire bytes, so it runs about 9% above the payload Gantry accounts for. The
  difference is protocol framing, not traffic that bypassed Gantry.
- **Peer traffic** is roughly 42 TB in every run. That is the work the cluster
  would otherwise have asked the registry to do.
- **Latency** is pod startup measured from cluster audit logs. It is dominated
  by writing and unpacking 40 GiB on each node, so it is largely insensitive to
  the registry-traffic improvements above.
- Runs older than `chair-https-030517` are prior measurements carried forward
  unchanged, and their duration definitions were not re-verified, so treat
  cross-run duration differences with care.
