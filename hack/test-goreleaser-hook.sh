#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# hack/test-goreleaser-hook.sh
#
# Tests that the goreleaser pre-hooks correctly stamp the machina and
# unbounded-net deployment manifests with the release tag before
# building kubectl-unbounded.
#
# Usage: ./hack/test-goreleaser-hook.sh

set -euo pipefail

TAG="v0.0.0-test"
MANIFEST="deploy/machina/rendered/04-deployment.yaml"
EXPECTED_IMAGE="ghcr.io/azure/machina:${TAG}"
NET_MANIFEST="deploy/net/rendered/controller/03-deployment.yaml"
EXPECTED_NET_IMAGE="ghcr.io/azure/unbounded-net-controller:${TAG}"

# Save the original manifest so we can restore it on exit.
cleanup() {
    echo "Restoring manifests to default..."
    make machina-manifests net-render-manifests
    git tag -d "$TAG" 2>/dev/null || true
    echo "Done."
}
trap cleanup EXIT

echo "=== Creating local tag ${TAG} ==="
git tag "$TAG"

echo "=== Running goreleaser snapshot ==="
goreleaser release --snapshot --clean

echo "=== Checking machina manifest ==="
actual=$(grep 'image:' "$MANIFEST" | xargs)
if [[ "$actual" == *"$EXPECTED_IMAGE"* ]]; then
    echo "PASS: machina manifest stamped correctly -> ${EXPECTED_IMAGE}"
else
    echo "FAIL: expected '${EXPECTED_IMAGE}' but got '${actual}'"
    exit 1
fi

echo "=== Checking net manifest ==="
actual_net=$(grep 'image:' "$NET_MANIFEST" | xargs)
if [[ "$actual_net" == *"$EXPECTED_NET_IMAGE"* ]]; then
    echo "PASS: net manifest stamped correctly -> ${EXPECTED_NET_IMAGE}"
else
    echo "FAIL: expected '${EXPECTED_NET_IMAGE}' but got '${actual_net}'"
    exit 1
fi

echo "=== Checking embedded image in binary ==="
# goreleaser puts binaries under dist/; find the linux amd64 one.
binary=$(find dist -name 'kubectl-unbounded' -path '*linux_amd64*' | head -1)
if [[ -z "$binary" ]]; then
    echo "FAIL: could not find kubectl-unbounded binary in dist/"
    exit 1
fi

if grep -qF "$EXPECTED_IMAGE" <(strings "$binary"); then
    echo "PASS: binary embeds ${EXPECTED_IMAGE}"
else
    echo "FAIL: binary does not contain ${EXPECTED_IMAGE}"
    grep -F 'machina:' <(strings "$binary") || true
    exit 1
fi

if grep -qF "$EXPECTED_NET_IMAGE" <(strings "$binary"); then
    echo "PASS: binary embeds ${EXPECTED_NET_IMAGE}"
else
    echo "FAIL: binary does not contain ${EXPECTED_NET_IMAGE}"
    grep -F 'unbounded-net-controller:' <(strings "$binary") || true
    exit 1
fi

echo ""
echo "All checks passed."
