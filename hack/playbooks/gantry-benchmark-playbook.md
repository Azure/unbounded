# Gantry Benchmark Tool Playbook

This playbook runs the `hack/gantry-benchmark` tool on an existing AKS cluster
with Gantry and ACR already installed. It is written for a dedicated benchmark
cluster, not a shared production cluster.

Run commands from the Unbounded repository root. Stop at each checkpoint and
inspect the output before continuing.

## What The Tool Measures

The benchmark compares two fresh image pulls:

| Phase | Intended pull path |
|---|---|
| Baseline | containerd -> counting proxy -> ACR |
| Gantry cold | containerd -> local Gantry -> peer or counting proxy -> ACR |

The counting proxy is the measured ACR origin. A valid Gantry-cold run should
show most large blob bytes coming from `client_class="gantry"`, with peer-hit
metrics increasing. If large blob bytes mostly come from
`client_class="containerd"`, the Gantry-cold route did not flow through Gantry
as intended and the run is not a valid comparison even if the Kubernetes Job
completes.

## Prerequisites

The cluster should already have:

- An AKS context selected with `kubectl`.
- A Ready `gantry-system/gantry` DaemonSet on every benchmark node.
- The target ACR listed in Gantry's `upstream_registries` config.
- kube-prometheus-stack installed, including Prometheus, Grafana, and
  PodMonitor CRDs.
- Containerd configured to read `/etc/containerd/certs.d`.
- Permission to create privileged hostPath DaemonSets and patch the Gantry
  ConfigMap.

The operator machine should have:

- `az`, `kubectl`, `jq`, and either `podman` or Docker Buildx.
- ACR push permission. For ACR admin-disabled clusters, use
  `az acr login --expose-token` and the all-zero username.

## Safety Checks

Confirm the target cluster:

```bash
kubectl config current-context
kubectl get nodes -L kubernetes.azure.com/agentpool,kubernetes.io/os,kubernetes.io/arch
kubectl -n gantry-system get daemonset gantry
kubectl -n monitoring get pods,svc
```

Check for existing benchmark state:

```bash
make -C hack/gantry-benchmark status || true
kubectl -n gantry-system get configmap gantry-benchmark-lock -o yaml 2>/dev/null || true
kubectl -n gantry-benchmark get configmap gantry-benchmark-state -o yaml 2>/dev/null || true
```

If an old benchmark namespace or lock exists, clean it up before starting a new
run:

```bash
BENCHMARK_CONFIRM_CONTEXT=$(kubectl config current-context) make -C hack/gantry-benchmark disable
```

Do not manually delete benchmark resources unless you are deliberately doing
failure recovery and have inspected the current routing state.

## Configure The Environment

Create local config:

```bash
cp hack/gantry-benchmark/env.example hack/gantry-benchmark/env.local
```

Edit `hack/gantry-benchmark/env.local`. Keep secrets out of git. At minimum,
set:

```bash
export BENCHMARK_CONFIRM_CONTEXT="$(kubectl config current-context)"

export ACR_NAME="<acr-name>"
export ACR_LOGIN_SERVER="<acr-name>.azurecr.io"
export ACR_USERNAME="00000000-0000-0000-0000-000000000000"
export BENCHMARK_PROXY_IMAGE="${ACR_LOGIN_SERVER}/acr-origin-proxy:benchmark-$(date -u +%Y%m%d%H%M%S)"

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
export BENCHMARK_MINIMUM_BYTE_REDUCTION="0.90"
export BENCHMARK_MAXIMUM_LATENCY_RATIO="1.0"

export MONITORING_NAMESPACE="monitoring"
export PROMETHEUS_SERVICE="kps-kube-prometheus-stack-prometheus"
export KPS_RELEASE="kps"
export CONTAINER_ENGINE="podman"
```

Set `BENCHMARK_NODE_COUNT` to the exact number of eligible nodes the benchmark
will target. If you add dedicated proxy or monitoring nodes and the benchmark
tool still sees them as eligible, either include them deliberately or adjust
the node targeting before `enable`. The persisted benchmark state records the
node count at enable time; changing `env.local` later does not rewrite existing
state.

## Image Shape (single vs multi-layer)

`BENCHMARK_IMAGE_SIZE_MIB` and `BENCHMARK_IMAGE_LAYERS` together define the
workload image. The tool splits the total payload across `BENCHMARK_IMAGE_LAYERS`
separate `COPY` layers (each a fresh random blob per run, so every layer digest
is unique and the pull is genuinely cold). The last layer absorbs any remainder.

| Setting | Models | Notes |
|---|---|---|
| `IMAGE_LAYERS=1` | One giant layer | Pathological worst case. Whole-blob store-and-forward means no cross-layer pipelining, so the cascade pays the full per-layer `log(N)` fan-out penalty. |
| `IMAGE_LAYERS=8`, `IMAGE_SIZE_MIB=8192` | 8 GiB / 8 x 1 GiB | Representative multi-layer image. The 8 layers form 8 independent per-digest cascades that pipeline across each other, so throughput per node is higher than the single-layer case. |

Constraints: `IMAGE_LAYERS` must be `>= 1` and `<= IMAGE_SIZE_MIB`. Use a
multi-layer shape when validating the target 20-40 GiB many-layer workload;
use the single 1 GiB layer only to measure the worst case.

For admin-disabled ACR auth, inject the refresh token only when building,
pushing, or running:

```bash
set -a
. hack/gantry-benchmark/env.local
set +a

acr_refresh_token=$(az acr login --name "$ACR_NAME" --expose-token --query accessToken -o tsv)
export ACR_PASSWORD="$acr_refresh_token"
```

Unset it after the command that needs it:

```bash
unset ACR_PASSWORD acr_refresh_token
```

## Optional Capacity Placement

For large runs, keep the counting proxy and Prometheus away from small worker
nodes. If using a dedicated AKS pool such as `bench16`, label and taint it when
creating the pool, for example:

```bash
gantry-benchmark-proxy=true
gantry-benchmark-proxy=true:NoSchedule
```

After `enable`, place the proxy there:

```bash
kubectl -n gantry-benchmark patch deployment acr-origin-proxy --type merge -p '{
  "spec": {
    "template": {
      "spec": {
        "nodeSelector": {"gantry-benchmark-proxy": "true"},
        "tolerations": [
          {"key": "gantry-benchmark-proxy", "operator": "Equal", "value": "true", "effect": "NoSchedule"}
        ]
      }
    }
  }
}'

kubectl -n gantry-benchmark patch deployment acr-origin-proxy --type json -p '[
  {"op":"replace","path":"/spec/template/spec/containers/0/resources","value":{"requests":{"cpu":"8","memory":"8Gi"},"limits":{"cpu":"8","memory":"8Gi"}}}
]'

kubectl -n gantry-benchmark rollout status deployment/acr-origin-proxy --timeout=10m
```

If Prometheus also needs to move, prefer Helm values for durable configuration.
For temporary lab runs, patch the Prometheus custom resource only after
confirming the release object name:

```bash
kubectl -n monitoring get prometheus

kubectl -n monitoring patch prometheus kps-kube-prometheus-stack-prometheus --type merge -p '{
  "spec": {
    "nodeSelector": {"gantry-benchmark-proxy": "true"},
    "tolerations": [
      {"key": "gantry-benchmark-proxy", "operator": "Equal", "value": "true", "effect": "NoSchedule"}
    ]
  }
}'

kubectl -n monitoring get pods -l app.kubernetes.io/name=prometheus -o wide
```

## Build And Push The Proxy

Run the focused tests first:

```bash
make -C hack/gantry-benchmark test
```

Build and push the proxy image:

```bash
set -a
. hack/gantry-benchmark/env.local
set +a

acr_refresh_token=$(az acr login --name "$ACR_NAME" --expose-token --query accessToken -o tsv)
export ACR_PASSWORD="$acr_refresh_token"

make -C hack/gantry-benchmark proxy-image
make -C hack/gantry-benchmark proxy-push

unset ACR_PASSWORD acr_refresh_token
```

## Enable Instrumentation

Enable creates the benchmark namespace, proxy, monitoring objects, state
ConfigMap, and Gantry-namespace lock. It does not change node routing. It does
patch the matching Gantry upstream registry entry to point at the counting proxy
and rolls the Gantry DaemonSet during enable, so the DHT can reconverge before
the measured run. `disable` restores the original Gantry ConfigMap.

```bash
BENCHMARK_CONFIRM_CONTEXT=$(kubectl config current-context) make -C hack/gantry-benchmark enable
```

Checkpoint:

```bash
kubectl -n gantry-benchmark get pods,svc,configmap
kubectl -n gantry-system get configmap gantry-benchmark-lock -o jsonpath='{.data.run-id}{"\n"}'
make -C hack/gantry-benchmark status
```

Expected state is `enabled`.

## Preflight

Run preflight before any per-node containerd routing changes:

```bash
BENCHMARK_CONFIRM_CONTEXT=$(kubectl config current-context) make -C hack/gantry-benchmark preflight
```

Preflight must pass before `run`. It checks proxy-to-ACR smoke requests,
node-to-proxy reachability, Gantry metrics, and proxy metrics.

Checkpoint:

```bash
make -C hack/gantry-benchmark status
kubectl -n gantry-benchmark get daemonset acr-proxy-node-reachability 2>/dev/null || true
```

Expected state is `preflight-passed`.

## Run The Benchmark

Start the run:

```bash
set -a
. hack/gantry-benchmark/env.local
set +a

acr_refresh_token=$(az acr login --name "$ACR_NAME" --expose-token --query accessToken -o tsv)
export ACR_PASSWORD="$acr_refresh_token"

BENCHMARK_CONFIRM_CONTEXT=$(kubectl config current-context) make -C hack/gantry-benchmark run
status=$?

unset ACR_PASSWORD acr_refresh_token
exit "$status"
```

Useful watch commands in another terminal:

```bash
kubectl -n gantry-benchmark get configmap gantry-benchmark-state -o jsonpath='{.data.state\.json}' | jq '{status,run_id,node_count}'
kubectl -n gantry-benchmark get jobs -l app.kubernetes.io/part-of=gantry-benchmark
kubectl -n gantry-system get daemonset gantry
kubectl get nodes -o json | jq -r '.items[] | select(any(.status.conditions[]; (.type=="MemoryPressure" or .type=="DiskPressure" or .type=="PIDPressure") and .status=="True")) | [.metadata.name, (.metadata.labels["kubernetes.azure.com/agentpool"] // "-"), ([.status.conditions[] | select((.type=="MemoryPressure" or .type=="DiskPressure" or .type=="PIDPressure") and .status=="True") | .type] | join(","))] | @tsv'
```

During baseline, expect a Job named like:

```text
gantry-benchmark-baseline-<run-id>
```

During Gantry cold, expect:

```text
gantry-benchmark-gantry-cold-<run-id>
```

`run` restores the per-node ACR-specific `hosts.toml` routing before it exits,
including when a phase or regression gate fails. It leaves the benchmark
namespace, proxy, dashboard, artifacts, and Gantry proxy patch in place for
inspection until `disable` is run.

## Validate Gantry Cold Routing

Do not trust pod pull time alone. Validate the actual route.

Confirm Gantry remains patched to the proxy after `enable` and through the run:

```bash
kubectl -n gantry-system get configmap gantry-config -o jsonpath='{.data.config\.yaml}' \
  | grep -n -E 'gantryauth|acr-origin-proxy|endpoint|ns_alias' \
  | sed -n '1,120p'
```

Confirm benchmark host routing DaemonSet is installed and ready:

```bash
kubectl -n gantry-benchmark get daemonset gantry-benchmark-hosts
```

Inspect proxy traffic by phase and client class:

```bash
kubectl get --raw /api/v1/namespaces/gantry-benchmark/services/http:acr-origin-proxy:9090/proxy/debug/summary \
  | jq '.totals.by_phase.gantry_cold.by_client_class'
```

For a valid Gantry-cold result, large blob bytes should be mostly under the
`gantry` client class. If most bytes are under `containerd`, the run used the
direct fallback path heavily and should not be used for the comparison.

Check peer activity:

```bash
query='sum by (controller_revision_hash)(p2p_peer_fetch_total{namespace="gantry-system",gantry_benchmark="true",outcome="hit"})'
kubectl get --raw "/api/v1/namespaces/monitoring/services/http:kps-kube-prometheus-stack-prometheus:9090/proxy/api/v1/query?query=$(printf '%s' "$query" | jq -sRr @uri)" \
  | jq '.data.result'
```

Peer hits should increase during the Gantry-cold phase.

## Inspect Results

After `run` exits, inspect state and artifacts:

```bash
make -C hack/gantry-benchmark status
run_id=$(jq -r '.run_id' tmp/gantry-benchmark/*/state.json | tail -1)
cat "tmp/gantry-benchmark/${run_id}/comparison.md"
jq . "tmp/gantry-benchmark/${run_id}/comparison.json"
```

The comparison includes upstream bytes, request counts, Gantry origin pulls,
peer fetch hits, and pod latency summaries.

Open Grafana if needed:

```bash
kubectl -n monitoring port-forward service/kps-grafana 3000:80
```

Use dashboard `Gantry ACR Benchmark` and select the run ID.

## Cleanup

Use the tool cleanup path first:

```bash
BENCHMARK_CONFIRM_CONTEXT=$(kubectl config current-context) make -C hack/gantry-benchmark disable
```

Cleanup restores node routing, restores Gantry config, verifies Gantry, removes
the benchmark namespace and dashboard, and releases the lock.

Checkpoint after cleanup:

```bash
kubectl get namespace gantry-benchmark 2>/dev/null || true
kubectl -n gantry-system get configmap gantry-benchmark-lock 2>/dev/null || true
kubectl -n gantry-system get daemonset gantry
kubectl -n gantry-system get configmap gantry-config -o jsonpath='{.data.config\.yaml}' \
  | grep -n -E 'gantryauth|acr-origin-proxy|endpoint|ns_alias' \
  | sed -n '1,120p'
```

Expected after cleanup:

- `gantry-benchmark` namespace is gone.
- `gantry-system/gantry-benchmark-lock` is gone.
- Gantry DaemonSet is fully Ready.
- Gantry config points back to the real ACR endpoint, not
  `acr-origin-proxy`.

## Recovery Notes

If `run` returns nonzero but state is `run-failed-restored`, per-node routing was
restored and Gantry remains patched to the proxy for inspection. Run `disable`
for a clean start before enabling another benchmark.

If state is `restore-failed` or `disabling`, inspect before making changes:

```bash
kubectl -n gantry-benchmark get configmap gantry-benchmark-state -o jsonpath='{.data.state\.json}' | jq .
kubectl -n gantry-benchmark get daemonsets,pods,jobs -l app.kubernetes.io/part-of=gantry-benchmark -o wide
kubectl -n gantry-system get configmap gantry-config -o jsonpath='{.data.config\.yaml}' \
  | grep -n -E 'gantryauth|acr-origin-proxy|endpoint|ns_alias' \
  | sed -n '1,120p'
```

If a restore DaemonSet is stuck on one node, identify the node and memory
pressure before acting:

```bash
kubectl -n gantry-benchmark get pods -l app.kubernetes.io/name=gantry-benchmark-hosts-restore -o wide
node=<node-name>
kubectl describe node "$node"
kubectl get pods -A --field-selector spec.nodeName="$node" -o wide
kubectl top pod -A --containers --sort-by=memory 2>/dev/null | sed -n '1,40p'
```

Do not patch DaemonSets, force delete pods, or delete the namespace until the
current routing state is understood. If manual recovery is needed, record:

- Current benchmark state.
- Current Gantry ConfigMap endpoint for the target ACR.
- Whether `gantry-benchmark-hosts` or `gantry-benchmark-hosts-restore` is
  installed and ready.
- Which node is blocking and why.
