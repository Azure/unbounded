# internal/helpers/

Small, stateless, generic helpers that two or more components share. This directory is not a Go package: each helper
is its own package under it, named for its concept. Never add Go files directly to `internal/helpers/`; `make lint`
fails if you do.

## What belongs here

All of these must be true:

- **Two or more components use it.** The root `AGENTS.md` defines a component, under `cmd/`. Code under `internal/` or
  `pkg/` counts for the component it belongs to, and code under `hack/` does not count. Count only direct imports by a
  component's own code, not imports that arrive through another component's package. To list the packages that import
  a helper:

  ```sh
  go list -f '{{range .Imports}}{{if eq . "github.com/Azure/unbounded/internal/helpers/<name>"}}{{$.ImportPath}}{{"\n"}}{{end}}{{end}}' ./...
  ```

  A helper only one component uses stays private to that component, and when a helper drops to one user, it moves
  into that user.
- **Generic.** It knows nothing about Machines, Sites, the operator or any other component's types or rules. If it
  needs them, it belongs to that component.
- **Small and stateless.** Functions and constants only: no types with behavior, no goroutines, no package-level mutable
  state, no `init`. The one exception is an explicit, idempotent registration with a third-party library, such as
  `dockermedia.RegisterParsers`, documented on the function.
- **A leaf.** It imports only the standard library and third-party modules, never anything from this repository.
  `make lint` enforces this.
- **Not a contract.** When two components must produce and check the same value, sharing a helper hides that contract.
  The producing component exports it instead (see `internal/AGENTS.md`).

When a helper outgrows these rules, it gets an owner: the component driving the growth, or `internal/<name>` if it has
become a project convention.

## Naming and API

- **Package:** one lowercase word naming the concept, such as `statusmsg`, `dockermedia` or `ociregistry`. Never
  `util`, `utils`, `common`, `misc`, `shared`, `types` or `helpers`, and no `util` suffix.
- **No stutter:** names read with the package name and do not repeat it. `statusmsg.Truncate`, not
  `statusmsg.TruncateStatusMessage`; `dockermedia.ConvertToOCI`, not `dockermedia.ConvertDockerMediaTypes`.
- **Export the behavior, not its settings.** Prefer a function that applies a limit over exporting the limit, unless
  callers need the value itself: `statusmsg.TruncateCondition(msg)`, with the limit unexported.
- **Verbs for functions that act** (`Truncate`, `ConfigurePlainHTTP`), `Is` or `Has` for predicates, and `Parse` and
  `Format` for conversions from and to strings.
- **Units in constant names:** `conditionMaxBytes`, not `conditionMax`. Durations are `time.Duration`, never an
  integer number of seconds.
- **Plain parameters,** at most three. Beyond that, take one `Options` struct. No functional options.
- **Errors:** sentinels are `Err<Condition>`, types are `<Condition>Error`. Messages are lowercase with no trailing
  punctuation.

## Documentation and tests

- A package comment stating the concept and why it is shared.
- A doc comment on every exported identifier, starting with its name. Document inputs that panic, as
  `statusmsg.Truncate` does for a negative limit.
- Tests for every exported function, covering limits and edge cases. Write new tests table-driven.
