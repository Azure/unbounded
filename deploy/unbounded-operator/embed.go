// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package unboundedoperator embeds the rendered unbounded-operator deployment
// manifests so they can be bundled into binaries that bootstrap a cluster.
package unboundedoperator

import (
	"embed"
	"io/fs"
)

//go:embed all:rendered
var manifestsRaw embed.FS

// Manifests exposes the rendered manifests as a filesystem rooted at rendered/.
var Manifests = mustSub(manifestsRaw, "rendered")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}

	return sub
}
