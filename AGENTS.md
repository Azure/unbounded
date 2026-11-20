# Project Overview

Project Unbounded is an open source initiative to enable Kubernetes users to run worker Nodes anywhere and connect them 
back a running control plane. This allows you to run workloads in any environment, including on-premises, in the cloud, 
and at the edge, without being limited by the location of your control plane.

## Repository Structure

This section says what each top-level directory is for. Look in the directory to see what it contains.

- `api/` - API types: Kubernetes CRD types and gRPC protobuf schemas, one directory per group and version, such as
  `api/machina/v1alpha3/`. Run `make generate` after changing them.
- `bin/` - build output.
- `bpf/` - eBPF C programs for network encapsulation (compiled with clang).
- `clients/` - client libraries in other languages, such as the Python Racer client and vLLM plugin in
  `clients/python/`.
- `cmd/` - one directory per component. See "Components" below.
- `demo/` - example environment setups, such as `demo/nebius-ssh/`.
- `deploy/` - Kubernetes manifests, one directory per component or plugin, such as `deploy/machina/`. `make help`
  lists the `-manifests` targets that render them.
- `designs/` - design documents and internal planning.
- `docs/` - the public web site only. Do not put design documents or internal notes here.
- `e2e/` - end-to-end tests, one directory per suite. Build them with `-tags e2e`.
- `frontend/` - React/TypeScript web UI for network topology (built with Vite).
- `hack/` - development tools and scripts. These are not components.
  - `cmd/` - Go development tools, such as forge and `render-manifests`.
  - `scripts/` - shell scripts.
  - `scratch/` - space for quick Go experiments.
- `images/` - container image definitions.
- `internal/` - shared code that no single component owns. Read `internal/AGENTS.md` before adding anything here.
- `pkg/` - Go libraries that other repositories import, such as `pkg/racersdk`. Export only what they need.
- `plugins/` - integrations that are separate Go modules. `plugins/karpenter/` must not import the root module. It
  keeps its own copy of the API fields it uses, and its tests check them against the generated CRDs.
- `tmp/` - temporary files. Use it instead of `/tmp`.

### Components

A component is one directory under `cmd/`, such as `cmd/machina` or `cmd/racer`. It can hold Go, Rust or both. The
network binaries (`cmd/unbounded-net-*`, `cmd/unbounded-ipam`, `cmd/unping`, `cmd/unroute`) count as one component.

Some component code still lives in `internal/` and `pkg/`. For example, `internal/gantry` belongs to `cmd/gantry`.
#1175 moves it into `cmd/`.

## Go

Keep component code private by default. To share code, follow
[Where new code goes](internal/AGENTS.md#where-new-code-goes).

- Run `make fmt` before committing. `make test` runs lint and tests locally. `make build` compiles the root Go module.
- Use `make help` for component targets and `make -C hack/net help` for network deployment commands.
- Karpenter has its own Go module. Use `make karpenter-fmt` to format it and `make karpenter` to lint, test and build
  it. Do not run `golangci-lint fmt` inside `plugins/`: it reformats the whole repository.

## Rust

- Keep Rust code and its test fixtures with the component that owns them.
- Use `make help` to find formatting, Clippy, and test targets for the component you changed. If there is no target,
  use the crate's own build or test instructions.

## Coding Standards

- Do not use em-dashes (`—`) in comments, strings, or any source/config files. Use a plain ASCII hyphen (`-`)
  or rephrase the sentence instead.
- Write American English, not British. Use `behavior`, `initialize`, `labeled`, `catalog`, `defense`, `judgment`,
  not `behaviour`, `initialise`, `labelled`, `catalogue`, `defence`, `judgement`. This applies to comments, doc
  strings, identifiers, user-facing strings, and Markdown, in every language in the repo.
  `make lint` catches the common cases in Go via `misspell`, but its dictionary is not exhaustive: it misses
  `judgement` and `acknowledgement`, and it does not look at shell or Markdown at all. Treat it as
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
    - Adding a project-wide convention in `internal/<name>`.
    - Proceeding when a design doc or comment contradicts the code.
- **Never**
    - Commit secrets, credentials, or tokens.
    - Edit generated files by hand when a generation workflow exists.
    - Use destructive git operations unless explicitly requested.
    - Add a package under `internal/` unless it is an approved convention or a helper in `internal/helpers/<name>`.
    - Go outside the project boundary, for example, DO NOT edit files in user's home directories, add or edit files 
      in /tmp or anywhere else on the host filesystem.

## Miscellaneous

- **DO NOT** give time or effort estimates for work in this project. For example, do not say "this is a half day 
  project" or "this will take a week". You are a computer. You are not a person with a concept of human scheduling and 
  time.
