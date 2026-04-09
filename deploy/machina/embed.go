// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package machina embeds the Machina controller deployment manifests so they
// can be bundled into binaries that need to apply them (e.g. the kubectl
// plugin). The manifests remain the single source of truth in this directory.
package machina

import "embed"

// Manifests embeds all YAML files in deploy/machina/ including the crd/
// subdirectory. Consumers can use fs.WalkDir or fs.ReadFile to access
// individual files.
//
//go:embed *.yaml crd/*.yaml
var Manifests embed.FS
