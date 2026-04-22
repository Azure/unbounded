# hack/net/Makefile - cluster deploy/undeploy targets for unbounded-net.
#
# Invoke from the repo root: `make -C hack/net <target>`.
#
# These targets apply rendered manifests to whatever cluster the current
# kubectl context points at. Rendering is auto-invoked from the root
# Makefile via `make net-manifests`.

REPO_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)
SCRIPTS_DIR := $(dir $(lastword $(MAKEFILE_LIST)))

# Knobs.
NET_NAMESPACE       ?= unbounded-net
NET_LOG_LEVEL       ?=
NET_AZURE_TENANT_ID ?=

# Paths into the repo (not user-overridable).
NET_CRD_DIR  := $(REPO_ROOT)/deploy/net/crd
RENDERED_DIR := $(REPO_ROOT)/deploy/net/rendered

.PHONY: help render deploy deploy-crds deploy-namespace deploy-config deploy-controller deploy-node undeploy

help: ## Show this help
	@echo ""
	@echo "Usage: make -C hack/net <target> [VAR=value ...]"
	@echo ""
	@echo "Cluster deploy targets (apply to current kubectl context):"
	@echo "  deploy              Deploy CRDs, namespace, config, controller, node"
	@echo "  deploy-crds         Apply CRDs only"
	@echo "  deploy-namespace    Apply namespace only (skipped when NET_NAMESPACE=kube-system)"
	@echo "  deploy-config       Apply configmap (and patch when NET_LOG_LEVEL or NET_AZURE_TENANT_ID set)"
	@echo "  deploy-controller   Apply controller manifests and roll the deployment"
	@echo "  deploy-node         Apply node DaemonSet manifests and roll it"
	@echo "  undeploy            Tear down all net resources in dependency order"
	@echo "  render              Render manifests via the root Makefile"
	@echo ""
	@echo "Variables:"
	@echo "  NET_NAMESPACE=$(NET_NAMESPACE)"
	@echo "  NET_LOG_LEVEL=$(NET_LOG_LEVEL)"
	@echo "  NET_AZURE_TENANT_ID=$(NET_AZURE_TENANT_ID)"

render:
	$(MAKE) -C $(REPO_ROOT) net-manifests

deploy: deploy-crds deploy-namespace deploy-config deploy-controller deploy-node ## Apply all net components and roll workloads

deploy-crds: ## Apply the net CRDs
	kubectl apply -f "$(NET_CRD_DIR)/"

deploy-namespace: render ## Apply the net namespace (skipped when NET_NAMESPACE=kube-system)
	@if [ "$(NET_NAMESPACE)" != "kube-system" ]; then \
		kubectl apply -f "$(RENDERED_DIR)/00-namespace.yaml"; \
	fi

deploy-config: deploy-namespace render ## Apply the net configmap; patches LOG_LEVEL/AZURE_TENANT_ID when set
	NAMESPACE="$(NET_NAMESPACE)" \
	RENDERED_DIR="$(RENDERED_DIR)" \
	LOG_LEVEL="$(NET_LOG_LEVEL)" \
	AZURE_TENANT_ID="$(NET_AZURE_TENANT_ID)" \
		$(SCRIPTS_DIR)deploy-config.sh

deploy-controller: deploy-namespace deploy-crds deploy-config render ## Apply controller manifests and roll the deployment
	NAMESPACE="$(NET_NAMESPACE)" \
	RENDERED_DIR="$(RENDERED_DIR)" \
		$(SCRIPTS_DIR)deploy-controller.sh

deploy-node: deploy-namespace deploy-crds deploy-config render ## Apply node DaemonSet manifests and roll it
	NAMESPACE="$(NET_NAMESPACE)" \
	RENDERED_DIR="$(RENDERED_DIR)" \
		$(SCRIPTS_DIR)deploy-node.sh

undeploy: render ## Tear down all net resources in dependency order
	NAMESPACE="$(NET_NAMESPACE)" \
	RENDERED_DIR="$(RENDERED_DIR)" \
	CRD_DIR="$(NET_CRD_DIR)" \
		$(SCRIPTS_DIR)undeploy.sh
