// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/unbounded"
)

// BootstrapNamespace server-side applies the shared system namespace with its
// canonical identity and Pod Security Admission labels
// (unbounded.SystemNamespaceLabels). It is the single owner of that namespace
// object: components skip Namespace objects during reconcile (see
// component.NamespaceKind), so this is the only place the operator writes it.
//
// Preservation contract: the apply declares only the operator's own label keys
// and no annotations. Server-side apply tracks ownership per map key for
// metadata.labels and metadata.annotations, so labels and annotations placed on
// the namespace by any other actor (an admin, a GitOps tool, a policy engine)
// are left untouched. ForceOwnership only makes the operator authoritative for
// the keys it declares; it reasserts those (for example pod-security enforce =
// privileged, which the privileged/hostPath workloads here require) without
// disturbing anything else.
//
// It creates the namespace if absent and is idempotent, so it is safe to run on
// every operator start and on the BootstrapMaintainer interval.
func BootstrapNamespace(ctx context.Context, c client.Client, namespace string) error {
	if namespace == "" {
		namespace = unbounded.SystemNamespace()
	}

	// Installing into a pre-consolidation namespace is unsafe: the migration
	// reaper drains and deletes those. Refuse rather than stamp labels on a
	// namespace that is about to be removed.
	if unbounded.IsLegacyNamespace(namespace) {
		return fmt.Errorf("refusing to bootstrap legacy namespace %q", namespace)
	}

	labels := make(map[string]any)
	for key, value := range unbounded.SystemNamespaceLabels() {
		labels[key] = value
	}

	// Build a minimal object: only name and the operator-owned labels. Declaring
	// nothing else (no annotations, no spec/status) keeps the operator's managed
	// field set to exactly these label keys, which is what preserves everyone
	// else's labels and annotations under server-side apply.
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       component.NamespaceKind,
		"metadata": map[string]any{
			"name":   namespace,
			"labels": labels,
		},
	}}

	applyCfg := client.ApplyConfigurationFromUnstructured(obj)
	if err := c.Apply(ctx, applyCfg, client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply namespace %s: %w", namespace, err)
	}

	log.FromContext(ctx).WithName("namespace-bootstrap").Info("applied system namespace", "namespace", namespace)

	return nil
}
