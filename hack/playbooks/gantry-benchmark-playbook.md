# Gantry Benchmark Tool Playbook

This playbook runs the `hack/gantry-benchmark` tool on an existing AKS cluster
with Gantry and ACR already installed. It is written for a dedicated benchmark
cluster, not a shared production cluster.

Run commands from the Unbounded repository root. Stop at each checkpoint and
inspect the output before continuing.

## What The Tool Measures

The benchmark compares two fresh image pulls. The pull path depends on
`BENCHMARK_MODE`.

`BENCHMARK_MODE=proxy` (default):

| Phase | Intended pull path |
|---|---|
| Baseline | containerd -> counting proxy -> ACR |
| Gantry cold | containerd -> local Gantry -> peer or counting proxy -> ACR |

The counting proxy is the measured ACR origin. A valid Gantry-cold run should
show most large blob bytes coming from `client_class="gantry"`, with peer-hit
metrics increasing. If large blob bytes mostly come from
`client_class="containerd"`, the run is not a valid Gantry comparison even if
the Kubernetes Job completes.

`BENCHMARK_MODE=direct`:

| Phase | Intended pull path |
|---|---|
| Baseline | containerd -> ACR |
| Gantry cold | containerd -> local Gantry -> peer or ACR |

There is no proxy. Origin bytes come from two sources:

- Baseline: completed pull pods x total image size (analytic, because Gantry is
  deliberately bypassed).
- Gantry cold: `gantry_origin_bytes_total` measured at Gantry's upstream
  response-body boundary. It includes partial failed transfers and retries.

The routing and peer-health assumptions are enforced as regression gates rather
than footnotes:

- `baseline_bypassed_gantry` - the baseline must show zero Gantry origin pulls
  and zero peer hits. Gantry's node configurator owns
  `/etc/containerd/certs.d/_default/hosts.toml` and routes every registry
  through the local mirror, so the benchmark installs an explicit direct-to-ACR
  host file for the ACR. If that override fails, the baseline silently runs
  through Gantry and the comparison is void.
- `no_origin_fallback` - `p2p_origin_fallback_total` must stay at zero. Fallback
  bytes are counted, but a fallback means peer distribution exhausted and the
  run is not a healthy Gantry comparison.

Direct mode also never patches or rolls the Gantry DaemonSet, and needs no
`BENCHMARK_PROXY_IMAGE`.

Choose direct mode when a single proxy replica cannot sustain the transfer
volume. A 300-node run of an 8 GiB image moves roughly 2.4 TiB in the baseline
phase.

## Prerequisites

The cluster should already have:

- An AKS context selected with `kubectl`.
- A Ready `gantry-system/gantry` DaemonSet on every benchmark node.
- The target ACR listed in Gantry's `upstream_registries` config.
- kube-prometheus-stack installed, including Prometheus, Grafana, and
  PodMonitor CRDs. Nothing in this repository installs it; see
  [Install kube-prometheus-stack](#install-kube-prometheus-stack).
- Containerd configured to read `/etc/containerd/certs.d`.
- Permission to create privileged hostPath DaemonSets and patch the Gantry
  ConfigMap.

The operator machine should have:

- `az`, `kubectl`, `jq`, `helm`, and either `podman` or Docker Buildx.
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

## Install kube-prometheus-stack

The benchmark does not install monitoring, but `preflight` fails without it. It
queries Prometheus for `p2p_dht_health_score` across every Gantry pod and
installs PodMonitors that Prometheus must discover.

The release name matters. `enable` labels its PodMonitors `release=$KPS_RELEASE`
(default `kps`), which is what the stack's default `podMonitorSelector` matches,
and the tool addresses Prometheus at `$PROMETHEUS_SERVICE` (default
`kps-kube-prometheus-stack-prometheus`). Installing under a different release
name requires updating both variables.

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

helm install kps prometheus-community/kube-prometheus-stack \
  --namespace monitoring \
  --create-namespace \
  --set grafana.sidecar.dashboards.enabled=true \
  --wait
```

On a large cluster, pin Prometheus and Grafana to non-benchmark nodes and raise
Prometheus resources before running. See
[Optional Capacity Placement](#optional-capacity-placement).

Checkpoint:

```bash
kubectl -n monitoring get pods
kubectl -n monitoring get svc kps-kube-prometheus-stack-prometheus
kubectl get crd podmonitors.monitoring.coreos.com
```

## Configure Azure-Side Measurement

This section is required when `BENCHMARK_AZURE_TELEMETRY=true`. It makes each
requested metric source-authoritative:

- ACR repository logs: image pull count.
- ACR Private Endpoint `PEBytesIn`: bytes transferred from ACR to AKS.
- AKS `AKSAuditAdmin`: pod startup latency.
- Gantry Prometheus metrics: per-pod peer bytes.

ACR itself does not expose an egress-byte metric. The Private Endpoint is the
supported Azure-side byte meter. The benchmark requires public ACR access to be
disabled so traffic cannot bypass that meter.

The operator machine needs registry data-plane access only while `prepare`
builds and pushes both generated images. After that, disable public access and
run preflight plus both measured phases from the same operator machine; only
AKS nodes transfer image content through the Private Endpoint.

Set the Azure resource variables:

```bash
: "${RESOURCE_GROUP:?Set RESOURCE_GROUP}"
: "${CLUSTER_NAME:?Set CLUSTER_NAME}"
: "${ACR_NAME:?Set ACR_NAME}"

LAW_NAME="${LAW_NAME:-vapa-gantry-bench-law}"
PRIVATE_ENDPOINT_NAME="${PRIVATE_ENDPOINT_NAME:-vapa-gantry-bench-acr-pe}"
PRIVATE_ENDPOINT_SUBNET_NAME="${PRIVATE_ENDPOINT_SUBNET_NAME:-acr-private-endpoints}"
: "${PRIVATE_ENDPOINT_SUBNET_CIDR:?Set a /27 inside the AKS VNet address space}"

AKS_ID=$(az aks show -g "$RESOURCE_GROUP" -n "$CLUSTER_NAME" --query id -o tsv)
ACR_ID=$(az acr show -g "$RESOURCE_GROUP" -n "$ACR_NAME" --query id -o tsv)
NODE_SUBNET_ID=$(az aks show -g "$RESOURCE_GROUP" -n "$CLUSTER_NAME" \
  --query 'agentPoolProfiles[0].vnetSubnetId' -o tsv)

VNET_ID=${NODE_SUBNET_ID%/subnets/*}
VNET_NAME=${VNET_ID##*/}
VNET_RG=$(sed -n 's#^.*/resourceGroups/\([^/]*\)/.*#\1#p' <<<"$VNET_ID")
```

Create Log Analytics and route resource-specific ACR and AKS logs:

```bash
az monitor log-analytics workspace create \
  -g "$RESOURCE_GROUP" \
  -n "$LAW_NAME" \
  -l canadacentral \
  --retention-time 30 \
  --only-show-errors

LAW_ID=$(az monitor log-analytics workspace show \
  -g "$RESOURCE_GROUP" -n "$LAW_NAME" --query id -o tsv)
LAW_CUSTOMER_ID=$(az monitor log-analytics workspace show \
  -g "$RESOURCE_GROUP" -n "$LAW_NAME" --query customerId -o tsv)

az monitor diagnostic-settings create \
  --name vapa-gantry-acr-diag \
  --resource "$ACR_ID" \
  --workspace "$LAW_ID" \
  --export-to-resource-specific true \
  --logs '[{"category":"ContainerRegistryRepositoryEvents","enabled":true},{"category":"ContainerRegistryLoginEvents","enabled":true}]' \
  --metrics '[{"category":"AllMetrics","enabled":true}]' \
  --only-show-errors

az monitor diagnostic-settings create \
  --name vapa-gantry-aks-diag \
  --resource "$AKS_ID" \
  --workspace "$LAW_ID" \
  --export-to-resource-specific true \
  --logs '[{"category":"kube-audit-admin","enabled":true},{"category":"kube-apiserver","enabled":true},{"category":"kube-scheduler","enabled":true}]' \
  --metrics '[{"category":"AllMetrics","enabled":true}]' \
  --only-show-errors
```

Create a dedicated Private Endpoint subnet, endpoint, and private DNS:

```bash
if ! az network vnet subnet show \
  -g "$VNET_RG" --vnet-name "$VNET_NAME" \
  -n "$PRIVATE_ENDPOINT_SUBNET_NAME" --output none 2>/dev/null; then
  az network vnet subnet create \
    -g "$VNET_RG" \
    --vnet-name "$VNET_NAME" \
    -n "$PRIVATE_ENDPOINT_SUBNET_NAME" \
    --address-prefixes "$PRIVATE_ENDPOINT_SUBNET_CIDR" \
    --disable-private-endpoint-network-policies true \
    --only-show-errors
fi

PE_SUBNET_ID=$(az network vnet subnet show \
  -g "$VNET_RG" --vnet-name "$VNET_NAME" \
  -n "$PRIVATE_ENDPOINT_SUBNET_NAME" --query id -o tsv)

az network private-dns zone create \
  -g "$RESOURCE_GROUP" -n privatelink.azurecr.io --only-show-errors

DNS_ZONE_ID=$(az network private-dns zone show \
  -g "$RESOURCE_GROUP" -n privatelink.azurecr.io --query id -o tsv)

az network private-dns link vnet create \
  -g "$RESOURCE_GROUP" \
  -n vapa-gantry-acr-dns-link \
  -z privatelink.azurecr.io \
  -v "$VNET_ID" \
  -e false \
  --only-show-errors

az network private-endpoint create \
  -g "$RESOURCE_GROUP" \
  -n "$PRIVATE_ENDPOINT_NAME" \
  --subnet "$PE_SUBNET_ID" \
  --private-connection-resource-id "$ACR_ID" \
  --group-ids registry \
  --connection-name vapa-gantry-acr \
  --only-show-errors

az network private-endpoint dns-zone-group create \
  -g "$RESOURCE_GROUP" \
  --endpoint-name "$PRIVATE_ENDPOINT_NAME" \
  -n acr \
  --private-dns-zone "$DNS_ZONE_ID" \
  --zone-name privatelink.azurecr.io \
  --only-show-errors

PRIVATE_ENDPOINT_ID=$(az network private-endpoint show \
  -g "$RESOURCE_GROUP" -n "$PRIVATE_ENDPOINT_NAME" --query id -o tsv)
```

From an AKS pod, confirm that both the login and data endpoints resolve
privately. Run `prepare` before disabling public ACR access:

```bash
az acr show-endpoints -n "$ACR_NAME" -o table
getent ahostsv4 "${ACR_NAME}.azurecr.io"
getent ahostsv4 "${ACR_NAME}.canadacentral.data.azurecr.io"

az acr update -g "$RESOURCE_GROUP" -n "$ACR_NAME" \
  --public-network-enabled false --only-show-errors
```

Preflight independently verifies the resource binding, approved connection,
disabled public access, `PEBytesIn`, and both Log Analytics tables.

## Configure The Environment

Create local config:

```bash
cp hack/gantry-benchmark/env.example hack/gantry-benchmark/env.local
```

Edit `hack/gantry-benchmark/env.local`. Keep secrets out of git. At minimum,
set:

```bash
export BENCHMARK_CONFIRM_CONTEXT="$(kubectl config current-context)"

# proxy (default) routes both phases through the counting proxy.
# direct removes the proxy and uses Gantry's direct origin-byte counter.
export BENCHMARK_MODE="direct"

export ACR_NAME="<acr-name>"
export ACR_LOGIN_SERVER="<acr-name>.azurecr.io"
export ACR_USERNAME="00000000-0000-0000-0000-000000000000"
# Proxy mode only.
export BENCHMARK_PROXY_IMAGE="${ACR_LOGIN_SERVER}/acr-origin-proxy:benchmark-$(date -u +%Y%m%d%H%M%S)"

export BENCHMARK_AZURE_TELEMETRY="true"
export AZURE_LOG_ANALYTICS_WORKSPACE_ID="$LAW_CUSTOMER_ID"
export AZURE_ACR_RESOURCE_ID="$ACR_ID"
export AZURE_AKS_RESOURCE_ID="$AKS_ID"
export AZURE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID="$PRIVATE_ENDPOINT_ID"
export BENCHMARK_TELEMETRY_TIMEOUT="15m"
export BENCHMARK_TELEMETRY_POLL_INTERVAL="15s"

export GANTRY_NAMESPACE="gantry-system"
export GANTRY_DAEMONSET="gantry"
export GANTRY_CONFIGMAP="gantry-config"

export BENCHMARK_NAMESPACE="gantry-benchmark"
export BENCHMARK_NODE_COUNT="300"
export BENCHMARK_IMAGE_SIZE_MIB="8192"
export BENCHMARK_IMAGE_LAYERS="8"
export BENCHMARK_IMAGE_PLATFORM="linux/amd64"
export BENCHMARK_WORKLOAD_REPOSITORY="gantry-benchmark-pull"
export BENCHMARK_JOB_TIMEOUT="180m"
export BENCHMARK_ROLLOUT_TIMEOUT="30m"
export BENCHMARK_MINIMUM_BYTE_REDUCTION="0.90"
export BENCHMARK_MAXIMUM_LATENCY_RATIO="1.0"

export MONITORING_NAMESPACE="monitoring"
export PROMETHEUS_SERVICE="kps-kube-prometheus-stack-prometheus"
export KPS_RELEASE="kps"
export CONTAINER_ENGINE="podman"
```

Direct mode supports uneven layer sizes because Gantry origin bytes are measured
directly rather than reconstructed from pull counts.

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

For admin-disabled ACR auth, inject the refresh token only when building or
pushing the proxy and when running `prepare`:

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

Proxy mode only. Direct mode installs no proxy, so skip straight to
[Enable Instrumentation](#enable-instrumentation).

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
ConfigMap, and Gantry-namespace lock. It does not change node routing or Gantry
routing.

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

## Prepare Workload Images

While public ACR access is still enabled, build and push both fresh phase
images and bind their digest references to benchmark state:

```bash
set -a
. hack/gantry-benchmark/env.local
set +a

acr_refresh_token=$(az acr login --name "$ACR_NAME" --expose-token --query accessToken -o tsv)
export ACR_PASSWORD="$acr_refresh_token"

BENCHMARK_CONFIRM_CONTEXT=$(kubectl config current-context) make -C hack/gantry-benchmark prepare

unset ACR_PASSWORD acr_refresh_token
```

Checkpoint:

```bash
make -C hack/gantry-benchmark status
```

Expected state is `images-prepared`. For Azure telemetry, now disable public
access so all measured image traffic traverses the Private Endpoint:

```bash
az acr update -g "$RESOURCE_GROUP" -n "$ACR_NAME" \
  --public-network-enabled false --only-show-errors
```

## Preflight

Run preflight before any routing changes:

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
BENCHMARK_CONFIRM_CONTEXT=$(kubectl config current-context) make -C hack/gantry-benchmark run
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

## ACR Throttling In Direct Mode

Proxy mode funnels every pull through one client, which incidentally shields ACR
from the node fan-out. Direct mode removes that shield: the baseline phase is
300 nodes pulling from ACR at once.

ACR Premium allows roughly 20,000 DataplaneRead requests per minute per
registry, but only 10,000 per identity per registry. Every AKS node authenticates
with the same kubelet managed identity, so all 300 nodes share a single
per-identity bucket. Microsoft also documents that a sudden ramp from a low
baseline can temporarily yield reduced throughput while the registry scales out,
returning HTTP 429 with `Retry-After`.

Practical consequences:

- Raise `BENCHMARK_JOB_TIMEOUT` well above the 90m default. The 180m in this
  playbook is a starting point, not a measured value.
- Expect 429s in the baseline. Containerd retries them, which inflates baseline
  latency. That is real-world ACR behaviour at cold start, but record it so the
  comparison is read correctly.
- Sample containerd logs on a node during the baseline to capture the evidence:

```bash
kubectl -n gantry-benchmark exec "$node" -- \
  sh -c 'grep -ci "429\|too many requests" /host-var-log/containerd.log || true'
```

If throttling dominates the run, consider ACR dedicated data endpoints,
geo-replication, or an Azure Support limit increase before drawing conclusions
from the latency numbers.

## Validate Gantry Cold Routing

Do not trust pod pull time alone. Validate the actual route.

### Direct mode

Confirm the baseline host file pointed containerd straight at ACR rather than
falling through to Gantry's `_default` mirror entry. Check this on a node during
or just after the baseline phase:

```bash
node=$(kubectl -n gantry-benchmark get pods -l app.kubernetes.io/name=gantry-benchmark-hosts \
  -o jsonpath='{.items[0].metadata.name}')

kubectl -n gantry-benchmark exec "$node" -- \
  cat "/host-certs/${ACR_LOGIN_SERVER}/hosts.toml"
```

During the baseline this must contain `server = "https://<acr>.azurecr.io"` and
must not mention `127.0.0.1:5000`. During the Gantry phase it must contain only
the `[host."http://127.0.0.1:5000"]` block with no `server =` line.

Confirm Gantry was never patched. Direct mode leaves the ConfigMap untouched, so
this hash must match before and after the run:

```bash
kubectl -n gantry-system get configmap gantry-config \
  -o jsonpath='{.data.config\.yaml}' | sha256sum
```

The two direct-mode routing/health assumptions are enforced automatically. After
`run`, check them in the comparison:

```bash
run_id=$(jq -r '.run_id' tmp/gantry-benchmark/*/state.json | tail -1)
jq '.checks | {baseline_bypassed_gantry, no_origin_fallback}' \
  "tmp/gantry-benchmark/${run_id}/comparison.json"
```

Both must report `"passed": true`. A failed `baseline_bypassed_gantry` means the
baseline ran through Gantry and the comparison is void. A failed
`no_origin_fallback` means peer distribution exhausted. Fallback bytes remain
included in `gantry_origin_bytes_total`, but the run is not a healthy Gantry
comparison.

### Proxy mode

Confirm Gantry is patched to the proxy during the Gantry phase:

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

### Both modes

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

If `run` returns nonzero but state is `run-failed-restored`, routing was
restored. Run `preflight` again before another `run`, or run `disable` for a
clean start.

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
