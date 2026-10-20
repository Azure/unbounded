#!/usr/bin/env bash
set -uo pipefail

stage="${PROJECT_SIGNAL_STAGE_NAME:-}"
verdict=Pass
reason=Healthy
details=

emit_stage() {
  local status="$1"
  local event_reason="${2:-}"
  local message="${3:-}"

  printf 'PROJECT_SIGNAL_STAGE {"name":"%s","status":"%s","at_epoch":%s,"reason":"%s","message":"%s"}\n' \
    "${stage}" "${status}" "$(date +%s)" "${event_reason}" "${message}"
}

finish() {
  printf '%s' "${verdict}" > /results/verdict
  printf '%s' "${reason}" > /results/reason
  printf 'stage=%s verdict=%s reason=%s\n%s\n' \
    "${stage}" "${verdict}" "${reason}" "${details}" > /results/measurements.txt
  printf 'verdict=%s reason=%s stage=%s\n' "${verdict}" "${reason}" "${stage}" > /dev/termination-log

  if [[ "${verdict}" == Pass ]]; then
    emit_stage passed "${reason}" "${details//$'\n'/; }"
  else
    emit_stage failed "${reason}" "${details//$'\n'/; }"
  fi
}

validate_guards() {
  if [[ -z "${EXPECTED_GPU_PRODUCT_REGEX:-}" ||
        "${EXPECTED_GPU_PRODUCT_REGEX}" == *REPLACE_WITH_VERIFIED* ]]; then
    verdict=Fail
    reason=ProductGuardNotConfigured

    return 1
  fi

  if ! [[ "${EXPECTED_GPU_COUNT:-}" =~ ^[1-9][0-9]*$ ]]; then
    verdict=Fail
    reason=CountGuardNotConfigured

    return 1
  fi

  if ! command -v nvidia-smi >/dev/null 2>&1; then
    verdict=Fail
    reason=NvidiaSMIMissing

    return 1
  fi
}

query_visible_gpus() {
  local product_output gpu_output
  if ! product_output="$(nvidia-smi --query-gpu=name --format=csv,noheader)" ||
     ! gpu_output="$(
    nvidia-smi \
      --query-gpu=index,uuid,name,pci.bus_id,pstate,temperature.gpu,driver_version \
      --format=csv,noheader,nounits
  )"; then
    verdict=Fail
    reason=GPUQueryFailed
    details="nvidia-smi returned a nonzero exit status"
    return 1
  fi
  if [[ -z "${product_output}" || -z "${gpu_output}" ]]; then
    verdict=Inconclusive
    reason=GPUMeasurementsMissing
    return 1
  fi
  mapfile -t products <<< "${product_output}"
  mapfile -t gpu_rows <<< "${gpu_output}"
  details="$(printf '%s\n' "${gpu_rows[@]}")"

  if [[ "${#products[@]}" -ne "$1" || "${#gpu_rows[@]}" -ne "$1" ]]; then
    verdict=Fail
    reason=RuntimeGPUCountMismatch

    return 1
  fi

  local product
  for product in "${products[@]}"; do
    if ! grep -Eq "${EXPECTED_GPU_PRODUCT_REGEX}" <<<"${product}"; then
      verdict=Fail
      reason=GPUProductMismatch

      return 1
    fi
  done
}

emit_stage running

case "${stage}" in
preflight)
  if validate_guards; then
    if [[ "${OBSERVED_GPU_CAPACITY:-}" != "${EXPECTED_GPU_COUNT}" ]]; then
      verdict=Fail
      reason=NodeGPUCapacityMismatch
      details="expected=${EXPECTED_GPU_COUNT} observed-capacity=${OBSERVED_GPU_CAPACITY:-unset} observed-allocatable=${OBSERVED_GPU_ALLOCATABLE:-unset}"
    elif [[ "${OBSERVED_GPU_ALLOCATABLE:-}" != "${EXPECTED_GPU_COUNT}" ]]; then
      verdict=Fail
      reason=NodeGPUAllocatableMismatch
      details="expected=${EXPECTED_GPU_COUNT} observed-capacity=${OBSERVED_GPU_CAPACITY:-unset} observed-allocatable=${OBSERVED_GPU_ALLOCATABLE:-unset}"
    else
      query_visible_gpus 1 || true
      if [[ "${verdict}" == Pass ]]; then
        reason=NodeInventoryHealthy
        details="expected=${EXPECTED_GPU_COUNT} observed-capacity=${OBSERVED_GPU_CAPACITY} observed-allocatable=${OBSERVED_GPU_ALLOCATABLE}"$'\n'"${details}"
      fi
    fi
  fi
  ;;
workload)
  if validate_guards; then
    query_visible_gpus "${EXPECTED_GPU_COUNT}" || true
  fi

  if [[ "${verdict}" == Pass ]]; then
    pids=()
    for ((index = 0; index < EXPECTED_GPU_COUNT; index++)); do
      nvidia-smi -i "${index}" \
        --query-gpu=index,uuid,name,pci.bus_id,pstate,temperature.gpu \
        --format=csv,noheader,nounits > "/results/gpu-${index}.txt" &
      pids+=("$!")
    done

    for pid in "${pids[@]}"; do
      if ! wait "${pid}"; then
        verdict=Fail
        reason=ParallelGPUHealthQueryFailed
      fi
    done
    for ((index = 0; index < EXPECTED_GPU_COUNT; index++)); do
      if [[ ! -s "/results/gpu-${index}.txt" ]]; then
        if [[ "${verdict}" == Pass ]]; then
          verdict=Inconclusive
          reason=GPUMeasurementsMissing
        fi
      else
        details+=$'\n'"device_index=${index} $(<"/results/gpu-${index}.txt")"
      fi
    done
  fi

  if [[ "${verdict}" == Pass ]]; then
    reason=FullNodeWorkloadPassed
  fi
  ;;
health-after)
  if validate_guards; then
    query_visible_gpus 1 || true
  fi

  if [[ "${verdict}" == Pass ]]; then
    reason=FinalHealthPassed
  fi
  ;;
*)
  verdict=Fail
  reason=UnknownStage
  details="stage=${stage:-unset}"
  ;;
esac

finish

# Hardware failures are typed evidence, not workflow infrastructure errors.
exit 0
