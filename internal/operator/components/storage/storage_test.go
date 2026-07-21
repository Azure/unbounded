// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storage

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
	"github.com/Azure/unbounded/internal/operator/component"
)

func testEnv(t *testing.T, objects ...client.Object) *component.Env {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, unboundedv1alpha3.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Namespace: component.DefaultNamespace,
	}
}

func TestEnabled(t *testing.T) {
	if (Component{}).Enabled(&unboundedv1alpha3.Site{}) {
		t.Fatal("storage enabled with no component spec")
	}

	enabled := true
	site := &unboundedv1alpha3.Site{Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{
		Storage: &unboundedv1alpha3.StorageComponentSpec{SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled}},
	}}}

	if !(Component{}).Enabled(site) {
		t.Fatal("storage not enabled when spec enables it")
	}
}

func TestMutateObjectScopesDaemonSetToSite(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": daemonSetName},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": daemonSetName}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": daemonSetName}},
				"spec": map[string]any{
					"initContainers": []any{map[string]any{"name": "install", "image": "old:init"}},
					"containers":     []any{map[string]any{"name": "run", "image": "old:run"}},
				},
			},
		},
	}}

	cfg := component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"}
	if err := mutateObject(site, cfg, "storage-hash", obj); err != nil {
		t.Fatalf("mutateObject returned error: %v", err)
	}

	if got := obj.GetName(); got != "unbounded-storage-supervisor-rack-a" {
		t.Fatalf("name = %q, want unbounded-storage-supervisor-rack-a", got)
	}

	if got := obj.GetLabels()[component.SiteLabelKey]; got != "rack-a" {
		t.Fatalf("metadata site label = %q, want rack-a", got)
	}

	for _, path := range [][]string{
		{"spec", "selector", "matchLabels", component.SiteLabelKey},
		{"spec", "template", "metadata", "labels", component.SiteLabelKey},
	} {
		got, ok, err := unstructured.NestedString(obj.Object, path...)
		if err != nil || !ok {
			t.Fatalf("missing %v: ok=%t err=%v", path, ok, err)
		}

		if got != "rack-a" {
			t.Fatalf("%v = %q, want rack-a", path, got)
		}
	}

	assertSiteOwnerRef(t, obj.GetOwnerReferences(), "rack-a", "site-uid")

	annotations, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	if annotations[ConfigHashAnnotation] != "storage-hash" {
		t.Fatalf("storage config hash annotation = %q, want storage-hash", annotations[ConfigHashAnnotation])
	}

	affinity, ok, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec", "affinity")
	if err != nil || !ok {
		t.Fatalf("missing storage affinity: ok=%t err=%v", ok, err)
	}

	assertSiteAffinityMap(t, affinity, "rack-a")

	for _, field := range []string{"initContainers", "containers"} {
		containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
		if got := containers[0].(map[string]any)["image"]; got != "registry.example.com/azure/unbounded-storage-supervisor:v1.2.3" {
			t.Fatalf("%s image = %q", field, got)
		}
	}
}

func TestDaemonSetPointsAtPerSiteConfig(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": daemonSetName},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": daemonSetName}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": daemonSetName}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "run"}},
					"volumes": []any{map[string]any{
						"name":      "config-source",
						"configMap": map[string]any{"name": configName},
					}},
				},
			},
		},
	}}

	if err := mutateObject(site, component.Config{}, "storage-hash", obj); err != nil {
		t.Fatalf("mutateObject returned error: %v", err)
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

func TestEnsureConfigCreatesDefaultWhenAbsent(t *testing.T) {
	env := testEnv(t)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}

	hash, err := ensureConfig(t.Context(), env, site)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: "unbounded-storage-config-rack-a"}, &got); err != nil {
		t.Fatalf("get created configmap: %v", err)
	}

	if got.Data["config.yaml"] == "" {
		t.Fatalf("default config.yaml was not seeded")
	}

	if hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact stored payload hash", hash)
	}

	assertSiteOwnerRef(t, got.OwnerReferences, "rack-a", "site-uid")
}

func TestEnsureConfigAdoptsExistingAndPreservesData(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config-rack-a", Namespace: component.DefaultNamespace},
		Data:       map[string]string{"config.yaml": "custom: true"},
	}
	env := testEnv(t, existing)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}

	hash, err := ensureConfig(t.Context(), env, site)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get adopted configmap: %v", err)
	}

	if got.Data["config.yaml"] != "custom: true" {
		t.Fatalf("existing config data was not preserved: %q", got.Data["config.yaml"])
	}

	if hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact adopted payload hash", hash)
	}

	assertSiteOwnerRef(t, got.OwnerReferences, "rack-a", "site-uid")
}

func TestCleanupDeletesPerSiteResourcesOnly(t *testing.T) {
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-supervisor-rack-a", Namespace: component.DefaultNamespace}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config-rack-a", Namespace: component.DefaultNamespace}}
	otherDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-supervisor-rack-b", Namespace: component.DefaultNamespace}}

	env := testEnv(t, ds, cm, otherDS)

	if err := (Component{}).Cleanup(t.Context(), env, &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(ds), &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected rack-a DaemonSet deleted, got err=%v", err)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected rack-a ConfigMap deleted, got err=%v", err)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(otherDS), &appsv1.DaemonSet{}); err != nil {
		t.Fatalf("expected rack-b DaemonSet preserved, got err=%v", err)
	}
}

func assertSiteOwnerRef(t *testing.T, refs []metav1.OwnerReference, siteName, uid string) {
	t.Helper()

	if len(refs) != 1 {
		t.Fatalf("ownerReferences len = %d, want 1: %#v", len(refs), refs)
	}

	ref := refs[0]
	if ref.APIVersion != unboundedv1alpha3.GroupVersion.String() || ref.Kind != "Site" || ref.Name != siteName {
		t.Fatalf("unexpected ownerRef: %#v", ref)
	}

	if uid != "" && string(ref.UID) != uid {
		t.Fatalf("ownerRef UID = %q, want %q", ref.UID, uid)
	}

	// The reference must be a controller reference; Owns() enqueues only via
	// metav1.GetControllerOf, so a non-controller ref breaks per-site self-heal.
	if ref.Controller == nil || !*ref.Controller {
		t.Fatalf("ownerRef is not a controller reference: %#v", ref)
	}
}

func assertSiteAffinityMap(t *testing.T, affinity map[string]any, siteName string) {
	t.Helper()

	converted := &corev1.Affinity{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(affinity, converted); err != nil {
		t.Fatalf("convert affinity: %v", err)
	}

	if converted.NodeAffinity == nil || converted.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("missing node affinity: %#v", converted)
	}

	terms := converted.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 2 {
		t.Fatalf("node selector terms len = %d, want 2", len(terms))
	}

	want := map[string]bool{component.SiteLabelKey: false, component.DeprecatedSiteLabelKey: false}

	for _, term := range terms {
		expr := term.MatchExpressions[0]
		if len(expr.Values) != 1 || expr.Values[0] != siteName {
			t.Fatalf("unexpected site affinity expression: %#v", expr)
		}

		want[expr.Key] = true
	}

	for key, seen := range want {
		if !seen {
			t.Fatalf("site affinity missing key %q", key)
		}
	}
}
