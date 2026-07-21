#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

VERSION="v0.0.0-test"
OUTPUT="build/unbounded-operator-${VERSION}.yaml"

assert_reference() {
    local reference="$1"

    if ! grep -qF -- "$reference" "$OUTPUT"; then
        echo "expected ${OUTPUT} to contain ${reference}" >&2
        exit 1
    fi
}

make unbounded-operator-release-manifest VERSION="$VERSION"
assert_reference "image: ghcr.io/azure/unbounded-operator:${VERSION}"
assert_reference "--metalman-image=ghcr.io/azure/metalman:${VERSION}"

make unbounded-operator-release-manifest \
    VERSION="$VERSION" \
    CONTAINER_REGISTRY="registry.example.com/unbounded"
assert_reference "image: registry.example.com/unbounded/unbounded-operator:${VERSION}"
assert_reference "--metalman-image=registry.example.com/unbounded/metalman:${VERSION}"

echo "Operator release manifest checks passed."
