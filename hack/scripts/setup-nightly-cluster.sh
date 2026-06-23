#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# setup-nightly-cluster.sh - One-shot provisioning of the unbounded-nightly
# integration cluster and everything the nightly deploy workflow needs.
#
# This script automates the operator runbook for the unbounded-nightly
# cluster (the nightly sibling of unbounded-stable; see
# .github/workflows/nightly.yaml). It:
#
#   1. Builds the forge tool (go build -o bin/forge).
#   2. Creates an AKS cluster with `forge cluster create`. forge also makes
#      the gateway node pool (already labeled
#      unbounded-cloud.io/unbounded-net-gateway=true), opens the WireGuard
#      ports, lays down the unbounded bootstrap token, and writes a
#      kubeconfig. Its stdout is a JSON object with ResourceGroup,
#      NodePoolsResourceGroup, SubscriptionID, ClusterName, KubeconfigPath.
#   3. Auto-detects the cluster node/pod CIDRs from AKS (the same way
#      hack/scripts/aks-quickstart.sh does). The site node/pod CIDRs are
#      unbounded facts and default to constants (overridable).
#   4. Creates the Orca origin: an Azure Blob storage account (default
#      ub<site>01, e.g. ubnightly01) + container, and reads its key.
#   5. Configures the unbounded-nightly GitHub Environment via
#      hack/scripts/setup-deploy-environment.sh.
#   6. Creates the unbounded-kube namespace and the orca-credentials Secret
#      on the cluster (hack/orca/create-credentials-secret.sh).
#   7. Triggers the nightly workflow (force_init=true) and watches it to
#      completion.
#
# It is idempotent: re-running skips an existing cluster / storage account /
# namespace, and the Environment + Secret are create-or-update.
#
# Prerequisites:
#   - az CLI, logged in (az login) to the target tenant/subscription.
#   - kubectl, gh (authenticated with admin on --repo), go, openssl, jq.
#   - The nightly workflow must already be on the repo's default branch
#     (merge the PR that adds .github/workflows/nightly.yaml first), or
#     step 7 has nothing to trigger.
#
# Usage:
#   hack/scripts/setup-nightly-cluster.sh \
#     --subscription <sub-id> \
#     --orca-azure-container orca-origin \
#     [flags]
#
# See --help for all flags.

set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults.
# ---------------------------------------------------------------------------
ENV_NAME="unbounded-nightly"
SITE_NAME="nightly"
CLUSTER_NAME="unbounded-nightly"
LOCATION="canadacentral"
REPO="Azure/unbounded"
SUBSCRIPTION="${AZURE_SUBSCRIPTION_ID:-}"
MANAGE_CNI_PLUGIN="true"

# Site (unbounded overlay) CIDRs - unbounded facts, default constants.
SITE_NODE_CIDR="10.1.0.0/16"
SITE_POD_CIDR="100.125.0.0/16"
# Standard Kubernetes pod CIDR fallback when AKS reports none (BYO CNI).
DEFAULT_POD_CIDR="10.244.0.0/16"

# Cluster CIDRs - auto-detected from AKS unless overridden.
CLUSTER_NODE_CIDR=""
CLUSTER_POD_CIDR=""

# forge cluster sizing (pass-through).
SYSTEM_POOL_NODE_COUNT=""
GATEWAY_POOL_NODE_COUNT=""
SYSTEM_POOL_NODE_SKU=""
GATEWAY_POOL_NODE_SKU=""

# Orca origin.
ORIGIN_ACCOUNT=""
ORIGIN_CONTAINER="orca-origin"
ORIGIN_RG=""
ORIGIN_KEY="${ORCA_AZUREBLOB_ACCOUNT_KEY:-}"
ORCA_AZURE_ENDPOINT=""

ASSUME_YES="false"
WATCH="true"
TRIGGER="true"

# Populated by ensure_cluster.
RESOURCE_GROUP=""
NODE_RESOURCE_GROUP=""
KUBECONFIG_PATH=""

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

# ---------------------------------------------------------------------------
# Logging helpers.
# ---------------------------------------------------------------------------
log()  { echo -e ">> $*" >&2; }
info() { echo -e "   $*" >&2; }
warn() { echo -e "!! $*" >&2; }
die()  { echo -e "!! $*" >&2; exit 1; }

usage() {
  awk '
    /^#!/ { next }
    /^#/  { sub(/^# ?/, ""); print; next }
    { exit }
  ' "${BASH_SOURCE[0]}"
  cat >&2 <<'EOF'

Flags:
  --subscription ID          Azure subscription ID (or env AZURE_SUBSCRIPTION_ID)
  --location LOC             Azure location (default: canadacentral)
  --cluster-name NAME        AKS cluster / resource group name (default: unbounded-nightly)
  --env-name NAME            GitHub Environment name (default: unbounded-nightly)
  --site-name NAME           unbounded site name (default: nightly)
  --repo OWNER/NAME          Target repository (default: Azure/unbounded)
  --manage-cni-plugin BOOL   Whether unbounded manages the CNI (default: true)

  --site-node-cidr CIDR      Site node CIDR (default: 10.1.0.0/16)
  --site-pod-cidr CIDR       Site pod CIDR (default: 100.125.0.0/16)
  --cluster-node-cidr CIDR   Override auto-detected cluster node CIDR
  --cluster-pod-cidr CIDR    Override auto-detected cluster pod CIDR

  --origin-account NAME      Orca origin storage account (default: ub<site>01)
  --origin-container NAME    Orca origin blob container (default: orca-origin)
  --origin-rg NAME           Resource group for the origin account (default: cluster RG)
  --origin-key KEY           Origin account key (default: env ORCA_AZUREBLOB_ACCOUNT_KEY or fetched via az)
  --orca-azure-endpoint URL  Azure blob endpoint (default: *.blob.core.windows.net)

  --system-pool-node-count N forge system pool node count
  --gateway-pool-node-count N forge gateway pool node count
  --system-pool-node-sku SKU forge system pool VM SKU
  --gateway-pool-node-sku SKU forge gateway pool VM SKU

  --no-watch                 Trigger the deploy run but do not wait for it
  --no-trigger               Provision only; do not trigger the deploy run.
                             Use this to test the workflow from a branch before
                             it is on the default branch: provision, then push
                             the branch to fire its push-triggered run.
  --yes                      Skip confirmation prompts
  --help                     Show this help
EOF
  exit "${1:-0}"
}

# ---------------------------------------------------------------------------
# CIDR helpers (mirrors hack/scripts/aks-quickstart.sh).
# ---------------------------------------------------------------------------
is_valid_cidr() {
  local cidr="$1"
  [[ "$cidr" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$ ]] || return 1
  local prefix="${cidr#*/}"
  (( prefix >= 0 && prefix <= 32 )) || return 1
  return 0
}

ip4_to_int() {
  local IFS=.
  read -r a b c d <<< "$1"
  echo $(( (a << 24) | (b << 16) | (c << 8) | d ))
}

# subnet_contains_all <prefix/len> <newline-separated IPs>
subnet_contains_all() {
  local prefix="${1%/*}"
  local len="${1#*/}"
  local mask=$(( 0xFFFFFFFF << (32 - len) & 0xFFFFFFFF ))
  local net_int
  net_int=$(ip4_to_int "$prefix")
  local network=$(( net_int & mask ))
  while IFS= read -r ip; do
    [[ -z "$ip" ]] && continue
    local ip_int
    ip_int=$(ip4_to_int "$ip")
    [[ $(( ip_int & mask )) -eq $network ]] || return 1
  done <<< "$2"
  return 0
}

KCTL() { kubectl --kubeconfig "${KUBECONFIG_PATH}" "$@"; }

# ---------------------------------------------------------------------------
# Argument parsing.
# ---------------------------------------------------------------------------
require_value() {
  if [[ -z "${2:-}" || "${2:0:2}" == "--" ]]; then
    die "flag $1 requires a value"
  fi
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --subscription)            require_value "$1" "${2:-}"; SUBSCRIPTION="$2"; shift 2 ;;
    --location)                require_value "$1" "${2:-}"; LOCATION="$2"; shift 2 ;;
    --cluster-name)            require_value "$1" "${2:-}"; CLUSTER_NAME="$2"; shift 2 ;;
    --env-name)                require_value "$1" "${2:-}"; ENV_NAME="$2"; shift 2 ;;
    --site-name)               require_value "$1" "${2:-}"; SITE_NAME="$2"; shift 2 ;;
    --repo)                    require_value "$1" "${2:-}"; REPO="$2"; shift 2 ;;
    --manage-cni-plugin)       require_value "$1" "${2:-}"; MANAGE_CNI_PLUGIN="$2"; shift 2 ;;
    --site-node-cidr)          require_value "$1" "${2:-}"; SITE_NODE_CIDR="$2"; shift 2 ;;
    --site-pod-cidr)           require_value "$1" "${2:-}"; SITE_POD_CIDR="$2"; shift 2 ;;
    --cluster-node-cidr)       require_value "$1" "${2:-}"; CLUSTER_NODE_CIDR="$2"; shift 2 ;;
    --cluster-pod-cidr)        require_value "$1" "${2:-}"; CLUSTER_POD_CIDR="$2"; shift 2 ;;
    --origin-account)          require_value "$1" "${2:-}"; ORIGIN_ACCOUNT="$2"; shift 2 ;;
    --origin-container)        require_value "$1" "${2:-}"; ORIGIN_CONTAINER="$2"; shift 2 ;;
    --origin-rg)               require_value "$1" "${2:-}"; ORIGIN_RG="$2"; shift 2 ;;
    --origin-key)              require_value "$1" "${2:-}"; ORIGIN_KEY="$2"; shift 2 ;;
    --orca-azure-endpoint)     require_value "$1" "${2:-}"; ORCA_AZURE_ENDPOINT="$2"; shift 2 ;;
    --system-pool-node-count)  require_value "$1" "${2:-}"; SYSTEM_POOL_NODE_COUNT="$2"; shift 2 ;;
    --gateway-pool-node-count) require_value "$1" "${2:-}"; GATEWAY_POOL_NODE_COUNT="$2"; shift 2 ;;
    --system-pool-node-sku)    require_value "$1" "${2:-}"; SYSTEM_POOL_NODE_SKU="$2"; shift 2 ;;
    --gateway-pool-node-sku)   require_value "$1" "${2:-}"; GATEWAY_POOL_NODE_SKU="$2"; shift 2 ;;
    --no-watch)                WATCH="false"; shift ;;
    --no-trigger)              TRIGGER="false"; shift ;;
    --yes)                     ASSUME_YES="true"; shift ;;
    --help|-h)                 usage 0 ;;
    *)                         die "unknown argument: $1 (try --help)" ;;
  esac
done

# Derive defaults that depend on other flags.
[[ -n "${ORIGIN_ACCOUNT}" ]] || ORIGIN_ACCOUNT="ub${SITE_NAME}01"

# ---------------------------------------------------------------------------
# Preflight.
# ---------------------------------------------------------------------------
preflight() {
  log "Preflight checks"

  for tool in az kubectl gh go openssl jq; do
    command -v "$tool" >/dev/null 2>&1 || die "'$tool' not found on PATH"
  done

  az account show >/dev/null 2>&1 || die "az is not logged in; run 'az login' first"
  gh auth status >/dev/null 2>&1 || die "gh is not authenticated; run 'gh auth login' first"

  [[ -n "${SUBSCRIPTION}" ]] || SUBSCRIPTION="$(az account show --query id -o tsv)"
  [[ -n "${SUBSCRIPTION}" ]] || die "--subscription is required (or set AZURE_SUBSCRIPTION_ID)"

  is_valid_cidr "${SITE_NODE_CIDR}" || die "invalid --site-node-cidr: ${SITE_NODE_CIDR}"
  is_valid_cidr "${SITE_POD_CIDR}"  || die "invalid --site-pod-cidr: ${SITE_POD_CIDR}"
  case "${MANAGE_CNI_PLUGIN}" in true|false) ;; *) die "--manage-cni-plugin must be true|false" ;; esac

  # Storage account names: 3-24 lowercase alphanumeric.
  [[ "${ORIGIN_ACCOUNT}" =~ ^[a-z0-9]{3,24}$ ]] \
    || die "origin account '${ORIGIN_ACCOUNT}' must be 3-24 lowercase alphanumeric chars (override with --origin-account)"

  # The nightly workflow must exist on the default branch to be triggerable
  # via workflow_dispatch. Skipped when --no-trigger (e.g. pre-merge testing,
  # where the run is fired by pushing the branch instead).
  if [[ "${TRIGGER}" == "true" ]]; then
    if ! gh workflow view nightly.yaml --repo "${REPO}" >/dev/null 2>&1; then
      die "workflow 'nightly.yaml' not found on ${REPO}'s default branch; merge the PR that adds it, or pass --no-trigger to provision and test from a branch"
    fi
  fi

  info "subscription: ${SUBSCRIPTION}"
  info "cluster:      ${CLUSTER_NAME} (${LOCATION})"
  info "environment:  ${ENV_NAME} / site ${SITE_NAME}"
  info "origin:       ${ORIGIN_ACCOUNT}/${ORIGIN_CONTAINER}"
}

confirm() {
  [[ "${ASSUME_YES}" == "true" ]] && return 0
  echo >&2
  read -r -p "Proceed with provisioning ${ENV_NAME}? [y/N] " reply
  case "${reply}" in y|Y|yes|YES) ;; *) die "aborted" ;; esac
}

# ---------------------------------------------------------------------------
# Build forge.
# ---------------------------------------------------------------------------
build_forge() {
  log "Building forge (go build -o bin/forge ./hack/cmd/forge)"
  ( cd "${REPO_ROOT}" && go build -o bin/forge ./hack/cmd/forge )
}

# ---------------------------------------------------------------------------
# Create (or reuse) the AKS cluster. Sets RESOURCE_GROUP,
# NODE_RESOURCE_GROUP, KUBECONFIG_PATH.
# ---------------------------------------------------------------------------
ensure_cluster() {
  if az aks show -g "${CLUSTER_NAME}" -n "${CLUSTER_NAME}" --subscription "${SUBSCRIPTION}" >/dev/null 2>&1; then
    log "Cluster ${CLUSTER_NAME} already exists; reusing it"
    RESOURCE_GROUP="${CLUSTER_NAME}"
    NODE_RESOURCE_GROUP="$(az aks show -g "${CLUSTER_NAME}" -n "${CLUSTER_NAME}" \
      --subscription "${SUBSCRIPTION}" --query nodeResourceGroup -o tsv)"
    KUBECONFIG_PATH="${HOME}/.unbounded-forge/${CLUSTER_NAME}/kubeconfig"
    mkdir -p "$(dirname "${KUBECONFIG_PATH}")"
    az aks get-credentials -g "${CLUSTER_NAME}" -n "${CLUSTER_NAME}" \
      --subscription "${SUBSCRIPTION}" --file "${KUBECONFIG_PATH}" --overwrite-existing >/dev/null
  else
    log "Creating AKS cluster ${CLUSTER_NAME} with forge"
    local args=(cluster create --name "${CLUSTER_NAME}" --location "${LOCATION}" --subscription "${SUBSCRIPTION}")
    [[ -n "${SYSTEM_POOL_NODE_COUNT}" ]]  && args+=(--system-pool-node-count "${SYSTEM_POOL_NODE_COUNT}")
    [[ -n "${GATEWAY_POOL_NODE_COUNT}" ]] && args+=(--gateway-pool-node-count "${GATEWAY_POOL_NODE_COUNT}")
    [[ -n "${SYSTEM_POOL_NODE_SKU}" ]]    && args+=(--system-pool-node-sku "${SYSTEM_POOL_NODE_SKU}")
    [[ -n "${GATEWAY_POOL_NODE_SKU}" ]]   && args+=(--gateway-pool-node-sku "${GATEWAY_POOL_NODE_SKU}")

    # forge prints progress logs to stderr and the result JSON to stdout.
    local out
    out="$(AZURE_AUTH_CHAIN_ORDER=CLI "${REPO_ROOT}/bin/forge" "${args[@]}")"

    RESOURCE_GROUP="$(jq -r '.ResourceGroup' <<<"${out}")"
    NODE_RESOURCE_GROUP="$(jq -r '.NodePoolsResourceGroup' <<<"${out}")"
    KUBECONFIG_PATH="$(jq -r '.KubeconfigPath' <<<"${out}")"
  fi

  [[ -n "${RESOURCE_GROUP}" && "${RESOURCE_GROUP}" != "null" ]]           || die "could not determine cluster resource group"
  [[ -n "${NODE_RESOURCE_GROUP}" && "${NODE_RESOURCE_GROUP}" != "null" ]] || die "could not determine node resource group"
  [[ -f "${KUBECONFIG_PATH}" ]]                                          || die "kubeconfig not found at ${KUBECONFIG_PATH}"

  [[ -z "${ORIGIN_RG}" ]] && ORIGIN_RG="${RESOURCE_GROUP}"

  info "resource group:      ${RESOURCE_GROUP}"
  info "node resource group: ${NODE_RESOURCE_GROUP}"
  info "kubeconfig:          ${KUBECONFIG_PATH}"
}

# ---------------------------------------------------------------------------
# Detect cluster node/pod CIDRs from AKS (skipped if both overridden).
# Mirrors hack/scripts/aks-quickstart.sh:detect_cluster_cidrs.
# ---------------------------------------------------------------------------
detect_cluster_cidrs() {
  if [[ -n "${CLUSTER_POD_CIDR}" && -n "${CLUSTER_NODE_CIDR}" ]]; then
    info "using provided cluster CIDRs (node=${CLUSTER_NODE_CIDR}, pod=${CLUSTER_POD_CIDR})"
    return 0
  fi

  log "Detecting cluster CIDRs from AKS"

  if [[ -z "${CLUSTER_POD_CIDR}" ]]; then
    CLUSTER_POD_CIDR="$(az aks show --subscription "${SUBSCRIPTION}" \
      --resource-group "${CLUSTER_NAME}" --name "${CLUSTER_NAME}" \
      --query "networkProfile.podCidr" -o tsv)"
    [[ "${CLUSTER_POD_CIDR}" == "None" ]] && CLUSTER_POD_CIDR=""
    if [[ -z "${CLUSTER_POD_CIDR}" ]]; then
      CLUSTER_POD_CIDR="${DEFAULT_POD_CIDR}"
      info "no pod CIDR in AKS network profile (expected with BYO CNI); using default ${CLUSTER_POD_CIDR}"
    fi
  fi

  if [[ -z "${CLUSTER_NODE_CIDR}" ]]; then
    # Nodes register with an InternalIP even before the CNI makes them Ready.
    local node_ips=""
    local elapsed=0
    while (( elapsed < 300 )); do
      node_ips="$(KCTL get nodes \
        -o jsonpath='{range .items[?(@.spec.providerID)]}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{end}' \
        2>/dev/null | grep -v '^$' || true)"
      [[ -n "${node_ips}" ]] && break
      info "waiting for node IPs to appear..."
      sleep 10
      (( elapsed += 10 ))
    done
    [[ -n "${node_ips}" ]] || die "could not retrieve node internal IPs"

    CLUSTER_NODE_CIDR="$(az network vnet list \
      --subscription "${SUBSCRIPTION}" --resource-group "${NODE_RESOURCE_GROUP}" \
      --query "[].subnets[].addressPrefix" -o tsv | while IFS= read -r prefix; do
        [[ -z "${prefix}" ]] && continue
        subnet_contains_all "${prefix}" "${node_ips}" && echo "${prefix}" && break
      done)"
    [[ -n "${CLUSTER_NODE_CIDR}" ]] || die "could not find a VNet subnet containing all node IPs"
  fi

  is_valid_cidr "${CLUSTER_NODE_CIDR}" || die "detected invalid cluster node CIDR: ${CLUSTER_NODE_CIDR}"
  is_valid_cidr "${CLUSTER_POD_CIDR}"  || die "invalid cluster pod CIDR: ${CLUSTER_POD_CIDR}"
  info "cluster node CIDR: ${CLUSTER_NODE_CIDR}"
  info "cluster pod CIDR:  ${CLUSTER_POD_CIDR}"
}

# ---------------------------------------------------------------------------
# Create the Orca origin storage account + container; resolve its key.
# ---------------------------------------------------------------------------
ensure_origin() {
  log "Ensuring Orca origin storage account ${ORIGIN_ACCOUNT} (rg ${ORIGIN_RG})"

  if az storage account show -n "${ORIGIN_ACCOUNT}" -g "${ORIGIN_RG}" \
      --subscription "${SUBSCRIPTION}" >/dev/null 2>&1; then
    info "storage account ${ORIGIN_ACCOUNT} already exists"
  else
    az storage account create \
      --name "${ORIGIN_ACCOUNT}" --resource-group "${ORIGIN_RG}" \
      --location "${LOCATION}" --subscription "${SUBSCRIPTION}" \
      --sku Standard_LRS --kind StorageV2 --min-tls-version TLS1_2 \
      --allow-blob-public-access false --only-show-errors >/dev/null
    info "created storage account ${ORIGIN_ACCOUNT}"
  fi

  if [[ -z "${ORIGIN_KEY}" ]]; then
    ORIGIN_KEY="$(az storage account keys list \
      --account-name "${ORIGIN_ACCOUNT}" --resource-group "${ORIGIN_RG}" \
      --subscription "${SUBSCRIPTION}" --query "[0].value" -o tsv)"
  fi
  [[ -n "${ORIGIN_KEY}" ]] || die "could not resolve storage account key for ${ORIGIN_ACCOUNT}"

  log "Ensuring blob container ${ORIGIN_CONTAINER}"
  az storage container create \
    --name "${ORIGIN_CONTAINER}" \
    --account-name "${ORIGIN_ACCOUNT}" --account-key "${ORIGIN_KEY}" \
    --only-show-errors >/dev/null
}

# ---------------------------------------------------------------------------
# Configure the GitHub Environment.
# ---------------------------------------------------------------------------
configure_environment() {
  log "Configuring GitHub Environment ${ENV_NAME}"
  local args=(
    --env-name "${ENV_NAME}"
    --kubeconfig "${KUBECONFIG_PATH}"
    --site-name "${SITE_NAME}"
    --cluster-node-cidr "${CLUSTER_NODE_CIDR}"
    --cluster-pod-cidr "${CLUSTER_POD_CIDR}"
    --site-node-cidr "${SITE_NODE_CIDR}"
    --site-pod-cidr "${SITE_POD_CIDR}"
    --manage-cni-plugin "${MANAGE_CNI_PLUGIN}"
    --channel nightly
    --orca-azure-account "${ORIGIN_ACCOUNT}"
    --orca-azure-container "${ORIGIN_CONTAINER}"
    --repo "${REPO}"
    --yes
  )
  [[ -n "${ORCA_AZURE_ENDPOINT}" ]] && args+=(--orca-azure-endpoint "${ORCA_AZURE_ENDPOINT}")

  "${SCRIPT_DIR}/setup-deploy-environment.sh" "${args[@]}"
}

# ---------------------------------------------------------------------------
# Create the unbounded-kube namespace + orca-credentials Secret on cluster.
# ---------------------------------------------------------------------------
ensure_secret() {
  log "Ensuring unbounded-kube namespace and orca-credentials Secret"
  KCTL get namespace unbounded-kube >/dev/null 2>&1 || KCTL create namespace unbounded-kube

  KUBECONFIG="${KUBECONFIG_PATH}" "${REPO_ROOT}/hack/orca/create-credentials-secret.sh" \
    --azure-account-key "${ORIGIN_KEY}"
}

# ---------------------------------------------------------------------------
# Trigger the nightly workflow (force_init) and optionally watch it.
#
# With --no-trigger we provision only and tell the operator how to fire the
# run by pushing the branch (used to test the workflow before it is on the
# default branch, where workflow_dispatch is not available).
# ---------------------------------------------------------------------------
trigger_deploy() {
  if [[ "${TRIGGER}" != "true" ]]; then
    local branch
    branch="$(git -C "${REPO_ROOT}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '<branch>')"
    log "Skipping deploy trigger (--no-trigger)"
    info "Provisioning is complete. To run the workflow from this branch, push it:"
    info "    git push origin ${branch}"
    info "The push-triggered run does the build, init deploy, Orca deploy, and smoke."
    info "Watch it: gh run watch \$(gh run list --repo ${REPO} --workflow nightly.yaml --limit 1 --json databaseId --jq '.[0].databaseId') --repo ${REPO}"
    return 0
  fi

  log "Triggering nightly workflow (force_init=true)"

  # Record the newest run id before dispatch so we can identify the new one.
  local before
  before="$(gh run list --repo "${REPO}" --workflow nightly.yaml \
    --limit 1 --json databaseId --jq '.[0].databaseId // 0' 2>/dev/null || echo 0)"

  gh workflow run nightly.yaml --repo "${REPO}" -f force_init=true

  log "Waiting for the run to register..."
  local run_id="" elapsed=0
  while (( elapsed < 60 )); do
    run_id="$(gh run list --repo "${REPO}" --workflow nightly.yaml \
      --limit 1 --json databaseId --jq '.[0].databaseId // 0' 2>/dev/null || echo 0)"
    [[ -n "${run_id}" && "${run_id}" != "0" && "${run_id}" != "${before}" ]] && break
    sleep 3
    (( elapsed += 3 ))
  done

  if [[ -z "${run_id}" || "${run_id}" == "0" || "${run_id}" == "${before}" ]]; then
    warn "could not identify the new run; check: gh run list --repo ${REPO} --workflow nightly.yaml"
    return 0
  fi

  local run_url
  run_url="$(gh run view "${run_id}" --repo "${REPO}" --json url --jq '.url' 2>/dev/null || true)"
  info "run: ${run_url:-${run_id}}"

  if [[ "${WATCH}" == "true" ]]; then
    log "Watching run ${run_id} to completion"
    gh run watch "${run_id}" --repo "${REPO}" --exit-status
  fi
}

# ---------------------------------------------------------------------------
# Print a verification summary.
# ---------------------------------------------------------------------------
verify() {
  log "Cluster state"
  KCTL -n unbounded-net  rollout status deploy/unbounded-net-controller --timeout=60s 2>/dev/null || true
  KCTL -n unbounded-net  rollout status ds/unbounded-net-node           --timeout=60s 2>/dev/null || true
  KCTL -n unbounded-kube rollout status deploy/machina-controller       --timeout=60s 2>/dev/null || true
  KCTL -n unbounded-kube get deploy orca garage 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Main.
# ---------------------------------------------------------------------------
main() {
  preflight
  confirm
  build_forge
  ensure_cluster
  detect_cluster_cidrs
  ensure_origin
  configure_environment
  ensure_secret
  trigger_deploy

  # Cluster state is only meaningful once a deploy has actually completed.
  if [[ "${TRIGGER}" == "true" && "${WATCH}" == "true" ]]; then
    verify
  fi

  if [[ "${TRIGGER}" == "true" ]]; then
    log "Done. unbounded-nightly is provisioned and the first deploy was triggered."
    info "Subsequent runs deploy automatically every morning at 06:00 UTC."
  else
    log "Done. unbounded-nightly is provisioned (deploy not triggered)."
  fi
}

main "$@"
