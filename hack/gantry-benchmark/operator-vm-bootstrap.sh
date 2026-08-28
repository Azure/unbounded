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
  <build-disk-lun> <build-mount> <source-image> <source-revision> \
  <adopt-baseline-image> <adopt-gantry-image> <adopt-payload-sha256> \
  <auto-reuse-images>
USAGE
}

[[ $# -eq 24 ]] || { usage >&2; exit 2; }

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
source_image=${19}
source_revision=${20}
adopt_baseline_image=${21}
adopt_gantry_image=${22}
adopt_payload_sha256=${23}
auto_reuse_images=${24}
[[ "$adopt_baseline_image" != - ]] || adopt_baseline_image=""
[[ "$adopt_gantry_image" != - ]] || adopt_gantry_image=""
[[ "$adopt_payload_sha256" != - ]] || adopt_payload_sha256=""

adoption_values=0
for value in "$adopt_baseline_image" "$adopt_gantry_image" "$adopt_payload_sha256"; do
  [[ -z "$value" ]] || adoption_values=$((adoption_values + 1))
done
if ((adoption_values != 0 && adoption_values != 3)); then
  echo "adopted baseline image, Gantry image, and payload digest must be set together" >&2
  exit 2
fi
[[ "$auto_reuse_images" == true || "$auto_reuse_images" == false ]] || {
  echo "auto-reuse-images must be true or false" >&2
  exit 2
}

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

acr_access_token() {
  local acr_name=$1
  local attempts=0
  local maximum=60
  local token

  until token=$(az acr login --name "$acr_name" --expose-token --query accessToken -o tsv); do
    attempts=$((attempts + 1))
    if ((attempts >= maximum)); then
      echo "failed to obtain managed-identity ACR token for $acr_name after $attempts attempts" >&2
      return 1
    fi
    sleep 10
  done

  printf '%s' "$token"
}

private_dns_ip() {
  local record_name=$1
  local attempts=0
  local maximum=60
  local ip

  until ip=$(az network private-dns record-set a show \
    --resource-group "$resource_group" \
    --zone-name privatelink.azurecr.io \
    --name "$record_name" \
    --query 'aRecords[0].ipv4Address' \
    --output tsv 2>/dev/null) && [[ -n "$ip" ]]; do
    attempts=$((attempts + 1))
    if ((attempts >= maximum)); then
      echo "private DNS record $record_name did not become readable" >&2
      return 1
    fi
    sleep 10
  done

  printf '%s' "$ip"
}

require_private_resolution() {
  local host=$1
  local expected_ip=$2
  local attempts=0
  local maximum=60

  local resolved
  until resolved=$(getent ahostsv4 "$host" | awk '{print $1}' | sort -u) && [[ "$resolved" == "$expected_ip" ]]; do
    attempts=$((attempts + 1))
    if ((attempts >= maximum)); then
      echo "$host did not resolve to private IP $expected_ip" >&2
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

retry az login --identity --allow-no-subscriptions --output none
az account set --subscription "$subscription_id"

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

retry az acr show -g "$resource_group" -n "$baseline_acr_name" --output none
retry az acr show -g "$resource_group" -n "$gantry_acr_name" --output none
for acr_name in "$baseline_acr_name" "$gantry_acr_name"; do
  public_access=$(az acr show -g "$resource_group" -n "$acr_name" --query publicNetworkAccess -o tsv)
  data_endpoint=$(az acr show -g "$resource_group" -n "$acr_name" --query dataEndpointEnabled -o tsv)
  [[ "$public_access" == Disabled ]] || { echo "$acr_name public access is $public_access, want Disabled" >&2; exit 1; }
  [[ "$data_endpoint" == true ]] || { echo "$acr_name dedicated data endpoint is not enabled" >&2; exit 1; }
done

baseline_location=$(az acr show -g "$resource_group" -n "$baseline_acr_name" --query location -o tsv)
gantry_location=$(az acr show -g "$resource_group" -n "$gantry_acr_name" --query location -o tsv)
baseline_login_ip=$(private_dns_ip "$baseline_acr_name")
baseline_data_ip=$(private_dns_ip "$baseline_acr_name.$baseline_location.data")
gantry_login_ip=$(private_dns_ip "$gantry_acr_name")
gantry_data_ip=$(private_dns_ip "$gantry_acr_name.$gantry_location.data")
require_private_resolution "$baseline_acr_name.azurecr.io" "$baseline_login_ip"
require_private_resolution "$baseline_acr_name.$baseline_location.data.azurecr.io" "$baseline_data_ip"
require_private_resolution "$gantry_acr_name.azurecr.io" "$gantry_login_ip"
require_private_resolution "$gantry_acr_name.$gantry_location.data.azurecr.io" "$gantry_data_ip"

repo_root="$build_mount/unbounded"
source_description="$repo_url ($repo_branch)"
for lifecycle_service in gantry-benchmark-operator.service gantry-benchmark-image-builder.service; do
  if systemctl is-active --quiet "$lifecycle_service"; then
    echo "$lifecycle_service is active; finish it before refreshing the operator checkout" >&2
    exit 1
  fi
done
if [[ -n "$source_image" ]]; then
  gantry_login_server=$(az acr show -g "$resource_group" -n "$gantry_acr_name" --query loginServer -o tsv)
  source_token=$(acr_access_token "$gantry_acr_name")
  printf '%s' "$source_token" | podman login "$gantry_login_server" \
    --username 00000000-0000-0000-0000-000000000000 \
    --password-stdin
  unset source_token

  podman pull "$source_image"
  actual_source_revision=$(podman image inspect \
    --format '{{ index .Labels "org.opencontainers.image.revision" }}' \
    "$source_image")
  if [[ -n "$source_revision" && "$actual_source_revision" != "$source_revision" ]]; then
    echo "source image revision $actual_source_revision, want $source_revision" >&2
    exit 1
  fi
  source_container=$(podman create "$source_image")
  rm -rf "$repo_root"
  install -d -m 0755 "$repo_root"
  podman cp "$source_container:/workspace/." "$repo_root/"
  podman rm "$source_container"
  podman logout "$gantry_login_server"
  source_description="$source_image ($actual_source_revision)"
elif [[ -d "$repo_root/.git" ]]; then
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

retry az acr show -g "$resource_group" -n "$baseline_acr_name" --output none
retry az acr show -g "$resource_group" -n "$gantry_acr_name" --output none
retry az aks show -g "$resource_group" -n "$aks_cluster" --output none

baseline_acr_id=$(az acr show -g "$resource_group" -n "$baseline_acr_name" --query id -o tsv)
gantry_acr_id=$(az acr show -g "$resource_group" -n "$gantry_acr_name" --query id -o tsv)
aks_id=$(az aks show -g "$resource_group" -n "$aks_cluster" --query id -o tsv)
baseline_login_server=$(az acr show -g "$resource_group" -n "$baseline_acr_name" --query loginServer -o tsv)
gantry_login_server=$(az acr show -g "$resource_group" -n "$gantry_acr_name" --query loginServer -o tsv)

valid_adopted_image() {
  local image=$1
  local login_server=$2
  local prefix="$login_server/gantry-benchmark-pull@"
  [[ "$image" == "$prefix"* && "${image#"$prefix"}" =~ ^sha256:[0-9a-f]{64}$ ]]
}
if ((adoption_values == 3)); then
  valid_adopted_image "$adopt_baseline_image" "$baseline_login_server" || {
    echo "adopted baseline image is not an immutable gantry-benchmark-pull image in $baseline_login_server" >&2
    exit 2
  }
  valid_adopted_image "$adopt_gantry_image" "$gantry_login_server" || {
    echo "adopted Gantry image is not an immutable gantry-benchmark-pull image in $gantry_login_server" >&2
    exit 2
  }
  [[ "$adopt_payload_sha256" =~ ^sha256:[0-9a-f]{64}$ ]] || {
    echo "adopted payload fingerprint must be a sha256 digest" >&2
    exit 2
  }
fi

cat >/etc/gantry-benchmark/env <<ENV
AZURE_SUBSCRIPTION_ID="$subscription_id"
AZURE_RESOURCE_GROUP="$resource_group"
AZURE_AKS_CLUSTER_NAME="$aks_cluster"
BENCHMARK_REPO_ROOT="$repo_root"
BENCHMARK_BUILD_MOUNT="$build_mount"
BENCHMARK_ARTIFACT_ROOT="/var/lib/gantry-benchmark/artifacts"
BENCHMARK_OPERATOR_HOME="/var/lib/gantry-benchmark"
BENCHMARK_IMAGE_POOL_ROOT="/var/lib/gantry-benchmark/image-pool"
BENCHMARK_IMAGE_POOL_BUILD_ROOT="$repo_root/tmp/gantry-benchmark/image-pool-build"
BENCHMARK_IMAGE_POOL_PROGRESS="/var/lib/gantry-benchmark/image-pool-progress.json"
BENCHMARK_IMAGE_POOL_LOG="/var/lib/gantry-benchmark/image-pool-builder.log"
BENCHMARK_LIFECYCLE_LOCK="/var/lib/gantry-benchmark/benchmark-lifecycle.lock"
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
BENCHMARK_AUTO_REUSE_IMAGES="$auto_reuse_images"
ADOPT_BASELINE_IMAGE="$adopt_baseline_image"
ADOPT_GANTRY_IMAGE="$adopt_gantry_image"
ADOPT_PAYLOAD_SHA256="$adopt_payload_sha256"
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

cat >/etc/systemd/system/gantry-benchmark-image-builder.service <<UNIT
[Unit]
Description=Gantry reusable benchmark image pool builder
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
WorkingDirectory=$repo_root
Environment=GANTRY_BENCHMARK_CONFIG=/etc/gantry-benchmark/env
EnvironmentFile=/etc/gantry-benchmark/image-pool.env
ExecStart=$repo_root/hack/gantry-benchmark/operator-vm-prebuild-images.sh
StandardOutput=append:/var/log/gantry-benchmark/image-pool-service.log
StandardError=append:/var/log/gantry-benchmark/image-pool-service.log
TimeoutStartSec=0
TimeoutStopSec=45min

[Install]
WantedBy=multi-user.target
UNIT

if [[ ! -f /etc/gantry-benchmark/image-pool.env ]]; then
  cat >/etc/gantry-benchmark/image-pool.env <<'ENV'
GANTRY_IMAGE_POOL_COUNT="1"
ENV
fi

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
repo: $repo_root ($source_description)
build mount: $build_mount
Podman graph root: $podman_graph_root
config: /etc/gantry-benchmark/env
service: gantry-benchmark-operator.service
artifacts: /var/lib/gantry-benchmark/artifacts
SUMMARY
