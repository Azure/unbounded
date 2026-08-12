// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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

// overridableSite is a SiteComponent that plans one overridable per-Site
// workload, so tests can assert that a failure naming one component leaves the
// others reconciling.
type overridableSite struct{}

func (overridableSite) Name() string          { return "metalman" }
func (overridableSite) ConditionType() string { return "MetalmanReady" }

func (overridableSite) Enabled(site *unboundedv1alpha3.Site) bool {
	return site.Spec.Components.Metalman != nil &&
		unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.Metalman.SiteComponentSpec)
}

func (c overridableSite) Plan(_ context.Context, env *component.Env, site *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	workload := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "metalman-controller-" + site.Name, "namespace": env.Namespace},
		"spec": map[string]any{
			"replicas": int64(1),
			"selector": map[string]any{"matchLabels": map[string]any{"app": "metalman"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "metalman"}},
				"spec": map[string]any{
					"containers": []any{
						map[string]any{
							"name":  "metalman",
							"image": "ghcr.io/azure/metalman:v1",
							"args":  []any{"serve-pxe", "--site=" + site.Name},
						},
					},
				},
			},
		},
	}}

	plan := component.NewPlan()
	plan.Add(component.Operation{
		Kind: component.OpApply, Object: workload,
		Component: c.Name(), Site: site.Name, Overridable: true,
	})

	return plan, component.Reconciled(), nil
}

func (c overridableSite) CleanupPlan(context.Context, *component.Env, *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	return component.NewPlan(), component.Disabled("component disabled"), nil
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
			Site:    []component.SiteComponent{overridableSite{}},
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
		if snapshot.state != overridesAbsent || !snapshot.quarantine().empty() {
			t.Fatalf("snapshot = %+v, want absent and withholding nothing", snapshot)
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

	t.Run("a key that does not parse withholds everything", func(t *testing.T) {
		env := &component.Env{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(overridesConfigMap(map[string]string{
				"a.yaml": "overrides: []\n",
			})).Build(),
			Namespace: component.DefaultNamespace,
		}

		snapshot := loadOverrides(t.Context(), env)
		if snapshot.usable() {
			t.Fatalf("snapshot = %+v, want unusable", snapshot)
		}

		// The key held no readable entries, so there is no way to know what it
		// would have changed and every overridable workload is in doubt.
		if q := snapshot.quarantine(); !q.all {
			t.Fatalf("quarantine = %+v, want everything withheld", q)
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

	withheld := dropOverridableOperations(plan, overrideQuarantine{
		all:   true,
		cause: errors.New("document is unusable"),
	})

	if len(withheld) != 1 {
		t.Fatalf("withheld = %d, want 1", len(withheld))
	}

	// The component and Site travel with it, or the status path cannot
	// attribute the withheld workload to anything.
	if withheld[0].Component != "net" {
		t.Fatalf("withheld component = %q, want net", withheld[0].Component)
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
	plan := planWithApplied(map[string]string{"a": "h1", "b": "h2"})

	status := overrideStatusFor("edge",
		overrideSnapshot{state: overridesValid, resourceVersion: "7"},
		&override.Report{Workloads: []override.WorkloadResult{
			{Ref: component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "a")), Site: "edge", Hash: "h1"},
			{Ref: component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "b")), Site: "edge", Hash: "h2"},
		}},
		plan,
		allSucceeded(plan),
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
	plan := planWithApplied(map[string]string{"a": "stale"})

	status := overrideStatusFor("edge",
		overrideSnapshot{state: overridesValid, resourceVersion: "7"},
		&override.Report{Workloads: []override.WorkloadResult{
			{Ref: component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "a")), Site: "edge", Hash: "desired"},
		}},
		plan,
		allSucceeded(plan),
	)

	if status.Phase != unboundedv1alpha3.OverridePhaseDegraded {
		t.Fatalf("phase = %q, want Degraded when applied differs from desired", status.Phase)
	}
}

// allSucceeded is the execution result of a plan that ran cleanly, which is
// what most status tests want to hold constant while they vary the hashes.
func allSucceeded(plan *component.Plan) component.ExecutionResult {
	var exec component.ExecutionResult

	for _, op := range plan.Operations {
		exec.Results = append(exec.Results, component.OperationResult{
			Ref:       op.Ref(),
			Kind:      op.Kind,
			Component: op.Component,
			Site:      op.Site,
			Status:    component.OpSucceeded,
		})
	}

	return exec
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

// TestOverrideStatusReportsWhatWasWrittenNotWhatWasPlanned is a regression test.
//
// Applied hashes were read from the plan alone, so they described intent. A
// workload whose apply the API server rejected, or that the executor skipped
// because something it depends on failed, still had its desired hash reported
// as applied: the Site said Applied for an override that had never reached the
// cluster, which is the one thing this status exists to catch.
func TestOverrideStatusReportsWhatWasWrittenNotWhatWasPlanned(t *testing.T) {
	plan := planWithApplied(map[string]string{"a": "desired"})

	report := &override.Report{Workloads: []override.WorkloadResult{
		{Ref: component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "a")), Site: "edge", Hash: "desired"},
	}}

	for name, exec := range map[string]component.ExecutionResult{
		"the write failed": {Results: []component.OperationResult{{
			Ref:    component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "a")),
			Kind:   component.OpApply,
			Status: component.OpFailed,
			Err:    errors.New("apiserver said no"),
		}}},
		"the write was skipped": {Results: []component.OperationResult{{
			Ref:    component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "a")),
			Kind:   component.OpApply,
			Status: component.OpSkipped,
			Err:    errors.New("dependency did not complete"),
		}}},
		"the plan never executed": {},
	} {
		t.Run(name, func(t *testing.T) {
			status := overrideStatusFor("edge",
				overrideSnapshot{state: overridesValid, resourceVersion: "7"}, report, plan, exec)

			if status.Phase != unboundedv1alpha3.OverridePhaseDegraded {
				t.Fatalf("phase = %q, want Degraded: the override did not reach the cluster", status.Phase)
			}

			if got := status.Workloads[0].AppliedHash; got != "" {
				t.Fatalf("applied hash = %q, want empty for a workload that was never written", got)
			}
		})
	}
}

// TestOverrideConfigMapEventsAreRecordedOnTheConfigMap is a regression test.
//
// A rejected document was reported only on Site conditions and in the operator
// log. The overrides ConfigMap is cluster-scoped and one document routinely
// targets several components, so no single Site owns the failure; and with no
// Site yet created there was no Site to carry it at all. A user who mistyped a
// patch got no signal on the object they edited.
func TestOverrideConfigMapEventsAreRecordedOnTheConfigMap(t *testing.T) {
	site := siteFor("edge")

	r, _, _ := overrideTestEnv(t, site, overridesConfigMap(map[string]string{
		"overrides.yaml": "apiVersion: " + override.APIVersion +
			"\noverrides:\n  - component: net\n    kind: DaemonSet\n    patch:\n      metadata:\n        name: renamed\n",
	}))

	recorder := &recordingEventSink{}
	r.Recorder = recorder

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "edge"}}); err == nil {
		t.Fatal("expected the invalid document to fail the pass")
	}

	event, found := recorder.on(override.ConfigMapName)
	if !found {
		t.Fatalf("no Event recorded against %s; events = %+v", override.ConfigMapName, recorder.events)
	}

	if event.eventType != corev1.EventTypeWarning {
		t.Fatalf("event type = %q, want Warning", event.eventType)
	}

	if !strings.Contains(event.note, "left unchanged") {
		t.Fatalf("note = %q, want it to say the workloads were left alone", event.note)
	}
}

// TestOverrideConfigMapEventsAreNotRepeatedPerPass checks the Event does not
// fire on every reconcile. A rejected document requeues the pass, so an
// unconditional Event would produce one per retry for as long as the document
// stays broken.
func TestOverrideConfigMapEventsAreNotRepeatedPerPass(t *testing.T) {
	site := siteFor("edge")

	r, _, _ := overrideTestEnv(t, site, overridesConfigMap(map[string]string{
		"overrides.yaml": "apiVersion: " + override.APIVersion +
			"\noverrides:\n  - component: net\n    kind: DaemonSet\n    patch:\n      metadata:\n        name: renamed\n",
	}))

	recorder := &recordingEventSink{}
	r.Recorder = recorder

	for range 3 {
		//nolint:errcheck // the pass is expected to fail; the Events are what matter
		r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "edge"}})
	}

	var count int

	for _, event := range recorder.events {
		if event.name == override.ConfigMapName {
			count++
		}
	}

	if count != 1 {
		t.Fatalf("recorded %d Events for one unchanged document, want 1", count)
	}
}

// recordedEvent is one Event captured by recordingEventSink.
type recordedEvent struct {
	name      string
	eventType string
	reason    string
	note      string
}

// recordingEventSink captures Events so tests can assert what the operator told
// the user and where it told them.
type recordingEventSink struct {
	events []recordedEvent
}

func (s *recordingEventSink) Eventf(
	regarding runtime.Object,
	_ runtime.Object,
	eventType, reason, _, note string,
	args ...any,
) {
	name := ""
	if accessor, err := apimeta.Accessor(regarding); err == nil {
		name = accessor.GetName()
	}

	s.events = append(s.events, recordedEvent{
		name:      name,
		eventType: eventType,
		reason:    reason,
		note:      fmt.Sprintf(note, args...),
	})
}

// on returns the first Event recorded against the named object.
func (s *recordingEventSink) on(name string) (recordedEvent, bool) {
	for _, event := range s.events {
		if event.name == name {
			return event, true
		}
	}

	return recordedEvent{}, false
}

// TestInvalidDocumentLeavesComponentsNotReady is a regression test.
//
// An unusable document removes every overridable workload from the plan before
// execution. Because those operations never ran, they produced no result, and
// CombineResult had nothing to look at, so each component reported the
// Reconciled verdict it had planned with. The Site simultaneously said
// overrides were Degraded and that every component was Ready, about the same
// objects.
//
// Leaving the running workload untouched is the point; claiming it was
// reconciled is not.
func TestInvalidDocumentLeavesComponentsNotReady(t *testing.T) {
	site := siteFor("edge")

	r, _, cl := overrideTestEnv(t, site, overridesConfigMap(map[string]string{
		"overrides.yaml": "apiVersion: " + override.APIVersion +
			"\noverrides:\n  - component: net\n    kind: DaemonSet\n    patch:\n      metadata:\n        name: renamed\n",
	}))

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "edge"}}); err == nil {
		t.Fatal("an unusable document must fail the pass")
	}

	var got unboundedv1alpha3.Site
	if err := cl.Get(t.Context(), client.ObjectKey{Name: "edge"}, &got); err != nil {
		t.Fatalf("get site: %v", err)
	}

	if got.Status.Overrides == nil || got.Status.Overrides.Phase != unboundedv1alpha3.OverridePhaseDegraded {
		t.Fatalf("override status = %+v, want Degraded", got.Status.Overrides)
	}

	condition := apimeta.FindStatusCondition(got.Status.Conditions, "NetReady")
	if condition == nil {
		t.Fatal("NetReady condition not found")
	}

	if condition.Status != metav1.ConditionTrue {
		if condition.Reason != component.ReasonOverrideNotApplied {
			t.Fatalf("NetReady reason = %q, want %q", condition.Reason, component.ReasonOverrideNotApplied)
		}

		return
	}

	t.Fatalf("NetReady = True while overrides are Degraded; the component withheld its workload and cannot be reconciled")
}

// TestOverrideStatusMessageIsBounded is a regression test.
//
// Validation reports every problem it finds rather than only the first, which
// is right for a user fixing a document and wrong for a status field: the
// joined error grows with the document and is copied into every Site. A large
// enough document produced a status patch past the API server's request limit,
// which fails, and retries forever without ever recording why.
func TestOverrideStatusMessageIsBounded(t *testing.T) {
	huge := strings.Repeat("this document is deeply and repetitively wrong. ", 200000)

	report := &override.Report{
		Withheld: []override.WithheldOperation{{
			Ref:       component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "node")),
			Component: "net",
			Err:       errors.New(huge),
		}},
	}

	status := overrideStatusFor("edge",
		overrideSnapshot{state: overridesInvalid, err: errors.New(huge), resourceVersion: "7"},
		report, component.NewPlan(), component.ExecutionResult{})

	if status.Phase != unboundedv1alpha3.OverridePhaseDegraded {
		t.Fatalf("phase = %q, want Degraded", status.Phase)
	}

	if len(status.Message) > maxStatusMessage {
		t.Fatalf("message is %d bytes, over the %d byte cap", len(status.Message), maxStatusMessage)
	}

	if !strings.Contains(status.Message, "truncated") {
		t.Fatal("a truncated message must say so, and say where the rest is")
	}
}

// validOverrideDocument is a document that resolves and merges cleanly.
func validOverrideDocument() string {
	return `apiVersion: ` + override.APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: node
                imagePullPolicy: Always
`
}

// TestConfigMapEventReportsExecutionOutcome is a regression test.
//
// The ConfigMap Event was chosen from the merge result alone, so a document
// that merged cleanly produced a Normal success Event even when the writes were
// rejected by admission, failed against the API server, or were skipped because
// something they depend on failed. With no Sites this Event is the only verdict
// a user gets.
func TestConfigMapEventReportsExecutionOutcome(t *testing.T) {
	ref := component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "node"))

	report := &override.Report{Workloads: []override.WorkloadResult{
		{Ref: ref, Site: "edge", Hash: "h1"},
	}}

	snapshot := overrideSnapshot{state: overridesValid, resourceVersion: "7"}

	t.Run("the write failed", func(t *testing.T) {
		exec := component.ExecutionResult{Results: []component.OperationResult{{
			Ref: ref, Kind: component.OpApply, Status: component.OpFailed,
			Err: errors.New("admission webhook denied the request"),
		}}}

		eventType, reason, note := overrideConfigMapEvent(snapshot, report, exec)

		if eventType != corev1.EventTypeWarning {
			t.Fatalf("event type = %q, want Warning; the override never reached the cluster", eventType)
		}

		if reason != "OverridesNotWritten" {
			t.Fatalf("reason = %q", reason)
		}

		if !strings.Contains(note, "admission webhook denied") {
			t.Fatalf("note = %q, want the underlying reason", note)
		}
	})

	t.Run("the write was skipped", func(t *testing.T) {
		exec := component.ExecutionResult{Results: []component.OperationResult{{
			Ref: ref, Kind: component.OpApply, Status: component.OpSkipped,
			Err: errors.New("net did not complete its config successfully"),
		}}}

		if eventType, _, _ := overrideConfigMapEvent(snapshot, report, exec); eventType != corev1.EventTypeWarning {
			t.Fatalf("event type = %q, want Warning", eventType)
		}
	})

	t.Run("the plan never executed", func(t *testing.T) {
		eventType, _, note := overrideConfigMapEvent(snapshot, report, component.ExecutionResult{})

		if eventType != corev1.EventTypeWarning {
			t.Fatalf("event type = %q, want Warning", eventType)
		}

		if !strings.Contains(note, "never executed") {
			t.Fatalf("note = %q", note)
		}
	})

	t.Run("the write succeeded", func(t *testing.T) {
		exec := component.ExecutionResult{Results: []component.OperationResult{{
			Ref: ref, Kind: component.OpApply, Status: component.OpSucceeded,
		}}}

		eventType, reason, _ := overrideConfigMapEvent(snapshot, report, exec)

		if eventType != corev1.EventTypeNormal || reason != "OverridesApplied" {
			t.Fatalf("event = %s/%s, want a Normal OverridesApplied", eventType, reason)
		}
	})
}

// TestConfigMapEventDedupeKeepsScopeApart is a regression test.
//
// A pass for one Site resolves overrides against that Site alone, so its
// verdict describes part of the document. Deduplicating on the document version
// and verdict alone let the first such pass claim "1 workload overridden" for a
// two-Site document, then suppress the complete fan-out verdict that followed,
// because both were Normal OverridesApplied at the same resourceVersion.
func TestConfigMapEventDedupeKeepsScopeApart(t *testing.T) {
	r := &SiteReconciler{}

	if !r.overrideEventIsNew("7", "OverridesApplied", "site/alpha", "1 workload(s) overridden") {
		t.Fatal("the first verdict must be emitted")
	}

	if r.overrideEventIsNew("7", "OverridesApplied", "site/alpha", "1 workload(s) overridden") {
		t.Fatal("an identical repeat must be suppressed")
	}

	if !r.overrideEventIsNew("7", "OverridesApplied", "all-sites", "2 workload(s) overridden") {
		t.Fatal("the complete fan-out verdict must not be suppressed by a single-Site one")
	}
}

// TestSiteEventFollowsStatusPersistence is a regression test.
//
// The Event describes a status transition, so emitting it before the patch left
// users with an Event for a state the Site never reached when the patch lost an
// optimistic lock or failed outright, and the retry emitted it again.
func TestSiteEventFollowsStatusPersistence(t *testing.T) {
	site := siteFor("edge")

	r, _, _ := overrideTestEnv(t, site, overridesConfigMap(map[string]string{
		"overrides.yaml": validOverrideDocument(),
	}))

	recorder := &recordingEventSink{}
	r.Recorder = recorder

	// Every status patch fails, so no transition is ever committed.
	scheme := newReconcilerTestScheme(t)
	r.Client = fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(site, overridesConfigMap(map[string]string{"overrides.yaml": validOverrideDocument()})).
		WithStatusSubresource(&unboundedv1alpha3.Site{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(
				context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption,
			) error {
				return errors.New("status patch rejected")
			},
		}).
		Build()

	//nolint:errcheck // the pass is expected to fail; the Events are what matter
	r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "edge"}})

	for _, event := range recorder.events {
		if event.name == "edge" {
			t.Fatalf("an Event described a Site status transition that was never committed: %+v", event)
		}
	}
}

// TestQuarantineScopesWithholdingToWhatIsActuallyInDoubt is the regression test
// for the blast radius of a broken document.
//
// One typo used to withhold every overridable workload of every component on
// every Site, and discard the entries in every other ConfigMap key while doing
// it. The format is one document per key so a document can be split by concern
// or by team, and that is only useful if a mistake in one key is contained.
func TestQuarantineScopesWithholdingToWhatIsActuallyInDoubt(t *testing.T) {
	planFor := func() *component.Plan {
		plan := component.NewPlan()
		plan.Add(
			component.Operation{
				Kind: component.OpApply, Object: unstructuredOf("apps/v1", "DaemonSet", "unbounded-net-node"),
				Component: "net", Overridable: true,
			},
			component.Operation{
				Kind: component.OpApply, Object: unstructuredOf("apps/v1", "Deployment", "machina-controller"),
				Component: "machina", Overridable: true,
			},
			component.Operation{
				Kind: component.OpApply, Object: unstructuredOf("apps/v1", "DaemonSet", "storage-west"),
				Component: "storage", Site: "edge-west", Overridable: true,
			},
			component.Operation{
				Kind: component.OpApply, Object: unstructuredOf("apps/v1", "DaemonSet", "storage-east"),
				Component: "storage", Site: "edge-east", Overridable: true,
			},
			component.Operation{
				Kind: component.OpApply, Object: unstructuredOf("v1", "ConfigMap", "cfg"),
				Component: "net",
			},
		)

		return plan
	}

	withheldNames := func(withheld []override.WithheldOperation) []string {
		names := make([]string, 0, len(withheld))
		for _, op := range withheld {
			names = append(names, op.Ref.Name)
		}

		sort.Strings(names)

		return names
	}

	t.Run("an entry naming a component and Site withholds only that workload", func(t *testing.T) {
		source := override.Source{Key: "a.yaml", Index: 0}
		q := quarantineFor([]override.Problem{{
			Key: "a.yaml", Source: &source,
			Component: "storage", Kind: "DaemonSet", Sites: []string{"edge-west"},
			Err: errors.New("nope"),
		}})

		plan := planFor()

		got := withheldNames(dropOverridableOperations(plan, q))
		if len(got) != 1 || got[0] != "storage-west" {
			t.Fatalf("withheld = %v, want only storage-west", got)
		}

		if len(plan.Operations) != 4 {
			t.Fatalf("remaining = %d, want everything else to keep reconciling", len(plan.Operations))
		}
	})

	t.Run("an entry naming no kind withholds every kind that component emits", func(t *testing.T) {
		source := override.Source{Key: "a.yaml", Index: 0}
		q := quarantineFor([]override.Problem{{
			Key: "a.yaml", Source: &source, Component: "storage", Err: errors.New("nope"),
		}})

		got := withheldNames(dropOverridableOperations(planFor(), q))
		if len(got) != 2 {
			t.Fatalf("withheld = %v, want both Sites' storage workloads", got)
		}
	})

	// Resolution matches on component, so an entry naming none could never have
	// resolved to a workload. Withholding anything for it would punish the rest
	// of the document for a typo that changed nothing.
	t.Run("an entry naming no component withholds nothing", func(t *testing.T) {
		source := override.Source{Key: "a.yaml", Index: 0}
		q := quarantineFor([]override.Problem{{
			Key: "a.yaml", Source: &source, Err: errors.New("component is required"),
		}})

		if !q.empty() {
			t.Fatalf("quarantine = %+v, want nothing withheld", q)
		}
	})

	// A key that did not parse held entries nobody read, so there is no way to
	// know what it would have changed.
	t.Run("a key that did not parse withholds everything", func(t *testing.T) {
		q := quarantineFor([]override.Problem{{Key: "a.yaml", Err: errors.New("broken")}})

		plan := planFor()

		got := withheldNames(dropOverridableOperations(plan, q))
		if len(got) != 4 {
			t.Fatalf("withheld = %v, want every overridable workload", got)
		}

		// Even then, only the workloads. RBAC, Services and ConfigMaps must
		// keep reconciling, or an override typo becomes an outage.
		if len(plan.Operations) != 1 || plan.Operations[0].Object.GetKind() != "ConfigMap" {
			t.Fatalf("remaining = %+v, want the non-overridable operations untouched", plan.Operations)
		}
	})
}

// TestLoadOverridesKeepsUsableEntriesAlongsideBrokenOnes pins that a document
// with one bad entry still applies the rest of itself.
func TestLoadOverridesKeepsUsableEntriesAlongsideBrokenOnes(t *testing.T) {
	scheme := newReconcilerTestScheme(t)

	good := `apiVersion: ` + override.APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    extraArgs:
      node: [--flag]
`

	// machina emits no DaemonSet, so this entry can never match anything.
	bad := `apiVersion: ` + override.APIVersion + `
overrides:
  - component: machina
    kind: DaemonSet
    extraArgs:
      machina-controller: [--flag]
`

	env := &component.Env{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(overridesConfigMap(map[string]string{
			"good.yaml": good,
			"bad.yaml":  bad,
		})).Build(),
		Namespace: component.DefaultNamespace,
	}

	snapshot := loadOverrides(t.Context(), env)

	if snapshot.state != overridesPartial {
		t.Fatalf("state = %v, want partial", snapshot.state)
	}

	if len(snapshot.entries) != 1 || snapshot.entries[0].Entry.Component != "net" {
		t.Fatalf("entries = %+v, want the net entry to survive", snapshot.entries)
	}

	// The entry that failed validation must not be merged even though it
	// parsed: applying a patch just declared invalid is worse than not applying
	// it.
	for _, entry := range snapshot.entries {
		if entry.Source.Key == "bad.yaml" {
			t.Fatal("an entry that failed validation must not be merged")
		}
	}

	q := snapshot.quarantine()
	if q.all {
		t.Fatal("a per-entry failure must not withhold every workload")
	}

	if len(q.targets) != 1 || q.targets[0].component != "machina" {
		t.Fatalf("quarantine targets = %+v, want machina alone", q.targets)
	}
}

// TestBadEntryLeavesUnrelatedComponentsReady is the end-to-end form of the
// blast-radius fix.
//
// An entry that fails validation used to withhold every overridable workload of
// every component on every Site, so one typo turned NetReady, MachinaReady,
// GantryReady, MetalmanReady and StorageReady False everywhere at once. Any
// automation gating on `kubectl wait --for=condition=NetReady` broke on a
// mistake in an unrelated part of the document.
func TestBadEntryLeavesUnrelatedComponentsReady(t *testing.T) {
	enabled := true
	site := siteFor("edge")
	site.Spec.Components.Metalman = &unboundedv1alpha3.MetalmanComponentSpec{
		SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
	}

	// spec.replicas on metalman is owned by spec.components.metalman.replicas,
	// so this entry is rejected. Nothing about it concerns net.
	document := "apiVersion: " + override.APIVersion + `
overrides:
  - component: metalman
    kind: Deployment
    patch:
      spec:
        replicas: 3
`

	r, _, cl := overrideTestEnv(t, site, overridesConfigMap(map[string]string{"overrides.yaml": document}))

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Name: "edge"}}); err == nil {
		t.Fatal("a rejected entry must still fail the pass, so it requeues and is logged")
	}

	var got unboundedv1alpha3.Site
	if err := cl.Get(t.Context(), client.ObjectKey{Name: "edge"}, &got); err != nil {
		t.Fatalf("get site: %v", err)
	}

	// The component the entry named did not write what it planned.
	metalmanReady := apimeta.FindStatusCondition(got.Status.Conditions, "MetalmanReady")
	if metalmanReady == nil || metalmanReady.Status != metav1.ConditionFalse {
		t.Fatalf("MetalmanReady = %+v, want False", metalmanReady)
	}

	if metalmanReady.Reason != component.ReasonOverrideNotApplied {
		t.Fatalf("MetalmanReady reason = %q, want %q", metalmanReady.Reason, component.ReasonOverrideNotApplied)
	}

	// Every other component is untouched by a metalman entry and must still be
	// reconciling.
	for _, conditionType := range []string{"NetReady"} {
		condition := apimeta.FindStatusCondition(got.Status.Conditions, conditionType)
		if condition == nil {
			t.Fatalf("%s condition not found", conditionType)
		}

		if condition.Status != metav1.ConditionTrue {
			t.Fatalf("%s = %s (%s: %s), want True: a metalman entry puts no other component in doubt",
				conditionType, condition.Status, condition.Reason, condition.Message)
		}
	}
}

// TestOverrideStatusReportsWithheldWorkloads is a regression test.
//
// A withheld workload is dropped from the plan before overrides are applied, so
// it never becomes a resolution target and never reached report.Workloads. It
// therefore had no row in status at all, and nothing could tell "no override
// targets this workload" from "the operator declined to write it".
func TestOverrideStatusReportsWithheldWorkloads(t *testing.T) {
	report := &override.Report{
		Withheld: []override.WithheldOperation{{
			Ref:       component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "unbounded-net-node")),
			Component: "net",
			Err:       errors.New("overrides.yaml[0]: not an overridable field"),
		}},
	}

	status := overrideStatusFor("edge", overrideSnapshot{
		state: overridesPartial, resourceVersion: "9",
	}, report, component.NewPlan(), component.ExecutionResult{})

	if status.Phase != unboundedv1alpha3.OverridePhaseDegraded {
		t.Fatalf("phase = %q, want Degraded", status.Phase)
	}

	if len(status.Workloads) != 1 {
		t.Fatalf("workloads = %+v, want the withheld workload reported", status.Workloads)
	}

	got := status.Workloads[0]
	if got.State != unboundedv1alpha3.OverrideStateWithheld {
		t.Fatalf("state = %q, want Withheld", got.State)
	}

	if got.Name != "unbounded-net-node" {
		t.Fatalf("name = %q, want the withheld workload named", got.Name)
	}
}

// TestOverrideStatusDistinguishesFailedFromUntargeted pins that a workload
// whose override failed reports a desired hash and a Failed state, so it cannot
// be mistaken for one no override targets.
func TestOverrideStatusDistinguishesFailedFromUntargeted(t *testing.T) {
	ref := component.RefOf(unstructuredOf("apps/v1", "DaemonSet", "unbounded-net-node"))

	report := &override.Report{
		Workloads: []override.WorkloadResult{{
			Ref:  ref,
			Hash: "deadbeef",
			Err:  errors.New("overrides.yaml[0]: patch targets container \"typo\""),
		}},
	}

	status := overrideStatusFor("edge", overrideSnapshot{
		state: overridesValid, resourceVersion: "9",
	}, report, component.NewPlan(), component.ExecutionResult{})

	if len(status.Workloads) != 1 {
		t.Fatalf("workloads = %+v, want one", status.Workloads)
	}

	got := status.Workloads[0]
	if got.State != unboundedv1alpha3.OverrideStateFailed {
		t.Fatalf("state = %q, want Failed", got.State)
	}

	// The desired hash is what the user asked for, and it is knowable even
	// though the merge failed. Leaving it empty is what made this row read as
	// "no override".
	if got.DesiredHash == "" {
		t.Fatal("a failed workload must still report the hash that was desired")
	}
}

// TestOverrideStatusDoesNotDegradeOnADeferredWrite pins that a lost race reads
// as pending rather than as a failure.
func TestOverrideStatusDoesNotDegradeOnADeferredWrite(t *testing.T) {
	workload := unstructuredOf("apps/v1", "DaemonSet", "unbounded-net-node")
	ref := component.RefOf(workload)

	plan := component.NewPlan()
	plan.Add(component.Operation{
		Kind: component.OpApply, Object: workload, Component: "net", Overridable: true,
	})

	report := &override.Report{Workloads: []override.WorkloadResult{{Ref: ref, Hash: "deadbeef"}}}

	status := overrideStatusFor("edge", overrideSnapshot{
		state: overridesValid, resourceVersion: "9",
	}, report, plan, component.ExecutionResult{Deferred: []component.ObjectRef{ref}})

	if status.Workloads[0].State != unboundedv1alpha3.OverrideStatePending {
		t.Fatalf("state = %q, want Pending", status.Workloads[0].State)
	}

	if status.Phase == unboundedv1alpha3.OverridePhaseDegraded {
		t.Fatal("a write deferred to the next pass is not a degradation")
	}
}
