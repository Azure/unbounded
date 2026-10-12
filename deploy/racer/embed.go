// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer embeds controller-only operator manifests.
package racer

import "embed"

// Manifests contains no dataplane workloads or generated CRDs.
//
//go:embed *.yaml
var Manifests embed.FS
