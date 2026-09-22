// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer embeds the generated Racer API definitions for operator bootstrap
// and continuous CRD drift repair.
package racer

import "embed"

//go:embed crd/*.yaml
var Manifests embed.FS
