# Project Overview

Project Unbounded is an open source initiative to enable Kubernetes users to run worker Nodes anywhere and connect them 
back a running control plane. This allows you to run workloads in any environment, including on-premises, in the cloud, 
and at the edge, without being limited by the location of your control plane.

## Repository Structure

unbounded-kube is organized into several directories:

- `api/` - where API definitions for custom resources are located.
  - `v1alpha3/` - Machine and Image CRD types.
  - `net/v1alpha1/` - unbounded-net CRD types (Sites, GatewayPools, SitePeerings, etc.).
- `bin/` - where generated binary artifacts should be placed.
- `bpf/` - eBPF C source code for unbounded-net tunnel encapsulation.
- `cmd/` - where the sources for each binary artifact are located. Each subdirectory corresponds to a binary artifact.
  - `agent` - sources for the unbounded-agent.
  - `inventory` - sources for the inventory controller.
  - `kubectl-unbounded` - sources for the `kubectl unbounded` plugin (includes `net` subcommand group).
  - `machina` - sources for the machina controller.
  - `metalman` - sources for the metalman controller.
  - `unbounded-net-controller` - sources for the unbounded-net control-plane controller.
  - `unbounded-net-node` - sources for the unbounded-net node agent DaemonSet.
  - `unbounded-net-routeplan-debug` - debug utility for route plan inspection.
  - `unping` - network ping utility.
  - `unroute` - route diagnostic utility.
- `deploy/` - component manifests for deploying on a Kubernetes cluster.
  - `machina/` - machina controller manifests.
  - `net/` - unbounded-net deployment templates and CRDs.
- `docs/` - documentation for the project.
  - `net/` - unbounded-net specific documentation.
- `frontend/` - frontend web applications.
  - `net/` - React/TypeScript dashboard for unbounded-net.
- `hack/` - where development tools and scripts are located.
  - `cmd/` - development tools that are built as Go binaries.
  - `net/` - unbounded-net operational scripts.
  - `scratch/` - scratch space for quick go experiments.
- `images/` - where OCI image definitions and related assets for building container images are located.
  - `unbounded-net/` - Containerfile for unbounded-net-controller and unbounded-net-node images.
- `internal/` - where shared but internal to this project packages are located.
  - `net/` - unbounded-net internal packages (allocator, config, controller, ebpf, netlink, routeplan, etc.).
- `tmp/` - project local temporary directory for intermediate stuff that will be cleaned up quickly.

## Building and Testing

- To build `machina` use `make machina` which runs formatters, lint, tests, and go build.
- To build `machina` without lint/test use `make machina-build` (used in Containerfiles).
- To build `metalman` use `make metalman` which runs formatters, lint, tests, and builds the binary.
- To build `metalman` without lint/test use `make metalman-build` (used in Containerfiles).
- For unbounded-net targets, use `make net-<target>`, e.g. `make net-build`, `make net-test`, `make net-lint`.
  These delegate to `net.Makefile`.

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
