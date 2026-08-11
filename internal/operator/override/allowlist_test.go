// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"strings"
	"testing"
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
