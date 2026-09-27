// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer embeds the Racer manifests rendered by make racer-manifests.
// Templates and generated CRDs are the sources of truth; rendered is gitignored.
package racer

import (
	"embed"
	"io/fs"
)

//go:embed all:rendered
var manifestsRaw embed.FS

// Manifests contains the rendered controller resources and CRD.
var Manifests = mustSub(manifestsRaw, "rendered")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}

	return sub
}
