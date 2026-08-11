#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/../.." && pwd)
. "$script_dir/operator-vm-ssh-common.sh"

usage() {
  cat <<'USAGE'
Usage: deploy.sh <plan|deploy|status> [config-file]

One idempotent entrypoint for the complete Gantry benchmark stack. The config
file is a shell environment file. No credentials are stored in it.

  plan    validate inputs and print the complete deployment contract
  deploy  create or validate every resource and leave the benchmark ready
  status  report Azure and Kubernetes readiness without mutation
USAGE
}

action=${1:-plan}
config_file=${2:-${GANTRY_BENCHMARK_DEPLOY_CONFIG:-$script_dir/deploy.env}}

case "$action" in
plan | deploy | status) ;;
-h | --help | help)
  usage
  exit 0
  ;;
*)
  usage >&2
  exit 2
  ;;
esac

[[ -f "$config_file" ]] || {
  echo "missing deployment config: $config_file" >&2
  echo "copy $script_dir/deploy.env.example and set the required values" >&2
  exit 2
}

set -a
# shellcheck source=/dev/null
. "$config_file"
set +a

: "${AZURE_SUBSCRIPTION_ID:?Set AZURE_SUBSCRIPTION_ID}"
: "${DEPLOYMENT_NAME:?Set DEPLOYMENT_NAME}"
: "${BASELINE_ACR_NAME:?Set globally unique BASELINE_ACR_NAME}"
: "${GANTRY_ACR_NAME:?Set globally unique GANTRY_ACR_NAME}"

AZURE_LOCATION=${AZURE_LOCATION:-canadacentral}
AZURE_RESOURCE_GROUP=${AZURE_RESOURCE_GROUP:-$DEPLOYMENT_NAME}
AZURE_AKS_CLUSTER_NAME=${AZURE_AKS_CLUSTER_NAME:-${DEPLOYMENT_NAME}-aks}
AZURE_NODE_RESOURCE_GROUP=${AZURE_NODE_RESOURCE_GROUP:-${DEPLOYMENT_NAME}-nodes-rg}
AZURE_LOG_ANALYTICS_WORKSPACE_NAME=${AZURE_LOG_ANALYTICS_WORKSPACE_NAME:-${DEPLOYMENT_NAME}-law}

VNET_NAME=${VNET_NAME:-${DEPLOYMENT_NAME}-vnet}
VNET_CIDR=${VNET_CIDR:-10.224.0.0/12}
AKS_SUBNET_NAME=${AKS_SUBNET_NAME:-aks-nodes}
AKS_SUBNET_CIDR=${AKS_SUBNET_CIDR:-10.224.0.0/20}
PRIVATE_ENDPOINT_SUBNET_NAME=${PRIVATE_ENDPOINT_SUBNET_NAME:-acr-private-endpoints}
PRIVATE_ENDPOINT_SUBNET_CIDR=${PRIVATE_ENDPOINT_SUBNET_CIDR:-10.225.0.0/27}
OPERATOR_SUBNET_NAME=${OPERATOR_SUBNET_NAME:-gantry-benchmark-operator}
OPERATOR_SUBNET_CIDR=${OPERATOR_SUBNET_CIDR:-10.236.0.0/24}

POD_CIDR=${POD_CIDR:-10.64.0.0/12}
SERVICE_CIDR=${SERVICE_CIDR:-10.0.0.0/16}
DNS_SERVICE_IP=${DNS_SERVICE_IP:-10.0.0.10}
AKS_KUBERNETES_VERSION=${AKS_KUBERNETES_VERSION:-1.35}
AKS_NODE_POOL_NAME=${AKS_NODE_POOL_NAME:-system}
AKS_NODE_COUNT=${AKS_NODE_COUNT:-1000}
AKS_NODE_VM_SIZE=${AKS_NODE_VM_SIZE:-Standard_D8s_v3}
AKS_NODE_OS_DISK_GB=${AKS_NODE_OS_DISK_GB:-512}
AKS_MAX_PODS=${AKS_MAX_PODS:-250}

BENCHMARK_NODE_COUNT=${BENCHMARK_NODE_COUNT:-$AKS_NODE_COUNT}
BENCHMARK_IMAGE_SIZE_MIB=${BENCHMARK_IMAGE_SIZE_MIB:-40960}
BENCHMARK_IMAGE_LAYERS=${BENCHMARK_IMAGE_LAYERS:-40}
BENCHMARK_MINIMUM_BYTE_REDUCTION=${BENCHMARK_MINIMUM_BYTE_REDUCTION:-0.90}
BENCHMARK_MAXIMUM_LATENCY_RATIO=${BENCHMARK_MAXIMUM_LATENCY_RATIO:-1.0}
ADOPT_BASELINE_IMAGE=${ADOPT_BASELINE_IMAGE:-}
ADOPT_GANTRY_IMAGE=${ADOPT_GANTRY_IMAGE:-}
ADOPT_PAYLOAD_SHA256=${ADOPT_PAYLOAD_SHA256:-}
BASELINE_PULL_MAX_NODE_REPLACEMENTS=${BASELINE_PULL_MAX_NODE_REPLACEMENTS:-5}

GANTRY_NAMESPACE=${GANTRY_NAMESPACE:-gantry-system}
BENCHMARK_NAMESPACE=${BENCHMARK_NAMESPACE:-gantry-benchmark}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-monitoring}
KPS_RELEASE=${KPS_RELEASE:-kps}
KPS_CHART_VERSION=${KPS_CHART_VERSION:-87.21.0}
PROMETHEUS_SERVICE=${PROMETHEUS_SERVICE:-kps-kube-prometheus-stack-prometheus}

OPERATOR_VM_NAME=${OPERATOR_VM_NAME:-gantry-benchmark-operator}
OPERATOR_VM_SIZE=${OPERATOR_VM_SIZE:-Standard_D32ds_v5}
OPERATOR_VM_ZONE=${OPERATOR_VM_ZONE:-1}
OPERATOR_OS_DISK_GB=${OPERATOR_OS_DISK_GB:-128}
OPERATOR_BUILD_DISK_GB=${OPERATOR_BUILD_DISK_GB:-512}
OPERATOR_BUILD_DISK_SKU=${OPERATOR_BUILD_DISK_SKU:-PremiumV2_LRS}
OPERATOR_BUILD_DISK_IOPS=${OPERATOR_BUILD_DISK_IOPS:-20000}
OPERATOR_BUILD_DISK_MBPS=${OPERATOR_BUILD_DISK_MBPS:-750}
OPERATOR_SSH_PORT=${OPERATOR_SSH_PORT:-50001}
OPERATOR_SSH_PUBLIC_IP_NAME=${OPERATOR_SSH_PUBLIC_IP_NAME:-gantry-benchmark-operator-ssh}
OPERATOR_SSH_NSG_RULE_NAME=${OPERATOR_SSH_NSG_RULE_NAME:-allow-operator-ssh-50001}
OPERATOR_SSH_SOURCE_CIDR=${OPERATOR_SSH_SOURCE_CIDR:-}
OPERATOR_SSH_HOST_ALIAS=${OPERATOR_SSH_HOST_ALIAS:-$OPERATOR_VM_NAME}

START_BENCHMARK=${START_BENCHMARK:-false}
DEPLOY_CONFIRM=${DEPLOY_CONFIRM:-}
DEPLOY_STATE_DIR=${DEPLOY_STATE_DIR:-$repo_root/tmp/$DEPLOYMENT_NAME}
KUBECONFIG=${DEPLOY_KUBECONFIG:-$DEPLOY_STATE_DIR/kubeconfig}

BASELINE_PRIVATE_ENDPOINT_NAME=${BASELINE_PRIVATE_ENDPOINT_NAME:-${DEPLOYMENT_NAME}-baseline-acr-pe}
GANTRY_PRIVATE_ENDPOINT_NAME=${GANTRY_PRIVATE_ENDPOINT_NAME:-${DEPLOYMENT_NAME}-gantry-acr-pe}
PRIVATE_DNS_ZONE=privatelink.azurecr.io
PRIVATE_DNS_LINK_NAME=${PRIVATE_DNS_LINK_NAME:-${DEPLOYMENT_NAME}-acr-link}

BASELINE_ACR_LOGIN_SERVER=${BASELINE_ACR_NAME}.azurecr.io
GANTRY_ACR_LOGIN_SERVER=${GANTRY_ACR_NAME}.azurecr.io
BASELINE_ACR_DATA_HOST=${BASELINE_ACR_NAME}.${AZURE_LOCATION}.data.azurecr.io
GANTRY_ACR_DATA_HOST=${GANTRY_ACR_NAME}.${AZURE_LOCATION}.data.azurecr.io

[[ "$AKS_NODE_COUNT" =~ ^[1-9][0-9]*$ ]] || { echo "AKS_NODE_COUNT must be positive" >&2; exit 2; }
[[ "$BENCHMARK_NODE_COUNT" == "$AKS_NODE_COUNT" ]] || {
  echo "BENCHMARK_NODE_COUNT must equal AKS_NODE_COUNT for this topology" >&2
  exit 2
}
[[ "$BENCHMARK_IMAGE_LAYERS" =~ ^[1-9][0-9]*$ ]] || { echo "BENCHMARK_IMAGE_LAYERS must be positive" >&2; exit 2; }
((BENCHMARK_IMAGE_LAYERS <= BENCHMARK_IMAGE_SIZE_MIB)) || {
  echo "BENCHMARK_IMAGE_LAYERS cannot exceed BENCHMARK_IMAGE_SIZE_MIB" >&2
  exit 2
}
[[ "$BASELINE_PULL_MAX_NODE_REPLACEMENTS" =~ ^[1-9][0-9]*$ ]] || {
  echo "BASELINE_PULL_MAX_NODE_REPLACEMENTS must be positive" >&2
  exit 2
}
for acr_name in "$BASELINE_ACR_NAME" "$GANTRY_ACR_NAME"; do
  [[ "$acr_name" =~ ^[a-z0-9]{5,50}$ ]] || {
    echo "invalid ACR name $acr_name: use 5-50 lowercase alphanumeric characters" >&2
    exit 2
  }
done
[[ "$START_BENCHMARK" == true || "$START_BENCHMARK" == false ]] || {
  echo "START_BENCHMARK must be true or false" >&2
  exit 2
}
[[ "$OPERATOR_SSH_PORT" == 50001 ]] || {
  echo "OPERATOR_SSH_PORT=$OPERATOR_SSH_PORT is unsupported; the operator contract requires 50001" >&2
  exit 2
}
valid_adopted_image() {
  local image=$1
  local login_server=$2
  local prefix="$login_server/gantry-benchmark-pull@"
  [[ "$image" == "$prefix"* && "${image#"$prefix"}" =~ ^sha256:[0-9a-f]{64}$ ]]
}
adoption_values=0
for value in "$ADOPT_BASELINE_IMAGE" "$ADOPT_GANTRY_IMAGE" "$ADOPT_PAYLOAD_SHA256"; do
  [[ -z "$value" ]] || adoption_values=$((adoption_values + 1))
done
if ((adoption_values != 0 && adoption_values != 3)); then
  echo "ADOPT_BASELINE_IMAGE, ADOPT_GANTRY_IMAGE, and ADOPT_PAYLOAD_SHA256 must be set together" >&2
  exit 2
fi
if ((adoption_values == 3)); then
  valid_adopted_image "$ADOPT_BASELINE_IMAGE" "$BASELINE_ACR_LOGIN_SERVER" || {
    echo "ADOPT_BASELINE_IMAGE must be an immutable gantry-benchmark-pull image in $BASELINE_ACR_LOGIN_SERVER" >&2
    exit 2
  }
  valid_adopted_image "$ADOPT_GANTRY_IMAGE" "$GANTRY_ACR_LOGIN_SERVER" || {
    echo "ADOPT_GANTRY_IMAGE must be an immutable gantry-benchmark-pull image in $GANTRY_ACR_LOGIN_SERVER" >&2
    exit 2
  }
  [[ "$ADOPT_PAYLOAD_SHA256" =~ ^sha256:[0-9a-f]{64}$ ]] || {
    echo "ADOPT_PAYLOAD_SHA256 must be a sha256 digest" >&2
    exit 2
  }
fi
assert_default() {
  local name=$1
  local actual=$2
  local expected=$3
  [[ "$actual" == "$expected" ]] || {
    echo "$name=$actual is unsupported; the operator contract requires $expected" >&2
    exit 2
  }
}
assert_default GANTRY_NAMESPACE "$GANTRY_NAMESPACE" gantry-system
assert_default BENCHMARK_NAMESPACE "$BENCHMARK_NAMESPACE" gantry-benchmark
assert_default MONITORING_NAMESPACE "$MONITORING_NAMESPACE" monitoring
assert_default KPS_RELEASE "$KPS_RELEASE" kps
assert_default PROMETHEUS_SERVICE "$PROMETHEUS_SERVICE" kps-kube-prometheus-stack-prometheus

log() { printf '%s [deploy] %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"; }

require_command() {
  command -v "$1" >/dev/null 2>&1 || { echo "required command not found: $1" >&2; exit 1; }
}

retry_command() {
  local attempts=$1
  local delay=$2
  shift 2
  local attempt
  for attempt in $(seq 1 "$attempts"); do
    if "$@"; then
      return 0
    fi
    if ((attempt == attempts)); then
      return 1
    fi
    log "attempt $attempt/$attempts failed; retrying in ${delay}s"
    sleep "$delay"
  done
}

print_plan() {
  local image_preparation="build and push fresh workload images"
  if ((adoption_values == 3)); then
    image_preparation="adopt existing immutable workload images"
  fi
  cat <<PLAN
Gantry benchmark deployment plan

Source
  repository:          $repo_root
  revision:            $(git -C "$repo_root" rev-parse HEAD 2>/dev/null || echo unknown)
  source carrier:      ACR Task before registry privatization
  runtime images:      managed-identity operator over Private Link

Azure
  subscription:        $AZURE_SUBSCRIPTION_ID
  location:            $AZURE_LOCATION
  resource group:      $AZURE_RESOURCE_GROUP
  AKS:                 $AZURE_AKS_CLUSTER_NAME
  node resource group: $AZURE_NODE_RESOURCE_GROUP
  node pool:           $AKS_NODE_POOL_NAME ($AKS_NODE_COUNT x $AKS_NODE_VM_SIZE)
  node OS disk:        ${AKS_NODE_OS_DISK_GB} GiB managed
  Kubernetes:          $AKS_KUBERNETES_VERSION

Network
  VNet:                $VNET_NAME $VNET_CIDR
  AKS subnet:          $AKS_SUBNET_NAME $AKS_SUBNET_CIDR
  pod CIDR:            $POD_CIDR
  service CIDR:        $SERVICE_CIDR
  PE subnet:           $PRIVATE_ENDPOINT_SUBNET_NAME $PRIVATE_ENDPOINT_SUBNET_CIDR
  operator subnet:     $OPERATOR_SUBNET_NAME $OPERATOR_SUBNET_CIDR
  operator SSH:        public TCP $OPERATOR_SSH_PORT, source ${OPERATOR_SSH_SOURCE_CIDR:-deploying workstation IPv4/32}

Registries
  baseline:            $BASELINE_ACR_LOGIN_SERVER
  Gantry:              $GANTRY_ACR_LOGIN_SERVER
  access:              Premium, dedicated data endpoint, Private Endpoint, public disabled at completion

Benchmark
  nodes:               $BENCHMARK_NODE_COUNT
  payload:             ${BENCHMARK_IMAGE_SIZE_MIB} MiB in $BENCHMARK_IMAGE_LAYERS layers
  image preparation:   $image_preparation
  adopted baseline:    ${ADOPT_BASELINE_IMAGE:-none}
  adopted Gantry:      ${ADOPT_GANTRY_IMAGE:-none}
  adopted payload:     ${ADOPT_PAYLOAD_SHA256:-none}
  monitoring:          kube-prometheus-stack $KPS_CHART_VERSION with benchmark-only discovery
  operator:            $OPERATOR_VM_SIZE with ${OPERATOR_BUILD_DISK_GB} GiB $OPERATOR_BUILD_DISK_SKU
  start benchmark:     $START_BENCHMARK
PLAN
}

if [[ "$action" == plan ]]; then
  print_plan
  exit 0
fi

for command in az curl jq kubectl helm git make sha256sum ssh ssh-keygen ssh-keyscan timeout; do
  require_command "$command"
done

az account set --subscription "$AZURE_SUBSCRIPTION_ID"
if ! timeout 60s az account get-access-token --resource https://management.core.windows.net/ --output none; then
  echo "Azure management authentication is unavailable; run az login once before deploy.sh" >&2
  exit 1
fi

if [[ "$action" == status ]]; then
  az group show -g "$AZURE_RESOURCE_GROUP" --query '{name:name,location:location,state:properties.provisioningState}' -o json
  az aks show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" \
    --query '{state:provisioningState,version:kubernetesVersion,nodeResourceGroup:nodeResourceGroup}' -o json
  az acr list -g "$AZURE_RESOURCE_GROUP" \
    --query '[].{name:name,publicNetworkAccess:publicNetworkAccess,dataEndpointEnabled:dataEndpointEnabled}' -o json
  mkdir -p "$(dirname "$KUBECONFIG")"
  az aks get-credentials -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" \
    --admin --file "$KUBECONFIG" --overwrite-existing --only-show-errors
  chmod 0600 "$KUBECONFIG"
  export KUBECONFIG
  kubectl get nodes -o json | jq '{total:(.items|length),ready:([.items[]|select(any(.status.conditions[];.type=="Ready" and .status=="True"))]|length),unschedulable:([.items[]|select(.spec.unschedulable==true)]|length)}'
  kubectl get daemonset -A -o json | jq '[.items[]|select(.metadata.name|test("gantry|benchmark"))|{namespace:.metadata.namespace,name:.metadata.name,desired:.status.desiredNumberScheduled,ready:.status.numberReady}]'
  exit 0
fi

[[ "$DEPLOY_CONFIRM" == "$AZURE_RESOURCE_GROUP" ]] || {
  echo "set DEPLOY_CONFIRM=$AZURE_RESOURCE_GROUP to authorize deployment" >&2
  exit 2
}

[[ -z "$(git -C "$repo_root" status --porcelain)" ]] || {
  echo "deployment requires a clean Git worktree" >&2
  exit 1
}

source_revision=$(git -C "$repo_root" rev-parse HEAD)
source_short=$(git -C "$repo_root" rev-parse --short=12 HEAD)

mkdir -p "$DEPLOY_STATE_DIR"
chmod 0700 "$DEPLOY_STATE_DIR"

public_restore_needed=false
set_acrs_private() {
  for acr in "$BASELINE_ACR_NAME" "$GANTRY_ACR_NAME"; do
    if az acr show -g "$AZURE_RESOURCE_GROUP" -n "$acr" --output none >/dev/null 2>&1; then
      az acr update -g "$AZURE_RESOURCE_GROUP" -n "$acr" \
        --data-endpoint-enabled true --default-action Deny --public-network-enabled false \
        --only-show-errors -o none
    fi
  done
}

restore_private_access() {
  local status=$?
  if [[ "$public_restore_needed" == true ]]; then
    set_acrs_private || true
  fi
  exit "$status"
}
trap restore_private_access EXIT INT TERM

assert_equal() {
  local description=$1
  local actual=$2
  local expected=$3
  [[ "$actual" == "$expected" ]] || {
    echo "$description is $actual, want $expected" >&2
    exit 1
  }
}

guard_active_benchmark() {
  if ! az aks show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" --output none 2>/dev/null; then
    return
  fi

  mkdir -p "$(dirname "$KUBECONFIG")"
  az aks get-credentials -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" \
    --admin --file "$KUBECONFIG" --overwrite-existing --only-show-errors
  chmod 0600 "$KUBECONFIG"
  export KUBECONFIG
  if kubectl -n "$BENCHMARK_NAMESPACE" get configmap gantry-benchmark-state >/dev/null 2>&1 ||
    kubectl -n "$GANTRY_NAMESPACE" get configmap gantry-benchmark-lock >/dev/null 2>&1; then
    echo "an active benchmark state or lock exists; finish or disable it before deployment" >&2
    exit 1
  fi
}

ensure_group() {
  if [[ $(az group exists -n "$AZURE_RESOURCE_GROUP") == false ]]; then
    log "creating resource group $AZURE_RESOURCE_GROUP"
    az group create -n "$AZURE_RESOURCE_GROUP" -l "$AZURE_LOCATION" --only-show-errors -o none
  fi
  assert_equal "resource group location" \
    "$(az group show -n "$AZURE_RESOURCE_GROUP" --query location -o tsv)" "$AZURE_LOCATION"
}

ensure_vnet() {
  if ! az network vnet show -g "$AZURE_RESOURCE_GROUP" -n "$VNET_NAME" --output none 2>/dev/null; then
    log "creating VNet $VNET_NAME"
    az network vnet create -g "$AZURE_RESOURCE_GROUP" -n "$VNET_NAME" -l "$AZURE_LOCATION" \
      --address-prefixes "$VNET_CIDR" --subnet-name "$AKS_SUBNET_NAME" \
      --subnet-prefixes "$AKS_SUBNET_CIDR" --only-show-errors -o none
  fi
  local actual_prefix
  actual_prefix=$(az network vnet show -g "$AZURE_RESOURCE_GROUP" -n "$VNET_NAME" --query 'addressSpace.addressPrefixes[0]' -o tsv)
  assert_equal "VNet prefix" "$actual_prefix" "$VNET_CIDR"

  if ! az network vnet subnet show -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" -n "$AKS_SUBNET_NAME" --output none 2>/dev/null; then
    az network vnet subnet create -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" \
      -n "$AKS_SUBNET_NAME" --address-prefixes "$AKS_SUBNET_CIDR" --only-show-errors -o none
  fi
  assert_equal "AKS subnet prefix" \
    "$(az network vnet subnet show -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" -n "$AKS_SUBNET_NAME" --query addressPrefix -o tsv)" \
    "$AKS_SUBNET_CIDR"

  if ! az network vnet subnet show -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" -n "$PRIVATE_ENDPOINT_SUBNET_NAME" --output none 2>/dev/null; then
    az network vnet subnet create -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" \
      -n "$PRIVATE_ENDPOINT_SUBNET_NAME" --address-prefixes "$PRIVATE_ENDPOINT_SUBNET_CIDR" \
      --disable-private-endpoint-network-policies true --only-show-errors -o none
  fi
  assert_equal "Private Endpoint subnet prefix" \
    "$(az network vnet subnet show -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" -n "$PRIVATE_ENDPOINT_SUBNET_NAME" --query addressPrefix -o tsv)" \
    "$PRIVATE_ENDPOINT_SUBNET_CIDR"

  if ! az network vnet subnet show -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" -n "$OPERATOR_SUBNET_NAME" --output none 2>/dev/null; then
    az network vnet subnet create -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" \
      -n "$OPERATOR_SUBNET_NAME" --address-prefixes "$OPERATOR_SUBNET_CIDR" \
      --only-show-errors -o none
  fi
  assert_equal "operator subnet prefix" \
    "$(az network vnet subnet show -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" -n "$OPERATOR_SUBNET_NAME" --query addressPrefix -o tsv)" \
    "$OPERATOR_SUBNET_CIDR"
}

ensure_acr() {
  local name=$1
  if ! az acr show -g "$AZURE_RESOURCE_GROUP" -n "$name" --output none 2>/dev/null; then
    log "creating Premium ACR $name"
    az acr create -g "$AZURE_RESOURCE_GROUP" -n "$name" -l "$AZURE_LOCATION" \
      --sku Premium --public-network-enabled false --only-show-errors -o none
  fi
  assert_equal "$name SKU" "$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$name" --query sku.name -o tsv)" Premium
  assert_equal "$name location" "$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$name" --query location -o tsv)" "$AZURE_LOCATION"
  az acr update -g "$AZURE_RESOURCE_GROUP" -n "$name" \
    --data-endpoint-enabled true --only-show-errors -o none
}

acr_public_access_enabled() {
  local state
  state=$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" -o json)
  [[ $(jq -r .publicNetworkAccess <<<"$state") == Enabled &&
    $(jq -r .networkRuleSet.defaultAction <<<"$state") == Allow ]]
}

build_source_image() {
  log "publishing private source carrier from $source_revision"
  SOURCE_IMAGE=$GANTRY_ACR_LOGIN_SERVER/gantry-benchmark-source:$source_revision

  public_restore_needed=true
  az acr update -g "$AZURE_RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" \
    --default-action Allow --public-network-enabled true --only-show-errors -o none

  retry_command 30 10 acr_public_access_enabled

  local build_log=$DEPLOY_STATE_DIR/source-carrier-build.log
  local built=false
  local attempt
  for attempt in $(seq 1 18); do
    if az acr build \
      --registry "$GANTRY_ACR_NAME" \
      --image "gantry-benchmark-source:$source_revision" \
      --file "$repo_root/images/gantry-benchmark-source/Containerfile" \
      --build-arg "SOURCE_REVISION=$source_revision" \
      "$repo_root" --only-show-errors -o none >"$build_log" 2>&1; then
      built=true
      break
    fi
    log "source-carrier ACR Task is waiting for firewall propagation ($attempt/18)"
    sleep 30
  done
  if [[ "$built" != true ]]; then
    cat "$build_log" >&2
    return 1
  fi
  cat "$build_log"

  set_acrs_private
  public_restore_needed=false
}

ensure_aks() {
  local subnet_id
  subnet_id=$(az network vnet subnet show -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" \
    -n "$AKS_SUBNET_NAME" --query id -o tsv)
  if ! az aks show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" --output none 2>/dev/null; then
    log "creating AKS cluster $AZURE_AKS_CLUSTER_NAME"
    az aks create -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" -l "$AZURE_LOCATION" \
      --tier standard --enable-managed-identity --node-resource-group "$AZURE_NODE_RESOURCE_GROUP" \
      --nodepool-name "$AKS_NODE_POOL_NAME" --node-count "$AKS_NODE_COUNT" \
      --node-vm-size "$AKS_NODE_VM_SIZE" --node-osdisk-type Managed \
      --node-osdisk-size "$AKS_NODE_OS_DISK_GB" --max-pods "$AKS_MAX_PODS" \
      --os-sku Ubuntu --network-plugin azure --network-plugin-mode overlay \
      --network-dataplane azure --pod-cidr "$POD_CIDR" --service-cidr "$SERVICE_CIDR" \
      --dns-service-ip "$DNS_SERVICE_IP" --vnet-subnet-id "$subnet_id" \
      --load-balancer-sku standard --outbound-type loadBalancer \
      --kubernetes-version "$AKS_KUBERNETES_VERSION" --no-ssh-key --only-show-errors -o none
  fi

  az aks wait -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" \
    --created --interval 30 --timeout 7200

  local cluster_json pool_json
  cluster_json=$(az aks show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" -o json)
  assert_equal "AKS location" "$(jq -r .location <<<"$cluster_json")" "$AZURE_LOCATION"
  assert_equal "AKS Kubernetes version" "$(jq -r .kubernetesVersion <<<"$cluster_json")" "$AKS_KUBERNETES_VERSION"
  assert_equal "AKS pod CIDR" "$(jq -r .networkProfile.podCidr <<<"$cluster_json")" "$POD_CIDR"
  assert_equal "AKS service CIDR" "$(jq -r .networkProfile.serviceCidr <<<"$cluster_json")" "$SERVICE_CIDR"
  assert_equal "AKS node resource group" "$(jq -r .nodeResourceGroup <<<"$cluster_json")" "$AZURE_NODE_RESOURCE_GROUP"

  pool_json=$(az aks nodepool show -g "$AZURE_RESOURCE_GROUP" --cluster-name "$AZURE_AKS_CLUSTER_NAME" \
    -n "$AKS_NODE_POOL_NAME" -o json)
  assert_equal "AKS node count" "$(jq -r .count <<<"$pool_json")" "$AKS_NODE_COUNT"
  assert_equal "AKS node VM size" "$(jq -r .vmSize <<<"$pool_json")" "$AKS_NODE_VM_SIZE"
  assert_equal "AKS max pods" "$(jq -r .maxPods <<<"$pool_json")" "$AKS_MAX_PODS"
  assert_equal "AKS node OS disk" "$(jq -r .osDiskSizeGb <<<"$pool_json")" "$AKS_NODE_OS_DISK_GB"
  assert_equal "AKS node OS SKU" "$(jq -r .osSku <<<"$pool_json")" Ubuntu
  assert_equal "AKS node-pool mode" "$(jq -r .mode <<<"$pool_json")" System
  assert_equal "AKS node subnet" "$(jq -r .vnetSubnetId <<<"$pool_json")" "$subnet_id"
}

ensure_role() {
  local principal=$1
  local role=$2
  local scope=$3
  if [[ $(az role assignment list --assignee-object-id "$principal" --scope "$scope" --role "$role" --query 'length(@)' -o tsv) == 0 ]]; then
    az role assignment create --assignee-object-id "$principal" \
      --assignee-principal-type ServicePrincipal --role "$role" --scope "$scope" \
      --only-show-errors -o none
  fi
}

ensure_diagnostics() {
  local law_id aks_id baseline_id gantry_id
  if ! az monitor log-analytics workspace show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_LOG_ANALYTICS_WORKSPACE_NAME" --output none 2>/dev/null; then
    az monitor log-analytics workspace create -g "$AZURE_RESOURCE_GROUP" \
      -n "$AZURE_LOG_ANALYTICS_WORKSPACE_NAME" -l "$AZURE_LOCATION" \
      --retention-time 30 --only-show-errors -o none
  fi
  assert_equal "Log Analytics location" \
    "$(az monitor log-analytics workspace show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_LOG_ANALYTICS_WORKSPACE_NAME" --query location -o tsv)" \
    "$AZURE_LOCATION"
  law_id=$(az monitor log-analytics workspace show -g "$AZURE_RESOURCE_GROUP" \
    -n "$AZURE_LOG_ANALYTICS_WORKSPACE_NAME" --query id -o tsv)
  aks_id=$(az aks show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" --query id -o tsv)
  baseline_id=$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$BASELINE_ACR_NAME" --query id -o tsv)
  gantry_id=$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" --query id -o tsv)

  az monitor diagnostic-settings create --name "${DEPLOYMENT_NAME}-baseline-acr-diag" \
    --resource "$baseline_id" --workspace "$law_id" --export-to-resource-specific true \
    --logs '[{"category":"ContainerRegistryRepositoryEvents","enabled":true},{"category":"ContainerRegistryLoginEvents","enabled":true}]' \
    --metrics '[{"category":"AllMetrics","enabled":true}]' --only-show-errors -o none
  az monitor diagnostic-settings create --name "${DEPLOYMENT_NAME}-gantry-acr-diag" \
    --resource "$gantry_id" --workspace "$law_id" --export-to-resource-specific true \
    --logs '[{"category":"ContainerRegistryRepositoryEvents","enabled":true},{"category":"ContainerRegistryLoginEvents","enabled":true}]' \
    --metrics '[{"category":"AllMetrics","enabled":true}]' --only-show-errors -o none
  az monitor diagnostic-settings create --name "${DEPLOYMENT_NAME}-aks-diag" \
    --resource "$aks_id" --workspace "$law_id" --export-to-resource-specific true \
    --logs '[{"category":"kube-audit-admin","enabled":true},{"category":"kube-apiserver","enabled":true},{"category":"kube-scheduler","enabled":true}]' \
    --metrics '[{"category":"AllMetrics","enabled":true}]' --only-show-errors -o none
}

ensure_private_endpoint() {
  local name=$1
  local acr_id=$2
  local connection_name=$3
  local subnet_id zone_id zone_group_count
  subnet_id=$(az network vnet subnet show -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" \
    -n "$PRIVATE_ENDPOINT_SUBNET_NAME" --query id -o tsv)
  zone_id=$(az network private-dns zone show -g "$AZURE_RESOURCE_GROUP" -n "$PRIVATE_DNS_ZONE" --query id -o tsv)

  if ! az network private-endpoint show -g "$AZURE_RESOURCE_GROUP" -n "$name" --output none 2>/dev/null; then
    az network private-endpoint create -g "$AZURE_RESOURCE_GROUP" -n "$name" -l "$AZURE_LOCATION" \
      --subnet "$subnet_id" --private-connection-resource-id "$acr_id" \
      --group-ids registry --connection-name "$connection_name" --only-show-errors -o none
  fi
  zone_group_count=$(az network private-endpoint dns-zone-group list -g "$AZURE_RESOURCE_GROUP" \
    --endpoint-name "$name" --query 'length(@)' -o tsv)
  if [[ "$zone_group_count" == 0 ]]; then
    az network private-endpoint dns-zone-group create -g "$AZURE_RESOURCE_GROUP" \
      --endpoint-name "$name" -n acr --private-dns-zone "$zone_id" \
      --zone-name "$PRIVATE_DNS_ZONE" --only-show-errors -o none
  fi
  assert_equal "$name private DNS zone" \
    "$(az network private-endpoint dns-zone-group show -g "$AZURE_RESOURCE_GROUP" \
      --endpoint-name "$name" -n acr --query 'privateDnsZoneConfigs[0].privateDnsZoneId' -o tsv)" \
    "$zone_id"
  assert_equal "$name connection state" \
    "$(az network private-endpoint show -g "$AZURE_RESOURCE_GROUP" -n "$name" --query 'privateLinkServiceConnections[0].privateLinkServiceConnectionState.status' -o tsv)" \
    Approved
  assert_equal "$name target resource" \
    "$(az network private-endpoint show -g "$AZURE_RESOURCE_GROUP" -n "$name" --query 'privateLinkServiceConnections[0].privateLinkServiceId' -o tsv)" \
    "$acr_id"
  assert_equal "$name subnet" \
    "$(az network private-endpoint show -g "$AZURE_RESOURCE_GROUP" -n "$name" --query subnet.id -o tsv)" \
    "$subnet_id"
}

ensure_private_network() {
  if ! az network private-dns zone show -g "$AZURE_RESOURCE_GROUP" -n "$PRIVATE_DNS_ZONE" --output none 2>/dev/null; then
    az network private-dns zone create -g "$AZURE_RESOURCE_GROUP" -n "$PRIVATE_DNS_ZONE" --only-show-errors -o none
  fi
  local vnet_id
  vnet_id=$(az network vnet show -g "$AZURE_RESOURCE_GROUP" -n "$VNET_NAME" --query id -o tsv)
  if ! az network private-dns link vnet show -g "$AZURE_RESOURCE_GROUP" -z "$PRIVATE_DNS_ZONE" \
    -n "$PRIVATE_DNS_LINK_NAME" --output none 2>/dev/null; then
    az network private-dns link vnet create -g "$AZURE_RESOURCE_GROUP" -z "$PRIVATE_DNS_ZONE" \
      -n "$PRIVATE_DNS_LINK_NAME" -v "$vnet_id" -e false --only-show-errors -o none
  fi
  assert_equal "private DNS VNet link" \
    "$(az network private-dns link vnet show -g "$AZURE_RESOURCE_GROUP" -z "$PRIVATE_DNS_ZONE" -n "$PRIVATE_DNS_LINK_NAME" --query virtualNetwork.id -o tsv)" \
    "$vnet_id"

  local baseline_id gantry_id
  baseline_id=$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$BASELINE_ACR_NAME" --query id -o tsv)
  gantry_id=$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" --query id -o tsv)
  ensure_private_endpoint "$BASELINE_PRIVATE_ENDPOINT_NAME" "$baseline_id" "${DEPLOYMENT_NAME}-baseline-acr"
  ensure_private_endpoint "$GANTRY_PRIVATE_ENDPOINT_NAME" "$gantry_id" "${DEPLOYMENT_NAME}-gantry-acr"
}

wait_for_nodes() {
  mkdir -p "$(dirname "$KUBECONFIG")"
  rm -f "$KUBECONFIG"
  az aks get-credentials -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" \
    --admin --file "$KUBECONFIG" --overwrite-existing --only-show-errors
  chmod 0600 "$KUBECONFIG"
  export KUBECONFIG

  if kubectl -n "$BENCHMARK_NAMESPACE" get configmap gantry-benchmark-state >/dev/null 2>&1 ||
    kubectl -n "$GANTRY_NAMESPACE" get configmap gantry-benchmark-lock >/dev/null 2>&1; then
    echo "an active benchmark state or lock exists; finish or disable it before deployment" >&2
    exit 1
  fi

  local attempt total ready unschedulable
  for attempt in $(seq 1 120); do
    local nodes
    nodes=$(kubectl get nodes -o json)
    total=$(jq '.items|length' <<<"$nodes")
    ready=$(jq '[.items[]|select(any(.status.conditions[];.type=="Ready" and .status=="True"))]|length' <<<"$nodes")
    unschedulable=$(jq '[.items[]|select(.spec.unschedulable==true)]|length' <<<"$nodes")
    if [[ "$total" == "$AKS_NODE_COUNT" && "$ready" == "$AKS_NODE_COUNT" && "$unschedulable" == 0 ]]; then
      log "AKS nodes ready: $ready/$total"
      return
    fi
    log "waiting for nodes: total=$total ready=$ready unschedulable=$unschedulable"
    sleep 30
  done
  echo "AKS did not reach $AKS_NODE_COUNT Ready schedulable nodes" >&2
  exit 1
}

install_monitoring() {
  export KUBECONFIG
  local values=$DEPLOY_STATE_DIR/kps-values.yaml
  cat >"$values" <<VALUES
grafana:
  sidecar:
    dashboards:
      enabled: true
defaultRules:
  create: false
prometheus:
  prometheusSpec:
    retention: 2d
    resources:
      requests:
        cpu: "4"
        memory: 16Gi
      limits:
        cpu: "8"
        memory: 24Gi
    serviceMonitorSelectorNilUsesHelmValues: false
    serviceMonitorSelector:
      matchLabels:
        gantry_benchmark: "true"
    podMonitorSelectorNilUsesHelmValues: false
    podMonitorSelector:
      matchLabels:
        gantry_benchmark: "true"
    probeSelectorNilUsesHelmValues: false
    probeSelector:
      matchLabels:
        gantry_benchmark: "true"
    ruleSelectorNilUsesHelmValues: false
    ruleSelector:
      matchLabels:
        gantry_benchmark: "true"
VALUES
  helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update
  helm repo update
  helm upgrade --install "$KPS_RELEASE" prometheus-community/kube-prometheus-stack \
    --version "$KPS_CHART_VERSION" --namespace "$MONITORING_NAMESPACE" --create-namespace \
    --values "$values" --wait --timeout 45m
  kubectl -n "$MONITORING_NAMESPACE" rollout status \
    "statefulset/prometheus-$PROMETHEUS_SERVICE" --timeout=20m
}

private_dns_ip() {
  local record=$1
  local attempt value
  for attempt in $(seq 1 60); do
    value=$(az network private-dns record-set a show -g "$AZURE_RESOURCE_GROUP" \
      -z "$PRIVATE_DNS_ZONE" -n "$record" --query 'aRecords[0].ipv4Address' -o tsv 2>/dev/null || true)
    if [[ -n "$value" ]]; then
      printf '%s' "$value"
      return
    fi
    sleep 10
  done
  echo "private DNS record $record did not appear" >&2
  exit 1
}

install_node_configuration() {
  export KUBECONFIG
  kubectl create namespace "$GANTRY_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply -f "$repo_root/hack/gantry-benchmark/manifests/containerd.yaml"
  kubectl -n "$GANTRY_NAMESPACE" rollout status \
    daemonset/gantry-benchmark-containerd-config --timeout=45m

  local baseline_login_ip baseline_data_ip gantry_login_ip gantry_data_ip
  baseline_login_ip=$(private_dns_ip "$BASELINE_ACR_NAME")
  baseline_data_ip=$(private_dns_ip "$BASELINE_ACR_NAME.$AZURE_LOCATION.data")
  gantry_login_ip=$(private_dns_ip "$GANTRY_ACR_NAME")
  gantry_data_ip=$(private_dns_ip "$GANTRY_ACR_NAME.$AZURE_LOCATION.data")

  local guard=$DEPLOY_STATE_DIR/acr-private-dns-guard.yaml
  cat >"$guard" <<GUARD
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: gantry-acr-private-dns-guard
  namespace: $GANTRY_NAMESPACE
  labels:
    app.kubernetes.io/name: gantry-acr-private-dns-guard
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: gantry-acr-private-dns-guard
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 100
  template:
    metadata:
      labels:
        app.kubernetes.io/name: gantry-acr-private-dns-guard
    spec:
      hostNetwork: true
      hostPID: true
      dnsPolicy: Default
      nodeSelector:
        kubernetes.io/os: linux
      tolerations:
        - operator: Exists
      containers:
        - name: guard
          image: mcr.microsoft.com/cbl-mariner/busybox:2.0
          command: ["chroot", "/host", "sh", "-c"]
          args:
            - |
              set -eu
              hosts=/etc/hosts
              begin='# BEGIN GANTRY BENCHMARK ACR PRIVATE DNS'
              end='# END GANTRY BENCHMARK ACR PRIVATE DNS'
              temp=/etc/.hosts.gantry-benchmark.tmp
              awk -v begin="\$begin" -v end="\$end" '
                \$0 == begin { skip=1; next }
                \$0 == end { skip=0; next }
                !skip { print }
              ' "\$hosts" >"\$temp"
              cat >>"\$temp" <<'HOSTS'
              # BEGIN GANTRY BENCHMARK ACR PRIVATE DNS
              $baseline_login_ip $BASELINE_ACR_LOGIN_SERVER
              $baseline_data_ip $BASELINE_ACR_DATA_HOST
              $gantry_login_ip $GANTRY_ACR_LOGIN_SERVER
              $gantry_data_ip $GANTRY_ACR_DATA_HOST
              # END GANTRY BENCHMARK ACR PRIVATE DNS
              HOSTS
              cat "\$temp" >"\$hosts"
              rm "\$temp"
              resolvectl flush-caches
              exec sleep 2147483647
          readinessProbe:
            exec:
              command:
                - chroot
                - /host
                - sh
                - -c
                - |
                    set -eu
                    check() {
                      resolved=\$(getent ahostsv4 "\$1" | awk '{print \$1}' | sort -u)
                      test "\$resolved" = "\$2"
                    }
                    check $BASELINE_ACR_LOGIN_SERVER $baseline_login_ip
                    check $BASELINE_ACR_DATA_HOST $baseline_data_ip
                    check $GANTRY_ACR_LOGIN_SERVER $gantry_login_ip
                    check $GANTRY_ACR_DATA_HOST $gantry_data_ip
            initialDelaySeconds: 2
            timeoutSeconds: 10
            periodSeconds: 15
            failureThreshold: 3
          resources:
            requests: {cpu: 1m, memory: 4Mi}
            limits: {cpu: 20m, memory: 16Mi}
          securityContext:
            privileged: true
            runAsUser: 0
          volumeMounts:
            - name: host
              mountPath: /host
      volumes:
        - name: host
          hostPath:
            path: /
            type: Directory
GUARD
  kubectl apply -f "$guard"
  kubectl -n "$GANTRY_NAMESPACE" rollout status daemonset/gantry-acr-private-dns-guard --timeout=30m
}

replace_private_pull_tls_nodes() {
  local pods_json node provider_id machine_name current_count
  local -a nodes
  pods_json=$(kubectl -n "$GANTRY_NAMESPACE" get pods \
    -l app.kubernetes.io/name=gantry-baseline-acr-pull-probe -o json)
  mapfile -t nodes < <(jq -r '.items[] |
    select(any(.status.containerStatuses[]?; ((.state.waiting.message? // "") | contains("TLS handshake timeout")))) |
    .spec.nodeName' <<<"$pods_json" | sort -u)
  ((${#nodes[@]} > 0)) || return 1
  ((${#nodes[@]} <= BASELINE_PULL_MAX_NODE_REPLACEMENTS)) || {
    echo "refusing to replace ${#nodes[@]} nodes with ACR TLS handshake timeouts; limit is $BASELINE_PULL_MAX_NODE_REPLACEMENTS" >&2
    return 1
  }

  for node in "${nodes[@]}"; do
    provider_id=$(kubectl get node "$node" -o jsonpath='{.spec.providerID}')
    machine_name=$(az aks machine list -g "$AZURE_RESOURCE_GROUP" \
      --cluster-name "$AZURE_AKS_CLUSTER_NAME" --nodepool-name "$AKS_NODE_POOL_NAME" -o json | \
      jq -r --arg resource_id "${provider_id#azure://}" \
        '.[] | select((.properties.resourceId | ascii_downcase) == ($resource_id | ascii_downcase)) | .name')
    [[ -n "$machine_name" ]] || {
      echo "cannot resolve AKS machine for $node provider ID $provider_id" >&2
      return 1
    }
    log "replacing $node (AKS machine $machine_name) after persistent ACR Private Endpoint TLS timeouts"
    az aks nodepool delete-machines -g "$AZURE_RESOURCE_GROUP" \
      --cluster-name "$AZURE_AKS_CLUSTER_NAME" -n "$AKS_NODE_POOL_NAME" \
      --machine-names "$machine_name" --only-show-errors -o none
  done

  current_count=$(az aks nodepool show -g "$AZURE_RESOURCE_GROUP" \
    --cluster-name "$AZURE_AKS_CLUSTER_NAME" -n "$AKS_NODE_POOL_NAME" --query count -o tsv)
  if [[ "$current_count" != "$AKS_NODE_COUNT" ]]; then
    log "restoring AKS node pool count from $current_count to $AKS_NODE_COUNT"
    az aks nodepool scale -g "$AZURE_RESOURCE_GROUP" --cluster-name "$AZURE_AKS_CLUSTER_NAME" \
      -n "$AKS_NODE_POOL_NAME" --node-count "$AKS_NODE_COUNT" --only-show-errors -o none
  fi

  local attempt total ready old_nodes_remaining
  for attempt in $(seq 1 180); do
    total=$(kubectl get nodes -l "agentpool=$AKS_NODE_POOL_NAME" -o json | jq '.items | length')
    ready=$(kubectl get nodes -l "agentpool=$AKS_NODE_POOL_NAME" -o json | \
      jq '[.items[] | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))] | length')
    old_nodes_remaining=0
    for node in "${nodes[@]}"; do
      kubectl get node "$node" >/dev/null 2>&1 && ((old_nodes_remaining += 1))
    done
    if [[ "$total" == "$AKS_NODE_COUNT" && "$ready" == "$AKS_NODE_COUNT" && "$old_nodes_remaining" == 0 ]]; then
      break
    fi
    sleep 10
  done
  [[ "$total" == "$AKS_NODE_COUNT" && "$ready" == "$AKS_NODE_COUNT" && "$old_nodes_remaining" == 0 ]] || {
    echo "AKS node replacement did not restore $AKS_NODE_COUNT Ready nodes or remove all failed nodes" >&2
    return 1
  }
  kubectl -n "$GANTRY_NAMESPACE" rollout status \
    daemonset/gantry-benchmark-containerd-config --timeout=30m
  kubectl -n "$GANTRY_NAMESPACE" rollout status \
    daemonset/gantry-acr-private-dns-guard --timeout=30m
}

verify_private_baseline_pull() {
  export KUBECONFIG
  local manifest=$DEPLOY_STATE_DIR/baseline-private-pull-probe.yaml
  cat >"$manifest" <<PROBE
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: gantry-baseline-acr-pull-probe
  namespace: $GANTRY_NAMESPACE
  labels:
    app.kubernetes.io/name: gantry-baseline-acr-pull-probe
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: gantry-baseline-acr-pull-probe
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 100
  template:
    metadata:
      labels:
        app.kubernetes.io/name: gantry-baseline-acr-pull-probe
    spec:
      nodeSelector:
        kubernetes.io/os: linux
      tolerations:
        - operator: Exists
      containers:
        - name: probe
          image: $BASELINE_PROBE_IMAGE
          imagePullPolicy: Always
          command: ["sh", "-c", "exec sleep 2147483647"]
          resources:
            requests: {cpu: 1m, memory: 4Mi}
            limits: {cpu: 20m, memory: 16Mi}
PROBE
  kubectl apply -f "$manifest"
  local ready=false
  local repair_attempted=false
  local attempt
  for attempt in $(seq 1 6); do
    if kubectl -n "$GANTRY_NAMESPACE" rollout status \
      daemonset/gantry-baseline-acr-pull-probe --timeout=5m; then
      ready=true
      break
    fi
    if [[ "$repair_attempted" == false ]] && replace_private_pull_tls_nodes; then
      repair_attempted=true
      continue
    fi
    log "waiting for baseline AcrPull propagation ($attempt/6)"
  done
  if [[ "$ready" != true ]]; then
    kubectl -n "$GANTRY_NAMESPACE" get pods -l app.kubernetes.io/name=gantry-baseline-acr-pull-probe -o json | \
      jq '{total:(.items|length),waiting:([.items[].status.containerStatuses[]?.state.waiting|select(.!=null)]|group_by(.reason)|map({reason:.[0].reason,count:length,messages:(map(.message)|unique)}))}' >&2
    return 1
  fi
  kubectl -n "$GANTRY_NAMESPACE" delete daemonset gantry-baseline-acr-pull-probe --wait=true
}

deploy_gantry() {
  export KUBECONFIG
  local rendered=$DEPLOY_STATE_DIR/gantry-rendered
  rm -rf "$rendered"
  GOTOOLCHAIN=auto go run "$repo_root/hack/cmd/render-manifests" \
    --templates-dir "$repo_root/deploy/gantry" --output-dir "$rendered" \
    --set "Namespace=$GANTRY_NAMESPACE" --set "Image=$GANTRY_IMAGE" \
    --set "PprofListen=127.0.0.1:6060"
  sed -i "s/registry\.example\.com/$GANTRY_ACR_LOGIN_SERVER/g" "$rendered/configmap.yaml"

  kubectl apply -f "$rendered/serviceaccount.yaml"
  kubectl apply -f "$rendered/configmap.yaml"
  kubectl apply -f "$rendered/node-config.yaml"
  kubectl apply -f "$rendered/daemonset.yaml"
  kubectl -n "$GANTRY_NAMESPACE" rollout status daemonset/gantry-containerd-config --timeout=30m
  kubectl -n "$GANTRY_NAMESPACE" rollout status daemonset/gantry --timeout=45m
}

provision_operator() {
  export AZURE_SUBSCRIPTION_ID AZURE_RESOURCE_GROUP AZURE_AKS_CLUSTER_NAME
  export BASELINE_ACR_NAME GANTRY_ACR_NAME AZURE_LOG_ANALYTICS_WORKSPACE_NAME
  export OPERATOR_VNET_RESOURCE_GROUP=$AZURE_RESOURCE_GROUP OPERATOR_VNET_NAME=$VNET_NAME
  export AZURE_LOCATION OPERATOR_VM_NAME OPERATOR_VM_SIZE OPERATOR_VM_ZONE
  export OPERATOR_OS_DISK_GB OPERATOR_BUILD_DISK_GB OPERATOR_BUILD_DISK_SKU
  export OPERATOR_BUILD_DISK_IOPS OPERATOR_BUILD_DISK_MBPS OPERATOR_SUBNET_NAME OPERATOR_SUBNET_CIDR
  export OPERATOR_SSH_PORT OPERATOR_SSH_PUBLIC_IP_NAME OPERATOR_SSH_NSG_RULE_NAME
  export OPERATOR_SSH_SOURCE_CIDR OPERATOR_SSH_HOST_ALIAS
  export BENCHMARK_SOURCE_IMAGE=$SOURCE_IMAGE BENCHMARK_SOURCE_REVISION=$source_revision
  export BENCHMARK_NODE_COUNT BENCHMARK_IMAGE_SIZE_MIB BENCHMARK_IMAGE_LAYERS
  export BENCHMARK_AZURE_TELEMETRY=true BENCHMARK_MINIMUM_BYTE_REDUCTION BENCHMARK_MAXIMUM_LATENCY_RATIO
  export ADOPT_BASELINE_IMAGE ADOPT_GANTRY_IMAGE ADOPT_PAYLOAD_SHA256
  AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID=$(az network private-endpoint show \
    -g "$AZURE_RESOURCE_GROUP" -n "$BASELINE_PRIVATE_ENDPOINT_NAME" --query id -o tsv)
  AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID=$(az network private-endpoint show \
    -g "$AZURE_RESOURCE_GROUP" -n "$GANTRY_PRIVATE_ENDPOINT_NAME" --query id -o tsv)
  export AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID
  START_BENCHMARK=false "$repo_root/hack/gantry-benchmark/operator-vm-provision.sh"

  assert_equal "operator VM size" \
    "$(az vm show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_VM_NAME" --query hardwareProfile.vmSize -o tsv)" \
    "$OPERATOR_VM_SIZE"
  local operator_nic_id public_ip_id expected_public_ip_id
  operator_nic_id=$(az vm show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_VM_NAME" \
    --query 'networkProfile.networkInterfaces[0].id' -o tsv)
  public_ip_id=$(az network nic show --ids "$operator_nic_id" \
    --query 'ipConfigurations[0].publicIPAddress.id' -o tsv)
  expected_public_ip_id=$(az network public-ip show -g "$AZURE_RESOURCE_GROUP" \
    -n "$OPERATOR_SSH_PUBLIC_IP_NAME" --query id -o tsv)
  assert_equal "operator SSH public IP" "$public_ip_id" "$expected_public_ip_id"
  assert_equal "operator SSH NSG port" \
    "$(az network nsg rule show -g "$AZURE_RESOURCE_GROUP" --nsg-name "${OPERATOR_NSG_NAME:-gantry-benchmark-operator-nsg}" \
      -n "$OPERATOR_SSH_NSG_RULE_NAME" --query destinationPortRange -o tsv)" "$OPERATOR_SSH_PORT"
  local build_disk_name
  build_disk_name=${OPERATOR_BUILD_DISK_NAME:-${OPERATOR_VM_NAME}-build}
  assert_equal "operator build disk size" \
    "$(az disk show -g "$AZURE_RESOURCE_GROUP" -n "$build_disk_name" --query diskSizeGB -o tsv)" \
    "$OPERATOR_BUILD_DISK_GB"
  assert_equal "operator build disk SKU" \
    "$(az disk show -g "$AZURE_RESOURCE_GROUP" -n "$build_disk_name" --query sku.name -o tsv)" \
    "$OPERATOR_BUILD_DISK_SKU"
}

build_operator_images() {
  log "building Gantry and pull-probe images inside the private operator VM"
  local output
  operator_ssh_init
  output=$(operator_ssh sudo -n bash -s -- \
    "$AZURE_SUBSCRIPTION_ID" "$BASELINE_ACR_NAME" "$GANTRY_ACR_NAME" \
    "$source_revision" "$source_short" \
    <"$repo_root/hack/gantry-benchmark/operator-vm-build-images.sh")
  local result_json
  result_json=$(tr -d '\r' <<<"$output" | sed -n 's/^DEPLOYMENT_IMAGES_JSON=//p' | tail -1)
  jq -e 'type == "object" and (.gantry_image | type == "string") and (.baseline_probe_image | type == "string")' \
    <<<"$result_json" >/dev/null || {
    echo "operator did not return valid deployment image JSON" >&2
    return 1
  }
  GANTRY_IMAGE=$(jq -r .gantry_image <<<"$result_json")
  BASELINE_PROBE_IMAGE=$(jq -r .baseline_probe_image <<<"$result_json")
  [[ "$GANTRY_IMAGE" == "$GANTRY_ACR_LOGIN_SERVER/gantry@sha256:"* ]] || {
    echo "operator did not return an immutable Gantry image" >&2
    return 1
  }
  [[ "$BASELINE_PROBE_IMAGE" == "$BASELINE_ACR_LOGIN_SERVER/gantry-deploy-probe@sha256:"* ]] || {
    echo "operator did not return an immutable baseline probe image" >&2
    return 1
  }
}

guard_active_benchmark
ensure_group
ensure_vnet
ensure_acr "$BASELINE_ACR_NAME"
ensure_acr "$GANTRY_ACR_NAME"
build_source_image
ensure_private_network
ensure_aks
ensure_diagnostics

kubelet_object_id=$(az aks show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" \
  --query identityProfile.kubeletidentity.objectId -o tsv)
ensure_role "$kubelet_object_id" AcrPull \
  "$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$BASELINE_ACR_NAME" --query id -o tsv)"
ensure_role "$kubelet_object_id" AcrPull \
  "$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" --query id -o tsv)"

wait_for_nodes
install_node_configuration
install_monitoring

set_acrs_private

provision_operator
build_operator_images
verify_private_baseline_pull
deploy_gantry

log "validating final deployment"
assert_equal "baseline ACR public access" \
  "$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$BASELINE_ACR_NAME" --query publicNetworkAccess -o tsv)" Disabled
assert_equal "Gantry ACR public access" \
  "$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" --query publicNetworkAccess -o tsv)" Disabled

kubectl -n "$MONITORING_NAMESPACE" get endpoints "$PROMETHEUS_SERVICE" -o json | \
  jq -e '.subsets | any(.addresses | length > 0)' >/dev/null
for daemonset in gantry-benchmark-containerd-config gantry-acr-private-dns-guard gantry-containerd-config gantry; do
  namespace=$GANTRY_NAMESPACE
  desired=$(kubectl -n "$namespace" get daemonset "$daemonset" -o jsonpath='{.status.desiredNumberScheduled}')
  ready=$(kubectl -n "$namespace" get daemonset "$daemonset" -o jsonpath='{.status.numberReady}')
  assert_equal "$daemonset readiness" "$ready" "$desired"
done

if [[ "$START_BENCHMARK" == true ]]; then
  log "starting benchmark operator service"
  "$repo_root/hack/gantry-benchmark/operator-vm-start.sh"
fi

trap - EXIT INT TERM
log "deployment complete"
print_plan
cat <<NEXT

Status:
  GANTRY_BENCHMARK_DEPLOY_CONFIG=$config_file $script_dir/deploy.sh status

Operator:
  AZURE_RESOURCE_GROUP=$AZURE_RESOURCE_GROUP OPERATOR_VM_NAME=$OPERATOR_VM_NAME make -C $script_dir operator-vm-status
NEXT
