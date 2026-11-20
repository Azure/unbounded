# internal/helpers/

Small helpers that two or more components share. Each helper is its own package, such as
`internal/helpers/statusmsg`.

## What belongs here

A helper must meet all of these:

- **Two or more components import it directly.** Two packages from the same component count once. Count packages in
  `internal/` and `pkg/` toward their owner. Ignore `hack/`. If only one component uses the helper, move it there.
- Helpers may use public types from `api/`. Keep component-specific decisions in the component that owns them.
- **Small.** Functions and constants only. No types with methods, goroutines, global variables or `init`. One
  exception: a function that registers something with a third-party library, if calling it twice is safe and its
  comment says so, such as `dockermedia.RegisterParsers`.
- **Not an agreement between components.** If one component writes a value and another checks it, the writer owns that
  code (rule 3 in `internal/AGENTS.md`). Do not put it here.

If a helper stops meeting these rules, move it into the component that needs the change, or make it a convention
(`internal/<name>`; ask first).

### Find direct users

Replace `<name>` with the helper's package name:

```sh
go list -f '{{range .Imports}}{{if eq . "github.com/Azure/unbounded/internal/helpers/<name>"}}{{$.ImportPath}}{{"\n"}}{{end}}{{end}}' ./...
```

## Naming and API

- **Package name:** one lowercase word for the concept, such as `statusmsg`, `dockermedia` or `ociregistry`. Not
  `util`, `utils`, `common`, `misc`, `shared`, `types` or `helpers`, and nothing ending in `util`.
- **Don't repeat the package name:** `statusmsg.Truncate`, not `statusmsg.TruncateStatusMessage`.
- **Export the function, not its limit:** `statusmsg.TruncateCondition(msg)`, with the limit unexported, unless
  callers need the number.
- **Function names:** a verb for functions that do something (`Truncate`, `ConfigurePlainHTTP`), `Is` or `Has` for
  ones that answer yes or no, and `Parse` and `Format` for converting from and to strings.
- **Units in constant names:** `conditionMaxBytes`, not `conditionMax`. Use `time.Duration` for durations, never an
  integer number of seconds.
- **Parameters:** at most three. Beyond that, take one `Options` struct. No functional options.
- **Error types:** `<Condition>Error`.

## Documentation and tests

- A package comment that says what the helper is for and why components share it.
- A doc comment on every exported name, starting with the name. Say which inputs panic, as `statusmsg.Truncate` does
  for a negative limit.
- Tests for every exported function, including limits and edge cases. Write new tests table-driven.
