// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package component

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/unbounded"
)

func TestConfigImage(t *testing.T) {
	cases := []struct {
		name     string
		registry string
		want     string
	}{
		{name: "bare host", registry: "ghcr.io", want: "ghcr.io/machina:v1.2.3"},
		{name: "host and org", registry: "ghcr.io/azure", want: "ghcr.io/azure/machina:v1.2.3"},
		{name: "fork org", registry: "ghcr.io/myorg", want: "ghcr.io/myorg/machina:v1.2.3"},
		{name: "host and multi-segment path", registry: "registry.corp.internal/unbounded/mirror", want: "registry.corp.internal/unbounded/mirror/machina:v1.2.3"},
		{name: "trailing slash", registry: "registry.example.com/mirror/", want: "registry.example.com/mirror/machina:v1.2.3"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{ImageRegistry: tc.registry, ImageTag: "v1.2.3"}
			if got := cfg.Image("machina"); got != tc.want {
				t.Fatalf("Image = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetPodSpecImages(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"initContainers": []any{map[string]any{"name": "install", "image": "old:init"}},
			"containers":     []any{map[string]any{"name": "run", "image": "old:run"}},
		}}},
	}}

	if err := SetPodSpecImages(obj, "registry.example.com/azure/component:v1"); err != nil {
		t.Fatalf("SetPodSpecImages: %v", err)
	}

	for _, field := range []string{"initContainers", "containers"} {
		containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
		if err != nil {
			t.Fatalf("get %s: %v", field, err)
		}

		if got := containers[0].(map[string]any)["image"]; got != "registry.example.com/azure/component:v1" {
			t.Fatalf("%s image = %q", field, got)
		}
	}
}

func TestSetNamedContainerImage(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"initContainers": []any{map[string]any{"name": "install", "image": "keep:init"}},
			"containers": []any{
				map[string]any{"name": "run", "image": "old:run"},
				map[string]any{"name": "sidecar", "image": "keep:sidecar"},
			},
		}}},
	}}

	if err := SetNamedContainerImage(obj, "run", "registry.example.com/azure/component:v1"); err != nil {
		t.Fatalf("SetNamedContainerImage: %v", err)
	}

	initContainers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "initContainers")
	containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")

	if got := initContainers[0].(map[string]any)["image"]; got != "keep:init" {
		t.Fatalf("init container image = %q", got)
	}

	if got := containers[0].(map[string]any)["image"]; got != "registry.example.com/azure/component:v1" {
		t.Fatalf("named container image = %q", got)
	}

	if got := containers[1].(map[string]any)["image"]; got != "keep:sidecar" {
		t.Fatalf("sidecar image = %q", got)
	}

	if err := SetNamedContainerImage(obj, "missing", "unused"); err == nil || err.Error() != `container "missing" not found` {
		t.Fatalf("missing container error = %v", err)
	}
}

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

// TestSingletonRequestIsTheOnlyFanOut pins the shape of the cluster-component
// watches.
//
// A ConfigMap change used to enqueue the singleton request plus one request per
// Site. The singleton pass already reconciles every Site, so those were N
// redundant full passes for a single edit, each re-planning and re-applying
// every component for a Site the singleton pass had just covered.
//
// The helper that produced them also listed Sites at event-delivery time and,
// when the List failed, logged and returned only the singleton, silently
// dropping the fan-out with no retry. Keeping the fan-out inside Reconcile
// means a failure is returned and retried with backoff instead.
func TestSingletonRequestIsTheOnlyFanOut(t *testing.T) {
	env := &Env{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(
		&unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}},
		&unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}},
	).Build()}

	queue := workqueue.NewTypedRateLimitingQueue(
		workqueue.DefaultTypedControllerRateLimiter[reconcile.Request](),
	)
	defer queue.ShutDown()

	changed := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "unbounded-system", Name: "net-config"}}

	env.RequestSingleton().Create(t.Context(), event.CreateEvent{Object: changed}, queue)

	// Two Sites exist, and neither may produce a request of its own.
	if got := queue.Len(); got != 1 {
		t.Fatalf("one ConfigMap change enqueued %d requests, want only the singleton", got)
	}

	request, _ := queue.Get()
	if request.Name != SingletonRequestName {
		t.Fatalf("request = %q, want %q", request.Name, SingletonRequestName)
	}
}

func TestDecodeManifestDataSkipsNilledObjects(t *testing.T) {
	env := &Env{
		Client:    fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		Namespace: "unbounded-system",
	}

	mutated := 0
	data := []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: keep\n  namespace: unbounded-system\n")

	objects, err := env.decodeManifestData(data, func(obj *unstructured.Unstructured) error {
		mutated++
		obj.Object = nil // skip

		return nil
	})
	if err != nil {
		t.Fatalf("decodeManifestData: %v", err)
	}

	if mutated != 1 {
		t.Fatalf("mutate called %d times, want 1", mutated)
	}

	if len(objects) != 0 {
		t.Fatalf("decoded %d objects, want 0; a nilled object must be dropped", len(objects))
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

func TestSiteOwnerReferenceIsController(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}
	ref := SiteOwnerReference(site)

	// Owns() enqueues via metav1.GetControllerOf, so the reference must be a
	// controller reference or per-site self-heal watches never fire.
	if ref.Controller == nil || !*ref.Controller {
		t.Fatalf("SiteOwnerReference is not a controller reference: %#v", ref)
	}

	// BlockOwnerDeletion must stay unset: setting it requires update on
	// sites/finalizers, which the operator ServiceAccount does not hold.
	if ref.BlockOwnerDeletion != nil {
		t.Fatalf("BlockOwnerDeletion should be unset, got %v", *ref.BlockOwnerDeletion)
	}

	obj := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:            "metalman-controller-rack-a",
		OwnerReferences: []metav1.OwnerReference{ref},
	}}
	if controller := metav1.GetControllerOf(obj); controller == nil || controller.Name != "rack-a" {
		t.Fatalf("GetControllerOf did not return the Site owner: %#v", controller)
	}
}

func TestUpsertOwnerReferenceConvergesControllerFlag(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}
	owner := SiteOwnerReference(site)

	// A reference adopted before controller ownership existed (Controller unset).
	legacy := owner
	legacy.Controller = nil

	refs, changed := UpsertOwnerReference([]metav1.OwnerReference{legacy}, owner)
	if !changed {
		t.Fatal("upsert did not converge a non-controller reference to a controller reference")
	}

	if len(refs) != 1 || refs[0].Controller == nil || !*refs[0].Controller {
		t.Fatalf("owner reference not upgraded to controller: %#v", refs)
	}

	// Idempotent once converged.
	if _, again := UpsertOwnerReference(refs, owner); again {
		t.Fatal("upsert reported a change for an already-controller reference")
	}

	// A different owner is appended, not replaced.
	other := SiteOwnerReference(&unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-b", UID: "other-uid"}})

	appended, changed := UpsertOwnerReference(refs, other)
	if !changed || len(appended) != 2 {
		t.Fatalf("upsert of a distinct owner = (%v, %#v)", changed, appended)
	}
}

func TestOwnedWorkloadPredicate(t *testing.T) {
	env := &Env{Namespace: "target"}
	pred := env.OwnedWorkloadPredicate()

	workload := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: "metalman-controller-rack-a"}}

	if !pred.Create(event.CreateEvent{Object: workload}) {
		t.Fatal("owned create was filtered")
	}

	if !pred.Delete(event.DeleteEvent{Object: workload}) {
		t.Fatal("owned delete was filtered (self-heal on deletion would not fire)")
	}

	bumped := workload.DeepCopy()
	bumped.SetGeneration(workload.GetGeneration() + 1)

	if !pred.Update(event.UpdateEvent{ObjectOld: workload, ObjectNew: bumped}) {
		t.Fatal("spec drift (generation change) was filtered")
	}

	// A status-only update (same generation, e.g. replica counts) must be
	// dropped so pod churn does not re-apply the workload.
	if pred.Update(event.UpdateEvent{ObjectOld: bumped, ObjectNew: bumped.DeepCopy()}) {
		t.Fatal("status-only update was accepted; Owns would re-apply on pod churn")
	}

	if pred.Generic(event.GenericEvent{Object: workload}) {
		t.Fatal("generic event was accepted")
	}
}

func TestSiteOwnedResourceEnqueuesViaOwnsHandler(t *testing.T) {
	scheme := testScheme(t)

	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{unboundedv1alpha3.GroupVersion})
	mapper.Add(unboundedv1alpha3.GroupVersion.WithKind("Site"), meta.RESTScopeRoot)

	// Mirror controller-runtime's builder.Owns() wiring exactly: enqueue the
	// owner only for the controller owner reference.
	h := handler.EnqueueRequestForOwner(scheme, mapper, &unboundedv1alpha3.Site{}, handler.OnlyControllerOwner())

	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}
	owned := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace:       "unbounded-system",
		Name:            "metalman-controller-rack-a",
		OwnerReferences: []metav1.OwnerReference{SiteOwnerReference(site)},
	}}

	newQueue := func() workqueue.TypedRateLimitingInterface[reconcile.Request] {
		return workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	}

	created := newQueue()
	h.Create(t.Context(), event.CreateEvent{Object: owned}, created)

	if created.Len() != 1 {
		t.Fatalf("controller-owned resource did not enqueue its Site: queue len = %d", created.Len())
	}

	req, _ := created.Get()
	if req.Name != "rack-a" || req.Namespace != "" {
		t.Fatalf("enqueued request = %#v, want cluster-scoped Site rack-a", req)
	}

	// Deletion must also enqueue so the resource is recreated.
	deleted := newQueue()
	h.Delete(t.Context(), event.DeleteEvent{Object: owned}, deleted)

	if deleted.Len() != 1 {
		t.Fatal("deletion of a controller-owned resource did not enqueue its Site")
	}

	// A non-controller owner reference (the pre-fix regression) enqueues nothing,
	// so Owns() would never self-heal.
	legacy := SiteOwnerReference(site)
	legacy.Controller = nil
	nonController := owned.DeepCopy()
	nonController.OwnerReferences = []metav1.OwnerReference{legacy}

	ignored := newQueue()
	h.Create(t.Context(), event.CreateEvent{Object: nonController}, ignored)

	if ignored.Len() != 0 {
		t.Fatal("non-controller owner reference unexpectedly enqueued; guard for the self-heal regression")
	}
}

func TestIsRBACObject(t *testing.T) {
	rbac := func(kind, name string) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetKind(kind)
		obj.SetName(name)

		return obj
	}

	cases := []struct {
		obj  *unstructured.Unstructured
		want bool
	}{
		{rbac("ServiceAccount", "metalman-controller"), true},
		{rbac("ClusterRole", "unbounded-metalman"), true},
		{rbac("RoleBinding", "metalman-lease"), true},
		{rbac("ServiceAccount", "machina-controller"), false},
		{rbac("Deployment", "metalman-controller"), false},
		{rbac("ConfigMap", "metalman-config"), false},
	}

	for _, tc := range cases {
		if got := IsRBACObject(tc.obj, "metalman"); got != tc.want {
			t.Fatalf("IsRBACObject(%s/%s) = %v, want %v", tc.obj.GetKind(), tc.obj.GetName(), got, tc.want)
		}
	}
}

// TestWorkloadPredicateFiresOnOperatorMetadataDrift is a regression test.
//
// Kubernetes increments generation for spec changes only, so removing the
// override hash annotation, or editing a label the operator set, produced no
// event and therefore no pass. Server-side apply would have repaired it, but
// nothing was going to ask the operator to look, so a Site could report Applied
// indefinitely for an object carrying no sign of an override.
func TestWorkloadPredicateFiresOnOperatorMetadataDrift(t *testing.T) {
	env := &Env{Namespace: DefaultNamespace}
	predicate := env.ManagedWorkloadPredicate(func(client.Object) bool { return true })

	withAnnotations := func(generation int64, annotations map[string]string) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
			Namespace:   DefaultNamespace,
			Name:        "agent",
			Generation:  generation,
			Annotations: annotations,
		}}
	}

	hash := map[string]string{unbounded.ReservedPrefix + "override-hash": "h1"}

	cases := map[string]struct {
		old, updated *appsv1.DaemonSet
		want         bool
	}{
		"override hash removed": {
			old:     withAnnotations(1, hash),
			updated: withAnnotations(1, nil),
			want:    true,
		},
		"override hash changed": {
			old:     withAnnotations(1, hash),
			updated: withAnnotations(1, map[string]string{unbounded.ReservedPrefix + "override-hash": "h2"}),
			want:    true,
		},
		"override hash added": {
			old:     withAnnotations(1, nil),
			updated: withAnnotations(1, hash),
			want:    true,
		},
		"spec changed": {
			old:     withAnnotations(1, hash),
			updated: withAnnotations(2, hash),
			want:    true,
		},
		// The Deployment controller writes this on every rollout, and kubectl
		// writes its own. Firing on them would be the churn the predicate
		// exists to suppress.
		"unrelated annotation churn": {
			old: withAnnotations(1, hash),
			updated: withAnnotations(1, map[string]string{
				unbounded.ReservedPrefix + "override-hash": "h1",
				"deployment.kubernetes.io/revision":        "7",
			}),
			want: false,
		},
		"nothing changed": {
			old:     withAnnotations(1, hash),
			updated: withAnnotations(1, hash),
			want:    false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := predicate.Update(event.UpdateEvent{ObjectOld: tc.old, ObjectNew: tc.updated})
			if got != tc.want {
				t.Fatalf("predicate fired = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestOwnedWorkloadPredicateSharesTheSameRule confirms the per-Site components,
// which use Owns() rather than a named watch, get the same drift detection.
func TestOwnedWorkloadPredicateSharesTheSameRule(t *testing.T) {
	env := &Env{Namespace: DefaultNamespace}
	predicate := env.OwnedWorkloadPredicate()

	older := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
		Generation:  1,
		Annotations: map[string]string{unbounded.ReservedPrefix + "override-hash": "h1"},
	}}
	newer := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Generation: 1}}

	if !predicate.Update(event.UpdateEvent{ObjectOld: older, ObjectNew: newer}) {
		t.Fatal("an owned workload losing its override hash must trigger a pass")
	}
}
