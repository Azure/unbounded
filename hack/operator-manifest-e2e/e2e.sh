#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -euo pipefail

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
WORKDIR="${REPO_ROOT}/tmp/operator-manifest-e2e"
KUBECONFIG="${WORKDIR}/kubeconfig"
CLUSTER_NAME="${E2E_CLUSTER_NAME:-operator-manifest-e2e}"
REGISTRY_NAME="${E2E_REGISTRY_NAME:-operator-manifest-e2e-registry}"
REGISTRY_PORT="${E2E_REGISTRY_PORT:-5002}"
VERSION="${E2E_VERSION:-e2e}"
CONTAINER_ENGINE="${CONTAINER_ENGINE:-docker}"
REGISTRY="localhost:${REGISTRY_PORT}"
NAMESPACE=unbounded-system
FAILED=1

log() {
    printf '[operator-manifest-e2e] %s\n' "$*"
}

die() {
    log "ERROR: $*" >&2
    exit 1
}

kubectl_e2e() {
    kubectl --kubeconfig "${KUBECONFIG}" "$@"
}

cluster_exists() {
    kind get clusters 2>/dev/null | grep -Fxq "${CLUSTER_NAME}"
}

diagnostics() {
    if ! cluster_exists || [[ ! -f "${KUBECONFIG}" ]]; then
        return
    fi

    log "collecting failure diagnostics"
    kubectl_e2e get nodes -o wide || true
    kubectl_e2e -n "${NAMESPACE}" get deploy,ds,pods -o wide || true
    kubectl_e2e -n "${NAMESPACE}" describe pods || true
    kubectl_e2e -n "${NAMESPACE}" get events --sort-by=.lastTimestamp || true
    kubectl_e2e -n "${NAMESPACE}" logs deploy/unbounded-operator --all-containers --tail=300 || true
}

cleanup() {
    if [[ "${FAILED}" == 1 ]]; then
        diagnostics
    fi

    if [[ "${E2E_KEEP:-0}" == 1 ]]; then
        log "keeping cluster ${CLUSTER_NAME} and registry ${REGISTRY_NAME}"
        return
    fi

    [[ -n "${CLUSTER_NAME}" ]] || die "refusing to clean up an empty cluster name"
    [[ -n "${REGISTRY_NAME}" ]] || die "refusing to clean up an empty registry name"
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
    docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
}

trap cleanup EXIT

check_prerequisites() {
    local command
    for command in docker kind kubectl go make sed curl grep "${CONTAINER_ENGINE}"; do
        command -v "${command}" >/dev/null 2>&1 || die "missing required command: ${command}"
    done

    docker info >/dev/null 2>&1 || die "Docker is not reachable"
    [[ "${REGISTRY_PORT}" =~ ^[0-9]+$ ]] || die "E2E_REGISTRY_PORT must be numeric"
    [[ -n "${VERSION}" ]] || die "E2E_VERSION cannot be empty"
}

start_registry() {
    docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
    docker run -d --restart=always \
        -p "127.0.0.1:${REGISTRY_PORT}:5000" \
        --name "${REGISTRY_NAME}" registry:2 >/dev/null

    local attempt
    for attempt in $(seq 1 60); do
        if curl -fsS "http://${REGISTRY}/v2/" >/dev/null; then
            return
        fi
        sleep 1
    done

    die "local registry did not become ready"
}

create_cluster() {
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
    cat >"${WORKDIR}/kind-config.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
nodes:
- role: control-plane
EOF
    kind create cluster --name "${CLUSTER_NAME}" \
        --config "${WORKDIR}/kind-config.yaml" \
        --kubeconfig "${KUBECONFIG}" --wait 240s

    docker network connect kind "${REGISTRY_NAME}" >/dev/null 2>&1 || true

    local node registry_dir
    registry_dir="/etc/containerd/certs.d/${REGISTRY}"
    while read -r node; do
        docker exec "${node}" mkdir -p "${registry_dir}"
        printf '[host."http://%s:5000"]\n' "${REGISTRY_NAME}" | \
            docker exec -i "${node}" cp /dev/stdin "${registry_dir}/hosts.toml"
    done < <(kind get nodes --name "${CLUSTER_NAME}")

    kubectl_e2e apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "${REGISTRY}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF
}

build_and_push_images() {
    local image
    log "building and pushing images to ${REGISTRY}"

    make -C "${REPO_ROOT}" image-net-controller-local \
        VERSION="${VERSION}" CONTAINER_REGISTRY="${REGISTRY}/azure" \
        CONTAINER_ENGINE="${CONTAINER_ENGINE}"
    make -C "${REPO_ROOT}" image-net-node-local \
        VERSION="${VERSION}" CONTAINER_REGISTRY="${REGISTRY}/azure" \
        CONTAINER_ENGINE="${CONTAINER_ENGINE}"
    make -C "${REPO_ROOT}" image-machina-local \
        VERSION="${VERSION}" CONTAINER_REGISTRY="${REGISTRY}/azure" \
        CONTAINER_ENGINE="${CONTAINER_ENGINE}"
    make -C "${REPO_ROOT}" image-unbounded-operator-local \
        VERSION="${VERSION}" CONTAINER_REGISTRY="${REGISTRY}/azure" \
        CONTAINER_ENGINE="${CONTAINER_ENGINE}"

    for image in unbounded-net-controller unbounded-net-node machina unbounded-operator; do
        "${CONTAINER_ENGINE}" push "${REGISTRY}/azure/${image}:${VERSION}"
    done
}

patch_release_manifest() {
    local source_manifest patched_manifest
    source_manifest="${REPO_ROOT}/build/unbounded-operator-${VERSION}.yaml"
    patched_manifest="${WORKDIR}/unbounded-operator-${VERSION}-mirrored.yaml"

    make -C "${REPO_ROOT}" unbounded-operator-release-manifest VERSION="${VERSION}"

    grep -Fq "ghcr.io/azure/unbounded-operator:${VERSION}" "${source_manifest}" || \
        die "release artifact does not contain the expected GHCR operator image"
    grep -Fq 'UNBOUNDED_IMAGE_REGISTRY: "ghcr.io"' "${source_manifest}" || \
        die "release artifact does not contain the expected GHCR component registry"

    sed "s#ghcr\.io#${REGISTRY}#g" "${source_manifest}" >"${patched_manifest}"

    if grep -Fq 'ghcr.io' "${patched_manifest}"; then
        die "patched release artifact still references ghcr.io"
    fi
    grep -Fq "${REGISTRY}/azure/unbounded-operator:${VERSION}" "${patched_manifest}" || \
        die "patched release artifact does not reference the mirrored operator image"
    grep -Fq "UNBOUNDED_IMAGE_REGISTRY: \"${REGISTRY}\"" "${patched_manifest}" || \
        die "patched release artifact does not configure the mirrored component registry"
}

create_site() {
    local node_ip node_cidr
    node_ip=$(kubectl_e2e get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
    [[ "${node_ip}" =~ ^([0-9]+)\.([0-9]+)\. ]] || die "unexpected kind node InternalIP: ${node_ip}"
    node_cidr="${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.0.0/16"

    kubectl_e2e apply -f - <<EOF
apiVersion: unbounded-cloud.io/v1alpha3
kind: Site
metadata:
  name: kind
spec:
  nodeCidrs:
    - ${node_cidr}
  manageCniPlugin: false
  podCidrAssignments:
    - cidrBlocks:
        - 10.244.0.0/16
      assignmentEnabled: false
  components:
    machina:
      enabled: true
EOF
}

assert_image() {
    local resource=$1 expected=$2 actual
    actual=$(kubectl_e2e -n "${NAMESPACE}" get "${resource}" \
        -o jsonpath='{.spec.template.spec.containers[*].image}')
    [[ "${actual}" == "${expected}" ]] || \
        die "${resource} image is ${actual}, expected ${expected}"
}

deploy_and_verify() {
    local manifest=$1 images
    log "applying the sed-patched single-file release artifact"
    kubectl_e2e apply -f "${manifest}"
    kubectl_e2e wait --for=condition=Established \
        crd/sites.unbounded-cloud.io crd/sitenodeslices.net.unbounded-cloud.io \
        --timeout=300s
    kubectl_e2e -n "${NAMESPACE}" rollout status deploy/unbounded-operator --timeout=360s

    assert_image deploy/unbounded-operator "${REGISTRY}/azure/unbounded-operator:${VERSION}"
    create_site

    kubectl_e2e -n "${NAMESPACE}" rollout status deploy/unbounded-net-controller --timeout=600s
    kubectl_e2e -n "${NAMESPACE}" rollout status ds/unbounded-net-node --timeout=600s
    kubectl_e2e -n "${NAMESPACE}" rollout status deploy/machina-controller --timeout=600s

    assert_image deploy/unbounded-net-controller "${REGISTRY}/azure/unbounded-net-controller:${VERSION}"
    assert_image ds/unbounded-net-node "${REGISTRY}/azure/unbounded-net-node:${VERSION}"
    assert_image deploy/machina-controller "${REGISTRY}/azure/machina:${VERSION}"

    images=$(kubectl_e2e -n "${NAMESPACE}" get deploy,ds \
        -o jsonpath='{range .items[*]}{range .spec.template.spec.containers[*]}{.image}{"\n"}{end}{end}')
    if grep -Fq 'ghcr.io' <<<"${images}"; then
        die "operator-managed workload images still reference ghcr.io"
    fi
}

main() {
    check_prerequisites
    mkdir -p "${WORKDIR}"
    start_registry
    create_cluster
    build_and_push_images

    patch_release_manifest
    deploy_and_verify "${WORKDIR}/unbounded-operator-${VERSION}-mirrored.yaml"

    FAILED=0
    log "PASS: the sed-patched release artifact runs without GHCR project images"
}

main "$@"
