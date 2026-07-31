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

`BENCHMARK_MODE=direct` (the dual-ACR benchmark):

| Phase | Intended pull path |
|---|---|
| Baseline | containerd -> baseline ACR |
| Gantry cold | containerd -> local Gantry -> peer or Gantry ACR |

There is no proxy. `prepare` generates one payload set and pushes the same
repository, tag, payload bytes, size, and layer count to both ACRs. Payload
layers use phase-specific destination paths so OCI digests differ and the
second phase cannot reuse the first phase's containerd content cache. Origin
bytes come from two sources:

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
  host file for the baseline ACR. If that override fails, the baseline silently runs
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
- A dedicated Gantry ACR listed in Gantry's `upstream_registries` config, and a
  different baseline ACR that Gantry does not use as its origin.
- kube-prometheus-stack installed, including Prometheus, Grafana, and
  PodMonitor CRDs. Nothing in this repository installs it; see
  [Install kube-prometheus-stack](#install-kube-prometheus-stack).
- Containerd configured to read `/etc/containerd/certs.d`.
- Permission to create privileged hostPath DaemonSets and patch the Gantry
  ConfigMap.

The admin workstation needs only:

- Azure CLI permission to create the operator VM/network resources and assign
  roles.
- Git and Make for invoking the repository provisioning target.

The private operator VM installs and owns `az`, `kubectl`, `jq`, Go, Podman,
the repository, kubeconfig, image builds, lifecycle commands, telemetry
queries, logs, and result artifacts.

## Safety Checks

Bootstrap retrieves the kubeconfig and verifies cluster-admin authorization.
`enable` and `preflight` then confirm the exact context, eligible nodes, Gantry
DaemonSet, monitoring, and upstream registry from the VM.

For direct mode, the matching upstream entry must be named
`$GANTRY_ACR_LOGIN_SERVER` and its endpoint must be
`https://$GANTRY_ACR_LOGIN_SERVER`. `enable` rejects any other binding. The
baseline ACR is reached only through the benchmark's explicit direct
containerd configuration.

The service refuses to start when an old benchmark state exists. Its cleanup
trap invokes `disable` when either state or the Gantry-namespace lock remains.
Do not manually delete benchmark resources unless VM logs show that normal
restoration cannot proceed.

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

Both ACRs remain private-only. The operator VM resolves their login and data
endpoints through the AKS VNet, builds and pushes both images over Private Link,
and runs preflight plus both measured phases from that same VM.

Set the Azure resource variables:

```bash
: "${RESOURCE_GROUP:?Set RESOURCE_GROUP}"
: "${CLUSTER_NAME:?Set CLUSTER_NAME}"
: "${BASELINE_ACR_NAME:?Set globally unique BASELINE_ACR_NAME, for example teamgantrybaseline}"
: "${GANTRY_ACR_NAME:?Set globally unique GANTRY_ACR_NAME, for example teamgantryp2p}"

LAW_NAME="${LAW_NAME:-vapa-gantry-bench-law}"
BASELINE_PRIVATE_ENDPOINT_NAME="${BASELINE_PRIVATE_ENDPOINT_NAME:-gantry-benchmark-baseline-acr-pe}"
GANTRY_PRIVATE_ENDPOINT_NAME="${GANTRY_PRIVATE_ENDPOINT_NAME:-gantry-benchmark-gantry-acr-pe}"
PRIVATE_ENDPOINT_SUBNET_NAME="${PRIVATE_ENDPOINT_SUBNET_NAME:-acr-private-endpoints}"
: "${PRIVATE_ENDPOINT_SUBNET_CIDR:?Set a /27 inside the AKS VNet address space}"

if ! az acr show -g "$RESOURCE_GROUP" -n "$BASELINE_ACR_NAME" --output none 2>/dev/null; then
  az acr create -g "$RESOURCE_GROUP" -n "$BASELINE_ACR_NAME" \
    --sku Premium --only-show-errors
fi
if ! az acr show -g "$RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" --output none 2>/dev/null; then
  az acr create -g "$RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" \
    --sku Premium --only-show-errors
fi

AKS_ID=$(az aks show -g "$RESOURCE_GROUP" -n "$CLUSTER_NAME" --query id -o tsv)
BASELINE_ACR_ID=$(az acr show -g "$RESOURCE_GROUP" -n "$BASELINE_ACR_NAME" --query id -o tsv)
GANTRY_ACR_ID=$(az acr show -g "$RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" --query id -o tsv)
BASELINE_ACR_LOGIN_SERVER=$(az acr show -g "$RESOURCE_GROUP" -n "$BASELINE_ACR_NAME" --query loginServer -o tsv)
GANTRY_ACR_LOGIN_SERVER=$(az acr show -g "$RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" --query loginServer -o tsv)
KUBELET_OBJECT_ID=$(az aks show -g "$RESOURCE_GROUP" -n "$CLUSTER_NAME" \
  --query identityProfile.kubeletidentity.objectId -o tsv)

for acr_id in "$BASELINE_ACR_ID" "$GANTRY_ACR_ID"; do
  az role assignment create \
    --assignee-object-id "$KUBELET_OBJECT_ID" \
    --assignee-principal-type ServicePrincipal \
    --role AcrPull \
    --scope "$acr_id" \
    --only-show-errors
done

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
  --name gantry-benchmark-baseline-acr-diag \
  --resource "$BASELINE_ACR_ID" \
  --workspace "$LAW_ID" \
  --export-to-resource-specific true \
  --logs '[{"category":"ContainerRegistryRepositoryEvents","enabled":true},{"category":"ContainerRegistryLoginEvents","enabled":true}]' \
  --metrics '[{"category":"AllMetrics","enabled":true}]' \
  --only-show-errors

az monitor diagnostic-settings create \
  --name gantry-benchmark-gantry-acr-diag \
  --resource "$GANTRY_ACR_ID" \
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
  -n "$BASELINE_PRIVATE_ENDPOINT_NAME" \
  --subnet "$PE_SUBNET_ID" \
  --private-connection-resource-id "$BASELINE_ACR_ID" \
  --group-ids registry \
  --connection-name gantry-benchmark-baseline-acr \
  --only-show-errors

az network private-endpoint dns-zone-group create \
  -g "$RESOURCE_GROUP" \
  --endpoint-name "$BASELINE_PRIVATE_ENDPOINT_NAME" \
  -n acr \
  --private-dns-zone "$DNS_ZONE_ID" \
  --zone-name privatelink.azurecr.io \
  --only-show-errors

az network private-endpoint create \
  -g "$RESOURCE_GROUP" \
  -n "$GANTRY_PRIVATE_ENDPOINT_NAME" \
  --subnet "$PE_SUBNET_ID" \
  --private-connection-resource-id "$GANTRY_ACR_ID" \
  --group-ids registry \
  --connection-name gantry-benchmark-gantry-acr \
  --only-show-errors

az network private-endpoint dns-zone-group create \
  -g "$RESOURCE_GROUP" \
  --endpoint-name "$GANTRY_PRIVATE_ENDPOINT_NAME" \
  -n acr \
  --private-dns-zone "$DNS_ZONE_ID" \
  --zone-name privatelink.azurecr.io \
  --only-show-errors

BASELINE_PRIVATE_ENDPOINT_ID=$(az network private-endpoint show \
  -g "$RESOURCE_GROUP" -n "$BASELINE_PRIVATE_ENDPOINT_NAME" --query id -o tsv)
GANTRY_PRIVATE_ENDPOINT_ID=$(az network private-endpoint show \
  -g "$RESOURCE_GROUP" -n "$GANTRY_PRIVATE_ENDPOINT_NAME" --query id -o tsv)
```

From an AKS pod, confirm that both ACR login and regional data endpoints resolve
privately. Run `prepare` before disabling public ACR access:

```bash
for acr_name in "$BASELINE_ACR_NAME" "$GANTRY_ACR_NAME"; do
  az acr show-endpoints -n "$acr_name" -o table
  getent ahostsv4 "${acr_name}.azurecr.io"
  getent ahostsv4 "${acr_name}.canadacentral.data.azurecr.io"
done
```

Preflight independently verifies both resource bindings, both approved
connections, disabled public access on both ACRs, `PEBytesIn` on both Private
Endpoints, and both Log Analytics tables.

During collection, each phase also requires `PEBytesIn` to meet a minimum
derived from the transferred workload. Non-null endpoint points containing only
control traffic are incomplete and invalidate Azure promotion.

## Provision The Private Operator VM

The provisioning script is idempotent. It creates a private subnet, an NSG with
no inbound rules, a NAT gateway for outbound dependencies, a private VM with a
system-assigned identity, and the required role assignments. It then uses Azure
Run Command to install tools, clone the private benchmark branch, write a
VM-only configuration, fetch an admin kubeconfig, and install the systemd unit.

```bash
export AZURE_SUBSCRIPTION_ID="<subscription-id>"
export AZURE_RESOURCE_GROUP="$RESOURCE_GROUP"
export AZURE_AKS_CLUSTER_NAME="$CLUSTER_NAME"
export BASELINE_ACR_NAME="$BASELINE_ACR_NAME"
export GANTRY_ACR_NAME="$GANTRY_ACR_NAME"
export AZURE_LOG_ANALYTICS_WORKSPACE_NAME="$LAW_NAME"
export AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID="$BASELINE_PRIVATE_ENDPOINT_ID"
export AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID="$GANTRY_PRIVATE_ENDPOINT_ID"
export OPERATOR_VNET_RESOURCE_GROUP="$VNET_RG"
export OPERATOR_VNET_NAME="$VNET_NAME"

# Directly select a known size; do not enumerate all regional SKUs.
export OPERATOR_VM_SIZE="Standard_D8ds_v5"
export OPERATOR_OS_DISK_GB="512"
export OPERATOR_SUBNET_CIDR="10.236.0.0/24"

export BENCHMARK_REPO_BRANCH="private/gantry-benchmark-hardening"
export BENCHMARK_NODE_COUNT="300"
export BENCHMARK_IMAGE_SIZE_MIB="8192"
export BENCHMARK_IMAGE_LAYERS="8"
export BENCHMARK_AZURE_TELEMETRY="true"
export BENCHMARK_MINIMUM_BYTE_REDUCTION="0.90"
export BENCHMARK_MAXIMUM_LATENCY_RATIO="1.0"

make -C hack/gantry-benchmark operator-vm-provision
```

The VM identity receives:

- `AcrPush` on each ACR.
- `Azure Kubernetes Service Cluster Admin Role` on the AKS resource, used only
  to retrieve its VM-local kubeconfig.
- `Reader` on the benchmark resource group for ACR, AKS, Private Endpoint, and
  metric reads.
- `Log Analytics Reader` on the workspace.

The VM has no public IP. A 512 GiB OS disk accommodates the repository, Go and
Podman caches, and two phase images for large benchmark payloads. Increase it
before provisioning for repeated 20-40 GiB runs.

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

During `prepare`, the VM exchanges its managed-identity AAD token for one
short-lived ACR refresh token per registry. Tokens are exported only to that
process, removed immediately afterward, and never stored in the VM config,
Kubernetes state, or result artifacts.

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

## Run The Full Lifecycle From The VM

The service performs `enable`, managed-identity token exchange, `prepare`,
`preflight`, `run`, artifact preservation, local image pruning, and `disable`.
Both ACRs remain private-only throughout.

```bash
OPERATOR_VM_NAME="${OPERATOR_VM_NAME:-gantry-benchmark-operator}"

az vm run-command invoke \
  -g "$RESOURCE_GROUP" \
  -n "$OPERATOR_VM_NAME" \
  --command-id RunShellScript \
  --scripts 'systemctl start --no-block gantry-benchmark-operator.service'
```

Inspect status, logs, and the latest report without logging into the VM:

```bash
az vm run-command invoke \
  -g "$RESOURCE_GROUP" \
  -n "$OPERATOR_VM_NAME" \
  --command-id RunShellScript \
  --scripts \
    'systemctl status gantry-benchmark-operator.service --no-pager || true' \
    'tail -100 /var/log/gantry-benchmark/service.log' \
    'cat /var/lib/gantry-benchmark/artifacts/last-run.json 2>/dev/null || true' \
    'cat /var/lib/gantry-benchmark/artifacts/latest/comparison.md 2>/dev/null || true'
```

Artifacts persist under `/var/lib/gantry-benchmark/artifacts/<run-id>/` on the
VM. The service cleanup trap restores registry routing and removes benchmark
instrumentation whether the run passes, fails a regression gate, or is stopped.

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
300 nodes pulling from the baseline ACR at once.

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
  cat "/host-certs/${BASELINE_ACR_LOGIN_SERVER}/hosts.toml"

kubectl -n gantry-benchmark exec "$node" -- \
  cat "/host-certs/${GANTRY_ACR_LOGIN_SERVER}/hosts.toml"
```

The baseline ACR file must contain its direct HTTPS server and must not mention
`127.0.0.1:5000`. The Gantry ACR file must contain only the
`[host."http://127.0.0.1:5000"]` block with no `server =` line.

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
jq '.checks | {same_workload_payload, baseline_bypassed_gantry, no_origin_fallback}' \
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

After the VM service exits, inspect its preserved artifacts through Run Command:

```bash
az vm run-command invoke \
  -g "$RESOURCE_GROUP" \
  -n "${OPERATOR_VM_NAME:-gantry-benchmark-operator}" \
  --command-id RunShellScript \
  --scripts \
    'cat /var/lib/gantry-benchmark/artifacts/last-run.json' \
    'cat /var/lib/gantry-benchmark/artifacts/latest/comparison.md' \
    'jq . /var/lib/gantry-benchmark/artifacts/latest/comparison.json'
```

The comparison includes upstream bytes, request counts, Gantry origin pulls,
peer fetch hits, and pod latency summaries.

Use dashboard `Gantry ACR Benchmark` through the cluster's normal monitoring
access and select the run ID.

## Cleanup

Cleanup is automatic in the VM service. To retry cleanup after a stopped or
failed service, invoke `disable` on the VM:

```bash
az vm run-command invoke \
  -g "$RESOURCE_GROUP" \
  -n "${OPERATOR_VM_NAME:-gantry-benchmark-operator}" \
  --command-id RunShellScript \
  --scripts \
    'set -a; . /etc/gantry-benchmark/env; set +a' \
    'export HOME=/var/lib/gantry-benchmark KUBECONFIG=/var/lib/gantry-benchmark/kubeconfig' \
    'export BENCHMARK_CONFIRM_CONTEXT=$(kubectl config current-context)' \
    'make -C /opt/gantry-benchmark/unbounded/hack/gantry-benchmark disable'
```

Cleanup restores node routing, restores Gantry config, verifies Gantry, removes
the benchmark namespace and dashboard, and releases the lock.

Checkpoint after cleanup from the VM:

```bash
az vm run-command invoke \
  -g "$RESOURCE_GROUP" \
  -n "${OPERATOR_VM_NAME:-gantry-benchmark-operator}" \
  --command-id RunShellScript \
  --scripts \
    'export KUBECONFIG=/var/lib/gantry-benchmark/kubeconfig' \
    'kubectl get namespace gantry-benchmark 2>/dev/null || true' \
    'kubectl -n gantry-system get configmap gantry-benchmark-lock 2>/dev/null || true' \
    'kubectl -n gantry-system get daemonset gantry'
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
- Current Gantry ConfigMap endpoint for the dedicated Gantry ACR.
- Whether `gantry-benchmark-hosts` or `gantry-benchmark-hosts-restore` is
  installed and ready.
- Which node is blocking and why.
