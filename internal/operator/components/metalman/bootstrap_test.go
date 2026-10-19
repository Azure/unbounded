// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package metalman

import (
	"context"
	"errors"
	"testing"

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
// the retained grant is kept behind the admission restriction, stably.
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

			for pass := range 2 {
				if _, err := runBootstrap(t, env, nil); err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
			}

			err := cl.Get(t.Context(), client.ObjectKey{Name: StaleAutoApproveBindingName}, &rbacv1.ClusterRoleBinding{})
			if !apierrors.IsNotFound(err) {
				t.Fatalf("get stale binding = %v, want NotFound", err)
			}

			for _, want := range []string{
				"ValidatingAdmissionPolicy/" + bootstrapPolicyName,
				"ValidatingAdmissionPolicyBinding/" + bootstrapPolicyName,
				"ClusterRoleBinding/" + BootstrapMachineBindingName,
			} {
				if applied[want] != 2 {
					t.Fatalf("%s applied %d times, want once per pass", want, applied[want])
				}
			}

			if n := deletes["ClusterRoleBinding/"+BootstrapMachineBindingName]; n != 0 {
				t.Fatalf("bootstrap grant deleted %d times, want 0", n)
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

// TestBootstrapWithSitesDefersToSiteComponent verifies the cluster component
// plans nothing when Sites exist, so the per-Site planner's shared ops are the
// only ones and nothing is duplicated or conflicting.
func TestBootstrapWithSitesDefersToSiteComponent(t *testing.T) {
	applied, deletes := map[string]int{}, map[string]int{}
	cl := bootstrapClient(t, "", applied, deletes, staleAutoApprove(), bootstrapGrant())
	env := &component.Env{Client: cl, Scheme: testScheme(t), Namespace: component.DefaultNamespace}

	sites := []unboundedv1alpha3.Site{*enabledSite(), *disabledSite("rack-b")}

	clusterPlan, res, err := BootstrapComponent{}.Plan(t.Context(), env, sites)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if clusterPlan.Len() != 0 {
		t.Fatalf("cluster plan has %d operations with Sites present, want 0", clusterPlan.Len())
	}

	if !res.Ready {
		t.Fatalf("result = %+v, want ready", res)
	}

	plan := passPlan(t, env, &sites[0], &sites[1])
	plan.Merge(clusterPlan)

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

	if _, _, err := (BootstrapComponent{}).Plan(t.Context(), env, nil); err == nil {
		t.Fatal("plan succeeded, want lookup error")
	}
}
