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
				}},
			}}

			if err := applyMutator("net-hash")(obj); err != nil {
				t.Fatalf("applyMutator: %v", err)
			}

			annotations, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
			if annotations[ConfigHashAnnotation] != "net-hash" || annotations["existing"] != "kept" {
				t.Fatalf("pod template annotations = %#v", annotations)
			}
		})
	}

	config := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": configName},
	}}
	if err := applyMutator("net-hash")(config); err != nil || config.Object != nil {
		t.Fatalf("embedded net ConfigMap was not skipped: err=%v object=%#v", err, config.Object)
	}

	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": component.CRDKind,
		"metadata": map[string]any{"name": "sites.unbounded-cloud.io"},
	}}
	if err := applyMutator("net-hash")(crd); err != nil || crd.Object != nil {
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
