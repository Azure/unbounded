#! /bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#
# remove-nebius-site.sh - Delete a nebius site with a matching prefix
#
# Usage:
#   ./remove-nebius-site.sh [options] PREFIX
#
# Required arguments:
#   -p, --prefix        Prefix to search in
#
# Optional arguments:
#   --compute-only      Only remove VMs, disks, allocations, and routes
#   --region            Region to look in (default eu-north1)
#   --parallel <#>      Number of parallel threads to use for VM/disk deletion
#
# usage-end

set -euo pipefail
shopt -s lastpipe

# Set default values
PREFIX="$(whoami)"
REGION="eu-north1"
REMOVE_VPC="true"
PARALLEL="4"

usage() {
  awk 'NR >=3 { if ($0 ~ /usage-end/) exit; print substr($0, 3)}' "$0"
}

die() {
  printf "Error: $1\n\n" >&2
  usage >&2
  exit 1
}

# ------------------------------------------------------------------
# Parse arguments
# ------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        -p|--prefix)         PREFIX="${2:-$(whoami)}"; shift 2 ;;
        -s|--site)           SITE="$2"; shift 2 ;;
        --region)            REGION="$2"; shift 2 ;;
        --compute-only)      REMOVE_VPC=""; shift ;;
        --parallel)          PARALLEL="$2"; shift 2;;
        --help)              usage; exit 0 ;;
        *)                   die "Unknown argument: $1" ;;
    esac
done

# Validate required arguments
[[ -n "$SITE" ]] || die "-s/--site is required"

PREFIX="${PREFIX}-${SITE}"

printf "Looking for resources starting with %s in region %s.\n\n" "$PREFIX" "$REGION"

ROUTE_TABLE=""

function kubectl-delete-commands {
  declare -n VAR="$1"
  declare KIND="$2"
  printf "Fetching ${KIND}s... "
  case "${KIND@L}" in 
    node)
      printf "%s\n" "${INSTANCES[@]}" \
        | awk -v PREFIX="$PREFIX" '$0 ~ PREFIX {print "delete node " $NF}' \
        | while IFS= read -r LINE; do
          VAR+=("$LINE")
        done;;
    site)
      kubectl get site "$SITE" -o name &>/dev/null && VAR+=("delete site ${SITE}");;
    sitegatewaypoolassignment|sitepeering|gatewaypool)
      kubectl get "${KIND@L}" -o json \
        | jq --arg kind "$KIND" --arg site "$SITE" -r '.items | map(select(.spec.sites | any(contains($site))) | ("delete " + $kind + " " + .metadata.name))[]' \
        | while IFS= read -r LINE; do VAR+=("$LINE"); done
  esac
  printf "done.\n"
}

function nebius-delete-commands {
  declare -n VAR="$1"
  GROUP="$2"
  OBJECT="$3"
  PARENT="${4:-}"

  printf "Fetching %ss%s... " "$2 $3" "${PARENT:+ (parent: $PARENT)}"
  if [[ -z "$PARENT" ]]; then
    nebius --no-browser "$GROUP" "$OBJECT" ${PARENT:+--parent-id $PARENT} list --all --format json \
      | jq -cr --arg PREFIX "$PREFIX" --arg COMMAND "$GROUP $OBJECT" '.items | map(select((.metadata.name | startswith($PREFIX)))) | map($COMMAND + " delete " + .metadata.id + " # " +.metadata.name + (" " + .spec.hostname)//"")[]'
  else
    nebius --no-browser "$GROUP" "$OBJECT" --parent-id $PARENT list --all --format json \
    | jq -cr --arg PREFIX "$PREFIX" --arg COMMAND "$GROUP $OBJECT" --arg PARENT "$PARENT" '.items | map($COMMAND + " delete " + .metadata.id + " # " + $PARENT + "/" +.metadata.name)[]'
  fi | while IFS= read -r LINE; do VAR+=("$LINE"); done
  printf "done.\n"
}

declare -a ALLOCATIONS=() CRS=() DISKS=() INSTANCES=() NODES=() ROUTES=() VPC=()
# These need to be generated in this order as they'll be deleted in the reverse order.
nebius-delete-commands VPC vpc pool
nebius-delete-commands VPC vpc network
nebius-delete-commands VPC vpc route-table
nebius-delete-commands VPC vpc subnet

ROUTE_TABLES="$(printf "%s\n" "${VPC[@]}" | awk '/vpc route-table/ {print $4}')"
if [[ ! -z "$ROUTE_TABLES" ]]; then
  for RT in "$ROUTE_TABLES"; do
    nebius-delete-commands ROUTES vpc route $RT
  done
fi

nebius-delete-commands ALLOCATIONS vpc allocation
nebius-delete-commands DISKS compute disk
nebius-delete-commands INSTANCES compute instance

kubectl cluster-info 2>/dev/null && {
  kubectl-delete-commands NODES node
  kubectl-delete-commands CRS site
  kubectl-delete-commands CRS sitegatewaypoolassignment
} || {
  printf "Cluster configuration not working, skipping k8s resources...\n"
}

# Remove default-egress route command if we're leaving the VPC
if [[ -z "$REMOVE_VPC" ]]; then
  VPC=()
  for i in "${!ROUTES[@]}"; do
    if [[ ${ROUTES[i]} =~ /default-egress$ ]]; then
      unset 'ROUTES[i]'
    fi
  done
fi

if [[ $((${#CRS[@]} + ${#NODES[@]} + ${#ROUTES[@]} + ${#ALLOCATIONS[@]} + ${#DISKS[@]} + ${#INSTANCES[@]} + ${#VPC[@]})) == 0 ]]; then
  printf "\nNo resources found!\n"
  exit 0
fi

printf "\nCommands to be executed:\n"
for i in CRS NODES INSTANCES DISKS ROUTES ALLOCATIONS; do
  declare -n VAR="$i"
  if [[ ${#VAR[@]} > 0 ]]; then
    if [[ "$i" == "CRS" || "$i" == "NODES" ]]; then
      printf -- "- kubectl %s\n" "${VAR[@]}"
    else
      printf -- "- nebius %s\n" "${VAR[@]}"
    fi
  fi
done
if [[ ! -z "$REMOVE_VPC" ]]; then
  for ((i = $((${#VPC[@]} - 1)); i >= 0; i--)); do
    printf -- "- nebius %s\n" "${VPC[$i]}"
  done
fi

printf "\nContinue (y/N)? "
while true; do
  read -n1 -rs ANSWER
  case "$ANSWER" in 
    [yY]        ) printf "$ANSWER\n"; break;;
    [nN]        ) printf "$ANSWER\nAborting.\n"; exit;;
    ""          ) printf "$ANSWER\nAborting.\n"; exit;;
    *           ) ;;
  esac
done

for i in NODES CRS INSTANCES DISKS ROUTES ALLOCATIONS; do
  declare -n VAR="$i"
  if [[ ${#VAR[@]} > 0 ]]; then
    printf "\nRemoving ${i@L} using ${PARALLEL} threads...\n"
    if [[ "$i" == "CRS" || "$i" == "NODES" ]]; then
      printf "%s\0" "${VAR[@]}" | xargs -r -0 -I{} -P${PARALLEL} bash -c 'kubectl {}' 2>&1 | sed -e "s/^/> /" || true
    else
      printf "%s\0" "${VAR[@]}" | xargs -r -0 -I{} -P${PARALLEL} bash -c 'nebius --no-browser --format json {}' 2>&1 | sed -e "s/^/> /" || true
    fi
  fi
done

if [[ ${#VPC[@]} > 0 ]]; then
  printf "\nRemoving VPC resources...\n"
  for ((i = $((${#VPC[@]} - 1)); i >= 0; i--)); do
    printf "\n%s:\n" "${VPC[$i]}"
    bash -c "nebius --format json ${VPC[$i]}" 2>&1 | sed -e "s/^/> /"
  done
fi

exit 0
