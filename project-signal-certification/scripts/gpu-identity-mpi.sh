#!/usr/bin/env bash
# MPI roles are dispatched before the unchanged baseline result entrypoint.
case "${1:-}" in
  mpi-launcher)
    if [[ "$#" -ne 2 || ! "${2:-}" =~ ^[1-9][0-9]*$ ]]; then
      printf 'Usage: %s mpi-launcher <positive-gpu-count>\n' "$0" >&2
      exit 2
    fi
    exec mpirun --allow-run-as-root -np 1 --bind-to none --map-by slot -mca plm_rsh_no_tree_spawn 1 -mca plm_rsh_args "-o ConnectionAttempts=10 -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/tmp/project-signal-known-hosts" /bin/true
    ;;
  mpi-worker)
    if [[ "$#" -ne 1 ]]; then
      printf 'Usage: %s mpi-worker\n' "$0" >&2
      exit 2
    fi
    install -d -m 0700 /run/project-signal-ssh && install -m 0600 /root/.ssh/authorized_keys /run/project-signal-ssh/authorized_keys && mkdir -p /run/sshd && ssh-keygen -A && exec /usr/sbin/sshd -D -e -o AuthorizedKeysFile=/run/project-signal-ssh/authorized_keys
    exit "$?"
    ;;
  mpi-*)
    printf 'Unsupported MPI role: %s\n' "$1" >&2
    exit 2
    ;;
esac

#!/usr/bin/env bash
set -uo pipefail

# This public example verifies only GPU identity and count. Replace it with
# an organization-reviewed diagnostic before using Project Signal for
# hardware qualification.
verdict=Pass
reason=ExampleCheckPassed
details=

if [[ -z "${EXPECTED_GPU_PRODUCT_REGEX:-}" ||
      "${EXPECTED_GPU_PRODUCT_REGEX}" == *REPLACE_WITH_VERIFIED* ]]; then
  verdict=Fail
  reason=ProductGuardNotConfigured
elif ! [[ "${EXPECTED_GPU_COUNT:-}" =~ ^[1-9][0-9]*$ ]]; then
  verdict=Fail
  reason=CountGuardNotConfigured
elif ! command -v nvidia-smi >/dev/null 2>&1; then
  verdict=Fail
  reason=NvidiaSMIMissing
elif ! product_output="$(nvidia-smi --query-gpu=name --format=csv,noheader)"; then
  verdict=Fail
  reason=GPUIdentityQueryFailed
else
  mapfile -t products <<< "${product_output}"
  details="$(printf '%s\n' "${products[@]}")"

  if [[ "${#products[@]}" -ne "${EXPECTED_GPU_COUNT}" ]]; then
    verdict=Fail
    reason=GPUCountMismatch
  else
    for product in "${products[@]}"; do
      if ! grep -Eq "${EXPECTED_GPU_PRODUCT_REGEX}" <<<"${product}"; then
        verdict=Fail
        reason=GPUProductMismatch
        break
      fi
    done

    if [[ "${verdict}" == Pass ]]; then
      if ! details="$(nvidia-smi --query-gpu=uuid,pci.bus_id,name --format=csv,noheader)"; then
        verdict=Fail
        reason=GPUIdentityQueryFailed
      elif [[ -z "${details}" ]]; then
        verdict=Inconclusive
        reason=GPUMeasurementsMissing
      fi
    fi
  fi
fi

printf '%s' "${verdict}" > /results/verdict
printf '%s' "${reason}" > /results/reason
printf 'verdict=%s reason=%s\n%s\n' "${verdict}" "${reason}" "${details}" > /results/measurements.txt
printf 'verdict=%s reason=%s\n' "${verdict}" "${reason}" > /dev/termination-log
printf 'PROJECT_SIGNAL_EXAMPLE verdict=%s reason=%s\n%s\n' \
  "${verdict}" "${reason}" "${details}"

# A diagnostic result is evidence, not a workflow infrastructure error.
exit 0
