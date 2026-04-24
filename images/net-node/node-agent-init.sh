#!/bin/sh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

# This script is run in an initContainer to install CNI plugins and
# configure host sysctls before the main node agent starts.

# Ensure source validation honors fwmark-based routing.
# This must run in the host namespaces because /proc/sys is read-only in the container.
if ! command -v nsenter >/dev/null 2>&1; then
  echo "nsenter not found in image; cannot set src_valid_mark on host" >&2
  exit 1
fi

nsenter -t 1 -m -u -i -n -p -- sh -c '
set -eu
# Enable IPv4 forwarding -- required for overlay routing between pods.
sysctl -w net.ipv4.ip_forward=1 >/dev/null
# Enable IPv6 forwarding if any IPv6 addresses are configured on the node.
if [ -d /proc/sys/net/ipv6 ]; then
  sysctl -w net.ipv6.conf.all.forwarding=1 >/dev/null
fi
sysctl -w net.ipv4.conf.all.src_valid_mark=1 >/dev/null
sysctl -w net.ipv4.conf.default.src_valid_mark=1 >/dev/null
# Disable rp_filter on all interfaces. Overlay tunnels (GENEVE, VXLAN, IPIP)
# receive decapsulated packets with overlay source IPs that do not match the
# arrival interface; both strict (1) and loose (2) rp_filter would drop them.
# The kernel uses max(all, iface) so "all" and "default" must both be 0.
sysctl -w net.ipv4.conf.all.rp_filter=0 >/dev/null
sysctl -w net.ipv4.conf.default.rp_filter=0 >/dev/null
for p in /proc/sys/net/ipv4/conf/*/src_valid_mark; do
  [ -e "$p" ] || continue
  echo 1 > "$p"
done
for p in /proc/sys/net/ipv4/conf/*/rp_filter; do
  [ -e "$p" ] || continue
  echo 0 > "$p"
done
# These conntrack settings improve tunnel reliability but may not exist on
# all kernels. Apply them best-effort.
sysctl -w net.netfilter.nf_conntrack_be_liberal=1 >/dev/null 2>&1 || true
sysctl -w net.netfilter.nf_conntrack_ignore_invalid_rst=1 >/dev/null 2>&1 || true
'

echo "Set host ip_forward=1, ipv6.forwarding=1, rp_filter=0, src_valid_mark=1, nf_conntrack_be_liberal=1, nf_conntrack_ignore_invalid_rst=1 via nsenter"

# Ensure the host CNI bin mount is present before copying plugins.
if [ ! -d /host/opt/cni/bin ]; then
  echo "Missing /host/opt/cni/bin mount" >&2
  exit 1
fi

# Install CNI plugins onto the host filesystem, skipping existing files.
for src in /opt/cni/bin/*; do
  if [ ! -e "$src" ]; then
    continue
  fi
  dest="/host/opt/cni/bin/$(basename "$src")"
  if [ -e "$dest" ] || [ -L "$dest" ]; then
    continue
  fi
  cp -f "$src" "$dest"
done

echo "CNI plugins installed"
