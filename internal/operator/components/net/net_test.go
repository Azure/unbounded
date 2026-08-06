// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"context"
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

func TestEnsureConfigCreatesDefaultOnlyWhenAbsent(t *testing.T) {
	env := testEnv(t)

	hash, err := ensureConfig(t.Context(), env)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}

	var got corev1.ConfigMap

	key := client.ObjectKey{Namespace: component.DefaultNamespace, Name: configName}
	if err := env.Client.Get(t.Context(), key, &got); err != nil {
		t.Fatalf("get default net config: %v", err)
	}

	if got.Data["config.yaml"] == "" || hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("default net config/hash missing: hash=%q data=%#v", hash, got.Data)
	}
}

func TestEnsureConfigPreservesExistingPayload(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: component.DefaultNamespace},
		Data:       map[string]string{"config.yaml": "custom: true", "extra": "keep"},
		BinaryData: map[string][]byte{"routing.bin": {0, 1, 2}},
	}
	env := testEnv(t, existing)

	hash, err := ensureConfig(t.Context(), env)
	if err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get preserved net config: %v", err)
	}

	if got.Data["config.yaml"] != "custom: true" || got.Data["extra"] != "keep" {
		t.Fatalf("existing net config changed: %#v", got.Data)
	}

	if hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact payload hash", hash)
	}
}

func TestApplyMutatorStampsBothWorkloads(t *testing.T) {
	cfg := component.Config{ImageRegistry: "registry.example.com", ImageTag: "v1.2.3"}

	for _, tc := range []struct{ kind, name string }{
		{kind: "Deployment", name: controllerName},
		{kind: "DaemonSet", name: nodeName},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "apps/v1",
				"kind":       tc.kind,
				"metadata":   map[string]any{"name": tc.name},
				"spec": map[string]any{"template": map[string]any{
					"metadata": map[string]any{"annotations": map[string]any{"existing": "kept"}},
					"spec": map[string]any{
						"initContainers": []any{map[string]any{"name": "init", "image": "old:init"}},
						"containers":     []any{map[string]any{"name": "main", "image": "old:main"}},
					},
				}},
			}}

			if err := applyMutator(cfg, "net-hash")(obj); err != nil {
				t.Fatalf("applyMutator: %v", err)
			}

			annotations, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
			if annotations[ConfigHashAnnotation] != "net-hash" || annotations["existing"] != "kept" {
				t.Fatalf("pod template annotations = %#v", annotations)
			}

			wantRepository := "unbounded-net-controller"
			if tc.name == nodeName {
				wantRepository = "unbounded-net-node"
			}

			for _, field := range []string{"initContainers", "containers"} {
				containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
				if got := containers[0].(map[string]any)["image"]; got != "registry.example.com/"+wantRepository+":v1.2.3" {
					t.Fatalf("%s image = %q", field, got)
				}
			}
		})
	}

	config := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": configName},
	}}
	if err := applyMutator(cfg, "net-hash")(config); err != nil || config.Object != nil {
		t.Fatalf("embedded net ConfigMap was not skipped: err=%v object=%#v", err, config.Object)
	}

	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": component.CRDKind,
		"metadata": map[string]any{"name": "sites.unbounded-cloud.io"},
	}}
	if err := applyMutator(cfg, "net-hash")(crd); err != nil || crd.Object != nil {
		t.Fatalf("CRD was not skipped: err=%v object=%#v", err, crd.Object)
	}
}

func TestReconcileRetainedWithNoSites(t *testing.T) {
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: component.DefaultNamespace, Name: configName},
		Data:       map[string]string{"config.yaml": "custom: retained"},
	}
	env, appliedHashes := retainedEnv(t, config)

	res := Component{}.Reconcile(t.Context(), env, nil)
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	wantHash := component.ConfigMapPayloadHash(config)
	for _, name := range []string{controllerName, nodeName} {
		if appliedHashes[name] != wantHash {
			t.Fatalf("%s applied hash = %q, want %q", name, appliedHashes[name], wantHash)
		}
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(config), &got); err != nil {
		t.Fatalf("get retained net config: %v", err)
	}

	if got.Data["config.yaml"] != "custom: retained" {
		t.Fatalf("retained net config changed: %#v", got.Data)
	}
}

func TestReconcileRecreatesDeletedRetainedConfigWithNoSites(t *testing.T) {
	retained := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: component.DefaultNamespace,
		Name:      controllerName,
	}}
	env, appliedHashes := retainedEnv(t, retained)

	res := Component{}.Reconcile(t.Context(), env, nil)
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	var config corev1.ConfigMap

	key := client.ObjectKey{Namespace: component.DefaultNamespace, Name: configName}
	if err := env.Client.Get(t.Context(), key, &config); err != nil {
		t.Fatalf("get recreated net config: %v", err)
	}

	wantHash := component.ConfigMapPayloadHash(&config)
	if config.Data["config.yaml"] == "" || appliedHashes[controllerName] != wantHash || appliedHashes[nodeName] != wantHash {
		t.Fatalf("recreated config/workload hashes = data=%#v hashes=%#v", config.Data, appliedHashes)
	}
}

func TestReconcileDoesNotCreateFromNothingWithNoSites(t *testing.T) {
	env, appliedHashes := retainedEnv(t)

	res := Component{}.Reconcile(t.Context(), env, nil)
	if !res.Ready || res.Reason != component.ReasonNoSites {
		t.Fatalf("Reconcile = %+v, want ready with NoSites", res)
	}

	err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: configName}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) || len(appliedHashes) != 0 {
		t.Fatalf("zero-Site reconcile created net from nothing: config err=%v hashes=%#v", err, appliedHashes)
	}
}

func retainedEnv(t *testing.T, objects ...client.Object) (*component.Env, map[string]string) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, unboundedv1alpha3.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	appliedHashes := map[string]string{}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applied, ok := obj.(interface {
					GetName() string
					GetKind() string
					UnstructuredContent() map[string]any
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				name := applied.GetName()
				if (applied.GetKind() != "Deployment" || name != controllerName) &&
					(applied.GetKind() != "DaemonSet" || name != nodeName) {
					return nil
				}

				hash, _, err := unstructured.NestedString(
					applied.UnstructuredContent(),
					"spec", "template", "metadata", "annotations", ConfigHashAnnotation,
				)
				if err != nil {
					t.Fatalf("get applied hash for %s: %v", name, err)
				}

				appliedHashes[name] = hash

				return nil
			},
		}).
		Build()

	return &component.Env{Client: cl, Scheme: scheme, Namespace: component.DefaultNamespace}, appliedHashes
}

func TestApplyMutatorScopesBaseNodeToUnsitedNodes(t *testing.T) {
	cfg := component.Config{ImageRegistry: "ghcr.io/azure", ImageTag: "v1"}

	node := daemonSetObject(nodeName)
	if err := applyMutator(cfg, "net-hash")(node); err != nil {
		t.Fatalf("applyMutator node: %v", err)
	}

	assertUnsitedAffinity(t, node)

	// The controller Deployment runs on the control plane and must not be scoped
	// to un-Sited nodes.
	controller := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": controllerName},
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{},
			"spec":     map[string]any{"containers": []any{map[string]any{"name": "controller", "image": "old"}}},
		}},
	}}
	if err := applyMutator(cfg, "net-hash")(controller); err != nil {
		t.Fatalf("applyMutator controller: %v", err)
	}

	if _, ok, _ := unstructured.NestedMap(controller.Object, "spec", "template", "spec", "affinity"); ok {
		t.Fatal("controller Deployment must not be scoped to un-Sited nodes")
	}
}

func TestScopeNodeDaemonSetToSite(t *testing.T) {
	base, err := baseNodeDaemonSet()
	if err != nil {
		t.Fatalf("baseNodeDaemonSet: %v", err)
	}

	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", UID: "edge-uid"},
		Spec:       unboundedv1alpha3.SiteSpec{ImageRegistry: "registry.corp.internal/unbounded"},
	}
	cfg := component.ConfigForSite(component.Config{ImageRegistry: "ghcr.io/azure", ImageTag: "v1.2.3"}, site)

	ds := base.DeepCopy()
	if err := scopeNodeDaemonSetToSite(site, cfg, "net-hash", ds); err != nil {
		t.Fatalf("scopeNodeDaemonSetToSite: %v", err)
	}

	if got := ds.GetName(); got != "unbounded-net-node-edge" {
		t.Fatalf("name = %q, want unbounded-net-node-edge", got)
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

	assertSiteAffinity(t, ds)
	assertSiteOwnerRef(t, ds.GetOwnerReferences(), "edge", "edge-uid")

	// Every container (init and main) pulls from the Site's registry.
	for _, field := range []string{"initContainers", "containers"} {
		containers, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", field)
		for _, c := range containers {
			if got := c.(map[string]any)["image"]; got != "registry.corp.internal/unbounded/unbounded-net-node:v1.2.3" {
				t.Fatalf("%s image = %q, want site-registry net node", field, got)
			}
		}
	}

	// The config volume and the LOG_LEVEL env both point at the per-Site config.
	volumes, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "volumes")
	if name := configVolumeName(t, volumes); name != "unbounded-net-config-edge" {
		t.Fatalf("config volume = %q, want unbounded-net-config-edge", name)
	}

	if name := logLevelConfigRef(t, ds); name != "unbounded-net-config-edge" {
		t.Fatalf("LOG_LEVEL configMapKeyRef = %q, want unbounded-net-config-edge", name)
	}

	annotations, _, _ := unstructured.NestedStringMap(ds.Object, "spec", "template", "metadata", "annotations")
	if annotations[ConfigHashAnnotation] != "net-hash" {
		t.Fatalf("config hash annotation = %q", annotations[ConfigHashAnnotation])
	}
}

func TestEnsureSiteConfigSeedsFromSharedConfig(t *testing.T) {
	shared := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configName, Namespace: component.DefaultNamespace},
		Data:       map[string]string{"config.yaml": "shared: tuned", "LOG_LEVEL": "4"},
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

	if got.Data["config.yaml"] != "shared: tuned" || got.Data["LOG_LEVEL"] != "4" {
		t.Fatalf("per-site config not seeded from shared config: %#v", got.Data)
	}

	if hash != component.ConfigMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact payload hash", hash)
	}

	assertSiteOwnerRef(t, got.OwnerReferences, "edge", "edge-uid")
}

func TestEnsureSiteConfigPreservesExisting(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: SiteConfigName("edge"), Namespace: component.DefaultNamespace},
		Data:       map[string]string{"config.yaml": "custom: true"},
	}
	env := testEnv(t, existing)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge", UID: "edge-uid"}}

	if _, err := ensureSiteConfig(t.Context(), env, site); err != nil {
		t.Fatalf("ensureSiteConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get preserved per-site config: %v", err)
	}

	if got.Data["config.yaml"] != "custom: true" {
		t.Fatalf("existing per-site config changed: %#v", got.Data)
	}

	assertSiteOwnerRef(t, got.OwnerReferences, "edge", "edge-uid")
}

func TestReconcileCreatesPerSiteNodeAndConfig(t *testing.T) {
	env, applied := appliedEnv(t)
	site := unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "edge", UID: "edge-uid"},
		Spec:       unboundedv1alpha3.SiteSpec{ImageRegistry: "registry.corp.internal/unbounded"},
	}

	res := Component{}.Reconcile(t.Context(), env, []unboundedv1alpha3.Site{site})
	if !res.Ready || res.Err != nil {
		t.Fatalf("Reconcile = %+v, want ready", res)
	}

	// Per-site config is created from the seeded shared default.
	var cfg corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: SiteConfigName("edge")}, &cfg); err != nil {
		t.Fatalf("per-site config not created: %v", err)
	}

	base := applied[nodeName]
	if base == nil {
		t.Fatalf("base node DaemonSet not applied; applied=%v", appliedNames(applied))
	}

	assertUnsitedAffinity(t, base)

	perSite := applied["unbounded-net-node-edge"]
	if perSite == nil {
		t.Fatalf("per-site node DaemonSet not applied; applied=%v", appliedNames(applied))
	}

	containers, _, _ := unstructured.NestedSlice(perSite.Object, "spec", "template", "spec", "containers")
	if got := containers[0].(map[string]any)["image"]; got != "registry.corp.internal/unbounded/unbounded-net-node:v1.2.3" {
		t.Fatalf("per-site node image = %q, want site registry", got)
	}
}

func daemonSetObject(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "DaemonSet",
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{"template": map[string]any{
			"metadata": map[string]any{},
			"spec":     map[string]any{"containers": []any{map[string]any{"name": "node", "image": "old"}}},
		}},
	}}
}

func configVolumeName(t *testing.T, volumes []any) string {
	t.Helper()

	for _, v := range volumes {
		vol, ok := v.(map[string]any)
		if !ok {
			continue
		}

		cm, ok := vol["configMap"].(map[string]any)
		if ok && vol["name"] == "runtime-config" {
			name, _ := cm["name"].(string)

			return name
		}
	}

	t.Fatal("runtime-config volume not found")

	return ""
}

func logLevelConfigRef(t *testing.T, ds *unstructured.Unstructured) string {
	t.Helper()

	containers, _, _ := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "containers")
	for _, c := range containers {
		container, ok := c.(map[string]any)
		if !ok {
			continue
		}

		envVars, _ := container["env"].([]any)
		for _, e := range envVars {
			envVar, ok := e.(map[string]any)
			if !ok || envVar["name"] != "LOG_LEVEL" {
				continue
			}

			valueFrom, _ := envVar["valueFrom"].(map[string]any)
			ref, _ := valueFrom["configMapKeyRef"].(map[string]any)
			name, _ := ref["name"].(string)

			return name
		}
	}

	t.Fatal("LOG_LEVEL configMapKeyRef not found")

	return ""
}

func assertUnsitedAffinity(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()

	affinity, ok, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec", "affinity")
	if err != nil || !ok {
		t.Fatalf("missing affinity: ok=%t err=%v", ok, err)
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

func appliedNames(applied map[string]*unstructured.Unstructured) []string {
	names := make([]string, 0, len(applied))
	for name := range applied {
		names = append(names, name)
	}

	return names
}

// appliedEnv builds an Env whose Apply interceptor captures each applied object
// by name so tests can assert on the server-side-applied content.
func appliedEnv(t *testing.T) (*component.Env, map[string]*unstructured.Unstructured) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{appsv1.AddToScheme, corev1.AddToScheme, unboundedv1alpha3.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}

	applied := map[string]*unstructured.Unstructured{}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				o, ok := obj.(interface {
					GetName() string
					UnstructuredContent() map[string]any
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				applied[o.GetName()] = &unstructured.Unstructured{Object: o.UnstructuredContent()}

				return nil
			},
		}).
		Build()

	return &component.Env{
		Client:    cl,
		Scheme:    scheme,
		Namespace: component.DefaultNamespace,
		Config:    component.Config{ImageRegistry: "ghcr.io/azure", ImageTag: "v1.2.3"},
	}, applied
}
