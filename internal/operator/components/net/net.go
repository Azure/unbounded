// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package net implements the unbounded-net cluster component. Net is not a
// per-Site component: one controller/node pair reads every Site, so it is
// reconciled as a cluster singleton whenever at least one Site exists and kept
// reconciled if it was already installed.
package net

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	netmanifests "github.com/Azure/unbounded/deploy/net"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	configName     = "unbounded-net-config"
	controllerName = "unbounded-net-controller"
	nodeName       = "unbounded-net-node"

	ConfigHashAnnotation = "unbounded-cloud.io/net-config-hash"
)

// Component reconciles the unbounded-net cluster singleton.
type Component struct{}

// New returns the net cluster component.
func New() component.ClusterComponent { return Component{} }

// Name implements component.ClusterComponent.
func (Component) Name() string { return "net" }

// ConditionType implements component.ClusterComponent.
func (Component) ConditionType() string { return "NetReady" }

// Plan deploys the unbounded-net cluster singleton whenever at least one Site
// exists and keeps an existing installation reconciled with no Sites.
func (c Component) Plan(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	if len(sites) == 0 {
		// Net is the cluster dataplane. Do not auto-delete it just because the
		// last Site was removed; deleting net-node can break pod networking
		// across the whole cluster. A future explicit uninstall flow should
		// handle removal.
		retained, err := resourcesExist(ctx, env)
		if err != nil {
			return nil, component.Result{}, err
		}

		if !retained {
			return nil, component.NoSites("no sites; net retained"), nil
		}
	}

	configHash, configOp, err := planConfig(ctx, env)
	if err != nil {
		return nil, component.Result{}, err
	}

	objects, err := env.DecodeManifestFS(netmanifests.Manifests, applyMutator(env.Config, configHash))
	if err != nil {
		return nil, component.Result{}, err
	}

	plan := component.NewPlan()

	// The config is written before the workloads that carry its hash, so a
	// failure to write it does not leave a pod unable to mount.
	var dependsOn []component.ObjectRef

	if configOp != nil {
		plan.Add(*configOp)

		dependsOn = []component.ObjectRef{configOp.Ref()}
	}

	for _, obj := range objects {
		op := component.Operation{
			Kind:      component.OpApply,
			Object:    obj,
			Component: c.Name(),
		}

		if isManagedWorkload(obj) {
			op.Overridable = true
			op.DependsOn = dependsOn
		}

		plan.Add(op)
	}

	return plan, component.Reconciled(), nil
}

// isManagedWorkload reports whether obj is one of the two workloads net owns.
func isManagedWorkload(obj *unstructured.Unstructured) bool {
	return (obj.GetKind() == "Deployment" && obj.GetName() == controllerName) ||
		(obj.GetKind() == "DaemonSet" && obj.GetName() == nodeName)
}

// SetupWatches reconciles net on changes to its config payload and on
// create/delete/generation changes of its managed workloads.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	// The singleton request already fans out to every Site, so enqueuing
	// the Sites as well would run one redundant pass per Site for a single
	// ConfigMap edit.
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(configName))))
	b.Watches(&appsv1.Deployment{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(controllerName))))
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(nodeName))))
}

func resourcesExist(ctx context.Context, env *component.Env) (bool, error) {
	for _, object := range []client.Object{
		&corev1.ConfigMap{},
		&appsv1.Deployment{},
		&appsv1.DaemonSet{},
	} {
		name := configName

		switch object.(type) {
		case *appsv1.Deployment:
			name = controllerName
		case *appsv1.DaemonSet:
			name = nodeName
		}

		key := client.ObjectKey{Namespace: env.Namespace, Name: name}
		if err := env.Client.Get(ctx, key, object); err == nil {
			return true, nil
		} else if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get retained net resource %s/%s: %w", env.Namespace, name, err)
		}
	}

	return false, nil
}

// applyMutator skips the separately reconciled ConfigMap and stamps its exact
// payload hash on both net workloads so config changes roll them together.
func applyMutator(cfg component.Config, configHash string) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		if obj.GetKind() == component.CRDKind {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() == "ConfigMap" && obj.GetName() == configName {
			obj.Object = nil

			return nil
		}

		if (obj.GetKind() == "Deployment" && obj.GetName() == controllerName) ||
			(obj.GetKind() == "DaemonSet" && obj.GetName() == nodeName) {
			if err := component.SetPodSpecImages(obj, cfg.Image(obj.GetName())); err != nil {
				return fmt.Errorf("set net workload images: %w", err)
			}

			annotations, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
			if err != nil {
				return fmt.Errorf("get net pod template annotations: %w", err)
			}

			if annotations == nil {
				annotations = map[string]string{}
			}

			annotations[ConfigHashAnnotation] = configHash
			if err := unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
				return fmt.Errorf("set net config hash annotation: %w", err)
			}
		}

		return nil
	}
}

// planConfig hashes the net config payload and, when no config exists, returns
// the operation that creates the embedded default. Existing migrated or
// user-managed payloads are never applied over, so the returned operation is
// create-if-absent rather than an apply.
//
// The hash is computed here, at plan time, from either the observed payload or
// the default about to be created. If another writer creates a different
// payload between planning and execution, the workloads briefly carry a hash
// for content that is not there; the config watch fires on that create and the
// next pass corrects it.
func planConfig(ctx context.Context, env *component.Env) (string, *component.Operation, error) {
	key := client.ObjectKey{Namespace: env.Namespace, Name: configName}
	existing := &corev1.ConfigMap{}

	err := env.Client.Get(ctx, key, existing)
	if err == nil {
		return component.ConfigMapPayloadHash(existing), nil, nil
	}

	if !apierrors.IsNotFound(err) {
		return "", nil, fmt.Errorf("get net config %s/%s: %w", key.Namespace, key.Name, err)
	}

	desired, err := env.DefaultConfigMap(netmanifests.Manifests, configName, "net")
	if err != nil {
		return "", nil, err
	}

	return component.ConfigMapPayloadHash(desired), &component.Operation{
		Kind:      component.OpCreateIfAbsent,
		Object:    component.ToUnstructured(desired),
		Component: Component{}.Name(),
	}, nil
}
