// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"slices"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
)

// TestEveryStructuralPathDeclaresAShape stops pathShapes falling behind
// permittedPaths.
//
// The shape check is what turns a missing `-` in the YAML into a message naming
// the field and the indentation, instead of a merge that succeeds, an override
// reported Applied, and an apiserver decoding error naming a Go type. A
// permitted path with no declared shape silently loses that, so the requirement
// is enforced here rather than left to whoever edits the list.
func TestEveryStructuralPathDeclaresAShape(t *testing.T) {
	var walk func(node *pathNode, path string)

	walk = func(node *pathNode, path string) {
		_, hasWildcard := node.children[wildcard]

		// A subtree node is a whole object or list the patch supplies wholesale;
		// a node with a wildcard child is a list or a user-keyed map. Either
		// way its type is fixed by the Kubernetes schema.
		if path != "" && (node.subtree || hasWildcard) && shapeOf(path) == shapeAny {
			t.Errorf("%s is structural but declares no shape; add it to pathShapes", path)
		}

		for name, child := range node.children {
			walk(child, joinPath(path, name))
		}
	}

	walk(compiledAllowlist.root, "")
}

// TestInitContainersMatchContainers pins the parity the comment in the
// allowlist claims.
//
// Kubernetes uses one Container type for both, and a native sidecar is an init
// container that runs for the life of the pod, so it needs probes, lifecycle
// hooks and ports exactly as an ordinary container does. The two lists drifted
// apart once already: initContainers were missing eight fields the comment said
// they had.
func TestInitContainersMatchContainers(t *testing.T) {
	const (
		containerPrefix = "spec.template.spec.containers.*."
		initPrefix      = "spec.template.spec.initContainers.*."
	)

	// restartPolicy is the one deliberate asymmetry: on an init container it
	// declares a native sidecar, and Kubernetes rejects it on an ordinary one.
	exempt := map[string]bool{"restartPolicy": true}

	permitted := map[string]bool{}
	for _, entry := range permittedPaths {
		permitted[entry.path] = true
	}

	for _, entry := range permittedPaths {
		switch {
		case strings.HasPrefix(entry.path, containerPrefix):
			field := strings.TrimPrefix(entry.path, containerPrefix)
			if !exempt[field] && !permitted[initPrefix+field] {
				t.Errorf("containers permit %q but initContainers do not", field)
			}

		case strings.HasPrefix(entry.path, initPrefix):
			field := strings.TrimPrefix(entry.path, initPrefix)
			if !exempt[field] && !permitted[containerPrefix+field] {
				t.Errorf("initContainers permit %q but containers do not", field)
			}
		}
	}
}

// TestMergeKeyTypesAreReachable stops the merge-key table naming paths a patch
// can never contain, which would look like protection while providing none.
func TestMergeKeyTypesAreReachable(t *testing.T) {
	for path := range mergeKeyTypes {
		node := compiledAllowlist.root

		for _, element := range strings.Split(path, ".") {
			if node = node.lookup(element); node == nil {
				break
			}
		}

		if node == nil {
			t.Errorf("mergeKeyTypes names %q, which no patch can reach; either permit it or drop the entry", path)
		}
	}
}

// TestStringValuedPathsAreReachable does the same for the string-value table.
func TestStringValuedPathsAreReachable(t *testing.T) {
	for path := range stringValuedPaths {
		node := compiledAllowlist.root

		for _, element := range strings.Split(path, ".") {
			if node = node.lookup(element); node == nil {
				break
			}
		}

		if node == nil {
			t.Errorf("stringValuedPaths names %q, which no patch can reach", path)
		}
	}
}

// TestNoPermittedPathIsBuriedUnderASubtree catches an entry that can never be
// consulted because an ancestor already permits everything below it. Such an
// entry reads as a deliberate narrowing while having no effect at all.
func TestNoPermittedPathIsBuriedUnderASubtree(t *testing.T) {
	for _, outer := range permittedPaths {
		if !outer.subtree {
			continue
		}

		for _, inner := range permittedPaths {
			if inner.path != outer.path && strings.HasPrefix(inner.path, outer.path+".") {
				t.Errorf("%q is below the subtree %q and is never consulted", inner.path, outer.path)
			}
		}
	}
}

// TestEveryReachableMergeKeyedListIsTypeChecked pins the invariant that keeps
// the operator from panicking on a validated document.
//
// strategicpatch compares merge keys with Go's == operator
// (findMapInSliceBasedOnKeyValue), which panics at runtime on an uncomparable
// type, so a list element whose key holds a slice or a map crashes the merge.
// mergeKeyTypes exists to reject that before the merge runs.
//
// TestMergeKeyTypesAreReachable checks the other direction, that every entry in
// the table is reachable. That one catches a stale entry. This one catches the
// dangerous case: a merge-keyed list a patch can reach with no entry in the
// table at all. Adding spec.template.spec.hostAliases or volumeDevices to the
// allowlist would reintroduce the panic, and nothing else would notice.
func TestEveryReachableMergeKeyedListIsTypeChecked(t *testing.T) {
	for _, target := range []any{&appsv1.Deployment{}, &appsv1.DaemonSet{}} {
		schema, err := strategicpatch.NewPatchMetaFromStruct(target)
		if err != nil {
			t.Fatalf("load patch schema: %v", err)
		}

		walkMergeKeyedLists(t, schema, compiledAllowlist.root, "")
	}
}

// walkMergeKeyedLists descends the strategic-merge schema alongside the
// allowlist, visiting only what a patch can actually reach.
func walkMergeKeyedLists(t *testing.T, schema strategicpatch.LookupPatchMeta, node *pathNode, path string) {
	t.Helper()

	if node == nil || path != "" && strings.Count(path, ".") > 12 {
		return
	}

	for _, name := range childNames(node) {
		child := node.lookup(name)
		childPath := joinPath(path, name)

		fieldSchema, meta, err := schema.LookupPatchMetadataForStruct(name)
		if err != nil {
			// Not a struct field: a map key such as a label name, or a
			// wildcard standing for a list index. Neither is a merge-keyed
			// list of its own.
			continue
		}

		if slices.Contains(meta.GetPatchStrategies(), "merge") {
			if _, checked := mergeKeyTypes[childPath]; !checked {
				t.Errorf("%s is a merge-keyed list (key %q) that a patch can reach, but mergeKeyTypes has no entry for it; "+
					"strategicpatch compares that key with == and panics on an uncomparable value",
					childPath, meta.GetPatchMergeKey())
			}
		}

		// Descend through the list-element wildcard when there is one, so
		// containers.*.env is reached as the allowlist writes it.
		elementSchema, _, elementErr := schema.LookupPatchMetadataForSlice(name)
		if elementErr == nil && child.lookup(wildcard) != nil {
			walkMergeKeyedLists(t, elementSchema, child.lookup(wildcard), joinPath(childPath, wildcard))

			continue
		}

		walkMergeKeyedLists(t, fieldSchema, child, childPath)
	}
}

// childNames returns a node's children in a stable order, expanding a subtree
// into the schema fields below it so fail-open subtrees are still walked.
func childNames(node *pathNode) []string {
	names := make([]string, 0, len(node.children))
	for name := range node.children {
		if name != wildcard {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	return names
}
