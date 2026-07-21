REPO_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)
ENV_FILE ?= $(CURDIR)/env.local

.PHONY: help test enable preflight report run status disable

help: ## Show benchmark targets
	@echo ""
	@echo "Usage: make -C hack/gantry-benchmark <target> [ENV_FILE=path]"
	@echo ""
	@echo "  test         Run focused benchmark tests"
	@echo "  enable       Install benchmark instrumentation"
	@echo "  preflight    Validate ACR, monitoring, Gantry, and all target nodes"
	@echo "  report       Regenerate comparison artifacts for RUN_ID"
	@echo "  run          Execute baseline and Gantry cold phases"
	@echo "  status       Print active benchmark state"
	@echo "  disable      Restore routing and remove instrumentation"

test:
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go test ./hack/cmd/gantry-benchmark

enable preflight run status disable:
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark $@

report:
	: "$${RUN_ID:?set RUN_ID}"; \
	set -a; \
	[ ! -f "$(ENV_FILE)" ] || . "$(ENV_FILE)"; \
	set +a; \
	cd "$(REPO_ROOT)" && GOTOOLCHAIN=auto go run ./hack/cmd/gantry-benchmark report "$${RUN_ID}"