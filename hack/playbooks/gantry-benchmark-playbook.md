# Gantry Benchmark Tool Playbook

This playbook runs the proxy-free `hack/gantry-benchmark` workflow on an
existing AKS cluster with Gantry, ACR, and kube-prometheus-stack installed.

## Configure

```bash
cp hack/gantry-benchmark/env.example hack/gantry-benchmark/env.local
```

At minimum, configure:

```bash
export BENCHMARK_CONFIRM_CONTEXT="$(kubectl config current-context)"
export ACR_NAME="<acr-name>"
export ACR_LOGIN_SERVER="${ACR_NAME}.azurecr.io"
export ACR_USERNAME="00000000-0000-0000-0000-000000000000"

export GANTRY_NAMESPACE="gantry-system"
export GANTRY_DAEMONSET="gantry"
export GANTRY_CONFIGMAP="gantry-config"

export BENCHMARK_NAMESPACE="gantry-benchmark"
export BENCHMARK_NODE_COUNT="300"
export BENCHMARK_IMAGE_SIZE_MIB="1024"
export BENCHMARK_IMAGE_LAYERS="1"
export BENCHMARK_IMAGE_PLATFORM="linux/amd64"
export BENCHMARK_WORKLOAD_REPOSITORY="gantry-benchmark-pull"
export BENCHMARK_JOB_TIMEOUT="90m"
export BENCHMARK_ROLLOUT_TIMEOUT="20m"
export BENCHMARK_METRICS_SETTLE_TIME="2m"

export MONITORING_NAMESPACE="monitoring"
export PROMETHEUS_SERVICE="kps-kube-prometheus-stack-prometheus"
export KPS_RELEASE="kps"
export CONTAINER_ENGINE="podman"
```

`BENCHMARK_NODE_COUNT` must equal the eligible Ready node count. ACR metrics
have no repository dimension, so use a dedicated registry or stop unrelated
pull traffic for the duration of the run.

## Checkpoint: cluster

```bash
kubectl get nodes -L kubernetes.azure.com/agentpool,kubernetes.io/os,kubernetes.io/arch
kubectl -n gantry-system get daemonset gantry
kubectl -n monitoring get pods,svc
make -C hack/gantry-benchmark status || true
```

Clean any prior state with the lifecycle command, not manual namespace
deletion:

```bash
make -C hack/gantry-benchmark disable
```

## Enable and preflight

```bash
make -C hack/gantry-benchmark test
make -C hack/gantry-benchmark enable
make -C hack/gantry-benchmark preflight
```

Expected state is `preflight-passed`. No registry proxy, credential Secret, or
Gantry config patch is created.

## Run

Mint a short-lived ACR token without writing it to `env.local`:

```bash
set -a
. hack/gantry-benchmark/env.local
set +a
export ACR_PASSWORD="$(az acr login --name "$ACR_NAME" --expose-token --query accessToken -o tsv)"
make -C hack/gantry-benchmark run
unset ACR_PASSWORD
```

The baseline routes containerd directly to ACR. The cold phase routes
containerd only to local Gantry. The command restores the original per-node
route before it exits, including on failure.

Watch progress separately:

```bash
make -C hack/gantry-benchmark status
kubectl -n gantry-benchmark get jobs,pods
kubectl -n gantry-system get daemonset gantry
```

## Interpret

```bash
run_id=$(find tmp/gantry-benchmark -mindepth 1 -maxdepth 1 -type d -name 'run-*' -printf '%f\n' | sort | tail -1)
cat "tmp/gantry-benchmark/${run_id}/comparison.md"
jq . "tmp/gantry-benchmark/${run_id}/comparison.json"
```

Primary signals:

- Pod start P50/P95/P100 and total completion time.
- ACR total and successful pull counts for each phase window.
- Estimated origin-byte reduction.
- Kubelet pull operations, errors, and average duration.
- Gantry origin pulls, successful origin layer pulls, and peer hits.
- Kubernetes warning markers for HTTP 429/5xx, ACR egress limits, auth,
  timeouts, and connection failures.
- Gantry peer `busy`, `stall`, `notfound`, `unavailable`, and error outcomes.

Treat byte values as estimates and ACR values as registry-wide one-minute
metrics. The Job and Gantry counters are the phase-specific signals. Treat
latency as informational; it does not affect PASS/FAIL.

## Cleanup

```bash
make -C hack/gantry-benchmark disable
```

Verify that the benchmark namespace and lock are gone and Gantry remains fully
Ready. If restoration fails, preserve the namespace and inspect the state plus
`gantry-benchmark-hosts-restore` DaemonSet before taking manual action.