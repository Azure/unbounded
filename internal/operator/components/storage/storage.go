// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package storage implements the per-Site unbounded-storage supervisor
// component. The supervisor runs as a site-scoped DaemonSet on the nodes
// belonging to the Site, mounting a per-Site config ConfigMap.
package storage

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
	storagemanifests "github.com/Azure/unbounded/deploy/unbounded-storage-supervisor"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	daemonSetName = "unbounded-storage-supervisor"
	configName    = "unbounded-storage-config"
	configPrefix  = configName + "-"

	ConfigHashAnnotation = "unbounded-cloud.io/storage-config-hash"
)

// Component reconciles the per-Site storage supervisor.
type Component struct{}

// New returns the storage per-Site component.
func New() component.SiteComponent { return Component{} }

// Name implements component.SiteComponent.
func (Component) Name() string { return "storage" }

// ConditionType implements component.SiteComponent.
func (Component) ConditionType() string { return "StorageReady" }

// Enabled reports whether the Site enables storage.
func (Component) Enabled(site *unboundedv1alpha3.Site) bool {
	if site.Spec.Components.Storage == nil {
		return false
	}

	return unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.Storage.SiteComponentSpec)
}

// Reconcile deploys a site-scoped storage supervisor DaemonSet that runs only on
// the nodes belonging to the Site.
func (c Component) Plan(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	configHash, configOp, err := planConfig(ctx, env, site)
	if err != nil {
		return nil, component.Result{}, err
	}

	objects, err := env.DecodeManifestFS(storagemanifests.Manifests, func(obj *unstructured.Unstructured) error {
		return mutateObject(site, env.Config, configHash, obj)
	})
	if err != nil {
		return nil, component.Result{}, err
	}

	plan := component.NewPlan()

	var dependsOn []component.ObjectRef

	if configOp != nil {
		plan.Add(*configOp)

		dependsOn = []component.ObjectRef{configOp.Ref()}
	}

	daemonSet := SiteDaemonSetName(site.Name)

	for _, obj := range objects {
		op := component.Operation{
			Kind:      component.OpApply,
			Object:    obj,
			Component: c.Name(),
			Site:      site.Name,
		}

		if obj.GetKind() == "DaemonSet" && obj.GetName() == daemonSet {
			op.Overridable = true
			op.DependsOn = dependsOn
		} else {
			// The namespace and RBAC are identical for every Site, so they are
			// written once per pass rather than once per Site.
			op.SharedKey = "storage/shared/" + component.RefOf(obj).String()
		}

		plan.Add(op)
	}

	return plan, component.Reconciled(), nil
}

// Cleanup removes the per-site storage DaemonSet and ConfigMap.
func (c Component) CleanupPlan(_ context.Context, env *component.Env, site *unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	plan := component.NewPlan()
	plan.Add(
		component.DeleteOperation(&appsv1.DaemonSet{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
			ObjectMeta: metav1.ObjectMeta{Name: SiteDaemonSetName(site.Name), Namespace: env.Namespace},
		}, c.Name(), site.Name),
		component.DeleteOperation(&corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Name: SiteConfigName(site.Name), Namespace: env.Namespace},
		}, c.Name(), site.Name),
	)

	return plan, component.Disabled("component disabled"), nil
}

// SetupWatches reconciles a Site on changes to its per-site config payload and
// recreates the per-site DaemonSet if it is deleted or drifts. The DaemonSet
// predicate drops status-only updates so pod churn does not re-apply it.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&corev1.ConfigMap{},
		env.RequestSiteFromConfigName(configPrefix),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceWithPrefix(configPrefix))),
	)
	b.Owns(&appsv1.DaemonSet{}, builder.WithPredicates(env.OwnedWorkloadPredicate()))
}

// SiteDaemonSetName is the per-site storage supervisor DaemonSet name.
func SiteDaemonSetName(site string) string { return daemonSetName + "-" + site }

// SiteConfigName is the per-site storage supervisor ConfigMap name. Storage
// config is per-Site in the API, so each Site gets its own ConfigMap.
func SiteConfigName(site string) string { return configPrefix + site }

// ensureConfig creates the per-site storage ConfigMap from the embedded default
// when it is absent. If an operator/user already created it (or the reaper
// migrated it), the data is preserved and only adopted with a Site owner
// reference so it is garbage-collected with the Site.
// planConfig hashes the per-site storage config payload and returns the
// operation that reconciles it, if one is needed.
//
// An existing payload is adopted rather than replaced: only the Site owner
// reference is added, under optimistic lock, so the data an operator, user or
// the reaper put there survives and the ConfigMap is still garbage-collected
// with its Site. Owner references do not participate in the payload hash, so
// adoption never changes it.
//
// See net.planConfig for why the hash is computed at plan time.
func planConfig(ctx context.Context, env *component.Env, site *unboundedv1alpha3.Site) (string, *component.Operation, error) {
	key := client.ObjectKey{Namespace: env.Namespace, Name: SiteConfigName(site.Name)}
	existing := &corev1.ConfigMap{}

	err := env.Client.Get(ctx, key, existing)
	switch {
	case err == nil:
		return component.ConfigMapPayloadHash(existing), adoptOperation(site, existing), nil
	case !apierrors.IsNotFound(err):
		return "", nil, fmt.Errorf("get storage config %s/%s: %w", key.Namespace, key.Name, err)
	}

	cm, err := defaultConfigMap(site, env.Namespace)
	if err != nil {
		return "", nil, err
	}

	return component.ConfigMapPayloadHash(cm), &component.Operation{
		Kind:      component.OpCreateIfAbsent,
		Object:    component.ToUnstructured(cm),
		Component: Component{}.Name(),
		Site:      site.Name,
	}, nil
}

// adoptOperation returns the operation that adds the Site owner reference to an
// existing ConfigMap, or nil when it already carries one.
func adoptOperation(site *unboundedv1alpha3.Site, cm *corev1.ConfigMap) *component.Operation {
	refs, changed := component.UpsertOwnerReference(cm.OwnerReferences, component.SiteOwnerReference(site))
	if !changed {
		return nil
	}

	// Client.Get strips TypeMeta, so restore it before converting to
	// unstructured; the apiserver needs the GVK to route the patch.
	cm.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}

	before := cm.DeepCopy()

	adopted := cm.DeepCopy()
	adopted.OwnerReferences = refs

	return &component.Operation{
		Kind:      component.OpMergePatch,
		Object:    component.ToUnstructured(adopted),
		Base:      component.ToUnstructured(before),
		Component: Component{}.Name(),
		Site:      site.Name,
	}
}

func defaultConfigMap(site *unboundedv1alpha3.Site, namespace string) (*corev1.ConfigMap, error) {
	files, err := component.YamlFiles(storagemanifests.Manifests)
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		data, err := fs.ReadFile(storagemanifests.Manifests, file)
		if err != nil {
			return nil, fmt.Errorf("read storage manifest %s: %w", file, err)
		}

		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

		for {
			obj := &unstructured.Unstructured{}
			if err := decoder.Decode(obj); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				return nil, fmt.Errorf("decode storage manifest %s: %w", file, err)
			}

			if obj.Object == nil || obj.GetKind() != "ConfigMap" || obj.GetName() != configName {
				continue
			}

			cm := &corev1.ConfigMap{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, cm); err != nil {
				return nil, fmt.Errorf("convert storage config template: %w", err)
			}

			cm.Name = SiteConfigName(site.Name)
			cm.Namespace = namespace
			cm.OwnerReferences = []metav1.OwnerReference{component.SiteOwnerReference(site)}

			return cm, nil
		}
	}

	return nil, errors.New("storage config template not found")
}

// mutateObject scopes the storage supervisor manifests to the Site. The
// DaemonSet is per-site (name, labels, node affinity, and config mount). The
// per-site ConfigMap is handled by ensureConfig so existing config data is
// preserved; the ServiceAccount and RBAC are shared across sites.
func mutateObject(site *unboundedv1alpha3.Site, cfg component.Config, configHash string, obj *unstructured.Unstructured) error {
	switch {
	case obj.GetKind() == "DaemonSet" && obj.GetName() == daemonSetName:
		if err := component.SetPodSpecImages(obj, cfg.Image(daemonSetName)); err != nil {
			return fmt.Errorf("set storage supervisor images: %w", err)
		}

		return scopeDaemonSetToSite(site, configHash, obj)
	case obj.GetKind() == "ConfigMap" && obj.GetName() == configName:
		obj.Object = nil

		return nil
	default:
		return nil
	}
}

func scopeDaemonSetToSite(site *unboundedv1alpha3.Site, configHash string, obj *unstructured.Unstructured) error {
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
			return fmt.Errorf("scope storage daemonset (%v): %w", path, err)
		}
	}

	affinityMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(component.SiteNodeAffinity(site.Name))
	if err != nil {
		return fmt.Errorf("scope storage daemonset affinity: %w", err)
	}

	if err := unstructured.SetNestedMap(obj.Object, affinityMap, "spec", "template", "spec", "affinity"); err != nil {
		return fmt.Errorf("set storage daemonset affinity: %w", err)
	}

	annotations, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		return fmt.Errorf("get storage daemonset pod template annotations: %w", err)
	}

	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[ConfigHashAnnotation] = configHash
	if err := unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
		return fmt.Errorf("set storage config hash annotation: %w", err)
	}

	return pointDaemonSetAtSiteConfig(site, obj)
}

// pointDaemonSetAtSiteConfig repoints the DaemonSet's config-source volume at the
// Site's per-site ConfigMap so each Site mounts its own storage config.
func pointDaemonSetAtSiteConfig(site *unboundedv1alpha3.Site, obj *unstructured.Unstructured) error {
	volumes, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return fmt.Errorf("get storage daemonset volumes: %w", err)
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
		return fmt.Errorf("set storage daemonset volumes: %w", err)
	}

	return nil
}
