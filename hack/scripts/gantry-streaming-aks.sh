#!/usr/bin/env bash
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

# gantry-streaming-aks.sh -- Stand up a throwaway AKS cluster and Premium ACR
# for testing Gantry against ACR Artifact Streaming, with streaming enabled
# from the start rather than staged on afterwards.
#
# Usage:
#   ACR=<globally-unique-name> hack/scripts/gantry-streaming-aks.sh up
#   hack/scripts/gantry-streaming-aks.sh cleanup-operator
#   hack/scripts/gantry-streaming-aks.sh install
#   hack/scripts/gantry-streaming-aks.sh verify
#   hack/scripts/gantry-streaming-aks.sh uninstall
#   hack/scripts/gantry-streaming-aks.sh down
#
# Environment:
#   ACR             required on 'up'; must be globally unique
#   RESOURCE_GROUP  default gantry-streaming
#   LOCATION        default westus3
#   CLUSTER         default gantry-streaming
#   STREAM_POOL     default stream
#   STREAM_NODES    default 2; peer reuse cannot be observed with fewer
#   NODE_LABEL      default gantry-streaming=true
#   NODE_VM_SIZE    default Standard_D4s_v5
#   IMAGE_TAG        default current git describe; shared by both Gantry images
#
# Artifact streaming is an AKS/ACR preview feature and can only be enabled when
# a node pool is created, never on an existing pool. The az flags below move
# with the preview; check the current Azure docs if one is rejected.
#
# Azure operations here are long-running. Interrupting this script does not
# cancel the server-side operation: re-run 'up' to converge, or 'down' to
# remove everything.

set -euo pipefail

RESOURCE_GROUP="${RESOURCE_GROUP:-gantry-streaming}"
LOCATION="${LOCATION:-westus3}"
CLUSTER="${CLUSTER:-gantry-streaming}"
ACR="${ACR:-}"
STREAM_POOL="${STREAM_POOL:-stream}"
STREAM_NODES="${STREAM_NODES:-2}"
SYSTEM_NODES="${SYSTEM_NODES:-1}"
NODE_LABEL="${NODE_LABEL:-gantry-streaming=true}"
NODE_VM_SIZE="${NODE_VM_SIZE:-Standard_D4s_v5}"
NAMESPACE="${NAMESPACE:-unbounded-system}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
IMAGE_TAG="${IMAGE_TAG:-$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)}"
IMAGE_TAG="${IMAGE_TAG//\//-}"

log() { printf '\n== %s\n' "$*"; }
die() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

require_tools() {
	for bin in "$@"; do
		command -v "$bin" >/dev/null 2>&1 || die "missing required tool: $bin"
	done
}

require_azure() {
	require_tools az
	az account show >/dev/null 2>&1 || die "run 'az login' first"
}

cmd_up() {
	require_azure
	require_tools kubectl make
	[ -n "$ACR" ] || die "set ACR to a globally unique registry name"

	log "Enabling the aks-preview extension"
	az extension add --name aks-preview --upgrade --only-show-errors >/dev/null

	log "Resource group $RESOURCE_GROUP in $LOCATION"
	az group create -n "$RESOURCE_GROUP" -l "$LOCATION" --only-show-errors >/dev/null

	# Artifact streaming requires a Premium registry.
	log "Premium ACR $ACR"
	if ! az acr show -n "$ACR" >/dev/null 2>&1; then
		az acr create -n "$ACR" -g "$RESOURCE_GROUP" --sku Premium --only-show-errors >/dev/null
	fi

	log "AKS cluster $CLUSTER (long-running)"
	if ! az aks show -n "$CLUSTER" -g "$RESOURCE_GROUP" >/dev/null 2>&1; then
		# Overlay is pinned so the network profile carries a podCidr, which
		# get-aks-cluster-cidrs.sh needs to build the 'site init' command.
		az aks create -n "$CLUSTER" -g "$RESOURCE_GROUP" \
			--node-count "$SYSTEM_NODES" --node-vm-size "$NODE_VM_SIZE" \
			--network-plugin azure --network-plugin-mode overlay \
			--attach-acr "$ACR" --generate-ssh-keys --only-show-errors >/dev/null
	fi

	log "Artifact streaming node pool $STREAM_POOL (${STREAM_NODES} nodes)"
	if ! az aks nodepool show --cluster-name "$CLUSTER" -g "$RESOURCE_GROUP" -n "$STREAM_POOL" >/dev/null 2>&1; then
		az aks nodepool add --cluster-name "$CLUSTER" -g "$RESOURCE_GROUP" -n "$STREAM_POOL" \
			--node-count "$STREAM_NODES" --node-vm-size "$NODE_VM_SIZE" \
			--enable-artifact-streaming --labels "$NODE_LABEL" --only-show-errors >/dev/null
	fi

	log "Fetching kubeconfig"
	az aks get-credentials -n "$CLUSTER" -g "$RESOURCE_GROUP" --overwrite-existing --only-show-errors

	cmd_images

	cat <<EOF

Cluster is up. Next:

	hack/scripts/gantry-streaming-aks.sh cleanup-operator  # only after an operator install
  hack/scripts/gantry-streaming-aks.sh install
  hack/scripts/gantry-streaming-aks.sh verify

Push a streaming test image and enable streaming on its repository:

  az acr artifact-streaming update -n $ACR --repository <repo> --enable true

EOF
}

cmd_images() {
	require_azure
	require_tools make
	[ -n "$ACR" ] || die "set ACR to the registry holding the test images"

	log "Building and pushing images to $ACR"
	az acr login -n "$ACR" --only-show-errors >/dev/null
	make -C "$REPO_ROOT" image-gantry-push image-gantry-node-config-push \
		CONTAINER_REGISTRY="${ACR}.azurecr.io" VERSION="$IMAGE_TAG"
}

cmd_cleanup_operator() {
	require_tools kubectl

	local manager
	manager="$(kubectl get priorityclass gantry-low \
		-o jsonpath='{.metadata.annotations.gantry\.unbounded-cloud\.io/manager}' 2>/dev/null || true)"
	if [ "$manager" = "helm" ]; then
		die "refusing operator cleanup: Gantry is managed by Helm"
	fi

	if [ "$manager" != "unbounded-operator" ] && \
		! kubectl -n "$NAMESPACE" get deployment/unbounded-operator >/dev/null 2>&1; then
		die "no unbounded-operator installation found"
	fi

	log "Stopping unbounded-operator"
	kubectl -n "$NAMESPACE" scale deployment/unbounded-operator --replicas=0 2>/dev/null || true

	log "Restoring OverlayBD before removing operator-managed Gantry"
	kubectl -n "$NAMESPACE" delete daemonset/gantry-overlaybd-config --ignore-not-found --wait=true --timeout=2m

	log "Removing operator namespace and cluster-scoped resources"
	kubectl delete namespace "$NAMESPACE" --ignore-not-found --wait=true --timeout=3m
	kubectl delete clusterrole \
		unbounded-operator unbounded-net-controller unbounded-net-node \
		unbounded-net-status-viewer token-refresher --ignore-not-found
	kubectl delete clusterrolebinding \
		unbounded-operator unbounded-net-controller unbounded-net-node \
		unbounded-net-kube-proxy token-refresher --ignore-not-found
	kubectl delete apiservice \
		v1alpha1.net.unbounded-cloud.io v1alpha3.unbounded-cloud.io --ignore-not-found
	kubectl delete priorityclass gantry-low --ignore-not-found

	local crds
	crds="$(kubectl get crd -o name 2>/dev/null | grep -E '(\.unbounded-cloud\.io$)' || true)"
	if [ -n "$crds" ]; then
		printf '%s\n' "$crds" | xargs kubectl delete
	fi
}

cmd_install() {
	require_tools kubectl make
	[ -n "$ACR" ] || die "set ACR to the registry holding the Gantry images"

	local helm="$REPO_ROOT/bin/helm"
	[ -x "$helm" ] || make -C "$REPO_ROOT" install-helm

	local key="${NODE_LABEL%%=*}"
	local value="${NODE_LABEL#*=}"
	local registry="${ACR}.azurecr.io"

	kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

	log "Installing Gantry with AKS Artifact Streaming"
	"$helm" upgrade --install gantry "$REPO_ROOT/deploy/gantry/chart" \
		--namespace "$NAMESPACE" \
		--set-string "image.reference=${registry}/gantry:${IMAGE_TAG}" \
		--set image.pullPolicy=Always \
		--set-string "overlaybdConfig.image.reference=${registry}/gantry-node-config:${IMAGE_TAG}" \
		--set overlaybdConfig.image.pullPolicy=Always \
		--set-string "nodeSelector.${key}=${value}" \
		--set overlaybdConfig.enabled=true \
		--set-string "overlaybdConfig.nodeSelector.${key}=${value}" \
		--set gantry.artifactStreaming.enabled=true \
		--set-string "gantry.upstreamRegistries[0].name=${registry}" \
		--set-string "gantry.upstreamRegistries[0].endpoint=http://127.0.0.1:8578?ns=${registry}" \
		--wait --timeout 5m
}

cmd_uninstall() {
	require_tools kubectl

	local helm="$REPO_ROOT/bin/helm"
	[ -x "$helm" ] || die "missing Helm binary: run 'make install-helm'"

	log "Uninstalling Gantry"
	"$helm" uninstall gantry --namespace "$NAMESPACE" --wait --timeout 3m
}

# gantry-overlaybd-config reports ready only once the host config and the
# running OverlayBD services agree, so its rollout is the host-side check.
cmd_verify() {
	require_tools kubectl

	log "Gantry agent rollout"
	kubectl -n "$NAMESPACE" rollout status ds/gantry --timeout=5m

	log "Containerd mirror configurator rollout"
	kubectl -n "$NAMESPACE" rollout status ds/gantry-containerd-config --timeout=5m

	log "OverlayBD configurator rollout"
	kubectl -n "$NAMESPACE" rollout status ds/gantry-overlaybd-config --timeout=5m

	log "Streaming metrics"
	local pod
	pod="$(kubectl -n "$NAMESPACE" get pods \
		-l 'app.kubernetes.io/name=gantry,app.kubernetes.io/component=agent' \
		-o jsonpath='{.items[0].metadata.name}')"
	[ -n "$pod" ] || die "no gantry pod found in $NAMESPACE"

	kubectl -n "$NAMESPACE" port-forward "pod/$pod" 19095:9095 >/dev/null 2>&1 &
	local forward=$!
	trap "kill '$forward' 2>/dev/null || true" EXIT

	local attempt metrics
	for attempt in $(seq 1 20); do
		if metrics="$(curl -fsS --max-time 2 http://127.0.0.1:19095/metrics 2>/dev/null)" &&
			printf '%s\n' "$metrics" | grep -E '^gantry_streaming_(requests|rejected)_total'; then
			return 0
		fi

		[ "$attempt" -lt 20 ] || die "streaming metrics unavailable on $pod"
		sleep 1
	done
}

cmd_down() {
	require_azure

	log "Deleting resource group $RESOURCE_GROUP"
	az group delete -n "$RESOURCE_GROUP" --yes --no-wait --only-show-errors
	echo "Deletion runs in the background; 'az group show -n $RESOURCE_GROUP' reports progress."
}

case "${1:-}" in
up) cmd_up ;;
images) cmd_images ;;
cleanup-operator) cmd_cleanup_operator ;;
install) cmd_install ;;
verify) cmd_verify ;;
uninstall) cmd_uninstall ;;
down) cmd_down ;;
*)
	echo "usage: $0 {up|images|cleanup-operator|install|verify|uninstall|down}" >&2
	exit 2
	;;
esac
