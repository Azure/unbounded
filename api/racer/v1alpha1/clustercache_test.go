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
