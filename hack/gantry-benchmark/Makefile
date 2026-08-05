REPO_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)
ENV_FILE ?= $(CURDIR)/env.local
DEPLOY_CONFIG ?= $(CURDIR)/deploy.env

.PHONY: help test deploy deploy-plan deploy-status operator-vm-check operator-vm-provision operator-vm-status operator-vm-watch proxy-image proxy-push enable prepare prepare-adopt prepare-gantry prepare-gantry-fresh prepare-gantry-adopt preflight run run-gantry status disable

help: ## Show benchmark targets
	@echo ""
	@echo "Usage: make -C hack/gantry-benchmark <target> [ENV_FILE=path]"
	@echo ""
	@echo "  test         Run focused proxy and benchmark tests"
	@echo "  deploy       Idempotently deploy the complete benchmark stack"
	@echo "  deploy-plan  Print the resolved deployment contract without mutation"
	@echo "  deploy-status  Report deployment readiness without mutation"
	@echo "  operator-vm-check      Validate operator VM scripts"
	@echo "  operator-vm-provision  Provision/bootstrap the private operator VM"
	@echo "  operator-vm-status     Print one live operator VM progress snapshot"
	@echo "  operator-vm-watch      Follow operator VM progress until completion"
	@echo "  proxy-image  Build BENCHMARK_PROXY_IMAGE"
	@echo "  proxy-push   Push BENCHMARK_PROXY_IMAGE"
	@echo "  enable       Install benchmark instrumentation"
	@echo "  prepare      Build and push both digest-pinned workload images"
	@echo "  prepare-adopt  Adopt already-pushed direct-mode workload images"
	@echo "  prepare-gantry  Build only a cache-cold Gantry image from a retained baseline"
	@echo "  prepare-gantry-fresh  Build only Gantry with a brand-new random payload"
	@echo "  prepare-gantry-adopt  Adopt an already-pushed fresh Gantry image digest"
	@echo "  preflight    Validate ACR, monitoring, Gantry, and all target nodes"
	@echo "  run          Execute baseline and Gantry cold phases"
	@echo "  run-gantry   Execute only Gantry cold against the retained baseline"
	@echo "  status       Print active benchmark state"
	@echo "  disable      Restore routing and remove instrumentation"

test:
	$(MAKE) operator-vm-check
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go test ./hack/cmd/acr-origin-proxy ./hack/cmd/gantry-benchmark

operator-vm-check:
	bash -n deploy.sh operator-vm-provision.sh operator-vm-bootstrap.sh operator-vm-run.sh operator-vm-status.sh operator-vm-watch.sh
	./deploy.sh plan deploy.env.example >/dev/null

deploy: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/deploy.sh deploy "$(DEPLOY_CONFIG)"

deploy-plan: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/deploy.sh plan "$(DEPLOY_CONFIG)"

deploy-status: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/deploy.sh status "$(DEPLOY_CONFIG)"

operator-vm-provision: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-provision.sh

operator-vm-status: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-watch.sh

operator-vm-watch: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-watch.sh --follow

proxy-image:
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	: "$${BENCHMARK_PROXY_IMAGE:?set BENCHMARK_PROXY_IMAGE}"; \
	engine="$${CONTAINER_ENGINE:-podman}"; \
	if [ "$${engine}" = podman ]; then isolation_args="--isolation chroot"; else isolation_args=""; fi; \
	cd "$(REPO_ROOT)" && "$${engine}" build $${isolation_args} \
		--platform "$${BENCHMARK_IMAGE_PLATFORM:-linux/amd64}" \
		--tag "$${BENCHMARK_PROXY_IMAGE}" \
		--file images/acr-origin-proxy/Containerfile .

proxy-push:
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	: "$${BENCHMARK_PROXY_IMAGE:?set BENCHMARK_PROXY_IMAGE}"; \
	: "$${ACR_LOGIN_SERVER:?set ACR_LOGIN_SERVER}"; \
	: "$${ACR_USERNAME:?set ACR_USERNAME}"; \
	: "$${ACR_PASSWORD:?set ACR_PASSWORD}"; \
	printf '%s\n' "$${ACR_PASSWORD}" | "$${CONTAINER_ENGINE:-podman}" login \
		--username "$${ACR_USERNAME}" --password-stdin "$${ACR_LOGIN_SERVER}"; \
	"$${CONTAINER_ENGINE:-podman}" push "$${BENCHMARK_PROXY_IMAGE}"

enable prepare preflight run run-gantry status disable:
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark $@

prepare-adopt:
	@test -n "$(ADOPT_BASELINE_IMAGE)" || { echo "ADOPT_BASELINE_IMAGE is required" >&2; exit 2; }
	@test -n "$(ADOPT_GANTRY_IMAGE)" || { echo "ADOPT_GANTRY_IMAGE is required" >&2; exit 2; }
	@test -n "$(ADOPT_PAYLOAD_SHA256)" || { echo "ADOPT_PAYLOAD_SHA256 is required" >&2; exit 2; }
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark prepare-adopt \
		"$(ADOPT_BASELINE_IMAGE)" "$(ADOPT_GANTRY_IMAGE)" "$(ADOPT_PAYLOAD_SHA256)"

prepare-gantry:
	@test -n "$(GANTRY_ONLY_BASELINE_RUN_ID)" || { echo "GANTRY_ONLY_BASELINE_RUN_ID is required" >&2; exit 2; }
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark prepare-gantry \
		"$(GANTRY_ONLY_BASELINE_RUN_ID)" $(if $(GANTRY_ONLY_PREPARED_RUN_ID),"$(GANTRY_ONLY_PREPARED_RUN_ID)",)

prepare-gantry-fresh:
	@test -n "$(GANTRY_ONLY_BASELINE_RUN_ID)" || { echo "GANTRY_ONLY_BASELINE_RUN_ID is required" >&2; exit 2; }
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark prepare-gantry-fresh \
		"$(GANTRY_ONLY_BASELINE_RUN_ID)"

prepare-gantry-adopt:
	@test -n "$(GANTRY_ONLY_BASELINE_RUN_ID)" || { echo "GANTRY_ONLY_BASELINE_RUN_ID is required" >&2; exit 2; }
	@test -n "$(GANTRY_ONLY_ADOPT_IMAGE)" || { echo "GANTRY_ONLY_ADOPT_IMAGE is required" >&2; exit 2; }
	@test -n "$(GANTRY_ONLY_ADOPT_PAYLOAD_SHA256)" || { echo "GANTRY_ONLY_ADOPT_PAYLOAD_SHA256 is required" >&2; exit 2; }
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark prepare-gantry-adopt \
		"$(GANTRY_ONLY_BASELINE_RUN_ID)" "$(GANTRY_ONLY_ADOPT_IMAGE)" "$(GANTRY_ONLY_ADOPT_PAYLOAD_SHA256)"