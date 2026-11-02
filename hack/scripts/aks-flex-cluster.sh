#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Self-contained, repeatable AKS + Unbounded-Net + AKS Flex Node deployment.
# All Azure resources are created through az; no other checkout, templates,
# local Go build, or preinstalled kubectl-unbounded is required.
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
NAME=""
LOCATION=""
SUBSCRIPTION=""
RESOURCE_GROUP=""
FLEX_COUNT=3
VM_SIZE="Standard_D4s_v6"
SYSTEM_VM_SIZE="Standard_D4s_v6"
KUBERNETES_VERSION=""
UNBOUNDED_VERSION="v0.8.0"
AGENT_VERSION="v0.2.0"
VNET_CIDR="10.91.0.0/16"
CLUSTER_POD_CIDR="10.93.0.0/16"
FLEX_POD_CIDR="10.95.0.0/16"
SERVICE_CIDR="10.94.0.0/16"
SSH_KEY=""
SSH_SOURCE_IP=""
STATE_DIR=""
TIMEOUT=3600
ASSUME_YES=false
PLAN_ONLY=false
BOOTSTRAP_TRANSPORT="run-command"
ACR=""
ACR_PULL_IDENTITY=""
ACR_PROBE_IMAGE=""
ACR_PULL_ROLE="7f951dda-4ed3-4680-a7ca-43fe172d538d"
REGISTRY_SUBJECT="system:serviceaccount:unbounded-system:registry-pull"
REGISTRY_AUDIENCE="api://AzureADTokenExchange"
LOCK_HELD=false
ADMIN_USER="azureuser"
FLEX_POOL="flexpool"
OWNER_TAG="unbounded-flex-cluster"
CONFIG_TAG="unbounded-flex-config"
FLEX_ROLE="8f139b0f-7eaf-460b-a9da-5b1246d9ed0d"
NETWORK_ROLE="4d97b98b-1d4f-4787-a291-c67834d212e7"
POOL_API_VERSION="2026-05-02-preview"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

usage() {
  printf '%s\n' 'Usage: bash hack/scripts/aks-flex-cluster.sh --name NAME --location REGION [options]'
  printf '%s\n' '
Creates one AKS system node and three standalone Ubuntu 24.04 Flex VMs by
default. Reuses matching resources and already-joined hosts on subsequent runs.
Requires az >= 2.90.0, kubectl, jq, python3, curl, tar, openssl, and OpenSSH.
Flex VM sizes must support x86_64; the host image and agent archive use amd64.
Uses the active Azure account unless --subscription is supplied. The selected
subscription must support the Flex preview and publish the Flex Node Agent Role.

Options:
  --name NAME                 Required, lowercase resource prefix (3-30 characters)
  --location REGION           Required Azure region
  --subscription UUID         Azure subscription (default: active account)
  --resource-group NAME       Dedicated resource group (default: NAME)
  --flex-count N              Flex VM count (default: 3, range: 1-20)
  --vm-size SKU               Flex VM size (default: Standard_D4s_v6)
  --system-vm-size SKU        AKS system VM size (default: Standard_D4s_v6)
  --kubernetes-version V      Initial AKS version (default: AKS default)
  --unbounded-version TAG     Pinned plugin/operator release (default: v0.8.0)
  --agent-version TAG         Pinned Flex agent release (default: v0.2.0)
  --vnet-cidr CIDR            IPv4 VNet, /16 through /23 (default: 10.91.0.0/16)
  --cluster-pod-cidr CIDR     System-site pods (default: 10.93.0.0/16)
  --flex-pod-cidr CIDR        Flex-site pods (default: 10.95.0.0/16)
  --service-cidr CIDR         Services (default: 10.94.0.0/16)
  --ssh-key PATH              Existing private key (default: generate in state dir)
  --ssh-source-ip IPv4        SSH source (default: detected public IPv4); always /32
  --bootstrap-transport MODE  run-command (default, Azure VM agent) or ssh
  --acr NAME                  Existing shared ACR to onboard (never created/deleted)
  --acr-pull-identity ID       Optional existing user-assigned identity resource ID
                              (default: one owned NAME-acr-pull identity per cluster)
  --acr-probe-image IMAGE     Optional existing private image for an authorization probe
  --state-dir PATH            State/keys/cache (default: repo tmp/aks-flex/NAME)
  --timeout SECONDS           Per-phase wait timeout (default: 3600)
  --plan                      Print the plan without network calls or file writes
  --yes                       Skip confirmation
  -h, --help                  Show help

Keep the state directory and use the same provisioning arguments on reruns.
Changing SSH source IP is supported; topology, keys, sizes, and release changes
are rejected rather than silently replacing or downgrading existing resources.
This is not a scaling, upgrade, reset, or teardown tool. It never deletes Azure
resources or resets an existing node. A partially bootstrapped host that needs a
reset is reported, not automatically destroyed. See designs/aks-flex-cluster.md.'
}

parse_args() {
  while (($#)); do
    case "$1" in
      --plan) PLAN_ONLY=true; shift ;;
      --yes) ASSUME_YES=true; shift ;;
      -h|--help) usage; exit 0 ;;
      --name|--location|--subscription|--resource-group|--flex-count|--vm-size|--system-vm-size|--kubernetes-version|--unbounded-version|--agent-version|--vnet-cidr|--cluster-pod-cidr|--flex-pod-cidr|--service-cidr|--ssh-key|--ssh-source-ip|--bootstrap-transport|--state-dir|--timeout|--acr|--acr-pull-identity|--acr-probe-image)
        [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || die "$1 requires a value"
        case "$1" in
          --name) NAME="$2" ;;
          --location) LOCATION="$2" ;;
          --subscription) SUBSCRIPTION="$2" ;;
          --resource-group) RESOURCE_GROUP="$2" ;;
          --flex-count) FLEX_COUNT="$2" ;;
          --vm-size) VM_SIZE="$2" ;;
          --system-vm-size) SYSTEM_VM_SIZE="$2" ;;
          --kubernetes-version) KUBERNETES_VERSION="$2" ;;
          --unbounded-version) UNBOUNDED_VERSION="$2" ;;
          --agent-version) AGENT_VERSION="$2" ;;
          --vnet-cidr) VNET_CIDR="$2" ;;
          --cluster-pod-cidr) CLUSTER_POD_CIDR="$2" ;;
          --flex-pod-cidr) FLEX_POD_CIDR="$2" ;;
          --service-cidr) SERVICE_CIDR="$2" ;;
          --ssh-key) SSH_KEY="$2" ;;
          --ssh-source-ip) SSH_SOURCE_IP="$2" ;;
          --bootstrap-transport) BOOTSTRAP_TRANSPORT="$2" ;;
          --state-dir) STATE_DIR="$2" ;;
          --timeout) TIMEOUT="$2" ;;
          --acr) ACR="$2" ;;
          --acr-pull-identity) ACR_PULL_IDENTITY="$2" ;;
          --acr-probe-image) ACR_PROBE_IMAGE="$2" ;;
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
  [[ "$FLEX_COUNT" =~ ^[1-9][0-9]?$ ]] || die "--flex-count must be 1-20"
  ((FLEX_COUNT <= 20)) || die "--flex-count must be 1-20"
  [[ "$TIMEOUT" =~ ^[1-9][0-9]{0,5}$ ]] || die "--timeout must be a positive integer of at most six digits"
  [[ "$BOOTSTRAP_TRANSPORT" == ssh || "$BOOTSTRAP_TRANSPORT" == run-command ]] || die "--bootstrap-transport must be ssh or run-command"
  [[ "$VM_SIZE" =~ ^Standard_[a-zA-Z0-9_]+$ && "$SYSTEM_VM_SIZE" =~ ^Standard_[a-zA-Z0-9_]+$ ]] || die "invalid VM size"
  [[ -z "$KUBERNETES_VERSION" || "$KUBERNETES_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--kubernetes-version must be a full x.y.z version"
  local tag
  for tag in "$UNBOUNDED_VERSION" "$AGENT_VERSION"; do
    [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][a-zA-Z0-9.-]+)?$ ]] || die "release versions must be pinned vX.Y.Z tags (optionally prerelease)"
  done
  validate_registry_args
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
    if not 16 <= vnet.prefixlen <= 23:
        raise ValueError("VNet must have a /16 through /23 prefix")
    if cluster.prefixlen > 24 or flex.prefixlen > 24:
        raise ValueError("pod networks must have /24 or larger address spaces")
    if services.num_addresses <= 16:
        raise ValueError("service network must have more than 16 addresses")
    for index, network in enumerate(networks):
        if any(network.overlaps(other) for other in networks[index + 1:]):
            raise ValueError("VNet, pod, and service CIDRs must not overlap")
    subnets = vnet.subnets(new_prefix=24)
    print(json.dumps({"system": str(next(subnets)), "flex": str(next(subnets)),
                      "dns": str(services.network_address + 10)}))
except ValueError as error:
    print(f"invalid network configuration: {error}", file=sys.stderr)
    sys.exit(1)
PY
  ) || die "invalid CIDRs"
  SYSTEM_NODE_CIDR=$(jq -r .system <<<"$network")
  FLEX_NODE_CIDR=$(jq -r .flex <<<"$network")
  DNS_SERVICE_IP=$(jq -r .dns <<<"$network")
}

validate_registry_args() {
  [[ -z "$ACR" || "$ACR" =~ ^[a-z0-9]{5,50}$ ]] || die "--acr must be an Azure registry name, not a URL"
  [[ -n "$ACR" || ( -z "$ACR_PULL_IDENTITY" && -z "$ACR_PROBE_IMAGE" ) ]] || die "--acr-pull-identity and --acr-probe-image require --acr"
  if [[ -n "$ACR_PULL_IDENTITY" ]]; then
    [[ "$ACR_PULL_IDENTITY" =~ ^/subscriptions/[0-9a-fA-F-]{36}/resource[Gg]roups/[^/]+/providers/Microsoft.ManagedIdentity/userAssignedIdentities/[a-zA-Z0-9_-]+$ ]] || die "--acr-pull-identity must be a full user-assigned identity resource ID"
  fi
  [[ -z "$ACR_PROBE_IMAGE" || "$ACR_PROBE_IMAGE" =~ ^[a-z0-9][a-zA-Z0-9./:_@-]+$ ]] || die "invalid --acr-probe-image"
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
  printf '  Initial Kubernetes:       %s\n' "${KUBERNETES_VERSION:-AKS default}"
  printf '  Operator / Flex agent:    %s / %s\n' "$UNBOUNDED_VERSION" "$AGENT_VERSION"
  printf '  Node subnets:             %s / %s\n' "$SYSTEM_NODE_CIDR" "$FLEX_NODE_CIDR"
  printf '  Pod CIDRs:                %s / %s\n' "$CLUSTER_POD_CIDR" "$FLEX_POD_CIDR"
  printf '  Services / DNS:           %s / %s\n' "$SERVICE_CIDR" "$DNS_SERVICE_IP"
  if [[ -n "$SSH_SOURCE_IP" ]]; then
    printf '  SSH ingress:              %s/32\n' "$SSH_SOURCE_IP"
  else
    printf '  SSH ingress:              public IPv4 detected before provisioning\n'
  fi
  printf '  Bootstrap transport:      %s\n' "$BOOTSTRAP_TRANSPORT"
  if [[ -n "$ACR" ]]; then
    printf '  Shared ACR:               %s (existing; pull access only)\n' "$ACR"
    printf '  Federated pull identity:  %s\n' "${ACR_PULL_IDENTITY:-$NAME-acr-pull (one identity per cluster)}"
    printf '  Infrastructure subject:   %s\n' "$REGISTRY_SUBJECT"
    printf '  Private-image probe:      %s\n' "${ACR_PROBE_IMAGE:-not configured; repository permission remains unverified}"
  fi
  printf '  State:                    %s\n\n' "${STATE_DIR:-$SCRIPT_ROOT/tmp/aks-flex/$NAME}"
}

az_cmd() { az "$@" --subscription "$SUBSCRIPTION" --only-show-errors; }
kube() { kubectl --kubeconfig "$STATE_DIR/kubeconfig" "$@"; }
sha256() { openssl dgst -sha256 "$1" | awk '{print $NF}'; }

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
  cli_version=$(az version --query '"azure-cli"' -o tsv) || die "cannot read Azure CLI version"
  version_at_least "$cli_version" 2.90.0 || die "Azure CLI >= 2.90.0 required; found $cli_version"
  if [[ -z "$SUBSCRIPTION" ]]; then
    SUBSCRIPTION=$(az account show --query id -o tsv) || die "log in with az login first"
  fi
  validate_subscription
  az_cmd account show -o none || die "cannot access the selected subscription"
  if [[ -z "$SSH_SOURCE_IP" ]]; then
    SSH_SOURCE_IP=$(curl -4 -fsS --connect-timeout 10 --max-time 30 https://api.ipify.org) || die "cannot detect public IPv4; pass --ssh-source-ip"
  fi
  validate_ssh_ip
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
  jq -n --arg subscription "$SUBSCRIPTION" --arg name "$NAME" --arg rg "$RESOURCE_GROUP" \
    --arg location "$LOCATION" --argjson count "$FLEX_COUNT" --arg vm "$VM_SIZE" \
    --arg systemVM "$SYSTEM_VM_SIZE" --arg k8s "$KUBERNETES_VERSION" \
    --arg unbounded "$UNBOUNDED_VERSION" --arg agent "$AGENT_VERSION" \
    --arg vnet "$VNET_CIDR" --arg clusterPods "$CLUSTER_POD_CIDR" \
    --arg flexPods "$FLEX_POD_CIDR" --arg services "$SERVICE_CIDR" \
    --rawfile sshKey "$STATE_DIR/ssh.pub" \
    '{subscription:$subscription,name:$name,resourceGroup:$rg,location:$location,
      flexCount:$count,vmSize:$vm,systemVMSize:$systemVM,kubernetesVersion:$k8s,
      unboundedVersion:$unbounded,agentVersion:$agent,vnetCIDR:$vnet,
      clusterPodCIDR:$clusterPods,flexPodCIDR:$flexPods,serviceCIDR:$services,sshKey:$sshKey}' \
    > "$STATE_DIR/intent.next.json"
  if [[ -f "$STATE_DIR/intent.json" ]]; then
    cmp -s "$STATE_DIR/intent.json" "$STATE_DIR/intent.next.json" || die "provisioning arguments differ from $STATE_DIR/intent.json; this script does not scale, resize, rotate keys, or upgrade existing clusters"
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

prepare_registry_intent() {
  local saved="$STATE_DIR/registry-intent.json" next="$STATE_DIR/registry-intent.next.json"
  if [[ -f "$saved" && -z "$ACR" ]]; then
    ACR=$(jq -er .registryName "$saved")
    ACR_PULL_IDENTITY=$(jq -r '.identityResourceId // ""' "$saved")
    ACR_PROBE_IMAGE=$(jq -r '.probeImage // ""' "$saved")
  fi
  [[ -n "$ACR" ]] || return 0
  validate_registry_args
  jq -n --arg registry "$ACR" --arg identity "$ACR_PULL_IDENTITY" --arg probe "$ACR_PROBE_IMAGE" \
    '{registryName:$registry,identityResourceId:$identity,probeImage:$probe}' > "$next"
  if [[ -f "$saved" ]]; then
    [[ "$(jq -cS 'del(.probeImage)' "$saved")" == "$(jq -cS 'del(.probeImage)' "$next")" ]] \
      || die "registry onboarding identity or ACR differs from saved intent; refusing to change shared-registry access implicitly"
    if [[ -z "$ACR_PROBE_IMAGE" ]]; then ACR_PROBE_IMAGE=$(jq -r '.probeImage // ""' "$saved"); fi
  fi
  jq --arg probe "$ACR_PROBE_IMAGE" '.probeImage=$probe' "$next" > "$saved"
  rm "$next"
}

registry_configuration() {
  local source owner
  source=$(kube -n unbounded-system get configmap gantry-config -o json) || die "cannot read operator Gantry configuration"
  jq -e '.data["config.yaml"] | type == "string" and length > 0' <<<"$source" >/dev/null || die "operator has not created a complete gantry-config yet; rerun after reconciliation"
  owner=$(jq -r '.metadata.annotations["unbounded-cloud.io/registry-auth-owner"] // ""' <<<"$source")
  if [[ -n "$(jq -r '.data["node-auth.yaml"] // ""' <<<"$source")" && "$owner" != "$NAME" ]]; then
    die "existing node-auth policy is not owned by this deployment; refusing to overwrite it"
  fi
  printf '%s\n' "$source"
}

ensure_registry_auth() {
  [[ -n "$ACR" ]] || return 0
  local help registry cluster issuer identity identity_group identity_name principal client tenant
  local credentials credential credential_name matching credential_owned=true role mode source policy patched=false attempt
  help=$(kube -n unbounded-system exec deployment/unbounded-operator -- \
    /unbounded/bin/unbounded-operator registry-auth-node --help 2>"$STATE_DIR/registry-operator-capability.log") \
    || die "selected operator does not support federated registry onboarding; install a compatible release before --acr"
  [[ "$help" == *--token-file* ]] || die "operator lacks the federated broker; no Azure registry grants were made"
  source=$(registry_configuration)
  registry=$(az_cmd acr show --name "$ACR" -o json) || die "cannot read shared ACR $ACR; this script never creates or adopts a registry"
  ACR_RESOURCE_ID=$(jq -er .id <<<"$registry")
  ACR_LOGIN_SERVER=$(jq -er .loginServer <<<"$registry")
  [[ "$ACR_LOGIN_SERVER" =~ ^[a-z0-9.-]+\.azurecr\.io$ ]] || die "this integration requires a public-cloud ACR login server"
  [[ -z "$ACR_PROBE_IMAGE" || "$ACR_PROBE_IMAGE" == "$ACR_LOGIN_SERVER/"* ]] || die "--acr-probe-image must belong to the selected shared ACR"
  cluster=$(az_cmd aks show -g "$RESOURCE_GROUP" --name "$NAME" -o json)
  if [[ "$(jq -r '.oidcIssuerProfile.enabled // false' <<<"$cluster")" != true ]]; then
    wait_cluster_operation
    az_cmd aks update -g "$RESOURCE_GROUP" --name "$NAME" --enable-oidc-issuer -o none
    cluster=$(az_cmd aks show -g "$RESOURCE_GROUP" --name "$NAME" -o json)
  fi
  issuer=$(jq -er '.oidcIssuerProfile.issuerUrl | select(startswith("https://"))' <<<"$cluster") \
    || die "cluster has no usable OIDC issuer; no registry identity can be federated"
  if [[ -n "$ACR_PULL_IDENTITY" ]]; then
    identity=$(az_cmd identity show --ids "$ACR_PULL_IDENTITY" -o json) || die "cannot resolve the supplied pull identity"
    jq -e --arg id "$ACR_PULL_IDENTITY" '(.id | ascii_downcase) == ($id | ascii_downcase)' <<<"$identity" >/dev/null \
      || die "resolved pull identity differs from the requested identity"
  else
    identity_name="$NAME-acr-pull"
    identity=$(lookup_resource Microsoft.ManagedIdentity/userAssignedIdentities "$identity_name")
    if [[ "$identity" == null ]]; then
      az_cmd identity create -g "$RESOURCE_GROUP" --name "$identity_name" --location "$LOCATION" --tags "${TAGS[@]}" -o none
    fi
    identity=$(az_cmd identity show -g "$RESOURCE_GROUP" --name "$identity_name" -o json)
  fi
  jq -e --arg subscription "$SUBSCRIPTION" '(.id | split("/")[2] | ascii_downcase) == ($subscription | ascii_downcase)' <<<"$identity" >/dev/null \
    || die "pull identity must be in the selected subscription"
  PULL_IDENTITY_ID=$(jq -er .id <<<"$identity")
  identity_group=$(jq -r '.id | split("/")[4]' <<<"$identity")
  identity_name=$(jq -r '.id | split("/")[8]' <<<"$identity")
  principal=$(jq -er .principalId <<<"$identity")
  client=$(jq -er .clientId <<<"$identity")
  tenant=$(jq -er .tenantId <<<"$identity")
  python3 - "$principal" "$client" "$tenant" <<'PY' || die "pull identity is missing valid principal, client, or tenant IDs"
import sys
import uuid
for value in sys.argv[1:]:
    uuid.UUID(value)
PY
  role="$ACR_PULL_ROLE"
  mode=$(jq -r '.roleAssignmentMode // "LegacyRegistryPermissions"' <<<"$registry")
  case "$mode" in
    LegacyRegistryPermissions) ;;
    AbacRepositoryPermissions)
      credentials=$(az_cmd role definition list --name 'Container Registry Repository Reader' -o json)
      role=$(jq -er '[.[] | select(.roleType == "BuiltInRole")] | if length == 1 then .[0].name else error("missing or ambiguous repository-reader role") end' <<<"$credentials") ;;
    *) die "unsupported shared ACR role-assignment mode: $mode" ;;
  esac
  credential_name="unbounded-$NAME-$(printf '%s' "$AKS_ID/$issuer/$REGISTRY_SUBJECT" | python3 -c 'import hashlib,sys; print(hashlib.sha256(sys.stdin.buffer.read()).hexdigest()[:12])')"
  credentials=$(az_cmd identity federated-credential list -g "$identity_group" --identity-name "$identity_name" -o json)
  credential=$(jq -c --arg name "$credential_name" '[.[] | select(.name == $name)][0] // null' <<<"$credentials")
  if [[ "$credential" == null ]]; then
    matching=$(jq -c --arg issuer "$issuer" --arg subject "$REGISTRY_SUBJECT" --arg audience "$REGISTRY_AUDIENCE" \
      '[.[] | select(.issuer == $issuer and .subject == $subject and .audiences == [$audience])] | if length > 1 then error("ambiguous matching federation") else .[0] // null end' <<<"$credentials")
    if [[ "$matching" != null ]]; then
      credential="$matching"
      credential_name=$(jq -er .name <<<"$matching")
      credential_owned=false
    fi
  fi
  if [[ "$credential" == null ]]; then
    az_cmd identity federated-credential create -g "$identity_group" --identity-name "$identity_name" --name "$credential_name" \
      --issuer "$issuer" --subject "$REGISTRY_SUBJECT" --audiences "$REGISTRY_AUDIENCE" -o none
  else
    jq -e --arg issuer "$issuer" --arg subject "$REGISTRY_SUBJECT" --arg audience "$REGISTRY_AUDIENCE" \
      '.issuer == $issuer and .subject == $subject and .audiences == [$audience]' <<<"$credential" >/dev/null \
      || die "existing federation binding conflicts with this cluster; refusing to overwrite it"
  fi
  # One registry-scoped grant to the selected pull identity, never to VM identities.
  ensure_role "$principal" "$role" "$ACR_RESOURCE_ID"
  jq -n --arg registry "$ACR_RESOURCE_ID" --arg identity "$PULL_IDENTITY_ID" --arg principal "$principal" \
    --arg client "$client" --arg tenant "$tenant" --arg issuer "$issuer" --arg credential "$credential_name" \
    --arg subject "$REGISTRY_SUBJECT" --arg role "$role" --arg supplied "$ACR_PULL_IDENTITY" --argjson managedFederation "$credential_owned" \
    '{registryId:$registry,identityResourceId:$identity,principalId:$principal,clientId:$client,tenantId:$tenant,
      issuer:$issuer,federatedCredentialName:$credential,subject:$subject,pullRoleId:$role,identityCreatedByDeployment:($supplied == ""),federationManagedByDeployment:$managedFederation}' \
    > "$STATE_DIR/registry-access.json"
  policy=$(jq -cn --arg registry "$ACR_LOGIN_SERVER" --arg client "$client" --arg tenant "$tenant" --arg probe "$ACR_PROBE_IMAGE" \
    '{mode:"workloadIdentity",registry:$registry,clientID:$client,tenantID:$tenant} + (if $probe == "" then {} else {probeImage:$probe} end)')
  for attempt in 1 2 3 4 5; do
    source=$(registry_configuration)
    jq -n --argjson source "$source" --arg policy "$policy" --arg name "$NAME" \
      '[{op:"test",path:"/metadata/resourceVersion",value:$source.metadata.resourceVersion},
        {op:"add",path:"/metadata/annotations",value:(($source.metadata.annotations // {}) + {"unbounded-cloud.io/registry-auth-owner":$name})},
        {op:"add",path:"/data/node-auth.yaml",value:$policy}]' > "$STATE_DIR/registry-config-patch.json"
    if kube -n unbounded-system patch configmap gantry-config --type=json --patch-file "$STATE_DIR/registry-config-patch.json" -o name \
      2>"$STATE_DIR/registry-config-patch.log"; then patched=true; break; fi
    log "Registry configuration patch attempt $attempt/5 failed; retrying against current state"
    sleep 1
  done
  [[ "$patched" == true ]] || die "could not merge registry onboarding policy; inspect registry-config-patch.log and rerun"
  log "Configured shared ACR $ACR_LOGIN_SERVER with cluster pull identity $PULL_IDENTITY_ID"
  log "Application specifications and ServiceAccounts are unchanged; registry auth uses the dedicated federated pull identity"
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
    found=$(kube -n unbounded-system get "$resource" --ignore-not-found -o name) \
      || die "cannot query $resource while waiting for operator reconciliation"
    [[ -z "$found" ]] || break
    ((SECONDS < deadline)) || die "operator did not create $resource before the timeout"
    sleep 5
  done
  kube -n unbounded-system rollout status "$resource" --timeout="${TIMEOUT}s"
}

setup_cni() {
  log "Installing Unbounded operator and private-L3 CNI topology"
  # Invoke the plugin explicitly so kubectl's plugin dispatch does not need to
  # interpret a --kubeconfig flag before the plugin name.
  local deployments
  deployments=$(kube get deployments -A -o json)
  if ! jq -e 'any(.items[]; .metadata.name == "unbounded-operator" and .metadata.namespace == "unbounded-system")' <<<"$deployments" >/dev/null; then
    kubectl-unbounded install --kubeconfig "$STATE_DIR/kubeconfig" --timeout "${TIMEOUT}s"
  else
    # Retain an operator that has been upgraded since initial provisioning.
    # Replaying the pinned installer would otherwise downgrade its image.
    kube -n unbounded-system rollout status deployment/unbounded-operator --timeout="${TIMEOUT}s"
    kube wait --for=condition=Established crd/sites.unbounded-cloud.io --timeout="${TIMEOUT}s"
  fi
  jq -n --arg system "$SYSTEM_NODE_CIDR" --arg flex "$FLEX_NODE_CIDR" \
    --arg clusterPods "$CLUSTER_POD_CIDR" --arg flexPods "$FLEX_POD_CIDR" \
    '{apiVersion:"v1",kind:"List",items:[
      {apiVersion:"unbounded-cloud.io/v1alpha3",kind:"Site",metadata:{name:"cluster"},
       spec:{nodeCidrs:[$system],manageCniPlugin:true,podCidrAssignments:[{assignmentEnabled:true,cidrBlocks:[$clusterPods]}],components:{machina:{enabled:true}}}},
      {apiVersion:"unbounded-cloud.io/v1alpha3",kind:"Site",metadata:{name:"flex-site"},
       spec:{nodeCidrs:[$flex],manageCniPlugin:true,podCidrAssignments:[{assignmentEnabled:true,cidrBlocks:[$flexPods]}]}},
      {apiVersion:"net.unbounded-cloud.io/v1alpha1",kind:"SitePeering",metadata:{name:"cluster-flex-private-l3"},
       spec:{sites:["cluster","flex-site"],meshNodes:true,tunnelProtocol:"Auto"}}
    ]}' > "$STATE_DIR/sites.json"
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

ensure_vm() {
  local name="$1" existing pip nic vm power
  existing=$(lookup_resource Microsoft.Network/publicIPAddresses "$name-pip")
  if [[ "$existing" == null ]]; then
    az_cmd network public-ip create -g "$RESOURCE_GROUP" --name "$name-pip" --location "$LOCATION" \
      --sku Standard --allocation-method Static --version IPv4 --tags "${TAGS[@]}" -o none
  fi
  pip=$(az_cmd network public-ip show -g "$RESOURCE_GROUP" --name "$name-pip" -o json)
  jq -e '.sku.name == "Standard" and .publicIPAllocationMethod == "Static"' <<<"$pip" >/dev/null || die "$name public IP has conflicting settings"
  existing=$(lookup_resource Microsoft.Network/networkInterfaces "$name-nic")
  if [[ "$existing" == null ]]; then
    az_cmd network nic create -g "$RESOURCE_GROUP" --name "$name-nic" --location "$LOCATION" \
      --subnet "$FLEX_SUBNET_ID" --public-ip-address "$(jq -r .id <<<"$pip")" --ip-forwarding true --tags "${TAGS[@]}" -o none
  fi
  nic=$(az_cmd network nic show -g "$RESOURCE_GROUP" --name "$name-nic" -o json)
  jq -e --arg subnet "$FLEX_SUBNET_ID" --arg pip "$(jq -r .id <<<"$pip")" \
    '.enableIPForwarding == true and .ipConfigurations[0].subnet.id == $subnet and .ipConfigurations[0].publicIPAddress.id == $pip' <<<"$nic" >/dev/null \
    || die "$name NIC does not match the Flex network"
  existing=$(lookup_resource Microsoft.Compute/virtualMachines "$name")
  if [[ "$existing" == null ]]; then
    log "Creating Ubuntu 24.04 Flex VM $name"
    az_cmd vm create -g "$RESOURCE_GROUP" --name "$name" --location "$LOCATION" \
      --image Canonical:ubuntu-24_04-lts:server:latest --size "$VM_SIZE" --nics "$(jq -r .id <<<"$nic")" \
      --admin-username "$ADMIN_USER" --ssh-key-values "$STATE_DIR/ssh.pub" --assign-identity '[system]' \
      --os-disk-size-gb 128 --storage-sku StandardSSD_LRS --security-type Standard --tags "${TAGS[@]}" -o none
  fi
  vm=$(az_cmd vm show -g "$RESOURCE_GROUP" --name "$name" -o json)
  jq -e --arg size "$VM_SIZE" --arg name "$name" --arg nic "$(jq -r .id <<<"$nic")" \
    '.hardwareProfile.vmSize == $size and .osProfile.computerName == $name
     and .storageProfile.imageReference.publisher == "Canonical" and .storageProfile.imageReference.offer == "ubuntu-24_04-lts"
     and .networkProfile.networkInterfaces[0].id == $nic and (.identity.type | contains("SystemAssigned"))
     and (.identity.principalId | type == "string" and length > 0)' <<<"$vm" >/dev/null \
    || die "$name VM has conflicting hardware, image, network, or identity settings"
  ensure_role "$(jq -r .identity.principalId <<<"$vm")" "$FLEX_ROLE" "$POOL_ID"
  power=$(az_cmd vm get-instance-view -g "$RESOURCE_GROUP" --name "$name" --query "instanceView.statuses[?starts_with(code, 'PowerState/')].code | [0]" -o tsv)
  case "$power" in
    PowerState/running) ;;
    PowerState/deallocated|PowerState/stopped) az_cmd vm start -g "$RESOURCE_GROUP" --name "$name" -o none ;;
    *) die "$name has unexpected power state: $power" ;;
  esac
  VM_PUBLIC_IP=$(jq -r .ipAddress <<<"$pip")
  VM_PRIVATE_IP=$(jq -r .ipConfigurations[0].privateIPAddress <<<"$nic")
  [[ "$VM_PUBLIC_IP" != null && "$VM_PRIVATE_IP" != null ]] || die "$name has no assigned IP addresses"
}

ssh_vm() {
  ssh -i "$SSH_KEY" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectTimeout=10 \
    -o StrictHostKeyChecking=accept-new -o "UserKnownHostsFile=$STATE_DIR/known_hosts" \
    "$ADMIN_USER@$VM_PUBLIC_IP" "$@"
}

write_remote_script() {
  local name="$1" file="$2"
  {
    printf '#!/usr/bin/env bash\nset -euo pipefail\numask 077\n'
    printf 'AKS_ID=%q\nPOOL=%q\nNODE=%q\nNODE_IP=%q\nVERSION=%q\nDIGEST=%q\nWAIT_TIMEOUT=%q\n' \
      "$AKS_ID" "$FLEX_POOL" "$name" "$VM_PRIVATE_IP" "$AGENT_VERSION" "$AGENT_DIGEST" "$TIMEOUT"
    cat <<'REMOTE'
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
cloud-init status --wait
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates jq util-linux nftables systemd-container
install -d -m 0700 /run/aks-flex-node-bootstrap
# Role assignment presence in ARM does not mean that IMDS and the RP have
# accepted it yet. Wait for an authorized Machine GET before changing the host
# runtime. A 404 is expected for a machine that has never joined this pool.
deadline=$((SECONDS + WAIT_TIMEOUT))
while true; do
  token_response=$(curl -fsS --connect-timeout 10 --max-time 30 -H Metadata:true \
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

run_vm_command() {
  local name="$1" file="$2" marker wrapper response status
  marker="__AKS_FLEX_REMOTE_EXIT_$(openssl rand -hex 12)__"
  wrapper="$STATE_DIR/run-command-$name.sh"
  response="$STATE_DIR/run-command-$name.json"
  {
    # The Azure Linux handler may invoke this file with /bin/sh, regardless
    # of its shebang. Enter Bash explicitly before using Bash-only syntax.
    printf '#!/bin/sh\nexec /bin/bash <<\047AKS_FLEX_COMMAND_BASH\047\nset -euo pipefail\numask 077\nmarker=%q\n' "$marker"
    cat <<'RUN_COMMAND'
# Run Command reports extension success independently of the shell exit code.
# Emit a unique final stdout marker even on failure, and verify it locally.
trap 'status=$?; printf "%s=%s\n" "$marker" "$status"' EXIT
base64 -d > /run/aks-flex-node-deployment.sh <<'DEPLOYMENT_BASE64'
RUN_COMMAND
    base64 < "$file"
    cat <<'RUN_COMMAND'
DEPLOYMENT_BASE64
chmod 0700 /run/aks-flex-node-deployment.sh
RUN_COMMAND
    printf 'timeout --signal=TERM --kill-after=30s %q bash /run/aks-flex-node-deployment.sh\n' "$TIMEOUT"
    printf 'rm -f /run/aks-flex-node-deployment.sh\nAKS_FLEX_COMMAND_BASH\n'
  } > "$wrapper"
  bash -n "$wrapper"
  log "Bootstrapping/checking $name through the Azure VM agent; SSH ingress remains $SSH_SOURCE_IP/32"
  az_cmd vm run-command invoke -g "$RESOURCE_GROUP" --name "$name" \
    --command-id RunShellScript --scripts "@$wrapper" -o json > "$response" \
    || die "Azure Run Command failed on $name; inspect $response before retrying"
  status=$(jq -er --arg marker "$marker=" \
    '[.value[]? | .message | split("\n")[] | select(startswith($marker)) | ltrimstr($marker) | sub("\\r$"; "")] | if length == 1 then .[0] else error("missing or ambiguous remote exit marker") end' "$response") \
    || die "Run Command on $name did not report a shell exit code; inspect $response (no automatic reset)"
  jq -r '.value[]? | .message' "$response"
  [[ "$status" == 0 ]] || die "bootstrap/check failed on $name with exit $status; inspect $response (no automatic reset)"
}

join_vm() {
  local name="$1" deadline=$((SECONDS + TIMEOUT)) file="$STATE_DIR/remote-$1.sh"
  write_remote_script "$name" "$file"
  if [[ "$BOOTSTRAP_TRANSPORT" == run-command ]]; then
    run_vm_command "$name" "$file"
    return
  fi
  log "Waiting for SSH on $name ($VM_PUBLIC_IP); ingress is restricted to $SSH_SOURCE_IP/32"
  until ssh_vm true >/dev/null 2>&1; do
    ((SECONDS < deadline)) || die "SSH unavailable on $name; check your public IP, NSG, and SSH key"
    sleep 10
  done
  # Do not retry the entire bootstrap after it has modified a host. Bound
  # library throttling retries and leave any interrupted deployment intact.
  ssh_vm sudo timeout --signal=TERM --kill-after=30s "$TIMEOUT" bash -s < "$file" || die "bootstrap/check failed on $name; inspect its logs before retrying (no automatic reset)"
}

validate_deployment() {
  local machines nodes index name
  log "Checking every Flex Node and its ARM Machine"
  machines=$(az_cmd rest --method get --url "https://management.azure.com$POOL_ID/machines?api-version=$POOL_API_VERSION" -o json)
  for ((index=1; index<=FLEX_COUNT; index++)); do
    name="$NAME-flex$index"
    jq -e --arg name "$name" 'any(.value[]; (.name | ascii_downcase) == $name and .properties.provisioningState == "Succeeded")' <<<"$machines" >/dev/null \
      || die "ARM Machine $name is missing or not successfully provisioned"
    kube wait --for=condition=Ready "node/$name" --timeout="${TIMEOUT}s"
    nodes=$(kube get "node/$name" -o json)
    jq -e --arg pool "$FLEX_POOL" '.metadata.labels["kubernetes.azure.com/agentpool"] == $pool and .metadata.labels["unbounded-cloud.io/site"] == "flex-site"' <<<"$nodes" >/dev/null \
      || die "$name is not assigned to the expected pool and network site"
  done
  wait_component_rollout daemonset/unbounded-net-node
  wait_component_rollout daemonset/unbounded-net-kube-proxy-flex-site
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
  prepare_registry_intent
  ensure_features
  ensure_group
  ensure_network
  ensure_cluster
  install_plugin
  setup_cni
  ensure_registry_auth
  ensure_flex_pool
  AGENT_DIGEST=$(release_digest Azure/AKSFlexNode "$AGENT_VERSION" aks-flex-node-linux-amd64.tar.gz)
  fetch_file "https://raw.githubusercontent.com/Azure/AKSFlexNode/$AGENT_VERSION/scripts/bootstrap.sh" "$STATE_DIR/cache/bootstrap-$AGENT_VERSION.sh"
  bash -n "$STATE_DIR/cache/bootstrap-$AGENT_VERSION.sh"
  local index name
  : > "$STATE_DIR/ssh-access.txt"
  for ((index=1; index<=FLEX_COUNT; index++)); do
    name="$NAME-flex$index"
    ensure_vm "$name"
    printf 'ssh -i %q -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%q %q\n' \
      "$SSH_KEY" "$STATE_DIR/known_hosts" "$ADMIN_USER@$VM_PUBLIC_IP" >> "$STATE_DIR/ssh-access.txt"
    join_vm "$name"
  done
  validate_deployment
  if [[ -n "$ACR" ]]; then wait_component_rollout daemonset/gantry-acr-auth; fi
  smoke_test
  log "Deployment ready: one system node and $FLEX_COUNT Flex nodes"
  kube get nodes -L kubernetes.azure.com/agentpool,unbounded-cloud.io/site -o wide
  printf '\nKubeconfig: %s\nSSH ingress: %s/32\nSSH access:\n' "$STATE_DIR/kubeconfig" "$SSH_SOURCE_IP"
  while IFS= read -r line; do printf '  %s\n' "$line"; done < "$STATE_DIR/ssh-access.txt"
  printf '\nRerun with the same arguments and state directory. No node reset or agent downgrade is performed.\n'
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
