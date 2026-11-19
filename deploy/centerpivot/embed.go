// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package centerpivot embeds the rendered centerpivot manifests for the
// unbounded operator.
package centerpivot

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
