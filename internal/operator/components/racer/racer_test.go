// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1alpha1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func testEnv(t *testing.T, funcs interceptor.Funcs, objects ...client.Object) *component.Env {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, policyv1.AddToScheme, rbacv1.AddToScheme, unboundedv1alpha3.AddToScheme, racerv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	return &component.Env{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithInterceptorFuncs(funcs).Build(), Scheme: scheme, Namespace: "custom", Config: component.Config{ImageRegistry: "example.test/team", ImageTag: "v1"}}
}

func combinedPlan(t *testing.T, env *component.Env, sites ...*unboundedv1alpha3.Site) *component.Plan {
	t.Helper()

	values := make([]unboundedv1alpha3.Site, 0, len(sites))
	for _, s := range sites {
		values = append(values, *s)
	}

	plan, res, err := NewControlPlane().Plan(t.Context(), env, values)
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("control plan: %+v %v", res, err)
	}

	p, res, err := NewDataplane().Plan(t.Context(), env, values)
	if err != nil || res.RequeueAfter != 0 {
		t.Fatalf("dataplane plan: %+v %v", res, err)
	}

	plan.Merge(p)

	return plan
}

func execute(t *testing.T, env *component.Env, plan *component.Plan) component.ExecutionResult {
	t.Helper()

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}

	return result
}

func testCache() *racerv1alpha1.ClusterCache {
	return &racerv1alpha1.ClusterCache{ObjectMeta: metav1.ObjectMeta{Name: "custom-cache", UID: "custom-cache-uid"}, Spec: racerv1alpha1.ClusterCacheSpec{CacheGeneration: 1, MaxCandidateAttempts: 3}}
}

// The fake API does not allocate UIDs. Retention deliberately requires the same
// incarnation preconditions as the real API, so supply them in lifecycle tests.
func assignWorkloadUIDs(t *testing.T, env *component.Env) {
	t.Helper()

	for _, obj := range []client.Object{controlDeployment(env.Namespace, env.Config), dataplaneDaemonSet(env.Namespace, env.Config)} {
		if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatal(err)
		}

		obj.SetUID(types.UID(obj.GetName()))

		if err := env.Client.Update(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOptInRetentionAndCleanup(t *testing.T) {
	env := testEnv(t, interceptor.Funcs{})

	site := testSite("rack-a")
	if p := combinedPlan(t, env, site); p.Len() != 0 {
		t.Fatal("Site without cache planned installation")
	}

	if p := combinedPlan(t, env); p.Len() != 0 {
		t.Fatal("empty fresh install planned writes")
	}

	cache := testCache()

	cache.Spec.SiteSelector.MatchLabels = map[string]string{"absent": "true"}
	if err := env.Client.Create(t.Context(), cache); err != nil {
		t.Fatal(err)
	}

	if result := execute(t, env, combinedPlan(t, env)); result.Err() != nil {
		t.Fatal(result.Err())
	}

	assignWorkloadUIDs(t, env)

	if err := env.Client.Delete(t.Context(), cache); err != nil {
		t.Fatal(err)
	}

	plan := combinedPlan(t, env, site)
	deletes := 0

	for _, op := range plan.Operations {
		if op.Kind == component.OpDelete {
			deletes++
		}
	}

	if deletes != 0 {
		t.Fatal("last cache deletion deleted retained installation")
	}

	if result := execute(t, env, plan); result.Err() != nil {
		t.Fatal(result.Err())
	}

	for _, op := range combinedPlan(t, env, site).Operations {
		if op.Kind == component.OpDelete {
			t.Fatal("repeated delete on absent dataplane")
		}
	}
	// A deleted Site is no longer an input. Shared resources still converge.
	retained := combinedPlan(t, env)
	if retained.Len() == 0 {
		t.Fatal("singleton lost after Site deletion")
	}

	for _, op := range retained.Operations {
		if (op.Kind != component.OpApply && op.Kind != component.OpApplyExisting) || len(op.Object.GetOwnerReferences()) != 0 {
			t.Fatal("retained resources must be ownerless and applied")
		}
	}
	// Removing one workload never causes its sibling to reinstall it.
	if err := env.Client.Delete(t.Context(), controlDeployment(env.Namespace, env.Config)); err != nil {
		t.Fatal(err)
	}

	if result := execute(t, env, combinedPlan(t, env)); result.Err() != nil {
		t.Fatal(result.Err())
	}

	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: env.Namespace, Name: controlPlaneName}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatal("deleted deployment was recreated", err)
	}
}

func TestSingletonRetentionMarkers(t *testing.T) {
	for _, marker := range []client.Object{serviceAccount(controlPlaneName, "custom"), controlDeployment("custom", component.Config{}), dataplaneDaemonSet("custom", component.Config{})} {
		t.Run(reflect.TypeOf(marker).String(), func(t *testing.T) {
			env := testEnv(t, interceptor.Funcs{}, marker)
			for _, sites := range [][]*unboundedv1alpha3.Site{nil, {testSite("a"), testSite("b")}} {
				plan := combinedPlan(t, env, sites...)
				workloads := map[string]int{}

				for _, op := range plan.Operations {
					if op.Site != "" || len(op.Object.GetOwnerReferences()) != 0 {
						t.Fatal("singleton operation has Site scope or owner")
					}

					if op.Overridable {
						workloads[op.Object.GetName()]++
					}
				}

				want := map[string]int{}
				if marker.GetName() == dataplaneName || reflect.TypeOf(marker) == reflect.TypeOf(&appsv1.Deployment{}) {
					want[marker.GetName()] = 1
				}

				if !reflect.DeepEqual(workloads, want) {
					t.Fatalf("workloads: %v", workloads)
				}

				if len(want) == 0 && plan.Len() != 0 {
					t.Fatal("support marker reinstalled resources")
				}
			}
		})
	}
}

func TestNoOpWritesDriftAndControllerOwnedState(t *testing.T) {
	writes := 0
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "racer-ca", Namespace: "custom"}, Data: map[string][]byte{"private": []byte("keep")}}
	state := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-state", Namespace: "custom"}, Data: map[string]string{"manifest": "keep"}}
	trust := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-trust", Namespace: "custom"}, Data: map[string]string{"bundle.json": "keep"}}
	env := testEnv(t, interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
		writes++
		return c.Apply(ctx, obj, opts...)
	}}, secret, state, trust, testCache())
	site := testSite("rack-a")
	first := combinedPlan(t, env, site)

	second := combinedPlan(t, env, site)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("constructors are not deterministic")
	}

	if result := execute(t, env, first); result.Err() != nil {
		t.Fatal(result.Err())
	}

	if writes != first.Len() {
		t.Fatalf("writes=%d plan=%d", writes, first.Len())
	}
	// Simulate server-owned metadata/status/defaults, which must not invalidate
	// the exact desired payload hash or cause another SSA write.
	for _, op := range first.Operations {
		current := op.Object.DeepCopy()
		if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(current), current); err != nil {
			t.Fatal(err)
		}

		current.SetCreationTimestamp(metav1.Now())

		if err := env.Client.Update(t.Context(), current); err != nil {
			t.Fatal(err)
		}
	}

	writes = 0

	if result := execute(t, env, combinedPlan(t, env, site)); result.Err() != nil {
		t.Fatal(result.Err())
	}

	if writes != 0 {
		t.Fatalf("steady state made %d writes", writes)
	}

	var ds appsv1.DaemonSet

	key := client.ObjectKey{Namespace: env.Namespace, Name: dataplaneName}
	if err := env.Client.Get(t.Context(), key, &ds); err != nil {
		t.Fatal(err)
	}

	ds.Spec.Template.Spec.Containers[0].Image = "drifted"
	delete(ds.Labels, componentLabel)

	if err := env.Client.Update(t.Context(), &ds); err != nil {
		t.Fatal(err)
	}

	if result := execute(t, env, combinedPlan(t, env, site)); result.Err() != nil {
		t.Fatal(result.Err())
	}

	if writes != 1 {
		t.Fatalf("drift should repair one object, wrote %d", writes)
	}

	if err := env.Client.Get(t.Context(), key, &ds); err != nil {
		t.Fatal(err)
	}

	if ds.Spec.Template.Spec.Containers[0].Image != env.Config.Image(dataplaneName) || ds.Labels[componentLabel] != dataplaneName {
		t.Fatal("drift survived despite unchanged applied hash")
	}

	var (
		gotSecret corev1.Secret
		gotState  corev1.ConfigMap
		gotTrust  corev1.ConfigMap
	)

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(secret), &gotSecret); err != nil {
		t.Fatal(err)
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(state), &gotState); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(gotSecret.Data, secret.Data) || !reflect.DeepEqual(gotState.Data, state.Data) || len(gotSecret.OwnerReferences) != 0 || len(gotState.OwnerReferences) != 0 {
		t.Fatal("operator changed controller-owned durable state")
	}

	if err := env.Client.Get(t.Context(), client.ObjectKeyFromObject(trust), &gotTrust); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(gotTrust.Data, trust.Data) || len(gotTrust.OwnerReferences) != 0 {
		t.Fatal("operator changed controller-owned public trust")
	}
}

func TestDependencyFailureAndConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "conflict"}[conflict], func(t *testing.T) {
			env := testEnv(t, interceptor.Funcs{Apply: func(ctx context.Context, c client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				u, ok := obj.(interface {
					GetKind() string
					GetName() string
				})
				if !ok {
					t.Fatalf("unexpected apply type %T", obj)
				}

				if u.GetKind() == "Role" && u.GetName() == bootstrapControllerRoleName {
					if conflict {
						return apierrors.NewConflict(schema.GroupResource{Group: rbacv1.GroupName, Resource: "roles"}, u.GetName(), errors.New("raced"))
					}

					return errors.New("RBAC denied")
				}

				return c.Apply(ctx, obj, opts...)
			}}, testCache())
			result := execute(t, env, combinedPlan(t, env, testSite("rack-a"), testSite("rack-b")))
			blocked := 0

			for _, r := range result.Results {
				if r.Ref.GVK.Kind != "Deployment" && r.Ref.GVK.Kind != "DaemonSet" {
					continue
				}

				want := component.OpSkipped
				if conflict {
					want = component.OpDeferred
				}

				if r.Status != want {
					t.Fatalf("dependency bypass: %s=%s", r.Ref, r.Status)
				}

				blocked++
			}

			if blocked != 2 {
				t.Fatalf("expected both dependent workloads, got %d", blocked)
			}
		})
	}
}

func TestWatchesIntentNotStatus(t *testing.T) {
	match := func(o client.Object) bool { return o.GetNamespace() == "custom" }
	p := managedPredicate(match)
	base := dataplaneDaemonSet("custom", component.Config{})
	base.UID = "original-workload-uid"

	for _, tc := range []struct {
		name   string
		mutate func(*appsv1.DaemonSet)
		want   bool
	}{
		{"identical relist", func(*appsv1.DaemonSet) {}, false},
		{"UID-only recreation", func(d *appsv1.DaemonSet) { d.UID = "replacement-workload-uid" }, true},
		{"resource version only", func(d *appsv1.DaemonSet) { d.ResourceVersion = "new" }, false},
		{"status", func(d *appsv1.DaemonSet) { d.Status.NumberReady = 1 }, false},
		{"foreign annotation", func(d *appsv1.DaemonSet) {
			d.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "x"}
		}, false},
		{"spec", func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.Containers[0].Image = "drift" }, true},
		{"racer label", func(d *appsv1.DaemonSet) { delete(d.Labels, componentLabel) }, true},
		{"racer annotation", func(d *appsv1.DaemonSet) { d.Annotations = map[string]string{racermeta.CacheStatusAnnotationKey: "x"} }, true},
		{"applied hash", func(d *appsv1.DaemonSet) { d.Labels[component.AppliedHashLabel] = "x" }, true},
		{"owner", func(d *appsv1.DaemonSet) {
			d.OwnerReferences = []metav1.OwnerReference{component.SiteOwnerReference(testSite("foreign"))}
		}, true},
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
		t.Fatal("bad event boundaries")
	}

	unmanaged := base.DeepCopy()
	unmanaged.Namespace = "unmanaged"
	replacement := unmanaged.DeepCopy()

	replacement.UID = "replacement-workload-uid"
	if p.Update(event.UpdateEvent{ObjectOld: unmanaged, ObjectNew: replacement}) {
		t.Fatal("identity changes must still respect the managed-object filter")
	}

	role := sharedResources("custom")[3].(*rbacv1.Role)
	changed := role.DeepCopy()

	changed.Rules = nil
	if !p.Update(event.UpdateEvent{ObjectOld: role, ObjectNew: changed}) {
		t.Fatal("RBAC drift ignored")
	}

	dep := controlDeployment("custom", component.Config{})
	ready := dep.DeepCopy()

	ready.Status.ReadyReplicas = 1
	if p.Update(event.UpdateEvent{ObjectOld: dep, ObjectNew: ready}) {
		t.Fatal("unused readiness status must not trigger passes")
	}
}

func TestOverridesKeepSiteAndExclusionAffinity(t *testing.T) {
	env := testEnv(t, interceptor.Funcs{}, testCache())
	site := testSite("rack-a")
	plan := combinedPlan(t, env, site)

	entries, problems, err := override.Parse(map[string]string{"racer.yaml": `apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
- component: racer-dataplane
  kind: DaemonSet
  patch:
    spec:
      template:
        spec:
          affinity:
            nodeAffinity:
              requiredDuringSchedulingIgnoredDuringExecution:
                nodeSelectorTerms:
                - matchExpressions:
                  - key: disk
                    operator: In
                    values: [fast]
          containers:
          - name: dataplane
            image: custom.test/racer:pinned
`})
	if err != nil || len(problems) != 0 {
		t.Fatalf("parse: %v %v", err, problems)
	}

	if err := override.ValidateErr(entries); err != nil {
		t.Fatal(err)
	}

	report := override.Apply(plan, entries, []string{site.Name})
	if report.Err() != nil || len(report.Workloads) != 1 || report.Workloads[0].VersionDrift == "" {
		t.Fatalf("override: %+v", report)
	}

	var ds appsv1.DaemonSet

	for _, op := range plan.Operations {
		if op.Object.GetKind() == "DaemonSet" {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, &ds); err != nil {
				t.Fatal(err)
			}
		}
	}

	for _, tc := range []struct {
		site, disk, exclude string
		want                bool
	}{{"rack-a", "fast", "", true}, {"rack-b", "fast", "", true}, {"rack-a", "slow", "", false}, {"rack-a", "fast", "true", false}} {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{corev1.LabelOSStable: "linux", racermeta.SiteLabelKey: tc.site, "disk": tc.disk, racermeta.ExcludeLabelKey: tc.exclude}}}
		if got := matchesNode(t, ds.Spec.Template.Spec, n); got != tc.want {
			t.Fatalf("override widened membership: %+v", tc)
		}
	}

	if ds.Spec.Template.Spec.Containers[0].Image != "custom.test/racer:pinned" || len(ds.OwnerReferences) != 0 || ds.Spec.Template.Labels[racermeta.UniverseKey] != "" {
		t.Fatal("override lost workload identity")
	}
}

func TestOverrideEmptyAffinityTermCannotEnrollNodes(t *testing.T) {
	env := testEnv(t, interceptor.Funcs{}, testCache())
	site := testSite("rack-a")
	plan := combinedPlan(t, env, site)

	entries, problems, err := override.Parse(map[string]string{"empty.yaml": `apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
- component: racer-dataplane
  kind: DaemonSet
  patch:
    spec:
      template:
        spec:
          affinity:
            nodeAffinity:
              requiredDuringSchedulingIgnoredDuringExecution:
                nodeSelectorTerms:
                - {}
`})
	if err != nil || len(problems) != 0 {
		t.Fatalf("parse: %v %v", err, problems)
	}

	if err := override.ValidateErr(entries); err != nil {
		t.Fatal(err)
	}

	report := override.Apply(plan, entries, []string{site.Name})
	if report.Err() != nil || len(report.Workloads) != 1 {
		t.Fatalf("override: %+v", report)
	}

	for _, op := range plan.Operations {
		if op.Object.GetKind() != "DaemonSet" {
			continue
		}

		var ds appsv1.DaemonSet
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, &ds); err != nil {
			t.Fatal(err)
		}

		for _, key := range []string{racermeta.SiteLabelKey, racermeta.DeprecatedSiteLabelKey} {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{key: site.Name, corev1.LabelOSStable: "linux"}}}
			if matchesNode(t, ds.Spec.Template.Spec, node) {
				t.Fatalf("match-nothing override enrolled %s=%s", key, site.Name)
			}
		}
	}
}
