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
MACHINA_TAG ?= latest
CONTAINER_REGISTRY ?= ghcr.io/azure
MACHINA_IMAGE=$(CONTAINER_REGISTRY)/machina:$(MACHINA_TAG)
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
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# Shared ldflags for injecting version metadata into all binaries.
VERSION_LDFLAGS=-X github.com/Azure/unbounded-kube/internal/version.Version=$(VERSION) -X github.com/Azure/unbounded-kube/internal/version.GitCommit=$(GIT_COMMIT)

# kubectl-unbounded also stamps the metalman image reference.
KUBECTL_UNBOUNDED_LDFLAGS=$(VERSION_LDFLAGS) -X github.com/Azure/unbounded-kube/cmd/kubectl-unbounded/app.MetalmanImage=$(METALMAN_IMAGE)

METALMAN_TAG ?= latest
METALMAN_IMAGE=$(CONTAINER_REGISTRY)/metalman:$(METALMAN_TAG)

.PHONY: all help fmt lint test build vulncheck check-deps install-tools install-protoc generate kubectl-unbounded forge inventory inventory-amd64 inventory-arm64 unbounded-agent machina machina-build machina-oci machina-oci-push machina-manifests metalman metalman-build metalman-oci metalman-oci-push gomod docs-serve unbounded-net-controller unbounded-net-node unbounded-net-routeplan-debug unping unroute

##@ General

all: kubectl-unbounded forge machina unbounded-net-controller unbounded-net-node unbounded-net-routeplan-debug unping unroute ## Build all binaries (default)

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-28s\033[0m %s\n", $$1, $$2 } \
	/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

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

fmt: check-deps ## Format all Go source files with gofumpt
	$(GOFMT) -w .

ifdef CI
# In CI each job is independent; skip chained prerequisites.

lint: ## Run golangci-lint
	$(GOLINT) --fix -E wsl_v5 ./...
	$(GOLINT) ./...

test: ## Run all tests with race detector
	$(GOTEST) -race ./...

else
# Locally, chain targets for convenience: test -> lint -> fmt -> check-deps.

lint: fmt ## Run golangci-lint (implies fmt)
	$(GOLINT) --fix -E wsl_v5 ./...
	$(GOLINT) ./...

test: lint ## Run all tests (implies lint)
	$(GOTEST) ./...

endif

build: ## Build all Go packages
	$(GOBUILD) ./...

generate: install-protoc ## Run go generate for API types (deepcopy, CRDs) and protobuf
	PATH="$(PROTOC_DIR)/bin:$$PATH" $(GOCMD) generate ./...

vulncheck: ## Run govulncheck for known vulnerabilities
	$(GOCMD) tool govulncheck ./...

gomod: ## Tidy go.mod and go.sum
	$(GOMOD) tidy

##@ Build

kubectl-unbounded: test machina-manifests ## Build the kubectl-unbounded plugin (implies test)
	$(GOBUILD) -ldflags '$(KUBECTL_UNBOUNDED_LDFLAGS)' -o $(KUBECTL_UNBOUNDED_BIN) $(KUBECTL_UNBOUNDED_CMD)/main.go

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
	GOOS=linux $(GOBUILD) -ldflags '$(VERSION_LDFLAGS)' -o $(AGENT_BIN) $(AGENT_CMD)/main.go

machina-build: machina-manifests ## Build the machina binary (no lint/test)
	$(GOBUILD) -ldflags '$(VERSION_LDFLAGS)' -o $(MACHINA_BIN) $(MACHINA_CMD)/main.go

machina: test machina-build ## Build the machina controller (implies test)

metalman-build: ## Build the metalman binary (no lint/test)
	$(GOBUILD) -ldflags '$(VERSION_LDFLAGS)' -o $(METALMAN_BIN) $(METALMAN_CMD)/main.go

metalman: check-deps ## Format, lint, test, and build metalman
	$(GOFMT) -w $(METALMAN_CMD) ./internal/metalman
	$(GOLINT) --fix -E wsl_v5 $(METALMAN_CMD)/... ./internal/metalman/...
	$(GOLINT) $(METALMAN_CMD)/... ./internal/metalman/...
	$(GOTEST) $(METALMAN_CMD)/... ./internal/metalman/...
	$(GOBUILD) -ldflags '$(VERSION_LDFLAGS)' -o $(METALMAN_BIN) $(METALMAN_CMD)/main.go

##@ Net Binaries

unbounded-net-controller: test ## Build the unbounded-net-controller (implies test)
	$(GOBUILD) -o $(NET_CONTROLLER_BIN) $(NET_CONTROLLER_CMD)

unbounded-net-node: test ## Build the unbounded-net-node (implies test)
	$(GOBUILD) -o $(NET_NODE_BIN) $(NET_NODE_CMD)

unbounded-net-routeplan-debug: test ## Build the routeplan debug tool (implies test)
	$(GOBUILD) -o $(NET_ROUTEPLAN_DEBUG_BIN) $(NET_ROUTEPLAN_DEBUG_CMD)

unping: test ## Build the unping utility (implies test)
	$(GOBUILD) -o $(UNPING_BIN) $(UNPING_CMD)

unroute: test ## Build the unroute utility (implies test)
	$(GOBUILD) -o $(UNROUTE_BIN) $(UNROUTE_CMD)

##@ Container Images

machina-oci: ## Build the machina container image
	$(CONTAINER_ENGINE) build -t machina:$(MACHINA_TAG) -t $(MACHINA_IMAGE) -f ./images/machina/Containerfile .

machina-oci-push: machina-oci ## Build and push the machina container image
	$(CONTAINER_ENGINE) push $(MACHINA_IMAGE)

machina-manifests: ## Stamp the machina deployment manifest with the container image
	@sed -i 's|image: .*|image: $(MACHINA_IMAGE)|' deploy/machina/04-deployment.yaml
	@echo "Updated deploy/machina/04-deployment.yaml → image: $(MACHINA_IMAGE)"

machina-run: machina ## Replace the in-cluster machina with a locally built binary
	kubectl scale deployment/machina-controller --replicas=0 -n unbounded-kube
	kubectl get configmap machina-config -n unbounded-kube -o jsonpath='{.data.config\.yaml}' > hack/machina-config.yaml
	$(MACHINA_BIN) controller --config=hack/machina-config.yaml

metalman-oci: ## Build the metalman container image
	$(CONTAINER_ENGINE) build -t metalman:$(METALMAN_TAG) -t $(METALMAN_IMAGE) -f ./images/metalman/Containerfile .

metalman-oci-push: metalman-oci ## Build and push the metalman container image
	$(CONTAINER_ENGINE) push $(METALMAN_IMAGE)

##@ Documentation

docs-serve: ## Start a local Hugo dev server with live-reload
	@command -v hugo >/dev/null 2>&1 || \
		{ echo "error: hugo not found. Install it from:"; \
		  echo "  https://gohugo.io/installation/"; exit 1; }
	cd docs && hugo server
