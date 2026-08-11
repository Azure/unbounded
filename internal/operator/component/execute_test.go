// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
