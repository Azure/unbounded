# Direct Zipf dashboard: 16384 x 1 GiB (16 TiB catalog)

`grafana-direct-zipf.json` has six panels and ten queries. Its UID is `racer-direct-zipf`.
Use datasource UID `prometheus`, 60-second scrapes, and the existing
`kubernetes-pods` and `node-exporter` jobs. The default namespace is
`unbounded-system`.

Run only the intended `racer-loadgen` workload in the selected namespace:
direct UDS, `--catalog-blobs=16384`, `--blob-bytes=1073741824`,
shared `--seed=zipf-balanced-v2`, `--profile=zipf`, and `--zipf-exponent=1.1`
(Zipf exponent 1.1), with 32 catalog workers (`--catalog-workers=32`).
The second-sight local disk retention experiment keeps C16,
48 GiB plaintext, 64 GiB ciphertext, and 2 GiB each for dirty and registered
buffers. `RACER_ADMISSION_MODE=second-sight` is the runtime default. Repeated
nonowner reads can admit verified ciphertext to local disk, subject to resource
limits. A retry within the same logical read is not a second sight. Resource
admission remains enforced; disk retention does not bypass dirty-buffer limits.
This configuration does not imply balanced hit shares. Treat post-rollout
observations as warm-up, not steady state; a full query window alone does not
prove steady state.

This dashboard update does not change dataplane quotas, devices, worker sizing,
or resource limits. Provision only the dashboard ConfigMap key below.

The mode default is in `cmd/racer-dataplane/src/config.rs`; `app.rs` wires it to
the shared retention policy. The tests in `src/read/tests/fill.rs`
(`second_sight_memory_hit_persists_original_ciphertext_and_retry_does_not` and
`second_sight_selected_and_optional_pressure_preserve_read_success`) cover
repeated nonowner reads and resource pressure.

Successful payload is completed successful operations times that exact size.
It works without client hashing but is not verified goodput or wire traffic.
The node average divides by distinct discovered `racer-dataplane` nodes,
including down scrape targets, not expected fleet size.

The size multiplier is fixed. The success counter has no blob-size label.
Both successful-payload queries select the Pod-template label
`racer.unbounded-cloud.io/benchmark=zipf-16t-v3`, scraped as
`racer_unbounded_cloud_io_benchmark="zipf-16t-v3"`. Keep the namespace, job, and
`app_kubernetes_io_name="racer-loadgen"` selectors. Old and unlabeled loadgens are
excluded. The prior exponent 1.0 run used `zipf-16t-v2` with the same catalog
bytes and v2 seed. The v3 label separates popularity changes, not content changes.
Do not label old Pods or reuse this label for another blob size or exponent.
Missing labeled successful-pull samples leave no data, not zero, including
during catalog hashing. Wait for new-pod
readiness and a full five-minute window before interpreting throughput.
When changing sizes, change the benchmark label and both multipliers together.

Check the number of labeled loadgens contributing successful-counter samples
to the five-minute rate, not all discovered Pods or request-counter samples:

```promql
count(rate(racer_loadgen_pulls_total{job="kubernetes-pods",namespace="${namespace}",app_kubernetes_io_name="racer-loadgen",racer_unbounded_cloud_io_benchmark="zipf-16t-v3",result="success"}[5m]))
```

This counts eligible series, not successful operations or scrapes. With one
scrape target per loadgen, expect 100 after rollout and enough samples; verify
discovery separately. A zero rate can still be counted, and no matching series
returns no data. The source counter increments once per completed operation
(`cmd/racer-loadgen/pull.go:251-281`); `result="success"` requires no error
(`pull.go:314-324`). Generic batches contain one blob
(`cmd/racer-loadgen/blob.go:149`).

The cache panel shows shares of memory, disk, and peer **hit events**, excluding
origin fills. These events are nonexclusive, not a client cache-hit ratio or byte breakdown. A peer page
can also count as a memory or disk hit on its serving node. Missing data and
idle cache-hit ratios stay undefined.

Host CPU is **average busy logical cores per Racer node**, not a percentage
or total cluster cores. The query sums `1 - rate(idle[5m])` across logical CPUs
by node, then averages those sums across discovered Racer hosts with CPU samples.
It includes all host workloads and I/O wait, with equal weight per sampled
Racer host. System-only nodes are excluded; missing CPU samples are not zero.
On 64-logical-CPU hosts, this equals the previous non-idle fraction times 64
(or the displayed percentage divided by 100, then times 64). The number of
Racer nodes does not multiply the result. Mixed CPU counts still use each
host's own core sum, not a fixed 64-core multiplier.

The disk panel uses IEC byte units. It sums `racer_disk_size_bytes` for total
assigned physical segment capacity and `racer_disk_used_bytes` for reserved
physical record bytes. These share the same accounting basis, unlike physical
capacity and indexed payload bytes. Total excludes raw-device guards and
unassigned remainder. Used includes aligned record overhead, padding, and
reservations until segment recycle; it does not count unused sealed tails.
It is not live payload, filesystem usage, or effective payload capacity.
Total minus used is therefore not guaranteed writable payload space. Both
series include only scraped dataplanes; verify disk metrics cover all 100 nodes.

The source contract is in `cmd/racer-dataplane/alloc/src/segments.rs`
(`configure_usage_groups`, `usage`, and `append`),
`cmd/racer-dataplane/src/store.rs` (`observe_disks` and aligned record allocation),
and `cmd/racer-dataplane/src/telemetry.rs` (`write_disks`). Workers sharing a disk
are summed once by disk label before scrape export.

## Network throughput

**Total Racer-node network TX/RX (Gbit/s)** sums five-minute byte-counter rates
on physical interface `ens1`, then converts bytes/s to decimal Gbit/s with
`* 8 / 1e9`. TX and RX stay separate. Do not add them as unique traffic: a
cross-host transfer appears in TX at one end and RX at the other.

The `and on (node) group by (node) (up{...racer-dataplane...})` join limits
each direction to discovered Racer nodes in the selected namespace, including
down dataplane scrape targets. Live validation found 100 Racer nodes among
103 node-exporter nodes. System-only nodes are excluded. Discovery is not
expected inventory; removed targets disappear. Missing network samples reduce
totals and are not zero-filled.

This is host traffic, including non-Racer and external traffic, not peer-only
throughput or node-pair attribution. It approximates traffic between nodes in
this workload; the counters cannot identify each node pair. Count only `ens1`,
not `eth0` or Geneve as well, to avoid counting traffic at multiple interfaces.
The shared `geneve0` tunnel is not Racer-exclusive: network routing includes
peer pod/node IPs and gateway prefixes
(`cmd/unbounded-net-node/tunnel_config.go:467-504`). This fixed interface choice
must be checked again if the host network layout changes.

Check coverage for **each** direction, replacing `transmit` with `receive`:

```promql
count(rate(node_network_transmit_bytes_total{job="node-exporter",device="ens1"}[5m]) and on (node) group by (node) (up{job="kubernetes-pods",namespace="${namespace}",app_kubernetes_io_name="racer-dataplane"}))
```

Expect 100 contributing series with one exporter target per node. Check distinct
node labels and recent source-counter timestamps too; a rate window can contain
old samples. Zero traffic still contributes a series, while absent samples do not.

Run the dashboard checks with:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 -B deploy/racer/grafana_direct_zipf_test.py
timeout --signal=TERM --kill-after=10s 300s python3 -B deploy/racer-loadgen/zipf/render_test.py
```

Merge only the `racer-direct-zipf.json` data key into the existing
`monitoring/grafana-dashboards` ConfigMap. Preserve other keys. The current
Grafana file provider polls its mounted directory every 30 seconds; no restart
is needed. See [the provisioning guide](grafana-README.md#provision-through-the-existing-mounted-configmap).
Open `/d/racer-direct-zipf` through the existing Grafana access path.

If Grafana says the datasource is missing, check its datasource health API and
run all ten panel queries through Grafana, not just Prometheus. The UID can exist while
the Prometheus plugin is not registered. See the
[startup recovery note](grafana-README.md#grafana-startup-troubleshooting).
Preserve Grafana's database and plugin files before replacing a pod that uses
an `emptyDir` data volume.
