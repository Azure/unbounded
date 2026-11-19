// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestClusterCacheFillRegistrationAndJSON(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))

	for kind, expected := range map[string]runtime.Object{
		"ClusterCacheFill":     &ClusterCacheFill{},
		"ClusterCacheFillList": &ClusterCacheFillList{},
	} {
		object, err := scheme.New(GroupVersion.WithKind(kind))
		require.NoError(t, err)
		require.IsType(t, expected, object)
	}

	fill := &ClusterCacheFill{
		ObjectMeta: metav1.ObjectMeta{Name: "models"},
		Spec: ClusterCacheFillSpec{
			CacheName:    "cache",
			Prefixes:     []string{"models/", "tokenizer/"},
			Replicas:     2,
			MaxFootprint: 1 << 30,
		},
	}

	data, err := json.Marshal(fill)
	require.NoError(t, err)
	require.JSONEq(t, `{"metadata":{"name":"models"},"spec":{"cacheName":"cache","prefixes":["models/","tokenizer/"],"replicas":2,"maxFootprint":1073741824}}`, string(data))

	decoded := &ClusterCacheFill{}
	require.NoError(t, json.Unmarshal(data, decoded))
	require.Equal(t, fill, decoded)

	copy := fill.DeepCopy()
	copy.Spec.Prefixes[0] = "copy"
	require.Equal(t, "models/", fill.Spec.Prefixes[0])

	list := &ClusterCacheFillList{Items: []ClusterCacheFill{*fill}}
	listCopy := list.DeepCopy()
	listCopy.Items[0].Spec.CacheName = "copy"
	require.Equal(t, "cache", list.Items[0].Spec.CacheName)
}

func TestClusterCacheFillStatusJSONAndDeepCopy(t *testing.T) {
	listed := metav1.NewTime(time.Unix(1700000000, 0).UTC())
	fill := &ClusterCacheFill{
		ObjectMeta: metav1.ObjectMeta{Name: "models"},
		Status: ClusterCacheFillStatus{
			ObservedGeneration:   3,
			CacheUID:             "uid",
			Manifest:             &FillManifestStatus{Objects: 12, LogicalBytes: 4096, Digest: "abc", ListedAt: &listed},
			FootprintBytes:       8192,
			ResidentBytes:        8000,
			UnderReplicatedBytes: 512,
			Nodes:                4,
			Conditions: []metav1.Condition{{
				Type: FillConditionReady, Status: metav1.ConditionTrue, Reason: FillReasonComplete,
			}},
		},
	}

	data, err := json.Marshal(fill)
	require.NoError(t, err)

	decoded := &ClusterCacheFill{}
	require.NoError(t, json.Unmarshal(data, decoded))
	roundTrip, err := json.Marshal(decoded)
	require.NoError(t, err)
	require.JSONEq(t, string(data), string(roundTrip))

	copy := fill.DeepCopy()
	copy.Status.Manifest.Digest = "copy"
	copy.Status.Conditions[0].Reason = "copy"
	require.Equal(t, "abc", fill.Status.Manifest.Digest)
	require.Equal(t, FillReasonComplete, fill.Status.Conditions[0].Reason)
}

func TestClusterCacheFillEffectiveDefaults(t *testing.T) {
	spec := ClusterCacheFillSpec{}
	require.Equal(t, DefaultFillReplicas, spec.EffectiveReplicas())
	require.Equal(t, DefaultFillResyncInterval, spec.EffectiveResyncInterval())

	for _, replicas := range []int32{-1, 0, 1, 2, 3, 4} {
		spec.Replicas = replicas
		expected := replicas
		if replicas < 1 || replicas > MaxFillReplicas {
			expected = DefaultFillReplicas
		}

		require.Equal(t, expected, spec.EffectiveReplicas())
	}

	for _, interval := range []time.Duration{
		0, 30 * time.Second, time.Duration(MinFillResyncInterval) * time.Second, time.Hour, time.Duration(MaxFillResyncInterval) * time.Second, 25 * time.Hour,
	} {
		spec.ResyncInterval = &metav1.Duration{Duration: interval}

		expected := int64(interval / time.Second)
		if expected < MinFillResyncInterval || expected > MaxFillResyncInterval {
			expected = DefaultFillResyncInterval
		}

		require.Equal(t, expected, spec.EffectiveResyncInterval(), interval.String())
	}
}
