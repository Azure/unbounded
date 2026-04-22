// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package machina embeds the rendered Machina controller deployment manifests
// so they can be bundled into binaries that need to apply them (e.g. the
// kubectl plugin). The sources of truth are the *.yaml.tmpl files in this
// directory and the controller-gen generated CRDs under crd/; the rendered
// tree under rendered/ is produced by `make machina-manifests` and is
// gitignored.
package machina

import (
	"embed"
	"io/fs"
)

//go:embed rendered/*.yaml rendered/crd/*.yaml
var manifestsRaw embed.FS

// Manifests exposes the rendered manifests as a filesystem rooted at the
// rendered/ directory, so consumers see the familiar flat layout
// (e.g. "01-namespace.yaml", "crd/unbounded-kube.io_machines.yaml").
var Manifests = mustSub(manifestsRaw, "rendered")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}

	return sub
}
