// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestDetectClusterProvider(t *testing.T) {
	t.Parallel()

	aksMetadata := func(namespace string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "aks-cluster-metadata", Namespace: namespace}}
	}
	errUnavailable := errors.New("apiserver unavailable")

	tests := []struct {
		name    string
		objects []runtime.Object
		getErr  error
		want    *ClusterProvider
	}{
		{
			name:    "AKS",
			objects: []runtime.Object{aksMetadata(metav1.NamespacePublic)},
			want: &ClusterProvider{
				Name:       "microsoft-aks",
				NodeLabels: map[string]string{"kubernetes.azure.com/managed": "false"},
			},
		},
		{
			name: "unknown provider",
		},
		{
			name:    "AKS metadata outside kube-public",
			objects: []runtime.Object{aksMetadata(metav1.NamespaceDefault)},
		},
		{
			name:   "lookup fails",
			getErr: errUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			kubeCli := fake.NewClientset(tt.objects...)
			if tt.getErr != nil {
				kubeCli.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getErr
				})
			}

			got, err := DetectClusterProvider(t.Context(), kubeCli)
			if tt.getErr != nil {
				require.ErrorIs(t, err, tt.getErr)
				require.Nil(t, got)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestDetectClusterProviderTriesDetectorsInOrder(t *testing.T) {
	t.Parallel()

	errDetect := errors.New("detection failed")
	match := func(name string) clusterProviderDetector {
		return func(context.Context, kubernetes.Interface) (*ClusterProvider, error) {
			return &ClusterProvider{Name: name}, nil
		}
	}
	noMatch := func(context.Context, kubernetes.Interface) (*ClusterProvider, error) { return nil, nil }
	fail := func(context.Context, kubernetes.Interface) (*ClusterProvider, error) { return nil, errDetect }

	tests := []struct {
		name      string
		detectors []clusterProviderDetector
		want      string
		wantErr   error
		wantCalls int
	}{
		{
			name:      "first match wins",
			detectors: []clusterProviderDetector{match("first"), match("second")},
			want:      "first",
			wantCalls: 1,
		},
		{
			name:      "skips detectors that do not match",
			detectors: []clusterProviderDetector{noMatch, match("second")},
			want:      "second",
			wantCalls: 2,
		},
		{
			name:      "error stops detection",
			detectors: []clusterProviderDetector{fail, match("second")},
			wantErr:   errDetect,
			wantCalls: 1,
		},
		{
			name:      "no match",
			detectors: []clusterProviderDetector{noMatch, noMatch},
			wantCalls: 2,
		},
		{
			name: "no detectors",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			calls := 0
			counted := make([]clusterProviderDetector, len(tt.detectors))

			for i, detect := range tt.detectors {
				counted[i] = func(ctx context.Context, k kubernetes.Interface) (*ClusterProvider, error) {
					calls++
					return detect(ctx, k)
				}
			}

			got, err := detectClusterProvider(t.Context(), fake.NewClientset(), counted)
			require.Equal(t, tt.wantCalls, calls)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				require.Nil(t, got)

				return
			}

			require.NoError(t, err)

			if tt.want == "" {
				require.Nil(t, got)

				return
			}

			require.NotNil(t, got)
			require.Equal(t, tt.want, got.Name)
		})
	}
}
