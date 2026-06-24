// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package machineops embeds the rendered machine-ops-controller deployment
// manifests so they can be bundled into binaries that need to apply them
// (e.g. the kubectl plugin). The sources of truth are the *.yaml.tmpl files in
// this directory; the rendered tree under rendered/ is produced by
// `make machine-ops-manifests` and is gitignored.
//
// The `all:` prefix in the embed directive plus the tracked
// rendered/.gitignore placeholder ensures the directive is satisfiable on a
// fresh clone (before `make machine-ops-manifests` has run), so Go tooling
// (`go build`, `go vet`, golangci-lint, gopls, ...) can load this package
// without requiring the rendering step to have happened first. The
// placeholder file is harmless at runtime: consumers that materialise the
// FS only apply *.yaml/*.yml files.
package machineops

import (
	"embed"
	"io/fs"
)

//go:embed all:rendered
var manifestsRaw embed.FS

// Manifests exposes the rendered manifests as a filesystem rooted at the
// rendered/ directory, so consumers see the familiar flat layout
// (e.g. "00-namespace.yaml", "03-deployment.yaml").
var Manifests = mustSub(manifestsRaw, "rendered")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}

	return sub
}
