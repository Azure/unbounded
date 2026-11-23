# internal/

Shared code that no single component owns.

## What can go here

Only two kinds of package:

- **Conventions, `internal/<name>`:** something every component must do the same way. The approved conventions are the
  build version (`version`), the log format (`logger`), the system namespace (`systemns`) and the bootstrap-token format
  (`bootstraptoken`). Ask before adding one.
- **Helpers, `internal/helpers/<name>`:** small functions or constants that two or more components use. Read
  `internal/helpers/AGENTS.md` first.

## Where new code goes

Use the first rule that fits:

1. It is part of a CRD or other API type: `api/`.
2. One component uses it: keep it in that component.
3. If one component writes an annotation and another reads it, the writer defines the key. Both use that definition.
   The same rule applies to message formats and shared validation rules: import the owner's definition, not a copy.
4. Every component must agree on it and none owns it: `internal/<name>`.
5. It is a small function or constant that two or more components use and none owns: `internal/helpers/<name>`.

If nothing fits, each component may keep a short private copy.

## Rules for `internal/<name>`

- One convention per package, named for it. Not `util`, `common`, `helpers`, `misc`, `shared`, `types` or `kube`, and
  nothing ending in `util`.
- It may import only `api/`, `internal/helpers/`, the standard library and third-party modules.
- Its package comment says what the convention is and why every component follows it.

## Until #1175

You may edit existing component packages such as `internal/gantry`. Put new private packages in
`cmd/<component>/internal/`. Export shared definitions from the owning component's existing package under `internal/`.
