// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMutateStorageScopesDaemonSetToSite(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": "unbounded-storage-supervisor"},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "run"}}},
			},
		},
	}}

	if err := mutateStorageObject(site, obj); err != nil {
		t.Fatalf("mutateStorageObject returned error: %v", err)
	}

	if got := obj.GetName(); got != "unbounded-storage-supervisor-rack-a" {
		t.Fatalf("name = %q, want unbounded-storage-supervisor-rack-a", got)
	}

	if got := obj.GetLabels()[siteLabelKey]; got != "rack-a" {
		t.Fatalf("metadata site label = %q, want rack-a", got)
	}

	for _, path := range [][]string{
		{"spec", "selector", "matchLabels", siteLabelKey},
		{"spec", "template", "metadata", "labels", siteLabelKey},
		{"spec", "template", "spec", "nodeSelector", siteLabelKey},
	} {
		got, ok, err := unstructured.NestedString(obj.Object, path...)
		if err != nil || !ok {
			t.Fatalf("missing %v: ok=%t err=%v", path, ok, err)
		}

		if got != "rack-a" {
			t.Fatalf("%v = %q, want rack-a", path, got)
		}
	}
}

func TestMutateStorageAppliesConfigOverride(t *testing.T) {
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
			Storage: &unboundedv1alpha3.StorageComponentSpec{Config: "custom: true"},
		}},
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "unbounded-storage-config"},
		"data":       map[string]any{"config.yaml": "custom: false"},
	}}

	if err := mutateStorageObject(site, obj); err != nil {
		t.Fatalf("mutateStorageObject returned error: %v", err)
	}

	got, ok, err := unstructured.NestedString(obj.Object, "data", "config.yaml")
	if err != nil || !ok {
		t.Fatalf("missing data.config.yaml: ok=%t err=%v", ok, err)
	}

	if got != "custom: true" {
		t.Fatalf("config.yaml = %q, want custom: true", got)
	}
}

func TestMutateMachinaSkipsMetalmanSupport(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata":   map[string]any{"name": "metalman-controller"},
	}}

	r := &SiteReconciler{}
	if err := r.mutateMachinaObject(obj); err != nil {
		t.Fatalf("mutateMachinaObject returned error: %v", err)
	}

	if obj.Object != nil {
		t.Fatalf("metalman support object was not skipped")
	}
}

func TestMutateMachinaInjectsAPIServerEndpoint(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "machina-config"},
		"data":       map[string]any{"config.yaml": `apiServerEndpoint: ""`},
	}}

	r := &SiteReconciler{Config: Config{APIServerEndpoint: "https://api.example:6443"}}
	if err := r.mutateMachinaObject(obj); err != nil {
		t.Fatalf("mutateMachinaObject returned error: %v", err)
	}

	got, ok, err := unstructured.NestedString(obj.Object, "data", "config.yaml")
	if err != nil || !ok {
		t.Fatalf("missing data.config.yaml: ok=%t err=%v", ok, err)
	}

	if got != `apiServerEndpoint: "https://api.example:6443"` {
		t.Fatalf("config.yaml = %q", got)
	}
}

func TestMutateMetalmanSupportObject(t *testing.T) {
	keep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "Role",
		"metadata":   map[string]any{"name": "metalman-controller"},
	}}
	if err := mutateMetalmanSupportObject(keep); err != nil {
		t.Fatalf("mutateMetalmanSupportObject returned error: %v", err)
	}

	if keep.Object == nil {
		t.Fatalf("metalman support object was dropped")
	}

	drop := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata":   map[string]any{"name": "machina-controller"},
	}}
	if err := mutateMetalmanSupportObject(drop); err != nil {
		t.Fatalf("mutateMetalmanSupportObject returned error: %v", err)
	}

	if drop.Object != nil {
		t.Fatalf("non-metalman object was not dropped")
	}
}

func TestMetalmanDeployment(t *testing.T) {
	enabled := true
	dhcpAuto := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
			DHCPAutoInterface: &dhcpAuto,
		}}},
	}

	deployment := metalmanDeployment(site, Config{MetalmanImage: "example/metalman:default"})
	if deployment.Name != "metalman-controller-rack-a" {
		t.Fatalf("name = %q", deployment.Name)
	}

	if deployment.Namespace != DefaultNamespace {
		t.Fatalf("namespace = %q, want %q", deployment.Namespace, DefaultNamespace)
	}

	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Image != "example/metalman:default" {
		t.Fatalf("image = %q", container.Image)
	}

	if got := container.Args; len(got) != 3 || got[0] != "serve-pxe" || got[1] != "--site=rack-a" || got[2] != "--dhcp-auto-interface" {
		t.Fatalf("args = %#v", got)
	}

	if got := deployment.Spec.Template.Spec.NodeSelector[unboundedv1alpha3.MachineSiteLabelKey]; got != "rack-a" {
		t.Fatalf("site node selector = %q", got)
	}
}

func TestApplyObject(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add appsv1 to scheme: %v", err)
	}

	r := &SiteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}}},
		},
	}

	if err := r.applyObject(t.Context(), deployment); err != nil {
		t.Fatalf("applyObject returned error: %v", err)
	}
}
