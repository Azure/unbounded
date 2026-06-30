// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package unbounded holds small cross-cutting constants shared by the
// unbounded components. It deliberately has no dependencies so that any
// package (under cmd/, internal/, or hack/cmd/) can import it without risking
// an import cycle.
package unbounded

// SystemNamespace is the default Kubernetes namespace that unbounded
// components install into.
//
// It is the Go-side source of truth for binary defaults and fallbacks. The
// Makefile UNBOUNDED_NAMESPACE variable and the manifest template defaults
// (`{{ default "unbounded-system" .Namespace }}`) must agree with this value;
// that invariant is enforced by the drift-guard test in this package.
const SystemNamespace = "unbounded-system"
