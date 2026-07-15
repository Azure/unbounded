#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# install-cloud-hypervisor.sh -- install the cloud-hypervisor and ch-remote
# static binaries used by the metalman Redfish test fixture
# (internal/metalman/redfish/qemusvr).
#
# The metalman smoke tests drive a Cloud Hypervisor VM through the fixture.
# Cloud Hypervisor publishes statically linked release binaries, so we simply
# download and install them rather than building from source.

set -euo pipefail

CH_VERSION="${CH_VERSION:-v53.0}"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
BASE_URL="https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/${CH_VERSION}"

# Use sudo only when we cannot already write to the install directory (e.g. the
# script may run as root in CI).
sudo=""
if [ ! -w "${INSTALL_DIR}" ]; then
  sudo="sudo"
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "${tmpdir}"' EXIT

echo "==> Downloading cloud-hypervisor ${CH_VERSION} static binaries"
curl -fsSL -o "${tmpdir}/cloud-hypervisor" "${BASE_URL}/cloud-hypervisor-static"
curl -fsSL -o "${tmpdir}/ch-remote" "${BASE_URL}/ch-remote-static"

echo "==> Installing to ${INSTALL_DIR}"
${sudo} install -m 0755 "${tmpdir}/cloud-hypervisor" "${INSTALL_DIR}/cloud-hypervisor"
${sudo} install -m 0755 "${tmpdir}/ch-remote" "${INSTALL_DIR}/ch-remote"

echo "==> Installed:"
"${INSTALL_DIR}/cloud-hypervisor" --version
"${INSTALL_DIR}/ch-remote" --version
