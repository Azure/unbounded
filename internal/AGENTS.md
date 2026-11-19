# internal/

Shared code that no single component owns. Only this Go module can import it.

## What can go here

Only two kinds of package:

- **Conventions, `internal/<name>`:** something every component must do the same way, such as the build version
  (`version`) or the log format (`logger`). Ask before adding one.
- **Helpers, `internal/helpers/<name>`:** small generic functions that two or more components use. Read
  `internal/helpers/AGENTS.md` first.

## Where new code goes

Use the first rule that fits:

1. It is part of a CRD or other API type: `api/`.
2. One component uses it: keep it in that component.
3. Two components must agree on it, such as a message format, an annotation key or a validation rule: the component
   that writes it owns it, and the other imports it. Do not copy it.
4. Every component must agree on it and none owns it: `internal/<name>`.
5. It is a small generic function that two or more components use: `internal/helpers/<name>`.

If nothing fits, copy a short function into each component. Never copy something covered by rule 3.

## Rules for `internal/<name>`

- One convention per package, named for it. Not `util`, `common`, `helpers`, `misc`, `shared`, `types` or `kube`, and
  nothing ending in `util`.
- It may import only `api/`, `internal/helpers/`, the standard library and third-party modules.
- Its package comment says what the convention is and why every component follows it.

## Until #1175

Only `version`, `logger` and `helpers/` follow the rules above. Every other package here is component code that #1175
moves into `cmd/`. Until then:

- You may change those packages.
- Do not add packages, not even inside an existing tree such as `internal/gantry/`. Add the code to an existing
  package, or to a new package under `cmd/<component>/internal/`.
- Go does not let code in `internal/` import a package under `cmd/<component>/internal/`. If it needs your new
  package, its component has to move first.
- For rule 3, "the component that writes it" means one of that component's existing packages in `internal/`.
