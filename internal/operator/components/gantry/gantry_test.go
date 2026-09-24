// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, schedulingv1.AddToScheme, unboundedv1alpha3.AddToScheme} {
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

func TestPlanRejectsHelmManagedInstallation(t *testing.T) {
	priorityClass := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{
		Name: priorityClassName,
		Annotations: map[string]string{
			installationManagerAnnotation: installationManagerHelm,
		},
	}}
	env := testEnv(t, priorityClass)

	plan, res, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if plan.Len() != 0 {
		t.Fatalf("plan has %d operations, want none", plan.Len())
	}

	if res.Ready || res.Reason != reasonInstallationConflict || !strings.Contains(res.Message, `managed by "helm"`) {
		t.Fatalf("result = %+v, want Helm installation conflict", res)
	}
}

func TestPlanLeavesHelmManagedInstallationWhenDisabled(t *testing.T) {
	no := false
	priorityClass := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{
		Name: priorityClassName,
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "Helm",
		},
	}}
	env := testEnv(t, priorityClass)

	plan, res, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", &no)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if plan.Len() != 0 {
		t.Fatalf("plan has %d operations, want none", plan.Len())
	}

	if !res.Ready || res.Reason != component.ReasonDisabled {
		t.Fatalf("result = %+v, want external installation left disabled", res)
	}
}

func TestEnsureConfigCreatesDefaultOnlyWhenAbsent(t *testing.T) {
	env := testEnv(t)

	hash, err := ensureConfig(t, env)
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

	hash, err := ensureConfig(t, env)
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

	if err := applyMutator("ghcr.io/azure/gantry:test", "ghcr.io/azure/gantry-node-config:test", "gantry-hash", artifactStreamingConfig{})(ds); err != nil {
		t.Fatalf("applyMutator: %v", err)
	}

	annotations, _, _ := unstructured.NestedStringMap(ds.Object, "spec", "template", "metadata", "annotations")
	if annotations[configHashAnnotation] != "gantry-hash" {
		t.Fatalf("pod template annotations = %#v", annotations)
	}

	config := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": configName},
	}}
	if err := applyMutator("ghcr.io/azure/gantry:test", "ghcr.io/azure/gantry-node-config:test", "gantry-hash", artifactStreamingConfig{})(config); err != nil || config.Object != nil {
		t.Fatalf("gantry ConfigMap was not skipped: err=%v object=%#v", err, config.Object)
	}
}

func TestOperatorManifestAllowlist(t *testing.T) {
	want := map[string]bool{
		"configmap.yaml":         true,
		"daemonset.yaml":         true,
		"overlaybd-config.yaml":  true,
		"rendezvous-leases.yaml": true,
		"serviceaccount.yaml":    true,
	}

	if len(operatorManifestFiles) != len(want) {
		t.Fatalf("operator manifest allowlist = %v, want %v", operatorManifestFiles, want)
	}

	for file := range want {
		if _, ok := operatorManifestFiles[file]; !ok {
			t.Fatalf("operator manifest allowlist is missing %s", file)
		}
	}

	for _, standalone := range []string{"node-config.yaml", "examples/networkpolicy.yaml", "examples/registry-secret.example.yaml", "ownership-check.yaml"} {
		if _, ok := operatorManifestFiles[standalone]; ok {
			t.Fatalf("standalone resource %s is operator-managed", standalone)
		}
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

	if err := applyMutator(derived, "ghcr.io/azure/gantry-node-config:v1.2.3", "h", artifactStreamingConfig{})(agent); err != nil {
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

	if err := applyMutator(derived, "ghcr.io/azure/gantry-node-config:v1.2.3", "h", artifactStreamingConfig{})(nodeDS); err != nil {
		t.Fatalf("applyMutator node-config: %v", err)
	}

	if img := containerImage(t, nodeDS, "containers", "configure"); img != "mcr.microsoft.com/cbl-mariner/busybox:2.0" {
		t.Fatalf("node-config busybox image was rewritten to %q; must stay pinned", img)
	}
}

func TestApplyMutatorEnablesArtifactStreaming(t *testing.T) {
	artifact := artifactStreamingConfig{
		Enabled:      true,
		NodeSelector: map[string]string{"kubernetes.azure.com/agentpool": "streaming"},
	}

	agent := &unstructured.Unstructured{Object: map[string]any{
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
	overlay := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": overlayBDConfigDaemonSetName},
		"spec": map[string]any{"template": map[string]any{
			"spec": map[string]any{
				"nodeSelector": map[string]any{},
				"containers": []any{
					map[string]any{"name": overlayBDConfigContainerName, "image": "placeholder"},
				},
			},
		}},
	}}

	mutate := applyMutator("gantry:test", "gantry-node-config:test", "hash", artifact)
	if err := mutate(agent); err != nil {
		t.Fatal(err)
	}

	if err := mutate(overlay); err != nil {
		t.Fatal(err)
	}

	containers, _, err := unstructured.NestedSlice(agent.Object, "spec", "template", "spec", "containers")
	if err != nil {
		t.Fatal(err)
	}

	variableFound := false

	for _, entry := range containers[0].(map[string]any)["env"].([]any) {
		variable := entry.(map[string]any)
		if variable["name"] == artifactStreamingEnabledEnv && variable["value"] == "true" {
			variableFound = true
		}
	}

	if !variableFound {
		t.Fatal("artifact streaming environment override missing")
	}

	if image := containerImage(t, overlay, "containers", overlayBDConfigContainerName); image != "gantry-node-config:test" {
		t.Fatalf("configurator image = %q", image)
	}

	selector, _, err := unstructured.NestedStringMap(overlay.Object, "spec", "template", "spec", "nodeSelector")
	if err != nil || selector["kubernetes.azure.com/agentpool"] != "streaming" {
		t.Fatalf("selector = %#v, err=%v", selector, err)
	}
}

func TestResolveArtifactStreamingRejectsConflictingSites(t *testing.T) {
	sites := []unboundedv1alpha3.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Gantry: &unboundedv1alpha3.GantryComponentSpec{ArtifactStreaming: &unboundedv1alpha3.GantryArtifactStreamingSpec{Enabled: true, NodeSelector: map[string]string{"pool": "a"}}}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Gantry: &unboundedv1alpha3.GantryComponentSpec{ArtifactStreaming: &unboundedv1alpha3.GantryArtifactStreamingSpec{Enabled: true, NodeSelector: map[string]string{"pool": "b"}}}}}},
	}

	if _, err := resolveArtifactStreaming(sites); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("error = %v, want selector conflict", err)
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

	res := reconcile(t, env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	// Core Gantry objects are applied, while host configuration remains owned by
	// unbounded-agent.
	for _, want := range []string{
		"ServiceAccount/gantry", "DaemonSet/gantry", "PriorityClass/gantry-low", "Role/gantry-agent",
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

func TestReconcileDeletesOverlayBDConfiguratorWhenArtifactStreamingDisabled(t *testing.T) {
	configurator := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Namespace: component.DefaultNamespace,
		Name:      overlayBDConfigDaemonSetName,
	}}
	env, applied := reconcilerEnv(t, configurator)

	res := reconcile(t, env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(configurator), configurator); !apierrors.IsNotFound(err) {
		t.Fatalf("disabled artifact streaming retained configurator: %v", err)
	}

	if applied["DaemonSet/"+overlayBDConfigDaemonSetName] {
		t.Fatal("disabled artifact streaming reapplied configurator")
	}
}

func TestPlanAppliesOverlayBDConfiguratorWhenArtifactStreamingEnabled(t *testing.T) {
	gantryEnabled := true
	site := siteWithGantry("edge", &gantryEnabled)
	site.Spec.Components.Gantry.ArtifactStreaming = &unboundedv1alpha3.GantryArtifactStreamingSpec{
		Enabled:      true,
		NodeSelector: map[string]string{"pool": "streaming"},
	}

	plan, res, err := (Component{}).Plan(t.Context(), testEnv(t), []unboundedv1alpha3.Site{*site})
	if err != nil || !res.Ready {
		t.Fatalf("Plan = (%+v, %v), want ready", res, err)
	}

	summary := plan.Summary()

	want := "Apply DaemonSet/unbounded-system/" + overlayBDConfigDaemonSetName
	if !strings.Contains(summary, want) {
		t.Fatalf("plan does not apply enabled configurator:\n%s", summary)
	}

	if strings.Contains(summary, "Delete DaemonSet/unbounded-system/"+overlayBDConfigDaemonSetName) {
		t.Fatalf("plan deletes enabled configurator:\n%s", summary)
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

	res := reconcile(t, env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
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

	res := reconcile(t, env, []unboundedv1alpha3.Site{*siteWithGantry("edge", &no)})
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

			res := reconcile(t, env, []unboundedv1alpha3.Site{*siteWithGantry("edge", &no)})
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

// ensureConfig plans and executes the gantry config operation, mirroring what
// the reconciler does so these tests exercise the production path.
func ensureConfig(t *testing.T, env *component.Env) (string, error) {
	t.Helper()

	hash, op, err := planConfig(t.Context(), env)
	if err != nil {
		return "", err
	}

	if op == nil {
		return hash, nil
	}

	plan := component.NewPlan()
	plan.Add(*op)

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		return "", err
	}

	return hash, exec.Err()
}

// reconcile plans and executes the component, mirroring the reconciler.
func reconcile(t *testing.T, env *component.Env, sites []unboundedv1alpha3.Site) component.Result {
	t.Helper()

	c := Component{}

	plan, res, err := c.Plan(t.Context(), env, sites)
	if err != nil {
		return component.Failed(err)
	}

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		return component.Failed(err)
	}

	// gantry is a cluster component and plans no per-Site operations, so there
	// is no Site to attribute results to.
	return component.CombineResult(c.Name(), "", res, exec)
}

// TestPlanGolden pins the complete set of operations the gantry component
// plans.
//
// Two properties matter beyond the object set. The legacy node config is
// removed first and every apply depends on those deletes, so a failure to
// remove the legacy DaemonSet skips the replacement rather than running both
// side by side. The operator chart profile omits standalone node configuration
// and hardening resources, which remain owned by their respective installers.
func TestPlanGolden(t *testing.T) {
	env := testEnv(t)

	plan, res, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	const after = " [after DaemonSet/unbounded-system/gantry-containerd-config " +
		"ConfigMap/unbounded-system/gantry-containerd-hosts " +
		"ClusterRoleBinding/gantry-agent " +
		"ClusterRole/gantry-agent " +
		"DaemonSet/unbounded-system/gantry-overlaybd-config " +
		"ConfigMap/unbounded-system/gantry-config]"

	var chairPlan strings.Builder
	for index := range 64 {
		fmt.Fprintf(&chairPlan, "CreateIfAbsent Lease/unbounded-system/gantry-chair-%02d%s\n", index, after)
	}

	want := `Delete DaemonSet/unbounded-system/gantry-containerd-config
Delete ConfigMap/unbounded-system/gantry-containerd-hosts
Delete ClusterRoleBinding/gantry-agent
Delete ClusterRole/gantry-agent
Delete DaemonSet/unbounded-system/gantry-overlaybd-config
CreateIfAbsent ConfigMap/unbounded-system/gantry-config
Apply DaemonSet/unbounded-system/gantry [overridable]` + after + `
` + chairPlan.String() + `Apply PriorityClass/gantry-low` + after + `
Apply ServiceAccount/unbounded-system/gantry` + after + `
Apply Role/unbounded-system/gantry-agent` + after + `
Apply RoleBinding/unbounded-system/gantry-agent` + after + `
`

	if got := plan.Summary(); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// TestExecutionOrderGolden pins what the executor runs, as distinct from what
// the component emits.
//
// Gantry is the component where removal order matters: the legacy node-config
// DaemonSet is deleted before the ConfigMap it mounted, so a failure to remove
// the workload cannot strip the configuration out from under it.
func TestExecutionOrderGolden(t *testing.T) {
	env := testEnv(t)

	plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{*siteWithGantry("edge", nil)})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	got, err := plan.ExecutionOrder()
	if err != nil {
		t.Fatalf("ExecutionOrder: %v", err)
	}

	var chairOrder strings.Builder
	for index := range 64 {
		fmt.Fprintf(&chairOrder, "CreateIfAbsent Lease/unbounded-system/gantry-chair-%02d\n", index)
	}

	want := `Delete DaemonSet/unbounded-system/gantry-containerd-config
Delete DaemonSet/unbounded-system/gantry-overlaybd-config
Delete ConfigMap/unbounded-system/gantry-containerd-hosts
Delete ClusterRoleBinding/gantry-agent
Delete ClusterRole/gantry-agent
CreateIfAbsent ConfigMap/unbounded-system/gantry-config
Apply PriorityClass/gantry-low
Apply ServiceAccount/unbounded-system/gantry
Apply Role/unbounded-system/gantry-agent
Apply RoleBinding/unbounded-system/gantry-agent
` + chairOrder.String() + `Apply DaemonSet/unbounded-system/gantry
`

	if got != want {
		t.Fatalf("execution order =\n%s\nwant\n%s", got, want)
	}
}

func TestChairLeasePredicateReconcilesDeletionOnly(t *testing.T) {
	predicate := chairLeaseDeletePredicate("unbounded-system")
	chair := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{
		Name:      "gantry-chair-07",
		Namespace: "unbounded-system",
	}}

	if !predicate.Delete(event.DeleteEvent{Object: chair}) {
		t.Fatal("chair deletion did not trigger reconciliation")
	}

	if predicate.Create(event.CreateEvent{Object: chair}) ||
		predicate.Update(event.UpdateEvent{ObjectOld: chair, ObjectNew: chair}) {
		t.Fatal("chair create/update unexpectedly triggered reconciliation")
	}

	other := chair.DeepCopy()

	other.Name = "gantry-chair-99"
	if predicate.Delete(event.DeleteEvent{Object: other}) {
		t.Fatal("out-of-range chair name triggered reconciliation")
	}
}
