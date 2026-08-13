// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestDecodeManifestFSDropsNamespaces is a regression test.
//
// Every component's manifests carry a Namespace so each set stays installable
// on its own with kubectl, and every component reconciled the one it shipped.
// The manifests do not agree on its labels and they are applied under a single
// field owner, so the label flipped on every pass depending on which component
// was planned last. Server-side apply makes that a write loop rather than a
// race that settles, since an owner that stops declaring a field removes it.
//
// The drop is here rather than in each component's mutator so a component
// cannot opt back into the fight by forgetting about it.
func TestDecodeManifestFSDropsNamespaces(t *testing.T) {
	manifests := fstest.MapFS{
		"00-namespace.yaml": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: Namespace
metadata:
  name: ` + BuildDefaultNamespace + `
  labels:
    app.kubernetes.io/name: some-component
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: agent
  namespace: ` + BuildDefaultNamespace + `
`)},
	}

	env := &Env{Namespace: BuildDefaultNamespace}

	objects, err := env.DecodeManifestFS(manifests, nil)
	if err != nil {
		t.Fatalf("DecodeManifestFS: %v", err)
	}

	if len(objects) != 1 || objects[0].GetKind() != "ServiceAccount" {
		t.Fatalf("decoded %d object(s), want only the ServiceAccount", len(objects))
	}
}

// TestNamespaceOperationTargetsTheConfiguredNamespace checks that the canonical
// owner follows the operator's namespace rather than the build-time default,
// since the operator can be installed elsewhere.
func TestNamespaceOperationTargetsTheConfiguredNamespace(t *testing.T) {
	op := NamespaceOperation("elsewhere")

	if op.Object.GetName() != "elsewhere" {
		t.Fatalf("name = %q, want elsewhere", op.Object.GetName())
	}

	if op.Object.GetKind() != NamespaceKind {
		t.Fatalf("kind = %q, want %s", op.Object.GetKind(), NamespaceKind)
	}

	// The executor rejects an object with no GVK, and ToUnstructured on a typed
	// object is exactly where TypeMeta is easy to lose.
	if op.Object.GetAPIVersion() == "" {
		t.Fatal("namespace operation carries no apiVersion")
	}

	if !strings.Contains(op.Component, "operator") {
		t.Fatalf("component = %q, want the operation attributed to the operator", op.Component)
	}
}

// TestNamespaceOperationRunsBeforeAnythingInIt pins the ordering the single
// owner depends on. It is planned first, but it must be ordered first even if
// it were not.
func TestNamespaceOperationRunsBeforeAnythingInIt(t *testing.T) {
	env, attempted := applyEnv(t, nil)

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: daemonSetObject("agent"), Component: "a"},
		NamespaceOperation(DefaultNamespace),
	)

	if _, err := env.Execute(t.Context(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *attempted, []string{
		"Namespace/" + DefaultNamespace,
		"DaemonSet/agent",
	})
}
