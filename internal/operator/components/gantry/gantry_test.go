// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gantry

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
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

func TestNodeConfigUsesAgentMarkerAndAcceptsLegacyMarker(t *testing.T) {
	manifest, err := fs.ReadFile(gantrymanifests.Manifests, "node-config.yaml")
	if err != nil {
		t.Fatalf("read node-config manifest: %v", err)
	}

	const (
		agentMarker  = "# Managed by unbounded-agent for Gantry."
		legacyMarker = "# Managed by the Gantry node-config DaemonSet."
	)

	content := string(manifest)
	if strings.Count(content, agentMarker) < 2 {
		t.Fatalf("node-config manifest does not write and recognize agent marker %q", agentMarker)
	}

	if !strings.Contains(content, legacyMarker) {
		t.Fatalf("node-config manifest does not recognize legacy marker %q", legacyMarker)
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

	if err := applyMutator("ghcr.io/azure/gantry:test", "gantry-hash")(ds); err != nil {
		t.Fatalf("applyMutator: %v", err)
	}

	annotations, _, _ := unstructured.NestedStringMap(ds.Object, "spec", "template", "metadata", "annotations")
	if annotations[configHashAnnotation] != "gantry-hash" {
		t.Fatalf("pod template annotations = %#v", annotations)
	}

	config := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": configName},
	}}
	if err := applyMutator("ghcr.io/azure/gantry:test", "gantry-hash")(config); err != nil || config.Object != nil {
		t.Fatalf("gantry ConfigMap was not skipped: err=%v object=%#v", err, config.Object)
	}
}

// TestApplyMutatorImagesOnlyAgentContainer asserts the operator-derived image is
// stamped only on the gantry agent's own container, leaving the busybox init
// container with its pinned public image.
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

	if err := applyMutator(derived, "h")(agent); err != nil {
		t.Fatalf("applyMutator: %v", err)
	}

	if initImg := containerImage(t, agent, "initContainers", "chown-hostpaths"); initImg != "mcr.microsoft.com/cbl-mariner/busybox:2.0" {
		t.Fatalf("busybox init image was rewritten to %q; must stay pinned", initImg)
	}

	if agentImg := containerImage(t, agent, "containers", agentContainerName); agentImg != derived {
		t.Fatalf("agent container image = %q, want derived %q", agentImg, derived)
	}

	// The standalone node-config DaemonSet is not an operator-managed workload.
	nodeDS := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": legacyNodeConfigDaemonSetName},
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{},
			"spec": map[string]any{
				"containers": []any{
					map[string]any{"name": "configure", "image": "mcr.microsoft.com/cbl-mariner/busybox:2.0"},
				},
			},
		}},
	}}

	if err := applyMutator(derived, "h")(nodeDS); err != nil {
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
	legacyConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: legacyNodeConfigName}}
	legacyDaemonSet := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: legacyNodeConfigDaemonSetName}}
	env, applied := reconcilerEnv(t, legacyConfig, legacyDaemonSet)

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	// Core Gantry objects are applied, while host configuration remains owned by
	// unbounded-agent.
	for _, want := range []string{
		"ServiceAccount/gantry", "DaemonSet/gantry", "PriorityClass/gantry-low", "ClusterRole/gantry-agent",
	} {
		if !applied[want] {
			t.Fatalf("expected %s to be applied; applied=%#v", want, applied)
		}
	}

	// The standalone node configurator and optional examples must not be applied.
	for key := range applied {
		if key == "ConfigMap/"+legacyNodeConfigName || key == "DaemonSet/"+legacyNodeConfigDaemonSetName ||
			key == "NetworkPolicy/gantry" || key == "Secret/gantry-registry-credentials" {
			t.Fatalf("excluded object %s was applied by the operator", key)
		}
	}

	var config corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: configName}, &config); err != nil {
		t.Fatalf("get operator-managed gantry config: %v", err)
	}

	for _, obj := range []client.Object{legacyConfig, legacyDaemonSet} {
		if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Fatalf("legacy node-config object %T was not deleted: %v", obj, err)
		}
	}

	// The gantry ConfigMap is reconciled separately, not via the manifest apply.
	if applied["ConfigMap/gantry-config"] {
		t.Fatal("gantry-config ConfigMap should be reconciled separately, not applied")
	}
}

func TestReconcileFailsWhenLegacyNodeConfigCleanupFails(t *testing.T) {
	wantErr := errors.New("delete denied")
	scheme := testScheme(t)
	applied := false
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.DeleteOption) error {
				return wantErr
			},
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applied = true

				return nil
			},
		}).
		Build()
	env := &component.Env{Client: cl, Scheme: scheme, Namespace: component.DefaultNamespace}

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
	if res.Ready || !errors.Is(res.Err, wantErr) {
		t.Fatalf("Reconcile = %+v, want failure wrapping %v", res, wantErr)
	}

	if applied {
		t.Fatal("manifests were applied after legacy node-config cleanup failed")
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
		{name: "legacy node config", existing: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: legacyNodeConfigName}}},
		{name: "legacy node-config DaemonSet", existing: &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: legacyNodeConfigDaemonSetName}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, applied := reconcilerEnv(t, tc.existing)

			res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", &no)})
			if !res.Ready || res.Err != nil {
				t.Fatalf("Reconcile = %+v, want ready", res)
			}

			legacy := tc.existing.GetName() == legacyNodeConfigName || tc.existing.GetName() == legacyNodeConfigDaemonSetName
			if legacy {
				if res.Reason != component.ReasonDisabled || len(applied) != 0 {
					t.Fatalf("legacy-only install was retained: result=%+v applied=%#v", res, applied)
				}

				if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(tc.existing), tc.existing); !apierrors.IsNotFound(err) {
					t.Fatalf("legacy-only object %T was not deleted: %v", tc.existing, err)
				}

				return
			}

			if !applied["DaemonSet/gantry"] {
				t.Fatalf("retained gantry install did not reconcile DaemonSet/gantry; applied=%#v", applied)
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
		want     bool
	}{
		{name: "agent config", existing: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: configName}}, want: true},
		{name: "agent DaemonSet", existing: &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: daemonSetName}}, want: true},
		{name: "legacy node config", existing: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: legacyNodeConfigName}}},
		{name: "legacy node-config DaemonSet", existing: &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: legacyNodeConfigDaemonSetName}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t, tc.existing)

			got, err := resourcesExist(t.Context(), env)
			if err != nil || got != tc.want {
				t.Fatalf("resourcesExist = %t, %v; want %t", got, err, tc.want)
			}
		})
	}
}

func TestApplyMutatorScopesBaseToUnsitedNodes(t *testing.T) {
	base, err := baseDaemonSet()
	if err != nil {
		t.Fatalf("baseDaemonSet: %v", err)
	}

	if err := applyMutator("ghcr.io/azure/gantry:v1", "h")(base); err != nil {
		t.Fatalf("applyMutator: %v", err)
	}

	affinity, ok, err := unstructured.NestedMap(base.Object, "spec", "template", "spec", "affinity")
	if err != nil || !ok {
		t.Fatalf("base gantry missing affinity: ok=%t err=%v", ok, err)
	}

	converted := &corev1.Affinity{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(affinity, converted); err != nil {
		t.Fatalf("convert affinity: %v", err)
	}

	terms := converted.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 2 {
		t.Fatalf("un-Sited affinity = %#v, want one term with two DoesNotExist", terms)
	}

	for _, expr := range terms[0].MatchExpressions {
		if expr.Operator != corev1.NodeSelectorOpDoesNotExist {
			t.Fatalf("expression %q operator = %q, want DoesNotExist", expr.Key, expr.Operator)
		}
	}
}

func TestScopeDaemonSetToSite(t *testing.T) {
	base, err := baseDaemonSet()
	if err != nil {
		t.Fatalf("baseDaemonSet: %v", err)
	}

	wantInit := containerImage(t, base, "initContainers", "chown-hostpaths")

	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", UID: "edge-uid"},
		Spec:       unboundedv1alpha3.SiteSpec{ImageRegistry: "registry.corp.internal/unbounded"},
	}
	cfg := component.ConfigForSite(component.Config{ImageRegistry: "ghcr.io/azure", ImageTag: "v1.2.3"}, site)

	ds := base.DeepCopy()
	if err := scopeDaemonSetToSite(site, cfg, "gantry-hash", ds); err != nil {
		t.Fatalf("scopeDaemonSetToSite: %v", err)
	}

	if got := ds.GetName(); got != "gantry-edge" {
		t.Fatalf("name = %q, want gantry-edge", got)
	}

	for _, path := range [][]string{
		{"spec", "selector", "matchLabels", component.SiteLabelKey},
		{"spec", "template", "metadata", "labels", component.SiteLabelKey},
	} {
		got, ok, err := unstructured.NestedString(ds.Object, path...)
		if err != nil || !ok || got != "edge" {
			t.Fatalf("%v = %q (ok=%t err=%v), want edge", path, got, ok, err)
		}
	}

	// Only the agent container is repointed at the Site's registry; the busybox
	// init container keeps its pinned public image.
	if got := containerImage(t, ds, "containers", agentContainerName); got != "registry.corp.internal/unbounded/gantry:v1.2.3" {
		t.Fatalf("agent image = %q, want site-registry gantry", got)
	}

	if got := containerImage(t, ds, "initContainers", "chown-hostpaths"); got != wantInit {
		t.Fatalf("busybox init image = %q, want unchanged %q", got, wantInit)
	}

	assertSiteAffinity(t, ds)
	assertSiteOwnerRef(t, ds.GetOwnerReferences(), "edge", "edge-uid")

	volumes, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "volumes")
	if name := configVolumeName(t, volumes); name != "gantry-config-edge" {
		t.Fatalf("config volume = %q, want gantry-config-edge", name)
	}

	annotations, _, _ := unstructured.NestedStringMap(ds.Object, "spec", "template", "metadata", "annotations")
	if annotations[configHashAnnotation] != "gantry-hash" {
		t.Fatalf("config hash annotation = %q", annotations[configHashAnnotation])
	}
}

func TestEnsureSiteConfigSeedsFromSharedConfig(t *testing.T) {
	shared := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: component.DefaultNamespace},
		Data:       map[string]string{"config.yaml": "upstream_registries:\n  - name: mirror\n"},
	}
	env := testEnv(t, shared)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge", UID: "edge-uid"}}

	hash, err := ensureSiteConfig(t.Context(), env, site)
	if err != nil {
		t.Fatalf("ensureSiteConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: SiteConfigName("edge")}, &got); err != nil {
		t.Fatalf("get per-site config: %v", err)
	}

	if got.Data["config.yaml"] != "upstream_registries:\n  - name: mirror\n" {
		t.Fatalf("per-site config not seeded from shared config: %#v", got.Data)
	}

	if hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact payload hash", hash)
	}

	assertSiteOwnerRef(t, got.OwnerReferences, "edge", "edge-uid")
}

func TestReconcileFansOutPerSiteAndCleansUpOptedOut(t *testing.T) {
	no := false
	// A per-Site DaemonSet and config left over for a site that now opts out.
	staleDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: SiteDaemonSetName("legacy")}}
	staleConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: SiteConfigName("legacy")}}

	env, applied := reconcilerEnv(t, staleDS, staleConfig)

	sites := []unboundedv1alpha3.Site{
		*siteWithGantry("edge", nil),   // enabled (default)
		*siteWithGantry("legacy", &no), // opted out
	}

	res := Component{}.Reconcile(t.Context(), env, sites)
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	// Base plus the enabled Site's DaemonSet are applied.
	if !applied["DaemonSet/gantry"] || !applied["DaemonSet/gantry-edge"] {
		t.Fatalf("expected base and per-site DaemonSets applied; applied=%#v", applied)
	}

	// The opted-out Site gets no per-site DaemonSet applied and its stale one is
	// removed.
	if applied["DaemonSet/gantry-legacy"] {
		t.Fatal("opted-out site had a per-site DaemonSet applied")
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(staleDS), &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("opted-out per-site DaemonSet not cleaned up: %v", err)
	}

	// The opted-out Site's config is preserved (user-editable; only GC'd with the
	// Site), so re-enabling does not lose custom registries/credentials.
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(staleConfig), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("opted-out per-site config was deleted, want preserved: %v", err)
	}

	// The enabled Site's config is created.
	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: SiteConfigName("edge")}, &corev1.ConfigMap{}); err != nil {
		t.Fatalf("enabled site config not created: %v", err)
	}
}

func TestReconcileFailSafeOrderingKeepsBaseOnPerSiteFailure(t *testing.T) {
	scheme := testScheme(t)
	applied := map[string]bool{}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
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

				if o.GetKind() == "DaemonSet" && o.GetName() == SiteDaemonSetName("edge") {
					return errors.New("apply per-site gantry failed")
				}

				return nil
			},
		}).
		Build()

	env := &component.Env{Client: cl, Scheme: scheme, Namespace: component.DefaultNamespace}

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
	if res.Ready || res.Err == nil {
		t.Fatalf("Reconcile = %+v, want failure", res)
	}

	// The base gantry DaemonSet must not be applied after the per-site apply
	// failed, so the blanket base keeps covering the fleet.
	if applied["DaemonSet/"+daemonSetName] {
		t.Fatalf("base gantry DaemonSet was applied despite per-site failure; applied=%#v", applied)
	}

	if !applied["DaemonSet/"+SiteDaemonSetName("edge")] {
		t.Fatalf("per-site apply was not attempted; applied=%#v", applied)
	}
}

func assertSiteAffinity(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()

	affinity, ok, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec", "affinity")
	if err != nil || !ok {
		t.Fatalf("missing site affinity: ok=%t err=%v", ok, err)
	}

	converted := &corev1.Affinity{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(affinity, converted); err != nil {
		t.Fatalf("convert affinity: %v", err)
	}

	terms := converted.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 2 {
		t.Fatalf("site affinity terms = %d, want 2", len(terms))
	}
}

func assertSiteOwnerRef(t *testing.T, refs []metav1.OwnerReference, siteName, uid string) {
	t.Helper()

	if len(refs) != 1 {
		t.Fatalf("ownerReferences len = %d, want 1: %#v", len(refs), refs)
	}

	ref := refs[0]
	if ref.Kind != "Site" || ref.Name != siteName || string(ref.UID) != uid {
		t.Fatalf("unexpected ownerRef: %#v", ref)
	}

	if ref.Controller == nil || !*ref.Controller {
		t.Fatalf("ownerRef is not a controller reference: %#v", ref)
	}
}

func configVolumeName(t *testing.T, volumes []any) string {
	t.Helper()

	for _, v := range volumes {
		vol, ok := v.(map[string]any)
		if !ok {
			continue
		}

		if cm, ok := vol["configMap"].(map[string]any); ok && vol["name"] == "config" {
			name, _ := cm["name"].(string)

			return name
		}
	}

	t.Fatal("config volume not found")

	return ""
}
