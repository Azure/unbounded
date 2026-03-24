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

MACHINA_BIN=bin/machina
MACHINA_CMD=./cmd/machina
MACHINA_TAG ?= latest
MACHINA_REGISTRY=stargatetmedev.azurecr.io
MACHINA_IMAGE=$(MACHINA_REGISTRY)/machina:$(MACHINA_TAG)
CONTAINER_ENGINE ?= podman

.PHONY: all fmt lint test kubectl-unbounded forge machina machina-oci machina-oci-push gomod

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

forge: test
	$(GOBUILD) -o $(FORGE_BIN) $(FORGE_CMD)/main.go

machina: test
	$(GOBUILD) -o $(MACHINA_BIN) $(MACHINA_CMD)/main.go

machina-oci:
	$(CONTAINER_ENGINE) build -t machina:$(MACHINA_TAG) -t $(MACHINA_IMAGE) -f ./cmd/machina/oci/Containerfile .

machina-oci-push: machina-oci
	$(CONTAINER_ENGINE) push $(MACHINA_IMAGE)

gomod:
	GOPROXY=direct $(GOMOD) tidy
