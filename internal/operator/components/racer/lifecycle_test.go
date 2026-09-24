// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1alpha1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
)

func TestCacheInstallationIndependentOfSites(t *testing.T) {
	for _, sites := range [][]*unboundedv1alpha3.Site{nil, {testSite("unselected")}} {
		cache := testCache()
		cache.Spec.SiteSelector.MatchLabels = map[string]string{"no": "match"}
		env := testEnv(t, interceptor.Funcs{}, cache)

		plan := combinedPlan(t, env, sites...)
		if plan.Len() != len(sharedResources(env.Namespace))+2 {
			t.Fatalf("cache did not install all resources with %d Sites: %s", len(sites), plan.Summary())
		}

		if result := execute(t, env, plan); result.Err() != nil {
			t.Fatal(result.Err())
		}

		for _, obj := range []client.Object{controlDeployment(env.Namespace, env.Config), dataplaneDaemonSet(env.Namespace, env.Config)} {
			if err := env.Client.Delete(t.Context(), obj); err != nil {
				t.Fatal(err)
			}

			if result := execute(t, env, combinedPlan(t, env, sites...)); result.Err() != nil {
				t.Fatal(result.Err())
			}

			if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
				t.Fatalf("live cache failed to repair workload: %v", err)
			}
		}
	}
}

func TestCacheUninstallWorkloadDeletionOrders(t *testing.T) {
	for _, first := range []string{controlPlaneName, dataplaneName} {
		t.Run(first+"-first", func(t *testing.T) {
			writes := 0
			cache := testCache()

			env := testEnv(t, interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				writes++
				return c.Apply(ctx, obj, opts...)
			}}, cache)
			if result := execute(t, env, combinedPlan(t, env)); result.Err() != nil {
				t.Fatal(result.Err())
			}

			assignWorkloadUIDs(t, env)

			if err := env.Client.Delete(t.Context(), cache); err != nil {
				t.Fatal(err)
			}

			workloads := []client.Object{controlDeployment(env.Namespace, env.Config), dataplaneDaemonSet(env.Namespace, env.Config)}
			if first == dataplaneName {
				workloads[0], workloads[1] = workloads[1], workloads[0]
			}

			if err := env.Client.Delete(t.Context(), workloads[0]); err != nil {
				t.Fatal(err)
			}

			// Support is still repaired even when only the dataplane survives.
			if err := env.Client.Delete(t.Context(), serviceAccount(controlPlaneName, env.Namespace)); err != nil {
				t.Fatal(err)
			}

			env.Config.ImageTag = "upgrade"
			if result := execute(t, env, combinedPlan(t, env)); result.Err() != nil || len(result.Deferred) != 0 {
				t.Fatalf("retained upgrade: %+v", result)
			}

			if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(workloads[0]), workloads[0]); !apierrors.IsNotFound(err) {
				t.Fatalf("deleted sibling was recreated: %v", err)
			}

			if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(workloads[1]), workloads[1]); err != nil {
				t.Fatal(err)
			}

			var image string

			switch obj := workloads[1].(type) {
			case *appsv1.Deployment:
				image = obj.Spec.Template.Spec.Containers[0].Image
			case *appsv1.DaemonSet:
				image = obj.Spec.Template.Spec.Containers[0].Image
			}

			if image != env.Config.Image(workloads[1].GetName()) {
				t.Fatalf("surviving workload did not upgrade: %s", image)
			}

			if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: env.Namespace, Name: controlPlaneName}, serviceAccount(controlPlaneName, env.Namespace)); err != nil {
				t.Fatal("survivor lost shared support", err)
			}

			writes = 0
			if result := execute(t, env, combinedPlan(t, env)); result.Err() != nil || writes != 0 {
				t.Fatalf("retained convergence: %v, writes=%d", result.Err(), writes)
			}

			if err := env.Client.Delete(t.Context(), workloads[1]); err != nil {
				t.Fatal(err)
			}

			if plan := combinedPlan(t, env, testSite("still-present")); plan.Len() != 0 {
				t.Fatalf("support remnants reinstalled resources: %s", plan.Summary())
			}
		})
	}
}

func TestTerminatingLifecycleInputs(t *testing.T) {
	now := metav1.Now()
	cache := testCache()
	cache.DeletionTimestamp = &now
	cache.Finalizers = []string{"example.test/hold"}

	for _, obj := range []client.Object{controlDeployment("custom", component.Config{}), dataplaneDaemonSet("custom", component.Config{})} {
		obj.SetDeletionTimestamp(&now)
		obj.SetFinalizers([]string{"example.test/hold"})

		env := testEnv(t, interceptor.Funcs{}, cache, obj)
		if plan := combinedPlan(t, env); plan.Len() != 0 {
			t.Fatalf("terminating cache/workload voted to install: %s", plan.Summary())
		}
	}
}

func TestEverySupportResourceIsInsufficientToReinstall(t *testing.T) {
	for _, marker := range sharedResources("custom") {
		t.Run(marker.GetObjectKind().GroupVersionKind().Kind+"/"+marker.GetName(), func(t *testing.T) {
			env := testEnv(t, interceptor.Funcs{}, marker)
			if plan := combinedPlan(t, env, testSite("live")); plan.Len() != 0 {
				t.Fatalf("support resource triggered installation: %s", plan.Summary())
			}
		})
	}
}

func TestLifecycleReadErrorsPreserveDeployedConfig(t *testing.T) {
	for _, failure := range []string{"list caches", controlPlaneName, dataplaneName} {
		t.Run(failure, func(t *testing.T) {
			blocked := false
			boom := errors.New("API unavailable")

			env := testEnv(t, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if blocked && failure == "list caches" {
						return boom
					}

					return c.List(ctx, list, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if blocked && key.Name == failure {
						return boom
					}

					return c.Get(ctx, key, obj, opts...)
				},
			}, testCache())
			if result := execute(t, env, combinedPlan(t, env)); result.Err() != nil {
				t.Fatal(result.Err())
			}

			assignWorkloadUIDs(t, env)

			if err := env.Client.Delete(t.Context(), testCache()); err != nil {
				t.Fatal(err)
			}

			before := dataplaneDaemonSet(env.Namespace, env.Config)
			if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(before), before); err != nil {
				t.Fatal(err)
			}

			blocked = true

			env.Config.ImageTag = "upgrade"
			for _, c := range []component.ClusterComponent{NewControlPlane(), NewDataplane()} {
				plan, _, err := c.Plan(t.Context(), env, nil)
				if !errors.Is(err, boom) || plan.Len() != 0 {
					t.Fatalf("API failure must return retryable error without writes: %v %v", plan, err)
				}
			}

			blocked = false

			after := dataplaneDaemonSet(env.Namespace, env.Config)
			if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(after), after); err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(before, after) {
				t.Fatal("read failure changed deployed config")
			}

			if result := execute(t, env, combinedPlan(t, env)); result.Err() != nil {
				t.Fatal("retry did not recover", result.Err())
			}
		})
	}
}

func TestCacheWatchesIntentNotStatus(t *testing.T) {
	p := cachePredicate()
	base := testCache()
	base.UID = "original-cache-uid"

	for _, tc := range []struct {
		name   string
		mutate func(*racerv1alpha1.P2PCache)
		want   bool
	}{
		{"identical relist", func(*racerv1alpha1.P2PCache) {}, false},
		{"UID-only recreation", func(c *racerv1alpha1.P2PCache) { c.UID = "replacement-cache-uid" }, true},
		{"status", func(c *racerv1alpha1.P2PCache) { c.Status.Participants.Ready = 2 }, false},
		{"resource version", func(c *racerv1alpha1.P2PCache) { c.ResourceVersion = "new" }, false},
		{"selector", func(c *racerv1alpha1.P2PCache) { c.Spec.SiteSelector.MatchLabels = map[string]string{"x": "y"} }, true},
		{"generation", func(c *racerv1alpha1.P2PCache) { c.Spec.CacheGeneration++ }, true},
		{"termination", func(c *racerv1alpha1.P2PCache) { now := metav1.Now(); c.DeletionTimestamp = &now }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := base.DeepCopy()
			tc.mutate(next)

			if got := p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: next}); got != tc.want {
				t.Fatalf("watch=%v want=%v", got, tc.want)
			}
		})
	}

	if !p.Create(event.CreateEvent{Object: base}) || !p.Delete(event.DeleteEvent{Object: base}) || p.Generic(event.GenericEvent{Object: base}) {
		t.Fatal("cache watch dropped creation/deletion or accepted generic event")
	}
}
