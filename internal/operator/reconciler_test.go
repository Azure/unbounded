// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"testing"
	"time"

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
func (f fakeCluster) Plan(context.Context, *component.Env, []unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	if f.ran != nil {
		*f.ran = true
	}

	return component.NewPlan(), f.result, nil
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
func (f fakeSite) Plan(context.Context, *component.Env, *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	if f.reconciled != nil {
		*f.reconciled = true
	}

	return component.NewPlan(), f.result, nil
}

func (f fakeSite) CleanupPlan(context.Context, *component.Env, *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	if f.cleaned != nil {
		*f.cleaned = true
	}

	if f.cleanupErr != nil {
		return nil, component.Result{}, f.cleanupErr
	}

	return component.NewPlan(), component.Disabled("component disabled"), nil
}

// countingSite is a SiteComponent that counts how many Sites it planned for.
type countingSite struct {
	name      string
	condition string
	runs      *int
}

func (c countingSite) Name() string                       { return c.name }
func (c countingSite) ConditionType() string              { return c.condition }
func (countingSite) Enabled(*unboundedv1alpha3.Site) bool { return true }

func (c countingSite) Plan(context.Context, *component.Env, *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	*c.runs++

	return component.NewPlan(), component.Reconciled(), nil
}

func (countingSite) CleanupPlan(context.Context, *component.Env, *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	return component.NewPlan(), component.Disabled("component disabled"), nil
}

// statefulCluster is a pointer-receiver ClusterComponent that counts how many
// times it planned, used to prove the driver reuses a single registry
// instance instead of rebuilding it each pass.
type statefulCluster struct {
	runs int
}

func (c *statefulCluster) Name() string          { return "stateful" }
func (c *statefulCluster) ConditionType() string { return "StatefulReady" }
func (c *statefulCluster) Plan(context.Context, *component.Env, []unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	c.runs++

	return component.NewPlan(), component.Reconciled(), nil
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

func TestReconcileSiteLessPassWithNoSitesRunsClusterComponentsOnly(t *testing.T) {
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

	// With no Sites there is nothing to fan out to, and nowhere to publish
	// conditions.
	if siteReconciled || siteCleaned {
		t.Fatalf("site component ran with no Sites: reconciled=%t cleaned=%t", siteReconciled, siteCleaned)
	}
}

// TestReconcileSiteLessPassFansOutToEverySite covers why the fan-out exists.
//
// The overrides ConfigMap watch enqueues only the synthetic singleton request,
// because a handler that listed Sites at event-delivery time would consume the
// event and lose the fan-out permanently whenever that List failed. Without
// fanning out here, per-Site components would never see an override change.
func TestReconcileSiteLessPassFansOutToEverySite(t *testing.T) {
	scheme := newReconcilerTestScheme(t)

	var reconciled int

	registry := &component.Registry{
		Cluster: []component.ClusterComponent{
			fakeCluster{name: "net", condition: "NetReady", result: component.Reconciled()},
		},
		Site: []component.SiteComponent{
			countingSite{name: "storage", condition: "StorageReady", runs: &reconciled},
		},
	}

	alpha := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}}
	bravo := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "bravo"}}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(alpha, bravo).
		WithStatusSubresource(alpha, bravo).
		Build()

	r := &SiteReconciler{Client: cl, Scheme: scheme, Registry: registry}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: component.SingletonRequestName}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if reconciled != 2 {
		t.Fatalf("site component ran %d times, want once per Site", reconciled)
	}

	// Conditions must be published on every Site the pass touched, or a
	// fanned-out failure would be invisible.
	for _, name := range []string{"alpha", "bravo"} {
		var got unboundedv1alpha3.Site
		if err := cl.Get(t.Context(), client.ObjectKey{Name: name}, &got); err != nil {
			t.Fatalf("get %s: %v", name, err)
		}

		if len(got.Status.Conditions) == 0 {
			t.Fatalf("Site %s received no conditions from the fan-out", name)
		}
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

func TestPlanSiteComponentDisabledPlansCleanup(t *testing.T) {
	cleaned := false
	c := fakeSite{name: "storage", condition: "StorageReady", enabled: false, cleaned: &cleaned}

	_, res, err := planSiteComponent(t.Context(), &component.Env{}, c, &unboundedv1alpha3.Site{})
	if err != nil {
		t.Fatalf("planSiteComponent: %v", err)
	}

	if !cleaned {
		t.Fatal("cleanup was not planned for a disabled component")
	}

	if !res.Ready || res.Reason != component.ReasonDisabled {
		t.Fatalf("result = %+v, want ready with reason %q", res, component.ReasonDisabled)
	}
}

func TestPlanSiteComponentDisabledCleanupErrorFails(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	c := fakeSite{name: "storage", condition: "StorageReady", enabled: false, cleanupErr: cleanupErr}

	_, _, err := planSiteComponent(t.Context(), &component.Env{}, c, &unboundedv1alpha3.Site{})
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("err = %v, want the cleanup error", err)
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

	wantConditions := map[string]bool{"NetReady": false, "MachinaReady": false, "GantryReady": false, "MetalmanReady": false, "StorageReady": false}

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

func TestReconcileRequeuesAfterSmallestComponentInterval(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}

	registry := &component.Registry{
		Cluster: []component.ClusterComponent{
			fakeCluster{name: "net", condition: "NetReady", result: component.NotReadyAfter("Waiting", "net not ready", 5*time.Minute)},
			fakeCluster{name: "machina", condition: "MachinaReady", result: component.NotReadyAfter("Waiting", "machina not ready", 30*time.Second)},
		},
		Site: []component.SiteComponent{
			fakeSite{name: "storage", condition: "StorageReady", enabled: true, result: component.Reconciled()},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(site).WithStatusSubresource(site).Build()
	r := &SiteReconciler{Client: cl, Scheme: scheme, Registry: registry}

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(site)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %s, want the smallest positive interval 30s", res.RequeueAfter)
	}
}

func TestReconcileSiteLessPassHonorsRequeue(t *testing.T) {
	scheme := newReconcilerTestScheme(t)

	registry := &component.Registry{
		Cluster: []component.ClusterComponent{
			fakeCluster{name: "net", condition: "NetReady", result: component.NotReadyAfter("Waiting", "net not ready", time.Minute)},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &SiteReconciler{Client: cl, Scheme: scheme, Registry: registry}

	// The Site-less pass has no status to publish, but a not-ready cluster
	// component must still schedule a retry (regression: the old path dropped it).
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: component.SingletonRequestName}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if res.RequeueAfter != time.Minute {
		t.Fatalf("RequeueAfter = %s, want 1m on the Site-less pass", res.RequeueAfter)
	}
}

func TestReconcilePrunesStaleConditions(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "rack-a"},
		Status: unboundedv1alpha3.SiteStatus{
			Conditions: []metav1.Condition{{
				// A condition from a component that has since been removed/renamed.
				Type:   "LegacyReady",
				Status: metav1.ConditionTrue,
				Reason: "Reconciled",
			}},
		},
	}

	registry := &component.Registry{
		Cluster: []component.ClusterComponent{
			fakeCluster{name: "net", condition: "NetReady", result: component.Reconciled()},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(site).WithStatusSubresource(site).Build()
	r := &SiteReconciler{Client: cl, Scheme: scheme, Registry: registry}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(site)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got unboundedv1alpha3.Site
	if err := cl.Get(t.Context(), client.ObjectKeyFromObject(site), &got); err != nil {
		t.Fatalf("get reconciled Site: %v", err)
	}

	if apimeta.FindStatusCondition(got.Status.Conditions, "LegacyReady") != nil {
		t.Fatalf("stale condition LegacyReady was not pruned: %#v", got.Status.Conditions)
	}

	if apimeta.FindStatusCondition(got.Status.Conditions, "NetReady") == nil {
		t.Fatalf("current condition NetReady missing after prune: %#v", got.Status.Conditions)
	}
}

func TestRegistryMaterializedOnceAndInstanceReused(t *testing.T) {
	// registry() must materialize r.Registry once and return the same instance,
	// so watch wiring and every Reconcile share components (not throwaway copies).
	r := &SiteReconciler{}

	first := r.registry()
	if r.Registry == nil {
		t.Fatal("registry() did not materialize r.Registry")
	}

	if second := r.registry(); second != first {
		t.Fatal("registry() rebuilt the registry instead of reusing the materialized instance")
	}

	// A stateful component reused across reconciles retains its state, which is
	// only possible because the same instance is reused each pass.
	scheme := newReconcilerTestScheme(t)
	site := &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}}
	stateful := &statefulCluster{}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(site).WithStatusSubresource(site).Build()
	sr := &SiteReconciler{
		Client:   cl,
		Scheme:   scheme,
		Registry: &component.Registry{Cluster: []component.ClusterComponent{stateful}},
	}

	for i := 0; i < 2; i++ {
		if _, err := sr.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(site)}); err != nil {
			t.Fatalf("Reconcile %d: %v", i, err)
		}
	}

	if stateful.runs != 2 {
		t.Fatalf("stateful component runs = %d, want 2 (instance not reused across reconciles)", stateful.runs)
	}
}

// TestPlanHasExactlyOneNamespaceOperation is a regression test over the real
// registry.
//
// Every component's manifests ship the Namespace so each set stays installable
// on its own, and every component used to reconcile the one it shipped: once
// per cluster component, and once per Site for the per-Site ones. They do not
// agree on its labels and all apply under one field owner, so the label flipped
// on every pass depending on which component was planned last. Under
// server-side apply that is a write loop, because an owner that stops declaring
// a field removes it.
//
// Asserting over DefaultRegistry rather than a fake is the point: the failure
// mode is a component shipping a Namespace nobody remembered about.
func TestPlanHasExactlyOneNamespaceOperation(t *testing.T) {
	scheme := newReconcilerTestScheme(t)

	sites := []unboundedv1alpha3.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "rack-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "rack-b"}},
	}

	targets := []*unboundedv1alpha3.Site{&sites[0], &sites[1]}

	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	env := &component.Env{Client: cl, Scheme: scheme, Namespace: component.DefaultNamespace}

	_, _, plan := planComponents(t.Context(), env, DefaultRegistry(), sites, targets)

	var namespaces []string

	for _, op := range plan.Operations {
		if op.Object.GetKind() == component.NamespaceKind {
			namespaces = append(namespaces, op.Component+" "+op.Kind.String()+" "+op.Ref().String())
		}
	}

	if len(namespaces) != 1 {
		t.Fatalf("plan holds %d namespace operations, want exactly one: %v", len(namespaces), namespaces)
	}

	if got := namespaces[0]; got != component.NamespaceOwner+" Apply Namespace/"+component.DefaultNamespace {
		t.Fatalf("namespace operation = %q, want it owned by the operator", got)
	}
}
