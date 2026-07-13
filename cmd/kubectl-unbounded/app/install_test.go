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
		"operator-image",
		"metalman-image",
		"api-server-endpoint",
		"wait",
		"timeout",
	} {
		require.NotNil(t, cmd.Flags().Lookup(name), "flag %s", name)
	}

	for _, name := range []string{
		"net-namespace",
		"net-controller-image",
		"net-node-image",
		"machina-image",
		"storage-supervisor-image",
		// CRDs are now owned by the operator (BootstrapCRDs); install no longer
		// applies them, so --skip-crds is gone.
		"skip-crds",
	} {
		require.Nil(t, cmd.Flags().Lookup(name), "flag %s should be removed", name)
	}
}

func TestInstallRejectsLegacyNamespace(t *testing.T) {
	for _, ns := range []string{"unbounded-kube", "unbounded-net"} {
		h := installHandler{namespace: ns, logger: discardLogger()}

		err := h.execute(context.Background())
		require.Error(t, err, "install into legacy namespace %q must be rejected", ns)
		require.Contains(t, err.Error(), "legacy namespace")
		require.Contains(t, err.Error(), ns)
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
		operatorImage:     "operator:test",
		apiServerEndpoint: "https://api.example.test:6443",
		wait:              false,
		kubeResourcesCli:  cli,
		logger:            discardLogger(),
	}

	require.NoError(t, h.execute(context.Background()))
	// install applies only the operator manifests; CRDs are installed by the
	// operator at startup (BootstrapCRDs), so install must NOT apply them.
	require.Contains(t, applied, "unbounded-operator")
	require.Contains(t, applied, "unbounded-operator-config")
	require.NotContains(t, applied, "sites.unbounded-cloud.io")
	require.NotContains(t, applied, "gatewaypools.net.unbounded-cloud.io")
}

func TestMutateOperatorObjectWritesConfigEndpoint(t *testing.T) {
	t.Parallel()

	h := &installHandler{namespace: "unbounded-system", apiServerEndpoint: "https://api.example.test:6443"}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "unbounded-operator-config", "namespace": "unbounded-system"},
		"data":       map[string]any{"UNBOUNDED_API_SERVER_ENDPOINT": ""},
	}}

	require.NoError(t, h.mutateOperatorObject(obj))

	got, _, err := unstructured.NestedString(obj.Object, "data", "UNBOUNDED_API_SERVER_ENDPOINT")
	require.NoError(t, err)
	require.Equal(t, "https://api.example.test:6443", got)
}

func TestMutateOperatorObjectRetargetsNamespace(t *testing.T) {
	t.Parallel()

	h := &installHandler{namespace: "custom-system"}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "unbounded-operator", "namespace": "unbounded-system"},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{
						"name": "controller",
						"args": []any{
							"--leader-elect=true",
							"--leader-elect-namespace=unbounded-system",
							"--namespace=unbounded-system",
							"--metalman-image=x",
							"--api-server-endpoint=y",
						},
					}},
				},
			},
		},
	}}

	require.NoError(t, h.mutateOperatorObject(obj))

	require.Equal(t, "custom-system", obj.GetNamespace())

	containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)

	args, _, err := unstructured.NestedStringSlice(containers[0].(map[string]any), "args")
	require.NoError(t, err)
	require.Contains(t, args, "--namespace=custom-system")
	require.Contains(t, args, "--leader-elect-namespace=custom-system")
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
