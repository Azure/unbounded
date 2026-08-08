// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package gantry implements the gantry peer-to-peer OCI distribution cluster
// component. Gantry runs one agent DaemonSet per node fronting containerd's
// image pulls, so it is reconciled as a cluster singleton. Unlike the other
// components it defaults to enabled: it is deployed unless every Site explicitly
// opts out via spec.components.gantry.enabled=false.
//
// The unbounded agent manages the per-node containerd wiring, so the operator
// installs only the Gantry workload and its Kubernetes configuration.
package gantry

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	daemonSetName = "gantry"
	configName    = "gantry-config"

	// imageRepository is the operator-managed image repository for the gantry
	// agent. The operator derives the full reference at reconcile time via
	// component.Config.Image so gantry is version-matched to the operator like
	// the other components.
	imageRepository = "gantry"

	// agentContainerName is the gantry agent's main container. Only this
	// container carries the operator-managed image; the DaemonSet's busybox
	// init container keeps its pinned public image.
	agentContainerName = "gantry"

	// legacyNodeConfigName and legacyNodeConfigDaemonSetName were installed by
	// older operator versions. The unbounded agent owns this host configuration.
	legacyNodeConfigName          = "gantry-containerd-hosts"
	legacyNodeConfigDaemonSetName = "gantry-containerd-config"

	configHashAnnotation = "unbounded-cloud.io/gantry-config-hash"
)

// Component reconciles the gantry cluster singleton.
type Component struct{}

// New returns the gantry cluster component.
func New() component.ClusterComponent { return Component{} }

// Name implements component.ClusterComponent.
func (Component) Name() string { return "gantry" }

// ConditionType implements component.ClusterComponent.
func (Component) ConditionType() string { return "GantryReady" }

// EnabledFor reports whether a Site enables the gantry component. Gantry
// defaults to enabled: only an explicit enabled=false opts a Site out.
func EnabledFor(site *unboundedv1alpha3.Site) bool {
	g := site.Spec.Components.Gantry
	if g == nil || g.Enabled == nil {
		return true
	}

	return *g.Enabled
}

// Reconcile deploys the gantry cluster singleton whenever at least one Site does
// not opt out and keeps an existing installation reconciled when every Site opts
// out.
func (Component) Reconcile(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) component.Result {
	if err := cleanupLegacyNodeConfig(ctx, env); err != nil {
		return component.Failed(err)
	}

	enabled := false

	for i := range sites {
		if EnabledFor(&sites[i]) {
			enabled = true
			break
		}
	}

	if !enabled {
		// Keep gantry installed once the operator has taken ownership; automatic
		// singleton removal is surprising. A future explicit uninstall flow
		// should handle removal.
		retained, err := resourcesExist(ctx, env)
		if err != nil {
			return component.Failed(err)
		}

		if !retained {
			return component.Disabled("no site enables gantry")
		}
	}

	configHash, err := ensureConfig(ctx, env)
	if err != nil {
		return component.Failed(err)
	}

	if err := applyManifests(ctx, env, applyMutator(env.Config.Image(imageRepository), configHash)); err != nil {
		return component.Failed(err)
	}

	return component.Reconciled()
}

// SetupWatches reconciles Gantry on changes to its active resources and when
// legacy node-config resources appear so they can be removed.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&corev1.ConfigMap{}, env.RequestSingletonAndAllSites(),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(configName, legacyNodeConfigName))))
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(daemonSetName, legacyNodeConfigDaemonSetName))))
}

// applyManifests applies Gantry's operator-managed top-level manifests. The
// standalone node configurator and examples subtree are intentionally excluded.
func applyManifests(ctx context.Context, env *component.Env, mutate func(*unstructured.Unstructured) error) error {
	files, err := component.YamlFiles(gantrymanifests.Manifests)
	if err != nil {
		return err
	}

	topLevel := make([]string, 0, len(files))

	for _, file := range files {
		if file == "node-config.yaml" || strings.Contains(file, "/") {
			continue
		}

		topLevel = append(topLevel, file)
	}

	return env.ApplyManifestFiles(ctx, gantrymanifests.Manifests, topLevel, mutate)
}

func resourcesExist(ctx context.Context, env *component.Env) (bool, error) {
	for _, resource := range []struct {
		name   string
		object client.Object
	}{
		{name: configName, object: &corev1.ConfigMap{}},
		{name: daemonSetName, object: &appsv1.DaemonSet{}},
	} {
		key := client.ObjectKey{Namespace: env.Namespace, Name: resource.name}
		if err := env.Client.Get(ctx, key, resource.object); err == nil {
			return true, nil
		} else if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get retained gantry resource %s/%s: %w", key.Namespace, key.Name, err)
		}
	}

	return false, nil
}

func cleanupLegacyNodeConfig(ctx context.Context, env *component.Env) error {
	for _, obj := range []client.Object{
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: env.Namespace, Name: legacyNodeConfigDaemonSetName}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: env.Namespace, Name: legacyNodeConfigName}},
	} {
		if err := env.DeleteIfExists(ctx, obj); err != nil {
			return fmt.Errorf("remove legacy Gantry node config: %w", err)
		}
	}

	return nil
}

// applyMutator skips CRDs and the separately reconciled gantry-config ConfigMap,
// stamps the operator-derived agent image onto the agent DaemonSet, and stamps
// the config payload hash on its pod template so a config change rolls the
// DaemonSet. Only the agent's own container is re-imaged.
func applyMutator(agentImage, configHash string) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		if obj.GetKind() == component.CRDKind {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() == "ConfigMap" && obj.GetName() == configName {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() == "DaemonSet" && obj.GetName() == daemonSetName {
			if err := component.SetNamedContainerImage(obj, agentContainerName, agentImage); err != nil {
				return fmt.Errorf("set gantry agent image: %w", err)
			}

			return stampConfigHash(obj, configHashAnnotation, configHash)
		}

		return nil
	}
}

// stampConfigHash sets a config-hash annotation on a workload's pod template so a
// config change rolls it.
func stampConfigHash(obj *unstructured.Unstructured, annotation, hash string) error {
	annotations, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		return fmt.Errorf("get %s pod template annotations: %w", obj.GetName(), err)
	}

	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[annotation] = hash
	if err := unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
		return fmt.Errorf("set %s config hash annotation: %w", obj.GetName(), err)
	}

	return nil
}

// ensureConfig creates the embedded default only when no config exists. An
// existing operator/user-managed payload (for example an edited
// upstream_registries list) is never applied over.
func ensureConfig(ctx context.Context, env *component.Env) (string, error) {
	key := client.ObjectKey{Namespace: env.Namespace, Name: configName}
	existing := &corev1.ConfigMap{}

	err := env.Client.Get(ctx, key, existing)
	if err == nil {
		return component.ConfigMapPayloadHash(existing), nil
	}

	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("get gantry config %s/%s: %w", key.Namespace, key.Name, err)
	}

	desired, err := env.DefaultConfigMap(gantrymanifests.Manifests, configName, "gantry")
	if err != nil {
		return "", err
	}

	if err := env.Client.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create gantry config %s/%s: %w", key.Namespace, key.Name, err)
		}

		if err := env.Client.Get(ctx, key, existing); err != nil {
			return "", fmt.Errorf("get raced gantry config %s/%s: %w", key.Namespace, key.Name, err)
		}

		return component.ConfigMapPayloadHash(existing), nil
	}

	return component.ConfigMapPayloadHash(desired), nil
}
