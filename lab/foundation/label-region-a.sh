#!/usr/bin/env bash
# Apply the Region A node labels (lab.unbounded.cloud/region=a,
# lab.unbounded.cloud/hardware-class=dgx-spark-gb10) to the Spark nodes.
# Idempotent.
set -euo pipefail

NODES=(spark-3d37 spark-2c24)

for node in "${NODES[@]}"; do
  kubectl label node "$node" \
    lab.unbounded.cloud/region=a \
    lab.unbounded.cloud/hardware-class=dgx-spark-gb10 \
    --overwrite
done

echo
echo "Verify with:"
echo "  kubectl get nodes -L lab.unbounded.cloud/region,lab.unbounded.cloud/hardware-class"
