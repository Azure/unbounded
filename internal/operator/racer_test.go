// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
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
	for _, s := range sites {
		s.Spec.Components.Racer = &unboundedv1alpha3.RacerComponentSpec{SiteComponentSpec: enabled()}
	}

	writes := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sites[0], sites[1]).WithStatusSubresource(&unboundedv1alpha3.Site{}).WithInterceptorFuncs(interceptor.Funcs{
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
	// Capacity is an independent signed runtime policy. Even repeated Site
	// reconciliation must not write deployment intent or trigger a Pod rollout.
	var before appsv1.DaemonSet

	key := client.ObjectKey{Namespace: "custom", Name: "racer-dataplane"}
	if err := c.Get(t.Context(), key, &before); err != nil {
		t.Fatal(err)
	}

	for _, capacity := range []string{"2Ti", "4Ti", "32Mi", ""} {
		var site unboundedv1alpha3.Site
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(sites[0]), &site); err != nil {
			t.Fatal(err)
		}

		site.Spec.Components.Racer.CacheSize = nil

		if capacity != "" {
			q := resource.MustParse(capacity)
			site.Spec.Components.Racer.CacheSize = &q
		}

		if err := c.Update(t.Context(), &site); err != nil {
			t.Fatal(err)
		}

		run(site.Name)
		run(component.SingletonRequestName)

		var after appsv1.DaemonSet
		if err := c.Get(t.Context(), key, &after); err != nil {
			t.Fatal(err)
		}

		if writes != 0 || !reflect.DeepEqual(before.Spec, after.Spec) || before.ResourceVersion != after.ResourceVersion {
			t.Fatalf("capacity %q changed managed deployment: %d SSA writes", capacity, writes)
		}
	}
	// A singleton override event must reach the shared DaemonSet.
	cm := overridesConfigMap(map[string]string{"racer.yaml": `apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
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

	var disabled unboundedv1alpha3.Site
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(sites[0]), &disabled); err != nil {
		t.Fatal(err)
	}

	disabled.Spec.Components.Racer.Enabled = ptr.To(false)
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
