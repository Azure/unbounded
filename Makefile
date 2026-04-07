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
AGENT_LDFLAGS=-X github.com/project-unbounded/unbounded-kube/internal/version.Version=$(VERSION) -X github.com/project-unbounded/unbounded-kube/internal/version.GitCommit=$(shell git rev-parse --short HEAD)

MACHINA_BIN=bin/machina
MACHINA_CMD=./cmd/machina
MACHINA_TAG ?= latest
CONTAINER_REGISTRY ?= ghcr.io/project-unbounded
MACHINA_IMAGE=$(CONTAINER_REGISTRY)/machina:$(MACHINA_TAG)
CONTAINER_ENGINE ?= podman

VERSION ?= v0.0.4
STORAGE_ACCOUNT ?= kubemetaldev
BLOB_CONTAINER ?= release

KUBECTL_PLUGIN_PLATFORMS = linux-amd64 linux-arm64 darwin-amd64 darwin-arm64

.PHONY: all help fmt lint test check-deps kubectl-unbounded kubectl-unbounded-cross krew-manifest forge inventory inventory-amd64 inventory-arm64 unbounded-agent machina machina-build machina-oci machina-oci-push machina-manifests metalman metalman-build metalman-oci metalman-oci-push gomod push-blobs

##@ General

all: kubectl-unbounded forge machina ## Build kubectl-unbounded, forge, and machina (default)

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-28s\033[0m %s\n", $$1, $$2 } \
	/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

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

lint: fmt ## Run golangci-lint (implies fmt)
	$(GOLINT) --fix -E wsl_v5 ./...
	$(GOLINT) ./...

test: lint ## Run all tests (implies lint)
	$(GOTEST) ./...

gomod: ## Tidy go.mod and go.sum
	GOPROXY=direct $(GOMOD) tidy

##@ Build

kubectl-unbounded: test machina-manifests ## Build the kubectl-unbounded plugin (implies test)
	$(GOBUILD) -ldflags '$(KUBECTL_UNBOUNDED_LDFLAGS)' -o $(KUBECTL_UNBOUNDED_BIN) $(KUBECTL_UNBOUNDED_CMD)/main.go

kubectl-unbounded-cross: ## Cross-compile kubectl-unbounded for all supported platforms
	@for p in $(KUBECTL_PLUGIN_PLATFORMS); do \
		os=$${p%%-*}; \
		arch=$${p##*-}; \
		echo "Building kubectl-unbounded for $$os/$$arch..."; \
		GOOS=$$os GOARCH=$$arch $(GOBUILD) -ldflags '$(KUBECTL_UNBOUNDED_LDFLAGS)' -o bin/kubectl-unbounded-$$p $(KUBECTL_UNBOUNDED_CMD)/main.go; \
		mkdir -p bin/tar-staging; \
		cp bin/kubectl-unbounded-$$p bin/tar-staging/kubectl-unbounded; \
		tar -czf bin/kubectl-unbounded-$$p.tar.gz -C bin/tar-staging kubectl-unbounded; \
		rm -rf bin/tar-staging; \
	done

krew-manifest: kubectl-unbounded-cross ## Generate the krew plugin manifest with checksums
	@cp deploy/krew/unbounded.yaml.tmpl bin/unbounded.yaml
	@sed -i 's|{{VERSION}}|$(VERSION)|g' bin/unbounded.yaml
	@sed -i 's|{{STORAGE_ACCOUNT}}|$(STORAGE_ACCOUNT)|g' bin/unbounded.yaml
	@sed -i 's|{{BLOB_CONTAINER}}|$(BLOB_CONTAINER)|g' bin/unbounded.yaml
	@for p in $(KUBECTL_PLUGIN_PLATFORMS); do \
		sha=$$(sha256sum bin/kubectl-unbounded-$$p.tar.gz | awk '{print $$1}'); \
		key=$$(echo $$p | tr '-' '_' | tr '[:lower:]' '[:upper:]'); \
		sed -i "s|{{SHA256_$$key}}|$$sha|g" bin/unbounded.yaml; \
	done
	@echo "Generated krew manifest: bin/unbounded.yaml"

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
	GOOS=linux $(GOBUILD) -ldflags '$(AGENT_LDFLAGS)' -o $(AGENT_BIN) $(AGENT_CMD)/main.go

machina-build: machina-manifests ## Build the machina binary (no lint/test)
	$(GOBUILD) -o $(MACHINA_BIN) $(MACHINA_CMD)/main.go

machina: test machina-build ## Build the machina controller (implies test)

METALMAN_BIN=bin/metalman
METALMAN_CMD=./cmd/metalman

metalman-build: ## Build the metalman binary (no lint/test)
	$(GOBUILD) -o $(METALMAN_BIN) $(METALMAN_CMD)/main.go

metalman: check-deps ## Format, lint, test, and build metalman
	$(GOFMT) -w $(METALMAN_CMD) ./internal/metalman
	$(GOLINT) --fix -E wsl_v5 $(METALMAN_CMD)/... ./internal/metalman/...
	$(GOLINT) $(METALMAN_CMD)/... ./internal/metalman/...
	$(GOTEST) $(METALMAN_CMD)/... ./internal/metalman/...
	$(GOBUILD) -o $(METALMAN_BIN) $(METALMAN_CMD)/main.go

##@ Container Images

machina-oci: ## Build the machina container image
	$(CONTAINER_ENGINE) build -t machina:$(MACHINA_TAG) -t $(MACHINA_IMAGE) -f ./images/machina/Containerfile .

machina-oci-push: machina-oci ## Build and push the machina container image
	$(CONTAINER_ENGINE) push $(MACHINA_IMAGE)

machina-manifests: ## Stamp the machina deployment manifest with the container image
	@sed -i 's|image: .*|image: $(MACHINA_IMAGE)|' deploy/machina/04-deployment.yaml
	@echo "Updated deploy/machina/04-deployment.yaml → image: $(MACHINA_IMAGE)"

machina-run: machina ## Replace the in-cluster machina with a locally built binary
	kubectl scale deployment/machina-controller --replicas=0 -n machina-system
	kubectl get configmap machina-config -n machina-system -o jsonpath='{.data.config\.yaml}' > hack/machina-config.yaml
	$(MACHINA_BIN) controller --config=hack/machina-config.yaml

METALMAN_TAG ?= latest
METALMAN_IMAGE=$(CONTAINER_REGISTRY)/metalman:$(METALMAN_TAG)

metalman-oci: ## Build the metalman container image
	$(CONTAINER_ENGINE) build -t metalman:$(METALMAN_TAG) -t $(METALMAN_IMAGE) -f ./images/metalman/Containerfile .

metalman-oci-push: metalman-oci ## Build and push the metalman container image
	$(CONTAINER_ENGINE) push $(METALMAN_IMAGE)

##@ Release

push-blobs: krew-manifest ## Upload release artifacts to Azure Blob Storage
	@for p in $(KUBECTL_PLUGIN_PLATFORMS); do \
		az storage blob upload \
			--file bin/kubectl-unbounded-$$p.tar.gz \
			--container-name $(BLOB_CONTAINER) \
			--name $(VERSION)/kubectl-unbounded-$$p.tar.gz \
			--account-name $(STORAGE_ACCOUNT) \
			--account-key $(AZURE_STORAGE_KEY) \
			--overwrite; \
		echo "Uploaded kubectl-unbounded-$$p.tar.gz to https://$(STORAGE_ACCOUNT).blob.core.windows.net/$(BLOB_CONTAINER)/$(VERSION)/kubectl-unbounded-$$p.tar.gz"; \
	done
	@az storage blob upload \
		--file bin/unbounded.yaml \
		--container-name $(BLOB_CONTAINER) \
		--name $(VERSION)/unbounded.yaml \
		--account-name $(STORAGE_ACCOUNT) \
		--account-key $(AZURE_STORAGE_KEY) \
		--overwrite
	@echo "Uploaded krew manifest to https://$(STORAGE_ACCOUNT).blob.core.windows.net/$(BLOB_CONTAINER)/$(VERSION)/unbounded.yaml"

KUBECTL_UNBOUNDED_BIN=bin/kubectl-unbounded
KUBECTL_UNBOUNDED_CMD=./cmd/kubectl-unbounded
KUBECTL_UNBOUNDED_LDFLAGS=-X github.com/project-unbounded/unbounded-kube/cmd/kubectl-unbounded/app.MetalmanImage=$(METALMAN_IMAGE)
