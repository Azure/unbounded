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

	if err := env.ApplyManifestFS(ctx, netmanifests.Manifests, applyMutator(configHash, env.Config)); err != nil {
		return component.Failed(err)
	}

	return component.Reconciled()
}

// SetupWatches reconciles net on changes to its config payload and on
// create/delete/generation changes of its managed workloads.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&corev1.ConfigMap{}, env.RequestSingletonAndAllSites(),
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
func applyMutator(configHash string, cfg component.Config) func(*unstructured.Unstructured) error {
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
			if obj.GetKind() == "Deployment" {
				if err := component.SetNamedContainerImage(obj, "containers", "controller", cfg.NetControllerImage); err != nil {
					return err
				}

				if cfg.ManagedKubeProxyImage != "" {
					if err := component.AppendNamedContainerArg(obj, "controller", "--managed-kube-proxy-image="+cfg.ManagedKubeProxyImage); err != nil {
						return err
					}
				}
			} else {
				if err := component.SetNamedContainerImage(obj, "initContainers", "install-cni-plugins", cfg.NetNodeImage); err != nil {
					return err
				}

				if err := component.SetNamedContainerImage(obj, "containers", "node", cfg.NetNodeImage); err != nil {
					return err
				}
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
