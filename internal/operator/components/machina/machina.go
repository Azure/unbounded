// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package machina implements the machina controller cluster component. Machina
// is a singleton for now: it is deployed whenever any Site enables it and kept
// reconciled when an installation already exists.
package machina

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
	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/components/metalman"
)

const (
	configName     = "machina-config"
	controllerName = "machina-controller"

	ConfigHashAnnotation = "unbounded-cloud.io/machina-config-hash"
)

// Component reconciles the machina controller singleton.
type Component struct{}

// New returns the machina cluster component.
func New() component.ClusterComponent { return Component{} }

// Name implements component.ClusterComponent.
func (Component) Name() string { return "machina" }

// ConditionType implements component.ClusterComponent.
func (Component) ConditionType() string { return "MachinaReady" }

// Reconcile deploys the machina controller singleton whenever any Site enables
// it and keeps an existing installation reconciled when none do.
func (Component) Reconcile(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) component.Result {
	enabled := false

	for i := range sites {
		if EnabledFor(&sites[i]) {
			enabled = true
			break
		}
	}

	if !enabled {
		// Keep machina installed once the operator has taken ownership. Automatic
		// singleton removal is surprising and can strand related
		// controllers/RBAC; a future explicit uninstall flow should handle it.
		retained, err := resourcesExist(ctx, env)
		if err != nil {
			return component.Failed(err)
		}

		if !retained {
			return component.Disabled("no site enables machina; retained")
		}
	}

	configHash, err := ensureConfig(ctx, env)
	if err != nil {
		return component.Failed(err)
	}

	if err := env.ApplyManifestFS(ctx, machinamanifests.Manifests, applyMutator(configHash)); err != nil {
		return component.Failed(err)
	}

	return component.Reconciled()
}

// SetupWatches reconciles machina on changes to its config payload and on
// create/delete/generation changes of its controller Deployment.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&corev1.ConfigMap{}, env.RequestSingletonAndAllSites(),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(configName))))
	b.Watches(&appsv1.Deployment{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(controllerName))))
}

// EnabledFor reports whether a Site enables the machina component.
func EnabledFor(site *unboundedv1alpha3.Site) bool {
	if site.Spec.Components.Machina == nil {
		return false
	}

	return unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.Machina.SiteComponentSpec)
}

func resourcesExist(ctx context.Context, env *component.Env) (bool, error) {
	for _, resource := range []struct {
		name   string
		object client.Object
	}{
		{name: configName, object: &corev1.ConfigMap{}},
		{name: controllerName, object: &appsv1.Deployment{}},
	} {
		key := client.ObjectKey{Namespace: env.Namespace, Name: resource.name}
		if err := env.Client.Get(ctx, key, resource.object); err == nil {
			return true, nil
		} else if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get retained machina resource %s/%s: %w", key.Namespace, key.Name, err)
		}
	}

	return false, nil
}

// applyMutator skips CRDs and the metalman RBAC (applied per-site by the
// metalman component), skips the separately reconciled ConfigMap, and stamps its
// exact content hash on the Deployment so every config change rolls Machina.
func applyMutator(configHash string) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		// CRDs are installed once at operator startup, not from the reconcile
		// loop; the metalman RBAC ships in the machina manifests but is applied
		// per-site by the metalman component. Skip both here.
		if obj.GetKind() == component.CRDKind || metalman.IsSupportObject(obj) {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() == "ConfigMap" && obj.GetName() == configName {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() == "Deployment" && obj.GetName() == controllerName {
			if err := unstructured.SetNestedField(obj.Object, configHash,
				"spec", "template", "metadata", "annotations", ConfigHashAnnotation); err != nil {
				return fmt.Errorf("set machina config hash annotation: %w", err)
			}
		}

		return nil
	}
}

func ensureConfig(ctx context.Context, env *component.Env) (string, error) {
	desired, err := env.DefaultConfigMap(machinamanifests.Manifests, configName, "machina")
	if err != nil {
		return "", err
	}

	key := client.ObjectKeyFromObject(desired)
	existing := &corev1.ConfigMap{}

	err = env.Client.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		config, mergeErr := SetAPIServerEndpoint(desired.Data["config.yaml"], env.Config.APIServerEndpoint)
		if mergeErr != nil {
			return "", mergeErr
		}

		desired.Data["config.yaml"] = config
		if createErr := env.Client.Create(ctx, desired); createErr != nil {
			return "", fmt.Errorf("create machina config %s/%s: %w", key.Namespace, key.Name, createErr)
		}

		return component.ConfigMapPayloadHash(desired), nil
	}

	if err != nil {
		return "", fmt.Errorf("get machina config %s/%s: %w", key.Namespace, key.Name, err)
	}

	config := existing.Data["config.yaml"]

	merged, err := SetAPIServerEndpoint(config, env.Config.APIServerEndpoint)
	if err != nil {
		return "", err
	}

	if merged != config {
		before := existing.DeepCopy()
		if existing.Data == nil {
			existing.Data = map[string]string{}
		}

		existing.Data["config.yaml"] = merged

		patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
		if err := env.Client.Patch(ctx, existing, patch); err != nil {
			return "", fmt.Errorf("update machina config %s/%s: %w", key.Namespace, key.Name, err)
		}
	}

	return component.ConfigMapPayloadHash(existing), nil
}
