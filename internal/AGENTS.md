# internal/

Code shared by more than one component that no single component owns. Go only lets packages inside this module import
it.

## What belongs here

Two kinds of package, and nothing else:

- **Project conventions: `internal/<name>`.** Something every component must agree on and no component owns, such as
  the build version (`version`) or the log format (`logger`). Ask before adding one.
- **Small generic helpers: `internal/helpers/<name>`.** Read `internal/helpers/AGENTS.md` before adding or changing
  one.

## Where new code goes

Check in order and stop at the first match:

1. It is part of a CRD or API type: `api/`, however many components use it.
2. Only one component uses it: it stays in that component.
3. Two components must agree on it (a wire type, an annotation key, a validation rule both sides apply): the component
   that produces it exports it, and the other imports it. Never copy it. Until #1175, that means one of the producing
   component's existing packages under `internal/`; afterward, an exported package under `cmd/<component>/`.
4. Every component must agree on it and none owns it: `internal/<name>`.
5. It is a small, stateless, generic function or constant that two or more components use: `internal/helpers/<name>`.

If nothing fits, a short private copy in each component is acceptable, except for anything covered by step 3.

## Rules for `internal/<name>`

- One convention per package, named for it. Never `util`, `common`, `helpers`, `misc`, `shared`, `types` or `kube`,
  and no `util` suffix.
- Imports only `api/`, `internal/helpers/`, the standard library and third-party modules. Never `cmd/`, `pkg/` or a
  component's code.
- The package comment states the convention and why components must agree on it.

## Everything else here today

Only `version`, `logger` and `helpers/` follow these rules so far. Every other package under `internal/` is component
code, or shared code still waiting for an owner, and #1174 and #1175 move it out.

Change those packages where they are, but do not add packages under `internal/`. That includes sub-packages inside an
existing component's tree, such as `internal/gantry/<new>`. Instead:

- Add the code to an existing package, or
- Put it in a new package under `cmd/<component>/internal/`, if nothing outside that `cmd/` tree needs it.

If component code under `internal/` needs the new package, move that component first (#1175).
