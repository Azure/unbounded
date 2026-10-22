// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/unbounded/internal/operator/component"
)

func webhookService(ns string, family corev1.IPFamily) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: webhookName, Namespace: ns},
		Spec:       corev1.ServiceSpec{IPFamilies: []corev1.IPFamily{family}},
	}
}

func webhookSlice(ns string, family discoveryv1.AddressType, labels map[string]string) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: webhookName, Namespace: ns, UID: "old-slice", Labels: labels},
		AddressType: family,
	}
}

func operatorSliceLabels() map[string]string {
	return map[string]string{
		discoveryv1.LabelServiceName: webhookName,
		discoveryv1.LabelManagedBy:   sliceManager,
		managerLabel:                 component.FieldOwner,
	}
}

func planSlice(t *testing.T, env *component.Env) *component.Plan {
	t.Helper()

	plan := component.NewPlan()
	require.NoError(t, planEndpointSlice(t.Context(), env, plan))

	return plan
}

func TestEndpointSliceMatchingFamilyIsKept(t *testing.T) {
	ns := "custom-system"
	env := testEnv(t, webhookService(ns, corev1.IPv6Protocol), webhookSlice(ns, discoveryv1.AddressTypeIPv6, operatorSliceLabels()))

	require.Zero(t, planSlice(t, env).Len())
}

func TestEndpointSliceReplacedWhenServiceFamilyChanges(t *testing.T) {
	ns := "custom-system"
	env := testEnv(t, webhookService(ns, corev1.IPv6Protocol), webhookSlice(ns, discoveryv1.AddressTypeIPv4, operatorSliceLabels()))

	plan := planSlice(t, env)
	require.Equal(t, 2, plan.Len())
	require.Equal(t, component.OpDelete, plan.Operations[0].Kind)
	require.Equal(t, component.OpCreateIfAbsent, plan.Operations[1].Kind)

	persist(t, env, plan)

	slice := &discoveryv1.EndpointSlice{}
	require.NoError(t, env.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: webhookName}, slice))
	require.Equal(t, discoveryv1.AddressTypeIPv6, slice.AddressType)
	require.NotEqual(t, "old-slice", string(slice.UID))
	require.Equal(t, sliceManager, slice.Labels[discoveryv1.LabelManagedBy])

	require.Zero(t, planSlice(t, env).Len(), "replacement converges")
}

func TestEndpointSliceWithoutOperatorLabelsIsNotDeleted(t *testing.T) {
	ns := "custom-system"
	// Adoptable through the manager annotation, but not labeled as created by
	// the operator: report it rather than deleting it.
	slice := webhookSlice(ns, discoveryv1.AddressTypeIPv4, nil)
	slice.Annotations = managed()
	env := testEnv(t, webhookService(ns, corev1.IPv6Protocol), slice)

	plan := component.NewPlan()
	require.Error(t, planEndpointSlice(t.Context(), env, plan))
	require.Zero(t, plan.Len())
}

func TestEndpointSliceWaitsForService(t *testing.T) {
	ns := "custom-system"

	env := testEnv(t, webhookSlice(ns, discoveryv1.AddressTypeIPv4, operatorSliceLabels()))
	require.Zero(t, planSlice(t, env).Len())

	env = testEnv(t)
	require.Zero(t, planSlice(t, env).Len())
	require.True(t, apierrors.IsNotFound(env.Client.Get(t.Context(), client.ObjectKey{Namespace: ns, Name: webhookName}, &discoveryv1.EndpointSlice{})))
}
