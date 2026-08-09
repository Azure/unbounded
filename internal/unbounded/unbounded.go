// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package unbounded holds small cross-cutting helpers shared by the unbounded
// components. It deliberately has no dependencies (other than the standard
// library) so that any package (under cmd/, internal/, or hack/cmd/) can import
// it without risking an import cycle.
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

const (
	// LegacyKubeNamespace is where machina, metalman, and storage ran before
	// the consolidation onto SystemNamespace().
	LegacyKubeNamespace = "unbounded-kube"

	// LegacyNetNamespace is where unbounded-net ran before the consolidation.
	LegacyNetNamespace = "unbounded-net"
)

// IsLegacyNamespace reports whether ns is one of the pre-consolidation
// namespaces the operator's migration reaper drains and deletes. Installing
// unbounded into one of these is unsafe: the reaper would delete the very
// namespace it just migrated into.
func IsLegacyNamespace(ns string) bool {
	return ns == LegacyKubeNamespace || ns == LegacyNetNamespace
}

const (
	// SystemNamespaceAppName is the canonical value of the app.kubernetes.io/name
	// label on the shared system namespace. Every component's manifest template
	// declares this single value (rather than a per-component name) so the
	// operator-owned namespace has one stable identity instead of a label that
	// churns as each component reapplies it.
	SystemNamespaceAppName = "unbounded-cloud"

	// SystemNamespaceManagedBy is the value of the app.kubernetes.io/managed-by
	// label on the shared system namespace.
	SystemNamespaceManagedBy = "unbounded-operator"

	// SystemNamespacePSAEnforce is the Pod Security Admission enforce level for
	// the shared system namespace. The privileged workloads that run here
	// (net-node hostNetwork, storage hostPath, gantry containerd socket and
	// /etc/containerd/certs.d hostPath) require the privileged level; a stricter
	// level would reject them at admission.
	SystemNamespacePSAEnforce = "privileged"

	// SystemNamespacePSAEnforceVersion pins the Pod Security Standard version the
	// enforce level is evaluated against. "latest" tracks the cluster's current
	// version; privileged is the most permissive level, so version drift is
	// low-risk.
	SystemNamespacePSAEnforceVersion = "latest"
)

// Well-known label keys stamped on the shared system namespace. They are the
// only keys the operator manages on the namespace; see SystemNamespaceLabels.
const (
	appNameLabelKey           = "app.kubernetes.io/name"
	appManagedByLabelKey      = "app.kubernetes.io/managed-by"
	psaEnforceLabelKey        = "pod-security.kubernetes.io/enforce"
	psaEnforceVersionLabelKey = "pod-security.kubernetes.io/enforce-version"
)

// SystemNamespaceLabels returns the canonical label set the operator stamps on
// the shared system namespace. It is the single source of truth shared by the
// operator's namespace bootstrap and the manifest-template drift guard, so the
// Go-side owner and the rendered templates cannot diverge. The operator applies
// these keys authoritatively (server-side apply, granular per-key ownership) and
// leaves every other label and annotation on the namespace untouched.
func SystemNamespaceLabels() map[string]string {
	return map[string]string{
		appNameLabelKey:           SystemNamespaceAppName,
		appManagedByLabelKey:      SystemNamespaceManagedBy,
		psaEnforceLabelKey:        SystemNamespacePSAEnforce,
		psaEnforceVersionLabelKey: SystemNamespacePSAEnforceVersion,
	}
}
