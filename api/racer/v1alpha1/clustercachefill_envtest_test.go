// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestEnvtestClusterCacheFillValidation(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS for real API validation tests")
	}

	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"crd"}, ErrorIfCRDPathMissing: true}
	rc, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })

	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))
	c, err := client.New(rc, client.Options{Scheme: scheme})
	require.NoError(t, err)

	n := 0
	create := func(spec map[string]any) (*unstructured.Unstructured, error) {
		n++

		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": GroupVersion.String(),
			"kind":       "ClusterCacheFill",
			"metadata":   map[string]any{"name": fmt.Sprintf("fill-%d", n)},
		}}
		if spec != nil {
			obj.Object["spec"] = spec
		}

		return obj, c.Create(t.Context(), obj)
	}

	valid := map[string]map[string]any{
		"one prefix":              {"cacheName": "models", "prefixes": []any{"llama/"}, "maxFootprint": int64(1)},
		"whole container":         {"cacheName": "models", "prefixes": []any{""}, "maxFootprint": int64(1 << 30)},
		"two replicas":            {"cacheName": "models", "prefixes": []any{"a/", "b/"}, "replicas": int64(2), "maxFootprint": int64(1 << 30)},
		"three replicas":          {"cacheName": "models", "prefixes": []any{"a/"}, "replicas": int64(3), "maxFootprint": int64(1 << 30)},
		"resync":                  {"cacheName": "models", "prefixes": []any{"a/"}, "resyncInterval": "30m", "maxFootprint": int64(1 << 30)},
		"unicode prefix boundary": {"cacheName": "models", "prefixes": []any{strings.Repeat("界", 1024)}, "maxFootprint": int64(1)},
		"overlapping prefixes":    {"cacheName": "models", "prefixes": []any{"a/", "a/b/"}, "maxFootprint": int64(1)},
		"sixteen prefixes": {"cacheName": "models", "prefixes": []any{
			"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p",
		}, "maxFootprint": int64(1 << 30)},
	}
	for name, spec := range valid {
		t.Run("accepts "+name, func(t *testing.T) {
			_, err := create(spec)
			require.NoError(t, err)
		})
	}

	invalid := map[string]struct {
		spec    map[string]any
		message string
	}{
		"no spec":             {nil, "spec"},
		"no cache":            {map[string]any{"prefixes": []any{"a/"}, "maxFootprint": int64(1)}, "cacheName"},
		"no prefixes":         {map[string]any{"cacheName": "models", "maxFootprint": int64(1)}, "prefixes"},
		"empty prefixes":      {map[string]any{"cacheName": "models", "prefixes": []any{}, "maxFootprint": int64(1)}, "prefixes"},
		"duplicate prefixes":  {map[string]any{"cacheName": "models", "prefixes": []any{"a/", "a/"}, "maxFootprint": int64(1)}, "prefixes"},
		"long unicode prefix": {map[string]any{"cacheName": "models", "prefixes": []any{strings.Repeat("界", 1025)}, "maxFootprint": int64(1)}, "prefixes"},
		"too many prefixes": {map[string]any{"cacheName": "models", "prefixes": []any{
			"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l", "m", "n", "o", "p", "q",
		}, "maxFootprint": int64(1)}, "prefixes"},
		"zero footprint":     {map[string]any{"cacheName": "models", "prefixes": []any{"a/"}, "maxFootprint": int64(0)}, "maxFootprint"},
		"negative footprint": {map[string]any{"cacheName": "models", "prefixes": []any{"a/"}, "maxFootprint": int64(-1)}, "maxFootprint"},
		"bad replicas":       {map[string]any{"cacheName": "models", "prefixes": []any{"a/"}, "replicas": int64(4), "maxFootprint": int64(1)}, "replicas"},
		"bad resync short":   {map[string]any{"cacheName": "models", "prefixes": []any{"a/"}, "resyncInterval": "59s", "maxFootprint": int64(1)}, "resyncInterval"},
		"bad resync long":    {map[string]any{"cacheName": "models", "prefixes": []any{"a/"}, "resyncInterval": "25h", "maxFootprint": int64(1)}, "resyncInterval"},
		"bad cache name":     {map[string]any{"cacheName": "Not_Valid", "prefixes": []any{"a/"}, "maxFootprint": int64(1)}, "cacheName"},
	}
	for name, tc := range invalid {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := create(tc.spec)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.message)
		})
	}

	t.Run("defaults replicas and resync", func(t *testing.T) {
		obj, err := create(map[string]any{"cacheName": "models", "prefixes": []any{"a/"}, "maxFootprint": int64(1)})
		require.NoError(t, err)
		require.Equal(t, int64(2), mustNestedInt(t, obj.Object, "spec", "replicas"))
		require.Equal(t, "1h", mustNestedString(t, obj.Object, "spec", "resyncInterval"))
	})

	t.Run("cache name is immutable", func(t *testing.T) {
		obj, err := create(map[string]any{"cacheName": "models", "prefixes": []any{"a/"}, "maxFootprint": int64(1)})
		require.NoError(t, err)
		require.NoError(t, unstructured.SetNestedField(obj.Object, "other", "spec", "cacheName"))
		err = c.Update(t.Context(), obj)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cacheName is immutable")
	})

	t.Run("prefixes are mutable", func(t *testing.T) {
		obj, err := create(map[string]any{"cacheName": "models", "prefixes": []any{"a/"}, "maxFootprint": int64(1)})
		require.NoError(t, err)
		require.NoError(t, unstructured.SetNestedSlice(obj.Object, []any{"b/", "c/"}, "spec", "prefixes"))
		require.NoError(t, c.Update(t.Context(), obj))
	})

	t.Run("status is a subresource", func(t *testing.T) {
		obj, err := create(map[string]any{"cacheName": "models", "prefixes": []any{"a/"}, "maxFootprint": int64(1)})
		require.NoError(t, err)

		// The spec annotation is not part of status: a status write must not
		// change the generation nor be observable in the spec.
		require.NoError(t, unstructured.SetNestedMap(obj.Object, map[string]any{
			"observedGeneration": int64(1),
			"footprintBytes":     int64(4096),
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "True", "reason": "Complete", "message": "all pages pinned", "lastTransitionTime": metav1.Now().UTC().Format("2006-01-02T15:04:05Z"),
			}},
		}, "status"))
		require.NoError(t, c.Status().Update(t.Context(), obj))

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(obj.GroupVersionKind())
		require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: obj.GetName()}, got))
		require.Equal(t, int64(1), mustNestedInt(t, got.Object, "metadata", "generation"))
		require.Equal(t, int64(4096), mustNestedInt(t, got.Object, "status", "footprintBytes"))
	})
}

func mustNestedInt(t *testing.T, obj map[string]any, fields ...string) int64 {
	t.Helper()

	value, found, err := unstructured.NestedInt64(obj, fields...)
	require.NoError(t, err)
	require.True(t, found, "%v", fields)

	return value
}

func mustNestedString(t *testing.T, obj map[string]any, fields ...string) string {
	t.Helper()

	value, found, err := unstructured.NestedString(obj, fields...)
	require.NoError(t, err)
	require.True(t, found, "%v", fields)

	return value
}
