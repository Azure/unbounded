// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package unboundedstoragesupervisor embeds the rendered supervisor manifests.
package unboundedstoragesupervisor

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
