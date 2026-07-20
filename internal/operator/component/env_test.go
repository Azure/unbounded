// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	for name, add := range map[string]func(*runtime.Scheme) error{
		"apps/v1":     appsv1.AddToScheme,
		"core/v1":     corev1.AddToScheme,
		"machina API": unboundedv1alpha3.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add %s to scheme: %v", name, err)
		}
	}

	return scheme
}

func TestConfigMapPayloadHashIncludesDataAndBinaryData(t *testing.T) {
	first := &corev1.ConfigMap{
		Data:       map[string]string{"b": "2", "a": "1"},
		BinaryData: map[string][]byte{"z": {3}, "x": {1, 2}},
	}
	orderedDifferently := &corev1.ConfigMap{
		Data:       map[string]string{"a": "1", "b": "2"},
		BinaryData: map[string][]byte{"x": {1, 2}, "z": {3}},
	}

	if ConfigMapPayloadHash(first) != ConfigMapPayloadHash(orderedDifferently) {
		t.Fatal("payload hash depends on map iteration order")
	}

	changedData := first.DeepCopy()
	changedData.Data["a"] = "changed"
	changedBinary := first.DeepCopy()
	changedBinary.BinaryData["x"] = []byte{2, 1}

	if ConfigMapPayloadHash(first) == ConfigMapPayloadHash(changedData) ||
		ConfigMapPayloadHash(first) == ConfigMapPayloadHash(changedBinary) {
		t.Fatal("payload hash did not include all Data and BinaryData")
	}
}

func TestRetargetNamespaceRewritesToCustomNamespace(t *testing.T) {
	env := &Env{Namespace: "custom-ns"}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "x", "namespace": "unbounded-system"},
		"subjects":   []any{map[string]any{"namespace": "unbounded-system"}},
	}}

	env.RetargetNamespace(obj)

	if got := obj.GetNamespace(); got != "custom-ns" {
		t.Fatalf("namespace = %q, want custom-ns", got)
	}

	subjects, _, _ := unstructured.NestedSlice(obj.Object, "subjects")
	if subjects[0].(map[string]any)["namespace"] != "custom-ns" {
		t.Fatalf("subject namespace not rewritten: %v", subjects[0])
	}

	// Default install is a no-op.
	def := &Env{Namespace: BuildDefaultNamespace}
	nsObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": "unbounded-system"},
	}}
	def.RetargetNamespace(nsObj)

	if nsObj.GetName() != "unbounded-system" {
		t.Fatalf("default install rewrote namespace to %q", nsObj.GetName())
	}
}

func TestRetargetNamespaceRewritesServiceAccountAndArgs(t *testing.T) {
	env := &Env{Namespace: "custom-ns"}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "admissionregistration.k8s.io/v1",
		"kind":       "ValidatingAdmissionPolicy",
		"metadata":   map[string]any{"name": "p"},
		"spec": map[string]any{
			"matchConditions": []any{
				map[string]any{"expression": "request.userInfo.username == 'system:serviceaccount:unbounded-system:unbounded-net-controller'"},
			},
			"args":  []any{"--leader-elect-resource-namespace=unbounded-system"},
			"image": "ghcr.io/azure/unbounded-system:v1",
		},
	}}

	env.RetargetNamespace(obj)

	conds, _, _ := unstructured.NestedSlice(obj.Object, "spec", "matchConditions")
	gotExpr := conds[0].(map[string]any)["expression"].(string)

	if gotExpr != "request.userInfo.username == 'system:serviceaccount:custom-ns:unbounded-net-controller'" {
		t.Fatalf("SA username not rewritten: %q", gotExpr)
	}

	args, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "args")
	if len(args) != 1 || args[0] != "--leader-elect-resource-namespace=custom-ns" {
		t.Fatalf("flag value not rewritten: %v", args)
	}

	if img, _, _ := unstructured.NestedString(obj.Object, "spec", "image"); img != "ghcr.io/azure/unbounded-system:v1" {
		t.Fatalf("image ref must not be rewritten, got %q", img)
	}
}

func TestRetargetNamespaceInString(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"unbounded-system", "custom-ns"},
		{"system:serviceaccount:unbounded-system:sa", "system:serviceaccount:custom-ns:sa"},
		{"--leader-elect-resource-namespace=unbounded-system", "--leader-elect-resource-namespace=custom-ns"},
		{"ghcr.io/azure/unbounded-system:v1", "ghcr.io/azure/unbounded-system:v1"},
		{"unbounded-system-other", "unbounded-system-other"},
	}

	for _, tc := range cases {
		if got := retargetNamespaceInString(tc.in, "unbounded-system", "custom-ns"); got != tc.want {
			t.Fatalf("retargetNamespaceInString(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestApplyObject(t *testing.T) {
	env := &Env{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}}},
		},
	}

	if err := env.ApplyObject(t.Context(), deployment); err != nil {
		t.Fatalf("ApplyObject returned error: %v", err)
	}
}

func TestManagedConfigPredicate(t *testing.T) {
	env := &Env{Namespace: "target"}
	predicate := env.ManagedConfigPredicate(env.InNamespaceNamed("machina-config", "unbounded-net-config"))

	for _, name := range []string{"machina-config", "unbounded-net-config"} {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: name}}
		if !predicate.Create(event.CreateEvent{Object: cm}) {
			t.Fatalf("%s create was filtered", name)
		}
	}

	if predicate.Create(event.CreateEvent{Object: &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "machina-config"}}}) {
		t.Fatal("config in a different namespace was accepted")
	}

	oldConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: "machina-config"},
		Data:       map[string]string{"config.yaml": "same"},
		BinaryData: map[string][]byte{"x": {1}},
	}
	metadataOnly := oldConfig.DeepCopy()
	metadataOnly.Labels = map[string]string{"changed": "true"}

	if predicate.Update(event.UpdateEvent{ObjectOld: oldConfig, ObjectNew: metadataOnly}) {
		t.Fatal("metadata-only update should not enqueue")
	}

	payloadChange := oldConfig.DeepCopy()
	payloadChange.BinaryData["x"] = []byte{2}

	if !predicate.Update(event.UpdateEvent{ObjectOld: oldConfig, ObjectNew: payloadChange}) {
		t.Fatal("payload update should enqueue")
	}
}

func TestManagedWorkloadPredicate(t *testing.T) {
	env := &Env{Namespace: "target"}
	predicate := env.ManagedWorkloadPredicate(env.InNamespaceNamed("machina-controller", "unbounded-net-controller"))

	workload := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: "machina-controller"}}

	if !predicate.Create(event.CreateEvent{Object: workload}) {
		t.Fatal("startup create was filtered")
	}

	if !predicate.Delete(event.DeleteEvent{Object: workload}) {
		t.Fatal("delete was filtered")
	}

	updated := workload.DeepCopy()
	updated.SetGeneration(workload.GetGeneration() + 1)

	if !predicate.Update(event.UpdateEvent{ObjectOld: workload, ObjectNew: updated}) {
		t.Fatal("generation change was filtered")
	}

	if predicate.Update(event.UpdateEvent{ObjectOld: updated, ObjectNew: updated.DeepCopy()}) {
		t.Fatal("same-generation update was accepted")
	}

	unmanaged := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: "metalman-controller-edge"}}
	if predicate.Create(event.CreateEvent{Object: unmanaged}) {
		t.Fatal("unmanaged workload create was accepted")
	}
}

func TestSingletonRequestBuilders(t *testing.T) {
	env := &Env{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}},
		&unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}},
	).Build()}

	if got := env.singletonRequest(); len(got) != 1 || got[0].Name != SingletonRequestName {
		t.Fatalf("singletonRequest = %#v", got)
	}

	requests := env.singletonAndAllSiteRequests(t.Context())

	names := map[string]bool{}
	for _, request := range requests {
		names[request.Name] = true
	}

	if len(requests) != 3 || !names[SingletonRequestName] || !names["edge"] || !names["cluster"] {
		t.Fatalf("singletonAndAllSiteRequests = %#v, want singleton, edge, cluster", requests)
	}
}

func TestSingletonAndAllSiteRequestsPreservesSingletonOnListError(t *testing.T) {
	listErr := errors.New("Site list failed")
	env := &Env{Client: fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return listErr
			},
		}).
		Build()}

	requests := env.singletonAndAllSiteRequests(t.Context())
	if len(requests) != 1 || requests[0].Name != SingletonRequestName {
		t.Fatalf("singletonAndAllSiteRequests = %#v, want singleton after list failure", requests)
	}
}

func TestApplyManifestDataSkipsNilledObjects(t *testing.T) {
	env := &Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Namespace: "unbounded-system",
	}

	applied := 0
	data := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: keep\n  namespace: unbounded-system\n")

	err := env.applyManifestData(t.Context(), data, func(obj *unstructured.Unstructured) error {
		applied++
		obj.Object = nil // skip

		return nil
	})
	if err != nil {
		t.Fatalf("applyManifestData: %v", err)
	}

	if applied != 1 {
		t.Fatalf("mutate called %d times, want 1", applied)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: "unbounded-system", Name: "keep"}, &corev1.ConfigMap{}); err == nil {
		t.Fatal("nilled object was applied")
	}
}

func TestToUnstructuredRoundTrip(t *testing.T) {
	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "y"},
		Data:       map[string]string{"a": "b"},
	}

	u := ToUnstructured(cm)
	if u.GetName() != "x" || u.GetNamespace() != "y" {
		t.Fatalf("unstructured = %#v", u.Object)
	}

	// An already-unstructured object is returned as-is.
	if got := ToUnstructured(u); got != u {
		t.Fatal("unstructured input was copied")
	}
}
