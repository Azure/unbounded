// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
	problems = append(problems, reportTypedFieldConflicts(at, entry)...)

	return problems
}

// reportTypedFieldConflicts rejects a patch that sets something a typed Site
// field already owns.
//
// Site.spec is the supported customization surface and an override is the
// escape hatch for what it does not cover, so where both describe the same
// thing the typed field decides. Accepting the patch instead would leave the
// user editing the typed field to no effect, or the operator recomputing it
// every pass and fighting the override forever. Naming the field to use makes
// the precedence discoverable at the point it matters.
func reportTypedFieldConflicts(at string, entry Entry) []string {
	owned, ok := typedFieldOwners[entry.Component]
	if !ok || len(entry.Patch) == 0 {
		return nil
	}

	paths := make([]string, 0, len(owned))
	for path := range owned {
		paths = append(paths, path)
	}

	sort.Strings(paths)

	var problems []string

	for _, path := range paths {
		if !patchSets(entry.Patch, path) {
			continue
		}

		problems = append(problems, fmt.Sprintf(
			"%s: %s is owned by %s on the Site and cannot be overridden; set that field instead",
			at, path, owned[path]))
	}

	return problems
}

// patchSets reports whether a patch assigns a value at a dotted path.
func patchSets(patch map[string]any, path string) bool {
	_, found, err := unstructured.NestedFieldNoCopy(patch, strings.Split(path, ".")...)

	return err == nil && found
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

	report := func(problem string) {
		problems = append(problems, at+": "+problem)
	}

	walkPatch(patch, "", compiledAllowlist.root, report)
	reportAffinityTerms(patch, report)

	return problems
}

// reportAffinityTerms rejects required node affinity that cannot mean anything.
//
// Required terms are ORed, so the operator's terms and the user's are combined
// with a Cartesian product. That product has two degenerate inputs worth
// catching here rather than letting them through:
//
// An empty nodeSelectorTerms list is rejected by the API server anyway ("must
// have at least one node selector term"), but it would first pass through the
// product as an identity, silently leaving the operator's own constraint as
// the entire result. The user would see their affinity accepted and the Site
// report Applied, with nothing having changed.
//
// A term that is not a mapping cannot be combined with anything, and would
// otherwise be dropped from the product without comment.
func reportAffinityTerms(patch map[string]any, report func(string)) {
	const at = "spec.template.spec.affinity.nodeAffinity." +
		"requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms"

	path := append([]string{"spec", "template", "spec", "affinity"}, requiredTermsPath...)

	value, found, err := unstructured.NestedFieldNoCopy(patch, path...)
	if err != nil || !found {
		return
	}

	terms, ok := value.([]any)
	if !ok {
		report(fmt.Sprintf("%s must be a list, but holds %T", at, value))

		return
	}

	if len(terms) == 0 {
		report(at + " is empty; an empty term list matches nothing and is rejected by the API server, so " +
			"remove the field instead of setting it to an empty list")

		return
	}

	for i, term := range terms {
		if _, ok := term.(map[string]any); !ok {
			report(fmt.Sprintf("%s[%d] must be a mapping, but holds %T", at, i, term))
		}
	}
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

		if stringValuedPaths[childPath] {
			reportNonStringValues(value[key], childPath, report)
		}

		if !reportShape(value[key], childPath, report) {
			// The value is the wrong type, so walking into it would produce a
			// cascade of misleading messages about paths that only exist
			// because the shape is wrong.
			continue
		}

		reportMergeKeyTypes(value[key], childPath, report)

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

	// Descend with an explicit wildcard segment so paths below a list read as
	// spec.template.spec.containers.*.env, matching how the allowlist and the
	// merge-key table are written. Without it the path collapses to
	// containers.env and neither table matches.
	elementPath := joinPath(path, wildcard)

	for _, element := range value {
		walkPatch(element, elementPath, child, report)
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

// reportShape rejects a value whose JSON type the Kubernetes schema fixes,
// reporting whether the value may be walked into.
//
// Strategic merge does not police this. A patch writing `containers` as a
// mapping rather than a list merged cleanly and produced an object whose
// containers field was no longer an array; the override was hashed and reported
// Applied, and the apiserver rejected the workload with a decoding error naming
// a Go type. One missing `-` in the YAML is the most ordinary mistake a user
// can make here, and it deserves to be named.
func reportShape(value any, path string, report func(string)) bool {
	// An explicit null is a deletion attempt, which has its own message and a
	// far more important reason for being refused.
	if value == nil {
		return true
	}

	want := shapeOf(path)
	if want == shapeAny || matchesShape(value, want) {
		return true
	}

	report(fmt.Sprintf("%s must be %s, but the patch has %s; check the indentation",
		path, want, describeValueShape(value)))

	return false
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

// reportNonStringValues rejects non-string values in maps Kubernetes requires
// to hold strings.
//
// Beyond being invalid, a single non-string makes unstructured's GetAnnotations
// return nil for the whole map, so the operator would replace every annotation
// with its own bookkeeping instead of merging into them.
func reportNonStringValues(value any, path string, report func(string)) {
	values, ok := value.(map[string]any)
	if !ok {
		return
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		if _, isString := values[key].(string); isString || values[key] == nil {
			continue
		}

		report(fmt.Sprintf("%s must be a string, but holds %T; quote the value",
			joinPath(path, key), values[key]))
	}
}

// reportMergeKeyTypes rejects a merge key of the wrong type.
//
// strategicpatch compares merge keys with Go's == operator, which panics on an
// uncomparable type, so a list element whose key holds a slice or a map would
// crash the operator during the merge rather than fail validation.
func reportMergeKeyTypes(value any, path string, report func(string)) {
	elements, ok := value.([]any)
	if !ok {
		return
	}

	spec, keyed := mergeKeyTypes[mergeKeyPathFor(path)]
	if !keyed || spec.field == "" {
		return
	}

	for _, element := range elements {
		mapped, ok := element.(map[string]any)
		if !ok {
			continue
		}

		raw, present := mapped[spec.field]
		if !present {
			report(fmt.Sprintf("%s has an entry with no %s; strategic merge needs it to identify the entry",
				path, spec.field))

			continue
		}

		if !matchesMergeKeyType(raw, spec.kind) {
			report(fmt.Sprintf("%s entry has %s of type %T, want a %s; strategic merge compares this value and cannot handle other types",
				path, spec.field, raw, spec.kind))
		}
	}
}

func matchesMergeKeyType(value any, kind string) bool {
	switch kind {
	case "string":
		_, ok := value.(string)

		return ok
	case "number":
		switch value.(type) {
		case int64, float64:
			return true
		default:
			return false
		}
	default:
		return false
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
