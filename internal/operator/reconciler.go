// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	netmanifests "github.com/Azure/unbounded/deploy/net"
	storagemanifests "github.com/Azure/unbounded/deploy/unbounded-storage-supervisor"
	"github.com/Azure/unbounded/internal/unbounded"
)

const (
	FieldOwner = "unbounded-operator"

	ComponentNet      = "net"
	ComponentMachina  = "machina"
	ComponentMetalman = "metalman"
	ComponentStorage  = "storage"

	// siteLabelKey is the canonical node label for site membership
	// (unbounded-cloud.io/site). The net controller applies it to Nodes and the
	// operator uses it to node-select site-scoped components (storage, metalman).
	siteLabelKey = unboundedv1alpha3.MachineSiteLabelKey

	// siteFinalizer guards Site deletion so the operator can tear down the
	// per-site workloads it created (metalman Deployment, storage DaemonSet and
	// config) before the Site object is removed.
	siteFinalizer = "unbounded-cloud.io/operator-cleanup"
)

// DefaultNamespace is the namespace the operator installs components into. It
// follows the operator's own namespace (unbounded.SystemNamespace()).
var DefaultNamespace = unbounded.SystemNamespace()

// buildDefaultNamespace is the namespace baked into the embedded component
// manifests at build time (the render default). retargetNamespace rewrites it to
// the operator's configured namespace when they differ. It must match the
// manifest render default and internal/unbounded.systemNamespace.
const buildDefaultNamespace = "unbounded-system"

type Config struct {
	// MetalmanImage is the image for the synthesized per-site metalman
	// Deployment. It is stamped to the operator's version by the operator
	// Deployment; the other components carry their version-matched image in
	// the embedded release manifests.
	MetalmanImage string

	// APIServerEndpoint is injected into the machina controller config.
	APIServerEndpoint string
}

type SiteReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config Config

	// Namespace is the namespace components are reconciled into. When empty it
	// falls back to DefaultNamespace so the operator follows the namespace it
	// is installed in (see cmd/unbounded-operator --namespace).
	Namespace string
}

// namespace returns the namespace the reconciler installs components into,
// falling back to the package default when unset.
func (r *SiteReconciler) namespace() string {
	if r.Namespace != "" {
		return r.Namespace
	}

	return DefaultNamespace
}

func (r *SiteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var site unboundedv1alpha3.Site
	if err := r.Get(ctx, req.NamespacedName, &site); err != nil {
		if apierrors.IsNotFound(err) {
			// A Site was deleted; reconcile the cluster singletons so net/machina
			// are torn down when the last Site goes away.
			return ctrl.Result{}, r.reconcileSingletons(ctx)
		}

		return ctrl.Result{}, err
	}

	// A Site pending deletion: tear down the per-site workloads the operator
	// created, then release the finalizer. The cluster singletons are
	// re-evaluated on the subsequent not-found reconcile once the object is
	// gone.
	if !site.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalizeSite(ctx, &site)
	}

	if controllerutil.AddFinalizer(&site, siteFinalizer) {
		if err := r.Update(ctx, &site); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
	}

	statuses := map[string]unboundedv1alpha3.SiteComponentStatus{}

	// Cluster singletons: net is deployed whenever at least one Site exists;
	// machina whenever any Site enables it. Reconciled idempotently on every
	// Site event and reported on each Site's status for visibility.
	statuses[ComponentNet] = r.reconcileNet(ctx)
	statuses[ComponentMachina] = r.reconcileMachina(ctx)

	// Per-site components: reconcile when enabled, tear down when disabled so
	// spec.components.*.enabled is declarative.
	statuses[ComponentMetalman] = r.reconcilePerSiteComponent(
		componentEnabled(&site, ComponentMetalman),
		func() error { return r.reconcileMetalman(ctx, &site) },
		func() error { return r.cleanupMetalman(ctx, &site) },
	)
	statuses[ComponentStorage] = r.reconcilePerSiteComponent(
		componentEnabled(&site, ComponentStorage),
		func() error { return r.reconcileStorage(ctx, &site) },
		func() error { return r.cleanupStorage(ctx, &site) },
	)

	for name, status := range statuses {
		if !status.Ready {
			logger.Info("component not ready", "site", site.Name, "component", name, "message", status.Message)
		}
	}

	patch := client.MergeFrom(site.DeepCopy())

	site.Status.Components = statuses
	if err := r.Status().Patch(ctx, &site, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch site status: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *SiteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&unboundedv1alpha3.Site{}).
		Complete(r)
}

// finalizeSite removes the per-site workloads the operator created for a Site
// that is being deleted and then releases the operator finalizer.
func (r *SiteReconciler) finalizeSite(ctx context.Context, site *unboundedv1alpha3.Site) error {
	if err := r.cleanupMetalman(ctx, site); err != nil {
		return err
	}

	if err := r.cleanupStorage(ctx, site); err != nil {
		return err
	}

	if controllerutil.RemoveFinalizer(site, siteFinalizer) {
		if err := r.Update(ctx, site); err != nil {
			return fmt.Errorf("remove finalizer: %w", err)
		}
	}

	return nil
}

// reconcilePerSiteComponent reconciles a per-site component when enabled and
// tears it down when disabled, turning the outcome into a SiteComponentStatus.
func (r *SiteReconciler) reconcilePerSiteComponent(enabled bool, reconcile, cleanup func() error) unboundedv1alpha3.SiteComponentStatus {
	if !enabled {
		if err := cleanup(); err != nil {
			return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
		}

		return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "disabled"}
	}

	if err := reconcile(); err != nil {
		return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
	}

	return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "reconciled"}
}

// reconcileSingletons reconciles just the cluster-singleton components (net,
// machina); used when a Site is deleted.
func (r *SiteReconciler) reconcileSingletons(ctx context.Context) error {
	if s := r.reconcileNet(ctx); !s.Ready {
		return fmt.Errorf("net: %s", s.Message)
	}

	if s := r.reconcileMachina(ctx); !s.Ready {
		return fmt.Errorf("machina: %s", s.Message)
	}

	return nil
}

// reconcileNet deploys the unbounded-net cluster singleton whenever at least
// one Site exists. Net is not a per-Site component: a single controller/node
// pair reads the networking spec of every Site.
func (r *SiteReconciler) reconcileNet(ctx context.Context) unboundedv1alpha3.SiteComponentStatus {
	sites, err := r.listSites(ctx)
	if err != nil {
		return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
	}

	if len(sites) == 0 {
		// No Sites left: tear down the net singleton so a drained cluster does
		// not keep hostNetwork net workloads running.
		if err := r.deleteManifestFS(ctx, netmanifests.Manifests, nil); err != nil {
			return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
		}

		return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "no sites"}
	}

	if err := r.applyManifestFS(ctx, netmanifests.Manifests, nil); err != nil {
		return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
	}

	return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "reconciled"}
}

// reconcileMachina deploys the machina controller singleton whenever any Site
// enables it. (Machina is a singleton for now; it may become site-scoped.)
func (r *SiteReconciler) reconcileMachina(ctx context.Context) unboundedv1alpha3.SiteComponentStatus {
	sites, err := r.listSites(ctx)
	if err != nil {
		return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
	}

	enabled := false

	for i := range sites {
		if componentEnabled(&sites[i], ComponentMachina) {
			enabled = true
			break
		}
	}

	if !enabled {
		// No Site enables machina: tear down the singleton. The metalman RBAC
		// that also ships in the machina manifests is skipped by
		// mutateMachinaObject, so per-site metalman is unaffected.
		if err := r.deleteManifestFS(ctx, machinamanifests.Manifests, r.mutateMachinaObject); err != nil {
			return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
		}

		return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "disabled"}
	}

	if err := r.applyManifestFS(ctx, machinamanifests.Manifests, r.machinaApplyMutator(ctx)); err != nil {
		return unboundedv1alpha3.SiteComponentStatus{Ready: false, Message: err.Error()}
	}

	return unboundedv1alpha3.SiteComponentStatus{Ready: true, Message: "reconciled"}
}

// machinaApplyMutator returns the mutate used when applying the machina
// singleton. It layers migrated-config preservation on top of mutateMachinaObject:
// the machina-config ConfigMap is created with embedded defaults only when it is
// absent, so config migrated by the reaper (or later edited by an operator) is
// never clobbered by a force-apply of the embedded defaults.
func (r *SiteReconciler) machinaApplyMutator(ctx context.Context) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		if err := r.mutateMachinaObject(obj); err != nil {
			return err
		}

		if obj.Object == nil {
			return nil
		}

		if obj.GetKind() == "ConfigMap" && obj.GetName() == "machina-config" {
			exists, err := r.objectExists(ctx, r.namespace(), "machina-config", &corev1.ConfigMap{})
			if err != nil {
				return err
			}

			if exists {
				// Preserve the existing (migrated/edited) config.
				obj.Object = nil
			}
		}

		return nil
	}
}

// objectExists reports whether a namespaced object of the given empty type
// exists.
func (r *SiteReconciler) objectExists(ctx context.Context, namespace, name string, obj client.Object) (bool, error) {
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, fmt.Errorf("get %s/%s: %w", namespace, name, err)
	}
}

// reconcileMetalman deploys the per-site metalman PXE controller and its RBAC.
func (r *SiteReconciler) reconcileMetalman(ctx context.Context, site *unboundedv1alpha3.Site) error {
	if err := r.applyManifestFS(ctx, machinamanifests.Manifests, mutateMetalmanSupportObject); err != nil {
		return err
	}

	return r.applyObject(ctx, metalmanDeployment(site, r.namespace(), r.Config))
}

// reconcileStorage deploys a site-scoped storage supervisor DaemonSet that runs
// only on the nodes belonging to the Site.
func (r *SiteReconciler) reconcileStorage(ctx context.Context, site *unboundedv1alpha3.Site) error {
	return r.applyManifestFS(ctx, storagemanifests.Manifests, func(obj *unstructured.Unstructured) error {
		return mutateStorageObject(site, obj)
	})
}

// cleanupMetalman removes the per-site metalman Deployment when metalman is
// disabled on the Site or the Site is being deleted. The shared metalman RBAC is
// left in place; it is harmless when unreferenced and may still be used by other
// sites.
func (r *SiteReconciler) cleanupMetalman(ctx context.Context, site *unboundedv1alpha3.Site) error {
	return r.deleteIfExists(ctx, &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Name: metalmanDeploymentName(site.Name), Namespace: r.namespace()},
	})
}

// cleanupStorage removes the per-site storage DaemonSet and ConfigMap when
// storage is disabled on the Site or the Site is being deleted.
func (r *SiteReconciler) cleanupStorage(ctx context.Context, site *unboundedv1alpha3.Site) error {
	if err := r.deleteIfExists(ctx, &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: storageDaemonSetName(site.Name), Namespace: r.namespace()},
	}); err != nil {
		return err
	}

	return r.deleteIfExists(ctx, &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: storageConfigName(site.Name), Namespace: r.namespace()},
	})
}

// deleteIfExists deletes an object, treating an already-absent object as success.
func (r *SiteReconciler) deleteIfExists(ctx context.Context, obj client.Object) error {
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete %s %s/%s: %w",
			obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName(), err)
	}

	return nil
}

func (r *SiteReconciler) listSites(ctx context.Context) ([]unboundedv1alpha3.Site, error) {
	var sites unboundedv1alpha3.SiteList
	if err := r.List(ctx, &sites); err != nil {
		return nil, fmt.Errorf("list sites: %w", err)
	}

	return sites.Items, nil
}

func (r *SiteReconciler) applyManifestFS(ctx context.Context, manifests fs.FS, mutate func(*unstructured.Unstructured) error) error {
	files, err := yamlFiles(manifests)
	if err != nil {
		return err
	}

	for _, file := range files {
		data, err := fs.ReadFile(manifests, file)
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", file, err)
		}

		if err := r.applyManifestData(ctx, data, mutate); err != nil {
			return fmt.Errorf("apply manifest %s: %w", file, err)
		}
	}

	return nil
}

func (r *SiteReconciler) applyManifestData(ctx context.Context, data []byte, mutate func(*unstructured.Unstructured) error) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return fmt.Errorf("decode resource: %w", err)
		}

		if obj.Object == nil {
			continue
		}

		if mutate != nil {
			if err := mutate(obj); err != nil {
				return err
			}
		}

		if obj.Object == nil {
			continue
		}

		r.retargetNamespace(obj)

		if err := r.applyObject(ctx, obj); err != nil {
			return err
		}
	}

	return nil
}

func (r *SiteReconciler) applyObject(ctx context.Context, obj client.Object) error {
	applyCfg := client.ApplyConfigurationFromUnstructured(toUnstructured(obj))
	if err := r.Apply(ctx, applyCfg, client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("apply %s %s/%s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName(), err)
	}

	return nil
}

// deleteManifestFS deletes every object in a set of embedded manifests, applying
// the same mutate (so objects the mutate drops or transforms are handled the
// same way as on apply). Cluster-shared infrastructure (the unified Namespace,
// CRDs) is never deleted; see isClusterSharedInfra.
func (r *SiteReconciler) deleteManifestFS(ctx context.Context, manifests fs.FS, mutate func(*unstructured.Unstructured) error) error {
	files, err := yamlFiles(manifests)
	if err != nil {
		return err
	}

	for _, file := range files {
		data, err := fs.ReadFile(manifests, file)
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", file, err)
		}

		if err := r.deleteManifestData(ctx, data, mutate); err != nil {
			return fmt.Errorf("delete manifest %s: %w", file, err)
		}
	}

	return nil
}

func (r *SiteReconciler) deleteManifestData(ctx context.Context, data []byte, mutate func(*unstructured.Unstructured) error) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			return fmt.Errorf("decode resource: %w", err)
		}

		if obj.Object == nil {
			continue
		}

		if mutate != nil {
			if err := mutate(obj); err != nil {
				return err
			}
		}

		if obj.Object == nil {
			continue
		}

		if isClusterSharedInfra(obj) {
			continue
		}

		r.retargetNamespace(obj)

		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
			return fmt.Errorf("delete %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
	}

	return nil
}

// isClusterSharedInfra reports whether an object is cluster-shared infrastructure
// that must survive a component teardown: the unified Namespace (deleting it
// would remove the operator and every other component) and CRDs (shared, and
// deleting one cascades to all of its custom resources, including Sites).
func isClusterSharedInfra(obj *unstructured.Unstructured) bool {
	switch obj.GetKind() {
	case "Namespace", "CustomResourceDefinition":
		return true
	default:
		return false
	}
}

// retargetNamespace rewrites embedded-manifest namespace references from the
// build-time default (buildDefaultNamespace) to the operator's configured
// namespace, so components follow a custom install namespace. It is a no-op in
// the default install where the two namespaces are identical.
func (r *SiteReconciler) retargetNamespace(obj *unstructured.Unstructured) {
	ns := r.namespace()
	if ns == "" || ns == buildDefaultNamespace {
		return
	}

	if obj.GetKind() == "Namespace" {
		if obj.GetName() == buildDefaultNamespace {
			obj.SetName(ns)
		}

		return
	}

	if obj.GetNamespace() == buildDefaultNamespace {
		obj.SetNamespace(ns)
	}

	rewriteStringValues(obj.Object, buildDefaultNamespace, ns)
}

// rewriteStringValues recursively replaces every string value equal to oldValue
// with newValue, so embedded cross-references (RBAC subjects, webhook client
// configs, config-map references) follow a retargeted namespace.
func rewriteStringValues(value any, oldValue, newValue string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, v := range typed {
			if str, ok := v.(string); ok && str == oldValue {
				typed[key] = newValue
				continue
			}

			rewriteStringValues(v, oldValue, newValue)
		}
	case []any:
		for i, v := range typed {
			if str, ok := v.(string); ok && str == oldValue {
				typed[i] = newValue
				continue
			}

			rewriteStringValues(v, oldValue, newValue)
		}
	}
}

func (r *SiteReconciler) mutateMachinaObject(obj *unstructured.Unstructured) error {
	// Metalman RBAC ships in the machina manifests but is applied per-site by
	// reconcileMetalman; skip it here so the singleton apply does not create it.
	if isMetalmanSupportObject(obj) {
		obj.Object = nil
		return nil
	}

	if obj.GetKind() == "ConfigMap" && obj.GetName() == "machina-config" && r.Config.APIServerEndpoint != "" {
		current, _, err := unstructured.NestedString(obj.Object, "data", "config.yaml")
		if err != nil {
			return fmt.Errorf("get machina config data: %w", err)
		}

		updated := strings.ReplaceAll(current, `apiServerEndpoint: ""`, fmt.Sprintf(`apiServerEndpoint: %q`, r.Config.APIServerEndpoint))
		if err := unstructured.SetNestedField(obj.Object, updated, "data", "config.yaml"); err != nil {
			return fmt.Errorf("set machina config data: %w", err)
		}
	}

	return nil
}

// mutateMetalmanSupportObject keeps only the metalman RBAC objects from the
// machina manifests (they are already rendered into unbounded-system).
func mutateMetalmanSupportObject(obj *unstructured.Unstructured) error {
	if filepath.Base(obj.GetName()) == ".gitignore" {
		return nil
	}

	if !isMetalmanSupportObject(obj) {
		obj.Object = nil
	}

	return nil
}

// metalmanDeploymentName is the per-site metalman Deployment name.
func metalmanDeploymentName(site string) string { return "metalman-controller-" + site }

// storageDaemonSetName is the per-site storage supervisor DaemonSet name.
func storageDaemonSetName(site string) string { return "unbounded-storage-supervisor-" + site }

// storageConfigName is the per-site storage supervisor ConfigMap name. Storage
// config is per-Site in the API, so each Site gets its own ConfigMap rather than
// sharing a single one.
func storageConfigName(site string) string { return "unbounded-storage-config-" + site }

// mutateStorageObject scopes the storage supervisor manifests to the Site: the
// DaemonSet is per-site (name, labels, nodeSelector, and config mount) and the
// ConfigMap is per-site (name + Site-provided config). The SA and RBAC are
// shared across sites.
func mutateStorageObject(site *unboundedv1alpha3.Site, obj *unstructured.Unstructured) error {
	switch {
	case obj.GetKind() == "DaemonSet" && obj.GetName() == "unbounded-storage-supervisor":
		return scopeStorageDaemonSetToSite(site, obj)
	case obj.GetKind() == "ConfigMap" && obj.GetName() == "unbounded-storage-config":
		obj.SetName(storageConfigName(site.Name))

		if site.Spec.Components.Storage != nil && site.Spec.Components.Storage.Config != "" {
			obj.Object["data"] = map[string]any{"config.yaml": site.Spec.Components.Storage.Config}
		}

		return nil
	default:
		return nil
	}
}

func scopeStorageDaemonSetToSite(site *unboundedv1alpha3.Site, obj *unstructured.Unstructured) error {
	obj.SetName(storageDaemonSetName(site.Name))

	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}

	labels[siteLabelKey] = site.Name
	obj.SetLabels(labels)

	for _, path := range [][]string{
		{"spec", "selector", "matchLabels", siteLabelKey},
		{"spec", "template", "metadata", "labels", siteLabelKey},
		{"spec", "template", "spec", "nodeSelector", siteLabelKey},
	} {
		if err := unstructured.SetNestedField(obj.Object, site.Name, path...); err != nil {
			return fmt.Errorf("scope storage daemonset (%v): %w", path, err)
		}
	}

	return pointDaemonSetAtSiteConfig(site, obj)
}

// pointDaemonSetAtSiteConfig repoints the DaemonSet's config-source volume at
// the Site's per-site ConfigMap so each Site mounts its own storage config.
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

		if cm["name"] == "unbounded-storage-config" {
			cm["name"] = storageConfigName(site.Name)
			vol["configMap"] = cm
			volumes[i] = vol
		}
	}

	if err := unstructured.SetNestedSlice(obj.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		return fmt.Errorf("set storage daemonset volumes: %w", err)
	}

	return nil
}

func componentEnabled(site *unboundedv1alpha3.Site, component string) bool {
	switch component {
	case ComponentMachina:
		return unboundedv1alpha3.ComponentEnabled(site.Spec.Components.Machina)
	case ComponentMetalman:
		if site.Spec.Components.Metalman == nil {
			return false
		}

		return unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.Metalman.SiteComponentSpec)
	case ComponentStorage:
		if site.Spec.Components.Storage == nil {
			return false
		}

		return unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.Storage.SiteComponentSpec)
	default:
		return false
	}
}

func isMetalmanSupportObject(obj *unstructured.Unstructured) bool {
	switch obj.GetKind() {
	case "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding":
		return strings.Contains(obj.GetName(), "metalman")
	default:
		return false
	}
}

func metalmanDeployment(site *unboundedv1alpha3.Site, namespace string, cfg Config) *appsv1.Deployment {
	image := cfg.MetalmanImage
	name := metalmanDeploymentName(site.Name)
	labels := map[string]string{
		"app":                                 "unbounded-pxe",
		"app.kubernetes.io/name":              "metalman-controller",
		"app.kubernetes.io/component":         "metalman",
		unboundedv1alpha3.MachineSiteLabelKey: site.Name,
	}

	args := []string{"serve-pxe", "--site=" + site.Name}
	if site.Spec.Components.Metalman.DHCPAutoInterface != nil && *site.Spec.Components.Metalman.DHCPAutoInterface {
		args = append(args, "--dhcp-auto-interface")
	}

	// metalman is hostNetwork and binds host ports (DHCP/TFTP/HTTP), so a surge
	// pod cannot start while the old pod holds them on the same node. Terminate
	// the old pod before creating the new one to avoid a rollout deadlock.
	maxSurge := intstr.FromInt32(0)
	maxUnavailable := intstr.FromInt32(1)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptrInt32(1),
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       &maxSurge,
					MaxUnavailable: &maxUnavailable,
				},
			},
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					HostNetwork:        true,
					ServiceAccountName: "metalman-controller",
					// Node selection uses the canonical site-membership label
					// (unbounded-cloud.io/site) the net controller applies to
					// Nodes. Storage scopes its DaemonSet the same way.
					NodeSelector: map[string]string{
						siteLabelKey: site.Name,
					},
					Containers: []corev1.Container{{
						Name:            "metalman",
						Image:           image,
						ImagePullPolicy: corev1.PullAlways,
						Args:            args,
						Env: []corev1.EnvVar{{
							// Metalman resolves its leader-election lease
							// namespace from POD_NAMESPACE so the lease and its
							// namespace-scoped RBAC stay co-located with the
							// Deployment under any install namespace.
							Name: "POD_NAMESPACE",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
							},
						}},
						Ports: []corev1.ContainerPort{
							{Name: "http", ContainerPort: 8880, Protocol: corev1.ProtocolTCP},
							{Name: "health", ContainerPort: 8081, Protocol: corev1.ProtocolTCP},
							{Name: "dhcp", ContainerPort: 67, Protocol: corev1.ProtocolUDP},
							{Name: "tftp", ContainerPort: 69, Protocol: corev1.ProtocolUDP},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "tmp", MountPath: "/tmp"},
							{Name: "cache", MountPath: "/var/cache/metalman"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

func yamlFiles(fsys fs.FS) ([]string, error) {
	var files []string

	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".yaml" || ext == ".yml" {
			files = append(files, path)
		}

		return nil
	}); err != nil {
		return nil, err
	}

	sort.Strings(files)

	return files, nil
}

func toUnstructured(obj client.Object) *unstructured.Unstructured {
	if unstr, ok := obj.(*unstructured.Unstructured); ok {
		return unstr
	}

	objectMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		panic(err)
	}

	return &unstructured.Unstructured{Object: objectMap}
}

func ptrInt32(value int32) *int32 {
	return &value
}
