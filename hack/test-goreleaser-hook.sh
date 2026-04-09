#!/usr/bin/env bash
# hack/test-goreleaser-hook.sh
#
# Tests that the goreleaser pre-hook correctly stamps the machina
# deployment manifest with the release tag before building kubectl-unbounded.
#
# Usage: ./hack/test-goreleaser-hook.sh

set -euo pipefail

TAG="v0.0.0-test"
MANIFEST="deploy/machina/04-deployment.yaml"
EXPECTED_IMAGE="ghcr.io/azure/machina:${TAG}"

# Save the original manifest so we can restore it on exit.
cleanup() {
    echo "Restoring manifest to default..."
    make machina-manifests
    git tag -d "$TAG" 2>/dev/null || true
    echo "Done."
}
trap cleanup EXIT

echo "=== Creating local tag ${TAG} ==="
git tag "$TAG"

echo "=== Running goreleaser snapshot ==="
goreleaser release --snapshot --clean

echo "=== Checking manifest ==="
actual=$(grep 'image:' "$MANIFEST" | xargs)
if [[ "$actual" == *"$EXPECTED_IMAGE"* ]]; then
    echo "PASS: manifest stamped correctly → ${EXPECTED_IMAGE}"
else
    echo "FAIL: expected '${EXPECTED_IMAGE}' but got '${actual}'"
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

echo ""
echo "All checks passed."
