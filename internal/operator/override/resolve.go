// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Azure/unbounded/internal/operator/component"
)

// Target is one workload an override applies to, with the entries that
// contribute to it in deterministic order.
type Target struct {
	// Index is the position of the operation within the plan.
	Index int

	// Ref identifies the workload, for messages and status.
	Ref component.ObjectRef

	// Site is the Site the workload belongs to, empty for cluster singletons.
	// Status is published per Site, so results have to carry it.
	Site string

	// Contributors are the entries resolving to this workload, ordered by
	// sorted ConfigMap key then position within that key's document.
	Contributors []SourcedEntry
}

// Resolution is the outcome of matching entries against a plan.
type Resolution struct {
	// Targets are the workloads at least one entry resolves to.
	Targets []Target

	// UnmatchedSites names Sites an entry selected that do not exist.
	//
	// These are reported rather than fatal. A document may legitimately be
	// written before its Site exists, and deleting a Site must not retroactively
	// invalidate an unrelated override.
	UnmatchedSites []string

	// InertEntries are entries that resolved to no workload at all, so a user
	// can tell "applied nothing" from "matched nothing".
	InertEntries []Source
}

// Resolve matches entries against the overridable operations in a plan.
//
// Only operations a component marked Overridable are candidates, which is what
// confines overrides to the Deployments and DaemonSets the operator generates.
// Entries are matched on component and kind, and for per-Site components on the
// Site selector; an absent selector matches every Site.
func Resolve(plan *component.Plan, entries []SourcedEntry, knownSites []string) *Resolution {
	resolution := &Resolution{}

	byIndex := map[int]*Target{}

	// The Sites this plan actually covers, which for a Site-scoped pass is one
	// of them. An entry selecting a different Site did not fail to match; it
	// was simply out of scope for this pass.
	planned := map[string]bool{}

	for i := range plan.Operations {
		op := plan.Operations[i]
		if !op.Overridable {
			continue
		}

		planned[op.Site] = true

		for _, sourced := range entries {
			if !matches(sourced.Entry, op) {
				continue
			}

			target, ok := byIndex[i]
			if !ok {
				target = &Target{Index: i, Ref: op.Ref(), Site: op.Site}
				byIndex[i] = target
			}

			target.Contributors = append(target.Contributors, sourced)
		}
	}

	indexes := make([]int, 0, len(byIndex))
	for index := range byIndex {
		indexes = append(indexes, index)
	}

	sort.Ints(indexes)

	for _, index := range indexes {
		resolution.Targets = append(resolution.Targets, *byIndex[index])
	}

	resolution.UnmatchedSites = UnmatchedSites(entries, knownSites)
	resolution.InertEntries = inertEntries(entries, byIndex, planned, knownSites)

	return resolution
}

// matches reports whether an entry targets an operation.
func matches(entry Entry, op component.Operation) bool {
	if entry.Component != op.Component || entry.Kind != op.Object.GetKind() {
		return false
	}

	// A nil selector matches every Site, including the empty Site of a cluster
	// singleton. An explicitly empty selector is rejected during validation.
	if entry.Sites == nil {
		return true
	}

	for _, site := range entry.Sites {
		if site == op.Site {
			return true
		}
	}

	return false
}

// UnmatchedSites returns the Site names entries select that do not exist,
// sorted and deduplicated.
//
// It is exported because the CLI reports the same thing before a document is
// applied, and a second implementation of Site-selector matching is a second
// place for the semantics to drift.
//
// These are reported rather than fatal. A document may legitimately be written
// before its Site exists, and deleting a Site must not retroactively invalidate
// an unrelated override.
func UnmatchedSites(entries []SourcedEntry, knownSites []string) []string {
	known := make(map[string]bool, len(knownSites))
	for _, site := range knownSites {
		known[site] = true
	}

	seen := map[string]bool{}

	var unmatched []string

	for _, sourced := range entries {
		for _, site := range sourced.Entry.Sites {
			if known[site] || seen[site] {
				continue
			}

			seen[site] = true

			unmatched = append(unmatched, site)
		}
	}

	sort.Strings(unmatched)

	return unmatched
}

// inertEntries returns the entries that matched no workload, excluding those
// that were merely out of this pass's scope.
//
// A Site-scoped pass plans per-Site components for one Site, so every entry
// selecting a different Site resolves to nothing in it. Reporting those as
// inert produced a log line per entry per pass and, worse, a Normal Event on
// the ConfigMap announcing that entries "matched nothing" when they had simply
// not been looked at. Inert has to mean "this entry matches nothing anywhere",
// which is the only reading a user can act on.
func inertEntries(entries []SourcedEntry, byIndex map[int]*Target, planned map[string]bool, knownSites []string) []Source {
	used := map[string]bool{}

	for _, target := range byIndex {
		for _, contributor := range target.Contributors {
			used[contributor.Source.String()] = true
		}
	}

	var inert []Source

	for _, sourced := range entries {
		if used[sourced.Source.String()] {
			continue
		}

		if outOfScope(sourced.Entry, planned, knownSites) {
			continue
		}

		inert = append(inert, sourced.Source)
	}

	return inert
}

// outOfScope reports whether an entry names a Site that exists but that this
// pass did not plan for, which is the difference between "matches nothing" and
// "was not looked at".
func outOfScope(entry Entry, planned map[string]bool, knownSites []string) bool {
	if len(entry.Sites) == 0 {
		return false
	}

	known := make(map[string]bool, len(knownSites))
	for _, site := range knownSites {
		known[site] = true
	}

	for _, site := range entry.Sites {
		// A Site that does not exist is reported through UnmatchedSites, and a
		// Site this pass planned for is genuinely unmatched, so neither puts
		// the entry out of scope.
		if !known[site] || planned[site] {
			return false
		}
	}

	return true
}

// checkResolvable verifies an entry's container references against the workload
// it resolved to.
//
// This is the resolution half of validation, and it can only happen here:
// whether a container exists depends on the workload the running operator
// renders, which is why the CLI's offline validation deliberately stops short
// of it.
//
// Strategic merge cannot tell a sidecar from a typo. Both are "this name is not
// present", and merging by name silently creates a container either way, so a
// patch meaning to raise a limit on machina-controller but spelling it
// machina-contoller would add an image-less container and leave the real limit
// untouched. Container names are also release-specific, so a name correct at
// authoring time can stop matching after an upgrade with the same outcome.
func checkResolvable(entry Entry, source Source, workload *unstructured.Unstructured) []error {
	var problems []error

	for _, field := range []struct {
		key      string
		declared []string
	}{
		{key: "containers", declared: entry.AddContainers},
		{key: "initContainers", declared: entry.AddInitContainers},
	} {
		problems = append(problems, checkContainerNames(entry, source, workload, field.key, field.declared)...)
	}

	problems = append(problems, checkExtraArgsTargets(entry, source, workload)...)
	problems = append(problems, checkMountCollisions(entry, source, workload)...)
	problems = append(problems, checkVolumeCollisions(entry, source, workload)...)
	problems = append(problems, checkExclusiveFields(entry, source, workload)...)
	problems = append(problems, checkSelectorLabels(entry, source, workload)...)

	return problems
}

// checkSelectorLabels rejects a patch that rewrites a template label the
// workload's selector matches.
//
// spec.selector is protected, but spec.template.metadata.labels is a permitted
// subtree, so a patch changing a selector-matched label validated, merged, and
// was then silently restamped by restoreIdentity: the user got no error and no
// effect. Restamping is still the backstop, because a template that stops
// satisfying its selector is rejected outright by the API server and that must
// not depend on this check being exhaustive. Saying so here is what turns a
// silent no-op into an answer.
//
// Setting the label to the value it already has is permitted. It changes
// nothing and refusing it would fail a patch that merely restates the workload.
func checkSelectorLabels(entry Entry, source Source, workload *unstructured.Unstructured) []error {
	if len(entry.Patch) == 0 {
		return nil
	}

	matchLabels := nestedStringMap(workload.Object, "spec", "selector", "matchLabels")
	if len(matchLabels) == 0 {
		return nil
	}

	patched, found, err := unstructured.NestedFieldNoCopy(
		entry.Patch, "spec", "template", "metadata", "labels",
	)
	if err != nil || !found {
		return nil
	}

	labels, ok := patched.(map[string]any)
	if !ok {
		return nil
	}

	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	var problems []error

	for _, key := range keys {
		current, selects := matchLabels[key]
		if !selects {
			continue
		}

		if value, ok := labels[key].(string); ok && value == current {
			continue
		}

		problems = append(problems, fmt.Errorf(
			"%s: patch sets template label %q, which the workload selector matches; "+
				"a template that stops satisfying its selector is rejected by the API server, "+
				"so the operator restores it and the change would do nothing. Add a label under a different key instead",
			source, key,
		))
	}

	return problems
}

// checkExclusiveFields rejects a patch that would leave two mutually exclusive
// Kubernetes fields set at once.
//
// This class exists because an override cannot delete anything: explicit null
// is refused everywhere, since strategic merge treats it as deletion and that
// is how operator-managed content would be removed. A patch can therefore only
// add to what is already there, and where the schema says two fields may not
// both be present, adding one is not enough.
//
// The two reachable cases are checked by name rather than by consulting the
// Kubernetes schema, which is not a dependency of this repository and would be
// a disproportionate one for two rules. Both are caught here, at resolution,
// because both depend on what the operator's own workload already contains.
func checkExclusiveFields(entry Entry, source Source, workload *unstructured.Unstructured) []error {
	if len(entry.Patch) == 0 {
		return nil
	}

	var problems []error

	problems = append(problems, checkEnvValueSources(entry, source, workload)...)
	problems = append(problems, checkStrategyExclusivity(entry, source, workload)...)

	return problems
}

// checkEnvValueSources rejects setting value on an env entry the operator
// defines with valueFrom, or the reverse. Kubernetes permits exactly one.
func checkEnvValueSources(entry Entry, source Source, workload *unstructured.Unstructured) []error {
	var problems []error

	for _, field := range []string{"containers", "initContainers"} {
		existing := containerEnvFields(workload, field)

		for _, patched := range patchedContainers(entry.Patch, field) {
			name, _ := patched["name"].(string) //nolint:errcheck // absent means unnamed

			envs, ok := patched["env"].([]any)
			if !ok {
				continue
			}

			for _, raw := range envs {
				env, ok := raw.(map[string]any)
				if !ok {
					continue
				}

				variable, _ := env["name"].(string) //nolint:errcheck // absent means unnamed
				if variable == "" {
					continue
				}

				current, known := existing[containerEnv{container: name, variable: variable}]
				if !known {
					continue
				}

				for _, pair := range []struct{ set, conflicts string }{
					{set: "value", conflicts: "valueFrom"},
					{set: "valueFrom", conflicts: "value"},
				} {
					if _, sets := env[pair.set]; sets && current[pair.conflicts] {
						problems = append(problems, fmt.Errorf(
							"%s: env %q in container %q sets %s, but the operator defines it with %s, and Kubernetes "+
								"permits only one; an override cannot remove the other, so this cannot be expressed",
							source, variable, name, pair.set, pair.conflicts,
						))
					}
				}
			}
		}
	}

	return problems
}

// checkStrategyExclusivity rejects switching a Deployment to Recreate while the
// operator's rollingUpdate block is still present, which the API server refuses.
func checkStrategyExclusivity(entry Entry, source Source, workload *unstructured.Unstructured) []error {
	strategyType, _, err := unstructured.NestedString(entry.Patch, "spec", "strategy", "type")
	if err != nil || strategyType != "Recreate" {
		return nil
	}

	// A patch supplying its own rollingUpdate alongside Recreate is rejected by
	// the same API server rule, whether or not the operator set one.
	patched, err := hasRollingUpdate(entry.Patch)
	if err != nil {
		return []error{fmt.Errorf("%s: read spec.strategy.rollingUpdate from the patch: %w", source, err)}
	}

	if patched {
		return []error{fmt.Errorf(
			"%s: spec.strategy sets type Recreate and rollingUpdate together, which Kubernetes rejects", source,
		)}
	}

	present, err := hasRollingUpdate(workload.Object)
	if err != nil || !present {
		return nil
	}

	return []error{fmt.Errorf(
		"%s: spec.strategy.type is set to Recreate, but the operator defines spec.strategy.rollingUpdate and "+
			"Kubernetes rejects a Deployment carrying both; an override cannot remove it, so this cannot be expressed",
		source,
	)}
}

// hasRollingUpdate reports whether an object carries a rollingUpdate block.
func hasRollingUpdate(object map[string]any) (bool, error) {
	_, present, err := unstructured.NestedMap(object, "spec", "strategy", "rollingUpdate")

	return present, err
}

type containerEnv struct {
	container string
	variable  string
}

// containerEnvFields indexes which of value and valueFrom the workload's own
// env entries set.
func containerEnvFields(workload *unstructured.Unstructured, field string) map[containerEnv]map[string]bool {
	out := map[containerEnv]map[string]bool{}

	for _, raw := range nestedSlice(workload.Object, "spec", "template", "spec", field) {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		name, _ := container["name"].(string) //nolint:errcheck // absent means unnamed

		envs, ok := container["env"].([]any)
		if !ok {
			continue
		}

		for _, rawEnv := range envs {
			env, ok := rawEnv.(map[string]any)
			if !ok {
				continue
			}

			variable, _ := env["name"].(string) //nolint:errcheck // absent means unnamed
			if variable == "" {
				continue
			}

			set := map[string]bool{}

			for _, key := range []string{"value", "valueFrom"} {
				if _, present := env[key]; present {
					set[key] = true
				}
			}

			out[containerEnv{container: name, variable: variable}] = set
		}
	}

	return out
}

// checkVolumeCollisions rejects a patch that would redefine an
// operator-declared volume.
//
// This is the other half of the mount protection, and without it that
// protection is bypassable. Mounts are protected on (container, mountPath)
// because that is the key strategic merge uses for volumeMounts, so a patch
// cannot repoint an operator mount at a different volume. But volumes
// themselves merge on name, so a patch can leave every mount untouched and
// instead redefine what the volume is:
//
//	volumes:
//	  - name: host-run
//	    hostPath: {path: /etc/kubernetes}
//
// Every container mounting host-run now reads a different host directory, with
// nothing in the patch naming a mountPath at all. On workloads that are already
// privileged and host-networked, silently repointing a hostPath is the most
// consequential edit an override can make.
//
// Adding new volumes remains the point of the field: a sidecar needs somewhere
// to write. Only names the operator already declares are refused.
func checkVolumeCollisions(entry Entry, source Source, workload *unstructured.Unstructured) []error {
	if len(entry.Patch) == 0 {
		return nil
	}

	operatorVolumes := volumeNames(workload)
	if len(operatorVolumes) == 0 {
		return nil
	}

	var problems []error

	for _, raw := range nestedSlice(entry.Patch, "spec", "template", "spec", "volumes") {
		volume, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		name, _ := volume["name"].(string) //nolint:errcheck // absent means unnamed
		if name == "" || !operatorVolumes[name] {
			continue
		}

		// An entry carrying nothing but the name changes nothing, so it is not
		// worth failing a document over.
		if len(volume) == 1 {
			continue
		}

		problems = append(problems, fmt.Errorf(
			"%s: patch redefines volume %q, which the operator declares; "+
				"volumes merge on name, so this would repoint every mount that uses it "+
				"without naming a mountPath; add a volume under a different name instead",
			source, name,
		))
	}

	return problems
}

// volumeNames returns the names of the volumes a workload declares.
func volumeNames(workload *unstructured.Unstructured) map[string]bool {
	out := map[string]bool{}

	for _, raw := range nestedSlice(workload.Object, "spec", "template", "spec", "volumes") {
		volume, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		if name, ok := volume["name"].(string); ok && name != "" {
			out[name] = true
		}
	}

	return out
}

func checkContainerNames(entry Entry, source Source, workload *unstructured.Unstructured, field string, declared []string) []error {
	existing := containerNames(workload, field)

	declaredSet := make(map[string]bool, len(declared))
	for _, name := range declared {
		declaredSet[name] = true
	}

	var problems []error

	// A name declared as an addition that already exists means the entry
	// intended to create something that is already there, which is as likely to
	// be a mistake as an addition that does not exist.
	for _, name := range declared {
		if existing[name] {
			problems = append(problems, fmt.Errorf(
				"%s: %s declares container %q as an addition, but the workload already has one with that name",
				source, addFieldFor(field), name,
			))
		}
	}

	for _, name := range patchedContainerNames(entry.Patch, field) {
		if existing[name] || declaredSet[name] {
			continue
		}

		problems = append(problems, fmt.Errorf(
			"%s: patch targets %s %q, which the workload does not have; "+
				"list it in %s to add it, or correct the name",
			source, singular(field), name, addFieldFor(field),
		))
	}

	return problems
}

func checkExtraArgsTargets(entry Entry, source Source, workload *unstructured.Unstructured) []error {
	if len(entry.ExtraArgs) == 0 {
		return nil
	}

	existing := containerNames(workload, "containers")
	for name := range containerNames(workload, "initContainers") {
		existing[name] = true
	}

	added := make(map[string]bool, len(entry.AddContainers)+len(entry.AddInitContainers))

	for _, name := range append(append([]string{}, entry.AddContainers...), entry.AddInitContainers...) {
		added[name] = true
	}

	names := make([]string, 0, len(entry.ExtraArgs))
	for name := range entry.ExtraArgs {
		names = append(names, name)
	}

	sort.Strings(names)

	var problems []error

	for _, name := range names {
		if existing[name] || added[name] {
			continue
		}

		problems = append(problems, fmt.Errorf(
			"%s: extraArgs targets container %q, which the workload does not have", source, name,
		))
	}

	return problems
}

// checkMountCollisions rejects a patch that would repoint an operator-declared
// mount.
//
// volumeMounts merge on mountPath, not on name, so protecting operator mounts
// by name would be bypassable: a mount named anything at all but sharing a
// mountPath merges onto the operator's entry and can repoint it at a different
// volume. Identity here is therefore (container, mountPath), matching the merge
// key strategic merge actually uses.
func checkMountCollisions(entry Entry, source Source, workload *unstructured.Unstructured) []error {
	if len(entry.Patch) == 0 {
		return nil
	}

	var problems []error

	for _, field := range []string{"containers", "initContainers"} {
		operatorMounts := operatorMountPaths(workload, field)

		for _, patched := range patchedContainers(entry.Patch, field) {
			name, _ := patched["name"].(string) //nolint:errcheck // absent means unnamed

			mounts, ok := patched["volumeMounts"].([]any)
			if !ok {
				continue
			}

			for _, raw := range mounts {
				mount, ok := raw.(map[string]any)
				if !ok {
					continue
				}

				path, _ := mount["mountPath"].(string) //nolint:errcheck // absent means no path
				if path == "" {
					continue
				}

				operatorVolume, collides := operatorMounts[containerMount{container: name, mountPath: path}]
				if !collides {
					continue
				}

				mountName, _ := mount["name"].(string) //nolint:errcheck // absent means unnamed
				if mountName == operatorVolume {
					continue
				}

				problems = append(problems, fmt.Errorf(
					"%s: patch mounts volume %q at %q in container %q, where the operator already mounts %q; "+
						"volumeMounts merge on mountPath, so this would repoint an operator-managed mount",
					source, mountName, path, name, operatorVolume,
				))
			}
		}
	}

	return problems
}

type containerMount struct {
	container string
	mountPath string
}

// operatorMountPaths indexes the workload's existing mounts by container and
// mountPath, mapping to the volume name the operator mounts there.
func operatorMountPaths(workload *unstructured.Unstructured, field string) map[containerMount]string {
	out := map[containerMount]string{}

	containers := nestedSlice(workload.Object, "spec", "template", "spec", field)

	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		name, _ := container["name"].(string) //nolint:errcheck // absent means unnamed

		mounts, ok := container["volumeMounts"].([]any)
		if !ok {
			continue
		}

		for _, rawMount := range mounts {
			mount, ok := rawMount.(map[string]any)
			if !ok {
				continue
			}

			path, _ := mount["mountPath"].(string) //nolint:errcheck // absent means no path
			volume, _ := mount["name"].(string)    //nolint:errcheck // absent means unnamed

			if path != "" {
				out[containerMount{container: name, mountPath: path}] = volume
			}
		}
	}

	return out
}

// containerNames returns the names of the workload's containers in a field.
func containerNames(workload *unstructured.Unstructured, field string) map[string]bool {
	out := map[string]bool{}

	containers := nestedSlice(workload.Object, "spec", "template", "spec", field)

	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		if name, ok := container["name"].(string); ok && name != "" {
			out[name] = true
		}
	}

	return out
}

// patchedContainers returns the container maps a patch declares in a field.
func patchedContainers(patch map[string]any, field string) []map[string]any {
	containers := nestedSlice(patch, "spec", "template", "spec", field)
	if containers == nil {
		return nil
	}

	out := make([]map[string]any, 0, len(containers))

	for _, raw := range containers {
		if container, ok := raw.(map[string]any); ok {
			out = append(out, container)
		}
	}

	return out
}

// patchedContainerNames returns the container names a patch targets in a field.
func patchedContainerNames(patch map[string]any, field string) []string {
	var names []string

	for _, container := range patchedContainers(patch, field) {
		if name, ok := container["name"].(string); ok && name != "" {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	return names
}

func addFieldFor(field string) string {
	if field == "initContainers" {
		return "addInitContainers"
	}

	return "addContainers"
}

func singular(field string) string {
	if field == "initContainers" {
		return "init container"
	}

	return "container"
}
