// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package apiserver

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"

	rebound "github.com/Azure/unbounded/api/rebound/v1alpha1"
)

func TestCreateUnsupportedMetadata(t *testing.T) {
	for _, tc := range []struct {
		field string
		set   func(*metav1.ObjectMeta)
	}{
		{"ownerReferences", func(m *metav1.ObjectMeta) {
			m.OwnerReferences = []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "owner", UID: "owner-uid"}}
		}},
		{"finalizers", func(m *metav1.ObjectMeta) { m.Finalizers = []string{"example.com/cleanup"} }},
		{"deletionTimestamp", func(m *metav1.ObjectMeta) { m.DeletionTimestamp = ptr.To(metav1.Now()) }},
		{"deletionTimestamp", func(m *metav1.ObjectMeta) { m.DeletionTimestamp = &metav1.Time{} }},
		{"deletionGracePeriodSeconds", func(m *metav1.ObjectMeta) { m.DeletionGracePeriodSeconds = ptr.To(int64(30)) }},
		{"deletionGracePeriodSeconds", func(m *metav1.ObjectMeta) { m.DeletionGracePeriodSeconds = ptr.To(int64(0)) }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			s, client, ctx := fixture(t)
			g := grant("test", "node-a")
			tc.set(&g.ObjectMeta)
			client.ClearActions()

			_, err := s.Create(ctx, g, nil, nil)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			status, ok := err.(apierrors.APIStatus)
			require.True(t, ok)
			require.Equal(t, int32(422), status.Status().Code)
			require.Len(t, status.Status().Details.Causes, 1)
			cause := status.Status().Details.Causes[0]
			require.Equal(t, metav1.CauseType(field.ErrorTypeForbidden), cause.Type)
			require.Equal(t, "metadata."+tc.field, cause.Field)
			require.Contains(t, cause.Message, "not supported by NodeGrant")
			require.Empty(t, client.Actions())
			require.Empty(t, cached(t, s).Data)
		})
	}
}

func TestCreateIgnoresServerMetadata(t *testing.T) {
	s, _, ctx := fixture(t)
	g := grant("test", "node-a")
	g.OwnerReferences = []metav1.OwnerReference{}
	g.Finalizers = []string{}
	g.UID = "client-uid"
	g.ResourceVersion = "999"
	g.CreationTimestamp = metav1.NewTime(s.clock.Now().Add(-time.Hour))
	g.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "client", Operation: metav1.ManagedFieldsOperationUpdate}}
	g.Generation = 42
	g.SelfLink = "/client" //nolint:staticcheck // Verify that legacy client metadata is ignored.

	obj, err := s.Create(ctx, g, nil, nil)
	require.NoError(t, err)

	got, ok := obj.(*rebound.NodeGrant)
	require.True(t, ok)
	require.NotEmpty(t, got.UID)
	require.NotEqual(t, g.UID, got.UID)
	require.Equal(t, "2", got.ResourceVersion)
	require.Equal(t, s.clock.Now(), got.CreationTimestamp.Time)
	require.Empty(t, got.ManagedFields)
	require.Zero(t, got.Generation)
	require.Empty(t, got.SelfLink) //nolint:staticcheck // Verify that legacy client metadata is ignored.
	require.Empty(t, got.OwnerReferences)
	require.Empty(t, got.Finalizers)
	require.Equal(t, g.Labels, got.Labels)
	require.Equal(t, g.Annotations, got.Annotations)
	stored, err := s.Get(ctx, g.Name, nil)
	require.NoError(t, err)
	require.Equal(t, got, stored)
}
