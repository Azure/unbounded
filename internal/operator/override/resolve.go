// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

	for i := range plan.Operations {
		op := plan.Operations[i]
		if !op.Overridable {
			continue
		}

		for _, sourced := range entries {
			if !matches(sourced.Entry, op) {
				continue
			}

			target, ok := byIndex[i]
			if !ok {
				target = &Target{Index: i, Ref: op.Ref()}
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

	resolution.UnmatchedSites = unmatchedSites(entries, knownSites)
	resolution.InertEntries = inertEntries(entries, byIndex)

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

func unmatchedSites(entries []SourcedEntry, knownSites []string) []string {
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

func inertEntries(entries []SourcedEntry, byIndex map[int]*Target) []Source {
	used := map[string]bool{}

	for _, target := range byIndex {
		for _, contributor := range target.Contributors {
			used[contributor.Source.String()] = true
		}
	}

	var inert []Source

	for _, sourced := range entries {
		if !used[sourced.Source.String()] {
			inert = append(inert, sourced.Source)
		}
	}

	return inert
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

	return problems
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
				source, addFieldFor(field), name))
		}
	}

	for _, name := range patchedContainerNames(entry.Patch, field) {
		if existing[name] || declaredSet[name] {
			continue
		}

		problems = append(problems, fmt.Errorf(
			"%s: patch targets %s %q, which the workload does not have; "+
				"list it in %s to add it, or correct the name",
			source, singular(field), name, addFieldFor(field)))
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
			"%s: extraArgs targets container %q, which the workload does not have", source, name))
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
					source, mountName, path, name, operatorVolume))
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
