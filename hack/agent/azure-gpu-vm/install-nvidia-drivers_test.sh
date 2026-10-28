#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# Tests for the GPUDirect Storage helpers in install-nvidia-drivers.sh.
# They run without root, apt, or a GPU.
#
# Usage: ./hack/agent/azure-gpu-vm/install-nvidia-drivers_test.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_NVIDIA_DRIVERS_SOURCED=1
# shellcheck source=hack/agent/azure-gpu-vm/install-nvidia-drivers.sh
. "${SCRIPT_DIR}/install-nvidia-drivers.sh"
set +e

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
failures=0

expect_eq() {
    local name="$1" want="$2" got="$3"
    if [[ "${want}" != "${got}" ]]; then
        echo "FAIL ${name}: want '${want}', got '${got}'"
        failures=$((failures + 1))
    else
        echo "ok   ${name}"
    fi
}

# Run a command in a subshell and report whether it exited non-zero.
expect_fail() {
    local name="$1"
    shift
    if ("$@") >/dev/null 2>&1; then
        echo "FAIL ${name}: expected failure"
        failures=$((failures + 1))
    else
        echo "ok   ${name}"
    fi
}

os_release() {
    printf 'ID=%s\nVERSION_ID="%s"\n' "$1" "$2" > "${TMP}/os-release"
    echo "${TMP}/os-release"
}

expect_eq "repo ubuntu 22.04 x86_64" "ubuntu2204/x86_64" "$(cuda_repo_path "$(os_release ubuntu 22.04)" x86_64)"
expect_eq "repo ubuntu 24.04 x86_64" "ubuntu2404/x86_64" "$(cuda_repo_path "$(os_release ubuntu 24.04)" x86_64)"
expect_eq "repo ubuntu 24.04 aarch64" "ubuntu2404/sbsa" "$(cuda_repo_path "$(os_release ubuntu 24.04)" aarch64)"
expect_fail "repo rejects non-Ubuntu" cuda_repo_path "$(os_release azurelinux 3.0)" x86_64
expect_fail "repo rejects bad version" cuda_repo_path "$(os_release ubuntu noble)" x86_64
expect_fail "repo rejects unknown arch" cuda_repo_path "$(os_release ubuntu 24.04)" riscv64

expect_eq "gds open without Secure Boot" "0" "$( (check_gds_supported open no) >/dev/null 2>&1; echo $?)"
expect_fail "gds rejects proprietary driver" check_gds_supported proprietary no
expect_fail "gds rejects Secure Boot" check_gds_supported open yes

write_gds_modules_load "${TMP}/nvidia-fs.conf"
expect_eq "modules-load entry" "nvidia-fs" "$(cat "${TMP}/nvidia-fs.conf")"

write_cuda_pin "${TMP}/pin"
expect_eq "pin lowers CUDA repo" "1" "$(grep -c '^Pin-Priority: 100$' "${TMP}/pin")"
expect_eq "pin raises nvidia-fs and dkms" "Package: nvidia-fs nvidia-fs-dkms dkms" "$(grep '^Package: nvidia-fs' "${TMP}/pin")"

expect_fail "main rejects unknown option" main Standard_ND96isr_H100_v5 --bogus
expect_fail "main requires a SKU" main

if [[ "${failures}" -ne 0 ]]; then
    echo "${failures} test(s) failed"
    exit 1
fi
echo "all tests passed"
