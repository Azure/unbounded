// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
)

// overrideState is what a pass found when it read the overrides ConfigMap.
//
// The states are distinguished because conflating them turns a typo into an
// uninstall: removing overrides is a deliberate request for defaults, while
// breaking the document is not.
type overrideState int

const (
	// overridesAbsent means no ConfigMap exists. Applying vanilla manifests is
	// the requested outcome.
	overridesAbsent overrideState = iota

	// overridesValid means every key parsed and every entry validated.
	overridesValid

	// overridesPartial means some entries are usable and some are not. The
	// usable ones are merged; the workloads the unusable ones could have
	// targeted are withheld.
	overridesPartial

	// overridesInvalid means nothing usable could be read: the payload failed
	// as a whole, or every key failed to parse.
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

	// problems are the parse and validation failures, each carrying what it
	// could have targeted so the withholding can be scoped to those workloads.
	problems []override.Problem

	// err is set only for a failure of the payload as a whole: an API read that
	// failed, or a limit that applies across every key. A per-key or per-entry
	// failure is in problems instead, because it can be attributed.
	err error

	// configMap is the object the snapshot was read from, kept so Events can
	// be recorded against the thing the user actually edited. It is nil when
	// no ConfigMap exists or the read failed, which is exactly when there is
	// nothing to attach an Event to.
	configMap *corev1.ConfigMap
}

// usable reports whether any override can be merged this pass.
func (s overrideSnapshot) usable() bool {
	return s.state == overridesValid || s.state == overridesPartial
}

// rejected reports whether any part of the document could not be used.
//
// It is deliberately not the same question as "was anything withheld". An entry
// naming a component that is disabled, or not installed on this cluster,
// resolves to no workload, so withholding has nothing to withhold; the document
// is still wrong and the user still needs to hear so.
func (s overrideSnapshot) rejected() bool {
	return s.err != nil || len(s.problems) > 0
}

// failure renders everything wrong with the document as one error, for the pass
// error and for the Event on the ConfigMap.
func (s overrideSnapshot) failure() error {
	if s.err != nil {
		return s.err
	}

	return override.ProblemsError(s.problems)
}

// quarantine returns the workloads this pass must not write.
//
// Withholding rather than reverting is the core of the failure model. Applying
// vanilla manifests over a workload whose override could not be read is not a
// safe fallback, because defaults are not the current state: falling back
// rewrites running infrastructure, and a single mis-indented line would strip
// resources, tolerations, sidecars and pinned images and roll the workload,
// including a zero-available window on the host-networked workloads that use
// maxSurge: 0.
//
// The scope of the withholding is what changed. A failure whose targets are
// knowable withholds only those; one whose targets are not knowable withholds
// everything an override could reach.
func (s overrideSnapshot) quarantine() overrideQuarantine {
	switch s.state {
	case overridesAbsent, overridesValid:
		return overrideQuarantine{}
	case overridesUnreadable:
		return overrideQuarantine{all: true, cause: s.err}
	}

	if s.err != nil {
		// A payload-level failure: no key is at fault and none could be read.
		return overrideQuarantine{all: true, cause: s.err}
	}

	return quarantineFor(s.problems)
}

// overrideQuarantine is the set of workloads a pass must not write because the
// overrides that would have shaped them could not be used.
type overrideQuarantine struct {
	// all withholds every overridable workload. It is set for a failure whose
	// targets cannot be known: a key that did not parse, so the entries it held
	// were never read, or a payload that could not be read at all.
	all bool

	// cause explains an `all` quarantine, which belongs to no single entry.
	cause error

	// targets withhold specific workloads.
	targets []quarantineTarget
}

// quarantineTarget is one component's workloads put in doubt by one problem.
type quarantineTarget struct {
	component string

	// kind is empty when the offending entry named no usable kind, in which
	// case every kind the component emits is in doubt.
	kind string

	// sites is nil when every Site is in doubt, which is what an absent or
	// empty selector means.
	sites map[string]bool

	problem override.Problem
}

// quarantineFor works out what a set of problems puts in doubt.
//
// An entry naming no component is skipped entirely rather than treated as
// unknowable. Resolution matches on component, so such an entry could never
// have resolved to a workload and withholding anything for it would punish the
// rest of the document for a typo that changed nothing.
func quarantineFor(problems []override.Problem) overrideQuarantine {
	var q overrideQuarantine

	for _, problem := range problems {
		if problem.KeyLevel() {
			q.all = true
			q.cause = override.ProblemsError(problems)

			return q
		}

		if problem.Component == "" {
			continue
		}

		q.targets = append(q.targets, quarantineTarget{
			component: problem.Component,
			kind:      problem.Kind,
			sites:     siteSet(problem.Sites),
			problem:   problem,
		})
	}

	return q
}

// siteSet turns a Site selector into a lookup, treating both an absent and an
// empty selector as every Site. An entry that named no Site could have meant
// any of them, so the conservative reading is the safe one.
func siteSet(sites []string) map[string]bool {
	if len(sites) == 0 {
		return nil
	}

	out := make(map[string]bool, len(sites))
	for _, site := range sites {
		out[site] = true
	}

	return out
}

// empty reports whether the quarantine withholds nothing.
func (q overrideQuarantine) empty() bool {
	return !q.all && len(q.targets) == 0
}

// covers reports whether an operation must be withheld, and why.
func (q overrideQuarantine) covers(op component.Operation) (error, bool) {
	if !op.Overridable {
		return nil, false
	}

	if q.all {
		return q.cause, true
	}

	for _, target := range q.targets {
		if target.component != op.Component {
			continue
		}

		if target.kind != "" && target.kind != op.Object.GetKind() {
			continue
		}

		// A cluster singleton carries no Site and is withheld by any entry
		// naming its component, whatever that entry said about Sites.
		if target.sites != nil && op.Site != "" && !target.sites[op.Site] {
			continue
		}

		return errors.New(target.problem.String()), true
	}

	return nil, false
}

// loadOverrides reads and validates the overrides ConfigMap once per pass.
//
// Parsing and validation are pure functions of the payload, so this is the
// atomic part of the pass: if it fails, nothing has been written, and nothing
// will be for any workload the failure puts in doubt.
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

	entries, problems, err := override.Parse(configMap.Data)
	if err != nil {
		snapshot.state = overridesInvalid
		snapshot.err = err

		return snapshot
	}

	// Validation runs on the entries that parsed. An entry that failed to parse
	// is already accounted for and cannot be validated.
	problems = append(problems, override.Validate(entries)...)

	// Entries that themselves failed validation must not be merged, even though
	// they parsed. Their workloads are withheld by the quarantine; leaving the
	// entry in would apply a patch that was just declared invalid.
	entries = usableEntries(entries, problems)

	switch {
	case len(problems) == 0:
		snapshot.state = overridesValid
	case len(entries) == 0:
		snapshot.state = overridesInvalid
	default:
		snapshot.state = overridesPartial
	}

	snapshot.entries = entries
	snapshot.problems = problems

	return snapshot
}

// usableEntries drops the entries that any problem names, so a document with
// one bad entry still applies the rest.
func usableEntries(entries []override.SourcedEntry, problems []override.Problem) []override.SourcedEntry {
	if len(problems) == 0 {
		return entries
	}

	rejected := make(map[override.Source]bool, len(problems))

	for _, problem := range problems {
		if problem.Source != nil {
			rejected[*problem.Source] = true
		}
	}

	kept := make([]override.SourcedEntry, 0, len(entries))

	for _, entry := range entries {
		if rejected[entry.Source] {
			continue
		}

		kept = append(kept, entry)
	}

	return kept
}

// dropOverridableOperations removes the workloads a quarantine covers.
//
// Only Overridable operations are candidates. RBAC, Services, component
// ConfigMaps, adoptions and deletes all still execute, so an override typo does
// not stop the operator doing its other work. The cost, which is deliberate, is
// that drift on the withheld workloads is not corrected until the document is
// fixed.
func dropOverridableOperations(plan *component.Plan, q overrideQuarantine) []override.WithheldOperation {
	if q.empty() {
		return nil
	}

	kept := make([]component.Operation, 0, len(plan.Operations))

	var withheld []override.WithheldOperation

	for _, op := range plan.Operations {
		cause, covered := q.covers(op)
		if !covered {
			kept = append(kept, op)

			continue
		}

		withheld = append(withheld, override.WithheldOperation{
			Ref:       op.Ref(),
			Component: op.Component,
			Site:      op.Site,
			Err:       cause,
		})
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

	if report == nil {
		return status
	}

	// A withheld workload gets a row of its own. It never became a resolution
	// target, because it was dropped from the plan before overrides were
	// applied, so without this it was absent from status entirely and there was
	// no way to tell "no override targets this" from "the operator declined to
	// write it".
	for _, withheld := range report.Withheld {
		if withheld.Site != "" && withheld.Site != site {
			continue
		}

		status.Workloads = append(status.Workloads, unboundedv1alpha3.OverriddenWorkload{
			Kind:  withheld.Ref.GVK.Kind,
			Name:  withheld.Ref.Name,
			State: unboundedv1alpha3.OverrideStateWithheld,
		})

		status.Phase = unboundedv1alpha3.OverridePhaseDegraded

		if status.Message == "" && withheld.Err != nil {
			status.Message = truncateMessage(withheld.Err.Error())
		}
	}

	applied := appliedHashes(plan, exec)
	deferred := deferredRefs(exec)

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

		switch {
		case workload.Err != nil:
			entry.State = unboundedv1alpha3.OverrideStateFailed
		case deferred[workload.Ref]:
			entry.State = unboundedv1alpha3.OverrideStatePending
		case entry.AppliedHash != entry.DesiredHash:
			entry.State = unboundedv1alpha3.OverrideStateFailed
		default:
			entry.State = unboundedv1alpha3.OverrideStateApplied
		}

		status.Workloads = append(status.Workloads, entry)

		switch {
		case workload.Err != nil:
			status.Phase = unboundedv1alpha3.OverridePhaseDegraded

			if status.Message == "" {
				status.Message = truncateMessage(workload.Err.Error())
			}
		case entry.State == unboundedv1alpha3.OverrideStatePending:
			// The cluster moved under this pass and the next one writes it.
			// Nothing is wrong, so this must not read as Degraded for the
			// second it takes to re-plan.
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

	// Cut on a rune boundary. A byte-offset slice can split a multi-byte rune
	// and produce a status field that is not valid UTF-8, which the API server
	// rejects.
	cut := maxStatusMessage - len(notice)
	for cut > 0 && !utf8.ValidString(message[:cut]) {
		cut--
	}

	return message[:cut] + notice
}

// deferredRefs indexes the objects whose write was deferred to the next pass,
// so a one-second race does not report as a failure.
func deferredRefs(exec component.ExecutionResult) map[component.ObjectRef]bool {
	out := make(map[component.ObjectRef]bool, len(exec.Deferred))
	for _, ref := range exec.Deferred {
		out[ref] = true
	}

	return out
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
	// A plan may hold more than one operation on an object, so a ref counts as
	// written only when every operation on it succeeded. Setting it from any
	// success and never clearing it would report an applied hash for an object
	// a later operation failed to finish, which is the divergence this status
	// exists to surface. unwrittenOverrides already reads it this way.
	written := map[component.ObjectRef]bool{}

	for _, result := range exec.Results {
		if result.Status == component.OpSucceeded {
			if _, seen := written[result.Ref]; !seen {
				written[result.Ref] = true
			}

			continue
		}

		written[result.Ref] = false
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
