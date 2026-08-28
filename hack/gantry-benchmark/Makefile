REPO_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)
ENV_FILE ?= $(CURDIR)/env.local
DEPLOY_CONFIG ?= $(CURDIR)/deploy.env

.PHONY: help test monitor profile-gantry deploy deploy-plan deploy-status operator-vm-check operator-vm-provision operator-vm-ssh-configure operator-vm-ssh operator-vm-status operator-vm-watch operator-vm-prebuild operator-vm-run-pool operator-vm-run-fresh operator-vm-image-pool-status proxy-image proxy-push image-pool-status prebuild-gantry enable prepare prepare-adopt prepare-gantry prepare-gantry-fresh prepare-gantry-adopt prepare-gantry-pool preflight run run-gantry status disable

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
	@echo "  operator-vm-provision  Provision/bootstrap the operator VM"
	@echo "  operator-vm-ssh-configure  Reconcile SSH endpoint and workstation config"
	@echo "  operator-vm-ssh        Open SSH to the operator VM on TCP 50001"
	@echo "  operator-vm-status     Print one live operator VM progress snapshot"
	@echo "  operator-vm-watch      Follow operator VM progress until completion"
	@echo "  operator-vm-prebuild   Start an asynchronous operator-VM pool build"
	@echo "  operator-vm-run-pool   Start Gantry-only using the next compatible pool image"
	@echo "  operator-vm-run-fresh  Build a brand-new image, then run Gantry-only"
	@echo "  operator-vm-image-pool-status  Show operator-VM pool builder and entries"
	@echo "  proxy-image  Build BENCHMARK_PROXY_IMAGE"
	@echo "  proxy-push   Push BENCHMARK_PROXY_IMAGE"
	@echo "  image-pool-status  List ready and claimed prebuilt Gantry images"
	@echo "  prebuild-gantry  Build GANTRY_IMAGE_POOL_COUNT reusable Gantry images"
	@echo "  enable       Install benchmark instrumentation"
	@echo "  prepare      Build and push both digest-pinned workload images"
	@echo "  prepare-adopt  Adopt already-pushed direct-mode workload images"
	@echo "  prepare-gantry  Build only a cache-cold Gantry image from a retained baseline"
	@echo "  prepare-gantry-fresh  Build only Gantry with a brand-new random payload"
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
	bash -n deploy.sh profile-gantry.sh operator-vm-provision.sh operator-vm-bootstrap.sh operator-vm-build-images.sh operator-vm-configure-sshd.sh operator-vm-run.sh operator-vm-prebuild-images.sh operator-vm-image-pool.sh operator-vm-ssh-common.sh operator-vm-ssh-configure.sh operator-vm-status.sh operator-vm-watch.sh
	./deploy.sh plan deploy.env.example >/dev/null
	! grep -Eq 'az acr login|podman (build|push|login|pull|tag)' deploy.sh
	! grep -Eq '^[[:space:]]*az login([[:space:]]|$$)' deploy.sh
	! grep -Eq '^[[:space:]]*export START_BENCHMARK=false' deploy.sh
	grep -Fq 'Port 50001' operator-vm-configure-sshd.sh
	! grep -Eq '^[[:space:]]*Port 22([[:space:]]|$$)' operator-vm-configure-sshd.sh
	! grep -Fq 'az vm run-command invoke' operator-vm-watch.sh
	grep -Fq 'git -C "$$repo_root" archive --format=tar HEAD | tar -xf - -C "$$source_context"' deploy.sh
	grep -Fq 'kubectl apply -f "$$rendered/rendezvous-leases.yaml"' deploy.sh

deploy: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/deploy.sh deploy "$(DEPLOY_CONFIG)"

deploy-plan: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/deploy.sh plan "$(DEPLOY_CONFIG)"

deploy-status: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/deploy.sh status "$(DEPLOY_CONFIG)"

operator-vm-provision: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-provision.sh

operator-vm-ssh-configure: operator-vm-check
	set -a; \
	. "$(DEPLOY_CONFIG)"; \
	set +a; \
	export AZURE_RESOURCE_GROUP="$${AZURE_RESOURCE_GROUP:-$${DEPLOYMENT_NAME:?Set DEPLOYMENT_NAME}}"; \
	export OPERATOR_VM_NAME="$${OPERATOR_VM_NAME:-gantry-benchmark-operator}"; \
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-ssh-configure.sh

operator-vm-ssh: operator-vm-check
	set -a; \
	. "$(DEPLOY_CONFIG)"; \
	set +a; \
	resource_group="$${AZURE_RESOURCE_GROUP:-$${DEPLOYMENT_NAME:?Set DEPLOYMENT_NAME}}"; \
	config="$${OPERATOR_SSH_CONFIG_PATH:-$(REPO_ROOT)/tmp/$$resource_group/ssh-config}"; \
	host_alias="$${OPERATOR_SSH_HOST_ALIAS:-$${OPERATOR_VM_NAME:-gantry-benchmark-operator}}"; \
	test -f "$$config" || { echo "missing operator SSH config $$config; run make -C hack/gantry-benchmark operator-vm-ssh-configure" >&2; exit 1; }; \
	ssh -F "$$config" "$$host_alias"

operator-vm-status: operator-vm-check
	set -a; \
	. "$(DEPLOY_CONFIG)"; \
	set +a; \
	export AZURE_RESOURCE_GROUP="$${AZURE_RESOURCE_GROUP:-$${DEPLOYMENT_NAME:?Set DEPLOYMENT_NAME}}"; \
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-watch.sh

operator-vm-watch: operator-vm-check
	set -a; \
	. "$(DEPLOY_CONFIG)"; \
	set +a; \
	export AZURE_RESOURCE_GROUP="$${AZURE_RESOURCE_GROUP:-$${DEPLOYMENT_NAME:?Set DEPLOYMENT_NAME}}"; \
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-watch.sh --follow

operator-vm-prebuild: operator-vm-check
	@test -n "$(GANTRY_IMAGE_POOL_COUNT)" || { echo "GANTRY_IMAGE_POOL_COUNT is required" >&2; exit 2; }
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh start "$(GANTRY_IMAGE_POOL_COUNT)"

operator-vm-run-pool: operator-vm-check
	@test -n "$(GANTRY_ONLY_BASELINE_RUN_ID)" || { echo "GANTRY_ONLY_BASELINE_RUN_ID is required" >&2; exit 2; }
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh run "$(GANTRY_ONLY_BASELINE_RUN_ID)"

operator-vm-run-fresh: operator-vm-check
	@test -n "$(GANTRY_ONLY_BASELINE_RUN_ID)" || { echo "GANTRY_ONLY_BASELINE_RUN_ID is required" >&2; exit 2; }
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh fresh "$(GANTRY_ONLY_BASELINE_RUN_ID)"

operator-vm-image-pool-status: operator-vm-check
	cd "$(REPO_ROOT)" && hack/gantry-benchmark/operator-vm-image-pool.sh status

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
