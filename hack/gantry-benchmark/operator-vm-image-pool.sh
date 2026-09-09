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
  operator-vm-image-pool.sh compare-layers IMAGE_A IMAGE_B
  operator-vm-image-pool.sh resolve-standalone-image RUN_ID
  operator-vm-image-pool.sh prune
  operator-vm-image-pool.sh prune-status

Starts an asynchronous operator-VM image-pool build, a full benchmark, a
standalone Gantry run, a baseline-backed Gantry-only run, or an unused-image
prune. These operations are mutually exclusive because pushes to the Gantry ACR
during a measured phase invalidate Azure telemetry.

Set ADOPT_BASELINE_IMAGE, ADOPT_GANTRY_IMAGE, and ADOPT_PAYLOAD_SHA256 together
with "full" to reuse an existing digest-pinned image pair.

"resolve-standalone-image" reports the digest-pinned reference and payload
fingerprint that a previous standalone run pushed, so the pair can be adopted
without rebuilding the workload image.
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

    image_size_mib=${BENCHMARK_IMAGE_SIZE_MIB:-}
    image_layers=${BENCHMARK_IMAGE_LAYERS:-}
    if [[ -n "$image_size_mib" || -n "$image_layers" ]]; then
      [[ "$image_size_mib" =~ ^[1-9][0-9]*$ ]] || { echo "BENCHMARK_IMAGE_SIZE_MIB must be a positive integer" >&2; exit 2; }
      [[ "$image_layers" =~ ^[1-9][0-9]*$ ]] || { echo "BENCHMARK_IMAGE_LAYERS must be a positive integer" >&2; exit 2; }
      ((image_layers <= image_size_mib)) || { echo "BENCHMARK_IMAGE_LAYERS cannot exceed BENCHMARK_IMAGE_SIZE_MIB" >&2; exit 2; }
    fi

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
if [[ -n "$image_size_mib" ]]; then
  build_config=/etc/gantry-benchmark/image-pool-build.env
  cp /etc/gantry-benchmark/env "\$build_config"
  sed -i \
    -e 's/^BENCHMARK_IMAGE_SIZE_MIB=.*/BENCHMARK_IMAGE_SIZE_MIB="$image_size_mib"/' \
    -e 's/^BENCHMARK_IMAGE_LAYERS=.*/BENCHMARK_IMAGE_LAYERS="$image_layers"/' \
    "\$build_config"
  grep -q '^BENCHMARK_IMAGE_SIZE_MIB=' "\$build_config" || echo 'BENCHMARK_IMAGE_SIZE_MIB="$image_size_mib"' >>"\$build_config"
  grep -q '^BENCHMARK_IMAGE_LAYERS=' "\$build_config" || echo 'BENCHMARK_IMAGE_LAYERS="$image_layers"' >>"\$build_config"
  echo 'GANTRY_BENCHMARK_CONFIG=/etc/gantry-benchmark/image-pool-build.env' >>/etc/gantry-benchmark/image-pool.env
else
  rm -f /etc/gantry-benchmark/image-pool-build.env
fi
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
        if [[ -n "${GANTRY_ONLY_STANDALONE_ADOPT_IMAGE:-}" ]]; then
          : "${GANTRY_ONLY_STANDALONE_ADOPT_PAYLOAD_SHA256:?Set GANTRY_ONLY_STANDALONE_ADOPT_PAYLOAD_SHA256 with GANTRY_ONLY_STANDALONE_ADOPT_IMAGE}"
          [[ "$GANTRY_ONLY_STANDALONE_ADOPT_IMAGE" =~ ^[a-z0-9.-]+/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$ ]] || {
            echo "GANTRY_ONLY_STANDALONE_ADOPT_IMAGE must be an immutable digest reference" >&2
            exit 2
          }
          [[ "$GANTRY_ONLY_STANDALONE_ADOPT_PAYLOAD_SHA256" =~ ^sha256:[0-9a-f]{64}$ ]] || {
            echo "GANTRY_ONLY_STANDALONE_ADOPT_PAYLOAD_SHA256 must be a sha256 digest" >&2
            exit 2
          }
          mode_config+=$'\n'"GANTRY_ONLY_STANDALONE_ADOPT_IMAGE=\"$GANTRY_ONLY_STANDALONE_ADOPT_IMAGE\""
          mode_config+=$'\n'"GANTRY_ONLY_STANDALONE_ADOPT_PAYLOAD_SHA256=\"$GANTRY_ONLY_STANDALONE_ADOPT_PAYLOAD_SHA256\""
        fi
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
  if sha256sum --check --strict "\$standalone_patched_hashes" >/dev/null 2>&1; then
    echo "standalone benchmark source is already present"
  elif sha256sum --check --strict "\$standalone_base_hashes" >/dev/null 2>&1; then
    git apply --check "\$standalone_patch"
    git apply "\$standalone_patch"
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
  compare-layers)
    (($# == 2)) || { usage >&2; exit 2; }
    image_a=$1
    image_b=$2
    image_pattern='^[a-z0-9.-]+/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$'
    [[ "$image_a" =~ $image_pattern ]] || { echo "IMAGE_A must be an immutable digest reference" >&2; exit 2; }
    [[ "$image_b" =~ $image_pattern ]] || { echo "IMAGE_B must be an immutable digest reference" >&2; exit 2; }

    script=$(cat <<'SCRIPT'
set -Eeuo pipefail
source /etc/gantry-benchmark/env
image_a='__IMAGE_A__'
image_b='__IMAGE_B__'
cleanup() {
  unset aad_access_token refresh_token registry_token
}
trap cleanup EXIT

az login --identity --allow-no-subscriptions --output none
az account set --subscription "$AZURE_SUBSCRIPTION_ID"
tenant_id=$(az account show --query tenantId -o tsv)
aad_access_token=$(az account get-access-token --resource https://containerregistry.azure.net --query accessToken -o tsv)
refresh_token=$(curl -fsS -X POST \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode grant_type=access_token \
  --data-urlencode "service=$GANTRY_ACR_LOGIN_SERVER" \
  --data-urlencode "tenant=$tenant_id" \
  --data-urlencode "access_token=$aad_access_token" \
  "https://$GANTRY_ACR_LOGIN_SERVER/oauth2/exchange" | jq -er '.refresh_token')
unset aad_access_token

registry_a=${image_a%%/*}
registry_b=${image_b%%/*}
path_a=${image_a#*/}
path_b=${image_b#*/}
repository_a=${path_a%@*}
repository_b=${path_b%@*}
digest_a=${image_a##*@}
digest_b=${image_b##*@}
[[ "$registry_a" == "$GANTRY_ACR_LOGIN_SERVER" && "$registry_b" == "$GANTRY_ACR_LOGIN_SERVER" ]] || {
  echo "images must belong to $GANTRY_ACR_LOGIN_SERVER" >&2
  exit 1
}
[[ "$repository_a" == "$repository_b" ]] || { echo "images must use the same repository" >&2; exit 1; }

registry_token=$(curl -fsS -X POST \
  -H 'Content-Type: application/x-www-form-urlencoded' \
  --data-urlencode grant_type=refresh_token \
  --data-urlencode "service=$GANTRY_ACR_LOGIN_SERVER" \
  --data-urlencode "scope=repository:$repository_a:pull" \
  --data-urlencode "refresh_token=$refresh_token" \
  "https://$GANTRY_ACR_LOGIN_SERVER/oauth2/token" | jq -er '.access_token')
unset refresh_token

manifest_accept='application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
layers_a=$(curl -fsS \
  -H "Authorization: Bearer $registry_token" \
  -H "Accept: $manifest_accept" \
  "https://$GANTRY_ACR_LOGIN_SERVER/v2/$repository_a/manifests/$digest_a" |
  jq -ce '[.layers[].digest]')
layers_b=$(curl -fsS \
  -H "Authorization: Bearer $registry_token" \
  -H "Accept: $manifest_accept" \
  "https://$GANTRY_ACR_LOGIN_SERVER/v2/$repository_b/manifests/$digest_b" |
  jq -ce '[.layers[].digest]')
unset registry_token
jq -cn \
  --arg image_a "$image_a" \
  --arg image_b "$image_b" \
  --argjson layers_a "$layers_a" \
  --argjson layers_b "$layers_b" \
  '($layers_a | unique) as $layers_a | ($layers_b | unique) as $layers_b |
   ($layers_a - ($layers_a - $layers_b)) as $shared |
   {image_a:$image_a,image_b:$image_b,
    image_a_layers:($layers_a|length),image_b_layers:($layers_b|length),
    shared_layers:($shared|length),shared_digests:$shared,
    image_a_only:($layers_a-$layers_b),image_b_only:($layers_b-$layers_a)}'
SCRIPT
)
    script=${script//__IMAGE_A__/$image_a}
    script=${script//__IMAGE_B__/$image_b}
    invoke_remote "$script"
    ;;
  resolve-standalone-image)
    (($# == 1)) || { usage >&2; exit 2; }
    resolve_run_id=$1
    [[ "$resolve_run_id" =~ ^[A-Za-z0-9._-]+$ ]] || {
      echo "run ID must contain only letters, digits, dots, dashes, or underscores" >&2
      exit 2
    }

    script=$(cat <<'SCRIPT'
set -u
source /etc/gantry-benchmark/env
run_id='__RUN_ID__'
# Mirrors buildFreshGantryOnlyImage's tag derivation.
tag="${run_id}-gantry-fresh"
tag="${tag//_/-}"
reference="$GANTRY_ACR_LOGIN_SERVER/$BENCHMARK_WORKLOAD_REPOSITORY:$tag"
printf 'tagged_reference: %s\n' "$reference"
if podman image exists "$reference"; then
  printf 'repo_digests: %s\n' "$(podman image inspect "$reference" --format '{{range .RepoDigests}}{{.}} {{end}}')"
  printf 'payload_sha256: %s\n' "$(podman image inspect "$reference" --format '{{index .Labels "io.unbounded.gantry-benchmark.payload-sha256"}}')"
else
  printf 'repo_digests: (tag not present in local podman storage)\n'
fi
printf '\n=== prepare log ===\n'
grep -aE "prepared standalone Gantry image|payload fingerprint|$BENCHMARK_WORKLOAD_REPOSITORY@sha256:" \
  /var/log/gantry-benchmark/service.log 2>/dev/null | tail -20 || true
SCRIPT
)
    script=${script//__RUN_ID__/$resolve_run_id}
  invoke_remote "$script"
    ;;
  standalone-source-status)
    (($# == 0)) || { usage >&2; exit 2; }

    script=$(cat <<'SCRIPT'
set -u
source /etc/gantry-benchmark/env
cd "$BENCHMARK_REPO_ROOT"
printf '=== HEAD ===\n'
git rev-parse HEAD
printf '\n=== standalone source hashes ===\n'
sha256sum \
  hack/cmd/gantry-benchmark/gantry_only.go \
  hack/cmd/gantry-benchmark/main.go \
  hack/cmd/gantry-benchmark/state.go \
  hack/gantry-benchmark/operator-vm-run.sh
printf '\n=== working tree ===\n'
git status --short -- \
  hack/cmd/gantry-benchmark/gantry_only.go \
  hack/cmd/gantry-benchmark/main.go \
  hack/cmd/gantry-benchmark/state.go \
  hack/gantry-benchmark/operator-vm-run.sh
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
