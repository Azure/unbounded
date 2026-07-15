// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestReconcilePatchesAllConditionsAndReturnsComponentErrors(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a", Generation: 7},
	}
	netErr := errors.New("net reconcile failed")
	machinaErr := errors.New("machina reconcile failed")
	listErrs := []error{netErr, machinaErr}
	listCalls := 0

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(site).
		WithStatusSubresource(site).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*unboundedv1alpha3.SiteList); !ok {
					t.Fatalf("unexpected list type %T", list)
				}

				err := listErrs[listCalls]
				listCalls++

				return err
			},
		}).
		Build()

	r := &SiteReconciler{Client: cl, Scheme: scheme}

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(site)})
	if !errors.Is(err, netErr) || !errors.Is(err, machinaErr) {
		t.Fatalf("Reconcile error = %v, want joined net and machina errors", err)
	}

	var got unboundedv1alpha3.Site
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(site), &got); err != nil {
		t.Fatalf("get reconciled Site: %v", err)
	}

	want := map[string]struct {
		status metav1.ConditionStatus
		reason string
	}{
		"NetReady":      {status: metav1.ConditionFalse, reason: reasonReconcileError},
		"MachinaReady":  {status: metav1.ConditionFalse, reason: reasonReconcileError},
		"MetalmanReady": {status: metav1.ConditionTrue, reason: reasonDisabled},
		"StorageReady":  {status: metav1.ConditionTrue, reason: reasonDisabled},
	}

	if len(got.Status.Conditions) != len(want) {
		t.Fatalf("conditions len = %d, want %d: %#v", len(got.Status.Conditions), len(want), got.Status.Conditions)
	}

	for conditionType, expected := range want {
		condition := apimeta.FindStatusCondition(got.Status.Conditions, conditionType)
		if condition == nil {
			t.Fatalf("condition %q not found", conditionType)
		}

		if condition.Status != expected.status || condition.Reason != expected.reason || condition.ObservedGeneration != site.Generation {
			t.Fatalf("condition %q = %#v, want status=%s reason=%s generation=%d", conditionType, condition, expected.status, expected.reason, site.Generation)
		}
	}
}

func TestReconcileJoinsStatusPatchError(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}
	componentErr := errors.New("component reconcile failed")
	patchErr := errors.New("status patch failed")

	var patched *unboundedv1alpha3.Site

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(site).
		WithStatusSubresource(site).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, _ client.ObjectList, _ ...client.ListOption) error {
				return componentErr
			},
			SubResourcePatch: func(
				_ context.Context,
				_ client.Client,
				subResourceName string,
				obj client.Object,
				_ client.Patch,
				_ ...client.SubResourcePatchOption,
			) error {
				if subResourceName != "status" {
					t.Fatalf("subresource = %q, want status", subResourceName)
				}

				patched = obj.(*unboundedv1alpha3.Site).DeepCopy()

				return patchErr
			},
		}).
		Build()

	r := &SiteReconciler{Client: cl, Scheme: scheme}

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(site)})
	if !errors.Is(err, componentErr) || !errors.Is(err, patchErr) {
		t.Fatalf("Reconcile error = %v, want joined component and status patch errors", err)
	}

	if patched == nil || len(patched.Status.Conditions) != 4 {
		t.Fatalf("status patch received conditions %#v, want all four", patched)
	}
}

func newReconcilerTestScheme(t *testing.T) *runtime.Scheme {
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

func TestMutateStorageScopesDaemonSetToSite(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": "unbounded-storage-supervisor"},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "run"}}},
			},
		},
	}}

	if err := mutateStorageObject(site, "storage-hash", obj); err != nil {
		t.Fatalf("mutateStorageObject returned error: %v", err)
	}

	if got := obj.GetName(); got != "unbounded-storage-supervisor-rack-a" {
		t.Fatalf("name = %q, want unbounded-storage-supervisor-rack-a", got)
	}

	if got := obj.GetLabels()[siteLabelKey]; got != "rack-a" {
		t.Fatalf("metadata site label = %q, want rack-a", got)
	}

	for _, path := range [][]string{
		{"spec", "selector", "matchLabels", siteLabelKey},
		{"spec", "template", "metadata", "labels", siteLabelKey},
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
	if annotations[storageConfigHashAnnotation] != "storage-hash" {
		t.Fatalf("storage config hash annotation = %q, want storage-hash", annotations[storageConfigHashAnnotation])
	}

	affinity, ok, err := unstructured.NestedMap(obj.Object, "spec", "template", "spec", "affinity")
	if err != nil || !ok {
		t.Fatalf("missing storage affinity: ok=%t err=%v", ok, err)
	}

	assertSiteAffinityMap(t, affinity, "rack-a")
}

func TestMutateMachinaSkipsMetalmanSupport(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata":   map[string]any{"name": "metalman-controller"},
	}}

	r := &SiteReconciler{}
	if err := r.mutateMachinaObject(obj); err != nil {
		t.Fatalf("mutateMachinaObject returned error: %v", err)
	}

	if obj.Object != nil {
		t.Fatalf("metalman support object was not skipped")
	}
}

func TestSetMachinaAPIServerEndpointPreservesConfig(t *testing.T) {
	input := `# retained comment
apiServerEndpoint: "https://old.example:6443"
maxConcurrentReconciles: 17
customField: keep-me
`
	endpoint := "https://api.example:6443/path?mode=\"strict\"&ready=true"

	got, err := setMachinaAPIServerEndpoint(input, endpoint)
	if err != nil {
		t.Fatalf("setMachinaAPIServerEndpoint: %v", err)
	}

	var config map[string]any
	if err := yaml.Unmarshal([]byte(got), &config); err != nil {
		t.Fatalf("unmarshal merged config: %v", err)
	}

	if config["apiServerEndpoint"] != endpoint {
		t.Fatalf("apiServerEndpoint = %q, want %q", config["apiServerEndpoint"], endpoint)
	}

	if config["maxConcurrentReconciles"] != 17 || config["customField"] != "keep-me" {
		t.Fatalf("custom config was not preserved: %#v", config)
	}

	if !strings.Contains(got, "# retained comment") {
		t.Fatalf("config comment was not preserved: %q", got)
	}
}

func TestEnsureMachinaConfigUpdatesEndpointAndPreservesData(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "machina-config", Namespace: DefaultNamespace},
		Data: map[string]string{
			"config.yaml": "apiServerEndpoint: https://old.example:6443\ncustom: true\n",
		},
	}
	r := &SiteReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build(),
		Config: Config{APIServerEndpoint: "https://new.example:6443"},
	}

	hash, err := r.ensureMachinaConfig(t.Context())
	if err != nil {
		t.Fatalf("ensureMachinaConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get machina config: %v", err)
	}

	var config map[string]any
	if err := yaml.Unmarshal([]byte(got.Data["config.yaml"]), &config); err != nil {
		t.Fatalf("unmarshal updated config: %v", err)
	}

	if config["apiServerEndpoint"] != "https://new.example:6443" || config["custom"] != true {
		t.Fatalf("updated config = %#v", config)
	}

	if hash != configMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want hash of exact stored config", hash)
	}
}

func TestEnsureMachinaConfigOptimisticConflictReloadsFreshState(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "machina-config", Namespace: DefaultNamespace, ResourceVersion: "1"},
		Data:       map[string]string{"config.yaml": "apiServerEndpoint: https://old.example:6443\n"},
	}

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	conflicted := false
	cl := interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, underlying client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			data, err := patch.Data(obj)
			if err != nil {
				t.Fatalf("encode patch: %v", err)
			}

			if !bytes.Contains(data, []byte(`"resourceVersion":`)) {
				t.Fatalf("optimistic patch does not include resourceVersion: %s", data)
			}

			if !conflicted {
				conflicted = true

				var latest corev1.ConfigMap
				if err := underlying.Get(ctx, client.ObjectKeyFromObject(existing), &latest); err != nil {
					t.Fatalf("get concurrent config: %v", err)
				}

				latest.Data["concurrent"] = "preserved"
				if err := underlying.Update(ctx, &latest); err != nil {
					t.Fatalf("write concurrent config: %v", err)
				}

				return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, obj.GetName(), errors.New("concurrent edit"))
			}

			return underlying.Patch(ctx, obj, patch, opts...)
		},
	})
	r := &SiteReconciler{Client: cl, Config: Config{APIServerEndpoint: "https://new.example:6443"}}

	if _, err := r.ensureMachinaConfig(t.Context()); !apierrors.IsConflict(err) {
		t.Fatalf("first ensureMachinaConfig error = %v, want conflict", err)
	}

	if _, err := r.ensureMachinaConfig(t.Context()); err != nil {
		t.Fatalf("retry ensureMachinaConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get updated config: %v", err)
	}

	if got.Data["concurrent"] != "preserved" || !strings.Contains(got.Data["config.yaml"], "https://new.example:6443") {
		t.Fatalf("retry did not preserve fresh config: %#v", got.Data)
	}
}

func TestEnsureNetConfigCreatesDefaultOnlyWhenAbsent(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	r := &SiteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	hash, err := r.ensureNetConfig(t.Context())
	if err != nil {
		t.Fatalf("ensureNetConfig: %v", err)
	}

	var got corev1.ConfigMap

	key := client.ObjectKey{Namespace: DefaultNamespace, Name: "unbounded-net-config"}
	if err := r.Get(t.Context(), key, &got); err != nil {
		t.Fatalf("get default net config: %v", err)
	}

	if got.Data["config.yaml"] == "" || hash != configMapPayloadHash(&got) {
		t.Fatalf("default net config/hash missing: hash=%q data=%#v", hash, got.Data)
	}
}

func TestEnsureNetConfigPreservesExistingPayload(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unbounded-net-config", Namespace: DefaultNamespace},
		Data:       map[string]string{"config.yaml": "custom: true", "extra": "keep"},
		BinaryData: map[string][]byte{"routing.bin": {0, 1, 2}},
	}
	r := &SiteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()}

	hash, err := r.ensureNetConfig(t.Context())
	if err != nil {
		t.Fatalf("ensureNetConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get preserved net config: %v", err)
	}

	if got.Data["config.yaml"] != "custom: true" || got.Data["extra"] != "keep" || !bytes.Equal(got.BinaryData["routing.bin"], []byte{0, 1, 2}) {
		t.Fatalf("existing net config changed: data=%#v binaryData=%#v", got.Data, got.BinaryData)
	}

	if hash != configMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact payload hash", hash)
	}
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

	if configMapPayloadHash(first) != configMapPayloadHash(orderedDifferently) {
		t.Fatal("payload hash depends on map iteration order")
	}

	changedData := first.DeepCopy()
	changedData.Data["a"] = "changed"
	changedBinary := first.DeepCopy()
	changedBinary.BinaryData["x"] = []byte{2, 1}

	if configMapPayloadHash(first) == configMapPayloadHash(changedData) ||
		configMapPayloadHash(first) == configMapPayloadHash(changedBinary) {
		t.Fatal("payload hash did not include all Data and BinaryData")
	}
}

func TestManagedConfigPayloadChangesAndNames(t *testing.T) {
	r := &SiteReconciler{Namespace: "target"}
	for _, name := range []string{"machina-config", "unbounded-net-config", "unbounded-storage-config-edge"} {
		if !r.isManagedConfig(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: name}}) {
			t.Fatalf("%s is not watched", name)
		}
	}

	oldConfig := &corev1.ConfigMap{Data: map[string]string{"config.yaml": "same"}, BinaryData: map[string][]byte{"x": {1}}}
	metadataOnly := oldConfig.DeepCopy()

	metadataOnly.Labels = map[string]string{"changed": "true"}
	if configMapPayloadChanged(oldConfig, metadataOnly) {
		t.Fatal("metadata-only update should not enqueue Sites")
	}

	binaryChange := oldConfig.DeepCopy()

	binaryChange.BinaryData["x"] = []byte{2}
	if !configMapPayloadChanged(oldConfig, binaryChange) {
		t.Fatal("BinaryData update should enqueue Sites")
	}
}

func TestRequestsForManagedConfigEnqueuesAllSites(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	r := &SiteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge"}},
		&unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "cluster"}},
	).Build()}

	requests := r.requestsForManagedConfig(t.Context(), &corev1.ConfigMap{})

	got := map[string]bool{}
	for _, request := range requests {
		got[request.Name] = true
	}

	if len(requests) != 3 || !got[singletonRequestName] || !got["edge"] || !got["cluster"] {
		t.Fatalf("requestsForManagedConfig = %#v, want singleton, edge, and cluster", requests)
	}
}

func TestRequestsForManagedConfigEnqueuesSingletonWithNoSites(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	r := &SiteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}

	requests := r.requestsForManagedConfig(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: "unbounded-net-config"},
	})

	if len(requests) != 1 || requests[0].Name != singletonRequestName {
		t.Fatalf("requestsForManagedConfig = %#v, want singleton request", requests)
	}
}

func TestRequestsForManagedConfigEnqueuesStorageSiteDirectly(t *testing.T) {
	r := &SiteReconciler{}
	requests := r.requestsForManagedConfig(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config-edge"},
	})

	if len(requests) != 1 || requests[0].Name != "edge" {
		t.Fatalf("requestsForManagedConfig = %#v, want only edge", requests)
	}
}

func TestRequestsForManagedConfigPreservesSingletonWhenSiteListFails(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	listErr := errors.New("Site list failed")
	r := &SiteReconciler{Client: fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return listErr
			},
		}).
		Build()}

	requests := r.requestsForManagedConfig(t.Context(), &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: "machina-config"},
	})

	if len(requests) != 1 || requests[0].Name != singletonRequestName {
		t.Fatalf("requestsForManagedConfig = %#v, want singleton request after list failure", requests)
	}
}

func TestManagedSingletonWorkloadStartupEventsEnqueueSingleton(t *testing.T) {
	r := &SiteReconciler{Namespace: "target"}
	predicates := r.managedSingletonWorkloadPredicates()

	for _, workload := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: "machina-controller"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: "unbounded-net-controller"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: "unbounded-net-node"}},
	} {
		if !predicates.Create(event.CreateEvent{Object: workload}) {
			t.Fatalf("startup Create event for %T %q was filtered", workload, workload.GetName())
		}

		requests := r.requestsForManagedSingletonWorkload(t.Context(), workload)
		if len(requests) != 1 || requests[0].Name != singletonRequestName {
			t.Fatalf("startup requests for %s = %#v, want singleton", workload.GetName(), requests)
		}

		if !predicates.Delete(event.DeleteEvent{Object: workload}) {
			t.Fatalf("Delete event for %T %q was filtered", workload, workload.GetName())
		}

		updated := workload.DeepCopyObject().(client.Object)
		updated.SetGeneration(workload.GetGeneration() + 1)

		if !predicates.Update(event.UpdateEvent{ObjectOld: workload, ObjectNew: updated}) {
			t.Fatalf("generation update for %T %q was filtered", workload, workload.GetName())
		}

		if predicates.Update(event.UpdateEvent{ObjectOld: updated, ObjectNew: updated.DeepCopyObject().(client.Object)}) {
			t.Fatalf("same-generation update for %T %q was accepted", workload, workload.GetName())
		}
	}

	unmanaged := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "target", Name: "metalman-controller-edge"}}
	if predicates.Create(event.CreateEvent{Object: unmanaged}) {
		t.Fatal("unmanaged Deployment Create event was accepted")
	}
}

func TestNetApplyMutatorStampsBothWorkloads(t *testing.T) {
	for _, tc := range []struct {
		kind string
		name string
	}{
		{kind: "Deployment", name: "unbounded-net-controller"},
		{kind: "DaemonSet", name: "unbounded-net-node"},
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

			if err := (&SiteReconciler{}).netApplyMutator("net-hash")(obj); err != nil {
				t.Fatalf("netApplyMutator: %v", err)
			}

			annotations, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
			if annotations[netConfigHashAnnotation] != "net-hash" || annotations["existing"] != "kept" {
				t.Fatalf("pod template annotations = %#v", annotations)
			}
		})
	}

	config := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "unbounded-net-config"},
	}}
	if err := (&SiteReconciler{}).netApplyMutator("net-hash")(config); err != nil || config.Object != nil {
		t.Fatalf("embedded net ConfigMap was not skipped: err=%v object=%#v", err, config.Object)
	}
}

func TestReconcileRetainedNetWithNoSites(t *testing.T) {
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: "unbounded-net-config"},
		Data:       map[string]string{"config.yaml": "custom: retained"},
	}
	r, appliedHashes := retainedNetReconciler(t, config)

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: singletonRequestName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	wantHash := configMapPayloadHash(config)
	for _, name := range []string{"unbounded-net-controller", "unbounded-net-node"} {
		if appliedHashes[name] != wantHash {
			t.Fatalf("%s applied hash = %q, want %q", name, appliedHashes[name], wantHash)
		}
	}

	var got corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(config), &got); err != nil {
		t.Fatalf("get retained net config: %v", err)
	}

	if got.Data["config.yaml"] != "custom: retained" {
		t.Fatalf("retained net config changed: %#v", got.Data)
	}
}

func TestReconcileRecreatesDeletedRetainedNetConfigWithNoSites(t *testing.T) {
	retained := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: DefaultNamespace,
		Name:      "unbounded-net-controller",
	}}
	r, appliedHashes := retainedNetReconciler(t, retained)

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: singletonRequestName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var config corev1.ConfigMap

	key := client.ObjectKey{Namespace: DefaultNamespace, Name: "unbounded-net-config"}
	if err := r.Get(t.Context(), key, &config); err != nil {
		t.Fatalf("get recreated net config: %v", err)
	}

	wantHash := configMapPayloadHash(&config)
	if config.Data["config.yaml"] == "" || appliedHashes["unbounded-net-controller"] != wantHash ||
		appliedHashes["unbounded-net-node"] != wantHash {
		t.Fatalf("recreated config/workload hashes = data=%#v hashes=%#v", config.Data, appliedHashes)
	}
}

func TestReconcileDoesNotCreateNetFromNothingWithNoSites(t *testing.T) {
	r, appliedHashes := retainedNetReconciler(t)

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: singletonRequestName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	err := r.Get(t.Context(), client.ObjectKey{Namespace: DefaultNamespace, Name: "unbounded-net-config"}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) || len(appliedHashes) != 0 {
		t.Fatalf("zero-Site reconcile created net from nothing: config err=%v hashes=%#v", err, appliedHashes)
	}
}

func retainedNetReconciler(t *testing.T, objects ...client.Object) (*SiteReconciler, map[string]string) {
	t.Helper()

	scheme := newReconcilerTestScheme(t)
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
				if (applied.GetKind() != "Deployment" || name != "unbounded-net-controller") &&
					(applied.GetKind() != "DaemonSet" || name != "unbounded-net-node") {
					return nil
				}

				hash, _, err := unstructured.NestedString(
					applied.UnstructuredContent(),
					"spec", "template", "metadata", "annotations", netConfigHashAnnotation,
				)
				if err != nil {
					t.Fatalf("get applied hash for %s: %v", name, err)
				}

				appliedHashes[name] = hash

				return nil
			},
		}).
		Build()

	return &SiteReconciler{Client: cl, Scheme: scheme}, appliedHashes
}

func TestMachinaApplyMutatorStampsConfigHash(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "machina-controller"},
		"spec": map[string]any{
			"template": map[string]any{"metadata": map[string]any{}},
		},
	}}

	const hash = "config-hash"
	if err := (&SiteReconciler{}).machinaApplyMutator(hash)(obj); err != nil {
		t.Fatalf("machinaApplyMutator: %v", err)
	}

	got, found, err := unstructured.NestedString(obj.Object,
		"spec", "template", "metadata", "annotations", machinaConfigHashAnnotation)
	if err != nil || !found || got != hash {
		t.Fatalf("config hash = %q found=%t err=%v, want %q", got, found, err, hash)
	}
}

func TestReconcileRetainedMachinaWithNoSitesUpdatesConfigAndHash(t *testing.T) {
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: "machina-config"},
		Data: map[string]string{
			"config.yaml": "apiServerEndpoint: https://old.example:6443\ncustom: retained\n",
		},
	}
	r, appliedHashes := retainedMachinaReconciler(t, config)
	r.Config.APIServerEndpoint = "https://new.example:6443"

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: singletonRequestName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(config), &got); err != nil {
		t.Fatalf("get retained machina config: %v", err)
	}

	if !strings.Contains(got.Data["config.yaml"], "https://new.example:6443") ||
		!strings.Contains(got.Data["config.yaml"], "custom: retained") {
		t.Fatalf("retained machina config was not merged: %q", got.Data["config.yaml"])
	}

	wantHash := configMapPayloadHash(&got)
	if appliedHashes["machina-controller"] != wantHash {
		t.Fatalf("applied Machina hash = %q, want %q", appliedHashes["machina-controller"], wantHash)
	}
}

func TestReconcileRecreatesDeletedRetainedMachinaConfigWithNoSites(t *testing.T) {
	retained := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Namespace: DefaultNamespace,
		Name:      "machina-controller",
	}}
	deletedConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: "machina-config"},
		Data:       map[string]string{"config.yaml": "apiServerEndpoint: https://deleted.example:6443\n"},
	}
	r, appliedHashes := retainedMachinaReconciler(t, retained, deletedConfig)

	r.Config.APIServerEndpoint = "https://api.example:6443"
	if err := r.Delete(t.Context(), deletedConfig); err != nil {
		t.Fatalf("delete machina config: %v", err)
	}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: singletonRequestName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var config corev1.ConfigMap

	key := client.ObjectKey{Namespace: DefaultNamespace, Name: "machina-config"}
	if err := r.Get(t.Context(), key, &config); err != nil {
		t.Fatalf("get recreated machina config: %v", err)
	}

	wantHash := configMapPayloadHash(&config)
	if !strings.Contains(config.Data["config.yaml"], "https://api.example:6443") ||
		appliedHashes["machina-controller"] != wantHash {
		t.Fatalf("recreated config/workload hash = data=%#v hashes=%#v", config.Data, appliedHashes)
	}
}

func TestReconcileDoesNotCreateMachinaFromNothingWithNoSites(t *testing.T) {
	r, appliedHashes := retainedMachinaReconciler(t)

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: singletonRequestName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	err := r.Get(t.Context(), client.ObjectKey{Namespace: DefaultNamespace, Name: "machina-config"}, &corev1.ConfigMap{})
	if !apierrors.IsNotFound(err) || len(appliedHashes) != 0 {
		t.Fatalf("zero-Site reconcile created Machina from nothing: config err=%v hashes=%#v", err, appliedHashes)
	}
}

func retainedMachinaReconciler(t *testing.T, objects ...client.Object) (*SiteReconciler, map[string]string) {
	t.Helper()

	scheme := newReconcilerTestScheme(t)
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

				if applied.GetKind() != "Deployment" || applied.GetName() != "machina-controller" {
					return nil
				}

				hash, _, err := unstructured.NestedString(
					applied.UnstructuredContent(),
					"spec", "template", "metadata", "annotations", machinaConfigHashAnnotation,
				)
				if err != nil {
					t.Fatalf("get applied Machina hash: %v", err)
				}

				appliedHashes[applied.GetName()] = hash

				return nil
			},
		}).
		Build()

	return &SiteReconciler{Client: cl, Scheme: scheme}, appliedHashes
}

func TestMutateMetalmanSupportObject(t *testing.T) {
	keep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "Role",
		"metadata":   map[string]any{"name": "metalman-controller"},
	}}
	if err := mutateMetalmanSupportObject(keep); err != nil {
		t.Fatalf("mutateMetalmanSupportObject returned error: %v", err)
	}

	if keep.Object == nil {
		t.Fatalf("metalman support object was dropped")
	}

	drop := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ServiceAccount",
		"metadata":   map[string]any{"name": "machina-controller"},
	}}
	if err := mutateMetalmanSupportObject(drop); err != nil {
		t.Fatalf("mutateMetalmanSupportObject returned error: %v", err)
	}

	if drop.Object != nil {
		t.Fatalf("non-metalman object was not dropped")
	}
}

func TestMetalmanDeployment(t *testing.T) {
	enabled := true
	dhcpAuto := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
			DHCPAutoInterface: &dhcpAuto,
		}}},
	}

	deployment := metalmanDeployment(site, DefaultNamespace, Config{MetalmanImage: "example/metalman:default", APIServerEndpoint: "https://api.example:6443"})
	if deployment.Name != "metalman-controller-rack-a" {
		t.Fatalf("name = %q", deployment.Name)
	}

	if deployment.Namespace != DefaultNamespace {
		t.Fatalf("namespace = %q, want %q", deployment.Namespace, DefaultNamespace)
	}

	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Image != "example/metalman:default" {
		t.Fatalf("image = %q", container.Image)
	}

	if got := container.Args; len(got) != 3 || got[0] != "serve-pxe" || got[1] != "--site=rack-a" || got[2] != "--dhcp-auto-interface" {
		t.Fatalf("args = %#v", got)
	}

	// POD_NAMESPACE is sourced from the Downward API so the lease/RBAC stay
	// co-located with the Deployment under any install namespace.
	podNS := findEnv(container.Env, "POD_NAMESPACE")
	if podNS == nil || podNS.ValueFrom == nil || podNS.ValueFrom.FieldRef == nil ||
		podNS.ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
		t.Fatalf("POD_NAMESPACE env = %#v, want Downward API metadata.namespace", podNS)
	}

	// The operator-resolved endpoint is handed to metalman as METALMAN_APISERVER_URL.
	if got := findEnv(container.Env, "METALMAN_APISERVER_URL"); got == nil || got.Value != "https://api.example:6443" {
		t.Fatalf("METALMAN_APISERVER_URL env = %#v, want https://api.example:6443", got)
	}

	assertSiteOwnerRef(t, deployment.OwnerReferences, "rack-a", "site-uid")
	assertSiteAffinity(t, deployment.Spec.Template.Spec.Affinity, "rack-a")

	// hostNetwork singleton: must terminate the old pod before creating the new
	// one so it can rebind its host ports on a rolling restart.
	strategy := deployment.Spec.Strategy
	if strategy.Type != appsv1.RollingUpdateDeploymentStrategyType || strategy.RollingUpdate == nil {
		t.Fatalf("expected RollingUpdate strategy, got %+v", strategy)
	}

	if got := strategy.RollingUpdate.MaxSurge; got == nil || got.IntValue() != 0 {
		t.Fatalf("expected maxSurge=0, got %v", got)
	}

	if got := strategy.RollingUpdate.MaxUnavailable; got == nil || got.IntValue() != 1 {
		t.Fatalf("expected maxUnavailable=1, got %v", got)
	}
}

func TestMetalmanDeploymentRespectsNamespace(t *testing.T) {
	enabled := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
		}}},
	}

	deployment := metalmanDeployment(site, "custom-ns", Config{MetalmanImage: "example/metalman:default"})
	if deployment.Namespace != "custom-ns" {
		t.Fatalf("namespace = %q, want custom-ns", deployment.Namespace)
	}
}

func TestMetalmanDeploymentOmitsEmptyAPIServerURL(t *testing.T) {
	enabled := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Spec: unboundedv1alpha3.SiteSpec{Components: unboundedv1alpha3.SiteComponents{Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
			SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
		}}},
	}

	deployment := metalmanDeployment(site, DefaultNamespace, Config{MetalmanImage: "example/metalman:default"})

	container := deployment.Spec.Template.Spec.Containers[0]
	if got := findEnv(container.Env, "METALMAN_APISERVER_URL"); got != nil {
		t.Fatalf("METALMAN_APISERVER_URL env = %#v, want unset when APIServerEndpoint is empty", got)
	}
}

func findEnv(env []corev1.EnvVar, name string) *corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			return &env[i]
		}
	}

	return nil
}

func TestReconcilerNamespaceFallsBackToDefault(t *testing.T) {
	r := &SiteReconciler{}
	if got := r.namespace(); got != DefaultNamespace {
		t.Fatalf("namespace() = %q, want %q", got, DefaultNamespace)
	}

	r.Namespace = "custom-ns"
	if got := r.namespace(); got != "custom-ns" {
		t.Fatalf("namespace() = %q, want custom-ns", got)
	}
}

func TestStorageDaemonSetPointsAtPerSiteConfig(t *testing.T) {
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": "unbounded-storage-supervisor"},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app.kubernetes.io/name": "unbounded-storage-supervisor"}},
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "run"}},
					"volumes": []any{map[string]any{
						"name":      "config-source",
						"configMap": map[string]any{"name": "unbounded-storage-config"},
					}},
				},
			},
		},
	}}

	if err := mutateStorageObject(site, "storage-hash", obj); err != nil {
		t.Fatalf("mutateStorageObject returned error: %v", err)
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

func TestEnsureStorageConfigCreatesDefaultWhenAbsent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	r := &SiteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}

	hash, err := r.ensureStorageConfig(t.Context(), site)
	if err != nil {
		t.Fatalf("ensureStorageConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKey{Namespace: DefaultNamespace, Name: "unbounded-storage-config-rack-a"}, &got); err != nil {
		t.Fatalf("get created configmap: %v", err)
	}

	if got.Data["config.yaml"] == "" {
		t.Fatalf("default config.yaml was not seeded")
	}

	if hash != configMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact stored payload hash", hash)
	}

	assertSiteOwnerRef(t, got.OwnerReferences, "rack-a", "site-uid")
}

func TestEnsureStorageConfigAdoptsExistingAndPreservesData(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config-rack-a", Namespace: DefaultNamespace},
		Data:       map[string]string{"config.yaml": "custom: true"},
	}
	r := &SiteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()}
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "site-uid"}}

	hash, err := r.ensureStorageConfig(t.Context(), site)
	if err != nil {
		t.Fatalf("ensureStorageConfig: %v", err)
	}

	var got corev1.ConfigMap
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(existing), &got); err != nil {
		t.Fatalf("get adopted configmap: %v", err)
	}

	if got.Data["config.yaml"] != "custom: true" {
		t.Fatalf("existing config data was not preserved: %q", got.Data["config.yaml"])
	}

	if hash != configMapPayloadHash(&got) {
		t.Fatalf("hash = %q, want exact adopted payload hash", hash)
	}

	assertSiteOwnerRef(t, got.OwnerReferences, "rack-a", "site-uid")
}

func TestReconcilePerSiteComponentDisabledRunsCleanup(t *testing.T) {
	cleaned := false
	status := (&SiteReconciler{}).reconcilePerSiteComponent(
		false,
		func() error { t.Fatal("reconcile must not run when disabled"); return nil },
		func() error { cleaned = true; return nil },
	)

	if !cleaned {
		t.Fatal("cleanup was not called for a disabled component")
	}

	if !status.ready || status.reason != reasonDisabled {
		t.Fatalf("status = %+v, want ready with reason %q", status, reasonDisabled)
	}
}

func TestCleanupStorageDeletesPerSiteResourcesOnly(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add appsv1: %v", err)
	}

	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}

	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-supervisor-rack-a", Namespace: DefaultNamespace}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-config-rack-a", Namespace: DefaultNamespace}}
	otherDS := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "unbounded-storage-supervisor-rack-b", Namespace: DefaultNamespace}}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ds, cm, otherDS).Build()
	r := &SiteReconciler{Client: cl}

	if err := r.cleanupStorage(t.Context(), &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}); err != nil {
		t.Fatalf("cleanupStorage: %v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(ds), &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected rack-a DaemonSet deleted, got err=%v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(cm), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected rack-a ConfigMap deleted, got err=%v", err)
	}

	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(otherDS), &appsv1.DaemonSet{}); err != nil {
		t.Fatalf("expected rack-b DaemonSet preserved, got err=%v", err)
	}
}

func TestRetargetNamespaceRewritesToCustomNamespace(t *testing.T) {
	r := &SiteReconciler{Namespace: "custom-ns"}

	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "x", "namespace": "unbounded-system"},
		"subjects":   []any{map[string]any{"namespace": "unbounded-system"}},
	}}

	r.retargetNamespace(obj)

	if got := obj.GetNamespace(); got != "custom-ns" {
		t.Fatalf("namespace = %q, want custom-ns", got)
	}

	subjects, _, _ := unstructured.NestedSlice(obj.Object, "subjects")
	if subjects[0].(map[string]any)["namespace"] != "custom-ns" {
		t.Fatalf("subject namespace not rewritten: %v", subjects[0])
	}

	// Default install is a no-op.
	def := &SiteReconciler{}
	nsObj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": "unbounded-system"},
	}}
	def.retargetNamespace(nsObj)

	if nsObj.GetName() != "unbounded-system" {
		t.Fatalf("default install rewrote namespace to %q", nsObj.GetName())
	}
}

// Finding 4: retargeting a custom namespace must also rewrite the namespace
// where it appears as a substring - service-account usernames in VAP CEL and
// flag values - while leaving unrelated strings (image refs) untouched.
func TestRetargetNamespaceRewritesServiceAccountAndArgs(t *testing.T) {
	r := &SiteReconciler{Namespace: "custom-ns"}

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

	r.retargetNamespace(obj)

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

// Finding 3: the reconcile loop never applies CRDs (the operator installs them
// once at startup via BootstrapCRDs).
func TestSkipCRDObjects(t *testing.T) {
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "sites.unbounded-cloud.io"},
	}}

	if err := skipCRDObjects(crd); err != nil {
		t.Fatalf("skipCRDObjects: %v", err)
	}

	if crd.Object != nil {
		t.Fatal("expected CRD object nilled out")
	}

	other := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "x"},
	}}

	if err := skipCRDObjects(other); err != nil {
		t.Fatalf("skipCRDObjects: %v", err)
	}

	if other.Object == nil {
		t.Fatal("non-CRD object must be preserved")
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
}

func assertSiteAffinity(t *testing.T, affinity *corev1.Affinity, siteName string) {
	t.Helper()

	if affinity == nil || affinity.NodeAffinity == nil || affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("missing node affinity: %#v", affinity)
	}

	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 2 {
		t.Fatalf("node selector terms len = %d, want 2: %#v", len(terms), terms)
	}

	want := map[string]bool{siteLabelKey: false, deprecatedSiteLabelKey: false}

	for _, term := range terms {
		if len(term.MatchExpressions) != 1 {
			t.Fatalf("term must have one expression: %#v", term)
		}

		expr := term.MatchExpressions[0]
		if expr.Operator != corev1.NodeSelectorOpIn || len(expr.Values) != 1 || expr.Values[0] != siteName {
			t.Fatalf("unexpected site affinity expression: %#v", expr)
		}

		if _, ok := want[expr.Key]; !ok {
			t.Fatalf("unexpected site affinity key %q", expr.Key)
		}

		want[expr.Key] = true
	}

	for key, seen := range want {
		if !seen {
			t.Fatalf("site affinity missing key %q", key)
		}
	}
}

func assertSiteAffinityMap(t *testing.T, affinity map[string]any, siteName string) {
	t.Helper()

	converted := &corev1.Affinity{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(affinity, converted); err != nil {
		t.Fatalf("convert affinity: %v", err)
	}

	assertSiteAffinity(t, converted, siteName)
}

func TestApplyObject(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add appsv1 to scheme: %v", err)
	}

	r := &SiteReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	deployment := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}}},
		},
	}

	if err := r.applyObject(t.Context(), deployment); err != nil {
		t.Fatalf("applyObject returned error: %v", err)
	}
}
