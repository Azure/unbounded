# Go parameters
GOCMD=go
GOFMT=gofumpt
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod
GOLINT=golangci-lint run -c .golangci.yaml
RACER_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
RACER_CLUSTER_ID ?=
RACER_CONTROLLER_IMAGE ?= $(CONTAINER_REGISTRY)/racer-controller:$(VERSION_TAG)
RACER_DATAPLANE_IMAGE ?= $(CONTAINER_REGISTRY)/racer-dataplane:$(VERSION_TAG)
# Rust dataplane packaging is independent of the Go controller scaffold.
RACER_CARGO ?= cargo
RACER_DATAPLANE_BIN ?= bin/racer-dataplane
RACER_CARGO_TARGET_DIR ?= $(CURDIR)/bin/racer-cargo
RACER_NATIVE_RDMA ?= false
RACER_NATIVE_LIB ?= bin/libracer_rdma.so.1
RACER_PREFIX ?= /usr/local
RACER_LIBDIR ?= $(RACER_PREFIX)/lib
RACER_RUST_IMAGE ?= docker.io/library/rust:1.96.0-bookworm
RACER_RUNTIME_IMAGE ?= docker.io/library/debian:bookworm-slim
GO_PACKAGE_PATTERNS=./api/... ./cmd/... ./deploy/... ./e2e/... ./hack/... ./internal/... ./pkg/...
# e2e packages hold nothing but files behind the e2e build tag, so `go list`
# needs the tag to see them at all. Without it they are silently skipped by
# both the formatter and the linter, which is how they accumulated whitespace
# and unchecked-error violations that CI never reported. Code generation has no
# business in e2e suites, so GO_PACKAGES stays without the tag.
#
# deploy/ holds the embed.go files and the render tests that guard the shipped
# manifests. Those load on a fresh clone by design (see deploy/machina/embed.go),
# so there is no reason for the linter to skip them, and it did.
GO_PACKAGES=$(shell $(GOCMD) list ./api/... ./cmd/... ./hack/... ./internal/... ./pkg/...)
GO_PACKAGE_DIRS=$(shell $(GOCMD) list -tags e2e -f '{{.Dir}}' $(GO_PACKAGE_PATTERNS))

CONTAINER_ENGINE ?= podman
CONTAINER_REGISTRY ?= ghcr.io/azure

# Unified install namespace for all unbounded components. Each component's
# *_NAMESPACE var derives from this by default, so overriding UNBOUNDED_NAMESPACE
# moves everything at once, while a component var can still be overridden
# individually when needed. Components resolve their runtime namespace from the
# POD_NAMESPACE Downward-API env (see internal/unbounded.SystemNamespace), so a
# non-default namespace lines up end to end; when installing to a non-default
# namespace, pass `kubectl unbounded machine register --namespace <ns>` so the
# SSH secret and its Machine ref land where machina runs.
UNBOUNDED_NAMESPACE ?= unbounded-system

FORGE_BIN=bin/forge
FORGE_CMD=./hack/cmd/forge

RELCTL_BIN=bin/relctl
RELCTL_CMD=./hack/cmd/relctl

AGENT_ARTIFACTS_BUILDER_BIN=bin/agent-artifacts-builder
AGENT_ARTIFACTS_BUILDER_CMD=./hack/cmd/agent-artifacts-builder

INVENTORY_AGENT_BIN=bin/inventory-agent
INVENTORY_AGENT_CMD=./cmd/inventory/inventory-agent

INVENTORY_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
INVENTORY_MANIFEST_TEMPLATES_DIR := deploy/inventory
INVENTORY_MANIFEST_RENDERED_DIR  := deploy/inventory/rendered

INVENTORY_AGGREGATOR_BIN=bin/inventory-aggregator
INVENTORY_AGGREGATOR_CMD=./cmd/inventory/inventory-aggregator
INVENTORY_AGGREGATOR_TAG ?= $(VERSION_TAG)
INVENTORY_AGGREGATOR_IMAGE=$(CONTAINER_REGISTRY)/inventory-aggregator:$(INVENTORY_AGGREGATOR_TAG)

INVENTORY_INSPECTOR_BIN=bin/inventory-inspector
INVENTORY_INSPECTOR_CMD=./cmd/inventory/inventory-inspector
INVENTORY_INSPECTOR_TAG ?= $(VERSION_TAG)
INVENTORY_INSPECTOR_IMAGE=$(CONTAINER_REGISTRY)/inventory-inspector:$(INVENTORY_INSPECTOR_TAG)

INVENTORY_VIEWER_BIN=bin/inventory-viewer
INVENTORY_VIEWER_CMD=./cmd/inventory/inventory-viewer
INVENTORY_VIEWER_TAG ?= $(VERSION_TAG)
INVENTORY_VIEWER_IMAGE=$(CONTAINER_REGISTRY)/inventory-viewer:$(INVENTORY_VIEWER_TAG)

AGENT_BIN=bin/unbounded-agent
AGENT_CMD=./cmd/agent

MACHINA_BIN=bin/machina
MACHINA_CMD=./cmd/machina
# Fall back to the default even when the variable is set to an empty string, not
# just when unset. GNU make's `?=` treats a set-but-empty environment variable as
# already defined; a Docker `ARG MACHINA_IMAGE=` exported into the operator image
# build as "" therefore defeated `?=` and blanked the image baked into the
# operator's embedded machina manifests. `override` also neutralizes an empty
# value passed on the command line; `=` keeps CONTAINER_REGISTRY/VERSION_TAG
# expansion deferred (VERSION_TAG is defined later in this file).
ifeq ($(strip $(MACHINA_IMAGE)),)
override MACHINA_IMAGE = $(CONTAINER_REGISTRY)/machina:$(VERSION_TAG)
endif

TOKEN_REFRESHER_BIN=bin/token-refresher
TOKEN_REFRESHER_CMD=./cmd/token-refresher
TOKEN_REFRESHER_IMAGE ?= $(CONTAINER_REGISTRY)/token-refresher:$(VERSION_TAG)

MACHINE_OPS_CONTROLLER_BIN=bin/machine-ops-controller
MACHINE_OPS_CONTROLLER_CMD=./cmd/machine-ops-controller
MACHINE_OPS_CONTROLLER_IMAGE ?= $(CONTAINER_REGISTRY)/machine-ops-controller:$(VERSION_TAG)
MACHINE_OPS_CONTROLLER_NAME ?= machine-ops-controller
MACHINE_OPS_PROVIDER ?=
MACHINE_OPS_SITE ?=

METALMAN_BIN=bin/metalman
METALMAN_CMD=./cmd/metalman
NETBOOT_IMAGE ?= $(CONTAINER_REGISTRY)/netboot:$(VERSION_TAG)

PLAYPEN_TAG ?= $(VERSION_TAG)
PLAYPEN_IMAGE ?= $(CONTAINER_REGISTRY)/playpen:$(PLAYPEN_TAG)

UNBOUNDED_OPERATOR_BIN=bin/unbounded-operator
UNBOUNDED_OPERATOR_CMD=./cmd/unbounded-operator
UNBOUNDED_OPERATOR_IMAGE ?= $(CONTAINER_REGISTRY)/unbounded-operator:$(VERSION_TAG)
UNBOUNDED_OPERATOR_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
UNBOUNDED_OPERATOR_API_SERVER_ENDPOINT ?=
# Full image-repository prefix the operator resolves component images under. It
# derives from CONTAINER_REGISTRY so it cannot drift from the operator's own
# image: overriding CONTAINER_REGISTRY (as the release workflow does per fork)
# points components at the same registry/org as the operator.
UNBOUNDED_OPERATOR_IMAGE_REGISTRY ?= $(CONTAINER_REGISTRY)
UNBOUNDED_OPERATOR_REAP_LEGACY_RESOURCES ?= true
export UNBOUNDED_OPERATOR_API_SERVER_ENDPOINT
UNBOUNDED_OPERATOR_MANIFEST_TEMPLATES_DIR := deploy/unbounded-operator
UNBOUNDED_OPERATOR_MANIFEST_RENDERED_DIR  := deploy/unbounded-operator/rendered

TOKEN_REFRESHER_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
TOKEN_REFRESHER_MANIFEST_TEMPLATES_DIR := deploy/token-refresher
TOKEN_REFRESHER_MANIFEST_RENDERED_DIR  := deploy/token-refresher/rendered

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

# Gantry (peer-to-peer OCI distribution)
GANTRY_BIN=bin/gantry
GANTRY_CMD=./cmd/gantry
GANTRY_IMAGE ?= $(CONTAINER_REGISTRY)/gantry:$(VERSION_TAG)
GANTRY_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
GANTRY_CHART_DIR := deploy/gantry/chart
GANTRY_MANIFEST_RENDERED_DIR  := deploy/gantry/rendered
GANTRY_OPERATOR_RENDER_DIR    := tmp/gantry-operator-render
GANTRY_SUPPORT_RENDER_DIR     := tmp/gantry-support-render
GANTRY_CHART_VERSION ?= 0.0.0-dev
GANTRY_CHART_APP_VERSION ?= $(VERSION_TAG)
GANTRY_CHART_PACKAGE_DIR := build/charts
GANTRY_CHART_STAGE_DIR := tmp/gantry-chart-package
GANTRY_CHART_IMAGE_REPOSITORY ?= $(CONTAINER_REGISTRY)/gantry

# Version is derived from the latest git tag. Override with: make VERSION=v1.0.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# VERSION_TAG is VERSION made safe for use as a Docker image tag: git describe can
# surface a nearest tag containing a slash (e.g. agent-artifacts/v20260710), which
# is invalid in an image reference. VERSION itself is kept intact for the embedded
# version string (ldflags) and release artifact paths.
VERSION_TAG ?= $(subst /,-,$(VERSION))
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

# Shared ldflags for injecting version metadata into all binaries.
STAMP_LDFLAGS=-X github.com/Azure/unbounded/internal/version.Version=$(VERSION) \
              -X github.com/Azure/unbounded/internal/version.GitCommit=$(GIT_COMMIT) \
              -X github.com/Azure/unbounded/internal/version.BuildTime=$(BUILD_TIME)
METALMAN_LDFLAGS=$(STAMP_LDFLAGS) -X github.com/Azure/unbounded/internal/metalman/commands.DefaultNetbootImage=$(NETBOOT_IMAGE)

METALMAN_IMAGE=$(CONTAINER_REGISTRY)/metalman:$(VERSION_TAG)

# Orca configuration
ORCA_BIN=bin/orca
ORCA_CMD=./cmd/orca
ORCA_IMAGE ?= $(CONTAINER_REGISTRY)/orca:$(VERSION_TAG)
ORCA_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
ORCA_MANIFEST_TEMPLATES_DIR := deploy/orca
ORCA_MANIFEST_RENDERED_DIR  := deploy/orca/rendered

# Dev image tag used by the orca-kind-up / orca-install paths.
# Pinned to :dev so kind load and rollout-restart use a stable
# identifier (the auto-derived VERSION can include slashes from git
# tags like images/agent-ubuntu2404-nvidia/v..., which are illegal
# in OCI tags). Override with ORCA_DEV_IMAGE=... when targeting a
# remote registry.
ORCA_DEV_IMAGE ?= ghcr.io/azure/orca:dev

# Kind cluster name used by orca-kind-up / orca-kind-down. Mirrors
# the default in hack/orca/kind-up.sh.
ORCA_KIND_CLUSTER ?= orca-dev

KUBECTL_UNBOUNDED_LDFLAGS=$(STAMP_LDFLAGS)

# --- Net (unbounded-net) configuration -------------------------------------
# Container images for the net controller and node agent.
# See the MACHINA_IMAGE note above: default when empty-or-unset so an empty
# Docker ARG cannot blank the images baked into the embedded net manifests.
ifeq ($(strip $(NET_CONTROLLER_IMAGE)),)
override NET_CONTROLLER_IMAGE = $(CONTAINER_REGISTRY)/unbounded-net-controller:$(VERSION_TAG)
endif
ifeq ($(strip $(NET_NODE_IMAGE)),)
override NET_NODE_IMAGE = $(CONTAINER_REGISTRY)/unbounded-net-node:$(VERSION_TAG)
endif

# CNI plugins version baked into the net-node image. Keep in sync with the
# defaults in images/net-{node,controller}/Dockerfile and the workflow envs.
CNI_PLUGINS_VERSION  ?= v1.9.1

# Host architecture for local image builds (amd64 / arm64). Used to pick the
# right CNI plugins tarball for the current machine.
HOST_GOARCH := $(shell $(GOCMD) env GOARCH)

# Kubernetes deploy knobs.
NET_NAMESPACE           ?= $(UNBOUNDED_NAMESPACE)
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

.PHONY: all help fmt lint lint-actions test build vulncheck check-deps kubectl-unbounded kubectl-unbounded-build install-tools install-protoc install-helm generate kubectl-unbounded forge relctl relctl-build agent-artifacts-builder agent-artifacts-builder-build orcadev unbounded-agent machina machina-build machina-oci machina-oci-push machina-manifests machine-ops-controller machine-ops-controller-build machine-ops-controller-oci machine-ops-controller-oci-push machine-ops-manifests metalman metalman-build metalman-oci metalman-oci-push unbounded-operator unbounded-operator-build unbounded-operator-manifests playpen-manifests e2e-gantry e2e-playpen gomod docs-serve unbounded-net-controller unbounded-net-controller-build unbounded-net-node unbounded-net-node-build unbounded-net-routeplan-debug unping unping-build unroute unroute-build license-check notice notice-check gantry gantry-build gantry-manifests inventory-manifests
.PHONY: net-frontend net-frontend-clean net-ebpf-build net-ebpf-generate net-ebpf-verify net-manifests gantry-chart-lint gantry-chart-package release-bom release-manifests unbounded-operator-release-manifest
.PHONY: image-machina-local image-token-refresher-local image-machine-ops-controller-local image-metalman-local image-unbounded-operator-local image-unbounded-operator-push image-playpen-local image-net-controller-local image-net-node-local image-gantry-local image-gantry-push images-local
.PHONY: image-net-controller-push image-net-node-push images-net-all images-net-all-push

##@ General

all: kubectl-unbounded forge relctl machina machine-ops-controller token-refresher unbounded-operator unbounded-net-controller unbounded-net-node unbounded-net-routeplan-debug unping unroute gantry ## Build all binaries (default)

help: ## Show this help
	@echo ""
	@echo "Usage: make <target> [VAR=value ...]"
	@echo ""
	@echo "General:"
	@echo "  all                              Build all Go binaries (default)"
	@echo "  help                             Show this help"
	@echo "  install-tools                    Install gofumpt, golangci-lint, protoc-gen-go, protoc-gen-go-grpc, controller-gen, actionlint"
	@echo "  install-protoc                   Download pinned protoc into bin/protoc/"
	@echo "  install-helm                     Download pinned Helm into bin/"
	@echo ""
	@echo "Development:"
	@echo "  fmt                              Format Go source (gofumpt + wsl_v5)"
	@echo "  lint                             Run golangci-lint and actionlint"
	@echo "  lint-actions                     Run actionlint over .github/workflows"
	@echo "  test                             Run all tests"
	@echo "  build                            Compile all Go packages"
	@echo "  generate                         Run go generate (deepcopy, CRDs, protobuf)"
	@echo "  vulncheck                        Run govulncheck; fails only on fixable vulnerabilities"
	@echo "  gomod                            go mod tidy"
	@echo "  e2e-gantry                       Run the kind-based Gantry e2e suite"
	@echo "  e2e-racer                        Run the operator-installed Racer e2e suite (prebuilt images)"
	@echo "  e2e-playpen                      Run the kind-based playpen e2e suite"
	@echo "  license-check                    Verify project-owned license declarations"
	@echo "  notice                           Regenerate NOTICE from Go and npm dependencies"
	@echo "  notice-check                     Verify NOTICE is in sync with dependencies"
	@echo "  toolchain-shell                  Drop into the toolchain container with the repo mounted at /project (set TOOLCHAIN_FLAVOR=fedora|ubuntu to pick a flavor)"
	@echo "  toolchain-build                  Rebuild the toolchain container image (honors TOOLCHAIN_FLAVOR)"
	@echo ""
	@echo "Build:"
	@echo "  kubectl-unbounded                Build kubectl-unbounded plugin"
	@echo "  forge                            Build forge dev tool"
	@echo "  relctl                           Build the relctl release tool"
	@echo "  agent-artifacts-builder          Build offline agent artifacts builder"
	@echo "  agent-artifacts-builder-build    Build offline agent artifacts builder without test"
	@echo "  orcadev                          Build orcadev dev/debug tool"
	@echo "  inventory-all                    Build all inventory components"
	@echo "  inventory-agent                  Build inventory-agent for amd64 and arm64"
	@echo "  inventory-agent-build            Build inventory-agent for the host GOOS/GOARCH without test"
	@echo "  inventory-agent-amd64            Build inventory-agent for amd64"
	@echo "  inventory-agent-arm64            Build inventory-agent for arm64"
	@echo "  inventory-aggregator             Build inventory-aggregator"
	@echo "  inventory-aggregator-build       Build inventory-aggregator without test"
	@echo "  inventory-inspector              Build inventory-inspector"
	@echo "  inventory-inspector-build        Build inventory-inspector without test"
	@echo "  inventory-viewer                 Build inventory-viewer"
	@echo "  inventory-viewer-build           Build inventory-viewer without test"
	@echo "  unbounded-agent                  Build unbounded-agent (linux)"
	@echo "  machina | machina-build          Build machina controller (with/without lint/test)"
	@echo "  machine-ops-controller           Build machine-ops-controller"
	@echo "  metalman | metalman-build        Build metalman controller (with/without lint/test)"
	@echo "  unbounded-operator | unbounded-operator-build  Build the top-level Site operator"
	@echo "  unbounded-net-controller         Build net controller"
	@echo "  unbounded-net-node               Build net node agent"
	@echo "  unbounded-net-routeplan-debug    Build net routeplan debug tool"
	@echo "  unping                           Build unping health-check utility"
	@echo "  unroute                          Build unroute eBPF inspection utility"
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
	@echo "  image-token-refresher-local      Build token-refresher image"
	@echo "  image-machine-ops-controller-local Build machine-ops-controller image"
	@echo "  image-metalman-local             Build metalman image"
	@echo "  image-unbounded-operator-local   Build unbounded-operator image"
	@echo "  image-unbounded-operator-push    Build and push unbounded-operator image"
	@echo "  image-playpen-local              Build playpen image"
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
	@echo "  unbounded-operator-manifests     Render unbounded-operator manifests into deploy/unbounded-operator/rendered"
	@echo "  gantry-chart-lint                Validate the standalone Gantry Helm chart"
	@echo "  gantry-chart-package             Package the standalone Gantry Helm chart"
	@echo "  unbounded-operator-release-manifest Build a versioned, directly applicable operator manifest under build/"
	@echo ""
	@echo "Net Kubernetes (apply to current kubectl context):"
	@echo "  See \`make -C hack/net help\` for cluster deploy/undeploy targets."
	@echo ""
	@echo "Orca Dev Install (see hack/orca/README.md for the developer quickstart):"
	@echo "  orca | orca-build                Build orca binary (with/without lint/test)"
	@echo "  orcadev                          Build orcadev dev/debug tool"
	@echo "  orca-install                     Install Orca into the current kubectl context"
	@echo "  orca-kind-up | orca-up           Create kind cluster + install Orca (build + side-load image)"
	@echo "  orca-kind-down | orca-down       Delete the kind cluster"
	@echo "  orca-reset                       Rebuild image and rolling-restart Orca on kind"
	@echo "  orca-inttest                     Run orca integration tests (Docker required)"
	@echo ""
	@echo "Documentation:"
	@echo "  docs-serve                       Start local Hugo dev server"
	@echo ""
	@echo "Racer:"
	@echo "  racer-controller-build           Build the Go controller scaffold"
	@echo "  racer-test | racer-generate | racer-manifests  Check/generate/render Racer scaffolds"
	@echo "  racer-dataplane-build             Build the locked Rust release binary into bin/"
	@echo "  racer-dataplane-native-build      Build the optional real-libibverbs adapter into bin/"
	@echo "  racer-dataplane-native-install    Install adapter (DESTDIR, RACER_PREFIX, RACER_LIBDIR)"
	@echo "  image-racer-dataplane-local       Build local image (RACER_NATIVE_RDMA=false|true)"
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

GOFUMPT_VERSION ?= v0.11.0
GOLANGCI_LINT_VERSION ?= v2.13.1
PROTOC_GEN_GO_VERSION ?= v1.36.11
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.1
CONTROLLER_GEN_VERSION ?= v0.21.0
ACTIONLINT_VERSION ?= v1.7.12
HELM_VERSION ?= 3.21.3
HELM ?= $(CURDIR)/bin/helm
HELM_STAMP := $(CURDIR)/bin/.helm-v$(HELM_VERSION)

HELM_UNAME_S := $(shell uname -s)
HELM_UNAME_M := $(shell uname -m)
ifeq ($(HELM_UNAME_S),Darwin)
	HELM_OS := darwin
else
	HELM_OS := linux
endif
ifeq ($(HELM_UNAME_M),x86_64)
	HELM_ARCH := amd64
else ifeq ($(HELM_UNAME_M),aarch64)
	HELM_ARCH := arm64
else ifeq ($(HELM_UNAME_M),arm64)
	HELM_ARCH := arm64
else
	HELM_ARCH := unsupported
endif

ifeq ($(HELM_OS)-$(HELM_ARCH),linux-amd64)
	HELM_SHA256 := 15e041a93a590dce8100f39385cd98c84a765c9e36aeeb9e2dc6ff9e4769e2e0
else ifeq ($(HELM_OS)-$(HELM_ARCH),linux-arm64)
	HELM_SHA256 := 67f58155079ff9ffab98ba5c88daff0ed9b542f3a4732f5dd426dde7dd0f5244
else ifeq ($(HELM_OS)-$(HELM_ARCH),darwin-amd64)
	HELM_SHA256 := 76d0db4730b05d3d625eee11e80f0721b32b4d8422f4e5d093de6337bf3ac9f8
else ifeq ($(HELM_OS)-$(HELM_ARCH),darwin-arm64)
	HELM_SHA256 := 19879a848cad832b7a1ac24b767a481d20fb3b95ab53a220849649422ada144e
endif

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

install-tools: ## Install development tools (gofumpt, golangci-lint, protoc-gen-go, protoc-gen-go-grpc, controller-gen, actionlint)
	go install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
	go install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)

install-protoc: $(PROTOC) ## Download pinned protoc into bin/protoc/

install-helm: $(HELM) ## Download pinned Helm into bin/

$(HELM_STAMP):
	@test -n "$(HELM_SHA256)" || { echo "unsupported Helm platform $(HELM_OS)-$(HELM_ARCH)" >&2; exit 1; }
	@mkdir -p $(dir $(HELM)) tmp
	@archive="helm-v$(HELM_VERSION)-$(HELM_OS)-$(HELM_ARCH).tar.gz"; \
	  echo "Downloading Helm v$(HELM_VERSION) for $(HELM_OS)-$(HELM_ARCH)..."; \
	  curl -fsSL --max-time 30 -o "tmp/$$archive" "https://get.helm.sh/$$archive"; \
	  if command -v sha256sum >/dev/null 2>&1; then \
	    actual=$$(sha256sum "tmp/$$archive" | awk '{print $$1}'); \
	  else \
	    actual=$$(shasum -a 256 "tmp/$$archive" | awk '{print $$1}'); \
	  fi; \
	  test "$$actual" = "$(HELM_SHA256)" || { echo "Helm checksum mismatch: got $$actual" >&2; rm -f "tmp/$$archive"; exit 1; }; \
	  tar -xzf "tmp/$$archive" -C tmp; \
	  cp "tmp/$(HELM_OS)-$(HELM_ARCH)/helm" "$(HELM)"; \
	  chmod +x "$(HELM)"; \
	  rm -rf "tmp/$$archive" "tmp/$(HELM_OS)-$(HELM_ARCH)"; \
	  touch "$(HELM_STAMP)"
	@$(HELM) version --short

$(HELM): $(HELM_STAMP)
	@test -x $(HELM) || { rm -f $(HELM_STAMP); $(MAKE) $(HELM_STAMP); }

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

# --fix applies every auto-fixable linter and formatter that .golangci.yaml
# enables, wsl_v5 among them; it does not need to be named again here. It is
# also what applies gofumpt's group-params rule, which the plain `gofumpt -w`
# recipe line does not: gofumpt's standalone -extra flag is all-or-nothing and
# would pull in rules .golangci.yaml does not ask for.
fmt: check-deps ## Format all Go source files (gofumpt + golangci-lint auto-fixes)
	$(GOFMT) -w $(GO_PACKAGE_DIRS)
	$(GOLINT) --fix $(GO_PACKAGE_PATTERNS)

# lint runs the same checks locally and in CI and does NOT auto-fix. Run
# `make fmt` to apply fixes. wsl_v5 is enforced via .golangci.yaml.
lint: ## Run golangci-lint and actionlint (matches CI; run `make fmt` to auto-fix)
	$(GOLINT) $(GO_PACKAGE_PATTERNS)
	@$(MAKE) --no-print-directory lint-actions

# lint-actions is part of `lint` because a workflow file is only ever parsed
# when it is dispatched. A duplicate `default:` key made release-prepare.yaml
# undispatchable while every check in CI stayed green, and the workflow that
# mints release tags is the worst possible place to find that out by hand.
#
# The shellcheck and pyflakes integrations are disabled explicitly rather than
# left at their defaults: GitHub-hosted runners ship shellcheck and most
# workstations do not, so leaving them on would make `make lint` mean something
# different in CI than it does locally. Enabling shellcheck over every `run:`
# block is worth doing on its own terms, with its own findings list.
lint-actions: ## Run actionlint over .github/workflows
	@command -v actionlint >/dev/null 2>&1 || \
		{ echo "error: actionlint not found. Install it with:"; \
		  echo "  go install github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION)"; exit 1; }
	actionlint -shellcheck= -pyflakes=

ifdef CI
# In CI each job is independent; skip chained prerequisites.

test: machina-manifests token-refresher-manifests machine-ops-manifests playpen-manifests net-manifests unbounded-operator-manifests gantry-manifests ## Run all tests with race detector
	$(GOTEST) -race ./...

else
# Locally, chain test -> lint for convenience.

test: lint machina-manifests token-refresher-manifests machine-ops-manifests playpen-manifests net-manifests unbounded-operator-manifests gantry-manifests ## Run all tests (implies lint)
	$(GOTEST) ./...

endif

e2e-gantry: $(HELM) ## Run the kind-based Gantry e2e suite
	CONTAINER_ENGINE="$(CONTAINER_ENGINE)" KIND_EXPERIMENTAL_PROVIDER="$(CONTAINER_ENGINE)" PATH="$(CURDIR)/bin:$$PATH" \
		$(GOTEST) -tags=e2e -count=1 -timeout=120m -v ./e2e/gantry

.PHONY: e2e-racer
e2e-racer: ## Run the operator-installed Racer e2e suite with prebuilt Docker images
	KIND_EXPERIMENTAL_PROVIDER=docker PATH="$(CURDIR)/bin:$$PATH" \
		$(GOTEST) -tags=e2e -count=1 -timeout=10m -v ./e2e/racer

e2e-playpen: ## Run the kind-based playpen e2e suite
	$(GOTEST) -tags=e2e ./e2e/playpen -v -timeout=10m

.PHONY: racer-controller racer-controller-build racer-test racer-server-test racer-envtest racer-scale racer-generate racer-manifests
racer-controller: racer-server-test racer-controller-build ## Test and build the Racer controller

racer-controller-build: ## Build the Racer controller without lint/test
	@mkdir -p bin
	$(GOBUILD) -o bin/racer-controller ./cmd/racer-controller

.PHONY: racer-dataplane-build racer-dataplane-native-build racer-dataplane-native-install image-racer-dataplane-local
racer-dataplane-build: ## Build the locked Rust release binary; optionally enable the RDMA loader
	@case "$(RACER_NATIVE_RDMA)" in true|false) ;; *) echo "RACER_NATIVE_RDMA must be true or false" >&2; exit 1 ;; esac
	$(RACER_CARGO) build --locked --release --manifest-path cmd/racer-dataplane/Cargo.toml \
		--target-dir "$(RACER_CARGO_TARGET_DIR)" --bin racer-dataplane \
		--no-default-features $(if $(filter true,$(RACER_NATIVE_RDMA)),--features rdma)
	install -D -m 0755 "$(RACER_CARGO_TARGET_DIR)/release/racer-dataplane" "$(RACER_DATAPLANE_BIN)"

racer-dataplane-native-build: ## Compile against installed libibverbs headers and libraries
	CC="$(CC)" sh images/racer-dataplane/build-native.sh \
		cmd/racer-dataplane/native/rdma.c "$(RACER_NATIVE_LIB)"

racer-dataplane-native-install: racer-dataplane-native-build ## Stage or install the optional native library
	install -D -m 0755 "$(RACER_NATIVE_LIB)" "$(DESTDIR)$(RACER_LIBDIR)/libracer_rdma.so.1"

image-racer-dataplane-local: ## Build the Racer dataplane image locally (single-arch)
	@case "$(RACER_NATIVE_RDMA)" in true|false) ;; *) echo "RACER_NATIVE_RDMA must be true or false" >&2; exit 1 ;; esac
	$(CONTAINER_ENGINE) build \
		--build-arg RUST_IMAGE="$(RACER_RUST_IMAGE)" \
		--build-arg RUNTIME_IMAGE="$(RACER_RUNTIME_IMAGE)" \
		--build-arg RACER_NATIVE_RDMA="$(RACER_NATIVE_RDMA)" \
		--build-arg VERSION="$(VERSION)" --build-arg GIT_COMMIT="$(GIT_COMMIT)" \
		-t racer-dataplane:$(VERSION_TAG) -t $(RACER_DATAPLANE_IMAGE) \
		-f ./images/racer-dataplane/Containerfile .
	$(call trivy-maybe,$(RACER_DATAPLANE_IMAGE))

racer-server-test: ## Lint and race-test the Racer server and deployment contracts
	$(GOLINT) ./api/racer/... ./internal/racer/... ./cmd/racer-controller/... ./deploy/racer/...
	$(GOTEST) -race ./api/racer/... ./internal/racer/... ./cmd/racer-controller/... ./deploy/racer/...

racer-test: racer-server-test ## Check Racer server and committed Rust contracts
	cargo fmt --manifest-path cmd/racer-dataplane/Cargo.toml --check
	cargo check --locked --manifest-path cmd/racer-dataplane/Cargo.toml --all-targets --all-features
	cargo test --locked --manifest-path cmd/racer-dataplane/Cargo.toml --all-features

racer-envtest: ## Run real API-server, manager election, TLS and crash-recovery tests
	@test -n "$(KUBEBUILDER_ASSETS)" || { echo "Set KUBEBUILDER_ASSETS to repository-local envtest binaries"; exit 1; }
	@mkdir -p tmp/racer-envtest
	TMPDIR="$(CURDIR)/tmp/racer-envtest" KUBEBUILDER_ASSETS="$(KUBEBUILDER_ASSETS)" $(GOTEST) -race ./internal/racer -run '^TestEnvtestServer$$' -count=1 -v -timeout=3m

racer-scale: ## Measure 100,000-member reconciliation and publication waiters (not HTTPS capacity)
	@mkdir -p tmp/racer-scale
	TMPDIR="$(CURDIR)/tmp/racer-scale" RACER_SCALE=1 GOMAXPROCS=8 $(GOTEST) ./internal/racer -run '^TestServerScale$$' -count=1 -v -timeout=3m

racer-generate: ## Generate Racer deepcopy and CRD artifacts
	$(GOCMD) generate ./api/racer/v1alpha1

racer-manifests: ## Render Racer controller manifests
	@mkdir -p deploy/racer/rendered/crd
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir deploy/racer --output-dir deploy/racer/rendered \
		--set Namespace=$(RACER_NAMESPACE) --set ClusterID=$(RACER_CLUSTER_ID) \
		--set InitializationState=$(RACER_INITIALIZATION_STATE) \
		--set ControllerImage=$(RACER_CONTROLLER_IMAGE) --set DataplaneImage=$(RACER_DATAPLANE_IMAGE)
	@cp deploy/racer/crd/*.yaml deploy/racer/rendered/crd/

build: machina-manifests token-refresher-manifests machine-ops-manifests playpen-manifests net-manifests unbounded-operator-manifests gantry-manifests ## Build all Go packages
	$(GOBUILD) ./...

generate: install-protoc ## Run go generate for API types (deepcopy, CRDs) and protobuf
	PATH="$(PROTOC_DIR)/bin:$$PATH" $(GOCMD) generate $(GO_PACKAGES)

vulncheck: machina-manifests token-refresher-manifests machine-ops-manifests playpen-manifests net-manifests unbounded-operator-manifests gantry-manifests ## Run govulncheck; fails only on vulnerabilities that have an available fix
	@# The JSON stream is the documented programmatic interface. The gate owns
	@# the verdict, so govulncheck is not asked for one: in JSON mode it exits 0
	@# whether or not it found anything, and a non-zero exit here means the scan
	@# itself failed, which must still fail the target. Progress goes to stderr
	@# and stays visible.
	@mkdir -p tmp
	$(GOCMD) tool govulncheck -format json $(GO_PACKAGE_PATTERNS) > tmp/govulncheck.json
	$(GOCMD) run ./hack/cmd/vulncheck-gate tmp/govulncheck.json

gomod: ## Tidy go.mod and go.sum
	$(GOMOD) tidy

license-check: ## Verify project-owned source license declarations
	@set -e; \
	[ "$$(sha256sum LICENSE | cut -d' ' -f1)" = "c71d239df91726fc519c6eb72d318ec65820627232b2f796219e87dcf35d0ab4" ] || { \
		echo "ERROR: LICENSE is not the Apache License 2.0 text." >&2; \
		exit 1; \
	}; \
	stale="$$(git grep --untracked -nI -E \
		'Licensed under the MIT License|SPDX-License-Identifier:[[:space:]]*MIT|org\.opencontainers\.image\.licenses="MIT"|MIT License\]\(LICENSE\)|[; ]MIT License<|license[[:space:]]*[=:][[:space:]]*"?MIT"?' -- \
		':(top)**' \
		':(top,exclude)Makefile' \
		':(top,exclude)NOTICE' \
		':(top,exclude)frontend/package-lock.json' \
		':(top,exclude)hack/cmd/notice/**' || { status=$$?; [ "$$status" -eq 1 ] || exit "$$status"; })"; \
	notice_stale="$$(git grep --untracked -nI -E \
		'Licensed under the MIT License|SPDX-License-Identifier:[[:space:]]*MIT' \
		-- ':(top)hack/cmd/notice/**' || { status=$$?; [ "$$status" -eq 1 ] || exit "$$status"; })"; \
	if [ -n "$$stale$$notice_stale" ]; then \
		echo "ERROR: stale repository-owned MIT declaration(s):" >&2; \
		[ -z "$$stale" ] || printf '%s\n' "$$stale" >&2; \
		[ -z "$$notice_stale" ] || printf '%s\n' "$$notice_stale" >&2; \
		exit 1; \
	fi; \
	missing="$$(git ls-files -- '*.go' \
		':(exclude)**/vendor/**' \
		':(exclude)**/third_party/**' \
		':(exclude)**/node_modules/**' \
		':(exclude)**/target/**' | while IFS= read -r file; do \
		case "$$file" in \
			*.go) grep -qE '^// Code generated .* DO NOT EDIT\.$$' "$$file" && continue ;; \
		esac; \
		grep -qF 'SPDX-License-Identifier: Apache-2.0' "$$file" || printf '%s\n' "$$file"; \
	done)"; \
	if [ -n "$$missing" ]; then \
		echo "ERROR: tracked hand-written Go source missing SPDX-License-Identifier: Apache-2.0:" >&2; \
		printf '%s\n' "$$missing" >&2; \
		exit 1; \
	fi

notice: ## Regenerate NOTICE from Go and npm dependencies
	@if [ ! -d "$(NET_FRONTEND_DIR)/node_modules" ]; then \
		echo "ERROR: $(NET_FRONTEND_DIR)/node_modules not found." >&2; \
		echo "Run: (cd $(NET_FRONTEND_DIR) && npm ci)" >&2; \
		exit 1; \
	fi
	$(GOCMD) run ./hack/cmd/notice generate --output NOTICE

notice-check: ## Verify NOTICE is in sync with Go and npm dependencies
	@if [ ! -d "$(NET_FRONTEND_DIR)/node_modules" ]; then \
		echo "ERROR: $(NET_FRONTEND_DIR)/node_modules not found." >&2; \
		echo "Run: (cd $(NET_FRONTEND_DIR) && npm ci)" >&2; \
		exit 1; \
	fi
	$(GOCMD) run ./hack/cmd/notice check --notice NOTICE

.PHONY: toolchain-shell
toolchain-shell: ## Drop into the toolchain container with the repo mounted at /project (builds the image on first use)
	@./images/toolchain/toolchain.sh

.PHONY: toolchain-build
toolchain-build: ## Rebuild the toolchain container image (otherwise built lazily on first toolchain-shell use)
	@TOOLCHAIN_REBUILD=1 ./images/toolchain/toolchain.sh true

##@ Build

kubectl-unbounded-build: machina-manifests net-manifests unbounded-operator-manifests ## Build the kubectl-unbounded binary (no lint/test)
	$(GOBUILD) -ldflags '$(KUBECTL_UNBOUNDED_LDFLAGS)' -o $(KUBECTL_UNBOUNDED_BIN) $(KUBECTL_UNBOUNDED_CMD)/main.go

kubectl-unbounded: test kubectl-unbounded-build ## Build the kubectl-unbounded plugin (implies test)

forge: test ## Build the forge dev tool (implies test)
	$(GOBUILD) -o $(FORGE_BIN) $(FORGE_CMD)/main.go

relctl-build: ## Build the relctl release tool (no lint/test)
	$(GOBUILD) -o $(RELCTL_BIN) $(RELCTL_CMD)/main.go

relctl: test relctl-build ## Build the relctl release tool (implies test)

agent-artifacts-builder-build: ## Build the offline agent artifacts builder (no lint/test)
	$(GOBUILD) -o $(AGENT_ARTIFACTS_BUILDER_BIN) $(AGENT_ARTIFACTS_BUILDER_CMD)/main.go

agent-artifacts-builder: test agent-artifacts-builder-build ## Build the offline agent artifacts builder (implies test)

ORCADEV_BIN=bin/orcadev
ORCADEV_CMD=./hack/cmd/orcadev

orcadev: test ## Build the orcadev dev/debug tool (implies test)
	$(GOBUILD) -o $(ORCADEV_BIN) $(ORCADEV_CMD)/main.go

.PHONY: inventory-all
inventory-all: inventory-agent inventory-aggregator inventory-inspector inventory-viewer ## Build all inventory components

.PHONY: inventory-agent
inventory-agent: test inventory-agent-amd64 inventory-agent-arm64 ## Build inventory-agent for amd64 and arm64, symlink to host arch (implies test)
	@HOST_ARCH=$$(uname -m); \
	case "$$HOST_ARCH" in \
		x86_64)  ARCH=amd64 ;; \
		aarch64) ARCH=arm64 ;; \
		*)       echo "unsupported architecture: $$HOST_ARCH" >&2; exit 1 ;; \
	esac; \
	ln -sf inventory-agent-$$ARCH $(INVENTORY_AGENT_BIN)

# inventory-agent-build honors GOOS/GOARCH from the environment so the
# container image can cross-compile natively on the build host, matching the
# other component -build targets. The -amd64/-arm64 variants stay for local
# use, where both are wanted side by side.
.PHONY: inventory-agent-build
inventory-agent-build: ## Build the inventory-agent binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(INVENTORY_AGENT_BIN) $(INVENTORY_AGENT_CMD)

.PHONY: inventory-agent-amd64
inventory-agent-amd64: ## Build inventory-agent for linux/amd64
	GOOS=linux GOARCH=amd64 $(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(INVENTORY_AGENT_BIN)-amd64 $(INVENTORY_AGENT_CMD)

.PHONY: inventory-agent-arm64
inventory-agent-arm64: ## Build inventory-agent for linux/arm64
	GOOS=linux GOARCH=arm64 $(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(INVENTORY_AGENT_BIN)-arm64 $(INVENTORY_AGENT_CMD)

.PHONY: inventory-aggregator-build
inventory-aggregator-build: ## Build the inventory-aggregator binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(INVENTORY_AGGREGATOR_BIN) $(INVENTORY_AGGREGATOR_CMD)

.PHONY: inventory-aggregator
inventory-aggregator: test inventory-aggregator-build ## Build the inventory-aggregator (implies test)

.PHONY: inventory-inspector-build
inventory-inspector-build: ## Build the inventory-inspector binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(INVENTORY_INSPECTOR_BIN) $(INVENTORY_INSPECTOR_CMD)

.PHONY: inventory-inspector
inventory-inspector: test inventory-inspector-build ## Build the inventory-inspector (implies test)

.PHONY: inventory-viewer-build
inventory-viewer-build: ## Build the inventory-viewer binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(INVENTORY_VIEWER_BIN) $(INVENTORY_VIEWER_CMD)

.PHONY: inventory-viewer
inventory-viewer: test inventory-viewer-build ## Build the inventory-viewer web server (implies test)

unbounded-agent: test ## Build the unbounded-agent for linux (implies test)
	GOOS=linux $(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(AGENT_BIN) $(AGENT_CMD)/main.go

machina-build: machina-manifests ## Build the machina binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(MACHINA_BIN) $(MACHINA_CMD)/main.go

machina: test machina-build ## Build the machina controller (implies test)

.PHONY: token-refresher-build
token-refresher-build: ## Build the token-refresher binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(TOKEN_REFRESHER_BIN) $(TOKEN_REFRESHER_CMD)/main.go

.PHONY: token-refresher
token-refresher: test token-refresher-build ## Build token-refresher (implies test)

machine-ops-controller-build: machine-ops-manifests ## Build the machine-ops-controller binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(MACHINE_OPS_CONTROLLER_BIN) $(MACHINE_OPS_CONTROLLER_CMD)

machine-ops-controller: test machine-ops-controller-build ## Build the machine-ops-controller (implies test)

metalman-build: ## Build the metalman binary (no lint/test)
	$(GOBUILD) -ldflags '$(METALMAN_LDFLAGS)' -o $(METALMAN_BIN) $(METALMAN_CMD)/main.go

metalman: test metalman-build ## Build the metalman controller (implies test)

unbounded-operator-build: machina-manifests token-refresher-manifests net-manifests unbounded-operator-manifests gantry-manifests ## Build the unbounded-operator binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(UNBOUNDED_OPERATOR_BIN) $(UNBOUNDED_OPERATOR_CMD)/main.go

unbounded-operator: test unbounded-operator-build ## Build the unbounded-operator (implies test)

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

##@ Gantry (peer-to-peer OCI distribution)

gantry-build: ## Build the gantry binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(GANTRY_BIN) $(GANTRY_CMD)

gantry: test gantry-build ## Build gantry (implies test)

gantry-manifests: $(HELM) ## Render the Gantry operator profile into deploy/gantry/rendered
	@mkdir -p $(GANTRY_MANIFEST_RENDERED_DIR)
	@find $(GANTRY_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	@rm -rf $(GANTRY_OPERATOR_RENDER_DIR)
	$(HELM) template gantry $(GANTRY_CHART_DIR) \
		--namespace $(GANTRY_NAMESPACE) \
		--values $(GANTRY_CHART_DIR)/values-operator.yaml \
		--skip-schema-validation \
		--set-string image.reference=$(GANTRY_IMAGE) \
		--output-dir $(GANTRY_OPERATOR_RENDER_DIR)
	@cp $(GANTRY_OPERATOR_RENDER_DIR)/gantry/templates/*.yaml $(GANTRY_MANIFEST_RENDERED_DIR)/
	@rm -rf $(GANTRY_SUPPORT_RENDER_DIR)
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir deploy/gantry \
		--output-dir $(GANTRY_SUPPORT_RENDER_DIR) \
		--set Namespace=$(GANTRY_NAMESPACE)
	@cp -R $(GANTRY_SUPPORT_RENDER_DIR)/. $(GANTRY_MANIFEST_RENDERED_DIR)/
	@rm -rf $(GANTRY_OPERATOR_RENDER_DIR) $(GANTRY_SUPPORT_RENDER_DIR)
	@echo "Rendered gantry manifests into $(GANTRY_MANIFEST_RENDERED_DIR) (namespace: $(GANTRY_NAMESPACE))"

gantry-chart-lint: $(HELM) ## Validate the standalone Gantry Helm chart
	$(HELM) lint $(GANTRY_CHART_DIR)

gantry-chart-package: gantry-chart-lint ## Package the standalone Gantry Helm chart
	@mkdir -p $(GANTRY_CHART_PACKAGE_DIR)
	@rm -f $(GANTRY_CHART_PACKAGE_DIR)/gantry-$(GANTRY_CHART_VERSION).tgz
	@rm -rf $(GANTRY_CHART_STAGE_DIR)
	$(GOCMD) run ./hack/cmd/gantry-chart-stage \
		--source $(GANTRY_CHART_DIR) \
		--output $(GANTRY_CHART_STAGE_DIR) \
		--image-repository $(GANTRY_CHART_IMAGE_REPOSITORY)
	$(HELM) package $(GANTRY_CHART_STAGE_DIR) \
		--version $(GANTRY_CHART_VERSION) \
		--app-version $(GANTRY_CHART_APP_VERSION) \
		--destination $(GANTRY_CHART_PACKAGE_DIR)
	@rm -rf $(GANTRY_CHART_STAGE_DIR)

# Inventory render knobs. SSLMode/Password feed the database config and
# secret templates; Password is base64-encoded data and defaults empty so
# the generic target stays secret-free (hack/inventory-dev/local.sh supplies
# a generated value).
INVENTORY_SSL_MODE        ?= disable
INVENTORY_PG_PASSWORD_B64 ?=

inventory-manifests: ## Render inventory deployment manifests into deploy/inventory/rendered
	@mkdir -p $(INVENTORY_MANIFEST_RENDERED_DIR)
	@find $(INVENTORY_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(INVENTORY_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(INVENTORY_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(INVENTORY_NAMESPACE) \
		--set AggregatorImage=$(INVENTORY_AGGREGATOR_IMAGE) \
		--set InspectorImage=$(INVENTORY_INSPECTOR_IMAGE) \
		--set ViewerImage=$(INVENTORY_VIEWER_IMAGE) \
		--set SSLMode=$(INVENTORY_SSL_MODE) \
		--set Password=$(INVENTORY_PG_PASSWORD_B64)
	@echo "Rendered inventory manifests into $(INVENTORY_MANIFEST_RENDERED_DIR) (namespace: $(INVENTORY_NAMESPACE))"

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

.PHONY: image-inventory-aggregator-local
image-inventory-aggregator-local: ## Build the inventory-aggregator container image
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t inventory-aggregator:$(INVENTORY_AGGREGATOR_TAG) -t $(INVENTORY_AGGREGATOR_IMAGE) \
		-f ./images/inventory/aggregator/Containerfile .
	$(call trivy-maybe,$(INVENTORY_AGGREGATOR_IMAGE))

.PHONY: image-inventory-aggregator-push
image-inventory-aggregator-push: image-inventory-aggregator-local ## Build and push the inventory-aggregator container image
	$(CONTAINER_ENGINE) push $(INVENTORY_AGGREGATOR_IMAGE)

.PHONY: image-inventory-inspector-local
image-inventory-inspector-local: ## Build the inventory-inspector container image
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t inventory-inspector:$(INVENTORY_INSPECTOR_TAG) -t $(INVENTORY_INSPECTOR_IMAGE) \
		-f ./images/inventory/inspector/Containerfile .
	$(call trivy-maybe,$(INVENTORY_INSPECTOR_IMAGE))

.PHONY: image-inventory-inspector-push
image-inventory-inspector-push: image-inventory-inspector-local ## Build and push the inventory-inspector container image
	$(CONTAINER_ENGINE) push $(INVENTORY_INSPECTOR_IMAGE)

.PHONY: image-inventory-viewer-local
image-inventory-viewer-local: ## Build the inventory-viewer container image
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t inventory-viewer:$(INVENTORY_VIEWER_TAG) -t $(INVENTORY_VIEWER_IMAGE) \
		-f ./images/inventory/viewer/Containerfile .
	$(call trivy-maybe,$(INVENTORY_VIEWER_IMAGE))

.PHONY: image-inventory-viewer-push
image-inventory-viewer-push: image-inventory-viewer-local ## Build and push the inventory-viewer container image
	$(CONTAINER_ENGINE) push $(INVENTORY_VIEWER_IMAGE)

image-machina-local: ## Build the machina container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t machina:$(VERSION_TAG) -t $(MACHINA_IMAGE) \
		-f ./images/machina/Containerfile .
	$(call trivy-maybe,$(MACHINA_IMAGE))

# Retained for backwards compatibility with external callers (release pipelines).
machina-oci: image-machina-local ## Alias for image-machina-local

machina-oci-push: machina-oci ## Build and push the machina container image
	$(CONTAINER_ENGINE) push $(MACHINA_IMAGE)

image-token-refresher-local: ## Build the token-refresher container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t token-refresher:$(VERSION_TAG) -t $(TOKEN_REFRESHER_IMAGE) \
		-f ./images/token-refresher/Containerfile .
	$(call trivy-maybe,$(TOKEN_REFRESHER_IMAGE))

image-machine-ops-controller-local: ## Build the machine-ops-controller container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t machine-ops-controller:$(VERSION_TAG) -t $(MACHINE_OPS_CONTROLLER_IMAGE) \
		-f ./images/machine-ops-controller/Containerfile .
	$(call trivy-maybe,$(MACHINE_OPS_CONTROLLER_IMAGE))

machine-ops-controller-oci: image-machine-ops-controller-local ## Alias for image-machine-ops-controller-local

machine-ops-controller-oci-push: machine-ops-controller-oci ## Build and push the machine-ops-controller image
	$(CONTAINER_ENGINE) push $(MACHINE_OPS_CONTROLLER_IMAGE)

MACHINA_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
MACHINA_API_SERVER_ENDPOINT ?=
MACHINA_MANIFEST_TEMPLATES_DIR := deploy/machina
MACHINA_MANIFEST_RENDERED_DIR  := deploy/machina/rendered
MACHINE_OPS_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
MACHINE_OPS_API_SERVER_ENDPOINT ?=
MACHINE_OPS_MANIFEST_TEMPLATES_DIR := deploy/machine-ops
MACHINE_OPS_MANIFEST_RENDERED_DIR  := deploy/machine-ops/rendered
PLAYPEN_NAMESPACE ?= playpen
PLAYPEN_AMD64_RUNNERS ?= 2
PLAYPEN_ARM64_RUNNERS ?= 2
PLAYPEN_RUNNER_WIREGUARD_HOST_PORT_START ?= 51820
PLAYPEN_RUNNER_WIREGUARD_HOST_PORT_END ?= 51899
PLAYPEN_CONTROL_PLANE_COUNT ?= 1
PLAYPEN_CONTROL_PLANE_VERSIONS ?= v1.33.0
PLAYPEN_CONTROL_PLANE_IMAGE ?= rancher/k3s:{version}-k3s1
PLAYPEN_CONTROL_PLANE_API_SERVER_HOST_PORT_START ?= 16443
PLAYPEN_CONTROL_PLANE_API_SERVER_HOST_PORT_END ?= 16499
PLAYPEN_MANIFEST_TEMPLATES_DIR := deploy/playpen
PLAYPEN_MANIFEST_RENDERED_DIR  := deploy/playpen/rendered

machina-manifests: ## Render machina deployment manifests into deploy/machina/rendered
	@mkdir -p $(MACHINA_MANIFEST_RENDERED_DIR)
	@find $(MACHINA_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	@mkdir -p $(MACHINA_MANIFEST_RENDERED_DIR)/crd
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(MACHINA_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(MACHINA_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(MACHINA_NAMESPACE) \
		--set ControllerImage=$(MACHINA_IMAGE) \
		--set APIServerEndpoint=$(MACHINA_API_SERVER_ENDPOINT)
	@cp $(MACHINA_MANIFEST_TEMPLATES_DIR)/crd/*.yaml $(MACHINA_MANIFEST_RENDERED_DIR)/crd/
	@echo "Rendered machina manifests into $(MACHINA_MANIFEST_RENDERED_DIR) (image: $(MACHINA_IMAGE))"

.PHONY: token-refresher-manifests
token-refresher-manifests: ## Render token-refresher manifests into deploy/token-refresher/rendered
	@mkdir -p $(TOKEN_REFRESHER_MANIFEST_RENDERED_DIR)
	@find $(TOKEN_REFRESHER_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(TOKEN_REFRESHER_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(TOKEN_REFRESHER_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(TOKEN_REFRESHER_NAMESPACE) \
		--set Image=$(TOKEN_REFRESHER_IMAGE)
	@echo "Rendered token-refresher manifests into $(TOKEN_REFRESHER_MANIFEST_RENDERED_DIR) (image: $(TOKEN_REFRESHER_IMAGE))"

unbounded-operator-manifests: ## Render unbounded-operator manifests into deploy/unbounded-operator/rendered
	@mkdir -p $(UNBOUNDED_OPERATOR_MANIFEST_RENDERED_DIR)
	@find $(UNBOUNDED_OPERATOR_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(UNBOUNDED_OPERATOR_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(UNBOUNDED_OPERATOR_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(UNBOUNDED_OPERATOR_NAMESPACE) \
		--set OperatorImage=$(UNBOUNDED_OPERATOR_IMAGE) \
		--set ImageRegistry=$(UNBOUNDED_OPERATOR_IMAGE_REGISTRY) \
		--set "APIServerEndpoint=$${UNBOUNDED_OPERATOR_API_SERVER_ENDPOINT}" \
		--set ReapLegacyResources=$(UNBOUNDED_OPERATOR_REAP_LEGACY_RESOURCES)
	@echo "Rendered unbounded-operator manifests into $(UNBOUNDED_OPERATOR_MANIFEST_RENDERED_DIR) (image: $(UNBOUNDED_OPERATOR_IMAGE))"

machine-ops-manifests: ## Render machine-ops-controller manifests into deploy/machine-ops/rendered
	@mkdir -p $(MACHINE_OPS_MANIFEST_RENDERED_DIR)
	@find $(MACHINE_OPS_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(MACHINE_OPS_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(MACHINE_OPS_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(MACHINE_OPS_NAMESPACE) \
		--set ControllerName=$(MACHINE_OPS_CONTROLLER_NAME) \
		--set ControllerImage=$(MACHINE_OPS_CONTROLLER_IMAGE) \
		--set Provider=$(MACHINE_OPS_PROVIDER) \
		--set Site=$(MACHINE_OPS_SITE) \
		--set APIServerEndpoint=$(MACHINE_OPS_API_SERVER_ENDPOINT)
	@echo "Rendered machine-ops manifests into $(MACHINE_OPS_MANIFEST_RENDERED_DIR) (image: $(MACHINE_OPS_CONTROLLER_IMAGE))"

playpen-manifests: ## Render playpen operator and runner manifests into deploy/playpen/rendered
	@mkdir -p $(PLAYPEN_MANIFEST_RENDERED_DIR)
	@find $(PLAYPEN_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(PLAYPEN_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(PLAYPEN_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(PLAYPEN_NAMESPACE) \
		--set PlaypenImage=$(PLAYPEN_IMAGE) \
		--set RunnerAMD64Count=$(PLAYPEN_AMD64_RUNNERS) \
		--set RunnerARM64Count=$(PLAYPEN_ARM64_RUNNERS) \
		--set RunnerWireGuardHostPortStart=$(PLAYPEN_RUNNER_WIREGUARD_HOST_PORT_START) \
		--set RunnerWireGuardHostPortEnd=$(PLAYPEN_RUNNER_WIREGUARD_HOST_PORT_END) \
		--set ControlPlaneCount=$(PLAYPEN_CONTROL_PLANE_COUNT) \
		--set ControlPlaneVersions=$(PLAYPEN_CONTROL_PLANE_VERSIONS) \
		--set ControlPlaneImage=$(PLAYPEN_CONTROL_PLANE_IMAGE) \
		--set ControlPlaneAPIServerHostPortStart=$(PLAYPEN_CONTROL_PLANE_API_SERVER_HOST_PORT_START) \
		--set ControlPlaneAPIServerHostPortEnd=$(PLAYPEN_CONTROL_PLANE_API_SERVER_HOST_PORT_END)
	@echo "Rendered playpen manifests into $(PLAYPEN_MANIFEST_RENDERED_DIR) (image: $(PLAYPEN_IMAGE))"

machina-run: machina ## Replace the in-cluster machina with a locally built binary
	kubectl scale deployment/machina-controller --replicas=0 -n $(MACHINA_NAMESPACE)
	kubectl get configmap machina-config -n $(MACHINA_NAMESPACE) -o jsonpath='{.data.config\.yaml}' > hack/machina-config.yaml
	$(MACHINA_BIN) controller --config=hack/machina-config.yaml

image-metalman-local: ## Build the metalman container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		--build-arg CONTAINER_REGISTRY=$(CONTAINER_REGISTRY) \
		-t metalman:$(VERSION_TAG) -t $(METALMAN_IMAGE) \
		-f ./images/metalman/Containerfile .
	$(call trivy-maybe,$(METALMAN_IMAGE))

metalman-oci: image-metalman-local ## Alias for image-metalman-local

metalman-oci-push: metalman-oci ## Build and push the metalman container image
	$(CONTAINER_ENGINE) push $(METALMAN_IMAGE)

image-unbounded-operator-local: ## Build the unbounded-operator container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t unbounded-operator:$(VERSION_TAG) -t $(UNBOUNDED_OPERATOR_IMAGE) \
		-f ./images/unbounded-operator/Containerfile .
	$(call trivy-maybe,$(UNBOUNDED_OPERATOR_IMAGE))

image-unbounded-operator-push: image-unbounded-operator-local ## Build and push the unbounded-operator image
	$(CONTAINER_ENGINE) push $(UNBOUNDED_OPERATOR_IMAGE)

image-playpen-local: ## Build the playpen container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t playpen:$(PLAYPEN_TAG) -t $(PLAYPEN_IMAGE) \
		-f ./images/playpen/Containerfile .
	$(call trivy-maybe,$(PLAYPEN_IMAGE))

image-gantry-local: ## Build the gantry container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t gantry:$(VERSION_TAG) -t $(GANTRY_IMAGE) \
		-f ./images/gantry/Containerfile .
	$(call trivy-maybe,$(GANTRY_IMAGE))

image-gantry-push: image-gantry-local ## Build and push the gantry container image
	$(CONTAINER_ENGINE) push $(GANTRY_IMAGE)

##@ Orca

.PHONY: orca orca-build orca-manifests orca-oci orca-oci-push \
        orca-install orca-kind-up orca-kind-down orca-up orca-down orca-reset \
        orca-inttest image-orca-local

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
		-t orca:$(VERSION_TAG) -t $(ORCA_IMAGE) \
		-f ./images/orca/Containerfile .

orca-oci: image-orca-local ## Alias for image-orca-local

orca-oci-push: orca-oci ## Build and push the orca container image
	$(CONTAINER_ENGINE) push $(ORCA_IMAGE)

# Dev install entrypoints. There is exactly one supported install
# path: ./hack/orca/setup-orca.sh. The Make targets below are thin
# wrappers around it for muscle memory. See hack/orca/README.md for
# the developer quickstart.

orca-install: ## Install Orca into the current kubectl context (no kind assumptions)
	@ctx=$$(kubectl config current-context 2>/dev/null || echo none); \
	case "$$ctx" in \
	  kind-*) : ;; \
	  *) \
	    if [ "$(ORCA_DEV_IMAGE)" = "ghcr.io/azure/orca:dev" ]; then \
	      echo "ERROR: current kubectl context '$$ctx' is not kind-*, but ORCA_DEV_IMAGE is the default ghcr.io/azure/orca:dev." >&2; \
	      echo "       That image is not in any registry your cluster can pull from and the install will ImagePullBackOff." >&2; \
	      echo "       Either:" >&2; \
	      echo "         (a) switch to a kind context: kubectl config use-context kind-orca-dev" >&2; \
	      echo "         (b) build, push, and pass a reachable image:" >&2; \
	      echo "             make image-orca-local ORCA_IMAGE=my-registry/orca:dev" >&2; \
	      echo "             podman push my-registry/orca:dev" >&2; \
	      echo "             make orca-install ORCA_DEV_IMAGE=my-registry/orca:dev" >&2; \
	      exit 1; \
	    fi ;; \
	esac
	./hack/orca/setup-orca.sh --image $(ORCA_DEV_IMAGE) --namespace $(ORCA_NAMESPACE)

orca-kind-up: ## Create the orca-dev kind cluster + install Orca (build + side-load image)
	./hack/orca/kind-up.sh --name $(ORCA_KIND_CLUSTER)
	./hack/orca/setup-orca.sh \
		--context kind-$(ORCA_KIND_CLUSTER) \
		--namespace $(ORCA_NAMESPACE) \
		--image $(ORCA_DEV_IMAGE) \
		--build --kind-load

orca-kind-down: ## Delete the orca-dev kind cluster
	./hack/orca/kind-down.sh --name $(ORCA_KIND_CLUSTER)

# Back-compat aliases. orca-up / orca-down used to be the only
# entrypoints and developers' muscle memory still reaches for them.
orca-up: orca-kind-up ## Alias for orca-kind-up
orca-down: orca-kind-down ## Alias for orca-kind-down

orca-reset: ## Rebuild orca image, side-load into kind, rolling-restart the deployment
	$(MAKE) image-orca-local ORCA_IMAGE=$(ORCA_DEV_IMAGE)
	tmp=$$(mktemp -d) && trap "rm -rf $$tmp" EXIT && \
		$(CONTAINER_ENGINE) save -o $$tmp/orca.tar $(ORCA_DEV_IMAGE) && \
		kind load image-archive $$tmp/orca.tar --name $(ORCA_KIND_CLUSTER)
	kubectl --context kind-$(ORCA_KIND_CLUSTER) -n $(ORCA_NAMESPACE) rollout restart deployment/orca
	kubectl --context kind-$(ORCA_KIND_CLUSTER) -n $(ORCA_NAMESPACE) rollout status deployment/orca --timeout=180s

# orca-inttest mirrors the test/test-race pattern: race detector in CI
# (ubuntu-latest has gcc), no -race locally so developers without a C
# toolchain can still run integration tests.
ifdef CI
orca-inttest: ## Run orca integration tests (Garage + Azurite via testcontainers; requires Docker)
	$(GOTEST) -tags=integrationtest -race -timeout 15m ./internal/orca/inttest/...
else
orca-inttest: ## Run orca integration tests (Garage + Azurite via testcontainers; requires Docker)
	$(GOTEST) -tags=integrationtest -race -count=1 -timeout 15m ./internal/orca/inttest/...
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

images-local: image-machina-local image-token-refresher-local image-machine-ops-controller-local image-metalman-local image-unbounded-operator-local image-net-controller-local image-net-node-local image-gantry-local ## Build all container images locally

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
UNBOUNDED_OPERATOR_RELEASE_MANIFEST := build/unbounded-operator-$(VERSION).yaml
RELEASE_BOM_OUTPUT ?= build/unbounded-release-bom-$(VERSION).json

release-bom: ## Generate a digest-pinned release bill of materials
	$(GOCMD) run ./hack/cmd/release-bom \
		--tag "$(VERSION)" \
		--commit "$(GIT_COMMIT)" \
		--registry "$(CONTAINER_REGISTRY)" \
		--net-cni-version "$(CNI_PLUGINS_VERSION)" \
		--output "$(RELEASE_BOM_OUTPUT)"

unbounded-operator-release-manifest: UNBOUNDED_OPERATOR_API_SERVER_ENDPOINT :=
unbounded-operator-release-manifest: unbounded-operator-manifests ## Build a versioned, directly applicable operator manifest under build/
	@mkdir -p build
	@cat $$(ls -1 "$(UNBOUNDED_OPERATOR_MANIFEST_RENDERED_DIR)"/*.yaml | LC_ALL=C sort) > "$(UNBOUNDED_OPERATOR_RELEASE_MANIFEST)"
	@echo "Operator release manifest: $(UNBOUNDED_OPERATOR_RELEASE_MANIFEST)"

# The inventory tree is shipped without its pg-creds Secret. That template
# renders from INVENTORY_PG_PASSWORD_B64, which is empty for a release build, so
# including it would put a Secret with an empty password inside a signed tarball
# and invite someone to apply it. Operators supply their own credentials.
release-manifests: NET_APISERVER_URL :=
release-manifests: UNBOUNDED_OPERATOR_API_SERVER_ENDPOINT :=
release-manifests: machina-manifests machine-ops-manifests token-refresher-manifests net-manifests gantry-manifests unbounded-operator-manifests inventory-manifests ## Build stamped combined manifest tarball under build/
	@rm -rf $(RELEASE_MANIFESTS_STAGE_DIR)
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machina
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/token-refresher
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machine-ops
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/net
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/gantry
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/unbounded-operator
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/inventory
	@cp -R $(MACHINA_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machina/
	@cp -R $(TOKEN_REFRESHER_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/token-refresher/
	@cp -R $(MACHINE_OPS_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machine-ops/
	@cp -R $(NET_MANIFEST_RENDERED_DIR)/.     $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/net/
	@cp -R $(GANTRY_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/gantry/
	@cp -R $(UNBOUNDED_OPERATOR_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/unbounded-operator/
	@cp -R $(INVENTORY_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/inventory/
	@rm -f $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/inventory/common/03-secret.yaml
	@cp LICENSE NOTICE $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/
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
