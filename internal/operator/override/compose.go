// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// additivePaths are patch paths whose contributions concatenate rather than
// overwrite, so two contributors touching them can never conflict.
//
// Scheduling is additive because the operator's own constraints must survive:
// metalman and storage place their workloads with a mandatory per-Site node
// affinity, and NodeSelectorTerms carries no patchMergeKey, so a raw merge
// would replace it and let two Sites schedule onto the same nodes.
var additivePaths = map[string]bool{
	"spec.template.spec.tolerations":               true,
	"spec.template.spec.topologySpreadConstraints": true,
	"spec.template.spec.affinity":                  true,
}

// mergeKeys names the field each merge-keyed list is identified by, so
// conflict detection compares like with like rather than by list position.
var mergeKeys = map[string]string{
	"spec.template.spec.containers":                    "name",
	"spec.template.spec.initContainers":                "name",
	"spec.template.spec.volumes":                       "name",
	"spec.template.spec.imagePullSecrets":              "name",
	"spec.template.spec.containers.*.env":              "name",
	"spec.template.spec.initContainers.*.env":          "name",
	"spec.template.spec.containers.*.volumeMounts":     "mountPath",
	"spec.template.spec.initContainers.*.volumeMounts": "mountPath",
	"spec.template.spec.containers.*.ports":            "containerPort",
	"spec.template.spec.initContainers.*.ports":        "containerPort",
}

// atomicListPaths are lists strategic merge replaces wholesale, so two
// contributors supplying different lists genuinely disagree.
var atomicListPaths = map[string]bool{
	"spec.template.spec.containers.*.args":        true,
	"spec.template.spec.containers.*.command":     true,
	"spec.template.spec.initContainers.*.args":    true,
	"spec.template.spec.initContainers.*.command": true,
}

// checkConflicts reports where two contributors to the same workload disagree.
//
// "Different values at the same leaf" is not a sufficient definition, because
// several parts of this mechanism are not leaf assignments: tolerations append,
// affinity takes a Cartesian product, extraArgs concatenate, and addContainers
// declares intent. Comparing raw patch leaves would report conflicts that do
// not exist and miss ones that do, so detection runs over normalized
// contributions instead.
//
// Two rules are worth stating outright. Identical values never conflict, since
// two teams independently setting the same memory limit is not an error and
// failing it would make the ownership-split use case unusable. And where
// composition would otherwise be order-dependent, the result is rejected rather
// than resolved: deterministic ordering exists so composition is reproducible,
// not so that silent precedence can be inferred from ConfigMap key names.
func checkConflicts(contributors []SourcedEntry) []error {
	if len(contributors) < 2 {
		return nil
	}

	var problems []error

	problems = append(problems, patchConflicts(contributors)...)
	problems = append(problems, addContainerConflicts(contributors)...)
	problems = append(problems, extraArgsConflicts(contributors)...)

	return problems
}

// extraArgsConflicts rejects two contributors appending arguments to the same
// container.
//
// Unlike a patch leaf, extraArgs concatenates, so two contributors do not
// overwrite one another: both lists land, in sorted ConfigMap key order. That
// makes the outcome depend on what the keys happen to be called, which is
// exactly the silent precedence the deterministic ordering exists to avoid
// rather than to provide. Two teams appending --log-level=debug and
// --log-level=warn both get their way, and which one the component honours is
// decided by its own flag parsing.
//
// Identical lists are rejected too, which is where this differs from the
// identical-values rule for patch leaves. Setting one value twice is the same
// as setting it once; appending the same argument twice is not.
func extraArgsConflicts(contributors []SourcedEntry) []error {
	type claimant struct {
		source Source
	}

	claimed := map[string]claimant{}

	var problems []error

	for _, contributor := range contributors {
		names := make([]string, 0, len(contributor.Entry.ExtraArgs))
		for name := range contributor.Entry.ExtraArgs {
			names = append(names, name)
		}

		sort.Strings(names)

		for _, name := range names {
			existing, seen := claimed[name]
			if !seen {
				claimed[name] = claimant{source: contributor.Source}

				continue
			}

			problems = append(problems, fmt.Errorf(
				"%s and %s both append extraArgs to container %q; arguments concatenate rather than "+
					"overwrite, so the result would depend on ConfigMap key ordering. "+
					"Put every argument for one container in one entry",
				existing.source, contributor.Source, name))
		}
	}

	return problems
}

// claim records which contributor set a path, and to what.
type claim struct {
	value  any
	source Source
}

func patchConflicts(contributors []SourcedEntry) []error {
	claims := map[string]claim{}

	var problems []error

	for _, contributor := range contributors {
		flat := map[string]any{}
		flattenPatch(contributor.Entry.Patch, "", flat)

		paths := make([]string, 0, len(flat))
		for path := range flat {
			paths = append(paths, path)
		}

		sort.Strings(paths)

		for _, path := range paths {
			existing, claimed := claims[path]
			if !claimed {
				claims[path] = claim{value: flat[path], source: contributor.Source}

				continue
			}

			if reflect.DeepEqual(existing.value, flat[path]) {
				continue
			}

			problems = append(problems, fmt.Errorf(
				"%s and %s both set %s to different values; overrides do not resolve disagreement by ordering",
				existing.source, contributor.Source, path))
		}
	}

	return problems
}

// addContainerConflicts reports two contributors adding the same container name
// with different definitions. Adding the same container identically is fine.
func addContainerConflicts(contributors []SourcedEntry) []error {
	type addition struct {
		definition any
		source     Source
	}

	added := map[string]addition{}

	var problems []error

	for _, contributor := range contributors {
		for _, field := range []string{"containers", "initContainers"} {
			for _, container := range patchedContainers(contributor.Entry.Patch, field) {
				name, _ := container["name"].(string) //nolint:errcheck // absent means unnamed
				if name == "" || !declaresAddition(contributor.Entry, field, name) {
					continue
				}

				existing, seen := added[name]
				if !seen {
					added[name] = addition{definition: container, source: contributor.Source}

					continue
				}

				if reflect.DeepEqual(existing.definition, container) {
					continue
				}

				problems = append(problems, fmt.Errorf(
					"%s and %s both add %s %q with different definitions",
					existing.source, contributor.Source, singular(field), name))
			}
		}
	}

	return problems
}

func declaresAddition(entry Entry, field, name string) bool {
	declared := entry.AddContainers
	if field == "initContainers" {
		declared = entry.AddInitContainers
	}

	for _, candidate := range declared {
		if candidate == name {
			return true
		}
	}

	return false
}

// flattenPatch reduces a patch to the set of paths it claims, stopping at any
// path whose composition is additive or whose list is atomic.
//
// Merge-keyed lists descend by key rather than by index, so two contributors
// touching different containers do not collide, and two touching the same
// container are compared field by field.
func flattenPatch(value any, path string, out map[string]any) {
	// Look schema rules up by the un-indexed path, so
	// containers[name=node].env resolves as containers.*.env.
	schemaPath := mergeKeyPathFor(path)

	if additivePaths[schemaPath] {
		// Additive paths concatenate; contributors never conflict there.
		return
	}

	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			flattenPatch(child, joinPath(path, key), out)
		}

	case []any:
		if atomicListPaths[schemaPath] {
			// The whole list is the value, because strategic merge replaces it.
			out[path] = typed

			return
		}

		key, keyed := mergeKeys[schemaPath]
		if !keyed {
			out[path] = typed

			return
		}

		for _, element := range typed {
			mapped, ok := element.(map[string]any)
			if !ok {
				out[path] = typed

				return
			}

			elementPath := fmt.Sprintf("%s[%s=%v]", path, key, mapped[key])

			for field, child := range mapped {
				if field == key {
					continue
				}

				flattenPatch(child, joinPath(elementPath, field), out)
			}
		}

	default:
		out[path] = typed
	}
}

// mergeKeyPathFor normalizes an indexed element path back to its schema path,
// so `containers[name=node].env` looks up as `containers.*.env`.
func mergeKeyPathFor(path string) string {
	var b strings.Builder

	for i := 0; i < len(path); i++ {
		if path[i] != '[' {
			b.WriteByte(path[i])

			continue
		}

		end := strings.IndexByte(path[i:], ']')
		if end < 0 {
			break
		}

		b.WriteString(".*")

		i += end
	}

	return b.String()
}
