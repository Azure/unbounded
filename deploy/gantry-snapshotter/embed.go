// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package gantrysnapshotter embeds the rendered gantry-snapshotter manifests
// so they can be bundled into binaries that apply them (the
// unbounded-operator). The sources of truth are the *.yaml.tmpl files in this
// directory; the rendered tree under rendered/ is produced by
// `make gantry-snapshotter-manifests` and is gitignored.
//
// The `all:` prefix in the embed directive plus the tracked
// rendered/.gitignore placeholder keeps the directive satisfiable on a fresh
// clone, before `make gantry-snapshotter-manifests` has run, so `go build`,
// `go vet`, golangci-lint and gopls can load this package without the render
// step having happened first. The placeholder is harmless at runtime:
// consumers only apply *.yaml and *.yml files.
package gantrysnapshotter

import (
	"embed"
	"io/fs"
)

//go:embed all:rendered
var manifestsRaw embed.FS

// Manifests exposes the rendered manifests as a filesystem rooted at the
// rendered/ directory, so consumers see the familiar layout
// ("00-serviceaccount.yaml", "01-runtimeclass.yaml", "02-node-config.yaml",
// "03-daemonset.yaml").
//
// The numeric prefixes are the apply order and they matter. The runtime class
// has to exist before the DaemonSet that selects it, and the node-config agent
// has to be creating the containerd handler that class names before the
// snapshotter's pod can be scheduled at all.
var Manifests = mustSub(manifestsRaw, "rendered")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}

	return sub
}
