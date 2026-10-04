# Racer performance dashboard

[`grafana-dashboard.json`](grafana-dashboard.json) is the hand-edited source asset
for **Racer Performance**, UID `racer-performance`. It has 26 data panels covering
availability, serving throughput/latency, cache lookups, admission pressure,
optional loadgen pulls, and shared-node resources. No generator is required.

## Data contract

- Grafana Prometheus datasource UID: **`prometheus`**. Configure its scrape
  interval as **60s**; dashboard rates use **`[5m]`**, with a one-minute refresh.
- Existing pod discovery job: **`kubernetes-pods`**. Scrape each process once.
  Target labels must include `namespace`, `node`, `pod`, and
  `app_kubernetes_io_name` (`racer-dataplane`, `gantry`, `racer-loadgen`).
  The current installation uses pod scrape annotations, dataplane port `19090`,
  Gantry port `9095`, and `/metrics`; these are installation choices, not new
  scrape jobs. This dashboard does not change Prometheus configuration.
  Live pod metadata-only annotations do **not** survive pod replacement. Persist
  equivalent `prometheus.io/scrape`, `prometheus.io/port`, and `prometheus.io/path`
  annotations through the owning workload's pod template or supported overrides
  during a **planned rollout**, not an immediate fleet rollout for this dashboard.
  Scrape recovery for this installation patched existing pods only; no Prometheus
  configuration, workload templates, or overrides were changed.
- Namespace and node selectors support multiple values and All. Namespace
  initially selects `unbounded-system`; select All when components span namespaces.
- Node-resource panels require current node-exporter series carrying a `node`
  label matching pod discovery. They deliberately do not use historical cAdvisor
  metrics. Namespace does **not** apply to shared-host resources; Node still does.
  NIC queries allow `eth[0-9]+|en.*` interfaces; adapt this allowlist if needed.
- Outlier panels are **instant `topk(10)`** rankings. Range-query topk can accumulate
  many different nodes over time and is deliberately avoided for large fleets.
- Missing telemetry remains **No data**; ratios with no observations are undefined.
  Allow several scrapes after enabling targets. A successful scrape is not
  application readiness; a removed target disappears rather than exporting zero.

Downstream, origin, received, and verified bytes are different, overlapping
boundaries, not additive bandwidth. Verified bytes require successful, fully
verified image pulls. Page-source events are not request counts or byte rates.
Only explicit per-tier lookup hits/(hits+misses) are presented as cache hit ratios.
Histograms estimate latency using coarse buckets; Gantry handler latency includes
errors/aborts and downstream writes. Node CPU, memory, and NIC panels include all
host workloads, not just Racer. There is no exported cache occupancy metric here.

Definitions: `cmd/gantry/agent_racer_metrics.go`,
`cmd/racer-dataplane/src/telemetry.rs`, and `cmd/racer-loadgen/metrics.go`.
See also `designs/racer-prometheus-queries.md` and the dataplane telemetry
`INTEGRATION.md` for interpretation, checking implementation when prose drifts.

## Provision through the existing mounted ConfigMap

The current Grafana deployment mounts **`monitoring/grafana-dashboards`** at
`/etc/grafana/dashboards`, and **`monitoring/grafana-provisioning`** under
`/etc/grafana/provisioning`. This is file provisioning, **not a dashboard sidecar**.
A `grafana_dashboard=1` label on a new ConfigMap alone will not load this dashboard.

The existing file provider should point to the mounted directory, with
**30-second polling** (retain the installation's provider name/folder):

```yaml
apiVersion: 1
providers:
  - name: dashboards
    type: file
    updateIntervalSeconds: 30
    options:
      path: /etc/grafana/dashboards
```

From the repository root, inspect the current context and existing mount/provider
first. Then merge only this dashboard key into the **existing** ConfigMap:

```sh
timeout --signal=TERM --kill-after=10s 300s kubectl config current-context
timeout --signal=TERM --kill-after=10s 300s kubectl -n monitoring get deployment grafana -o yaml
timeout --signal=TERM --kill-after=10s 300s kubectl -n monitoring get configmap grafana-provisioning -o yaml
set -o pipefail
timeout --signal=TERM --kill-after=10s 300s kubectl -n monitoring create configmap grafana-dashboards \
  --from-file=racer-performance.json=deploy/racer/grafana-dashboard.json \
  --dry-run=client -o json |
  timeout --signal=TERM --kill-after=10s 300s kubectl -n monitoring patch configmap grafana-dashboards \
    --type merge --patch-file /dev/stdin
```

JSON merge patch preserves other dashboard keys and unrelated ConfigMap fields.
Do not replace the ConfigMap, overwrite the provider/datasource files, or add new
scrape jobs for this dashboard. Directory-mounted ConfigMap updates propagate
asynchronously, followed by the provider's polling interval. A `subPath` file
mount does not receive updates; resolve that deployment difference before relying
on polling. Provider configuration changes may require a Grafana restart; merely
updating the dashboard JSON normally does not.

### Grafana startup troubleshooting

In the validated **Grafana 13.2.3** installation, a failed startup auto-update on
`readOnlyRootFilesystem` unregistered the bundled Prometheus plugin. If the
datasource plugin disappears, inspect startup logs and disable startup plugin
installation/update on the Grafana Deployment with both environment settings:

```text
GF_PLUGINS_PREINSTALL_AUTO_UPDATE=false
GF_PLUGINS_PREINSTALL_DISABLED=true
```

Keep the read-only filesystem protection. Also provide adequate Grafana memory:
this installation required raising its limit from **512Mi to 1Gi** after an OOM.
That is an observed recovery setting, not a universal sizing recommendation.
The operational recovery changed the Grafana Deployment, not Prometheus config
or Racer/Gantry workload templates. No generated deployment manifests are needed
to import this dashboard.

## Access and validation

Use the existing authenticated Grafana URL, or a bounded local forward:

```sh
timeout --signal=TERM --kill-after=10s 300s kubectl -n monitoring port-forward service/grafana 3000:3000
```

Adjust the Service name/port to its actual configuration. Open
`http://localhost:3000/d/racer-performance` and sign in using the installation's
normal credentials. Confirm the selected namespace/node, populated overview and
latency panels, optional loadgen activity, and node-resource labels. Check Grafana
Query Inspector for errors; an absent optional metric should remain No data.
The forward ends after five minutes; restart it only when still needed.
