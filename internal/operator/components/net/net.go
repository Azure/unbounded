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

// Reconcile deploys the unbounded-net cluster singleton whenever at least one
// Site exists and keeps an existing installation reconciled with no Sites.
func (Component) Reconcile(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) component.Result {
	if len(sites) == 0 {
		// Net is the cluster dataplane. Do not auto-delete it just because the
		// last Site was removed; deleting net-node can break pod networking
		// across the whole cluster. A future explicit uninstall flow should
		// handle removal.
		retained, err := resourcesExist(ctx, env)
		if err != nil {
			return component.Failed(err)
		}

		if !retained {
			return component.NoSites("no sites; net retained")
		}
	}

	configHash, err := ensureConfig(ctx, env)
	if err != nil {
		return component.Failed(err)
	}

	// Apply the workloads before asking whether the current Deployment revision
	// is serving. Reading first would let the outgoing revision open the gate for
	// registrations applied immediately after changing the pod template.
	if err := env.ApplyManifestFS(ctx, netmanifests.Manifests,
		applyMutator(env.Config, configHash, backendState{})); err != nil {
		return component.Failed(err)
	}

	backend, err := readBackendState(ctx, env)
	if err != nil {
		return component.Failed(err)
	}

	if backend.ready {
		if err := env.ApplyManifestFS(ctx, netmanifests.Manifests, registrationMutator(backend.caBundle)); err != nil {
			return component.Failed(err)
		}

		return component.Reconciled()
	}

	pending, err := pendingRegistrations(ctx, env, backend.caBundle)
	if err != nil {
		return component.Failed(err)
	}

	if len(pending) > 0 {
		return component.NotReadyAfter(component.ReasonBackendNotReady,
			fmt.Sprintf("holding back %s until the controller is serving: %s",
				strings.Join(pending, ", "), backend.reason),
			backendPollInterval)
	}

	// Existing registrations remain usable during the expected maxSurge: 0
	// rollout gap, so do not flap NetReady. Still poll: Deployment status and
	// endpoints are not watched, and the desired registrations were withheld.
	return component.ReconciledAfter(backendPollInterval)
}

// SetupWatches reconciles net on changes to its config payload and on
// create/delete/generation changes of its managed workloads.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	// The serving CA is watched alongside the config because the registration
	// gate cannot proceed without it: when the controller publishes it the
	// operator should stamp and register straight away rather than wait out
	// backendPollInterval.
	b.Watches(&corev1.ConfigMap{}, env.RequestSingletonAndAllSites(),
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

// registrationMutator applies only the registrations after the live backend
// check has established that the just-applied controller revision is serving.
func registrationMutator(ca []byte) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		if !isBackendRegistration(obj) {
			obj.Object = nil

			return nil
		}

		return stampCABundle(obj, ca)
	}
}

// ensureConfig creates the embedded default only when no config exists. Existing
// migrated or user-managed payloads are never applied over.
func ensureConfig(ctx context.Context, env *component.Env) (string, error) {
	key := client.ObjectKey{Namespace: env.Namespace, Name: configName}
	existing := &corev1.ConfigMap{}

	err := env.Client.Get(ctx, key, existing)
	if err == nil {
		return component.ConfigMapPayloadHash(existing), nil
	}

	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("get net config %s/%s: %w", key.Namespace, key.Name, err)
	}

	desired, err := env.DefaultConfigMap(netmanifests.Manifests, configName, "net")
	if err != nil {
		return "", err
	}

	if err := env.Client.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create net config %s/%s: %w", key.Namespace, key.Name, err)
		}

		if err := env.Client.Get(ctx, key, existing); err != nil {
			return "", fmt.Errorf("get raced net config %s/%s: %w", key.Namespace, key.Name, err)
		}

		return component.ConfigMapPayloadHash(existing), nil
	}

	return component.ConfigMapPayloadHash(desired), nil
}
