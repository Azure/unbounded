#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Compile gcm_kernel.cu as host C++ (kernel_emulation.cc) and run it.
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: run-kernel-emulation.sh OUTPUT_DIR" >&2
    exit 2
fi

native=$(dirname "$0")
mkdir -p "$1"
"${CXX:-c++}" -std=c++20 -O2 -Wall -Wextra -Werror -pthread -I "$native" \
    -o "$1/kernel-emulation" "$native/kernel_emulation.cc"
"$1/kernel-emulation"
