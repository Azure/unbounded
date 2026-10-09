# Direct Zipf dashboard: 512 x 2 GiB (1 TiB catalog)

`grafana-direct-zipf.json` has five panels. Its UID is `racer-direct-zipf`.
Use datasource UID `prometheus`, 60-second scrapes, and the existing
`kubernetes-pods` and `node-exporter` jobs. The default namespace is
`unbounded-system`.

Run only the intended `racer-loadgen` workload in the selected namespace:
direct UDS, `--catalog-blobs=512`, `--blob-bytes=2147483648`,
`--seed=zipf-balanced-v1`, `--profile=zipf`, and `--zipf-exponent=0.5`.
The balanced-config experiment uses owned-only disk retention (admission
disabled), 8 GiB plaintext, 16 GiB ciphertext, and 2 GiB each for dirty and
registered buffers. This is a workload description, not a claim
that measured memory, disk, and peer event shares are balanced.
Successful payload is completed successful operations times that exact size.
It works without client hashing but is not verified goodput or wire traffic.
The node average divides by distinct discovered `racer-dataplane` nodes,
including down scrape targets, not expected fleet size.

The size multiplier is fixed. The success counter has no blob-size label.
Both throughput queries join on job, namespace, and pod with
`process_start_time_seconds >= 1791576350` (2026-10-09 20:05:50 UTC).
This rollout boundary excludes old 512 MiB and 64 MiB processes, including
historical windows. All 100 new processes were observed starting after that
boundary. Missing process metrics or successful new-pod samples leave no data,
not a zero or an inflated old-size result. Wait for new-pod readiness and a full
five-minute window before interpreting throughput. The boundary is not a size
label: do not mix later loadgen sizes in this namespace. When reusing this
dashboard for another size, change both multipliers and the rollout boundary.

The cache panel shows shares of memory, disk, and peer **hit events**, excluding
origin fills. It is not a client cache-hit ratio or byte breakdown. A peer page
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

Run the dashboard checks with:

```sh
timeout --signal=TERM --kill-after=10s 300s python3 deploy/racer/grafana_direct_zipf_test.py
```

Merge only the `racer-direct-zipf.json` data key into the existing
`monitoring/grafana-dashboards` ConfigMap. Preserve other keys. The current
Grafana file provider polls its mounted directory every 30 seconds; no restart
is needed. See [the provisioning guide](grafana-README.md#provision-through-the-existing-mounted-configmap).
Open `/d/racer-direct-zipf` through the existing Grafana access path.

If Grafana says the datasource is missing, check its datasource health API and
run a panel query through Grafana, not just Prometheus. The UID can exist while
the Prometheus plugin is not registered. See the
[startup recovery note](grafana-README.md#grafana-startup-troubleshooting).
Preserve Grafana's database and plugin files before replacing a pod that uses
an `emptyDir` data volume.
