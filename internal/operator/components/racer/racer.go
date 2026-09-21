// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer installs the shared Racer control plane and opt-in Site dataplanes.
package racer

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

type (
	ControlPlane struct{}
	Dataplane    struct{}
)

func NewControlPlane() component.ClusterComponent { return ControlPlane{} }
func NewDataplane() component.SiteComponent       { return Dataplane{} }
func (ControlPlane) Name() string                 { return controlPlaneName }
func (ControlPlane) ConditionType() string        { return "RacerControlPlaneReady" }
func (Dataplane) Name() string                    { return dataplaneName }
func (Dataplane) ConditionType() string           { return "RacerDataplaneReady" }

func (Dataplane) Enabled(site *unboundedv1alpha3.Site) bool {
	return site.Spec.Components.Racer != nil && unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.Racer.SiteComponentSpec)
}

// Plan retains and repairs an installed singleton after the last Site is disabled
// or deleted. The shared ServiceAccount is the installation marker; the Deployment
// also recognizes installations whose account was removed. No runtime state is
// read or written, and no all-replicas-ready rollout gate or polling is needed.
func (ControlPlane) Plan(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	enabled := false

	for i := range sites {
		if (Dataplane{}).Enabled(&sites[i]) {
			enabled = true
			break
		}
	}

	if !enabled {
		for _, obj := range []client.Object{serviceAccount(controlPlaneName, env.Namespace), controlDeployment(env.Namespace, env.Config)} {
			err := env.Client.Get(ctx, client.ObjectKeyFromObject(obj), obj)
			if err == nil {
				enabled = true
				break
			}

			if !apierrors.IsNotFound(err) {
				return nil, component.Result{}, fmt.Errorf("check retained Racer installation: %w", err)
			}
		}
	}

	plan := component.NewPlan()
	if !enabled {
		return plan, component.Disabled("no Site enables Racer and no retained installation exists"), nil
	}

	var dependencies []component.ObjectRef

	for _, obj := range sharedResources(env.Namespace) {
		op := component.Operation{Kind: component.OpApply, Object: resourceObject(obj), Component: controlPlaneName}
		plan.Add(op)
		dependencies = append(dependencies, op.Ref())
	}

	plan.Add(component.Operation{Kind: component.OpApply, Object: resourceObject(controlDeployment(env.Namespace, env.Config)), Component: controlPlaneName, Overridable: true, DependsOn: dependencies})

	return plan, component.Reconciled(), nil
}

func (Dataplane) Plan(_ context.Context, env *component.Env, site *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	plan := component.NewPlan()

	var dependencies []component.ObjectRef
	for _, obj := range sharedResources(env.Namespace) {
		dependencies = append(dependencies, component.RefOf(component.ToUnstructured(obj)))
	}

	plan.Add(component.Operation{Kind: component.OpApply, Object: resourceObject(dataplaneDaemonSet(env.Namespace, env.Config, site)), Component: dataplaneName, Site: site.Name, Overridable: true, DependsOn: dependencies})

	return plan, component.Reconciled(), nil
}

// CleanupPlan uses the cache before planning a delete, so Sites that never opted
// in and repeated disabled passes generate no delete traffic. Site deletion uses
// the DaemonSet's controller owner reference; host slab data is never removed.
func (Dataplane) CleanupPlan(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	plan := component.NewPlan()
	ds := &appsv1.DaemonSet{}

	err := env.Client.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: SiteDaemonSetName(site.Name)}, ds)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, component.Result{}, fmt.Errorf("read disabled Racer dataplane: %w", err)
	}

	if err == nil && ds.DeletionTimestamp.IsZero() {
		ds.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}
		plan.Add(component.DeleteOperation(ds, dataplaneName, site.Name))
	}

	return plan, component.Disabled("Racer is disabled for this Site"), nil
}

// SetupWatches maps every shared installation dependency to the singleton request.
// The reconciler fans that request out to Sites, including after RBAC repairs.
// Runtime Secrets, ConfigMaps and Leases belong to the Racer controller and are
// deliberately absent. Component conditions describe applied intent, so workload
// status updates (including standby readiness) do not trigger writes.
func (ControlPlane) SetupWatches(b *builder.Builder, env *component.Env) {
	objects := append(sharedResources(env.Namespace), controlDeployment(env.Namespace, env.Config))
	byKind := map[string][]client.Object{}

	var kinds []string

	for _, obj := range objects {
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		if _, exists := byKind[kind]; !exists {
			kinds = append(kinds, kind)
		}

		byKind[kind] = append(byKind[kind], obj)
	}

	for _, kind := range kinds {
		objects := byKind[kind]
		match := func(obj client.Object) bool {
			for _, desired := range objects {
				if client.ObjectKeyFromObject(obj) == client.ObjectKeyFromObject(desired) {
					return true
				}
			}

			return false
		}
		b.Watches(objects[0], env.RequestSingleton(), builder.WithPredicates(managedPredicate(match)))
	}
}

func (Dataplane) SetupWatches(b *builder.Builder, env *component.Env) {
	// Use the singleton fanout rather than deriving Site names from the safe
	// resource name. This also repairs a deleted or corrupted owner reference.
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(), builder.WithPredicates(managedPredicate(env.InNamespaceWithPrefix(dataplaneName+"-"))))
}

func managedPredicate(match func(client.Object) bool) predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return match(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return match(e.Object) },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return (match(e.ObjectOld) || match(e.ObjectNew)) && !reflect.DeepEqual(watchedFields(e.ObjectOld), watchedFields(e.ObjectNew))
		},
	}
}

func watchedFields(obj client.Object) map[string]any {
	u := component.ToUnstructured(obj).DeepCopy()
	delete(u.Object, "status")
	delete(u.Object, "metadata")
	// Typed cache reads may omit GVK; it is not an input to desired state.
	delete(u.Object, "apiVersion")
	delete(u.Object, "kind")
	u.Object["ownedLabels"] = reservedMetadata(obj.GetLabels())
	u.Object["ownedAnnotations"] = reservedMetadata(obj.GetAnnotations())
	u.Object["owners"] = obj.GetOwnerReferences()
	u.Object["deleting"] = !obj.GetDeletionTimestamp().IsZero()

	return u.Object
}

func reservedMetadata(values map[string]string) map[string]string {
	owned := map[string]string{}

	for key, value := range values {
		if strings.HasPrefix(key, racermeta.MetadataPrefix) || strings.HasPrefix(key, "unbounded-cloud.io/") {
			owned[key] = value
		}
	}

	return owned
}

// resourceObject omits server-owned fields emitted by typed zero values. In
// particular creationTimestamp:null would never match a cached persisted object,
// defeating the executor's applied-payload hash fast path on every pass.
func resourceObject(obj client.Object) *unstructured.Unstructured {
	u := component.ToUnstructured(obj)
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "spec", "template", "metadata", "creationTimestamp")

	return u
}
