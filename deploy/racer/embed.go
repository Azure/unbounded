// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer embeds the rendered Racer manifests for the operator.
package racer

import (
	"embed"
	"io/fs"
)

//go:embed all:rendered
var manifestsRaw embed.FS

// Manifests is rooted at the rendered manifest directory.
var Manifests = manifestFS()

func manifestFS() fs.FS {
	sub, err := fs.Sub(manifestsRaw, "rendered")
	if err != nil {
		panic(err)
	}

	return sub
}
