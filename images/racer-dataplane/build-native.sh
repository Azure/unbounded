#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -eu

if [ "$#" -ne 2 ]; then
    echo "usage: build-native.sh SOURCE OUTPUT" >&2
    exit 2
fi

# Fail before compiling if the real verbs development package is unavailable.
pkg-config --exists libibverbs
mkdir -p "$(dirname "$2")"
# pkg-config emits compiler/linker argument lists, intentionally word-split.
# shellcheck disable=SC2046
"${CC:-cc}" $(pkg-config --cflags libibverbs) \
    -O2 -g -std=gnu11 -Wall -Wextra -Werror -fPIC -shared \
    -Wl,-soname,libracer_rdma.so.1 -Wl,-z,defs \
    -o "$2" "$1" $(pkg-config --libs libibverbs)
