// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/members"
)

func TestVolumeChanges(t *testing.T) {
	volume := racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: "cache", UID: testNodeUID}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
	p := volumeChanges()
	require.True(t, p.Create(event.CreateEvent{Object: &volume}))
	require.True(t, p.Delete(event.DeleteEvent{Object: &volume}))

	for _, tt := range []struct {
		name   string
		mutate func(*racerv1.ClusterVolume)
		want   bool
	}{
		{"unchanged", func(*racerv1.ClusterVolume) {}, false},
		{"resource version", func(v *racerv1.ClusterVolume) { v.ResourceVersion = "2" }, false},
		{"labels", func(v *racerv1.ClusterVolume) { v.Labels = map[string]string{"test": "value"} }, false},
		{"uid", func(v *racerv1.ClusterVolume) { v.UID = testOtherUID }, true},
		{"name", func(v *racerv1.ClusterVolume) { v.Name = "other" }, true},
		{"type", func(v *racerv1.ClusterVolume) { v.Spec.Type = "Future" }, true},
		{"zero type", func(v *racerv1.ClusterVolume) { v.Spec.Type = "" }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			updated := volume.DeepCopy()
			tt.mutate(updated)
			require.Equal(t, tt.want, p.Update(event.UpdateEvent{ObjectOld: &volume, ObjectNew: updated}))
		})
	}

	require.True(t, p.Update(event.UpdateEvent{ObjectOld: &corev1.Node{}, ObjectNew: &volume}))
}

func admissionVolume(name string, gvk schema.GroupVersionKind) *unstructured.Unstructured {
	v := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"type": "Cache"},
	}}
	v.SetGroupVersionKind(gvk)

	return v
}

func cleanupAdmissionObject(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		require.NoError(t, c.Delete(ctx, obj))
	})
}

func integrationVolumeTypeAdmission(t *testing.T, c client.Client) {
	t.Helper()

	for _, tt := range []struct {
		name   string
		mutate func(*unstructured.Unstructured)
		want   string
	}{
		{"cache", func(*unstructured.Unstructured) {}, ""},
		{"missing-spec", func(v *unstructured.Unstructured) { delete(v.Object, "spec") }, "spec: Required value"},
		{"null-spec", func(v *unstructured.Unstructured) { v.Object["spec"] = nil }, "spec: Required value"},
		{"missing-type", func(v *unstructured.Unstructured) { v.Object["spec"] = map[string]any{} }, "spec.type: Required value"},
		{"null-type", func(v *unstructured.Unstructured) { v.Object["spec"] = map[string]any{"type": nil} }, "spec.type: Required value"},
		{"empty-type", func(v *unstructured.Unstructured) { v.Object["spec"] = map[string]any{"type": ""} }, "spec.type: Unsupported value"},
		{"invalid-type", func(v *unstructured.Unstructured) { v.Object["spec"] = map[string]any{"type": "Future"} }, "spec.type: Unsupported value"},
		{"wrong-case", func(v *unstructured.Unstructured) { v.Object["spec"] = map[string]any{"type": "cache"} }, "spec.type: Unsupported value"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := admissionVolume("type-"+tt.name, racerv1.GroupVersion.WithKind("ClusterVolume"))
			tt.mutate(v)

			err := c.Create(t.Context(), v)
			if err == nil {
				cleanupAdmissionObject(t, c, v)
			}

			if tt.want != "" {
				require.True(t, apierrors.IsInvalid(err), "%v", err)
				require.ErrorContains(t, err, tt.want)

				return
			}

			require.NoError(t, err)
			v.SetLabels(map[string]string{"test": "metadata-update"})
			require.NoError(t, c.Update(t.Context(), v))
			v.Object["spec"] = map[string]any{"type": "Future"}
			err = c.Update(t.Context(), v)
			require.True(t, apierrors.IsInvalid(err), "%v", err)
			require.ErrorContains(t, err, "spec.type: Unsupported value")
		})
	}
}

func integrationVolumeTypeImmutability(t *testing.T, c client.Client) {
	t.Helper()

	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: "clustervolumes." + racerv1.GroupName}, crd))
	crd.Object["metadata"] = map[string]any{"name": "volumetypeprobes." + racerv1.GroupName}
	delete(crd.Object, "status")
	require.NoError(t, unstructured.SetNestedMap(crd.Object, map[string]any{
		"kind": "VolumeTypeProbe", "listKind": "VolumeTypeProbeList", "plural": "volumetypeprobes", "singular": "volumetypeprobes",
	}, "spec", "names"))
	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	require.NoError(t, err)
	require.True(t, found)

	version := versions[0].(map[string]any)
	require.NoError(t, unstructured.SetNestedSlice(version, []any{"Cache", "Future"}, "schema", "openAPIV3Schema", "properties", "spec", "properties", "type", "enum"))
	require.NoError(t, unstructured.SetNestedSlice(crd.Object, versions, "spec", "versions"))
	require.NoError(t, c.Create(t.Context(), crd))
	cleanupAdmissionObject(t, c, crd)
	require.NoError(t, wait.PollUntilContextTimeout(t.Context(), 100*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(crd), crd); err != nil {
			return false, err
		}

		raw, _, err := unstructured.NestedFieldNoCopy(crd.Object, "status", "conditions")
		if raw == nil || err != nil {
			return false, err
		}

		for _, item := range raw.([]any) {
			condition := item.(map[string]any)
			if condition["type"] == "Established" && condition["status"] == string(metav1.ConditionTrue) {
				return true, nil
			}
		}

		return false, err
	}))

	v := admissionVolume("immutable", racerv1.GroupVersion.WithKind("VolumeTypeProbe"))
	require.NoError(t, c.Create(t.Context(), v))
	cleanupAdmissionObject(t, c, v)
	v.SetLabels(map[string]string{"test": "same-type"})
	require.NoError(t, c.Update(t.Context(), v))
	v.Object["spec"] = map[string]any{"type": "Future"}
	err = c.Update(t.Context(), v)
	require.True(t, apierrors.IsInvalid(err), "%v", err)
	require.ErrorContains(t, err, "type is immutable")
	require.NotContains(t, err.Error(), "Unsupported value")
}

func integrationVolumeNameAdmission(t *testing.T, c client.Client) {
	t.Helper()

	for _, name := range []string{"a", "0.cache-1.2", strings.Repeat("a", 63), strings.Repeat("a", 63) + "." + strings.Repeat("b", 18)} {
		v := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
		require.NoError(t, c.Create(t.Context(), v))
		cleanupAdmissionObject(t, c, v)
		catalog, err := members.BuildVolumeCatalog([]racerv1.ClusterVolume{*v})
		require.NoError(t, err)
		require.Len(t, catalog, 1)
	}

	for _, name := range []string{"", "Upper", "under_score", "a..b", "-cache", "cache-", "cache/child", strings.Repeat("a", 64), strings.Repeat("a", 63) + "." + strings.Repeat("b", 19)} {
		v := &racerv1.ClusterVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: racerv1.ClusterVolumeSpec{Type: racerv1.ClusterVolumeTypeCache}}
		require.True(t, apierrors.IsInvalid(c.Create(t.Context(), v)), name)
	}
}
