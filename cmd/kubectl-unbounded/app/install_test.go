// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/operator"
)

func TestInstallCommandFlags(t *testing.T) {
	cmd := installCommand()

	for _, name := range []string{
		"kubeconfig",
		"namespace",
		"operator-image",
		"image-registry",
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
		"metalman-image",
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

	for _, resource := range applied {
		require.NotContains(t, resource, "CustomResourceDefinition/", "install must leave CRD field ownership with unbounded-operator")
	}

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

type capturedOperatorApply struct {
	configMap  *unstructured.Unstructured
	deployment *unstructured.Unstructured
	applyCount int
}

func newCapturingInstallClient(objects ...client.Object) (client.Client, *capturedOperatorApply) {
	captured := &capturedOperatorApply{}
	cli := fakeclient.NewClientBuilder().
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				named, ok := obj.(interface {
					GetKind() string
					GetName() string
					DeepCopy() *unstructured.Unstructured
				})
				if !ok {
					return fmt.Errorf("unexpected apply configuration %T", obj)
				}

				captured.applyCount++

				switch {
				case named.GetKind() == "ConfigMap" && named.GetName() == "unbounded-operator-config":
					captured.configMap = named.DeepCopy()
				case named.GetKind() == "Deployment" && named.GetName() == "unbounded-operator":
					captured.deployment = named.DeepCopy()
				}

				return nil
			},
		}).
		Build()

	return cli, captured
}

func TestMutateOperatorObjectWritesConfigEndpoint(t *testing.T) {
	t.Parallel()

	h := &installHandler{
		namespace: "unbounded-system",
		operatorConfigData: map[string]string{
			"UNBOUNDED_API_SERVER_ENDPOINT":   "https://api.example.test:6443",
			"UNBOUNDED_IMAGE_REGISTRY":        "ghcr.io",
			"UNBOUNDED_REAP_LEGACY_RESOURCES": "true",
		},
	}

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

	got, _, err = unstructured.NestedString(obj.Object, "data", "UNBOUNDED_REAP_LEGACY_RESOURCES")
	require.NoError(t, err)
	require.Equal(t, "true", got)
}

func TestInstallMergesLiveReaperConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		existingConfig *unstructured.Unstructured
		wantReaper     string
	}{
		{
			name: "existing false remains false",
			existingConfig: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "unbounded-operator-config",
					"namespace": "unbounded-system",
				},
				"data": map[string]any{
					"UNBOUNDED_API_SERVER_ENDPOINT":   "https://old.example.test:6443",
					"UNBOUNDED_REAP_LEGACY_RESOURCES": "FALSE",
				},
			}},
			wantReaper: "false",
		},
		{
			name: "existing config without reaper defaults true",
			existingConfig: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "unbounded-operator-config",
					"namespace": "unbounded-system",
				},
				"data": map[string]any{"UNBOUNDED_API_SERVER_ENDPOINT": "https://old.example.test:6443"},
			}},
			wantReaper: "true",
		},
		{
			name:       "missing config defaults true",
			wantReaper: "true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var objects []client.Object
			if tt.existingConfig != nil {
				objects = append(objects, tt.existingConfig)
			}

			cli, captured := newCapturingInstallClient(objects...)
			h := installHandler{
				namespace:         "unbounded-system",
				apiServerEndpoint: "https://api.example.test:6443",
				kubeResourcesCli:  cli,
				logger:            discardLogger(),
			}

			require.NoError(t, h.execute(context.Background()))
			require.NotNil(t, captured.configMap)
			require.NotNil(t, captured.deployment)

			data, found, err := unstructured.NestedStringMap(captured.configMap.Object, "data")
			require.NoError(t, err)
			require.True(t, found)

			// The registry is derived from the embedded manifest, which the make
			// render stamps from CONTAINER_REGISTRY; read it rather than hardcode
			// so a fork build (non-default CONTAINER_REGISTRY) still passes.
			wantRegistry, err := (&installHandler{}).embeddedImageRegistry()
			require.NoError(t, err)
			require.Equal(t, map[string]string{
				"UNBOUNDED_API_SERVER_ENDPOINT":   "https://api.example.test:6443",
				"UNBOUNDED_IMAGE_REGISTRY":        wantRegistry,
				"UNBOUNDED_REAP_LEGACY_RESOURCES": tt.wantReaper,
			}, data)

			gotHash, found, err := unstructured.NestedString(captured.deployment.Object, "spec", "template", "metadata", "annotations", operatorConfigHashAnnotation)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, operatorConfigHash(data), gotHash)

			strategyType, found, err := unstructured.NestedString(captured.deployment.Object, "spec", "strategy", "type")
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, "Recreate", strategyType)

			rollingUpdate, found, err := unstructured.NestedFieldNoCopy(captured.deployment.Object, "spec", "strategy", "rollingUpdate")
			require.NoError(t, err)
			require.True(t, found, "rollingUpdate must remain explicit through manifest apply conversion")
			require.Nil(t, rollingUpdate)
		})
	}
}

// TestInstallReinstallDerivesRegistryFromBuild asserts the component registry is
// re-derived from the binary's embedded manifests on every install, not
// preserved from cluster state. An older cluster storing the pre-#574 bare
// "ghcr.io" (or any custom value) is overwritten with the embedded registry, so
// it cannot drift from the operator image install also re-derives. The embedded
// registry is injected (rather than read from the make-rendered manifests) so the
// assertion does not depend on the build-time CONTAINER_REGISTRY.
func TestInstallReinstallDerivesRegistryFromBuild(t *testing.T) {
	t.Parallel()

	const embeddedRegistry = "registry.test/unbounded"

	cases := []struct {
		name  string
		value string
	}{
		{name: "legacy bare host overwritten", value: "ghcr.io"},
		{name: "stale custom mirror overwritten", value: "registry.old/mirror"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			existing := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "unbounded-operator-config",
					"namespace": "unbounded-system",
				},
				"data": map[string]any{"UNBOUNDED_IMAGE_REGISTRY": tc.value},
			}}
			cli, captured := newCapturingInstallClient(existing)
			h := installHandler{
				namespace:         "unbounded-system",
				kubeResourcesCli:  cli,
				operatorManifests: operatorManifestsFS(embeddedRegistry, embeddedRegistry+"/unbounded-operator:v1"),
				logger:            discardLogger(),
			}

			require.NoError(t, h.execute(context.Background()))
			require.NotNil(t, captured.configMap)

			data, _, err := unstructured.NestedStringMap(captured.configMap.Object, "data")
			require.NoError(t, err)
			require.Equal(t, embeddedRegistry, data["UNBOUNDED_IMAGE_REGISTRY"])
		})
	}
}

// TestInstallImageRegistryFlagOverridesEmbedded asserts an explicit
// --image-registry wins over the embedded default.
func TestInstallImageRegistryFlagOverridesEmbedded(t *testing.T) {
	t.Parallel()

	cli, captured := newCapturingInstallClient()
	h := installHandler{
		namespace:        "unbounded-system",
		kubeResourcesCli: cli,
		imageRegistry:    "registry.corp/unbounded",
		logger:           discardLogger(),
	}

	require.NoError(t, h.execute(context.Background()))
	require.NotNil(t, captured.configMap)

	data, _, err := unstructured.NestedStringMap(captured.configMap.Object, "data")
	require.NoError(t, err)
	require.Equal(t, "registry.corp/unbounded", data["UNBOUNDED_IMAGE_REGISTRY"])
}

// TestInstallForkBuildDerivesEmbeddedRegistry asserts a fork build (whose
// embedded manifests were rendered with CONTAINER_REGISTRY=ghcr.io/myorg)
// installs its own operator image and configures components from its own
// registry, rather than discarding the embedded value for ghcr.io/azure.
func TestInstallForkBuildDerivesEmbeddedRegistry(t *testing.T) {
	t.Parallel()

	const (
		registry      = "ghcr.io/myorg"
		operatorImage = "ghcr.io/myorg/unbounded-operator:v1.2.3"
	)

	cli, captured := newCapturingInstallClient()
	h := installHandler{
		namespace:         "unbounded-system",
		kubeResourcesCli:  cli,
		operatorManifests: operatorManifestsFS(registry, operatorImage),
		logger:            discardLogger(),
	}

	require.NoError(t, h.execute(context.Background()))
	require.NotNil(t, captured.configMap)
	require.NotNil(t, captured.deployment)

	data, _, err := unstructured.NestedStringMap(captured.configMap.Object, "data")
	require.NoError(t, err)
	require.Equal(t, registry, data["UNBOUNDED_IMAGE_REGISTRY"])

	containers, _, err := unstructured.NestedSlice(captured.deployment.Object, "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.Equal(t, operatorImage, containers[0].(map[string]any)["image"])
}

// operatorManifestsFS builds a minimal embedded operator manifest set (the
// unbounded-operator-config ConfigMap and the operator Deployment) with a chosen
// component registry and operator image. Injecting it lets a test assert the
// build-derived registry/image without depending on the make-rendered manifests
// or the build-time CONTAINER_REGISTRY.
func operatorManifestsFS(registry, operatorImage string) fstest.MapFS {
	configMap := fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: unbounded-operator-config
  namespace: unbounded-system
data:
  UNBOUNDED_API_SERVER_ENDPOINT: ""
  UNBOUNDED_IMAGE_REGISTRY: %q
  UNBOUNDED_REAP_LEGACY_RESOURCES: "true"
`, registry)

	deployment := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: unbounded-operator
  namespace: unbounded-system
spec:
  template:
    spec:
      containers:
        - name: controller
          image: %q
          args:
            - --namespace=unbounded-system
`, operatorImage)

	return fstest.MapFS{
		"03-configmap.yaml":  &fstest.MapFile{Data: []byte(configMap)},
		"04-deployment.yaml": &fstest.MapFile{Data: []byte(deployment)},
	}
}

// TestEmbeddedImageRegistryFailsClosed asserts the build-derived registry lookup
// returns an error (rather than silently falling back to ghcr.io/azure) when the
// embedded manifests are missing the ConfigMap, carry an empty value, or are
// malformed, so a broken build cannot quietly install upstream components.
func TestEmbeddedImageRegistryFailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		manifests fstest.MapFS
		want      string // "" means expect an error
	}{
		{
			name: "valid",
			manifests: fstest.MapFS{"03-configmap.yaml": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: unbounded-operator-config
data:
  UNBOUNDED_IMAGE_REGISTRY: "ghcr.io/myorg"
`)}},
			want: "ghcr.io/myorg",
		},
		{
			name:      "no configmap",
			manifests: fstest.MapFS{"04-deployment.yaml": &fstest.MapFile{Data: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: unbounded-operator\n")}},
		},
		{
			name: "empty value",
			manifests: fstest.MapFS{"03-configmap.yaml": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: unbounded-operator-config
data:
  UNBOUNDED_IMAGE_REGISTRY: ""
`)}},
		},
		{
			name:      "malformed yaml",
			manifests: fstest.MapFS{"03-configmap.yaml": &fstest.MapFile{Data: []byte("this: [is: not: valid: yaml")}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := installHandler{operatorManifests: tc.manifests}

			got, err := h.embeddedImageRegistry()
			if tc.want == "" {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestInstallFailsClosedOnUnresolvableRegistry asserts install errors (rather
// than defaulting to ghcr.io/azure) when the embedded registry cannot be
// resolved and no --image-registry is given, and that --image-registry overrides
// an unresolvable embedded value.
func TestInstallFailsClosedOnUnresolvableRegistry(t *testing.T) {
	t.Parallel()

	brokenManifests := fstest.MapFS{
		"03-configmap.yaml": &fstest.MapFile{Data: []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: unbounded-operator-config
data:
  UNBOUNDED_IMAGE_REGISTRY: ""
`)},
	}

	t.Run("errors without a flag", func(t *testing.T) {
		t.Parallel()

		cli, captured := newCapturingInstallClient()
		h := installHandler{
			namespace:         "unbounded-system",
			kubeResourcesCli:  cli,
			operatorManifests: brokenManifests,
			logger:            discardLogger(),
		}

		err := h.execute(context.Background())
		require.Error(t, err)
		require.Contains(t, err.Error(), "--image-registry")
		require.Zero(t, captured.applyCount)
	})

	t.Run("flag overrides unresolvable embedded value", func(t *testing.T) {
		t.Parallel()

		cli, captured := newCapturingInstallClient()
		h := installHandler{
			namespace:         "unbounded-system",
			kubeResourcesCli:  cli,
			operatorManifests: brokenManifests,
			imageRegistry:     "registry.corp/unbounded",
			logger:            discardLogger(),
		}

		require.NoError(t, h.execute(context.Background()))
		require.NotNil(t, captured.configMap)

		data, _, err := unstructured.NestedStringMap(captured.configMap.Object, "data")
		require.NoError(t, err)
		require.Equal(t, "registry.corp/unbounded", data["UNBOUNDED_IMAGE_REGISTRY"])
	})
}

func TestInstallRejectsInvalidLiveReaperConfigBeforeApply(t *testing.T) {
	t.Parallel()

	existing := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "unbounded-operator-config",
			"namespace": "unbounded-system",
		},
		"data": map[string]any{"UNBOUNDED_REAP_LEGACY_RESOURCES": "not-a-bool"},
	}}
	cli, captured := newCapturingInstallClient(existing)
	h := installHandler{
		namespace:        "unbounded-system",
		kubeResourcesCli: cli,
		logger:           discardLogger(),
	}

	err := h.execute(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "UNBOUNDED_REAP_LEGACY_RESOURCES")
	require.Zero(t, captured.applyCount)
}

func TestInstallExecuteReinitializesDerivedConfig(t *testing.T) {
	t.Parallel()

	existing := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      "unbounded-operator-config",
			"namespace": "unbounded-system",
		},
		"data": map[string]any{
			"UNBOUNDED_REAP_LEGACY_RESOURCES": "false",
			"UNBOUNDED_API_SERVER_ENDPOINT":   "https://preserved.example.test:6443",
		},
	}}
	firstClient, first := newCapturingInstallClient(existing)
	h := installHandler{
		namespace:        "unbounded-system",
		kubeResourcesCli: firstClient,
		restConfig:       &rest.Config{Host: "https://first.example.test:6443"},
		logger:           discardLogger(),
	}

	require.NoError(t, h.execute(context.Background()))

	firstData, _, err := unstructured.NestedStringMap(first.configMap.Object, "data")
	require.NoError(t, err)
	require.Equal(t, "false", firstData["UNBOUNDED_REAP_LEGACY_RESOURCES"])
	require.Equal(t, "https://preserved.example.test:6443", firstData["UNBOUNDED_API_SERVER_ENDPOINT"])

	secondClient, second := newCapturingInstallClient()
	h.kubeResourcesCli = secondClient
	h.restConfig.Host = "https://second.example.test:6443"
	h.operatorRepairToken = "stale-repair-token"

	require.NoError(t, h.execute(context.Background()))

	secondData, _, err := unstructured.NestedStringMap(second.configMap.Object, "data")
	require.NoError(t, err)
	require.Equal(t, "true", secondData["UNBOUNDED_REAP_LEGACY_RESOURCES"])
	require.Equal(t, "", secondData["UNBOUNDED_API_SERVER_ENDPOINT"])

	_, found, err := unstructured.NestedString(second.deployment.Object, "spec", "template", "metadata", "annotations", operatorCRDRepairAnnotation)
	require.NoError(t, err)
	require.False(t, found)
}

func TestMutateOperatorObjectRetargetsNamespace(t *testing.T) {
	t.Parallel()

	h := &installHandler{
		namespace: "custom-system",
		operatorConfigData: map[string]string{
			"UNBOUNDED_API_SERVER_ENDPOINT":   "",
			"UNBOUNDED_IMAGE_REGISTRY":        "ghcr.io",
			"UNBOUNDED_REAP_LEGACY_RESOURCES": "true",
		},
	}

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

// TestMutateOperatorObjectRetargetsClusterRoleBindingSubjectWithPodNamespaceSet
// guards against a regression where the namespace rewrite source was
// SystemNamespace() instead of the build-time literal. A ClusterRoleBinding is
// cluster-scoped, so its subject namespace can only be corrected by
// rewriteNamespace (setNamespace never touches it). When POD_NAMESPACE is set
// (an in-cluster installer) the old source no longer matched the baked
// "unbounded-system", so the subject was left pointing at the wrong namespace
// and the operator ServiceAccount got no ClusterRole grant.
func TestMutateOperatorObjectRetargetsClusterRoleBindingSubjectWithPodNamespaceSet(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "installer-ns")

	h := &installHandler{namespace: "custom-system"}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRoleBinding",
		"metadata":   map[string]any{"name": "unbounded-operator"},
		"roleRef": map[string]any{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "ClusterRole",
			"name":     "unbounded-operator",
		},
		"subjects": []any{map[string]any{
			"kind":      "ServiceAccount",
			"name":      "unbounded-operator",
			"namespace": "unbounded-system",
		}},
	}}

	require.NoError(t, h.mutateOperatorObject(obj))

	subjects, _, err := unstructured.NestedSlice(obj.Object, "subjects")
	require.NoError(t, err)
	require.Len(t, subjects, 1)

	ns, _, err := unstructured.NestedString(subjects[0].(map[string]any), "namespace")
	require.NoError(t, err)
	require.Equal(t, "custom-system", ns, "ClusterRoleBinding subject namespace must be retargeted even when POD_NAMESPACE is set")
}

func TestCRDEstablished(t *testing.T) {
	t.Parallel()

	established := &apiextensionsv1.CustomResourceDefinition{
		Status: apiextensionsv1.CustomResourceDefinitionStatus{
			Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{
				{
					Type:   apiextensionsv1.Established,
					Status: apiextensionsv1.ConditionTrue,
				},
			},
		},
	}

	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(established)
	require.NoError(t, err)

	obj := &unstructured.Unstructured{Object: unstructuredObj}
	require.True(t, crdEstablished(obj))

	now := metav1.Now()
	obj.SetDeletionTimestamp(&now)
	require.False(t, crdEstablished(obj), "a terminating CRD must not be treated as established")
}

// operatorDeployment builds an unbounded-operator Deployment with the given
// replica count and rollout status, for the rollout-gate tests.
func operatorDeployment(status appsv1.DeploymentStatus) *appsv1.Deployment {
	replicas := int32(1)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
		},
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

	newConfigMap := func(data map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "unbounded-operator-config", "namespace": "unbounded-system"},
			"data":       data,
		}}
	}

	newDeployment := func() *unstructured.Unstructured {
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

	hashOf := func(data map[string]any) string {
		configData := make(map[string]string, len(data))
		for key, value := range data {
			configData[key] = value.(string)
		}

		h := &installHandler{namespace: "unbounded-system", operatorConfigData: configData}
		require.NoError(t, h.mutateOperatorObject(newConfigMap(data)))

		obj := newDeployment()
		require.NoError(t, h.mutateOperatorObject(obj))

		got, found, err := unstructured.NestedString(obj.Object, "spec", "template", "metadata", "annotations", operatorConfigHashAnnotation)
		require.NoError(t, err)
		require.True(t, found, "operator config-hash annotation must be stamped on the pod template")

		return got
	}

	first := map[string]any{
		"UNBOUNDED_API_SERVER_ENDPOINT":   "https://api.example.test:6443",
		"UNBOUNDED_REAP_LEGACY_RESOURCES": "true",
	}
	second := map[string]any{
		"UNBOUNDED_REAP_LEGACY_RESOURCES": "true",
		"UNBOUNDED_API_SERVER_ENDPOINT":   "https://api.example.test:6443",
	}

	wantData := map[string]string{
		"UNBOUNDED_API_SERVER_ENDPOINT":   "https://api.example.test:6443",
		"UNBOUNDED_REAP_LEGACY_RESOURCES": "true",
	}
	require.Equal(t, operatorConfigHash(wantData), hashOf(first))
	require.Equal(t, hashOf(first), hashOf(second), "hash must be independent of ConfigMap map order")

	changedEndpoint := map[string]any{
		"UNBOUNDED_API_SERVER_ENDPOINT":   "https://other.example.test:6443",
		"UNBOUNDED_REAP_LEGACY_RESOURCES": "true",
	}
	changedReaper := map[string]any{
		"UNBOUNDED_API_SERVER_ENDPOINT":   "https://api.example.test:6443",
		"UNBOUNDED_REAP_LEGACY_RESOURCES": "false",
	}

	require.NotEqual(t, hashOf(first), hashOf(changedEndpoint))
	require.NotEqual(t, hashOf(first), hashOf(changedReaper))
}

func TestWaitForCRDsChecksCompleteOperatorContract(t *testing.T) {
	t.Parallel()

	var names []string

	cli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				crd, ok := obj.(*unstructured.Unstructured)
				if !ok || crd.GetKind() != "CustomResourceDefinition" {
					return fmt.Errorf("unexpected get %s into %T", key, obj)
				}

				names = append(names, key.Name)
				crd.SetName(key.Name)
				crd.Object["status"] = map[string]any{
					"conditions": []any{map[string]any{"type": "Established", "status": "True"}},
				}

				return nil
			},
		}).
		Build()

	h := installHandler{
		timeout:          5 * time.Second,
		kubeResourcesCli: cli,
	}
	require.NoError(t, h.waitForCRDs(context.Background()))

	want := append([]string(nil), operator.RequiredCRDNames[:]...)

	sort.Strings(names)
	sort.Strings(want)
	require.Equal(t, want, names)
}

func TestWaitForCRDsRejectsTerminatingEstablishedCRD(t *testing.T) {
	t.Parallel()

	terminatingName := operator.RequiredCRDNames[0]
	now := metav1.Now()
	cli := fakeclient.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
				crd, ok := obj.(*unstructured.Unstructured)
				if !ok || crd.GetKind() != "CustomResourceDefinition" {
					return fmt.Errorf("unexpected get %s into %T", key, obj)
				}

				crd.SetName(key.Name)

				crd.Object["status"] = map[string]any{
					"conditions": []any{map[string]any{"type": "Established", "status": "True"}},
				}
				if key.Name == terminatingName {
					crd.SetDeletionTimestamp(&now)
				}

				return nil
			},
		}).
		Build()

	h := installHandler{timeout: 100 * time.Millisecond, kubeResourcesCli: cli}
	err := h.waitForCRDs(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "waiting for CRDs to be established")
}

func TestInstallRepairsCRDsOnlyByRestartingExistingOperator(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		existingDeploy    bool
		unhealthyCRDName  string
		unhealthyExists   bool
		unhealthyDeleting bool
		oldRepairToken    string
		rolloutComplete   bool
		wantRepairToken   string
		wantTokenCalls    int
	}{
		{
			name:             "missing CRD restarts existing operator",
			existingDeploy:   true,
			unhealthyCRDName: operator.RequiredCRDNames[0],
			rolloutComplete:  true,
			wantRepairToken:  "fresh-repair-token",
			wantTokenCalls:   1,
		},
		{
			name:             "unestablished CRD restarts existing operator",
			existingDeploy:   true,
			unhealthyCRDName: operator.RequiredCRDNames[1],
			unhealthyExists:  true,
			rolloutComplete:  true,
			wantRepairToken:  "fresh-repair-token",
			wantTokenCalls:   1,
		},
		{
			name:              "terminating established CRD restarts existing operator",
			existingDeploy:    true,
			unhealthyCRDName:  operator.RequiredCRDNames[2],
			unhealthyExists:   true,
			unhealthyDeleting: true,
			rolloutComplete:   true,
			wantRepairToken:   "fresh-repair-token",
			wantTokenCalls:    1,
		},
		{
			name:             "fresh install needs no repair token",
			unhealthyCRDName: operator.RequiredCRDNames[0],
		},
		{
			name:            "healthy reinstall preserves existing token",
			existingDeploy:  true,
			oldRepairToken:  "existing-repair-token",
			wantRepairToken: "existing-repair-token",
		},
		{
			name:             "in-progress repair preserves existing token",
			existingDeploy:   true,
			unhealthyCRDName: operator.RequiredCRDNames[0],
			oldRepairToken:   "existing-repair-token",
			wantRepairToken:  "existing-repair-token",
		},
		{
			name:             "in-progress repair preserves empty token",
			existingDeploy:   true,
			unhealthyCRDName: operator.RequiredCRDNames[0],
		},
		{
			name:           "healthy reinstall adds no token",
			existingDeploy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			objects := make([]client.Object, 0, len(operator.RequiredCRDNames)+1)
			for _, name := range operator.RequiredCRDNames {
				if name == tt.unhealthyCRDName && !tt.unhealthyExists {
					continue
				}

				crd := establishedCRD(name)
				if name == tt.unhealthyCRDName {
					if tt.unhealthyDeleting {
						now := metav1.Now()
						crd.SetDeletionTimestamp(&now)
						crd.SetFinalizers([]string{"test.unbounded-cloud.io/hold"})
					} else {
						delete(crd.Object, "status")
					}
				}

				objects = append(objects, crd)
			}

			if tt.existingDeploy {
				annotations := map[string]string{}
				if tt.oldRepairToken != "" {
					annotations[operatorCRDRepairAnnotation] = tt.oldRepairToken
				}

				status := appsv1.DeploymentStatus{}
				if tt.rolloutComplete {
					status = appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1}
				}

				deploy := operatorDeployment(status)
				deploy.Spec.Template.Annotations = annotations
				deployObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(deploy)
				require.NoError(t, err)

				objects = append(objects, &unstructured.Unstructured{Object: deployObject})
			}

			var (
				appliedDeployment *unstructured.Unstructured
				appliedKinds      []string
				tokenCalls        int
			)

			cli := fakeclient.NewClientBuilder().
				WithObjects(objects...).
				WithInterceptorFuncs(interceptor.Funcs{
					Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
						named, ok := obj.(interface {
							GetKind() string
							GetName() string
							DeepCopy() *unstructured.Unstructured
						})
						if !ok {
							return fmt.Errorf("unexpected apply configuration %T", obj)
						}

						appliedKinds = append(appliedKinds, named.GetKind())
						if named.GetKind() == "Deployment" && named.GetName() == "unbounded-operator" {
							appliedDeployment = named.DeepCopy()
						}

						return nil
					},
				}).
				Build()

			h := installHandler{
				namespace:        "unbounded-system",
				wait:             false,
				kubeResourcesCli: cli,
				logger:           discardLogger(),
				newRepairToken: func() (string, error) {
					tokenCalls++

					return "fresh-repair-token", nil
				},
			}

			require.NoError(t, h.execute(context.Background()))
			require.NotNil(t, appliedDeployment)
			require.Equal(t, tt.wantTokenCalls, tokenCalls)

			gotToken, _, err := unstructured.NestedString(appliedDeployment.Object, "spec", "template", "metadata", "annotations", operatorCRDRepairAnnotation)
			require.NoError(t, err)
			require.Equal(t, tt.wantRepairToken, gotToken)

			configHash, found, err := unstructured.NestedString(appliedDeployment.Object, "spec", "template", "metadata", "annotations", operatorConfigHashAnnotation)
			require.NoError(t, err)
			require.True(t, found)
			require.NotEmpty(t, configHash)
			require.NotContains(t, appliedKinds, "CustomResourceDefinition")
		})
	}
}

func TestPrepareOperatorRepairRapidRetryPreservesNewToken(t *testing.T) {
	t.Parallel()

	objects := make([]client.Object, 0, len(operator.RequiredCRDNames))
	for _, name := range operator.RequiredCRDNames[1:] {
		objects = append(objects, establishedCRD(name))
	}

	deploy := operatorDeployment(appsv1.DeploymentStatus{
		ObservedGeneration: 2,
		Replicas:           1,
		UpdatedReplicas:    1,
		AvailableReplicas:  1,
	})
	deployObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(deploy)
	require.NoError(t, err)

	objects = append(objects, &unstructured.Unstructured{Object: deployObject})
	cli := fakeclient.NewClientBuilder().WithObjects(objects...).Build()

	tokenCalls := 0
	h := installHandler{
		namespace:        "unbounded-system",
		kubeResourcesCli: cli,
		newRepairToken: func() (string, error) {
			tokenCalls++

			return "first-repair-token", nil
		},
	}

	require.NoError(t, h.prepareOperatorRepair(context.Background()))
	require.Equal(t, "first-repair-token", h.operatorRepairToken)
	require.Equal(t, 1, tokenCalls)

	inProgressDeploy := operatorDeployment(appsv1.DeploymentStatus{
		ObservedGeneration: 2,
		Replicas:           1,
		UpdatedReplicas:    1,
	})
	inProgressDeploy.Spec.Template.Annotations = map[string]string{
		operatorCRDRepairAnnotation: "first-repair-token",
	}
	inProgressObject, err := runtime.DefaultUnstructuredConverter.ToUnstructured(inProgressDeploy)
	require.NoError(t, err)

	objects[len(objects)-1] = &unstructured.Unstructured{Object: inProgressObject}
	h.kubeResourcesCli = fakeclient.NewClientBuilder().WithObjects(objects...).Build()

	h.operatorRepairToken = ""
	require.NoError(t, h.prepareOperatorRepair(context.Background()))
	require.Equal(t, "first-repair-token", h.operatorRepairToken)
	require.Equal(t, 1, tokenCalls, "rapid retry must not generate another rollout token")
}

func establishedCRD(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": name},
		"status": map[string]any{
			"conditions": []any{map[string]any{"type": "Established", "status": "True"}},
		},
	}}
}
