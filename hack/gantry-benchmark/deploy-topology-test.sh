#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. "$script_dir/deploy-topology.sh"
. "$script_dir/deploy-pools.sh"
. "$script_dir/deploy.env.example"
configure_topology
[[ ${#POOL_NAMES[@]} == 1 && ${POOL_COUNTS[0]} == 1000 ]]
AKS_NODE_COUNT=5000
BENCHMARK_NODE_COUNT=4997
AKS_NODE_POOL_NAME=system
AKS_SYSTEM_NODE_COUNT=3
AKS_NODE_VM_SIZE=Standard_D4s_v3
AKS_SUBNET_CIDR=10.224.0.0/19
POD_CIDR=10.64.0.0/11
AKS_OUTBOUND_IP_COUNT=20
AKS_OUTBOUND_PORTS=256
AKS_LOAD_BALANCER_BACKEND_POOL_TYPE=nodeIP
configure_topology
[[ ${#POOL_NAMES[@]} == 6 && ${POOL_COUNTS[*]} == '3 1000 1000 1000 1000 997' ]]
[[ ${POOL_SIZES[0]} == Standard_D16s_v3 && ${POOL_SIZES[5]} == Standard_D4s_v3 ]]
[[ $BENCHMARK_NODE_LABEL == gantry-benchmark ]]
expect_failure() {
  if (export "$1"; configure_topology) >/dev/null 2>&1; then
    echo "unexpectedly accepted $1" >&2
    exit 1
  fi
}
expect_failure AKS_NODE_COUNT=5001
expect_failure BENCHMARK_NODE_COUNT=5000
expect_failure AKS_SUBNET_CIDR=10.224.0.0/20
expect_failure POD_CIDR=10.64.0.0/12
expect_failure PRIVATE_ENDPOINT_SUBNET_CIDR=10.224.0.0/27
expect_failure OPERATOR_SUBNET_CIDR=10.240.0.0/24
expect_failure AKS_OUTBOUND_IP_COUNT=19
expect_failure AKS_LOAD_BALANCER_BACKEND_POOL_TYPE=nodeIPConfiguration
expect_failure AKS_SYSTEM_NODE_COUNT=1
expect_failure AKS_SCALE_BATCH_SIZE=0

assert_equal() { [[ "$2" == "$3" ]] || return 1; }
validate_pool 0 '{
  "vmSize":"Standard_D16s_v3","mode":"System","type":"Microsoft.ContainerService/managedClusters/agentPools",
  "typePropertiesType":"VirtualMachineScaleSets","maxPods":250,"osDiskSizeGb":512,"osDiskType":"Managed",
  "osSku":"Ubuntu","vnetSubnetId":"subnet-id","nodeLabels":{"gantry-benchmark":"system"},
  "nodeTaints":["CriticalAddonsOnly=true:NoSchedule"]
}' subnet-id

log() { :; }
timeout() { return 124; }
azure_submit aks create --no-wait
echo 'deployment topology tests passed'