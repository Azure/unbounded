// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// configMapOp builds an OpApply operation for a ConfigMap, which is the
// smallest object that exercises the executor end to end.
func configMapOp(name string) Operation {
	return Operation{
		Kind:      OpApply,
		Object:    configMapObject(name),
		Component: "test",
	}
}

func configMapObject(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	obj.SetNamespace(DefaultNamespace)
	obj.SetName(name)

	return obj
}

func daemonSetObject(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"})
	obj.SetNamespace(DefaultNamespace)
	obj.SetName(name)

	return obj
}

// recordingEnv returns an Env whose writes are recorded rather than performed,
// so tests can assert ordering and which operations were attempted at all.
func recordingEnv(t *testing.T, objects ...client.Object) (*Env, *[]string) {
	t.Helper()

	var calls []string

	scheme := testScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				named, ok := obj.(interface {
					GetKind() string
					GetName() string
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				calls = append(calls, "apply "+named.GetKind()+"/"+named.GetName())

				return nil
			},
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				calls = append(calls, "create "+obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())

				return cl.Create(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				calls = append(calls, "patch "+obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())

				return cl.Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				calls = append(calls, "delete "+obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName())

				return cl.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	return &Env{Client: cl, Scheme: scheme, Namespace: DefaultNamespace}, &calls
}

func TestExecuteEmptyPlan(t *testing.T) {
	env, calls := recordingEnv(t)

	result, err := env.Execute(t.Context(), NewPlan())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(result.Results) != 0 {
		t.Fatalf("results = %d, want 0", len(result.Results))
	}

	if len(*calls) != 0 {
		t.Fatalf("calls = %v, want none", *calls)
	}
}

func TestExecuteRunsEachOperationKind(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: "adopt-me", ResourceVersion: "1"},
	}
	doomed := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: "doomed"},
	}

	env, calls := recordingEnv(t, existing, doomed)

	base := configMapObject("adopt-me")
	base.SetResourceVersion("1")

	adopted := base.DeepCopy()
	adopted.SetLabels(map[string]string{"adopted": "true"})

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: configMapObject("applied"), Component: "a"},
		Operation{Kind: OpCreateIfAbsent, Object: configMapObject("created"), Component: "a"},
		Operation{Kind: OpMergePatch, Object: adopted, Base: base, Component: "a"},
		Operation{Kind: OpDelete, Object: configMapObject("doomed"), Component: "a"},
	)

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if err := result.Err(); err != nil {
		t.Fatalf("execution errors: %v", err)
	}

	// Removals run first, so a component's cleanup cannot undo something it
	// writes later in the same pass. Within a tier, operations execute in the
	// order the component planned them.
	want := []string{
		"delete ConfigMap/doomed",
		"apply ConfigMap/applied",
		"create ConfigMap/created",
		"patch ConfigMap/adopt-me",
	}

	assertCalls(t, *calls, want)
}

// TestExecuteCreateIfAbsentToleratesAlreadyExists covers the race the operation
// exists for: another writer created the ConfigMap first, and its payload must
// survive rather than be overwritten.
func TestExecuteCreateIfAbsentToleratesAlreadyExists(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: "raced"},
		Data:       map[string]string{"user": "payload"},
	}

	env, _ := recordingEnv(t, existing)

	plan := NewPlan()
	plan.Add(Operation{Kind: OpCreateIfAbsent, Object: configMapObject("raced"), Component: "a"})

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if err := result.Err(); err != nil {
		t.Fatalf("AlreadyExists must be success, got %v", err)
	}

	var got corev1.ConfigMap
	if err := env.Client.Get(t.Context(), client.ObjectKey{Namespace: DefaultNamespace, Name: "raced"}, &got); err != nil {
		t.Fatalf("get raced ConfigMap: %v", err)
	}

	if got.Data["user"] != "payload" {
		t.Fatalf("existing payload was overwritten: %v", got.Data)
	}
}

func TestExecuteMergePatchRequiresBase(t *testing.T) {
	env, _ := recordingEnv(t)

	plan := NewPlan()
	plan.Add(Operation{Kind: OpMergePatch, Object: configMapObject("no-base"), Component: "a"})

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if err := result.Err(); err == nil {
		t.Fatal("expected an error when a merge patch carries no observed state")
	}
}

// TestExecuteOrdersByDependency covers the ordering the design requires: a
// ConfigMap whose hash a workload carries must be written before the workload.
func TestExecuteOrdersByDependency(t *testing.T) {
	env, calls := recordingEnv(t)

	config := configMapObject("component-config")
	workload := daemonSetObject("component-agent")

	plan := NewPlan()
	// Deliberately planned workload-first, so a naive executor would get it wrong.
	plan.Add(
		Operation{Kind: OpApply, Object: workload, Component: "a", DependsOn: []ObjectRef{RefOf(config)}},
		Operation{Kind: OpApply, Object: config, Component: "a"},
	)

	if _, err := env.Execute(t.Context(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *calls, []string{
		"apply ConfigMap/component-config",
		"apply DaemonSet/component-agent",
	})
}

// TestExecuteContinuesPastFailureButSkipsDependents is the core of the
// execution contract: a failure does not abort the pass, independent work still
// happens, and dependents are skipped rather than attempted.
func TestExecuteContinuesPastFailureButSkipsDependents(t *testing.T) {
	scheme := testScheme(t)

	var attempted []string

	failure := errors.New("apiserver said no")

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				named, ok := obj.(interface {
					GetKind() string
					GetName() string
				})
				if !ok {
					t.Fatalf("applied object has unexpected type %T", obj)
				}

				name := named.GetKind() + "/" + named.GetName()
				attempted = append(attempted, name)

				if name == "ConfigMap/broken-config" {
					return failure
				}

				return nil
			},
		}).
		Build()

	env := &Env{Client: cl, Scheme: scheme, Namespace: DefaultNamespace}

	broken := configMapObject("broken-config")
	dependent := daemonSetObject("dependent-agent")
	independent := configMapObject("unrelated")

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: broken, Component: "a"},
		Operation{Kind: OpApply, Object: dependent, Component: "a", DependsOn: []ObjectRef{RefOf(broken)}},
		Operation{Kind: OpApply, Object: independent, Component: "b"},
	)

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The dependent must never have been attempted.
	for _, name := range attempted {
		if name == "DaemonSet/dependent-agent" {
			t.Fatal("dependent of a failed operation was attempted; it must be skipped")
		}
	}

	// The independent operation must still have run.
	var sawIndependent bool

	for _, name := range attempted {
		if name == "ConfigMap/unrelated" {
			sawIndependent = true
		}
	}

	if !sawIndependent {
		t.Fatal("independent operation did not execute; a failure must not abort the pass")
	}

	if got := len(result.Failed()); got != 1 {
		t.Fatalf("failed operations = %d, want 1", got)
	}

	if got := len(result.Skipped()); got != 1 {
		t.Fatalf("skipped operations = %d, want 1", got)
	}

	if !errors.Is(result.Err(), failure) {
		t.Fatalf("Err() = %v, want it to wrap %v", result.Err(), failure)
	}
}

// TestExecuteErrDoesNotRepeatSkippedOperations guards the condition message: a
// skipped operation must not restate the failure that caused it, or a Site
// condition buries the real cause.
func TestExecuteErrDoesNotRepeatSkippedOperations(t *testing.T) {
	result := ExecutionResult{Results: []OperationResult{
		{Ref: RefOf(configMapObject("a")), Kind: OpApply, Status: OpFailed, Err: errors.New("root cause")},
		{Ref: RefOf(daemonSetObject("b")), Kind: OpApply, Status: OpSkipped, Err: errors.New("dependency did not complete")},
	}}

	err := result.Err()
	if err == nil {
		t.Fatal("expected an error")
	}

	if got := err.Error(); got == "" || strings.Contains(got, "dependency did not complete") {
		t.Fatalf("Err() = %q, want only the root cause", got)
	}
}

func TestExecuteDetectsDependencyCycle(t *testing.T) {
	env, _ := recordingEnv(t)

	first := configMapObject("first")
	second := configMapObject("second")

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: first, Component: "a", DependsOn: []ObjectRef{RefOf(second)}},
		Operation{Kind: OpApply, Object: second, Component: "a", DependsOn: []ObjectRef{RefOf(first)}},
	)

	if _, err := env.Execute(t.Context(), plan); err == nil {
		t.Fatal("expected a cycle error")
	}
}

// TestExecuteTreatsUnknownDependencyAsSatisfied covers cross-component
// dependencies, where the object may be planned by another component or may
// already exist.
func TestExecuteTreatsUnknownDependencyAsSatisfied(t *testing.T) {
	env, calls := recordingEnv(t)

	plan := NewPlan()
	plan.Add(Operation{
		Kind:      OpApply,
		Object:    daemonSetObject("agent"),
		Component: "a",
		DependsOn: []ObjectRef{RefOf(configMapObject("owned-by-someone-else"))},
	})

	if _, err := env.Execute(t.Context(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *calls, []string{"apply DaemonSet/agent"})
}

// TestExecuteDeduplicatesSharedOperations covers the metalman case: the same
// support RBAC is planned once per Site and must be written once per pass.
func TestExecuteDeduplicatesSharedOperations(t *testing.T) {
	env, calls := recordingEnv(t)

	plan := NewPlan()
	for _, site := range []string{"alpha", "bravo", "charlie"} {
		plan.Add(Operation{
			Kind:      OpApply,
			Object:    configMapObject("shared-support"),
			Component: "metalman",
			Site:      site,
			SharedKey: "metalman/support",
		})
	}

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *calls, []string{"apply ConfigMap/shared-support"})

	// Every contributing Site must still receive a result, or the reconciler
	// cannot report the outcome on each Site's conditions.
	sites := map[string]bool{}
	for _, r := range result.Results {
		sites[r.Site] = true
	}

	for _, site := range []string{"alpha", "bravo", "charlie"} {
		if !sites[site] {
			t.Fatalf("site %q received no result for the shared operation", site)
		}
	}
}

// TestExecuteRejectsUnequalSharedOperations guards against resolving a planning
// bug by letting Site iteration order decide which version wins.
func TestExecuteRejectsUnequalSharedOperations(t *testing.T) {
	env, calls := recordingEnv(t)

	alpha := configMapObject("shared-support")
	alpha.SetLabels(map[string]string{"site": "alpha"})

	bravo := configMapObject("shared-support")
	bravo.SetLabels(map[string]string{"site": "bravo"})

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: alpha, Component: "metalman", Site: "alpha", SharedKey: "metalman/support"},
		Operation{Kind: OpApply, Object: bravo, Component: "metalman", Site: "bravo", SharedKey: "metalman/support"},
	)

	_, err := env.Execute(t.Context(), plan)
	if err == nil {
		t.Fatal("expected unequal shared operations to be rejected")
	}

	if len(*calls) != 0 {
		t.Fatalf("nothing may be written when a plan is rejected, got %v", *calls)
	}

	for _, want := range []string{"alpha", "bravo"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q must name contributor %q", err, want)
		}
	}
}

// TestExecuteRejectsMismatchedSharedOperations covers the other two ways a
// shared key can be planned inconsistently: disagreeing on what to do with the
// object, or pointing at two different objects entirely.
func TestExecuteRejectsMismatchedSharedOperations(t *testing.T) {
	cases := []struct {
		name string
		ops  []Operation
		want string
	}{
		{
			name: "different operation kinds",
			ops: []Operation{
				{Kind: OpApply, Object: configMapObject("support"), Component: "metalman", Site: "alpha", SharedKey: "k"},
				{Kind: OpDelete, Object: configMapObject("support"), Component: "metalman", Site: "bravo", SharedKey: "k"},
			},
			want: "contributors must agree",
		},
		{
			name: "different objects",
			ops: []Operation{
				{Kind: OpApply, Object: configMapObject("support-a"), Component: "metalman", Site: "alpha", SharedKey: "k"},
				{Kind: OpApply, Object: configMapObject("support-b"), Component: "metalman", Site: "bravo", SharedKey: "k"},
			},
			want: "must identify one object",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, calls := recordingEnv(t)

			plan := NewPlan()
			plan.Add(tc.ops...)

			_, err := env.Execute(t.Context(), plan)
			if err == nil {
				t.Fatal("expected the plan to be rejected")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}

			if len(*calls) != 0 {
				t.Fatalf("nothing may be written when a plan is rejected, got %v", *calls)
			}
		})
	}
}

// TestExecutePreservesWithinComponentOrder guards the ordering contract that
// commit C's golden tests depend on: a component's operations execute in the
// order it planned them, so deliberate sequencing survives.
//
// gantry removes its legacy node config before applying anything, and storage
// writes its ConfigMap before the DaemonSet that carries its hash. Sorting by
// kind or name inside a component would silently reorder both.
func TestExecutePreservesWithinComponentOrder(t *testing.T) {
	env, calls := recordingEnv(t)

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpDelete, Object: daemonSetObject("legacy-agent"), Component: "gantry"},
		Operation{Kind: OpDelete, Object: configMapObject("legacy-config"), Component: "gantry"},
		Operation{Kind: OpApply, Object: configMapObject("agent-config"), Component: "gantry"},
		Operation{Kind: OpApply, Object: daemonSetObject("agent"), Component: "gantry"},
	)

	if _, err := env.Execute(t.Context(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *calls, []string{
		"delete DaemonSet/legacy-agent",
		"delete ConfigMap/legacy-config",
		"apply ConfigMap/agent-config",
		"apply DaemonSet/agent",
	})
}

// TestExecuteIsRepeatable guards golden-plan stability: executing the same plan
// twice produces the same sequence.
func TestExecuteIsRepeatable(t *testing.T) {
	build := func() []string {
		env, calls := recordingEnv(t)

		plan := NewPlan()
		plan.Add(
			Operation{Kind: OpApply, Object: configMapObject("zulu"), Component: "net"},
			Operation{Kind: OpApply, Object: configMapObject("alpha"), Component: "net"},
			Operation{Kind: OpApply, Object: daemonSetObject("agent"), Component: "net"},
			Operation{Kind: OpApply, Object: configMapObject("machina"), Component: "machina"},
		)

		if _, err := env.Execute(t.Context(), plan); err != nil {
			t.Fatalf("Execute: %v", err)
		}

		return *calls
	}

	assertCalls(t, build(), build())
}

// TestExecutePreservesComponentOrder guards that registry order rather than
// alphabetical order decides which component's operations run first, so
// conditions and rollouts stay in the documented order.
func TestExecutePreservesComponentOrder(t *testing.T) {
	env, calls := recordingEnv(t)

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: configMapObject("net-config"), Component: "net"},
		Operation{Kind: OpApply, Object: configMapObject("machina-config"), Component: "machina"},
		Operation{Kind: OpApply, Object: configMapObject("gantry-config"), Component: "gantry"},
	)

	if _, err := env.Execute(t.Context(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *calls, []string{
		"apply ConfigMap/net-config",
		"apply ConfigMap/machina-config",
		"apply ConfigMap/gantry-config",
	})
}

func TestPlanHelpers(t *testing.T) {
	var nilPlan *Plan
	if nilPlan.Len() != 0 {
		t.Fatal("nil plan must report zero length")
	}

	plan := NewPlan()
	plan.Merge(nil)

	if plan.Len() != 0 {
		t.Fatal("merging nil must be a no-op")
	}

	other := NewPlan()
	other.Add(configMapOp("a"), configMapOp("b"))
	plan.Merge(other)

	if plan.Len() != 2 {
		t.Fatalf("Len = %d, want 2", plan.Len())
	}
}

func TestObjectRefString(t *testing.T) {
	cases := []struct {
		name string
		ref  ObjectRef
		want string
	}{
		{
			name: "namespaced",
			ref:  ObjectRef{GVK: schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}, Namespace: "ns", Name: "cm"},
			want: "ConfigMap/ns/cm",
		},
		{
			name: "cluster scoped",
			ref:  ObjectRef{GVK: schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}, Name: "ns"},
			want: "Namespace/ns",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOpKindAndStatusStrings(t *testing.T) {
	kinds := map[OpKind]string{
		OpApply:          "Apply",
		OpCreateIfAbsent: "CreateIfAbsent",
		OpMergePatch:     "MergePatch",
		OpDelete:         "Delete",
	}

	for kind, want := range kinds {
		if got := kind.String(); got != want {
			t.Fatalf("OpKind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}

	statuses := map[OpStatus]string{
		OpSucceeded: "Succeeded",
		OpFailed:    "Failed",
		OpSkipped:   "Skipped",
	}

	for status, want := range statuses {
		if got := status.String(); got != want {
			t.Fatalf("OpStatus(%d).String() = %q, want %q", int(status), got, want)
		}
	}
}

// TestExecuteDeleteToleratesMissingObject covers cleanup running twice, which
// happens whenever a Site is disabled and reconciled again.
func TestExecuteDeleteToleratesMissingObject(t *testing.T) {
	env, _ := recordingEnv(t)

	plan := NewPlan()
	plan.Add(Operation{Kind: OpDelete, Object: daemonSetObject("never-existed"), Component: "storage", Site: "edge"})

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if err := result.Err(); err != nil {
		t.Fatalf("deleting an absent object must be success, got %v", err)
	}
}

func TestExecuteReportsUnknownOperationKind(t *testing.T) {
	env, _ := recordingEnv(t)

	plan := NewPlan()
	plan.Add(Operation{Kind: OpKind(99), Object: configMapObject("weird"), Component: "a"})

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if err := result.Err(); err == nil {
		t.Fatal("expected an unknown operation kind to fail")
	}
}

// assertCalls compares recorded client calls against the expected sequence.
func assertCalls(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	}
}

// TestCreateIfAbsentReportsLosingTheRaceAsStale is a regression test.
//
// AlreadyExists was treated as plain success. It is not: planning read the
// object and found nothing, so everything the pass computed from that read is
// wrong. The operator hashes a ConfigMap's payload and stamps that hash on the
// workload mounting it, so losing this race stamped the hash of a payload the
// cluster does not have. The workload rolled to a hash matching nothing, and
// rolled again when a later pass finally read the real payload.
func TestCreateIfAbsentReportsLosingTheRaceAsStale(t *testing.T) {
	winner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: DefaultNamespace, Name: "config"},
		Data:       map[string]string{"payload": "the winner's"},
	}

	env, _ := recordingEnv(t, winner)

	desired := configMapObject("config")
	if err := unstructured.SetNestedStringMap(desired.Object,
		map[string]string{"payload": "the operator's"}, "data"); err != nil {
		t.Fatalf("build desired ConfigMap: %v", err)
	}

	plan := NewPlan()
	plan.Add(Operation{Kind: OpCreateIfAbsent, Object: desired, Component: "a"})

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The operation succeeded: the existing payload surviving is the whole
	// point of CreateIfAbsent, and nothing failed.
	if err := result.Err(); err != nil {
		t.Fatalf("losing the race is not a failure: %v", err)
	}

	if len(result.Stale) != 1 || result.Stale[0].Name != "config" {
		t.Fatalf("stale = %v, want the ConfigMap that already existed", result.Stale)
	}

	// The object now describes the cluster rather than the proposal, so
	// anything reading it back in this pass sees the truth.
	payload, _, err := unstructured.NestedString(desired.Object, "data", "payload")
	if err != nil {
		t.Fatalf("read back payload: %v", err)
	}

	if payload != "the winner's" {
		t.Fatalf("payload = %q, want the object refreshed from the cluster", payload)
	}
}

// TestCreateIfAbsentIsNotStaleWhenItWins confirms the ordinary path is quiet:
// creating the object is not a reason to re-plan.
func TestCreateIfAbsentIsNotStaleWhenItWins(t *testing.T) {
	env, _ := recordingEnv(t)

	plan := NewPlan()
	plan.Add(Operation{Kind: OpCreateIfAbsent, Object: configMapObject("config"), Component: "a"})

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(result.Stale) != 0 {
		t.Fatalf("stale = %v, want none when the create succeeded", result.Stale)
	}
}

// TestConflictDefersRatherThanFails pins that losing an optimistic lock is the
// mechanism working, not a failure.
//
// OpMergePatch takes an optimistic lock precisely so a concurrent edit produces
// a conflict instead of a silent clobber. Reporting that conflict as a failure
// gated the component's later tiers, turned every Site's condition False, and
// put the pass into error backoff, all for a race that the very next pass
// resolves. Machina merges the operator-resolved apiServerEndpoint into
// user-owned config content, so any edit to machina-config triggers it.
func TestConflictDefersRatherThanFails(t *testing.T) {
	base := configMapObject("cfg")
	base.SetResourceVersion("1")

	scheme := testScheme(t)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
				return apierrors.NewConflict(
					schema.GroupResource{Resource: "configmaps"}, "cfg", errors.New("concurrent edit"))
			},
		}).
		Build()

	env := &Env{Client: cl, Scheme: scheme, Namespace: DefaultNamespace}

	desired := configMapObject("cfg")
	desired.SetResourceVersion("1")

	plan := NewPlan()
	plan.Add(Operation{
		Kind: OpMergePatch, Object: desired, Base: base,
		Component: "machina",
	})

	// A workload for the same component in a later tier. It must still be
	// attempted: nothing failed, so nothing is gated.
	plan.Add(Operation{Kind: OpApply, Object: daemonSetObject("node"), Component: "machina"})

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if err := exec.Err(); err != nil {
		t.Fatalf("a lost optimistic lock must not be a pass error: %v", err)
	}

	if len(exec.Deferred) != 1 {
		t.Fatalf("deferred = %v, want the conflicted patch", exec.Deferred)
	}

	if skipped := exec.Skipped(); len(skipped) != 0 {
		t.Fatalf("skipped = %+v, want nothing gated by a deferral", skipped)
	}

	// The planning verdict survives, so the Site condition does not flap for
	// the second it takes to re-plan.
	result := CombineResult("machina", "", Reconciled(), exec)
	if !result.Ready {
		t.Fatalf("result = %+v, want the planning verdict to survive a deferral", result)
	}
}

// TestStaleCreateDefersItsDependents is a regression test.
//
// createIfAbsent reports errStale when it loses a create race, because
// everything the pass computed from "this object does not exist" is now wrong.
// The dependents were applied anyway: storage stamps the hash of the ConfigMap
// payload it read onto the DaemonSet that mounts it, so the DaemonSet rolled to
// a hash matching nothing and rolled again once a later pass read the real
// payload. Two rollouts of a host-networked DaemonSet for one lost race.
func TestStaleCreateDefersItsDependents(t *testing.T) {
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: DefaultNamespace},
		Data:       map[string]string{"config.yaml": "written by someone else"},
	}

	env, calls := recordingEnv(t, existing)

	config := configMapObject("cfg")

	workload := daemonSetObject("node")
	workload.SetAnnotations(map[string]string{"unbounded-cloud.io/config-hash": "hash-of-a-payload-that-is-not-there"})

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpCreateIfAbsent, Object: config, Component: "storage", Site: "edge"},
		Operation{
			Kind: OpApply, Object: workload, Component: "storage", Site: "edge",
			DependsOn: []ObjectRef{RefOf(config)},
		},
	)

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(exec.Stale) != 1 {
		t.Fatalf("stale = %v, want the lost create race recorded", exec.Stale)
	}

	if len(exec.Deferred) != 1 || exec.Deferred[0].Name != "node" {
		t.Fatalf("deferred = %v, want the dependent workload deferred", exec.Deferred)
	}

	for _, call := range *calls {
		if strings.Contains(call, "DaemonSet") && strings.HasPrefix(call, "apply") {
			t.Fatalf("the workload was applied with a hash for a payload the cluster does not have: %v", *calls)
		}
	}

	if err := exec.Err(); err != nil {
		t.Fatalf("a lost create race is not a failure: %v", err)
	}
}

// TestDependencyWaitsForEveryOperationOnTheObject pins that a ref is not
// treated as ready after only the first of several operations on it.
//
// A plan legitimately holds more than one operation on an object: a ConfigMap
// is created if absent and then merge-patched to add operator-owned keys.
// Ordering marked the ref done after the first, so a dependent whose own rank
// is lower than the second operation's became eligible early and was emitted
// between the two, observing a half-built object.
//
// Rank is the primary order, so this only bites when the dependent ranks below
// its dependency. A ConfigMap waiting on a workload is the shape that does it.
func TestDependencyWaitsForEveryOperationOnTheObject(t *testing.T) {
	workload := daemonSetObject("node")

	base := daemonSetObject("node")
	base.SetResourceVersion("1")

	patched := daemonSetObject("node")
	patched.SetResourceVersion("1")

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: workload, Component: "test"},
		Operation{Kind: OpMergePatch, Object: patched, Base: base, Component: "test"},
		Operation{
			Kind: OpApply, Object: configMapObject("after"), Component: "test",
			DependsOn: []ObjectRef{RefOf(workload)},
		},
	)

	order, err := plan.ExecutionOrder()
	if err != nil {
		t.Fatalf("ExecutionOrder: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(order), "\n")

	patchAt, dependentAt := -1, -1

	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "MergePatch") && strings.Contains(line, "node"):
			patchAt = i
		case strings.Contains(line, "ConfigMap") && strings.Contains(line, "after"):
			dependentAt = i
		}
	}

	if patchAt < 0 || dependentAt < 0 {
		t.Fatalf("order =\n%s\nwant both the patch and the dependent", order)
	}

	if dependentAt < patchAt {
		t.Fatalf("the dependent ran before the second operation on its dependency:\n%s", order)
	}
}
