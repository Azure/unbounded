#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -Eeuo pipefail

usage() {
  cat <<'USAGE'
Usage: operator-vm-bootstrap.sh <subscription> <resource-group> <aks-cluster> \
  <baseline-acr> <gantry-acr> <workspace-customer-id> <baseline-pe-id> \
  <gantry-pe-id> <repo-url> <repo-branch> <node-count> <image-size-mib> \
  <image-layers> <azure-telemetry> <minimum-byte-reduction> <maximum-latency-ratio> \
  <build-disk-lun> <build-mount>
USAGE
}

[[ $# -eq 18 ]] || { usage >&2; exit 2; }

subscription_id=$1
resource_group=$2
aks_cluster=$3
baseline_acr_name=$4
gantry_acr_name=$5
workspace_customer_id=$6
baseline_private_endpoint_id=$7
gantry_private_endpoint_id=$8
repo_url=$9
repo_branch=${10}
node_count=${11}
image_size_mib=${12}
image_layers=${13}
azure_telemetry=${14}
minimum_byte_reduction=${15}
maximum_latency_ratio=${16}
build_disk_lun=${17}
build_mount=${18}

retry() {
  local attempts=0
  local maximum=18

  until "$@"; do
    attempts=$((attempts + 1))
    if ((attempts >= maximum)); then
      echo "command failed after $attempts attempts: $*" >&2
      return 1
    fi

    sleep 10
  done
}

export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y ca-certificates curl e2fsprogs git gnupg jq make podman golang-go

if ! command -v az >/dev/null 2>&1; then
  curl -sL https://aka.ms/InstallAzureCLIDeb | bash
fi

if ! command -v kubectl >/dev/null 2>&1; then
  az aks install-cli --install-location /usr/local/bin/kubectl
fi

build_device="/dev/disk/azure/scsi1/lun${build_disk_lun}"
retry test -b "$build_device"

if ! blkid "$build_device" >/dev/null 2>&1; then
  mkfs.ext4 -F -E lazy_itable_init=0,lazy_journal_init=0 "$build_device"
fi

build_uuid=$(blkid -s UUID -o value "$build_device")
install -d -m 0755 "$build_mount"
if ! grep -Fq "UUID=$build_uuid " /etc/fstab; then
  printf 'UUID=%s %s ext4 defaults,noatime,nofail 0 2\n' "$build_uuid" "$build_mount" >>/etc/fstab
fi
mount "$build_mount" 2>/dev/null || mount -a
findmnt --mountpoint "$build_mount" >/dev/null

install -d -m 0711 "$build_mount/containers"
install -d -m 0755 /etc/containers
cat >/etc/containers/storage.conf <<STORAGE
[storage]
driver = "overlay"
runroot = "/run/containers/storage"
graphroot = "$build_mount/containers"

[storage.options.overlay]
STORAGE

install -d -m 0700 /var/lib/gantry-benchmark
install -d -m 0750 /etc/gantry-benchmark
install -d -m 0750 /var/log/gantry-benchmark

repo_root="$build_mount/unbounded"
if [[ -d "$repo_root/.git" ]]; then
  git -C "$repo_root" fetch origin "$repo_branch"
  git -C "$repo_root" checkout -B "$repo_branch" "origin/$repo_branch"
else
  rm -rf "$repo_root"
  git clone --branch "$repo_branch" --single-branch "$repo_url" "$repo_root"
fi

chmod +x "$repo_root"/hack/gantry-benchmark/operator-vm-*.sh

podman_graph_root=$(podman info --format '{{.Store.GraphRoot}}')
[[ "$podman_graph_root" == "$build_mount/containers" ]] || {
  echo "Podman graph root $podman_graph_root, want $build_mount/containers" >&2
  exit 1
}

retry az login --identity --allow-no-subscriptions --output none
az account set --subscription "$subscription_id"

retry az acr show -g "$resource_group" -n "$baseline_acr_name" --output none
retry az acr show -g "$resource_group" -n "$gantry_acr_name" --output none
retry az aks show -g "$resource_group" -n "$aks_cluster" --output none

baseline_acr_id=$(az acr show -g "$resource_group" -n "$baseline_acr_name" --query id -o tsv)
gantry_acr_id=$(az acr show -g "$resource_group" -n "$gantry_acr_name" --query id -o tsv)
aks_id=$(az aks show -g "$resource_group" -n "$aks_cluster" --query id -o tsv)
baseline_login_server=$(az acr show -g "$resource_group" -n "$baseline_acr_name" --query loginServer -o tsv)
gantry_login_server=$(az acr show -g "$resource_group" -n "$gantry_acr_name" --query loginServer -o tsv)

cat >/etc/gantry-benchmark/env <<ENV
AZURE_SUBSCRIPTION_ID="$subscription_id"
AZURE_RESOURCE_GROUP="$resource_group"
AZURE_AKS_CLUSTER_NAME="$aks_cluster"
BENCHMARK_REPO_ROOT="$repo_root"
BENCHMARK_BUILD_MOUNT="$build_mount"
BENCHMARK_ARTIFACT_ROOT="/var/lib/gantry-benchmark/artifacts"
BENCHMARK_OPERATOR_HOME="/var/lib/gantry-benchmark"
BENCHMARK_CONFIRM_CONTEXT="$aks_cluster"
BENCHMARK_MODE="direct"

BASELINE_ACR_NAME="$baseline_acr_name"
BASELINE_ACR_LOGIN_SERVER="$baseline_login_server"
BASELINE_ACR_USERNAME="00000000-0000-0000-0000-000000000000"
GANTRY_ACR_NAME="$gantry_acr_name"
GANTRY_ACR_LOGIN_SERVER="$gantry_login_server"
GANTRY_ACR_USERNAME="00000000-0000-0000-0000-000000000000"

BENCHMARK_AZURE_TELEMETRY="$azure_telemetry"
AZURE_LOG_ANALYTICS_WORKSPACE_ID="$workspace_customer_id"
AZURE_AKS_RESOURCE_ID="$aks_id"
AZURE_BASELINE_ACR_RESOURCE_ID="$baseline_acr_id"
AZURE_BASELINE_ACR_PRIVATE_ENDPOINT_RESOURCE_ID="$baseline_private_endpoint_id"
AZURE_GANTRY_ACR_RESOURCE_ID="$gantry_acr_id"
AZURE_GANTRY_ACR_PRIVATE_ENDPOINT_RESOURCE_ID="$gantry_private_endpoint_id"
BENCHMARK_TELEMETRY_TIMEOUT="20m"
BENCHMARK_TELEMETRY_POLL_INTERVAL="15s"

GANTRY_NAMESPACE="gantry-system"
GANTRY_DAEMONSET="gantry"
GANTRY_CONFIGMAP="gantry-config"
MONITORING_NAMESPACE="monitoring"
PROMETHEUS_SERVICE="kps-kube-prometheus-stack-prometheus"
KPS_RELEASE="kps"

BENCHMARK_NAMESPACE="gantry-benchmark"
BENCHMARK_NODE_COUNT="$node_count"
BENCHMARK_IMAGE_SIZE_MIB="$image_size_mib"
BENCHMARK_IMAGE_LAYERS="$image_layers"
BENCHMARK_IMAGE_PLATFORM="linux/amd64"
BENCHMARK_WORKLOAD_REPOSITORY="gantry-benchmark-pull"
BENCHMARK_ROLLOUT_TIMEOUT="15m"
BENCHMARK_MINIMUM_BYTE_REDUCTION="$minimum_byte_reduction"
BENCHMARK_MAXIMUM_LATENCY_RATIO="$maximum_latency_ratio"
CONTAINER_ENGINE="podman"
ENV
chmod 0600 /etc/gantry-benchmark/env

cat >/etc/systemd/system/gantry-benchmark-operator.service <<UNIT
[Unit]
Description=Gantry dual-ACR benchmark operator
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
WorkingDirectory=$repo_root
Environment=GANTRY_BENCHMARK_CONFIG=/etc/gantry-benchmark/env
ExecStart=$repo_root/hack/gantry-benchmark/operator-vm-run.sh
StandardOutput=append:/var/log/gantry-benchmark/service.log
StandardError=append:/var/log/gantry-benchmark/service.log
TimeoutStartSec=0
TimeoutStopSec=45min

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload

retry az aks get-credentials \
  --resource-group "$resource_group" \
  --name "$aks_cluster" \
  --admin \
  --file /var/lib/gantry-benchmark/kubeconfig \
  --overwrite-existing \
  --only-show-errors
chmod 0600 /var/lib/gantry-benchmark/kubeconfig
retry sh -c 'KUBECONFIG=/var/lib/gantry-benchmark/kubeconfig kubectl auth can-i "*" "*" --all-namespaces | grep -qx yes'

getent ahostsv4 "$baseline_login_server"
getent ahostsv4 "$gantry_login_server"
baseline_status=$(curl -sS -o /dev/null -w '%{http_code}' "https://$baseline_login_server/v2/")
gantry_status=$(curl -sS -o /dev/null -w '%{http_code}' "https://$gantry_login_server/v2/")
[[ "$baseline_status" == 401 ]] || { echo "baseline ACR returned HTTP $baseline_status, want 401" >&2; exit 1; }
[[ "$gantry_status" == 401 ]] || { echo "Gantry ACR returned HTTP $gantry_status, want 401" >&2; exit 1; }
echo "baseline ACR status: $baseline_status"
echo "Gantry ACR status: $gantry_status"

cat <<SUMMARY
operator VM bootstrap complete
repo: $repo_root ($repo_branch)
build mount: $build_mount
Podman graph root: $podman_graph_root
config: /etc/gantry-benchmark/env
service: gantry-benchmark-operator.service
artifacts: /var/lib/gantry-benchmark/artifacts
SUMMARY
