// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
)

// overridableCluster is a ClusterComponent that plans one overridable workload
// alongside one object overrides can never target, so tests can tell "workloads
// were skipped" from "the whole pass was skipped".
type overridableCluster struct{}

func (overridableCluster) Name() string          { return "net" }
func (overridableCluster) ConditionType() string { return "NetReady" }

func (c overridableCluster) Plan(context.Context, *component.Env, []unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	workload := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": "unbounded-net-node", "namespace": component.DefaultNamespace},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "node"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "node"}},
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":  "node",
							"image": "ghcr.io/azure/unbounded-net-node:v1",
							"args":  []any{"--config-file=/etc/unbounded-net/config.yaml"},
						},
					},
				},
			},
		},
	}}

	rbac := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata":   map[string]any{"name": "unbounded-net-node"},
	}}

	plan := component.NewPlan()
	plan.Add(
		component.Operation{Kind: component.OpApply, Object: rbac, Component: c.Name()},
		component.Operation{Kind: component.OpApply, Object: workload, Component: c.Name(), Overridable: true},
	)

	return plan, component.Reconciled(), nil
}

// overrideTestEnv builds a reconciler whose applies are recorded, so tests can
// assert exactly which objects reached the cluster.
func overrideTestEnv(t *testing.T, objects ...client.Object) (*SiteReconciler, *[]string, client.Client) {
	t.Helper()

	scheme := newReconcilerTestScheme(t)

	var applied []string

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&unboundedv1alpha3.Site{}).
		WithInterceptorFuncs(interceptor.Funcs{
			// Record what was applied, then let the write through so tests can
			// read back the object the operator actually produced.
			Apply: func(ctx context.Context, inner client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				named, ok := obj.(interface {
					GetKind() string
					GetName() string
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				applied = append(applied, named.GetKind()+"/"+named.GetName())

				return inner.Apply(ctx, obj, opts...)
			},
		}).
		Build()

	return &SiteReconciler{
		Client: cl,
		Scheme: scheme,
		Registry: &component.Registry{
			Cluster: []component.ClusterComponent{overridableCluster{}},
		},
	}, &applied, cl
}

// appliedDaemonSet reads back the workload the operator wrote.
func appliedDaemonSet(t *testing.T, cl client.Client, name string) *appsv1.DaemonSet {
	t.Helper()

	var got appsv1.DaemonSet
	if err := cl.Get(t.Context(), client.ObjectKey{Namespace: component.DefaultNamespace, Name: name}, &got); err != nil {
		t.Fatalf("read back %s: %v", name, err)
	}

	return &got
}

func overridesConfigMap(data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       component.DefaultNamespace,
			Name:            override.ConfigMapName,
			ResourceVersion: "42",
		},
		Data: data,
	}
}

func singletonRequest() ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Name: component.SingletonRequestName}}
}

func appliedContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}

	return false
}

// TestOverridesAbsentAppliesVanilla covers the state that must not be conflated
// with a broken document: removing overrides is a deliberate request for
// defaults.
func TestOverridesAbsentAppliesVanilla(t *testing.T) {
	r, applied, cl := overrideTestEnv(t)

	if _, err := r.Reconcile(t.Context(), singletonRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !appliedContains(*applied, "DaemonSet/unbounded-net-node") {
		t.Fatalf("applied = %v, want the workload applied with defaults", *applied)
	}

	if got := appliedDaemonSet(t, cl, "unbounded-net-node").Annotations[override.HashAnnotation]; got != "" {
		t.Fatalf("override hash = %q, want none when no overrides exist", got)
	}
}

// TestOverridesValidAreMerged is the end-to-end happy path through the pass.
func TestOverridesValidAreMerged(t *testing.T) {
	r, applied, cl := overrideTestEnv(t, overridesConfigMap(map[string]string{
		"overrides.yaml": `apiVersion: ` + override.APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    extraArgs:
      node: ["--verbose"]
`,
	}))

	if _, err := r.Reconcile(t.Context(), singletonRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if !appliedContains(*applied, "DaemonSet/unbounded-net-node") {
		t.Fatalf("applied = %v, want the workload applied", *applied)
	}

	workload := appliedDaemonSet(t, cl, "unbounded-net-node")

	if workload.Annotations[override.HashAnnotation] == "" {
		t.Fatal("an overridden workload must carry its override hash")
	}

	args := workload.Spec.Template.Spec.Containers[0].Args
	if len(args) != 2 || args[1] != "--verbose" {
		t.Fatalf("args = %v, want the operator's plus --verbose", args)
	}
}

// TestOverridesInvalidSkipWorkloadsButNotThePass is the core of the failure
// model.
//
// Applying vanilla manifests on invalid input is not a safe fallback: defaults
// are not the current state, so falling back rewrites running infrastructure. A
// single mis-indented line would strip resources, tolerations and pinned images
// from every component at once. Everything an override cannot target must still
// reconcile, or an override typo stops the operator doing its other work.
func TestOverridesInvalidSkipWorkloadsButNotThePass(t *testing.T) {
	r, applied, _ := overrideTestEnv(t, overridesConfigMap(map[string]string{
		"overrides.yaml": "apiVersion: " + override.APIVersion + "\noverrides:\n  - component: net\n    kind: DaemonSet\n    patch:\n      metadata:\n        name: renamed\n",
	}))

	_, err := r.Reconcile(t.Context(), singletonRequest())
	if err == nil {
		t.Fatal("an invalid override document must fail the pass so it requeues")
	}

	if !strings.Contains(err.Error(), "overrides unusable") {
		t.Fatalf("error = %v, want it to say overrides were unusable", err)
	}

	if appliedContains(*applied, "DaemonSet/unbounded-net-node") {
		t.Fatal("the workload must be skipped, not applied without its overrides")
	}

	if !appliedContains(*applied, "ClusterRole/unbounded-net-node") {
		t.Fatalf("applied = %v, want non-overridable objects to still reconcile", *applied)
	}
}

// TestOverridesUnreadableSkipWorkloads treats a failed read like a broken
// document, because guessing wrong in the other direction reverts running
// infrastructure.
func TestOverridesUnreadableSkipWorkloads(t *testing.T) {
	scheme := newReconcilerTestScheme(t)

	var applied []string

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if key.Name == override.ConfigMapName {
					return apierrors.NewInternalError(errors.New("etcd is unhappy"))
				}

				return cl.Get(ctx, key, obj, opts...)
			},
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				named, ok := obj.(interface {
					GetKind() string
					GetName() string
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				applied = append(applied, named.GetKind()+"/"+named.GetName())

				return nil
			},
		}).
		Build()

	r := &SiteReconciler{
		Client:   cl,
		Scheme:   scheme,
		Registry: &component.Registry{Cluster: []component.ClusterComponent{overridableCluster{}}},
	}

	_, err := r.Reconcile(t.Context(), singletonRequest())
	if err == nil {
		t.Fatal("an unreadable overrides ConfigMap must fail the pass so it requeues")
	}

	if appliedContains(applied, "DaemonSet/unbounded-net-node") {
		t.Fatal("a failed read must not apply the workload without its overrides")
	}

	if !appliedContains(applied, "ClusterRole/unbounded-net-node") {
		t.Fatalf("applied = %v, want non-overridable objects to still reconcile", applied)
	}
}

// TestOverridesRestartIsSafe is the regression test for the destructive path an
// earlier design would have taken.
//
// The operator runs replicas 1 with Recreate, so it restarts on every upgrade.
// A design caching last-known-good in memory would lose it there, and the next
// pass would strip every override and roll every workload. Because the model
// holds no in-memory state, a fresh reconciler behaves identically.
func TestOverridesRestartIsSafe(t *testing.T) {
	data := map[string]string{
		"overrides.yaml": "apiVersion: " + override.APIVersion + "\noverrides:\n  - component: net\n    kind: DaemonSet\n    patch:\n      metadata:\n        name: renamed\n",
	}

	for _, pass := range []string{"first", "after restart"} {
		t.Run(pass, func(t *testing.T) {
			// A brand new reconciler stands in for a restarted operator.
			r, applied, _ := overrideTestEnv(t, overridesConfigMap(data))

			if _, err := r.Reconcile(t.Context(), singletonRequest()); err == nil {
				t.Fatal("expected the invalid document to fail")
			}

			if appliedContains(*applied, "DaemonSet/unbounded-net-node") {
				t.Fatal("a restart must not cause the workload to be written without its overrides")
			}
		})
	}
}

// TestOverridesUnmatchedSiteIsInert covers a document written before its Site
// exists, which must not fail.
func TestOverridesUnmatchedSiteIsInert(t *testing.T) {
	r, applied, _ := overrideTestEnv(t, overridesConfigMap(map[string]string{
		"overrides.yaml": `apiVersion: ` + override.APIVersion + `
overrides:
  - component: storage
    kind: DaemonSet
    sites: [not-yet-created]
    extraArgs:
      run: ["--x"]
`,
	}))

	if _, err := r.Reconcile(t.Context(), singletonRequest()); err != nil {
		t.Fatalf("an override naming a Site that does not exist must be inert: %v", err)
	}

	if !appliedContains(*applied, "DaemonSet/unbounded-net-node") {
		t.Fatalf("applied = %v, want unrelated workloads to reconcile normally", *applied)
	}
}

// TestOverrideSnapshotStates covers the loader directly, including that the
// resourceVersion a pass acted on is recorded.
func TestOverrideSnapshotStates(t *testing.T) {
	scheme := newReconcilerTestScheme(t)

	t.Run("absent", func(t *testing.T) {
		env := &component.Env{
			Client:    fake.NewClientBuilder().WithScheme(scheme).Build(),
			Namespace: component.DefaultNamespace,
		}

		snapshot := loadOverrides(t.Context(), env)
		if snapshot.state != overridesAbsent || snapshot.blocksWorkloads() {
			t.Fatalf("snapshot = %+v, want absent and non-blocking", snapshot)
		}
	})

	t.Run("valid records resourceVersion", func(t *testing.T) {
		env := &component.Env{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(overridesConfigMap(map[string]string{
				"a.yaml": "apiVersion: " + override.APIVersion + "\noverrides:\n  - component: net\n    kind: DaemonSet\n    extraArgs:\n      node: [--x]\n",
			})).Build(),
			Namespace: component.DefaultNamespace,
		}

		snapshot := loadOverrides(t.Context(), env)
		if !snapshot.usable() {
			t.Fatalf("snapshot = %+v, want usable", snapshot)
		}

		if snapshot.resourceVersion == "" {
			t.Fatal("a pass must record the resourceVersion it acted on")
		}
	})

	t.Run("invalid blocks workloads", func(t *testing.T) {
		env := &component.Env{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(overridesConfigMap(map[string]string{
				"a.yaml": "overrides: []\n",
			})).Build(),
			Namespace: component.DefaultNamespace,
		}

		snapshot := loadOverrides(t.Context(), env)
		if snapshot.usable() || !snapshot.blocksWorkloads() {
			t.Fatalf("snapshot = %+v, want invalid and blocking", snapshot)
		}
	})
}

// TestDropOverridableOperationsKeepsEverythingElse pins the blast radius of an
// unusable document.
func TestDropOverridableOperationsKeepsEverythingElse(t *testing.T) {
	plan := component.NewPlan()
	plan.Add(
		component.Operation{Kind: component.OpApply, Object: unstructuredOf("v1", "ConfigMap", "cfg"), Component: "net"},
		component.Operation{Kind: component.OpApply, Object: unstructuredOf("apps/v1", "DaemonSet", "node"), Component: "net", Overridable: true},
		component.Operation{Kind: component.OpDelete, Object: unstructuredOf("apps/v1", "DaemonSet", "legacy"), Component: "gantry"},
	)

	skipped := dropOverridableOperations(plan)

	if len(skipped) != 1 {
		t.Fatalf("skipped = %d, want 1", len(skipped))
	}

	if len(plan.Operations) != 2 {
		t.Fatalf("remaining = %d, want the ConfigMap apply and the legacy delete", len(plan.Operations))
	}

	for _, op := range plan.Operations {
		if op.Overridable {
			t.Fatal("an overridable operation survived")
		}
	}
}

func unstructuredOf(apiVersion, kind, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": component.DefaultNamespace},
	}}
}

// siteFor builds a Site the reconciler can publish status onto.
func siteFor(name string) *unboundedv1alpha3.Site {
	return &unboundedv1alpha3.Site{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

// TestOverrideStatusReportsApplied covers the observability contract: a Site
// carries the desired and applied hash per workload, and they match when the
// override is in effect.
func TestOverrideStatusReportsApplied(t *testing.T) {
	site := siteFor("edge")

	r, _, cl := overrideTestEnv(t, site, overridesConfigMap(map[string]string{
		"overrides.yaml": `apiVersion: ` + override.APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    extraArgs:
      node: ["--verbose"]
`,
	}))

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "edge"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got unboundedv1alpha3.Site
	if err := cl.Get(t.Context(), client.ObjectKey{Name: "edge"}, &got); err != nil {
		t.Fatalf("get site: %v", err)
	}

	status := got.Status.Overrides
	if status == nil {
		t.Fatal("Site carries no override status")
	}

	if status.Phase != unboundedv1alpha3.OverridePhaseApplied {
		t.Fatalf("phase = %q, want Applied (message=%q)", status.Phase, status.Message)
	}

	if status.ObservedResourceVersion == "" {
		t.Fatal("status must record the resourceVersion it was computed from")
	}

	if len(status.Workloads) != 1 {
		t.Fatalf("workloads = %+v, want one", status.Workloads)
	}

	workload := status.Workloads[0]
	if workload.DesiredHash == "" || workload.DesiredHash != workload.AppliedHash {
		t.Fatalf("desired = %q applied = %q, want equal and non-empty", workload.DesiredHash, workload.AppliedHash)
	}
}

// TestOverrideStatusReportsDegraded covers the other half: an unusable document
// is visible on the Site rather than only in logs.
func TestOverrideStatusReportsDegraded(t *testing.T) {
	site := siteFor("edge")

	r, _, cl := overrideTestEnv(t, site, overridesConfigMap(map[string]string{
		"overrides.yaml": "apiVersion: " + override.APIVersion + "\noverrides:\n  - component: net\n    kind: DaemonSet\n    patch:\n      metadata:\n        name: renamed\n",
	}))

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "edge"}}); err == nil {
		t.Fatal("expected the invalid document to fail the pass")
	}

	var got unboundedv1alpha3.Site
	if err := cl.Get(t.Context(), client.ObjectKey{Name: "edge"}, &got); err != nil {
		t.Fatalf("get site: %v", err)
	}

	status := got.Status.Overrides
	if status == nil || status.Phase != unboundedv1alpha3.OverridePhaseDegraded {
		t.Fatalf("status = %+v, want Degraded", status)
	}

	if status.Message == "" {
		t.Fatal("a degraded status must explain itself")
	}
}

// TestOverrideStatusHashesAreComparable is a regression test for a specific
// defect: pairing one Site-wide desired hash against many per-workload applied
// hashes made them differ whenever a document targeted more than one workload,
// so the divergence signal was permanently on.
func TestOverrideStatusHashesAreComparable(t *testing.T) {
	status := overrideStatusFor("edge",
		overrideSnapshot{state: overridesValid, resourceVersion: "7"},
		&override.Report{Workloads: []override.WorkloadResult{
			{Ref: component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "a")), Site: "edge", Hash: "h1"},
			{Ref: component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "b")), Site: "edge", Hash: "h2"},
		}},
		planWithApplied(map[string]string{"a": "h1", "b": "h2"}),
	)

	if status.Phase != unboundedv1alpha3.OverridePhaseApplied {
		t.Fatalf("phase = %q, want Applied; per-workload hashes must be comparable", status.Phase)
	}

	for _, workload := range status.Workloads {
		if workload.DesiredHash != workload.AppliedHash {
			t.Fatalf("workload %s: desired %q != applied %q", workload.Name, workload.DesiredHash, workload.AppliedHash)
		}
	}
}

// TestOverrideStatusDetectsDivergence confirms the signal is not merely always
// off either: a workload that did not receive its override is Degraded.
func TestOverrideStatusDetectsDivergence(t *testing.T) {
	status := overrideStatusFor("edge",
		overrideSnapshot{state: overridesValid, resourceVersion: "7"},
		&override.Report{Workloads: []override.WorkloadResult{
			{Ref: component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "a")), Site: "edge", Hash: "desired"},
		}},
		planWithApplied(map[string]string{"a": "stale"}),
	)

	if status.Phase != unboundedv1alpha3.OverridePhaseDegraded {
		t.Fatalf("phase = %q, want Degraded when applied differs from desired", status.Phase)
	}
}

// planWithApplied builds a plan whose workloads carry the given hashes.
func planWithApplied(hashes map[string]string) *component.Plan {
	plan := component.NewPlan()

	for name, hash := range hashes {
		obj := unstructuredOf("apps/v1", "DaemonSet", name)
		obj.SetAnnotations(map[string]string{override.HashAnnotation: hash})

		plan.Add(component.Operation{Kind: component.OpApply, Object: obj, Component: "net", Site: "edge", Overridable: true})
	}

	return plan
}

// perSiteCluster is a SiteComponent that plans one overridable per-Site
// workload, so fan-out can be observed reaching it.
type perSiteStorage struct{}

func (perSiteStorage) Name() string                         { return "storage" }
func (perSiteStorage) ConditionType() string                { return "StorageReady" }
func (perSiteStorage) Enabled(*unboundedv1alpha3.Site) bool { return true }

func (c perSiteStorage) Plan(_ context.Context, _ *component.Env, site *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	workload := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata":   map[string]any{"name": "unbounded-storage-supervisor-" + site.Name, "namespace": component.DefaultNamespace},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": "storage"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "storage"}},
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": "run", "image": "storage:v1", "args": []any{"--config"}},
					},
				},
			},
		},
	}}

	plan := component.NewPlan()
	plan.Add(component.Operation{
		Kind:        component.OpApply,
		Object:      workload,
		Component:   c.Name(),
		Site:        site.Name,
		Overridable: true,
	})

	return plan, component.Reconciled(), nil
}

func (perSiteStorage) CleanupPlan(context.Context, *component.Env, *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	return component.NewPlan(), component.Disabled("component disabled"), nil
}

// TestOverridesReachPerSiteComponentsViaFanOut is the end-to-end proof that the
// fan-out does its job.
//
// The overrides ConfigMap watch enqueues only the singleton request. Without
// the Site-less pass reconciling every Site, an override targeting storage or
// metalman would sit in the ConfigMap and never take effect.
func TestOverridesReachPerSiteComponentsViaFanOut(t *testing.T) {
	scheme := newReconcilerTestScheme(t)

	alpha := siteFor("alpha")
	bravo := siteFor("bravo")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(alpha, bravo, overridesConfigMap(map[string]string{
			"overrides.yaml": `apiVersion: ` + override.APIVersion + `
overrides:
  - component: storage
    kind: DaemonSet
    sites: [bravo]
    extraArgs:
      run: ["--only-bravo"]
`,
		})).
		WithStatusSubresource(alpha, bravo).
		Build()

	r := &SiteReconciler{
		Client:   cl,
		Scheme:   scheme,
		Registry: &component.Registry{Site: []component.SiteComponent{perSiteStorage{}}},
	}

	// A singleton request, exactly what the ConfigMap watch enqueues.
	if _, err := r.Reconcile(t.Context(), singletonRequest()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// bravo received the override.
	got := appliedDaemonSet(t, cl, "unbounded-storage-supervisor-bravo")
	if args := got.Spec.Template.Spec.Containers[0].Args; len(args) != 2 || args[1] != "--only-bravo" {
		t.Fatalf("bravo args = %v, want the override appended", args)
	}

	// alpha was reconciled by the same pass but not selected, so it keeps the
	// operator's arguments untouched.
	untouched := appliedDaemonSet(t, cl, "unbounded-storage-supervisor-alpha")
	if args := untouched.Spec.Template.Spec.Containers[0].Args; len(args) != 1 {
		t.Fatalf("alpha args = %v, want the operator's only", args)
	}

	if untouched.Annotations[override.HashAnnotation] != "" {
		t.Fatal("an unselected Site's workload must carry no override hash")
	}
}
