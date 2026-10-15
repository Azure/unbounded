// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer embeds static Racer operator manifests.
package racer

import "embed"

// Manifests contains no generated CRDs. The operator builds the dataplane
// DaemonSets in Go; only their tuning ConfigMap and ServiceAccount are static.
//
//go:embed *.yaml
var Manifests embed.FS
