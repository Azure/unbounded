# Gantry ACR benchmark runbook

Run commands from the repository root on a dedicated test cluster.

## 1. Configure

```bash
cp hack/gantry-benchmark/env.example hack/gantry-benchmark/env.local
```

Set `BENCHMARK_CONFIRM_CONTEXT` to the exact `kubectl config current-context`
output. Set the ACR login server, node count, image shape, and local container
engine. `ACR_USERNAME` and `ACR_PASSWORD` are required by `run` for the local
image build and push, but are not stored in Kubernetes or result artifacts.

For an admin-disabled ACR:

```bash
set -a
. hack/gantry-benchmark/env.local
set +a
export ACR_PASSWORD="$(az acr login --name "$ACR_NAME" --expose-token --query accessToken -o tsv)"
```

Confirm the target:

```bash
kubectl config current-context
kubectl get nodes -l kubernetes.io/os=linux
kubectl -n gantry-system get daemonset gantry
```

## 2. Test and enable

```bash
make -C hack/gantry-benchmark test
make -C hack/gantry-benchmark enable
```

`enable` requires the confirmed context, exact Ready node count, converged
Gantry DaemonSet, and no existing benchmark lock. It creates monitoring and
state resources only; no data-path component is deployed.

## 3. Preflight

```bash
make -C hack/gantry-benchmark preflight
```

Preflight requires:

1. The exact configured count of Ready, schedulable target nodes.
2. A fully converged Gantry DaemonSet.
3. Azure Monitor definitions for ACR `TotalPullCount` and
   `SuccessfulPullCount`.
4. Prometheus coverage for all Gantry agents in containerd storage mode.
5. A positive minimum Gantry DHT health score.

Do not run the benchmark if preflight fails.

## 4. Run

```bash
make -C hack/gantry-benchmark run
```

The transaction:

1. Logs in to ACR through the local container engine.
2. Builds and pushes two independent random-payload images.
3. Backs up every node's ACR-specific containerd configuration.
4. Routes containerd directly to ACR and runs the baseline Job.
5. Waits for Azure Monitor ingestion and records ACR and kubelet metrics.
6. Routes containerd strictly to local Gantry and runs the cold Job.
7. Records native ACR, kubelet, Gantry, and Job metrics.
8. Writes the phase and comparison artifacts.
9. Restores every node's previous ACR routing.

Azure Monitor ACR metrics are registry-wide and emitted at one-minute
granularity. `BENCHMARK_METRICS_SETTLE_TIME` controls the post-phase ingestion
wait. Keep unrelated registry traffic quiet during both phases.

Useful watches:

```bash
make -C hack/gantry-benchmark status
kubectl -n gantry-benchmark get jobs,pods
kubectl -n gantry-system get daemonset gantry
```

## 5. Inspect

```bash
run_id=$(find tmp/gantry-benchmark -mindepth 1 -maxdepth 1 -type d -name 'run-*' -printf '%f\n' | sort | tail -1)
cat "tmp/gantry-benchmark/${run_id}/comparison.md"
jq . "tmp/gantry-benchmark/${run_id}/comparison.json"
```

The report explicitly distinguishes estimated origin bytes from native ACR
and Prometheus counters. It also records bounded Kubernetes warning markers,
Gantry origin failure classes, and all peer-fetch outcomes. Raw event messages
are not persisted because ACR errors can contain signed URLs. Latency is
informational and does not affect the gating result.

To open Grafana:

```bash
kubectl -n monitoring port-forward service/kps-grafana 3000:80
```

Use dashboard `Gantry ACR Benchmark`. ACR values are in the local artifacts;
Grafana shows live kubelet, Job, Gantry, and DHT telemetry.

## 6. Disable

```bash
make -C hack/gantry-benchmark disable
```

After cleanup, verify:

```bash
kubectl get namespace gantry-benchmark 2>/dev/null || true
kubectl -n gantry-system get configmap gantry-benchmark-lock 2>/dev/null || true
kubectl -n gantry-system get daemonset gantry
```

## Failure recovery

| Symptom | Action |
| --- | --- |
| Interrupted run | Run `disable` with the same environment. |
| Confirmed context mismatch | Independently verify the cluster, then correct `BENCHMARK_CONFIRM_CONTEXT`. |
| Wrong eligible-node count | Repair NotReady or cordoned nodes, or correct `BENCHMARK_NODE_COUNT`. |
| ACR metrics unavailable | Verify Azure CLI subscription access and ACR metric definitions. |
| ACR metrics remain empty | Increase `BENCHMARK_METRICS_SETTLE_TIME` and ensure the phase actually reached ACR. |
| Node `hosts.toml` conflict | Preserve concurrent changes; inspect the named node and rerun `disable` after reconciling. |
| Regression gate fails | Inspect artifacts; routing is restored even though `run` returns nonzero. |

Do not delete the namespace while state reports `baseline-routing`,
`gantry-routing`, `restore-failed`, or `disabling`. The state and restore
DaemonSet are what make per-node routing recovery safe.