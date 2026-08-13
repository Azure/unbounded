# Go parameters
GOCMD=go
GOFMT=gofumpt
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOMOD=$(GOCMD) mod
GOLINT=golangci-lint run -c .golangci.yaml
GO_PACKAGE_PATTERNS=./api/... ./cmd/... ./hack/... ./internal/... ./pkg/...
GO_PACKAGES=$(shell $(GOCMD) list $(GO_PACKAGE_PATTERNS))
GO_PACKAGE_DIRS=$(shell $(GOCMD) list -f '{{.Dir}}' $(GO_PACKAGE_PATTERNS))

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

AGENT_ARTIFACTS_BUILDER_BIN=bin/agent-artifacts-builder
AGENT_ARTIFACTS_BUILDER_CMD=./hack/cmd/agent-artifacts-builder

INVENTORY_AGENT_BIN=bin/inventory-agent
INVENTORY_AGENT_CMD=./cmd/inventory/inventory-agent

INVENTORY_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
INVENTORY_MANIFEST_TEMPLATES_DIR := deploy/inventory
INVENTORY_MANIFEST_RENDERED_DIR  := deploy/inventory/rendered

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
GANTRY_MANIFEST_TEMPLATES_DIR := deploy/gantry
GANTRY_MANIFEST_RENDERED_DIR  := deploy/gantry/rendered

# gantry-snapshotter (RACER-backed containerd snapshotter)
GANTRY_SNAPSHOTTER_BIN=bin/gantry-snapshotter
GANTRY_SNAPSHOTTER_CMD=./cmd/gantry-snapshotter
GANTRY_SNAPSHOTTER_IMAGE ?= $(CONTAINER_REGISTRY)/gantry-snapshotter:$(VERSION_TAG)
GANTRY_SNAPSHOTTER_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
GANTRY_SNAPSHOTTER_MANIFEST_TEMPLATES_DIR := deploy/gantry-snapshotter
GANTRY_SNAPSHOTTER_MANIFEST_RENDERED_DIR  := deploy/gantry-snapshotter/rendered

# racer-ctrl (per-node control plane for the racer distributed block device)
RACER_CTRL_BIN=bin/racer-ctrl
RACER_CTRL_CMD=./cmd/racer-ctrl
RACER_CTRL_IMAGE ?= $(CONTAINER_REGISTRY)/racer-ctrl:$(VERSION_TAG)
# The racer dataplane image, deployed as a sidecar alongside racer-ctrl. It is
# built from the Rust crate in cmd/racer and versioned with the rest of the
# repo, so it tracks VERSION_TAG like any other first-party image.
RACER_IMAGE ?= $(CONTAINER_REGISTRY)/racer:$(VERSION_TAG)
# Stock kubelet plugin registrar. Pinned by digest-free tag deliberately: the
# manifest guard test only forbids :latest, and a floating patch tag here would
# make the rendered manifests non-reproducible across a release.
RACER_REGISTRAR_IMAGE ?= registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.13.0
RACER_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
RACER_MANIFEST_TEMPLATES_DIR := deploy/racer
RACER_MANIFEST_RENDERED_DIR  := deploy/racer/rendered

# unbounded-storage-supervisor (Go binary; distinct from the Rust crate below)
UNBOUNDED_STORAGE_SUPERVISOR_BIN=bin/unbounded-storage-supervisor
UNBOUNDED_STORAGE_SUPERVISOR_CMD=./cmd/unbounded-storage-supervisor
# Default to the version-matched tag so operator-managed storage components stay
# aligned with the release; override for local/e2e (e.g. TAG=dev).
UNBOUNDED_STORAGE_SUPERVISOR_TAG ?= $(VERSION_TAG)
UNBOUNDED_STORAGE_SUPERVISOR_IMAGE=$(CONTAINER_REGISTRY)/unbounded-storage-supervisor:$(UNBOUNDED_STORAGE_SUPERVISOR_TAG)
UNBOUNDED_STORAGE_SUPERVISOR_NAMESPACE ?= $(UNBOUNDED_NAMESPACE)
UNBOUNDED_STORAGE_SUPERVISOR_MANIFEST_TEMPLATES_DIR := deploy/unbounded-storage-supervisor
UNBOUNDED_STORAGE_SUPERVISOR_MANIFEST_RENDERED_DIR  := deploy/unbounded-storage-supervisor/rendered

# Rust binaries
UNBOUNDED_STORAGE_BIN=bin/unbounded-storage
UNBOUNDED_STORAGE_CRATE=./cmd/unbounded-storage
CARGO ?= cargo

# Optional cargo features for unbounded-storage release builds. Set
# UNBOUNDED_STORAGE_PROFILING=1 to compile in the SIGUSR1 CPU profiler
# (see cmd/unbounded-storage/src/profiling.rs); this threads through
# unbounded-storage-build and therefore the tarball/push dev workflow.
ifeq ($(UNBOUNDED_STORAGE_PROFILING),1)
UNBOUNDED_STORAGE_CARGO_FEATURES := --features profiling
else
UNBOUNDED_STORAGE_CARGO_FEATURES :=
endif

# libfabric is built from source because distro packages predate the
# merge of the experimental `net` provider into `tcp` (libfabric 2.0),
# so they lack a native FI_EP_RDM `tcp` provider. We pin a recent
# release and install it under tmp/ (gitignored). Override LIBFABRIC_*
# to use a system install.
LIBFABRIC_VERSION ?= 2.5.1
LIBFABRIC_PREFIX ?= $(CURDIR)/tmp/libfabric/$(LIBFABRIC_VERSION)
LIBFABRIC_PKG_CONFIG_PATH := $(LIBFABRIC_PREFIX)/lib/pkgconfig
LIBFABRIC_STAMP := $(LIBFABRIC_PREFIX)/.installed
LIBFABRIC_URL ?= https://github.com/ofiwg/libfabric/releases/download/v$(LIBFABRIC_VERSION)/libfabric-$(LIBFABRIC_VERSION).tar.bz2

# OpenSSL is built from source because the backend's kernel-TLS receive
# path requires kTLS offload for the RX direction on TLS 1.3, which
# OpenSSL only wires up in 3.5+. Distro packages ship 3.0.x (which skips
# BIO_set_ktls for the read side on 1.3), so we pin a recent release and
# install it under tmp/ (gitignored). Override OPENSSL_* to use a system
# install.
OPENSSL_VERSION ?= 3.5.1
OPENSSL_PREFIX ?= $(CURDIR)/tmp/openssl/$(OPENSSL_VERSION)
OPENSSL_PKG_CONFIG_PATH := $(OPENSSL_PREFIX)/lib/pkgconfig
OPENSSL_STAMP := $(OPENSSL_PREFIX)/.installed
OPENSSL_URL ?= https://github.com/openssl/openssl/releases/download/openssl-$(OPENSSL_VERSION)/openssl-$(OPENSSL_VERSION).tar.gz

# Environment prefix that points cargo's build.rs (pkg-config) and the
# resulting binaries at the pinned libfabric and OpenSSL.
CARGO_FABRIC_ENV = LIBFABRIC_PKG_CONFIG_PATH=$(LIBFABRIC_PKG_CONFIG_PATH) \
	OPENSSL_PKG_CONFIG_PATH=$(OPENSSL_PKG_CONFIG_PATH) \
	LD_LIBRARY_PATH=$(LIBFABRIC_PREFIX)/lib:$(OPENSSL_PREFIX)/lib$${LD_LIBRARY_PATH:+:$$LD_LIBRARY_PATH}

# Release tarball packaging for unbounded-storage. ARCH defaults to the
# host (normalized to Go-style names) and can be overridden for CI matrix
# builds. The tarball bundles the binary plus the pinned libfabric shared
# objects under a single top-level directory.
STORAGE_TARBALL_ARCH ?= $(shell uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
STORAGE_DIST_DIR ?= dist
STORAGE_TARBALL_STEM := unbounded-storage-linux-$(STORAGE_TARBALL_ARCH)
STORAGE_TARBALL := $(STORAGE_DIST_DIR)/$(STORAGE_TARBALL_STEM).tar.gz

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

.PHONY: all help fmt lint test build vulncheck check-deps kubectl-unbounded kubectl-unbounded-build install-tools install-protoc generate kubectl-unbounded forge agent-artifacts-builder agent-artifacts-builder-build orcadev unbounded-agent machina machina-build machina-oci machina-oci-push machina-manifests machine-ops-controller machine-ops-controller-build machine-ops-controller-oci machine-ops-controller-oci-push machine-ops-manifests metalman metalman-build metalman-oci metalman-oci-push unbounded-operator unbounded-operator-build unbounded-operator-manifests playpen-manifests e2e-playpen gomod docs-serve unbounded-net-controller unbounded-net-controller-build unbounded-net-node unbounded-net-node-build unbounded-net-routeplan-debug unping unping-build unroute unroute-build notice notice-check gantry gantry-build gantry-manifests inventory-manifests
.PHONY: net-frontend net-frontend-clean net-ebpf-build net-ebpf-generate net-ebpf-verify net-manifests release-bom release-manifests unbounded-operator-release-manifest
.PHONY: image-machina-local image-machine-ops-controller-local image-metalman-local image-unbounded-operator-local image-unbounded-operator-push image-playpen-local image-net-controller-local image-net-node-local image-gantry-local image-gantry-push images-local
.PHONY: image-net-controller-push image-net-node-push images-net-all images-net-all-push
.PHONY: unbounded-storage unbounded-storage-build unbounded-storage-smoke unbounded-storage-tarball unbounded-storage-push bench unbounded-storage-test unbounded-storage-check unbounded-storage-model-check libfabric openssl
.PHONY: unbounded-storage-supervisor unbounded-storage-supervisor-build unbounded-storage-supervisor-manifests image-unbounded-storage-supervisor-local image-unbounded-storage-supervisor-push
.PHONY: racer-ctrl racer-ctrl-build racer-manifests image-racer-ctrl-local image-racer-ctrl-push \
        image-racer-local image-racer-push

##@ General

all: kubectl-unbounded forge machina machine-ops-controller unbounded-operator unbounded-net-controller unbounded-net-node unbounded-net-routeplan-debug unping unroute gantry gantry-snapshotter racer-ctrl ## Build all binaries (default)

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
	@echo "  notice                           Regenerate NOTICE from Go, npm, Cargo, and native dependencies"
	@echo "  notice-check                     Verify NOTICE is in sync with dependencies"
	@echo "  toolchain-shell                  Drop into the toolchain container with the repo mounted at /project (set TOOLCHAIN_FLAVOR=fedora|ubuntu to pick a flavor)"
	@echo "  toolchain-build                  Rebuild the toolchain container image (honors TOOLCHAIN_FLAVOR)"
	@echo ""
	@echo "Build:"
	@echo "  kubectl-unbounded                Build kubectl-unbounded plugin"
	@echo "  forge                            Build forge dev tool"
	@echo "  agent-artifacts-builder          Build offline agent artifacts builder"
	@echo "  agent-artifacts-builder-build    Build offline agent artifacts builder without test"
	@echo "  orcadev                          Build orcadev dev/debug tool"
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
	@echo "  unbounded-operator | unbounded-operator-build  Build the top-level Site operator"
	@echo "  unbounded-net-controller         Build net controller"
	@echo "  unbounded-net-node               Build net node agent"
	@echo "  unbounded-net-routeplan-debug    Build net routeplan debug tool"
	@echo "  unping                           Build unping health-check utility"
	@echo "  unroute                          Build unroute eBPF inspection utility"
	@echo "  unbounded-storage-supervisor | unbounded-storage-supervisor-build  Build the storage supervisor (with/without lint/test)"
	@echo ""
	@echo "Rust Binaries:"
	@echo "  unbounded-storage | unbounded-storage-build  Build unbounded-storage (with/without test)"
	@echo "  UNBOUNDED_STORAGE_PROFILING=1     Set on any build/push to compile in the SIGUSR1 CPU profiler"
	@echo "  unbounded-storage-smoke          Run the end-to-end smoke test (uses sudo)"
	@echo "  unbounded-storage-tarball        Package unbounded-storage + libfabric into a release tarball"
	@echo "  unbounded-storage-push           Push the unbounded-storage release tarball to Azure blob storage"
	@echo "  unbounded-storage-test           Run cargo tests for unbounded-storage"
	@echo "  unbounded-storage-check          Run cargo check for unbounded-storage"
	@echo "  unbounded-storage-model-check    Run TLC on all unbounded-storage TLA+ models"
	@echo "  unbounded-storage-model-check-<model>  Run TLC on one model (e.g. copy-on-write)"
	@echo "  libfabric                        Build/install the pinned libfabric from source"
	@echo "  openssl                          Build/install the pinned OpenSSL from source"
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
	@echo "  image-unbounded-storage-supervisor-local Build a local unbounded-storage-supervisor container image"
	@echo "  image-unbounded-storage-supervisor-push  Build and push the unbounded-storage-supervisor container image"
	@echo "  image-machina-local              Build machina image with \$$(CONTAINER_ENGINE)"
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
	@echo "  unbounded-operator-release-manifest Build a versioned, directly applicable operator manifest under build/"
	@echo "  unbounded-storage-supervisor-manifests  Render storage supervisor manifests into deploy/unbounded-storage-supervisor/rendered"
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
	@echo "  storage-inttest                  Run unbounded-storage -> orca -> Garage integration test (Docker + sudo)"
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
CONTROLLER_GEN_VERSION ?= v0.21.0

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
	$(GOFMT) -w $(GO_PACKAGE_DIRS)
	$(GOLINT) --fix -E wsl_v5 $(GO_PACKAGE_PATTERNS)

# lint runs the same checks locally and in CI and does NOT auto-fix. Run
# `make fmt` to apply fixes. wsl_v5 is enforced via .golangci.yaml.
lint: ## Run golangci-lint (matches CI; run `make fmt` to auto-fix)
	$(GOLINT) $(GO_PACKAGE_PATTERNS)

ifdef CI
# In CI each job is independent; skip chained prerequisites.

test: machina-manifests machine-ops-manifests playpen-manifests net-manifests unbounded-storage-supervisor-manifests unbounded-operator-manifests gantry-manifests racer-manifests ## Run all tests with race detector
	$(GOTEST) -race ./...

else
# Locally, chain test -> lint for convenience.

test: lint machina-manifests machine-ops-manifests playpen-manifests net-manifests unbounded-storage-supervisor-manifests unbounded-operator-manifests gantry-manifests racer-manifests ## Run all tests (implies lint)
	$(GOTEST) ./...

endif

e2e-playpen: ## Run the kind-based playpen e2e suite
	$(GOTEST) -tags=e2e ./e2e/playpen -v -timeout=10m

build: machina-manifests machine-ops-manifests playpen-manifests net-manifests unbounded-storage-supervisor-manifests unbounded-operator-manifests gantry-manifests racer-manifests ## Build all Go packages
	$(GOBUILD) ./...

generate: install-protoc ## Run go generate for API types (deepcopy, CRDs) and protobuf
	PATH="$(PROTOC_DIR)/bin:$$PATH" $(GOCMD) generate $(GO_PACKAGES)

vulncheck: machina-manifests machine-ops-manifests playpen-manifests net-manifests unbounded-storage-supervisor-manifests unbounded-operator-manifests gantry-manifests racer-manifests ## Run govulncheck for known vulnerabilities
	@# GO-2024-3218 (libp2p/go-libp2p-kad-dht): all versions affected, no fix
	@# available. Theoretical DHT content-censorship attack, not exploitable in
	@# gantry's private-cluster deployment model. Tracked upstream at
	@# https://github.com/advisories/GHSA-mqr9-hjr8-2m9w
	@tmpf=$$(mktemp); \
	$(GOCMD) tool govulncheck $(GO_PACKAGE_PATTERNS) > "$$tmpf" 2>&1; rc=$$?; \
	cat "$$tmpf"; \
	if [ $$rc -eq 0 ]; then rm -f "$$tmpf"; exit 0; fi; \
	if grep -q 'affected by 1 vulnerability' "$$tmpf" && grep -q 'GO-2024-3218' "$$tmpf"; then \
	  echo "vulncheck: only known-unfixable GO-2024-3218 found (accepted)"; \
	  rm -f "$$tmpf"; exit 0; \
	fi; \
	rm -f "$$tmpf"; exit $$rc

gomod: ## Tidy go.mod and go.sum
	$(GOMOD) tidy

notice: ## Regenerate NOTICE from Go, npm, Cargo, and pinned native dependencies
	@if [ ! -d "$(NET_FRONTEND_DIR)/node_modules" ]; then \
		echo "ERROR: $(NET_FRONTEND_DIR)/node_modules not found." >&2; \
		echo "Run: (cd $(NET_FRONTEND_DIR) && npm ci)" >&2; \
		exit 1; \
	fi
	$(GOCMD) run ./hack/cmd/notice generate --output NOTICE

notice-check: ## Verify NOTICE is in sync with Go, npm, Cargo, and pinned native dependencies
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

kubectl-unbounded-build: machina-manifests net-manifests unbounded-storage-supervisor-manifests unbounded-operator-manifests racer-manifests ## Build the kubectl-unbounded binary (no lint/test)
	$(GOBUILD) -ldflags '$(KUBECTL_UNBOUNDED_LDFLAGS)' -o $(KUBECTL_UNBOUNDED_BIN) $(KUBECTL_UNBOUNDED_CMD)/main.go

kubectl-unbounded: test kubectl-unbounded-build ## Build the kubectl-unbounded plugin (implies test)

forge: test ## Build the forge dev tool (implies test)
	$(GOBUILD) -o $(FORGE_BIN) $(FORGE_CMD)/main.go

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
	$(GOBUILD) -ldflags '$(METALMAN_LDFLAGS)' -o $(METALMAN_BIN) $(METALMAN_CMD)/main.go

metalman: test metalman-build ## Build the metalman controller (implies test)

unbounded-operator-build: machina-manifests net-manifests unbounded-storage-supervisor-manifests unbounded-operator-manifests gantry-manifests racer-manifests ## Build the unbounded-operator binary (no lint/test)
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

gantry-manifests: ## Render gantry deployment manifests into deploy/gantry/rendered
	@mkdir -p $(GANTRY_MANIFEST_RENDERED_DIR)
	@find $(GANTRY_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(GANTRY_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(GANTRY_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(GANTRY_NAMESPACE) \
		--set Image=$(GANTRY_IMAGE)
	@echo "Rendered gantry manifests into $(GANTRY_MANIFEST_RENDERED_DIR) (namespace: $(GANTRY_NAMESPACE))"

##@ gantry-snapshotter (RACER-backed containerd snapshotter)

.PHONY: gantry-snapshotter gantry-snapshotter-build gantry-snapshotter-manifests

gantry-snapshotter-build: ## Build the gantry-snapshotter binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(GANTRY_SNAPSHOTTER_BIN) $(GANTRY_SNAPSHOTTER_CMD)

gantry-snapshotter: test gantry-snapshotter-build ## Build gantry-snapshotter (implies test)

gantry-snapshotter-manifests: ## Render gantry-snapshotter manifests into deploy/gantry-snapshotter/rendered
	@mkdir -p $(GANTRY_SNAPSHOTTER_MANIFEST_RENDERED_DIR)
	@find $(GANTRY_SNAPSHOTTER_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(GANTRY_SNAPSHOTTER_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(GANTRY_SNAPSHOTTER_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(GANTRY_SNAPSHOTTER_NAMESPACE) \
		--set Image=$(GANTRY_SNAPSHOTTER_IMAGE)
	@echo "Rendered gantry-snapshotter manifests into $(GANTRY_SNAPSHOTTER_MANIFEST_RENDERED_DIR) (namespace: $(GANTRY_SNAPSHOTTER_NAMESPACE))"

##@ racer (distributed block device)

racer-ctrl-build: ## Build the racer-ctrl binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(RACER_CTRL_BIN) $(RACER_CTRL_CMD)

racer-ctrl: test racer-ctrl-build ## Build racer-ctrl (implies test)

racer-manifests: ## Render racer node manifests into deploy/racer/rendered
	@mkdir -p $(RACER_MANIFEST_RENDERED_DIR)
	@find $(RACER_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(RACER_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(RACER_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(RACER_NAMESPACE) \
		--set Image=$(RACER_CTRL_IMAGE) \
		--set RacerImage=$(RACER_IMAGE) \
		--set RegistrarImage=$(RACER_REGISTRAR_IMAGE)
	@echo "Rendered racer manifests into $(RACER_MANIFEST_RENDERED_DIR) (namespace: $(RACER_NAMESPACE))"

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
		--set SSLMode=$(INVENTORY_SSL_MODE) \
		--set Password=$(INVENTORY_PG_PASSWORD_B64)
	@echo "Rendered inventory manifests into $(INVENTORY_MANIFEST_RENDERED_DIR) (namespace: $(INVENTORY_NAMESPACE))"

unbounded-storage-supervisor-build: ## Build the unbounded-storage-supervisor binary (no lint/test)
	$(GOBUILD) -ldflags '$(STAMP_LDFLAGS)' -o $(UNBOUNDED_STORAGE_SUPERVISOR_BIN) $(UNBOUNDED_STORAGE_SUPERVISOR_CMD)

unbounded-storage-supervisor: test unbounded-storage-supervisor-build ## Build the unbounded-storage-supervisor (implies test)

unbounded-storage-supervisor-manifests: ## Render unbounded-storage-supervisor manifests into deploy/unbounded-storage-supervisor/rendered
	@mkdir -p $(UNBOUNDED_STORAGE_SUPERVISOR_MANIFEST_RENDERED_DIR)
	@find $(UNBOUNDED_STORAGE_SUPERVISOR_MANIFEST_RENDERED_DIR) -mindepth 1 -not -name .gitignore -delete
	$(GOCMD) run ./hack/cmd/render-manifests \
		--templates-dir $(UNBOUNDED_STORAGE_SUPERVISOR_MANIFEST_TEMPLATES_DIR) \
		--output-dir $(UNBOUNDED_STORAGE_SUPERVISOR_MANIFEST_RENDERED_DIR) \
		--set Namespace=$(UNBOUNDED_STORAGE_SUPERVISOR_NAMESPACE) \
		--set Image=$(UNBOUNDED_STORAGE_SUPERVISOR_IMAGE)
	@echo "Rendered unbounded-storage-supervisor manifests into $(UNBOUNDED_STORAGE_SUPERVISOR_MANIFEST_RENDERED_DIR) (image: $(UNBOUNDED_STORAGE_SUPERVISOR_IMAGE))"

##@ Rust Binaries

# Build and install the pinned libfabric from source (once). The stamp
# file marks a completed install so repeat builds are no-ops; remove
# tmp/libfabric to force a rebuild.
$(LIBFABRIC_STAMP):
	@echo "Building libfabric $(LIBFABRIC_VERSION) -> $(LIBFABRIC_PREFIX)"
	@rm -rf $(CURDIR)/tmp/libfabric/src
	@mkdir -p $(CURDIR)/tmp/libfabric/src
	@curl -fsSL $(LIBFABRIC_URL) | tar -xj -C $(CURDIR)/tmp/libfabric/src --strip-components=1
	cd $(CURDIR)/tmp/libfabric/src && ./configure --prefix=$(LIBFABRIC_PREFIX) \
		--enable-tcp=yes --with-uring=yes --enable-verbs=yes --enable-rxm=yes \
		--disable-sockets --disable-psm3 --disable-efa --disable-shm
	$(MAKE) -C $(CURDIR)/tmp/libfabric/src -j$$(nproc)
	$(MAKE) -C $(CURDIR)/tmp/libfabric/src install
	@rm -rf $(CURDIR)/tmp/libfabric/src
	@touch $(LIBFABRIC_STAMP)

libfabric: $(LIBFABRIC_STAMP) ## Build/install the pinned libfabric ($(LIBFABRIC_VERSION)) from source

# Build and install the pinned OpenSSL from source (once). Mirrors the
# libfabric stamp pattern; remove tmp/openssl to force a rebuild. We need
# >=3.5 for TLS 1.3 kTLS receive offload. `enable-ktls` turns on kernel
# TLS support; install_sw installs libs+headers and install_ssldirs installs
# the default openssl.cnf (needed by the `openssl` CLI and libcrypto default
# config load); both skip the man pages.
$(OPENSSL_STAMP):
	@echo "Building openssl $(OPENSSL_VERSION) -> $(OPENSSL_PREFIX)"
	@rm -rf $(CURDIR)/tmp/openssl/src
	@mkdir -p $(CURDIR)/tmp/openssl/src
	@curl -fsSL $(OPENSSL_URL) | tar -xz -C $(CURDIR)/tmp/openssl/src --strip-components=1
	cd $(CURDIR)/tmp/openssl/src && ./Configure --prefix=$(OPENSSL_PREFIX) --libdir=lib \
		enable-ktls shared no-tests no-docs
	$(MAKE) -C $(CURDIR)/tmp/openssl/src -j$$(nproc)
	$(MAKE) -C $(CURDIR)/tmp/openssl/src install_sw install_ssldirs
	@rm -rf $(CURDIR)/tmp/openssl/src
	@touch $(OPENSSL_STAMP)

openssl: $(OPENSSL_STAMP) ## Build/install the pinned OpenSSL ($(OPENSSL_VERSION)) from source

unbounded-storage-check: $(LIBFABRIC_STAMP) $(OPENSSL_STAMP) ## Run cargo check for unbounded-storage
	$(CARGO_FABRIC_ENV) $(CARGO) check --manifest-path $(UNBOUNDED_STORAGE_CRATE)/Cargo.toml --locked --all-targets

unbounded-storage-test: $(LIBFABRIC_STAMP) $(OPENSSL_STAMP) ## Run cargo tests for unbounded-storage (includes the profiling feature so it always compiles)
	$(CARGO_FABRIC_ENV) $(CARGO) test --manifest-path $(UNBOUNDED_STORAGE_CRATE)/Cargo.toml --locked --all-targets --features profiling

unbounded-storage-build: $(LIBFABRIC_STAMP) $(OPENSSL_STAMP) ## Build the unbounded-storage binary (no test; UNBOUNDED_STORAGE_PROFILING=1 adds the CPU profiler)
	$(CARGO_FABRIC_ENV) $(CARGO) build --manifest-path $(UNBOUNDED_STORAGE_CRATE)/Cargo.toml --release --locked $(UNBOUNDED_STORAGE_CARGO_FEATURES)
	@mkdir -p $(dir $(UNBOUNDED_STORAGE_BIN))
	cp $(UNBOUNDED_STORAGE_CRATE)/target/release/unbounded-storage $(UNBOUNDED_STORAGE_BIN)

unbounded-storage: unbounded-storage-test unbounded-storage-build ## Build the unbounded-storage binary (implies test)

unbounded-storage-smoke: unbounded-storage-build ## Run the end-to-end smoke test (requires sudo for hugepages/memlock)
	sudo -E env "PATH=$$PATH" \
		"LD_LIBRARY_PATH=$(LIBFABRIC_PREFIX)/lib:$(OPENSSL_PREFIX)/lib$${LD_LIBRARY_PATH:+:$$LD_LIBRARY_PATH}" \
		python3 hack/smoke-storage.py

unbounded-storage-tarball: unbounded-storage-build ## Package unbounded-storage + libfabric/OpenSSL into a release tarball ($(STORAGE_TARBALL))
	@echo "Assembling $(STORAGE_TARBALL)"
	@rm -rf $(STORAGE_DIST_DIR)/$(STORAGE_TARBALL_STEM)
	@mkdir -p $(STORAGE_DIST_DIR)/$(STORAGE_TARBALL_STEM)/bin $(STORAGE_DIST_DIR)/$(STORAGE_TARBALL_STEM)/lib
	install -m 0755 $(UNBOUNDED_STORAGE_BIN) $(STORAGE_DIST_DIR)/$(STORAGE_TARBALL_STEM)/bin/unbounded-storage
	@libdir=$(STORAGE_DIST_DIR)/$(STORAGE_TARBALL_STEM)/lib; \
	libfound=0; \
	for d in $(LIBFABRIC_PREFIX)/lib $(LIBFABRIC_PREFIX)/lib64; do \
		if [ -d "$$d" ] && cp -a "$$d"/libfabric.so* "$$libdir"/ 2>/dev/null; then \
			libfound=1; \
		fi; \
	done; \
	if [ "$$libfound" -ne 1 ]; then \
		echo "error: no libfabric.so* found under $(LIBFABRIC_PREFIX)" >&2; \
		exit 1; \
	fi; \
	libfabric_real="$$(readlink -f "$$libdir"/libfabric.so)"; \
	if [ -z "$$libfabric_real" ] || [ ! -f "$$libfabric_real" ]; then \
		echo "error: could not resolve bundled libfabric.so" >&2; \
		exit 1; \
	fi; \
	echo "Bundling libfabric runtime dependency closure ..."; \
	ldd "$$libfabric_real" | while read -r soname arrow path rest; do \
		[ "$$arrow" = "=>" ] || continue; \
		[ -f "$$path" ] || continue; \
		case "$$soname" in \
		ld-linux*.so.* | linux-vdso.so.* | libc.so.* | libm.so.* | \
		libdl.so.* | libpthread.so.* | librt.so.* | libresolv.so.* | \
		libnsl.so.* | libutil.so.* | libanl.so.* | libgcc_s.so.*) \
			continue;; \
		esac; \
		cp -L "$$path" "$$libdir/$$soname"; \
		chmod 0644 "$$libdir/$$soname"; \
		echo "  bundled $$soname"; \
	done; \
	if [ ! -e "$$libdir"/liburing.so.2 ]; then \
		echo "error: liburing.so.2 was not bundled; libfabric needs it at runtime." >&2; \
		echo "       install liburing development files on the build host and retry." >&2; \
		exit 1; \
	fi
	@libdir=$(STORAGE_DIST_DIR)/$(STORAGE_TARBALL_STEM)/lib; \
	sslfound=0; \
	for d in $(OPENSSL_PREFIX)/lib $(OPENSSL_PREFIX)/lib64; do \
		if [ -d "$$d" ] && cp -a "$$d"/libssl.so* "$$d"/libcrypto.so* "$$libdir"/ 2>/dev/null; then \
			sslfound=1; \
		fi; \
	done; \
	if [ "$$sslfound" -ne 1 ]; then \
		echo "error: no libssl.so*/libcrypto.so* found under $(OPENSSL_PREFIX); kTLS origins need the pinned OpenSSL." >&2; \
		exit 1; \
	fi; \
	echo "  bundled libssl/libcrypto from $(OPENSSL_PREFIX)"
	tar -czf $(STORAGE_TARBALL) -C $(STORAGE_DIST_DIR) $(STORAGE_TARBALL_STEM)
	cd $(STORAGE_DIST_DIR) && sha256sum $(STORAGE_TARBALL_STEM).tar.gz > $(STORAGE_TARBALL_STEM).tar.gz.sha256
	@rm -rf $(STORAGE_DIST_DIR)/$(STORAGE_TARBALL_STEM)
	@echo "Wrote $(STORAGE_TARBALL)"

# Azure blob storage destination for publishing the unbounded-storage release
# tarball. AZURE_STORAGE_KEY must be provided in the environment when pushing.
STORAGE_BLOB_ACCOUNT   ?=
STORAGE_BLOB_CONTAINER ?=

unbounded-storage-push: unbounded-storage-tarball ## Push the unbounded-storage release tarball to Azure blob storage
	@test -n "$(STORAGE_BLOB_ACCOUNT)" || { echo "error: STORAGE_BLOB_ACCOUNT is required"; exit 1; }
	@test -n "$(AZURE_STORAGE_KEY)" || { echo "error: AZURE_STORAGE_KEY is required for pushing artifacts"; exit 1; }
	@az storage blob upload \
		--file $(STORAGE_TARBALL) \
		--container-name $(STORAGE_BLOB_CONTAINER) \
		--name $(VERSION)/$(STORAGE_TARBALL_STEM).tar.gz \
		--account-name $(STORAGE_BLOB_ACCOUNT) \
		--account-key $(AZURE_STORAGE_KEY) \
		--overwrite
	@az storage blob upload \
		--file $(STORAGE_TARBALL).sha256 \
		--container-name $(STORAGE_BLOB_CONTAINER) \
		--name $(VERSION)/$(STORAGE_TARBALL_STEM).tar.gz.sha256 \
		--account-name $(STORAGE_BLOB_ACCOUNT) \
		--account-key $(AZURE_STORAGE_KEY) \
		--overwrite
	@az storage blob upload \
		--file hack/scripts/install-unbounded-storage.sh \
		--container-name $(STORAGE_BLOB_CONTAINER) \
		--name $(VERSION)/install.sh \
		--account-name $(STORAGE_BLOB_ACCOUNT) \
		--account-key $(AZURE_STORAGE_KEY) \
		--overwrite
	@az storage blob upload \
		--file hack/scripts/gen-storage-mesh-config.sh \
		--container-name $(STORAGE_BLOB_CONTAINER) \
		--name $(VERSION)/gen-config.sh \
		--account-name $(STORAGE_BLOB_ACCOUNT) \
		--account-key $(AZURE_STORAGE_KEY) \
		--overwrite
	@echo "Uploaded $(STORAGE_TARBALL_STEM).tar.gz to https://$(STORAGE_BLOB_ACCOUNT).blob.core.windows.net/$(STORAGE_BLOB_CONTAINER)/$(VERSION)/$(STORAGE_TARBALL_STEM).tar.gz"
	@echo "Install with:"
	@echo "  curl https://$(STORAGE_BLOB_ACCOUNT).blob.core.windows.net/$(STORAGE_BLOB_CONTAINER)/$(VERSION)/install.sh | bash -s -- https://$(STORAGE_BLOB_ACCOUNT).blob.core.windows.net/$(STORAGE_BLOB_CONTAINER)/$(VERSION)/$(STORAGE_TARBALL_STEM).tar.gz"
	@echo "Generate a mesh config with:"
	@echo "  curl https://$(STORAGE_BLOB_ACCOUNT).blob.core.windows.net/$(STORAGE_BLOB_CONTAINER)/$(VERSION)/gen-config.sh | bash"

bench: $(LIBFABRIC_STAMP) $(OPENSSL_STAMP) ## Build the bench tool (excluded from images)
	$(CARGO_FABRIC_ENV) $(CARGO) build --manifest-path $(UNBOUNDED_STORAGE_CRATE)/Cargo.toml --release --locked --bin bench
	@mkdir -p $(dir $(UNBOUNDED_STORAGE_BIN))
	cp $(UNBOUNDED_STORAGE_CRATE)/target/release/bench bin/bench

# TLA+ tooling for the unbounded-storage models.
# tla2tools.jar is fetched on demand into tmp/ (gitignored).  Override
# TLA_TOOLS_JAR to use a locally installed copy.
#
# The URL is pinned to a tagged release (not `latest/download`) and the
# downloaded artifact is verified against TLA_TOOLS_SHA256 so model-check
# runs are reproducible across machines and over time.
TLA_TOOLS_JAR ?= tmp/tla2tools.jar
TLA_TOOLS_VERSION ?= v1.8.0
TLA_TOOLS_URL ?= https://github.com/tlaplus/tlaplus/releases/download/$(TLA_TOOLS_VERSION)/tla2tools.jar
TLA_TOOLS_SHA256 ?= cc4803dce2a8ffaf0f5920a9dc39df4b5ee34ab4cb53fb58ac557277a7e516b3

# Root directory holding the TLA+ models.  Each subdirectory contains exactly
# one <Name>.tla plus a matching <Name>.cfg and is model-checked by a per-model
# target.  STORAGE_MODEL_DIRS lists the model basenames the aggregate target
# iterates over.
STORAGE_MODELS_ROOT := cmd/unbounded-storage/models
STORAGE_MODEL_DIRS := bufferpool-singleflight chord-routing copy-on-write engine-reclamation fabric-completion

$(TLA_TOOLS_JAR):
	@mkdir -p $(dir $(TLA_TOOLS_JAR))
	@echo "Downloading tla2tools.jar ($(TLA_TOOLS_VERSION)) -> $(TLA_TOOLS_JAR)"
	@curl -fsSL -o $(TLA_TOOLS_JAR) $(TLA_TOOLS_URL)
	@echo "$(TLA_TOOLS_SHA256)  $(TLA_TOOLS_JAR)" | sha256sum -c -

# Per-model pattern target: `make unbounded-storage-model-check-<dir>` runs TLC
# on the single .tla/.cfg pair found in cmd/unbounded-storage/models/<dir>.
unbounded-storage-model-check-%: $(TLA_TOOLS_JAR)
	@command -v java >/dev/null 2>&1 || { echo "java is required to run TLC" >&2; exit 1; }
	@dir="$(STORAGE_MODELS_ROOT)/$*"; \
	test -d "$$dir" || { echo "no such model directory: $$dir" >&2; exit 1; }; \
	echo "==> Model-checking $*"; \
	cd "$$dir" || exit 1; \
	count=0; tla=; \
	for f in *.tla; do \
		test -e "$$f" || continue; \
		count=$$((count + 1)); tla=$$f; \
	done; \
	if [ "$$count" -ne 1 ]; then \
		if [ "$$count" -eq 0 ]; then \
			echo "model directory $$dir contains no .tla file" >&2; \
		else \
			echo "model directory $$dir must contain exactly one .tla file, found $$count: "*.tla >&2; \
		fi; \
		exit 1; \
	fi; \
	base=$${tla%.tla}; \
	java -XX:+UseParallelGC -cp $(CURDIR)/$(TLA_TOOLS_JAR) tlc2.TLC -workers auto -config $$base.cfg $$base.tla

# Aggregate target: run TLC on every unbounded-storage TLA+ model.  Fails if any
# individual model fails.
unbounded-storage-model-check: $(addprefix unbounded-storage-model-check-,$(STORAGE_MODEL_DIRS)) ## Run TLC on all unbounded-storage TLA+ models
	@echo "All unbounded-storage TLA+ models checked successfully."

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

.PHONY: image-unbounded-storage-supervisor-local
image-unbounded-storage-supervisor-local: ## Build the unbounded-storage-supervisor container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t unbounded-storage-supervisor:$(UNBOUNDED_STORAGE_SUPERVISOR_TAG) -t $(UNBOUNDED_STORAGE_SUPERVISOR_IMAGE) \
		-f ./images/unbounded-storage-supervisor/Containerfile .
	$(call trivy-maybe,$(UNBOUNDED_STORAGE_SUPERVISOR_IMAGE))

.PHONY: image-unbounded-storage-supervisor-push
image-unbounded-storage-supervisor-push: image-unbounded-storage-supervisor-local ## Build and push the unbounded-storage-supervisor container image
	$(CONTAINER_ENGINE) push $(UNBOUNDED_STORAGE_SUPERVISOR_IMAGE)

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

image-racer-ctrl-local: ## Build the racer-ctrl container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t racer-ctrl:$(VERSION_TAG) -t $(RACER_CTRL_IMAGE) \
		-f ./images/racer-ctrl/Containerfile .
	$(call trivy-maybe,$(RACER_CTRL_IMAGE))

image-racer-ctrl-push: image-racer-ctrl-local ## Build and push the racer-ctrl container image
	$(CONTAINER_ENGINE) push $(RACER_CTRL_IMAGE)

image-racer-local: ## Build the racer dataplane container image locally (single-arch)
	$(CONTAINER_ENGINE) build \
		-t racer:$(VERSION_TAG) -t $(RACER_IMAGE) \
		-f ./images/racer/Containerfile .
	$(call trivy-maybe,$(RACER_IMAGE))

image-racer-push: image-racer-local ## Build and push the racer dataplane container image
	$(CONTAINER_ENGINE) push $(RACER_IMAGE)

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

# storage-inttest runs the unbounded-storage -> orca -> Garage
# integration test. It builds the libfabric-linked unbounded-storage
# binary and an integrationtest+storageboundary test binary (compiled as
# the current user so the Go caches stay user-owned), then runs that
# binary under sudo: it needs CAP_SYS_RESOURCE to raise RLIMIT_MEMLOCK
# for the storage children's io_uring pinned buffers. LD_LIBRARY_PATH is
# re-injected past sudo's env scrubbing so the spawned binaries find the
# pinned libfabric. The -test.run filter keeps it scoped to the storage
# boundary test alone (the rest of the orca integration suite is compiled
# into the binary but never executed). Requires Docker (Garage + Azurite
# via testcontainers).
.PHONY: storage-inttest
STORAGE_INTTEST_BIN := $(CURDIR)/tmp/storage-inttest.test

storage-inttest: libfabric openssl unbounded-storage-build ## Run the unbounded-storage -> orca -> Garage integration test (Docker + sudo)
	@mkdir -p $(CURDIR)/tmp
	$(GOTEST) -tags=integrationtest,storageboundary -c -o $(STORAGE_INTTEST_BIN) ./internal/orca/inttest/
	sudo -E env "PATH=$$PATH" "LD_LIBRARY_PATH=$(LIBFABRIC_PREFIX)/lib:$(OPENSSL_PREFIX)/lib" \
		$(STORAGE_INTTEST_BIN) -test.v -test.timeout 30m -test.run '^TestStorageBoundaryThroughOrca$$'


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

images-local: image-machina-local image-machine-ops-controller-local image-metalman-local image-unbounded-storage-supervisor-local image-unbounded-operator-local image-net-controller-local image-net-node-local image-gantry-local image-racer-ctrl-local image-racer-local ## Build all container images locally

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

release-manifests: NET_APISERVER_URL :=
release-manifests: UNBOUNDED_OPERATOR_API_SERVER_ENDPOINT :=
release-manifests: machina-manifests machine-ops-manifests net-manifests gantry-manifests racer-manifests unbounded-storage-supervisor-manifests unbounded-operator-manifests ## Build stamped combined manifest tarball under build/
	@rm -rf $(RELEASE_MANIFESTS_STAGE_DIR)
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machina
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machine-ops
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/net
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/gantry
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/racer
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/unbounded-storage-supervisor
	@mkdir -p $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/unbounded-operator
	@cp -R $(MACHINA_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machina/
	@cp -R $(MACHINE_OPS_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/machine-ops/
	@cp -R $(NET_MANIFEST_RENDERED_DIR)/.     $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/net/
	@cp -R $(GANTRY_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/gantry/
	@cp -R $(RACER_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/racer/
	@cp -R $(UNBOUNDED_STORAGE_SUPERVISOR_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/unbounded-storage-supervisor/
	@cp -R $(UNBOUNDED_OPERATOR_MANIFEST_RENDERED_DIR)/. $(RELEASE_MANIFESTS_STAGE_DIR)/$(RELEASE_MANIFESTS_NAME)/unbounded-operator/
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
