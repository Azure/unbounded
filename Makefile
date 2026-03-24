# Go parameters
GOCMD=go
GOFMT=gofumpt
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod
GOLINT=golangci-lint run -c .golangci.yaml

KUBECTL_UNBOUNDED_BIN=bin/kubectl-unbounded
KUBECTL_UNBOUNDED_CMD=./cmd/kubectl-unbounded

FORGE_BIN=bin/forge
FORGE_CMD=./hack/cmd/forge

INVENTORY_BIN=bin/inventory
INVENTORY_CMD=./cmd/inventory

MACHINA_BIN=bin/machina
MACHINA_CMD=./cmd/machina
MACHINA_TAG ?= latest
MACHINA_REGISTRY=stargatetmedev.azurecr.io
MACHINA_IMAGE=$(MACHINA_REGISTRY)/machina:$(MACHINA_TAG)
CONTAINER_ENGINE ?= podman

VERSION ?= v0.0.4
STORAGE_ACCOUNT ?= kubemetaldev
BLOB_CONTAINER ?= release

KUBECTL_PLUGIN_PLATFORMS = linux-amd64 linux-arm64 darwin-amd64 darwin-arm64

.PHONY: all fmt lint test kubectl-unbounded kubectl-unbounded-cross krew-manifest forge inventory inventory-amd64 inventory-arm64 machina machina-oci machina-oci-push metalman metalman-oci metalman-oci-push gomod images/ubuntu24/image.yaml push-blobs

all: kubectl-unbounded forge machina

fmt:
	$(GOFMT) -w .

lint: fmt
	$(GOLINT) --fix -E wsl_v5 ./...
	$(GOLINT) ./...

test: lint
	$(GOTEST) ./...

kubectl-unbounded: test
	$(GOBUILD) -o $(KUBECTL_UNBOUNDED_BIN) $(KUBECTL_UNBOUNDED_CMD)/main.go

kubectl-unbounded-cross:
	@for p in $(KUBECTL_PLUGIN_PLATFORMS); do \
		os=$${p%%-*}; \
		arch=$${p##*-}; \
		echo "Building kubectl-unbounded for $$os/$$arch..."; \
		GOOS=$$os GOARCH=$$arch $(GOBUILD) -o bin/kubectl-unbounded-$$p $(KUBECTL_UNBOUNDED_CMD)/main.go; \
		mkdir -p bin/tar-staging; \
		cp bin/kubectl-unbounded-$$p bin/tar-staging/kubectl-unbounded; \
		tar -czf bin/kubectl-unbounded-$$p.tar.gz -C bin/tar-staging kubectl-unbounded; \
		rm -rf bin/tar-staging; \
	done

krew-manifest: kubectl-unbounded-cross
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

forge: test
	$(GOBUILD) -o $(FORGE_BIN) $(FORGE_CMD)/main.go

inventory: inventory-amd64 inventory-arm64
	@HOST_ARCH=$$(uname -m); \
	case "$$HOST_ARCH" in \
		x86_64)  ARCH=amd64 ;; \
		aarch64) ARCH=arm64 ;; \
		*)       echo "unsupported architecture: $$HOST_ARCH" >&2; exit 1 ;; \
	esac; \
	ln -sf inventory-$$ARCH $(INVENTORY_BIN)

inventory-amd64:
	GOOS=linux GOARCH=amd64 $(GOBUILD) -o $(INVENTORY_BIN)-amd64 $(INVENTORY_CMD)/main.go

inventory-arm64:
	GOOS=linux GOARCH=arm64 $(GOBUILD) -o $(INVENTORY_BIN)-arm64 $(INVENTORY_CMD)/main.go

machina: test
	$(GOBUILD) -o $(MACHINA_BIN) $(MACHINA_CMD)/main.go

METALMAN_BIN=bin/metalman
METALMAN_CMD=./cmd/metalman

metalman:
	$(GOFMT) -w $(METALMAN_CMD)
	$(GOLINT) --fix -E wsl_v5 $(METALMAN_CMD)/...
	$(GOLINT) $(METALMAN_CMD)/...
	$(GOTEST) $(METALMAN_CMD)/...
	$(GOBUILD) -o $(METALMAN_BIN) $(METALMAN_CMD)/main.go

machina-oci:
	$(CONTAINER_ENGINE) build -t machina:$(MACHINA_TAG) -t $(MACHINA_IMAGE) -f ./cmd/machina/oci/Containerfile .

machina-oci-push: machina-oci
	$(CONTAINER_ENGINE) push $(MACHINA_IMAGE)

METALMAN_TAG ?= latest
METALMAN_REGISTRY=stargatetmedev.azurecr.io
METALMAN_IMAGE=$(METALMAN_REGISTRY)/metalman:$(METALMAN_TAG)

metalman-oci:
	$(CONTAINER_ENGINE) build -t metalman:$(METALMAN_TAG) -t $(METALMAN_IMAGE) -f ./cmd/metalman/oci/Containerfile .

metalman-oci-push: metalman-oci
	$(CONTAINER_ENGINE) push $(METALMAN_IMAGE)

images/ubuntu24/image.yaml:
	cd images/ubuntu24 && python3 build.py -o image.yaml

push-blobs: images/ubuntu24/image.yaml krew-manifest
	@az storage blob upload \
		--file images/ubuntu24/image.yaml \
		--container-name $(BLOB_CONTAINER) \
		--name $(VERSION)/images/ubuntu24/image.yaml \
		--account-name $(STORAGE_ACCOUNT) \
		--account-key $(AZURE_STORAGE_KEY) \
		--overwrite
	@echo "Uploaded ubuntu24 image to https://$(STORAGE_ACCOUNT).blob.core.windows.net/$(BLOB_CONTAINER)/$(VERSION)/images/ubuntu24/image.yaml"
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

gomod:
	GOPROXY=direct $(GOMOD) tidy
