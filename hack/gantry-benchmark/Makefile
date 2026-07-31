REPO_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)
ENV_FILE ?= $(CURDIR)/env.local

.PHONY: help test operator-vm-check operator-vm-provision operator-vm-status operator-vm-watch proxy-image proxy-push enable prepare preflight run status disable

help: ## Show benchmark targets
	@echo ""
	@echo "Usage: make -C hack/gantry-benchmark <target> [ENV_FILE=path]"
	@echo ""
	@echo "  test         Run focused proxy and benchmark tests"
	@echo "  operator-vm-check      Validate operator VM scripts"
	@echo "  operator-vm-provision  Provision/bootstrap the private operator VM"
	@echo "  operator-vm-status     Print one live operator VM progress snapshot"
	@echo "  operator-vm-watch      Follow operator VM progress until completion"
	@echo "  proxy-image  Build BENCHMARK_PROXY_IMAGE"
	@echo "  proxy-push   Push BENCHMARK_PROXY_IMAGE"
	@echo "  enable       Install benchmark instrumentation"
	@echo "  prepare      Build and push both digest-pinned workload images"
	@echo "  preflight    Validate ACR, monitoring, Gantry, and all target nodes"
	@echo "  run          Execute baseline and Gantry cold phases"
	@echo "  status       Print active benchmark state"
	@echo "  disable      Restore routing and remove instrumentation"

test:
	$(MAKE) operator-vm-check
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go test ./hack/cmd/acr-origin-proxy ./hack/cmd/gantry-benchmark

operator-vm-check:
	bash -n operator-vm-provision.sh operator-vm-bootstrap.sh operator-vm-run.sh operator-vm-status.sh operator-vm-watch.sh

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

enable prepare preflight run status disable:
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark $@