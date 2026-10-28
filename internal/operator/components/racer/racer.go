// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer installs the Racer dataplane on demand from ClusterCaches.
// Every dataplane Pod runs the Racer control plane, so the operator only
// installs static objects: the DaemonSet, its identity, RBAC, admission
// policies, the webhook configuration, and empty runtime state. It holds no
// keys, certificates, or authority state.
package racer

import (
	"context"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	name                = "racer"
	webhookConfigName   = "racer-object-sidecar-injector"
	pendingRequeue      = 5 * time.Second
	reasonMigrating     = "MigratingController"
	reasonGuards        = "AdmissionGuardUnavailable"
	reasonInitializing  = "Initializing"
	reasonOriginInvalid = "OriginInvalid"
	reasonOriginPending = "OriginPending"
)

// Component owns the Racer dataplane workloads and their inputs.
type Component struct{}

func New() component.ClusterComponent   { return Component{} }
func (Component) Name() string          { return name }
func (Component) ConditionType() string { return "RacerReady" }

func objectKey(env *component.Env, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: env.Namespace, Name: name}
}

// Plan reads state. Without ClusterCaches an existing installation is retained
// and not repaired or removed, but guard containment still runs: missing or
// drifted admission policies revoke the dataplane bindings.
func (Component) Plan(ctx context.Context, env *component.Env, _ []machinav1.Site) (*component.Plan, component.Result, error) {
	caches := &racerv1.ClusterCacheList{}
	if err := env.LiveReader().List(ctx, caches, client.Limit(1)); err != nil {
		return nil, component.Result{}, err
	}

	if len(caches.Items) == 0 {
		// A retained installation keeps its grants, so its admission guards
		// are still enforced: missing or drifted policies revoke the bindings
		// and are restored. Nothing else is repaired or installed.
		policies, err := decodePolicies(env)
		if err != nil {
			return nil, component.Result{}, err
		}

		containment, err := planContainment(ctx, env, policies)
		if err != nil {
			return nil, component.Result{}, err
		}

		renamed, err := planRenamedContainment(ctx, env)
		if err != nil {
			return nil, component.Result{}, err
		}

		// Pre-rename grants retained here are guarded the same way. The rename
		// cleanup itself waits for a ClusterCache, consistent with retaining
		// the existing installation.
		if renamed != nil {
			if containment == nil {
				containment = component.NewPlan()
			}

			containment.Merge(renamed)
		}

		if containment != nil {
			return containment, component.NotReadyAfter(reasonGuards, "revoking Racer dataplane permissions until its admission policies are restored", pendingRequeue), nil
		}

		return component.NewPlan(), component.ReconciledAfter("no ClusterCache; existing Racer resources retained with admission guards enforced", time.Hour), nil
	}

	plan, done, err := planLegacy(ctx, env)
	if err != nil {
		return nil, component.Result{}, err
	}

	if !done {
		return plan, component.NotReadyAfter(reasonMigrating, "waiting for the retired racer-controller to stop", pendingRequeue), nil
	}

	policies, err := decodePolicies(env)
	if err != nil {
		return nil, component.Result{}, err
	}

	containment, err := planContainment(ctx, env, policies)
	if err != nil {
		return nil, component.Result{}, err
	}

	renamed, err := planRenamedContainment(ctx, env)
	if err != nil {
		return nil, component.Result{}, err
	}

	if renamed != nil {
		if containment == nil {
			containment = component.NewPlan()
		}

		containment.Merge(renamed)
	}

	if containment != nil {
		plan.Merge(containment)
		return plan, component.NotReadyAfter(reasonGuards, "restoring Racer admission policies before granting dataplane permissions", pendingRequeue), nil
	}

	return planInstall(ctx, env, plan, policies)
}

func planInstall(ctx context.Context, env *component.Env, plan *component.Plan, policies []*unstructured.Unstructured) (*component.Plan, component.Result, error) {
	var guards []component.ObjectRef

	for _, policy := range policies {
		op, err := ownedOperation(ctx, env, component.Operation{Kind: component.OpApply, Object: policy, Component: name})
		if err != nil {
			return nil, component.Result{}, err
		}

		guards = append(guards, op.Ref())
		plan.Add(op)
	}

	objects, err := env.DecodeManifestFiles(manifests.Manifests, []string{"rbac.yaml", "mutating-webhook.yaml"}, nil)
	if err != nil {
		return nil, component.Result{}, err
	}

	var bindings []component.ObjectRef

	for _, obj := range objects {
		op := component.Operation{Kind: component.OpApply, Object: obj, Component: name}
		if kind := obj.GetKind(); kind == "RoleBinding" || kind == "ClusterRoleBinding" {
			// Grant only after every policy is written, and revoke if one fails.
			op.DependsOn = guards
			op.RevokeOnDependencyFailure = true
		}

		op, err = ownedOperation(ctx, env, op)
		if err != nil {
			return nil, component.Result{}, err
		}

		if op.DependsOn != nil {
			bindings = append(bindings, op.Ref())
		}

		plan.Add(op)
	}

	recorded, err := recordedStateUID(ctx, env)
	if err != nil {
		return nil, component.Result{}, err
	}

	state, err := planControl(ctx, env, plan, recorded)
	if err != nil {
		return nil, component.Result{}, err
	}

	config, err := planDataplaneConfig(ctx, env, plan)
	if err != nil {
		return nil, component.Result{}, err
	}

	if state.uid == "" {
		// The DaemonSet records the state UID, so it waits for the Secret.
		return plan, component.NotReadyAfter(reasonInitializing, "creating Racer control state", pendingRequeue), nil
	}

	if err := planDataplane(ctx, env, plan, config, state.uid, bindings); err != nil {
		return nil, component.Result{}, err
	}

	// Remove what this operator applied under the pre-rename names once the
	// new DaemonSet is observed live; its creation event requeues this
	// component. A config with a UID was read live.
	if _, err := planRenamedCleanup(ctx, env, plan, config.UID != ""); err != nil {
		return nil, component.Result{}, err
	}

	invalid, waiting, err := planOrigins(ctx, env, plan)
	if err != nil {
		return nil, component.Result{}, err
	}

	if len(invalid) != 0 {
		return plan, component.NotReadyAfter(reasonOriginInvalid, "Racer origins not reconciled for ClusterCache "+strings.Join(invalid, "; "), time.Minute), nil
	}

	if waiting {
		return plan, component.NotReadyAfter(reasonOriginPending, "Racer origins wait for the racer DaemonSet placement", 30*time.Second), nil
	}

	return plan, component.ReconciledAfter("Racer dataplane installation reconciled", time.Hour), nil
}

func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&admissionv1.ValidatingAdmissionPolicy{}, env.RequestSingleton(), builder.WithPredicates(guardPredicate()))
	b.Watches(&admissionv1.ValidatingAdmissionPolicyBinding{}, env.RequestSingleton(), builder.WithPredicates(guardPredicate()))
	b.Watches(&admissionv1.MutatingWebhookConfiguration{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool { return obj.GetName() == webhookConfigName })))
	b.Watches(&racerv1.ClusterCache{}, env.RequestSingleton(), builder.WithPredicates(clusterCachePredicate()))
	b.Watches(&appsv1.Deployment{}, env.RequestSingleton(), builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(legacyController))))
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(), builder.WithPredicates(env.ManagedWorkloadPredicate(runtimeWorkload(env))))
	b.Watches(&corev1.ServiceAccount{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(runtimeWorkload(env))))
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(), builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(dataplaneConfigName))))
	b.Watches(&corev1.Secret{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(secretWatched(env))))
}

// runtimeWorkload matches the dataplane and every origin workload or account.
func runtimeWorkload(env *component.Env) func(client.Object) bool {
	dataplane, origin := env.InNamespaceNamed(dataplaneName), env.InNamespaceWithPrefix(originPrefix)

	return func(obj client.Object) bool { return dataplane(obj) || origin(obj) }
}

// secretWatched matches racer-state and every Secret in the operator
// namespace that a ClusterCache origin reads its credentials from, so a
// rotated credential rolls the origin Pods.
func secretWatched(env *component.Env) func(client.Object) bool {
	state := env.InNamespaceNamed(stateName)

	return func(obj client.Object) bool {
		if state(obj) {
			return true
		}

		if obj.GetNamespace() != env.Namespace {
			return false
		}

		caches := &racerv1.ClusterCacheList{}
		if err := env.Client.List(context.Background(), caches); err != nil {
			// Reconcile rather than risk missing a rotation.
			return true
		}

		for i := range caches.Items {
			if credentialSecretName(&caches.Items[i]) == obj.GetName() {
				return true
			}
		}

		return false
	}
}

// clusterCachePredicate passes spec changes and the transition into
// termination. A finalizer-held cache only gains a deletionTimestamp, which
// does not bump the generation, but origin planning must prune it right away.
func clusterCachePredicate() predicate.Predicate {
	return predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.Funcs{
			CreateFunc: func(event.CreateEvent) bool { return false },
			DeleteFunc: func(event.DeleteEvent) bool { return false },
			UpdateFunc: func(e event.UpdateEvent) bool {
				if e.ObjectOld == nil || e.ObjectNew == nil {
					return false
				}

				return e.ObjectOld.GetDeletionTimestamp() == nil && e.ObjectNew.GetDeletionTimestamp() != nil
			},
			GenericFunc: func(event.GenericEvent) bool { return false },
		},
	)
}
