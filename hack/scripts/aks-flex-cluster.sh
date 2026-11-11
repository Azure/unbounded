#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Self-contained, repeatable AKS + Unbounded-Net + AKS Flex Node deployment.
# All Azure resources are created through az. Release mode needs no local build;
# --build-from-checkout builds and publishes the local Unbounded components.
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
NAME=""
LOCATION=""
SUBSCRIPTION=""
RESOURCE_GROUP=""
FLEX_COUNT=3
WORKER_ARCH="amd64"
WORKER_IMAGE_SKU="server"
VM_SIZE=""
WORKER_OS_DISK_GB=512
WORKER_OS_DISK_SKU="Premium_LRS"
SYSTEM_VM_SIZE="Standard_D4s_v6"
KUBERNETES_VERSION=""
UNBOUNDED_VERSION="v0.8.0"
UNBOUNDED_VERSION_SET=false
AGENT_VERSION="v0.2.0"
BUILD_FROM_CHECKOUT=false
CHECKOUT_SOURCE_HASH=""
CONTAINER_ENGINE=""
BUILD_DNS=""
PLUGIN_BINARY=""
PLUGIN_DIGEST=""
IMAGE_REGISTRY=""
OPERATOR_IMAGE=""
ARTIFACT_STREAMING=false
VNET_CIDR="10.91.0.0/16"
CLUSTER_POD_CIDR="10.93.0.0/16"
FLEX_POD_CIDR="10.96.0.0/14"
SERVICE_CIDR="10.94.0.0/16"
SSH_KEY=""
SSH_SOURCE_IP=""
STATE_DIR=""
TIMEOUT=3600
BOOTSTRAP_TIMEOUT=""
ASSUME_YES=false
PLAN_ONLY=false
LOCK_HELD=false
ADMIN_USER="azureuser"
FLEX_POOL="flexpool"
OWNER_TAG="unbounded-flex-cluster"
CONFIG_TAG="unbounded-flex-config"
FLEX_ROLE="8f139b0f-7eaf-460b-a9da-5b1246d9ed0d"
NETWORK_ROLE="4d97b98b-1d4f-4787-a291-c67834d212e7"
POOL_API_VERSION="2026-05-02-preview"
VMSS_API_VERSION="2026-04-01"
NETWORK_API_VERSION="2023-06-01"
MAX_FLEX_COUNT=1000
SSH_PORT_START=50000
SSH_PORT_END=50999

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  printf '%s\n' 'Usage: bash hack/scripts/aks-flex-cluster.sh --name NAME --location REGION [options]'
  printf '%s\n' '
Creates one AKS system node and one Uniform VMSS with three Ubuntu 24.04 Flex
instances by default. All worker instances share one managed identity and one
load-balancer public IP for outbound access and per-instance SSH NAT ports.
Each Flex worker uses a 512 GiB Premium SSD (Premium_LRS) OS disk.
New VMSS instances bootstrap automatically, including after a scale-out.
Requires az >= 2.90.0, kubectl, jq, python3, curl, tar, openssl, and OpenSSH.
Flex workers default to amd64; --worker-arch arm64 selects an ARM64 Ubuntu host
and agent archive. The system node remains amd64 unless its VM size is changed.
Uses the active Azure account unless --subscription is supplied. The selected
subscription must support the Flex preview and publish the Flex Node Agent Role.
Gantry is installed without registry-specific configuration or authentication setup.
Checkout builds additionally require git, make, Go, npm, sha256sum, and Podman
or Docker Buildx configured for linux/amd64 and linux/arm64, with registry push access.

Options:
  --name NAME                 Required, lowercase resource prefix (3-30 characters)
  --location REGION           Required Azure region
  --subscription UUID         Azure subscription (default: active account)
  --resource-group NAME       Dedicated resource group (default: NAME)
  --flex-count N              Initial VMSS capacity (default: 3, range: 1-1000)
  --worker-arch ARCH          Flex architecture: amd64 or arm64 (default: amd64)
  --vm-size SKU               Flex VM size (default: Standard_D8s_v3 for amd64,
                              Standard_D4ps_v6 for arm64)
  --system-vm-size SKU        AKS system VM size (default: Standard_D4s_v6)
  --kubernetes-version V      Initial AKS version (default: AKS default)
  --unbounded-version TAG     Pinned plugin/operator release (default: v0.8.0)
  --agent-version TAG         Pinned Flex agent release (default: v0.2.0)
  --build-from-checkout       Build/push the local installer and components;
                              requires --image-registry and public pull access
  --engine NAME               Checkout builder: podman or docker
                              (default: Podman if installed, otherwise Docker)
  --build-dns IP              DNS server for Podman build containers only;
                              requires --build-from-checkout --engine podman
  --plugin-binary PATH        Use this local kubectl-unbounded instead of a release
  --image-registry PREFIX     Operator/component image prefix, e.g. ghcr.io/myorg
  --operator-image NAME:TAG   Operator name and tag, without a registry
  --artifact-streaming       Enable Gantry streaming on Flex workers and preload
                              NBD before nspawn starts (requires a compatible build)
  --vnet-cidr CIDR            IPv4 VNet, /16 through /20 (default: 10.91.0.0/16)
  --cluster-pod-cidr CIDR     System-site pods (default: 10.93.0.0/16)
  --flex-pod-cidr CIDR        Flex-site pods, /14 or larger (default: 10.96.0.0/14)
  --service-cidr CIDR         Services (default: 10.94.0.0/16)
  --ssh-key PATH              Existing private key (default: generate in state dir)
  --ssh-source-ip IPv4        SSH source (default: detected public IPv4); always /32
  --state-dir PATH            State/keys/cache (default: repo tmp/aks-flex/NAME)
  --timeout SECONDS           Per-phase wait timeout (default: 3600)
  --plan                      Print the plan without network calls or file writes
  --yes                       Skip confirmation
  -h, --help                  Show help

Keep the state directory and use the same provisioning arguments on reruns.
Changing SSH source IP is supported; topology, keys, sizes, and release changes
are rejected rather than silently replacing or downgrading existing resources.
Scale the worker VMSS separately with az vmss scale; reruns retain its current
capacity. Custom installer/image selections are pinned in the saved intent.
The compiled operator version must match the component image tags published
under --image-registry. ACR attachment is performed once with a compatible
plugin/operator; future Linux Flex nodes inherit that policy without reattaching ACR.
Checkout mode pins source contents and reuses its saved build on reruns. Source
changes require a new deployment/state directory, not an in-place upgrade.
The separate AKS Flex host agent still uses --agent-version in checkout mode.
This is not an upgrade, reset, or teardown tool. It never deletes Azure
resources or resets an existing node. A partially bootstrapped host that needs a
reset is reported, not automatically destroyed. See README.md for the quickstart.'
}

parse_args() {
  while (($#)); do
    case "$1" in
      --plan) PLAN_ONLY=true; shift ;;
      --yes) ASSUME_YES=true; shift ;;
      --artifact-streaming) ARTIFACT_STREAMING=true; shift ;;
      --build-from-checkout) BUILD_FROM_CHECKOUT=true; shift ;;
      -h|--help) usage; exit 0 ;;
      --name|--location|--subscription|--resource-group|--flex-count|--worker-arch|--vm-size|--system-vm-size|--kubernetes-version|--unbounded-version|--agent-version|--engine|--build-dns|--plugin-binary|--image-registry|--operator-image|--vnet-cidr|--cluster-pod-cidr|--flex-pod-cidr|--service-cidr|--ssh-key|--ssh-source-ip|--state-dir|--timeout)
        [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || die "$1 requires a value"
        case "$1" in
          --name) NAME="$2" ;;
          --location) LOCATION="$2" ;;
          --subscription) SUBSCRIPTION="$2" ;;
          --resource-group) RESOURCE_GROUP="$2" ;;
          --flex-count) FLEX_COUNT="$2" ;;
          --worker-arch) WORKER_ARCH="$2" ;;
          --vm-size) VM_SIZE="$2" ;;
          --system-vm-size) SYSTEM_VM_SIZE="$2" ;;
          --kubernetes-version) KUBERNETES_VERSION="$2" ;;
          --unbounded-version) UNBOUNDED_VERSION="$2"; UNBOUNDED_VERSION_SET=true ;;
          --agent-version) AGENT_VERSION="$2" ;;
          --engine) CONTAINER_ENGINE="$2" ;;
          --build-dns) BUILD_DNS="$2" ;;
          --plugin-binary) PLUGIN_BINARY="$2" ;;
          --image-registry) IMAGE_REGISTRY="$2" ;;
          --operator-image) OPERATOR_IMAGE="$2" ;;
          --vnet-cidr) VNET_CIDR="$2" ;;
          --cluster-pod-cidr) CLUSTER_POD_CIDR="$2" ;;
          --flex-pod-cidr) FLEX_POD_CIDR="$2" ;;
          --service-cidr) SERVICE_CIDR="$2" ;;
          --ssh-key) SSH_KEY="$2" ;;
          --ssh-source-ip) SSH_SOURCE_IP="$2" ;;
          --state-dir) STATE_DIR="$2" ;;
          --timeout) TIMEOUT="$2" ;;
        esac
        shift 2 ;;
      *) die "unknown option: $1" ;;
    esac
  done
}

validate_args() {
  [[ "$NAME" =~ ^[a-z][a-z0-9-]{1,28}[a-z0-9]$ ]] || die "--name must be 3-30 lowercase letters, digits, or hyphens, starting with a letter"
  [[ "$LOCATION" =~ ^[a-z][a-z0-9]+$ ]] || die "--location must be an Azure region name"
  RESOURCE_GROUP="${RESOURCE_GROUP:-$NAME}"
  [[ "$RESOURCE_GROUP" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,79}$ && "$RESOURCE_GROUP" != *. ]] || die "invalid resource group name"
  [[ "$FLEX_COUNT" =~ ^[1-9][0-9]{0,3}$ ]] || die "--flex-count must be 1-1000"
  ((FLEX_COUNT <= MAX_FLEX_COUNT)) || die "--flex-count must be 1-1000"
  [[ "$TIMEOUT" =~ ^[1-9][0-9]{0,5}$ ]] || die "--timeout must be a positive integer of at most six digits"
  case "$WORKER_ARCH" in
    amd64) VM_SIZE="${VM_SIZE:-Standard_D8s_v3}"; WORKER_IMAGE_SKU="server" ;;
    arm64) VM_SIZE="${VM_SIZE:-Standard_D4ps_v6}"; WORKER_IMAGE_SKU="server-arm64" ;;
    *) die "--worker-arch must be amd64 or arm64" ;;
  esac
  [[ "$VM_SIZE" =~ ^Standard_[a-zA-Z0-9_]+$ && "$SYSTEM_VM_SIZE" =~ ^Standard_[a-zA-Z0-9_]+$ ]] || die "invalid VM size"
  if [[ -n "$PLUGIN_BINARY" ]]; then
    [[ -f "$PLUGIN_BINARY" && -r "$PLUGIN_BINARY" && -x "$PLUGIN_BINARY" ]] || die "--plugin-binary must be a readable executable file"
    PLUGIN_BINARY="$(cd "$(dirname "$PLUGIN_BINARY")" && pwd)/$(basename "$PLUGIN_BINARY")"
  fi
  [[ -z "$IMAGE_REGISTRY" || "$IMAGE_REGISTRY" =~ ^[a-z0-9][a-z0-9.-]*(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)*$ ]] || die "--image-registry must be a registry/repository prefix without a scheme or tag"
  [[ -z "$OPERATOR_IMAGE" || "$OPERATOR_IMAGE" =~ ^[a-z0-9][a-z0-9._-]*:[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$ ]] || die "--operator-image must be NAME:TAG without a registry"
  [[ -z "$KUBERNETES_VERSION" || "$KUBERNETES_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--kubernetes-version must be a full x.y.z version"
  local tag
  for tag in "$AGENT_VERSION" "$UNBOUNDED_VERSION"; do
    [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][a-zA-Z0-9.-]+)?$ ]] || die "release versions must be pinned vX.Y.Z tags (optionally prerelease)"
  done
  case "$CONTAINER_ENGINE" in
    ""|podman|docker) ;;
    *) die "--engine must be podman or docker" ;;
  esac
  [[ -z "$CONTAINER_ENGINE" || "$BUILD_FROM_CHECKOUT" == true ]] || die "--engine requires --build-from-checkout"
  if [[ -n "$BUILD_DNS" ]]; then
    [[ "$BUILD_FROM_CHECKOUT" == true && "$CONTAINER_ENGINE" == podman ]] \
      || die "--build-dns requires --build-from-checkout --engine podman"
    python3 - "$BUILD_DNS" <<'PY' || die "--build-dns must be an IPv4 or IPv6 address"
import ipaddress
import sys
try:
    ipaddress.ip_address(sys.argv[1])
except ValueError:
    sys.exit(1)
PY
  fi
  if [[ "$BUILD_FROM_CHECKOUT" == true ]]; then
    [[ -n "$IMAGE_REGISTRY" ]] || die "--build-from-checkout requires --image-registry"
    [[ -z "$PLUGIN_BINARY" && -z "$OPERATOR_IMAGE" && "$UNBOUNDED_VERSION_SET" == false ]] \
      || die "--build-from-checkout cannot be combined with --plugin-binary, --operator-image, or --unbounded-version"
    command -v git >/dev/null 2>&1 || die "git is required for --build-from-checkout"
    CHECKOUT_SOURCE_HASH=$(checkout_source_hash) || die "cannot fingerprint checkout sources"
    UNBOUNDED_VERSION="checkout-$CHECKOUT_SOURCE_HASH"
    OPERATOR_IMAGE="unbounded-operator:$UNBOUNDED_VERSION"
  fi
  if [[ -n "$SUBSCRIPTION" ]]; then validate_subscription; fi
  if [[ -n "$SSH_SOURCE_IP" ]]; then validate_ssh_ip; fi
  local network
  network=$(python3 - "$VNET_CIDR" "$CLUSTER_POD_CIDR" "$FLEX_POD_CIDR" "$SERVICE_CIDR" <<'PY'
import ipaddress
import json
import sys
try:
    networks = [ipaddress.IPv4Network(value, strict=True) for value in sys.argv[1:]]
    vnet, cluster, flex, services = networks
    if not 16 <= vnet.prefixlen <= 20:
        raise ValueError("VNet must have a /16 through /20 prefix")
    if cluster.prefixlen > 24 or flex.prefixlen > 14:
        raise ValueError("system pods need /24 or larger; Flex pods need /14 or larger for 1000 /24 node blocks")
    if services.num_addresses <= 16:
        raise ValueError("service network must have more than 16 addresses")
    for index, network in enumerate(networks):
        if any(network.overlaps(other) for other in networks[index + 1:]):
            raise ValueError("VNet, pod, and service CIDRs must not overlap")
    worker_subnets = vnet.subnets(new_prefix=21)
    next(worker_subnets)
    print(json.dumps({"system": str(next(vnet.subnets(new_prefix=24))), "flex": str(next(worker_subnets)),
                      "dns": str(services.network_address + 10)}))
except ValueError as error:
    print(f"invalid network configuration: {error}", file=sys.stderr)
    sys.exit(1)
PY
  ) || die "invalid CIDRs"
  SYSTEM_NODE_CIDR=$(jq -r .system <<<"$network")
  FLEX_NODE_CIDR=$(jq -r .flex <<<"$network")
  DNS_SERVICE_IP=$(jq -r .dns <<<"$network")
  VMSS_NAME="$NAME-workers"
}

validate_subscription() {
  [[ "$SUBSCRIPTION" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]] || die "subscription must be a UUID"
}

validate_ssh_ip() {
  python3 - "$SSH_SOURCE_IP" <<'PY' || die "--ssh-source-ip must be a single public IPv4 address, not a CIDR"
import ipaddress
import sys
try:
    address = ipaddress.IPv4Address(sys.argv[1])
    if not address.is_global:
        raise ValueError("not a public IPv4 address")
except ValueError:
    sys.exit(1)
PY
}

print_plan() {
  printf '\nAKS Flex deployment\n'
  printf '  Cluster / resource group: %s / %s\n' "$NAME" "$RESOURCE_GROUP"
  printf '  Subscription / region:   %s / %s\n' "${SUBSCRIPTION:-active account}" "$LOCATION"
  printf '  Nodes:                    1 system + %s Flex = %s total\n' "$FLEX_COUNT" "$((FLEX_COUNT + 1))"
  printf '  VM sizes:                 system=%s flex=%s\n' "$SYSTEM_VM_SIZE" "$VM_SIZE"
  printf '  Flex OS disk:             %s GiB %s\n' "$WORKER_OS_DISK_GB" "$WORKER_OS_DISK_SKU"
  printf '  Flex architecture/image:  %s / Ubuntu 24.04 %s\n' "$WORKER_ARCH" "$WORKER_IMAGE_SKU"
  printf '  Initial Kubernetes:       %s\n' "${KUBERNETES_VERSION:-AKS default}"
  printf '  Operator / Flex agent:    %s / %s\n' "$UNBOUNDED_VERSION" "$AGENT_VERSION"
  [[ "$BUILD_FROM_CHECKOUT" != true ]] || printf '  Checkout build:           build/push linux/amd64,linux/arm64 before Azure provisioning\n'
  [[ "$BUILD_FROM_CHECKOUT" != true ]] || printf '  Container engine:         %s\n' "${CONTAINER_ENGINE:-auto (Podman preferred)}"
  [[ -z "$BUILD_DNS" ]] || printf '  Build-container DNS:      %s (host and cluster DNS unchanged)\n' "$BUILD_DNS"
  [[ -z "$PLUGIN_BINARY" ]] || printf '  Installer binary:         %s\n' "$PLUGIN_BINARY"
  [[ -z "$IMAGE_REGISTRY" ]] || printf '  Image registry:           %s\n' "$IMAGE_REGISTRY"
  [[ -z "$OPERATOR_IMAGE" ]] || printf '  Operator image:           %s\n' "$OPERATOR_IMAGE"
  [[ "$ARTIFACT_STREAMING" != true ]] || printf '  Gantry streaming:         enabled on Flex workers (NBD preloaded)\n'
  printf '  Node subnets:             %s / %s\n' "$SYSTEM_NODE_CIDR" "$FLEX_NODE_CIDR"
  printf '  Pod CIDRs:                %s / %s\n' "$CLUSTER_POD_CIDR" "$FLEX_POD_CIDR"
  printf '  Services / DNS:           %s / %s\n' "$SERVICE_CIDR" "$DNS_SERVICE_IP"
  if [[ -n "$SSH_SOURCE_IP" ]]; then
    printf '  SSH ingress:              %s/32\n' "$SSH_SOURCE_IP"
  else
    printf '  SSH ingress:              public IPv4 detected before provisioning\n'
  fi
  printf '  Worker topology:          Uniform VMSS, automatic instance bootstrap\n'
  printf '  Worker scale ceiling:     %s; one identity and shared outbound/SSH IP\n' "$MAX_FLEX_COUNT"
  printf '  State:                    %s\n\n' "${STATE_DIR:-$SCRIPT_ROOT/tmp/aks-flex/$NAME}"
}

az_cmd() { az "$@" --subscription "$SUBSCRIPTION" --only-show-errors; }
kube() { kubectl --kubeconfig "$STATE_DIR/kubeconfig" "$@"; }
sha256() { openssl dgst -sha256 "$1" | awk '{print $NF}'; }

checkout_source_hash() {
  python3 - "$SCRIPT_ROOT" <<'PY'
import hashlib
import os
from pathlib import Path
import subprocess
import sys

root = Path(sys.argv[1])
files = subprocess.run(
    ["git", "-C", str(root), "ls-files", "-co", "--exclude-standard", "-z"],
    check=True, capture_output=True,
).stdout.split(b"\0")
digest = hashlib.sha256()
for name in sorted(set(files) - {b""}):
    path = root / os.fsdecode(name)
    digest.update(name + b"\0")
    if path.is_symlink():
        digest.update(b"symlink\0" + os.fsencode(os.readlink(path)))
    elif path.is_file():
        digest.update(b"executable\0" if path.stat().st_mode & 0o111 else b"file\0")
        digest.update(hashlib.sha256(path.read_bytes()).digest())
    elif not path.exists():
        digest.update(b"deleted\0")
    else:
        raise ValueError(f"unsupported checkout entry: {name}")
print(digest.hexdigest())
PY
}

build_checkout() {
  local build_dir="$STATE_DIR/checkout/$CHECKOUT_SOURCE_HASH" marker image file commit build_time digest manifest
  local target_args=() build_args=()
  marker="$build_dir/complete.json"
  if [[ -f "$STATE_DIR/intent.json" ]]; then
    jq -e --arg source "$CHECKOUT_SOURCE_HASH" --arg registry "$IMAGE_REGISTRY" \
      '.checkoutSourceSHA256 == $source and .imageRegistry == $registry' "$STATE_DIR/intent.json" >/dev/null \
      || die "checkout source or registry differs from saved intent; use a new deployment and state directory"
    [[ -f "$marker" ]] || die "saved checkout build is missing; restore the state directory instead of rebuilding a deployed tag"
  fi
  PLUGIN_BINARY="$build_dir/kubectl-unbounded"
  if [[ -f "$marker" ]]; then
    [[ -x "$PLUGIN_BINARY" ]] || die "saved checkout installer is missing or not executable"
    digest=$(sha256 "$PLUGIN_BINARY")
    jq -e --arg registry "$IMAGE_REGISTRY" --arg digest "$digest" \
      '.imageRegistry == $registry and .pluginSHA256 == $digest' "$marker" >/dev/null \
      || die "saved checkout build registry or installer checksum does not match"
    log "Reusing saved checkout build $UNBOUNDED_VERSION"
    return
  fi
  mkdir -p "$build_dir"
  commit=$(git -C "$SCRIPT_ROOT" rev-parse HEAD)
  build_time=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  log "Building checkout installer and publishing matching images to $IMAGE_REGISTRY"
  make -C "$SCRIPT_ROOT" net-image-resources kubectl-unbounded-build \
    "VERSION=$UNBOUNDED_VERSION" "VERSION_TAG=$UNBOUNDED_VERSION" \
    "GIT_COMMIT=$commit" "BUILD_TIME=$build_time" "CONTAINER_REGISTRY=$IMAGE_REGISTRY" \
    "KUBECTL_UNBOUNDED_BIN=$PLUGIN_BINARY"
  [[ -x "$PLUGIN_BINARY" ]] || die "checkout build did not produce an executable installer"
  for image in unbounded-operator machina token-refresher gantry gantry-node-config unbounded-net-controller unbounded-net-node; do
    target_args=()
    case "$image" in
      unbounded-net-controller) file=images/net/Containerfile; target_args=(--target controller) ;;
      unbounded-net-node) file=images/net/Containerfile; target_args=(--target node) ;;
      *) file="images/$image/Containerfile" ;;
    esac
    build_args=(--platform linux/amd64,linux/arm64 --file "$SCRIPT_ROOT/$file" \
      --build-arg "VERSION=$UNBOUNDED_VERSION" --build-arg "GIT_COMMIT=$commit" \
      --build-arg "BUILD_TIME=$build_time" --build-arg "CONTAINER_REGISTRY=$IMAGE_REGISTRY")
    if [[ "$CONTAINER_ENGINE" == podman ]]; then
      [[ -z "$BUILD_DNS" ]] || build_args+=(--dns "$BUILD_DNS")
      # Use a fresh local index so interrupted builds cannot leave stale entries.
      manifest="localhost/aks-flex-$image:build-$(openssl rand -hex 16)"
      podman build "${build_args[@]}" ${target_args[@]+"${target_args[@]}"} \
        --manifest "$manifest" "$SCRIPT_ROOT"
      podman manifest push --all --rm "$manifest" "docker://$IMAGE_REGISTRY/$image:$UNBOUNDED_VERSION"
    else
      docker buildx build "${build_args[@]}" ${target_args[@]+"${target_args[@]}"} \
        --tag "$IMAGE_REGISTRY/$image:$UNBOUNDED_VERSION" --push "$SCRIPT_ROOT"
    fi
  done
  [[ "$(checkout_source_hash)" == "$CHECKOUT_SOURCE_HASH" ]] || die "checkout changed during the build; no deployment was provisioned"
  jq -n --arg registry "$IMAGE_REGISTRY" --arg digest "$(sha256 "$PLUGIN_BINARY")" \
    '{imageRegistry:$registry,pluginSHA256:$digest}' > "$marker"
}

version_at_least() {
  # Azure CLI has final versions; aks-preview also uses versions like 22.0.0b8.
  python3 - "$1" "$2" <<'PY'
import re
import sys
def version(value):
    match = re.fullmatch(r"(\d+)\.(\d+)\.(\d+)(?:b(\d+))?", value)
    if match is None:
        raise ValueError(value)
    major, minor, patch, beta = match.groups()
    return (int(major), int(minor), int(patch), int(beta) if beta else 10**9)
try:
    sys.exit(0 if version(sys.argv[1]) >= version(sys.argv[2]) else 1)
except ValueError:
    sys.exit(1)
PY
}

preflight() {
  local tool cli_version
  for tool in az kubectl jq python3 curl tar openssl ssh ssh-keygen base64; do
    command -v "$tool" >/dev/null 2>&1 || die "required command not found: $tool"
  done
  if [[ "$BUILD_FROM_CHECKOUT" == true ]]; then
    if [[ -z "$CONTAINER_ENGINE" ]]; then
      if command -v podman >/dev/null 2>&1; then CONTAINER_ENGINE=podman
      elif command -v docker >/dev/null 2>&1; then CONTAINER_ENGINE=docker
      else die "checkout builds require Podman or Docker Buildx; install one or select it with --engine"
      fi
    fi
    for tool in git make go npm sha256sum "$CONTAINER_ENGINE"; do
      command -v "$tool" >/dev/null 2>&1 || die "required checkout build command not found: $tool"
    done
    "$CONTAINER_ENGINE" info >/dev/null || die "$CONTAINER_ENGINE is not running; on macOS, start its VM first"
    if [[ "$CONTAINER_ENGINE" == docker ]]; then
      docker buildx inspect --bootstrap >/dev/null || die "a working Docker Buildx builder is required"
    fi
  fi
  cli_version=$(az version --query '"azure-cli"' -o tsv) || die "cannot read Azure CLI version"
  version_at_least "$cli_version" 2.90.0 || die "Azure CLI >= 2.90.0 required; found $cli_version"
  if [[ -z "$SUBSCRIPTION" ]]; then
    SUBSCRIPTION=$(az account show --query id -o tsv) || die "log in with az login first"
  fi
  validate_subscription
  az_cmd account show -o none || die "cannot access the selected subscription"
  validate_worker_architecture
  if [[ -z "$SSH_SOURCE_IP" ]]; then
    SSH_SOURCE_IP=$(curl -4 -fsS --connect-timeout 10 --max-time 30 https://api.ipify.org) || die "cannot detect public IPv4; pass --ssh-source-ip"
  fi
  validate_ssh_ip
}

validate_worker_architecture() {
  local skus architecture="x64"
  [[ "$WORKER_ARCH" != arm64 ]] || architecture="arm64"
  skus=$(az_cmd vm list-skus --location "$LOCATION" --resource-type virtualMachines --size "$VM_SIZE" --all -o json) \
    || die "cannot verify Flex VM architecture"
  jq -e --arg size "$VM_SIZE" --arg arch "$architecture" \
    'any(.[]; .name == $size and any(.capabilities[]?;
      .name == "CpuArchitectureType" and (.value | ascii_downcase) == $arch))' <<<"$skus" >/dev/null \
    || die "Flex VM size $VM_SIZE does not support --worker-arch $WORKER_ARCH in $LOCATION"
}

cleanup() {
  if [[ "$LOCK_HELD" == true ]]; then
    rmdir "$STATE_DIR/.lock" || log "Could not remove lock directory: $STATE_DIR/.lock"
  fi
}

on_exit() {
  local status=$?
  trap - EXIT
  cleanup
  exit "$status"
}

prepare_state() {
  umask 077
  STATE_DIR="${STATE_DIR:-$SCRIPT_ROOT/tmp/aks-flex/$NAME}"
  mkdir -p "$STATE_DIR"
  STATE_DIR="$(cd "$STATE_DIR" && pwd)"
  chmod 0700 "$STATE_DIR"
  mkdir -p "$STATE_DIR/cache"
  mkdir "$STATE_DIR/.lock" 2>/dev/null || die "state directory is locked: $STATE_DIR/.lock; check for another run before removing a stale lock"
  LOCK_HELD=true
  trap on_exit EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  if [[ -z "$SSH_KEY" ]]; then
    SSH_KEY="$STATE_DIR/id_ed25519"
    if [[ ! -f "$SSH_KEY" ]]; then
      [[ ! -e "$STATE_DIR/intent.json" ]] || die "saved SSH key is missing; restore it instead of generating a new identity"
      ssh-keygen -q -t ed25519 -N '' -f "$SSH_KEY"
    fi
  fi
  [[ -r "$SSH_KEY" ]] || die "SSH private key not readable: $SSH_KEY"
  SSH_KEY="$(cd "$(dirname "$SSH_KEY")" && pwd)/$(basename "$SSH_KEY")"
  ssh-keygen -y -f "$SSH_KEY" > "$STATE_DIR/ssh.pub" || die "cannot derive the SSH public key (unlock encrypted keys before use)"
  BOOTSTRAP_TIMEOUT="$TIMEOUT"
  if [[ -f "$STATE_DIR/intent.json" ]]; then
    BOOTSTRAP_TIMEOUT=$(jq -r '.bootstrapTimeout // empty' "$STATE_DIR/intent.json")
    [[ "$BOOTSTRAP_TIMEOUT" =~ ^[1-9][0-9]{0,5}$ ]] || die "saved intent is not a VMSS deployment; this script does not migrate standalone VMs"
  fi
  [[ "$BUILD_FROM_CHECKOUT" != true ]] || build_checkout
  [[ -z "$PLUGIN_BINARY" ]] || PLUGIN_DIGEST=$(sha256 "$PLUGIN_BINARY")
  # Preserve default intent bytes so existing resource ownership hashes still match.
  jq -n --arg subscription "$SUBSCRIPTION" --arg name "$NAME" --arg rg "$RESOURCE_GROUP" \
    --arg location "$LOCATION" --argjson count "$FLEX_COUNT" --arg vm "$VM_SIZE" \
    --arg systemVM "$SYSTEM_VM_SIZE" --arg k8s "$KUBERNETES_VERSION" \
    --arg unbounded "$UNBOUNDED_VERSION" --arg agent "$AGENT_VERSION" \
    --arg arch "$WORKER_ARCH" --arg plugin "$PLUGIN_DIGEST" --argjson streaming "$ARTIFACT_STREAMING" \
    --arg source "$CHECKOUT_SOURCE_HASH" \
    --arg registry "$IMAGE_REGISTRY" --arg operator "$OPERATOR_IMAGE" \
    --arg vnet "$VNET_CIDR" --arg clusterPods "$CLUSTER_POD_CIDR" \
    --arg flexPods "$FLEX_POD_CIDR" --arg services "$SERVICE_CIDR" \
    --rawfile sshKey "$STATE_DIR/ssh.pub" --argjson bootstrapTimeout "$BOOTSTRAP_TIMEOUT" \
    '{subscription:$subscription,name:$name,resourceGroup:$rg,location:$location,
      workerTopology:"vmss",bootstrapTimeout:$bootstrapTimeout,flexCount:$count,vmSize:$vm,systemVMSize:$systemVM,kubernetesVersion:$k8s,
      unboundedVersion:$unbounded,agentVersion:$agent,vnetCIDR:$vnet,
      clusterPodCIDR:$clusterPods,flexPodCIDR:$flexPods,serviceCIDR:$services,sshKey:$sshKey}
      | if $arch == "amd64" then . else . + {workerArchitecture:$arch} end
      | if $source == "" then . else . + {checkoutSourceSHA256:$source} end
      | if $streaming then . + {artifactStreaming:true} else . end
      | if $plugin == "" then . else . + {pluginSHA256:$plugin} end
      | if $registry == "" then . else . + {imageRegistry:$registry} end
      | if $operator == "" then . else . + {operatorImage:$operator} end' \
    > "$STATE_DIR/intent.next.json"
  if [[ -f "$STATE_DIR/intent.json" ]]; then
    cmp -s "$STATE_DIR/intent.json" "$STATE_DIR/intent.next.json" || die "provisioning arguments differ from $STATE_DIR/intent.json; this script does not migrate standalone VMs, resize, rotate keys, or upgrade existing clusters"
  else
    mv "$STATE_DIR/intent.next.json" "$STATE_DIR/intent.json"
  fi
  CONFIG_HASH=$(sha256 "$STATE_DIR/intent.json")
  TAGS=("$OWNER_TAG=$NAME" "$CONFIG_TAG=$CONFIG_HASH")
}

assert_owned() {
  local resource="$1" label="$2"
  jq -e --arg owner "$OWNER_TAG" --arg config "$CONFIG_TAG" --arg name "$NAME" --arg hash "$CONFIG_HASH" \
    '.tags[$owner] == $name and .tags[$config] == $hash' <<<"$resource" >/dev/null \
    || die "$label exists but is not owned by this deployment with matching configuration; refusing to modify it"
}

lookup_resource() {
  local type="$1" name="$2" resources result
  resources=$(az_cmd resource list --resource-group "$RESOURCE_GROUP" --resource-type "$type" -o json) \
    || die "cannot list $type resources; not treating a failed query as a missing resource"
  result=$(jq -c --arg name "$name" '[.[] | select(.name == $name)] | if length > 1 then error("ambiguous resource") else .[0] // null end' <<<"$resources")
  if [[ "$result" != null ]]; then assert_owned "$result" "$type/$name"; fi
  printf '%s\n' "$result"
}

ensure_features() {
  local extensions version feature state deadline role
  extensions=$(az extension list -o json)
  version=$(jq -r '[.[] | select(.name == "aks-preview")][0].version // "0.0.0"' <<<"$extensions")
  if ! version_at_least "$version" 22.0.0b8; then
    log "Installing aks-preview 22.0.0b8"
    az extension add --name aks-preview --allow-preview true --version 22.0.0b8 --upgrade --only-show-errors
  fi
  for feature in AKSFlexNodePreview PutMachinePreview; do
    state=$(az_cmd feature show --namespace Microsoft.ContainerService --name "$feature" --query properties.state -o tsv)
    if [[ "$state" != Registered ]]; then
      if [[ "$state" != Registering && "$state" != Pending ]]; then
        az_cmd feature register --namespace Microsoft.ContainerService --name "$feature" -o none
      fi
      log "Waiting for $feature registration"
      deadline=$((SECONDS + TIMEOUT))
      while [[ "$state" != Registered ]]; do
        ((SECONDS < deadline)) || die "timed out waiting for $feature; rerun after registration completes"
        sleep 15
        state=$(az_cmd feature show --namespace Microsoft.ContainerService --name "$feature" --query properties.state -o tsv)
      done
    fi
  done
  # Refresh even on reruns: a prior attempt may have stopped after feature
  # registration but before the provider learned about the newly enabled feature.
  az_cmd provider register --namespace Microsoft.ContainerService --wait -o none
  role=$(az_cmd role definition list --name "$FLEX_ROLE" -o json)
  jq -e --arg role "$FLEX_ROLE" 'any(.[]; (.name | ascii_downcase) == $role and .roleType == "BuiltInRole")' <<<"$role" >/dev/null \
    || die "Flex Node Agent Role ($FLEX_ROLE) is not published in this subscription; no Contributor/admin fallback is permitted"
}

ensure_group() {
  local groups group
  groups=$(az_cmd group list -o json)
  group=$(jq -c --arg name "$RESOURCE_GROUP" '[.[] | select(.name == $name)][0] // null' <<<"$groups")
  if [[ "$group" == null ]]; then
    az_cmd group create --name "$RESOURCE_GROUP" --location "$LOCATION" --tags "${TAGS[@]}" -o none
  else
    assert_owned "$group" "resource group $RESOURCE_GROUP"
    [[ "$(jq -r .location <<<"$group")" == "$LOCATION" ]] || die "resource group region differs from --location"
  fi
}

ensure_role() {
  local principal="$1" role="$2" scope="$3" assignments
  assignments=$(az_cmd role assignment list --scope "$scope" --fill-principal-name false --fill-role-definition-name false -o json)
  if ! jq -e --arg principal "$principal" --arg role "$role" --arg scope "$scope" \
    'any(.[]; .principalId == $principal and (.roleDefinitionId | split("/")[-1] | ascii_downcase) == $role and (.scope | ascii_downcase) == ($scope | ascii_downcase))' <<<"$assignments" >/dev/null; then
    az_cmd role assignment create --assignee-object-id "$principal" --assignee-principal-type ServicePrincipal --role "$role" --scope "$scope" -o none
  fi
}

ensure_subnet() {
  local name="$1" cidr="$2" nsg="$3" subnets subnet args=()
  subnets=$(az_cmd network vnet subnet list -g "$RESOURCE_GROUP" --vnet-name "$NAME-vnet" -o json)
  subnet=$(jq -c --arg name "$name" '[.[] | select(.name == $name)][0] // null' <<<"$subnets")
  if [[ "$subnet" == null ]]; then
    [[ -z "$nsg" ]] || args+=(--network-security-group "$nsg")
    az_cmd network vnet subnet create -g "$RESOURCE_GROUP" --vnet-name "$NAME-vnet" --name "$name" --address-prefixes "$cidr" ${args[@]+"${args[@]}"} -o none
  else
    jq -e --arg cidr "$cidr" --arg nsg "$nsg" \
      '(.addressPrefix // .addressPrefixes[0]) == $cidr and (.networkSecurityGroup.id // "") == $nsg' <<<"$subnet" >/dev/null \
      || die "subnet $name has conflicting addresses or security settings"
  fi
}

ensure_network() {
  local existing vnet nsg rules rule identity
  existing=$(lookup_resource Microsoft.Network/virtualNetworks "$NAME-vnet")
  if [[ "$existing" == null ]]; then
    az_cmd network vnet create -g "$RESOURCE_GROUP" --name "$NAME-vnet" --location "$LOCATION" \
      --address-prefixes "$VNET_CIDR" --tags "${TAGS[@]}" -o none
  fi
  vnet=$(az_cmd network vnet show -g "$RESOURCE_GROUP" --name "$NAME-vnet" -o json)
  jq -e --arg cidr "$VNET_CIDR" '.addressSpace.addressPrefixes == [$cidr]' <<<"$vnet" >/dev/null || die "VNet address space differs from the requested topology"
  VNET_ID=$(jq -r .id <<<"$vnet")
  existing=$(lookup_resource Microsoft.Network/networkSecurityGroups "$NAME-flex-nsg")
  if [[ "$existing" == null ]]; then
    az_cmd network nsg create -g "$RESOURCE_GROUP" --name "$NAME-flex-nsg" --location "$LOCATION" --tags "${TAGS[@]}" -o none
  fi
  nsg=$(az_cmd network nsg show -g "$RESOURCE_GROUP" --name "$NAME-flex-nsg" -o json)
  NSG_ID=$(jq -r .id <<<"$nsg")
  rules=$(jq '.securityRules // []' <<<"$nsg")
  jq -e 'all(.[]; .name == "AllowOperatorSSH")' <<<"$rules" >/dev/null || die "Flex NSG has unexpected custom rules; refusing to weaken or overwrite them"
  rule=$(jq -c '.[0] // null' <<<"$rules")
  if ! jq -e --arg source "$SSH_SOURCE_IP/32" \
    '.sourceAddressPrefix == $source and .destinationPortRange == "22" and .protocol == "Tcp" and .access == "Allow" and .direction == "Inbound" and .priority == 100 and .destinationAddressPrefix == "*" and .sourcePortRange == "*"' <<<"$rule" >/dev/null; then
    az_cmd network nsg rule create -g "$RESOURCE_GROUP" --nsg-name "$NAME-flex-nsg" --name AllowOperatorSSH \
      --priority 100 --direction Inbound --access Allow --protocol Tcp --source-address-prefixes "$SSH_SOURCE_IP/32" \
      --source-port-ranges '*' --destination-address-prefixes '*' --destination-port-ranges 22 -o none
  fi
  ensure_subnet system "$SYSTEM_NODE_CIDR" ""
  ensure_subnet flex "$FLEX_NODE_CIDR" "$NSG_ID"
  SYSTEM_SUBNET_ID="$VNET_ID/subnets/system"
  FLEX_SUBNET_ID="$VNET_ID/subnets/flex"
  existing=$(lookup_resource Microsoft.ManagedIdentity/userAssignedIdentities "$NAME-controlplane")
  if [[ "$existing" == null ]]; then
    az_cmd identity create -g "$RESOURCE_GROUP" --name "$NAME-controlplane" --location "$LOCATION" --tags "${TAGS[@]}" -o none
  fi
  identity=$(az_cmd identity show -g "$RESOURCE_GROUP" --name "$NAME-controlplane" -o json)
  CLUSTER_IDENTITY_ID=$(jq -r .id <<<"$identity")
  ensure_role "$(jq -r .principalId <<<"$identity")" "$NETWORK_ROLE" "$VNET_ID"
}

ensure_cluster() {
  local existing cluster state deadline args=()
  existing=$(lookup_resource Microsoft.ContainerService/managedClusters "$NAME")
  if [[ "$existing" == null ]]; then
    [[ -z "$KUBERNETES_VERSION" ]] || args+=(--kubernetes-version "$KUBERNETES_VERSION")
    log "Creating AKS cluster $NAME with one system node and no built-in CNI"
    az_cmd aks create -g "$RESOURCE_GROUP" --name "$NAME" --location "$LOCATION" \
      --nodepool-name system --node-count 1 --node-vm-size "$SYSTEM_VM_SIZE" --os-sku Ubuntu \
      --vm-set-type VirtualMachineScaleSets --network-plugin none --pod-cidr "$CLUSTER_POD_CIDR" \
      --service-cidr "$SERVICE_CIDR" --dns-service-ip "$DNS_SERVICE_IP" --vnet-subnet-id "$SYSTEM_SUBNET_ID" \
      --enable-managed-identity --assign-identity "$CLUSTER_IDENTITY_ID" --admin-username "$ADMIN_USER" \
      --ssh-key-value "$STATE_DIR/ssh.pub" --tags "${TAGS[@]}" ${args[@]+"${args[@]}"} -o none
  fi
  deadline=$((SECONDS + TIMEOUT))
  while true; do
    cluster=$(az_cmd aks show -g "$RESOURCE_GROUP" --name "$NAME" -o json)
    state=$(jq -r .provisioningState <<<"$cluster")
    case "$state" in
      Succeeded) break ;;
      Failed|Canceled) die "AKS provisioning is $state; inspect the Azure operation before retrying" ;;
      Creating|Updating|InProgress) ((SECONDS < deadline)) || die "timed out waiting for AKS"; sleep 15 ;;
      *) die "unexpected AKS provisioning state: $state" ;;
    esac
  done
  jq -e --arg location "$LOCATION" --arg subnet "$SYSTEM_SUBNET_ID" --arg size "$SYSTEM_VM_SIZE" \
    --arg service "$SERVICE_CIDR" --arg dns "$DNS_SERVICE_IP" --arg pods "$CLUSTER_POD_CIDR" \
    '.location == $location and (.networkProfile.networkPlugin | ascii_downcase) == "none"
     and .networkProfile.serviceCidr == $service and .networkProfile.dnsServiceIp == $dns
     and ((.networkProfile.podCidr // $pods) == $pods)
     and any(.agentPoolProfiles[]; .name == "system" and .mode == "System" and .count >= 1 and .vmSize == $size and .vnetSubnetId == $subnet)' <<<"$cluster" >/dev/null \
    || die "existing AKS cluster does not match the required network/system-pool configuration"
  AKS_ID=$(jq -r .id <<<"$cluster")
  POOL_ID="$AKS_ID/agentPools/$FLEX_POOL"
  CURRENT_KUBERNETES_VERSION=$(jq -r '.currentKubernetesVersion // .kubernetesVersion' <<<"$cluster")
  if [[ -n "$KUBERNETES_VERSION" && "$CURRENT_KUBERNETES_VERSION" != "$KUBERNETES_VERSION" ]]; then
    die "cluster version is $CURRENT_KUBERNETES_VERSION, not $KUBERNETES_VERSION; this script does not perform upgrades/downgrades"
  fi
  az_cmd aks get-credentials -g "$RESOURCE_GROUP" --name "$NAME" --admin --overwrite-existing --file "$STATE_DIR/kubeconfig" -o none
  chmod 0600 "$STATE_DIR/kubeconfig"
}

fetch_file() {
  local url="$1" file="$2"
  if [[ ! -f "$file" ]]; then
    curl -fsSL --connect-timeout 30 --max-time 600 --retry 3 --retry-delay 2 "$url" -o "$file.part"
    mv "$file.part" "$file"
  fi
}

release_digest() {
  local repository="$1" version="$2" asset="$3" checksums digest
  checksums="$STATE_DIR/cache/${repository##*/}-$version-checksums.txt"
  fetch_file "https://github.com/$repository/releases/download/$version/checksums.txt" "$checksums"
  digest=$(awk -v asset="$asset" '$2 == asset { print $1 }' "$checksums")
  [[ "$digest" =~ ^[0-9a-fA-F]{64}$ ]] || die "no unique SHA-256 for $asset in $repository/$version"
  printf '%s\n' "$digest"
}

install_plugin() {
  local os arch asset archive digest bin_dir
  if [[ -n "$PLUGIN_BINARY" ]]; then
    bin_dir="$STATE_DIR/bin/custom-$PLUGIN_DIGEST"
    mkdir -p "$bin_dir"
    if [[ "$PLUGIN_BINARY" != "$bin_dir/kubectl-unbounded" ]]; then
      cp "$PLUGIN_BINARY" "$bin_dir/kubectl-unbounded"
    fi
    [[ "$(sha256 "$bin_dir/kubectl-unbounded")" == "$PLUGIN_DIGEST" ]] || die "custom installer changed since the deployment intent was saved"
    chmod 0700 "$bin_dir/kubectl-unbounded"
    export PATH="$bin_dir:$PATH"
    return
  fi
  case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) die "unsupported workstation OS" ;; esac
  case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) die "unsupported workstation architecture" ;; esac
  mkdir -p "$STATE_DIR/cache"
  asset="kubectl-unbounded-$os-$arch.tar.gz"
  archive="$STATE_DIR/cache/$UNBOUNDED_VERSION-$asset"
  digest=$(release_digest Azure/unbounded "$UNBOUNDED_VERSION" "$asset")
  fetch_file "https://github.com/Azure/unbounded/releases/download/$UNBOUNDED_VERSION/$asset" "$archive"
  [[ "$(sha256 "$archive")" == "$digest" ]] || die "kubectl-unbounded archive checksum mismatch; remove the corrupt cached archive and rerun"
  bin_dir="$STATE_DIR/bin/$UNBOUNDED_VERSION"
  mkdir -p "$bin_dir"
  # Extract only the expected executable, never arbitrary archive paths.
  tar -xzf "$archive" -C "$bin_dir" kubectl-unbounded
  chmod 0700 "$bin_dir/kubectl-unbounded"
  export PATH="$bin_dir:$PATH"
}

wait_component_rollout() {
  local resource="$1" found deadline=$((SECONDS + TIMEOUT))
  # Site reconciliation is asynchronous. A successful Site apply does not
  # imply that the operator has created its component workloads yet.
  while true; do
    found=$(kube -n unbounded-system get "$resource" --ignore-not-found -o json) \
      || die "cannot query $resource while waiting for operator reconciliation"
    [[ -z "$found" ]] || break
    ((SECONDS < deadline)) || die "operator did not create $resource before the timeout"
    sleep 5
  done
  if jq -e '.kind == "DaemonSet" and .spec.updateStrategy.type == "OnDelete"' <<<"$found" >/dev/null; then
    log "Waiting for $resource current-generation pods to become available"
    while ! jq -e '
      (.status.desiredNumberScheduled // 0) as $desired |
      $desired > 0
      and (.status.observedGeneration // 0) >= .metadata.generation
      and .status.currentNumberScheduled == $desired
      and .status.updatedNumberScheduled == $desired
      and .status.numberReady == $desired
      and .status.numberAvailable == $desired
    ' <<<"$found" >/dev/null; do
      ((SECONDS < deadline)) || die "timed out waiting for $resource current-generation pods to become available"
      sleep 5
      found=$(kube -n unbounded-system get "$resource" -o json) \
        || die "cannot query $resource while waiting for readiness"
    done
    return
  fi
  kube -n unbounded-system rollout status "$resource" --timeout="${TIMEOUT}s"
}

setup_cni() {
  log "Installing Unbounded operator and private-L3 CNI topology"
  # Invoke the plugin explicitly so kubectl's plugin dispatch does not need to
  # interpret a --kubeconfig flag before the plugin name.
  local deployments install_args=()
  [[ -z "$IMAGE_REGISTRY" ]] || install_args+=(--image-registry "$IMAGE_REGISTRY")
  [[ -z "$OPERATOR_IMAGE" ]] || install_args+=(--operator-image "$OPERATOR_IMAGE")
  deployments=$(kube get deployments -A -o json)
  if ! jq -e 'any(.items[]; .metadata.name == "unbounded-operator" and .metadata.namespace == "unbounded-system")' <<<"$deployments" >/dev/null; then
    kubectl-unbounded install --kubeconfig "$STATE_DIR/kubeconfig" --timeout "${TIMEOUT}s" ${install_args[@]+"${install_args[@]}"}
  else
    # Retain an operator that has been upgraded since initial provisioning.
    # Replaying the pinned installer would otherwise downgrade its image.
    kube -n unbounded-system rollout status deployment/unbounded-operator --timeout="${TIMEOUT}s"
    kube wait --for=condition=Established crd/sites.unbounded-cloud.io --timeout="${TIMEOUT}s"
  fi
  jq -n --arg system "$SYSTEM_NODE_CIDR" --arg flex "$FLEX_NODE_CIDR" \
    --arg clusterPods "$CLUSTER_POD_CIDR" --arg flexPods "$FLEX_POD_CIDR" \
    --arg arch "$WORKER_ARCH" --arg pool "$FLEX_POOL" --argjson streaming "$ARTIFACT_STREAMING" \
    '{apiVersion:"v1",kind:"List",items:[
      {apiVersion:"unbounded-cloud.io/v1alpha3",kind:"Site",metadata:{name:"cluster"},
       spec:{nodeCidrs:[$system],manageCniPlugin:true,podCidrAssignments:[{assignmentEnabled:true,cidrBlocks:[$clusterPods]}],components:{machina:{enabled:true}}}},
      {apiVersion:"unbounded-cloud.io/v1alpha3",kind:"Site",metadata:{name:"flex-site"},
       spec:{nodeCidrs:[$flex],manageCniPlugin:true,podCidrAssignments:[{assignmentEnabled:true,cidrBlocks:[$flexPods]}]}},
      {apiVersion:"net.unbounded-cloud.io/v1alpha1",kind:"SitePeering",metadata:{name:"cluster-flex-private-l3"},
       spec:{sites:["cluster","flex-site"],meshNodes:true,tunnelProtocol:"Auto"}}
    ]} | if $streaming then
      (.items[] | select(.kind == "Site" and .metadata.name == "flex-site") | .spec.components.gantry) =
        {enabled:true,artifactStreaming:{enabled:true,nodeSelector:{"kubernetes.io/arch":$arch,"kubernetes.azure.com/agentpool":$pool}}}
      else . end' > "$STATE_DIR/sites.json"
  kube apply --server-side --field-manager=aks-flex-cluster -f "$STATE_DIR/sites.json"
  wait_component_rollout deployment/unbounded-net-controller
  wait_component_rollout daemonset/unbounded-net-node
  kube wait --for=condition=Ready nodes -l kubernetes.azure.com/agentpool=system --timeout="${TIMEOUT}s"
}

wait_cluster_operation() {
  local operations state deadline=$((SECONDS + TIMEOUT))
  while true; do
    operations=$(az_cmd aks operation show-latest -g "$RESOURCE_GROUP" --name "$NAME" -o json)
    state=$(jq -r .status <<<"$operations")
    case "$state" in
      Succeeded) return ;;
      InProgress|Running|Updating) ((SECONDS < deadline)) || die "timed out waiting for latest AKS operation"; sleep 15 ;;
      *) die "latest AKS operation is $state; refusing a concurrent Flex pool operation" ;;
    esac
  done
}

ensure_flex_pool() {
  local pools pool state deadline
  pools=$(az_cmd aks nodepool list -g "$RESOURCE_GROUP" --cluster-name "$NAME" -o json)
  pool=$(jq -c --arg name "$FLEX_POOL" '[.[] | select(.name == $name)][0] // null' <<<"$pools")
  if [[ "$pool" == null ]]; then
    wait_cluster_operation
    log "Creating logical FlexNodes pool $FLEX_POOL"
    jq -n --arg version "$CURRENT_KUBERNETES_VERSION" \
      '{properties:{type:"FlexNodes",mode:"User",orchestratorVersion:$version,maxPods:250}}' > "$STATE_DIR/flex-pool.json"
    az_cmd rest --method put --url "https://management.azure.com$POOL_ID?api-version=$POOL_API_VERSION" --body "@$STATE_DIR/flex-pool.json" -o none
  fi
  deadline=$((SECONDS + TIMEOUT))
  while true; do
    pool=$(az_cmd rest --method get --url "https://management.azure.com$POOL_ID?api-version=$POOL_API_VERSION" -o json)
    jq -e '.properties.type == "FlexNodes" and .properties.mode == "User"' <<<"$pool" >/dev/null || die "flexpool is not a User FlexNodes pool"
    state=$(jq -r .properties.provisioningState <<<"$pool")
    case "$state" in
      Succeeded) break ;;
      Creating|Updating|InProgress) ((SECONDS < deadline)) || die "timed out waiting for Flex pool"; sleep 15 ;;
      *) die "Flex pool provisioning state: $state" ;;
    esac
  done
  [[ "$(jq -r .properties.orchestratorVersion <<<"$pool")" == "$CURRENT_KUBERNETES_VERSION" ]] || die "Flex pool version differs from the control plane; resolve version skew before onboarding"
}

ensure_worker_network() {
  local existing pip lb rule
  existing=$(lookup_resource Microsoft.Network/publicIPAddresses "$NAME-flex-pip")
  if [[ "$existing" == null ]]; then
    az_cmd network public-ip create -g "$RESOURCE_GROUP" --name "$NAME-flex-pip" --location "$LOCATION" \
      --sku Standard --allocation-method Static --version IPv4 --tags "${TAGS[@]}" -o none
  fi
  pip=$(az_cmd network public-ip show -g "$RESOURCE_GROUP" --name "$NAME-flex-pip" -o json)
  jq -e '.sku.name == "Standard" and .publicIPAllocationMethod == "Static"' <<<"$pip" >/dev/null || die "worker public IP has conflicting settings"
  WORKER_PUBLIC_IP=$(jq -r .ipAddress <<<"$pip")
  existing=$(lookup_resource Microsoft.Network/loadBalancers "$NAME-flex-lb")
  if [[ "$existing" == null ]]; then
    az_cmd network lb create -g "$RESOURCE_GROUP" --name "$NAME-flex-lb" --location "$LOCATION" \
      --sku Standard --public-ip-address "$(jq -r .id <<<"$pip")" --frontend-ip-name workers \
      --backend-pool-name workers --tags "${TAGS[@]}" -o none
  fi
  lb=$(az_cmd network lb show -g "$RESOURCE_GROUP" --name "$NAME-flex-lb" -o json)
  jq -e --arg pip "$(jq -r .id <<<"$pip")" \
    '.sku.name == "Standard" and (.frontendIPConfigurations | length) == 1
     and .frontendIPConfigurations[0].name == "workers" and .frontendIPConfigurations[0].publicIPAddress.id == $pip
     and (.backendAddressPools | length) == 1 and .backendAddressPools[0].name == "workers"
     and all(.inboundNatRules[]?; .name == "ssh") and all(.outboundRules[]?; .name == "egress")
     and ((.inboundNatPools // []) | length) == 0 and ((.loadBalancingRules // []) | length) == 0' <<<"$lb" >/dev/null \
    || die "worker load balancer has conflicting frontends, pools, or rules"
  WORKER_LB_ID=$(jq -r .id <<<"$lb")
  WORKER_BACKEND_ID="$WORKER_LB_ID/backendAddressPools/workers"
  rule=$(jq -c '[.inboundNatRules[]? | select(.name == "ssh")][0] // null' <<<"$lb")
  if [[ "$rule" == null ]]; then
    az_cmd network lb inbound-nat-rule create -g "$RESOURCE_GROUP" --lb-name "$NAME-flex-lb" --name ssh \
      --protocol Tcp --frontend-ip-name workers --backend-pool-name "$WORKER_BACKEND_ID" \
      --frontend-port-range-start "$SSH_PORT_START" --frontend-port-range-end "$SSH_PORT_END" --backend-port 22 -o none
  else
    jq -e --arg backend "$WORKER_BACKEND_ID" --argjson start "$SSH_PORT_START" --argjson end "$SSH_PORT_END" \
      '.protocol == "Tcp" and .backendPort == 22 and .frontendPortRangeStart == $start
       and .frontendPortRangeEnd == $end and .backendAddressPool.id == $backend
       and (.frontendIPConfiguration.id | endswith("/frontendIPConfigurations/workers"))' <<<"$rule" >/dev/null \
      || die "worker SSH NAT rule conflicts with the reserved 1000-instance range"
  fi
  rule=$(jq -c '[.outboundRules[]? | select(.name == "egress")][0] // null' <<<"$lb")
  if [[ "$rule" == null ]]; then
    # Reserve a bounded SNAT budget so 1000 backends and the SSH NAT range fit on one IP.
    az_cmd network lb outbound-rule create -g "$RESOURCE_GROUP" --lb-name "$NAME-flex-lb" --name egress \
      --address-pool "$WORKER_BACKEND_ID" --frontend-ip-configs workers --outbound-ports 32 --protocol All -o none
  else
    jq -e --arg backend "$WORKER_BACKEND_ID" \
      '.protocol == "All" and .allocatedOutboundPorts == 32 and .backendAddressPool.id == $backend
       and (.frontendIPConfigurations | length) == 1
       and (.frontendIPConfigurations[0].id | endswith("/frontendIPConfigurations/workers"))' <<<"$rule" >/dev/null \
      || die "worker outbound rule has conflicting port budget or frontend"
  fi
}

ensure_vmss() {
  local existing model state principal deadline bootstrap_hash
  VMSS_ID="/subscriptions/$SUBSCRIPTION/resourceGroups/$RESOURCE_GROUP/providers/Microsoft.Compute/virtualMachineScaleSets/$VMSS_NAME"
  write_vmss_custom_data "$STATE_DIR/vmss-custom-data.sh"
  bootstrap_hash=$(sha256 "$STATE_DIR/vmss-custom-data.sh")
  existing=$(lookup_resource Microsoft.Compute/virtualMachineScaleSets "$VMSS_NAME")
  if [[ "$existing" == null ]]; then
    log "Creating Uniform Flex VMSS $VMSS_NAME with $FLEX_COUNT instances and one shared identity"
    jq -n --arg location "$LOCATION" --arg size "$VM_SIZE" --argjson count "$FLEX_COUNT" \
      --arg imageSKU "$WORKER_IMAGE_SKU" \
      --argjson diskSize "$WORKER_OS_DISK_GB" --arg diskSKU "$WORKER_OS_DISK_SKU" \
      --arg owner "$NAME" --arg hash "$CONFIG_HASH" --arg bootstrap "$bootstrap_hash" \
      --arg prefix "${NAME:0:9}" --arg user "$ADMIN_USER" --rawfile key "$STATE_DIR/ssh.pub" \
      --arg data "$(base64 < "$STATE_DIR/vmss-custom-data.sh" | tr -d '\n')" \
      --arg subnet "$FLEX_SUBNET_ID" --arg nsg "$NSG_ID" --arg backend "$WORKER_BACKEND_ID" \
      '{location:$location,tags:{"unbounded-flex-cluster":$owner,"unbounded-flex-config":$hash,"unbounded-flex-bootstrap":$bootstrap},
        sku:{name:$size,tier:"Standard",capacity:$count},identity:{type:"SystemAssigned"},
        properties:{orchestrationMode:"Uniform",singlePlacementGroup:false,overprovision:false,
          upgradePolicy:{mode:"Manual"},virtualMachineProfile:{
            securityProfile:{securityType:"Standard"},
            osProfile:{computerNamePrefix:$prefix,adminUsername:$user,customData:$data,
              linuxConfiguration:{disablePasswordAuthentication:true,
                ssh:{publicKeys:[{path:("/home/"+$user+"/.ssh/authorized_keys"),keyData:($key | rtrimstr("\n"))}]}}},
            storageProfile:{imageReference:{publisher:"Canonical",offer:"ubuntu-24_04-lts",sku:$imageSKU,version:"latest"},
              osDisk:{createOption:"FromImage",diskSizeGB:$diskSize,managedDisk:{storageAccountType:$diskSKU}}},
            networkProfile:{networkInterfaceConfigurations:[{name:"worker-nic",properties:{
              primary:true,enableIPForwarding:true,networkSecurityGroup:{id:$nsg},
              ipConfigurations:[{name:"worker-ip",properties:{primary:true,subnet:{id:$subnet},
                loadBalancerBackendAddressPools:[{id:$backend}]}}]}}]}}}}' > "$STATE_DIR/vmss.json"
    az_cmd rest --method put --url "https://management.azure.com$VMSS_ID?api-version=$VMSS_API_VERSION" \
      --headers 'If-None-Match=*' --body "@$STATE_DIR/vmss.json" -o none
  fi
  deadline=$((SECONDS + TIMEOUT))
  while true; do
    model=$(az_cmd rest --method get --url "https://management.azure.com$VMSS_ID?api-version=$VMSS_API_VERSION" -o json)
    assert_owned "$model" "worker VMSS"
    jq -e --arg size "$VM_SIZE" --arg subnet "$FLEX_SUBNET_ID" --arg backend "$WORKER_BACKEND_ID" \
      --arg imageSKU "$WORKER_IMAGE_SKU" \
      --argjson diskSize "$WORKER_OS_DISK_GB" --arg diskSKU "$WORKER_OS_DISK_SKU" \
      --arg nsg "$NSG_ID" --arg bootstrap "$bootstrap_hash" --arg user "$ADMIN_USER" --arg prefix "${NAME:0:9}" --rawfile key "$STATE_DIR/ssh.pub" \
      '.sku.name == $size and .sku.capacity >= 1 and .sku.capacity <= 1000
       and .identity.type == "SystemAssigned" and ((.identity.userAssignedIdentities // {}) | length) == 0
       and .tags["unbounded-flex-bootstrap"] == $bootstrap and .properties.orchestrationMode == "Uniform"
       and .properties.singlePlacementGroup == false and .properties.overprovision == false
       and .properties.upgradePolicy.mode == "Manual"
       and .properties.virtualMachineProfile.osProfile.adminUsername == $user
       and .properties.virtualMachineProfile.osProfile.computerNamePrefix == $prefix
       and .properties.virtualMachineProfile.osProfile.linuxConfiguration.disablePasswordAuthentication == true
       and (.properties.virtualMachineProfile.osProfile.linuxConfiguration.ssh.publicKeys[0].keyData | rtrimstr("\n")) == ($key | rtrimstr("\n"))
       and .properties.virtualMachineProfile.storageProfile.imageReference.publisher == "Canonical"
       and .properties.virtualMachineProfile.storageProfile.imageReference.offer == "ubuntu-24_04-lts"
       and .properties.virtualMachineProfile.storageProfile.imageReference.sku == $imageSKU
       and .properties.virtualMachineProfile.storageProfile.osDisk.diskSizeGB == $diskSize
       and .properties.virtualMachineProfile.storageProfile.osDisk.managedDisk.storageAccountType == $diskSKU
       and (.properties.virtualMachineProfile.securityProfile.securityType // "Standard") == "Standard"
       and (.properties.virtualMachineProfile.networkProfile.networkInterfaceConfigurations | length) == 1
       and .properties.virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].properties.enableIPForwarding == true
       and .properties.virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].properties.networkSecurityGroup.id == $nsg
       and (.properties.virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].properties.ipConfigurations | length) == 1
       and .properties.virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].properties.ipConfigurations[0].properties.subnet.id == $subnet
       and .properties.virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].properties.ipConfigurations[0].properties.loadBalancerBackendAddressPools == [{id:$backend}]
       and .properties.virtualMachineProfile.networkProfile.networkInterfaceConfigurations[0].properties.ipConfigurations[0].properties.publicIPAddressConfiguration == null' <<<"$model" >/dev/null \
      || die "worker VMSS has conflicting bootstrap, identity, hardware, or network configuration"
    principal=$(jq -r '.identity.principalId // empty' <<<"$model")
    if [[ -n "$principal" ]]; then ensure_role "$principal" "$FLEX_ROLE" "$POOL_ID"; fi
    state=$(jq -r .properties.provisioningState <<<"$model")
    case "$state" in
      Succeeded) [[ -n "$principal" ]] || die "worker VMSS identity has no principal"; break ;;
      Creating|Updating|InProgress) ((SECONDS < deadline)) || die "timed out waiting for worker VMSS"; sleep 15 ;;
      *) die "worker VMSS provisioning state: $state" ;;
    esac
  done
  FLEX_COUNT=$(jq -r .sku.capacity <<<"$model")
}

write_remote_script() {
  local file="$1"
  {
    printf '#!/usr/bin/env bash\nset -euo pipefail\numask 077\n'
    printf 'AKS_ID=%q\nPOOL=%q\nVMSS_ID=%q\nVMSS_NAME=%q\nPREFIX=%q\nSUBNET=%q\nVERSION=%q\nDIGEST=%q\nWAIT_TIMEOUT=%q\n' \
      "$AKS_ID" "$FLEX_POOL" "$VMSS_ID" "$VMSS_NAME" "$NAME" "$FLEX_NODE_CIDR" "$AGENT_VERSION" "$AGENT_DIGEST" "${BOOTSTRAP_TIMEOUT:-$TIMEOUT}"
    if [[ "$WORKER_ARCH" == arm64 ]]; then
      # shellcheck disable=SC2016
      printf '%s\n' '[[ "$(uname -m)" == aarch64 ]] || { echo "expected an ARM64 Flex host" >&2; exit 78; }'
    fi
    cat <<'REMOTE'
coordinates=$(python3 - "$VMSS_ID" "$VMSS_NAME" "$PREFIX" "$SUBNET" <<'PY'
import ipaddress
import json
import re
import sys
import urllib.request
try:
    request = urllib.request.Request('http://169.254.169.254/metadata/instance?api-version=2021-02-01', headers={'Metadata': 'true'})
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=30) as response:
        data = json.load(response)
    compute = data['compute']
    resource = compute['resourceId']
    instance = resource.rsplit('/', 1)[1]
    if compute['vmScaleSetName'].lower() != sys.argv[2].lower() or resource.lower() != (sys.argv[1] + '/virtualMachines/' + instance).lower() or re.fullmatch(r'[0-9]+', instance) is None:
        raise ValueError('metadata does not identify an instance of the expected VMSS')
    address = data['network']['interface'][0]['ipv4']['ipAddress'][0]['privateIpAddress']
    if ipaddress.IPv4Address(address) not in ipaddress.IPv4Network(sys.argv[4]):
        raise ValueError('instance address is outside the expected Flex subnet')
    print(sys.argv[3] + '-flex' + instance)
    print(address)
except (KeyError, IndexError, TypeError, ValueError, OSError) as error:
    print(f'cannot identify Flex VMSS instance: {error}', file=sys.stderr)
    sys.exit(1)
PY
) || exit 1
NODE=${coordinates%%$'\n'*}
NODE_IP=${coordinates#*$'\n'}
config=/etc/aks-flex-node/config.json
unit=/etc/systemd/system/aks-flex-node-agent.service
if [[ -f "$unit" ]]; then
  [[ -r "$config" ]] || { echo 'existing agent has no readable config; inspect the host' >&2; exit 78; }
  jq -e --arg cluster "$AKS_ID" --arg pool "$POOL" --arg node "$NODE" --arg ip "$NODE_IP" \
    '.azure.targetCluster.resourceId == $cluster and .azure.targetAgentPoolName == $pool
     and .agent.nodeName == $node and .node.kubelet.nodeIP == $ip' "$config" >/dev/null \
    || { echo 'existing agent belongs to another cluster/pool/node or has a changed IP; refusing reset' >&2; exit 78; }
  # Recover a stopped daemon without replacing its config or downgrading an
  # agent that has been upgraded since the host was first provisioned.
  if ! systemctl is-active --quiet aks-flex-node-agent.service; then
    systemctl start aks-flex-node-agent.service
  fi
  systemctl is-active --quiet aks-flex-node-agent.service
  echo "FLEX_STATUS=already-joined node=$NODE"
  exit 0
fi
# A failed bootstrap may have started a worker without installing the daemon.
# Replaying bootstrap against it is unsafe; leave it intact for explicit repair.
if [[ -e /etc/systemd/nspawn/kube1.nspawn || -e /etc/systemd/nspawn/kube2.nspawn ]]; then
  echo 'partial node deployment exists; inspect/reset this host explicitly before retrying' >&2
  exit 78
fi
cloud_init_status=0
cloud-init status --wait || cloud_init_status=$?
case "$cloud_init_status" in
  0) ;;
  2) echo 'cloud-init completed with recoverable warnings; continuing bootstrap' >&2 ;;
  *) exit "$cloud_init_status" ;;
esac
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates jq util-linux nftables systemd-container
REMOTE
    if [[ "$ARTIFACT_STREAMING" == true ]]; then
      cat <<'REMOTE'
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq kmod
printf 'nbd\n' > /etc/modules-load.d/gantry-nbd.conf
printf 'options nbd nbds_max=1024\n' > /etc/modprobe.d/gantry-nbd.conf
modprobe nbd nbds_max=1024
if [[ "$(cat /sys/module/nbd/parameters/nbds_max)" -lt 64 ]]; then
  echo 'Gantry streaming requires at least 64 NBD devices before Flex bootstrap' >&2
  exit 78
fi
REMOTE
    fi
    cat <<'REMOTE'
install -d -m 0700 /run/aks-flex-node-bootstrap
# Role assignment presence in ARM does not mean that IMDS and the RP have
# accepted it yet. Wait for an authorized Machine GET before changing the host
# runtime. A 404 is expected for a machine that has never joined this pool.
deadline=$((SECONDS + WAIT_TIMEOUT))
while true; do
  token_response=$(curl -fsS --noproxy '*' --connect-timeout 10 --max-time 30 -H Metadata:true \
    'http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%3A%2F%2Fmanagement.azure.com%2F' 2>/dev/null || true)
  token=$(jq -r '.access_token // empty' <<<"$token_response" 2>/dev/null || true)
  unset token_response
  status=000
  if [[ -n "$token" ]]; then
    status=$(printf 'header = "Authorization: Bearer %s"\n' "$token" | \
      curl --config - -sS --connect-timeout 10 --max-time 30 -o /dev/null --write-out '%{http_code}' \
        "https://management.azure.com$AKS_ID/agentPools/$POOL/machines/$NODE?api-version=2026-05-02-preview") || status=000
  fi
  unset token
  case "$status" in
    200|404) break ;;
    000|401|403|408|429|5??) ;;
    *) echo "unexpected ARM Machine access status: $status" >&2; exit 1 ;;
  esac
  ((SECONDS < deadline)) || { echo 'managed identity/pool role is not usable yet; rerun after propagation' >&2; exit 1; }
  sleep 15
done
base64 -d > /run/aks-flex-node-bootstrap/bootstrap.sh <<'BOOTSTRAP_BASE64'
REMOTE
    base64 < "$STATE_DIR/cache/bootstrap-$AGENT_VERSION.sh"
    cat <<'REMOTE'
BOOTSTRAP_BASE64
chmod 0700 /run/aks-flex-node-bootstrap/bootstrap.sh
bash -n /run/aks-flex-node-bootstrap/bootstrap.sh
overrides=$(jq -cn --arg name "$NODE" --arg ip "$NODE_IP" \
  '{agent:{nodeName:$name,machineClient:{mode:"arm"},requireMachineRegistration:true},node:{kubelet:{nodeIP:$ip}}}')
bash /run/aks-flex-node-bootstrap/bootstrap.sh \
  --auth msi --fetch-bootstrap-data --cluster-resource-id "$AKS_ID" \
  --agent-pool-name "$POOL" --agent-version "$VERSION" --agent-sha256 "$DIGEST" \
  --config-overrides "$overrides"
systemctl is-active --quiet aks-flex-node-agent.service
rm -f /run/aks-flex-node-bootstrap/bootstrap.sh
echo "FLEX_STATUS=joined node=$NODE"
REMOTE
  } > "$file"
  bash -n "$file"
}

write_vmss_custom_data() {
  local file="$1" bootstrap="$STATE_DIR/vmss-bootstrap.sh"
  write_remote_script "$bootstrap"
  {
    cat <<'CUSTOM_DATA'
#!/bin/bash
set -euo pipefail
umask 077
install -d -m 0700 /etc/aks-flex-vmss-bootstrap
base64 -d > /etc/aks-flex-vmss-bootstrap/bootstrap.sh <<'VMSS_BOOTSTRAP_BASE64'
CUSTOM_DATA
    base64 < "$bootstrap"
    cat <<'CUSTOM_DATA'
VMSS_BOOTSTRAP_BASE64
chmod 0700 /etc/aks-flex-vmss-bootstrap/bootstrap.sh
cat > /etc/systemd/system/aks-flex-vmss-bootstrap.service <<'BOOTSTRAP_UNIT'
[Unit]
Description=Automatically join this VMSS instance to its AKS Flex pool
After=cloud-final.service network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=oneshot
RemainAfterExit=yes
TimeoutStartSec=0
Restart=on-failure
RestartSec=30
RestartPreventExitStatus=78
CUSTOM_DATA
    printf 'ExecStart=/usr/bin/timeout --signal=TERM --kill-after=30s %s /bin/bash /etc/aks-flex-vmss-bootstrap/bootstrap.sh\n' "${BOOTSTRAP_TIMEOUT:-$TIMEOUT}"
    cat <<'CUSTOM_DATA'

[Install]
WantedBy=multi-user.target
BOOTSTRAP_UNIT
systemctl daemon-reload
# Do not wait inside cloud-final for a service ordered after cloud-final.
systemctl enable --now --no-block aks-flex-vmss-bootstrap.service
CUSTOM_DATA
  } > "$file"
  bash -n "$file"
  (($(wc -c < "$file") <= 65535)) || die "VMSS custom data exceeds the Azure 64 KiB limit"
}

write_ssh_access() {
  local address mapping port
  az_cmd vmss nic list -g "$RESOURCE_GROUP" --vmss-name "$VMSS_NAME" -o json > "$STATE_DIR/vmss-nics.json"
  jq -er --argjson count "$FLEX_COUNT" \
    'if type == "array" and length == $count and all(.[]; .ipConfigurations[0].privateIPAddress | type == "string")
     then .[0:20][].ipConfigurations[0].privateIPAddress else error("VMSS NIC list does not match its capacity") end' \
    "$STATE_DIR/vmss-nics.json" > "$STATE_DIR/ssh-private-ips.txt"
  : > "$STATE_DIR/ssh-access.txt"
  while IFS= read -r address; do
    mapping=$(az_cmd rest --method post \
      --url "https://management.azure.com$WORKER_BACKEND_ID/queryInboundNatRulePortMapping?api-version=$NETWORK_API_VERSION" \
      --body "$(jq -cn --arg address "$address" '{ipAddress:$address}')" -o json)
    port=$(jq -er --argjson start "$SSH_PORT_START" --argjson end "$SSH_PORT_END" \
      '[.inboundNatRulePortMappings[] | select(.inboundNatRuleName == "ssh" and .protocol == "Tcp" and .backendPort == 22
        and .frontendPort >= $start and .frontendPort <= $end)] | if length == 1 then .[0].frontendPort else error("missing or ambiguous SSH NAT mapping") end' <<<"$mapping") \
      || die "SSH mapping for $address is not ready; inspect the worker load balancer"
    printf 'ssh -p %q -i %q -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%q %q\n' \
      "$port" "$SSH_KEY" "$STATE_DIR/known_hosts" "$ADMIN_USER@$WORKER_PUBLIC_IP" >> "$STATE_DIR/ssh-access.txt"
  done < "$STATE_DIR/ssh-private-ips.txt"
  printf '# At large capacities, commands above sample up to 20 instances. Query other private-IP mappings at:\n# %s\n' \
    "https://management.azure.com$WORKER_BACKEND_ID/queryInboundNatRulePortMapping?api-version=$NETWORK_API_VERSION" >> "$STATE_DIR/ssh-access.txt"
}

validate_deployment() {
  local page url deadline=$((SECONDS + TIMEOUT))
  log "Waiting for automatic VMSS bootstrap, ARM registration, and Flex node readiness"
  while true; do
    az_cmd vmss list-instances -g "$RESOURCE_GROUP" --name "$VMSS_NAME" -o json > "$STATE_DIR/vmss-instances.json"
    jq -e 'type == "array" and all(.[]; .instanceId | test("^[0-9]+$"))' "$STATE_DIR/vmss-instances.json" >/dev/null \
      || die "VMSS returned invalid instance identifiers"
    [[ "$(jq length "$STATE_DIR/vmss-instances.json")" == "$FLEX_COUNT" ]] && break
    ((SECONDS < deadline)) || die "VMSS instance list does not match its capacity before the timeout"
    sleep 5
  done
  jq -ce --arg prefix "$NAME-flex" --argjson count "$FLEX_COUNT" \
    'if length == $count and all(.[]; (.instanceId | test("^[0-9]+$"))) then
       [.[] | $prefix + .instanceId] | unique | if length == $count then . else error("duplicate instance IDs") end
     else error("VMSS instance list does not match its capacity") end' "$STATE_DIR/vmss-instances.json" > "$STATE_DIR/expected-worker-nodes.json"
  while true; do
    : > "$STATE_DIR/worker-machines.jsonl"
    url="https://management.azure.com$POOL_ID/machines?api-version=$POOL_API_VERSION"
    while [[ -n "$url" ]]; do
      [[ "$url" == "https://management.azure.com$POOL_ID/machines?"* ]] || die "ARM Machine pagination left the expected pool"
      page=$(az_cmd rest --method get --url "$url" -o json)
      jq -c '.value[] | {name: (.name | ascii_downcase), state: .properties.provisioningState}' <<<"$page" >> "$STATE_DIR/worker-machines.jsonl"
      url=$(jq -r '.nextLink // empty' <<<"$page")
    done
    kube get nodes -l "kubernetes.azure.com/agentpool=$FLEX_POOL" -o json > "$STATE_DIR/worker-nodes.json"
    if jq -en --slurpfile expected "$STATE_DIR/expected-worker-nodes.json" \
      --slurpfile machines "$STATE_DIR/worker-machines.jsonl" --slurpfile nodes "$STATE_DIR/worker-nodes.json" --arg pool "$FLEX_POOL" --arg arch "$WORKER_ARCH" \
      '($machines | map({key: .name, value: .state}) | from_entries) as $states |
       ($nodes[0].items | map({key: .metadata.name, value:
         (.metadata.labels["kubernetes.azure.com/agentpool"] == $pool
          and .metadata.labels["kubernetes.io/arch"] == $arch
          and .status.nodeInfo.architecture == $arch
          and .metadata.labels["unbounded-cloud.io/site"] == "flex-site"
          and any(.status.conditions[]?; .type == "Ready" and .status == "True"))}) | from_entries) as $ready |
       all($expected[0][]; $states[.] == "Succeeded" and $ready[.] == true)' >/dev/null; then
      break
    fi
    ((SECONDS < deadline)) || die "VMSS workers are not Ready, ARM-registered, or assigned to the expected pool and network site; inspect cloud-init and aks-flex-vmss-bootstrap.service logs"
    sleep 15
  done
  wait_component_rollout daemonset/unbounded-net-node
  wait_component_rollout daemonset/unbounded-net-kube-proxy-flex-site
  if [[ "$ARTIFACT_STREAMING" == true ]]; then
    wait_component_rollout daemonset/gantry
    wait_component_rollout daemonset/gantry-overlaybd-config
  fi
}

smoke_test() {
  local namespace=aks-flex-smoke namespaces existing pods pod targets=()
  log "Testing cross-node pods, Services, DNS, and API-server-to-kubelet exec/logs"
  namespaces=$(kube get namespaces -o json)
  existing=$(jq -c --arg ns "$namespace" '[.items[] | select(.metadata.name == $ns)][0] // null' <<<"$namespaces")
  if [[ "$existing" != null ]]; then
    jq -e --arg name "$NAME" '.metadata.labels["unbounded-flex-cluster"] == $name' <<<"$existing" >/dev/null || die "smoke namespace already exists and is not owned by this deployment"
  fi
  jq -n --arg ns "$namespace" --arg name "$NAME" --arg pool "$FLEX_POOL" \
    '{apiVersion:"v1",kind:"List",items:[
      {apiVersion:"v1",kind:"Namespace",metadata:{name:$ns,labels:{"unbounded-flex-cluster":$name}}},
      {apiVersion:"apps/v1",kind:"DaemonSet",metadata:{name:"echo",namespace:$ns},spec:{selector:{matchLabels:{app:"flex-smoke"}},
       template:{metadata:{labels:{app:"flex-smoke"}},spec:{nodeSelector:{"kubernetes.azure.com/agentpool":$pool},
        containers:[{name:"echo",image:"docker.io/library/busybox:1.37.0",command:["sh","-c","mkdir -p /www; hostname > /www/index.html; exec httpd -f -p 8080 -h /www"],
         ports:[{containerPort:8080}],resources:{requests:{cpu:"10m",memory:"16Mi"},limits:{memory:"64Mi"}},
         readinessProbe:{httpGet:{path:"/",port:8080}}}]}}}},
      {apiVersion:"v1",kind:"Service",metadata:{name:"echo",namespace:$ns},spec:{selector:{app:"flex-smoke"},ports:[{port:8080,targetPort:8080}]}}
    ]}' > "$STATE_DIR/smoke.json"
  kube apply --server-side --field-manager=aks-flex-cluster -f "$STATE_DIR/smoke.json"
  kube -n "$namespace" rollout status daemonset/echo --timeout="${TIMEOUT}s"
  pods=$(kube -n "$namespace" get pods -l app=flex-smoke -o json)
  [[ "$(jq '.items | length' <<<"$pods")" == "$FLEX_COUNT" ]] || die "smoke DaemonSet does not have exactly $FLEX_COUNT pods"
  # Bound cross-node sampling rather than issuing a quadratic all-pairs test at 1000 nodes.
  pods=$(jq '{items: (.items | sort_by(.metadata.name) | [.[0], .[length / 2 | floor], .[-1]] | unique_by(.metadata.name))}' <<<"$pods")
  while IFS= read -r pod; do targets+=("$pod"); done < <(jq -r '.items[].status.podIP' <<<"$pods")
  targets+=("echo.$namespace.svc.cluster.local")
  while IFS= read -r pod; do
    # Expansion belongs to the container shell, not the workstation shell.
    # shellcheck disable=SC2016
    kube -n "$namespace" exec "$pod" -- sh -c 'set -e; for target in "$@"; do wget -q -T 15 -O - "http://$target:8080/"; done' sh "${targets[@]}"
    kube -n "$namespace" logs "$pod" --tail=1 >/dev/null
  done < <(jq -r '.items[].metadata.name' <<<"$pods")
}

main() {
  parse_args "$@"
  command -v python3 >/dev/null 2>&1 || die "python3 is required"
  command -v jq >/dev/null 2>&1 || die "jq is required"
  validate_args
  if [[ "$PLAN_ONLY" == true ]]; then print_plan; return; fi
  preflight
  print_plan
  if [[ "$ASSUME_YES" != true ]]; then
    local confirmation
    read -r -p 'Create/reconcile these resources? [y/N] ' confirmation
    [[ "$confirmation" == y || "$confirmation" == Y ]] || die "deployment canceled"
  fi
  prepare_state
  AGENT_DIGEST=$(release_digest Azure/AKSFlexNode "$AGENT_VERSION" "aks-flex-node-linux-$WORKER_ARCH.tar.gz")
  fetch_file "https://raw.githubusercontent.com/Azure/AKSFlexNode/$AGENT_VERSION/scripts/bootstrap.sh" "$STATE_DIR/cache/bootstrap-$AGENT_VERSION.sh"
  bash -n "$STATE_DIR/cache/bootstrap-$AGENT_VERSION.sh"
  ensure_features
  ensure_group
  ensure_network
  ensure_cluster
  install_plugin
  setup_cni
  ensure_flex_pool
  ensure_worker_network
  ensure_vmss
  validate_deployment
  smoke_test
  write_ssh_access
  log "Deployment ready: one system node and $FLEX_COUNT Flex nodes"
  kube get nodes -L kubernetes.azure.com/agentpool,unbounded-cloud.io/site -o wide
  printf '\nKubeconfig: %s\nSSH ingress: %s/32\nSSH access:\n' "$STATE_DIR/kubeconfig" "$SSH_SOURCE_IP"
  while IFS= read -r line; do printf '  %s\n' "$line"; done < "$STATE_DIR/ssh-access.txt"
  printf '\nScale workers: az vmss scale --resource-group %q --name %q --new-capacity N --subscription %q\n' \
    "$RESOURCE_GROUP" "$VMSS_NAME" "$SUBSCRIPTION"
  printf 'New instances join automatically; an existing managed ACR policy applies without reattachment.\n'
  printf 'Rerun with the same initial arguments and state directory. Existing VMSS capacity is retained.\n'
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
