// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestApplyManifests_SingleResource(t *testing.T) {
	data := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
  namespace: default
data:
  key: value
`)

	var (
		mu             sync.Mutex
		appliedObjects []string
	)

	cli := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				u, ok := obj.(interface{ GetName() string })
				if ok {
					mu.Lock()

					appliedObjects = append(appliedObjects, u.GetName())
					mu.Unlock()
				}

				return nil
			},
		}).
		Build()

	err := applyManifests(context.Background(), discardLogger(), cli, "test-manager", data)
	require.NoError(t, err)
	require.Equal(t, []string{"test-cm"}, appliedObjects)
}

func TestApplyManifests_MultipleResources(t *testing.T) {
	data := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-one
  namespace: default
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-two
  namespace: default
`)

	var (
		mu             sync.Mutex
		appliedObjects []string
	)

	cli := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				u, ok := obj.(interface{ GetName() string })
				if ok {
					mu.Lock()

					appliedObjects = append(appliedObjects, u.GetName())
					mu.Unlock()
				}

				return nil
			},
		}).
		Build()

	err := applyManifests(context.Background(), discardLogger(), cli, "test-manager", data)
	require.NoError(t, err)
	require.Equal(t, []string{"cm-one", "cm-two"}, appliedObjects)
}

func TestApplyManifests_EmptyDocument(t *testing.T) {
	data := []byte(`---
---
`)

	cli := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				t.Fatal("Apply should not be called for empty documents")
				return nil
			},
		}).
		Build()

	err := applyManifests(context.Background(), discardLogger(), cli, "test-manager", data)
	require.NoError(t, err)
}

func TestApplyManifests_InvalidYAML(t *testing.T) {
	data := []byte(`not: valid: yaml: [`)

	cli := fake.NewClientBuilder().Build()

	err := applyManifests(context.Background(), discardLogger(), cli, "test-manager", data)
	require.Error(t, err)
	require.Contains(t, err.Error(), "decoding resource")
}
