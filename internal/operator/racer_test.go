// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"reflect"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1alpha1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/components/racer"
)

func TestRacerSingletonFanoutAndSiteLifecycle(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	sites := []*unboundedv1alpha3.Site{
		{ObjectMeta: metav1.ObjectMeta{Name: "rack-a", UID: "a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "rack-b", UID: "b"}},
	}
	cache := &racerv1alpha1.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "custom-cache"}}

	writes := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sites[0], sites[1], cache).WithStatusSubresource(&unboundedv1alpha3.Site{}).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			writes++
			return c.Apply(ctx, obj, opts...)
		},
	}).Build()
	r := &SiteReconciler{Client: c, Scheme: scheme, Namespace: "custom", Config: Config{ImageRegistry: "example.test/team", ImageTag: "v1"}, Registry: &component.Registry{Cluster: []component.ClusterComponent{racer.NewControlPlane(), racer.NewDataplane()}}}
	run := func(name string) {
		t.Helper()

		result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: name}})
		if err != nil || result.RequeueAfter != 0 {
			t.Fatalf("reconcile: %+v %v", result, err)
		}
	}
	run(component.SingletonRequestName)

	for _, s := range sites {
		var got unboundedv1alpha3.Site
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(s), &got); err != nil {
			t.Fatal(err)
		}

		for _, condition := range []string{"RacerControlPlaneReady", "RacerDataplaneReady"} {
			if !apimeta.IsStatusConditionTrue(got.Status.Conditions, condition) {
				t.Fatalf("missing applied-intent condition %s: %+v", condition, got.Status.Conditions)
			}
		}
	}

	writes = 0

	run(component.SingletonRequestName)

	if writes != 0 {
		t.Fatalf("steady singleton fanout made %d SSA writes", writes)
	}
	// Site labels and cache selection are runtime policy. Even repeated Site
	// reconciliation must not write deployment intent or trigger a Pod rollout.
	var before appsv1.DaemonSet

	key := client.ObjectKey{Namespace: "custom", Name: "racer-dataplane"}
	if err := c.Get(t.Context(), key, &before); err != nil {
		t.Fatal(err)
	}

	for _, selection := range []string{"rack-a", "rack-b", "no-match", ""} {
		var site unboundedv1alpha3.Site
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(sites[0]), &site); err != nil {
			t.Fatal(err)
		}

		site.Labels = map[string]string{"cache-group": selection}

		if err := c.Update(t.Context(), &site); err != nil {
			t.Fatal(err)
		}

		if err := c.Get(t.Context(), client.ObjectKeyFromObject(cache), cache); err != nil {
			t.Fatal(err)
		}

		cache.Spec.SiteSelector.MatchLabels = map[string]string{"cache-group": selection}
		if err := c.Update(t.Context(), cache); err != nil {
			t.Fatal(err)
		}

		run(site.Name)
		run(component.SingletonRequestName)

		var after appsv1.DaemonSet
		if err := c.Get(t.Context(), key, &after); err != nil {
			t.Fatal(err)
		}

		if writes != 0 || !reflect.DeepEqual(before.Spec, after.Spec) || before.ResourceVersion != after.ResourceVersion {
			t.Fatalf("selection %q changed managed deployment: %d SSA writes", selection, writes)
		}
	}
	// A singleton override event must reach the shared DaemonSet.
	cm := overridesConfigMap(map[string]string{"racer.yaml": `apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
- component: racer-controlplane
  kind: Deployment
  extraArgs:
    controller: ["--ca-overlap-delay=10m"]
- component: racer-dataplane
  kind: DaemonSet
  patch:
    spec:
      template:
        spec:
          containers:
          - name: dataplane
            image: example.test/pinned:v2
`})

	cm.Namespace = "custom"

	cm.ResourceVersion = ""
	if err := c.Create(t.Context(), cm); err != nil {
		t.Fatal(err)
	}

	run(component.SingletonRequestName)

	var ds appsv1.DaemonSet
	if err := c.Get(t.Context(), key, &ds); err != nil {
		t.Fatal(err)
	}

	if ds.Spec.Template.Spec.Containers[0].Image != "example.test/pinned:v2" {
		t.Fatal("singleton event failed to fan out overrides")
	}

	var cp appsv1.Deployment
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "custom", Name: "racer-controlplane"}, &cp); err != nil {
		t.Fatal(err)
	}

	args := cp.Spec.Template.Spec.Containers[0].Args
	if !slices.Contains(args, "-state-namespace=custom") || !slices.Contains(args, "--ca-overlap-delay=10m") {
		t.Fatalf("CA overlap override lost required or custom flags: %v", args)
	}

	var disabled unboundedv1alpha3.Site
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(sites[0]), &disabled); err != nil {
		t.Fatal(err)
	}

	disabled.Labels = nil
	if err := c.Update(t.Context(), &disabled); err != nil {
		t.Fatal(err)
	}

	run(disabled.Name)

	if err := c.Get(t.Context(), key, &appsv1.DaemonSet{}); err != nil {
		t.Fatalf("Site opt-out lost retained dataplane: %v", err)
	}

	for _, s := range sites {
		if err := c.Delete(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}

	run(sites[1].Name)

	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "custom", Name: "racer-controlplane"}, &appsv1.Deployment{}); err != nil {
		t.Fatal("singleton removed after Sites deleted", err)
	}
}

func TestRacerCacheSingletonWithoutSitesOrGantry(t *testing.T) {
	scheme := newReconcilerTestScheme(t)
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	writes := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			writes++
			return c.Apply(ctx, obj, opts...)
		},
	}).Build()
	r := &SiteReconciler{Client: c, Scheme: scheme, Namespace: "custom", Config: Config{ImageRegistry: "example.test/team", ImageTag: "v1"}, Registry: &component.Registry{Cluster: []component.ClusterComponent{racer.NewControlPlane(), racer.NewDataplane()}}}
	run := func() {
		t.Helper()

		result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: component.SingletonRequestName}})
		if err != nil || result.RequeueAfter != 0 {
			t.Fatalf("singleton reconcile: %+v %v", result, err)
		}
	}
	run()

	cache := &racerv1alpha1.P2PCache{ObjectMeta: metav1.ObjectMeta{Name: "independent-cache"}, Spec: racerv1alpha1.P2PCacheSpec{SiteSelector: metav1.LabelSelector{MatchLabels: map[string]string{"no": "match"}}}}
	if err := c.Create(t.Context(), cache); err != nil {
		t.Fatal(err)
	}

	run()

	workloads := []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "racer-controlplane", Namespace: "custom"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane", Namespace: "custom"}},
	}
	for _, obj := range workloads {
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal("cache singleton did not install Racer", err)
		}

		obj.SetUID("fake-uid")

		if err := c.Update(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.Delete(t.Context(), cache); err != nil {
		t.Fatal(err)
	}

	writes = 0

	run()

	if writes != 0 {
		t.Fatalf("cache deletion changed converged deployment: writes=%d", writes)
	}

	for _, obj := range workloads {
		if err := c.Delete(t.Context(), obj); err != nil {
			t.Fatal(err)
		}

		run()

		if err := c.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Fatalf("singleton resurrected removed workload: %v", err)
		}
	}

	if writes != 0 {
		t.Fatalf("uninstall made %d writes", writes)
	}
}
