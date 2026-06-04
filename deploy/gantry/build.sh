#!/usr/bin/env bash
#
# build.sh — local container-image build helper for gantry.
#
# Usage:
#   deploy/build.sh                      # single-arch, tag from `git describe`
#   deploy/build.sh -p linux/amd64,linux/arm64  # multi-arch (requires buildx + driver)
#   deploy/build.sh -t my-tag            # explicit tag
#   deploy/build.sh -r ghcr.io/me/gantry # registry+repo prefix; image becomes ${repo}:${tag}
#   deploy/build.sh --push               # docker buildx push instead of load
#
# Defaults:
#   - Single platform = host's GOOS/GOARCH equivalent.
#   - Tag = `git describe --tags --always --dirty` (falls back to "dev").
#   - Repository = "gantry" (no registry; image stays local).

set -euo pipefail

usage() {
    cat <<'EOF'
deploy/build.sh — build the gantry container image.

Flags:
  -p, --platform <list>   Comma-separated buildx platforms (e.g. linux/amd64,linux/arm64).
                          Default: $(uname -s)/$(uname -m).
  -t, --tag <value>       Image tag. Default: $(git describe --tags --always --dirty).
  -r, --repo <value>      Repository prefix (e.g. ghcr.io/me/gantry). Default: gantry.
      --push              docker buildx push to the registry after build.
      --load              docker buildx load into local docker (single-arch only). Default.
  -h, --help              Show this help.
EOF
}

cd "$(dirname "$0")/.."

PLATFORM=""
TAG=""
REPO="gantry"
ACTION="--load"

while [[ $# -gt 0 ]]; do
    case "$1" in
        -p|--platform) PLATFORM="$2"; shift 2 ;;
        -t|--tag)      TAG="$2";      shift 2 ;;
        -r|--repo)     REPO="$2";     shift 2 ;;
        --push)        ACTION="--push";        shift ;;
        --load)        ACTION="--load";        shift ;;
        -h|--help)     usage; exit 0 ;;
        *) echo "unknown flag: $1" >&2; usage; exit 2 ;;
    esac
done

# Default tag from `git describe`. When run outside a git checkout
# (e.g. release tarball), fall back to "dev".
if [[ -z "${TAG}" ]]; then
    if TAG="$(git describe --tags --always --dirty 2>/dev/null)"; then
        :
    else
        TAG="dev"
    fi
fi

# Default platform = Linux image for host CPU architecture. Docker
# container images are Linux here even when the developer runs the build
# helper from macOS.
if [[ -z "${PLATFORM}" ]]; then
    case "$(uname -m)" in
        x86_64|amd64) HOST_ARCH="amd64" ;;
        aarch64|arm64) HOST_ARCH="arm64" ;;
        *) HOST_ARCH="$(uname -m)" ;;
    esac
    PLATFORM="linux/${HOST_ARCH}"
fi

# Multi-arch builds require --push (buildx can't --load multi-arch
# manifests into the local engine).
if [[ "${PLATFORM}" == *,* && "${ACTION}" != "--push" ]]; then
    echo "multi-platform build requires --push" >&2
    exit 2
fi

IMAGE="${REPO}:${TAG}"
echo "==> Building ${IMAGE} for ${PLATFORM}"
docker buildx build \
    --platform="${PLATFORM}" \
    --build-arg GANTRY_VERSION="${TAG}" \
    --file=deploy/Dockerfile \
    --tag="${IMAGE}" \
    "${ACTION}" \
    .
echo "==> Done: ${IMAGE}"
