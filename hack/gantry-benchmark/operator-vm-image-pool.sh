#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -Eeuo pipefail

AZURE_RESOURCE_GROUP="${AZURE_RESOURCE_GROUP:-}"
OPERATOR_VM_NAME="${OPERATOR_VM_NAME:-gantry-benchmark-operator}"
OPERATOR_RUN_COMMAND_LOCK="${OPERATOR_RUN_COMMAND_LOCK:-${TMPDIR:-/tmp}/gantry-benchmark-${AZURE_RESOURCE_GROUP}-${OPERATOR_VM_NAME}.run-command.lock}"

usage() {
  cat <<'USAGE'
Usage: operator-vm-image-pool.sh start COUNT
  operator-vm-image-pool.sh run BASELINE_RUN_ID
  operator-vm-image-pool.sh fresh BASELINE_RUN_ID
       operator-vm-image-pool.sh status

Starts an asynchronous operator-VM image-pool build, starts a pool-backed or
brand-new-image Gantry-only run, or prints bounded status. Pool builds and
benchmark runs are mutually exclusive because pushes to the Gantry ACR during
a measured phase invalidate Azure telemetry.
USAGE
}

remote_status_marker="GANTRY_BENCHMARK_REMOTE_STATUS="

invoke_remote() {
  local body=$1
  local output
  local remote_status
  local transport_status
  local wrapped_script

  wrapped_script="#!/usr/bin/env bash
set +e
(
$body
)
gantry_benchmark_remote_status=\$?
printf '${remote_status_marker}%s\\n' \"\$gantry_benchmark_remote_status\"
exit 0"

  if output=$(az vm run-command invoke \
    -g "$AZURE_RESOURCE_GROUP" \
    -n "$OPERATOR_VM_NAME" \
    --command-id RunShellScript \
    --scripts "$wrapped_script" \
    --only-show-errors \
    --query 'value[0].message' \
    -o tsv); then
    :
  else
    transport_status=$?
    return "$transport_status"
  fi

  output=${output//$'\r'/}
  remote_status=$(sed -n "s/^${remote_status_marker}\\([0-9][0-9]*\\)$/\\1/p" <<<"$output" | tail -1)
  printf '%s\n' "$output" | sed \
    -e '/^Enable succeeded: *$/d' \
    -e '/^\[stdout\]$/d' \
    -e '/^\[stderr\]$/d' \
    -e "/^${remote_status_marker}[0-9][0-9]*$/d"

  if [[ ! "$remote_status" =~ ^[0-9]+$ ]]; then
    echo "operator VM command did not return a remote exit status" >&2
    return 1
  fi

  if ((remote_status != 0)); then
    echo "operator VM command failed with exit code $remote_status" >&2
    return "$remote_status"
  fi
}

(($# >= 1)) || { usage >&2; exit 2; }
action=$1
shift

: "${AZURE_RESOURCE_GROUP:?Set AZURE_RESOURCE_GROUP}"

exec {run_command_lock_fd}>"$OPERATOR_RUN_COMMAND_LOCK"
flock "$run_command_lock_fd"
trap 'flock -u "$run_command_lock_fd"' EXIT

case "$action" in
  start)
    (($# == 1)) || { usage >&2; exit 2; }
    count=$1
    [[ "$count" =~ ^[1-9][0-9]*$ ]] || { echo "COUNT must be a positive integer" >&2; exit 2; }
    ((count <= 100)) || { echo "COUNT must not exceed 100" >&2; exit 2; }

    script=$(cat <<SCRIPT
set -eu
if ! systemctl cat gantry-benchmark-image-builder.service >/dev/null 2>&1; then
  echo "gantry-benchmark-image-builder.service is not installed; refresh the operator VM with make -C hack/gantry-benchmark deploy" >&2
  exit 1
fi
source /etc/gantry-benchmark/env
: "\${BENCHMARK_IMAGE_POOL_ROOT:?operator VM image-pool configuration is missing; refresh the deployment}"
if [[ ! -x "\$BENCHMARK_REPO_ROOT/hack/gantry-benchmark/operator-vm-prebuild-images.sh" ]]; then
  echo "operator VM image-pool builder script is missing; refresh the deployment" >&2
  exit 1
fi
if systemctl is-active --quiet gantry-benchmark-operator.service; then
  echo "gantry-benchmark-operator.service is active; pool pushes would contaminate benchmark telemetry" >&2
  exit 1
fi
if systemctl is-active --quiet gantry-benchmark-image-builder.service; then
  echo "gantry-benchmark-image-builder.service is already active" >&2
  exit 1
fi
cat >/etc/gantry-benchmark/image-pool.env <<'ENV'
GANTRY_IMAGE_POOL_COUNT="$count"
ENV
if systemctl is-failed --quiet gantry-benchmark-image-builder.service; then
  systemctl reset-failed gantry-benchmark-image-builder.service
fi
systemctl start --no-block gantry-benchmark-image-builder.service
systemctl show gantry-benchmark-image-builder.service --property=ActiveState --property=SubState --no-pager
SCRIPT
)
  invoke_remote "$script"
    ;;
  run|fresh)
    (($# == 1)) || { usage >&2; exit 2; }
    baseline_run_id=$1
    [[ "$baseline_run_id" =~ ^[A-Za-z0-9._-]+$ && "$baseline_run_id" != "." && "$baseline_run_id" != ".." ]] || {
      echo "BASELINE_RUN_ID contains unsupported characters" >&2
      exit 2
    }

    mode_config='GANTRY_ONLY_USE_IMAGE_POOL="true"'
    if [[ "$action" == fresh ]]; then
      mode_config='GANTRY_ONLY_FRESH_IMAGE="true"'
    fi

    script=$(cat <<SCRIPT
set -eu
if ! systemctl cat gantry-benchmark-image-builder.service >/dev/null 2>&1; then
  echo "gantry-benchmark-image-builder.service is not installed; refresh the operator VM with make -C hack/gantry-benchmark deploy" >&2
  exit 1
fi
if systemctl is-active --quiet gantry-benchmark-image-builder.service; then
  echo "gantry-benchmark-image-builder.service is active; wait for pool building to finish" >&2
  exit 1
fi
if systemctl is-active --quiet gantry-benchmark-operator.service; then
  echo "gantry-benchmark-operator.service is already active" >&2
  exit 1
fi
source /etc/gantry-benchmark/env
baseline_dir="\$BENCHMARK_ARTIFACT_ROOT/$baseline_run_id"
test -s "\$baseline_dir/state.json" && test -s "\$baseline_dir/baseline.json" || {
  echo "retained baseline $baseline_run_id is missing state.json or baseline.json" >&2
  exit 1
}
if [[ "$action" == run ]]; then
  ready_count="\$(find "\$BENCHMARK_IMAGE_POOL_ROOT/ready" -maxdepth 1 -type f -name '*.json' 2>/dev/null | wc -l)"
  test "\$ready_count" -gt 0 || {
    echo "the Gantry image pool has no ready entries" >&2
    exit 1
  }
fi
sed -i '/^GANTRY_ONLY_/d' /etc/gantry-benchmark/env
cat >>/etc/gantry-benchmark/env <<'ENV'
GANTRY_ONLY_BASELINE_RUN_ID="$baseline_run_id"
$mode_config
ENV
if systemctl is-failed --quiet gantry-benchmark-operator.service; then
  systemctl reset-failed gantry-benchmark-operator.service
fi
systemctl start --no-block gantry-benchmark-operator.service
systemctl show gantry-benchmark-operator.service --property=ActiveState --property=SubState --no-pager
SCRIPT
)
  invoke_remote "$script"
    ;;
  status)
    (($# == 0)) || { usage >&2; exit 2; }

    script=$(cat <<'SCRIPT'
set -u
if ! systemctl cat gantry-benchmark-image-builder.service >/dev/null 2>&1; then
  echo "gantry-benchmark-image-builder.service is not installed; refresh the operator VM with make -C hack/gantry-benchmark deploy" >&2
  exit 1
fi
source /etc/gantry-benchmark/env
: "${BENCHMARK_IMAGE_POOL_ROOT:?operator VM image-pool configuration is missing; refresh the deployment}"
printf '=== Gantry image pool builder ===\n'
systemctl show gantry-benchmark-image-builder.service \
  --property=ActiveState --property=SubState --property=Result --property=ExecMainStatus --no-pager
printf '\n=== Progress ===\n'
cat "${BENCHMARK_IMAGE_POOL_PROGRESS:-$BENCHMARK_OPERATOR_HOME/image-pool-progress.json}" 2>/dev/null || echo '{}'
printf '\n=== Pool ===\n'
ready_dir="$BENCHMARK_IMAGE_POOL_ROOT/ready"
claimed_dir="$BENCHMARK_IMAGE_POOL_ROOT/claimed"
printf 'ready: %s\n' "$(find "$ready_dir" -maxdepth 1 -type f -name '*.json' 2>/dev/null | wc -l)"
printf 'claimed: %s\n' "$(find "$claimed_dir" -maxdepth 1 -type f -name '*.json' 2>/dev/null | wc -l)"
find "$ready_dir" -maxdepth 1 -type f -name '*.json' -printf '%f\n' 2>/dev/null | sort | tail -10
printf '\n=== Ready metadata ===\n'
for metadata in "$ready_dir"/*.json; do
  [[ -f "$metadata" ]] || continue
  jq -c '{schema_version,id,created_at,image,payload_sha256,image_size_mib,image_layers,image_platform,workload_repository,gantry_acr_login_server}' "$metadata"
done
printf '\n=== Local cleanup ===\n'
build_root="${BENCHMARK_IMAGE_POOL_BUILD_ROOT:-$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/image-pool-build}"
printf 'scratch entries: %s\n' "$(find "$build_root" -mindepth 1 -maxdepth 1 -print 2>/dev/null | wc -l)"
local_pool_tags=0
for metadata in "$ready_dir"/*.json "$claimed_dir"/*.json; do
  [[ -f "$metadata" ]] || continue
  entry_id="$(jq -r '.id' "$metadata")"
  if podman image exists "$GANTRY_ACR_LOGIN_SERVER/$BENCHMARK_WORKLOAD_REPOSITORY:$entry_id"; then
    ((local_pool_tags += 1))
  fi
done
printf 'local pool tags: %s\n' "$local_pool_tags"
printf '\n=== Recent log ===\n'
tail -20 "${BENCHMARK_IMAGE_POOL_LOG:-$BENCHMARK_OPERATOR_HOME/image-pool-builder.log}" 2>/dev/null || true
printf '\n=== VM space ===\n'
df -h / "$BENCHMARK_BUILD_MOUNT" | awk 'NR == 1 || !seen[$1]++'
SCRIPT
)
  invoke_remote "$script"
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
