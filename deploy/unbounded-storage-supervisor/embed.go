// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package unboundedstoragesupervisor embeds the rendered unbounded-storage
// supervisor deployment manifests so they can be bundled into binaries that
// need to apply them (e.g. the kubectl plugin). The sources of truth are the
// *.yaml.tmpl files in this directory; the rendered tree under rendered/ is
// produced by `make unbounded-storage-supervisor-manifests` and is gitignored.
//
// The `all:` prefix in the embed directive plus the tracked rendered/.gitignore
// placeholder ensures the directive is satisfiable on a fresh clone (before the
// render target has run), so Go tooling can load this package without requiring
// the rendering step to have happened first. The placeholder file is harmless at
// runtime: consumers that materialise the FS only apply *.yaml/*.yml files.
package unboundedstoragesupervisor

import (
	"embed"
	"io/fs"
)

//go:embed all:rendered
var manifestsRaw embed.FS

// Manifests exposes the rendered manifests as a filesystem rooted at the
// rendered/ directory, so consumers see the familiar layout
// (e.g. "01-namespace.yaml", "04-daemonset.yaml").
var Manifests = mustSub(manifestsRaw, "rendered")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}

	return sub
}
