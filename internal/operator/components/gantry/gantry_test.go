// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gantry

import (
	"context"
	"strings"
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
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{},
			"spec": map[string]any{"containers": []any{
				map[string]any{"name": agentContainerName, "image": "placeholder"},
			}},
		}},
	}}

	if err := applyMutator("ghcr.io/azure/gantry:test", "gantry-hash", "node-hash")(ds); err != nil {
		t.Fatalf("applyMutator: %v", err)
	}

	annotations, _, _ := unstructured.NestedStringMap(ds.Object, "spec", "template", "metadata", "annotations")
	if annotations[configHashAnnotation] != "gantry-hash" {
		t.Fatalf("pod template annotations = %#v", annotations)
	}

	nodeDS := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": nodeConfigDaemonSetName},
		"spec":       map[string]any{"template": map[string]any{"metadata": map[string]any{}}},
	}}

	if err := applyMutator("ghcr.io/azure/gantry:test", "gantry-hash", "node-hash")(nodeDS); err != nil {
		t.Fatalf("applyMutator node-config: %v", err)
	}

	nodeAnnotations, _, _ := unstructured.NestedStringMap(nodeDS.Object, "spec", "template", "metadata", "annotations")
	if nodeAnnotations[nodeConfigHashAnnotation] != "node-hash" {
		t.Fatalf("node-config pod template annotations = %#v", nodeAnnotations)
	}

	config := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": configName},
	}}
	if err := applyMutator("ghcr.io/azure/gantry:test", "gantry-hash", "node-hash")(config); err != nil || config.Object != nil {
		t.Fatalf("gantry ConfigMap was not skipped: err=%v object=%#v", err, config.Object)
	}
}

// TestApplyMutatorImagesOnlyAgentContainer asserts the operator-derived image is
// stamped only on the gantry agent's own container, leaving the busybox init
// container (and, on the node-config DaemonSet, the busybox worker) with their
// pinned public images.
func TestApplyMutatorImagesOnlyAgentContainer(t *testing.T) {
	const derived = "ghcr.io/azure/gantry:v1.2.3"

	agent := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": daemonSetName},
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{},
			"spec": map[string]any{
				"initContainers": []any{
					map[string]any{"name": "chown-hostpaths", "image": "mcr.microsoft.com/cbl-mariner/busybox:2.0"},
				},
				"containers": []any{
					map[string]any{"name": agentContainerName, "image": "placeholder"},
				},
			},
		}},
	}}

	if err := applyMutator(derived, "h", "n")(agent); err != nil {
		t.Fatalf("applyMutator: %v", err)
	}

	if initImg := containerImage(t, agent, "initContainers", "chown-hostpaths"); initImg != "mcr.microsoft.com/cbl-mariner/busybox:2.0" {
		t.Fatalf("busybox init image was rewritten to %q; must stay pinned", initImg)
	}

	if agentImg := containerImage(t, agent, "containers", agentContainerName); agentImg != derived {
		t.Fatalf("agent container image = %q, want derived %q", agentImg, derived)
	}

	// The node-config DaemonSet is entirely busybox and must not be re-imaged.
	nodeDS := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": nodeConfigDaemonSetName},
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{},
			"spec": map[string]any{
				"containers": []any{
					map[string]any{"name": "configure", "image": "mcr.microsoft.com/cbl-mariner/busybox:2.0"},
				},
			},
		}},
	}}

	if err := applyMutator(derived, "h", "n")(nodeDS); err != nil {
		t.Fatalf("applyMutator node-config: %v", err)
	}

	if img := containerImage(t, nodeDS, "containers", "configure"); img != "mcr.microsoft.com/cbl-mariner/busybox:2.0" {
		t.Fatalf("node-config busybox image was rewritten to %q; must stay pinned", img)
	}
}

func containerImage(t *testing.T, obj *unstructured.Unstructured, field, name string) string {
	t.Helper()

	containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
	if err != nil {
		t.Fatalf("get %s: %v", field, err)
	}

	for _, c := range containers {
		container, ok := c.(map[string]any)
		if ok && container["name"] == name {
			image, _ := container["image"].(string)

			return image
		}
	}

	t.Fatalf("container %q not found in %s", name, field)

	return ""
}

func TestReconcileAppliesCoreManifestsAndSkipsExamples(t *testing.T) {
	env, applied := reconcilerEnv(t)

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	// Core install objects are applied, including the node-config objects that
	// wire node containerd through the mirror.
	for _, want := range []string{
		"ServiceAccount/gantry", "DaemonSet/gantry", "PriorityClass/gantry-low", "ClusterRole/gantry-agent",
		"ConfigMap/gantry-containerd-hosts", "DaemonSet/gantry-containerd-config",
	} {
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

	// The shared system namespace is owned by the operator's namespace
	// bootstrap; a component reconcile must never apply (and clobber) it, even
	// though gantry ships a Namespace doc for the standalone kubectl-apply path.
	for key := range applied {
		if strings.HasPrefix(key, "Namespace/") {
			t.Fatalf("component reconcile applied a Namespace object %q; it must be skipped", key)
		}
	}
}

func TestReconcileRetainedWhenAllSitesOptOut(t *testing.T) {
	no := false
	env, applied := reconcilerEnv(t)

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", &no)})
	if !res.Ready || res.Reason != component.ReasonDisabled {
		t.Fatalf("Reconcile = %+v, want ready with Disabled", res)
	}

	if res.Message != "no site enables gantry" {
		t.Fatalf("disabled message = %q", res.Message)
	}

	if len(applied) != 0 {
		t.Fatalf("opted-out reconcile applied objects from nothing: %#v", applied)
	}
}

func TestReconcileRetainsExistingWhenAllSitesOptOut(t *testing.T) {
	no := false

	for _, tc := range []struct {
		name     string
		existing client.Object
	}{
		{name: "agent config", existing: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: configName}}},
		{name: "agent DaemonSet", existing: &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: daemonSetName}}},
		{name: "node config", existing: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: nodeConfigName}}},
		{name: "node-config DaemonSet", existing: &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: nodeConfigDaemonSetName}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, applied := reconcilerEnv(t, tc.existing)

			res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", &no)})
			if !res.Ready || res.Err != nil {
				t.Fatalf("Reconcile = %+v, want ready", res)
			}

			for _, want := range []string{"DaemonSet/gantry", "DaemonSet/gantry-containerd-config"} {
				if !applied[want] {
					t.Fatalf("retained gantry install did not reconcile %s; applied=%#v", want, applied)
				}
			}
		})
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

	for _, tc := range []struct {
		name     string
		existing client.Object
	}{
		{name: "agent config", existing: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: configName}}},
		{name: "agent DaemonSet", existing: &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: daemonSetName}}},
		{name: "node config", existing: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: nodeConfigName}}},
		{name: "node-config DaemonSet", existing: &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: nodeConfigDaemonSetName}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t, tc.existing)

			got, err := resourcesExist(t.Context(), env)
			if err != nil || !got {
				t.Fatalf("resourcesExist = %t, %v", got, err)
			}
		})
	}
}
