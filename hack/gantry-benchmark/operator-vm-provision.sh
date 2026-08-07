#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -Eeuo pipefail

: "${AZURE_SUBSCRIPTION_ID:?Set AZURE_SUBSCRIPTION_ID}"
: "${AZURE_RESOURCE_GROUP:?Set AZURE_RESOURCE_GROUP}"
: "${AZURE_AKS_CLUSTER_NAME:?Set AZURE_AKS_CLUSTER_NAME}"
: "${BASELINE_ACR_NAME:?Set BASELINE_ACR_NAME}"
: "${GANTRY_ACR_NAME:?Set GANTRY_ACR_NAME}"
: "${AZURE_LOG_ANALYTICS_WORKSPACE_NAME:?Set AZURE_LOG_ANALYTICS_WORKSPACE_NAME}"
: "${AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID:?Set AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID}"
: "${AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID:?Set AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID}"
: "${OPERATOR_VNET_RESOURCE_GROUP:?Set OPERATOR_VNET_RESOURCE_GROUP}"
: "${OPERATOR_VNET_NAME:?Set OPERATOR_VNET_NAME}"

AZURE_LOCATION="${AZURE_LOCATION:-canadacentral}"
OPERATOR_VM_NAME="${OPERATOR_VM_NAME:-gantry-benchmark-operator}"
OPERATOR_VM_SIZE="${OPERATOR_VM_SIZE:-Standard_D32ds_v5}"
OPERATOR_VM_ZONE="${OPERATOR_VM_ZONE:-1}"
OPERATOR_OS_DISK_GB="${OPERATOR_OS_DISK_GB:-128}"
OPERATOR_BUILD_DISK_NAME="${OPERATOR_BUILD_DISK_NAME:-${OPERATOR_VM_NAME}-build}"
OPERATOR_BUILD_DISK_GB="${OPERATOR_BUILD_DISK_GB:-512}"
OPERATOR_BUILD_DISK_SKU="${OPERATOR_BUILD_DISK_SKU:-PremiumV2_LRS}"
OPERATOR_BUILD_DISK_IOPS="${OPERATOR_BUILD_DISK_IOPS:-20000}"
OPERATOR_BUILD_DISK_MBPS="${OPERATOR_BUILD_DISK_MBPS:-750}"
OPERATOR_BUILD_DISK_LUN="${OPERATOR_BUILD_DISK_LUN:-0}"
OPERATOR_BUILD_MOUNT="${OPERATOR_BUILD_MOUNT:-/opt/gantry-benchmark}"
OPERATOR_SUBNET_NAME="${OPERATOR_SUBNET_NAME:-gantry-benchmark-operator}"
OPERATOR_SUBNET_CIDR="${OPERATOR_SUBNET_CIDR:-10.236.0.0/24}"
OPERATOR_NSG_NAME="${OPERATOR_NSG_NAME:-gantry-benchmark-operator-nsg}"
OPERATOR_NAT_NAME="${OPERATOR_NAT_NAME:-gantry-benchmark-operator-nat}"
OPERATOR_NAT_PUBLIC_IP_NAME="${OPERATOR_NAT_PUBLIC_IP_NAME:-gantry-benchmark-operator-egress}"
BENCHMARK_REPO_URL="${BENCHMARK_REPO_URL:-https://github.com/Azure/unbounded.git}"
BENCHMARK_REPO_BRANCH="${BENCHMARK_REPO_BRANCH:-private/gantry-benchmark-hardening}"
BENCHMARK_SOURCE_IMAGE="${BENCHMARK_SOURCE_IMAGE:-}"
BENCHMARK_SOURCE_REVISION="${BENCHMARK_SOURCE_REVISION:-}"
BENCHMARK_NODE_COUNT="${BENCHMARK_NODE_COUNT:-5}"
BENCHMARK_IMAGE_SIZE_MIB="${BENCHMARK_IMAGE_SIZE_MIB:-128}"
BENCHMARK_IMAGE_LAYERS="${BENCHMARK_IMAGE_LAYERS:-4}"
BENCHMARK_AZURE_TELEMETRY="${BENCHMARK_AZURE_TELEMETRY:-true}"
BENCHMARK_MINIMUM_BYTE_REDUCTION="${BENCHMARK_MINIMUM_BYTE_REDUCTION:-0.70}"
BENCHMARK_MAXIMUM_LATENCY_RATIO="${BENCHMARK_MAXIMUM_LATENCY_RATIO:-3.0}"
ADOPT_BASELINE_IMAGE="${ADOPT_BASELINE_IMAGE:-}"
ADOPT_GANTRY_IMAGE="${ADOPT_GANTRY_IMAGE:-}"
ADOPT_PAYLOAD_SHA256="${ADOPT_PAYLOAD_SHA256:-}"
START_BENCHMARK="${START_BENCHMARK:-false}"

repo_root=$(git rev-parse --show-toplevel)
bootstrap_script="$repo_root/hack/gantry-benchmark/operator-vm-bootstrap.sh"
key_path="$repo_root/tmp/gantry-benchmark-operator-key"

az account set --subscription "$AZURE_SUBSCRIPTION_ID"

if ! az network nsg show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_NSG_NAME" --output none 2>/dev/null; then
  az network nsg create -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_NSG_NAME" -l "$AZURE_LOCATION" --only-show-errors -o none
fi

if ! az network public-ip show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_NAT_PUBLIC_IP_NAME" --output none 2>/dev/null; then
  az network public-ip create \
    -g "$AZURE_RESOURCE_GROUP" \
    -n "$OPERATOR_NAT_PUBLIC_IP_NAME" \
    -l "$AZURE_LOCATION" \
    --sku Standard \
    --allocation-method Static \
    --only-show-errors \
    -o none
fi

public_ip_id=$(az network public-ip show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_NAT_PUBLIC_IP_NAME" --query id -o tsv)
if ! az network nat gateway show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_NAT_NAME" --output none 2>/dev/null; then
  az network nat gateway create \
    -g "$AZURE_RESOURCE_GROUP" \
    -n "$OPERATOR_NAT_NAME" \
    -l "$AZURE_LOCATION" \
    --public-ip-addresses "$public_ip_id" \
    --idle-timeout 10 \
    --only-show-errors \
    -o none
fi

nat_id=$(az network nat gateway show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_NAT_NAME" --query id -o tsv)
nsg_id=$(az network nsg show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_NSG_NAME" --query id -o tsv)

if ! az network vnet subnet show -g "$OPERATOR_VNET_RESOURCE_GROUP" --vnet-name "$OPERATOR_VNET_NAME" -n "$OPERATOR_SUBNET_NAME" --output none 2>/dev/null; then
  az network vnet subnet create \
    -g "$OPERATOR_VNET_RESOURCE_GROUP" \
    --vnet-name "$OPERATOR_VNET_NAME" \
    -n "$OPERATOR_SUBNET_NAME" \
    --address-prefixes "$OPERATOR_SUBNET_CIDR" \
    --network-security-group "$nsg_id" \
    --nat-gateway "$nat_id" \
    --only-show-errors \
    -o none
else
  az network vnet subnet update \
    -g "$OPERATOR_VNET_RESOURCE_GROUP" \
    --vnet-name "$OPERATOR_VNET_NAME" \
    -n "$OPERATOR_SUBNET_NAME" \
    --network-security-group "$nsg_id" \
    --nat-gateway "$nat_id" \
    --only-show-errors \
    -o none
fi

subnet_id=$(az network vnet subnet show -g "$OPERATOR_VNET_RESOURCE_GROUP" --vnet-name "$OPERATOR_VNET_NAME" -n "$OPERATOR_SUBNET_NAME" --query id -o tsv)

if [[ ! -f "$key_path.pub" ]]; then
  mkdir -p "$(dirname "$key_path")"
  ssh-keygen -q -t ed25519 -N '' -f "$key_path"
fi

if ! az vm show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_VM_NAME" --output none 2>/dev/null; then
  az vm create \
    -g "$AZURE_RESOURCE_GROUP" \
    -n "$OPERATOR_VM_NAME" \
    -l "$AZURE_LOCATION" \
    --image Canonical:ubuntu-24_04-lts:server:latest \
    --size "$OPERATOR_VM_SIZE" \
    --zone "$OPERATOR_VM_ZONE" \
    --subnet "$subnet_id" \
    --nsg '' \
    --public-ip-address '' \
    --assign-identity \
    --admin-username benchmark \
    --ssh-key-values "$key_path.pub" \
    --os-disk-size-gb "$OPERATOR_OS_DISK_GB" \
    --storage-sku Premium_LRS \
    --only-show-errors \
    -o none
fi

if ! az disk show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_BUILD_DISK_NAME" --output none 2>/dev/null; then
  az disk create \
    -g "$AZURE_RESOURCE_GROUP" \
    -n "$OPERATOR_BUILD_DISK_NAME" \
    -l "$AZURE_LOCATION" \
    --zone "$OPERATOR_VM_ZONE" \
    --sku "$OPERATOR_BUILD_DISK_SKU" \
    --size-gb "$OPERATOR_BUILD_DISK_GB" \
    --disk-iops-read-write "$OPERATOR_BUILD_DISK_IOPS" \
    --disk-mbps-read-write "$OPERATOR_BUILD_DISK_MBPS" \
    --only-show-errors \
    -o none
fi

build_disk_id=$(az disk show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_BUILD_DISK_NAME" --query id -o tsv)
attached_disk_id=$(az vm show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_VM_NAME" \
  --query "storageProfile.dataDisks[?lun==\`$OPERATOR_BUILD_DISK_LUN\`].managedDisk.id | [0]" -o tsv)
if [[ -z "$attached_disk_id" ]]; then
  az vm disk attach \
    -g "$AZURE_RESOURCE_GROUP" \
    --vm-name "$OPERATOR_VM_NAME" \
    --ids "$build_disk_id" \
    --lun "$OPERATOR_BUILD_DISK_LUN" \
    --caching None \
    --only-show-errors \
    -o none
elif [[ ! "$attached_disk_id" =~ /$OPERATOR_BUILD_DISK_NAME$ ]]; then
  echo "operator VM LUN $OPERATOR_BUILD_DISK_LUN is already occupied by $attached_disk_id" >&2
  exit 1
fi

principal_id=$(az vm show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_VM_NAME" --query identity.principalId -o tsv)
vm_id=$(az vm show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_VM_NAME" --query id -o tsv)
aks_id=$(az aks show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" --query id -o tsv)
baseline_acr_id=$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$BASELINE_ACR_NAME" --query id -o tsv)
gantry_acr_id=$(az acr show -g "$AZURE_RESOURCE_GROUP" -n "$GANTRY_ACR_NAME" --query id -o tsv)
workspace_id=$(az monitor log-analytics workspace show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_LOG_ANALYTICS_WORKSPACE_NAME" --query id -o tsv)
workspace_customer_id=$(az monitor log-analytics workspace show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_LOG_ANALYTICS_WORKSPACE_NAME" --query customerId -o tsv)

assign_role() {
  local role=$1
  local scope=$2
  if [[ "$(az role assignment list --assignee-object-id "$principal_id" --scope "$scope" --role "$role" --query 'length(@)' -o tsv)" == 0 ]]; then
    az role assignment create \
      --assignee-object-id "$principal_id" \
      --assignee-principal-type ServicePrincipal \
      --role "$role" \
      --scope "$scope" \
      --only-show-errors \
      -o none
  fi
}

assign_role "Azure Kubernetes Service Cluster Admin Role" "$aks_id"
assign_role AcrPush "$baseline_acr_id"
assign_role AcrPush "$gantry_acr_id"
assign_role Reader "/subscriptions/$AZURE_SUBSCRIPTION_ID/resourceGroups/$AZURE_RESOURCE_GROUP"
assign_role "Log Analytics Reader" "$workspace_id"

az vm run-command invoke \
  -g "$AZURE_RESOURCE_GROUP" \
  -n "$OPERATOR_VM_NAME" \
  --command-id RunShellScript \
  --scripts @"$bootstrap_script" \
  --parameters \
    "$AZURE_SUBSCRIPTION_ID" \
    "$AZURE_RESOURCE_GROUP" \
    "$AZURE_AKS_CLUSTER_NAME" \
    "$BASELINE_ACR_NAME" \
    "$GANTRY_ACR_NAME" \
    "$workspace_customer_id" \
    "$AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID" \
    "$AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID" \
    "$BENCHMARK_REPO_URL" \
    "$BENCHMARK_REPO_BRANCH" \
    "$BENCHMARK_NODE_COUNT" \
    "$BENCHMARK_IMAGE_SIZE_MIB" \
    "$BENCHMARK_IMAGE_LAYERS" \
    "$BENCHMARK_AZURE_TELEMETRY" \
    "$BENCHMARK_MINIMUM_BYTE_REDUCTION" \
    "$BENCHMARK_MAXIMUM_LATENCY_RATIO" \
    "$OPERATOR_BUILD_DISK_LUN" \
    "$OPERATOR_BUILD_MOUNT" \
    "$BENCHMARK_SOURCE_IMAGE" \
    "$BENCHMARK_SOURCE_REVISION" \
    "${ADOPT_BASELINE_IMAGE:--}" \
    "${ADOPT_GANTRY_IMAGE:--}" \
    "${ADOPT_PAYLOAD_SHA256:--}" \
  --only-show-errors \
  -o json

if [[ "$START_BENCHMARK" == true ]]; then
  az vm run-command invoke \
    -g "$AZURE_RESOURCE_GROUP" \
    -n "$OPERATOR_VM_NAME" \
    --command-id RunShellScript \
    --scripts 'systemctl start --no-block gantry-benchmark-operator.service' \
    --only-show-errors \
    -o none
fi

cat <<SUMMARY
operator VM: $vm_id
private subnet: $subnet_id
managed identity: $principal_id
build disk: $build_disk_id ($OPERATOR_BUILD_DISK_IOPS IOPS, $OPERATOR_BUILD_DISK_MBPS MB/s)
build mount: $OPERATOR_BUILD_MOUNT
benchmark service: gantry-benchmark-operator.service
image pool service: gantry-benchmark-image-builder.service
start command:
  az vm run-command invoke -g $AZURE_RESOURCE_GROUP -n $OPERATOR_VM_NAME --command-id RunShellScript --scripts 'systemctl start --no-block gantry-benchmark-operator.service'
status command:
  AZURE_RESOURCE_GROUP=$AZURE_RESOURCE_GROUP OPERATOR_VM_NAME=$OPERATOR_VM_NAME make -C hack/gantry-benchmark operator-vm-status
watch command:
  AZURE_RESOURCE_GROUP=$AZURE_RESOURCE_GROUP OPERATOR_VM_NAME=$OPERATOR_VM_NAME make -C hack/gantry-benchmark operator-vm-watch
prebuild command:
  AZURE_RESOURCE_GROUP=$AZURE_RESOURCE_GROUP OPERATOR_VM_NAME=$OPERATOR_VM_NAME GANTRY_IMAGE_POOL_COUNT=10 make -C hack/gantry-benchmark operator-vm-prebuild
run from pool command:
  AZURE_RESOURCE_GROUP=$AZURE_RESOURCE_GROUP OPERATOR_VM_NAME=$OPERATOR_VM_NAME GANTRY_ONLY_BASELINE_RUN_ID=<run-id> make -C hack/gantry-benchmark operator-vm-run-pool
pool status command:
  AZURE_RESOURCE_GROUP=$AZURE_RESOURCE_GROUP OPERATOR_VM_NAME=$OPERATOR_VM_NAME make -C hack/gantry-benchmark operator-vm-image-pool-status
SUMMARY
