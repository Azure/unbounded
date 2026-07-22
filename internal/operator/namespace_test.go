// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/unbounded/internal/unbounded"
)

func namespaceTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func getNamespace(t *testing.T, c client.Client, name string) *corev1.Namespace {
	t.Helper()

	ns := &corev1.Namespace{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, ns); err != nil {
		t.Fatalf("get namespace %s: %v", name, err)
	}

	return ns
}

func TestBootstrapNamespaceStampsCanonicalLabels(t *testing.T) {
	c := namespaceTestClient(t)

	if err := BootstrapNamespace(context.Background(), c, unbounded.DefaultSystemNamespace); err != nil {
		t.Fatalf("BootstrapNamespace: %v", err)
	}

	ns := getNamespace(t, c, unbounded.DefaultSystemNamespace)
	for key, want := range unbounded.SystemNamespaceLabels() {
		if ns.Labels[key] != want {
			t.Fatalf("namespace label %q = %q, want %q", key, ns.Labels[key], want)
		}
	}
}

func TestBootstrapNamespaceIsIdempotent(t *testing.T) {
	c := namespaceTestClient(t)

	for i := 0; i < 2; i++ {
		if err := BootstrapNamespace(context.Background(), c, unbounded.DefaultSystemNamespace); err != nil {
			t.Fatalf("BootstrapNamespace pass %d: %v", i, err)
		}
	}

	ns := getNamespace(t, c, unbounded.DefaultSystemNamespace)
	for key, want := range unbounded.SystemNamespaceLabels() {
		if ns.Labels[key] != want {
			t.Fatalf("namespace label %q = %q, want %q", key, ns.Labels[key], want)
		}
	}
}

// TestBootstrapNamespacePreservesForeignLabelsAndAnnotations is the core
// preservation guarantee: labels and annotations placed on the namespace by
// another actor survive a bootstrap, while the operator reasserts its own keys.
//
// The foreign keys are applied under a distinct field manager (not seeded as
// unowned metadata) so this exercises the real multi-manager server-side-apply
// path: the operator's ForceOwnership apply must reassert only the keys it
// declares and leave keys owned by another manager untouched.
func TestBootstrapNamespacePreservesForeignLabelsAndAnnotations(t *testing.T) {
	foreignAnnotations := map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": "{}",
		"argocd.argoproj.io/tracking-id":                   "unbounded:/Namespace:unbounded-system",
	}

	c := namespaceTestClient(t)

	// A third party (GitOps tool) owns example.com/team and deliberately
	// tightened PSA to restricted; the operator owns the PSA key and must
	// reassert privileged (the workloads here require it).
	applyAsForeignManager(t, c, "gitops", unbounded.DefaultSystemNamespace, map[string]string{
		"example.com/team":                   "platform",
		"pod-security.kubernetes.io/enforce": "restricted",
	}, foreignAnnotations)

	if err := BootstrapNamespace(context.Background(), c, unbounded.DefaultSystemNamespace); err != nil {
		t.Fatalf("BootstrapNamespace: %v", err)
	}

	ns := getNamespace(t, c, unbounded.DefaultSystemNamespace)

	// Foreign labels survive.
	if got := ns.Labels["example.com/team"]; got != "platform" {
		t.Fatalf("foreign label example.com/team = %q, want platform (must be preserved)", got)
	}

	// Every foreign annotation survives untouched.
	for key, want := range foreignAnnotations {
		if got := ns.Annotations[key]; got != want {
			t.Fatalf("annotation %q = %q, want %q (annotations must be preserved)", key, got, want)
		}
	}

	// The operator reasserts its own keys, including correcting the PSA level.
	for key, want := range unbounded.SystemNamespaceLabels() {
		if ns.Labels[key] != want {
			t.Fatalf("operator label %q = %q, want %q (operator is authoritative on its own keys)", key, ns.Labels[key], want)
		}
	}
}

// applyAsForeignManager server-side applies the namespace's labels and
// annotations under fieldManager, so those keys are genuinely owned by a manager
// other than the operator. This models an admin, GitOps tool, or policy engine
// that maintains its own keys on the shared namespace.
func applyAsForeignManager(t *testing.T, c client.Client, fieldManager, namespace string, labels, annotations map[string]string) {
	t.Helper()

	meta := map[string]any{"name": namespace}
	if len(labels) > 0 {
		meta["labels"] = toAnyMap(labels)
	}

	if len(annotations) > 0 {
		meta["annotations"] = toAnyMap(annotations)
	}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   meta,
	}}

	applyCfg := client.ApplyConfigurationFromUnstructured(obj)
	if err := c.Apply(context.Background(), applyCfg, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		t.Fatalf("apply namespace as %q: %v", fieldManager, err)
	}
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}

	return out
}

func TestBootstrapNamespaceRefusesLegacyNamespace(t *testing.T) {
	c := namespaceTestClient(t)

	if err := BootstrapNamespace(context.Background(), c, unbounded.LegacyKubeNamespace); err == nil {
		t.Fatal("BootstrapNamespace must refuse a legacy namespace")
	}
}

func TestBootstrapNamespaceDefaultsEmptyNamespace(t *testing.T) {
	// Pin POD_NAMESPACE empty so the empty-namespace argument exercises the
	// documented fallback to the build default rather than whatever namespace
	// the test process happens to run in.
	t.Setenv("POD_NAMESPACE", "")

	c := namespaceTestClient(t)

	if err := BootstrapNamespace(context.Background(), c, ""); err != nil {
		t.Fatalf("BootstrapNamespace with empty namespace: %v", err)
	}

	// With POD_NAMESPACE unset, an empty namespace falls back to the build
	// default (DefaultSystemNamespace).
	getNamespace(t, c, unbounded.DefaultSystemNamespace)
}
