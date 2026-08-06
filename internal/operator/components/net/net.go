// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package net implements the unbounded-net cluster component. Net is not a
// per-Site component in the classic sense: one controller reads every Site, so
// the controller and its config are reconciled as cluster singletons whenever at
// least one Site exists and kept reconciled if already installed.
//
// The node dataplane, however, is split so a Site can pull the node image from
// its own registry and mount its own config. A cluster-wide "base" node
// DaemonSet (unbounded-net-node) runs on the nodes that do not belong to any
// Site (control plane and other un-Sited nodes) using the operator-wide registry
// and the shared config, while a per-Site DaemonSet (unbounded-net-node-<site>)
// runs on each Site's nodes using that Site's imageRegistry override and per-Site
// config. Node affinity partitions the two (a Node carries at most one site
// label), so every Node is covered by exactly one DaemonSet.
package net

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
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

	// nodeConfigPrefix names the per-Site node config ConfigMaps
	// (unbounded-net-config-<site>). The shared config keeps the bare
	// configName, so the trailing hyphen keeps the two disjoint.
	nodeConfigPrefix = configName + "-"

	// nodeDaemonSetManifest is the base node DaemonSet within the embedded net
	// manifests. Per-Site node DaemonSets are derived from it.
	nodeDaemonSetManifest = "node/03-daemonset.yaml"

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

// Reconcile deploys the unbounded-net controller and base node DaemonSet whenever
// at least one Site exists, keeps an existing installation reconciled with no
// Sites, and reconciles a per-Site node DaemonSet (and its config) for each Site.
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

	if err := env.ApplyManifestFS(ctx, netmanifests.Manifests, applyMutator(env.Config, configHash)); err != nil {
		return component.Failed(err)
	}

	if err := reconcileSiteNodes(ctx, env, sites); err != nil {
		return component.Failed(err)
	}

	return component.Reconciled()
}

// SetupWatches reconciles net on changes to its config payloads and on
// create/delete/generation changes of its managed workloads. Per-Site node
// DaemonSets are owned by their Site, so Owns() re-enqueues the Site when one
// drifts or is deleted.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&corev1.ConfigMap{}, env.RequestSingletonAndAllSites(),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(configName))))
	b.Watches(&corev1.ConfigMap{}, env.RequestSiteFromConfigName(nodeConfigPrefix),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceWithPrefix(nodeConfigPrefix))))
	b.Watches(&appsv1.Deployment{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(controllerName))))
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(nodeName))))
	b.Owns(&appsv1.DaemonSet{}, builder.WithPredicates(env.OwnedWorkloadPredicate()))
}

// SiteConfigName is the per-Site node config ConfigMap name.
func SiteConfigName(site string) string { return nodeConfigPrefix + site }

// SiteNodeDaemonSetName is the per-Site node DaemonSet name.
func SiteNodeDaemonSetName(site string) string { return nodeName + "-" + site }

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

// applyMutator skips the separately reconciled ConfigMap and CRDs, scopes the
// base node DaemonSet to un-Sited nodes, and stamps the shared config payload
// hash on both cluster workloads so config changes roll them together.
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

		if obj.GetKind() == "DaemonSet" && obj.GetName() == nodeName {
			// The base node DaemonSet runs only on nodes that do not belong to
			// any Site; each Site's nodes are covered by its per-Site DaemonSet.
			if err := setAffinity(obj, component.UnsitedNodeAffinity()); err != nil {
				return fmt.Errorf("scope base net node to un-Sited nodes: %w", err)
			}
		}

		if (obj.GetKind() == "Deployment" && obj.GetName() == controllerName) ||
			(obj.GetKind() == "DaemonSet" && obj.GetName() == nodeName) {
			if err := component.SetPodSpecImages(obj, cfg.Image(obj.GetName())); err != nil {
				return fmt.Errorf("set net workload images: %w", err)
			}

			if err := stampConfigHash(obj, configHash); err != nil {
				return err
			}
		}

		return nil
	}
}

// reconcileSiteNodes reconciles the per-Site node DaemonSet and its config for
// every Site. The DaemonSets are owned by their Site, so they are garbage
// collected when the Site is deleted.
func reconcileSiteNodes(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) error {
	base, err := baseNodeDaemonSet()
	if err != nil {
		return err
	}

	for i := range sites {
		site := &sites[i]

		configHash, err := ensureSiteConfig(ctx, env, site)
		if err != nil {
			return err
		}

		ds := base.DeepCopy()
		if err := scopeNodeDaemonSetToSite(site, component.ConfigForSite(env.Config, site), configHash, ds); err != nil {
			return fmt.Errorf("scope net node for site %s: %w", site.Name, err)
		}

		env.RetargetNamespace(ds)

		if err := env.ApplyObject(ctx, ds); err != nil {
			return err
		}
	}

	return nil
}

// baseNodeDaemonSet decodes the base node DaemonSet from the embedded manifest,
// which may bundle several documents ahead of it.
func baseNodeDaemonSet() (*unstructured.Unstructured, error) {
	data, err := fs.ReadFile(netmanifests.Manifests, nodeDaemonSetManifest)
	if err != nil {
		return nil, fmt.Errorf("read base net node manifest: %w", err)
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, fmt.Errorf("decode base net node manifest: %w", err)
		}

		if obj.GetKind() == "DaemonSet" && obj.GetName() == nodeName {
			return obj, nil
		}
	}

	return nil, fmt.Errorf("base net node DaemonSet %q not found in manifest", nodeName)
}

// scopeNodeDaemonSetToSite turns the base node DaemonSet into the Site's own
// DaemonSet: a per-Site name, labels, selector, node affinity, image (from the
// Site's registry), config mount, config-hash, and Site owner reference.
func scopeNodeDaemonSetToSite(site *unboundedv1alpha3.Site, cfg component.Config, configHash string, obj *unstructured.Unstructured) error {
	obj.SetName(SiteNodeDaemonSetName(site.Name))
	obj.SetOwnerReferences([]metav1.OwnerReference{component.SiteOwnerReference(site)})

	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}

	labels[component.SiteLabelKey] = site.Name
	obj.SetLabels(labels)

	for _, path := range [][]string{
		{"spec", "selector", "matchLabels", component.SiteLabelKey},
		{"spec", "template", "metadata", "labels", component.SiteLabelKey},
	} {
		if err := unstructured.SetNestedField(obj.Object, site.Name, path...); err != nil {
			return fmt.Errorf("scope net node daemonset (%v): %w", path, err)
		}
	}

	if err := setAffinity(obj, component.SiteNodeAffinity(site.Name)); err != nil {
		return fmt.Errorf("set net node affinity: %w", err)
	}

	if err := component.SetPodSpecImages(obj, cfg.Image(nodeName)); err != nil {
		return fmt.Errorf("set net node image: %w", err)
	}

	if err := pointNodeDaemonSetAtSiteConfig(site, obj); err != nil {
		return err
	}

	return stampConfigHash(obj, configHash)
}

// pointNodeDaemonSetAtSiteConfig repoints the DaemonSet's config volume and the
// LOG_LEVEL env reference at the Site's per-Site config ConfigMap so each Site
// mounts its own node config.
func pointNodeDaemonSetAtSiteConfig(site *unboundedv1alpha3.Site, obj *unstructured.Unstructured) error {
	siteConfig := SiteConfigName(site.Name)

	volumes, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return fmt.Errorf("get net node volumes: %w", err)
	}

	if found {
		for i, v := range volumes {
			vol, ok := v.(map[string]any)
			if !ok {
				continue
			}

			cm, ok := vol["configMap"].(map[string]any)
			if !ok {
				continue
			}

			if cm["name"] == configName {
				cm["name"] = siteConfig
				vol["configMap"] = cm
				volumes[i] = vol
			}
		}

		if err := unstructured.SetNestedSlice(obj.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
			return fmt.Errorf("set net node volumes: %w", err)
		}
	}

	return repointConfigEnv(obj, configName, siteConfig)
}

// repointConfigEnv rewrites every container env var (init and main) that sources
// a value from the old ConfigMap to the new one, so per-Site node pods read
// LOG_LEVEL (and any other configMapKeyRef) from their own config.
func repointConfigEnv(obj *unstructured.Unstructured, oldName, newName string) error {
	for _, field := range []string{"initContainers", "containers"} {
		containers, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
		if err != nil {
			return fmt.Errorf("get net node %s: %w", field, err)
		}

		if !found {
			continue
		}

		changed := false

		for i := range containers {
			container, ok := containers[i].(map[string]any)
			if !ok {
				continue
			}

			envVars, ok := container["env"].([]any)
			if !ok {
				continue
			}

			for j := range envVars {
				envVar, ok := envVars[j].(map[string]any)
				if !ok {
					continue
				}

				valueFrom, ok := envVar["valueFrom"].(map[string]any)
				if !ok {
					continue
				}

				ref, ok := valueFrom["configMapKeyRef"].(map[string]any)
				if !ok {
					continue
				}

				if ref["name"] == oldName {
					ref["name"] = newName
					changed = true
				}
			}
		}

		if changed {
			if err := unstructured.SetNestedSlice(obj.Object, containers, "spec", "template", "spec", field); err != nil {
				return fmt.Errorf("set net node %s: %w", field, err)
			}
		}
	}

	return nil
}

// setAffinity sets the pod template node affinity on a workload.
func setAffinity(obj *unstructured.Unstructured, affinity *corev1.Affinity) error {
	affinityMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(affinity)
	if err != nil {
		return fmt.Errorf("convert affinity: %w", err)
	}

	if err := unstructured.SetNestedMap(obj.Object, affinityMap, "spec", "template", "spec", "affinity"); err != nil {
		return fmt.Errorf("set affinity: %w", err)
	}

	return nil
}

// stampConfigHash sets the net config-hash annotation on a workload pod template
// so a config change rolls it.
func stampConfigHash(obj *unstructured.Unstructured, configHash string) error {
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

	return nil
}

// ensureConfig creates the embedded default only when no shared config exists.
// Existing migrated or user-managed payloads are never applied over.
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

// ensureSiteConfig creates the per-Site node config ConfigMap by cloning the
// shared config when it is absent, so a Site starts from the operator-tuned
// baseline. An existing per-Site config (operator/user-edited) is preserved and
// only adopted with a Site owner reference so it is garbage collected with the
// Site.
func ensureSiteConfig(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) (string, error) {
	key := client.ObjectKey{Namespace: env.Namespace, Name: SiteConfigName(site.Name)}
	existing := &corev1.ConfigMap{}

	err := env.Client.Get(ctx, key, existing)
	switch {
	case err == nil:
		if err := adoptSiteConfig(ctx, env, site, existing); err != nil {
			return "", err
		}

		return component.ConfigMapPayloadHash(existing), nil
	case !apierrors.IsNotFound(err):
		return "", fmt.Errorf("get net site config %s/%s: %w", key.Namespace, key.Name, err)
	}

	cm, err := seedSiteConfig(ctx, env, site)
	if err != nil {
		return "", err
	}

	if err := env.Client.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create net site config %s/%s: %w", cm.Namespace, cm.Name, err)
		}

		if getErr := env.Client.Get(ctx, key, existing); getErr != nil {
			return "", fmt.Errorf("get raced net site config %s/%s: %w", key.Namespace, key.Name, getErr)
		}

		if err := adoptSiteConfig(ctx, env, site, existing); err != nil {
			return "", err
		}

		return component.ConfigMapPayloadHash(existing), nil
	}

	return component.ConfigMapPayloadHash(cm), nil
}

// seedSiteConfig builds the per-Site node config from the shared config so
// operator tuning carries over. It falls back to the embedded default if the
// shared config is somehow absent.
func seedSiteConfig(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) (*corev1.ConfigMap, error) {
	shared := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: env.Namespace, Name: configName}

	err := env.Client.Get(ctx, key, shared)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		shared, err = env.DefaultConfigMap(netmanifests.Manifests, configName, "net")
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("get net config to seed site %s: %w", site.Name, err)
	}

	// Deep-copy the payload so the per-Site ConfigMap does not alias the shared
	// config's maps (a later mutation of either must not affect the other).
	payload := shared.DeepCopy()

	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: SiteConfigName(site.Name), Namespace: env.Namespace, OwnerReferences: []metav1.OwnerReference{component.SiteOwnerReference(site)}},
		Data:       payload.Data,
		BinaryData: payload.BinaryData,
	}

	return cm, nil
}

func adoptSiteConfig(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site, cm *corev1.ConfigMap) error {
	owner := component.SiteOwnerReference(site)

	refs, changed := component.UpsertOwnerReference(cm.OwnerReferences, owner)
	if !changed {
		return nil
	}

	before := cm.DeepCopy()
	cm.OwnerReferences = refs

	patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
	if err := env.Client.Patch(ctx, cm, patch); err != nil {
		return fmt.Errorf("adopt net site config %s/%s: %w", cm.Namespace, cm.Name, err)
	}

	return nil
}
