#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

azure_read() { timeout 30s az "$@"; }
azure_submit() {
  local status
  timeout 55s az "$@" && return
  status=$?
  if ((status == 124)); then
    log "Azure CLI submission timed out locally; checking the resource operation state"
    return 0
  fi
  return "$status"
}

wait_for_aks_operation() {
  local pool=${1:-} state deadline=$((SECONDS + 7200))
  while ((SECONDS < deadline)); do
    if [[ -n "$pool" ]]; then
      state=$(azure_read aks nodepool show -g "$AZURE_RESOURCE_GROUP" --cluster-name "$AZURE_AKS_CLUSTER_NAME" -n "$pool" --query provisioningState -o tsv) || return
    else
      state=$(azure_read aks show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" --query provisioningState -o tsv) || return
    fi
    log "AKS ${pool:-cluster} provisioning: $state"
    case "$state" in
      Succeeded) return 0 ;;
      Failed|Canceled) echo "AKS operation failed; inspect Azure error before resuming" >&2; return 1 ;;
    esac
    sleep 30
  done
  echo "AKS wait deadline reached; remote operation may still be active" >&2
  return 1
}

wait_for_pool_nodes() {
  local pool=$1 expected=$2 counts deadline=$((SECONDS + 3600))
  while ((SECONDS < deadline)); do
    counts=$(kubectl --request-timeout=30s get nodes -l "agentpool=$pool" -o json | jq -r \
      '[.items[] | select(.spec.unschedulable != true and any(.status.conditions[]; .type == "Ready" and .status == "True"))] | length') || return
    log "AKS pool $pool Ready: $counts/$expected"
    [[ "$counts" == "$expected" ]] && return 0
    sleep 30
  done
  echo "pool $pool readiness deadline reached" >&2
  return 1
}

validate_pool() {
  local index=$1 pool_json=$2 subnet_id=$3
  assert_equal "${POOL_NAMES[index]} VM size" "$(jq -r .vmSize <<<"$pool_json")" "${POOL_SIZES[index]}"
  assert_equal "${POOL_NAMES[index]} mode" "$(jq -r .mode <<<"$pool_json")" "${POOL_MODES[index]}"
  assert_equal "${POOL_NAMES[index]} VMSS type" "$(jq -r .typePropertiesType <<<"$pool_json")" VirtualMachineScaleSets
  assert_equal "${POOL_NAMES[index]} max pods" "$(jq -r .maxPods <<<"$pool_json")" "$AKS_MAX_PODS"
  assert_equal "${POOL_NAMES[index]} OS disk" "$(jq -r .osDiskSizeGb <<<"$pool_json")" "$AKS_NODE_OS_DISK_GB"
  assert_equal "${POOL_NAMES[index]} OS disk type" "$(jq -r .osDiskType <<<"$pool_json")" Managed
  assert_equal "${POOL_NAMES[index]} OS SKU" "$(jq -r .osSku <<<"$pool_json")" Ubuntu
  assert_equal "${POOL_NAMES[index]} subnet" "$(jq -r .vnetSubnetId <<<"$pool_json")" "$subnet_id"
  if ((AKS_SYSTEM_NODE_COUNT > 0)); then
    local role=worker
    ((index != 0)) || role=system
    assert_equal "${POOL_NAMES[index]} benchmark label" "$(jq -r '.nodeLabels["gantry-benchmark"]' <<<"$pool_json")" "$role"
    if ((index == 0)); then
      jq -e '.nodeTaints | index("CriticalAddonsOnly=true:NoSchedule") != null' <<<"$pool_json" >/dev/null
    else
      jq -e '(.nodeTaints // []) | length == 0' <<<"$pool_json" >/dev/null
    fi
  fi
}

reconcile_pool() {
  local index=$1 subnet_id=$2 pool desired next current pool_json
  pool=${POOL_NAMES[index]}
  desired=${POOL_COUNTS[index]}
  if ! pool_json=$(azure_read aks nodepool show -g "$AZURE_RESOURCE_GROUP" \
    --cluster-name "$AZURE_AKS_CLUSTER_NAME" -n "$pool" -o json 2>/dev/null); then
    next=$desired
    ((next <= AKS_SCALE_BATCH_SIZE)) || next=$AKS_SCALE_BATCH_SIZE
    log "submitting node pool $pool with $next nodes"
    azure_submit aks nodepool add -g "$AZURE_RESOURCE_GROUP" --cluster-name "$AZURE_AKS_CLUSTER_NAME" \
      -n "$pool" --mode User --node-count "$next" --node-vm-size "${POOL_SIZES[index]}" \
      --node-osdisk-type Managed --node-osdisk-size "$AKS_NODE_OS_DISK_GB" --os-sku Ubuntu \
      --max-pods "$AKS_MAX_PODS" --vnet-subnet-id "$subnet_id" --labels gantry-benchmark=worker \
      --no-wait --only-show-errors -o none
  fi
  wait_for_aks_operation "$pool"
  pool_json=$(azure_read aks nodepool show -g "$AZURE_RESOURCE_GROUP" \
    --cluster-name "$AZURE_AKS_CLUSTER_NAME" -n "$pool" -o json)
  validate_pool "$index" "$pool_json" "$subnet_id"
  current=$(jq -r .count <<<"$pool_json")
  ((current <= desired)) || { echo "$pool has $current nodes above target $desired; refusing scale-down" >&2; return 1; }
  wait_for_pool_nodes "$pool" "$current"
  while ((current < desired)); do
    next=$((current + AKS_SCALE_BATCH_SIZE))
    ((next <= desired)) || next=$desired
    log "submitting scale $pool: $current -> $next"
    azure_submit aks nodepool scale -g "$AZURE_RESOURCE_GROUP" --cluster-name "$AZURE_AKS_CLUSTER_NAME" \
      -n "$pool" --node-count "$next" --no-wait --only-show-errors -o none
    wait_for_aks_operation "$pool"
    wait_for_pool_nodes "$pool" "$next"
    current=$next
  done
}

ensure_aks() {
  local subnet_id cluster_json pools_json index pool initial status=0
  local -a pool_pids=()
  subnet_id=$(azure_read network vnet subnet show -g "$AZURE_RESOURCE_GROUP" --vnet-name "$VNET_NAME" -n "$AKS_SUBNET_NAME" --query id -o tsv)
  local -a system_args=()
  if ((AKS_SYSTEM_NODE_COUNT > 0)); then
    system_args=(--nodepool-labels gantry-benchmark=system --nodepool-taints CriticalAddonsOnly=true:NoSchedule)
  fi
  local exists
  exists=$(azure_read resource list -g "$AZURE_RESOURCE_GROUP" --resource-type Microsoft.ContainerService/managedClusters \
    --query "[?name=='$AZURE_AKS_CLUSTER_NAME'] | length(@)" -o tsv)
  if [[ "$exists" == 0 ]]; then
    initial=${POOL_COUNTS[0]}
    ((initial <= AKS_SCALE_BATCH_SIZE)) || initial=$AKS_SCALE_BATCH_SIZE
    log "submitting AKS creation with $initial ${POOL_SIZES[0]} system nodes"
    azure_submit aks create -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" -l "$AZURE_LOCATION" \
      --tier standard --enable-managed-identity --node-resource-group "$AZURE_NODE_RESOURCE_GROUP" \
      --vm-set-type VirtualMachineScaleSets --nodepool-name "$AKS_NODE_POOL_NAME" --node-count "$initial" \
      --node-vm-size "${POOL_SIZES[0]}" --node-osdisk-type Managed \
      --node-osdisk-size "$AKS_NODE_OS_DISK_GB" --max-pods "$AKS_MAX_PODS" \
      --os-sku Ubuntu --network-plugin azure --network-plugin-mode overlay --network-dataplane azure \
      --pod-cidr "$POD_CIDR" --service-cidr "$SERVICE_CIDR" --dns-service-ip "$DNS_SERVICE_IP" \
      --vnet-subnet-id "$subnet_id" --load-balancer-sku standard --outbound-type loadBalancer \
      --load-balancer-backend-pool-type "$AKS_LOAD_BALANCER_BACKEND_POOL_TYPE" \
      --load-balancer-managed-outbound-ip-count "$AKS_OUTBOUND_IP_COUNT" --load-balancer-outbound-ports "$AKS_OUTBOUND_PORTS" \
      --kubernetes-version "$AKS_KUBERNETES_VERSION" "${system_args[@]}" --no-ssh-key --no-wait --only-show-errors -o none
  fi
  wait_for_aks_operation
  cluster_json=$(azure_read aks show -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" -o json)
  assert_equal "AKS location" "$(jq -r .location <<<"$cluster_json")" "$AZURE_LOCATION"
  local actual_version
  actual_version=$(jq -r .kubernetesVersion <<<"$cluster_json")
  [[ "$actual_version" == "$AKS_KUBERNETES_VERSION" || "$actual_version" == "$AKS_KUBERNETES_VERSION."* ]] || {
    echo "AKS version $actual_version does not match $AKS_KUBERNETES_VERSION" >&2; return 1;
  }
  assert_equal "AKS pod CIDR" "$(jq -r .networkProfile.podCidr <<<"$cluster_json")" "$POD_CIDR"
  assert_equal "AKS service CIDR" "$(jq -r .networkProfile.serviceCidr <<<"$cluster_json")" "$SERVICE_CIDR"
  assert_equal "AKS node resource group" "$(jq -r .nodeResourceGroup <<<"$cluster_json")" "$AZURE_NODE_RESOURCE_GROUP"
  assert_equal "AKS network mode" "$(jq -r .networkProfile.networkPluginMode <<<"$cluster_json")" overlay
  assert_equal "AKS load balancer backend pool type" \
    "$(jq -r .networkProfile.loadBalancerProfile.backendPoolType <<<"$cluster_json")" \
    "$AKS_LOAD_BALANCER_BACKEND_POOL_TYPE"
  assert_equal "AKS outbound IP count" "$(jq -r .networkProfile.loadBalancerProfile.managedOutboundIPs.count <<<"$cluster_json")" "$AKS_OUTBOUND_IP_COUNT"
  assert_equal "AKS outbound ports" "$(jq -r .networkProfile.loadBalancerProfile.allocatedOutboundPorts <<<"$cluster_json")" "$AKS_OUTBOUND_PORTS"
  mkdir -p "$(dirname "$KUBECONFIG")"
  azure_read aks get-credentials -g "$AZURE_RESOURCE_GROUP" -n "$AZURE_AKS_CLUSTER_NAME" --admin --file "$KUBECONFIG" --overwrite-existing --only-show-errors
  chmod 0600 "$KUBECONFIG"
  export KUBECONFIG
  pools_json=$(azure_read aks nodepool list -g "$AZURE_RESOURCE_GROUP" --cluster-name "$AZURE_AKS_CLUSTER_NAME" -o json)
  while IFS= read -r pool; do
    [[ " ${POOL_NAMES[*]} " == *" $pool "* ]] || { echo "unexpected node pool $pool; refusing to reconcile" >&2; return 1; }
  done < <(jq -r '.[].name' <<<"$pools_json")
  reconcile_pool 0 "$subnet_id"
  for ((index = 1; index < ${#POOL_NAMES[@]}; index++)); do
    reconcile_pool "$index" "$subnet_id" &
    pool_pids+=("$!")
  done
  for index in "${!pool_pids[@]}"; do
    if ! wait "${pool_pids[index]}"; then
      echo "node pool ${POOL_NAMES[index + 1]} reconciliation failed" >&2
      status=1
    fi
  done
  return "$status"
}