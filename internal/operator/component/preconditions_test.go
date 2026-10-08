// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package component

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestApplyPreservesPreconditionsWithoutHashChurn(t *testing.T) {
	desired := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "default", UID: "original", ResourceVersion: "1"},
		Data:       map[string]string{"key": "value"},
	}
	desired.Labels = map[string]string{"app": "test"}
	hash, err := AppliedPayloadHash(ToUnstructured(desired))
	require.NoError(t, err)

	current := desired.DeepCopy()
	current.ResourceVersion = "2"
	current.Labels[AppliedHashLabel] = hash
	updatedHash, err := AppliedPayloadHash(ToUnstructured(current))
	require.NoError(t, err)
	require.Equal(t, hash, updatedHash)

	applies := 0
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(current).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, cfg runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			applies++
			data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cfg)
			require.NoError(t, err)
			require.Equal(t, "original", data["metadata"].(map[string]any)["uid"])
			require.Equal(t, "1", data["metadata"].(map[string]any)["resourceVersion"])

			return nil
		},
	}).Build()
	env := &Env{Client: c}
	require.NoError(t, env.ApplyObject(t.Context(), desired))
	require.Zero(t, applies)

	desired.Data["key"] = "changed"
	require.NoError(t, env.ApplyObject(t.Context(), desired))
	require.Equal(t, 1, applies)
}

func TestDeleteCarriesObservedPreconditions(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconditional", true: "observed"}[observed], func(t *testing.T) {
			obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owned", Namespace: "default"}}
			if observed {
				obj.UID, obj.ResourceVersion = "original", "7"
			}

			called := false
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, opts ...client.DeleteOption) error {
					called = true

					options := (&client.DeleteOptions{}).ApplyOptions(opts)
					if observed {
						require.Equal(t, obj.UID, *options.Preconditions.UID)
						require.Equal(t, obj.ResourceVersion, *options.Preconditions.ResourceVersion)
					} else {
						require.Nil(t, options.Preconditions)
					}

					return nil
				},
			}).Build()
			require.NoError(t, (&Env{Client: c}).DeleteIfExists(t.Context(), obj))
			require.True(t, called)
		})
	}
}
