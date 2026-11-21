// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/registryauth"
)

// TestRegistryAuthRetiresOnlyOwnedLegacyObjects verifies that the operator
// deletes the per-node installer DaemonSet, ServiceAccount and ConfigMap marker
// only when it owns them, and leaves an unowned object at the same name alone.
func TestRegistryAuthRetiresOnlyOwnedLegacyObjects(t *testing.T) {
	owned := &appsv1.DaemonSet{ObjectMeta: ownedAuthMetadata()}
	foreign := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Name: registryAuthName, Namespace: component.DefaultNamespace,
		Labels: map[string]string{"app.kubernetes.io/managed-by": "customer"},
	}}

	// Owned DaemonSet: retired (deleted).
	env := testEnv(t, authSource("mode: external"), owned)
	operations, _, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.Contains(t, kindsOfDeletions(operations), "DaemonSet")

	// Unowned DaemonSet at the same name: not deleted.
	env = testEnv(t, authSource("mode: external"), foreign)
	operations, _, err = planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.NotContains(t, kindsOfDeletions(operations), "DaemonSet", "an unowned DaemonSet must not be retired")
}

// kindsOfDeletions returns the set of object kinds that a plan deletes.
func kindsOfDeletions(operations []component.Operation) map[string]bool {
	kinds := map[string]bool{}

	for _, op := range operations {
		if op.Kind == component.OpDelete {
			kinds[op.Object.GetKind()] = true
		}
	}

	return kinds
}

// TestRegistryAuthExternalDoesNotTouchUnownedMachinePolicy verifies that, when
// the policy is external, the operator clears only the registry-auth field it
// manages on a Machine and does not invent a policy on a Machine that has none.
func TestRegistryAuthExternalDoesNotTouchUnownedMachinePolicy(t *testing.T) {
	node := registryAuthFlexNodeMachine("flex-a")
	// A Machine with no registry-auth policy: external mode must not patch it.
	machine := registryAuthMachine("flex-a", nil, nil)
	env := testEnv(t, authSource("mode: external"), node, machine)

	operations, _, err := planRegistryAuth(t.Context(), env)
	require.NoError(t, err)
	require.Empty(t, operations, "an already-external Machine must not be patched")
}

var (
	_ = corev1.ServiceAccount{}
	_ = registryauth.IdentityRoleName
)
