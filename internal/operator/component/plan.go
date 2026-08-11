// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// OpKind is what the executor does with an Operation's object.
//
// Reconciliation is not reducible to "produce objects, apply them": components
// also create ConfigMaps only when absent so user payloads survive, adopt
// existing objects by patching owner references under optimistic lock, merge
// operator-owned keys into user-owned config content, and delete objects that
// should no longer exist. A plan carries the intent, and the executor is the
// only thing that writes.
type OpKind int

const (
	// OpApply server-side applies the object, taking ownership of every field
	// it declares. This is the default for operator-owned objects.
	OpApply OpKind = iota

	// OpCreateIfAbsent creates Object only when nothing exists at its key, and
	// never overwrites an existing payload. Used for component ConfigMaps,
	// whose contents are a documented user escape hatch. An AlreadyExists race
	// is success: whoever won owns the payload.
	OpCreateIfAbsent

	// OpMergePatch patches from Base to Object under optimistic lock, leaving
	// every field neither mentions untouched. Base carries the observed state
	// the plan was computed from, including its resourceVersion, so a
	// concurrent edit produces a conflict and the pass retries rather than
	// silently clobbering. Used for owner-reference adoption and for merging
	// operator-owned keys into user-owned config content.
	OpMergePatch

	// OpDelete removes the object, treating absence as success.
	OpDelete
)

// String renders an OpKind for error messages and test failures.
func (k OpKind) String() string {
	switch k {
	case OpApply:
		return "Apply"
	case OpCreateIfAbsent:
		return "CreateIfAbsent"
	case OpMergePatch:
		return "MergePatch"
	case OpDelete:
		return "Delete"
	default:
		return fmt.Sprintf("OpKind(%d)", int(k))
	}
}

// ObjectRef identifies an object for dependency declarations. It is comparable,
// so it works as a map key.
type ObjectRef struct {
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string
}

// String renders an ObjectRef for error messages, in a shape that reads well in
// a Site condition.
func (r ObjectRef) String() string {
	if r.Namespace == "" {
		return fmt.Sprintf("%s/%s", r.GVK.Kind, r.Name)
	}

	return fmt.Sprintf("%s/%s/%s", r.GVK.Kind, r.Namespace, r.Name)
}

// RefOf returns the ObjectRef identifying obj.
func RefOf(obj *unstructured.Unstructured) ObjectRef {
	return ObjectRef{
		GVK:       obj.GroupVersionKind(),
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
}

// Operation is one unit of desired state a component wants written.
type Operation struct {
	Kind   OpKind
	Object *unstructured.Unstructured

	// Base is the observed state for OpMergePatch, and is ignored otherwise.
	// The executor computes the patch from Base to Object.
	Base *unstructured.Unstructured

	// Component names the component that planned this operation, and Site the
	// Site it was planned for. Site is empty for cluster-scoped operations.
	// Both are used to attribute results back to a Site condition.
	Component string
	Site      string

	// Overridable marks the workloads user-supplied overrides may target. Only
	// these are merge candidates, and only these are dropped when an override
	// document fails preflight, so an override typo cannot stop RBAC, Services
	// or ConfigMaps from reconciling.
	Overridable bool

	// SharedKey, when non-empty, identifies an operation that is identical
	// across Sites and must execute once per pass. Per-Site planning otherwise
	// re-applies shared support objects once per Site.
	SharedKey string

	// DependsOn declares ordering. The executor runs dependencies first and
	// skips dependents when a dependency fails, so a failure does not leave a
	// workload pointing at a ConfigMap that was never written.
	DependsOn []ObjectRef
}

// Ref returns the ObjectRef identifying this operation's object.
func (o Operation) Ref() ObjectRef { return RefOf(o.Object) }

// Plan is the set of operations a component wants executed for one pass.
//
// Planning may read cluster state, because decisions like "does this ConfigMap
// already exist" and "is this singleton retained" depend on it. Planning must
// not write: that is the executor's job, and it is what makes preflight
// validation atomic.
type Plan struct {
	Operations []Operation
}

// Add appends operations to the plan. A nil plan receiver is not valid; use
// NewPlan.
func (p *Plan) Add(ops ...Operation) {
	p.Operations = append(p.Operations, ops...)
}

// Merge appends every operation from other. A nil other is a no-op, so callers
// can merge the result of a planner that returned no work.
func (p *Plan) Merge(other *Plan) {
	if other == nil {
		return
	}

	p.Operations = append(p.Operations, other.Operations...)
}

// Len reports the number of operations in the plan, treating a nil plan as
// empty so callers can test emptiness without a nil check.
func (p *Plan) Len() int {
	if p == nil {
		return 0
	}

	return len(p.Operations)
}

// NewPlan returns an empty plan.
func NewPlan() *Plan { return &Plan{} }

// ExecutionOrder renders the order the executor will run this plan in, one
// operation per line, without running any of it.
//
// Summary renders the order components emitted their operations in; this
// renders what actually happens. The distinction cost real defects: execution
// order was changed while every golden plan test kept passing, because the
// executor sorts a copy and Summary reads the original. Components pin what
// they intend to write with Summary; this pins what the cluster sees.
func (p *Plan) ExecutionOrder() (string, error) {
	if p.Len() == 0 {
		return "", nil
	}

	ops := make([]Operation, len(p.Operations))
	copy(ops, p.Operations)
	sortOperations(ops)

	deduped, err := dedupeShared(ops)
	if err != nil {
		return "", err
	}

	ordered, err := orderByDependency(byRank(deduped))
	if err != nil {
		return "", err
	}

	var b strings.Builder

	for _, op := range ordered {
		b.WriteString(op.Kind.String())
		b.WriteString(" ")
		b.WriteString(op.Ref().String())
		b.WriteString("\n")
	}

	return b.String(), nil
}

// Summary renders the plan as one line per operation, in plan order.
//
// It exists so tests can pin exactly what a component intends to write, and so
// a plan can be logged or displayed without dumping whole objects. The reaper
// gates its migration on the objects and annotations components produce, so an
// object silently appearing, disappearing or being renamed is a real hazard.
func (p *Plan) Summary() string {
	if p.Len() == 0 {
		return ""
	}

	var b strings.Builder

	for _, op := range p.Operations {
		b.WriteString(op.Kind.String())
		b.WriteString(" ")
		b.WriteString(op.Ref().String())

		if op.Overridable {
			b.WriteString(" [overridable]")
		}

		if op.SharedKey != "" {
			b.WriteString(" [shared]")
		}

		if len(op.DependsOn) > 0 {
			b.WriteString(" [after")

			for _, dep := range op.DependsOn {
				b.WriteString(" ")
				b.WriteString(dep.String())
			}

			b.WriteString("]")
		}

		b.WriteString("\n")
	}

	return b.String()
}

// sortOperations groups operations by the component that planned them,
// preserving the order components were planned in and, within a component, the
// order that component emitted its operations.
//
// Within-component order is preserved rather than sorted because components
// plan deliberately: gantry removes its legacy node config before applying
// anything, and storage writes its ConfigMap before the DaemonSet that hashes
// it. Sorting by kind or name would silently reorder that intent. Components
// plan deterministically, walking sorted manifest file lists, so preserving
// their order is still stable across passes.
//
// Component order comes from first appearance rather than from the name, so the
// registry order the reconciler iterates in is what reaches the cluster, and
// conditions and rollouts stay in the documented order.
func sortOperations(ops []Operation) {
	order := map[string]int{}

	for _, op := range ops {
		if _, seen := order[op.Component]; !seen {
			order[op.Component] = len(order)
		}
	}

	sort.SliceStable(ops, func(i, j int) bool {
		return order[ops[i].Component] < order[ops[j].Component]
	})
}
