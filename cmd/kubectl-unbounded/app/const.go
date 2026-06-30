// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import "github.com/Azure/unbounded/internal/unbounded"

const (
	fieldManagerID = "kubectl-unbounded"
	// machinaNamespace is the default namespace for unbounded-operator and the
	// components it manages. It tracks the unified unbounded-system namespace.
	machinaNamespace = unbounded.SystemNamespace
)
