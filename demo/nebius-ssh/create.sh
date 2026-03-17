#!/usr/bin/env bash
#
# Provision Nebius cloud resources for the unbounded project.
#
# Subcommands:
#   network               Create private IP pool, VPC network + subnet (172.20.0.0/16)
#   instance <name> <ssh-public-key-path>
#                         Create boot disk + CPU VM with public IP and SSH key
#   clean network         Delete subnet, network, and pool
#   clean instance <name> Delete VM instance and its boot disk
#
# Usage:
#   ./create.sh network
#   ./create.sh instance my-machine ~/.ssh/id_rsa.pub
#   ./create.sh clean instance my-machine
#   ./create.sh clean network
#
# Prerequisites:
#   - nebius CLI installed and authenticated (`nebius config list` should show a valid profile)
#   - jq installed
#
set -euo pipefail

###############################################################################
# Configuration — edit these as needed
###############################################################################
POOL_NAME="nebius-unbounded-pool"
NETWORK_NAME="nebius-unbounded-network"
SUBNET_NAME="nebius-unbounded-subnet"
SUBNET_CIDR="172.20.0.0/16"
DISK_SIZE_GB=64
DISK_TYPE="network_ssd"
IMAGE_FAMILY="ubuntu22.04-driverless"
PLATFORM="cpu-d3"
PRESET="4vcpu-16gb"
SSH_USER="ubuntu"

###############################################################################
# Helpers
###############################################################################
log()  { printf "\n==> %s\n" "$*"; }
die()  { printf "ERROR: %s\n" "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "'$1' is required but not found in PATH"; }

usage() {
  cat <<EOF
Usage:
  $0 network
  $0 instance <name> <ssh-public-key-path>
  $0 clean instance <name>
  $0 clean network

Subcommands:
  network          Create private IP pool, VPC network, and subnet ($SUBNET_CIDR)
  instance         Create a boot disk and CPU VM with public IP and SSH key
  clean instance   Delete a VM instance and its boot disk by name
  clean network    Delete the subnet, network, and pool

Examples:
  $0 network
  $0 instance my-machine ~/.ssh/id_rsa.pub
  $0 clean instance my-machine
  $0 clean network
EOF
  exit 1
}

###############################################################################
# Common setup
###############################################################################
need nebius
need jq

PARENT_ID=$(nebius config get parent-id 2>/dev/null) \
  || die "Could not determine parent-id from nebius config. Run 'nebius config list' to check."

###############################################################################
# Subcommand: network
###############################################################################
cmd_network() {
  log "Using parent-id: $PARENT_ID"

  # 1. Create private IP pool with the desired CIDR
  log "Creating private IP pool '$POOL_NAME' (CIDR $SUBNET_CIDR) ..."

  POOL_JSON=$(nebius vpc pool create \
    --name "$POOL_NAME" \
    --parent-id "$PARENT_ID" \
    --version ipv4 \
    --visibility private \
    --cidrs "[{\"cidr\": \"$SUBNET_CIDR\"}]" \
    --format json)

  POOL_ID=$(echo "$POOL_JSON" | jq -r '.metadata.id')
  echo "  Pool ID: $POOL_ID"

  # 2. Create VPC network using the custom pool
  log "Creating VPC network '$NETWORK_NAME' ..."

  NETWORK_JSON=$(nebius vpc network create \
    --name "$NETWORK_NAME" \
    --parent-id "$PARENT_ID" \
    --ipv4-private-pools-pools "[{\"id\": \"$POOL_ID\"}]" \
    --format json)

  NETWORK_ID=$(echo "$NETWORK_JSON" | jq -r '.metadata.id')
  echo "  Network ID: $NETWORK_ID"

  # 3. Create subnet using the network's pool
  log "Creating subnet '$SUBNET_NAME' (CIDR $SUBNET_CIDR) in network $NETWORK_ID ..."

  SUBNET_JSON=$(nebius vpc subnet create \
    --name "$SUBNET_NAME" \
    --parent-id "$PARENT_ID" \
    --network-id "$NETWORK_ID" \
    --format json)

  SUBNET_ID=$(echo "$SUBNET_JSON" | jq -r '.metadata.id')
  echo "  Subnet ID: $SUBNET_ID"

  log "Network resources created:"
  echo "  Pool:    $POOL_ID  ($POOL_NAME, $SUBNET_CIDR)"
  echo "  Network: $NETWORK_ID  ($NETWORK_NAME)"
  echo "  Subnet:  $SUBNET_ID  ($SUBNET_NAME)"
}

###############################################################################
# Subcommand: instance
###############################################################################
cmd_instance() {
  local vm_name="$1"
  local ssh_key_path="$2"

  [[ -f "$ssh_key_path" ]] || die "SSH public key file not found: $ssh_key_path"
  local ssh_key
  ssh_key=$(cat "$ssh_key_path")

  local disk_name="${vm_name}-boot-disk"

  log "Using parent-id: $PARENT_ID"

  # Look up the subnet by name to get its ID.
  log "Looking up subnet '$SUBNET_NAME' ..."

  SUBNET_ID=$(nebius vpc subnet get-by-name \
    --parent-id "$PARENT_ID" \
    --name "$SUBNET_NAME" \
    --format json | jq -r '.metadata.id')

  [[ -n "$SUBNET_ID" && "$SUBNET_ID" != "null" ]] \
    || die "Subnet '$SUBNET_NAME' not found. Run '$0 network' first."
  echo "  Subnet ID: $SUBNET_ID"

  # 1. Create boot disk
  log "Creating boot disk '$disk_name' (${DISK_SIZE_GB} GiB, $DISK_TYPE, image family $IMAGE_FAMILY) ..."

  DISK_JSON=$(nebius compute disk create \
    --name "$disk_name" \
    --parent-id "$PARENT_ID" \
    --type "$DISK_TYPE" \
    --size-gibibytes "$DISK_SIZE_GB" \
    --source-image-family-image-family "$IMAGE_FAMILY" \
    --format json)

  DISK_ID=$(echo "$DISK_JSON" | jq -r '.metadata.id')
  echo "  Disk ID: $DISK_ID"

  # 2. Create CPU VM
  log "Creating VM '$vm_name' (platform=$PLATFORM, preset=$PRESET) ..."

  local cloud_init
  cloud_init=$(cat <<EOF
#cloud-config
users:
  - name: ${SSH_USER}
    groups: sudo
    shell: /bin/bash
    sudo: ALL=(ALL) NOPASSWD:ALL
    ssh_authorized_keys:
      - ${ssh_key}
EOF
  )

  INSTANCE_JSON=$(nebius compute instance create \
    --name "$vm_name" \
    --parent-id "$PARENT_ID" \
    --resources-platform "$PLATFORM" \
    --resources-preset "$PRESET" \
    --boot-disk-attach-mode read_write \
    --boot-disk-existing-disk-id "$DISK_ID" \
    --network-interfaces "[{\"name\": \"eth0\", \"subnet_id\": \"$SUBNET_ID\", \"ip_address\": {}, \"public_ip_address\": {}}]" \
    --cloud-init-user-data "$cloud_init" \
    --format json)

  INSTANCE_ID=$(echo "$INSTANCE_JSON" | jq -r '.metadata.id')
  echo "  Instance ID: $INSTANCE_ID"

  # 3. Print summary
  local public_ip
  public_ip=$(echo "$INSTANCE_JSON" | jq -r '
    .status.network_interfaces[]?
    | .public_ip_address.address // empty' | head -n1)

  log "Instance resources created:"
  echo "  Disk:     $DISK_ID  ($disk_name)"
  echo "  Instance: $INSTANCE_ID  ($vm_name)"
  if [[ -n "${public_ip:-}" ]]; then
    echo ""
    echo "  Public IP: $public_ip"
    echo "  SSH:       ssh ${SSH_USER}@${public_ip}"
  else
    echo ""
    echo "  Public IP not yet assigned. Check with:"
    echo "    nebius compute instance get --id $INSTANCE_ID --format json | jq '.status.network_interfaces[].public_ip_address'"
  fi
}

###############################################################################
# Subcommand: clean instance <name>
###############################################################################
cmd_clean_instance() {
  local vm_name="$1"
  local disk_name="${vm_name}-boot-disk"

  log "Using parent-id: $PARENT_ID"

  # 1. Look up and delete the VM instance
  log "Looking up instance '$vm_name' ..."

  local instance_id
  instance_id=$(nebius compute instance get-by-name \
    --parent-id "$PARENT_ID" \
    --name "$vm_name" \
    --format json 2>/dev/null | jq -r '.metadata.id') || true

  if [[ -n "$instance_id" && "$instance_id" != "null" ]]; then
    log "Deleting instance '$vm_name' ($instance_id) ..."
    nebius compute instance delete --id "$instance_id"
    echo "  Deleted instance: $instance_id"
  else
    echo "  Instance '$vm_name' not found, skipping."
  fi

  # 2. Look up and delete the boot disk
  log "Looking up disk '$disk_name' ..."

  local disk_id
  disk_id=$(nebius compute disk get-by-name \
    --parent-id "$PARENT_ID" \
    --name "$disk_name" \
    --format json 2>/dev/null | jq -r '.metadata.id') || true

  if [[ -n "$disk_id" && "$disk_id" != "null" ]]; then
    log "Deleting disk '$disk_name' ($disk_id) ..."
    nebius compute disk delete --id "$disk_id"
    echo "  Deleted disk: $disk_id"
  else
    echo "  Disk '$disk_name' not found, skipping."
  fi

  log "Instance cleanup complete."
}

###############################################################################
# Subcommand: clean network
###############################################################################
cmd_clean_network() {
  log "Using parent-id: $PARENT_ID"

  # 1. Delete subnet
  log "Looking up subnet '$SUBNET_NAME' ..."

  local subnet_id
  subnet_id=$(nebius vpc subnet get-by-name \
    --parent-id "$PARENT_ID" \
    --name "$SUBNET_NAME" \
    --format json 2>/dev/null | jq -r '.metadata.id') || true

  if [[ -n "$subnet_id" && "$subnet_id" != "null" ]]; then
    log "Deleting subnet '$SUBNET_NAME' ($subnet_id) ..."
    nebius vpc subnet delete --id "$subnet_id"
    echo "  Deleted subnet: $subnet_id"
  else
    echo "  Subnet '$SUBNET_NAME' not found, skipping."
  fi

  # 2. Delete network
  log "Looking up network '$NETWORK_NAME' ..."

  local network_id
  network_id=$(nebius vpc network get-by-name \
    --parent-id "$PARENT_ID" \
    --name "$NETWORK_NAME" \
    --format json 2>/dev/null | jq -r '.metadata.id') || true

  if [[ -n "$network_id" && "$network_id" != "null" ]]; then
    log "Deleting network '$NETWORK_NAME' ($network_id) ..."
    nebius vpc network delete --id "$network_id"
    echo "  Deleted network: $network_id"
  else
    echo "  Network '$NETWORK_NAME' not found, skipping."
  fi

  # 3. Delete pool
  log "Looking up pool '$POOL_NAME' ..."

  local pool_id
  pool_id=$(nebius vpc pool get-by-name \
    --parent-id "$PARENT_ID" \
    --name "$POOL_NAME" \
    --format json 2>/dev/null | jq -r '.metadata.id') || true

  if [[ -n "$pool_id" && "$pool_id" != "null" ]]; then
    log "Deleting pool '$POOL_NAME' ($pool_id) ..."
    nebius vpc pool delete --id "$pool_id"
    echo "  Deleted pool: $pool_id"
  else
    echo "  Pool '$POOL_NAME' not found, skipping."
  fi

  log "Network cleanup complete."
}

###############################################################################
# Main dispatch
###############################################################################
[[ $# -ge 1 ]] || usage

case "$1" in
  network)
    cmd_network
    ;;
  instance)
    [[ $# -ge 3 ]] || die "Usage: $0 instance <name> <ssh-public-key-path>"
    cmd_instance "$2" "$3"
    ;;
  clean)
    [[ $# -ge 2 ]] || die "Usage: $0 clean <network|instance <name>>"
    case "$2" in
      network)
        cmd_clean_network
        ;;
      instance)
        [[ $# -ge 3 ]] || die "Usage: $0 clean instance <name>"
        cmd_clean_instance "$3"
        ;;
      *)
        die "Unknown clean target: $2. Use 'network' or 'instance'."
        ;;
    esac
    ;;
  *)
    die "Unknown subcommand: $1. Use 'network', 'instance', or 'clean'."
    ;;
esac
