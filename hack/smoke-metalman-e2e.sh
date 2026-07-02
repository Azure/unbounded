#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# Create or reuse the local Kind target cluster for the Playpen-backed
# metalman e2e suite, then run the Go tests. The tests themselves never create
# the cluster; this wrapper owns local developer setup.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-metalman-e2e}"
PLAYPEN_KUBECONFIG="${PLAYPEN_KUBECONFIG:-${HOME}/.kube/config.bench}"
KUBECONFIG_OUT="${METALMAN_E2E_KUBECONFIG:-${ROOT_DIR}/.kube/metalman-e2e.kubeconfig}"
GATEWAY_IP="${METALMAN_E2E_PLAYPEN_GATEWAY:-}"
KEEP_CLUSTER="${E2E_KEEP:-0}"
PLAYPEN_ENDPOINT="${METALMAN_E2E_PLAYPEN_ENDPOINT:-relay}"
RUN_TESTS=1

usage() {
  sed -n '2,18p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  cat <<EOF

Usage: $0 [flags]

Flags:
  --cluster-name NAME      Kind cluster name (default: ${KIND_CLUSTER_NAME})
  --playpen-kubeconfig P  Playpen kubeconfig (default: ${PLAYPEN_KUBECONFIG})
  --kubeconfig-out P      Target cluster kubeconfig path (default: ${KUBECONFIG_OUT})
  --gateway-ip IP         Playpen guest gateway IP; skips discovery allocation
  --prepare-only          Create/reuse Kind and print environment, but do not run tests
  -h, --help              Show this help

Environment:
  METALMAN_E2E_ARTIFACT_MODE=local|ghcr controls image source.
  METALMAN_E2E_PLAYPEN_ENDPOINT=direct|loadbalancer|relay controls WireGuard exposure.
  For ghcr mode, set METALMAN_E2E_HOST_IMAGE, METALMAN_E2E_NETBOOT_IMAGE,
  and METALMAN_E2E_AGENT_IMAGE before running this wrapper.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cluster-name) KIND_CLUSTER_NAME="$2"; shift 2 ;;
    --playpen-kubeconfig) PLAYPEN_KUBECONFIG="$2"; shift 2 ;;
    --kubeconfig-out) KUBECONFIG_OUT="$2"; shift 2 ;;
    --gateway-ip) GATEWAY_IP="$2"; shift 2 ;;
    --prepare-only) RUN_TESTS=0; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown flag: $1" >&2; usage >&2; exit 1 ;;
  esac
done

require() {
  command -v "$1" >/dev/null 2>&1 || { echo "required command missing: $1" >&2; exit 1; }
}

require docker
require go
require kind
require kubectl

if [[ ! -f "${PLAYPEN_KUBECONFIG}" ]]; then
  echo "Playpen kubeconfig not found: ${PLAYPEN_KUBECONFIG}" >&2
  exit 1
fi

if [[ -z "${GATEWAY_IP}" ]]; then
  echo ">> Discovering Playpen guest gateway via ${PLAYPEN_KUBECONFIG}" >&2
  DISCOVERY_OUTPUT="$(cd "${ROOT_DIR}" && PLAYPEN_KUBECONFIG="${PLAYPEN_KUBECONFIG}" METALMAN_E2E_PRINT_PLAYPEN_GATEWAY=1 go test -tags=e2e ./e2e/metalman -run TestPrintPlaypenGateway -count=1 -v 2>&1)"
  GATEWAY_IP="$(awk '/METALMAN_E2E_PLAYPEN_GATEWAY=/ { sub(/.*METALMAN_E2E_PLAYPEN_GATEWAY=/, ""); print; exit }' <<<"${DISCOVERY_OUTPUT}")"
fi

if [[ -z "${GATEWAY_IP}" ]]; then
  printf '%s\n' "${DISCOVERY_OUTPUT:-}" >&2
  echo "failed to discover Playpen gateway IP" >&2
  exit 1
fi

mkdir -p "$(dirname "${KUBECONFIG_OUT}")" "${ROOT_DIR}/tmp"
KIND_CONFIG="${ROOT_DIR}/tmp/metalman-e2e-kind.yaml"
cat >"${KIND_CONFIG}" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  apiServerAddress: 127.0.0.1
nodes:
- role: control-plane
  kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    apiServer:
      certSANs:
      - "${GATEWAY_IP}"
      - "127.0.0.1"
      - "localhost"
EOF

if kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER_NAME}"; then
  echo ">> Kind cluster '${KIND_CLUSTER_NAME}' already exists; reusing it" >&2
else
  echo ">> Creating Kind cluster '${KIND_CLUSTER_NAME}' with API cert SAN ${GATEWAY_IP}" >&2
  kind create cluster --name "${KIND_CLUSTER_NAME}" --config "${KIND_CONFIG}" --wait 120s
fi

kind get kubeconfig --name "${KIND_CLUSTER_NAME}" >"${KUBECONFIG_OUT}"
chmod 0600 "${KUBECONFIG_OUT}"

export PLAYPEN_KUBECONFIG
export METALMAN_E2E_KUBECONFIG="${KUBECONFIG_OUT}"
export METALMAN_E2E_APISERVER_URL="https://${GATEWAY_IP}:6443"
export METALMAN_E2E_PLAYPEN_ENDPOINT="${PLAYPEN_ENDPOINT}"

echo ">> Target kubeconfig: ${METALMAN_E2E_KUBECONFIG}" >&2
echo ">> Guest-reachable API URL: ${METALMAN_E2E_APISERVER_URL}" >&2
echo ">> Playpen WireGuard endpoint mode: ${METALMAN_E2E_PLAYPEN_ENDPOINT}" >&2

if [[ "${RUN_TESTS}" == "0" ]]; then
  cat <<EOF
export PLAYPEN_KUBECONFIG="${PLAYPEN_KUBECONFIG}"
export METALMAN_E2E_KUBECONFIG="${METALMAN_E2E_KUBECONFIG}"
export METALMAN_E2E_APISERVER_URL="${METALMAN_E2E_APISERVER_URL}"
export METALMAN_E2E_PLAYPEN_ENDPOINT="${METALMAN_E2E_PLAYPEN_ENDPOINT}"
EOF
  exit 0
fi

if [[ "${KEEP_CLUSTER}" != "1" ]]; then
  trap 'kind delete cluster --name "${KIND_CLUSTER_NAME}" >/dev/null 2>&1 || true' EXIT
fi

cd "${ROOT_DIR}"
make e2e-metalman
