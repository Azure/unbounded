// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package gantry implements the gantry peer-to-peer OCI distribution cluster
// component. Gantry runs one agent DaemonSet per node fronting containerd's
// image pulls, so it is reconciled as a cluster singleton. Unlike the other
// components it defaults to enabled: it is deployed unless every Site explicitly
// opts out via spec.components.gantry.enabled=false.
//
// Note: deploying the gantry workload does not by itself route node containerd
// through the mirror; that requires a per-node containerd hosts.toml under
// /etc/containerd/certs.d, which is not yet managed by the operator.
package gantry

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
	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	daemonSetName = "gantry"
	configName    = "gantry-config"

	// nodeConfigName and nodeConfigDaemonSetName are the operator-managed
	// containerd node-wiring objects: a ConfigMap carrying the certs.d
	// hosts.toml and the DaemonSet that installs it into
	// /etc/containerd/certs.d/_default on every node.
	nodeConfigName          = "gantry-containerd-hosts"
	nodeConfigDaemonSetName = "gantry-containerd-config"

	configHashAnnotation     = "unbounded-cloud.io/gantry-config-hash"
	nodeConfigHashAnnotation = "unbounded-cloud.io/gantry-node-config-hash"
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
			return component.Disabled("no site enables gantry; retained")
		}
	}

	configHash, err := ensureConfig(ctx, env)
	if err != nil {
		return component.Failed(err)
	}

	// The node-config ConfigMap is operator-owned static content (the certs.d
	// hosts.toml), applied directly from the embedded manifests. Hash it so a
	// content change rolls the writer DaemonSet, which otherwise writes once and
	// sleeps.
	nodeConfigHash, err := nodeConfigPayloadHash(env)
	if err != nil {
		return component.Failed(err)
	}

	if err := applyManifests(ctx, env, applyMutator(configHash, nodeConfigHash)); err != nil {
		return component.Failed(err)
	}

	return component.Reconciled()
}

// SetupWatches reconciles gantry on changes to its config payloads and on
// create/delete/generation changes of its agent and node-config DaemonSets.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&corev1.ConfigMap{}, env.RequestSingletonAndAllSites(),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(configName, nodeConfigName))))
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(daemonSetName, nodeConfigDaemonSetName))))
}

// applyManifests applies gantry's top-level manifests. The examples/ subtree in
// the embedded manifests (optional NetworkPolicy hardening and a sample registry
// Secret) is intentionally excluded from the default install.
func applyManifests(ctx context.Context, env *component.Env, mutate func(*unstructured.Unstructured) error) error {
	files, err := component.YamlFiles(gantrymanifests.Manifests)
	if err != nil {
		return err
	}

	topLevel := make([]string, 0, len(files))

	for _, file := range files {
		if strings.Contains(file, "/") {
			continue // skip examples/ and any other subdirectory
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

// applyMutator skips CRDs and the separately reconciled gantry-config ConfigMap,
// and stamps the config payload hashes on the workloads they belong to so a
// config change rolls the corresponding DaemonSet.
func applyMutator(configHash, nodeConfigHash string) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		if obj.GetKind() == component.CRDKind {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() == "ConfigMap" && obj.GetName() == configName {
			obj.Object = nil

			return nil
		}

		switch {
		case obj.GetKind() == "DaemonSet" && obj.GetName() == daemonSetName:
			return stampConfigHash(obj, configHashAnnotation, configHash)
		case obj.GetKind() == "DaemonSet" && obj.GetName() == nodeConfigDaemonSetName:
			return stampConfigHash(obj, nodeConfigHashAnnotation, nodeConfigHash)
		default:
			return nil
		}
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

// nodeConfigPayloadHash returns the payload hash of the embedded node-config
// hosts ConfigMap so the writer DaemonSet can be rolled when it changes.
func nodeConfigPayloadHash(env *component.Env) (string, error) {
	cm, err := env.DefaultConfigMap(gantrymanifests.Manifests, nodeConfigName, "gantry node-config")
	if err != nil {
		return "", err
	}

	return component.ConfigMapPayloadHash(cm), nil
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
