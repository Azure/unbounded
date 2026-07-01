// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package unbounded holds small cross-cutting helpers shared by the unbounded
// components. It deliberately has no dependencies (other than the standard
// library) so that any package (under cmd/, internal/, or hack/cmd/) can import
// it without risking an import cycle.
package unbounded

import "os"

// systemNamespace is the default Kubernetes namespace that unbounded components
// install into. It is the Go-side source of truth for binary defaults and
// fallbacks. The Makefile UNBOUNDED_NAMESPACE variable and the manifest template
// defaults (`{{ default "unbounded-system" .Namespace }}`) must agree with this
// value; that invariant is enforced by the drift-guard test in this package.
const systemNamespace = "unbounded-system"

// SystemNamespace returns the namespace unbounded components run in. When
// POD_NAMESPACE is set (injected into pods via the Downward API) it is used, so
// components follow whatever namespace they are deployed into; otherwise the
// default systemNamespace is returned. Client-side tools, which have no
// POD_NAMESPACE, therefore see the default.
func SystemNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}

	return systemNamespace
}
