#!/bin/bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

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

# UNBOUNDED_UNINSTALL_ROOT is an optional path prefix for every file the script
# touches. It is empty on real hosts and exists so the script can be tested
# against a temporary directory.
ROOT="${UNBOUNDED_UNINSTALL_ROOT:-}"

if [ "$(id -u)" -ne 0 ]; then
  echo "Error: this script must be run as root (use sudo)" >&2
  exit 1
fi

echo "Uninstalling unbounded-kube node: ${MACHINE_NAME}"

# -----------------------------------------------------------------
# 0. Determine the isolation mode
#
# The mode is the "Isolation" key of the applied config of the active
# slot (kube1 or kube2). A host with no applied config, or a config
# that records no mode, was installed by an agent that only knew
# nspawn, so it takes the nspawn cleanup path unchanged.
# -----------------------------------------------------------------
ISOLATION_MODE=""
SLOT="${MACHINE_NAME}"
for candidate in "${MACHINE_NAME}" kube1 kube2; do
  applied="${ROOT}/etc/unbounded/agent/${candidate}-applied-config.json"
  if [ -f "${applied}" ]; then
    SLOT="${candidate}"
    ISOLATION_MODE="$(sed -n 's/.*"Isolation"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${applied}" | head -n 1)"
    break
  fi
done

case "${ISOLATION_MODE}" in
  "") ISOLATION_MODE="nspawn" ;;
  nspawn | none | container) ;;
  *)
    echo "Error: unsupported isolation mode \"${ISOLATION_MODE}\" recorded in the applied config" >&2
    exit 1
    ;;
esac

echo "Isolation mode: ${ISOLATION_MODE}"

# remove_unit stops, disables and deletes a unit from /etc/systemd/system.
remove_unit() {
  local unit="$1"
  systemctl disable --now "${unit}" 2>/dev/null || true
  rm -f "${ROOT}/etc/systemd/system/${unit}"
}

# --- nspawn ---------------------------------------------------------
stop_node_nspawn() {
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
}

remove_node_nspawn() {
  echo "Removing nspawn configuration..."
  rm -f "${ROOT}/etc/systemd/nspawn/${MACHINE_NAME}.nspawn"
  rm -rf "${ROOT}/etc/systemd/system/systemd-nspawn@${MACHINE_NAME}.service.d"

  echo "Removing machine rootfs ${ROOT}/var/lib/machines/${MACHINE_NAME}..."
  if command -v machinectl &>/dev/null && machinectl show "${MACHINE_NAME}" &>/dev/null; then
    machinectl remove "${MACHINE_NAME}" 2>/dev/null || true
  fi
  rm -rf "${ROOT}/var/lib/machines/${MACHINE_NAME}"
}

# --- none: node stack runs directly on the host ---------------------
stop_node_none() {
  echo "Stopping host node units..."
  systemctl stop unbounded-kubelet.service 2>/dev/null || true
  systemctl stop unbounded-containerd.service 2>/dev/null || true
  systemctl stop unbounded-localdns.service 2>/dev/null || true
}

remove_node_none() {
  echo "Removing host node units..."
  remove_unit unbounded-kubelet.service
  remove_unit unbounded-containerd.service
  remove_unit unbounded-localdns.service
  remove_unit unbounded-localdns-network.service
  # Drop-ins the backend writes next to its units (PATH and LocalDNS ordering).
  rm -rf "${ROOT}/etc/systemd/system/unbounded-kubelet.service.d" \
    "${ROOT}/etc/systemd/system/unbounded-containerd.service.d" \
    "${ROOT}/etc/systemd/system/unbounded-localdns.service.d"
  # The NVIDIA CDI spec the agent generates for the host driver.
  rm -f "${ROOT}/etc/cdi/nvidia.yaml"
  echo "Removing ${ROOT}/opt/unbounded/node..."
  rm -rf "${ROOT}/opt/unbounded/node"
}

# --- container: node stack runs in an agent-owned container ---------
# The units are named unbounded-node-<slot>.service by the container backend
# (container.UnitName); uninstall_script_test.go keeps this pattern in step.
stop_node_container() {
  echo "Stopping agent-owned container units..."
  local unit
  for unit_path in "${ROOT}"/etc/systemd/system/unbounded-node-*.service; do
    [ -e "${unit_path}" ] || continue
    unit="$(basename "${unit_path}")"
    systemctl stop "${unit}" 2>/dev/null || true
  done
}

remove_node_container() {
  echo "Removing agent-owned container units..."
  local unit
  for unit_path in "${ROOT}"/etc/systemd/system/unbounded-node-*.service; do
    [ -e "${unit_path}" ] || continue
    unit="$(basename "${unit_path}")"
    remove_unit "${unit}"
  done
  echo "Removing ${ROOT}/var/lib/unbounded/containers/${SLOT}..."
  rm -rf "${ROOT}/var/lib/unbounded/containers/${SLOT}"
}

# -----------------------------------------------------------------
# 1. Stop the node stack (mode specific)
# -----------------------------------------------------------------
stop_node_${ISOLATION_MODE}

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
rm -f "${ROOT}/etc/wireguard/server.priv" "${ROOT}/etc/wireguard/server.pub"

# -----------------------------------------------------------------
# 4. Remove the node stack and its configuration (mode specific)
# -----------------------------------------------------------------
remove_node_${ISOLATION_MODE}

# -----------------------------------------------------------------
# 5. Remove nftables flush service and unbounded-kube config dir
# -----------------------------------------------------------------
echo "Removing nftables flush service..."
systemctl disable --now nftables-flush.service 2>/dev/null || true
rm -f "${ROOT}/etc/systemd/system/nftables-flush.service"
rm -rf "${ROOT}/etc/unbounded/kube"

# -----------------------------------------------------------------
# 6. Remove sysctl configuration and reload
# -----------------------------------------------------------------
echo "Removing Kubernetes sysctl configuration..."
rm -f "${ROOT}/etc/sysctl.d/99-kubernetes.conf"
sysctl --system &>/dev/null || true

# -----------------------------------------------------------------
# 7. Restore Docker
# -----------------------------------------------------------------
echo "Restoring Docker configuration..."
systemctl unmask docker.service 2>/dev/null || true
systemctl unmask docker.socket 2>/dev/null || true
rm -f "${ROOT}/etc/docker/daemon.json"

# -----------------------------------------------------------------
# 8. Restore swap
# -----------------------------------------------------------------
if [ -f "${ROOT}/etc/fstab.bak" ]; then
  echo "Restoring swap from /etc/fstab.bak..."
  cp "${ROOT}/etc/fstab.bak" "${ROOT}/etc/fstab"
  rm -f "${ROOT}/etc/fstab.bak"
  swapon -a 2>/dev/null || true
else
  echo "No /etc/fstab.bak found, skipping swap restore."
fi

# -----------------------------------------------------------------
# 9. Clean up any leftover routes
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
# 10. Remove agent binaries and config artifacts
# -----------------------------------------------------------------
echo "Removing agent binaries and configuration..."
rm -f "${ROOT}/usr/local/bin/unbounded-agent"
rm -f "${ROOT}/usr/local/bin/unbounded-agent-install.sh"
rm -f "${ROOT}/usr/local/bin/unbounded-agent-uninstall.sh"
rm -rf "${ROOT}/etc/unbounded/agent"
rm -rf "${ROOT}/tmp/unbounded-agent"
rm -f "${ROOT}"/tmp/unbounded-agent-config.*.json

# -----------------------------------------------------------------
# 11. Reload systemd
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
