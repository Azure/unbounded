#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
version=""
registry="ghcr.io/azure"
namespace="unbounded-system"
api_server_endpoint=""
output="${repo_root}/build/operator.yaml"
storage_release_base_url=""
managed_kube_proxy_image=""
reap_legacy_resources="true"
operator_image=""
metalman_image=""
netboot_image=""
machina_image=""
net_controller_image=""
net_node_image=""
storage_supervisor_image=""

usage() {
  cat <<'EOF'
Usage: hack/scripts/generate-operator-manifest.sh --version VERSION [options]

Options:
  --registry REGISTRY
  --namespace NAMESPACE
  --api-server-endpoint URL
  --output PATH
  --storage-release-base-url URL
  --managed-kube-proxy-image IMAGE
  --operator-image IMAGE
  --metalman-image IMAGE
  --netboot-image IMAGE
  --machina-image IMAGE
  --net-controller-image IMAGE
  --net-node-image IMAGE
  --storage-supervisor-image IMAGE
  --reap-legacy-resources BOOL
EOF
}

while (($#)); do
  case "$1" in
    --version) version=${2:?}; shift 2 ;;
    --registry) registry=${2:?}; shift 2 ;;
    --namespace) namespace=${2:?}; shift 2 ;;
    --api-server-endpoint) api_server_endpoint=${2-}; shift 2 ;;
    --output) output=${2:?}; shift 2 ;;
    --storage-release-base-url) storage_release_base_url=${2-}; shift 2 ;;
    --managed-kube-proxy-image) managed_kube_proxy_image=${2-}; shift 2 ;;
    --operator-image) operator_image=${2:?}; shift 2 ;;
    --metalman-image) metalman_image=${2:?}; shift 2 ;;
    --netboot-image) netboot_image=${2:?}; shift 2 ;;
    --machina-image) machina_image=${2:?}; shift 2 ;;
    --net-controller-image) net_controller_image=${2:?}; shift 2 ;;
    --net-node-image) net_node_image=${2:?}; shift 2 ;;
    --storage-supervisor-image) storage_supervisor_image=${2:?}; shift 2 ;;
    --reap-legacy-resources) reap_legacy_resources=${2:?}; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) printf 'error: unknown argument %s\n' "$1" >&2; usage >&2; exit 2 ;;
  esac
done

if [[ -z "$version" ]]; then
  printf 'error: --version is required\n' >&2
  exit 2
fi
if [[ "$namespace" == "unbounded-kube" || "$namespace" == "unbounded-net" ]]; then
  printf 'error: refusing legacy namespace %s\n' "$namespace" >&2
  exit 2
fi
if [[ "$registry" != "ghcr.io/azure" && -z "$managed_kube_proxy_image" ]]; then
  printf 'error: --managed-kube-proxy-image is required with a mirrored registry\n' >&2
  exit 2
fi

registry=${registry%/}
version_tag=${version//\//-}
operator_image=${operator_image:-"${registry}/unbounded-operator:${version_tag}"}
metalman_image=${metalman_image:-"${registry}/metalman:${version_tag}"}
netboot_image=${netboot_image:-"${registry}/netboot:${version_tag}"}
machina_image=${machina_image:-"${registry}/machina:${version_tag}"}
net_controller_image=${net_controller_image:-"${registry}/unbounded-net-controller:${version_tag}"}
net_node_image=${net_node_image:-"${registry}/unbounded-net-node:${version_tag}"}
storage_supervisor_image=${storage_supervisor_image:-"${registry}/unbounded-storage-supervisor:${version_tag}"}
storage_release_base_url=${storage_release_base_url:-"https://github.com/Azure/unbounded/releases/download/${version}"}

work_dir=$(mktemp -d "${repo_root}/tmp/operator-manifest.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT

go run "${repo_root}/hack/cmd/render-manifests" \
  --templates-dir "${repo_root}/deploy/unbounded-operator" \
  --output-dir "$work_dir" \
  --set "Namespace=${namespace}" \
  --set "OperatorImage=${operator_image}" \
  --set "MetalmanImage=${metalman_image}" \
  --set "NetbootImage=${netboot_image}" \
  --set "MachinaImage=${machina_image}" \
  --set "NetControllerImage=${net_controller_image}" \
  --set "NetNodeImage=${net_node_image}" \
  --set "StorageSupervisorImage=${storage_supervisor_image}" \
  --set "ManagedKubeProxyImage=${managed_kube_proxy_image}" \
  --set "StorageVersion=${version}" \
  --set "StorageReleaseBaseURL=${storage_release_base_url}" \
  --set "APIServerEndpoint=${api_server_endpoint}" \
  --set "ReapLegacyResources=${reap_legacy_resources}"

mkdir -p "$(dirname "$output")"
: > "$output"
for manifest in 00-namespace 01-serviceaccount 02-rbac 03-configmap 04-deployment; do
  printf '%s\n' "$(<"${work_dir}/${manifest}.yaml")" >> "$output"
done
