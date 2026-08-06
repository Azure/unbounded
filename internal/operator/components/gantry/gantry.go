// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package gantry implements the gantry peer-to-peer OCI distribution cluster
// component. Gantry runs one agent DaemonSet per node fronting containerd's
// image pulls, so it is reconciled as a cluster singleton. Unlike the other
// components it defaults to enabled: it is deployed unless every Site explicitly
// opts out via spec.components.gantry.enabled=false.
//
// Like the net node dataplane, the agent DaemonSet is split so a Site can pull
// the gantry image from its own registry and mount its own config. A cluster-wide
// "base" DaemonSet (gantry) runs on the nodes that do not belong to any Site
// using the operator-wide registry and the shared config, while a per-Site
// DaemonSet (gantry-<site>) runs on each gantry-enabled Site's nodes using that
// Site's imageRegistry override and per-Site config. Node affinity partitions the
// two so every Node is covered by at most one DaemonSet.
//
// The unbounded agent manages the per-node containerd wiring, so the operator
// installs only the Gantry workload and its Kubernetes configuration.
package gantry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

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
	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	daemonSetName = "gantry"
	configName    = "gantry-config"

	// configPrefix names the per-Site config ConfigMaps (gantry-config-<site>).
	// The shared config keeps the bare configName; the trailing hyphen keeps the
	// two disjoint.
	configPrefix = configName + "-"

	// daemonSetManifest is the base DaemonSet within the embedded gantry
	// manifests. Per-Site DaemonSets are derived from it.
	daemonSetManifest = "daemonset.yaml"

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

// Reconcile deploys the gantry base DaemonSet whenever at least one Site does not
// opt out, keeps an existing installation reconciled when every Site opts out,
// and reconciles a per-Site DaemonSet (and its config) for each gantry-enabled
// Site.
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

	if err := reconcileSiteDaemonSets(ctx, env, sites); err != nil {
		return component.Failed(err)
	}

	return component.Reconciled()
}

// SetupWatches reconciles Gantry on changes to its active resources, when legacy
// node-config resources appear so they can be removed, and on drift or deletion
// of the per-Site DaemonSets (owned by their Site) and per-Site config.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&corev1.ConfigMap{}, env.RequestSingletonAndAllSites(),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(configName, legacyNodeConfigName))))
	b.Watches(&corev1.ConfigMap{}, env.RequestSiteFromConfigName(configPrefix),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceWithPrefix(configPrefix))))
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(daemonSetName, legacyNodeConfigDaemonSetName))))
	b.Owns(&appsv1.DaemonSet{}, builder.WithPredicates(env.OwnedWorkloadPredicate()))
}

// SiteConfigName is the per-Site gantry config ConfigMap name.
func SiteConfigName(site string) string { return configPrefix + site }

// SiteDaemonSetName is the per-Site gantry DaemonSet name.
func SiteDaemonSetName(site string) string { return daemonSetName + "-" + site }

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
// scopes the base agent DaemonSet to un-Sited nodes, stamps the operator-derived
// agent image onto it, and stamps the config payload hash on its pod template so
// a config change rolls the DaemonSet. Only the agent's own container is
// re-imaged.
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
			// The base DaemonSet runs only on nodes that do not belong to any
			// Site; each Site's nodes are covered by its per-Site DaemonSet.
			if err := setAffinity(obj, component.UnsitedNodeAffinity()); err != nil {
				return fmt.Errorf("scope base gantry to un-Sited nodes: %w", err)
			}

			if err := component.SetNamedContainerImage(obj, agentContainerName, agentImage); err != nil {
				return fmt.Errorf("set gantry agent image: %w", err)
			}

			return stampConfigHash(obj, configHashAnnotation, configHash)
		}

		return nil
	}
}

// reconcileSiteDaemonSets reconciles the per-Site gantry DaemonSet and its config
// for every gantry-enabled Site, and tears down the per-Site resources of a Site
// that opts out. The DaemonSets are owned by their Site, so they are also garbage
// collected when the Site is deleted.
func reconcileSiteDaemonSets(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) error {
	base, err := baseDaemonSet()
	if err != nil {
		return err
	}

	for i := range sites {
		site := &sites[i]

		if !EnabledFor(site) {
			if err := cleanupSiteDaemonSet(ctx, env, site); err != nil {
				return err
			}

			continue
		}

		configHash, err := ensureSiteConfig(ctx, env, site)
		if err != nil {
			return err
		}

		ds := base.DeepCopy()
		if err := scopeDaemonSetToSite(site, component.ConfigForSite(env.Config, site), configHash, ds); err != nil {
			return fmt.Errorf("scope gantry for site %s: %w", site.Name, err)
		}

		env.RetargetNamespace(ds)

		if err := env.ApplyObject(ctx, ds); err != nil {
			return err
		}
	}

	return nil
}

// cleanupSiteDaemonSet removes the per-Site DaemonSet and config for a Site that
// opts out of gantry. The Site's nodes then run no gantry (the base excludes
// Sited nodes).
func cleanupSiteDaemonSet(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) error {
	if err := env.DeleteIfExists(ctx, &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: SiteDaemonSetName(site.Name), Namespace: env.Namespace},
	}); err != nil {
		return err
	}

	return env.DeleteIfExists(ctx, &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: SiteConfigName(site.Name), Namespace: env.Namespace},
	})
}

// baseDaemonSet decodes the base gantry DaemonSet from the embedded manifest,
// which may bundle several documents (for example a PriorityClass) ahead of it.
func baseDaemonSet() (*unstructured.Unstructured, error) {
	data, err := fs.ReadFile(gantrymanifests.Manifests, daemonSetManifest)
	if err != nil {
		return nil, fmt.Errorf("read base gantry manifest: %w", err)
	}

	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return nil, fmt.Errorf("decode base gantry manifest: %w", err)
		}

		if obj.GetKind() == "DaemonSet" && obj.GetName() == daemonSetName {
			return obj, nil
		}
	}

	return nil, fmt.Errorf("base gantry DaemonSet %q not found in manifest", daemonSetName)
}

// scopeDaemonSetToSite turns the base gantry DaemonSet into the Site's own
// DaemonSet: a per-Site name, labels, selector, node affinity, agent image (from
// the Site's registry), config mount, config-hash, and Site owner reference.
func scopeDaemonSetToSite(site *unboundedv1alpha3.Site, cfg component.Config, configHash string, obj *unstructured.Unstructured) error {
	obj.SetName(SiteDaemonSetName(site.Name))
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
			return fmt.Errorf("scope gantry daemonset (%v): %w", path, err)
		}
	}

	if err := setAffinity(obj, component.SiteNodeAffinity(site.Name)); err != nil {
		return fmt.Errorf("set gantry affinity: %w", err)
	}

	if err := component.SetNamedContainerImage(obj, agentContainerName, cfg.Image(imageRepository)); err != nil {
		return fmt.Errorf("set gantry agent image: %w", err)
	}

	if err := pointDaemonSetAtSiteConfig(site, obj); err != nil {
		return err
	}

	return stampConfigHash(obj, configHashAnnotation, configHash)
}

// pointDaemonSetAtSiteConfig repoints the DaemonSet's config volume at the Site's
// per-Site config ConfigMap so each Site mounts its own gantry config.
func pointDaemonSetAtSiteConfig(site *unboundedv1alpha3.Site, obj *unstructured.Unstructured) error {
	volumes, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return fmt.Errorf("get gantry volumes: %w", err)
	}

	if !found {
		return nil
	}

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
			cm["name"] = SiteConfigName(site.Name)
			vol["configMap"] = cm
			volumes[i] = vol
		}
	}

	if err := unstructured.SetNestedSlice(obj.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		return fmt.Errorf("set gantry volumes: %w", err)
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

// ensureConfig creates the embedded default only when no shared config exists. An
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

// ensureSiteConfig creates the per-Site config ConfigMap by cloning the shared
// gantry config when it is absent, so a Site starts from the operator/user-tuned
// baseline (for example an edited upstream_registries list). An existing per-Site
// config is preserved and only adopted with a Site owner reference so it is
// garbage collected with the Site.
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
		return "", fmt.Errorf("get gantry site config %s/%s: %w", key.Namespace, key.Name, err)
	}

	cm, err := seedSiteConfig(ctx, env, site)
	if err != nil {
		return "", err
	}

	if err := env.Client.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create gantry site config %s/%s: %w", cm.Namespace, cm.Name, err)
		}

		if getErr := env.Client.Get(ctx, key, existing); getErr != nil {
			return "", fmt.Errorf("get raced gantry site config %s/%s: %w", key.Namespace, key.Name, getErr)
		}

		if err := adoptSiteConfig(ctx, env, site, existing); err != nil {
			return "", err
		}

		return component.ConfigMapPayloadHash(existing), nil
	}

	return component.ConfigMapPayloadHash(cm), nil
}

// seedSiteConfig builds the per-Site config from the shared gantry config so
// operator/user tuning carries over. It falls back to the embedded default if the
// shared config is somehow absent.
func seedSiteConfig(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) (*corev1.ConfigMap, error) {
	shared := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: env.Namespace, Name: configName}

	err := env.Client.Get(ctx, key, shared)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		shared, err = env.DefaultConfigMap(gantrymanifests.Manifests, configName, "gantry")
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("get gantry config to seed site %s: %w", site.Name, err)
	}

	cm := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: SiteConfigName(site.Name), Namespace: env.Namespace, OwnerReferences: []metav1.OwnerReference{component.SiteOwnerReference(site)}},
		Data:       shared.Data,
		BinaryData: shared.BinaryData,
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
		return fmt.Errorf("adopt gantry site config %s/%s: %w", cm.Namespace, cm.Name, err)
	}

	return nil
}
