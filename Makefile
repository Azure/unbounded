# Go parameters
GOCMD=go
GOFMT=gofumpt
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod
GOLINT=golangci-lint run -c .golangci.yaml

CONTAINER_ENGINE ?= podman
CONTAINER_REGISTRY ?= ghcr.io/azure

FORGE_BIN=bin/forge
FORGE_CMD=./hack/cmd/forge

INVENTORY_AGENT_BIN=bin/inventory-agent
INVENTORY_AGENT_CMD=./cmd/inventory/inventory-agent

INVENTORY_AGGREGATOR_BIN=bin/inventory-aggregator
INVENTORY_AGGREGATOR_CMD=./cmd/inventory/inventory-aggregator
INVENTORY_AGGREGATOR_TAG ?= latest
INVENTORY_AGGREGATOR_IMAGE=$(CONTAINER_REGISTRY)/inventory-aggregator:$(INVENTORY_AGGREGATOR_TAG)

INVENTORY_INSPECTOR_BIN=bin/inventory-inspector
INVENTORY_INSPECTOR_CMD=./cmd/inventory/inventory-inspector
INVENTORY_INSPECTOR_TAG ?= latest
INVENTORY_INSPECTOR_IMAGE=$(CONTAINER_REGISTRY)/inventory-inspector:$(INVENTORY_INSPECTOR_TAG)

INVENTORY_VIEWER_BIN=bin/inventory-viewer
INVENTORY_VIEWER_CMD=./cmd/inventory/inventory-viewer
INVENTORY_VIEWER_TAG ?= latest
INVENTORY_VIEWER_IMAGE=$(CONTAINER_REGISTRY)/inventory-viewer:$(INVENTORY_VIEWER_TAG)

AGENT_BIN=bin/unbounded-agent
AGENT_CMD=./cmd/agent

MACHINA_BIN=bin/machina
MACHINA_CMD=./cmd/machina
MACHINA_IMAGE ?= $(CONTAINER_REGISTRY)/machina:$(VERSION)

MACHINE_OPS_CONTROLLER_BIN=bin/machine-ops-controller
MACHINE_OPS_CONTROLLER_CMD=./cmd/machine-ops-controller
MACHINE_OPS_CONTROLLER_IMAGE ?= $(CONTAINER_REGISTRY)/machine-ops-controller:$(VERSION)

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

# Rust binaries
UNBOUNDED_STORAGE_BIN=bin/unbounded-storage
UNBOUNDED_STORAGE_CRATE=./cmd/unbounded-storage
CARGO ?= cargo

# Mercury (mercury-hpc) is a native dependency of the unbounded-storage crate.
# We build a pinned version from source into a project-local prefix; the
# unbounded-storage build.rs honours $MERCURY_PKG_CONFIG_PATH so it picks up
# our local install ahead of any system copy.
MERCURY_VERSION ?= v2.4.1
MERCURY_PREFIX  ?= $(CURDIR)/tmp/mercury-prefix
MERCURY_SRC     ?= $(CURDIR)/tmp/mercury-src
MERCURY_PC_FILE := $(MERCURY_PREFIX)/lib/pkgconfig/mercury.pc
MERCURY_INSTALL_SCRIPT := hack/scripts/install-mercury.sh

# Version is derived from the latest git tag. Override with: make VERSION=v1.0.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Shared ldflags for injecting version metadata into all binaries.
STAMP_LDFLAGS=-X github.com/Azure/unbounded/internal/version.Version=$(VERSION) \
              -X github.com/Azure/unbounded/internal/version.GitCommit=$(GIT_COMMIT) \
              -X github.com/Azure/unbounded/internal/version.BuildTime=$(BUILD_TIME)

METALMAN_IMAGE=$(CONTAINER_REGISTRY)/metalman:$(VERSION)

# Orca configuration
ORCA_BIN=bin/orca
ORCA_CMD=./cmd/orca
ORCA_IMAGE ?= $(CONTAINER_REGISTRY)/orca:$(VERSION)
ORCA_NAMESPACE ?= unbounded-kube
ORCA_MANIFEST_TEMPLATES_DIR := deploy/orca
ORCA_MANIFEST_RENDERED_DIR  := deploy/orca/rendered

# kubectl-unbounded also stamps the metalman image reference.
KUBECTL_UNBOUNDED_LDFLAGS=$(STAMP_LDFLAGS) -X github.com/Azure/unbounded/cmd/kubectl-unbounded/app.MetalmanImage=$(METALMAN_IMAGE)

# --- Net (unbounded-net) configuration -------------------------------------
# Container images for the net controller and node agent.
NET_CONTROLLER_IMAGE ?= $(CONTAINER_REGISTRY)/unbounded-net-controller:$(VERSION)
NET_NODE_IMAGE       ?= $(CONTAINER_REGISTRY)/unbounded-net-node:$(VERSION)

# CNI plugins version baked into the net-node image. Keep in sync with the
# defaults in images/net-{node,controller}/Dockerfile and the workflow envs.
CNI_PLUGINS_VERSION  ?= v1.9.1

# Host architecture for local image builds (amd64 / arm64). Used to pick the
# right CNI plugins tarball for the current machine.
HOST_GOARCH := $(shell $(GOCMD) env GOARCH)

# Kubernetes deploy knobs.
NET_NAMESPACE           ?= unbounded-net
NET_FORCE_NOT_LEADER    ?= false
NET_AZURE_TENANT_ID     ?=
NET_APISERVER_URL       ?= $(shell kubectl config view --flatten --minify --template '{{ (index .clusters 0).cluster.server }}' 2>/dev/null)
# When set (e.g. NET_LOG_LEVEL=4), `make -C hack/net deploy-config` patches the live configmap.
NET_LOG_LEVEL           ?=

# Paths.
NET_MANIFEST_TEMPLATES_DIR := deploy/net
NET_MANIFEST_RENDERED_DIR  := deploy/net/rendered
NET_CRD_DIR                := deploy/net/crd
NET_FRONTEND_DIR           := frontend
NET_FRONTEND_DIST_DIR      := internal/net/html/dist
NET_FRONTEND_CACHE_FILE    := $(NET_FRONTEND_DIST_DIR)/.frontend-build-key

# Frontend build toggle (dev builds produce unminified output with sourcemaps).
REACT_DEV ?= false

.PHONY: all help fmt lint test build vulncheck check-deps kubectl-unbounded kubectl-unbounded-build install-tools install-protoc generate kubectl-unbounded forge unbounded-agent machina machina-build machina-oci machina-oci-push machina-manifests machine-ops-controller machine-ops-controller-build machine-ops-controller-oci machine-ops-controller-oci-push machine-ops-manifests metalman metalman-build metalman-oci metalman-oci-push gomod docs-serve unbounded-net-controller unbounded-net-controller-build unbounded-net-node unbounded-net-node-build unbounded-net-routeplan-debug unping unping-build unroute unroute-build notice notice-check
.PHONY: net-frontend net-frontend-clean net-ebpf-build net-ebpf-generate net-ebpf-verify net-manifests release-manifests
.PHONY: image-machina-local image-machine-ops-controller-local image-metalman-local image-net-controller-local image-net-node-local images-local
.PHONY: image-net-controller-push image-net-node-push images-net-all images-net-all-push
.PHONY: unbounded-storage unbounded-storage-build unbounded-storage-test unbounded-storage-check unbounded-storage-model-check mercury mercury-clean

##@ General

all: kubectl-unbounded forge machina machine-ops-controller unbounded-net-controller unbounded-net-node unbounded-net-routeplan-debug unping unroute ## Build all binaries (default)

help: ## Show this help
	@echo ""
	@echo "Usage: make <target> [VAR=value ...]"
	@echo ""
	@echo "General:"
	@echo "  all                              Build all Go binaries (default)"
	@echo "  help                             Show this help"
	@echo "  install-tools                    Install gofumpt, golangci-lint, protoc-gen-go, protoc-gen-go-grpc, controller-gen"
	@echo "  install-protoc                   Download pinned protoc into bin/protoc/"
	@echo ""
	@echo "Development:"
	@echo "  fmt                              Format Go source (gofumpt + wsl_v5)"
	@echo "  lint                             Run golangci-lint"
	@echo "  test                             Run all tests"
	@echo "  build                            Compile all Go packages"
	@echo "  generate                         Run go generate (deepcopy, CRDs, protobuf)"
	@echo "  vulncheck                        Run govulncheck"
	@echo "  gomod                            go mod tidy"
	@echo "  notice                           Regenerate NOTICE from go.mod and frontend/package.json"
	@echo "  notice-check                     Verify NOTICE is in sync with dependencies"
	@echo ""
	@echo "Build:"
	@echo "  kubectl-unbounded                Build kubectl-unbounded plugin"
	@echo "  forge                            Build forge dev tool"
	@echo "  inventory-all                    Build all inventory components"
	@echo "  inventory-agent                  Build inventory-agent for amd64 and arm64"
	@echo "  inventory-agent-amd64            Build inventory-agent for amd64"
	@echo "  inventory-agent-arm64            Build inventory-agent for arm64"
	@echo "  inventory-aggregator             Build inventory-aggregator"
	@echo "  inventory-inspector              Build inventory-inspector"
	@echo "  inventory-viewer                 Build inventory-viewer"
	@echo "  unbounded-agent                  Build unbounded-agent (linux)"
	@echo "  machina | machina-build          Build machina controller (with/without lint/test)"
	@echo "  machine-ops-controller           Build machine-ops-controller"
	@echo "  metalman | metalman-build        Build metalman controller (with/without lint/test)"
	@echo "  unbounded-net-controller         Build net controller"
	@echo "  unbounded-net-node               Build net node agent"
	@echo "  unbounded-net-routeplan-debug    Build net routeplan debug tool"
	@echo "  unping                           Build unping health-check utility"
	@echo "  unroute                          Build unroute eBPF inspection utility"
	@echo ""
	@echo "Rust Binaries:"
	@echo "  unbounded-storage | unbounded-storage-build  Build unbounded-storage (with/without test)"
	@echo "  unbounded-storage-test           Run cargo tests for unbounded-storage"
	@echo "  unbounded-storage-check          Run cargo check for unbounded-storage"
	@echo "  unbounded-storage-model-check    Run TLC on the CoW B+tree crash-consistency model"
	@echo "  mercury                          Build pinned Mercury into \$$(MERCURY_PREFIX)"
	@echo "  mercury-clean                    Remove the local Mercury prefix and source tree"
	@echo ""
	@echo "Container Images (local, single-arch):"
	@echo "  image-inventory-all-local        Build all local inventory container images"
	@echo "  image-inventory-all-push         Build and push all inventory container images"
	@echo "  image-inventory-aggregator-local Build a local inventory-aggregator container image"
	@echo "  image-inventory-aggregator-push  Build and push the inventory-aggregator container image"
	@echo "  image-inventory-inspector-local  Build a local inventory-inspector container image"
	@echo "  image-inventory-inspector-push   Build and push the inventory-inspector container image"
	@echo "  image-inventory-viewer-local     Build a local inventory-viewer container image"
	@echo "  image-inventory-viewer-push      Build and push the inventory-viewer container image"
	@echo "  image-machina-local              Build machina image with \$$(CONTAINER_ENGINE)"
	@echo "  image-machine-ops-controller-local Build machine-ops-controller image"
	@echo "  image-metalman-local             Build metalman image"
	@echo "  image-net-controller-local       Build unbounded-net-controller image"
	@echo "  image-net-controller-push        Build and push unbounded-net-controller image"
	@echo "  image-net-node-local             Build unbounded-net-node image"
	@echo "  image-net-node-push              Build and push unbounded-net-node image"
	@echo "  images-net-all                   Build all unbounded-net images"
	@echo "  images-net-all-push              Build and push all unbounded-net images"
	@echo "  images-local                     Build all local images"
	@echo "  machina-oci-push                 Build machina image and push"
	@echo "  machine-ops-controller-oci-push  Build machine-ops-controller image and push"
	@echo "  metalman-oci-push                Build metalman image and push"
	@echo "  image-orca-local                 Build orca image"
	@echo "  orca-oci-push                    Build orca image and push"
	@echo ""
	@echo "Net Frontend:"
	@echo "  net-frontend                     Build frontend into \$$(NET_FRONTEND_DIST_DIR) (cached)"
	@echo "  net-frontend-clean               Remove node_modules and dist artifacts"
	@echo ""
	@echo "Net eBPF:"
	@echo "  net-ebpf-build                   Compile bpf/unbounded_encap.c (requires clang-18; see bpf/clang-version)"
	@echo "  net-ebpf-generate                Regenerate bpf/vmlinux.h from pinned Ubuntu kernel (requires bpftool, curl, dpkg-deb, python3)"
	@echo "  net-ebpf-verify                  Verify bpf/vmlinux.h matches bpf/btf-kernel-pin{,-hashes} (no extra tools)"
	@echo ""
	@echo "Net Manifests:"
	@echo "  machina-manifests                Render machina manifests into deploy/machina/rendered"
	@echo "  machine-ops-manifests            Render machine-ops manifests into deploy/machine-ops/rendered"
	@echo "  net-manifests                    Render net manifests into \$$(NET_MANIFEST_RENDERED_DIR)"
	@echo "  orca-manifests                   Render orca manifests into deploy/orca/rendered"
	@echo ""
	@echo "Net Kubernetes (apply to current kubectl context):"
	@echo "  See \`make -C hack/net help\` for cluster deploy/undeploy targets."
	@echo ""
	@echo "Orca Dev Harness (Kind cluster):"
	@echo "  orca | orca-build                Build orca binary (with/without lint/test)"
	@echo "  orca-up                          Bring up Orca dev harness in Kind"
	@echo "  orca-down                        Tear down Orca dev harness Kind cluster"
	@echo "  orca-reset                       Rebuild image and rollout-restart deployment"
	@echo "  orca-inttest                     Run orca integration tests (Docker required)"
	@echo "  See \`make -C hack/orca help\` for full list."
	@echo ""
	@echo "Documentation:"
	@echo "  docs-serve                       Start local Hugo dev server"
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
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.1
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

install-tools: ## Install development tools (gofumpt, golangci-lint, protoc-gen-go, protoc-gen-go-grpc, controller-gen)
	go install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
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

test: machina-manifests net-manifests ## Run all tests with race detector
	$(GOTEST) -race ./...

else
# Locally, chain targets for convenience: test -> lint -> fmt -> check-deps.

lint: fmt ## Run golangci-lint (implies fmt)
	$(GOLINT) ./...

test: lint machina-manifests net-manifests ## Run all tests (implies lint)
	$(GOTEST) ./...

endif

build: machina-manifests net-manifests ## Build all Go packages
	$(GOBUILD) ./...

generate: install-protoc ## Run go generate for API types (deepcopy, CRDs) and protobuf
	PATH="$(PROTOC_DIR)/bin:$$PATH" $(GOCMD) generate ./...

vulncheck: machina-manifests net-manifests ## Run govulncheck for known vulnerabilities
	$(GOCMD) tool govulncheck ./...

gomod: ## Tidy go.mod and go.sum
	$(GOMOD) tidy

notice: ## Regenerate NOTICE from go.mod and frontend/package.json
	@if [ ! -d "$(NET_FRONTEND_DIR)/node_modules" ]; then \
		echo "ERROR: $(NET_FRONTEND_DIR)/node_modules not found." >&2; \
		echo "Run: (cd $(NET_FRONTEND_DIR) && npm ci)" >&2; \
		exit 1; \
	fi
	$(GOCMD) run ./hack/cmd/notice generate --output NOTICE

notice-check: ## Verify NOTICE is in sync with go.mod and frontend/package.json
	@if [ ! -d "$(NET_FRONTEND_DIR)/node_modules" ]; then \
		echo "ERROR: $(NET_FRONTEND_DIR)/node_modules not found." >&2; \
		echo "Run: (cd $(NET_FRONTEND_DIR) && npm ci)" >&2; \
		exit 1; \
	fi
	$(GOCMD) run ./hack/cmd/notice check --notice NOTICE

##@ Build

kubectl-unbounded-build: machina-manifests net-manifests ## Build the kubectl-unbounded binary (no lint/test)
	$(GOBUILD) -ldflags '$(KUBECTL_UNBOUNDED_LDFLAGS)' -o $(KUBECTL_UNBOUNDED_BIN) $(KUBECTL_UNBOUNDED_CMD)/main.go

kubectl-unbounded: test kubectl-unbounded-build ## Build the kubectl-unbounded plugin (implies test)

forge: test ## Build the forge dev tool (implies test)
	$(GOBUILD) -o $(FORGE_BIN) $(FORGE_CMD)/main.go

.PHONY: inventory-all
inventory-all: inventory-agent inventory-aggregator inventory-inspector inventory-viewer ## Build all inventory components

.PHONY: inventory-agent
inventory-agent: inventory-agent-amd64 inventory-agent-arm64 ## Build inventory for amd64 and arm64, symlink to host arch
	@HOST_ARCH=$$(uname -m); \
	case "$$HOST_ARCH" in \
		x86_64)  ARCH=amd64 ;; \
		aarch64) ARCH=arm64 ;; \
		*)       echo "unsupported architecture: $$HOST_ARCH" >&2; exit 1 ;; \
	esac; \
	ln -sf inventory-agent-$$ARCH $(INVENTORY_AGENT_BIN)

.PHONY: inventory-agent-amd64
inventory-agent-amd64: test ## Build inventory for linux/amd64 (implies test)
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(INVENTORY_AGENT_BIN)-amd64 $(INVENTORY_AGENT_CMD)/main.go

.PHONY: inventory-agent-arm64
inventory-agent-arm64: test ## Build inventory for linux/arm64 (implies test)
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(INVENTORY_AGENT_BIN)-arm64 $(INVENTORY_AGENT_CMD)/main.go

.PHONY: inventory-aggregator
inventory-aggregator: test ## Build the inventory-aggregator for linux (implies test)
	$(GOBUILD) -o $(INVENTORY_AGGREGATOR_BIN) $(INVENTORY_AGGREGATOR_CMD)/main.go

.PHONY: inventory-inspector
inventory-inspector: test ## Build the inventory-inspector (implies test)
	$(GOBUILD) -o $(INVENTORY_INSPECTOR_BIN) $(INVENTORY_INSPECTOR_CMD)/main.go

.PHONY: inventory-viewer
inventory-viewer: test ## Build the inventory-viewer web server (implies test)
	$(GOBUILD) -o $(INVENTORY_VIEWER_BIN) $(INVENTORY_VIEWER_CMD)/main.go

unbounded-agent: test ## Build the unbounded-agent for linux (implies test)
	GOOS=linux $(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(AGENT_BIN) $(AGENT_CMD)/main.go

machina-build: machina-manifests ## Build the machina binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(MACHINA_BIN) $(MACHINA_CMD)/main.go

machina: test machina-build ## Build the machina controller (implies test)

machine-ops-controller-build: machine-ops-manifests ## Build the machine-ops-controller binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(MACHINE_OPS_CONTROLLER_BIN) $(MACHINE_OPS_CONTROLLER_CMD)

machine-ops-controller: test machine-ops-controller-build ## Build the machine-ops-controller (implies test)

metalman-build: ## Build the metalman binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(METALMAN_BIN) $(METALMAN_CMD)/main.go

metalman: test metalman-build ## Build the metalman controller (implies test)

##@ Net Binaries

unbounded-net-controller-build: ## Build the unbounded-net-controller binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(NET_CONTROLLER_BIN) $(NET_CONTROLLER_CMD)

unbounded-net-controller: test unbounded-net-controller-build ## Build the unbounded-net-controller (implies test)

unbounded-net-node-build: ## Build the unbounded-net-node binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(NET_NODE_BIN) $(NET_NODE_CMD)

unbounded-net-node: test unbounded-net-node-build ## Build the unbounded-net-node (implies test)

unbounded-net-routeplan-debug: test ## Build the routeplan debug tool (implies test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(NET_ROUTEPLAN_DEBUG_BIN) $(NET_ROUTEPLAN_DEBUG_CMD)

unping-build: ## Build the unping utility binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(UNPING_BIN) $(UNPING_CMD)

unping: test unping-build ## Build the unping utility (implies test)

unroute-build: ## Build the unroute utility binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(UNROUTE_BIN) $(UNROUTE_CMD)

unroute: test unroute-build ## Build the unroute utility (implies test)

##@ Rust Binaries

# Mercury env: prepend our local prefix so cargo/build.rs find the right .pc
# files and so the resulting binaries/tests can dlopen libmercury at runtime.
# Targets that invoke cargo must export these via `$(UNBOUNDED_STORAGE_ENV)`.
UNBOUNDED_STORAGE_ENV = \
	MERCURY_PKG_CONFIG_PATH="$(MERCURY_PREFIX)/lib/pkgconfig" \
	PKG_CONFIG_PATH="$(MERCURY_PREFIX)/lib/pkgconfig:$${PKG_CONFIG_PATH}" \
	LD_LIBRARY_PATH="$(MERCURY_PREFIX)/lib:$${LD_LIBRARY_PATH}"

mercury: $(MERCURY_PC_FILE) ## Build pinned Mercury into $(MERCURY_PREFIX) (idempotent)

# The install script is itself idempotent: if mercury.pc already advertises the
# pinned version it exits immediately. We still gate on the .pc file as the
# Make-level sentinel so a cached prefix avoids invoking the script at all.
$(MERCURY_PC_FILE): $(MERCURY_INSTALL_SCRIPT)
	MERCURY_VERSION=$(MERCURY_VERSION) \
	MERCURY_PREFIX=$(MERCURY_PREFIX) \
	MERCURY_SRC=$(MERCURY_SRC) \
		$(MERCURY_INSTALL_SCRIPT)

mercury-clean: ## Remove the local Mercury prefix and source tree
	rm -rf "$(MERCURY_PREFIX)" "$(MERCURY_SRC)"

unbounded-storage-check: mercury ## Run cargo check for unbounded-storage
	$(UNBOUNDED_STORAGE_ENV) $(CARGO) check --manifest-path $(UNBOUNDED_STORAGE_CRATE)/Cargo.toml --locked --all-targets

unbounded-storage-test: mercury ## Run cargo tests for unbounded-storage
	$(UNBOUNDED_STORAGE_ENV) $(CARGO) test --manifest-path $(UNBOUNDED_STORAGE_CRATE)/Cargo.toml --locked --all-targets

unbounded-storage-build: mercury ## Build the unbounded-storage binary (no test)
	$(UNBOUNDED_STORAGE_ENV) $(CARGO) build --manifest-path $(UNBOUNDED_STORAGE_CRATE)/Cargo.toml --release --locked
	@mkdir -p $(dir $(UNBOUNDED_STORAGE_BIN))
	cp $(UNBOUNDED_STORAGE_CRATE)/target/release/unbounded-storage $(UNBOUNDED_STORAGE_BIN)

unbounded-storage: unbounded-storage-test unbounded-storage-build ## Build the unbounded-storage binary (implies test)

# TLA+ tooling for the unbounded-storage CoW B+tree crash-consistency model.
# tla2tools.jar is fetched on demand into tmp/ (gitignored).  Override
# TLA_TOOLS_JAR to use a locally installed copy.
#
# The URL is pinned to a tagged release (not `latest/download`) and the
# downloaded artifact is verified against TLA_TOOLS_SHA256 so model-check
# runs are reproducible across machines and over time.
TLA_TOOLS_JAR ?= tmp/tla2tools.jar
TLA_TOOLS_VERSION ?= v1.8.0
TLA_TOOLS_URL ?= https://github.com/tlaplus/tlaplus/releases/download/$(TLA_TOOLS_VERSION)/tla2tools.jar
TLA_TOOLS_SHA256 ?= 71546dff3897a01b0ee4fa64135d9f5e9384d2b7e47b3cc20a16b655b0eb4f86
COW_MODEL_DIR := cmd/unbounded-storage/models/copy-on-write

$(TLA_TOOLS_JAR):
	@mkdir -p $(dir $(TLA_TOOLS_JAR))
	@echo "Downloading tla2tools.jar ($(TLA_TOOLS_VERSION)) -> $(TLA_TOOLS_JAR)"
	@curl -fsSL -o $(TLA_TOOLS_JAR) $(TLA_TOOLS_URL)
	@echo "$(TLA_TOOLS_SHA256)  $(TLA_TOOLS_JAR)" | sha256sum -c -

unbounded-storage-model-check: $(TLA_TOOLS_JAR) ## Run TLC on the CoW B+tree crash-consistency model
	@command -v java >/dev/null 2>&1 || { echo "java is required to run TLC" >&2; exit 1; }
	cd $(COW_MODEL_DIR) && java -XX:+UseParallelGC -cp $(CURDIR)/$(TLA_TOOLS_JAR) tlc2.TLC -workers auto -config CowBtreeCrash.cfg CowBtreeCrash.tla

##@ Container Images
#
# Trivy (image scanning)
# ----------------------
# Set TRIVY=1 (or any non-empty value) on the make command line to scan after
# each image-*-local build, e.g.:
#     TRIVY=1 make image-net-node-local
#     TRIVY=1 make images-local
#
# Knobs (all overridable on the command line or environment):
#   TRIVY            Enable scanning when non-empty. Default: unset (no scan).
#   TRIVY_VERSION    Trivy CLI version. Default: 0.69.3 (matches CI).
#   TRIVY_SEVERITY   Comma-separated severities. Default: CRITICAL,HIGH.
#   TRIVY_EXIT_CODE  Exit code on findings. Default: 1 (fail). Set 0 to warn-only.
#   TRIVY_IMAGE      Override the trivy container image entirely.
#                    Default: aquasec/trivy:$(TRIVY_VERSION).
#   TRIVY_CACHE_DIR  Host dir for the trivy DB cache.
#                    Default: $$HOME/.cache/trivy.

TRIVY            ?=
TRIVY_VERSION    ?= 0.69.3
TRIVY_SEVERITY   ?= CRITICAL,HIGH
TRIVY_EXIT_CODE  ?= 1
TRIVY_IMAGE      ?= aquasec/trivy:$(TRIVY_VERSION)
TRIVY_CACHE_DIR  ?= $(HOME)/.cache/trivy

# Single-line shell command; expands to nothing when TRIVY is empty.
# Usage in a recipe:  $(call trivy-maybe,image:tag)
#
# We pipe the image to trivy via `image save` + `--input` so the same
# recipe works with both docker and podman without needing a daemon
# socket mounted into the trivy container.
TRIVY_SCAN_CMD = mkdir -p $(TRIVY_CACHE_DIR) && \
    tmp=$$(mktemp -t trivy-scan-XXXXXX.tar) && trap 'rm -f $$tmp' EXIT && \
    $(CONTAINER_ENGINE) image save -o $$tmp $(1) && \
    $(CONTAINER_ENGINE) run --rm \
        -v $$tmp:/scan.tar:ro \
        -v $(TRIVY_CACHE_DIR):/root/.cache/trivy \
        $(TRIVY_IMAGE) image \
            --severity $(TRIVY_SEVERITY) \
            --exit-code $(TRIVY_EXIT_CODE) \
            --format table \
            --input /scan.tar

trivy-maybe = $(if $(strip $(TRIVY)),$(TRIVY_SCAN_CMD))

# Pre-fetch CNI plugins tarballs for local image builds.
# The Dockerfile reads resources/cni-plugins-linux-<arch>-<version>.tgz; this
# pattern rule fetches it on demand when the file is missing.
resources/cni-plugins-linux-%-$(CNI_PLUGINS_VERSION).tgz:
	@mkdir -p resources
	curl -fsSL \
		"https://github.com/containernetworking/plugins/releases/download/$(CNI_PLUGINS_VERSION)/cni-plugins-linux-$*-$(CNI_PLUGINS_VERSION).tgz" \
		-o $@

.PHONY: image-inventory-all-local
image-inventory-all-local: image-inventory-aggregator-local image-inventory-inspector-local image-inventory-viewer-local

.PHONY: image-inventory-all-push
image-inventory-all-push: image-inventory-aggregator-push image-inventory-inspector-push image-inventory-viewer-push

.PHONY: image-inventory-aggregator-build
image-inventory-aggregator-local: ## Build the inventory-aggregator container image
	$(CONTAINER_ENGINE) build -t inventory-aggregator:$(INVENTORY_AGGREGATOR_TAG) -t $(INVENTORY_AGGREGATOR_IMAGE) -f ./images/inventory/aggregator/Containerfile .

.PHONY: image-inventory-aggregator-push
image-inventory-aggregator-push: image-inventory-aggregator-local ## Build and push the inventory-aggregator container image
	$(CONTAINER_ENGINE) push $(INVENTORY_AGGREGATOR_IMAGE)

.PHONY: image-inventory-inspector-build
image-inventory-inspector-local: ## Build the inventory-inspector container image
	$(CONTAINER_ENGINE) build -t inventory-inspector:$(INVENTORY_INSPECTOR_TAG) -t $(INVENTORY_INSPECTOR_IMAGE) -f ./images/inventory/inspector/Containerfile .

.PHONY: image-inventory-inspector-push
image-inventory-inspector-push: image-inventory-inspector-local ## Build and push the inventory-inspector container image
	$(CONTAINER_ENGINE) push $(INVENTORY_INSPECTOR_IMAGE)

.PHONY: image-inventory-viewer-build
image-inventory-viewer-local: ## Build the inventory-viewer container image
	$(CONTAINER_ENGINE) build -t inventory-viewer:$(INVENTORY_VIEWER_TAG) -t $(INVENTORY_VIEWER_IMAGE) -f ./images/inventory/viewer/Containerfile .

.PHONY: image-inventory-viewer-push
image-inventory-viewer-push: image-inventory-viewer-local ## Build and push the inventory-viewer container image
	$(CONTAINER_ENGINE) push $(INVENTORY_VIEWER_IMAGE)

image-machina-local: ## Build the machina container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t machina:$(VERSION) -t $(MACHINA_IMAGE) \
		-f ./images/machina/Containerfile .
	$(call trivy-maybe,$(MACHINA_IMAGE))

# Retained for backwards compatibility with external callers (release pipelines).
machina-oci: image-machina-local ## Alias for image-machina-local

machina-oci-push: machina-oci ## Build and push the machina container image
	$(CONTAINER_ENGINE) push $(MACHINA_IMAGE)

image-machine-ops-controller-local: ## Build the machine-ops-controller container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t machine-ops-controller:$(VERSION) -t $(MACHINE_OPS_CONTROLLER_IMAGE) \
		-f ./images/machine-ops-controller/Containerfile .
	$(call trivy-maybe,$(MACHINE_OPS_CONTROLLER_IMAGE))

machine-ops-controller-oci: image-machine-ops-controller-local ## Alias for image-machine-ops-controller-local

machine-ops-controller-oci-push: machine-ops-controller-oci ## Build and push the machine-ops-controller image
	$(CONTAINER_ENGINE) push $(MACHINE_OPS_CONTROLLER_IMAGE)

MACHINA_NAMESPACE ?= unbounded-kube
MACHINA_MANIFEST_TEMPLATES_DIR := deploy/machina
MACHINA_MANIFEST_RENDERED_DIR  := deploy/machina/rendered
MACHINE_OPS_NAMESPACE ?= unbounded-kube
MACHINE_OPS_API_SERVER_ENDPOINT ?=
MACHINE_OPS_OCI_CONFIG_SECRET ?=
MACHINE_OPS_OCI_CONFIG_PROFILE ?= DEFAULT
MACHINE_OPS_OCI_AUTH ?= api_key
MACHINE_OPS_MANIFEST_TEMPLATES_DIR := deploy/machine-ops
MACHINE_OPS_MANIFEST_RENDERED_DIR  := deploy/machine-ops/rendered

machina-manifests: ## Render machina deployment manifests into deploy/machina/rendered
	@mkdir -p $(MACHINA_MANIFEST_RENDERED_DIR)
	@find $(MACHINA_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	@mkdir -p $(MACHINA_MANIFEST_RENDERED_DIR)/crd
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(MACHINA_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(MACHINA_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(MACHINA_NAMESPACE) \
		--set ControllerImage=$(MACHINA_IMAGE)
	@cp $(MACHINA_MANIFEST_TEMPLATES_DIR)/crd/*.yaml $(MACHINA_MANIFEST_RENDERED_DIR)/crd/
	@echo "Rendered machina manifests into $(MACHINA_MANIFEST_RENDERED_DIR) (image: $(MACHINA_IMAGE))"

machine-ops-manifests: ## Render machine-ops-controller manifests into deploy/machine-ops/rendered
	@mkdir -p $(MACHINE_OPS_MANIFEST_RENDERED_DIR)
	@find $(MACHINE_OPS_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(MACHINE_OPS_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(MACHINE_OPS_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(MACHINE_OPS_NAMESPACE) \
		--set ControllerImage=$(MACHINE_OPS_CONTROLLER_IMAGE) \
		--set APIServerEndpoint=$(MACHINE_OPS_API_SERVER_ENDPOINT) \
		--set OCIConfigSecretName=$(MACHINE_OPS_OCI_CONFIG_SECRET) \
		--set OCIConfigProfile=$(MACHINE_OPS_OCI_CONFIG_PROFILE) \
		--set OCIAuth=$(MACHINE_OPS_OCI_AUTH)
	@echo "Rendered machine-ops manifests into $(MACHINE_OPS_MANIFEST_RENDERED_DIR) (image: $(MACHINE_OPS_CONTROLLER_IMAGE))"

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
	$(call trivy-maybe,$(METALMAN_IMAGE))

metalman-oci: image-metalman-local ## Alias for image-metalman-local

metalman-oci-push: metalman-oci ## Build and push the metalman container image
	$(CONTAINER_ENGINE) push $(METALMAN_IMAGE)

##@ Orca

.PHONY: orca orca-build orca-manifests orca-oci orca-oci-push orca-up orca-down orca-reset orca-inttest image-orca-local

orca-build: ## Build the orca binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(ORCA_BIN) $(ORCA_CMD)/main.go

orca: test orca-build ## Build the orca binary (implies test)

orca-manifests: ## Render orca deployment manifests into deploy/orca/rendered
	@mkdir -p $(ORCA_MANIFEST_RENDERED_DIR)
	@find $(ORCA_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete 2>/dev/null || true
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(ORCA_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(ORCA_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(ORCA_NAMESPACE) \
		--set Image=$(ORCA_IMAGE)
	@echo "Rendered orca manifests into $(ORCA_MANIFEST_RENDERED_DIR) (image: $(ORCA_IMAGE))"

image-orca-local: ## Build the orca container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t orca:$(VERSION) -t $(ORCA_IMAGE) \
		-f ./images/orca/Containerfile .

orca-oci: image-orca-local ## Alias for image-orca-local

orca-oci-push: orca-oci ## Build and push the orca container image
	$(CONTAINER_ENGINE) push $(ORCA_IMAGE)

# Dev-cluster proxy targets. The actual implementations live in
# hack/orca/Makefile (see AGENTS.md convention; mirrors hack/net/).
orca-up: ## Bring up the Orca dev harness in a Kind cluster
	$(MAKE) -C hack/orca up

orca-down: ## Tear down the Orca dev harness Kind cluster
	$(MAKE) -C hack/orca down

orca-reset: ## Rebuild orca image and rolling-restart the dev deployment
	$(MAKE) -C hack/orca reset

# orca-inttest mirrors the test/test-race pattern: race detector in CI
# (ubuntu-latest has gcc), no -race locally so developers without a C
# toolchain can still run integration tests.
ifdef CI
orca-inttest: ## Run orca integration tests (LocalStack + Azurite via testcontainers; requires Docker)
	$(GOTEST) -tags=integrationtest -race -timeout 15m ./internal/orca/inttest/...
else
orca-inttest: ## Run orca integration tests (LocalStack + Azurite via testcontainers; requires Docker)
	$(GOTEST) -tags=integrationtest -timeout 15m ./internal/orca/inttest/...
endif

image-net-controller-local: net-frontend resources/cni-plugins-linux-$(HOST_GOARCH)-$(CNI_PLUGINS_VERSION).tgz ## Build the unbounded-net-controller image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		$(if $(PLATFORMS),--platform=$(PLATFORMS),) \
		--target controller \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		--build-arg CNI_PLUGINS_VERSION=$(CNI_PLUGINS_VERSION) \
		--build-arg BUILDARCH=$(HOST_GOARCH) \
		-t $(NET_CONTROLLER_IMAGE) \
		-f ./images/net/Containerfile .
	$(call trivy-maybe,$(NET_CONTROLLER_IMAGE))

image-net-node-local: resources/cni-plugins-linux-$(HOST_GOARCH)-$(CNI_PLUGINS_VERSION).tgz ## Build the unbounded-net-node image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		$(if $(PLATFORMS),--platform=$(PLATFORMS),) \
		--target node \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		--build-arg CNI_PLUGINS_VERSION=$(CNI_PLUGINS_VERSION) \
		--build-arg BUILDARCH=$(HOST_GOARCH) \
		-t $(NET_NODE_IMAGE) \
		-f ./images/net/Containerfile .
	$(call trivy-maybe,$(NET_NODE_IMAGE))

image-net-controller-push: image-net-controller-local ## Build and push the unbounded-net-controller image
	$(CONTAINER_ENGINE) push $(NET_CONTROLLER_IMAGE)

image-net-node-push: image-net-node-local ## Build and push the unbounded-net-node image
	$(CONTAINER_ENGINE) push $(NET_NODE_IMAGE)

images-net-all: image-net-controller-local image-net-node-local ## Build all unbounded-net container images locally

images-net-all-push: image-net-controller-push image-net-node-push ## Build and push all unbounded-net container images

images-local: image-machina-local image-machine-ops-controller-local image-metalman-local image-net-controller-local image-net-node-local ## Build all container images locally

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
	mkdir -p "$(NET_FRONTEND_DIST_DIR)"; \
	find "$(NET_FRONTEND_DIST_DIR)" -mindepth 1 -not -name .gitignore -delete; \
	cp -R "$(NET_FRONTEND_DIR)/dist/." "$(NET_FRONTEND_DIST_DIR)/"; \
	printf '%s\n' "$$frontend_key" > "$(NET_FRONTEND_CACHE_FILE)"

net-frontend-clean: ## Remove frontend node_modules and dist artifacts
	rm -rf "$(NET_FRONTEND_DIR)/node_modules" "$(NET_FRONTEND_DIR)/dist"
	@find "$(NET_FRONTEND_DIST_DIR)" -mindepth 1 -not -name .gitignore -delete 2>/dev/null || true

##@ Net eBPF

net-ebpf-build: ## Compile bpf/unbounded_encap.c to internal/net/ebpf/unbounded_encap_bpfel.o (requires clang-18; see bpf/clang-version)
	@echo "Compiling eBPF programs..."
	@clang-18 -O2 -g -target bpf \
		-I/usr/include \
		-c bpf/unbounded_encap.c \
		-o internal/net/ebpf/unbounded_encap_bpfel.o
	@echo "eBPF programs compiled."

net-ebpf-generate: ## Regenerate bpf/vmlinux.h from pinned kernel and run bpf2go (requires bpftool, curl, dpkg-deb, python3)
	@hack/scripts/net-ebpf-generate.sh

net-ebpf-verify: ## Verify bpf/vmlinux.h matches bpf/btf-kernel-pin and bpf/btf-kernel-pin-hashes
	@hack/scripts/net-ebpf-verify.sh

##@ Net Manifests

net-manifests: ## Render net manifests into $(NET_MANIFEST_RENDERED_DIR)
	@mkdir -p $(NET_MANIFEST_RENDERED_DIR)
	@find $(NET_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	@mkdir -p $(NET_MANIFEST_RENDERED_DIR)/crd
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir "$(NET_MANIFEST_TEMPLATES_DIR)" \
		--output-dir "$(NET_MANIFEST_RENDERED_DIR)" \
		--set Namespace=$(NET_NAMESPACE) \
		--set ControllerImage=$(NET_CONTROLLER_IMAGE) \
		--set NodeImage=$(NET_NODE_IMAGE) \
		--set ForceNotLeader=$(NET_FORCE_NOT_LEADER) \
		--set AzureTenantID=$(NET_AZURE_TENANT_ID) \
		--set ApiserverURL=$(NET_APISERVER_URL)
	@cp $(NET_CRD_DIR)/*.yaml $(NET_MANIFEST_RENDERED_DIR)/crd/
	@echo "Rendered net manifests into $(NET_MANIFEST_RENDERED_DIR) (controller: $(NET_CONTROLLER_IMAGE), node: $(NET_NODE_IMAGE))"

##@ Release Manifests

RELEASE_MANIFESTS_STAGE_DIR := build/release-manifests
RELEASE_MANIFESTS_NAME      := unbounded-manifests-$(VERSION)

release-manifests: machina-manifests machine-ops-manifests net-manifests ## Build stamped combined manifest tarball under build/
	@rm -rf $(RELEASE_MANIFESTS_STAGE_DIR)
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machina
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machine-ops
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/net
	@cp -R $(MACHINA_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machina/
	@cp -R $(MACHINE_OPS_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machine-ops/
	@cp -R $(NET_MANIFEST_RENDERED_DIR)/.     $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/net/
	@echo "$(VERSION)" > $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/VERSION
	@mkdir -p build
	tar czf "build/$(RELEASE_MANIFESTS_NAME).tar.gz" -C $(RELEASE_MANIFESTS_STAGE_DIR) $(RELEASE_MANIFESTS_NAME)
	@echo "Release manifests archive: build/$(RELEASE_MANIFESTS_NAME).tar.gz"

##@ Documentation

docs-serve: ## Start a local Hugo dev server with live-reload
	@command -v hugo >/dev/null 2>&1 || \
		{ echo "error: hugo not found. Install it from:"; \
		  echo "  https://gohugo.io/installation/"; exit 1; }
	cd docs && hugo server
