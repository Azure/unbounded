#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

AZURE_RESOURCE_GROUP="${AZURE_RESOURCE_GROUP:-}"
OPERATOR_VM_NAME="${OPERATOR_VM_NAME:-gantry-benchmark-operator}"
OPERATOR_RUN_COMMAND_LOCK="${OPERATOR_RUN_COMMAND_LOCK:-${TMPDIR:-/tmp}/gantry-benchmark-${AZURE_RESOURCE_GROUP}-${OPERATOR_VM_NAME}.run-command.lock}"

usage() {
  cat <<'USAGE'
Usage: operator-vm-image-pool.sh start COUNT
  operator-vm-image-pool.sh full
  operator-vm-image-pool.sh standalone
  operator-vm-image-pool.sh fail-open
  operator-vm-image-pool.sh run BASELINE_RUN_ID
  operator-vm-image-pool.sh fresh BASELINE_RUN_ID
  operator-vm-image-pool.sh status
  operator-vm-image-pool.sh prune
  operator-vm-image-pool.sh prune-status

Starts an asynchronous operator-VM image-pool build, a full benchmark, a
standalone Gantry run, a baseline-backed Gantry-only run, or an unused-image
prune. These operations are mutually exclusive because pushes to the Gantry ACR
during a measured phase invalidate Azure telemetry.

Set ADOPT_BASELINE_IMAGE, ADOPT_GANTRY_IMAGE, and ADOPT_PAYLOAD_SHA256 together
with "full" to reuse an existing digest-pinned image pair.
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
standalone_base_hashes_base64=
standalone_compatible_hashes_base64=
standalone_patched_hashes_base64=
standalone_patch_base64=

: "${AZURE_RESOURCE_GROUP:?Set AZURE_RESOURCE_GROUP}"

exec {run_command_lock_fd}>"$OPERATOR_RUN_COMMAND_LOCK"
flock "$run_command_lock_fd"
trap 'flock -u "$run_command_lock_fd"' EXIT

case "$action" in
  fail-open)
    (($# == 0)) || { usage >&2; exit 2; }
    local_repo_root=$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)
    fail_open_paths=(
      hack/cmd/gantry-benchmark/config.go
      hack/cmd/gantry-benchmark/enable.go
      hack/cmd/gantry-benchmark/hosts_routing.go
      hack/cmd/gantry-benchmark/state.go
    )
    fail_open_base_hashes_base64=$(base64 -w0 <<'HASHES'
5294650c6c63f3b459fe5e7e6627706768a1aa7379c626efefccffbb101e3f2e  hack/cmd/gantry-benchmark/config.go
ab7d8c8987b4f67ad0d464dcfa303826e111903bdf5f95b6568b6254a07401a4  hack/cmd/gantry-benchmark/enable.go
07771ed03cd9dad3d69a9b136ceb3e803c8cfdef787085dedfa6778fb2d5e533  hack/cmd/gantry-benchmark/hosts_routing.go
1e7917ec3ad44f60b4d681b8eb31ec318bb9ff00803071d80f715d9bc05d8dd0  hack/cmd/gantry-benchmark/state.go
HASHES
)
    fail_open_target_hashes_base64=$(
      cd "$local_repo_root"
      sha256sum "${fail_open_paths[@]}" | base64 -w0
    )
    fail_open_payload_base64=$(
      cd "$local_repo_root"
      tar -czf - "${fail_open_paths[@]}" | base64 -w0
    )

    script=$(cat <<SCRIPT
set -eu
operator_state="\$(systemctl is-active gantry-benchmark-operator.service 2>/dev/null || true)"
case "\$operator_state" in
  active|activating) ;;
  *)
    echo "gantry-benchmark-operator.service is \$operator_state, want active or activating" >&2
    exit 1
    ;;
esac
source /etc/gantry-benchmark/env
export HOME="${BENCHMARK_OPERATOR_HOME:-/var/lib/gantry-benchmark}"
base_hashes="\$(mktemp)"
target_hashes="\$(mktemp)"
payload="\$(mktemp)"
trap 'rm -f "\$base_hashes" "\$target_hashes" "\$payload"' EXIT
printf '%s' '$fail_open_base_hashes_base64' | base64 --decode >"\$base_hashes"
printf '%s' '$fail_open_target_hashes_base64' | base64 --decode >"\$target_hashes"
printf '%s' '$fail_open_payload_base64' | base64 --decode >"\$payload"
cd "\$BENCHMARK_REPO_ROOT"
if sha256sum --check --strict "\$target_hashes" >/dev/null 2>&1; then
  echo "fail-open benchmark source is already present"
elif sha256sum --check --strict "\$base_hashes" >/dev/null 2>&1; then
  tar -xzf "\$payload"
else
  echo "operator VM fail-open source files do not match the tested base or target contents" >&2
  exit 1
fi
sha256sum --check --strict "\$target_hashes" >/dev/null
GOTOOLCHAIN=auto go test ./hack/cmd/gantry-benchmark
echo "validated fail-open benchmark source while operator remains active"
SCRIPT
)
    invoke_remote "$script"
    ;;
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
if systemctl is-active --quiet gantry-benchmark-image-prune.service; then
  echo "gantry-benchmark-image-prune.service is active; wait for image pruning to finish" >&2
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
  full)
    (($# == 0)) || { usage >&2; exit 2; }

    adoption_values=0
    for value in "${ADOPT_BASELINE_IMAGE:-}" "${ADOPT_GANTRY_IMAGE:-}" "${ADOPT_PAYLOAD_SHA256:-}"; do
      [[ -z "$value" ]] || adoption_values=$((adoption_values + 1))
    done
    if ((adoption_values != 0 && adoption_values != 3)); then
      echo "ADOPT_BASELINE_IMAGE, ADOPT_GANTRY_IMAGE, and ADOPT_PAYLOAD_SHA256 must be set together" >&2
      exit 2
    fi

    adoption_config_base64=""
    if ((adoption_values == 3)); then
      [[ "$ADOPT_BASELINE_IMAGE" =~ ^[a-z0-9.-]+/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$ ]] || {
        echo "ADOPT_BASELINE_IMAGE must be an immutable digest reference" >&2
        exit 2
      }
      [[ "$ADOPT_GANTRY_IMAGE" =~ ^[a-z0-9.-]+/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$ ]] || {
        echo "ADOPT_GANTRY_IMAGE must be an immutable digest reference" >&2
        exit 2
      }
      [[ "$ADOPT_PAYLOAD_SHA256" =~ ^sha256:[0-9a-f]{64}$ ]] || {
        echo "ADOPT_PAYLOAD_SHA256 must be a sha256 digest" >&2
        exit 2
      }
      adoption_config_base64=$(printf '%s\n' \
        "ADOPT_BASELINE_IMAGE=$ADOPT_BASELINE_IMAGE" \
        "ADOPT_GANTRY_IMAGE=$ADOPT_GANTRY_IMAGE" \
        "ADOPT_PAYLOAD_SHA256=$ADOPT_PAYLOAD_SHA256" | base64 -w0)
    fi

    script=$(cat <<'SCRIPT'
set -eu
if systemctl is-active --quiet gantry-benchmark-image-builder.service; then
  echo "gantry-benchmark-image-builder.service is active; wait for pool building to finish" >&2
  exit 1
fi
if systemctl is-active --quiet gantry-benchmark-image-prune.service; then
  echo "gantry-benchmark-image-prune.service is active; wait for image pruning to finish" >&2
  exit 1
fi
if systemctl is-active --quiet gantry-benchmark-operator.service; then
  echo "gantry-benchmark-operator.service is already active" >&2
  exit 1
fi
sed -i \
  -e '/^GANTRY_ONLY_/d' \
  -e '/^ADOPT_BASELINE_IMAGE=/d' \
  -e '/^ADOPT_GANTRY_IMAGE=/d' \
  -e '/^ADOPT_PAYLOAD_SHA256=/d' \
  /etc/gantry-benchmark/env
if [[ -n '__ADOPTION_CONFIG_BASE64__' ]]; then
  printf '%s' '__ADOPTION_CONFIG_BASE64__' | base64 --decode >>/etc/gantry-benchmark/env
fi
if systemctl is-failed --quiet gantry-benchmark-operator.service; then
  systemctl reset-failed gantry-benchmark-operator.service
fi
systemctl start --no-block gantry-benchmark-operator.service
systemctl show gantry-benchmark-operator.service --property=ActiveState --property=SubState --no-pager
SCRIPT
)
  script=${script//__ADOPTION_CONFIG_BASE64__/$adoption_config_base64}
    invoke_remote "$script"
    ;;
  standalone|run|fresh)
    baseline_run_id=
    case "$action" in
      standalone)
        (($# == 0)) || { usage >&2; exit 2; }
        mode_config='GANTRY_ONLY_STANDALONE="true"'
        local_repo_root=$(git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel)
        standalone_paths=(
          hack/cmd/gantry-benchmark/gantry_only.go
          hack/cmd/gantry-benchmark/main.go
          hack/cmd/gantry-benchmark/state.go
          hack/gantry-benchmark/operator-vm-run.sh
        )
        standalone_base_hashes_base64=$(
          for path in "${standalone_paths[@]}"; do
            printf '%s  %s\n' \
              "$(git -C "$local_repo_root" show "HEAD:$path" | sha256sum | cut -d' ' -f1)" \
              "$path"
          done | base64 -w0
        )
        standalone_patched_hashes_base64=$(
          cd "$local_repo_root"
          sha256sum "${standalone_paths[@]}" | base64 -w0
        )
        standalone_compatible_hashes_base64=$(
          cd "$local_repo_root"
          for path in "${standalone_paths[@]}"; do
            if [[ "$path" == hack/gantry-benchmark/operator-vm-run.sh ]]; then
              printf '%s  %s\n' ae2b514d6d4cabe3476d758ba14399ad3ef4fdb2848a7e0beecd0b0ddeedc87b "$path"
            else
              sha256sum "$path"
            fi
          done | base64 -w0
        )
        standalone_patch_base64=$(git -C "$local_repo_root" diff --binary HEAD -- \
          "${standalone_paths[@]}" | base64 -w0)
        [[ -n "$standalone_patch_base64" ]] || {
          echo "standalone source patch is empty" >&2
          exit 1
        }
        ;;
      fresh)
        (($# == 1)) || { usage >&2; exit 2; }
        baseline_run_id=$1
        mode_config='GANTRY_ONLY_FRESH_IMAGE="true"'
        ;;
      run)
        (($# == 1)) || { usage >&2; exit 2; }
        baseline_run_id=$1
        mode_config='GANTRY_ONLY_USE_IMAGE_POOL="true"'
        ;;
    esac
    if [[ -n "$baseline_run_id" ]]; then
      [[ "$baseline_run_id" =~ ^[A-Za-z0-9._-]+$ && "$baseline_run_id" != "." && "$baseline_run_id" != ".." ]] || {
        echo "BASELINE_RUN_ID contains unsupported characters" >&2
        exit 2
      }
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
if systemctl is-active --quiet gantry-benchmark-image-prune.service; then
  echo "gantry-benchmark-image-prune.service is active; wait for image pruning to finish" >&2
  exit 1
fi
if systemctl is-active --quiet gantry-benchmark-operator.service; then
  echo "gantry-benchmark-operator.service is already active" >&2
  exit 1
fi
source /etc/gantry-benchmark/env
if [[ "$action" == standalone ]]; then
  standalone_base_hashes="\$(mktemp)"
  standalone_compatible_hashes="\$(mktemp)"
  standalone_patched_hashes="\$(mktemp)"
  standalone_patch="\$(mktemp)"
  trap 'rm -f "\$standalone_base_hashes" "\$standalone_compatible_hashes" "\$standalone_patched_hashes" "\$standalone_patch"' EXIT
  printf '%s' '$standalone_base_hashes_base64' | base64 --decode >"\$standalone_base_hashes"
  printf '%s' '$standalone_compatible_hashes_base64' | base64 --decode >"\$standalone_compatible_hashes"
  printf '%s' '$standalone_patched_hashes_base64' | base64 --decode >"\$standalone_patched_hashes"
  printf '%s' '$standalone_patch_base64' | base64 --decode >"\$standalone_patch"
  cd "\$BENCHMARK_REPO_ROOT"
  if sha256sum --check --strict "\$standalone_base_hashes" >/dev/null 2>&1; then
    git apply --check "\$standalone_patch"
    git apply "\$standalone_patch"
  elif sha256sum --check --strict "\$standalone_patched_hashes" >/dev/null 2>&1; then
    echo "standalone benchmark patch is already present"
  elif sha256sum --check --strict "\$standalone_compatible_hashes" >/dev/null 2>&1; then
    echo "compatible standalone benchmark patch is already present"
  else
    echo "operator VM standalone source files do not match the tested base or patched contents" >&2
    exit 1
  fi
  if ! sha256sum --check --strict "\$standalone_patched_hashes" >/dev/null 2>&1 &&
    ! sha256sum --check --strict "\$standalone_compatible_hashes" >/dev/null 2>&1; then
    echo "operator VM standalone source verification failed after patching" >&2
    exit 1
  fi
  rm -f "\$standalone_base_hashes" "\$standalone_compatible_hashes" "\$standalone_patched_hashes" "\$standalone_patch"
  trap - EXIT
fi
if [[ "$action" != standalone ]]; then
baseline_dir="\$BENCHMARK_ARTIFACT_ROOT/$baseline_run_id"
test -s "\$baseline_dir/state.json" && test -s "\$baseline_dir/baseline.json" || {
  echo "retained baseline $baseline_run_id is missing state.json or baseline.json" >&2
  exit 1
}
fi
if [[ "$action" == run ]]; then
  ready_count="\$(find "\$BENCHMARK_IMAGE_POOL_ROOT/ready" -maxdepth 1 -type f -name '*.json' 2>/dev/null | wc -l)"
  test "\$ready_count" -gt 0 || {
    echo "the Gantry image pool has no ready entries" >&2
    exit 1
  }
fi
sed -i '/^GANTRY_ONLY_/d' /etc/gantry-benchmark/env
if [[ "$action" == standalone ]]; then
  sed -i \
    -e '/^ADOPT_BASELINE_IMAGE=/d' \
    -e '/^ADOPT_GANTRY_IMAGE=/d' \
    -e '/^ADOPT_PAYLOAD_SHA256=/d' \
    /etc/gantry-benchmark/env
fi
cat >>/etc/gantry-benchmark/env <<'ENV'
$mode_config
ENV
if [[ "$action" != standalone ]]; then
  echo 'GANTRY_ONLY_BASELINE_RUN_ID="$baseline_run_id"' >>/etc/gantry-benchmark/env
fi
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
  prune)
    (($# == 0)) || { usage >&2; exit 2; }
    [[ "${CONFIRM_PRUNE_UNUSED_IMAGES:-}" == yes ]] || {
      echo "set CONFIRM_PRUNE_UNUSED_IMAGES=yes to prune every unused operator-VM image" >&2
      exit 2
    }

    prune_worker=$(base64 -w0 "$(dirname "$0")/operator-vm-prune-images-remote.sh")
    script=$(cat <<'SCRIPT'
set -Eeuo pipefail
prune_service=gantry-benchmark-image-prune.service
prune_script=/var/lib/gantry-benchmark/prune-unused-images.sh
prune_log=/var/log/gantry-benchmark/image-prune.log
if systemctl is-active --quiet "$prune_service"; then
  echo "$prune_service is already active" >&2
  exit 1
fi
printf '%s' '__PRUNE_WORKER_BASE64__' | base64 --decode >"$prune_script"
chmod 0700 "$prune_script"
"$prune_script" check
cat >/etc/systemd/system/gantry-benchmark-image-prune.service <<UNIT
[Unit]
Description=Prune unused Gantry benchmark operator images
After=network-online.target

[Service]
Type=oneshot
User=root
ExecStart=$prune_script run
StandardOutput=append:$prune_log
StandardError=append:$prune_log
TimeoutStartSec=45min
UNIT
systemctl daemon-reload
if systemctl is-failed --quiet "$prune_service"; then
  systemctl reset-failed "$prune_service"
fi
: >"$prune_log"
systemctl start --no-block "$prune_service"
systemctl show "$prune_service" --property=ActiveState --property=SubState --no-pager
SCRIPT
)
    script=${script/__PRUNE_WORKER_BASE64__/$prune_worker}
    invoke_remote "$script"
    ;;
  prune-status)
    (($# == 0)) || { usage >&2; exit 2; }

    script=$(cat <<'SCRIPT'
set -u
prune_service=gantry-benchmark-image-prune.service
prune_log=/var/log/gantry-benchmark/image-prune.log
if ! systemctl cat "$prune_service" >/dev/null 2>&1; then
  echo "$prune_service has not been installed"
  exit 0
fi
systemctl show "$prune_service" \
  --property=ActiveState --property=SubState --property=Result --property=ExecMainStatus --no-pager
printf '\n=== Recent log ===\n'
tail -40 "$prune_log" 2>/dev/null || true
printf '\n=== VM space ===\n'
source /etc/gantry-benchmark/env
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
