// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"errors"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Azure/unbounded/internal/operator/component"
)

// Annotation keys stamped on an overridden workload.
const (
	// HashAnnotation carries the hash of the contributor set merged into this
	// object. The matching desired hash lives in Site status rather than here,
	// because writing it would mean touching an object the operator may have
	// just decided not to apply.
	HashAnnotation = ReservedPrefix + "override-hash"

	// SourceAnnotation names the ConfigMap keys and entry indices that shaped
	// this object.
	SourceAnnotation = ReservedPrefix + "override-source"

	// VersionDriftAnnotation is present only when an override changed a
	// container image, breaking version lockstep with the operator.
	VersionDriftAnnotation = ReservedPrefix + "version-drift"
)

// WorkloadResult describes what happened to one overridden workload.
type WorkloadResult struct {
	Ref component.ObjectRef

	// Site is the Site the workload belongs to, empty for cluster singletons.
	Site string

	// Hash is the contributor hash merged into the object.
	Hash string

	// Sources names the contributing entries.
	Sources []Source

	// VersionDrift is set when the override changed a container image.
	VersionDrift string

	// Err is set when this workload could not be overridden. The operation is
	// dropped from the plan rather than applied un-overridden, so the running
	// workload is left as it is.
	Err error
}

// Report is the outcome of applying overrides to a plan.
type Report struct {
	// Workloads describes each workload an override resolved to, whether or not
	// it succeeded.
	Workloads []WorkloadResult

	// UnmatchedSites names Sites an entry selected that do not exist. Inert and
	// reported, never fatal.
	UnmatchedSites []string

	// InertEntries are entries that matched no workload in this pass.
	InertEntries []Source

	// Withheld names the operations removed from the plan because their
	// overrides could not be used, attributed to the component and Site that
	// planned them.
	//
	// The running workload is deliberately left as it is, but the component
	// did not write what it planned, so it must not report itself reconciled.
	// Without this the object was simply absent from the plan and nothing
	// downstream could tell that from "never planned".
	Withheld []WithheldOperation
}

// WithheldOperation is one operation removed from a plan before execution.
type WithheldOperation struct {
	Ref       component.ObjectRef
	Component string
	Site      string
	Err       error
}

// Failed reports whether any workload failed to have its overrides applied.
func (r Report) Failed() bool {
	for _, workload := range r.Workloads {
		if workload.Err != nil {
			return true
		}
	}

	return false
}

// Err joins every per-workload failure.
func (r Report) Err() error {
	var errs []error

	for _, workload := range r.Workloads {
		if workload.Err != nil {
			errs = append(errs, workload.Err)
		}
	}

	return errors.Join(errs...)
}

// Apply merges validated entries into the overridable operations of a plan.
//
// Failures are scoped to the workload that caused them. A resolution failure,
// a conflict between contributors, or a merge error drops that operation from
// the plan entirely rather than applying it un-overridden: the running workload
// keeps whatever it already has, which is recoverable, where reverting it to
// defaults would rewrite running infrastructure over a user's mistake.
//
// Entries must already have passed Validate. Apply does the half of validation
// that needs the rendered workload, which a client cannot do correctly under
// version skew.
func Apply(plan *component.Plan, entries []SourcedEntry, knownSites []string) Report {
	resolution := Resolve(plan, entries, knownSites)

	report := Report{
		UnmatchedSites: resolution.UnmatchedSites,
		InertEntries:   resolution.InertEntries,
	}

	var drop []int

	for _, target := range resolution.Targets {
		result := applyTarget(plan, target)
		report.Workloads = append(report.Workloads, result)

		if result.Err != nil {
			drop = append(drop, target.Index)

			op := plan.Operations[target.Index]
			report.Withheld = append(report.Withheld, WithheldOperation{
				Ref:       op.Ref(),
				Component: op.Component,
				Site:      op.Site,
				Err:       result.Err,
			})
		}
	}

	if len(drop) > 0 {
		dropOperations(plan, drop)
	}

	return report
}

// applyTarget applies one workload's contributors, leaving the operation
// untouched if anything fails.
func applyTarget(plan *component.Plan, target Target) WorkloadResult {
	result := WorkloadResult{Ref: target.Ref, Site: target.Site}

	for _, contributor := range target.Contributors {
		result.Sources = append(result.Sources, contributor.Source)
	}

	original := plan.Operations[target.Index].Object

	// The hash is computed first, from the contributor set alone, so it is
	// reported whether or not the merge succeeds. Leaving it empty on failure
	// made a failed workload indistinguishable in status from one no override
	// targets at all, and the CLI labeled it "no override": the exact opposite
	// of the truth, on the single most important row it prints.
	hash, err := contributorHash(target.Contributors)
	if err != nil {
		result.Err = err

		return result
	}

	result.Hash = hash

	var problems []error

	for _, contributor := range target.Contributors {
		problems = append(problems, checkResolvable(contributor.Entry, contributor.Source, original)...)
	}

	problems = append(problems, checkConflicts(target.Contributors)...)

	if err := errors.Join(problems...); err != nil {
		result.Err = err

		return result
	}

	// Merge into a copy, so a failure part-way through cannot leave the plan
	// carrying a half-merged object.
	candidate := original.DeepCopy()

	if err := merge(candidate, target.Contributors); err != nil {
		result.Err = err

		return result
	}

	result.VersionDrift = imageDrift(original, candidate)

	stampAnnotations(candidate, hash, contributorSources(target.Contributors), result.VersionDrift)

	plan.Operations[target.Index].Object = candidate

	return result
}

func stampAnnotations(workload *unstructured.Unstructured, hash, sources, drift string) {
	annotations := workload.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[HashAnnotation] = hash
	annotations[SourceAnnotation] = sources

	if drift != "" {
		annotations[VersionDriftAnnotation] = drift
	}

	workload.SetAnnotations(annotations)
}

// dropOperations removes operations whose overrides could not be applied.
func dropOperations(plan *component.Plan, indexes []int) {
	drop := make(map[int]bool, len(indexes))
	for _, index := range indexes {
		drop[index] = true
	}

	kept := make([]component.Operation, 0, len(plan.Operations))

	for i, op := range plan.Operations {
		if drop[i] {
			continue
		}

		kept = append(kept, op)
	}

	plan.Operations = kept
}
