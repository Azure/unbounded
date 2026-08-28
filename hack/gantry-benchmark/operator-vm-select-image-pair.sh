#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -Eeuo pipefail

if (($# != 9)); then
  echo "usage: operator-vm-select-image-pair.sh <artifact-root> <mode> <node-count> <platform> <size-mib> <layers> <repository> <baseline-acr> <gantry-acr>" >&2
  exit 2
fi

artifact_root=$1
mode=$2
node_count=$3
platform=$4
size_mib=$5
layers=$6
repository=$7
baseline_acr=$8
gantry_acr=$9
found=false

while IFS= read -r state_file; do
  selection=$(jq -er \
    --arg mode "$mode" \
    --argjson node_count "$node_count" \
    --arg platform "$platform" \
    --argjson size_mib "$size_mib" \
    --argjson layers "$layers" \
    --arg repository "$repository" \
    --arg baseline_acr "$baseline_acr" \
    --arg gantry_acr "$gantry_acr" \
    'select(
      .mode == $mode and
      .node_count == $node_count and
      .image_platform == $platform and
      .image_size_mib == $size_mib and
      .image_layers == $layers and
      .workload_repository == $repository and
      .baseline_acr_login_server == $baseline_acr and
      .gantry_acr_login_server == $gantry_acr and
      ((.workload_comparison_mode // "") == "" or .workload_comparison_mode == "identical_payload") and
      (.baseline_image | test("^" + ($baseline_acr | gsub("\\."; "\\.")) + "/" + $repository + "@sha256:[0-9a-f]{64}$")) and
      (.gantry_cold_image | test("^" + ($gantry_acr | gsub("\\."; "\\.")) + "/" + $repository + "@sha256:[0-9a-f]{64}$")) and
      (.workload_payload_sha256 | test("^sha256:[0-9a-f]{64}$"))
    ) | [.run_id, .baseline_image, .gantry_cold_image, .workload_payload_sha256] | @tsv' \
    "$state_file" 2>/dev/null || true)
  if [[ -n "$selection" ]]; then
    printf '%s\n' "$selection"
    found=true
  fi
done < <(find "$artifact_root" -mindepth 2 -maxdepth 2 -type f -name state.json -printf '%p\n' 2>/dev/null | sort -r)

[[ "$found" == true ]]
