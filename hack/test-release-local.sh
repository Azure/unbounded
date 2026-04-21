#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# hack/test-release-local.sh
#
# Exercise the unified release pipeline locally without pushing artifacts.
#
# Mirrors the GitHub Actions workflow .github/workflows/release.yaml as
# closely as practical:
#
#   1. preflight       - check required tools are installed
#   2. goreleaser check - validate .goreleaser.yml
#   3. frontend        - build the React UI into internal/net/html/dist
#   4. cni-plugins     - download CNI plugin tarballs into resources/
#   5. release-manifests - render and tar the combined machina+net manifests
#   6. goreleaser snapshot - build kube binaries and stamped manifest archive
#                            (skips publish, sign, sbom, docker)
#   7. test-goreleaser-hook - assert manifests + binaries are stamped with TAG
#   8. images          - docker buildx build machina, metalman, host (qemu),
#                        net-controller, net-node (native arch only)
#
# Default behavior:
#   - TAG=v0.0.0-localtest unless overridden via $TAG
#   - Builds amd64 only for net images. Pass --multi-arch to also build arm64
#     via QEMU emulation (slow).
#   - Skips host-ubuntu2404 by default (very large). Pass --include-host.
#   - Skips net image builds entirely with --skip-net.
#   - Removes dist/ and build/ on exit unless --keep-dist is given.
#
# Usage:
#   ./hack/test-release-local.sh [--multi-arch] [--include-host]
#                                [--skip-net] [--keep-dist]
#
# Env knobs:
#   TAG                  Tag to use for snapshot. Default v0.0.0-localtest.
#   CNI_PLUGINS_VERSION  CNI plugins version. Default v1.9.0.
#   GO_VERSION           Go version build-arg. Default parsed from go.mod.

set -euo pipefail

TAG="${TAG:-v0.0.0-localtest}"
CNI_PLUGINS_VERSION="${CNI_PLUGINS_VERSION:-v1.9.0}"

MULTI_ARCH=0
INCLUDE_HOST=0
SKIP_NET=0
KEEP_DIST=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --multi-arch)   MULTI_ARCH=1 ;;
        --include-host) INCLUDE_HOST=1 ;;
        --skip-net)     SKIP_NET=1 ;;
        --keep-dist)    KEEP_DIST=1 ;;
        -h|--help)
            sed -n '5,40p' "$0"
            exit 0
            ;;
        *)
            echo "unknown flag: $1" >&2
            exit 2
            ;;
    esac
    shift
done

# Resolve repo root from this script's location.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

GO_VERSION="${GO_VERSION:-$(awk '/^go [0-9]/ {print $2; exit}' go.mod)}"

NATIVE_ARCH="$(uname -m)"
case "$NATIVE_ARCH" in
    x86_64)  NATIVE_ARCH=amd64 ;;
    aarch64) NATIVE_ARCH=arm64 ;;
    arm64)   NATIVE_ARCH=arm64 ;;
    *)       echo "unsupported native arch: $NATIVE_ARCH" >&2; exit 2 ;;
esac

LOCAL_TAG_CREATED=0
cleanup() {
    local rc=$?
    if [[ "$LOCAL_TAG_CREATED" == "1" ]]; then
        git tag -d "$TAG" >/dev/null 2>&1 || true
    fi
    if [[ "$KEEP_DIST" != "1" ]]; then
        rm -rf dist build/release-manifests
    fi
    if [[ $rc -eq 0 ]]; then
        echo ""
        echo "All release pipeline stages passed."
    else
        echo ""
        echo "Release pipeline FAILED with exit $rc"
    fi
    exit $rc
}
trap cleanup EXIT

step() {
    echo ""
    echo "=== $* ==="
}

require() {
    local tool="$1"
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "missing required tool: $tool" >&2
        exit 2
    fi
}

# 1. preflight
step "1/8 preflight"
for tool in go make git docker goreleaser awk grep tar curl; do
    require "$tool"
done
docker buildx version >/dev/null 2>&1 || {
    echo "docker buildx not available" >&2; exit 2;
}
echo "Repo:        $REPO_ROOT"
echo "Tag:         $TAG"
echo "Go version:  $GO_VERSION"
echo "CNI version: $CNI_PLUGINS_VERSION"
echo "Native arch: $NATIVE_ARCH"
echo "Multi-arch:  $MULTI_ARCH"
echo "Include host:$INCLUDE_HOST"
echo "Skip net:    $SKIP_NET"

# 2. goreleaser check
step "2/8 goreleaser check"
goreleaser check

# 3. frontend
step "3/8 frontend (make net-frontend)"
make net-frontend

# 4. cni plugins
if [[ "$SKIP_NET" != "1" ]]; then
    step "4/8 cni plugins (download into resources/)"
    mkdir -p resources
    archs=("$NATIVE_ARCH")
    if [[ "$MULTI_ARCH" == "1" ]]; then
        archs=(amd64 arm64)
    fi
    for a in "${archs[@]}"; do
        f="resources/cni-plugins-linux-${a}-${CNI_PLUGINS_VERSION}.tgz"
        if [[ -f "$f" ]]; then
            echo "have $f"
        else
            url="https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${a}-${CNI_PLUGINS_VERSION}.tgz"
            echo "fetching $url"
            curl -fSL --retry 3 -o "$f" "$url"
        fi
    done
else
    step "4/8 cni plugins (skipped: --skip-net)"
fi

# Create local tag so VERSION-derived stamping works.
if git rev-parse --verify --quiet "refs/tags/$TAG" >/dev/null; then
    echo "tag $TAG already exists; reusing"
else
    git tag "$TAG"
    LOCAL_TAG_CREATED=1
fi

# 5. release-manifests tarball
step "5/8 release-manifests (make release-manifests)"
make release-manifests
ls -lh build/unbounded-manifests-*.tar.gz

# 6. goreleaser snapshot
step "6/8 goreleaser snapshot"
goreleaser release --snapshot --clean \
    --skip=publish --skip=sign --skip=sbom --skip=docker

# 7. embedded-stamp assertions (delegates to existing test)
step "7/8 test-goreleaser-hook (manifest + binary stamping assertions)"
# That script tags v0.0.0-test on its own; run it cleanly so it does not
# clash with our snapshot tag.
( unset TAG; "$REPO_ROOT/hack/test-goreleaser-hook.sh" )

# 8. images
if [[ "$SKIP_NET" != "1" || "$INCLUDE_HOST" == "1" || -n "${FORCE_IMAGES:-}" ]]; then
    step "8/8 docker buildx images"

    if [[ "$MULTI_ARCH" == "1" ]]; then
        platforms="linux/amd64,linux/arm64"
    else
        platforms="linux/${NATIVE_ARCH}"
    fi

    common_args=(
        --platform "$platforms"
        --build-arg "GO_VERSION=$GO_VERSION"
        --build-arg "VERSION=$TAG"
        --build-arg "GIT_COMMIT=$(git rev-parse HEAD)"
        --load
    )
    # --load only supports single-platform. Drop it for multi-arch builds
    # and just verify the build succeeds without importing into the daemon.
    if [[ "$MULTI_ARCH" == "1" ]]; then
        common_args=("${common_args[@]/--load/}")
    fi

    echo ""
    echo "--- machina ---"
    docker buildx build "${common_args[@]}" \
        -f images/machina/Containerfile \
        -t "ghcr.io/azure/machina:${TAG}" .

    echo ""
    echo "--- metalman ---"
    docker buildx build "${common_args[@]}" \
        -f images/metalman/Containerfile \
        -t "ghcr.io/azure/metalman:${TAG}" .

    if [[ "$INCLUDE_HOST" == "1" ]]; then
        echo ""
        echo "--- host-ubuntu2404 ---"
        docker buildx build "${common_args[@]}" \
            -f images/host-ubuntu2404/Containerfile \
            -t "ghcr.io/azure/host-ubuntu2404:${TAG}" .
    else
        echo "skipping host-ubuntu2404 (pass --include-host to enable)"
    fi

    if [[ "$SKIP_NET" != "1" ]]; then
        net_args=(
            "${common_args[@]}"
            --build-arg "CNI_PLUGINS_VERSION=$CNI_PLUGINS_VERSION"
        )
        echo ""
        echo "--- unbounded-net-controller ---"
        docker buildx build "${net_args[@]}" \
            -f images/net-controller/Dockerfile \
            --target controller \
            -t "ghcr.io/azure/unbounded-net-controller:${TAG}" .

        echo ""
        echo "--- unbounded-net-node ---"
        docker buildx build "${net_args[@]}" \
            -f images/net-node/Dockerfile \
            --target node \
            -t "ghcr.io/azure/unbounded-net-node:${TAG}" .
    else
        echo "skipping net images (--skip-net)"
    fi
else
    step "8/8 images (skipped)"
fi
