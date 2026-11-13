#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
#
# Build libracer_gpu_cuda.so.1 from cmd/racer/gpu/native.
#
# The shim dlopens the CUDA driver, NVML and cuFile, so it needs no CUDA
# headers or libraries. When nvcc is found (NVCC, or nvcc on PATH) the decrypt
# kernels are compiled to compute_80 PTX and embedded, along with SASS cubins
# for sm_90 and sm_100 when this nvcc supports them (the shim picks the cubin
# matching the device and falls back to the PTX); otherwise the shim builds without
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

# Architectures this nvcc can target, one per line (for example sm_90).
nvcc_archs() {
    "$nvcc" --list-gpu-arch 2>/dev/null || true
}

set --
if [ -n "$nvcc" ]; then
    # compute_80 (Ampere) is the PTX baseline; the driver JIT-compiles it for
    # GPUs without a matching cubin.
    "$nvcc" -ptx -arch=compute_80 -O3 -std=c++14 -I "$native" \
        -o "$work/gcm_kernel.ptx" "$native/gcm_kernel.cu"
    set -- "-DRACER_GPU_PTX=\"$work/gcm_kernel.ptx\""
    # --list-gpu-arch prints compute_XX names (CUDA 11.6+); older nvcc
    # lacks the flag and gets no cubins.
    archs=$(nvcc_archs)
    for sm in 90 100; do
        if printf '%s\n' "$archs" | grep -qx "compute_$sm"; then
            "$nvcc" -cubin -arch=sm_$sm -O3 -std=c++14 -I "$native" \
                -o "$work/gcm_kernel.sm_$sm.cubin" "$native/gcm_kernel.cu"
            set -- "$@" "-DRACER_GPU_CUBIN_SM$sm=\"$work/gcm_kernel.sm_$sm.cubin\""
        else
            echo "nvcc does not support sm_$sm: skipping its cubin" >&2
        fi
    done
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
