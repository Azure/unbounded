#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -eo pipefail

# -----------------------------------------------------------------
# Unbounded-kube node uninstall script
#
# This script fully reverses the bootstrap process, removing the
# nspawn machine, network interfaces, configuration files, and
# restoring the host to its original state. It is safe to run
# multiple times (idempotent).
#
# Machine: UNBOUNDED_MACHINE_NAME_PLACEHOLDER
# -----------------------------------------------------------------

MACHINE_NAME="UNBOUNDED_MACHINE_NAME_PLACEHOLDER"

if [ "$(id -u)" -ne 0 ]; then
  echo "Error: this script must be run as root (use sudo)" >&2
  exit 1
fi

echo "Uninstalling unbounded-kube node: ${MACHINE_NAME}"

# -----------------------------------------------------------------
# 1. Stop and remove the nspawn machine
# -----------------------------------------------------------------
echo "Stopping nspawn machine ${MACHINE_NAME}..."
if machinectl show "${MACHINE_NAME}" &>/dev/null; then
  machinectl stop "${MACHINE_NAME}" 2>/dev/null || true

  # Wait for the machine to fully stop (up to 30 seconds).
  for i in $(seq 1 30); do
    if ! machinectl show "${MACHINE_NAME}" &>/dev/null; then
      break
    fi
    sleep 1
  done

  # Force terminate if still running.
  if machinectl show "${MACHINE_NAME}" &>/dev/null; then
    echo "Machine did not stop gracefully, terminating..."
    machinectl terminate "${MACHINE_NAME}" 2>/dev/null || true
    sleep 2
  fi
fi

# -----------------------------------------------------------------
# 2. Remove network interfaces created by unbounded-net
#
# Since the nspawn container uses VirtualEthernet=no (shared host
# network namespace), all interfaces created inside the container
# are visible on the host and must be cleaned up here.
# -----------------------------------------------------------------
echo "Removing network interfaces..."

# Remove WireGuard interfaces (wg51820, wg51821, ...).
for iface in $(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | grep '^wg[0-9]'); do
  echo "  Removing interface: ${iface}"
  ip link delete "${iface}" 2>/dev/null || true
done

# Remove tunnel and overlay interfaces.
for iface in geneve0 vxlan0 ipip0 unbounded0 cbr0; do
  if ip link show "${iface}" &>/dev/null; then
    echo "  Removing interface: ${iface}"
    ip link delete "${iface}" 2>/dev/null || true
  fi
done

# -----------------------------------------------------------------
# 3. Remove WireGuard keys
# -----------------------------------------------------------------
echo "Removing WireGuard keys..."
rm -f /etc/wireguard/server.priv /etc/wireguard/server.pub

# -----------------------------------------------------------------
# 4. Remove nspawn configuration files
# -----------------------------------------------------------------
echo "Removing nspawn configuration..."
rm -f "/etc/systemd/nspawn/${MACHINE_NAME}.nspawn"
rm -rf "/etc/systemd/system/systemd-nspawn@${MACHINE_NAME}.service.d"

# -----------------------------------------------------------------
# 5. Remove the machine rootfs
# -----------------------------------------------------------------
echo "Removing machine rootfs /var/lib/machines/${MACHINE_NAME}..."
if command -v machinectl &>/dev/null && machinectl show "${MACHINE_NAME}" &>/dev/null; then
  machinectl remove "${MACHINE_NAME}" 2>/dev/null || true
fi
rm -rf "/var/lib/machines/${MACHINE_NAME}"

# -----------------------------------------------------------------
# 6. Remove nftables flush service and unbounded-kube config dir
# -----------------------------------------------------------------
echo "Removing nftables flush service..."
systemctl disable --now nftables-flush.service 2>/dev/null || true
rm -f /etc/systemd/system/nftables-flush.service
rm -rf /etc/unbounded-kube

# -----------------------------------------------------------------
# 7. Remove sysctl configuration and reload
# -----------------------------------------------------------------
echo "Removing Kubernetes sysctl configuration..."
rm -f /etc/sysctl.d/99-kubernetes.conf
sysctl --system &>/dev/null || true

# -----------------------------------------------------------------
# 8. Restore Docker
# -----------------------------------------------------------------
echo "Restoring Docker configuration..."
systemctl unmask docker.service 2>/dev/null || true
systemctl unmask docker.socket 2>/dev/null || true
rm -f /etc/docker/daemon.json

# -----------------------------------------------------------------
# 9. Restore swap
# -----------------------------------------------------------------
if [ -f /etc/fstab.bak ]; then
  echo "Restoring swap from /etc/fstab.bak..."
  cp /etc/fstab.bak /etc/fstab
  rm -f /etc/fstab.bak
  swapon -a 2>/dev/null || true
else
  echo "No /etc/fstab.bak found, skipping swap restore."
fi

# -----------------------------------------------------------------
# 10. Clean up any leftover routes
#
# Routes programmed by unbounded-net use the unbounded0, geneve0,
# wg*, and cbr0 devices. Deleting those interfaces in step 2
# automatically removes associated routes. Clean up any policy
# routing rules that may reference custom tables.
# -----------------------------------------------------------------
echo "Cleaning up policy routing rules..."
# Remove ip rules pointing to tables used by unbounded-net WireGuard
# gateways (tables numbered by WireGuard port: 51820-51899).
for table in $(seq 51820 51899); do
  while ip rule del table "${table}" 2>/dev/null; do :; done
done

# Flush those routing tables as well.
for table in $(seq 51820 51899); do
  ip route flush table "${table}" 2>/dev/null || true
done

# -----------------------------------------------------------------
# 11. Remove agent binaries and config artifacts
# -----------------------------------------------------------------
echo "Removing agent binaries and configuration..."
rm -f /usr/local/bin/unbounded-agent
rm -f /usr/local/bin/unbounded-agent-install.sh
rm -f /usr/local/bin/unbounded-agent-uninstall.sh
rm -rf /etc/unbounded-agent
rm -rf /tmp/unbounded-agent
rm -f /tmp/unbounded-agent-config.*.json

# -----------------------------------------------------------------
# 12. Reload systemd
# -----------------------------------------------------------------
echo "Reloading systemd daemon..."
systemctl daemon-reload

echo ""
echo "Unbounded-kube node ${MACHINE_NAME} has been fully reset."
echo ""
echo "NOTE: The Kubernetes Node object must be deleted separately from the"
echo "cluster, for example:"
echo "  kubectl delete node ${MACHINE_NAME}"
echo ""
echo "You may need to reboot the host to ensure all kernel modules and"
echo "network state are fully cleared."
