// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gantry

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, unboundedv1alpha3.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	return scheme
}

func testEnv(t *testing.T, objects ...client.Object) *component.Env {
	t.Helper()

	return &component.Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build(),
		Namespace: component.DefaultNamespace,
	}
}

func siteWithGantry(name string, enabled *bool) *unboundedv1alpha3.Site {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if enabled != nil {
		site.Spec.Components.Gantry = &unboundedv1alpha3.GantryComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: enabled},
		}
	}

	return site
}

func TestEnabledForDefaultsToEnabled(t *testing.T) {
	no := false
	yes := true

	cases := []struct {
		name    string
		site    *unboundedv1alpha3.Site
		enabled bool
	}{
		{name: "no gantry spec", site: siteWithGantry("a", nil), enabled: true},
		{name: "gantry spec without enabled", site: &unboundedv1alpha3.Site{Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Gantry: &unboundedv1alpha3.GantryComponentSpec{}}}}, enabled: true},
		{name: "explicit enabled=true", site: siteWithGantry("a", &yes), enabled: true},
		{name: "explicit enabled=false", site: siteWithGantry("a", &no), enabled: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EnabledFor(tc.site); got != tc.enabled {
				t.Fatalf("EnabledFor = %t, want %t", got, tc.enabled)
			}
		})
	}
}

func TestEnsureConfigCreatesDefaultOnlyWhenAbsent(t *testing.T) {
	env := testEnv(t)

	hash, err := ensureConfig(t.Context(), env)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: configName}, &got); err != nil {
		t.Fatalf("get default gantry config: %v", err)
	}

	if got.Data["config.yaml"] == "" || hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("default gantry config/hash missing: hash=%q data=%#v", hash, got.Data)
	}
}

func TestEnsureConfigPreservesExistingPayload(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: component.DefaultNamespace},
		Data:       map[string]string{"config.yaml": "upstream_registries:\n  - name: mine\n"},
	}
	env := testEnv(t, existing)

	hash, err := ensureConfig(t.Context(), env)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get preserved gantry config: %v", err)
	}

	if got.Data["config.yaml"] != "upstream_registries:\n  - name: mine\n" {
		t.Fatalf("existing gantry config changed: %#v", got.Data)
	}

	if hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact payload hash", hash)
	}
}

func TestApplyMutatorStampsDaemonSetAndSkipsConfig(t *testing.T) {
	ds := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": daemonSetName},
		"spec":       map[string]any{"template": map[string]any{"metadata": map[string]any{}}},
	}}

	if err := applyMutator("gantry-hash")(ds); err != nil {
		t.Fatalf("applyMutator: %v", err)
	}

	annotations, _, _ := unstructured.NestedStringMap(ds.Object, "spec", "template", "metadata", "annotations")
	if annotations[configHashAnnotation] != "gantry-hash" {
		t.Fatalf("pod template annotations = %#v", annotations)
	}

	config := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": configName},
	}}
	if err := applyMutator("gantry-hash")(config); err != nil || config.Object != nil {
		t.Fatalf("gantry ConfigMap was not skipped: err=%v object=%#v", err, config.Object)
	}
}

func TestReconcileAppliesCoreManifestsAndSkipsExamples(t *testing.T) {
	env, applied := reconcilerEnv(t)

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	// Core install objects are applied; the DaemonSet carries the config hash.
	for _, want := range []string{"ServiceAccount/gantry", "DaemonSet/gantry", "PriorityClass/gantry-low", "ClusterRole/gantry-agent"} {
		if !applied[want] {
			t.Fatalf("expected %s to be applied; applied=%#v", want, applied)
		}
	}

	// The optional examples (NetworkPolicy, sample Secret) must NOT be applied.
	for key := range applied {
		if key == "NetworkPolicy/gantry" || key == "Secret/gantry-registry-credentials" {
			t.Fatalf("example object %s was applied by the default install", key)
		}
	}

	// The gantry ConfigMap is reconciled separately, not via the manifest apply.
	if applied["ConfigMap/gantry-config"] {
		t.Fatal("gantry-config ConfigMap should be reconciled separately, not applied")
	}
}

func TestReconcileRetainedWhenAllSitesOptOut(t *testing.T) {
	no := false
	env, applied := reconcilerEnv(t)

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", &no)})
	if !res.Ready || res.Reason != component.ReasonDisabled {
		t.Fatalf("Reconcile = %+v, want ready with Disabled", res)
	}

	if len(applied) != 0 {
		t.Fatalf("opted-out reconcile applied objects from nothing: %#v", applied)
	}
}

func TestReconcileRetainsExistingWhenAllSitesOptOut(t *testing.T) {
	no := false
	existingDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: daemonSetName}}
	env, applied := reconcilerEnv(t, existingDS)

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", &no)})
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	if !applied["DaemonSet/gantry"] {
		t.Fatalf("retained gantry install was not reconciled; applied=%#v", applied)
	}
}

// reconcilerEnv builds an Env whose Apply interceptor records applied objects as
// "Kind/name" keys.
func reconcilerEnv(t *testing.T, objects ...client.Object) (*component.Env, map[string]bool) {
	t.Helper()

	scheme := testScheme(t)
	applied := map[string]bool{}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				o, ok := obj.(interface {
					GetName() string
					GetKind() string
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				applied[o.GetKind()+"/"+o.GetName()] = true

				return nil
			},
		}).
		Build()

	return &component.Env{Client: cl, Scheme: scheme, Namespace: component.DefaultNamespace}, applied
}

func TestResourcesExist(t *testing.T) {
	env := testEnv(t)

	got, err := resourcesExist(t.Context(), env)
	if err != nil || got {
		t.Fatalf("resourcesExist on empty cluster = %t, %v", got, err)
	}

	envWithDS := testEnv(t, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: daemonSetName}})

	got, err = resourcesExist(t.Context(), envWithDS)
	if err != nil || !got {
		t.Fatalf("resourcesExist with DaemonSet = %t, %v", got, err)
	}
}
