// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package crd embeds the generated Racer API definitions for operator bootstrap.
package crd

import "embed"

// Manifests contains the authoritative generated ClusterCache CRD.
//
//go:embed *.yaml
var Manifests embed.FS
