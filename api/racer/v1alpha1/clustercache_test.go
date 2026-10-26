// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestClusterCacheRegistrationAndJSON(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))

	for kind, expected := range map[string]runtime.Object{
		"ClusterCache":     &ClusterCache{},
		"ClusterCacheList": &ClusterCacheList{},
	} {
		object, err := scheme.New(GroupVersion.WithKind(kind))
		require.NoError(t, err)
		require.IsType(t, expected, object)
	}

	cache := &ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "cache", Labels: map[string]string{"test": "original"}}}
	data, err := json.Marshal(cache)
	require.NoError(t, err)

	var object map[string]any
	require.NoError(t, json.Unmarshal(data, &object))
	require.NotContains(t, object, "spec")

	copy := cache.DeepCopy()
	copy.Labels["test"] = "copy"
	require.Equal(t, "original", cache.Labels["test"])
	list := &ClusterCacheList{Items: []ClusterCache{*cache}}
	listCopy := list.DeepCopy()
	listCopy.Items[0].Labels["test"] = "list-copy"
	require.Equal(t, "original", list.Items[0].Labels["test"])
}

func TestClusterCacheOriginJSONAndDeepCopy(t *testing.T) {
	cache := &ClusterCache{
		ObjectMeta: metav1.ObjectMeta{Name: "cache"},
		Spec: ClusterCacheSpec{Origin: &ObjectOrigin{
			Azure: &AzureOrigin{
				ContainerURL: "https://account.blob.core.windows.net/models",
				Credential:   &AzureCredential{Default: &AzureDefaultCredential{SecretRef: &SecretReference{Name: "azure"}}},
			},
		}},
	}

	data, err := json.Marshal(cache)
	require.NoError(t, err)
	require.JSONEq(t, `{"metadata":{"name":"cache"},"spec":{"origin":{"azure":{"containerURL":"https://account.blob.core.windows.net/models","credential":{"default":{"secretRef":{"name":"azure"}}}}}}}`, string(data))

	decoded := &ClusterCache{}
	require.NoError(t, json.Unmarshal(data, decoded))
	require.Equal(t, cache, decoded)

	copy := cache.DeepCopy()
	copy.Spec.Origin.Azure.ContainerURL = "copy"
	copy.Spec.Origin.Azure.Credential.Default.SecretRef.Name = "copy"
	require.Equal(t, "https://account.blob.core.windows.net/models", cache.Spec.Origin.Azure.ContainerURL)
	require.Equal(t, "azure", cache.Spec.Origin.Azure.Credential.Default.SecretRef.Name)
}

func TestClusterCachePriorityJSONDeepCopyAndDefault(t *testing.T) {
	require.Equal(t, DefaultPriority, ClusterCacheSpec{}.EffectivePriority())

	for _, priority := range []int32{MinPriority, 1, DefaultPriority, MaxPriority} {
		spec := ClusterCacheSpec{Priority: &priority}
		require.Equal(t, priority, spec.EffectivePriority())
	}

	priority := int32(0)
	cache := &ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "cache"}, Spec: ClusterCacheSpec{Priority: &priority}}
	data, err := json.Marshal(cache)
	require.NoError(t, err)
	require.JSONEq(t, `{"metadata":{"name":"cache"},"spec":{"priority":0}}`, string(data))

	decoded := &ClusterCache{}
	require.NoError(t, json.Unmarshal(data, decoded))
	require.Equal(t, cache, decoded)
	require.Equal(t, int32(0), decoded.Spec.EffectivePriority())

	copy := cache.DeepCopy()
	*copy.Spec.Priority = 900
	require.Equal(t, int32(0), *cache.Spec.Priority)

	unset := &ClusterCache{}
	require.NoError(t, json.Unmarshal([]byte(`{"metadata":{"name":"cache"},"spec":{}}`), unset))
	require.Nil(t, unset.Spec.Priority)
	require.Equal(t, DefaultPriority, unset.Spec.EffectivePriority())
}
