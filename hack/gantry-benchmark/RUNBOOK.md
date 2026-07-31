# Gantry ACR benchmark runbook

Run every command from the Unbounded repository root. This workflow is for a
dedicated test cluster.

## 1. Configure

```bash
cp hack/gantry-benchmark/env.example hack/gantry-benchmark/env.local
```

Fill in:

- `BENCHMARK_CONFIRM_CONTEXT`
- `BENCHMARK_MODE=direct`
- `BASELINE_ACR_NAME`, `BASELINE_ACR_LOGIN_SERVER`, and baseline push credentials
- `GANTRY_ACR_NAME`, `GANTRY_ACR_LOGIN_SERVER`, and Gantry push credentials

Legacy proxy mode instead uses `ACR_LOGIN_SERVER`, `ACR_USERNAME`,
`ACR_PASSWORD`, and `BENCHMARK_PROXY_IMAGE`.

For source-authoritative measurements also fill in:

- `BENCHMARK_AZURE_TELEMETRY=true`
- `AZURE_LOG_ANALYTICS_WORKSPACE_ID`
- `AZURE_AKS_RESOURCE_ID`
- `AZURE_BASELINE_ACR_RESOURCE_ID`
- `AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID`
- `AZURE_GANTRY_ACR_RESOURCE_ID`
- `AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID`

`env.local` is gitignored. Direct-mode credentials are passed only to the local
container engine; they are not written to Kubernetes, benchmark state, or
result files. Proxy-mode credentials are also stored in its Kubernetes Secret.

Confirm the current cluster before continuing:

```bash
kubectl config current-context
kubectl get nodes -l kubernetes.io/os=linux
kubectl -n gantry-system get daemonset gantry
```

When Azure telemetry is enabled, the operator machine needs ACR data-plane
access only through image preparation. Preflight requires public access to be
disabled on both ACRs and verifies that each configured Private Endpoint is
approved for its exact registry and exposes `PEBytesIn`.

## 2. Build the proxy (proxy mode only)

```bash
make -C hack/gantry-benchmark test
make -C hack/gantry-benchmark proxy-image
make -C hack/gantry-benchmark proxy-push
```

The proxy exposes:

- Port `5002`: OCI registry proxy.
- Port `9090`: Prometheus metrics, `/debug/summary`, and authenticated phase
  control.

Skip this section in direct mode.

## 3. Enable instrumentation

```bash
make -C hack/gantry-benchmark enable
```

`enable` fails unless:

- The confirmed and current kubectl contexts match.
- Exactly 300 schedulable Ready nodes match `BENCHMARK_IMAGE_PLATFORM`.
- Gantry reports 300 desired, updated, Ready, and available pods.
- No benchmark state ConfigMap already exists.

It then creates in both modes:

- Namespace `gantry-benchmark`.
- Cluster-wide lock ConfigMap `gantry-system/gantry-benchmark-lock`.
- A PodMonitor scoped to Gantry. Its samples carry
   `gantry_benchmark="true"` so existing scrapes cannot double the results.
- The `Gantry ACR Benchmark` Grafana dashboard.
- A state ConfigMap containing the exact original Gantry configuration but no
  credentials.

Proxy mode additionally creates the ACR credential/phase-control Secret, one
counting-proxy Deployment and Service, and the proxy PodMonitor.

No containerd or Gantry routing is changed by `enable`.

## 4. Prepare workload images

While the operator machine can still reach both ACRs, mint their short-lived
tokens, then build and push both fresh,
digest-pinned phase images:

```bash
baseline_refresh_token=$(az acr login --name "$BASELINE_ACR_NAME" --expose-token --query accessToken -o tsv)
gantry_refresh_token=$(az acr login --name "$GANTRY_ACR_NAME" --expose-token --query accessToken -o tsv)
export BASELINE_ACR_PASSWORD="$baseline_refresh_token"
export GANTRY_ACR_PASSWORD="$gantry_refresh_token"

make -C hack/gantry-benchmark prepare

unset BASELINE_ACR_PASSWORD GANTRY_ACR_PASSWORD baseline_refresh_token gantry_refresh_token
```

`prepare` generates one random payload set and pushes the same repository and
tag to both ACRs. The payload SHA, bytes, size, and layer count are identical.
Phase-specific paths inside every payload layer intentionally produce different
OCI digests so the Gantry phase cannot reuse baseline content on the same node.
It does not run pull pods or warm target-node caches.

For Azure telemetry, disable public access on both ACRs after `prepare` succeeds:

```bash
az acr update -g "$RESOURCE_GROUP" -n "$BASELINE_ACR_NAME" \
   --public-network-enabled false
az acr update -g "$RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" \
   --public-network-enabled false
```

## 5. Run preflight

```bash
make -C hack/gantry-benchmark preflight
```

Preflight performs these mandatory checks before routing changes:

1. Requires Prometheus to report the current revision for every Gantry pod.
2. Requires the minimum Gantry DHT health score to be greater than zero.
3. In direct mode, requires `gantry_origin_bytes_total` on every Gantry pod.
4. Requires `gantry_peer_serve_bytes_total` on every Gantry pod.
5. With Azure telemetry, verifies Azure authentication, both ACR/Private
   Endpoint bindings, disabled public access on both ACRs, `PEBytesIn`, AKS,
   and both Log Analytics tables.
6. In proxy mode, smoke-tests the proxy, checks reachability from every target
   node, and requires the setup request in Prometheus.

Do not run the benchmark if preflight fails.

## 6. Run the comparison

```bash
make -C hack/gantry-benchmark run
```

The command executes one transaction using the images recorded by `prepare`:

1. Backs up each node's ACR-specific containerd configuration.
2. Installs direct routing for the baseline ACR and runs the 300-pod baseline Job.
3. Installs strict local-mirror routing for the Gantry ACR and runs the Gantry
   cold Job. Direct mode leaves Gantry pointed at its dedicated ACR.
4. Writes phase results and the comparison.
5. Restores both prior registry-specific files on every node or removes each
   file when it was originally absent.
6. Proxy mode restores the exact Gantry ConfigMap; direct mode verifies it was
   never changed.

Each pull container sleeps for 15 seconds after starting so AKS audit records a
running status patch. Audit startup latency is creation-to-first-running-status
receipt, not Job completion time.

In proxy mode, phase changes wait up to `BENCHMARK_ROLLOUT_TIMEOUT` for all
proxy requests attributed to the current phase to drain. Both proxy-mode
routing phases are fail-closed through the proxy, so fallback traffic remains
measured rather than escaping directly to ACR.

In direct mode, Gantry-cold origin bytes come from
`gantry_origin_bytes_total`. After the Job completes, the runner waits for zero
in-flight pulls and 20 seconds of stable counters before recording the phase.

With Azure telemetry enabled, each phase is isolated on whole UTC-minute
boundaries and includes a three-minute trailing guard for delayed Private
Endpoint metric buckets. The runner then polls until ACR pulls, `PEBytesIn`, and
all expected audit pod timelines are complete and stable. `comparison.json`
uses those Azure measurements as primary values and retains Kubernetes/Gantry
values as cross-checks.

The collector rejects implausibly small `PEBytesIn` totals even when Azure marks
the metric point non-null. Inspect `minimum_expected_bytes` in the phase
artifact when endpoint metering remains incomplete.

## 7. Inspect results

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

- `comparison.json` for ACR pull counts, `PEBytesIn`, and audit latency.
- Origin-byte and ACR image-pull reduction.
- Per-pod Gantry peer bytes.
- Gantry phase request source by `client_class`.
- Peer hits versus origin pulls.
- Completed pull pods by phase.
- Proxy CPU, network, and inflight requests.

A saturated single proxy can distort latency even though byte and request
totals remain valid. Treat sustained proxy CPU limits or a growing inflight
queue as an invalid latency run.

## 8. Disable

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