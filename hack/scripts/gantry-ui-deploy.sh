#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
set -euo pipefail

usage() {
    cat <<'EOF'
Build, push, and install the Gantry test dashboard (no CI or Gantry upgrade).
Usage: hack/scripts/gantry-ui-deploy.sh --kubeconfig PATH --context NAME --values FILE [options]
  --image REPO        Image repository (default ghcr.io/vpatelsj/gantry-ui)
  --tag TAG           Unique image tag (default dev-COMMIT-TIMESTAMP)
  --platform PLATFORM linux/amd64 or linux/arm64 (default linux/amd64)
  --namespace NAME    Dedicated demo namespace (default gantry-demo)
  --release NAME      Helm release (default gantry-ui)
  --engine NAME       docker or podman (default first available)
  --help             Show this help

Sign in to GHCR using the selected container engine first. Configure Azure
Workload Identity, registry permissions, and optional private pull Secrets
separately. The values file supplies the ACR, Azure identities, pool, and
optional builder/reclaimer settings. This command does not create Azure roles,
delete demos, modify Gantry, or start a port-forward.
EOF
}

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
kubeconfig= context= values= engine=
image=ghcr.io/vpatelsj/gantry-ui
tag= platform=linux/amd64 namespace=gantry-demo release=gantry-ui
while (($#)); do
    case "$1" in
        --help|-h) usage; exit 0 ;;
        --kubeconfig|--context|--values|--image|--tag|--platform|--namespace|--release|--engine)
            if (($# < 2)) || [[ "$2" == --* || -z "$2" ]]; then echo "Missing value for $1" >&2; exit 2; fi
            case "$1" in
                --kubeconfig) kubeconfig=$2 ;; --context) context=$2 ;;
                --values) values=$2 ;; --image) image=$2 ;; --tag) tag=$2 ;;
                --platform) platform=$2 ;; --namespace) namespace=$2 ;;
                --release) release=$2 ;; --engine) engine=$2 ;;
            esac
            shift 2 ;;
        *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done
if [[ ! -f "$kubeconfig" || ! -f "$values" || -z "$context" ]]; then
    echo "--kubeconfig, --context, and an existing --values file are required" >&2; exit 2
fi
case "$platform" in linux/amd64|linux/arm64) ;; *) echo "Unsupported platform: $platform" >&2; exit 2 ;; esac
if [[ ! "$image" =~ ^ghcr\.io/[a-z0-9][a-z0-9._/-]*$ ]]; then
    echo "--image must be an untagged lowercase ghcr.io repository" >&2; exit 2
fi
for name in "$namespace" "$release"; do
    if [[ ! "$name" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ || ${#name} -gt 53 ]]; then
        echo "Invalid namespace or release name: $name" >&2; exit 2
    fi
done
if [[ -z "$engine" ]]; then
    if command -v docker >/dev/null 2>&1; then engine=docker
    elif command -v podman >/dev/null 2>&1; then engine=podman
    else echo "Docker or Podman is required; install/start one and retry" >&2; exit 1
    fi
fi
case "$engine" in docker|podman) ;; *) echo "--engine must be docker or podman" >&2; exit 2 ;; esac
for tool in "$engine" kubectl helm git; do
    if ! command -v "$tool" >/dev/null 2>&1; then echo "Required tool missing: $tool" >&2; exit 1; fi
done
"$engine" info >/dev/null
if [[ -z "$tag" ]]; then tag="dev-$(git -C "$root" rev-parse --short HEAD)-$(date -u +%Y%m%d%H%M%S)"; fi
if [[ ! "$tag" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$ ]]; then echo "Invalid image tag" >&2; exit 2; fi

chart="$root/deploy/gantry-ui/chart"
arch=${platform#linux/}
kubectl --kubeconfig "$kubeconfig" --context "$context" cluster-info >/dev/null
echo "Target context: $context; namespace: $namespace; image: $image:$tag"
# Validate chart configuration before building or publishing. The real digest
# is resolved from the pushed registry manifest, never the local image ID.
helm template "$release" "$chart" --namespace "$namespace" --values "$values" \
    --set-string "image.repository=$image" \
    --set-string "image.digest=sha256:$(printf '%064d' 0)" \
    --set-string "architecture=$arch" \
    --set-string "nodeSelector.kubernetes\\.io/arch=$arch" >/dev/null
if [[ "$engine" == docker ]]; then
    docker buildx build --platform "$platform" --file "$root/images/gantry-ui/Containerfile" \
        --build-arg TARGETOS=linux --build-arg "TARGETARCH=$arch" --tag "$image:$tag" --push "$root"
    manifest=$(docker buildx imagetools inspect "$image:$tag")
    digest=$(printf '%s\n' "$manifest" | awk '$1 == "Digest:" {print $2; exit}')
else
    podman build --platform "$platform" --build-arg TARGETOS=linux --build-arg "TARGETARCH=$arch" \
        --file "$root/images/gantry-ui/Containerfile" --tag "$image:$tag" "$root"
    # A project-local temporary directory avoids touching shared host /tmp.
    mkdir -p "$root/tmp"
    scratch=$(mktemp -d "$root/tmp/gantry-ui-push.XXXXXX")
    trap 'rm -f "$scratch/digest"; rmdir "$scratch"' EXIT
    podman push --digestfile "$scratch/digest" "$image:$tag"
    digest=$(<"$scratch/digest")
fi
if [[ ! "$digest" =~ ^sha256:[a-f0-9]{64}$ ]]; then echo "Registry did not return a valid image digest" >&2; exit 1; fi
helm upgrade --install "$release" "$chart" --kubeconfig "$kubeconfig" --kube-context "$context" \
    --namespace "$namespace" --create-namespace --values "$values" \
    --set-string "image.repository=$image" --set-string "image.digest=$digest" \
    --set-string "architecture=$arch" \
    --set-string "nodeSelector.kubernetes\\.io/arch=$arch" --wait --timeout 5m
echo "Published $image@$digest"
echo "Use the port-forward command in Helm NOTES to open the dashboard."
