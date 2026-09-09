#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

set -Eeuo pipefail

usage() {
  cat <<'USAGE'
Usage: operator-vm-deploy-runtime.sh start
  operator-vm-deploy-runtime.sh status
  operator-vm-deploy-runtime.sh watch
  operator-vm-deploy-runtime.sh accelerate

Transfers the validated Gantry runtime changes to the private operator VM,
then asynchronously tests, builds, pushes, and rolls out the image before
starting a standalone benchmark run.
USAGE
}

(($# == 1)) || { usage >&2; exit 2; }
action=$1

repo_root=$(git rev-parse --show-toplevel)
config_file=${DEPLOY_CONFIG:-$repo_root/hack/gantry-benchmark/deploy.env}
[[ -f "$config_file" ]] || { echo "missing deployment config: $config_file" >&2; exit 1; }

set -a
# shellcheck source=/dev/null
. "$config_file"
set +a

: "${DEPLOYMENT_NAME:?DEPLOYMENT_NAME is required in $config_file}"

ssh_config=${OPERATOR_SSH_CONFIG:-$repo_root/tmp/$DEPLOYMENT_NAME/ssh-config}
ssh_target=${OPERATOR_SSH_TARGET:-gantry-benchmark-operator}
[[ -f "$ssh_config" ]] || { echo "missing operator SSH config: $ssh_config" >&2; exit 1; }

ssh_args=(-F "$ssh_config" -o BatchMode=yes -o ConnectTimeout=20 -T "$ssh_target")
remote_service=gantry-benchmark-runtime-deploy.service
remote_log=/var/log/gantry-benchmark/runtime-deploy.log
remote_progress=/var/lib/gantry-benchmark/runtime-deploy-progress.json
remote_result=/var/lib/gantry-benchmark/runtime-deploy-result.json

case "$action" in
  accelerate)
    exec ssh "${ssh_args[@]}" "sudo -n bash -s" <<'SCRIPT'
set -Eeuo pipefail
source /etc/gantry-benchmark/env
export HOME="${BENCHMARK_OPERATOR_HOME:-/var/lib/gantry-benchmark}"
export KUBECONFIG="${KUBECONFIG:-$HOME/kubeconfig}"

namespace=${GANTRY_NAMESPACE:-gantry-system}
daemonset=${GANTRY_DAEMONSET:-gantry}
before=$(kubectl -n "$namespace" get daemonset "$daemonset" -o jsonpath='{.spec.updateStrategy.rollingUpdate.maxUnavailable}')
kubectl -n "$namespace" patch daemonset "$daemonset" --type=merge \
  -p '{"spec":{"updateStrategy":{"type":"RollingUpdate","rollingUpdate":{"maxUnavailable":100}}}}'
after=$(kubectl -n "$namespace" get daemonset "$daemonset" -o jsonpath='{.spec.updateStrategy.rollingUpdate.maxUnavailable}')
printf 'maxUnavailable: %s -> %s\n' "$before" "$after"
kubectl -n "$namespace" get daemonset "$daemonset" \
  -o custom-columns=DESIRED:.status.desiredNumberScheduled,READY:.status.numberReady,UPDATED:.status.updatedNumberScheduled,AVAILABLE:.status.numberAvailable \
  --no-headers
SCRIPT
    ;;
  watch)
    exec ssh "${ssh_args[@]}" \
      "pid=\$(sudo -n systemctl show '$remote_service' --property=MainPID --value); test \"\$pid\" -gt 0; exec sudo -n tail --pid=\"\$pid\" -n 40 -F '$remote_log'"
    ;;
  status)
    exec ssh "${ssh_args[@]}" "sudo -n bash -s" <<SCRIPT
set -u
printf '=== Fixed Gantry deployment ===\n'
if systemctl cat '$remote_service' >/dev/null 2>&1; then
  systemctl show '$remote_service' \
    --property=ActiveState --property=SubState --property=Result --property=ExecMainStatus --no-pager
else
  echo 'service: not installed'
fi
printf '\n=== Progress ===\n'
cat '$remote_progress' 2>/dev/null || echo '{}'
printf '\n=== Result ===\n'
cat '$remote_result' 2>/dev/null || echo '{}'
printf '\n=== Benchmark operator ===\n'
systemctl show gantry-benchmark-operator.service \
  --property=ActiveState --property=SubState --property=Result --property=ExecMainStatus --no-pager
printf '\n=== Gantry rollout ===\n'
source /etc/gantry-benchmark/env
export HOME="\${BENCHMARK_OPERATOR_HOME:-/var/lib/gantry-benchmark}"
export KUBECONFIG="\${KUBECONFIG:-\$HOME/kubeconfig}"
kubectl -n gantry-system get daemonset gantry \
  -o custom-columns=DESIRED:.status.desiredNumberScheduled,READY:.status.numberReady,UPDATED:.status.updatedNumberScheduled,AVAILABLE:.status.numberAvailable,IMAGE:.spec.template.spec.containers[0].image \
  --no-headers 2>/dev/null || true
printf '\n=== Benchmark lifecycle ===\n'
"\$BENCHMARK_REPO_ROOT/hack/gantry-benchmark/operator-vm-status.sh"
printf '\n=== Gantry phase result ===\n'
state_json=\$(kubectl -n "\${BENCHMARK_NAMESPACE:-gantry-benchmark}" get configmap gantry-benchmark-state \
  -o jsonpath='{.data.state\.json}' 2>/dev/null || true)
run_id=
if [[ -n "\$state_json" ]]; then
  run_id=\$(jq -r '.run_id // empty' <<<"\$state_json")
fi
if [[ -z "\$run_id" && -f "\${BENCHMARK_ARTIFACT_ROOT:-/var/lib/gantry-benchmark/artifacts}/progress.json" ]]; then
  run_id=\$(jq -r '.run_id // empty' "\${BENCHMARK_ARTIFACT_ROOT:-/var/lib/gantry-benchmark/artifacts}/progress.json")
fi
for result_path in \
  "\$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/\$run_id/gantry-cold.json" \
  "\${BENCHMARK_ARTIFACT_ROOT:-/var/lib/gantry-benchmark/artifacts}/\$run_id/gantry-cold.json"; do
  if [[ -n "\$run_id" && -f "\$result_path" ]]; then
    jq '{
      run_id,
      phase,
      image,
      image_size_mib,
      image_layers,
      gantry,
      gantry_peer: (.gantry_peer | {total, source, complete}),
      gantry_diagnostic_totals: ([.gantry_diagnostics.pods[].counter_deltas | to_entries[]]
        | sort_by(.key)
        | group_by(.key)
        | map({key: .[0].key, value: (map(.value) | add)})
        | from_entries),
      azure: {
        window: .azure.window,
        acr: .azure.acr,
        private_endpoint: .azure.private_endpoint,
        audit: (.azure.audit | {pod_startup_latency, source, complete}),
        complete: .azure.complete
      },
      job: {
        phase_started_at: .job.phase_started_at,
        phase_finished_at: .job.phase_finished_at,
        completed_pods: (.job.pods | length),
        pod_start_latency: .job.pod_start_latency,
        pod_finish_latency: .job.pod_finish_latency
      },
      origin_bytes,
      origin_bytes_source,
      pod_startup_latency,
      pod_startup_latency_source,
      recorded_at
    }' "\$result_path"
    break
  fi
done
printf '\n=== Performance evidence ===\n'
for performance_path in \
  "\$BENCHMARK_REPO_ROOT/tmp/gantry-benchmark/\$run_id/gantry_cold-performance.json" \
  "\${BENCHMARK_ARTIFACT_ROOT:-/var/lib/gantry-benchmark/artifacts}/\$run_id/gantry_cold-performance.json"; do
  if [[ -n "\$run_id" && -f "\$performance_path" ]]; then
    jq --arg acr "\$GANTRY_ACR_LOGIN_SERVER" '{
      window,
      complete,
      journal_event_counts: (.containerd_journal_events
        | sort_by(.type)
        | group_by(.type)
        | map({key: .[0].type, value: length})
        | from_entries),
      response_header_timeout_mentions: ([.containerd_journal_events[]
        | select(.message | contains("timeout awaiting response headers"))] | length),
      acr_image_reference_mentions: ([.containerd_journal_events[]
        | select(.message | contains(\$acr))] | length)
    }' "\$performance_path"
    break
  fi
done
printf '\n=== Recent deployment log ===\n'
tail -40 '$remote_log' 2>/dev/null || true
SCRIPT
    ;;
  start)
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

runtime_paths=(
  cmd/gantry/agent_metrics.go
  cmd/gantry/main.go
  cmd/gantry/origin_pull_test.go
  internal/gantry/chairs/cache.go
  internal/gantry/chairs/chairs.go
  internal/gantry/chairs/manager.go
  internal/gantry/chairs/manager_internal_test.go
  internal/gantry/chairs/manager_test.go
  internal/gantry/chairs/store_test.go
  internal/gantry/coldstart/chair.go
  internal/gantry/coldstart/chair_test.go
  internal/gantry/config/config.go
  internal/gantry/config/config_test.go
  internal/gantry/coord/coord.go
  internal/gantry/coord/coord_test.go
  internal/gantry/discovery/discovery.go
  internal/gantry/ifaces/fakes/fakes.go
  internal/gantry/ifaces/ifaces.go
  internal/gantry/metrics/metrics.go
  internal/gantry/mirror/mirror.go
  internal/gantry/mirror/mirror_coldstart_test.go
  internal/gantry/mirror/mirror_peer_test.go
  internal/gantry/mirror/rediscover_test.go
  internal/gantry/transfer/client.go
  internal/gantry/transfer/client_test.go
)

git diff --check -- "${runtime_paths[@]}"

# The operator tree is an exported copy with no .git, so it cannot report its
# own revision. It was built from the branch tip, which stops being HEAD as soon
# as anything is committed locally; comparing against HEAD would then flag every
# untouched operator file as unknown.
runtime_base_rev=${RUNTIME_BASE_REV:-$(git rev-parse --verify --quiet '@{upstream}' || git rev-parse HEAD)}

base_hashes_base64=$(
  for path in "${runtime_paths[@]}"; do
    printf '%s  %s\n' \
      "$(git show "$runtime_base_rev:$path" | sha256sum | cut -d' ' -f1)" \
      "$path"
  done | base64 -w0
)
target_hashes=$(
  sha256sum "${runtime_paths[@]}"
)
target_hashes_base64=$(printf '%s\n' "$target_hashes" | base64 -w0)
source_hash=$(printf '%s\n' "$target_hashes" | sha256sum | cut -d' ' -f1)
source_short=${source_hash:0:12}
base_revision=$(git rev-parse HEAD)
source_revision="${base_revision}-runtime-${source_short}"
remote_payload="/var/tmp/gantry-runtime-${source_short}.tar.gz"

tar -czf - "${runtime_paths[@]}" | ssh "${ssh_args[@]}" "cat > '$remote_payload'"

worker=$(cat <<'WORKER'
#!/usr/bin/env bash
set -Eeuo pipefail

source /etc/gantry-benchmark/env
export HOME="${BENCHMARK_OPERATOR_HOME:-/var/lib/gantry-benchmark}"
export KUBECONFIG="${KUBECONFIG:-$HOME/kubeconfig}"

runtime_log=/var/log/gantry-benchmark/runtime-deploy.log
runtime_progress=/var/lib/gantry-benchmark/runtime-deploy-progress.json
runtime_result=/var/lib/gantry-benchmark/runtime-deploy-result.json
source_revision=__SOURCE_REVISION__
source_short=__SOURCE_SHORT__

exec >>"$runtime_log" 2>&1

write_progress() {
  local stage=$1
  local message=$2
  local temporary="${runtime_progress}.tmp"

  jq -n \
    --arg stage "$stage" \
    --arg message "$message" \
    --arg source_revision "$source_revision" \
    --arg updated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{stage:$stage,message:$message,source_revision:$source_revision,updated_at:$updated_at}' \
    >"$temporary"
  mv "$temporary" "$runtime_progress"
}

failed() {
  local status=$?
  write_progress failed "fixed Gantry deployment exited with code $status"
  exit "$status"
}
trap failed ERR

cd "$BENCHMARK_REPO_ROOT"

write_progress test "running Gantry runtime tests"
GOTOOLCHAIN=auto go test ./cmd/gantry ./internal/gantry/...

write_progress authenticate "authenticating managed identity to Gantry ACR"
az login --identity --allow-no-subscriptions --output none
az account set --subscription "$AZURE_SUBSCRIPTION_ID"
token=$(az acr login --name "$GANTRY_ACR_NAME" --expose-token --query accessToken -o tsv)
printf '%s' "$token" | podman login "$GANTRY_ACR_LOGIN_SERVER" \
  --username 00000000-0000-0000-0000-000000000000 \
  --password-stdin
unset token
trap 'podman logout "$GANTRY_ACR_LOGIN_SERVER" >/dev/null 2>&1 || true' EXIT

gantry_tag="$GANTRY_ACR_LOGIN_SERVER/gantry:runtime-$source_short"
write_progress build "building fixed Gantry image $gantry_tag"
podman build --isolation chroot --platform linux/amd64 \
  --build-arg "VERSION=runtime-$source_short" \
  --build-arg "GIT_COMMIT=$source_revision" \
  --tag "$gantry_tag" --file images/gantry/Containerfile .

digest_file=/var/lib/gantry-benchmark/gantry-runtime-deploy.digest
write_progress push "pushing fixed Gantry image"
podman push --digestfile "$digest_file" "$gantry_tag"
gantry_digest=$(tr -d '[:space:]' <"$digest_file")
gantry_image="$GANTRY_ACR_LOGIN_SERVER/gantry@$gantry_digest"
podman logout "$GANTRY_ACR_LOGIN_SERVER" >/dev/null 2>&1 || true
trap - EXIT

write_progress rollout "rolling fixed Gantry image to all nodes"
kubectl -n "$GANTRY_NAMESPACE" patch daemonset "$GANTRY_DAEMONSET" --type=merge \
  -p '{"spec":{"updateStrategy":{"type":"RollingUpdate","rollingUpdate":{"maxUnavailable":100}}}}'
kubectl -n "$GANTRY_NAMESPACE" set image daemonset/"$GANTRY_DAEMONSET" gantry="$gantry_image"
kubectl -n "$GANTRY_NAMESPACE" rollout status daemonset/"$GANTRY_DAEMONSET" --timeout=45m

rollout=$(kubectl -n "$GANTRY_NAMESPACE" get daemonset "$GANTRY_DAEMONSET" -o json)
desired=$(jq -r '.status.desiredNumberScheduled // 0' <<<"$rollout")
ready=$(jq -r '.status.numberReady // 0' <<<"$rollout")
updated=$(jq -r '.status.updatedNumberScheduled // 0' <<<"$rollout")
deployed_image=$(jq -r '.spec.template.spec.containers[] | select(.name=="gantry") | .image' <<<"$rollout")
[[ "$desired" == "$BENCHMARK_NODE_COUNT" && "$ready" == "$desired" && "$updated" == "$desired" ]] || {
  echo "Gantry rollout desired=$desired ready=$ready updated=$updated, want $BENCHMARK_NODE_COUNT" >&2
  exit 1
}
[[ "$deployed_image" == "$gantry_image" ]] || {
  echo "deployed Gantry image $deployed_image, want $gantry_image" >&2
  exit 1
}

write_progress launch "starting standalone Gantry benchmark"
sed -i \
  -e '/^GANTRY_ONLY_/d' \
  -e '/^ADOPT_BASELINE_IMAGE=/d' \
  -e '/^ADOPT_GANTRY_IMAGE=/d' \
  -e '/^ADOPT_PAYLOAD_SHA256=/d' \
  /etc/gantry-benchmark/env
printf 'GANTRY_ONLY_STANDALONE="true"\n' >>/etc/gantry-benchmark/env

if systemctl is-failed --quiet gantry-benchmark-operator.service; then
  systemctl reset-failed gantry-benchmark-operator.service
fi
systemctl start --no-block gantry-benchmark-operator.service

jq -n \
  --arg source_revision "$source_revision" \
  --arg image "$gantry_image" \
  --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson desired "$desired" \
  --argjson ready "$ready" \
  '{source_revision:$source_revision,image:$image,desired:$desired,ready:$ready,benchmark_started:true,completed_at:$completed_at}' \
  >"$runtime_result"
write_progress completed "fixed Gantry is ready and standalone benchmark was started"
WORKER
)
worker=${worker//__SOURCE_REVISION__/$source_revision}
worker=${worker//__SOURCE_SHORT__/$source_short}
worker=$(printf '%s' "$worker" | base64 -w0)

ssh "${ssh_args[@]}" "sudo -n bash -s" <<SCRIPT
set -Eeuo pipefail

runtime_service='$remote_service'
runtime_script=/var/lib/gantry-benchmark/deploy-fixed-runtime.sh
runtime_log='$remote_log'
runtime_progress='$remote_progress'
runtime_result='$remote_result'
payload='$remote_payload'

for service in gantry-benchmark-operator.service gantry-benchmark-image-builder.service gantry-benchmark-image-prune.service "\$runtime_service"; do
  service_state="\$(systemctl is-active "\$service" 2>/dev/null || true)"
  case "\$service_state" in
    active|activating)
      echo "\$service is \$service_state; refusing fixed Gantry deployment" >&2
      exit 1
      ;;
  esac
done

source /etc/gantry-benchmark/env
export HOME="\${BENCHMARK_OPERATOR_HOME:-/var/lib/gantry-benchmark}"
export KUBECONFIG="\${KUBECONFIG:-\$HOME/kubeconfig}"

if kubectl -n "\${BENCHMARK_NAMESPACE:-gantry-benchmark}" get configmap gantry-benchmark-state >/dev/null 2>&1 ||
  kubectl -n "\${GANTRY_NAMESPACE:-gantry-system}" get configmap gantry-benchmark-lock >/dev/null 2>&1; then
  echo "benchmark state or lock is active; refusing fixed Gantry deployment" >&2
  exit 1
fi

base_hashes="\$(mktemp)"
target_hashes="\$(mktemp)"
trap 'rm -f "\$base_hashes" "\$target_hashes" "\$payload"' EXIT
printf '%s' '$base_hashes_base64' | base64 --decode >"\$base_hashes"
printf '%s' '$target_hashes_base64' | base64 --decode >"\$target_hashes"
cd "\$BENCHMARK_REPO_ROOT"

if sha256sum --check --strict "\$target_hashes" >/dev/null 2>&1; then
  echo 'fixed Gantry source is already present'
else
  # Fixes land incrementally, so the operator legitimately holds a mix of tested
  # base and already-deployed files. Verify each path is one or the other.
  while read -r want path; do
    have="\$(sha256sum "\$path" | cut -d' ' -f1)"
    base="\$(awk -v p="\$path" '\$2 == p { print \$1 }' "\$base_hashes")"

    if [ "\$have" != "\$want" ] && [ "\$have" != "\$base" ]; then
      echo "operator file \$path matches neither the tested base nor the fixed contents" >&2
      exit 1
    fi
  done <"\$target_hashes"

  tar -xzf "\$payload"
fi
sha256sum --check --strict "\$target_hashes" >/dev/null

printf '%s' '$worker' | base64 --decode >"\$runtime_script"
chmod 0700 "\$runtime_script"
cat >/etc/systemd/system/"\$runtime_service" <<UNIT
[Unit]
Description=Build and deploy fixed Gantry runtime, then start standalone benchmark
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
ExecStart=\$runtime_script
StandardOutput=append:\$runtime_log
StandardError=append:\$runtime_log
TimeoutStartSec=0
TimeoutStopSec=45min
UNIT

rm -f "\$runtime_progress" "\$runtime_result"
: >"\$runtime_log"
systemctl daemon-reload
if systemctl is-failed --quiet "\$runtime_service"; then
  systemctl reset-failed "\$runtime_service"
fi
systemctl start --no-block "\$runtime_service"
systemctl show "\$runtime_service" --property=ActiveState --property=SubState --no-pager
SCRIPT

printf 'submitted fixed Gantry runtime %s\n' "$source_revision"