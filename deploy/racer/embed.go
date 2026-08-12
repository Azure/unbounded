// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package racer embeds the rendered racer node manifests so they can be
// bundled into binaries that apply them (the unbounded-operator). The sources
// of truth are the *.yaml.tmpl files in this directory; the rendered tree
// under rendered/ is produced by `make racer-manifests` and is gitignored.
//
// The `all:` prefix in the embed directive plus the tracked
// rendered/.gitignore placeholder keeps the directive satisfiable on a fresh
// clone, before `make racer-manifests` has run, so `go build`, `go vet`,
// golangci-lint and gopls can load this package without the render step
// having happened first. The placeholder is harmless at runtime: consumers
// only apply *.yaml and *.yml files.
package racer

import (
	"embed"
	"io/fs"
)

//go:embed all:rendered
var manifestsRaw embed.FS

// Manifests exposes the rendered manifests as a filesystem rooted at the
// rendered/ directory, so consumers see the familiar layout ("00-rbac.yaml",
// "01-csidriver.yaml", "02-policy.yaml", "03-daemonset.yaml").
var Manifests = mustSub(manifestsRaw, "rendered")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}

	return sub
}
