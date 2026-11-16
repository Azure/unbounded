# The Unbounded Karpenter plugin is its own Go module, because upstream
# Karpenter pins Kubernetes libraries older than the root module's. Run these
# targets from the repository root as `make karpenter-<target>`.

ROOT := $(abspath ../..)
BIN_DIR ?= $(ROOT)/bin
RENDERED_DIR ?= $(ROOT)/deploy/karpenter/rendered
GOLANGCI_LINT ?= golangci-lint
CONTAINER_ENGINE ?= docker

VERSION ?= dev
NAMESPACE ?= unbounded-system
IMAGE ?= ghcr.io/azure/unbounded-karpenter:$(VERSION)
PRICE_CURRENCY ?= USD

export CGO_ENABLED := 0

.PHONY: all tidy fmt lint test build manifests image

all: lint test build

tidy:
	go mod tidy

# golangci-lint resolves paths from the directory of its config file, so
# `golangci-lint fmt` with the root config would format the whole repository.
# Format only this module, the way the root `make fmt` does.
fmt:
	gofumpt -w ./cmd ./internal
	$(GOLANGCI_LINT) run --fix -c "$(ROOT)/.golangci.yaml" ./...

lint:
	$(GOLANGCI_LINT) run -c "$(ROOT)/.golangci.yaml" ./...

# The API-server tests in internal/integration skip unless KUBEBUILDER_ASSETS
# is set. Use the repository's envtest assets when they are present, and give
# the binary test the built binary.
ENVTEST_ASSETS ?= $(firstword $(wildcard $(ROOT)/bin/envtest/k8s/*))

test: build
	KUBEBUILDER_ASSETS="$(or $(KUBEBUILDER_ASSETS),$(ENVTEST_ASSETS))" \
	UNBOUNDED_KARPENTER_BINARY="$(BIN_DIR)/unbounded-karpenter" \
		go test -count=1 ./...

build:
	go build -trimpath \
		-ldflags "-X sigs.k8s.io/karpenter/pkg/operator.Version=$(VERSION)" \
		-o "$(BIN_DIR)/unbounded-karpenter" ./cmd/unbounded-karpenter

manifests:
	@mkdir -p "$(RENDERED_DIR)"
	go run ./cmd/render-manifests \
		--namespace "$(NAMESPACE)" \
		--image "$(IMAGE)" \
		--price-currency "$(PRICE_CURRENCY)" \
		--output "$(RENDERED_DIR)/karpenter.yaml"

image:
	$(CONTAINER_ENGINE) build --build-arg VERSION="$(VERSION)" \
		-t "$(IMAGE)" -f "$(ROOT)/images/karpenter/Containerfile" "$(ROOT)"
