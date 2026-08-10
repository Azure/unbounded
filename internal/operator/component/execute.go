// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// OpStatus is the outcome of a single operation.
type OpStatus int

const (
	// OpSucceeded means the write completed, or was unnecessary.
	OpSucceeded OpStatus = iota

	// OpFailed means the write was attempted and returned an error.
	OpFailed

	// OpSkipped means the operation was never attempted because a dependency
	// failed. Skipping rather than attempting is deliberate: applying a
	// workload whose ConfigMap failed to write would produce a pod that cannot
	// mount, which is worse than not writing the workload at all.
	OpSkipped
)

// String renders an OpStatus for error messages and test failures.
func (s OpStatus) String() string {
	switch s {
	case OpSucceeded:
		return "Succeeded"
	case OpFailed:
		return "Failed"
	case OpSkipped:
		return "Skipped"
	default:
		return fmt.Sprintf("OpStatus(%d)", int(s))
	}
}

// OperationResult is the outcome of one operation, attributed to the component
// and Site that planned it.
//
// A deduplicated shared operation produces one OperationResult per contributor,
// all carrying the same Status, so every Site that depended on it sees the
// outcome even though the write happened once.
type OperationResult struct {
	Ref       ObjectRef
	Kind      OpKind
	Component string
	Site      string
	Status    OpStatus
	Err       error
}

// ExecutionResult is the outcome of executing a whole plan.
type ExecutionResult struct {
	Results []OperationResult
}

// Err joins every failure into a single error, or returns nil when the plan
// executed cleanly. Skipped operations do not contribute: the dependency that
// caused them already reported the underlying failure, and repeating it would
// bury the real cause in a Site condition.
func (r ExecutionResult) Err() error {
	var errs []error

	for _, result := range r.Results {
		if result.Status != OpFailed || result.Err == nil {
			continue
		}

		errs = append(errs, fmt.Errorf("%s %s: %w", result.Kind, result.Ref, result.Err))
	}

	return errors.Join(errs...)
}

// Failed returns the operations that were attempted and failed.
func (r ExecutionResult) Failed() []OperationResult {
	return r.withStatus(OpFailed)
}

// Skipped returns the operations that were never attempted because a
// dependency failed.
func (r ExecutionResult) Skipped() []OperationResult {
	return r.withStatus(OpSkipped)
}

func (r ExecutionResult) withStatus(status OpStatus) []OperationResult {
	var out []OperationResult

	for _, result := range r.Results {
		if result.Status == status {
			out = append(out, result)
		}
	}

	return out
}

// Execute runs a plan.
//
// Execution is explicitly not transactional. Kubernetes offers no multi-object
// transaction and no rollback, so the guarantees are narrower and stated here:
// operations run in dependency order; a failure does not abort the pass, so
// independent operations still execute; the transitive dependents of a failure
// are skipped rather than attempted; nothing already written is undone; and
// every outcome is attributed to the component and Site that planned it.
//
// Because every operation is idempotent, a partially failed pass is recoverable
// by re-planning and re-executing rather than by resuming from a checkpoint.
func (e *Env) Execute(ctx context.Context, plan *Plan) (ExecutionResult, error) {
	if plan.Len() == 0 {
		return ExecutionResult{}, nil
	}

	ops := make([]Operation, len(plan.Operations))
	copy(ops, plan.Operations)
	sortOperations(ops)

	deduped, aliases, err := dedupeShared(ops)
	if err != nil {
		return ExecutionResult{}, err
	}

	ordered, err := orderByDependency(deduped)
	if err != nil {
		return ExecutionResult{}, err
	}

	return e.run(ctx, ordered, aliases), nil
}

// run executes ordered operations, tracking which refs failed so dependents can
// be skipped. aliases maps a deduplicated operation's ref to the extra
// contributors that must receive the same result.
func (e *Env) run(ctx context.Context, ordered []Operation, aliases map[ObjectRef][]Operation) ExecutionResult {
	var (
		result ExecutionResult
		broken = map[ObjectRef]bool{}
	)

	record := func(op Operation, status OpStatus, err error) {
		result.Results = append(result.Results, OperationResult{
			Ref:       op.Ref(),
			Kind:      op.Kind,
			Component: op.Component,
			Site:      op.Site,
			Status:    status,
			Err:       err,
		})

		for _, alias := range aliases[op.Ref()] {
			result.Results = append(result.Results, OperationResult{
				Ref:       op.Ref(),
				Kind:      op.Kind,
				Component: alias.Component,
				Site:      alias.Site,
				Status:    status,
				Err:       err,
			})
		}
	}

	for _, op := range ordered {
		if blocker, blocked := firstBrokenDependency(op, broken); blocked {
			broken[op.Ref()] = true

			record(op, OpSkipped, fmt.Errorf("dependency %s did not complete", blocker))

			continue
		}

		if err := e.execute(ctx, op); err != nil {
			broken[op.Ref()] = true

			record(op, OpFailed, err)

			continue
		}

		record(op, OpSucceeded, nil)
	}

	return result
}

// firstBrokenDependency reports the first declared dependency that failed or
// was skipped. Dependencies not present in the plan are treated as satisfied:
// they may be objects another component owns, or objects that already exist.
func firstBrokenDependency(op Operation, broken map[ObjectRef]bool) (ObjectRef, bool) {
	for _, dep := range op.DependsOn {
		if broken[dep] {
			return dep, true
		}
	}

	return ObjectRef{}, false
}

// execute performs a single operation.
func (e *Env) execute(ctx context.Context, op Operation) error {
	switch op.Kind {
	case OpApply:
		return e.ApplyObject(ctx, op.Object)

	case OpCreateIfAbsent:
		// AlreadyExists is success: the payload belongs to whoever won, and
		// this operation exists precisely so an existing payload survives.
		if err := e.Client.Create(ctx, op.Object); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create %s: %w", op.Ref(), err)
		}

		return nil

	case OpMergePatch:
		if op.Base == nil {
			return fmt.Errorf("merge patch %s: plan carries no observed state", op.Ref())
		}

		patch := client.MergeFromWithOptions(op.Base, client.MergeFromWithOptimisticLock{})
		if err := e.Client.Patch(ctx, op.Object, patch); err != nil {
			return fmt.Errorf("patch %s: %w", op.Ref(), err)
		}

		return nil

	case OpDelete:
		return e.DeleteIfExists(ctx, op.Object)

	default:
		return fmt.Errorf("unknown operation kind %s for %s", op.Kind, op.Ref())
	}
}

// dedupeShared collapses operations carrying the same SharedKey.
//
// Per-Site planning produces duplicate operations for objects that are not
// per-Site: metalman plans the same support RBAC for every Site, byte for byte.
// Executing those once rather than once per Site is the point.
//
// Unequal operations sharing a key are rejected rather than resolved by letting
// the last one win, because that would make the result depend on Site iteration
// order. A shared object that differs by Site is a planning bug.
func dedupeShared(ops []Operation) ([]Operation, map[ObjectRef][]Operation, error) {
	var (
		out     []Operation
		aliases = map[ObjectRef][]Operation{}
		first   = map[string]Operation{}
	)

	for _, op := range ops {
		if op.SharedKey == "" {
			out = append(out, op)

			continue
		}

		existing, seen := first[op.SharedKey]
		if !seen {
			first[op.SharedKey] = op

			out = append(out, op)

			continue
		}

		if err := sharedOperationsEqual(existing, op); err != nil {
			return nil, nil, err
		}

		aliases[existing.Ref()] = append(aliases[existing.Ref()], op)
	}

	return out, aliases, nil
}

// sharedOperationsEqual reports whether two operations sharing a key describe
// the same write, naming both contributors when they do not.
func sharedOperationsEqual(a, b Operation) error {
	contributors := describeContributor(a) + " and " + describeContributor(b)

	if a.Kind != b.Kind {
		return fmt.Errorf("shared operation %q planned as %s by %s: contributors must agree",
			a.SharedKey, a.Kind.String()+" and "+b.Kind.String(), contributors)
	}

	if a.Ref() != b.Ref() {
		return fmt.Errorf("shared operation %q targets %s and %s from %s: a shared key must identify one object",
			a.SharedKey, a.Ref(), b.Ref(), contributors)
	}

	if !apiequality.Semantic.DeepEqual(a.Object.Object, b.Object.Object) {
		return fmt.Errorf("shared operation %q on %s differs between %s: shared objects must be identical",
			a.SharedKey, a.Ref(), contributors)
	}

	return nil
}

func describeContributor(op Operation) string {
	if op.Site == "" {
		return op.Component
	}

	return op.Component + " (site " + op.Site + ")"
}

// orderByDependency returns operations in an order where every declared
// dependency present in the plan precedes its dependents.
//
// The input is already deterministically sorted, and the algorithm preserves
// that order among operations whose dependencies are equally satisfied, so a
// plan executes identically on every pass. Dependencies not present in the plan
// are treated as satisfied.
func orderByDependency(ops []Operation) ([]Operation, error) {
	produced := map[ObjectRef]bool{}
	for _, op := range ops {
		produced[op.Ref()] = true
	}

	var (
		ordered  = make([]Operation, 0, len(ops))
		done     = map[ObjectRef]bool{}
		remained = ops
	)

	for len(remained) > 0 {
		var (
			ready   []Operation
			waiting []Operation
		)

		for _, op := range remained {
			if dependenciesSatisfied(op, produced, done) {
				ready = append(ready, op)

				continue
			}

			waiting = append(waiting, op)
		}

		if len(ready) == 0 {
			return nil, fmt.Errorf("operation plan has a dependency cycle among %s", refList(waiting))
		}

		for _, op := range ready {
			done[op.Ref()] = true
		}

		ordered = append(ordered, ready...)
		remained = waiting
	}

	return ordered, nil
}

// dependenciesSatisfied reports whether every dependency of op that the plan
// actually produces has already been ordered.
func dependenciesSatisfied(op Operation, produced, done map[ObjectRef]bool) bool {
	for _, dep := range op.DependsOn {
		if dep == op.Ref() {
			continue
		}

		if produced[dep] && !done[dep] {
			return false
		}
	}

	return true
}

// refList renders the refs of a set of operations for a cycle error, sorted so
// the message is stable.
func refList(ops []Operation) string {
	refs := make([]string, 0, len(ops))
	for _, op := range ops {
		refs = append(refs, op.Ref().String())
	}

	sort.Strings(refs)

	return strings.Join(refs, ", ")
}
