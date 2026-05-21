# hack/orca/Makefile - dev-harness targets for the Orca origin cache.
#
# Invoke from the repo root: `make -C hack/orca <target>`. The root
# Makefile also defines `orca-up`, `orca-down`, `orca-reset` which
# proxy here.
#
# These targets stand up a local Kind cluster, build the Orca container
# image with podman, side-load it into Kind, deploy LocalStack as the
# cachestore backend, and apply the rendered Orca manifests. The
# harness validates the Kubernetes deployment shape (manifests, image,
# headless-Service DNS, RBAC, init-Job ordering); for Go-level
# behavior coverage use `make orca-inttest` which runs the in-process
# integration suite under internal/orca/inttest/.

REPO_ROOT  := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)
HACK_DIR   := $(dir $(lastword $(MAKEFILE_LIST)))

# Cluster + namespace knobs.
CLUSTER_NAME      ?= orca-dev
NAMESPACE         ?= unbounded-kube
KIND_CONFIG       ?= $(HACK_DIR)kind-config.yaml

# Image tag pinned to :dev so kind load and rollout-restart use a
# stable identifier (the auto-derived VERSION can include slashes from
# git tags like images/agent-ubuntu2404-nvidia/v..., which are illegal
# in OCI tags).
ORCA_VERSION      ?= dev
ORCA_IMAGE        ?= ghcr.io/azure/orca:$(ORCA_VERSION)

# Container engine (podman in CI, podman or docker locally). kind load
# image-archive accepts an OCI tarball produced by either.
CONTAINER_ENGINE  ?= podman

# Path to user .env (sourced by helper scripts that need it).
ENV_FILE          ?= $(HACK_DIR).env

# Rendered manifest dirs (per-Makefile target overrides for the dev
# rendering of pluggable orca manifests + the dev-only LocalStack/init
# manifests).
ORCA_RENDERED     := $(REPO_ROOT)/deploy/orca/rendered
DEV_TEMPLATES     := $(REPO_ROOT)/deploy/orca/dev
DEV_RENDERED      := $(HACK_DIR)rendered-dev

.PHONY: help up down reset render render-dev image kind-create kind-load \
        deploy deploy-localstack deploy-azurite deploy-azurite-maybe \
        deploy-credentials deploy-orca \
        wait-ready logs port-forward dev-azure status \
        data-generate data-upload data-random data-list data-delete \
        roundtrip cache-list cache-inspect cache-clear bench scenario

help: ## Show this help
	@echo ""
	@echo "Usage: make -C hack/orca <target> [VAR=value ...]"
	@echo ""
	@echo "Lifecycle:"
	@echo "  up                  Bring up Kind cluster + LocalStack + Orca"
	@echo "  down                Delete Kind cluster"
	@echo "  reset               Rebuild image + rollout-restart deployment"
	@echo ""
	@echo "Pieces (typically called by 'up'):"
	@echo "  render              Render orca manifests"
	@echo "  render-dev          Render dev-only manifests (LocalStack, Azurite)"
	@echo "  image               Build Orca container image (image-orca-local)"
	@echo "  kind-create         Create the Kind cluster (idempotent)"
	@echo "  kind-load           Load the Orca image into Kind nodes"
	@echo "  deploy-localstack   Apply LocalStack Deployment (init-hook ConfigMap creates buckets)"
	@echo "  deploy-credentials  Create the orca-credentials Secret from .env"
	@echo "  deploy-orca         Apply rendered Orca manifests"
	@echo "  wait-ready          Block until 3/3 orca pods are Ready"
	@echo ""
	@echo "Operate:"
	@echo "  status              kubectl get pods -n $(NAMESPACE)"
	@echo "  logs                Tail logs from all Orca pods"
	@echo "  port-forward        Forward localhost:8443 -> svc/orca"
	@echo "  dev-azure           Upload a file to real Azure (FILE=path; requires .env creds)"
	@echo ""
	@echo "Seed origin (NodePort 30200 for awss3 / 30100 for azureblob; needs cluster up):"
	@echo "  data-generate ARGS='--size 10MiB --count 5'   Synthesise N blobs"
	@echo "  data-random   NAME=foo [SIZE=10MiB]            Synthesise one random blob 'foo1' of SIZE"
	@echo "  data-upload   FILE=/path/to/file               Upload a single file"
	@echo "  data-list     [ARGS='--prefix foo']            List origin objects"
	@echo "  data-delete   [PREFIX=foo] [ARGS='--yes']      Delete origin objects"
	@echo ""
	@echo "Roundtrip / cache / bench (orcadev wrappers; edge port-forward is auto-managed):"
	@echo "  roundtrip FILE=/path     Upload, GET via orca, verify SHA-256"
	@echo "  cache-list               Enumerate cache chunks"
	@echo "  cache-inspect BUCKET=b KEY=k   Show chunk presence for an object"
	@echo "  cache-clear   [ARGS='--all --yes']             Clear cache chunks"
	@echo "  bench KEY=k              Parallel GET throughput benchmark"
	@echo "  scenario NAME=cold-warm  Run a canned end-to-end scenario"
	@echo ""
	@echo "Note: For Go-level behavior testing (chunked GETs, cluster routing,"
	@echo "singleflight, peer fallback) use 'make orca-inttest' from the repo"
	@echo "root. That suite exercises the same code paths against testcontainers"
	@echo "without needing Kind."
	@echo ""
	@echo "Variables:"
	@echo "  CLUSTER_NAME=$(CLUSTER_NAME)"
	@echo "  NAMESPACE=$(NAMESPACE)"
	@echo "  ORCA_IMAGE=$(ORCA_IMAGE)"
	@echo "  CONTAINER_ENGINE=$(CONTAINER_ENGINE)"
	@echo "  ENV_FILE=$(ENV_FILE)"

# -- Top-level lifecycle ------------------------------------------------------

up: kind-create image kind-load deploy ## End-to-end bring-up

down: ## Delete Kind cluster
	CLUSTER_NAME="$(CLUSTER_NAME)" $(HACK_DIR)down.sh

reset: image kind-load ## Rebuild image and rolling-restart Orca
	kubectl --context kind-$(CLUSTER_NAME) -n $(NAMESPACE) rollout restart deployment/orca
	kubectl --context kind-$(CLUSTER_NAME) -n $(NAMESPACE) rollout status deployment/orca --timeout=120s

# `deploy` deploys whichever origin backend matches ORIGIN_DRIVER in
# .env (default: awss3 -> LocalStack only; azureblob also brings up
# Azurite). The cachestore is always LocalStack regardless. Init Jobs
# are idempotent so re-applying is safe.
deploy: render render-dev deploy-localstack deploy-azurite-maybe deploy-credentials deploy-orca wait-ready ## Apply all manifests + Secret

deploy-azurite-maybe: render-dev
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	driver="$${ORIGIN_DRIVER:-awss3}"; \
	if [ "$$driver" = "azureblob" ]; then \
		echo "ORIGIN_DRIVER=azureblob -> deploying Azurite"; \
		$(MAKE) deploy-azurite; \
	else \
		echo "ORIGIN_DRIVER=$$driver -> Azurite not required (skipping)"; \
	fi

# -- Rendering ----------------------------------------------------------------

# Render the pluggable orca manifests with the dev image. Default
# origin driver in the dev harness is awss3 pointing at the same
# in-cluster LocalStack instance (different bucket); reviewers can
# override by setting ORIGIN_DRIVER=azureblob and the appropriate
# AZURE_* values in .env. Credentials are NOT rendered into the
# ConfigMap; they ride in via the orca-credentials Secret as env vars
# (envFrom).
render:
	@echo "Rendering orca manifests with image=$(ORCA_IMAGE)"
	@mkdir -p "$(ORCA_RENDERED)"
	@find "$(ORCA_RENDERED)" -mindepth 1 -not -name .gitignore -delete 2>/dev/null || true
	@if [ -f "$(ENV_FILE)" ]; then \
		set -a && . "$(ENV_FILE)" && set +a; \
	fi; \
	driver="$${ORIGIN_DRIVER:-awss3}"; \
	if [ "$$driver" = "azureblob" ]; then \
		azure_account="$${AZURE_STORAGE_ACCOUNT:-devstoreaccount1}"; \
		azure_container="$${AZURE_CONTAINER:-$${AZURITE_CONTAINER:-orca-test}}"; \
		azure_endpoint="$${AZUREBLOB_ENDPOINT:-http://azurite.$(NAMESPACE).svc.cluster.local:10000/devstoreaccount1/}"; \
	else \
		azure_account="$${AZURE_STORAGE_ACCOUNT:-}"; \
		azure_container="$${AZURE_CONTAINER:-orca-test}"; \
		azure_endpoint="$${AZUREBLOB_ENDPOINT:-}"; \
	fi; \
	go run "$(REPO_ROOT)/hack/cmd/render-manifests" \
		--templates-dir "$(REPO_ROOT)/deploy/orca" \
		--output-dir "$(ORCA_RENDERED)" \
		--set Namespace="$(NAMESPACE)" \
		--set Image="$(ORCA_IMAGE)" \
		--set ImagePullPolicy=IfNotPresent \
		--set TargetReplicas="$${TARGET_REPLICAS:-3}" \
		--set OriginID="$${ORIGIN_ID:-awss3-localstack}" \
		--set OriginDriver="$$driver" \
		--set AzureAccount="$$azure_account" \
		--set AzureContainer="$$azure_container" \
		--set AzureEndpoint="$$azure_endpoint" \
		--set OriginAWSS3Endpoint="$${ORIGIN_AWSS3_ENDPOINT:-http://localstack.$(NAMESPACE).svc.cluster.local:4566}" \
		--set OriginAWSS3Region="$${ORIGIN_AWSS3_REGION:-us-east-1}" \
		--set OriginAWSS3Bucket="$${ORIGIN_AWSS3_BUCKET:-orca-origin}" \
		--set OriginAWSS3UsePathStyle="true" \
		--set CachestoreBucket="$${CACHESTORE_BUCKET:-orca-cache}" \
		--set CachestoreEndpoint="$${CACHESTORE_ENDPOINT:-http://localstack.$(NAMESPACE).svc.cluster.local:4566}" \
		--set CachestoreRegion="$${CACHESTORE_REGION:-us-east-1}" \
		--set ClusterService="orca-peers.$(NAMESPACE).svc.cluster.local" \
		--set ServerAuthEnabled=false \
		--set InternalTLSEnabled=false \
		--set LogLevel="$${LOG_LEVEL:-info}"

render-dev:
	@echo "Rendering dev manifests (LocalStack, init job, Azurite)"
	@mkdir -p "$(DEV_RENDERED)"
	@find "$(DEV_RENDERED)" -mindepth 1 -delete 2>/dev/null || true
	@if [ -f "$(ENV_FILE)" ]; then \
		set -a && . "$(ENV_FILE)" && set +a; \
	fi; \
	go run "$(REPO_ROOT)/hack/cmd/render-manifests" \
		--templates-dir "$(DEV_TEMPLATES)" \
		--output-dir "$(DEV_RENDERED)" \
		--set Namespace="$(NAMESPACE)" \
		--set CachestoreBucket="$${CACHESTORE_BUCKET:-orca-cache}" \
		--set OriginBucket="$${ORIGIN_AWSS3_BUCKET:-orca-origin}" \
		--set AzuriteContainer="$${AZURE_CONTAINER:-$${AZURITE_CONTAINER:-orca-test}}" \
		--set AzuriteNodePort="$${AZURITE_NODE_PORT:-30100}" \
		--set LocalstackNodePort="$${LOCALSTACK_NODE_PORT:-30200}"

# -- Image + cluster ----------------------------------------------------------

image:
	@echo "Building Orca image $(ORCA_IMAGE) with $(CONTAINER_ENGINE)"
	cd "$(REPO_ROOT)" && $(MAKE) image-orca-local \
		VERSION=$(ORCA_VERSION) \
		CONTAINER_ENGINE=$(CONTAINER_ENGINE) \
		ORCA_IMAGE=$(ORCA_IMAGE)

kind-create:
	CLUSTER_NAME="$(CLUSTER_NAME)" KIND_CONFIG="$(KIND_CONFIG)" $(HACK_DIR)kind-create.sh

kind-load:
	CLUSTER_NAME="$(CLUSTER_NAME)" \
	ORCA_IMAGE="$(ORCA_IMAGE)" \
	CONTAINER_ENGINE="$(CONTAINER_ENGINE)" \
		$(HACK_DIR)kind-load.sh

# -- Deploy steps -------------------------------------------------------------

# deploy-localstack applies the LocalStack Deployment and its
# init-buckets ConfigMap, then waits for LocalStack to be Ready and
# for its init-hook to have created the cachestore bucket. The hook
# fires on every LocalStack container start (including restarts), so
# this target is fully idempotent and is the clean recovery path if
# orca pods are crash-looping with NoSuchBucket.
deploy-localstack: render-dev render
	kubectl --context kind-$(CLUSTER_NAME) apply -f "$(ORCA_RENDERED)/01-namespace.yaml"
	kubectl --context kind-$(CLUSTER_NAME) apply -f "$(DEV_RENDERED)/01-localstack.yaml"
	kubectl --context kind-$(CLUSTER_NAME) -n $(NAMESPACE) rollout status deployment/localstack --timeout=120s
	@echo "Waiting for LocalStack init-hook to create cachestore bucket ..."
	@for i in $$(seq 1 30); do \
		if kubectl --context kind-$(CLUSTER_NAME) -n $(NAMESPACE) exec deploy/localstack -- \
				awslocal s3api head-bucket --bucket "$${CACHESTORE_BUCKET:-orca-cache}" >/dev/null 2>&1; then \
			echo "LocalStack buckets ready."; exit 0; \
		fi; sleep 2; \
	done; \
	echo "ERROR: LocalStack init hook did not create buckets within 60s" >&2; exit 1

# deploy-azurite applies the Azurite Deployment which carries an
# in-pod 'container-ensurer' sidecar that idempotently maintains the
# Azure container. As with deploy-localstack this is fully
# idempotent and the clean recovery path if Azurite has lost its
# in-memory container state.
deploy-azurite: render-dev
	kubectl --context kind-$(CLUSTER_NAME) apply -f "$(ORCA_RENDERED)/01-namespace.yaml"
	kubectl --context kind-$(CLUSTER_NAME) apply -f "$(DEV_RENDERED)/03-azurite.yaml"
	kubectl --context kind-$(CLUSTER_NAME) -n $(NAMESPACE) rollout status deployment/azurite --timeout=180s
	@echo "Waiting for Azurite container-ensurer sidecar to create the container ..."
	@for i in $$(seq 1 30); do \
		if kubectl --context kind-$(CLUSTER_NAME) -n $(NAMESPACE) exec deploy/azurite -c container-ensurer -- \
				az storage container exists --name "$${AZURE_CONTAINER:-orca-test}" --query exists -o tsv 2>/dev/null | grep -qi true; then \
			echo "Azurite container ready."; exit 0; \
		fi; sleep 2; \
	done; \
	echo "ERROR: Azurite container not created within 60s" >&2; exit 1

deploy-credentials:
	CLUSTER_NAME="$(CLUSTER_NAME)" \
	NAMESPACE="$(NAMESPACE)" \
	ENV_FILE="$(ENV_FILE)" \
		$(HACK_DIR)deploy-credentials.sh

deploy-orca: render
	kubectl --context kind-$(CLUSTER_NAME) apply -f "$(ORCA_RENDERED)/02-rbac.yaml"
	kubectl --context kind-$(CLUSTER_NAME) apply -f "$(ORCA_RENDERED)/03-config.yaml"
	# Service before Deployment: the headless orca-peers Service must
	# exist (with its DNS A-records) before the pods start so the
	# initial cluster.refresh sees the full peer set instead of
	# bootstrapping into the self-only fallback.
	kubectl --context kind-$(CLUSTER_NAME) apply -f "$(ORCA_RENDERED)/05-service.yaml"
	kubectl --context kind-$(CLUSTER_NAME) apply -f "$(ORCA_RENDERED)/04-deployment.yaml"

wait-ready:
	kubectl --context kind-$(CLUSTER_NAME) -n $(NAMESPACE) rollout status deployment/orca --timeout=180s

# -- Operate ------------------------------------------------------------------

status:
	kubectl --context kind-$(CLUSTER_NAME) -n $(NAMESPACE) get pods -o wide

logs:
	kubectl --context kind-$(CLUSTER_NAME) -n $(NAMESPACE) logs -l app.kubernetes.io/name=orca --tail=200 -f

port-forward:
	@echo "Forwarding localhost:8443 -> svc/orca:8443 ..."
	kubectl --context kind-$(CLUSTER_NAME) -n $(NAMESPACE) port-forward svc/orca 8443:8443

dev-azure: ## Upload a file to real Azure (requires AZURE_STORAGE_* in .env; pass FILE=...)
	@[ -n "$(FILE)" ] || { echo "Usage: make dev-azure FILE=/path/to/file [ARGS='--name foo']" >&2; exit 1; }
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	[ -n "$${AZURE_STORAGE_ACCOUNT:-}" ] || { echo "AZURE_STORAGE_ACCOUNT not set in $(ENV_FILE)" >&2; exit 1; }; \
	[ -n "$${AZURE_STORAGE_KEY:-}" ]     || { echo "AZURE_STORAGE_KEY not set in $(ENV_FILE)"     >&2; exit 1; }; \
	[ -n "$${AZURE_CONTAINER:-}" ]       || { echo "AZURE_CONTAINER not set in $(ENV_FILE)"       >&2; exit 1; }; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" upload \
		--origin-driver azureblob \
		--origin-endpoint "https://$${AZURE_STORAGE_ACCOUNT}.blob.core.windows.net/" \
		--origin-account "$${AZURE_STORAGE_ACCOUNT}" \
		--origin-account-key "$${AZURE_STORAGE_KEY}" \
		--origin-bucket "$${AZURE_CONTAINER}" \
		--file "$(FILE)" \
		$(ARGS)

# -- orcadev: dev / debug tool --------------------------------------------------
#
# These targets invoke hack/cmd/orcadev. The tool talks to the
# in-cluster Azurite (azureblob origin) on NodePort 30100, the
# LocalStack S3 (awss3 origin + cachestore) on NodePort 30200, and
# the orca edge listener at http://localhost:8443. For the edge
# listener orcadev auto-manages a kubectl port-forward to svc/orca
# (probes localhost:8443 first; spawns + cleans up only if the port
# is not already in use). Pass --auto-port-forward=false (via ARGS)
# to disable, or start `make -C hack/orca port-forward` in another
# shell for a long-lived forward.
#
# Pass extra flags via ARGS, e.g.:
#
#   make -C hack/orca data-generate ARGS='--size 10MiB --count 5'
#   make -C hack/orca data-upload   FILE=~/data.tar.gz
#   make -C hack/orca data-random   NAME=orca-test SIZE=10MiB
#   make -C hack/orca data-list
#   make -C hack/orca data-delete   PREFIX=synth ARGS='--yes'
#   make -C hack/orca roundtrip     FILE=~/data.bin
#   make -C hack/orca cache-list
#   make -C hack/orca cache-inspect BUCKET=orca-origin KEY=data.bin
#   make -C hack/orca bench         KEY=data.bin ARGS='--duration 30s'
#   make -C hack/orca scenario      NAME=cold-warm

data-generate: ## Generate synthetic blobs and upload to the origin
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" upload --generate $(ARGS)

data-upload: ## Upload a file to the origin (use FILE=/path/to/file)
	@[ -n "$(FILE)" ] || { echo "Usage: make data-upload FILE=/path/to/file [ARGS='--name foo']" >&2; exit 1; }
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" upload --file "$(FILE)" $(ARGS)

data-random: ## Synthesise one random blob and upload it (NAME=foo [SIZE=10MiB])
	@[ -n "$(NAME)" ] || { echo "Usage: make data-random NAME=foo [SIZE=10MiB] [ARGS='--seed 42']" >&2; exit 1; }
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" upload --generate \
		--count 1 --name "$(NAME)" --size "$${SIZE:-10MiB}" $(ARGS)

data-list: ## List objects in the origin
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" list $(ARGS)

data-delete: ## Delete objects from the origin (use PREFIX=foo)
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" delete --prefix "$(PREFIX)" $(ARGS)

roundtrip: ## Upload a file, fetch via orca, verify SHA-256 matches (FILE=path)
	@[ -n "$(FILE)" ] || { echo "Usage: make roundtrip FILE=/path/to/file [ARGS='--repeat 3']" >&2; exit 1; }
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" roundtrip --file "$(FILE)" $(ARGS)

cache-list: ## Enumerate cachestore chunks
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" cache list $(ARGS)

cache-inspect: ## Show chunk presence for an object (BUCKET=b KEY=k)
	@[ -n "$(BUCKET)" ] && [ -n "$(KEY)" ] || { echo "Usage: make cache-inspect BUCKET=b KEY=k" >&2; exit 1; }
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" cache inspect --bucket "$(BUCKET)" --key "$(KEY)" $(ARGS)

cache-clear: ## Delete chunks from the cachestore (see --all / --origin-id / --object)
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" cache clear $(ARGS)

bench: ## Parallel GET throughput / latency benchmark (KEY=name)
	@[ -n "$(KEY)" ] || { echo "Usage: make bench KEY=foo [ARGS='--duration 30s --concurrency 16']" >&2; exit 1; }
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" bench --key "$(KEY)" $(ARGS)

scenario: ## Run a canned end-to-end scenario (NAME=cold-warm|range-stress|empty-object|etag-change)
	@[ -n "$(NAME)" ] || { echo "Usage: make scenario NAME=cold-warm [ARGS='--size 8MiB']" >&2; exit 1; }
	@if [ -f "$(ENV_FILE)" ]; then set -a && . "$(ENV_FILE)" && set +a; fi; \
	go run "$(REPO_ROOT)/hack/cmd/orcadev" scenario "$(NAME)" $(ARGS)
