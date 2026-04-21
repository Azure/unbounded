# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# unbounded-net Makefile

# Include guard: prevent double-load when MAKEFILES env var is set
ifdef _UNBOUNDED_NET_MAKEFILE
# already loaded
else
_UNBOUNDED_NET_MAKEFILE := 1

# Registry settings
# Repo root: directory containing this Makefile, so targets work from any cwd.
REPO_ROOT := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))

REGISTRY := unboundednettmebuild.azurecr.io
BUILD_ACR_NAME := unboundednettmebuild
PUBLISH_REGISTRY := unboundednettme.azurecr.io
PUBLISH_ACR_NAME := unboundednettme
ACR_SUBSCRIPTION := 110efc33-11a4-46b9-9986-60716283fbe7
ACR_TENANT := 70a036f6-8e4d-4615-bad6-149c02e7720d
ACR_RESOURCE_GROUP := unbounded-net-acr
ifndef VERSION
VERSION := $(shell git -C "$(REPO_ROOT)" describe --tags --always --dirty 2>/dev/null || echo "dev")
endif
ifndef COMMIT
COMMIT := $(shell git -C "$(REPO_ROOT)" rev-parse --short HEAD 2>/dev/null || echo "unknown")
endif
ifndef BUILD_TIME
BUILD_TIME := $(shell date -u '+%Y-%m-%d %H:%M:%S UTC')
endif
CNI_PLUGINS_VERSION ?= v1.9.0
NAMESPACE ?= unbounded-net
RESOURCES_DIR ?= $(REPO_ROOT)resources
GOOS ?= linux
GOARCH ?= amd64
FRONTEND_DIR := frontend
FRONTEND_DIST_DIR := internal/net/html/dist
FRONTEND_CACHE_FILE := $(FRONTEND_DIST_DIR)/.frontend-build-key
MANIFEST_TEMPLATES_DIR := deploy/net
MANIFEST_RENDERED_DIR  := deploy/net/rendered
GO_IN_REPO = cd $(REPO_ROOT) && $(GO)
CONTROLLER_GEN_IN_REPO = cd $(REPO_ROOT) && $(GO) tool controller-gen

# Variables - Controller
CONTROLLER_BINARY_NAME := unbounded-net-controller
CONTROLLER_IMAGE_NAME := $(PUBLISH_REGISTRY)/$(CONTROLLER_BINARY_NAME)
UNBOUNDED_NET_CONTROLLER_IMAGE ?= $(CONTROLLER_IMAGE_NAME):$(VERSION)

# Variables - Node
NODE_BINARY_NAME := unbounded-net-node
NODE_IMAGE_NAME := $(PUBLISH_REGISTRY)/$(NODE_BINARY_NAME)
UNBOUNDED_NET_NODE_IMAGE ?= $(NODE_IMAGE_NAME):$(VERSION)

# Multi-arch settings
PLATFORMS := linux/amd64,linux/arm64
ARCHES := amd64 arm64

# Remote builder settings (for adding remote build nodes)
REMOTE_BUILDER_NODE ?=
REMOTE_BUILDER_HOST ?=
REMOTE_BUILDER_PLATFORM ?=

# Go settings
GO := go
GOFLAGS := -v
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X 'main.BuildTime=$(BUILD_TIME)'
RESHIM_ASDF_GO := if [ "$$(command -v $(GO))" = "$$HOME/.asdf/shims/go" ] && command -v asdf >/dev/null 2>&1; then asdf reshim; fi

# Progress display settings
PROGRESS ?= auto
NO_CACHE ?= false
REACT_DEV ?= false

# Log level override (klog -v=<N>); when set, overrides the default in deploy manifests
LOG_LEVEL ?= 2
FORCE_NOT_LEADER ?= false
AZURE_TENANT_ID ?=
APISERVER_URL ?= $(shell kubectl config view --flatten --minify --template '{{ (index .clusters 0).cluster.server }}' 2>/dev/null)
LOG_LEVEL_FROM_ARGS := $(if $(filter command line,$(origin LOG_LEVEL)),1,0)
AZURE_TENANT_ID_FROM_ARGS := $(if $(filter command line,$(origin AZURE_TENANT_ID)),1,0)

BAKE_FILE := $(REPO_ROOT)docker/docker-bake.hcl
BAKE_PLATFORMS ?= $(PLATFORMS)
BAKE_TARGETS ?=
BAKE_OUTPUT ?=
BAKE_MESSAGE ?= Running docker buildx bake...
BAKE_ENV := REGISTRY=$(REGISTRY) VERSION=$(VERSION)
VERSION_ORIGIN := $(origin VERSION)
SEMVER_V_REGEX := ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?(\+([0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*))?$$


.PHONY: all test bake build build-local build-controller build-node build-plugin build-setup build-check-platforms build-add-remote docker-login deploy fmt vet lint help
.PHONY: frontend frontend-clean controller node
.PHONY: deploy-config render validate-version

## Default target - generate, quality checks, then build+push
all: generate fmt vet lint build

## Validate explicit VERSION values as v-prefixed semver (e.g. v1.2.3)
validate-version:
	@if [ "$(VERSION_ORIGIN)" = "command line" ] || [ "$(VERSION_ORIGIN)" = "environment" ]; then \
		if ! printf '%s' "$(VERSION)" | grep -Eq '$(SEMVER_V_REGEX)'; then \
			echo "Error: VERSION must be valid semver with leading v (for example: v0.5.2)"; \
			exit 1; \
		fi; \
	fi

## Build and deploy controller (frontend, build, deploy)
controller: build-controller deploy-controller

## Build and deploy node (build, deploy)
node: build-ebpf build-node deploy-node

#
# eBPF Targets
#

## Compile eBPF programs (requires clang)
build-ebpf:
	@echo "Compiling eBPF programs..."
	@cd $(REPO_ROOT) && clang -O2 -g -target bpf \
		-I/usr/include \
		-c bpf/unbounded_encap.c \
		-o internal/net/ebpf/unbounded_encap_bpfel.o
	@echo "eBPF programs compiled."

#
# Test Targets
#

## Run tests
test:
	$(GO_IN_REPO) test -v -race ./...

## Run tests with coverage
coverage:
	$(GO_IN_REPO) test -v -race -coverprofile=coverage.out ./...
	$(GO_IN_REPO) tool cover -html=coverage.out -o coverage.html

#
# Code Quality Targets
#

## Format code
fmt:
	$(GO_IN_REPO) fmt ./...

## Run go vet
vet:
	$(GO_IN_REPO) vet ./...

## Run golangci-lint (must match CI version in .github/workflows/lint.yml)
GOLANGCI_LINT_VERSION := v2.11.4
lint:
	@if ! command -v golangci-lint >/dev/null 2>&1 || ! golangci-lint version 2>/dev/null | grep -q 'v2\.'; then \
		echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."; \
		$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
		$(RESHIM_ASDF_GO); \
	fi
	cd $(REPO_ROOT) && golangci-lint run ./...

## Download dependencies
deps:
	$(GO_IN_REPO) mod download
	$(GO_IN_REPO) mod tidy

## Build kubectl-unbounded-net plugin binary for all architectures (cached)
PLUGIN_CACHE_FILE := $(REPO_ROOT)build/kubectl-plugin/.plugin-build-key

build-plugin:
	@mkdir -p $(REPO_ROOT)build/kubectl-plugin
	@plugin_key="$$(find $(REPO_ROOT)tools/kubectl-plugin $(REPO_ROOT)pkg/status -name '*.go' 2>/dev/null | sort | xargs cat 2>/dev/null | sha256sum | awk '{print $$1}')-$$(sha256sum $(REPO_ROOT)go.mod $(REPO_ROOT)go.sum 2>/dev/null | sha256sum | awk '{print $$1}')"; \
	if [ -f "$(PLUGIN_CACHE_FILE)" ] && [ "$$(cat "$(PLUGIN_CACHE_FILE)")" = "$$plugin_key" ]; then \
		echo "kubectl plugin unchanged; using cached build"; \
		exit 0; \
	fi; \
	for arch in $(ARCHES); do \
		for os in linux darwin windows; do \
			ext=""; \
			if [ "$$os" = "windows" ]; then ext=".exe"; fi; \
			echo "Building kubectl-unbounded-net for $$os/$$arch..."; \
			goexp=; \
			if test -f "$$($(GO) env GOROOT)/src/crypto/systemcrypto_nocgo_linux.go"; then goexp=nosystemcrypto; fi; \
			cd $(REPO_ROOT) && CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch GOEXPERIMENT=$$goexp $(GO) build -ldflags "$(LDFLAGS)" \
				-o build/kubectl-plugin/kubectl-unbounded-net-$$os-$$arch$$ext \
				./tools/kubectl-plugin/cmd/kubectl-unbounded-net; \
		done; \
	done; \
	echo "$$plugin_key" > "$(PLUGIN_CACHE_FILE)"
	@# Symlink the native binary for local use
	@ln -sf kubectl-unbounded-net-$$(go env GOOS)-$$(go env GOARCH) \
		$(REPO_ROOT)build/kubectl-plugin/kubectl-unbounded-net 2>/dev/null || true

## Generate code (DeepCopy functions, CRDs, etc.)
generate:
	$(CONTROLLER_GEN_IN_REPO) object paths="./pkg/apis/..."
	find "$(REPO_ROOT)deploy/crds" -maxdepth 1 -type f -name "*.yaml" -delete
	$(CONTROLLER_GEN_IN_REPO) crd:crdVersions=v1 paths="./pkg/apis/..." output:crd:artifacts:config="$(REPO_ROOT)deploy/crds"

## Generate protobuf Go code (requires protoc and protoc-gen-go in PATH)
.PHONY: generate-proto
generate-proto:
	protoc --go_out=. --go_opt=paths=source_relative pkg/healthcheck/proto/healthcheck.proto
	protoc --go_out=. --go_opt=paths=source_relative pkg/status/proto/status.proto

## Build frontend assets locally for controller image
frontend:
	@set -e; \
	repo_root="$(REPO_ROOT)"; \
	frontend_key="$$( \
		git -C "$$repo_root" ls-files -co --exclude-standard -- $(FRONTEND_DIR) | LC_ALL=C sort | while read -r file; do \
			if [ -f "$$repo_root/$$file" ]; then sha256sum "$$repo_root/$$file"; fi; \
		done | sha256sum | awk '{print $$1}' \
	)-react_dev=$(REACT_DEV)"; \
	if [ -d "$$repo_root/$(FRONTEND_DIST_DIR)" ] && [ -f "$$repo_root/$(FRONTEND_CACHE_FILE)" ] && [ "$$(cat "$$repo_root/$(FRONTEND_CACHE_FILE)")" = "$$frontend_key" ]; then \
		echo "Frontend unchanged; using cached $(FRONTEND_DIST_DIR)"; \
		exit 0; \
	fi; \
	( cd "$$repo_root/$(FRONTEND_DIR)" && \
		if [ -f package-lock.json ]; then npm ci --prefer-offline --no-audit; else npm install; fi && \
		if [ "$(REACT_DEV)" = "true" ] || [ "$(REACT_DEV)" = "1" ]; then \
			NODE_ENV=development npm run build -- --mode development --minify false --sourcemap; \
	else \
			npm run build; \
		fi \
	); \
	rm -rf "$$repo_root/$(FRONTEND_DIST_DIR)"; \
	mkdir -p "$$repo_root/$(FRONTEND_DIST_DIR)"; \
	cp -R "$$repo_root/$(FRONTEND_DIR)/dist/." "$$repo_root/$(FRONTEND_DIST_DIR)/"; \
	printf '%s\n' "$$frontend_key" > "$$repo_root/$(FRONTEND_CACHE_FILE)"

## Clean frontend generated artifacts
frontend-clean:
	rm -rf "$(REPO_ROOT)$(FRONTEND_DIR)/node_modules" "$(REPO_ROOT)$(FRONTEND_DIR)/dist" "$(REPO_ROOT)$(FRONTEND_DIST_DIR)"

#
# Docker Targets - Common
#

## Login to Azure Container Registries (build + publish)
docker-login:
	@for acr in $(BUILD_ACR_NAME) $(PUBLISH_ACR_NAME); do \
		registry="$$acr.azurecr.io"; \
		if ! jq -e -r ".auths.\"$$registry\".identitytoken | split(\".\")[1] | @base64d | fromjson | .exp > (now + 15*60)" $(HOME)/.docker/config.json >/dev/null 2>&1; then \
			echo "$$registry docker login token missing or expiring soon, logging in..."; \
			if ! az account show --subscription $(ACR_SUBSCRIPTION) --query tenantId -o tsv >/dev/null 2>&1; then \
				echo "Can't query ACR subscription, signing in to tenant $(ACR_TENANT)..."; \
				az login --tenant $(ACR_TENANT); \
			fi; \
			az acr login --subscription $(ACR_SUBSCRIPTION) -g $(ACR_RESOURCE_GROUP) -n $$acr; \
		fi; \
	done

#
# Build Targets - Controller
#
## Build and push multi-arch Docker images for controller
## Builds to local OCI tarballs then pushes to registry
build-controller: BAKE_TARGETS=controller
build-controller: BAKE_MESSAGE=Building controller for $(PLATFORMS)...
build-controller: validate-version frontend build-setup build-check-platforms docker-login bake push-images
#
# Build Targets - Node
#
## Build and push multi-arch Docker images for node
## Builds to local OCI tarballs then pushes to registry
build-node: BAKE_TARGETS=node
build-node: BAKE_MESSAGE=Building node for $(PLATFORMS)...
build-node: validate-version build-setup build-check-platforms docker-login bake push-images
#
# Build Targets - Combined
#
## Build and push multi-arch Docker images for both using bake
## Builds to local OCI tarballs then pushes to registry
build: BAKE_MESSAGE=Building controller and node for $(PLATFORMS)...
build: validate-version frontend build-setup build-check-platforms docker-login bake push-images build-plugin
	
## Build and load single-platform images into local Docker image store
build-local: BAKE_PLATFORMS=linux/$(GOARCH)
build-local: BAKE_MESSAGE=Building and loading images for linux/$(GOARCH)...
build-local: BAKE_OUTPUT=--set "*.output=type=docker"
build-local: validate-version frontend build-setup bake

## Internal helper to run docker buildx bake with shared settings
bake:
	@echo "$(BAKE_MESSAGE)"
	@no_cache_flag=""; \
	if [ "$(NO_CACHE)" = "true" ] || [ "$(NO_CACHE)" = "1" ]; then \
		no_cache_flag="--no-cache"; \
	fi; \
	$(BAKE_ENV) docker buildx bake $(BAKE_TARGETS) \
		--progress=$(PROGRESS) \
		--file $(BAKE_FILE) \
		--set "*.args.VERSION=$(VERSION)" \
		--set "*.args.COMMIT=$(COMMIT)" \
		--set "*.args.BUILD_TIME=$(BUILD_TIME)" \
		--set "*.args.CNI_PLUGINS_VERSION=$(CNI_PLUGINS_VERSION)" \
		--set "*.args.REACT_DEV=$(REACT_DEV)" \
		--set "*.platform=$(BAKE_PLATFORMS)" \
		$$no_cache_flag \
	$(BAKE_OUTPUT)

## Create multi-arch manifests and publish to the final registry.
## Bake pushes to the build registry (no replicas, fast).
## This target creates the final manifest in the build registry, then
## copies it to the publish registry.
push-images:
	@for image in controller node; do \
		scratch="$(REGISTRY)/unbounded-net-$$image:$(VERSION)-buildscratch"; \
		build_final="$(REGISTRY)/unbounded-net-$$image:$(VERSION)"; \
		publish_final="$(PUBLISH_REGISTRY)/unbounded-net-$$image:$(VERSION)"; \
		if docker buildx imagetools inspect "$$scratch" >/dev/null 2>&1; then \
			echo "Creating manifest $$build_final from $$scratch..."; \
			docker buildx imagetools create --tag "$$build_final" "$$scratch"; \
			echo "Publishing $$publish_final..."; \
			docker buildx imagetools create --tag "$$publish_final" "$$build_final"; \
		fi; \
	done

## Download required source bundles once before building to leverage caching and avoid redundant downloads across build targets
## Setup docker buildx builder for multi-arch builds
build-setup:
	@mkdir -p "$(RESOURCES_DIR)"
	@for arch in $(ARCHES); do \
		file="$(RESOURCES_DIR)/cni-plugins-linux-$$arch-$(CNI_PLUGINS_VERSION).tgz"; \
		if [ ! -s "$$file" ] || ! tar -tzf "$$file" >/dev/null 2>&1; then \
			echo "Downloading CNI plugins $$file..."; \
			curl -fsSL "https://github.com/containernetworking/plugins/releases/download/$(CNI_PLUGINS_VERSION)/cni-plugins-linux-$$arch-$(CNI_PLUGINS_VERSION).tgz" -o "$$file"; \
			if ! tar -tzf "$$file" >/dev/null 2>&1; then \
				echo "CNI plugins archive is invalid: $$file"; \
				exit 1; \
			fi; \
		fi; \
	done
	@if ! docker buildx inspect unbounded-net-builder >/dev/null 2>&1; then \
		echo "Creating buildx builder..."; \
		docker buildx create --name unbounded-net-builder --use --bootstrap; \
	else \
		docker buildx use unbounded-net-builder; \
	fi

## Verify the buildx builder supports all platforms in PLATFORMS
build-check-platforms:
	@builder_platforms=$$(docker buildx inspect unbounded-net-builder 2>/dev/null | grep "Platforms:" | sed 's/.*Platforms: *//' | tr ',' '\n' | sed 's/^ *//; s/\*$$//' | sort -u | tr '\n' ',' | sed 's/,$$//'); \
	missing=""; \
	for plat in $$(echo "$(PLATFORMS)" | tr ',' ' '); do \
		if ! echo "$$builder_platforms" | tr ',' '\n' | grep -qx "$$plat"; then \
			missing="$$missing $$plat"; \
		fi; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "Error: Builder is missing support for platform(s):$$missing"; \
		echo ""; \
		echo "Builder currently supports: $$builder_platforms"; \
		echo "Required platforms: $(PLATFORMS)"; \
		echo ""; \
		echo "To add a remote builder for a missing platform, use:"; \
		echo "  make build-add-remote REMOTE_BUILDER_NODE=<name> REMOTE_BUILDER_HOST=ssh://user@host REMOTE_BUILDER_PLATFORM=<platform>"; \
		echo ""; \
		exit 1; \
	fi
	@echo "All platforms verified: $(PLATFORMS)"

## Add a remote builder node for multi-arch builds
## Usage: make build-add-remote REMOTE_BUILDER_NODE=arm64-builder REMOTE_BUILDER_HOST=ssh://user@host REMOTE_BUILDER_PLATFORM=linux/arm64
build-add-remote:
	@if [ -z "$(REMOTE_BUILDER_NODE)" ]; then echo "Error: REMOTE_BUILDER_NODE is required"; exit 1; fi
	@if [ -z "$(REMOTE_BUILDER_HOST)" ]; then echo "Error: REMOTE_BUILDER_HOST is required (e.g., ssh://user@host or tcp://host:port)"; exit 1; fi
	@if ! docker buildx inspect unbounded-net-builder >/dev/null 2>&1; then \
		echo "Creating buildx builder..."; \
		docker buildx create --name unbounded-net-builder --use --bootstrap; \
	fi
	BUILDX_NO_DEFAULT_ATTESTATIONS=1 docker buildx create --name unbounded-net-builder \
		--append \
		--node $(REMOTE_BUILDER_NODE) \
		--platform $(REMOTE_BUILDER_PLATFORM) \
		--driver docker-container \
		--driver-opt network=host \
		$(REMOTE_BUILDER_HOST)
	docker buildx use unbounded-net-builder
	@echo "Bootstrapping remote builder (this may take a moment)..."
	docker buildx inspect --bootstrap

## Render deployment templates into deploy/net/rendered
render: validate-version generate render-manifests
	cd $(REPO_ROOT) && tar czf "build/unbounded-net-manifests-$(VERSION).tar.gz" -C $(MANIFEST_RENDERED_DIR) .
	@echo "Rendered manifests written to $(REPO_ROOT)$(MANIFEST_RENDERED_DIR)"
	@echo "Rendered manifests archive written to $(REPO_ROOT)build/unbounded-net-manifests-$(VERSION).tar.gz"

# Render net manifests into $(MANIFEST_RENDERED_DIR). All deploy-* targets depend
# on this; the rendered tree is gitignored and safe to overwrite.
render-manifests:
	@rm -rf $(REPO_ROOT)$(MANIFEST_RENDERED_DIR)
	@mkdir -p $(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/crds
	cd $(REPO_ROOT) && $(GO) run ./hack/cmd/render-manifests \
		--templates-dir "$(REPO_ROOT)$(MANIFEST_TEMPLATES_DIR)" \
		--output-dir "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)" \
		--set Namespace=$(NAMESPACE) \
		--set ControllerImage=$(UNBOUNDED_NET_CONTROLLER_IMAGE) \
		--set NodeImage=$(UNBOUNDED_NET_NODE_IMAGE) \
		--set ForceNotLeader=$(FORCE_NOT_LEADER) \
		--set AzureTenantID=$(AZURE_TENANT_ID) \
		--set ApiserverURL=$(APISERVER_URL)
	@cp $(REPO_ROOT)deploy/net/crds/*.yaml $(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/crds/

#
# Kubernetes Targets
#

## Deploy to Kubernetes (applies all manifests and restarts workloads if needed)
deploy: validate-version deploy-crds deploy-namespace deploy-config deploy-controller deploy-node

## Create or patch shared runtime config used by controller/node.
## - Creates configmap when missing.
## - Does not overwrite existing values with empty/null values.
## - If LOG_LEVEL is provided on command line, patches LOG_LEVEL first.
deploy-config: validate-version deploy-namespace render-manifests
	@set -e; \
	CM_NAME="unbounded-net-config"; \
	if ! kubectl get configmap $$CM_NAME -n $(NAMESPACE) >/dev/null 2>&1; then \
		echo "Creating $$CM_NAME configmap in namespace $(NAMESPACE)"; \
		kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/01-configmap.yaml"; \
	fi; \
	if [ "$(LOG_LEVEL_FROM_ARGS)" = "1" ] && [ -n "$(LOG_LEVEL)" ]; then \
		echo "Patching $$CM_NAME LOG_LEVEL=$(LOG_LEVEL)"; \
		kubectl patch configmap $$CM_NAME -n $(NAMESPACE) --type merge -p '{"data":{"LOG_LEVEL":"$(LOG_LEVEL)"}}'; \
	fi; \
	if [ "$(AZURE_TENANT_ID_FROM_ARGS)" = "1" ] && [ -n "$(AZURE_TENANT_ID)" ]; then \
		echo "Patching $$CM_NAME AZURE_TENANT_ID=$(AZURE_TENANT_ID)"; \
		kubectl patch configmap $$CM_NAME -n $(NAMESPACE) --type merge -p '{"data":{"AZURE_TENANT_ID":"$(AZURE_TENANT_ID)"}}'; \
	fi

## Deploy only the namespace (skipped when NAMESPACE=kube-system)
deploy-namespace: render-manifests
	@if [ "$(NAMESPACE)" != "kube-system" ]; then \
		kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/00-namespace.yaml"; \
	fi

## Deploy only the CRDs to Kubernetes
deploy-crds: generate
	kubectl apply -f "$(REPO_ROOT)deploy/net/crds/"

## Deploy only the controller to Kubernetes (and restart if needed)
deploy-controller: validate-version deploy-namespace deploy-crds deploy-config render-manifests
	@CTRL_GEN_BEFORE=$$(kubectl get deployment/unbounded-net-controller -n $(NAMESPACE) -o jsonpath='{.metadata.generation}' 2>/dev/null || echo "0"); \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/01-serviceaccount.yaml"; \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/02-rbac.yaml"; \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/04-service.yaml"; \
	kubectl delete service unbounded-net-webhook -n $(NAMESPACE) --ignore-not-found; \
	kubectl delete validatingadmissionpolicy unbounded-net-webhook-field-restriction --ignore-not-found; \
	kubectl delete validatingadmissionpolicybinding unbounded-net-webhook-field-restriction --ignore-not-found; \
	kubectl delete validatingadmissionpolicy unbounded-net-csr-restriction --ignore-not-found; \
	kubectl delete validatingadmissionpolicybinding unbounded-net-csr-restriction --ignore-not-found; \
	kubectl delete validatingadmissionpolicy unbounded-net-csr-approval-restriction --ignore-not-found; \
	kubectl delete validatingadmissionpolicybinding unbounded-net-csr-approval-restriction --ignore-not-found; \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/06-validatingwebhook.yaml"; \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/07-apiservice.yaml"; \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/08-mutatingwebhook.yaml"; \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/09-vap.yaml"; \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/10-status-viewer.yaml"; \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/03-deployment.yaml"; \
	CTRL_GEN_AFTER=$$(kubectl get deployment/unbounded-net-controller -n $(NAMESPACE) -o jsonpath='{.metadata.generation}'); \
	if [ "$$CTRL_GEN_BEFORE" = "$$CTRL_GEN_AFTER" ]; then \
		echo "Controller manifest unchanged, restarting to pick up new image..."; \
		kubectl rollout restart deployment/unbounded-net-controller -n $(NAMESPACE); \
	fi; \
	echo "Waiting for controller rollout to complete..."; \
	kubectl rollout status deployment/unbounded-net-controller -n $(NAMESPACE) --timeout=120s

## Deploy only the node DaemonSet to Kubernetes (and restart if needed)
deploy-node: validate-version deploy-crds deploy-namespace deploy-config render-manifests
	@NODE_GEN_BEFORE=$$(kubectl get daemonset/unbounded-net-node -n $(NAMESPACE) -o jsonpath='{.metadata.generation}' 2>/dev/null || echo "0"); \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/node/01-serviceaccount.yaml"; \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/node/02-rbac.yaml"; \
	kubectl apply -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/node/03-daemonset.yaml"; \
	NODE_GEN_AFTER=$$(kubectl get daemonset/unbounded-net-node -n $(NAMESPACE) -o jsonpath='{.metadata.generation}'); \
	if [ "$$NODE_GEN_BEFORE" = "$$NODE_GEN_AFTER" ]; then \
		echo "Node manifest unchanged, restarting to pick up new image..."; \
		kubectl rollout restart daemonset/unbounded-net-node -n $(NAMESPACE); \
	fi; \
	echo "Waiting for node rollout to complete..."; \
	kubectl rollout status daemonset/unbounded-net-node -n $(NAMESPACE) --timeout=120s

## Remove from Kubernetes
undeploy: validate-version render-manifests
	@set -e; \
	kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/node/03-daemonset.yaml" --ignore-not-found; \
	kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/node/02-rbac.yaml" --ignore-not-found; \
	kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/node/01-serviceaccount.yaml" --ignore-not-found; \
	kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/09-vap.yaml" --ignore-not-found; \
	kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/08-mutatingwebhook.yaml" --ignore-not-found; \
	kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/07-apiservice.yaml" --ignore-not-found; \
	kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/06-validatingwebhook.yaml" --ignore-not-found; \
	kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/04-service.yaml" --ignore-not-found; \
	kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/03-deployment.yaml" --ignore-not-found; \
	kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/02-rbac.yaml" --ignore-not-found; \
	kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/controller/01-serviceaccount.yaml" --ignore-not-found; \
	kubectl delete -f "$(REPO_ROOT)deploy/net/crds/" --ignore-not-found; \
	kubectl delete apiservice status.net.unbounded-kube.io --ignore-not-found; \
	kubectl delete apiservice v1alpha1.status.net.unbounded-kube.io --ignore-not-found; \
	if [ "$(NAMESPACE)" != "kube-system" ]; then \
		kubectl delete -f "$(REPO_ROOT)$(MANIFEST_RENDERED_DIR)/00-namespace.yaml" --ignore-not-found; \
	fi

#
# Help
#

## Show help
help:
	@echo "unbounded-net Makefile"
	@echo ""
	@echo "Usage: make [target]"
	@echo ""
	@echo "  PLATFORMS defaults to multi-arch and will require builders for all listed platforms; override with PLATFORMS=linux/$$(go env GOARCH) for local only."
	@echo ""
	@echo "Default Target:"
	@echo "  all                          Generate code, run quality checks, and build and push images"
	@echo ""
	@echo "Build Targets (Docker Buildx):"
	@echo "  build                         Build and push multi-arch images for both"
	@echo "  controller                    Build and deploy controller"
	@echo "  build-local                   Build and load images for linux/$(GOARCH)"
	@echo "  build-controller              Build and push multi-arch controller image"
	@echo "  node                          Build and deploy node"
	@echo "  build-node                    Build and push multi-arch node image"
	@echo "  build-plugin                  Build kubectl plugin binary (build/kubectl-plugin/kubectl-unbounded-net)"
	@echo ""
	@echo "Test Targets:"
	@echo "  test                         Run tests"
	@echo "  coverage                     Run tests with coverage report"
	@echo ""
	@echo "Code Quality:"
	@echo "  fmt                          Format code"
	@echo "  vet                          Run go vet"
	@echo "  lint                         Run golangci-lint"
	@echo "  deps                         Download and tidy dependencies"
	@echo "  generate                     Generate DeepCopy and CRD manifests"
	@echo "  generate-proto               Generate protobuf Go code (requires protoc)"
	@echo "  frontend                     Build frontend assets locally"
	@echo "  frontend-clean               Remove frontend generated files"
	@echo ""
	@echo "Manifest Generation:"
	@echo "  render                       Render deployment templates and create versioned tarball"
	@echo ""
	@echo "Kubernetes Targets:"
	@echo "  deploy                       Deploy all components and restart workloads"
	@echo "  deploy-namespace             Deploy only the namespace"
	@echo "  deploy-crds                  Deploy only the CRDs"
	@echo "  deploy-controller            Deploy and restart the controller"
	@echo "  deploy-node                  Deploy and restart the node DaemonSet"
	@echo "  undeploy                     Remove all components from Kubernetes"
	@echo ""
	@echo "Remote Builder:"
	@echo "  build-add-remote             Add a remote builder node"
	@echo "  REMOTE_BUILDER_NODE          Name for the remote node (required)"
	@echo "  REMOTE_BUILDER_HOST          Remote host URL, e.g., ssh://user@host (required)"
	@echo "  REMOTE_BUILDER_PLATFORM      Platform for remote node (default: linux/arm64)"
	@echo "  Example: make build-add-remote REMOTE_BUILDER_NODE=arm64 REMOTE_BUILDER_HOST=ssh://user@arm-server"
	@echo ""
	@echo "Variables:"
	@echo "  NAMESPACE=$(NAMESPACE)"
	@echo "  REGISTRY=$(REGISTRY)"
	@echo "  VERSION=$(VERSION)"
	@echo "  COMMIT=$(COMMIT)"
	@echo "  PLATFORMS=$(PLATFORMS)"
	@echo "  CNI_PLUGINS_VERSION=$(CNI_PLUGINS_VERSION)"
	@echo "  UNBOUNDED_NET_CONTROLLER_IMAGE=$(UNBOUNDED_NET_CONTROLLER_IMAGE)"
	@echo "  UNBOUNDED_NET_NODE_IMAGE=$(UNBOUNDED_NET_NODE_IMAGE)"
	@echo "  REACT_DEV=$(REACT_DEV)"
	@echo "  FORCE_NOT_LEADER=$(FORCE_NOT_LEADER)"

endif # _UNBOUNDED_NET_MAKEFILE
