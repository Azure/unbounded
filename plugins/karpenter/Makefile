# The Unbounded Karpenter plugin is its own Go module, because upstream
# Karpenter pins Kubernetes libraries older than the root module's. Run these
# targets from the repository root as `make karpenter-<target>`.

ROOT := $(abspath ../..)
GOLANGCI_LINT ?= golangci-lint

export CGO_ENABLED := 0

.PHONY: all tidy fmt lint test

all: lint test

tidy:
	go mod tidy

# golangci-lint resolves paths from the directory of its config file, so
# `golangci-lint fmt` with the root config would format the whole repository.
# Format only this module, the way the root `make fmt` does.
fmt:
	gofumpt -w ./internal
	$(GOLANGCI_LINT) run --fix -c "$(ROOT)/.golangci.yaml" ./...

lint:
	$(GOLANGCI_LINT) run -c "$(ROOT)/.golangci.yaml" ./...

test:
	go test -count=1 ./...
