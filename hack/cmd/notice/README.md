# notice

Generates and verifies the project's `NOTICE` file from the direct dependencies
declared in `go.mod` and `frontend/package.json`.

## Usage

```
notice generate [--repo-root .] [--output NOTICE]
notice check    [--repo-root .] [--notice NOTICE]
```

The Makefile wraps these as `make notice` and `make notice-check`.

## Output schema

The on-disk `NOTICE` is YAML preceded by a generated-file header comment. Each
direct dependency contributes one entry:

```yaml
- dependency: github.com/spf13/cobra      # Module path or npm package name.
  ecosystem: go                           # Stable Collector.Name() value.
  copyright:                              # Lines extracted from LICENSE/NOTICE/AUTHORS.
    - Copyright (c) 2013 Steve Francia
  license:                                # One per classified license name.
    - name: Apache License, Version 2.0
      link: https://github.com/spf13/cobra/blob/v1.10.2/LICENSE.txt
```

Entries are sorted alphabetically by `dependency` regardless of ecosystem.

## Architecture

```
hack/cmd/notice/
  main.go                  # CLI: subcommands, flags, explicit collector slice.
  internal/
    notice/                # Core: Document, Entry, Collector iface, Build/Render/Diff,
                           # AssembleEntry helper.
    license/               # Ecosystem-agnostic helpers: Classify, FindFile,
                           # ExtractCopyright[FromDir], BuildURL, SPDXFriendly.
    gomod/                 # Collector for go.mod direct deps; Go vanity-domain
                           # repo-base heuristics.
    npm/                   # Collector for frontend/package.json direct deps.
    testutil/              # WriteTree + canonical license-text fixtures.
```

## Adding a new ecosystem

To add a new ecosystem (e.g. PyPI, Cargo):

1. Create `internal/<name>/` with a `Collector` implementation:

   ```go
   type Collector struct { /* injectable runner/fs */ }
   func New(opts ...Option) *Collector { ... }
   func (c *Collector) Name() string                          // "pypi", "cargo", ...
   func (c *Collector) Precheck(root string) error            // verify host setup
   func (c *Collector) Collect(root string) ([]notice.Entry, error)
   ```

2. Reuse the shared helpers wherever possible:

   - `notice.AssembleEntry(name, ecosystem, dir, repoBase, ref, declaredLicense)`
     wraps license discovery, classification, copyright extraction, and URL
     construction. Most collectors only need to compute `(dir, repoBase, ref)`
     for each direct dependency and call this.
   - `license.Classify`, `license.ExtractCopyright[FromDir]`, `license.FindFile`,
     `license.BuildURL`, and `license.SPDXFriendly` are ecosystem-agnostic.

3. Write hermetic tests using `testutil.WriteTree` to materialize fixture
   trees under `t.TempDir()`. Use the shared canonical license bodies
   (`testutil.MITLicense`, `testutil.Apache2License`, `testutil.BSD3License`)
   so the classifier sees realistic input. Inject fakes for any external
   tool the collector needs to invoke (`go`, `pip`, `cargo`); see
   `internal/gomod/gomod_test.go` for the pattern.

4. Append `<name>.New()` to the `collectors()` slice in `main.go`. No other
   files need editing.

## Conventions

- `Collect` MUST NOT sort its result. `notice.Build` performs a single global
  sort by `Dependency`.
- Do not commit fake `node_modules/`, module-cache, or `site-packages/` trees.
  Always materialize fixtures dynamically in tests via `testutil.WriteTree`.
- License URL forge dispatch (GitHub, GitLab, cs.opensource.google, Bitbucket)
  lives in `license.BuildURL` as a switch on URL prefix. Add a case here when a
  new forge is needed.
- Copyright extraction returns the placeholder `"See LICENSE file"` when no
  attribution can be discovered in LICENSE / NOTICE / AUTHORS / CONTRIBUTORS /
  COPYRIGHT files. Source-file headers are intentionally not scanned.
