#!/bin/sh
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -eu

if [ "$#" -ne 1 ]; then
    echo "usage: check-debug-info.sh ELF" >&2
    exit 2
fi

# Inspect the artifact, not just Cargo settings: environment overrides or stripping
# must not silently break Parca source/inline symbolization. readelf is in binutils,
# already supplied by the Rust builder's native compiler toolchain.
sections=$(LC_ALL=C readelf --wide --section-headers "$1")
for section in debug_info debug_line; do
    if ! printf '%s\n' "$sections" | grep -Eq "[[:space:]]\.${section}[[:space:]]+PROGBITS[[:space:]]"; then
        echo "$1: missing .$section; release artifacts require DWARF for Parca" >&2
        exit 1
    fi
done
