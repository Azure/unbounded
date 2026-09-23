# Project Overview

Project Unbounded is an open source initiative to enable Kubernetes users to run worker Nodes anywhere and connect them 
back a running control plane. This allows you to run workloads in any environment, including on-premises, in the cloud, 
and at the edge, without being limited by the location of your control plane.

## Repository Structure

unbounded-kube is organized into several directories:

- `api/` - where API definitions for custom resources are located.
  - `machina/v1alpha3/` - Machine CRD types (unbounded-cloud.io group).
  - `net/v1alpha1/` - Net CRD types (net.unbounded-cloud.io group): Site, GatewayPool, SitePeering, etc.
  - `racer/` - shared Racer control schema and generated Go bindings. Rust generates bindings from this schema using vendored protoc.
- `bin/` - where generated binary artifacts should be placed.
- `bpf/` - eBPF C programs for network encapsulation (compiled with clang).
- `cmd/` - where the sources for each binary artifact are located. Each subdirectory corresponds to a binary artifact.
  - `agent` - sources for the unbounded-agent.
  - `gantry` - sources for the gantry peer-to-peer OCI distribution agent.
  - `inventory` - sources for the inventory controller.
  - `kubectl-unbounded` - sources for the `kubectl unbounded` plugin (includes `net` subcommand).
  - `machina` - sources for the machina controller.
  - `metalman` - sources for the metalman controller.
  - `racer-controlplane` - standalone Rust Kubernetes topology controller, mTLS control server, and durable CA issuance/rotation manager. Its native Cargo.lock and target directory are independent of the dataplane.
  - `racer-dataplane` - standalone Linux Rust crate for the Racer distributed HTTP cache. Tests are attached to owning modules with `#[path]` and `include!`; `autotests = false` is intentional.
  - `racer-loadgen` - Go test load generator and origin fixture, using the root Go module.
  - `unbounded-net-controller` - sources for the unbounded-net network controller.
  - `unbounded-net-node` - sources for the unbounded-net node agent.
  - `unbounded-net-routeplan-debug` - debugging tool for route plans.
  - `unping` - health check probe utility.
  - `unroute` - eBPF route inspection utility.
- `deploy/` - component manifests for deploying on a Kubernetes cluster.
  - `machina/` - machina controller manifest templates (*.yaml.tmpl) plus generated CRDs under `crd/`; rendered output lives under `machina/rendered/` (gitignored, produced by `make machina-manifests`).
  - `gantry/` - gantry DaemonSet, ConfigMap, and ServiceAccount manifests.
  - `net/` - unbounded-net controller and node manifest templates (*.yaml.tmpl); rendered output lives under `net/rendered/` (gitignored, produced by `make net-manifests`).
- `designs/` - design documents, proposals, and internal planning documentation for the project.
- `docs/` - public web site documentation only. Do not place design documents, plans, or ad-hoc internal docs here.
- `frontend/` - React/TypeScript web UI for network topology visualization (built with Vite).
- `hack/` - where development tools and scripts are located.
  - `cmd/` - development tools that are built as Go binaries (forge, render-manifests). `render-manifests` is a generic Go template renderer driven by repeatable `--set key=value` flags; templates rely on sprig's `default` for fallbacks.
  - `scripts/` - operational and development shell scripts.
  - `scratch/` - scratch space for quick go experiments.
- `images/` - where OCI image definitions and related assets for building container images are located.
- `e2e/` - end-to-end integration test suites.
  - `gantry/` - kind-based e2e tests for gantry (guarded by `//go:build e2e`).
- `internal/` - where shared but internal to this project packages are located.
  - `gantry/` - gantry shared packages (21 sub-packages: config, mirror, transfer, discovery, coord, hrw, coldstart, members, metrics, etc.). Includes `internal/gantry/proto/coord/v1/` for the libp2p coordination RPC messages (pull intent, please-pull); kept under internal/ so the wire schema isn't an exported API surface.
  - `net/` - unbounded-net shared packages (APIs, controllers, networking, metrics, webhooks, etc.).
  - `racer/` - shared Racer metadata, Site-to-universe mapping, and protocol helpers. The public SDK and origin helpers live in `pkg/racer/`.
- `tmp/` - project local temporary directory for intermediate stuff that will be cleaned up quickly.

## Building and Testing

- `make` builds all binaries (kubectl-unbounded, forge, machina, and all net binaries).
- To build `machina` use `make machina` which runs formatters, lint, tests, and go build.
- To build `machina` without lint/test use `make machina-build` (used in Containerfiles).
- To build `metalman` use `make metalman` which runs formatters, lint, tests, and builds the binary.
- To build `metalman` without lint/test use `make metalman-build` (used in Containerfiles).
- To build individual net binaries: `make unbounded-net-controller`, `make unbounded-net-node`, `make unbounded-net-routeplan-debug`, `make unping`, `make unroute`.
- To build `gantry` use `make gantry` which runs tests and builds the binary.
- To build `gantry` without lint/test use `make gantry-build` (used in Containerfiles).
- To build Racer, install Rust 1.96.0, `cc`, `ar`, `make`, Perl, libibverbs and OpenSSL development headers, and `pkg-config` (Ubuntu: `build-essential perl libibverbs-dev libssl-dev pkg-config`), then run `make racer-build`. This produces `bin/racer-controlplane`, `bin/racer-dataplane`, `bin/racer-loadgen`, and `bin/racer-object`. Both Rust crates use their own committed Cargo.lock and vendored protoc with `../../api/racer`. The control plane uses system OpenSSL for certificate cryptography and rustls for transport; its image builds with `libssl-dev` and runs with `libssl3t64`. Override `RACER_CONTROLPLANE_CARGO_TARGET_DIR` for isolated control-plane builds; `RACER_CARGO_TARGET_DIR` selects only the dataplane cache. Build recipes pass `VERSION`, `GIT_COMMIT`, and `BUILD_TIME`; the control plane also receives `RACER_VERSION` and `RACER_COMMIT` for its native version output.
- The dataplane statically links vendored OpenSSL with kTLS enabled; it needs no system OpenSSL, protoc, or liburing library. Runtime kTLS eligibility requires Linux >= 6.14. To select an external OpenSSL for the dataplane instead, set `OPENSSL_NO_VENDOR=1` and install its development headers and `pkg-config` (or set `OPENSSL_DIR`); both the Rust bindings and C shim use that installation. External OpenSSL must be built with kTLS enabled and be >= 3.5 for offload; older libraries use encrypted software TLS for the whole connection. Set `RACER_REQUIRE_KTLS=1` when testing on a capable host to require actual TX/RX offload.
- Run `make racer-fmt-check racer-test` for Go tests, both Rust crates' all-target tests, and separate doctests. Use `racer-controlplane-test` or `racer-dataplane-test` to select one crate, and `racer-rust-test-compile` to compile both suites without running them. Formatting explicitly checks included `tests/**/*.rs`. There is no `sim` feature. Use `RACER_REQUIRE_URING=1 RUST_TEST_THREADS=2` on capable Linux hosts and an external timeout for real-kernel tests; report unavailable prerequisites separately from passing coverage.
- `make racer-crosslang-test` builds the daemon and object adapter for Go SDK interoperability tests. `make racer-controlplane-live-test` builds both Rust binaries and runs the `e2e`-tagged production-binary campaign against a real API server: two control planes, three dataplanes, independent convergence, failover, storage resize/restart, and CA rotation through root retirement with continuous verified reads. Set `KUBEBUILDER_ASSETS`, workspace-local ext4 `TMPDIR`, and an existing workspace-local `RACER_LIVE_SOCKET_ROOT` whose absolute path is at most 36 bytes. Provide sufficient locked memory and physical cores plus passwordless sudo for private network/mount namespaces. Missing prerequisites fail the explicit live target. `make e2e-racer-compile` compiles the campaign without requiring its runtime prerequisites.
- Build Racer images with root context using `make image-racer-controlplane-local image-racer-dataplane-local`; `image-racer-loadgen-local` is test-only. Managed images are version-matched `ghcr.io/azure/racer-controlplane` and `ghcr.io/azure/racer-dataplane`. Runtime binaries are `/racer-controlplane` and `/racer-dataplane`, also available under `/usr/local/bin/` for operator shell commands. The dataplane image includes libibverbs and its providers.
- NOTICE discovers standalone `cmd/*/Cargo.toml` crates. Fetch each crate's locked dependencies before `make notice`; never hand-author generated notices.
- Net-specific build tasks (container images, frontend, eBPF, render) are exposed via `net-` prefixed targets in the main `Makefile` (e.g., `make net-frontend`, `make net-ebpf-build`, `make net-ebpf-generate`, `make net-manifests`). Cluster deploy/undeploy targets live separately under `hack/net/` and are invoked via `make -C hack/net <target>` (e.g., `make -C hack/net deploy`). Run `make help` and `make -C hack/net help` for the full lists.
- `make generate` runs `go generate ./...` to regenerate deepcopy, CRDs, and protobuf for all packages.
- `make build` compiles all Go packages (`go build ./...`).
- `make vulncheck` runs `govulncheck` and fails only on vulnerabilities that are both reachable from our code and have a published fix, since those are the ones a module bump resolves. Reachable ones with no fix available are reported and allowed through; acting on those means dropping or replacing the dependency, which is a judgment call rather than a build failure.
- `make fmt` formats Go source (gofumpt + wsl_v5 blank-line rules); `make lint` runs golangci-lint; `make test` runs all tests.
- `make lint` runs the same checks locally and in CI and does NOT auto-fix. Always run `make fmt` before committing to satisfy the linter (gofumpt and wsl_v5 are enforced by `make lint`/CI); do not hand-format.
- Locally `test` implies `lint`. In CI (`CI=1`), each runs independently.

## Coding Standards

- Do not cross cmd/ package boundaries. For example, `cmd/agent` should not import from `cmd/machina`. If you need to
  share code between these packages, put it in `internal/`.
- Do not use em-dashes (`—`) in comments, strings, or any source/config files. Use a plain ASCII hyphen (`-`)
  or rephrase the sentence instead.
- Write American English, not British. Use `behavior`, `initialize`, `labeled`, `catalog`, `defense`, `judgment`,
  not `behaviour`, `initialise`, `labelled`, `catalogue`, `defence`, `judgement`. This applies to comments, doc
  strings, identifiers, user-facing strings, and Markdown, in every language in the repo.
  `make lint` catches the common cases in Go via `misspell`, but its dictionary is not exhaustive: it misses
  `judgement` and `acknowledgement`, and it does not look at Rust, shell, TLA+, or Markdown at all. Treat it as
  a backstop, not the rule.
  `make fmt` runs `golangci-lint --fix`, so `misspell` rewrites Go sources in place. When a British spelling is
  deliberate, it needs an exclusion in `.golangci.yaml` or the next `make fmt` will silently undo it.
  Exceptions are external contracts only, such as the GitHub Actions `cancelled()` expression and the
  `LICENCE` filename patterns in `hack/cmd/notice` that match upstream third-party files.

## Testing Standards

- Add tests for new behavior. Cover success, failure, and edge cases.

## Sources of Truth

- Code is authoritative. Design docs (`designs/`), site docs (`docs/`), comments, commit messages, and PR or issue
  text describe intent and drift from the code over time. Treat them as leads to verify, not as evidence.
- Establish what the code does before reading what it is said to do: the implementation first, then its tests for the
  contract as actually enforced, then the prose for intent. Read the prose too; do not skip it, and do not trust it.
- Verify before asserting:
    - Read a test's assertions before citing it as a constraint. A test named for a resource may assert a floor
      ("must grant") rather than a ceiling ("must not grant").
    - Read the enclosing block, not the matched line. A container in a pod spec may be an init container; a flag
      default may be unreachable.
    - Confirm a symbol is reachable before assuming it takes effect. An `-X` ldflag on a package the binary never
      imports is silently ignored.
- Cite `file:line` for any claim about behavior that a decision rests on. If a claim cannot be cited, say it is an
  inference.
- When code and prose disagree, report both with citations rather than silently following either. The doc may be
  stale, or the code may be the bug, and which it is changes the work. Ask when the answer would change what gets
  built; offer to fix it when it is merely stale.

## Boundaries

- **Ask first**
    - Large cross-package refactors.
    - New dependencies with broad impact.
    - Destructive data or migration changes.
    - Removal of _test.go or Test* functions or subtests.
    - Proceeding when a design doc or comment contradicts the code.
- **Never**
    - Commit secrets, credentials, or tokens.
    - Edit generated files by hand when a generation workflow exists.
    - Use destructive git operations unless explicitly requested.
    - Go outside the project boundary, for example, DO NOT edit files in user's home directories, add or edit files 
      in /tmp or anywhere else on the host filesystem.

## Miscellaneous

- **DO NOT** give time or effort estimates for work in this project. For example, do not say "this is a half day 
  project" or "this will take a week". You are a computer. You are not a person with a concept of human scheduling and 
  time.
