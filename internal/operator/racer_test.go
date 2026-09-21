// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	r := &SiteReconciler{Client: c, Scheme: scheme, Namespace: "custom", Config: Config{ImageRegistry: "example.test/team", ImageTag: "v1"}, Registry: &component.Registry{Cluster: []component.ClusterComponent{racer.NewControlPlane()}, Site: []component.SiteComponent{racer.NewDataplane()}}}
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
	// A singleton override event must reach both per-Site DaemonSets.
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

	for _, s := range sites {
		var ds appsv1.DaemonSet
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: "custom", Name: racer.SiteDaemonSetName(s.Name)}, &ds); err != nil {
			t.Fatal(err)
		}

		if ds.Spec.Template.Spec.Containers[0].Image != "example.test/pinned:v2" {
			t.Fatal("singleton event failed to fan out overrides")
		}
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

	if err := c.Get(t.Context(), client.ObjectKey{Namespace: "custom", Name: racer.SiteDaemonSetName(disabled.Name)}, &appsv1.DaemonSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("disabled dataplane survived: %v", err)
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
