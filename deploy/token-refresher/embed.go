// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package tokenrefresher embeds the rendered token-refresher manifests for the
// unbounded operator.
package tokenrefresher

import (
	"embed"
	"io/fs"
)

//go:embed all:rendered
var manifestsRaw embed.FS

// Manifests exposes the rendered manifests rooted at rendered/.
var Manifests = mustSub(manifestsRaw, "rendered")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}

	return sub
}
