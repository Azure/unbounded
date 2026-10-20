// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package metalman

import (
	"context"
	"errors"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	vapRef  = "ValidatingAdmissionPolicy/" + bootstrapPolicyName
	vapbRef = "ValidatingAdmissionPolicyBinding/" + bootstrapPolicyName
	crbRef  = "ClusterRoleBinding/" + BootstrapMachineBindingName
)

func livePolicy(t *testing.T) *admissionv1.ValidatingAdmissionPolicy {
	t.Helper()

	obj := &admissionv1.ValidatingAdmissionPolicy{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(supportObject(t, "ValidatingAdmissionPolicy", bootstrapPolicyName).Object, obj); err != nil {
		t.Fatalf("convert policy: %v", err)
	}

	return obj
}

func liveBinding(t *testing.T) *admissionv1.ValidatingAdmissionPolicyBinding {
	t.Helper()

	obj := &admissionv1.ValidatingAdmissionPolicyBinding{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(supportObject(t, "ValidatingAdmissionPolicyBinding", bootstrapPolicyName).Object, obj); err != nil {
		t.Fatalf("convert binding: %v", err)
	}

	return obj
}

// withDefaults fills in what the API server persists, so a healthy fixture
// matches the live shape rather than the manifest.
func withDefaults(policy *admissionv1.ValidatingAdmissionPolicy, binding *admissionv1.ValidatingAdmissionPolicyBinding) {
	policy.Spec.MatchConstraints.MatchPolicy = ptr.To(admissionv1.Equivalent)
	policy.Spec.MatchConstraints.NamespaceSelector = &metav1.LabelSelector{}
	policy.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{}

	for i := range policy.Spec.MatchConstraints.ResourceRules {
		policy.Spec.MatchConstraints.ResourceRules[i].Scope = ptr.To(admissionv1.AllScopes)
	}

	binding.Spec.MatchResources = &admissionv1.MatchResources{
		MatchPolicy:       ptr.To(admissionv1.Equivalent),
		NamespaceSelector: &metav1.LabelSelector{},
		ObjectSelector:    &metav1.LabelSelector{},
	}
}

func opsOf(plan *component.Plan, kind component.OpKind) map[string]int {
	out := map[string]int{}

	for _, op := range plan.Operations {
		if op.Kind == kind {
			out[op.Object.GetKind()+"/"+op.Object.GetName()]++
		}
	}

	return out
}

// planPaths returns the bootstrap component's plans with and without Sites,
// the two paths that emit the restriction.
func planPaths(t *testing.T, env *component.Env) map[string]*component.Plan {
	t.Helper()

	plans := map[string]*component.Plan{}

	plan, _, err := BootstrapComponent{}.Plan(t.Context(), env, []unboundedv1alpha3.Site{*enabledSite(), *disabledSite("rack-b")})
	if err != nil {
		t.Fatalf("plan with Sites: %v", err)
	}

	plans["Sites"] = plan

	plan, _, err = BootstrapComponent{}.Plan(t.Context(), env, []unboundedv1alpha3.Site{})
	if err != nil {
		t.Fatalf("plan without Sites: %v", err)
	}

	plans["NoSites"] = plan

	return plans
}

func TestHealthyRestrictionPlansNoRepair(t *testing.T) {
	policy, binding := livePolicy(t), liveBinding(t)
	withDefaults(policy, binding)

	applied, deletes := map[string]int{}, map[string]int{}
	cl := bootstrapClient(t, "", applied, deletes, policy, binding, bootstrapGrant())
	env := &component.Env{Client: cl, Namespace: component.DefaultNamespace}

	for path, plan := range planPaths(t, env) {
		if patches := opsOf(plan, component.OpMergePatch); len(patches) > 0 {
			t.Errorf("%s: healthy restriction planned repairs %v", path, patches)
		}

		if opsOf(plan, component.OpDelete)[crbRef] > 0 || opsOf(plan, component.OpApply)[crbRef] != 1 {
			t.Errorf("%s: healthy restriction does not apply the grant:\n%s", path, plan.Summary())
		}
	}
}

func TestDriftedRestrictionIsRepairedBeforeGrant(t *testing.T) {
	cases := map[string]struct {
		mutate func(*admissionv1.ValidatingAdmissionPolicy, *admissionv1.ValidatingAdmissionPolicyBinding)
		repair string
	}{
		"binding resource rules": {
			mutate: func(_ *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
				b.Spec.MatchResources.ResourceRules = []admissionv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionv1.RuleWithOperations{
						Operations: []admissionv1.OperationType{admissionv1.Create},
						Rule: admissionv1.Rule{
							APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"},
							Scope: ptr.To(admissionv1.AllScopes),
						},
					},
				}}
			},
			repair: vapbRef,
		},
		"binding object selector": {
			mutate: func(_ *admissionv1.ValidatingAdmissionPolicy, b *admissionv1.ValidatingAdmissionPolicyBinding) {
				b.Spec.MatchResources.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}}
			},
			repair: vapbRef,
		},
		"policy failure policy": {
			mutate: func(p *admissionv1.ValidatingAdmissionPolicy, _ *admissionv1.ValidatingAdmissionPolicyBinding) {
				p.Spec.FailurePolicy = ptr.To(admissionv1.Ignore)
			},
			repair: vapRef,
		},
		"policy object selector": {
			mutate: func(p *admissionv1.ValidatingAdmissionPolicy, _ *admissionv1.ValidatingAdmissionPolicyBinding) {
				p.Spec.MatchConstraints.ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}}
			},
			repair: vapRef,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			policy, binding := livePolicy(t), liveBinding(t)
			withDefaults(policy, binding)
			tc.mutate(policy, binding)

			applied, deletes := map[string]int{}, map[string]int{}
			cl := bootstrapClient(t, "", applied, deletes, policy, binding, bootstrapGrant())
			env := &component.Env{Client: cl, Namespace: component.DefaultNamespace}

			for path, plan := range planPaths(t, env) {
				if got := opsOf(plan, component.OpMergePatch); got[tc.repair] != 1 || len(got) != 1 {
					t.Errorf("%s: repairs = %v, want only %s", path, got, tc.repair)
				}

				if opsOf(plan, component.OpApply)[crbRef] > 0 || opsOf(plan, component.OpDelete)[crbRef] != 1 {
					t.Errorf("%s: drifted restriction does not revoke the grant:\n%s", path, plan.Summary())
				}
			}

			// First pass: repair the spec and revoke the grant.
			if res := reconcile(t, env, enabledSite()); res.Err != nil {
				t.Fatalf("first pass: %+v", res)
			}

			err := cl.Get(t.Context(), client.ObjectKey{Name: BootstrapMachineBindingName}, &rbacv1.ClusterRoleBinding{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("grant after drift = %v, want NotFound", err)
			}

			gotPolicy, gotBinding := &admissionv1.ValidatingAdmissionPolicy{}, &admissionv1.ValidatingAdmissionPolicyBinding{}
			if err := cl.Get(t.Context(), client.ObjectKey{Name: bootstrapPolicyName}, gotPolicy); err != nil {
				t.Fatal(err)
			}

			if err := cl.Get(t.Context(), client.ObjectKey{Name: bootstrapPolicyName}, gotBinding); err != nil {
				t.Fatal(err)
			}

			wantPolicy, wantBinding := livePolicy(t), liveBinding(t)
			if !equality.Semantic.DeepEqual(component.EffectiveAdmissionSpec(wantPolicy), component.EffectiveAdmissionSpec(gotPolicy)) {
				t.Fatalf("policy not repaired: %+v", gotPolicy.Spec)
			}

			if !equality.Semantic.DeepEqual(component.EffectiveAdmissionSpec(wantBinding), component.EffectiveAdmissionSpec(gotBinding)) {
				t.Fatalf("binding not repaired: %+v", gotBinding.Spec)
			}

			if m := gotBinding.Spec.MatchResources; m != nil && (len(m.ResourceRules) > 0 || (m.ObjectSelector != nil && len(m.ObjectSelector.MatchLabels) > 0)) {
				t.Fatalf("extra binding constraints survived repair: %+v", m)
			}

			// Second pass: verified, so the grant is applied and nothing is patched.
			plan, _, err := BootstrapComponent{}.Plan(t.Context(), env, []unboundedv1alpha3.Site{*enabledSite()})
			if err != nil {
				t.Fatal(err)
			}

			if got := opsOf(plan, component.OpMergePatch); len(got) > 0 {
				t.Fatalf("repaired restriction still planned repairs %v", got)
			}

			if opsOf(plan, component.OpApply)[crbRef] != 1 {
				t.Fatalf("verified restriction does not regrant:\n%s", plan.Summary())
			}
		})
	}
}

func TestTerminatingRestrictionWithholdsGrant(t *testing.T) {
	policy, binding := livePolicy(t), liveBinding(t)
	withDefaults(policy, binding)
	binding.Finalizers = []string{"example.com/hold"}
	binding.DeletionTimestamp = ptr.To(metav1.Now())

	applied, deletes := map[string]int{}, map[string]int{}
	cl := bootstrapClient(t, "", applied, deletes, policy, binding, bootstrapGrant())
	env := &component.Env{Client: cl, Namespace: component.DefaultNamespace}

	for path, plan := range planPaths(t, env) {
		if opsOf(plan, component.OpApply)[crbRef] > 0 || opsOf(plan, component.OpDelete)[crbRef] != 1 {
			t.Errorf("%s: terminating restriction does not revoke the grant:\n%s", path, plan.Summary())
		}
	}
}

// TestMissingLiveRestrictionWithholdsGrantDespiteStaleCache verifies that a
// restriction missing from the apiserver withholds the grant even while the
// cached client still holds a matching copy. The executor compares applies
// against the cache, so it may skip recreating the restriction this pass; the
// grant must not go live alongside it.
func TestMissingLiveRestrictionWithholdsGrantDespiteStaleCache(t *testing.T) {
	for _, missing := range []string{vapRef, vapbRef, "both"} {
		t.Run(missing, func(t *testing.T) {
			policy, binding := livePolicy(t), liveBinding(t)
			withDefaults(policy, binding)

			var live []client.Object
			if missing != vapRef && missing != "both" {
				live = append(live, policy.DeepCopy())
			}

			if missing != vapbRef && missing != "both" {
				live = append(live, binding.DeepCopy())
			}

			applied, deletes := map[string]int{}, map[string]int{}
			cached := bootstrapClient(t, "", applied, deletes, policy, binding, bootstrapGrant())
			apiReader := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(append(live, bootstrapGrant())...).Build()
			env := &component.Env{Client: cached, APIReader: apiReader, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

			for path, plan := range planPaths(t, env) {
				if opsOf(plan, component.OpApply)[crbRef] > 0 || opsOf(plan, component.OpDelete)[crbRef] != 1 {
					t.Errorf("%s: missing live restriction does not revoke the grant:\n%s", path, plan.Summary())
				}

				for _, ref := range []string{vapRef, vapbRef} {
					if opsOf(plan, component.OpApply)[ref] != 1 {
						t.Errorf("%s: %s not applied:\n%s", path, ref, plan.Summary())
					}
				}
			}

			reconcile(t, env, enabledSite())

			if applied[crbRef] != 0 {
				t.Fatalf("grant applied %d times, want 0", applied[crbRef])
			}

			err := cached.Get(t.Context(), client.ObjectKey{Name: BootstrapMachineBindingName}, &rbacv1.ClusterRoleBinding{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("grant after pass = %v, want NotFound", err)
			}
		})
	}
}

// TestVerificationFailureFailsResultAndRecovers verifies that a failed live
// read of the restriction keeps the grant withheld and reports a failed
// result from every path, so the pass is retried with backoff even when
// the grant is already absent and the pass writes nothing that fires a watch.
// Once the read succeeds, the grant is applied.
func TestVerificationFailureFailsResultAndRecovers(t *testing.T) {
	policy, binding := livePolicy(t), liveBinding(t)
	withDefaults(policy, binding)

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: BootstrapServiceAccountName, Namespace: component.DefaultNamespace}}
	applied, deletes := map[string]int{}, map[string]int{}
	cached := bootstrapClient(t, "", applied, deletes, policy, binding, sa)

	readErr := errors.New("apiserver unavailable")
	failing := true
	apiReader := interceptor.NewClient(
		fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(policy.DeepCopy(), binding.DeepCopy(), sa.DeepCopy()).Build(),
		interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*admissionv1.ValidatingAdmissionPolicyBinding); ok && failing {
					return readErr
				}

				return c.Get(ctx, key, obj, opts...)
			},
		},
	)
	env := &component.Env{Client: cached, APIReader: apiReader, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

	results := map[string]func() (*component.Plan, component.Result, error){
		"Sites": func() (*component.Plan, component.Result, error) {
			return BootstrapComponent{}.Plan(t.Context(), env, []unboundedv1alpha3.Site{*enabledSite(), *disabledSite("rack-b")})
		},
		"NoSites": func() (*component.Plan, component.Result, error) {
			return BootstrapComponent{}.Plan(t.Context(), env, nil)
		},
	}

	for path, planFn := range results {
		plan, res, err := planFn()
		if err != nil {
			t.Fatalf("%s: planning error %v, want the plan kept and a failed result", path, err)
		}

		if res.Ready || res.Reason != component.ReasonReconcileError || !errors.Is(res.Err, readErr) {
			t.Errorf("%s: result = %+v, want failed with the read error", path, res)
		}

		if opsOf(plan, component.OpApply)[crbRef] > 0 || opsOf(plan, component.OpDelete)[crbRef] != 1 {
			t.Errorf("%s: failed verification does not revoke the grant:\n%s", path, plan.Summary())
		}
	}

	// The executed pass succeeds, but the combined result still fails, so the
	// reconciler requeues with backoff.
	if res := reconcile(t, env, enabledSite()); !errors.Is(res.Err, readErr) {
		t.Fatalf("failed pass result = %+v, want the read error", res)
	}

	if applied[crbRef] != 0 {
		t.Fatalf("grant applied %d times while verification failed, want 0", applied[crbRef])
	}

	// The retry reads the restriction and restores the grant.
	failing = false

	for path, planFn := range results {
		plan, res, err := planFn()
		if err != nil || res.Err != nil {
			t.Fatalf("%s: recovered plan = %+v, %v, want success", path, res, err)
		}

		if opsOf(plan, component.OpDelete)[crbRef] > 0 || opsOf(plan, component.OpApply)[crbRef] != 1 {
			t.Errorf("%s: recovered verification does not apply the grant:\n%s", path, plan.Summary())
		}
	}

	if res := reconcile(t, env, enabledSite()); res.Err != nil || !res.Ready {
		t.Fatalf("recovered pass result = %+v, want ready", res)
	}

	if applied[crbRef] != 1 {
		t.Fatalf("grant applied %d times after recovery, want 1", applied[crbRef])
	}
}
