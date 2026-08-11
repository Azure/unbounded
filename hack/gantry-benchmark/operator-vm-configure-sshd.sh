#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -Eeuo pipefail

command -v /usr/sbin/sshd >/dev/null 2>&1 || {
  echo "openssh-server is required" >&2
  exit 1
}

install -d -m 0755 /run/sshd
install -d -m 0755 /etc/ssh/sshd_config.d
cat >/etc/ssh/sshd_config.d/00-gantry-benchmark.conf <<'SSH'
Port 50001
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
SSH
/usr/sbin/sshd -t
if systemctl list-unit-files ssh.socket >/dev/null 2>&1; then
  systemctl disable --now ssh.socket >/dev/null 2>&1 || true
  systemctl mask ssh.socket >/dev/null 2>&1
fi
systemctl enable ssh.service >/dev/null
systemctl restart ssh.service
mapfile -t ssh_ports < <(/usr/sbin/sshd -T | awk '$1 == "port" {print $2}')
[[ "${ssh_ports[*]}" == 50001 ]] || {
  echo "effective sshd ports are ${ssh_ports[*]}, want only 50001" >&2
  exit 1
}
ss -ltn | awk '$4 ~ /:50001$/ {found=1} END {exit !found}' || {
  echo "sshd is not listening on TCP 50001" >&2
  exit 1
}
if ss -ltn | awk '$4 ~ /:22$/ {found=1} END {exit !found}'; then
  echo "TCP 22 is still listening after SSH reconfiguration" >&2
  exit 1
fi
