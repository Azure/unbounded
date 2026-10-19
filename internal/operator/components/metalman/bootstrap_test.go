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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
)

// bootstrapClient returns a client seeded with objs that records applies and
// deletes. Applies of failing are rejected; other applies are not stored.
func bootstrapClient(t *testing.T, failing string, applied, deletes map[string]int, objs ...client.Object) client.Client {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				named := obj.(interface { //nolint:errcheck
					GetKind() string
					GetName() string
				})

				name := named.GetKind() + "/" + named.GetName()
				if name == failing {
					return errors.New("admission policy rejected")
				}

				applied[name]++

				return nil
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes[obj.GetObjectKind().GroupVersionKind().Kind+"/"+obj.GetName()]++

				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
}

// runBootstrap plans and executes one pass of the bootstrap component.
func runBootstrap(t *testing.T, env *component.Env, sites []unboundedv1alpha3.Site) (*component.Plan, error) {
	t.Helper()

	plan, _, err := BootstrapComponent{}.Plan(t.Context(), env, sites)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	return plan, exec.Err()
}

func staleAutoApprove() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: StaleAutoApproveBindingName}}
}

func bootstrapGrant() *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: BootstrapMachineBindingName}}
}

// TestBootstrapZeroSitesRestrictsLegacyAccess verifies the upgrade where the
// last Site was deleted earlier: the stale auto-approval binding is removed and
// the retained grant is revoked until the admission restriction is live, then
// kept behind it, stably.
func TestBootstrapZeroSitesRestrictsLegacyAccess(t *testing.T) {
	for name, objs := range map[string][]client.Object{
		"stale binding":   {staleAutoApprove(), bootstrapGrant()},
		"grant only":      {bootstrapGrant()},
		"service account": {&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: BootstrapServiceAccountName, Namespace: component.DefaultNamespace}}},
	} {
		t.Run(name, func(t *testing.T) {
			applied, deletes := map[string]int{}, map[string]int{}
			cl := bootstrapClient(t, "", applied, deletes, objs...)
			env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

			// First pass: the restriction is created and the grant withheld.
			if _, err := runBootstrap(t, env, nil); err != nil {
				t.Fatalf("pass 0: %v", err)
			}

			err := cl.Get(t.Context(), client.ObjectKey{Name: StaleAutoApproveBindingName}, &rbacv1.ClusterRoleBinding{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("get stale binding = %v, want NotFound", err)
			}

			if applied[crbRef] != 0 {
				t.Fatalf("grant applied %d times before the restriction was live, want 0", applied[crbRef])
			}

			err = cl.Get(t.Context(), client.ObjectKey{Name: BootstrapMachineBindingName}, &rbacv1.ClusterRoleBinding{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("get grant = %v, want NotFound", err)
			}

			// The fake client does not store applies, so make the restriction
			// and the bootstrap ServiceAccount live the way the first pass's
			// applies would.
			live := liveRestriction(t)
			if name != "service account" {
				live = append(live, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: BootstrapServiceAccountName, Namespace: component.DefaultNamespace}})
			}

			for _, obj := range live {
				if err := cl.Create(t.Context(), obj); err != nil {
					t.Fatal(err)
				}
			}

			// Later passes: verified, so the grant is applied and never revoked.
			for pass := 1; pass < 3; pass++ {
				if _, err := runBootstrap(t, env, nil); err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
			}

			for _, want := range []string{vapRef, vapbRef} {
				if applied[want] != 3 {
					t.Fatalf("%s applied %d times, want once per pass", want, applied[want])
				}
			}

			if applied[crbRef] != 2 {
				t.Fatalf("grant applied %d times, want once per verified pass", applied[crbRef])
			}

			// Only the first pass revokes.
			if n := deletes[crbRef]; n != 1 {
				t.Fatalf("bootstrap grant deleted %d times, want 1", n)
			}
		})
	}
}

// TestBootstrapZeroSitesNoLegacyIsNoop verifies a cluster that never ran
// metalman gets no metalman RBAC or admission policy installed.
func TestBootstrapZeroSitesNoLegacyIsNoop(t *testing.T) {
	applied, deletes := map[string]int{}, map[string]int{}
	cl := bootstrapClient(t, "", applied, deletes)
	env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

	plan, res, err := BootstrapComponent{}.Plan(t.Context(), env, nil)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if plan.Len() != 0 {
		t.Fatalf("plan has %d operations, want 0:\n%s", plan.Len(), plan.Summary())
	}

	if res.Reason != component.ReasonNoSites {
		t.Fatalf("result = %+v, want NoSites", res)
	}
}

// TestBootstrapWithSitesPlansSupportOnce verifies that with Sites present the
// cluster component plans the shared support set and the per-Site component
// plans none of it, so each shared object has exactly one planned operation.
func TestBootstrapWithSitesPlansSupportOnce(t *testing.T) {
	applied, deletes := map[string]int{}, map[string]int{}

	objs := append([]client.Object{staleAutoApprove(), bootstrapGrant()}, liveRestriction(t)...)
	cl := bootstrapClient(t, "", applied, deletes, objs...)
	env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

	sites := []*unboundedv1alpha3.Site{enabledSite(), disabledSite("rack-b")}

	results, plan, err := planPass(t, env, sites...)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if res := results[BootstrapComponent{}.Name()]; !res.Ready || res.Reason != component.ReasonReconciled {
		t.Fatalf("bootstrap result = %+v, want reconciled", res)
	}

	for _, op := range plan.Operations {
		if op.SharedKey != "" && op.Component != (BootstrapComponent{}).Name() {
			t.Fatalf("%s planned shared %s; only the bootstrap component may", op.Component, op.Ref())
		}
	}

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if err := exec.Err(); err != nil {
		t.Fatalf("pass: %v", err)
	}

	if n := applied["ValidatingAdmissionPolicy/"+bootstrapPolicyName]; n != 1 {
		t.Fatalf("policy applied %d times, want 1", n)
	}

	if n := deletes["ClusterRoleBinding/"+StaleAutoApproveBindingName]; n != 1 {
		t.Fatalf("stale binding deleted %d times, want 1", n)
	}

	if n := deletes["ClusterRoleBinding/"+BootstrapMachineBindingName]; n != 0 {
		t.Fatalf("bootstrap grant deleted %d times, want 0", n)
	}
}

// TestMultiSiteDifferingReadsStillRevoke verifies a pass over several Sites
// where the live restriction read fails once and then succeeds. Verification
// runs once per pass, so the grant is planned one way only: the pass executes,
// the grant is revoked, the stale auto-approval binding is removed, and the
// result fails so the pass is retried.
//
// When each Site verified separately, the first Site planned a delete of the
// grant and the next an apply, and the executor rejected the whole plan.
func TestMultiSiteDifferingReadsStillRevoke(t *testing.T) {
	policy, binding := livePolicy(t), liveBinding(t)
	withDefaults(policy, binding)

	applied, deletes := map[string]int{}, map[string]int{}
	cached := bootstrapClient(t, "", applied, deletes, policy, binding, staleAutoApprove(), bootstrapGrant())

	readErr := errors.New("apiserver unavailable")
	reads := 0
	apiReader := interceptor.NewClient(
		fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(policy.DeepCopy(), binding.DeepCopy()).Build(),
		interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*admissionv1.ValidatingAdmissionPolicyBinding); ok {
					reads++
					if reads == 1 {
						return readErr
					}
				}

				return c.Get(ctx, key, obj, opts...)
			},
		},
	)
	env := &component.Env{Client: cached, APIReader: apiReader, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

	second := enabledSite()
	second.Name = "rack-b"

	results, plan, err := planPass(t, env, enabledSite(), second, disabledSite("rack-c"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if reads != 1 {
		t.Fatalf("restriction binding read %d times in one pass, want once", reads)
	}

	if res := results[BootstrapComponent{}.Name()]; !errors.Is(res.Err, readErr) {
		t.Fatalf("bootstrap result = %+v, want the read error", res)
	}

	exec, err := env.Execute(t.Context(), plan)
	if err != nil {
		t.Fatalf("execute: %v, want the plan accepted", err)
	}

	if err := exec.Err(); err != nil {
		t.Fatalf("pass: %v", err)
	}

	if applied[crbRef] != 0 {
		t.Fatalf("grant applied %d times while verification failed, want 0", applied[crbRef])
	}

	if n := deletes[crbRef]; n != 1 {
		t.Fatalf("grant deleted %d times, want 1", n)
	}

	if n := deletes["ClusterRoleBinding/"+StaleAutoApproveBindingName]; n != 1 {
		t.Fatalf("stale binding deleted %d times, want 1", n)
	}
}

// TestBootstrapZeroSitesRestrictionFailureRevokesGrant verifies that with no
// Sites, a failure to install the restriction deletes the retained grant.
func TestBootstrapZeroSitesRestrictionFailureRevokesGrant(t *testing.T) {
	for _, failing := range []string{
		"ValidatingAdmissionPolicy/" + bootstrapPolicyName,
		"ValidatingAdmissionPolicyBinding/" + bootstrapPolicyName,
	} {
		t.Run(failing, func(t *testing.T) {
			applied, deletes := map[string]int{}, map[string]int{}
			cl := bootstrapClient(t, failing, applied, deletes, staleAutoApprove(), bootstrapGrant())
			env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

			if _, err := runBootstrap(t, env, nil); err == nil {
				t.Fatal("pass succeeded, want the restriction failure reported")
			}

			err := cl.Get(t.Context(), client.ObjectKey{Name: BootstrapMachineBindingName}, &rbacv1.ClusterRoleBinding{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("get bootstrap grant = %v, want NotFound", err)
			}

			err = cl.Get(t.Context(), client.ObjectKey{Name: StaleAutoApproveBindingName}, &rbacv1.ClusterRoleBinding{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("get stale binding = %v, want NotFound", err)
			}
		})
	}
}

// TestBootstrapLookupErrorFailsPlan verifies a failed presence check is an
// error rather than a silent no-op.
func TestBootstrapLookupErrorFailsPlan(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(testScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("apiserver unavailable")
		},
	}).Build()
	env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

	plan, res, err := (BootstrapComponent{}).Plan(t.Context(), env, nil)
	if err != nil {
		t.Fatalf("plan returned planning error %v, want failed result", err)
	}

	if res.Err == nil {
		t.Fatal("result has no error, want lookup failure for retry")
	}

	assertRevokesBindings(t, plan)
}

func TestRevokeBindingsPlanSharesKeysWithSupportOps(t *testing.T) {
	assertRevokesBindings(t, revokeBindingsPlan(BootstrapComponent{}.Name()))
}

func assertRevokesBindings(t *testing.T, plan *component.Plan) {
	t.Helper()

	want := map[string]bool{BootstrapMachineBindingName: false, StaleAutoApproveBindingName: false}

	for _, op := range plan.Operations {
		if op.Kind != component.OpDelete || op.Object.GetKind() != "ClusterRoleBinding" {
			t.Errorf("unexpected operation %v on %s", op.Kind, op.Object.GetName())

			continue
		}

		if op.SharedKey != sharedSupportKey(op.Object) {
			t.Errorf("delete of %s has shared key %q, want %q", op.Object.GetName(), op.SharedKey, sharedSupportKey(op.Object))
		}

		want[op.Object.GetName()] = true
	}

	for name, seen := range want {
		if !seen {
			t.Errorf("plan does not delete ClusterRoleBinding %s", name)
		}
	}
}
