// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package provision

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestDetectProviderLabels(t *testing.T) {
	t.Parallel()

	aksMetadata := func(namespace string) *corev1.ConfigMap {
		return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "aks-cluster-metadata", Namespace: namespace}}
	}
	errUnavailable := errors.New("apiserver unavailable")

	tests := []struct {
		name    string
		objects []runtime.Object
		getErr  error
		want    map[string]string
	}{
		{
			name:    "AKS",
			objects: []runtime.Object{aksMetadata(metav1.NamespacePublic)},
			want:    map[string]string{"kubernetes.azure.com/managed": "false"},
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

			got, err := DetectProviderLabels(t.Context(), kubeCli)
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
