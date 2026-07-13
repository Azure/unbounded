// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
				named, ok := obj.(interface {
					GetKind() string
					GetName() string
				})
				if ok {
					mu.Lock()

					applied = append(applied, named.GetKind()+"/"+named.GetName())
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
	require.Contains(t, applied, "Deployment/unbounded-operator")
	require.Contains(t, applied, "ConfigMap/unbounded-operator-config")
	require.NotContains(t, applied, "CustomResourceDefinition/sites.unbounded-cloud.io")
	require.NotContains(t, applied, "CustomResourceDefinition/gatewaypools.net.unbounded-cloud.io")
	require.Less(t,
		indexOf(applied, "ConfigMap/unbounded-operator-config"),
		indexOf(applied, "Deployment/unbounded-operator"),
		"operator ConfigMap must be applied before its Deployment",
	)
}

func indexOf(values []string, want string) int {
	for i, value := range values {
		if value == want {
			return i
		}
	}

	return -1
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

// operatorDeployment builds an unbounded-operator Deployment with the given
// replica count and rollout status, for the rollout-gate tests.
func operatorDeployment(status appsv1.DeploymentStatus) *appsv1.Deployment {
	replicas := int32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  "unbounded-system",
			Name:       "unbounded-operator",
			Generation: 2,
		},
		Spec:   appsv1.DeploymentSpec{Replicas: &replicas},
		Status: status,
	}
}

func TestDeploymentRolloutComplete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status appsv1.DeploymentStatus
		want   bool
	}{
		{
			name:   "complete: single updated replica available, no old replicas",
			status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
			want:   true,
		},
		{
			// The finding #1 case: the old ReplicaSet is still Available while the
			// new, crash-looping pod surges in. A weaker AvailableReplicas>=1 check
			// would wrongly report success here.
			name:   "upgrade in progress: old available, new surging",
			status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 2, UpdatedReplicas: 1, AvailableReplicas: 1},
			want:   false,
		},
		{
			name:   "new generation not yet observed",
			status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
			want:   false,
		},
		{
			name:   "updated but not yet available",
			status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 0},
			want:   false,
		},
		{
			name:   "fresh install not yet rolled out",
			status: appsv1.DeploymentStatus{},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, deploymentRolloutComplete(operatorDeployment(tt.status)))
		})
	}
}

func TestWaitForOperatorRejectsIncompleteRollout(t *testing.T) {
	t.Parallel()

	// Old pod Available while the new pod surges in and never becomes ready.
	deploy := operatorDeployment(appsv1.DeploymentStatus{
		ObservedGeneration: 2,
		Replicas:           2,
		UpdatedReplicas:    1,
		AvailableReplicas:  1,
	})

	cli := fakeclient.NewClientBuilder().WithObjects(deploy).Build()

	h := installHandler{
		namespace:        "unbounded-system",
		timeout:          200 * time.Millisecond,
		kubeResourcesCli: cli,
		logger:           discardLogger(),
	}

	err := h.waitForOperator(context.Background())
	require.Error(t, err, "waitForOperator must not report success while the new operator pod is unavailable")
	require.Contains(t, err.Error(), "unbounded-operator rollout")
}

func TestWaitForOperatorSucceedsOnCompleteRollout(t *testing.T) {
	t.Parallel()

	deploy := operatorDeployment(appsv1.DeploymentStatus{
		ObservedGeneration: 2,
		Replicas:           1,
		UpdatedReplicas:    1,
		AvailableReplicas:  1,
	})

	cli := fakeclient.NewClientBuilder().WithObjects(deploy).Build()

	h := installHandler{
		namespace:        "unbounded-system",
		timeout:          5 * time.Second,
		kubeResourcesCli: cli,
		logger:           discardLogger(),
	}

	require.NoError(t, h.waitForOperator(context.Background()))
}

func TestMutateOperatorObjectStampsConfigHash(t *testing.T) {
	t.Parallel()

	newDeploy := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": "unbounded-operator", "namespace": "unbounded-system"},
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{
						"containers": []any{map[string]any{"name": "controller"}},
					},
				},
			},
		}}
	}

	hashOf := func(endpoint string) string {
		h := &installHandler{namespace: "unbounded-system", apiServerEndpoint: endpoint}
		obj := newDeploy()
		require.NoError(t, h.mutateOperatorObject(obj))

		got, found, err := unstructured.NestedString(obj.Object, "spec", "template", "metadata", "annotations", operatorConfigHashAnnotation)
		require.NoError(t, err)
		require.True(t, found, "operator config-hash annotation must be stamped on the pod template")

		return got
	}

	// The stamp is a stable hash of the resolved endpoint, matching the
	// render-time hash so both install and make-rendered manifests agree.
	endpoint := "https://api.example.test:6443"
	require.Equal(t, operatorConfigHash(endpoint), hashOf(endpoint))

	// A changed endpoint changes the hash, which changes the pod template and
	// therefore triggers a rollout (finding #3).
	require.NotEqual(t, hashOf(endpoint), hashOf("https://other.example.test:6443"))
}
