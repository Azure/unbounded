#!/usr/bin/env bash
set -uo pipefail

verdict=Pass
reason=BurnCanaryPassed
details=
stop_file=/work/stop
gpu_log=/work/gpu-burn.log
declare -A baseline_volatile_ecc=()
declare -A baseline_aggregate_ecc=()
declare -A power_limit_excess_samples=()
worker_pids=()
cpu_pids=()
component="${PROJECT_SIGNAL_BURN_COMPONENT:-${1:-standalone}}"
gpu_completed=false
finished_normally=false

emit_stage() {
  local name="$1"
  local status="$2"
  local event_reason="${3:-}"
  local message="${4:-}"

  printf 'PROJECT_SIGNAL_STAGE {"name":"%s","status":"%s","at_epoch":%s,"reason":"%s","message":"%s"}\n' \
    "${name}" "${status}" "$(date +%s)" "${event_reason}" "${message}"
}

fail() {
  if [[ "${verdict}" == Pass ]]; then
    verdict=Fail
    reason="$1"
    details="${2:-}"
  fi
}

trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "${value}"
}

xid_count() {
  local messages
  messages="$(dmesg 2>/dev/null)" || return 1
  grep -Eic 'NVRM: Xid|Xid \(' <<< "${messages}" || true
}

capture_telemetry() {
  local mode="$1"
  local rows row index product temperature margin power_draw power_limit power_limit_threshold
  local volatile_ecc aggregate_ecc throttle excess_samples

  if ! rows="$(
    nvidia-smi \
      --query-gpu=index,name,temperature.gpu,temperature.gpu.tlimit,power.draw,power.limit,ecc.errors.uncorrected.volatile.total,ecc.errors.uncorrected.aggregate.total,clocks_throttle_reasons.active \
      --format=csv,noheader,nounits
  )"; then
    fail TelemetryQueryFailed "nvidia-smi telemetry query failed"

    return 1
  fi

  if [[ "$(wc -l <<<"${rows}")" -ne "${EXPECTED_GPU_COUNT}" ]]; then
    fail RuntimeGPUCountMismatch "expected=${EXPECTED_GPU_COUNT} observed=$(wc -l <<<"${rows}")"

    return 1
  fi

  while IFS= read -r row; do
    IFS=',' read -r index product temperature margin power_draw power_limit volatile_ecc aggregate_ecc throttle <<<"${row}"
    index="$(trim "${index}")"
    product="$(trim "${product}")"
    temperature="$(trim "${temperature}")"
    margin="$(trim "${margin}")"
    power_draw="$(trim "${power_draw}")"
    power_limit="$(trim "${power_limit}")"
    volatile_ecc="$(trim "${volatile_ecc}")"
    aggregate_ecc="$(trim "${aggregate_ecc}")"
    throttle="$(trim "${throttle}")"

    if ! grep -Eq "${EXPECTED_GPU_PRODUCT_REGEX}" <<<"${product}"; then
      fail GPUProductMismatch "index=${index} product=${product}"
    elif ! [[ "${temperature}" =~ ^[0-9]+([.][0-9]+)?$ &&
              "${margin}" =~ ^[0-9]+([.][0-9]+)?$ &&
              "${power_draw}" =~ ^[0-9]+([.][0-9]+)?$ &&
              "${power_limit}" =~ ^[0-9]+([.][0-9]+)?$ &&
              "${volatile_ecc}" =~ ^[0-9]+$ &&
              "${aggregate_ecc}" =~ ^[0-9]+$ ]]; then
      fail SafetyTelemetryUnavailable "index=${index} row=${row}"
    elif ! awk -v margin="${margin}" 'BEGIN { exit !(margin > 0) }'; then
      fail ThermalLimitReached "index=${index} temperature=${temperature} margin=${margin}"
    fi

    power_limit_threshold="$(
      awk -v limit="${power_limit}" -v tolerance="${POWER_LIMIT_TOLERANCE_PERCENT}" \
        'BEGIN { printf "%.2f", limit * (100 + tolerance) / 100 }'
    )"
    if [[ "${mode}" == baseline ]]; then
      power_limit_excess_samples["${index}"]=0
    elif awk -v draw="${power_draw}" -v threshold="${power_limit_threshold}" \
      'BEGIN { exit !(draw > threshold) }'; then
      excess_samples=$(( ${power_limit_excess_samples["${index}"]:-0} + 1 ))
      power_limit_excess_samples["${index}"]="${excess_samples}"
      if (( excess_samples >= POWER_LIMIT_CONSECUTIVE_SAMPLES )); then
        fail PowerLimitExceeded \
          "index=${index} draw=${power_draw} limit=${power_limit} threshold=${power_limit_threshold} consecutive_samples=${excess_samples}"
      fi
    else
      power_limit_excess_samples["${index}"]=0
    fi

    if [[ "${mode}" == baseline ]]; then
      baseline_volatile_ecc["${index}"]="${volatile_ecc}"
      baseline_aggregate_ecc["${index}"]="${aggregate_ecc}"
    elif (( volatile_ecc > baseline_volatile_ecc["${index}"] ||
            aggregate_ecc > baseline_aggregate_ecc["${index}"] )); then
      fail UncorrectableECCGrowth \
        "index=${index} volatile=${baseline_volatile_ecc["${index}"]}->${volatile_ecc} aggregate=${baseline_aggregate_ecc["${index}"]}->${aggregate_ecc}"
    fi

    printf 'PROJECT_SIGNAL_MEASUREMENT index=%s product=%s temperature=%s thermal_margin=%s power_draw=%s power_limit=%s power_limit_threshold=%s power_excess_samples=%s volatile_uncorrectable_ecc=%s aggregate_uncorrectable_ecc=%s throttle=%s\n' \
      "${index}" "${product// /_}" "${temperature}" "${margin}" "${power_draw}" "${power_limit}" \
      "${power_limit_threshold}" "${power_limit_excess_samples["${index}"]:-0}" \
      "${volatile_ecc}" "${aggregate_ecc}" "${throttle}"
  done <<<"${rows}"
}

stop_workers() {
  touch "${stop_file}" 2>/dev/null || true

  local pid
  for pid in "${worker_pids[@]:-}"; do
    if kill -0 "${pid}" 2>/dev/null; then
      kill "${pid}" 2>/dev/null || true
    fi
  done

  for pid in "${worker_pids[@]:-}"; do
    wait "${pid}" 2>/dev/null || true
  done
  worker_pids=()
}

pause_until_sample() {
  local until=$((SECONDS + $1))
  while (( SECONDS < until )) && [[ ! -e "${stop_file}" ]]; do
    sleep 0.1
  done
}

burn_log_excerpt() {
  if [[ -f "${gpu_log}" ]]; then
    tail -c 8192 "${gpu_log}" | tr '\r' '\n' | tail -n 200
  fi
}

finish_component() {
  local exit_code=$?
  stop_workers
  if (( exit_code != 0 )) && [[ "${verdict}" == Pass ]]; then
    verdict=Inconclusive
    reason=ComponentInterrupted
  fi
  {
    printf 'test=%s verdict=%s reason=%s iterations=%s\n' \
      "${component}" "${verdict}" "${reason}" "${iterations}"
    printf 'cpu_workers=%s memory_mib=%s storage_mib=%s\n' \
      "${CPU_WORKERS:-unset}" "${MEMORY_MIB:-unset}" "${STORAGE_MIB:-unset}"
    printf '%s\n' "${details}"
  } > "/results/${component}-measurements.txt"
  printf '%s' "${reason}" > "/results/${component}-reason"
  printf '%s' "${verdict}" > "/results/${component}-verdict.tmp"
  mv "/results/${component}-verdict.tmp" "/results/${component}-verdict"
  printf 'PROJECT_SIGNAL_TEST id=%s verdict=%s reason=%s iterations=%s\n' \
    "${component}" "${verdict}" "${reason}" "${iterations}"
}

run_component() {
  iterations=0
  reason=ComponentCompleted
  trap finish_component EXIT
  trap 'verdict=Inconclusive; reason=Canceled; exit 143' TERM
  trap 'verdict=Inconclusive; reason=Canceled; exit 130' INT

  while [[ ! -e /work/ready ]]; do
    if [[ -e "${stop_file}" ]]; then
      verdict=Inconclusive
      reason=PreflightFailed
      exit 0
    fi
    sleep 0.1
  done

  case "${component}" in
  memory-integrity|storage-integrity)
    if [[ "${component}" == memory-integrity ]]; then
      directory=/work/memory
      size="${MEMORY_MIB}"
      flags=()
    else
      directory=/work/storage
      size="${STORAGE_MIB}"
      flags=(oflag=direct conv=fsync)
    fi
    if ! mkdir -p "${directory}" ||
       ! dd if=/dev/zero of="${directory}/pattern.bin" bs=1M count="${size}" "${flags[@]}" status=none ||
       ! initial_hash="$(sha256sum "${directory}/pattern.bin")"; then
      fail IntegritySetupFailed "test=${component}"
      exit 0
    fi
    touch "/work/${component}.started"
    while [[ ! -e "${stop_file}" ]]; do
      if ! current_hash="$(sha256sum "${directory}/pattern.bin")" ||
         [[ "${current_hash}" != "${initial_hash}" ]]; then
        fail IntegrityMismatch "test=${component}"
        break
      fi
      iterations=$((iterations + 1))
      if [[ "${component}" == storage-integrity ]]; then
        pause_until_sample "${SAMPLE_INTERVAL_SECONDS}"
      fi
    done
    details="size_mib=${size} sha256=${initial_hash%% *}"
    ;;
  cpu-stress)
    for ((worker = 0; worker < CPU_WORKERS; worker++)); do
      (
        count=0
        while [[ ! -e "${stop_file}" ]]; do
          if ! dd if=/dev/zero bs=4M count=256 status=none | sha256sum >/dev/null; then
            exit 1
          fi
          count=$((count + 1))
          printf '%s' "${count}" > "/work/cpu-${worker}.iterations"
        done
      ) &
      worker_pids+=("$!")
    done
    touch "/work/${component}.started"
    while [[ ! -e "${stop_file}" ]]; do
      for pid in "${worker_pids[@]}"; do
        if ! kill -0 "${pid}" 2>/dev/null && [[ ! -e "${stop_file}" ]]; then
          fail HostStressFailed "worker_pid=${pid}"
          break
        fi
      done
      [[ "${verdict}" == Pass ]] || break
      sleep 0.1
    done
    if [[ "${verdict}" != Pass ]]; then
      touch "${stop_file}"
    fi
    for pid in "${worker_pids[@]}"; do
      if ! wait "${pid}"; then
        fail HostStressFailed "worker_pid=${pid}"
      fi
    done
    worker_pids=()
    for ((worker = 0; worker < CPU_WORKERS; worker++)); do
      if [[ ! -s "/work/cpu-${worker}.iterations" ]]; then
        fail HostStressMeasurementsMissing "worker=${worker}"
      else
        count="$(<"/work/cpu-${worker}.iterations")"
        iterations=$((iterations + count))
      fi
    done
    ;;
  *)
    fail UnknownBurnComponent "component=${component}"
    exit 0
    ;;
  esac

  if [[ "${verdict}" == Pass ]]; then
    if [[ ! -e /work/burn-complete ]]; then
      verdict=Inconclusive
      reason=GroupInterrupted
    elif (( iterations == 0 )); then
      verdict=Inconclusive
      reason=ComponentMeasurementsMissing
    fi
  fi
  exit 0
}

check_components() {
  local name result
  for name in cpu-stress memory-integrity storage-integrity; do
    if [[ -e "/results/${name}-verdict" ]]; then
      result="$(<"/results/${name}-reason")"
      fail HostTestExitedEarly "test=${name} reason=${result}"
      return 1
    fi
  done
  if [[ -e "${stop_file}" && ! -e /work/burn-complete ]]; then
    fail HostTestExitedEarly "A concurrent host test stopped the coupled workload"
    return 1
  fi
}

collect_component_results() {
  local name waited
  for ((waited = 0; waited < 120; waited++)); do
    if [[ -e /results/cpu-stress-verdict &&
          -e /results/memory-integrity-verdict &&
          -e /results/storage-integrity-verdict ]]; then
      break
    fi
    sleep 1
  done
  for name in cpu-stress memory-integrity storage-integrity; do
    if [[ ! -s "/results/${name}-verdict" ]]; then
      fail ComponentCleanupIncomplete "test=${name}"
    elif [[ "$(<"/results/${name}-verdict")" != Pass ]]; then
      fail HostTestFailed "test=${name} reason=$(<"/results/${name}-reason")"
    fi
  done
}

validate_gpu_measurements() {
  if grep -Eq 'WARNING!|DIED!' "${gpu_log}"; then
    fail GPUBurnComputationFailed "GPU Burn reported a computation error or a dead GPU worker"
  elif ! tr '\r' '\n' < "${gpu_log}" | awk -v expected="${EXPECTED_GPU_COUNT}" '
    /proc.d:/ {
      sub(/^.*proc.d: */, "")
      sub(/[[:space:]]+errors:.*/, "")
      count = split($0, devices, /[[:space:]]+-[[:space:]]+/)
      complete = count == expected
      for (i = 1; i <= count; i++) {
        if (devices[i] !~ /^[1-9][0-9]* \([1-9][0-9]* Gflop\/s\)/) complete = 0
      }
      if (complete) measured = 1
    }
    END { exit !measured }
  '; then
    fail GPUBurnMeasurementsMissing "No completed computation measurement for every assigned GPU"
  fi
}

finish() {
  local exit_code=$?
  stop_workers
  if [[ "${verdict}" == Pass ]] && { (( exit_code != 0 )) || ! "${finished_normally}"; }; then
    verdict=Inconclusive
    reason=BurnInterrupted
  fi
  if [[ "${component}" == gpu-burn ]]; then
    collect_component_results
  fi
  printf '%s' "${verdict}" > /results/verdict
  printf '%s' "${reason}" > /results/reason
  {
    printf 'verdict=%s reason=%s\n' "${verdict}" "${reason}"
    printf 'gpu_burn_commit=%s\n' "${GPU_BURN_COMMIT:-unknown}"
    printf 'duration_seconds=%s sample_interval_seconds=%s\n' \
      "${BURN_DURATION_SECONDS:-unset}" "${SAMPLE_INTERVAL_SECONDS:-unset}"
    printf 'power_limit_tolerance_percent=%s power_limit_consecutive_samples=%s\n' \
      "${POWER_LIMIT_TOLERANCE_PERCENT:-unset}" "${POWER_LIMIT_CONSECUTIVE_SAMPLES:-unset}"
    printf '%s\n' "${details}"
    if [[ -f /results/devices.txt ]]; then
      cat /results/devices.txt
    fi
    burn_log_excerpt
  } > /results/measurements.txt
  if [[ "${component}" == gpu-burn ]]; then
    gpu_verdict="${verdict}"
    gpu_reason="${reason}"
    if [[ "${reason}" == HostTest* || "${reason}" == ComponentCleanupIncomplete ]]; then
      if "${gpu_completed}"; then
        gpu_verdict=Pass
        gpu_reason=GPUBurnCompleted
      else
        gpu_verdict=Inconclusive
        gpu_reason=GroupInterrupted
      fi
    fi
    printf '%s' "${gpu_verdict}" > /results/gpu-burn-verdict
    printf '%s' "${gpu_reason}" > /results/gpu-burn-reason
    {
      printf 'test=gpu-burn verdict=%s reason=%s\n' "${gpu_verdict}" "${gpu_reason}"
      printf 'group_verdict=%s group_reason=%s\n' "${verdict}" "${reason}"
      tail -n +2 /results/measurements.txt
    } > /results/gpu-burn-measurements.txt
  fi
  printf 'PROJECT_SIGNAL_GPU_BURN_LOG_BEGIN\n'
  burn_log_excerpt
  printf 'PROJECT_SIGNAL_GPU_BURN_LOG_END\n'
  printf 'verdict=%s reason=%s\n' "${verdict}" "${reason}" > /dev/termination-log
}

if [[ "${component}" != standalone && "${component}" != gpu-burn ]]; then
  run_component
fi

trap finish EXIT
trap 'verdict=Inconclusive; reason=Canceled; exit 143' TERM
trap 'verdict=Inconclusive; reason=Canceled; exit 130' INT

emit_stage preflight running

if ! [[ "${EXPECTED_GPU_COUNT:-}" =~ ^[1-9][0-9]*$ &&
        "${BURN_DURATION_SECONDS:-}" =~ ^[1-9][0-9]*$ &&
        "${SAMPLE_INTERVAL_SECONDS:-}" =~ ^[1-9][0-9]*$ &&
        "${POWER_LIMIT_TOLERANCE_PERCENT:-}" =~ ^[0-9]+$ &&
        "${POWER_LIMIT_CONSECUTIVE_SAMPLES:-}" =~ ^[1-9][0-9]*$ &&
        "${CPU_WORKERS:-}" =~ ^[1-9][0-9]*$ &&
        "${MEMORY_MIB:-}" =~ ^[1-9][0-9]*$ &&
        "${STORAGE_MIB:-}" =~ ^[1-9][0-9]*$ ]]; then
  fail BurnConfigurationInvalid "numeric burn parameters are required"
elif [[ "${OBSERVED_GPU_CAPACITY:-}" != "${EXPECTED_GPU_COUNT}" ||
        "${OBSERVED_GPU_ALLOCATABLE:-}" != "${EXPECTED_GPU_COUNT}" ]]; then
  fail NodeGPUInventoryMismatch \
    "expected=${EXPECTED_GPU_COUNT} capacity=${OBSERVED_GPU_CAPACITY:-unset} allocatable=${OBSERVED_GPU_ALLOCATABLE:-unset}"
elif [[ -z "${EXPECTED_GPU_PRODUCT_REGEX:-}" ]]; then
  fail ProductGuardNotConfigured
elif ! command -v gpu_burn >/dev/null 2>&1 ||
     ! command -v nvidia-smi >/dev/null 2>&1; then
  fail BurnToolMissing
elif ! dmesg >/dev/null 2>&1; then
  fail XidTelemetryUnavailable "privileged dmesg access is required"
else
  if ! baseline_xids="$(xid_count)"; then
    fail XidTelemetryUnavailable "Cannot capture baseline kernel Xid telemetry"
  fi
  capture_telemetry baseline || true
  if [[ "${component}" == gpu-burn ]] &&
     ! nvidia-smi --query-gpu=index,uuid,pci.bus_id,name --format=csv,noheader > /results/devices.txt; then
    fail DeviceIdentityUnavailable "Cannot retain assigned GPU identities"
  fi
fi

if [[ "${verdict}" != Pass ]]; then
  emit_stage preflight failed "${reason}" "${details}"
  exit 0
fi

emit_stage preflight passed InventoryAndSafetyTelemetryReady \
  "gpus=${EXPECTED_GPU_COUNT} baseline_xids=${baseline_xids}"
emit_stage workload running BurnCanaryStarted \
  "duration_seconds=${BURN_DURATION_SECONDS} gpu_memory_percent=50 cpu_workers=${CPU_WORKERS}"

rm -f "${stop_file}"
mkdir -p /work/memory /work/storage

if [[ "${component}" == gpu-burn ]]; then
  touch /work/ready
  for ((waited = 0; waited < 120; waited++)); do
    if ! check_components; then
      break
    fi
    if [[ -e /work/cpu-stress.started &&
          -e /work/memory-integrity.started &&
          -e /work/storage-integrity.started ]]; then
      break
    fi
    sleep 1
  done
  if [[ ! -e /work/cpu-stress.started ||
        ! -e /work/memory-integrity.started ||
        ! -e /work/storage-integrity.started ]]; then
    fail HostTestStartupIncomplete "Not all concurrent host tests started"
  fi
else
dd if=/dev/zero of=/work/memory/pattern.bin bs=1M count="${MEMORY_MIB}" status=none
memory_hash="$(sha256sum /work/memory/pattern.bin | awk '{print $1}')"
(
  while [[ ! -e "${stop_file}" ]]; do
    [[ "$(sha256sum /work/memory/pattern.bin | awk '{print $1}')" == "${memory_hash}" ]] || exit 1
  done
) &
memory_pid=$!
worker_pids+=("${memory_pid}")

dd if=/dev/zero of=/work/storage/pattern.bin bs=1M count="${STORAGE_MIB}" oflag=direct conv=fsync status=none
storage_hash="$(sha256sum /work/storage/pattern.bin | awk '{print $1}')"
(
  while [[ ! -e "${stop_file}" ]]; do
    [[ "$(sha256sum /work/storage/pattern.bin | awk '{print $1}')" == "${storage_hash}" ]] || exit 1
    sleep "${SAMPLE_INTERVAL_SECONDS}"
  done
) &
storage_pid=$!
worker_pids+=("${storage_pid}")

for ((worker = 0; worker < CPU_WORKERS; worker++)); do
  (
    while [[ ! -e "${stop_file}" ]]; do
      dd if=/dev/zero bs=4M count=256 status=none | sha256sum >/dev/null
    done
  ) &
  cpu_pids+=("$!")
  worker_pids+=("$!")
done
fi

if [[ "${verdict}" == Pass ]]; then
  GPU_BURN_COMMIT="${GPU_BURN_COMMIT:-0d19c94dcb2e9b6858b7586a4c327674cd67113b}" \
    gpu_burn -m 50% -tc "${BURN_DURATION_SECONDS}" >"${gpu_log}" 2>&1 &
  gpu_pid=$!
  gpu_worker_index="${#worker_pids[@]}"
  worker_pids+=("${gpu_pid}")
fi

end_epoch=$(( $(date +%s) + BURN_DURATION_SECONDS ))
while [[ "${verdict}" == Pass ]] && (( $(date +%s) < end_epoch )); do
  if [[ "${component}" == gpu-burn ]]; then
    delay="${SAMPLE_INTERVAL_SECONDS}"
    remaining=$((end_epoch - $(date +%s)))
    (( remaining >= delay )) || delay="${remaining}"
    pause_until_sample "${delay}"
  else
    sleep "${SAMPLE_INTERVAL_SECONDS}"
  fi
  if (( $(date +%s) >= end_epoch )); then
    break
  fi

  if [[ "${component}" == gpu-burn ]] && ! check_components; then
    break
  fi
  if ! kill -0 "${gpu_pid}" 2>/dev/null; then
    fail GPUBurnExitedEarly "$(burn_log_excerpt | tail -n 5 | tr '\n' ';')"
    break
  elif [[ "${component}" == standalone ]] && ! kill -0 "${memory_pid}" 2>/dev/null; then
    fail MemoryIntegrityFailed
    break
  elif [[ "${component}" == standalone ]] && ! kill -0 "${storage_pid}" 2>/dev/null; then
    fail StorageIntegrityFailed
    break
  fi

  for cpu_pid in "${cpu_pids[@]}"; do
    if ! kill -0 "${cpu_pid}" 2>/dev/null; then
      fail HostStressFailed "worker_pid=${cpu_pid}"
      break
    fi
  done
  capture_telemetry sample || true
  if ! current_xids="$(xid_count)"; then
    fail XidTelemetryUnavailable "Kernel Xid telemetry became unavailable during the workload"
  fi
  if (( current_xids > baseline_xids )); then
    fail NewXidDetected "xids=${baseline_xids}->${current_xids}"
  fi

  if [[ "${verdict}" != Pass ]]; then
    break
  fi
done

if [[ "${component}" == gpu-burn ]]; then
  check_components || true
fi
if [[ "${verdict}" == Pass ]]; then
  gpu_exit=0
  wait "${gpu_pid}" || gpu_exit=$?
  unset "worker_pids[${gpu_worker_index}]"
  if (( gpu_exit != 0 )); then
    fail GPUBurnFailed
  else
    validate_gpu_measurements
    if [[ "${verdict}" == Pass && "${component}" == gpu-burn ]]; then
      gpu_completed=true
      touch /work/burn-complete
    fi
  fi

  touch "${stop_file}"
  if [[ "${component}" == standalone ]] && ! wait "${memory_pid}"; then
    fail MemoryIntegrityFailed
  fi
  if [[ "${component}" == standalone ]] && ! wait "${storage_pid}"; then
    fail StorageIntegrityFailed
  fi
fi

if [[ "${component}" == gpu-burn ]]; then
  stop_workers
  collect_component_results
fi

if [[ "${verdict}" == Pass ]]; then
  emit_stage workload passed BurnWorkloadCompleted
else
  emit_stage workload failed "${reason}" "${details}"
fi

emit_stage health-after running
capture_telemetry final || true
if ! final_xids="$(xid_count)"; then
  fail XidTelemetryUnavailable "Cannot capture final kernel Xid telemetry"
fi
if (( final_xids > baseline_xids )); then
  fail NewXidDetected "xids=${baseline_xids}->${final_xids}"
fi

if [[ "${verdict}" == Pass ]]; then
  emit_stage health-after passed BurnHealthAfterPassed
else
  emit_stage health-after failed "${reason}" "${details}"
fi

finished_normally=true
exit 0
