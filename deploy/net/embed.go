// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package net embeds the rendered unbounded-net controller and node deployment
// manifests so they can be bundled into binaries that need to apply them
// (e.g. the kubectl plugin). The sources of truth are the *.yaml.tmpl files
// in this directory and the controller-gen generated CRDs under crd/; the
// rendered tree under rendered/ is produced by `make net-render-manifests`
// and is gitignored.
package net

import (
	"embed"
	"io/fs"
)

//go:embed rendered/*.yaml rendered/controller/*.yaml rendered/node/*.yaml rendered/crd/*.yaml
var manifestsRaw embed.FS

// Manifests exposes the rendered manifests as a filesystem rooted at the
// rendered/ directory, so consumers see the familiar layout
// (e.g. "00-namespace.yaml", "controller/03-deployment.yaml",
// "crd/net.unbounded-kube.io_sites.yaml").
var Manifests = mustSub(manifestsRaw, "rendered")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}

	return sub
}
