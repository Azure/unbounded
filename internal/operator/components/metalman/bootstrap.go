// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package metalman

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	"github.com/Azure/unbounded/internal/operator/component"
)

// BootstrapServiceAccountName is the ServiceAccount whose tokens metalman hands
// to booting machines.
const BootstrapServiceAccountName = "metalman-bootstrap"

// BootstrapComponent keeps the shared metalman bootstrap security posture
// reconciled when no Site is left to do it.
//
// The per-Site metalman component plans the shared support set (admission
// restriction, restricted bootstrap grant, stale RBAC deletes) for every Site,
// enabled or not. With zero Sites no per-Site planner runs, and the machina
// component filters the support objects out of its own manifests, so an
// upgrade after the last Site was deleted would otherwise leave the old
// unrestricted grant and the legacy auto-approval binding in place while
// previously minted bootstrap tokens are still valid.
type BootstrapComponent struct{}

// NewBootstrap returns the cluster-scoped metalman bootstrap component.
func NewBootstrap() component.ClusterComponent { return BootstrapComponent{} }

// Name implements component.ClusterComponent.
func (BootstrapComponent) Name() string { return "metalman-bootstrap" }

// ConditionType implements component.ClusterComponent.
func (BootstrapComponent) ConditionType() string { return "MetalmanBootstrapReady" }

// Plan plans the shared support set only on the zero-Site pass and only when
// metalman bootstrap resources are already live.
//
// When any Site exists, the metalman Site component plans the same operations
// under the same shared keys, so this component plans nothing to avoid
// duplicate work. Note that the Site status this verdict is published on only
// exists in that case.
//
// The trigger for the zero-Site case is the live presence of any of: the
// metalman-bootstrap ServiceAccount, the metalman:bootstrap-machine binding, or
// the stale metalman:node-autoapprove-bootstrap binding. Any of them means a
// previous release installed metalman bootstrap access that may still be
// exercised by unexpired tokens. A cluster that never ran metalman has none of
// them and is left untouched, so the operator does not install metalman RBAC
// or admission policy there. Once applied, the ServiceAccount and binding stay
// present, so the trigger is stable across passes and does not churn.
func (c BootstrapComponent) Plan(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	plan := component.NewPlan()

	if len(sites) > 0 {
		return plan, component.Disabled("bootstrap restriction is reconciled by the metalman Site component"), nil
	}

	present, err := bootstrapResourcesPresent(ctx, env)
	if err != nil {
		return nil, component.Result{}, err
	}

	if !present {
		return plan, component.NoSites("no metalman bootstrap resources present"), nil
	}

	support, err := env.DecodeManifestFS(machinamanifests.Manifests, mutateSupportObject)
	if err != nil {
		return nil, component.Result{}, err
	}

	addSupportOps(plan, support, c.Name(), "")

	return plan, component.NoSites("metalman bootstrap access restricted while no Sites exist"), nil
}

// bootstrapResourcesPresent reports whether any metalman bootstrap resource
// from an earlier release is live. It reads the apiserver directly because the
// answer decides whether security-relevant objects are written.
func bootstrapResourcesPresent(ctx context.Context, env *component.Env) (bool, error) {
	reader := env.LiveReader()

	checks := []struct {
		key types.NamespacedName
		obj client.Object
	}{
		{types.NamespacedName{Namespace: env.Namespace, Name: BootstrapServiceAccountName}, &corev1.ServiceAccount{}},
		{types.NamespacedName{Name: BootstrapMachineBindingName}, &rbacv1.ClusterRoleBinding{}},
		{types.NamespacedName{Name: StaleAutoApproveBindingName}, &rbacv1.ClusterRoleBinding{}},
	}

	for _, check := range checks {
		err := reader.Get(ctx, check.key, check.obj)
		if err == nil {
			return true, nil
		}

		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("checking for metalman bootstrap resource %s: %w", check.key, err)
		}
	}

	return false, nil
}
