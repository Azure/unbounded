#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# restart-kubelet-vmss.sh - Restart kubelet on all instances in an Azure VMSS
# using az vmss run-command.
#
# Usage:
#   ./scripts/restart-kubelet-vmss.sh -g <resource-group> -n <vmss-name> [-s <subscription>] [--parallel <count>]

set -euo pipefail

RESOURCE_GROUP=""
VMSS_NAME=""
SUBSCRIPTION=""
PARALLEL=20

usage() {
    cat <<EOF
Usage: $(basename "$0") -g <resource-group> -n <vmss-name> [options]

Restart kubelet on all instances in an Azure VMSS using run-command.

Required:
  -g, --resource-group    Resource group containing the VMSS
  -n, --name              VMSS name

Options:
  -s, --subscription      Azure subscription ID
  -p, --parallel          Max parallel run-command invocations (default: $PARALLEL)
  -h, --help              Show this help
EOF
    exit 1
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -g|--resource-group) RESOURCE_GROUP="$2"; shift 2 ;;
        -n|--name) VMSS_NAME="$2"; shift 2 ;;
        -s|--subscription) SUBSCRIPTION="$2"; shift 2 ;;
        -p|--parallel) PARALLEL="$2"; shift 2 ;;
        -h|--help) usage ;;
        *) echo "Unknown option: $1"; usage ;;
    esac
done

if [[ -z "$RESOURCE_GROUP" || -z "$VMSS_NAME" ]]; then
    echo "Error: --resource-group and --name are required."
    usage
fi

SUB_ARGS=()
if [[ -n "$SUBSCRIPTION" ]]; then
    SUB_ARGS=(--subscription "$SUBSCRIPTION")
fi

echo "==> Listing instances in VMSS $VMSS_NAME..."
INSTANCE_IDS=$(az vmss list-instances \
    -g "$RESOURCE_GROUP" \
    -n "$VMSS_NAME" \
    "${SUB_ARGS[@]}" \
    --query "[].instanceId" \
    -o tsv 2>/dev/null)

if [[ -z "$INSTANCE_IDS" ]]; then
    echo "No instances found in VMSS $VMSS_NAME."
    exit 0
fi

TOTAL=$(echo "$INSTANCE_IDS" | wc -l)
echo "==> Found $TOTAL instances. Restarting kubelet with --parallel=$PARALLEL..."

RUNNING=0
COMPLETED=0
FAILED=0
PIDS=()
INSTANCE_FOR_PID=()

for INSTANCE_ID in $INSTANCE_IDS; do
    # Throttle parallel invocations
    while [[ $RUNNING -ge $PARALLEL ]]; do
        # Wait for any child to finish
        for i in "${!PIDS[@]}"; do
            if ! kill -0 "${PIDS[$i]}" 2>/dev/null; then
                wait "${PIDS[$i]}" 2>/dev/null && COMPLETED=$((COMPLETED + 1)) || FAILED=$((FAILED + 1))
                unset "PIDS[$i]"
                unset "INSTANCE_FOR_PID[$i]"
                RUNNING=$((RUNNING - 1))
            fi
        done
        # Compact arrays
        PIDS=("${PIDS[@]+"${PIDS[@]}"}")
        INSTANCE_FOR_PID=("${INSTANCE_FOR_PID[@]+"${INSTANCE_FOR_PID[@]}"}")
        sleep 0.5
    done

    az vmss run-command invoke \
        -g "$RESOURCE_GROUP" \
        -n "$VMSS_NAME" \
        --instance-id "$INSTANCE_ID" \
        --command-id RunShellScript \
        --scripts "rm -f /etc/cni/net.d/* && systemctl restart containerd && systemctl restart kubelet" \
        "${SUB_ARGS[@]}" \
        -o none 2>/dev/null &

    PIDS+=($!)
    INSTANCE_FOR_PID+=("$INSTANCE_ID")
    RUNNING=$((RUNNING + 1))
    echo "  Instance $INSTANCE_ID: restarting kubelet ($((COMPLETED + RUNNING))/$TOTAL)"
done

# Wait for remaining
for pid in "${PIDS[@]+"${PIDS[@]}"}"; do
    wait "$pid" 2>/dev/null && COMPLETED=$((COMPLETED + 1)) || FAILED=$((FAILED + 1))
done

echo ""
echo "==> Done. $COMPLETED succeeded, $FAILED failed out of $TOTAL instances."
