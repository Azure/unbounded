// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	applyv1 "k8s.io/client-go/applyconfigurations/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestApplyErrorsIdentifyNamespaceThenName(t *testing.T) {
	ctx := context.Background()
	applyErr := errors.New("apply failed")

	tests := map[string]struct {
		resource string
		apply    func(kubernetes.Interface) error
		want     string
	}{
		"secret": {
			resource: "secrets",
			apply: func(cli kubernetes.Interface) error {
				return ApplySecret(ctx, cli, applyv1.Secret("token", "workload-ns"), metav1.ApplyOptions{FieldManager: "test"})
			},
			want: `applying secret "workload-ns/token": apply failed`,
		},
		"config map": {
			resource: "configmaps",
			apply: func(cli kubernetes.Interface) error {
				return ApplyConfigMap(ctx, cli, applyv1.ConfigMap("settings", "workload-ns"), metav1.ApplyOptions{FieldManager: "test"})
			},
			want: `applying config map "workload-ns/settings": apply failed`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cli := fake.NewClientset()
			cli.PrependReactor("patch", tt.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, applyErr
			})

			err := tt.apply(cli)
			require.EqualError(t, err, tt.want)
			require.ErrorIs(t, err, applyErr)
		})
	}
}
