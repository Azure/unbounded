// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
func TestBootstrapNamespacePreservesForeignLabelsAndAnnotations(t *testing.T) {
	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: unbounded.DefaultSystemNamespace,
			Labels: map[string]string{
				"example.com/team": "platform",
				// A third party deliberately tightened PSA; the operator owns
				// this key and must reassert privileged (the workloads here
				// require it).
				"pod-security.kubernetes.io/enforce": "restricted",
			},
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": "{}",
				"argocd.argoproj.io/tracking-id":                   "unbounded:/Namespace:unbounded-system",
			},
		},
	}

	c := namespaceTestClient(t, existing)

	if err := BootstrapNamespace(context.Background(), c, unbounded.DefaultSystemNamespace); err != nil {
		t.Fatalf("BootstrapNamespace: %v", err)
	}

	ns := getNamespace(t, c, unbounded.DefaultSystemNamespace)

	// Foreign labels survive.
	if got := ns.Labels["example.com/team"]; got != "platform" {
		t.Fatalf("foreign label example.com/team = %q, want platform (must be preserved)", got)
	}

	// Every foreign annotation survives untouched.
	for key, want := range existing.Annotations {
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

func TestBootstrapNamespaceRefusesLegacyNamespace(t *testing.T) {
	c := namespaceTestClient(t)

	if err := BootstrapNamespace(context.Background(), c, unbounded.LegacyKubeNamespace); err == nil {
		t.Fatal("BootstrapNamespace must refuse a legacy namespace")
	}
}

func TestBootstrapNamespaceDefaultsEmptyNamespace(t *testing.T) {
	c := namespaceTestClient(t)

	if err := BootstrapNamespace(context.Background(), c, ""); err != nil {
		t.Fatalf("BootstrapNamespace with empty namespace: %v", err)
	}

	// SystemNamespace() falls back to the default when POD_NAMESPACE is unset.
	getNamespace(t, c, unbounded.SystemNamespace())
}
