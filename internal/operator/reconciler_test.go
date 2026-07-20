// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
)

// fakeCluster is a configurable ClusterComponent that records whether it ran.
type fakeCluster struct {
	name      string
	condition string
	result    component.Result
	ran       *bool
}

func (f fakeCluster) Name() string          { return f.name }
func (f fakeCluster) ConditionType() string { return f.condition }
func (f fakeCluster) Reconcile(context.Context, *component.Env, []unboundedv1alpha3.Site) component.Result {
	if f.ran != nil {
		*f.ran = true
	}

	return f.result
}

// fakeSite is a configurable SiteComponent that records enable/reconcile/cleanup.
type fakeSite struct {
	name       string
	condition  string
	enabled    bool
	result     component.Result
	cleanupErr error
	reconciled *bool
	cleaned    *bool
}

func (f fakeSite) Name() string                         { return f.name }
func (f fakeSite) ConditionType() string                { return f.condition }
func (f fakeSite) Enabled(*unboundedv1alpha3.Site) bool { return f.enabled }
func (f fakeSite) Reconcile(context.Context, *component.Env, *unboundedv1alpha3.Site) component.Result {
	if f.reconciled != nil {
		*f.reconciled = true
	}

	return f.result
}

func (f fakeSite) Cleanup(context.Context, *component.Env, *unboundedv1alpha3.Site) error {
	if f.cleaned != nil {
		*f.cleaned = true
	}

	return f.cleanupErr
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

func TestReconcilePatchesAllConditionsAndReturnsComponentErrors(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", Generation: 7}}

	netErr := errors.New("net reconcile failed")
	machinaErr := errors.New("machina reconcile failed")

	cleaned := false
	registry := &component.Registry{
		Cluster: []component.ClusterComponent{
			fakeCluster{name: "net", condition: "NetReady", result: component.Failed(netErr)},
			fakeCluster{name: "machina", condition: "MachinaReady", result: component.Failed(machinaErr)},
		},
		Site: []component.SiteComponent{
			fakeSite{name: "metalman", condition: "MetalmanReady", enabled: false, cleaned: &cleaned},
			fakeSite{name: "storage", condition: "StorageReady", enabled: false},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(site).WithStatusSubresource(site).Build()
	r := &SiteReconciler{Client: cl, Scheme: scheme, Registry: registry}

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(site)})
	if !errors.Is(err, netErr) || !errors.Is(err, machinaErr) {
		t.Fatalf("Reconcile error = %v, want joined net and machina errors", err)
	}

	if !cleaned {
		t.Fatal("disabled site component cleanup was not called")
	}

	var got unboundedv1alpha3.Site
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(site), &got); err != nil {
		t.Fatalf("get reconciled Site: %v", err)
	}

	want := map[string]struct {
		status metav1.ConditionStatus
		reason string
	}{
		"NetReady":      {status: metav1.ConditionFalse, reason: component.ReasonReconcileError},
		"MachinaReady":  {status: metav1.ConditionFalse, reason: component.ReasonReconcileError},
		"MetalmanReady": {status: metav1.ConditionTrue, reason: component.ReasonDisabled},
		"StorageReady":  {status: metav1.ConditionTrue, reason: component.ReasonDisabled},
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
			SubResourcePatch: func(_ context.Context, _ client.Client, subResourceName string, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption) error {
				if subResourceName != "status" {
					t.Fatalf("subresource = %q, want status", subResourceName)
				}

				patched = obj.(*unboundedv1alpha3.Site).DeepCopy()

				return patchErr
			},
		}).
		Build()

	registry := &component.Registry{
		Cluster: []component.ClusterComponent{
			fakeCluster{name: "net", condition: "NetReady", result: component.Failed(componentErr)},
			fakeCluster{name: "machina", condition: "MachinaReady", result: component.Reconciled()},
		},
		Site: []component.SiteComponent{
			fakeSite{name: "metalman", condition: "MetalmanReady", enabled: false},
			fakeSite{name: "storage", condition: "StorageReady", enabled: false},
		},
	}
	r := &SiteReconciler{Client: cl, Scheme: scheme, Registry: registry}

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(site)})
	if !errors.Is(err, componentErr) || !errors.Is(err, patchErr) {
		t.Fatalf("Reconcile error = %v, want joined component and status patch errors", err)
	}

	if patched == nil || len(patched.Status.Conditions) != 4 {
		t.Fatalf("status patch received conditions %#v, want all four", patched)
	}
}

func TestReconcileSiteLessPassRunsClusterComponentsOnly(t *testing.T) {
	scheme := newReconcilerTestScheme(t)

	clusterRan := false
	siteReconciled := false
	siteCleaned := false

	registry := &component.Registry{
		Cluster: []component.ClusterComponent{
			fakeCluster{name: "net", condition: "NetReady", result: component.Reconciled(), ran: &clusterRan},
		},
		Site: []component.SiteComponent{
			fakeSite{name: "storage", condition: "StorageReady", enabled: true, result: component.Reconciled(), reconciled: &siteReconciled, cleaned: &siteCleaned},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SiteReconciler{Client: cl, Scheme: scheme, Registry: registry}

	// A request for a name with no Site (deletion or the synthetic singleton
	// request) drives the Site-less pass.
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: component.SingletonRequestName}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !clusterRan {
		t.Fatal("cluster component did not run on the Site-less pass")
	}

	if siteReconciled || siteCleaned {
		t.Fatalf("site component ran on the Site-less pass: reconciled=%t cleaned=%t", siteReconciled, siteCleaned)
	}
}

func TestReconcileSiteLessPassJoinsClusterErrors(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	netErr := errors.New("net failed on singleton pass")

	registry := &component.Registry{
		Cluster: []component.ClusterComponent{
			fakeCluster{name: "net", condition: "NetReady", result: component.Failed(netErr)},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SiteReconciler{Client: cl, Scheme: scheme, Registry: registry}

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: component.SingletonRequestName}})
	if !errors.Is(err, netErr) {
		t.Fatalf("Reconcile error = %v, want net error", err)
	}
}

func TestReconcileSiteComponentDisabledRunsCleanup(t *testing.T) {
	cleaned := false
	c := fakeSite{name: "storage", condition: "StorageReady", enabled: false, cleaned: &cleaned}

	res := reconcileSiteComponent(t.Context(), &component.Env{}, c, &unboundedv1alpha3.Site{})
	if !cleaned {
		t.Fatal("cleanup was not called for a disabled component")
	}

	if !res.Ready || res.Reason != component.ReasonDisabled {
		t.Fatalf("result = %+v, want ready with reason %q", res, component.ReasonDisabled)
	}
}

func TestReconcileSiteComponentDisabledCleanupErrorFails(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	c := fakeSite{name: "storage", condition: "StorageReady", enabled: false, cleanupErr: cleanupErr}

	res := reconcileSiteComponent(t.Context(), &component.Env{}, c, &unboundedv1alpha3.Site{})
	if res.Ready || !errors.Is(res.Err, cleanupErr) {
		t.Fatalf("result = %+v, want failed with cleanup error", res)
	}
}

func TestReconcilerNamespaceFallsBackToDefault(t *testing.T) {
	r := &SiteReconciler{}
	if got := r.namespace(); got != component.DefaultNamespace {
		t.Fatalf("namespace() = %q, want %q", got, component.DefaultNamespace)
	}

	r.Namespace = "custom-ns"
	if got := r.namespace(); got != "custom-ns" {
		t.Fatalf("namespace() = %q, want custom-ns", got)
	}
}

func TestDefaultRegistryIsValidAndComplete(t *testing.T) {
	reg := DefaultRegistry()
	if err := reg.Validate(); err != nil {
		t.Fatalf("DefaultRegistry is invalid: %v", err)
	}

	wantConditions := map[string]bool{"NetReady": false, "MachinaReady": false, "MetalmanReady": false, "StorageReady": false}

	for _, c := range reg.Cluster {
		wantConditions[c.ConditionType()] = true
	}

	for _, c := range reg.Site {
		wantConditions[c.ConditionType()] = true
	}

	for condition, seen := range wantConditions {
		if !seen {
			t.Fatalf("DefaultRegistry is missing condition %q", condition)
		}
	}
}
