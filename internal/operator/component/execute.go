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

	// Stale names the objects that were written successfully but against a
	// cluster state the plan did not anticipate, so the pass should run again
	// from an accurate read. The operations themselves succeeded and are
	// reported as such; this is a request to re-plan, not a failure.
	Stale []ObjectRef
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

	if err := validatePlan(plan); err != nil {
		return ExecutionResult{}, err
	}

	ops := make([]Operation, len(plan.Operations))
	copy(ops, plan.Operations)
	sortOperations(ops)

	deduped, err := dedupeShared(ops)
	if err != nil {
		return ExecutionResult{}, err
	}

	ordered, err := orderByDependency(byRank(deduped))
	if err != nil {
		return ExecutionResult{}, err
	}

	return e.run(ctx, ordered), nil
}

// plannedOp is one operation the executor will run, together with the extra
// contributors that were deduplicated into it.
//
// The aliases travel on the operation rather than in a map keyed by ObjectRef,
// because an ObjectRef does not identify an operation. A plan legitimately
// holds more than one operation on the same object: a ConfigMap is created if
// absent and then merge-patched to add operator-owned keys. Keyed by ref, the
// second operation inherited the first one's contributors and reported results
// for Sites that had nothing to do with it.
type plannedOp struct {
	Operation

	aliases []Operation
}

// validatePlan rejects operations the executor cannot route.
//
// The common trap is converting an object read with Client.Get, which strips
// TypeMeta, so the resulting unstructured object carries no GVK and the
// apiserver rejects the write with an opaque "Kind is missing" error far from
// the code that built it. Catching it here names the component instead.
func validatePlan(plan *Plan) error {
	for _, op := range plan.Operations {
		if op.Object == nil {
			return fmt.Errorf("component %q planned a %s operation with no object", op.Component, op.Kind)
		}

		if op.Object.GetKind() == "" || op.Object.GetAPIVersion() == "" {
			return fmt.Errorf(
				"component %q planned a %s operation on %s/%s with no apiVersion or kind; "+
					"objects read with Client.Get lose TypeMeta and must have it restored",
				op.Component, op.Kind, op.Object.GetNamespace(), op.Object.GetName())
		}
	}

	return nil
}

// run executes ordered operations, tracking what failed so dependents are
// skipped rather than attempted.
//
// Three things gate an operation, and only the first is declared by the
// component that planned it:
//
//   - a declared DependsOn that failed
//   - an earlier operation on the same object that failed, since patching an
//     object whose creation failed only produces a second, more confusing error
//   - an earlier tier that failed for the same component and Site, which is the
//     inferred form: a component's workload is not attempted when that
//     component's own ConfigMap, RBAC or Namespace did not get written
//
// The inferred gate is scoped to one component and Site deliberately. Skipping
// every workload in the cluster because one component's ConfigMap failed would
// turn a contained failure into an outage; skipping only the component that
// owns the missing dependency keeps the blast radius where the failure is.
//
// A failed Namespace is the exception and gates every namespaced object in it,
// whichever component planned it, because nothing can be written into a
// namespace that does not exist.
func (e *Env) run(ctx context.Context, ordered []plannedOp) ExecutionResult {
	var (
		result    ExecutionResult
		brokenRef = map[ObjectRef]bool{}
		brokenNS  = map[string]bool{}
		brokenSub = map[subject]failure{}
	)

	record := func(op plannedOp, status OpStatus, err error) {
		result.Results = append(result.Results, OperationResult{
			Ref:       op.Ref(),
			Kind:      op.Kind,
			Component: op.Component,
			Site:      op.Site,
			Status:    status,
			Err:       err,
		})

		for _, alias := range op.aliases {
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

	fail := func(op plannedOp, status OpStatus, err error) {
		broke := failure{rank: rankOf(op.Operation), what: describeRank(op.Operation)}

		brokenRef[op.Ref()] = true

		// Every contributor to a deduplicated operation is gated, not only the
		// one whose copy was retained. Storage and metalman plan identical
		// support RBAC for every Site and it is written once; recording the
		// failure against the retained Site alone left every other Site free to
		// apply workloads referencing a ServiceAccount that was never created.
		for _, at := range append([]subject{op.subject()}, op.aliasSubjects()...) {
			// Operations run in rank order, so the first failure recorded for a
			// subject is its earliest and later ones do not lower it.
			if _, seen := brokenSub[at]; !seen {
				brokenSub[at] = broke
			}
		}

		if op.Kind != OpDelete && tierOf(op.Operation) == tierNamespace {
			brokenNS[op.Object.GetName()] = true
		}

		record(op, status, err)
	}

	for _, op := range ordered {
		if reason, blocked := blockedBy(op, brokenRef, brokenNS, brokenSub); blocked {
			fail(op, OpSkipped, errors.New(reason))

			continue
		}

		err := e.execute(ctx, op.Operation)

		switch {
		case errors.Is(err, errStale):
			result.Stale = append(result.Stale, op.Ref())

			record(op, OpSucceeded, nil)
		case err != nil:
			fail(op, OpFailed, err)
		default:
			record(op, OpSucceeded, nil)
		}
	}

	return result
}

// subject identifies the component and Site an operation was planned for, which
// is the scope the inferred tier gate applies to.
type subject struct {
	Component string
	Site      string
}

func (o plannedOp) subject() subject {
	return subject{Component: o.Component, Site: o.Site}
}

// aliasSubjects returns the contributors that were deduplicated into this
// operation, so a failure gates every one of them.
func (o plannedOp) aliasSubjects() []subject {
	out := make([]subject, 0, len(o.aliases))
	for _, alias := range o.aliases {
		out = append(out, subject{Component: alias.Component, Site: alias.Site})
	}

	return out
}

// failure records what a subject last failed at, so later operations for that
// subject can be gated and the skip explained.
type failure struct {
	rank int
	what string
}

// blockedBy reports why an operation must not be attempted, or false when it
// may run.
func blockedBy(op plannedOp, brokenRef map[ObjectRef]bool, brokenNS map[string]bool, brokenSub map[subject]failure) (string, bool) {
	for _, dep := range op.DependsOn {
		if dep != op.Ref() && brokenRef[dep] {
			return fmt.Sprintf("dependency %s did not complete", dep), true
		}
	}

	if brokenRef[op.Ref()] {
		return fmt.Sprintf("an earlier operation on %s did not complete", op.Ref()), true
	}

	if ns := op.Object.GetNamespace(); ns != "" && brokenNS[ns] {
		return fmt.Sprintf("namespace %s could not be written", ns), true
	}

	if failed, ok := brokenSub[op.subject()]; ok && failed.rank < rankOf(op.Operation) {
		return fmt.Sprintf("%s did not complete its %s successfully",
			describeContributor(op.Operation), failed.what), true
	}

	return "", false
}

// execute performs a single operation.
func (e *Env) execute(ctx context.Context, op Operation) error {
	switch op.Kind {
	case OpApply:
		return e.ApplyObject(ctx, op.Object)

	case OpCreateIfAbsent:
		return e.createIfAbsent(ctx, op)

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

// createIfAbsent creates an object, treating an existing one as authoritative.
//
// AlreadyExists is not a failure: the payload belongs to whoever won, and this
// operation exists precisely so an existing payload survives. It is not plain
// success either. Planning read the object and found nothing, so everything
// else the pass computed from that read is wrong: the operator hashes a
// ConfigMap's payload and stamps that hash on the workload that mounts it, so
// losing this race stamps the hash of a payload the cluster does not have. The
// workload rolls to a hash matching nothing, and rolls again once a later pass
// reads the real payload.
//
// The object is refreshed from the cluster so anything reading it back in this
// pass sees what is actually there rather than what the operator proposed, and
// the operation is reported stale so the pass runs again from an accurate read.
func (e *Env) createIfAbsent(ctx context.Context, op Operation) error {
	err := e.Client.Create(ctx, op.Object)

	switch {
	case err == nil:
		return nil
	case !apierrors.IsAlreadyExists(err):
		return fmt.Errorf("create %s: %w", op.Ref(), err)
	}

	if err := e.Client.Get(ctx, client.ObjectKeyFromObject(op.Object), op.Object); err != nil {
		return fmt.Errorf("read %s after losing a create race: %w", op.Ref(), err)
	}

	return errStale
}

// errStale marks an operation that succeeded against a cluster state the plan
// did not anticipate. It is not reported as a failure, but the pass is asked to
// run again.
var errStale = errors.New("plan was computed from stale state")

// dedupeShared collapses operations carrying the same SharedKey.
//
// Per-Site planning produces duplicate operations for objects that are not
// per-Site: metalman plans the same support RBAC for every Site, byte for byte.
// Executing those once rather than once per Site is the point.
//
// Unequal operations sharing a key are rejected rather than resolved by letting
// the last one win, because that would make the result depend on Site iteration
// order. A shared object that differs by Site is a planning bug.
func dedupeShared(ops []Operation) ([]plannedOp, error) {
	var (
		out   []plannedOp
		first = map[string]int{}
	)

	for _, op := range ops {
		if op.SharedKey == "" {
			out = append(out, plannedOp{Operation: op})

			continue
		}

		at, seen := first[op.SharedKey]
		if !seen {
			first[op.SharedKey] = len(out)

			out = append(out, plannedOp{Operation: op})

			continue
		}

		if err := sharedOperationsEqual(out[at].Operation, op); err != nil {
			return nil, err
		}

		out[at].aliases = append(out[at].aliases, op)
	}

	return out, nil
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

// byRank orders operations by the stage inferred from what they write, so that
// namespaces precede the objects in them, RBAC and config precede the workloads
// that consume them, admission and API registration follows the backends it
// points at, and removals happen first in the reverse of all that.
//
// The sort is stable, so within a rank the deterministic order established by
// sortOperations survives: components are grouped in registry order and each
// component's own emission order is preserved.
func byRank(ops []plannedOp) []plannedOp {
	sort.SliceStable(ops, func(i, j int) bool {
		return rankOf(ops[i].Operation) < rankOf(ops[j].Operation)
	})

	return ops
}

// orderByDependency returns operations in an order where every declared
// dependency present in the plan precedes its dependents.
//
// The input is already ordered by tier, and the algorithm preserves that order
// among operations whose dependencies are equally satisfied, so a plan executes
// identically on every pass. Dependencies not present in the plan are treated
// as satisfied.
func orderByDependency(ops []plannedOp) ([]plannedOp, error) {
	produced := map[ObjectRef]bool{}
	for _, op := range ops {
		produced[op.Ref()] = true
	}

	var (
		ordered  = make([]plannedOp, 0, len(ops))
		done     = map[ObjectRef]bool{}
		remained = ops
	)

	for len(remained) > 0 {
		var (
			ready   []plannedOp
			waiting []plannedOp
		)

		for _, op := range remained {
			if dependenciesSatisfied(op.Operation, produced, done) {
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
func refList(ops []plannedOp) string {
	refs := make([]string, 0, len(ops))
	for _, op := range ops {
		refs = append(refs, op.Ref().String())
	}

	sort.Strings(refs)

	return strings.Join(refs, ", ")
}

// CombineResult folds a component's execution outcomes for one Site into its
// planning verdict.
//
// Planning reaches verdicts execution cannot, such as Disabled or NoSites, so
// those survive when there was nothing to execute. When operations did run, a
// failure replaces the planning verdict, because a component that planned
// successfully but failed to write is not reconciled.
//
// Results are matched on both component and Site. A cluster component is
// recorded on every Site's status, and a per-Site component plans separately
// for each Site, so without the Site filter a failure writing one Site's
// DaemonSet appeared as a reconcile error on every other Site as well: every
// Site went NotReady and the condition message named an object belonging to a
// Site the reader was not looking at. Operations with no Site are cluster
// scoped and count towards every Site.
//
// Skipped operations do not produce a failure, because the dependency that
// caused them already reported the underlying error and repeating it here would
// bury the real cause. They do prevent a Ready verdict: the component did not
// write what it planned, and reporting Reconciled would claim otherwise.
func CombineResult(componentName, site string, planned Result, exec ExecutionResult) Result {
	var (
		errs    []error
		skipped []OperationResult
	)

	for _, result := range exec.Results {
		if result.Component != componentName {
			continue
		}

		if result.Site != "" && result.Site != site {
			continue
		}

		switch {
		case result.Status == OpFailed && result.Err != nil:
			errs = append(errs, fmt.Errorf("%s %s: %w", result.Kind, result.Ref, result.Err))
		case result.Status == OpSkipped:
			skipped = append(skipped, result)
		}
	}

	if len(errs) > 0 {
		return Failed(errors.Join(errs...))
	}

	if len(skipped) > 0 {
		return NotReady(ReasonDependencyNotWritten, describeSkipped(skipped))
	}

	return planned
}

// describeSkipped explains what was not written, naming the first object and
// the reason it was gated so the condition points at something actionable.
func describeSkipped(skipped []OperationResult) string {
	first := skipped[0]

	reason := "a dependency did not complete"
	if first.Err != nil {
		reason = first.Err.Error()
	}

	if len(skipped) == 1 {
		return fmt.Sprintf("%s was not written: %s", first.Ref, reason)
	}

	return fmt.Sprintf("%s and %d other object(s) were not written: %s",
		first.Ref, len(skipped)-1, reason)
}
