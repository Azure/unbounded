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
			Bucket: "models",
			Azure: &AzureOrigin{
				AccountURL: "https://account.blob.core.windows.net",
				Credential: &AzureCredential{Default: &AzureDefaultCredential{SecretRef: &SecretReference{Name: "azure"}}},
			},
		}},
	}

	data, err := json.Marshal(cache)
	require.NoError(t, err)
	require.JSONEq(t, `{"metadata":{"name":"cache"},"spec":{"origin":{"bucket":"models","azure":{"accountURL":"https://account.blob.core.windows.net","credential":{"default":{"secretRef":{"name":"azure"}}}}}}}`, string(data))

	decoded := &ClusterCache{}
	require.NoError(t, json.Unmarshal(data, decoded))
	require.Equal(t, cache, decoded)

	copy := cache.DeepCopy()
	copy.Spec.Origin.Bucket = "copy"
	copy.Spec.Origin.Azure.Credential.Default.SecretRef.Name = "copy"
	require.Equal(t, BucketName("models"), cache.Spec.Origin.Bucket)
	require.Equal(t, "azure", cache.Spec.Origin.Azure.Credential.Default.SecretRef.Name)
}
