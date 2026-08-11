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

	hash, err := ensureConfig(t, env)
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

	hash, err := ensureConfig(t, env)
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

	res := reconcile(t, env, nil)
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

	res := reconcile(t, env, nil)
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

	res := reconcile(t, env, nil)
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

// ensureConfig plans and executes the net config operation, mirroring what the
// reconciler does so these tests exercise the production path.
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

	// net is a cluster component and plans no per-Site operations, so there is
	// no Site to attribute results to.
	return component.CombineResult(c.Name(), "", res, exec)
}

// TestPlanGolden pins the complete set of operations the net component plans.
//
// Net is the cluster dataplane and applies the largest object set of any
// component, including the ValidatingAdmissionPolicy that restricts what its
// own ServiceAccount may create. The reaper gates its migration on the
// config-hash annotation the two workloads carry
// (internal/operator/migrate.go), so an object or annotation silently
// appearing, disappearing or being renamed here breaks the upgrade path.
//
// Both workloads depend on the config, so a failure to write the ConfigMap
// skips them rather than rolling pods that cannot mount it.
func TestPlanGolden(t *testing.T) {
	env := testEnv(t)
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}}

	plan, res, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{site})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	want := `CreateIfAbsent ConfigMap/unbounded-system/unbounded-net-config
Apply ServiceAccount/unbounded-system/unbounded-net-controller
Apply ServiceAccount/unbounded-system/unbounded-net-kube-proxy
Apply ClusterRole/unbounded-net-controller
Apply ClusterRoleBinding/unbounded-net-kube-proxy
Apply ClusterRoleBinding/unbounded-net-controller
Apply Role/unbounded-system/unbounded-net-controller
Apply RoleBinding/unbounded-system/unbounded-net-controller
Apply Role/kube-system/unbounded-net-controller
Apply RoleBinding/kube-system/unbounded-net-controller
Apply Deployment/unbounded-system/unbounded-net-controller [overridable] [after ConfigMap/unbounded-system/unbounded-net-config]
Apply Service/unbounded-system/unbounded-net-controller
Apply ValidatingWebhookConfiguration/unbounded-net-validating-webhook
Apply APIService/v1alpha1.status.net.unbounded-cloud.io
Apply MutatingWebhookConfiguration/unbounded-net-mutating-webhook
Apply ValidatingAdmissionPolicy/unbounded-net-create-restriction
Apply ValidatingAdmissionPolicyBinding/unbounded-net-create-restriction
Apply ValidatingAdmissionPolicy/unbounded-net-node-field-restriction
Apply ValidatingAdmissionPolicyBinding/unbounded-net-node-field-restriction
Apply ClusterRole/unbounded-net-status-viewer
Apply ServiceAccount/unbounded-system/unbounded-net-node
Apply ClusterRole/unbounded-net-node
Apply ClusterRoleBinding/unbounded-net-node
Apply DaemonSet/unbounded-system/unbounded-net-node [overridable] [after ConfigMap/unbounded-system/unbounded-net-config]
`

	if got := plan.Summary(); got != want {
		t.Fatalf("plan =\n%s\nwant\n%s", got, want)
	}
}

// TestExecutionOrderGolden pins the order the executor runs net's plan in, as
// distinct from the order the component emits it.
//
// Summary, which TestPlanGolden asserts on, renders emission order. The
// executor sorts a copy, so for a long time nothing pinned what the cluster
// actually sees, and execution order was changed twice without a single test
// noticing.
//
// Two properties here are load-bearing rather than incidental. The ConfigMap
// and Service precede both workloads, because pods mount one and resolve the
// other. Admission registration and the APIService come last, because each
// points at the controller Deployment: registering a failurePolicy: Ignore
// webhook before its backend exists is a window in which it silently enforces
// nothing.
func TestExecutionOrderGolden(t *testing.T) {
	env := testEnv(t)
	site := unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}}

	plan, _, err := (Component{}).Plan(t.Context(), env, []unboundedv1alpha3.Site{site})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	want := `Apply ServiceAccount/unbounded-system/unbounded-net-controller
Apply ServiceAccount/unbounded-system/unbounded-net-kube-proxy
Apply ClusterRole/unbounded-net-controller
Apply ClusterRoleBinding/unbounded-net-kube-proxy
Apply ClusterRoleBinding/unbounded-net-controller
Apply Role/unbounded-system/unbounded-net-controller
Apply RoleBinding/unbounded-system/unbounded-net-controller
Apply Role/kube-system/unbounded-net-controller
Apply RoleBinding/kube-system/unbounded-net-controller
Apply ClusterRole/unbounded-net-status-viewer
Apply ServiceAccount/unbounded-system/unbounded-net-node
Apply ClusterRole/unbounded-net-node
Apply ClusterRoleBinding/unbounded-net-node
CreateIfAbsent ConfigMap/unbounded-system/unbounded-net-config
Apply Service/unbounded-system/unbounded-net-controller
Apply Deployment/unbounded-system/unbounded-net-controller
Apply DaemonSet/unbounded-system/unbounded-net-node
Apply ValidatingWebhookConfiguration/unbounded-net-validating-webhook
Apply APIService/v1alpha1.status.net.unbounded-cloud.io
Apply MutatingWebhookConfiguration/unbounded-net-mutating-webhook
Apply ValidatingAdmissionPolicy/unbounded-net-create-restriction
Apply ValidatingAdmissionPolicyBinding/unbounded-net-create-restriction
Apply ValidatingAdmissionPolicy/unbounded-net-node-field-restriction
Apply ValidatingAdmissionPolicyBinding/unbounded-net-node-field-restriction
`

	got, err := plan.ExecutionOrder()
	if err != nil {
		t.Fatalf("ExecutionOrder: %v", err)
	}

	if got != want {
		t.Fatalf("execution order =\n%s\nwant\n%s", got, want)
	}
}
