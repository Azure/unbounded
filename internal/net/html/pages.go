// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package html embeds HTML page templates used by the controller and node binaries.
package html

import (
	"embed"
	"io/fs"
)

// ClusterStatusAssets contains the built frontend assets.
//
//go:embed dist/*
var ClusterStatusAssets embed.FS

// ClusterStatusFS returns the embedded assets rooted at dist.
func ClusterStatusFS() (fs.FS, error) {
	return fs.Sub(ClusterStatusAssets, "dist")
}

// ClusterStatusIndex returns the HTML entrypoint.
func ClusterStatusIndex() ([]byte, error) {
	return ClusterStatusAssets.ReadFile("dist/index.html")
}
