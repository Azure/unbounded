# hack/orca/Makefile - operational convenience verbs for an
# already-installed Orca dev cluster.
#
# The single coherent install entrypoint is hack/orca/setup-orca.sh
# (with hack/orca/kind-up.sh / hack/orca/kind-down.sh for kind cluster
# lifecycle). The root Makefile exposes proxies:
#
#   make orca-install      install Orca into the current kubectl context
#   make orca-kind-up      create kind cluster + install Orca (== make orca-up)
#   make orca-kind-down    delete kind cluster (== make orca-down)
#   make orca-reset        rebuild image + side-load + rollout-restart on kind
#
# This file only contains the post-install operational verbs (status,
# logs, port-forward) that don't fit naturally into the install
# script. Everything else - seeding data, roundtrips, benchmarks,
# scenarios, cachestore inspection - is done directly with
# `bin/orcadev <verb>`. See hack/orca/README.md.

REPO_ROOT  := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/../..)

# Cluster + namespace knobs. Defaults match setup-orca.sh /
# kind-up.sh; override on the command line if your install used a
# non-default context or namespace.
CLUSTER_NAME ?= orca-dev
NAMESPACE    ?= unbounded-kube
KUBECTL_CTX   = kind-$(CLUSTER_NAME)

.PHONY: help status logs port-forward

help: ## Show this help
	@echo ""
	@echo "Usage: make -C hack/orca <target> [VAR=value ...]"
	@echo ""
	@echo "Install / uninstall live in the root Makefile and use the"
	@echo "single coherent setup-orca.sh entrypoint. See hack/orca/README.md."
	@echo ""
	@echo "Operate (post-install convenience):"
	@echo "  status              kubectl get pods -n $(NAMESPACE)"
	@echo "  logs                Tail logs from all Orca pods"
	@echo "  port-forward        Foreground forward localhost:8443 -> svc/orca"
	@echo ""
	@echo "Seed data, roundtrips, benchmarks, scenarios:"
	@echo "  Use bin/orcadev <verb> directly. Run 'bin/orcadev --help'"
	@echo "  for the full subcommand tree. orcadev auto-opens port-forwards"
	@echo "  to svc/orca, svc/azurite, svc/garage as needed."
	@echo ""
	@echo "Variables:"
	@echo "  CLUSTER_NAME=$(CLUSTER_NAME)"
	@echo "  NAMESPACE=$(NAMESPACE)"
	@echo "  KUBECTL_CTX=$(KUBECTL_CTX)"

status: ## kubectl get pods -o wide -n $(NAMESPACE)
	kubectl --context $(KUBECTL_CTX) -n $(NAMESPACE) get pods -o wide

logs: ## Tail logs from all Orca pods
	kubectl --context $(KUBECTL_CTX) -n $(NAMESPACE) logs -l app.kubernetes.io/name=orca --tail=200 -f

port-forward: ## Foreground forward localhost:8443 -> svc/orca:8443
	@echo "Forwarding localhost:8443 -> svc/orca:8443 (Ctrl-C to stop)"
	kubectl --context $(KUBECTL_CTX) -n $(NAMESPACE) port-forward svc/orca 8443:8443
