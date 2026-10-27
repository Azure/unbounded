#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Build libracer_gpu_cuda.so.1 from cmd/racer-dataplane/gpu/native.
#
# The shim dlopens the CUDA driver, NVML and cuFile, so it needs no CUDA
# headers or libraries. When nvcc is found (NVCC, or nvcc on PATH) the decrypt
# kernels are compiled to PTX and embedded; otherwise the shim builds without
# them and GPU decryption reports "no kernels". Set RACER_GPU_REQUIRE_PTX=1 to
# fail instead of building without kernels.
set -eu

if [ "$#" -ne 2 ]; then
    echo "usage: build-native-cuda.sh NATIVE_DIR OUTPUT" >&2
    exit 2
fi

native=$1
output=$2
mkdir -p "$(dirname "$output")"
nvcc=${NVCC:-$(command -v nvcc || true)}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

set --
if [ -n "$nvcc" ]; then
    # compute_80 (Ampere) is the PTX baseline; the driver JIT-compiles it for
    # the installed GPU.
    "$nvcc" -ptx -arch=compute_80 -O3 -std=c++14 -I "$native" \
        -o "$work/gcm_kernel.ptx" "$native/gcm_kernel.cu"
    set -- "-DRACER_GPU_PTX=\"$work/gcm_kernel.ptx\""
elif [ "${RACER_GPU_REQUIRE_PTX:-0}" = 1 ]; then
    echo "nvcc not found and RACER_GPU_REQUIRE_PTX=1" >&2
    exit 1
else
    echo "nvcc not found: building libracer_gpu_cuda.so.1 without decrypt kernels" >&2
fi

"${CC:-cc}" "$@" -I "$native" \
    -O2 -g -fno-omit-frame-pointer -std=gnu11 -Wall -Wextra -Werror -fPIC -shared \
    -Wl,-soname,libracer_gpu_cuda.so.1 -Wl,-z,defs -Wl,-z,noexecstack \
    -o "$output" "$native/gpu_cuda.c" -ldl -lpthread
