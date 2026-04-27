# Project Overview

Project Unbounded is an open source initiative to enable Kubernetes users to run worker Nodes anywhere and connect them 
back a running control plane. This allows you to run workloads in any environment, including on-premises, in the cloud, 
and at the edge, without being limited by the location of your control plane.

## Repository Structure

unbounded-kube is organized into several directories:

- `api/` - where API definitions for custom resources are located.
  - `machina/v1alpha3/` - Machine CRD types (unbounded-cloud.io group).
  - `net/v1alpha1/` - Net CRD types (net.unbounded-cloud.io group): Site, GatewayPool, SitePeering, etc.
- `bin/` - where generated binary artifacts should be placed.
- `bpf/` - eBPF C programs for network encapsulation (compiled with clang).
- `cmd/` - where the sources for each binary artifact are located. Each subdirectory corresponds to a binary artifact.
  - `agent` - sources for the unbounded-agent.
  - `inventory` - sources for the inventory controller.
  - `kubectl-unbounded` - sources for the `kubectl unbounded` plugin (includes `net` subcommand).
  - `machina` - sources for the machina controller.
  - `metalman` - sources for the metalman controller.
  - `unbounded-net-controller` - sources for the unbounded-net network controller.
  - `unbounded-net-node` - sources for the unbounded-net node agent.
  - `unbounded-net-routeplan-debug` - debugging tool for route plans.
  - `unping` - health check probe utility.
  - `unroute` - eBPF route inspection utility.
- `deploy/` - component manifests for deploying on a Kubernetes cluster.
  - `machina/` - machina controller manifest templates (*.yaml.tmpl) plus generated CRDs under `crd/`; rendered output lives under `machina/rendered/` (gitignored, produced by `make machina-manifests`).
  - `net/` - unbounded-net controller and node manifest templates (*.yaml.tmpl); rendered output lives under `net/rendered/` (gitignored, produced by `make net-manifests`).
- `docs/` - documentation for the project.
  - `net/` - unbounded-net specific documentation.
- `frontend/` - React/TypeScript web UI for network topology visualization (built with Vite).
- `hack/` - where development tools and scripts are located.
  - `cmd/` - development tools that are built as Go binaries (forge, render-manifests). `render-manifests` is a generic Go template renderer driven by repeatable `--set key=value` flags; templates rely on sprig's `default` for fallbacks.
  - `scripts/` - operational and development shell scripts.
  - `scratch/` - scratch space for quick go experiments.
- `images/` - where OCI image definitions and related assets for building container images are located.
- `internal/` - where shared but internal to this project packages are located.
  - `net/` - unbounded-net shared packages (APIs, controllers, networking, metrics, webhooks, etc.).
- `tmp/` - project local temporary directory for intermediate stuff that will be cleaned up quickly.

## Building and Testing

- `make` builds all binaries (kubectl-unbounded, forge, machina, and all net binaries).
- To build `machina` use `make machina` which runs formatters, lint, tests, and go build.
- To build `machina` without lint/test use `make machina-build` (used in Containerfiles).
- To build `metalman` use `make metalman` which runs formatters, lint, tests, and builds the binary.
- To build `metalman` without lint/test use `make metalman-build` (used in Containerfiles).
- To build individual net binaries: `make unbounded-net-controller`, `make unbounded-net-node`, `make unbounded-net-routeplan-debug`, `make unping`, `make unroute`.
- Net-specific build tasks (container images, frontend, eBPF, render) are exposed via `net-` prefixed targets in the main `Makefile` (e.g., `make net-frontend`, `make net-build-ebpf`, `make net-manifests`). Cluster deploy/undeploy targets live separately under `hack/net/` and are invoked via `make -C hack/net <target>` (e.g., `make -C hack/net deploy`). Run `make help` and `make -C hack/net help` for the full lists.
- `make generate` runs `go generate ./...` to regenerate deepcopy, CRDs, and protobuf for all packages.
- `make build` compiles all Go packages (`go build ./...`).
- `make vulncheck` runs `govulncheck` for known vulnerabilities.
- `make fmt` formats with gofumpt; `make lint` runs golangci-lint; `make test` runs all tests.
- Locally these chain: `test` -> `lint` -> `fmt`. In CI (`CI=1`), each runs independently.

## Coding Standards

- Do not cross cmd/ package boundaries. For example, `cmd/agent` should not import from `cmd/machina`. If you need to
  share code between these packages, put it in `internal/`.
- Do not use em-dashes (`—`) in comments, strings, or any source/config files. Use a plain ASCII hyphen (`-`)
  or rephrase the sentence instead.

## Testing Standards

- Add tests for new behavior. Cover success, failure, and edge cases.

## Boundaries

- **Ask first**
    - Large cross-package refactors.
    - New dependencies with broad impact.
    - Destructive data or migration changes.
    - Removal of _test.go or Test* functions or subtests.
- **Never**
    - Commit secrets, credentials, or tokens.
    - Edit generated files by hand when a generation workflow exists.
    - Use destructive git operations unless explicitly requested.
    - Go outside the project boundary, for example, DO NOT edit files in user's home directories, add or edit files 
      in /tmp or anywhere else on the host filesystem.
