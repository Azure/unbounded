// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

// tier is the execution stage an operation belongs to, inferred from what it
// writes rather than declared by the component that planned it.
//
// Ordering used to depend entirely on components declaring DependsOn. That put
// a correctness requirement on every component author, in a place where getting
// it wrong is invisible: a DaemonSet applied before its ConfigMap exists
// produces pods that cannot mount and crash-loop until the next pass happens to
// order them the other way. Nothing failed, so nothing was reported, and the
// symptom appears in the workload rather than in the operator.
//
// The dependencies are a property of the Kubernetes object model, not of any
// particular component: a namespaced object needs its Namespace, a custom
// resource needs its CRD, a pod needs the ServiceAccount it binds and the
// ConfigMaps and Secrets it mounts. Inferring them from the kind makes the
// ordering true by construction for every component, present and future,
// including ones that never think about ordering at all.
//
// DependsOn is still honoured, and is still the way to express an ordering that
// does not follow from the kinds involved.
type tier int

const (
	// tierRemoval runs first. Components clean up before they build: gantry
	// removes its legacy node config before applying its current form, and a
	// removal never satisfies a dependency, so running removals first cannot
	// invalidate anything a later tier needs.
	tierRemoval tier = iota

	// tierNamespace creates the namespaces every namespaced object below
	// lands in.
	tierNamespace

	// tierSchema establishes types and cluster-wide policy that admission
	// consults: CRDs must exist before their instances, and a PriorityClass or
	// StorageClass must exist before a workload references it.
	tierSchema

	// tierIdentity creates ServiceAccounts and the roles and bindings that
	// grant them permission. A pod that starts before its RBAC exists gets
	// forbidden errors from the API server rather than a clean failure.
	tierIdentity

	// tierConfig creates the ConfigMaps, Secrets and Services a workload
	// mounts, projects or resolves.
	tierConfig

	// tierInstance creates custom resources, once tierSchema has established
	// their types.
	tierInstance

	// tierWorkload runs last, when everything a pod needs already exists.
	tierWorkload
)

// tierByKind maps the kinds the operator writes to their stage.
//
// Kinds are matched by name rather than by group so that a kind is placed
// correctly whether it arrives as a typed object or as unstructured YAML from a
// manifest, and so an unrecognised kind lands in tierInstance, after its CRD.
var tierByKind = map[string]tier{
	"Namespace": tierNamespace,

	"CustomResourceDefinition":       tierSchema,
	"PriorityClass":                  tierSchema,
	"StorageClass":                   tierSchema,
	"CSIDriver":                      tierSchema,
	"IngressClass":                   tierSchema,
	"RuntimeClass":                   tierSchema,
	"ValidatingWebhookConfiguration": tierSchema,
	"MutatingWebhookConfiguration":   tierSchema,

	"ServiceAccount":     tierIdentity,
	"ClusterRole":        tierIdentity,
	"ClusterRoleBinding": tierIdentity,
	"Role":               tierIdentity,
	"RoleBinding":        tierIdentity,

	"ConfigMap":             tierConfig,
	"Secret":                tierConfig,
	"Service":               tierConfig,
	"PersistentVolume":      tierConfig,
	"PersistentVolumeClaim": tierConfig,
	"PodDisruptionBudget":   tierConfig,

	"DaemonSet":   tierWorkload,
	"Deployment":  tierWorkload,
	"StatefulSet": tierWorkload,
	"ReplicaSet":  tierWorkload,
	"Job":         tierWorkload,
	"CronJob":     tierWorkload,
	"Pod":         tierWorkload,
}

// tierOf returns the stage an operation runs in.
func tierOf(op Operation) tier {
	if op.Kind == OpDelete {
		return tierRemoval
	}

	if known, ok := tierByKind[op.Object.GetKind()]; ok {
		return known
	}

	// An unrecognised kind is treated as a custom resource, which is what it
	// almost always is here. That places it after CRDs, and before workloads
	// in case a workload consumes it.
	return tierInstance
}

// String names a tier for the messages that explain why an operation was
// skipped.
func (t tier) String() string {
	switch t {
	case tierRemoval:
		return "removal"
	case tierNamespace:
		return "namespace"
	case tierSchema:
		return "schema"
	case tierIdentity:
		return "identity"
	case tierConfig:
		return "config"
	case tierInstance:
		return "custom resource"
	case tierWorkload:
		return "workload"
	default:
		return "unknown"
	}
}
