#!/usr/bin/env bash
# Configure a GitHub Environment used by the release-upgrade workflow to
# deploy a published Unbounded release to a Kubernetes cluster.
#
# This script only configures the GitHub side: it creates (or updates) the
# Environment, stores the kubeconfig as the KUBECONFIG secret, and sets the
# cluster-config variables consumed by the workflow. It does NOT modify the
# workflow file, label cluster nodes, or trigger a deploy.
#
# Usage:
#   hack/scripts/setup-deploy-environment.sh \
#     --env-name unbounded-stable \
#     --kubeconfig /path/to/kubeconfig \
#     --site-name stable \
#     --cluster-node-cidr 10.224.0.0/12 \
#     --cluster-pod-cidr  100.124.0.0/16 \
#     --site-node-cidr    10.1.0.0/16 \
#     --site-pod-cidr     100.125.0.0/16 \
#     [--manage-cni-plugin true] \
#     [--repo Azure/unbounded] \
#     [--yes]

set -euo pipefail
IFS=$'\n\t'

REPO="Azure/unbounded"
MANAGE_CNI_PLUGIN="true"
ASSUME_YES="false"

ENV_NAME=""
KUBECONFIG_PATH=""
SITE_NAME=""
CLUSTER_NODE_CIDR=""
CLUSTER_POD_CIDR=""
SITE_NODE_CIDR=""
SITE_POD_CIDR=""

# Optional Orca integration-deploy config. When account + container are
# provided, the deploy-orca job in release-upgrade.yaml deploys Orca with
# a real Azure Blob origin. These are non-confidential; the Azure account
# KEY and Garage S3 keys live in the cluster's pre-created
# orca-credentials Secret, not here.
ORCA_AZURE_ACCOUNT=""
ORCA_AZURE_CONTAINER=""
ORCA_AZURE_ENDPOINT=""

usage() {
    cat <<'EOF'
Configure a GitHub Environment for release-upgrade deploys.

Required:
  --env-name NAME            GitHub Environment name (e.g. unbounded-stable)
  --kubeconfig PATH          Path to the kubeconfig file for the target cluster
  --site-name NAME           Site name passed to 'kubectl unbounded site init'
  --cluster-node-cidr CIDR   Cluster node CIDR
  --cluster-pod-cidr CIDR    Cluster pod CIDR
  --site-node-cidr CIDR      Site node CIDR
  --site-pod-cidr CIDR       Site pod CIDR

Optional:
  --manage-cni-plugin BOOL   Whether unbounded manages the CNI (true|false). Default: true
  --orca-azure-account NAME  Azure storage account for the Orca origin (enables Orca deploy)
  --orca-azure-container NAME Azure blob container for the Orca origin
  --orca-azure-endpoint URL  Azure blob endpoint (optional; blank => *.blob.core.windows.net)
  --repo OWNER/NAME          Target repository. Default: Azure/unbounded
  --yes                      Skip the confirmation prompt
  --help                     Show this help

Exit codes:
  0 success
  1 usage or validation error
  2 gh not authenticated
  3 GitHub API call failed
EOF
}

die() {
    echo "error: $*" >&2
    exit 1
}

require_value() {
    # require_value <flag> <value>
    if [[ -z "${2:-}" || "${2:0:2}" == "--" ]]; then
        die "flag $1 requires a value"
    fi
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --env-name)            require_value "$1" "${2:-}"; ENV_NAME="$2"; shift 2 ;;
        --kubeconfig)          require_value "$1" "${2:-}"; KUBECONFIG_PATH="$2"; shift 2 ;;
        --site-name)           require_value "$1" "${2:-}"; SITE_NAME="$2"; shift 2 ;;
        --cluster-node-cidr)   require_value "$1" "${2:-}"; CLUSTER_NODE_CIDR="$2"; shift 2 ;;
        --cluster-pod-cidr)    require_value "$1" "${2:-}"; CLUSTER_POD_CIDR="$2"; shift 2 ;;
        --site-node-cidr)      require_value "$1" "${2:-}"; SITE_NODE_CIDR="$2"; shift 2 ;;
        --site-pod-cidr)       require_value "$1" "${2:-}"; SITE_POD_CIDR="$2"; shift 2 ;;
        --manage-cni-plugin)   require_value "$1" "${2:-}"; MANAGE_CNI_PLUGIN="$2"; shift 2 ;;
        --orca-azure-account)  require_value "$1" "${2:-}"; ORCA_AZURE_ACCOUNT="$2"; shift 2 ;;
        --orca-azure-container) require_value "$1" "${2:-}"; ORCA_AZURE_CONTAINER="$2"; shift 2 ;;
        --orca-azure-endpoint) require_value "$1" "${2:-}"; ORCA_AZURE_ENDPOINT="$2"; shift 2 ;;
        --repo)                require_value "$1" "${2:-}"; REPO="$2"; shift 2 ;;
        --yes)                 ASSUME_YES="true"; shift ;;
        --help|-h)             usage; exit 0 ;;
        *)                     die "unknown argument: $1 (try --help)" ;;
    esac
done

# Required-flag checks.
[[ -n "$ENV_NAME"          ]] || die "--env-name is required"
[[ -n "$KUBECONFIG_PATH"   ]] || die "--kubeconfig is required"
[[ -n "$SITE_NAME"         ]] || die "--site-name is required"
[[ -n "$CLUSTER_NODE_CIDR" ]] || die "--cluster-node-cidr is required"
[[ -n "$CLUSTER_POD_CIDR"  ]] || die "--cluster-pod-cidr is required"
[[ -n "$SITE_NODE_CIDR"    ]] || die "--site-node-cidr is required"
[[ -n "$SITE_POD_CIDR"     ]] || die "--site-pod-cidr is required"

# Validate CIDRs.
CIDR_RE='^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$'
for pair in \
    "cluster-node-cidr=$CLUSTER_NODE_CIDR" \
    "cluster-pod-cidr=$CLUSTER_POD_CIDR" \
    "site-node-cidr=$SITE_NODE_CIDR" \
    "site-pod-cidr=$SITE_POD_CIDR"; do
    name="${pair%%=*}"
    value="${pair#*=}"
    if [[ ! "$value" =~ $CIDR_RE ]]; then
        die "--$name value '$value' is not a valid IPv4 CIDR"
    fi
done

# Validate manage-cni-plugin.
case "$MANAGE_CNI_PLUGIN" in
    true|false) ;;
    *) die "--manage-cni-plugin must be 'true' or 'false', got '$MANAGE_CNI_PLUGIN'" ;;
esac

# Validate Orca config: account and container go together (endpoint is
# optional). If neither is set, the Orca deploy job is left unconfigured.
if [[ -n "$ORCA_AZURE_ACCOUNT" && -z "$ORCA_AZURE_CONTAINER" ]]; then
    die "--orca-azure-account requires --orca-azure-container"
fi
if [[ -z "$ORCA_AZURE_ACCOUNT" && -n "$ORCA_AZURE_CONTAINER" ]]; then
    die "--orca-azure-container requires --orca-azure-account"
fi

# Validate kubeconfig file.
[[ -f "$KUBECONFIG_PATH" ]] || die "kubeconfig file '$KUBECONFIG_PATH' does not exist"
[[ -r "$KUBECONFIG_PATH" ]] || die "kubeconfig file '$KUBECONFIG_PATH' is not readable"
[[ -s "$KUBECONFIG_PATH" ]] || die "kubeconfig file '$KUBECONFIG_PATH' is empty"

# Validate repo format.
if [[ ! "$REPO" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]]; then
    die "--repo value '$REPO' is not in OWNER/NAME format"
fi

# Check gh availability and authentication.
command -v gh >/dev/null 2>&1 || die "'gh' CLI not found on PATH; install from https://cli.github.com/"
if ! gh auth status >/dev/null 2>&1; then
    echo "error: 'gh' is not authenticated; run 'gh auth login' first" >&2
    exit 2
fi

# Print summary and confirm.
cat <<EOF

About to configure GitHub Environment:

  Repository:           $REPO
  Environment:          $ENV_NAME
  Kubeconfig source:    $KUBECONFIG_PATH

  Variables:
    SITE_NAME           $SITE_NAME
    CLUSTER_NODE_CIDR   $CLUSTER_NODE_CIDR
    CLUSTER_POD_CIDR    $CLUSTER_POD_CIDR
    SITE_NODE_CIDR      $SITE_NODE_CIDR
    SITE_POD_CIDR       $SITE_POD_CIDR
    MANAGE_CNI_PLUGIN   $MANAGE_CNI_PLUGIN
    ORCA_AZURE_ACCOUNT  ${ORCA_AZURE_ACCOUNT:-(unset; Orca deploy disabled)}
    ORCA_AZURE_CONTAINER ${ORCA_AZURE_CONTAINER:-(unset)}
    ORCA_AZURE_ENDPOINT ${ORCA_AZURE_ENDPOINT:-(unset; uses *.blob.core.windows.net)}

  Secret:
    KUBECONFIG          (contents of $KUBECONFIG_PATH)

EOF

if [[ "$ASSUME_YES" != "true" ]]; then
    read -r -p "Proceed? [y/N] " reply
    case "$reply" in
        y|Y|yes|YES) ;;
        *) echo "aborted"; exit 1 ;;
    esac
fi

# Create or update the environment (no required reviewers, fully automatic).
echo
echo "==> Creating/updating environment $ENV_NAME in $REPO"
if ! gh api --method PUT \
        -H "Accept: application/vnd.github+json" \
        "/repos/${REPO}/environments/${ENV_NAME}" \
        --input - <<'JSON' >/dev/null
{
  "wait_timer": 0,
  "prevent_self_review": false,
  "reviewers": [],
  "deployment_branch_policy": null
}
JSON
then
    echo "error: failed to create/update environment" >&2
    exit 3
fi

# Set the KUBECONFIG secret via stdin (avoids leaking via process args).
echo "==> Setting secret KUBECONFIG"
if ! gh secret set KUBECONFIG \
        --repo "$REPO" \
        --env  "$ENV_NAME" \
        < "$KUBECONFIG_PATH"; then
    echo "error: failed to set KUBECONFIG secret" >&2
    exit 3
fi

# Set variables (gh variable set is idempotent; updates if existing).
set_var() {
    local name="$1"
    local value="$2"
    echo "==> Setting variable $name"
    # Pipe the value via stdin instead of --body: `gh variable set --body ""`
    # treats an empty value as "no value" on a TTY and prompts interactively
    # ("Paste your variable"), which would hang non-interactive callers. Stdin
    # is never a TTY here, so an empty value is stored without prompting.
    if ! printf '%s' "$value" | gh variable set "$name" \
            --repo "$REPO" \
            --env  "$ENV_NAME"; then
        echo "error: failed to set variable $name" >&2
        exit 3
    fi
}

set_var SITE_NAME          "$SITE_NAME"
set_var CLUSTER_NODE_CIDR  "$CLUSTER_NODE_CIDR"
set_var CLUSTER_POD_CIDR   "$CLUSTER_POD_CIDR"
set_var SITE_NODE_CIDR     "$SITE_NODE_CIDR"
set_var SITE_POD_CIDR      "$SITE_POD_CIDR"
set_var MANAGE_CNI_PLUGIN  "$MANAGE_CNI_PLUGIN"

# Orca deploy vars (only when configured). The deploy-orca job is gated
# on ORCA_AZURE_ACCOUNT / ORCA_AZURE_CONTAINER being present.
if [[ -n "$ORCA_AZURE_ACCOUNT" ]]; then
    set_var ORCA_AZURE_ACCOUNT   "$ORCA_AZURE_ACCOUNT"
    set_var ORCA_AZURE_CONTAINER "$ORCA_AZURE_CONTAINER"
    set_var ORCA_AZURE_ENDPOINT  "$ORCA_AZURE_ENDPOINT"
fi

# Verify and summarize.
echo
echo "==> Configured secrets:"
gh secret list --repo "$REPO" --env "$ENV_NAME"
echo
echo "==> Configured variables:"
gh variable list --repo "$REPO" --env "$ENV_NAME"

cat <<EOF

Environment $ENV_NAME configured.

Next steps:

  1. Label at least one node as a gateway:
       kubectl label node <node-name> \\
         unbounded-cloud.io/unbounded-net-gateway=true --overwrite

  2. Trigger the first install (run once per cluster):
       gh workflow run release-upgrade.yaml \\
         --repo $REPO \\
         -f tag=vX.Y.Z \\
         -f force_init=true

  3. Subsequent published releases will deploy automatically to $ENV_NAME.
EOF
