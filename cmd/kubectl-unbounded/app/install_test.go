// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"sync"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/stretchr/testify/require"
)

func TestInstallCommandFlags(t *testing.T) {
	cmd := installCommand()

	for _, name := range []string{
		"kubeconfig",
		"namespace",
		"net-namespace",
		"operator-image",
		"net-controller-image",
		"net-node-image",
		"machina-image",
		"metalman-image",
		"storage-supervisor-image",
		"api-server-endpoint",
		"skip-crds",
		"wait",
		"timeout",
	} {
		require.NotNil(t, cmd.Flags().Lookup(name), "flag %s", name)
	}
}

func TestInstallHandlerApplyBootstrapManifests(t *testing.T) {
	var (
		mu      sync.Mutex
		applied []string
	)

	cli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				named, ok := obj.(interface{ GetName() string })
				if ok {
					mu.Lock()

					applied = append(applied, named.GetName())
					mu.Unlock()
				}

				return nil
			},
		}).
		Build()

	h := installHandler{
		namespace:         "custom-system",
		netNamespace:      "custom-net",
		operatorImage:     "operator:test",
		apiServerEndpoint: "https://api.example.test:6443",
		wait:              false,
		kubeResourcesCli:  cli,
		logger:            discardLogger(),
	}

	require.NoError(t, h.execute(context.Background()))
	require.Contains(t, applied, "sites.unbounded-cloud.io")
	require.Contains(t, applied, "gatewaypools.net.unbounded-cloud.io")
	require.Contains(t, applied, "unbounded-operator")
}

func TestCRDEstablished(t *testing.T) {
	t.Parallel()

	obj := &apiextensionsv1.CustomResourceDefinition{
		Status: apiextensionsv1.CustomResourceDefinitionStatus{
			Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{
				{
					Type:   apiextensionsv1.Established,
					Status: apiextensionsv1.ConditionTrue,
				},
			},
		},
	}

	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	require.NoError(t, err)
	require.True(t, crdEstablished(&unstructured.Unstructured{Object: unstructuredObj}))
}
