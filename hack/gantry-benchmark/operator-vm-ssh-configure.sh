#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -Eeuo pipefail

: "${AZURE_SUBSCRIPTION_ID:?Set AZURE_SUBSCRIPTION_ID}"
: "${AZURE_RESOURCE_GROUP:?Set AZURE_RESOURCE_GROUP}"

AZURE_LOCATION="${AZURE_LOCATION:-canadacentral}"
OPERATOR_VM_NAME="${OPERATOR_VM_NAME:-gantry-benchmark-operator}"
OPERATOR_VM_ZONE="${OPERATOR_VM_ZONE:-1}"
OPERATOR_NSG_NAME="${OPERATOR_NSG_NAME:-gantry-benchmark-operator-nsg}"
OPERATOR_SSH_PORT="${OPERATOR_SSH_PORT:-50001}"
OPERATOR_SSH_PUBLIC_IP_NAME="${OPERATOR_SSH_PUBLIC_IP_NAME:-gantry-benchmark-operator-ssh}"
OPERATOR_SSH_NSG_RULE_NAME="${OPERATOR_SSH_NSG_RULE_NAME:-allow-operator-ssh-50001}"
OPERATOR_SSH_SOURCE_CIDR="${OPERATOR_SSH_SOURCE_CIDR:-}"
OPERATOR_SSH_HOST_ALIAS="${OPERATOR_SSH_HOST_ALIAS:-$OPERATOR_VM_NAME}"

repo_root=$(git rev-parse --show-toplevel)
sshd_script="$repo_root/hack/gantry-benchmark/operator-vm-configure-sshd.sh"
key_path="$repo_root/tmp/gantry-benchmark-operator-key"
ssh_state_dir="${OPERATOR_SSH_STATE_DIR:-$repo_root/tmp/$AZURE_RESOURCE_GROUP}"
ssh_config_path="${OPERATOR_SSH_CONFIG_PATH:-$ssh_state_dir/ssh-config}"
ssh_known_hosts_path="${OPERATOR_SSH_KNOWN_HOSTS_PATH:-$ssh_state_dir/ssh-known-hosts}"

[[ "$OPERATOR_SSH_PORT" == 50001 ]] || {
  echo "OPERATOR_SSH_PORT=$OPERATOR_SSH_PORT is unsupported; the operator contract requires 50001" >&2
  exit 2
}
[[ "$OPERATOR_SSH_HOST_ALIAS" =~ ^[A-Za-z0-9._-]+$ ]] || {
  echo "OPERATOR_SSH_HOST_ALIAS contains unsupported characters" >&2
  exit 2
}
for command in az curl ssh ssh-keygen ssh-keyscan; do
  command -v "$command" >/dev/null 2>&1 || { echo "required command not found: $command" >&2; exit 1; }
done
[[ -x "$sshd_script" ]] || { echo "missing executable sshd configurator: $sshd_script" >&2; exit 1; }

valid_ipv4() {
  local address=$1
  local octet
  local -a octets
  IFS=. read -r -a octets <<<"$address"
  ((${#octets[@]} == 4)) || return 1
  for octet in "${octets[@]}"; do
    [[ "$octet" =~ ^[0-9]{1,3}$ ]] && ((10#$octet <= 255)) || return 1
  done
}

if [[ -z "$OPERATOR_SSH_SOURCE_CIDR" ]]; then
  workstation_public_ip=$(curl -4 -fsS --max-time 20 https://api.ipify.org)
  valid_ipv4 "$workstation_public_ip" || {
    echo "could not determine the workstation public IPv4 address" >&2
    exit 1
  }
  OPERATOR_SSH_SOURCE_CIDR="$workstation_public_ip/32"
else
  [[ "$OPERATOR_SSH_SOURCE_CIDR" =~ ^([^/]+)/([0-9]|[12][0-9]|3[0-2])$ ]] && \
    valid_ipv4 "${BASH_REMATCH[1]}" || {
    echo "OPERATOR_SSH_SOURCE_CIDR must be an IPv4 CIDR" >&2
    exit 2
  }
fi

az account set --subscription "$AZURE_SUBSCRIPTION_ID"
az vm show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_VM_NAME" --output none

if ! az network nsg show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_NSG_NAME" --output none 2>/dev/null; then
  az network nsg create -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_NSG_NAME" \
    -l "$AZURE_LOCATION" --only-show-errors -o none
fi
ssh_rule_action=create
if az network nsg rule show -g "$AZURE_RESOURCE_GROUP" --nsg-name "$OPERATOR_NSG_NAME" \
  -n "$OPERATOR_SSH_NSG_RULE_NAME" --output none 2>/dev/null; then
  ssh_rule_action=update
fi
az network nsg rule "$ssh_rule_action" \
  -g "$AZURE_RESOURCE_GROUP" \
  --nsg-name "$OPERATOR_NSG_NAME" \
  -n "$OPERATOR_SSH_NSG_RULE_NAME" \
  --priority 100 \
  --direction Inbound \
  --access Allow \
  --protocol Tcp \
  --source-address-prefixes "$OPERATOR_SSH_SOURCE_CIDR" \
  --source-port-ranges '*' \
  --destination-address-prefixes '*' \
  --destination-port-ranges "$OPERATOR_SSH_PORT" \
  --only-show-errors \
  -o none

if ! az network public-ip show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_SSH_PUBLIC_IP_NAME" --output none 2>/dev/null; then
  az network public-ip create \
    -g "$AZURE_RESOURCE_GROUP" \
    -n "$OPERATOR_SSH_PUBLIC_IP_NAME" \
    -l "$AZURE_LOCATION" \
    --sku Standard \
    --allocation-method Static \
    --zone "$OPERATOR_VM_ZONE" \
    --only-show-errors \
    -o none
fi
[[ $(az network public-ip show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_SSH_PUBLIC_IP_NAME" --query sku.name -o tsv) == Standard ]] || {
  echo "operator SSH public IP must use the Standard SKU" >&2
  exit 1
}
[[ $(az network public-ip show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_SSH_PUBLIC_IP_NAME" --query publicIPAllocationMethod -o tsv) == Static ]] || {
  echo "operator SSH public IP must use static allocation" >&2
  exit 1
}
ssh_public_ip_id=$(az network public-ip show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_SSH_PUBLIC_IP_NAME" --query id -o tsv)

install -d -m 0700 "$(dirname "$key_path")"
if [[ ! -f "$key_path.pub" ]]; then
  ssh-keygen -q -t ed25519 -N '' -f "$key_path"
fi
az vm user update \
  -g "$AZURE_RESOURCE_GROUP" \
  -n "$OPERATOR_VM_NAME" \
  --username benchmark \
  --ssh-key-value "$key_path.pub" \
  --only-show-errors \
  -o none

operator_nic_id=$(az vm show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_VM_NAME" --query 'networkProfile.networkInterfaces[0].id' -o tsv)
operator_nic_name=${operator_nic_id##*/}
operator_ip_config_name=$(az network nic show --ids "$operator_nic_id" --query 'ipConfigurations[0].name' -o tsv)
attached_public_ip_id=$(az network nic show --ids "$operator_nic_id" --query 'ipConfigurations[0].publicIPAddress.id' -o tsv)
if [[ -z "$attached_public_ip_id" || ! "$attached_public_ip_id" =~ /$OPERATOR_SSH_PUBLIC_IP_NAME$ ]]; then
  az network nic ip-config update \
    -g "$AZURE_RESOURCE_GROUP" \
    --nic-name "$operator_nic_name" \
    -n "$operator_ip_config_name" \
    --public-ip-address "$ssh_public_ip_id" \
    --only-show-errors \
    -o none
fi

ssh_public_ip=""
for attempt in $(seq 1 60); do
  ssh_public_ip=$(az network public-ip show -g "$AZURE_RESOURCE_GROUP" -n "$OPERATOR_SSH_PUBLIC_IP_NAME" --query ipAddress -o tsv)
  [[ -z "$ssh_public_ip" ]] || break
  sleep 5
done
[[ -n "$ssh_public_ip" ]] || {
  echo "operator SSH public IP address was not allocated" >&2
  exit 1
}

az vm run-command invoke \
  -g "$AZURE_RESOURCE_GROUP" \
  -n "$OPERATOR_VM_NAME" \
  --command-id RunShellScript \
  --scripts @"$sshd_script" \
  --only-show-errors \
  -o none

install -d -m 0700 "$ssh_state_dir"
known_hosts_temp="$ssh_known_hosts_path.tmp"
ssh_key_scanned=false
for attempt in $(seq 1 60); do
  if ssh-keyscan -T 10 -p "$OPERATOR_SSH_PORT" "$ssh_public_ip" >"$known_hosts_temp" 2>/dev/null && \
    [[ -s "$known_hosts_temp" ]]; then
    mv "$known_hosts_temp" "$ssh_known_hosts_path"
    ssh_key_scanned=true
    break
  fi
  sleep 5
done
rm -f "$known_hosts_temp"
[[ "$ssh_key_scanned" == true ]] || {
  echo "could not read the operator SSH host key from $ssh_public_ip:$OPERATOR_SSH_PORT" >&2
  exit 1
}
chmod 0600 "$ssh_known_hosts_path"

cat >"$ssh_config_path" <<SSH_CONFIG
Host $OPERATOR_SSH_HOST_ALIAS
  HostName $ssh_public_ip
  Port $OPERATOR_SSH_PORT
  User benchmark
  IdentityFile $key_path
  IdentitiesOnly yes
  PasswordAuthentication no
  ControlMaster auto
  ControlPersist 10m
  ControlPath /tmp/gantry-benchmark-ssh-$UID-%C
  ServerAliveInterval 30
  StrictHostKeyChecking yes
  UserKnownHostsFile $ssh_known_hosts_path
SSH_CONFIG
chmod 0600 "$ssh_config_path"

resolved_ssh_config=$(ssh -G -F "$ssh_config_path" "$OPERATOR_SSH_HOST_ALIAS")
grep -qx "hostname $ssh_public_ip" <<<"$resolved_ssh_config"
grep -qx "port $OPERATOR_SSH_PORT" <<<"$resolved_ssh_config"
grep -qx 'user benchmark' <<<"$resolved_ssh_config"
grep -Fqx "identityfile $key_path" <<<"$resolved_ssh_config"
ssh -F "$ssh_config_path" -o BatchMode=yes -o ConnectTimeout=20 "$OPERATOR_SSH_HOST_ALIAS" true

cat <<SUMMARY
operator SSH endpoint: $ssh_public_ip:$OPERATOR_SSH_PORT
operator SSH source: $OPERATOR_SSH_SOURCE_CIDR
operator SSH config: $ssh_config_path
operator SSH command: ssh -F $ssh_config_path $OPERATOR_SSH_HOST_ALIAS
SUMMARY