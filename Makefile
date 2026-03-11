# Go parameters
GOCMD=go
GOFMT=gofumpt
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod
GOLINT=golangci-lint run -c .golangci.yaml

MACHINA_BIN=bin/machina
MACHINA_CMD=./cmd/machina
MACHINA_TAG ?= latest
MACHINA_REGISTRY=stargatetmedev.azurecr.io
MACHINA_IMAGE=$(MACHINA_REGISTRY)/machina:$(MACHINA_TAG)
CONTAINER_ENGINE ?= podman

machina:
	$(GOFMT) -w $(MACHINA_CMD)
	$(GOLINT) --fix -E wsl_v5 $(MACHINA_CMD)/...
	$(GOLINT) $(MACHINA_CMD)/...
	$(GOTEST) $(MACHINA_CMD)/...
	$(GOBUILD) -o $(MACHINA_BIN) $(MACHINA_CMD)/main.go

machina-oci:
	$(CONTAINER_ENGINE) build -t machina:$(MACHINA_TAG) -t $(MACHINA_IMAGE) -f ./cmd/machina/oci/Containerfile .

machina-oci-push: machina-oci
	$(CONTAINER_ENGINE) push $(MACHINA_IMAGE)

gomod:
	GOPROXY=direct $(GOMOD) tidy
