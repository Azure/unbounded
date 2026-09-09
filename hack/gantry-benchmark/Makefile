REPO_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)
ENV_FILE ?= $(CURDIR)/env.local
DEPLOY_CONFIG ?= $(CURDIR)/deploy.env

.PHONY: help test monitor profile-gantry deploy deploy-plan deploy-status operator-vm-check operator-vm-provision operator-vm-status operator-vm-watch operator-vm-prebuild operator-vm-run-full operator-vm-run-pool operator-vm-run-fresh operator-vm-run-standalone operator-vm-ensure-fail-open operator-vm-deploy-runtime operator-vm-deploy-runtime-run operator-vm-deploy-runtime-status operator-vm-deploy-runtime-watch operator-vm-deploy-runtime-fast operator-vm-image-pool-status operator-vm-compare-layers operator-vm-resolve-standalone-image operator-vm-prune-images operator-vm-prune-images-status proxy-image proxy-push image-pool-status prebuild-gantry enable prepare prepare-adopt prepare-gantry prepare-gantry-fresh prepare-gantry-standalone prepare-gantry-standalone-adopt prepare-gantry-adopt prepare-gantry-pool preflight run run-gantry status disable

help: ## Show benchmark targets
	@echo ""
	@echo "Usage: make -C hack/gantry-benchmark <target> [ENV_FILE=path]"
	@echo ""
	@echo "  test         Run focused proxy and benchmark tests"
	@echo "  monitor      Live peer-outcome and layer-byte tables for the active Gantry phase"
	@echo "  profile-gantry  CPU-profile the hottest Gantry pods in an active diagnostic run"
	@echo "  deploy       Idempotently deploy the complete benchmark stack"
	@echo "  deploy-plan  Print the resolved deployment contract without mutation"
	@echo "  deploy-status  Report deployment readiness without mutation"
	@echo "  operator-vm-check      Validate operator VM scripts"
	@echo "  operator-vm-provision  Provision/bootstrap the private operator VM"
	@echo "  operator-vm-status     Print one live operator VM progress snapshot"
	@echo "  operator-vm-watch      Follow operator VM progress until completion"
	@echo "  operator-vm-prebuild   Start an asynchronous operator-VM pool build"
	@echo "  operator-vm-run-full   Start full lifecycle; optional ADOPT_* values reuse images"
	@echo "  operator-vm-run-pool   Start Gantry-only using the next compatible pool image"
	@echo "  operator-vm-run-fresh  Build a brand-new image, then run Gantry-only"
	@echo "  operator-vm-run-standalone  Run Gantry with no baseline or comparison"
	@echo "  operator-vm-ensure-fail-open  Install validated fail-open routing on the active run"
	@echo "  operator-vm-deploy-runtime  Deploy fixed Gantry without starting a benchmark"
	@echo "  operator-vm-deploy-runtime-run  Deploy fixed Gantry, then start standalone"
	@echo "  operator-vm-deploy-runtime-status  Show fixed Gantry deployment status"
	@echo "  operator-vm-deploy-runtime-watch  Follow fixed Gantry deployment progress"
	@echo "  operator-vm-deploy-runtime-fast  Raise active Gantry rollout parallelism"
	@echo "  operator-vm-image-pool-status  Show operator-VM pool builder and entries"
	@echo "  operator-vm-compare-layers  Compare layer digests for IMAGE_A and IMAGE_B"
	@echo "  operator-vm-resolve-standalone-image  Print a standalone run's pushed image digest and payload SHA"
	@echo "  operator-vm-prune-images  Asynchronously prune unused operator-VM images"
	@echo "  operator-vm-prune-images-status  Show operator-VM image-prune progress"
	@echo "  proxy-image  Build BENCHMARK_PROXY_IMAGE"
	@echo "  proxy-push   Push BENCHMARK_PROXY_IMAGE"
	@echo "  image-pool-status  List ready and claimed prebuilt Gantry images"
	@echo "  prebuild-gantry  Build GANTRY_IMAGE_POOL_COUNT reusable Gantry images"
	@echo "  enable       Install benchmark instrumentation"
	@echo "  prepare      Build and push both digest-pinned workload images"
	@echo "  prepare-adopt  Adopt already-pushed direct-mode workload images"
	@echo "  prepare-gantry  Build only a cache-cold Gantry image from a retained baseline"
	@echo "  prepare-gantry-fresh  Build only Gantry with a brand-new random payload"
	@echo "  prepare-gantry-standalone-adopt  Adopt an already-pushed standalone Gantry image digest"
	@echo "  prepare-gantry-adopt  Adopt an already-pushed fresh Gantry image digest"
	@echo "  prepare-gantry-pool  Claim one compatible prebuilt Gantry image"
	@echo "  preflight    Validate ACR, monitoring, Gantry, and all target nodes"
	@echo "  run          Execute baseline and Gantry cold phases"
	@echo "  run-gantry   Execute only Gantry cold against the retained baseline"
	@echo "  status       Print active benchmark state"
	@echo "  disable      Restore routing and remove instrumentation"

test:
	$(MAKE) operator-vm-check
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go test ./hack/cmd/acr-origin-proxy ./hack/cmd/gantry-benchmark ./hack/cmd/gantry-benchmark-monitor

monitor:
	set -a; \
	. "$(DEPLOY_CONFIG)"; \
	set +a; \
	export KUBECONFIG="$${DEPLOY_KUBECONFIG:-$(REPO_ROOT)/tmp/$${DEPLOYMENT_NAME}/kubeconfig}"; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark-monitor $(MONITOR_ARGS)

profile-gantry: operator-vm-check
	set -a; \
	. "$(DEPLOY_CONFIG)"; \
	set +a; \
	export KUBECONFIG="$${DEPLOY_KUBECONFIG:-$(REPO_ROOT)/tmp/$${DEPLOYMENT_NAME}/kubeconfig}"; \
	cd "$(REPO_ROOT)" && GANTRY_PPROF_SECONDS="$${GANTRY_PPROF_SECONDS:-30}" \
		GANTRY_PPROF_COUNT="$${GANTRY_PPROF_COUNT:-3}" \
		hack/gantry-benchmark/profile-gantry.sh

operator-vm-check:
	bash -n deploy.sh profile-gantry.sh operator-vm-provision.sh operator-vm-bootstrap.sh operator-vm-build-images.sh operator-vm-deploy-runtime.sh operator-vm-run.sh operator-vm-prebuild-images.sh operator-vm-image-pool.sh operator-vm-prune-images-remote.sh operator-vm-status.sh operator-vm-watch.sh
	bash deploy-topology-test.sh
	./deploy.sh plan deploy.env.example >/dev/null
	! grep -Eq 'az acr login|podman (build|push|login|pull|tag)' deploy.sh
	! grep -Eq '^[[:space:]]*az login([[:space:]]|$$)' deploy.sh
	! grep -Eq '^[[:space:]]*export START_BENCHMARK=false' deploy.sh
	! grep -Fq 'kubectl apply -f "$$rendered/rendezvous-leases.yaml"' deploy.sh
	grep -Fq 'ensure_chair_leases "$$rendered/rendezvous-leases.yaml"' deploy.sh

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

operator-vm-prebuild: operator-vm-check
	@test -n "$(GANTRY_IMAGE_POOL_COUNT)" || { echo "GANTRY_IMAGE_POOL_COUNT is required" >&2; exit 2; }
	cd "$(REPO_ROOT)" && BENCHMARK_IMAGE_SIZE_MIB="$(BENCHMARK_IMAGE_SIZE_MIB)" \
		BENCHMARK_IMAGE_LAYERS="$(BENCHMARK_IMAGE_LAYERS)" \
		hack/gantry-benchmark/operator-vm-image-pool.sh start "$(GANTRY_IMAGE_POOL_COUNT)"

operator-vm-run-full: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh full

operator-vm-run-pool: operator-vm-check
	@test -n "$(GANTRY_ONLY_BASELINE_RUN_ID)" || { echo "GANTRY_ONLY_BASELINE_RUN_ID is required" >&2; exit 2; }
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh run "$(GANTRY_ONLY_BASELINE_RUN_ID)"

operator-vm-run-fresh: operator-vm-check
	@test -n "$(GANTRY_ONLY_BASELINE_RUN_ID)" || { echo "GANTRY_ONLY_BASELINE_RUN_ID is required" >&2; exit 2; }
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh fresh "$(GANTRY_ONLY_BASELINE_RUN_ID)"

operator-vm-run-standalone: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh standalone

operator-vm-ensure-fail-open: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh fail-open

operator-vm-deploy-runtime: operator-vm-check
	cd "$(REPO_ROOT)" && DEPLOY_CONFIG="$(DEPLOY_CONFIG)" hack/gantry-benchmark/operator-vm-deploy-runtime.sh deploy

operator-vm-deploy-runtime-run: operator-vm-check
	cd "$(REPO_ROOT)" && DEPLOY_CONFIG="$(DEPLOY_CONFIG)" hack/gantry-benchmark/operator-vm-deploy-runtime.sh start

operator-vm-deploy-runtime-status: operator-vm-check
	cd "$(REPO_ROOT)" && DEPLOY_CONFIG="$(DEPLOY_CONFIG)" hack/gantry-benchmark/operator-vm-deploy-runtime.sh status

operator-vm-deploy-runtime-watch: operator-vm-check
	cd "$(REPO_ROOT)" && DEPLOY_CONFIG="$(DEPLOY_CONFIG)" hack/gantry-benchmark/operator-vm-deploy-runtime.sh watch

operator-vm-deploy-runtime-fast: operator-vm-check
	cd "$(REPO_ROOT)" && DEPLOY_CONFIG="$(DEPLOY_CONFIG)" hack/gantry-benchmark/operator-vm-deploy-runtime.sh accelerate

operator-vm-image-pool-status: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh status

operator-vm-compare-layers: operator-vm-check
	@test -n "$(IMAGE_A)" || { echo "IMAGE_A is required" >&2; exit 2; }
	@test -n "$(IMAGE_B)" || { echo "IMAGE_B is required" >&2; exit 2; }
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh compare-layers "$(IMAGE_A)" "$(IMAGE_B)"

operator-vm-resolve-standalone-image: operator-vm-check
	@test -n "$(RESOLVE_RUN_ID)" || { echo "RESOLVE_RUN_ID is required" >&2; exit 2; }
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh resolve-standalone-image "$(RESOLVE_RUN_ID)"

operator-vm-prune-images: operator-vm-check
	@test "$(CONFIRM_PRUNE_UNUSED_IMAGES)" = yes || { echo "set CONFIRM_PRUNE_UNUSED_IMAGES=yes to prune every unused operator-VM image" >&2; exit 2; }
	cd "$(REPO_ROOT)" && CONFIRM_PRUNE_UNUSED_IMAGES=yes hack/gantry-benchmark/operator-vm-image-pool.sh prune

operator-vm-prune-images-status: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh prune-status

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

image-pool-status:
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark image-pool-status

prebuild-gantry:
	@test -n "$(GANTRY_IMAGE_POOL_COUNT)" || { echo "GANTRY_IMAGE_POOL_COUNT is required" >&2; exit 2; }
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark prebuild-gantry "$(GANTRY_IMAGE_POOL_COUNT)"

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

prepare-gantry-standalone-adopt:
	@test -n "$(GANTRY_STANDALONE_ADOPT_IMAGE)" || { echo "GANTRY_STANDALONE_ADOPT_IMAGE is required" >&2; exit 2; }
	@test -n "$(GANTRY_STANDALONE_ADOPT_PAYLOAD_SHA256)" || { echo "GANTRY_STANDALONE_ADOPT_PAYLOAD_SHA256 is required" >&2; exit 2; }
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark prepare-gantry-standalone-adopt \
		"$(GANTRY_STANDALONE_ADOPT_IMAGE)" "$(GANTRY_STANDALONE_ADOPT_PAYLOAD_SHA256)"

prepare-gantry-standalone:
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark prepare-gantry-standalone

prepare-gantry-adopt:
	@test -n "$(GANTRY_ONLY_BASELINE_RUN_ID)" || { echo "GANTRY_ONLY_BASELINE_RUN_ID is required" >&2; exit 2; }
	@test -n "$(GANTRY_ONLY_ADOPT_IMAGE)" || { echo "GANTRY_ONLY_ADOPT_IMAGE is required" >&2; exit 2; }
	@test -n "$(GANTRY_ONLY_ADOPT_PAYLOAD_SHA256)" || { echo "GANTRY_ONLY_ADOPT_PAYLOAD_SHA256 is required" >&2; exit 2; }
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark prepare-gantry-adopt \
		"$(GANTRY_ONLY_BASELINE_RUN_ID)" "$(GANTRY_ONLY_ADOPT_IMAGE)" "$(GANTRY_ONLY_ADOPT_PAYLOAD_SHA256)"

prepare-gantry-pool:
	@test -n "$(GANTRY_ONLY_BASELINE_RUN_ID)" || { echo "GANTRY_ONLY_BASELINE_RUN_ID is required" >&2; exit 2; }
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark prepare-gantry-pool \
		"$(GANTRY_ONLY_BASELINE_RUN_ID)"
