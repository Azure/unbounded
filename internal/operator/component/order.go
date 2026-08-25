// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

// tier is the stage an operation belongs to, inferred from what it writes
// rather than declared by the component that planned it.
//
// Ordering used to depend entirely on components declaring DependsOn. That put
// a correctness requirement on every component author, in a place where getting
// it wrong is invisible: a DaemonSet applied before its ConfigMap exists
// produces pods that cannot mount and crash-loop until the next pass happens to
// order them the other way. Nothing failed, so nothing was reported, and the
// symptom appears in the workload rather than in the operator.
//
// Most of these dependencies are a property of the Kubernetes object model
// rather than of any particular component: a namespaced object needs its
// Namespace, a custom resource needs its CRD, a pod needs the ServiceAccount it
// binds and the ConfigMaps and Secrets it mounts.
//
// tierActivation is the exception, and the reason this is a tier list rather
// than a dependency order read straight off the object model. Admission and
// aggregation registration points at a backend, so it must come *after* that
// backend rather than before it. An earlier revision of this file treated
// webhook configurations as schema and put them near the front, which reversed
// the order the manifests had always used and left a window where both net
// webhooks, which are failurePolicy: Ignore, silently passed everything
// through.
//
// DependsOn is still honored, and is still the way to express an ordering that
// does not follow from the kinds involved.
type tier int

const (
	// tierNamespace creates the namespaces every namespaced object lands in.
	tierNamespace tier = iota

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

	// tierWorkload creates the pods, once everything they need exists.
	tierWorkload

	// tierActivation registers admission webhooks, admission policies and
	// aggregated APIs. It runs last because every one of them routes traffic to
	// a backend that has to be running first. A registration whose backend does
	// not exist is worse than no registration: a failurePolicy: Ignore webhook
	// silently stops enforcing, and an APIService returns errors for a type the
	// cluster believes is served.
	tierActivation
)

// tierByKind maps the kinds the operator writes to their stage.
//
// Kinds are matched by name rather than by group so that a kind is placed
// correctly whether it arrives as a typed object or as unstructured YAML from a
// manifest, and so an unrecognized kind lands in tierInstance, after its CRD.
var tierByKind = map[string]tier{
	"Namespace": tierNamespace,

	"CustomResourceDefinition": tierSchema,
	"PriorityClass":            tierSchema,
	"StorageClass":             tierSchema,
	"CSIDriver":                tierSchema,
	"IngressClass":             tierSchema,
	"RuntimeClass":             tierSchema,

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

	// Everything that points at a backend. See tierActivation.
	"ValidatingWebhookConfiguration":   tierActivation,
	"MutatingWebhookConfiguration":     tierActivation,
	"ValidatingAdmissionPolicy":        tierActivation,
	"ValidatingAdmissionPolicyBinding": tierActivation,
	"MutatingAdmissionPolicy":          tierActivation,
	"MutatingAdmissionPolicyBinding":   tierActivation,
	"APIService":                       tierActivation,
}

// tierOf returns the stage a kind is created in, regardless of whether this
// operation creates or removes it.
func tierOf(op Operation) tier {
	if known, ok := tierByKind[op.Object.GetKind()]; ok {
		return known
	}

	// An unrecognized kind is treated as a custom resource, which is what it
	// almost always is here. That places it after CRDs, and before workloads
	// in case a workload consumes it.
	return tierInstance
}

// removalPhase and writePhase separate the two halves of a pass. Removals run
// first: components clean up before they build, and a removal never satisfies
// a dependency, so running them first cannot invalidate anything a later
// operation needs.
const (
	removalPhase = 0
	writePhase   = 100
)

// rankOf returns the position an operation executes at.
//
// Writes ascend the tiers. Removals descend them, which is the ordering the
// tier list implies read backwards: a workload is deleted before the ConfigMap
// it mounts, and a namespace is deleted last of all. An earlier revision gave
// every removal one rank, so a failed workload delete did not stop the delete
// of the config that workload was still mounting.
func rankOf(op Operation) int {
	at := tierOf(op)

	if op.Kind == OpDelete {
		return removalPhase + int(tierActivation) - int(at)
	}

	return writePhase + int(at)
}

// describeRank names what an operation was doing, for the message that explains
// why a later operation was skipped.
func describeRank(op Operation) string {
	at := tierOf(op).String()

	if op.Kind == OpDelete {
		return at + " removal"
	}

	return at
}

// String names a tier for the messages that explain why an operation was
// skipped.
func (t tier) String() string {
	switch t {
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
	case tierActivation:
		return "admission and API registration"
	default:
		return "unknown"
	}
}
