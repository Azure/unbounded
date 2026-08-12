// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
)

// overrideState is what a pass found when it read the overrides ConfigMap.
//
// The four states are distinguished because conflating them turns a typo into
// an uninstall: removing overrides is a deliberate request for defaults, while
// breaking the document is not.
type overrideState int

const (
	// overridesAbsent means no ConfigMap exists. Applying vanilla manifests is
	// the requested outcome.
	overridesAbsent overrideState = iota

	// overridesValid means the document parsed and validated.
	overridesValid

	// overridesInvalid means the document exists but could not be used.
	overridesInvalid

	// overridesUnreadable means the API read failed. Treated as invalid for
	// safety, and the error is returned so the pass requeues.
	overridesUnreadable
)

// overrideSnapshot is one pass's view of the overrides ConfigMap.
//
// Every decision in a pass derives from this single read, and the
// resourceVersion it was taken at is recorded, so a result is always traceable
// to a specific input version. Passes are not serialized against user edits:
// someone can write the ConfigMap while a pass is executing, and different
// passes routinely observe different versions.
type overrideSnapshot struct {
	state           overrideState
	entries         []override.SourcedEntry
	resourceVersion string
	err             error

	// configMap is the object the snapshot was read from, kept so Events can
	// be recorded against the thing the user actually edited. It is nil when
	// no ConfigMap exists or the read failed, which is exactly when there is
	// nothing to attach an Event to.
	configMap *corev1.ConfigMap
}

// usable reports whether overrides can be merged this pass.
func (s overrideSnapshot) usable() bool {
	return s.state == overridesValid
}

// blocksWorkloads reports whether workload operations must be skipped.
//
// Skipping rather than reverting is the core of the failure model. Applying
// vanilla manifests on invalid input is not a safe fallback, because defaults
// are not the current state: falling back rewrites running infrastructure, and
// a single mis-indented line would strip resources, tolerations, sidecars and
// pinned images from every component at once and roll all of them, including a
// zero-available window on the two host-networked workloads that use
// maxSurge: 0.
func (s overrideSnapshot) blocksWorkloads() bool {
	return s.state == overridesInvalid || s.state == overridesUnreadable
}

// loadOverrides reads and validates the overrides ConfigMap once per pass.
//
// Parsing and validation are pure functions of the payload, so this is the
// atomic part of the pass: if it fails, nothing has been written, and nothing
// will be for any workload an override could target.
func loadOverrides(ctx context.Context, env *component.Env) overrideSnapshot {
	key := client.ObjectKey{Namespace: env.Namespace, Name: override.ConfigMapName}

	var configMap corev1.ConfigMap

	err := env.Client.Get(ctx, key, &configMap)

	switch {
	case apierrors.IsNotFound(err):
		return overrideSnapshot{state: overridesAbsent}
	case err != nil:
		return overrideSnapshot{
			state: overridesUnreadable,
			err:   fmt.Errorf("read overrides %s/%s: %w", key.Namespace, key.Name, err),
		}
	}

	snapshot := overrideSnapshot{
		resourceVersion: configMap.ResourceVersion,
		configMap:       configMap.DeepCopy(),
	}

	entries, err := override.Parse(configMap.Data)
	if err != nil {
		snapshot.state = overridesInvalid
		snapshot.err = err

		return snapshot
	}

	if err := override.Validate(entries); err != nil {
		snapshot.state = overridesInvalid
		snapshot.err = err

		return snapshot
	}

	snapshot.state = overridesValid
	snapshot.entries = entries

	return snapshot
}

// dropOverridableOperations removes every workload an override could target.
//
// Only Overridable operations are dropped. RBAC, Services, component
// ConfigMaps, adoptions and deletes all still execute, so an override typo does
// not stop the operator doing its other work. The cost, which is deliberate, is
// that drift on those workloads is not corrected until the document is fixed.
func dropOverridableOperations(plan *component.Plan, cause error) []override.WithheldOperation {
	kept := make([]component.Operation, 0, len(plan.Operations))

	var withheld []override.WithheldOperation

	for _, op := range plan.Operations {
		if op.Overridable {
			withheld = append(withheld, override.WithheldOperation{
				Ref:       op.Ref(),
				Component: op.Component,
				Site:      op.Site,
				Err:       cause,
			})

			continue
		}

		kept = append(kept, op)
	}

	plan.Operations = kept

	return withheld
}

// withheldResults renders withheld operations as execution results, so a
// component that had work removed before execution reaches the same status
// path as one whose work failed during it.
func withheldResults(withheld []override.WithheldOperation) []component.OperationResult {
	out := make([]component.OperationResult, 0, len(withheld))

	for _, op := range withheld {
		out = append(out, component.OperationResult{
			Ref:       op.Ref,
			Kind:      component.OpApply,
			Component: op.Component,
			Site:      op.Site,
			Status:    component.OpDropped,
			Err:       op.Err,
		})
	}

	return out
}

// siteNames returns the names of every Site, for resolving Site selectors.
func siteNames(sites []unboundedv1alpha3.Site) []string {
	names := make([]string, 0, len(sites))
	for i := range sites {
		names = append(names, sites[i].Name)
	}

	return names
}

// overrideStatusFor builds the Site status for one Site from an override
// report.
//
// Desired hashes come from what the operator computed this pass. Applied hashes
// come from the objects the plan carries, but only for operations the executor
// actually completed, so the status reports what reached the cluster rather
// than what the operator meant to write.
func overrideStatusFor(
	site string,
	snapshot overrideSnapshot,
	report *override.Report,
	plan *component.Plan,
	exec component.ExecutionResult,
) *unboundedv1alpha3.OverrideStatus {
	status := &unboundedv1alpha3.OverrideStatus{
		Phase:                   unboundedv1alpha3.OverridePhaseNone,
		ObservedResourceVersion: snapshot.resourceVersion,
	}

	if snapshot.blocksWorkloads() {
		status.Phase = unboundedv1alpha3.OverridePhaseDegraded
		status.Message = truncateMessage(snapshot.err.Error())

		return status
	}

	if report == nil {
		return status
	}

	applied := appliedHashes(plan, exec)

	for _, workload := range report.Workloads {
		// Cluster singletons carry no Site but every Site depends on them, so
		// their override state is reported on all of them. Filtering them out
		// would hide the common case, an override of net or machina, from
		// `kubectl get site` entirely.
		if workload.Site != "" && workload.Site != site {
			continue
		}

		entry := unboundedv1alpha3.OverriddenWorkload{
			Kind:         workload.Ref.GVK.Kind,
			Name:         workload.Ref.Name,
			DesiredHash:  workload.Hash,
			AppliedHash:  applied[workload.Ref],
			VersionDrift: workload.VersionDrift,
		}

		status.Workloads = append(status.Workloads, entry)

		switch {
		case workload.Err != nil:
			status.Phase = unboundedv1alpha3.OverridePhaseDegraded

			if status.Message == "" {
				status.Message = truncateMessage(workload.Err.Error())
			}
		case entry.AppliedHash != entry.DesiredHash:
			status.Phase = unboundedv1alpha3.OverridePhaseDegraded

			if status.Message == "" {
				status.Message = "override was not written to " + workload.Ref.String()
			}
		case status.Phase == unboundedv1alpha3.OverridePhaseNone:
			status.Phase = unboundedv1alpha3.OverridePhaseApplied
		}
	}

	sort.Slice(status.Workloads, func(i, j int) bool {
		if status.Workloads[i].Kind != status.Workloads[j].Kind {
			return status.Workloads[i].Kind < status.Workloads[j].Kind
		}

		return status.Workloads[i].Name < status.Workloads[j].Name
	})

	return status
}

// maxStatusMessage bounds the message copied into every Site's status.
//
// Validation reports every problem it finds rather than only the first, which
// is right for a user fixing a document and wrong for a status field: the
// joined error grows with the document, and it is written to every Site. A
// large enough document produced a status patch past the API server's request
// limit, which fails, and retries forever without ever recording why.
const maxStatusMessage = 2048

// truncateMessage bounds a status message, saying plainly that it was cut and
// where the rest is.
func truncateMessage(message string) string {
	if len(message) <= maxStatusMessage {
		return message
	}

	const notice = "\n[truncated; see the operator log and the Events on the " + override.ConfigMapName + " ConfigMap for the rest]"

	return message[:maxStatusMessage-len(notice)] + notice
}

// appliedHashes reads back the override hash of every overridable workload the
// executor successfully wrote.
//
// The hash is read from the plan, because that is the only place the merged
// object exists, but a hash is only reported when the operation carrying it
// completed. Reading the plan alone described intent: a DaemonSet whose apply
// was rejected by the API server, or skipped because its ConfigMap failed,
// still had its desired hash reported as applied, so the Site said Applied for
// an override that had never reached the cluster. That is precisely the
// divergence this status exists to surface.
//
// An operation dropped because its overrides could not be used is absent from
// the plan entirely, and so is absent here too, with the same effect.
func appliedHashes(plan *component.Plan, exec component.ExecutionResult) map[component.ObjectRef]string {
	written := map[component.ObjectRef]bool{}

	for _, result := range exec.Results {
		if result.Status == component.OpSucceeded {
			written[result.Ref] = true
		}
	}

	out := map[component.ObjectRef]string{}

	for _, op := range plan.Operations {
		if !op.Overridable || !written[op.Ref()] {
			continue
		}

		out[op.Ref()] = op.Object.GetAnnotations()[override.HashAnnotation]
	}

	return out
}
