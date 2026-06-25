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

func TestMutateNetObjectAppliesNamespaceAndImages(t *testing.T) {
	enabled := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "edge"},
		Spec: unboundedv1alpha3.SiteSpec{
			Components: unboundedv1alpha3.SiteComponents{
				Net: &unboundedv1alpha3.SiteComponentSpec{
					Enabled:   &enabled,
					Namespace: "custom-net",
					Image:     "example/net:site",
				},
			},
		},
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name":      "unbounded-net-node",
			"namespace": DefaultNetNamespace,
		},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"initContainers": []any{map[string]any{"name": "install-cni-plugins", "image": "old-init"}},
			"containers":     []any{map[string]any{"name": "node", "image": "old-node"}},
		}}},
	}}

	r := &SiteReconciler{}
	if err := r.mutateNetObject(site, obj); err != nil {
		t.Fatalf("mutateNetObject returned error: %v", err)
	}

	if got := obj.GetNamespace(); got != "custom-net" {
		t.Fatalf("namespace = %q, want custom-net", got)
	}

	if got := containerImage(t, obj, false, "node"); got != "example/net:site" {
		t.Fatalf("node image = %q, want site image", got)
	}

	if got := containerImage(t, obj, true, "install-cni-plugins"); got != "example/net:site" {
		t.Fatalf("init image = %q, want site image", got)
	}
}

func TestMutateMachinaSkipsMetalmanSupport(t *testing.T) {
	enabled := true
	site := &unboundedv1alpha3.Site{Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Machina: &unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled}}}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata":   map[string]any{"name": "metalman-controller"},
	}}

	r := &SiteReconciler{}
	if err := r.mutateMachinaObject(site, obj); err != nil {
		t.Fatalf("mutateMachinaObject returned error: %v", err)
	}

	if obj.Object != nil {
		t.Fatalf("metalman support object was not skipped")
	}
}

func TestMetalmanDeployment(t *testing.T) {
	enabled := true
	dhcpAuto := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled, Namespace: "metal", Image: "example/metalman:site"},
			DHCPAutoInterface: &dhcpAuto,
		}}},
	}

	deployment := metalmanDeployment(site, Config{DefaultNamespace: DefaultNamespace, MetalmanImage: "example/metalman:default"})
	if deployment.Name != "metalman-controller-rack-a" {
		t.Fatalf("name = %q", deployment.Name)
	}

	if deployment.Namespace != "metal" {
		t.Fatalf("namespace = %q", deployment.Namespace)
	}

	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Image != "example/metalman:site" {
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

func containerImage(t *testing.T, obj *unstructured.Unstructured, init bool, name string) string {
	t.Helper()

	field := "containers"
	if init {
		field = "initContainers"
	}

	containers, ok, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
	if err != nil || !ok {
		t.Fatalf("missing %s: ok=%t err=%v", field, ok, err)
	}

	for _, item := range containers {
		container, ok := item.(map[string]any)
		if !ok {
			continue
		}

		if container["name"] == name {
			image, _ := container["image"].(string)
			return image
		}
	}

	t.Fatalf("container %q not found", name)

	return ""
}
