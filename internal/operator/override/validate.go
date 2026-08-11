// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"fmt"
	"sort"
	"strings"
)

// knownComponents are the components that generate workloads an override can
// target. Cluster components are not per-Site, so an entry naming one may not
// carry a sites selector.
var knownComponents = map[string]struct{ perSite bool }{
	"net":      {perSite: false},
	"machina":  {perSite: false},
	"gantry":   {perSite: false},
	"metalman": {perSite: true},
	"storage":  {perSite: true},
}

// knownKinds are the workload kinds the operator emits.
var knownKinds = map[string]struct{}{
	"Deployment": {},
	"DaemonSet":  {},
}

// Validate checks every entry against the schema and the allowlist.
//
// It is a pure function of the parsed document: no cluster state is consulted,
// so it can run before anything is written and can run offline in the CLI. It
// deliberately does not resolve entries against real workloads, because whether
// a container or mountPath exists depends on the workload the running operator
// renders, and a client cannot answer that correctly under version skew.
//
// Every problem found is reported, rather than only the first, so a user fixing
// a document sees the whole list.
func Validate(entries []SourcedEntry) error {
	var problems []string

	for _, sourced := range entries {
		problems = append(problems, validateEntry(sourced)...)
	}

	if len(problems) == 0 {
		return nil
	}

	sort.Strings(problems)

	return fmt.Errorf("invalid override document:\n  %s", strings.Join(problems, "\n  "))
}

func validateEntry(sourced SourcedEntry) []string {
	var (
		problems []string
		entry    = sourced.Entry
		at       = sourced.Source.String()
	)

	component, known := knownComponents[entry.Component]

	switch {
	case entry.Component == "":
		problems = append(problems, fmt.Sprintf("%s: component is required", at))
	case !known:
		problems = append(problems, fmt.Sprintf("%s: unknown component %q, want one of %s",
			at, entry.Component, strings.Join(sortedComponents(), ", ")))
	}

	switch entry.Kind {
	case "":
		problems = append(problems, fmt.Sprintf("%s: kind is required", at))
	default:
		if _, ok := knownKinds[entry.Kind]; !ok {
			problems = append(problems, fmt.Sprintf("%s: unsupported kind %q, want Deployment or DaemonSet", at, entry.Kind))
		}
	}

	problems = append(problems, validateSites(at, entry, known, component.perSite)...)

	if !entry.HasWork() {
		problems = append(problems, fmt.Sprintf("%s: entry changes nothing; set patch, extraArgs, or both", at))
	}

	for container, args := range entry.ExtraArgs {
		if container == "" {
			problems = append(problems, fmt.Sprintf("%s: extraArgs has an empty container name", at))
		}

		if len(args) == 0 {
			problems = append(problems, fmt.Sprintf("%s: extraArgs for container %q is empty", at, container))
		}
	}

	problems = append(problems, validateAddNames(at, "addContainers", entry.AddContainers)...)
	problems = append(problems, validateAddNames(at, "addInitContainers", entry.AddInitContainers)...)
	problems = append(problems, validatePatch(at, entry.Patch)...)

	return problems
}

// validateSites checks the Site selector, which is meaningful only for per-Site
// components.
func validateSites(at string, entry Entry, componentKnown, perSite bool) []string {
	var problems []string

	if entry.Sites == nil {
		return nil
	}

	if len(entry.Sites) == 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: sites is present but empty; omit it to match every Site", at))

		return problems
	}

	if componentKnown && !perSite {
		problems = append(problems, fmt.Sprintf(
			"%s: component %q is a cluster singleton and is not per-Site, so sites must be omitted",
			at, entry.Component))
	}

	seen := map[string]bool{}

	for _, site := range entry.Sites {
		switch {
		case site == "":
			problems = append(problems, fmt.Sprintf("%s: sites contains an empty Site name", at))
		case seen[site]:
			problems = append(problems, fmt.Sprintf("%s: sites lists %q more than once", at, site))
		}

		seen[site] = true
	}

	return problems
}

func validateAddNames(at, field string, names []string) []string {
	var problems []string

	seen := map[string]bool{}

	for _, name := range names {
		switch {
		case name == "":
			problems = append(problems, fmt.Sprintf("%s: %s contains an empty container name", at, field))
		case seen[name]:
			problems = append(problems, fmt.Sprintf("%s: %s lists %q more than once", at, field, name))
		}

		seen[name] = true
	}

	return problems
}

// validatePatch walks a patch against the allowlist.
func validatePatch(at string, patch map[string]any) []string {
	if len(patch) == 0 {
		return nil
	}

	var problems []string

	walkPatch(patch, "", compiledAllowlist.root, func(problem string) {
		problems = append(problems, at+": "+problem)
	})

	return problems
}

// walkPatch descends a patch value alongside the allowlist trie, reporting
// every path that is not permitted.
//
// Three classes of problem are reported beyond an unenumerated path:
//
//   - a protected path, named with the reason it is protected
//   - any $-prefixed key, because the strategic merge directive namespace is
//     open and directives can delete operator-managed content
//   - an explicit null, which strategic merge treats as deletion and which a
//     path allowlist alone would not catch
func walkPatch(value any, path string, node *pathNode, report func(string)) {
	if value == nil {
		report(fmt.Sprintf("%s is explicitly null; strategic merge treats null as deletion, which cannot be used to remove operator-managed content",
			describePath(path)))

		return
	}

	switch typed := value.(type) {
	case map[string]any:
		walkPatchMap(typed, path, node, report)
	case []any:
		walkPatchList(typed, path, node, report)
	default:
		// A scalar at a permitted leaf is the normal case; anything not
		// permitted was already reported by the parent.
	}
}

func walkPatchMap(value map[string]any, path string, node *pathNode, report func(string)) {
	keys := make([]string, 0, len(value))

	for key := range value {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		childPath := joinPath(path, key)

		if strings.HasPrefix(key, "$") {
			report(fmt.Sprintf("%s uses the strategic merge directive %q; directives can delete operator-managed content and are not accepted",
				describePath(path), key))

			continue
		}

		if reason, protected := protectedReason(childPath); protected {
			report(fmt.Sprintf("%s is protected: %s", childPath, reason))

			continue
		}

		child := childNode(node, key)
		if child == nil {
			report(fmt.Sprintf("%s is not an overridable field", childPath))

			continue
		}

		// Labels and annotations are permitted subtrees, so the reserved-prefix
		// check has to happen on the keys of the map itself. Checking the
		// parent path would never see them, because the subtree walk does not
		// revisit the allowlist.
		if isReservedMetadataPath(childPath) {
			reportReservedKeys(value[key], childPath, report)
		}

		if child.subtree {
			// Everything below a subtree is permitted, but nulls and directives
			// still are not.
			walkSubtree(value[key], childPath, report)

			continue
		}

		if !child.permitted && len(child.children) == 0 {
			report(fmt.Sprintf("%s is not an overridable field", childPath))

			continue
		}

		walkPatch(value[key], childPath, child, report)
	}
}

func walkPatchList(value []any, path string, node *pathNode, report func(string)) {
	child := childNode(node, wildcard)
	if child == nil {
		report(fmt.Sprintf("%s is not an overridable field", describePath(path)))

		return
	}

	for _, element := range value {
		walkPatch(element, path, child, report)
	}
}

// childNode resolves the trie child for a key, treating a subtree node as
// matching everything below it.
func childNode(node *pathNode, key string) *pathNode {
	if node == nil {
		return nil
	}

	if node.subtree {
		return node
	}

	return node.lookup(key)
}

// walkSubtree checks only for nulls and directives inside a permitted subtree,
// since every path within it is allowed.
func walkSubtree(value any, path string, report func(string)) {
	switch typed := value.(type) {
	case nil:
		report(fmt.Sprintf("%s is explicitly null; strategic merge treats null as deletion, which cannot be used to remove operator-managed content",
			describePath(path)))
	case map[string]any:
		keys := make([]string, 0, len(typed))

		for key := range typed {
			keys = append(keys, key)
		}

		sort.Strings(keys)

		for _, key := range keys {
			if strings.HasPrefix(key, "$") {
				report(fmt.Sprintf("%s uses the strategic merge directive %q; directives can delete operator-managed content and are not accepted",
					describePath(path), key))

				continue
			}

			walkSubtree(typed[key], joinPath(path, key), report)
		}
	case []any:
		for _, element := range typed {
			walkSubtree(element, path, report)
		}
	}
}

// reportReservedKeys rejects label or annotation keys under the operator's own
// prefix. Those carry component config hashes, Site scoping and override
// visibility, so a patch able to write them could forge a hash the reaper gates
// on, or hide the fact that an override is in effect.
func reportReservedKeys(value any, path string, report func(string)) {
	labels, ok := value.(map[string]any)
	if !ok {
		return
	}

	keys := make([]string, 0, len(labels))

	for key := range labels {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		if strings.HasPrefix(key, ReservedPrefix) {
			report(fmt.Sprintf("%s is reserved; the %s prefix carries operator config hashes, Site scoping and override visibility",
				joinPath(path, key), ReservedPrefix))
		}
	}
}

// isReservedMetadataPath reports whether a map at this path holds labels or
// annotations, where the operator's own prefix must not be written.
func isReservedMetadataPath(path string) bool {
	switch path {
	case "metadata.labels", "metadata.annotations",
		"spec.template.metadata.labels", "spec.template.metadata.annotations":
		return true
	default:
		return false
	}
}

func sortedComponents() []string {
	out := make([]string, 0, len(knownComponents))

	for name := range knownComponents {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}
