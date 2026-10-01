# Read-only Racer measurement

`measure.py` requires Python 3 (stdlib only), GNU `timeout`, and an authenticated
`kubectl`. It issues only GET requests to Prometheus's instant-query API via the
Kubernetes service proxy. It never reads Secret resources, kubeconfig contents,
pod specs, or ConfigMaps; kubectl uses its normal authentication. There is no
port-forward, remote exec, load change, rollout, or other cluster mutation.

```sh
timeout --signal=TERM --kill-after=10s 300s \
  python3 -B hack/scripts/racer-rollout/measure.py \
  --context joolshev-scale-test --namespace unbounded-system
```

The context is explicitly passed on every request, with the default shown
above. Change `--prometheus-namespace monitoring`,
`--prometheus-service prometheus:9090`, or `--node-regex 'aks-.*'` as needed.
The service argument is a service name and port, not an arbitrary proxy path.
`--timestamp 2026-10-01T05:00:00Z` reproduces an evaluation within Prometheus's
retention period. All queries use that same UTC timestamp and a fixed `[5m]`
rate window, even though requests execute sequentially. Queries are included
in the JSON stdout report for audit/replay. Prometheus warnings are retained.

Optional `--raw-output tmp/measurement.json` writes both the report and original
Prometheus responses. The parent directory must already exist; the file must
not exist (exclusive creation, no overwrite). No directories are created.
Keep operational artifacts ignored and do not commit them. The default writes
only stdout. Each kubectl invocation has a request timeout, an external TERM
timeout with a 10-second kill grace, and a Python watchdog. The measurement
has a 240-second request budget and stops on the first failed query. Interrupts
terminate the subprocess group, including authentication-plugin children.
Kubectl stderr is suppressed to avoid exposing credential-plugin output.

## Meaning of the output

- `per_node` provides decimal **Gbps** (`bytes/second * 8 / 1e9`) for verified
  goodput and eth0 egress/ingress. `summary` includes observed count, aggregate
  sum, p05, median, p95, min, and max for every numeric field. Percentiles use
  linear interpolation at `(n-1)*q`; these are distributions across nodes,
  not latency percentiles. Missing nodes are excluded, never filled with zero;
  inspect `missing_nodes_by_metric` before interpreting a partial aggregate.
- **Completion accounting:** `racer_loadgen_verified_bytes_total` receives
  the manifest, config, and all layer sizes only after a full successful
  SHA-256-verified image pull (`cmd/racer-loadgen/pull.go:185-203`). It is not
  streaming wire throughput. Partial, failed, canceled, and unverified pulls
  earn no verified bytes (`cmd/racer-loadgen/pull_test.go:239-247`). Large images
  can make this rate bursty or zero while bytes are still moving. Received or
  mirror bytes include partial transfers and must not replace this metric.
- **No VF double counting:** NIC queries select exactly `device="eth0"`, never
  `eth*|en*` or a sum over all interfaces. On hosts where the VF and synthetic
  interface represent the same traffic, including both counts it twice.
  Duplicate eth0 scrape series are collapsed with `max by (node)`, not added.
  This intentionally differs from the dashboard's broad interface allowlist
  (`deploy/racer/grafana-dashboard.json:245,252`). RX and TX are reported
  separately; do not add them or equate them to verified goodput. They include
  all host traffic, including peer replication, origin traffic, and overhead.
- Success and error rates count **completed image attempts per second**, not
  individual HTTP requests. Errors include every result other than `success`,
  including timeouts and cancellations. Result counters are lazy: absent
  result series use a zero baseline only where the verified counter has rate
  history. Completion fractions are null when there are no completions or
  coverage is incomplete. A successful pull with verification disabled is
  still a success, but earns no verified goodput.
- `zero_progress_nodes` have an observed verified rate of exactly zero;
  `active_zero_progress_nodes` also have current applied concurrency above
  zero. Neither is proof of a stall. Paused nodes normally appear in the first
  list. Missing history is listed separately. Current gauges do not establish
  whether concurrency was constant throughout the five-minute window.
- Readiness is the dataplane's `racer_ready` gauge
  (`cmd/racer-dataplane/src/telemetry.rs:436-441`), not Kubernetes PodReady or
  NodeReady. A node is counted ready only if its dataplane scrape is also up.
  Missing and down scrapes are explicit. The node population is the union of
  observed loadgen targets, dataplane targets, and readiness series; entirely
  undiscovered nodes cannot be counted. Compare `node_count` to the campaign's
  expected population. `loadgen_up` separately reports loadgen scrape health.
- Applied concurrency is summed per node, with a node-count distribution;
  inflight is currently admitted image pulls. During a concurrency decrease,
  inflight can exceed applied concurrency while existing pulls drain
  (`cmd/racer-loadgen/metrics.go:36-37`). These are not CPU utilization.
- `resource_cpu_cores` is the five-minute rate of
  `container_cpu_usage_seconds_total` for the selected namespace, excluding
  empty/`POD` infrastructure containers. Use `--resource-job` to name the one
  job scraping kubelet `/metrics/resource` (default `kubelet-resource`). It
  requires `node`, `namespace`, and `container` labels. Missing resource
  telemetry stays null; it is not silently replaced with another CPU source.
  `process_cpu_cores_by_app` separately reports instrumented Racer/Gantry
  process CPU, which may not cover every process or child. Host non-idle CPU
  fraction includes every workload and iowait, not just Racer. These CPU
  measures must not be added to each other; a sum of host fractions is not a
  fleet utilization percentage.

The default queries expect the existing `kubernetes-pods` and `node-exporter`
jobs with node labels and one scrape target per pod. Scope namespaces/nodes to
one campaign, and avoid duplicate scrape jobs or replicas masquerading as
independent workers. The helper fails on malformed, duplicate output-label,
non-finite, or unsuccessful query results; missing telemetry is reported, not
treated as query failure or permission to resume load.

## Focused offline tests

```sh
timeout --signal=TERM --kill-after=10s 60s \
  python3 -B -m unittest discover -s hack/scripts/racer-rollout -p test_measure.py -v
```
