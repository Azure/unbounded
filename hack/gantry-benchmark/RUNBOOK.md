# Gantry ACR benchmark runbook

Run every command from the Unbounded repository root. This workflow is for a
dedicated test cluster.

## 1. Configure

```bash
cp hack/gantry-benchmark/env.example hack/gantry-benchmark/env.local
```

Fill in:

- `BENCHMARK_CONFIRM_CONTEXT`
- `ACR_LOGIN_SERVER`
- `ACR_USERNAME`
- `ACR_PASSWORD`
- `BENCHMARK_PROXY_IMAGE`

The proxy image must be in the same ACR being measured. `env.local` is
gitignored. Credentials are passed to the local container engine and stored
in a Kubernetes Secret; they are not written to benchmark state or result
files.

Confirm the current cluster before continuing:

```bash
kubectl config current-context
kubectl get nodes -l kubernetes.io/os=linux
kubectl -n gantry-system get daemonset gantry
```

## 2. Build the proxy

```bash
make -C hack/gantry-benchmark test
make -C hack/gantry-benchmark proxy-image
make -C hack/gantry-benchmark proxy-push
```

The proxy exposes:

- Port `5002`: OCI registry proxy.
- Port `9090`: Prometheus metrics, `/debug/summary`, and authenticated phase
  control.

## 3. Enable instrumentation

```bash
make -C hack/gantry-benchmark enable
```

`enable` fails unless:

- The confirmed and current kubectl contexts match.
- Exactly 300 schedulable Ready nodes match `BENCHMARK_IMAGE_PLATFORM`.
- Gantry reports 300 desired, updated, Ready, and available pods.
- No benchmark state ConfigMap already exists.

It then creates:

- Namespace `gantry-benchmark`.
- Cluster-wide lock ConfigMap `gantry-system/gantry-benchmark-lock`.
- The ACR credential and phase-control Secret.
- One counting-proxy Deployment and ClusterIP Service.
- PodMonitors scoped to the proxy and the Gantry pods. Their samples carry
   `gantry_benchmark="true"` so existing scrapes cannot double the results.
- The `Gantry ACR Benchmark` Grafana dashboard.
- A state ConfigMap containing the exact original Gantry configuration but no
  credentials.

No containerd or Gantry routing is changed by `enable`.

## 4. Run preflight

```bash
make -C hack/gantry-benchmark preflight
```

Preflight performs these mandatory checks before routing changes:

1. Pulls the proxy image manifest and config blob through the proxy and
   requires successful HTTP responses.
2. Runs a host-network DaemonSet and requires the proxy ClusterIP to be
   reachable from every target node.
3. Requires Prometheus to report containerd storage mode for 300 Gantry pods.
4. Requires the minimum Gantry DHT health score to be greater than zero.
5. Requires the proxy setup request to appear in Prometheus.

Do not run the benchmark if preflight fails.

## 5. Run the comparison

```bash
make -C hack/gantry-benchmark run
```

The command executes one transaction:

1. Logs in to ACR without putting the password on the command line.
2. Builds and pushes two independent 1024 MiB random-payload images.
3. Backs up each node's ACR-specific containerd configuration.
4. Installs baseline routing and runs the 300-pod baseline Job.
5. Patches only the matching ACR entry in Gantry's ConfigMap, rolls all Gantry
   pods, installs measured Gantry routing, and runs the Gantry cold Job.
6. Writes phase results and the comparison.
7. Restores every node's prior ACR-specific file or removes the file when it
   was originally absent.
8. Restores the exact Gantry ConfigMap and verifies the full DaemonSet rollout.

Phase changes wait up to `BENCHMARK_ROLLOUT_TIMEOUT` for all proxy requests
attributed to the current phase to drain before counters move to the next
phase.

Both ACR-specific routing modes are fail-closed through the proxy. The
Gantry-mode containerd fallback is the proxy, so fallback traffic remains
measured rather than escaping directly to ACR.

## 6. Inspect results

Print state:

```bash
make -C hack/gantry-benchmark status
```

Find the run ID in that output, then inspect:

```bash
jq . tmp/gantry-benchmark/<run-id>/comparison.json
cat tmp/gantry-benchmark/<run-id>/comparison.md
```

Open Grafana:

```bash
kubectl -n monitoring port-forward service/kps-grafana 3000:80
```

Select dashboard `Gantry ACR Benchmark` and choose the run ID. Check:

- Baseline versus Gantry ACR upstream bytes.
- Origin-byte reduction.
- Gantry phase request source by `client_class`.
- Peer hits versus origin pulls.
- Completed pull pods by phase.
- Proxy CPU, network, and inflight requests.

A saturated single proxy can distort latency even though byte and request
totals remain valid. Treat sustained proxy CPU limits or a growing inflight
queue as an invalid latency run.

## 7. Disable

After recording the results:

```bash
make -C hack/gantry-benchmark disable
```

`disable` is idempotent. It:

1. Switches the proxy to the idle phase when available.
2. Restores node routing from per-node backups.
3. Restores and rolls Gantry if necessary.
4. Requires Gantry to be fully Ready.
5. Deletes the Grafana dashboard and benchmark namespace.
6. Retains local result artifacts.

If restoration fails, the namespace and state ConfigMap remain in place. Fix
the reported conflict and rerun `disable`.

## Failure recovery

| Symptom | Action |
| --- | --- |
| Command interrupted during a phase | Rerun `make -C hack/gantry-benchmark disable`. |
| Enable was killed before state was written | Run `disable` with the same environment; it removes the partial namespace and Gantry-namespace lock. |
| Current context confirmation fails | Set `BENCHMARK_CONFIRM_CONTEXT` to the intended exact context after independently verifying it. |
| Fewer than 300 eligible nodes | Repair NotReady, cordoned, OS, or architecture mismatches before retrying. |
| Proxy OCI smoke fails | Inspect `kubectl -n gantry-benchmark logs deployment/acr-origin-proxy` and verify ACR admin credentials. |
| Node reachability is below 300 | Do not continue. Verify AKS node-to-Service routing and network policy. |
| Gantry ConfigMap hash conflict | Another operator changed the ConfigMap. Reconcile that change manually; the workflow will not overwrite it. |
| Node `hosts.toml` ownership conflict | Inspect the named node and ACR-specific directory. Preserve concurrent operator changes; do not remove markers blindly. |
| Regression gates fail | Inspect the generated comparison and Grafana. Routing is restored even though `run` returns nonzero. |