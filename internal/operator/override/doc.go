// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package override implements user-supplied customization of the Deployments
// and DaemonSets the operator generates.
//
// # Security
//
// Write access to the overrides ConfigMap is equivalent to root on every node
// in every affected Site, and therefore to cluster-admin. The workloads being
// patched already run with hostNetwork, hostPID, privileged containers and
// hostPath mounts of the host root, so changing a container image, its
// arguments, its environment, or adding a sidecar is arbitrary code execution
// on every node. Rejecting privileged: true or new hostPath volumes would
// achieve nothing against pods already in that state.
//
// The restrictions in this package are therefore integrity controls, not a
// privilege boundary. They stop an authorized operator from accidentally
// severing the operator's ability to reconcile a workload: detaching it from
// its selector, its ServiceAccount, or its per-Site node affinity.
//
// Two exceptions are genuine security controls, because they escape the
// workload rather than merely damaging it: the group, version and kind of the
// applied object, since the operator holds escalate and bind on
// ClusterRoleBindings; and serviceAccountName, since retargeting it borrows
// another identity's API permissions.
package override

// ConfigMapName is the ConfigMap the operator reads overrides from.
//
// The operator only ever reads it: it is never created, never seeded and never
// written, so an absent ConfigMap unambiguously means "no overrides" rather
// than "not yet initialized".
const ConfigMapName = "unbounded-component-overrides"

// APIVersion is the only document version this release accepts. It gates the
// schema so the format can evolve without silently reinterpreting documents
// written against an older shape.
const APIVersion = "overrides.unbounded-cloud.io/v1alpha1"

// Document is one overrides document. Each key in the ConfigMap holds exactly
// one of these.
type Document struct {
	APIVersion string  `yaml:"apiVersion"`
	Overrides  []Entry `yaml:"overrides"`
}

// Entry targets one workload, or one workload per Site, and describes what to
// change about it.
type Entry struct {
	// Component names the component that generates the workload: net, machina,
	// gantry, metalman or storage.
	Component string `yaml:"component"`

	// Kind is Deployment or DaemonSet. Together with Component it identifies
	// every workload the operator emits today, so users never have to
	// reconstruct a derived per-Site name.
	Kind string `yaml:"kind"`

	// Sites selects which Sites to affect, for per-Site components only.
	//
	// A nil slice means every Site. An explicitly empty slice is a validation
	// error, because it is far more likely to be a mistake than an intent to
	// match nothing. Naming a Site that does not exist is inert and reported,
	// not fatal: a document may legitimately be written before its Site.
	Sites []string `yaml:"sites,omitempty"`

	// AddContainers and AddInitContainers name containers this entry intends to
	// create rather than modify.
	//
	// Strategic merge cannot tell a sidecar from a typo: both are "this name is
	// not present", and merging by name silently creates a container either
	// way. Entries are therefore modify-only unless a name is listed here, so a
	// patch meaning to raise a limit on machina-controller but spelling it
	// machina-contoller fails instead of adding an image-less container.
	AddContainers     []string `yaml:"addContainers,omitempty"`
	AddInitContainers []string `yaml:"addInitContainers,omitempty"`

	// ExtraArgs appends arguments to a container, keyed by container name.
	//
	// It exists because args and command carry no patchMergeKey, so strategic
	// merge replaces them wholesale: a patch adding one flag drops every
	// operator-injected flag and will not receive new ones on upgrade. metalman
	// makes that concrete, since its args begin with the serve-pxe subcommand.
	ExtraArgs map[string][]string `yaml:"extraArgs,omitempty"`

	// Patch is a strategic merge patch applied to the whole workload object, so
	// metadata, spec.replicas and the pod template are all reachable through
	// one field.
	Patch map[string]any `yaml:"patch,omitempty"`
}

// HasWork reports whether the entry asks for any change at all.
func (e Entry) HasWork() bool {
	return len(e.Patch) > 0 || len(e.ExtraArgs) > 0
}
