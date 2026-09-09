#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

configure_topology() {
  AKS_NODE_POOL_NAME=${AKS_NODE_POOL_NAME:-system}
  AKS_SYSTEM_NODE_COUNT=${AKS_SYSTEM_NODE_COUNT:-0}
  AKS_SYSTEM_NODE_VM_SIZE=${AKS_SYSTEM_NODE_VM_SIZE:-Standard_D16s_v3}
  AKS_SCALE_BATCH_SIZE=${AKS_SCALE_BATCH_SIZE:-500}
  AKS_OUTBOUND_IP_COUNT=${AKS_OUTBOUND_IP_COUNT:-1}
  AKS_OUTBOUND_PORTS=${AKS_OUTBOUND_PORTS:-0}
  AKS_LOAD_BALANCER_BACKEND_POOL_TYPE=${AKS_LOAD_BALANCER_BACKEND_POOL_TYPE:-nodeIPConfiguration}
  BENCHMARK_NODE_LABEL=${BENCHMARK_NODE_LABEL:-}
  local value
  for value in "$AKS_NODE_COUNT" "$BENCHMARK_NODE_COUNT" "$AKS_SCALE_BATCH_SIZE" "$AKS_OUTBOUND_IP_COUNT"; do
    [[ "$value" =~ ^[1-9][0-9]*$ ]] || { echo "node counts, batch size and outbound IP count must be positive integers" >&2; return 2; }
  done
  [[ "$AKS_SYSTEM_NODE_COUNT" =~ ^(0|[1-9][0-9]*)$ && "$AKS_OUTBOUND_PORTS" =~ ^(0|[1-9][0-9]*)$ ]] || return 2
  ((AKS_NODE_COUNT <= 5000 && AKS_SYSTEM_NODE_COUNT <= 1000 && AKS_SCALE_BATCH_SIZE <= 500)) || {
    echo "maximum: 5000 cluster nodes, 1000 system nodes, 500 nodes per scale batch" >&2; return 2;
  }
  ((AKS_NODE_COUNT == AKS_SYSTEM_NODE_COUNT + BENCHMARK_NODE_COUNT)) || {
    echo "AKS_NODE_COUNT must equal system plus benchmark node counts" >&2; return 2;
  }
  if ((AKS_SYSTEM_NODE_COUNT > 0)); then
    ((AKS_SYSTEM_NODE_COUNT >= 3)) || { echo "dedicated system pool requires at least three nodes" >&2; return 2; }
    BENCHMARK_NODE_LABEL=gantry-benchmark
  elif ((AKS_NODE_COUNT > 1000)); then
    echo "more than 1000 nodes requires a dedicated system pool" >&2
    return 2
  fi
  ((AKS_OUTBOUND_IP_COUNT <= 100 && AKS_OUTBOUND_PORTS % 8 == 0)) || return 2
  [[ "$AKS_LOAD_BALANCER_BACKEND_POOL_TYPE" == nodeIPConfiguration || "$AKS_LOAD_BALANCER_BACKEND_POOL_TYPE" == nodeIP ]] || {
    echo "load balancer backend pool type must be nodeIPConfiguration or nodeIP" >&2
    return 2
  }
  if ((AKS_NODE_COUNT > 1000)) && [[ "$AKS_LOAD_BALANCER_BACKEND_POOL_TYPE" != nodeIP ]]; then
    echo "large clusters require nodeIP load balancer backend membership" >&2
    return 2
  fi
  if ((AKS_OUTBOUND_PORTS > 0)); then
    ((AKS_OUTBOUND_IP_COUNT * 64000 >= AKS_NODE_COUNT * AKS_OUTBOUND_PORTS)) || {
      echo "outbound SNAT allocation cannot support all nodes" >&2; return 2;
    }
  elif ((AKS_NODE_COUNT > 1000)); then
    echo "large clusters require explicit outbound ports" >&2
    return 2
  fi
  POOL_NAMES=("$AKS_NODE_POOL_NAME")
  POOL_COUNTS=("$AKS_NODE_COUNT")
  POOL_SIZES=("$AKS_NODE_VM_SIZE")
  POOL_MODES=(System)
  if ((AKS_SYSTEM_NODE_COUNT > 0)); then
    POOL_COUNTS=("$AKS_SYSTEM_NODE_COUNT")
    POOL_SIZES=("$AKS_SYSTEM_NODE_VM_SIZE")
    local remaining=$BENCHMARK_NODE_COUNT pool_number=1 pool_count
    while ((remaining > 0)); do
      pool_count=$remaining
      ((pool_count <= 1000)) || pool_count=1000
      POOL_NAMES+=("bench$pool_number")
      POOL_COUNTS+=("$pool_count")
      POOL_SIZES+=("$AKS_NODE_VM_SIZE")
      POOL_MODES+=(User)
      remaining=$((remaining - pool_count))
      pool_number=$((pool_number + 1))
    done
  fi
  [[ "$AKS_NODE_POOL_NAME" =~ ^[a-z][a-z0-9]{0,11}$ && ! "$AKS_NODE_POOL_NAME" =~ ^bench[0-9]+$ ]] || {
    echo "invalid or reserved system pool name" >&2; return 2;
  }
  jq -en --arg vnet "$VNET_CIDR" --arg nodes "$AKS_SUBNET_CIDR" --arg pods "$POD_CIDR" \
    --arg services "$SERVICE_CIDR" --arg endpoints "$PRIVATE_ENDPOINT_SUBNET_CIDR" \
    --arg operator "$OPERATOR_SUBNET_CIDR" --argjson count "$AKS_NODE_COUNT" '
    def cidr:
      split("/") as $parts |
      ($parts[0] | split(".") | map(tonumber)) as $octets |
      ($parts[1] | tonumber) as $prefix |
      if ($parts | length) != 2 or ($octets | length) != 4 or
        any($octets[]; . < 0 or . > 255 or floor != .) or
        $prefix < 0 or $prefix > 32 or ($prefix | floor) != $prefix then error("invalid CIDR") else . end |
      (reduce $octets[] as $octet (0; . * 256 + $octet)) as $start |
      pow(2; 32 - $prefix) as $size |
      if $start % $size != 0 then error("unaligned CIDR") else {start:$start,end:($start+$size),size:$size} end;
    def overlaps($left; $right): $left.start < $right.end and $right.start < $left.end;
    ($vnet | cidr) as $network |
    ([$nodes,$endpoints,$operator] | map(cidr)) as $subnets |
    ($pods | cidr) as $podnet |
    ($services | cidr) as $servicenet |
    ($subnets + [$podnet,$servicenet]) as $ranges |
    ($subnets[0].size - 5 >= $count) and ($podnet.size / 256 >= $count) and
    all($subnets[]; .start >= $network.start and .end <= $network.end) and
    (overlaps($network; $podnet) | not) and (overlaps($network; $servicenet) | not) and
    all(range(0; $ranges|length); . as $index |
      all(range($index+1; $ranges|length); overlaps($ranges[$index]; $ranges[.]) | not))
  ' >/dev/null || { echo "CIDRs are invalid, overlapping, or too small for the node count" >&2; return 2; }
}