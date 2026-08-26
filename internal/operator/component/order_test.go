// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// objectOfKind builds a namespaced object of an arbitrary kind, so ordering
// tests can plan the kinds the operator actually writes.
func objectOfKind(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(DefaultNamespace)
	obj.SetName(name)

	return obj
}

func namespaceObject(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Namespace"})
	obj.SetName(name)

	return obj
}

func serviceAccountObject(name string) *unstructured.Unstructured {
	return objectOfKind(schema.GroupVersionKind{Version: "v1", Kind: "ServiceAccount"}, name)
}

// applyEnv returns an Env that records every apply and delete and fails the
// ones named in broken, so a test can pin both the order operations ran in and
// which were never attempted.
//
// Applies are keyed "Kind/name" and deletes "delete Kind/name", matching the
// summaries these tests assert on.
func applyEnv(t *testing.T, broken map[string]error) (*Env, *[]string) {
	t.Helper()

	var attempted []string

	scheme := testScheme(t)
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

				return broken[name]
			},
			Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
				name := "delete " + obj.GetObjectKind().GroupVersionKind().Kind + "/" + obj.GetName()
				attempted = append(attempted, name)

				return broken[name]
			},
		}).
		Build()

	return &Env{Client: cl, Scheme: scheme, Namespace: DefaultNamespace}, &attempted
}

// TestExecuteOrdersByKindWithoutDeclaredDependencies is the point of inferring
// order from the kind: a component that declares no dependencies at all still
// gets its objects written in an order that works.
//
// Ordering used to be entirely the component author's responsibility. Getting
// it wrong was invisible, because nothing failed: the DaemonSet applied
// successfully and its pods then crash-looped unable to mount a ConfigMap that
// did not exist yet.
func TestExecuteOrdersByKindWithoutDeclaredDependencies(t *testing.T) {
	env, attempted := applyEnv(t, nil)

	// Planned in exactly the wrong order, with no DependsOn anywhere.
	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: daemonSetObject("agent"), Component: "a"},
		Operation{Kind: OpApply, Object: configMapObject("agent-config"), Component: "a"},
		Operation{Kind: OpApply, Object: serviceAccountObject("agent"), Component: "a"},
		Operation{Kind: OpApply, Object: namespaceObject(DefaultNamespace), Component: "a"},
	)

	if _, err := env.Execute(t.Context(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *attempted, []string{
		"Namespace/" + DefaultNamespace,
		"ServiceAccount/agent",
		"ConfigMap/agent-config",
		"DaemonSet/agent",
	})
}

// TestExecuteOrderingIsStableWithinATier checks that inferring order did not
// cost the ordering components rely on. Components plan deliberately, and
// within a tier their emission order and the registry order between them must
// survive.
func TestExecuteOrderingIsStableWithinATier(t *testing.T) {
	env, attempted := applyEnv(t, nil)

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: configMapObject("a-second"), Component: "a"},
		Operation{Kind: OpApply, Object: configMapObject("a-first"), Component: "a"},
		Operation{Kind: OpApply, Object: configMapObject("b-only"), Component: "b"},
	)

	if _, err := env.Execute(t.Context(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Not sorted by name: a-second precedes a-first because that is how the
	// component planned them.
	assertCalls(t, *attempted, []string{
		"ConfigMap/a-second",
		"ConfigMap/a-first",
		"ConfigMap/b-only",
	})
}

// TestExecuteSkipsAComponentsWorkloadWhenItsOwnConfigFails covers the inferred
// gate, and the limit on it. A component whose ConfigMap failed does not get
// its DaemonSet written, because those pods could not start; an unrelated
// component is untouched, because a contained failure must not become an
// outage.
func TestExecuteSkipsAComponentsWorkloadWhenItsOwnConfigFails(t *testing.T) {
	env, attempted := applyEnv(t, map[string]error{
		"ConfigMap/a-config": errors.New("apiserver said no"),
	})

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: configMapObject("a-config"), Component: "a"},
		Operation{Kind: OpApply, Object: daemonSetObject("a-agent"), Component: "a"},
		Operation{Kind: OpApply, Object: configMapObject("b-config"), Component: "b"},
		Operation{Kind: OpApply, Object: daemonSetObject("b-agent"), Component: "b"},
	)

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *attempted, []string{
		"ConfigMap/a-config",
		"ConfigMap/b-config",
		"DaemonSet/b-agent",
	})

	skipped := result.Skipped()
	if len(skipped) != 1 || skipped[0].Ref.Name != "a-agent" {
		t.Fatalf("skipped = %v, want only a-agent", skipped)
	}

	if !strings.Contains(skipped[0].Err.Error(), "config") {
		t.Fatalf("skip reason = %q, want it to name the tier that failed", skipped[0].Err)
	}
}

// TestExecuteFailedNamespaceGatesEveryComponent covers the one gate that is not
// scoped to a single component: nothing can be written into a namespace that
// does not exist, whoever planned it.
func TestExecuteFailedNamespaceGatesEveryComponent(t *testing.T) {
	env, attempted := applyEnv(t, map[string]error{
		"Namespace/" + DefaultNamespace: errors.New("quota exceeded"),
	})

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: namespaceObject(DefaultNamespace), Component: "a"},
		Operation{Kind: OpApply, Object: configMapObject("b-config"), Component: "b"},
		Operation{Kind: OpApply, Object: daemonSetObject("c-agent"), Component: "c"},
		// A cluster-scoped object is unaffected: it does not live in the
		// namespace that failed.
		Operation{Kind: OpApply, Object: namespaceObject("other"), Component: "d"},
	)

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *attempted, []string{
		"Namespace/" + DefaultNamespace,
		"Namespace/other",
	})

	if got := len(result.Skipped()); got != 2 {
		t.Fatalf("skipped = %d, want the two namespaced objects", got)
	}
}

// TestExecuteGatingIsPerSite checks that the inferred gate does not spread
// between Sites. A per-Site component plans separately for each Site, and one
// Site's failure says nothing about another's.
func TestExecuteGatingIsPerSite(t *testing.T) {
	env, attempted := applyEnv(t, map[string]error{
		"ConfigMap/config-east": errors.New("apiserver said no"),
	})

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: configMapObject("config-east"), Component: "s", Site: "east"},
		Operation{Kind: OpApply, Object: daemonSetObject("agent-east"), Component: "s", Site: "east"},
		Operation{Kind: OpApply, Object: configMapObject("config-west"), Component: "s", Site: "west"},
		Operation{Kind: OpApply, Object: daemonSetObject("agent-west"), Component: "s", Site: "west"},
	)

	if _, err := env.Execute(t.Context(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *attempted, []string{
		"ConfigMap/config-east",
		"ConfigMap/config-west",
		"DaemonSet/agent-west",
	})
}

// TestExecuteSkipsLaterOperationsOnAFailedObject covers the third gate. A plan
// legitimately holds more than one operation on the same object, and patching
// one whose creation failed produces only a second, more confusing error.
func TestExecuteSkipsLaterOperationsOnAFailedObject(t *testing.T) {
	env, attempted := applyEnv(t, map[string]error{
		"ConfigMap/twice": errors.New("apiserver said no"),
	})

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: configMapObject("twice"), Component: "a"},
		Operation{Kind: OpApply, Object: configMapObject("twice"), Component: "a"},
	)

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *attempted, []string{"ConfigMap/twice"})

	if got := len(result.Skipped()); got != 1 {
		t.Fatalf("skipped = %d, want the second operation on the same object", got)
	}
}

// TestExecuteAliasesDoNotLeakBetweenOperationsOnTheSameObject is a regression
// test.
//
// Deduplicated contributors were held in a map keyed by ObjectRef, but an
// ObjectRef does not identify an operation: a ConfigMap is created if absent
// and then merge-patched. The second operation inherited the first one's
// contributors, so it reported results for Sites that had nothing to do with
// it, and those Sites saw an outcome for a write that was never planned for
// them.
func TestExecuteAliasesDoNotLeakBetweenOperationsOnTheSameObject(t *testing.T) {
	env, _ := recordingEnv(t)

	base := configMapObject("cfg")
	base.SetResourceVersion("1")

	shared := func(site string) Operation {
		return Operation{
			Kind:      OpCreateIfAbsent,
			Object:    configMapObject("cfg"),
			Component: "a",
			Site:      site,
			SharedKey: "cfg",
		}
	}

	plan := NewPlan()
	plan.Add(
		shared("east"),
		shared("west"),
		// A second operation on the same object, planned for no Site.
		Operation{Kind: OpMergePatch, Object: configMapObject("cfg"), Base: base, Component: "a"},
	)

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var patchSites []string

	for _, r := range result.Results {
		if r.Kind == OpMergePatch {
			patchSites = append(patchSites, r.Site)
		}
	}

	if len(patchSites) != 1 || patchSites[0] != "" {
		t.Fatalf("merge patch attributed to sites %q, want exactly one result with no Site", patchSites)
	}

	// The shared create still reports to both contributors, which is the
	// behavior the aliasing exists for.
	var createSites []string

	for _, r := range result.Results {
		if r.Kind == OpCreateIfAbsent {
			createSites = append(createSites, r.Site)
		}
	}

	assertCalls(t, createSites, []string{"east", "west"})
}

// TestCombineResultIsScopedToOneSite is a regression test.
//
// Results were matched on the component alone, so a failure writing one Site's
// DaemonSet turned every other Site's condition False, with a message naming an
// object belonging to a Site the reader was not looking at.
func TestCombineResultIsScopedToOneSite(t *testing.T) {
	exec := ExecutionResult{Results: []OperationResult{
		{
			Ref:       ObjectRef{Name: "agent-east"},
			Kind:      OpApply,
			Component: "s",
			Site:      "east",
			Status:    OpFailed,
			Err:       errors.New("apiserver said no"),
		},
		{Ref: ObjectRef{Name: "agent-west"}, Kind: OpApply, Component: "s", Site: "west", Status: OpSucceeded},
	}}

	if res := CombineResult("s", "east", Reconciled(), exec); res.Err == nil {
		t.Fatal("the Site that failed must report the failure")
	}

	res := CombineResult("s", "west", Reconciled(), exec)
	if res.Err != nil {
		t.Fatalf("an unrelated Site must not inherit another Site's failure: %v", res.Err)
	}

	if !res.Ready {
		t.Fatalf("west = %+v, want the planning verdict", res)
	}
}

// TestCombineResultCountsClusterScopedOperationsForEverySite checks the other
// half of the filter: an operation planned for no Site belongs to all of them.
func TestCombineResultCountsClusterScopedOperationsForEverySite(t *testing.T) {
	exec := ExecutionResult{Results: []OperationResult{{
		Ref:       ObjectRef{Name: "cluster-role"},
		Kind:      OpApply,
		Component: "n",
		Status:    OpFailed,
		Err:       errors.New("forbidden"),
	}}}

	for _, site := range []string{"east", "west"} {
		if res := CombineResult("n", site, Reconciled(), exec); res.Err == nil {
			t.Fatalf("site %s: a cluster-scoped failure must reach every Site", site)
		}
	}
}

// TestCombineResultReportsSkippedWithoutFailing covers the verdict for a
// component that wrote nothing because something it depends on failed.
//
// Reporting Reconciled would claim the component wrote what it planned when it
// did not. Reporting a reconcile error would duplicate the failure that is
// already reported against the component that actually failed, and would bury
// the real cause.
func TestCombineResultReportsSkippedWithoutFailing(t *testing.T) {
	exec := ExecutionResult{Results: []OperationResult{{
		Ref:       ObjectRef{GVK: schema.GroupVersionKind{Kind: "DaemonSet"}, Namespace: "ns", Name: "agent"},
		Kind:      OpApply,
		Component: "s",
		Site:      "east",
		Status:    OpSkipped,
		Err:       errors.New("s (site east) did not write its config successfully"),
	}}}

	res := CombineResult("s", "east", Reconciled(), exec)

	if res.Ready {
		t.Fatal("a component that wrote nothing must not report Reconciled")
	}

	if res.Err != nil {
		t.Fatalf("a skip must not be reported as this component's error: %v", res.Err)
	}

	if res.Reason != ReasonDependencyNotWritten {
		t.Fatalf("reason = %q, want %q", res.Reason, ReasonDependencyNotWritten)
	}

	if !strings.Contains(res.Message, "DaemonSet/ns/agent") {
		t.Fatalf("message = %q, want it to name what was not written", res.Message)
	}
}

func webhookObject(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingWebhookConfiguration",
	})
	obj.SetName(name)

	return obj
}

func policyObject(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "admissionregistration.k8s.io", Version: "v1", Kind: "ValidatingAdmissionPolicy",
	})
	obj.SetName(name)

	return obj
}

func serviceObject(name string) *unstructured.Unstructured {
	return objectOfKind(schema.GroupVersionKind{Version: "v1", Kind: "Service"}, name)
}

// TestExecuteRegistersAdmissionAfterItsBackend is a regression test.
//
// Admission and aggregation registration points at a backend, so it has to
// follow that backend. An earlier revision treated webhook configurations as
// schema and ran them near the front, which reversed the order the manifests
// had always used. Both net webhooks are failurePolicy: Ignore, so the window
// between registering them and starting the Deployment behind them is a window
// where they silently enforce nothing.
func TestExecuteRegistersAdmissionAfterItsBackend(t *testing.T) {
	env, attempted := applyEnv(t, nil)

	// Planned in the order that used to be produced: registration first.
	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: webhookObject("net-validating"), Component: "net"},
		Operation{Kind: OpApply, Object: policyObject("net-create-restriction"), Component: "net"},
		Operation{Kind: OpApply, Object: serviceObject("net-controller"), Component: "net"},
		Operation{Kind: OpApply, Object: daemonSetObject("net-node"), Component: "net"},
	)

	if _, err := env.Execute(t.Context(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *attempted, []string{
		"Service/net-controller",
		"DaemonSet/net-node",
		"ValidatingWebhookConfiguration/net-validating",
		"ValidatingAdmissionPolicy/net-create-restriction",
	})
}

// TestExecuteFailedRegistrationGatesNothing pins the other half of that
// decision. Registration runs last, so nothing depends on it, and a cluster
// that rejects an admission policy still gets its workloads. The operation
// still fails, so the component reports the failure rather than hiding it.
func TestExecuteFailedRegistrationGatesNothing(t *testing.T) {
	env, attempted := applyEnv(t, map[string]error{
		"ValidatingAdmissionPolicy/net-create-restriction": errors.New("no matches for kind"),
	})

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpApply, Object: policyObject("net-create-restriction"), Component: "net"},
		Operation{Kind: OpApply, Object: webhookObject("net-validating"), Component: "net"},
		Operation{Kind: OpApply, Object: daemonSetObject("net-node"), Component: "net"},
	)

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Everything was attempted, including the sibling registration.
	assertCalls(t, *attempted, []string{
		"DaemonSet/net-node",
		"ValidatingAdmissionPolicy/net-create-restriction",
		"ValidatingWebhookConfiguration/net-validating",
	})

	if got := len(result.Skipped()); got != 0 {
		t.Fatalf("skipped %d operations, want none: registration is last so nothing depends on it", got)
	}

	// It is still reported, so the component does not claim success.
	if len(result.Failed()) != 1 {
		t.Fatalf("failed = %v, want the policy reported", result.Failed())
	}
}

// TestExecuteRemovesInReverseOrder is a regression test.
//
// Removals used to share one rank, so a failed workload delete did not stop the
// delete of the ConfigMap that workload was still mounting. Cleanup could take
// the configuration out from under a surviving pod.
func TestExecuteRemovesInReverseOrder(t *testing.T) {
	env, attempted := applyEnv(t, nil)

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpDelete, Object: configMapObject("agent-config"), Component: "storage", Site: "east"},
		Operation{Kind: OpDelete, Object: serviceAccountObject("agent"), Component: "storage", Site: "east"},
		Operation{Kind: OpDelete, Object: daemonSetObject("agent"), Component: "storage", Site: "east"},
	)

	if _, err := env.Execute(t.Context(), plan); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The workload goes first, then what it consumed.
	assertCalls(t, *attempted, []string{
		"delete DaemonSet/agent",
		"delete ConfigMap/agent-config",
		"delete ServiceAccount/agent",
	})
}

// TestExecuteFailedWorkloadDeleteGatesItsConfigDelete is the failure half of
// the same property.
func TestExecuteFailedWorkloadDeleteGatesItsConfigDelete(t *testing.T) {
	env, attempted := applyEnv(t, map[string]error{
		"delete DaemonSet/agent": errors.New("apiserver said no"),
	})

	plan := NewPlan()
	plan.Add(
		Operation{Kind: OpDelete, Object: daemonSetObject("agent"), Component: "storage", Site: "east"},
		Operation{Kind: OpDelete, Object: configMapObject("agent-config"), Component: "storage", Site: "east"},
	)

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	assertCalls(t, *attempted, []string{"delete DaemonSet/agent"})

	skipped := result.Skipped()
	if len(skipped) != 1 || skipped[0].Ref.Name != "agent-config" {
		t.Fatalf("skipped = %v, want the ConfigMap the surviving workload still mounts", skipped)
	}
}

// TestExecuteSharedFailureGatesEveryContributingSite is a regression test.
//
// metalman and storage plan identical support RBAC for every Site, and it is
// deduplicated so the write happens once. The failure was recorded against the
// retained operation's Site alone, so every other Site went on to apply
// workloads referencing a ServiceAccount that had never been created.
func TestExecuteSharedFailureGatesEveryContributingSite(t *testing.T) {
	env, attempted := applyEnv(t, map[string]error{
		"ServiceAccount/shared": errors.New("apiserver said no"),
	})

	shared := func(site string) Operation {
		return Operation{
			Kind:      OpApply,
			Object:    serviceAccountObject("shared"),
			Component: "storage",
			Site:      site,
			SharedKey: "storage/shared/sa",
		}
	}

	plan := NewPlan()
	plan.Add(
		shared("east"),
		shared("west"),
		Operation{Kind: OpApply, Object: daemonSetObject("agent-east"), Component: "storage", Site: "east"},
		Operation{Kind: OpApply, Object: daemonSetObject("agent-west"), Component: "storage", Site: "west"},
	)

	result, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The ServiceAccount was attempted once, and neither workload followed it.
	assertCalls(t, *attempted, []string{"ServiceAccount/shared"})

	skipped := map[string]bool{}
	for _, r := range result.Skipped() {
		skipped[r.Ref.Name] = true
	}

	if !skipped["agent-east"] || !skipped["agent-west"] {
		t.Fatalf("skipped = %v, want both Sites gated by the one shared failure", result.Skipped())
	}
}
