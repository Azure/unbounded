// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// knownComponents are the components that generate workloads an override can
// target, and the kinds each one actually emits.
//
// Component and kind used to be validated against separate lists, so seven of
// the ten pairs they accepted between them could not resolve to anything:
// machina emits no DaemonSet, gantry no Deployment. Such an entry validated,
// matched nothing, and was reported as a successfully applied document that
// overrode zero workloads. Naming the mistake is the whole job of validation.
//
// Cluster components are not per-Site, so an entry naming one may not carry a
// sites selector. TestOverrideKindsMatchWhatComponentsPlan holds this table
// against what the components actually mark overridable.
var knownComponents = map[string]struct {
	perSite bool
	kinds   []string
}{
	"net":      {perSite: false, kinds: []string{"DaemonSet", "Deployment"}},
	"machina":  {perSite: false, kinds: []string{"Deployment"}},
	"gantry":   {perSite: false, kinds: []string{"DaemonSet"}},
	"metalman": {perSite: true, kinds: []string{"Deployment"}},
	"storage":  {perSite: true, kinds: []string{"DaemonSet"}},
}

// knownKinds are the workload kinds the operator emits at all.
var knownKinds = map[string]struct{}{
	"Deployment": {},
	"DaemonSet":  {},
}

// ComponentKinds returns the kinds a component emits, for the CLI, the
// documentation and cross-package tests to read rather than restate.
func ComponentKinds(component string) []string {
	known, ok := knownComponents[component]
	if !ok {
		return nil
	}

	out := append([]string(nil), known.kinds...)
	sort.Strings(out)

	return out
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
// a document sees the whole list. Each is attributed to the entry that caused
// it and carries what that entry would have targeted, so a caller can withhold
// those workloads alone rather than every workload an override could reach.
func Validate(entries []SourcedEntry) []Problem {
	var problems []Problem

	for _, sourced := range entries {
		for _, message := range validateEntry(sourced) {
			problems = append(problems, entryProblem(sourced.Source, sourced.Entry, errors.New(message)))
		}
	}

	return problems
}

// ValidateErr reports validation as a single error, for callers that only need
// a yes or no. Attribution is discarded, so anything deciding what to withhold
// must use Validate.
func ValidateErr(entries []SourcedEntry) error {
	return ProblemsError(Validate(entries))
}

// validateEntry returns the problems with one entry, unattributed. Validate
// pairs each with its Source, so the origin is stamped in exactly one place
// rather than threaded through every helper below.
func validateEntry(sourced SourcedEntry) []string {
	var (
		problems []string
		entry    = sourced.Entry
	)

	component, known := knownComponents[entry.Component]

	switch {
	case entry.Component == "":
		problems = append(problems, "component is required")
	case !known:
		problems = append(problems, fmt.Sprintf("unknown component %q, want one of %s",
			entry.Component, strings.Join(sortedComponents(), ", ")))
	}

	switch entry.Kind {
	case "":
		problems = append(problems, "kind is required")
	default:
		if _, ok := knownKinds[entry.Kind]; !ok {
			problems = append(problems, fmt.Sprintf("unsupported kind %q, want Deployment or DaemonSet", entry.Kind))
		} else if known && !slices.Contains(component.kinds, entry.Kind) {
			problems = append(problems, fmt.Sprintf(
				"component %q emits no %s, so this entry can never match anything; it emits %s",
				entry.Component, entry.Kind, strings.Join(ComponentKinds(entry.Component), " and ")))
		}
	}

	problems = append(problems, validateSites(entry, known, component.perSite)...)

	if !entry.HasWork() {
		problems = append(problems, "entry changes nothing; set patch, extraArgs, or both")
	}

	for container, args := range entry.ExtraArgs {
		if container == "" {
			problems = append(problems, "extraArgs has an empty container name")
		}

		if len(args) == 0 {
			problems = append(problems, fmt.Sprintf("extraArgs for container %q is empty", container))
		}
	}

	problems = append(problems, validateAddNames("addContainers", entry.AddContainers)...)
	problems = append(problems, validateAddNames("addInitContainers", entry.AddInitContainers)...)
	problems = append(problems, reportAddedContainers(entry)...)
	problems = append(problems, validatePatch(entry.Patch)...)
	problems = append(problems, reportTypedFieldConflicts(entry)...)

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
func reportTypedFieldConflicts(entry Entry) []string {
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
			"%s is owned by %s on the Site and cannot be overridden; set that field instead",
			path, owned[path]))
	}

	return problems
}

// patchSets reports whether a patch assigns a value at a dotted path.
func patchSets(patch map[string]any, path string) bool {
	_, found, err := unstructured.NestedFieldNoCopy(patch, strings.Split(path, ".")...)

	return err == nil && found
}

// reportAddedContainers checks that declared additions can actually be created.
//
// A name in addContainers with no matching container in the patch creates
// nothing. That was accepted, and extraArgs targeting it was accepted too,
// because extraArgs validates against declarations rather than definitions. The
// entry passed validation, passed resolution, merged cleanly, was hashed, and
// did nothing at all.
//
// A name in both addContainers and addInitContainers is rejected because
// Kubernetes requires container names to be unique across the two lists. Each
// list was checked for duplicates on its own, so the collision between them was
// never seen and the apiserver refused the pod after the override had been
// applied and reported.
func reportAddedContainers(entry Entry) []string {
	var problems []string

	for _, declaration := range []struct {
		field string
		patch string
		names []string
	}{
		{field: "addContainers", patch: "containers", names: entry.AddContainers},
		{field: "addInitContainers", patch: "initContainers", names: entry.AddInitContainers},
	} {
		present := map[string]bool{}
		for _, name := range patchedContainerNames(entry.Patch, declaration.patch) {
			present[name] = true
		}

		for _, name := range declaration.names {
			if !present[name] {
				problems = append(problems, fmt.Sprintf(
					"%s declares %q, but the patch defines no %s with that name, so nothing would be created",
					declaration.field, name, singular(declaration.patch)))
			}
		}
	}

	initNames := map[string]bool{}
	for _, name := range entry.AddInitContainers {
		initNames[name] = true
	}

	for _, name := range entry.AddContainers {
		if initNames[name] {
			problems = append(problems, fmt.Sprintf(
				"%q is declared in both addContainers and addInitContainers; "+
					"Kubernetes requires container names to be unique across both lists", name))
		}
	}

	return problems
}

// validateSites checks the Site selector, which is meaningful only for per-Site
// components.
func validateSites(entry Entry, componentKnown, perSite bool) []string {
	var problems []string

	if entry.Sites == nil {
		return nil
	}

	if len(entry.Sites) == 0 {
		problems = append(problems, "sites is present but empty; omit it to match every Site")

		return problems
	}

	if componentKnown && !perSite {
		problems = append(problems, fmt.Sprintf(
			"component %q is a cluster singleton and is not per-Site, so sites must be omitted",
			entry.Component))
	}

	seen := map[string]bool{}

	for _, site := range entry.Sites {
		switch {
		case site == "":
			problems = append(problems, "sites contains an empty Site name")
		case seen[site]:
			problems = append(problems, fmt.Sprintf("sites lists %q more than once", site))
		}

		seen[site] = true
	}

	return problems
}

func validateAddNames(field string, names []string) []string {
	var problems []string

	seen := map[string]bool{}

	for _, name := range names {
		switch {
		case name == "":
			problems = append(problems, fmt.Sprintf("%s contains an empty container name", field))
		case seen[name]:
			problems = append(problems, fmt.Sprintf("%s lists %q more than once", field, name))
		}

		seen[name] = true
	}

	return problems
}

// validatePatch walks a patch against the allowlist.
func validatePatch(patch map[string]any) []string {
	if len(patch) == 0 {
		return nil
	}

	var problems []string

	report := func(problem string) {
		problems = append(problems, problem)
	}

	walkPatch(patch, "", compiledAllowlist.root, report)
	reportAffinityTerms(patch, report)
	reportAffinityExtras(patch, report)

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
		mapping, ok := term.(map[string]any)
		if !ok {
			report(fmt.Sprintf("%s[%d] must be a mapping, but holds %T", at, i, term))

			continue
		}

		reportTermExpressions(mapping, fmt.Sprintf("%s[%d]", at, i), report)
	}
}

// reportAffinityExtras checks the shape of the affinity sections that are
// concatenated rather than combined into a product.
//
// affinity is a permitted subtree, so the allowlist walker does not descend
// into it and no shape check applies. Only required node affinity was checked,
// so a preference or a pod affinity rule written as a mapping rather than a
// list reached the merge, where it was dropped without comment on any workload
// that already had operator affinity to merge into. Catching it here means the
// user is told which line is wrong rather than discovering the constraint
// silently did nothing.
func reportAffinityExtras(patch map[string]any, report func(string)) {
	const prefix = "spec.template.spec.affinity."

	base := []string{"spec", "template", "spec", "affinity"}

	for _, section := range affinityExtraSections {
		path := append(append([]string{}, base...), section...)

		value, found, err := unstructured.NestedFieldNoCopy(patch, path...)
		if err != nil || !found {
			continue
		}

		at := prefix + strings.Join(section, ".")

		items, ok := value.([]any)
		if !ok {
			report(fmt.Sprintf("%s must be a list, but holds %T", at, value))

			continue
		}

		for i, item := range items {
			if _, ok := item.(map[string]any); !ok {
				report(fmt.Sprintf("%s[%d] must be a mapping, but holds %T", at, i, item))
			}
		}
	}
}

// reportTermExpressions checks the two lists inside a node selector term.
//
// affinity is a permitted subtree, so the allowlist walker does not descend
// into it and no shape check applies. Combining terms then asserted these were
// lists and ignored the failure, so a malformed matchExpressions was discarded
// silently: the user's constraint vanished, the override was hashed, and the
// Site reported Applied. Worse, a term whose only field was malformed became an
// empty term, which matches every node, so a constraint meant to narrow
// scheduling widened it instead.
func reportTermExpressions(term map[string]any, at string, report func(string)) {
	for _, field := range []string{"matchExpressions", "matchFields"} {
		value, present := term[field]
		if !present {
			continue
		}

		items, ok := value.([]any)
		if !ok {
			report(fmt.Sprintf("%s.%s must be a list, but holds %T", at, field, value))

			continue
		}

		for i, item := range items {
			if _, ok := item.(map[string]any); !ok {
				report(fmt.Sprintf("%s.%s[%d] must be a mapping, but holds %T", at, field, i, item))
			}
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
			report(fmt.Sprintf("%s is a strategic merge directive; directives can delete operator-managed content and are not accepted",
				describePath(childPath)))

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
				report(fmt.Sprintf("%s is a strategic merge directive; directives can delete operator-managed content and are not accepted",
					describePath(joinPath(path, key))))

				continue
			}

			walkSubtree(typed[key], joinPath(path, key), report)
		}
	case []any:
		// Carry the index, so a problem several levels inside a list can be
		// found. Without it every element reported against the same path.
		for i, element := range typed {
			walkSubtree(element, fmt.Sprintf("%s[%d]", path, i), report)
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
