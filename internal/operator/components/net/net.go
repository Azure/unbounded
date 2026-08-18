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
	"strings"

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

	// Read the backend the registrations point at before deciding what to
	// plan. This is the same shape as planConfig: a live read whose answer
	// changes what the pass emits, and whose value is stamped into objects the
	// pass will apply. Withholding is therefore a property of the plan rather
	// than of a second apply, so plan.Summary() states it and the executor
	// needs to know nothing about it.
	backend, err := readBackendState(ctx, env)
	if err != nil {
		return nil, component.Result{}, err
	}

	// applyMutator drops the registrations while the backend is not serving
	// and stamps the published CA into them when it is. tierActivation orders
	// whatever survives after the workloads it points at.
	objects, err := env.DecodeManifestFS(netmanifests.Manifests, applyMutator(env.Config, configHash, backend))
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

	res, err := registrationVerdict(ctx, env, backend)
	if err != nil {
		return nil, component.Result{}, err
	}

	return plan, res, nil
}

// registrationVerdict reports what withholding the registrations means for the
// Site, which is a separate question from whether to withhold them.
//
// Withholding a registration that does not exist yet is a real difference
// between desired and actual state and the Site should say so. Withholding one
// that is already in place with the right CA changes nothing, and reporting it
// would turn NetReady False for the duration of every net rollout, because the
// controller is host-networked with maxSurge: 0 and is therefore briefly
// unavailable by design on every upgrade.
//
// Either way the pass asks to be run again. Deployment status and endpoints
// produce no event this operator sees: the workload predicate filters
// status-only updates, because reacting to them would re-apply every manifest
// on every pod restart, and the endpoint objects are deliberately not watched.
func registrationVerdict(ctx context.Context, env *component.Env, backend backendState) (component.Result, error) {
	if backend.ready {
		return component.Reconciled(), nil
	}

	pending, err := pendingRegistrations(ctx, env, backend.caBundle)
	if err != nil {
		return component.Result{}, err
	}

	if len(pending) > 0 {
		return component.NotReadyAfter(component.ReasonBackendNotReady,
			fmt.Sprintf("holding back %s until the controller is serving: %s",
				strings.Join(pending, ", "), backend.reason),
			backendPollInterval), nil
	}

	return component.ReconciledAfter(backendPollInterval), nil
}

// isManagedWorkload reports whether obj is one of the two workloads net owns.
func isManagedWorkload(obj *unstructured.Unstructured) bool {
	return (obj.GetKind() == "Deployment" && obj.GetName() == controllerName) ||
		(obj.GetKind() == "DaemonSet" && obj.GetName() == nodeName)
}

// SetupWatches reconciles net on changes to its config payload and on
// create/delete/generation changes of its managed workloads.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	// The serving CA is watched alongside the config because the registration
	// gate cannot proceed without it: when the controller publishes it the
	// operator should stamp and register straight away rather than wait out
	// backendPollInterval.
	//
	// The singleton request already fans out to every Site, so enqueuing
	// the Sites as well would run one redundant pass per Site for a single
	// ConfigMap edit.
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(configName, servingCAName))))
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

// applyMutator skips the separately reconciled ConfigMap, stamps its exact
// payload hash on both net workloads so config changes roll them together, and
// gates the registrations that point at the controller Service on that
// controller actually serving.
func applyMutator(cfg component.Config, configHash string, backend backendState) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		if obj.GetKind() == component.CRDKind {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() == "ConfigMap" && obj.GetName() == configName {
			obj.Object = nil

			return nil
		}

		if isBackendRegistration(obj) {
			// Withholding is unconditional while the backend is down, whether
			// or not a registration is already there. There is no CA to stamp,
			// so an apply could only either change nothing or overwrite a
			// working registration with one whose caBundle is empty, and an
			// empty caBundle is exactly the state that makes a webhook stop
			// enforcing and an APIService fail. Whatever is in the cluster is
			// left alone until the controller is serving again.
			if !backend.ready {
				obj.Object = nil

				return nil
			}

			return stampCABundle(obj, backend.caBundle)
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
