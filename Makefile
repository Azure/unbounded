# Go parameters
GOCMD=go
GOFMT=gofumpt
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod
GOLINT=golangci-lint run -c .golangci.yaml

FORGE_BIN=bin/forge
FORGE_CMD=./hack/cmd/forge

INVENTORY_BIN=bin/inventory
INVENTORY_CMD=./cmd/inventory

AGENT_BIN=bin/unbounded-agent
AGENT_CMD=./cmd/agent

MACHINA_BIN=bin/machina
MACHINA_CMD=./cmd/machina
CONTAINER_REGISTRY ?= ghcr.io/azure
MACHINA_IMAGE=$(CONTAINER_REGISTRY)/machina:$(VERSION)
CONTAINER_ENGINE ?= podman

METALMAN_BIN=bin/metalman
METALMAN_CMD=./cmd/metalman

KUBECTL_UNBOUNDED_BIN=bin/kubectl-unbounded
KUBECTL_UNBOUNDED_CMD=./cmd/kubectl-unbounded

# Net binaries
NET_CONTROLLER_BIN=bin/unbounded-net-controller
NET_CONTROLLER_CMD=./cmd/unbounded-net-controller

NET_NODE_BIN=bin/unbounded-net-node
NET_NODE_CMD=./cmd/unbounded-net-node

NET_ROUTEPLAN_DEBUG_BIN=bin/unbounded-net-routeplan-debug
NET_ROUTEPLAN_DEBUG_CMD=./cmd/unbounded-net-routeplan-debug

UNPING_BIN=bin/unping
UNPING_CMD=./cmd/unping

UNROUTE_BIN=bin/unroute
UNROUTE_CMD=./cmd/unroute

# Version is derived from the latest git tag. Override with: make VERSION=v1.0.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Shared ldflags for injecting version metadata into all binaries.
STAMP_LDFLAGS=-X github.com/Azure/unbounded-kube/internal/version.Version=$(VERSION) \
              -X github.com/Azure/unbounded-kube/internal/version.GitCommit=$(GIT_COMMIT) \
              -X github.com/Azure/unbounded-kube/internal/version.BuildTime=$(BUILD_TIME)

METALMAN_IMAGE=$(CONTAINER_REGISTRY)/metalman:$(VERSION)

# kubectl-unbounded also stamps the metalman image reference.
KUBECTL_UNBOUNDED_LDFLAGS=$(STAMP_LDFLAGS) -X github.com/Azure/unbounded-kube/cmd/kubectl-unbounded/app.MetalmanImage=$(METALMAN_IMAGE)

# --- Net (unbounded-net) configuration -------------------------------------
# Container images for the net controller and node agent.
NET_CONTROLLER_IMAGE ?= $(CONTAINER_REGISTRY)/unbounded-net-controller:$(VERSION)
NET_NODE_IMAGE       ?= $(CONTAINER_REGISTRY)/unbounded-net-node:$(VERSION)

# Kubernetes deploy knobs.
NET_NAMESPACE           ?= unbounded-net
NET_FORCE_NOT_LEADER    ?= false
NET_AZURE_TENANT_ID     ?=
NET_APISERVER_URL       ?= $(shell kubectl config view --flatten --minify --template '{{ (index .clusters 0).cluster.server }}' 2>/dev/null)
# When set (e.g. NET_LOG_LEVEL=4), net-deploy-config patches the live configmap.
NET_LOG_LEVEL           ?=

# Paths.
NET_MANIFEST_TEMPLATES_DIR := deploy/net
NET_MANIFEST_RENDERED_DIR  := deploy/net/rendered
NET_CRDS_DIR               := deploy/net/crds
NET_FRONTEND_DIR           := frontend
NET_FRONTEND_DIST_DIR      := internal/net/html/dist
NET_FRONTEND_CACHE_FILE    := $(NET_FRONTEND_DIST_DIR)/.frontend-build-key

# Frontend build toggle (dev builds produce unminified output with sourcemaps).
REACT_DEV ?= false

.PHONY: all help fmt lint test build vulncheck check-deps kubectl-unbounded kubectl-unbounded-build install-tools install-protoc generate kubectl-unbounded forge inventory inventory-amd64 inventory-arm64 unbounded-agent machina machina-build machina-oci machina-oci-push machina-manifests metalman metalman-build metalman-oci metalman-oci-push gomod docs-serve unbounded-net-controller unbounded-net-node unbounded-net-routeplan-debug unping unroute
.PHONY: net-frontend net-frontend-clean net-build-ebpf net-render-manifests net-render
.PHONY: image-machina-local image-metalman-local image-net-controller-local image-net-node-local images-local
.PHONY: net-deploy net-deploy-crds net-deploy-namespace net-deploy-config net-deploy-controller net-deploy-node net-undeploy

##@ General

all: kubectl-unbounded forge machina unbounded-net-controller unbounded-net-node unbounded-net-routeplan-debug unping unroute ## Build all binaries (default)

help: ## Show this help
	@echo ""
	@echo "Usage: make <target> [VAR=value ...]"
	@echo ""
	@echo "General:"
	@echo "  all                         Build all Go binaries (default)"
	@echo "  help                        Show this help"
	@echo "  install-tools               Install gofumpt, golangci-lint, protoc-gen-go, controller-gen"
	@echo "  install-protoc              Download pinned protoc into bin/protoc/"
	@echo ""
	@echo "Development:"
	@echo "  fmt                         Format Go source (gofumpt + wsl_v5)"
	@echo "  lint                        Run golangci-lint"
	@echo "  test                        Run all tests"
	@echo "  build                       Compile all Go packages"
	@echo "  generate                    Run go generate (deepcopy, CRDs, protobuf)"
	@echo "  vulncheck                   Run govulncheck"
	@echo "  gomod                       go mod tidy"
	@echo ""
	@echo "Build:"
	@echo "  kubectl-unbounded           Build kubectl-unbounded plugin"
	@echo "  forge                       Build forge dev tool"
	@echo "  inventory                   Build inventory for amd64 and arm64"
	@echo "  unbounded-agent             Build unbounded-agent (linux)"
	@echo "  machina | machina-build     Build machina controller (with/without lint/test)"
	@echo "  metalman | metalman-build   Build metalman controller (with/without lint/test)"
	@echo "  unbounded-net-controller    Build net controller"
	@echo "  unbounded-net-node          Build net node agent"
	@echo "  unbounded-net-routeplan-debug  Build net routeplan debug tool"
	@echo "  unping                      Build unping health-check utility"
	@echo "  unroute                     Build unroute eBPF inspection utility"
	@echo ""
	@echo "Container Images (local, single-arch):"
	@echo "  image-machina-local         Build machina image with \$$(CONTAINER_ENGINE)"
	@echo "  image-metalman-local        Build metalman image"
	@echo "  image-net-controller-local  Build unbounded-net-controller image"
	@echo "  image-net-node-local        Build unbounded-net-node image"
	@echo "  images-local                Build all four images"
	@echo "  machina-oci-push            Build machina image and push"
	@echo "  metalman-oci-push           Build metalman image and push"
	@echo ""
	@echo "Net Frontend:"
	@echo "  net-frontend                Build frontend into \$$(NET_FRONTEND_DIST_DIR) (cached)"
	@echo "  net-frontend-clean          Remove node_modules and dist artifacts"
	@echo ""
	@echo "Net eBPF:"
	@echo "  net-build-ebpf              Compile bpf/unbounded_encap.c (requires clang)"
	@echo ""
	@echo "Net Manifests:"
	@echo "  machina-manifests           Render machina manifests into deploy/machina/rendered"
	@echo "  net-render-manifests        Render net manifests into \$$(NET_MANIFEST_RENDERED_DIR)"
	@echo "  net-render                  Render net manifests and create a versioned tarball"
	@echo ""
	@echo "Net Kubernetes (apply to current kubectl context):"
	@echo "  net-deploy                  Deploy CRDs, namespace, config, controller, node"
	@echo "  net-deploy-crds             Apply CRDs only"
	@echo "  net-deploy-namespace        Apply namespace only"
	@echo "  net-deploy-config           Apply configmap (and patch when NET_LOG_LEVEL or NET_AZURE_TENANT_ID set)"
	@echo "  net-deploy-controller       Apply controller manifests and roll the deployment"
	@echo "  net-deploy-node             Apply node DaemonSet manifests and roll it"
	@echo "  net-undeploy                Tear down all net resources"
	@echo ""
	@echo "Documentation:"
	@echo "  docs-serve                  Start local Hugo dev server"
	@echo ""
	@echo "Common variables (override with VAR=value):"
	@echo "  VERSION=$(VERSION)"
	@echo "  GIT_COMMIT=$(GIT_COMMIT)"
	@echo "  CONTAINER_REGISTRY=$(CONTAINER_REGISTRY)"
	@echo "  CONTAINER_ENGINE=$(CONTAINER_ENGINE)"
	@echo "  NET_NAMESPACE=$(NET_NAMESPACE)"
	@echo "  NET_CONTROLLER_IMAGE=$(NET_CONTROLLER_IMAGE)"
	@echo "  NET_NODE_IMAGE=$(NET_NODE_IMAGE)"
	@echo "  REACT_DEV=$(REACT_DEV)"

##@ Development
#
# When CI is set (GitHub Actions sets CI=true automatically), targets run
# without their usual dependency chains so each CI job stays independent.

GOFUMPT_VERSION ?= v0.8.0
GOLANGCI_LINT_VERSION ?= v2.11.4
PROTOC_GEN_GO_VERSION ?= v1.36.11
CONTROLLER_GEN_VERSION ?= v0.20.1

# Pinned protoc for deterministic .pb.go output across environments.
# Downloaded from the upstream protobuf GitHub releases.
PROTOC_VERSION ?= 3.19.6
PROTOC_DIR     ?= $(CURDIR)/bin/protoc
PROTOC         := $(PROTOC_DIR)/bin/protoc

# Auto-detect OS/arch for protoc release archive naming.
# See https://github.com/protocolbuffers/protobuf/releases for valid combinations.
PROTOC_UNAME_S := $(shell uname -s)
PROTOC_UNAME_M := $(shell uname -m)
ifeq ($(PROTOC_UNAME_S),Darwin)
  PROTOC_OS ?= osx
else
  PROTOC_OS ?= linux
endif
ifeq ($(PROTOC_UNAME_M),x86_64)
  PROTOC_ARCH ?= x86_64
else ifeq ($(PROTOC_UNAME_M),aarch64)
  PROTOC_ARCH ?= aarch_64
else ifeq ($(PROTOC_UNAME_M),arm64)
  PROTOC_ARCH ?= aarch_64
else
  PROTOC_ARCH ?= $(PROTOC_UNAME_M)
endif

install-tools: ## Install development tools (gofumpt, golangci-lint, protoc-gen-go)
	go install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

install-protoc: $(PROTOC) ## Download pinned protoc into bin/protoc/

$(PROTOC):
	@mkdir -p $(PROTOC_DIR)
	@echo "Downloading protoc v$(PROTOC_VERSION) for $(PROTOC_OS)-$(PROTOC_ARCH)..."
	@curl -fsSL -o $(PROTOC_DIR)/protoc.zip \
	  https://github.com/protocolbuffers/protobuf/releases/download/v$(PROTOC_VERSION)/protoc-$(PROTOC_VERSION)-$(PROTOC_OS)-$(PROTOC_ARCH).zip
	@unzip -q -o $(PROTOC_DIR)/protoc.zip -d $(PROTOC_DIR)
	@rm $(PROTOC_DIR)/protoc.zip
	@$(PROTOC) --version

check-deps: ## Verify required tools (gofumpt, golangci-lint v2) are installed
	@command -v $(GOFMT) >/dev/null 2>&1 || \
		{ echo "error: $(GOFMT) not found. Install it with:"; \
		  echo "  go install mvdan.cc/gofumpt@latest"; exit 1; }
	@command -v golangci-lint >/dev/null 2>&1 || \
		{ echo "error: golangci-lint not found. Install it with:"; \
		  echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; exit 1; }
	@golangci-lint --version 2>&1 | grep -qE 'version v?2\.' || \
		{ echo "error: golangci-lint v2 is required (.golangci.yaml uses version: \"2\")."; \
		  echo "  Your installed version: $$(golangci-lint --version 2>&1 | head -1)"; \
		  echo "  Install v2 with:"; \
		  echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; exit 1; }

fmt: check-deps ## Format all Go source files (gofumpt + wsl_v5 whitespace)
	$(GOFMT) -w .
	$(GOLINT) --fix -E wsl_v5 ./...

ifdef CI
# In CI each job is independent; skip chained prerequisites.

lint: ## Run golangci-lint
	$(GOLINT) ./...

test: ## Run all tests with race detector
	$(GOTEST) -race ./...

else
# Locally, chain targets for convenience: test -> lint -> fmt -> check-deps.

lint: fmt ## Run golangci-lint (implies fmt)
	$(GOLINT) ./...

test: lint ## Run all tests (implies lint)
	$(GOTEST) ./...

endif

build: machina-manifests ## Build all Go packages
	$(GOBUILD) ./...

generate: install-protoc ## Run go generate for API types (deepcopy, CRDs) and protobuf
	PATH="$(PROTOC_DIR)/bin:$$PATH" $(GOCMD) generate ./...

vulncheck: ## Run govulncheck for known vulnerabilities
	$(GOCMD) tool govulncheck ./...

gomod: ## Tidy go.mod and go.sum
	$(GOMOD) tidy

##@ Build

kubectl-unbounded-build: machina-manifests ## Build the kubectl-unbounded binary (no lint/test)
	$(GOBUILD) -ldflags '$(KUBECTL_UNBOUNDED_LDFLAGS)' -o $(KUBECTL_UNBOUNDED_BIN) $(KUBECTL_UNBOUNDED_CMD)/main.go

kubectl-unbounded: test kubectl-unbounded-build ## Build the kubectl-unbounded plugin (implies test)

forge: test ## Build the forge dev tool (implies test)
	$(GOBUILD) -o $(FORGE_BIN) $(FORGE_CMD)/main.go

inventory: inventory-amd64 inventory-arm64 ## Build inventory for amd64 and arm64, symlink to host arch
	@HOST_ARCH=$$(uname -m); \
	case "$$HOST_ARCH" in \
		x86_64)  ARCH=amd64 ;; \
		aarch64) ARCH=arm64 ;; \
		*)       echo "unsupported architecture: $$HOST_ARCH" >&2; exit 1 ;; \
	esac; \
	ln -sf inventory-$$ARCH $(INVENTORY_BIN)

inventory-amd64: test ## Build inventory for linux/amd64 (implies test)
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(INVENTORY_BIN)-amd64 $(INVENTORY_CMD)/main.go

inventory-arm64: test ## Build inventory for linux/arm64 (implies test)
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(INVENTORY_BIN)-arm64 $(INVENTORY_CMD)/main.go

unbounded-agent: test ## Build the unbounded-agent for linux (implies test)
	GOOS=linux $(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(AGENT_BIN) $(AGENT_CMD)/main.go

machina-build: machina-manifests ## Build the machina binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(MACHINA_BIN) $(MACHINA_CMD)/main.go

machina: test machina-build ## Build the machina controller (implies test)

metalman-build: ## Build the metalman binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(METALMAN_BIN) $(METALMAN_CMD)/main.go

metalman: test metalman-build ## Build the metalman controller (implies test)

##@ Net Binaries

unbounded-net-controller: test ## Build the unbounded-net-controller (implies test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(NET_CONTROLLER_BIN) $(NET_CONTROLLER_CMD)

unbounded-net-node: test ## Build the unbounded-net-node (implies test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(NET_NODE_BIN) $(NET_NODE_CMD)

unbounded-net-routeplan-debug: test ## Build the routeplan debug tool (implies test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(NET_ROUTEPLAN_DEBUG_BIN) $(NET_ROUTEPLAN_DEBUG_CMD)

unping: test ## Build the unping utility (implies test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(UNPING_BIN) $(UNPING_CMD)

unroute: test ## Build the unroute utility (implies test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(UNROUTE_BIN) $(UNROUTE_CMD)

##@ Container Images

image-machina-local: ## Build the machina container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t machina:$(VERSION) -t $(MACHINA_IMAGE) \
		-f ./images/machina/Containerfile .

# Retained for backwards compatibility with external callers (release pipelines).
machina-oci: image-machina-local ## Alias for image-machina-local

machina-oci-push: machina-oci ## Build and push the machina container image
	$(CONTAINER_ENGINE) push $(MACHINA_IMAGE)

MACHINA_NAMESPACE ?= unbounded-kube
MACHINA_MANIFEST_TEMPLATES_DIR := deploy/machina
MACHINA_MANIFEST_RENDERED_DIR  := deploy/machina/rendered

machina-manifests: ## Render machina deployment manifests into deploy/machina/rendered
	@rm -rf $(MACHINA_MANIFEST_RENDERED_DIR)
	@mkdir -p $(MACHINA_MANIFEST_RENDERED_DIR)/crd
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(MACHINA_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(MACHINA_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(MACHINA_NAMESPACE) \
		--set ControllerImage=$(MACHINA_IMAGE)
	@cp $(MACHINA_MANIFEST_TEMPLATES_DIR)/crd/*.yaml $(MACHINA_MANIFEST_RENDERED_DIR)/crd/
	@echo "Rendered machina manifests into $(MACHINA_MANIFEST_RENDERED_DIR) (image: $(MACHINA_IMAGE))"

machina-run: machina ## Replace the in-cluster machina with a locally built binary
	kubectl scale deployment/machina-controller --replicas=0 -n unbounded-kube
	kubectl get configmap machina-config -n unbounded-kube -o jsonpath='{.data.config\.yaml}' > hack/machina-config.yaml
	$(MACHINA_BIN) controller --config=hack/machina-config.yaml

image-metalman-local: ## Build the metalman container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t metalman:$(VERSION) -t $(METALMAN_IMAGE) \
		-f ./images/metalman/Containerfile .

metalman-oci: image-metalman-local ## Alias for image-metalman-local

metalman-oci-push: metalman-oci ## Build and push the metalman container image
	$(CONTAINER_ENGINE) push $(METALMAN_IMAGE)

image-net-controller-local: net-frontend ## Build the unbounded-net-controller image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--target controller \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(NET_CONTROLLER_IMAGE) \
		-f ./images/net-controller/Dockerfile .

image-net-node-local: ## Build the unbounded-net-node image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--target node \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(NET_NODE_IMAGE) \
		-f ./images/net-node/Dockerfile .

images-local: image-machina-local image-metalman-local image-net-controller-local image-net-node-local ## Build all four container images locally

##@ Net Frontend

net-frontend: ## Build the React frontend into $(NET_FRONTEND_DIST_DIR) (cached by git-tracked contents)
	@set -e; \
	frontend_key="$$( \
		git ls-files -co --exclude-standard -- $(NET_FRONTEND_DIR) | LC_ALL=C sort | while read -r file; do \
			if [ -f "$$file" ]; then sha256sum "$$file"; fi; \
		done | sha256sum | awk '{print $$1}' \
	)-react_dev=$(REACT_DEV)"; \
	if [ -d "$(NET_FRONTEND_DIST_DIR)" ] && [ -f "$(NET_FRONTEND_CACHE_FILE)" ] && [ "$$(cat "$(NET_FRONTEND_CACHE_FILE)")" = "$$frontend_key" ]; then \
		echo "Frontend unchanged; using cached $(NET_FRONTEND_DIST_DIR)"; \
		exit 0; \
	fi; \
	( cd "$(NET_FRONTEND_DIR)" && \
		if [ -f package-lock.json ]; then npm ci --prefer-offline --no-audit; else npm install; fi && \
		if [ "$(REACT_DEV)" = "true" ] || [ "$(REACT_DEV)" = "1" ]; then \
			NODE_ENV=development npm run build -- --mode development --minify false --sourcemap; \
		else \
			npm run build; \
		fi \
	); \
	rm -rf "$(NET_FRONTEND_DIST_DIR)"; \
	mkdir -p "$(NET_FRONTEND_DIST_DIR)"; \
	cp -R "$(NET_FRONTEND_DIR)/dist/." "$(NET_FRONTEND_DIST_DIR)/"; \
	printf '%s\n' "$$frontend_key" > "$(NET_FRONTEND_CACHE_FILE)"

net-frontend-clean: ## Remove frontend node_modules and dist artifacts
	rm -rf "$(NET_FRONTEND_DIR)/node_modules" "$(NET_FRONTEND_DIR)/dist" "$(NET_FRONTEND_DIST_DIR)"

##@ Net eBPF

net-build-ebpf: ## Compile bpf/unbounded_encap.c to internal/net/ebpf/unbounded_encap_bpfel.o (requires clang)
	@echo "Compiling eBPF programs..."
	@clang -O2 -g -target bpf \
		-I/usr/include \
		-c bpf/unbounded_encap.c \
		-o internal/net/ebpf/unbounded_encap_bpfel.o
	@echo "eBPF programs compiled."

##@ Net Manifests

net-render-manifests: ## Render net manifests into $(NET_MANIFEST_RENDERED_DIR)
	@rm -rf $(NET_MANIFEST_RENDERED_DIR)
	@mkdir -p $(NET_MANIFEST_RENDERED_DIR)/crds
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir "$(NET_MANIFEST_TEMPLATES_DIR)" \
		--output-dir "$(NET_MANIFEST_RENDERED_DIR)" \
		--set Namespace=$(NET_NAMESPACE) \
		--set ControllerImage=$(NET_CONTROLLER_IMAGE) \
		--set NodeImage=$(NET_NODE_IMAGE) \
		--set ForceNotLeader=$(NET_FORCE_NOT_LEADER) \
		--set AzureTenantID=$(NET_AZURE_TENANT_ID) \
		--set ApiserverURL=$(NET_APISERVER_URL)
	@cp $(NET_CRDS_DIR)/*.yaml $(NET_MANIFEST_RENDERED_DIR)/crds/
	@echo "Rendered net manifests into $(NET_MANIFEST_RENDERED_DIR) (controller: $(NET_CONTROLLER_IMAGE), node: $(NET_NODE_IMAGE))"

net-render: net-render-manifests ## Render net manifests and create a versioned tarball under build/
	@mkdir -p build
	tar czf "build/unbounded-net-manifests-$(VERSION).tar.gz" -C $(NET_MANIFEST_RENDERED_DIR) .
	@echo "Rendered manifests archive: build/unbounded-net-manifests-$(VERSION).tar.gz"

##@ Net Kubernetes

net-deploy: net-deploy-crds net-deploy-namespace net-deploy-config net-deploy-controller net-deploy-node ## Apply all net components and roll workloads

net-deploy-crds: ## Apply the net CRDs
	kubectl apply -f "$(NET_CRDS_DIR)/"

net-deploy-namespace: net-render-manifests ## Apply the net namespace (skipped when NET_NAMESPACE=kube-system)
	@if [ "$(NET_NAMESPACE)" != "kube-system" ]; then \
		kubectl apply -f "$(NET_MANIFEST_RENDERED_DIR)/00-namespace.yaml"; \
	fi

net-deploy-config: net-deploy-namespace net-render-manifests ## Apply the net configmap; patches LOG_LEVEL/AZURE_TENANT_ID when set
	NAMESPACE="$(NET_NAMESPACE)" \
	RENDERED_DIR="$(NET_MANIFEST_RENDERED_DIR)" \
	LOG_LEVEL="$(NET_LOG_LEVEL)" \
	AZURE_TENANT_ID="$(NET_AZURE_TENANT_ID)" \
		hack/scripts/net-deploy-config.sh

net-deploy-controller: net-deploy-namespace net-deploy-crds net-deploy-config net-render-manifests ## Apply controller manifests and roll the deployment
	NAMESPACE="$(NET_NAMESPACE)" \
	RENDERED_DIR="$(NET_MANIFEST_RENDERED_DIR)" \
		hack/scripts/net-deploy-controller.sh

net-deploy-node: net-deploy-namespace net-deploy-crds net-deploy-config net-render-manifests ## Apply node DaemonSet manifests and roll it
	NAMESPACE="$(NET_NAMESPACE)" \
	RENDERED_DIR="$(NET_MANIFEST_RENDERED_DIR)" \
		hack/scripts/net-deploy-node.sh

net-undeploy: net-render-manifests ## Tear down all net resources in dependency order
	NAMESPACE="$(NET_NAMESPACE)" \
	RENDERED_DIR="$(NET_MANIFEST_RENDERED_DIR)" \
	CRDS_DIR="$(NET_CRDS_DIR)" \
		hack/scripts/net-undeploy.sh

##@ Documentation

docs-serve: ## Start a local Hugo dev server with live-reload
	@command -v hugo >/dev/null 2>&1 || \
		{ echo "error: hugo not found. Install it from:"; \
		  echo "  https://gohugo.io/installation/"; exit 1; }
	cd docs && hugo server
