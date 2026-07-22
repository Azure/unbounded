//go:build e2e

// This file holds the kind-based integration test for BootstrapNamespace. It
// validates the server-side-apply preservation contract against a real API
// server (real corev1 Namespace schema, real granular per-key field ownership),
// which the fake-client unit tests can only approximate. See
// internal/operator/namespace.go for the contract under test.
//
// Guarded by `//go:build e2e` like the rest of this package; run via
// `go test -tags=e2e ./e2e/operator/...`.
package operatore2e

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator"
	"github.com/Azure/unbounded/internal/unbounded"
)

const namespaceClusterName = "operator-namespace-e2e"

// TestBootstrapNamespacePreservationAgainstAPIServer is the real-apiserver
// counterpart to the fake-client preservation unit test. It stages the shared
// system namespace under a distinct field manager (a stand-in for an admin,
// GitOps tool, or policy engine) that owns a foreign label, a foreign
// annotation, and a deliberately tightened PSA level, then runs
// BootstrapNamespace and asserts:
//
//   - the foreign label and annotation survive untouched (per-key ownership),
//   - the operator reasserts its own keys, correcting the PSA level back to
//     privileged, and
//   - the operator's managed-field set is exactly its four label keys and no
//     annotations, which is what makes the preservation guarantee hold.
func TestBootstrapNamespacePreservationAgainstAPIServer(t *testing.T) {
	requireBins(t, "kind", "docker")

	kubeconfig := createClusterNamed(t, namespaceClusterName)
	cli := newClient(t, kubeconfig)
	ctx := context.Background()

	const (
		foreignManager    = "gitops-e2e"
		foreignLabelKey   = "example.com/team"
		foreignLabelValue = "platform"
		foreignAnnoKey    = "argocd.argoproj.io/tracking-id"
		foreignAnnoValue  = "unbounded:/Namespace:unbounded-system"
		psaEnforceKey     = "pod-security.kubernetes.io/enforce"
	)

	namespace := unbounded.DefaultSystemNamespace

	// A third-party manager creates the namespace with its own label + annotation
	// and a tightened PSA level. Because it applies under its own field manager,
	// the apiserver records it as the owner of these keys.
	applyForeignNamespace(ctx, t, cli, foreignManager, namespace, map[string]string{
		foreignLabelKey: foreignLabelValue,
		psaEnforceKey:   "restricted",
	}, map[string]string{
		foreignAnnoKey: foreignAnnoValue,
	})

	if err := operator.BootstrapNamespace(ctx, cli, namespace); err != nil {
		t.Fatalf("BootstrapNamespace: %v", err)
	}

	ns := &corev1.Namespace{}
	if err := cli.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		t.Fatalf("get namespace %s: %v", namespace, err)
	}

	// The foreign label survives.
	if got := ns.Labels[foreignLabelKey]; got != foreignLabelValue {
		t.Fatalf("foreign label %q = %q, want %q (must be preserved)", foreignLabelKey, got, foreignLabelValue)
	}

	// The foreign annotation survives.
	if got := ns.Annotations[foreignAnnoKey]; got != foreignAnnoValue {
		t.Fatalf("foreign annotation %q = %q, want %q (must be preserved)", foreignAnnoKey, got, foreignAnnoValue)
	}

	// The operator reasserts its own keys, including correcting the PSA level
	// back to privileged.
	for key, want := range unbounded.SystemNamespaceLabels() {
		if got := ns.Labels[key]; got != want {
			t.Fatalf("operator label %q = %q, want %q (operator is authoritative on its own keys)", key, got, want)
		}
	}

	assertOperatorManagesOnlyItsLabels(t, ns, foreignManager)
}

// applyForeignNamespace server-side applies the namespace's labels and
// annotations under fieldManager, so the apiserver records those keys as owned
// by a manager other than the operator.
func applyForeignNamespace(ctx context.Context, t *testing.T, cli client.Client, fieldManager, namespace string, labels, annotations map[string]string) {
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
	if err := cli.Apply(ctx, applyCfg, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
		t.Fatalf("apply namespace as %q: %v", fieldManager, err)
	}
}

// assertOperatorManagesOnlyItsLabels inspects metadata.managedFields and proves
// the operator's ownership is scoped to exactly its label keys: it owns every
// key in SystemNamespaceLabels, owns no annotations, and the foreign manager
// still has an ownership entry. This is the apiserver-level evidence behind the
// preservation contract.
func assertOperatorManagesOnlyItsLabels(t *testing.T, ns *corev1.Namespace, foreignManager string) {
	t.Helper()

	var operatorFields, foreignFields *metav1.FieldsV1

	for i := range ns.ManagedFields {
		entry := ns.ManagedFields[i]
		switch entry.Manager {
		case operator.FieldOwner:
			if entry.Operation == metav1.ManagedFieldsOperationApply {
				operatorFields = entry.FieldsV1
			}
		case foreignManager:
			foreignFields = entry.FieldsV1
		}
	}

	if operatorFields == nil {
		t.Fatalf("no Apply managed-fields entry for operator manager %q; managedFields=%+v", operator.FieldOwner, ns.ManagedFields)
	}

	if foreignFields == nil {
		t.Fatalf("foreign manager %q lost its managed-fields entry; it must retain ownership of its own keys", foreignManager)
	}

	metaFields := managedMetadataFields(t, operatorFields)

	labelFields, ok := metaFields["f:labels"].(map[string]any)
	if !ok {
		t.Fatalf("operator manages no labels; f:metadata=%v", metaFields)
	}

	for key := range unbounded.SystemNamespaceLabels() {
		if _, owned := labelFields["f:"+key]; !owned {
			t.Fatalf("operator does not own label %q; owned label fields=%v", key, labelFields)
		}
	}

	if _, hasAnnotations := metaFields["f:annotations"]; hasAnnotations {
		t.Fatalf("operator must not own any annotations; f:metadata=%v", metaFields)
	}
}

// managedMetadataFields unmarshals a FieldsV1 blob and returns the f:metadata
// sub-tree (the map of owned metadata field paths).
func managedMetadataFields(t *testing.T, fields *metav1.FieldsV1) map[string]any {
	t.Helper()

	set := map[string]any{}
	if err := json.Unmarshal(fields.GetRawBytes(), &set); err != nil {
		t.Fatalf("unmarshal managed fields: %v", err)
	}

	meta, ok := set["f:metadata"].(map[string]any)
	if !ok {
		t.Fatalf("managed fields have no f:metadata sub-tree; set=%v", set)
	}

	return meta
}

func toAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}

	return out
}
