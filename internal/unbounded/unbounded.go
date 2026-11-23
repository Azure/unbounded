// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package unbounded defines the system namespace used by Unbounded components.
package unbounded

import "os"

// DefaultSystemNamespace is the namespace baked into the rendered manifests at
// build time and the default install namespace for unbounded components. It is
// the build-time literal that manifest namespace references carry
// (`{{ default "unbounded-system" .Namespace }}`), so tooling that retargets a
// manifest to a custom namespace must use this constant as the rewrite source,
// NOT SystemNamespace() (which reflects the caller's own POD_NAMESPACE and would
// leave build-time references unrewritten). The Makefile UNBOUNDED_NAMESPACE
// variable and the manifest template defaults must agree with this value; that
// invariant is enforced by the drift-guard test in this package.
const DefaultSystemNamespace = "unbounded-system"

// systemNamespace is the default Kubernetes namespace that unbounded components
// install into. It is the Go-side source of truth for binary defaults and
// fallbacks.
const systemNamespace = DefaultSystemNamespace

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
