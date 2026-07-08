// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	if got := obj.GetName(); got != "unbounded-storage-config-rack-a" {
		t.Fatalf("configmap name = %q, want unbounded-storage-config-rack-a", got)
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

	deployment := metalmanDeployment(site, DefaultNamespace, Config{MetalmanImage: "example/metalman:default"})
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

	if got := deployment.Spec.Template.Spec.NodeSelector[siteLabelKey]; got != "rack-a" {
		t.Fatalf("site node selector = %q", got)
	}

	if siteLabelKey != unboundedv1alpha3.MachineSiteLabelKey {
		t.Fatalf("expected metalman to node-select on the canonical site label %q, got %q", unboundedv1alpha3.MachineSiteLabelKey, siteLabelKey)
	}

	if _, ok := deployment.Spec.Template.Spec.NodeSelector["net.unbounded-cloud.io/site"]; ok {
		t.Fatalf("metalman must not node-select on the deprecated net.unbounded-cloud.io/site label")
	}

	// hostNetwork singleton: must terminate the old pod before creating the new
	// one so it can rebind its host ports on a rolling restart.
	strategy := deployment.Spec.Strategy
	if strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || strategy.RollingUpdate == nil {
		t.Fatalf("expected RollingUpdate strategy, got %+v", strategy)
	}

	if got := strategy.RollingUpdate.MaxSurge; got == nil || got.IntValue() != 0 {
		t.Fatalf("expected maxSurge=0, got %v", got)
	}

	if got := strategy.RollingUpdate.MaxUnavailable; got == nil || got.IntValue() != 1 {
		t.Fatalf("expected maxUnavailable=1, got %v", got)
	}
}

func TestMetalmanDeploymentRespectsNamespace(t *testing.T) {
	enabled := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
		}}},
	}

	deployment := metalmanDeployment(site, "custom-ns", Config{MetalmanImage: "example/metalman:default"})
	if deployment.Namespace != "custom-ns" {
		t.Fatalf("namespace = %q, want custom-ns", deployment.Namespace)
	}
}

func TestReconcilerNamespaceFallsBackToDefault(t *testing.T) {
	r := &SiteReconciler{}
	if got := r.namespace(); got != DefaultNamespace {
		t.Fatalf("namespace() = %q, want %q", got, DefaultNamespace)
	}

	r.Namespace = "custom-ns"
	if got := r.namespace(); got != "custom-ns" {
		t.Fatalf("namespace() = %q, want custom-ns", got)
	}
}

func TestStorageDaemonSetPointsAtPerSiteConfig(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": "unbounded-storage-supervisor"},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "run"}},
					"volumes": []any{map[string]any{
						"name":      "config-source",
						"configMap": map[string]any{"name": "unbounded-storage-config"},
					}},
				},
			},
		},
	}}

	if err := mutateStorageObject(site, obj); err != nil {
		t.Fatalf("mutateStorageObject returned error: %v", err)
	}

	volumes, ok, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "volumes")
	if err != nil || !ok {
		t.Fatalf("missing volumes: ok=%t err=%v", ok, err)
	}

	vol := volumes[0].(map[string]any)
	cm := vol["configMap"].(map[string]any)

	if cm["name"] != "unbounded-storage-config-rack-a" {
		t.Fatalf("config volume name = %v, want unbounded-storage-config-rack-a", cm["name"])
	}
}

func TestReconcilePerSiteComponentDisabledRunsCleanup(t *testing.T) {
	cleaned := false
	status := (&SiteReconciler{}).reconcilePerSiteComponent(
		false,
		func() error { t.Fatal("reconcile must not run when disabled"); return nil },
		func() error { cleaned = true; return nil },
	)

	if !cleaned {
		t.Fatal("cleanup was not called for a disabled component")
	}

	if !status.Ready || status.Message != "disabled" {
		t.Fatalf("status = %+v, want ready/disabled", status)
	}
}

func TestCleanupStorageDeletesPerSiteResourcesOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add appsv1: %v", err)
	}

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-supervisor-rack-a", Namespace: DefaultNamespace}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config-rack-a", Namespace: DefaultNamespace}}
	otherDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-supervisor-rack-b", Namespace: DefaultNamespace}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ds, cm, otherDS).Build()
	r := &SiteReconciler{Client: cl}

	if err := r.cleanupStorage(t.Context(), &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}); err != nil {
		t.Fatalf("cleanupStorage: %v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(ds), &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected rack-a DaemonSet deleted, got err=%v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected rack-a ConfigMap deleted, got err=%v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(otherDS), &appsv1.DaemonSet{}); err != nil {
		t.Fatalf("expected rack-b DaemonSet preserved, got err=%v", err)
	}
}

func TestFinalizeSiteCleansUpAndReleasesFinalizer(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add appsv1: %v", err)
	}

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	if err := unboundedv1alpha3.AddToScheme(scheme); err != nil {
		t.Fatalf("add v1alpha3: %v", err)
	}

	now := metav1.Now()
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{
		Name:              "rack-a",
		Finalizers:        []string{siteFinalizer},
		DeletionTimestamp: &now,
	}}
	metalman := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "metalman-controller-rack-a", Namespace: DefaultNamespace}}
	storage := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-supervisor-rack-a", Namespace: DefaultNamespace}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(site, metalman, storage).Build()
	r := &SiteReconciler{Client: cl}

	if err := r.finalizeSite(t.Context(), site); err != nil {
		t.Fatalf("finalizeSite: %v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(metalman), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected metalman Deployment deleted, got err=%v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(storage), &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected storage DaemonSet deleted, got err=%v", err)
	}

	// The Site should now be fully gone (finalizer removed while deleting).
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(site), &unboundedv1alpha3.Site{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected Site removed after finalizer release, got err=%v", err)
	}
}

func TestDeleteManifestDataSkipsClusterSharedInfra(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add appsv1: %v", err)
	}

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-system"}}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-controller", Namespace: "unbounded-system"}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, dep).Build()
	r := &SiteReconciler{Client: cl}

	data := []byte(`apiVersion: v1
kind: Namespace
metadata:
  name: unbounded-system
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: sites.example.com
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: unbounded-net-controller
  namespace: unbounded-system
`)

	if err := r.deleteManifestData(t.Context(), data, nil); err != nil {
		t.Fatalf("deleteManifestData: %v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(ns), &corev1.Namespace{}); err != nil {
		t.Fatalf("Namespace must not be deleted on teardown, got err=%v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(dep), &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected workload Deployment deleted, got err=%v", err)
	}
}

func TestRetargetNamespaceRewritesToCustomNamespace(t *testing.T) {
	r := &SiteReconciler{Namespace: "custom-ns"}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "x", "namespace": "unbounded-system"},
		"subjects":   []any{map[string]any{"namespace": "unbounded-system"}},
	}}

	r.retargetNamespace(obj)

	if got := obj.GetNamespace(); got != "custom-ns" {
		t.Fatalf("namespace = %q, want custom-ns", got)
	}

	subjects, _, _ := unstructured.NestedSlice(obj.Object, "subjects")
	if subjects[0].(map[string]any)["namespace"] != "custom-ns" {
		t.Fatalf("subject namespace not rewritten: %v", subjects[0])
	}

	// Default install is a no-op.
	def := &SiteReconciler{}
	nsObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": "unbounded-system"},
	}}
	def.retargetNamespace(nsObj)

	if nsObj.GetName() != "unbounded-system" {
		t.Fatalf("default install rewrote namespace to %q", nsObj.GetName())
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
