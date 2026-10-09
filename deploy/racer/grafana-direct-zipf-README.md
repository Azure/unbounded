# Direct Zipf dashboard: 512 x 512 MiB

`grafana-direct-zipf.json` has four panels. Its UID is `racer-direct-zipf`.
Use datasource UID `prometheus`, 60-second scrapes, and the existing
`kubernetes-pods` and `node-exporter` jobs. The default namespace is
`unbounded-system`.

Run only the intended `racer-loadgen` workload in the selected namespace:
direct UDS, `--catalog-blobs=512`, `--blob-bytes=536870912`,
`--seed=zipf-balanced-v1`, `--profile=zipf`, and `--zipf-exponent=0.5`.
The balanced-config experiment uses owned-only disk retention (admission
disabled), 4 GiB plaintext, 8 GiB ciphertext, and 2 GiB each for dirty and
registered buffers. This is a workload description, not a claim
that measured memory, disk, and peer event shares are balanced.
Successful payload is completed successful operations times that exact size.
It works without client hashing but is not verified goodput or wire traffic.
The node average divides by distinct discovered `racer-dataplane` nodes,
including down scrape targets, not expected fleet size.

The size multiplier is fixed and there is no benchmark label selector. After a
size change, wait for all new loadgen pods to be ready and a full five-minute
window with only new-pod samples before interpreting throughput. Historical
64 MiB intervals and rollout windows are invalid under the 512 MiB multiplier.
Do not mix other loadgen sizes in the selected namespace.

The cache panel shows shares of memory, disk, and peer **hit events**, excluding
origin fills. It is not a client cache-hit ratio or byte breakdown. A peer page
can also count as a memory or disk hit on its serving node. Host CPU includes
all workloads and I/O wait, with equal weight per sampled Racer host. Missing
data and idle ratios stay undefined.

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
