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
		<td><strong><code>chair-design-112507</code></strong></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">871.4 s</td>
		<td align="right">382.2 GB</td><td align="right">352.2 GB</td>
		<td align="right">30.0 GB</td><td align="right">42.7 TB</td>
	</tr>
	<tr>
		<td><code>chair-design-040127</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">823.5 s</td>
		<td align="right">376.3 GB</td><td align="right">343.6 GB</td>
		<td align="right">32.6 GB</td><td align="right">42.6 TB</td>
	</tr>
	<tr>
		<td><code>chair-design-030517</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">785.6 s</td>
		<td align="right">375.0 GB</td><td align="right">343.6 GB</td>
		<td align="right">31.3 GB</td><td align="right">42.6 TB</td>
	</tr>
	<tr>
		<td><code>current-gantry-155811</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">851.9 s</td>
		<td align="right">1.4 TB</td><td align="right">1.3 TB</td>
		<td align="right">119.0 GB</td><td align="right">42.0 TB</td>
	</tr>
	<tr>
		<td><code>current-gantry-232309</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">761.2 s</td>
		<td align="right">601.0 GB</td><td align="right">552.0 GB</td>
		<td align="right">49.0 GB</td><td align="right">42.4 TB</td>
	</tr>
	<tr>
		<td><code>current-gantry-165929</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">1,166.0 s</td>
		<td align="right">1.0 TB</td><td align="right">494.0 GB</td>
		<td align="right">555.2 GB</td><td align="right">42.0 TB</td>
	</tr>
	<tr>
		<td><code>current-gantry-151506</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">927.1 s</td>
		<td align="right">1.7 TB</td><td align="right">525.1 GB</td>
		<td align="right">1.2 TB</td><td align="right">41.3 TB</td>
	</tr>
	<tr>
		<td><code>current-gantry-142520</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">798.5 s</td>
		<td align="right">729.1 GB</td><td align="right">513.3 GB</td>
		<td align="right">215.8 GB</td><td align="right">42.1 TB</td>
	</tr>
	<tr>
		<td><code>current-gantry-133121</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">827.4 s</td>
		<td align="right">561.3 GB</td><td align="right">486.5 GB</td>
		<td align="right">74.9 GB</td><td align="right">42.1 TB</td>
	</tr>
	<tr>
		<td><code>current-gantry-053922</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">911.0 s</td>
		<td align="right">1.4 TB</td><td align="right">497.2 GB</td>
		<td align="right">925.1 GB</td><td align="right">41.6 TB</td>
	</tr>
	<tr>
		<td><code>current-gantry-045041</code></td><td align="right">1,000</td>
		<td align="right">1,000/1,000</td><td align="right">933.7 s</td>
		<td align="right">1.0 TB</td><td align="right">597.1 GB</td>
		<td align="right">446.3 GB</td><td align="right">42.0 TB</td>
	</tr>
</table>


### Latency

<table border="1" cellspacing="0" cellpadding="6">
	<tr><th>Run</th><th>P50</th><th>P95</th><th>P100</th></tr>
	<tr>
		<td><strong><code>chair-design-112507</code></strong></td>
		<td align="right">633.0 s</td><td align="right">714.8 s</td><td align="right">851.9 s</td>
	</tr>
	<tr>
		<td><code>chair-design-040127</code></td>
		<td align="right">635.0 s</td><td align="right">711.2 s</td><td align="right">796.2 s</td>
	</tr>
	<tr>
		<td><code>chair-design-030517</code></td>
		<td align="right">640.3 s</td><td align="right">712.0 s</td><td align="right">763.7 s</td>
	</tr>
	<tr>
		<td><code>current-gantry-155811</code></td>
		<td align="right">671.0 s</td><td align="right">741.4 s</td><td align="right">831.2 s</td>
	</tr>
	<tr>
		<td><code>current-gantry-232309</code></td>
		<td align="right">619.2 s</td><td align="right">703.1 s</td><td align="right">735.5 s</td>
	</tr>
	<tr>
		<td><code>current-gantry-165929</code></td>
		<td align="right">635.6 s</td><td align="right">709.5 s</td><td align="right">1,140.1 s</td>
	</tr>
	<tr>
		<td><code>current-gantry-151506</code></td>
		<td align="right">637.7 s</td><td align="right">713.2 s</td><td align="right">902.7 s</td>
	</tr>
	<tr>
		<td><code>current-gantry-142520</code></td>
		<td align="right">629.0 s</td><td align="right">699.5 s</td><td align="right">770.3 s</td>
	</tr>
	<tr>
		<td><code>current-gantry-133121</code></td>
		<td align="right">628.8 s</td><td align="right">704.8 s</td><td align="right">800.3 s</td>
	</tr>
	<tr>
		<td><code>current-gantry-053922</code></td>
		<td align="right">635.1 s</td><td align="right">706.1 s</td><td align="right">886.3 s</td>
	</tr>
	<tr>
		<td><code>current-gantry-045041</code></td>
		<td align="right">637.9 s</td><td align="right">708.9 s</td><td align="right">909.9 s</td>
	</tr>
</table>

### 4,997-node ad hoc fleet pulls

These runs use a DaemonSet with one pod on every node labeled
`gantry-benchmark=worker`. Duration is measured from DaemonSet creation to the
latest container `startedAt` timestamp. It therefore includes image pull,
verification, unpack, and container creation, not only network transfer.

<table border="1" cellspacing="0" cellpadding="6">
	<tr>
		<th>Run</th><th>Image</th><th>Size</th><th>Payload layers</th>
		<th>Completed</th><th>Started</th><th>Last container start</th><th>Duration</th>
	</tr>
	<tr>
		<td><code>fleet-10g-20260909-134716</code></td>
		<td><code>sha256:aa95e2c0...a572fd56</code></td>
		<td align="right">10 GiB</td><td align="right">10</td>
		<td align="right">4,997/4,997</td>
		<td><code>2026-09-09T13:47:16Z</code></td>
		<td><code>2026-09-09T13:59:55Z</code></td>
		<td align="right">759 s (12m39s)</td>
	</tr>
	<tr>
		<td><code>fleet-20g-20260909-141523</code></td>
		<td><code>sha256:c6c274fe...6a916b8e</code></td>
		<td align="right">20 GiB</td><td align="right">20</td>
		<td align="right">4,997/4,997</td>
		<td><code>2026-09-09T14:15:23Z</code></td>
		<td><code>2026-09-09T14:37:17Z</code></td>
		<td align="right">1,314 s (21m54s)</td>
	</tr>
</table>

- The 10 GiB and 20 GiB images have ten and twenty unique 1 GiB payload layers,
  respectively. They share one Alpine base layer and no payload layers.
- The earliest container started at `2026-09-09T13:52:14Z`; all 4,997 were
  running in the 10 GiB run by the last-container timestamp above.
- For the 20 GiB run, the earliest container started at
  `2026-09-09T14:23:38Z`; all 4,997 were running by the last-container
  timestamp above.


- ACR traffic is wire bytes at the Private Endpoint and runs about 9% above the
  payload Gantry accounts for.
- Latency is dominated by writing and unpacking 40 GiB per node, so it is
  largely insensitive to registry-traffic changes.
- Runs older than `chair-design-030517` are carried forward unchanged and their
  duration definitions were not re-verified.
