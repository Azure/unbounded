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
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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

	// Condition reasons published on Site.status.conditions. Reasons must be
	// CamelCase alphanumeric to satisfy metav1.Condition validation.
	reasonReconciled     = "Reconciled"
	reasonDisabled       = "Disabled"
	reasonNoSites        = "NoSites"
	reasonReconcileError = "ReconcileError"

	// siteLabelKey is the canonical node label for site membership
	// (unbounded-cloud.io/site). The net controller applies it to Nodes and the
	// operator uses it to node-select site-scoped components (storage, metalman).
	siteLabelKey = unboundedv1alpha3.MachineSiteLabelKey

	// deprecatedSiteLabelKey is the node site-membership label used by released
	// net controllers before the switch to unbounded-cloud.io/site. Per-site
	// workloads target either key during the deprecation window so they schedule
	// before the upgraded net has converged all Nodes to the canonical label.
	deprecatedSiteLabelKey = "net.unbounded-cloud.io/site"

	// singletonRequestName cannot collide with a valid Site name. Managed
	// singleton resource events use it independently of Site fan-out.
	singletonRequestName = "__singletons"
)

// DefaultNamespace is the namespace the operator installs components into. It
// follows the operator's own namespace (unbounded.SystemNamespace()).
var DefaultNamespace = unbounded.SystemNamespace()

// buildDefaultNamespace is the namespace baked into the embedded component
// manifests at build time (the render default). retargetNamespace rewrites it to
// the operator's configured namespace when they differ. It shares the single
// build-default source of truth with the install tooling.
const buildDefaultNamespace = unbounded.DefaultSystemNamespace

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
			// A Site was deleted, or a managed singleton config event used the
			// synthetic request; reconcile the cluster singletons without status.
			return ctrl.Result{}, r.reconcileSingletons(ctx)
		}

		return ctrl.Result{}, err
	}

	results := map[string]componentResult{}

	// Cluster singletons: net is deployed whenever at least one Site exists and
	// kept reconciled if retained; machina is deployed whenever any Site enables
	// it. Reconcile them idempotently on every Site event and report status.
	results[ComponentNet] = r.reconcileNet(ctx)
	results[ComponentMachina] = r.reconcileMachina(ctx)

	// Per-site components: reconcile when enabled, tear down when disabled so
	// spec.components.*.enabled is declarative.
	results[ComponentMetalman] = r.reconcilePerSiteComponent(
		componentEnabled(&site, ComponentMetalman),
		func() error { return r.reconcileMetalman(ctx, &site) },
		func() error { return r.cleanupMetalman(ctx, &site) },
	)
	results[ComponentStorage] = r.reconcilePerSiteComponent(
		componentEnabled(&site, ComponentStorage),
		func() error { return r.reconcileStorage(ctx, &site) },
		func() error { return r.cleanupStorage(ctx, &site) },
	)

	patch := client.MergeFrom(site.DeepCopy())

	var reconcileErrs []error

	// Publish one condition per component in a stable order so the status patch
	// is deterministic and callers can `kubectl wait --for=condition=NetReady`.
	for _, name := range []string{ComponentNet, ComponentMachina, ComponentMetalman, ComponentStorage} {
		res := results[name]
		if res.err != nil {
			reconcileErrs = append(reconcileErrs, fmt.Errorf("%s: %w", name, res.err))
		}

		if !res.ready {
			logger.Info("component not ready", "site", site.Name, "component", name, "message", res.message)
		}

		status := metav1.ConditionTrue
		if !res.ready {
			status = metav1.ConditionFalse
		}

		apimeta.SetStatusCondition(&site.Status.Conditions, metav1.Condition{
			Type:               componentConditionType(name),
			Status:             status,
			Reason:             res.reason,
			Message:            res.message,
			ObservedGeneration: site.Generation,
		})
	}

	if err := r.Status().Patch(ctx, &site, patch); err != nil {
		reconcileErrs = append(reconcileErrs, fmt.Errorf("patch site status: %w", err))
	}

	return ctrl.Result{}, errors.Join(reconcileErrs...)
}

// componentResult is the internal outcome of reconciling a single component,
// translated into a Site status condition by Reconcile.
type componentResult struct {
	ready   bool
	reason  string
	message string
	err     error
}

// componentConditionType maps a component name to its Site status condition
// type (for example "net" -> "NetReady").
func componentConditionType(component string) string {
	switch component {
	case ComponentNet:
		return "NetReady"
	case ComponentMachina:
		return "MachinaReady"
	case ComponentMetalman:
		return "MetalmanReady"
	case ComponentStorage:
		return "StorageReady"
	default:
		return component + "Ready"
	}
}

func (r *SiteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&unboundedv1alpha3.Site{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		// Site status is written by the net controller (nodeCount/sliceCount) on
		// node churn; reconciling on those status-only updates would re-apply the
		// full net + machina manifest sets on every event. Filter to spec/
		// generation changes (Create/Delete still pass) so component reconcile is
		// driven by intent, not status noise.
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForManagedConfig),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc: func(e event.CreateEvent) bool { return r.isManagedConfig(e.Object) },
				DeleteFunc: func(e event.DeleteEvent) bool { return r.isManagedConfig(e.Object) },
				UpdateFunc: func(e event.UpdateEvent) bool {
					if !r.isManagedConfig(e.ObjectNew) {
						return false
					}

					oldConfig, oldOK := e.ObjectOld.(*corev1.ConfigMap)

					newConfig, newOK := e.ObjectNew.(*corev1.ConfigMap)
					if !oldOK || !newOK {
						return false
					}

					return configMapPayloadChanged(oldConfig, newConfig)
				},
				GenericFunc: func(event.GenericEvent) bool { return false },
			}),
		).
		Watches(
			&appsv1.Deployment{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForManagedSingletonWorkload),
			builder.WithPredicates(r.managedSingletonWorkloadPredicates()),
		).
		Watches(
			&appsv1.DaemonSet{},
			handler.EnqueueRequestsFromMapFunc(r.requestsForManagedSingletonWorkload),
			builder.WithPredicates(r.managedSingletonWorkloadPredicates()),
		).
		Complete(r)
}

func (r *SiteReconciler) isManagedConfig(obj client.Object) bool {
	if obj.GetNamespace() != r.namespace() {
		return false
	}

	if obj.GetName() == "machina-config" || obj.GetName() == "unbounded-net-config" {
		return true
	}

	return strings.TrimPrefix(obj.GetName(), "unbounded-storage-config-") != obj.GetName() &&
		strings.TrimPrefix(obj.GetName(), "unbounded-storage-config-") != ""
}

func (r *SiteReconciler) requestsForManagedConfig(ctx context.Context, obj client.Object) []ctrl.Request {
	if siteName := strings.TrimPrefix(obj.GetName(), "unbounded-storage-config-"); siteName != obj.GetName() && siteName != "" {
		return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: siteName}}}
	}

	requests := []ctrl.Request{{NamespacedName: client.ObjectKey{Name: singletonRequestName}}}

	sites, err := r.listSites(ctx)
	if err != nil {
		log.FromContext(ctx).Error(err, "list Sites after managed config change")

		return requests
	}

	for i := range sites {
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKey{Name: sites[i].Name}})
	}

	return requests
}

func (r *SiteReconciler) requestsForManagedSingletonWorkload(context.Context, client.Object) []ctrl.Request {
	return []ctrl.Request{{NamespacedName: client.ObjectKey{Name: singletonRequestName}}}
}

func (r *SiteReconciler) managedSingletonWorkloadPredicates() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return r.isManagedSingletonWorkload(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return r.isManagedSingletonWorkload(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			return r.isManagedSingletonWorkload(e.ObjectNew) &&
				e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func (r *SiteReconciler) isManagedSingletonWorkload(obj client.Object) bool {
	if obj.GetNamespace() != r.namespace() {
		return false
	}

	switch obj.(type) {
	case *appsv1.Deployment:
		return obj.GetName() == "machina-controller" || obj.GetName() == "unbounded-net-controller"
	case *appsv1.DaemonSet:
		return obj.GetName() == "unbounded-net-node"
	default:
		return false
	}
}

// reconcilePerSiteComponent reconciles a per-site component when enabled and
// tears it down when disabled, turning the outcome into a componentResult.
func (r *SiteReconciler) reconcilePerSiteComponent(enabled bool, reconcile, cleanup func() error) componentResult {
	if !enabled {
		if err := cleanup(); err != nil {
			return componentResult{ready: false, reason: reasonReconcileError, message: err.Error(), err: err}
		}

		return componentResult{ready: true, reason: reasonDisabled, message: "component disabled"}
	}

	if err := reconcile(); err != nil {
		return componentResult{ready: false, reason: reasonReconcileError, message: err.Error(), err: err}
	}

	return componentResult{ready: true, reason: reasonReconciled, message: "component reconciled"}
}

// reconcileSingletons reconciles just the cluster-singleton components (net,
// machina); used for deleted Sites and synthetic singleton requests.
func (r *SiteReconciler) reconcileSingletons(ctx context.Context) error {
	if s := r.reconcileNet(ctx); !s.ready {
		return fmt.Errorf("net: %s", s.message)
	}

	if s := r.reconcileMachina(ctx); !s.ready {
		return fmt.Errorf("machina: %s", s.message)
	}

	return nil
}

// reconcileNet deploys the unbounded-net cluster singleton whenever at least
// one Site exists and keeps an existing installation reconciled with no Sites.
// Net is not a per-Site component: one controller/node pair reads every Site.
func (r *SiteReconciler) reconcileNet(ctx context.Context) componentResult {
	sites, err := r.listSites(ctx)
	if err != nil {
		return componentResult{ready: false, reason: reasonReconcileError, message: err.Error(), err: err}
	}

	if len(sites) == 0 {
		// Net is the cluster dataplane. Do not auto-delete it just because the
		// last Site was removed; deleting net-node can break pod networking across
		// the whole cluster. A future explicit uninstall flow should handle removal.
		retained, err := r.netResourcesExist(ctx)
		if err != nil {
			return componentResult{ready: false, reason: reasonReconcileError, message: err.Error(), err: err}
		}

		if !retained {
			return componentResult{ready: true, reason: reasonNoSites, message: "no sites; net retained"}
		}
	}

	configHash, err := r.ensureNetConfig(ctx)
	if err != nil {
		return componentResult{ready: false, reason: reasonReconcileError, message: err.Error(), err: err}
	}

	if err := r.applyManifestFS(ctx, netmanifests.Manifests, r.netApplyMutator(configHash)); err != nil {
		return componentResult{ready: false, reason: reasonReconcileError, message: err.Error(), err: err}
	}

	return componentResult{ready: true, reason: reasonReconciled, message: "component reconciled"}
}

func (r *SiteReconciler) netResourcesExist(ctx context.Context) (bool, error) {
	key := func(name string) client.ObjectKey {
		return client.ObjectKey{Namespace: r.namespace(), Name: name}
	}

	for _, object := range []client.Object{
		&corev1.ConfigMap{},
		&appsv1.Deployment{},
		&appsv1.DaemonSet{},
	} {
		name := "unbounded-net-config"

		switch object.(type) {
		case *appsv1.Deployment:
			name = "unbounded-net-controller"
		case *appsv1.DaemonSet:
			name = "unbounded-net-node"
		}

		if err := r.Get(ctx, key(name), object); err == nil {
			return true, nil
		} else if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get retained net resource %s/%s: %w", r.namespace(), name, err)
		}
	}

	return false, nil
}

// netApplyMutator skips the separately reconciled ConfigMap and stamps its
// exact payload hash on both net workloads so config changes roll them together.
func (r *SiteReconciler) netApplyMutator(configHash string) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		if obj.GetKind() == crdKind {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() == "ConfigMap" && obj.GetName() == "unbounded-net-config" {
			obj.Object = nil

			return nil
		}

		if (obj.GetKind() == "Deployment" && obj.GetName() == "unbounded-net-controller") ||
			(obj.GetKind() == "DaemonSet" && obj.GetName() == "unbounded-net-node") {
			annotations, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
			if err != nil {
				return fmt.Errorf("get net pod template annotations: %w", err)
			}

			if annotations == nil {
				annotations = map[string]string{}
			}

			annotations[netConfigHashAnnotation] = configHash
			if err := unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
				return fmt.Errorf("set net config hash annotation: %w", err)
			}
		}

		return nil
	}
}

// ensureNetConfig creates the embedded default only when no config exists.
// Existing migrated or user-managed payloads are never applied over.
func (r *SiteReconciler) ensureNetConfig(ctx context.Context) (string, error) {
	key := client.ObjectKey{Namespace: r.namespace(), Name: "unbounded-net-config"}
	existing := &corev1.ConfigMap{}

	err := r.Get(ctx, key, existing)
	if err == nil {
		return configMapPayloadHash(existing), nil
	}

	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("get net config %s/%s: %w", key.Namespace, key.Name, err)
	}

	desired, err := defaultNetConfigMap(r.namespace())
	if err != nil {
		return "", err
	}

	if err := r.Create(ctx, desired); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create net config %s/%s: %w", key.Namespace, key.Name, err)
		}

		if err := r.Get(ctx, key, existing); err != nil {
			return "", fmt.Errorf("get raced net config %s/%s: %w", key.Namespace, key.Name, err)
		}

		return configMapPayloadHash(existing), nil
	}

	return configMapPayloadHash(desired), nil
}

func defaultNetConfigMap(namespace string) (*corev1.ConfigMap, error) {
	return defaultConfigMap(netmanifests.Manifests, namespace, "unbounded-net-config", "net")
}

// reconcileMachina deploys the machina controller singleton whenever any Site
// enables it and keeps an existing installation reconciled when none do.
// (Machina is a singleton for now; it may become site-scoped.)
func (r *SiteReconciler) reconcileMachina(ctx context.Context) componentResult {
	sites, err := r.listSites(ctx)
	if err != nil {
		return componentResult{ready: false, reason: reasonReconcileError, message: err.Error(), err: err}
	}

	enabled := false

	for i := range sites {
		if componentEnabled(&sites[i], ComponentMachina) {
			enabled = true
			break
		}
	}

	if !enabled {
		// Keep machina installed once the operator has taken ownership. Automatic
		// singleton removal is surprising and can strand related controllers/RBAC;
		// a future explicit uninstall flow should handle removal.
		retained, err := r.machinaResourcesExist(ctx)
		if err != nil {
			return componentResult{ready: false, reason: reasonReconcileError, message: err.Error(), err: err}
		}

		if !retained {
			return componentResult{ready: true, reason: reasonDisabled, message: "no site enables machina; retained"}
		}
	}

	configHash, err := r.ensureMachinaConfig(ctx)
	if err != nil {
		return componentResult{ready: false, reason: reasonReconcileError, message: err.Error(), err: err}
	}

	if err := r.applyManifestFS(ctx, machinamanifests.Manifests, r.machinaApplyMutator(configHash)); err != nil {
		return componentResult{ready: false, reason: reasonReconcileError, message: err.Error(), err: err}
	}

	return componentResult{ready: true, reason: reasonReconciled, message: "component reconciled"}
}

func (r *SiteReconciler) machinaResourcesExist(ctx context.Context) (bool, error) {
	for _, resource := range []struct {
		name   string
		object client.Object
	}{
		{name: "machina-config", object: &corev1.ConfigMap{}},
		{name: "machina-controller", object: &appsv1.Deployment{}},
	} {
		key := client.ObjectKey{Namespace: r.namespace(), Name: resource.name}
		if err := r.Get(ctx, key, resource.object); err == nil {
			return true, nil
		} else if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get retained machina resource %s/%s: %w", key.Namespace, key.Name, err)
		}
	}

	return false, nil
}

// machinaApplyMutator skips the separately reconciled ConfigMap and stamps its
// exact content hash on the Deployment so every config change rolls Machina.
func (r *SiteReconciler) machinaApplyMutator(configHash string) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		if err := r.mutateMachinaObject(obj); err != nil {
			return err
		}

		if obj.Object == nil {
			return nil
		}

		if obj.GetKind() == "ConfigMap" && obj.GetName() == "machina-config" {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() == "Deployment" && obj.GetName() == "machina-controller" {
			if err := unstructured.SetNestedField(obj.Object, configHash,
				"spec", "template", "metadata", "annotations", machinaConfigHashAnnotation); err != nil {
				return fmt.Errorf("set machina config hash annotation: %w", err)
			}
		}

		return nil
	}
}

func (r *SiteReconciler) ensureMachinaConfig(ctx context.Context) (string, error) {
	desired, err := defaultMachinaConfigMap(r.namespace())
	if err != nil {
		return "", err
	}

	key := client.ObjectKeyFromObject(desired)
	existing := &corev1.ConfigMap{}

	err = r.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		config, mergeErr := setMachinaAPIServerEndpoint(desired.Data["config.yaml"], r.Config.APIServerEndpoint)
		if mergeErr != nil {
			return "", mergeErr
		}

		desired.Data["config.yaml"] = config
		if createErr := r.Create(ctx, desired); createErr != nil {
			return "", fmt.Errorf("create machina config %s/%s: %w", key.Namespace, key.Name, createErr)
		}

		return configMapPayloadHash(desired), nil
	}

	if err != nil {
		return "", fmt.Errorf("get machina config %s/%s: %w", key.Namespace, key.Name, err)
	}

	config := existing.Data["config.yaml"]

	merged, err := setMachinaAPIServerEndpoint(config, r.Config.APIServerEndpoint)
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
		if err := r.Patch(ctx, existing, patch); err != nil {
			return "", fmt.Errorf("update machina config %s/%s: %w", key.Namespace, key.Name, err)
		}
	}

	return configMapPayloadHash(existing), nil
}

func defaultMachinaConfigMap(namespace string) (*corev1.ConfigMap, error) {
	return defaultConfigMap(machinamanifests.Manifests, namespace, "machina-config", "machina")
}

func defaultConfigMap(manifests fs.FS, namespace, name, component string) (*corev1.ConfigMap, error) {
	files, err := yamlFiles(manifests)
	if err != nil {
		return nil, err
	}

	for _, file := range files {
		data, err := fs.ReadFile(manifests, file)
		if err != nil {
			return nil, fmt.Errorf("read %s manifest %s: %w", component, file, err)
		}

		decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

		for {
			obj := &unstructured.Unstructured{}
			if err := decoder.Decode(obj); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}

				return nil, fmt.Errorf("decode %s manifest %s: %w", component, file, err)
			}

			if obj.Object == nil || obj.GetKind() != "ConfigMap" || obj.GetName() != name {
				continue
			}

			cm := &corev1.ConfigMap{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, cm); err != nil {
				return nil, fmt.Errorf("convert %s config template: %w", component, err)
			}

			cm.Namespace = namespace

			return cm, nil
		}
	}

	return nil, fmt.Errorf("%s config template not found", component)
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
	configHash, err := r.ensureStorageConfig(ctx, site)
	if err != nil {
		return err
	}

	return r.applyManifestFS(ctx, storagemanifests.Manifests, func(obj *unstructured.Unstructured) error {
		return mutateStorageObject(site, configHash, obj)
	})
}

// ensureStorageConfig creates the per-site storage ConfigMap from the embedded
// default when it is absent. If an operator/user already created it (or the
// reaper migrated it), preserve the data and only adopt it with a Site ownerRef
// so it is garbage-collected with the Site. Do not server-side-apply metadata
// for existing ConfigMaps: if the operator previously owned data.config.yaml,
// an apply that omits data would delete it.
func (r *SiteReconciler) ensureStorageConfig(ctx context.Context, site *unboundedv1alpha3.Site) (string, error) {
	key := client.ObjectKey{Namespace: r.namespace(), Name: storageConfigName(site.Name)}
	existing := &corev1.ConfigMap{}

	err := r.Get(ctx, key, existing)
	switch {
	case err == nil:
		if err := r.adoptStorageConfig(ctx, site, existing); err != nil {
			return "", err
		}

		return configMapPayloadHash(existing), nil
	case !apierrors.IsNotFound(err):
		return "", fmt.Errorf("get storage config %s/%s: %w", key.Namespace, key.Name, err)
	}

	cm, err := defaultStorageConfigMap(site, r.namespace())
	if err != nil {
		return "", err
	}

	if err := r.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("create storage config %s/%s: %w", cm.Namespace, cm.Name, err)
		}

		// Race with a user/reaper create; adopt what won without touching data.
		if getErr := r.Get(ctx, key, existing); getErr != nil {
			return "", fmt.Errorf("get raced storage config %s/%s: %w", key.Namespace, key.Name, getErr)
		}

		if err := r.adoptStorageConfig(ctx, site, existing); err != nil {
			return "", err
		}

		return configMapPayloadHash(existing), nil
	}

	return configMapPayloadHash(cm), nil
}

func (r *SiteReconciler) adoptStorageConfig(ctx context.Context, site *unboundedv1alpha3.Site, cm *corev1.ConfigMap) error {
	owner := siteOwnerReference(site)

	refs, changed := upsertOwnerReference(cm.OwnerReferences, owner)
	if !changed {
		return nil
	}

	before := cm.DeepCopy()
	cm.OwnerReferences = refs

	patch := client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})
	if err := r.Patch(ctx, cm, patch); err != nil {
		return fmt.Errorf("adopt storage config %s/%s: %w", cm.Namespace, cm.Name, err)
	}

	return nil
}

func defaultStorageConfigMap(site *unboundedv1alpha3.Site, namespace string) (*corev1.ConfigMap, error) {
	files, err := yamlFiles(storagemanifests.Manifests)
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

			if obj.Object == nil || obj.GetKind() != "ConfigMap" || obj.GetName() != "unbounded-storage-config" {
				continue
			}

			cm := &corev1.ConfigMap{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, cm); err != nil {
				return nil, fmt.Errorf("convert storage config template: %w", err)
			}

			cm.Name = storageConfigName(site.Name)
			cm.Namespace = namespace
			cm.OwnerReferences = []metav1.OwnerReference{siteOwnerReference(site)}

			return cm, nil
		}
	}

	return nil, errors.New("storage config template not found")
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

	rewriteNamespaceValues(obj.Object, buildDefaultNamespace, ns)
}

// rewriteNamespaceValues recursively retargets every string value that embeds the
// old namespace to the new one. Unlike a naive equality swap it also handles the
// namespace appearing as a substring, which the rendered manifests rely on:
//   - service-account usernames in ValidatingAdmissionPolicy CEL
//     ("system:serviceaccount:<ns>:<sa>"), and
//   - flag values ("--leader-elect-resource-namespace=<ns>").
//
// Runtime re-rendering of the templates is not an option here because the
// component image references are baked into the rendered manifests at build time
// and are not available to the operator process.
func rewriteNamespaceValues(value any, oldNS, newNS string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, v := range typed {
			if str, ok := v.(string); ok {
				typed[key] = retargetNamespaceInString(str, oldNS, newNS)
				continue
			}

			rewriteNamespaceValues(v, oldNS, newNS)
		}
	case []any:
		for i, v := range typed {
			if str, ok := v.(string); ok {
				typed[i] = retargetNamespaceInString(str, oldNS, newNS)
				continue
			}

			rewriteNamespaceValues(v, oldNS, newNS)
		}
	}
}

// retargetNamespaceInString rewrites the old namespace to the new one within a
// single string value, covering exact matches, service-account usernames, and
// flag values, while leaving unrelated strings (for example image references)
// untouched.
func retargetNamespaceInString(s, oldNS, newNS string) string {
	if s == oldNS {
		return newNS
	}

	if strings.Contains(s, "system:serviceaccount:"+oldNS+":") {
		s = strings.ReplaceAll(s, "system:serviceaccount:"+oldNS+":", "system:serviceaccount:"+newNS+":")
	}

	if strings.HasPrefix(s, "--") && strings.HasSuffix(s, "="+oldNS) {
		s = strings.TrimSuffix(s, "="+oldNS) + "=" + newNS
	}

	return s
}

func (r *SiteReconciler) mutateMachinaObject(obj *unstructured.Unstructured) error {
	// CRDs are installed once at operator startup (BootstrapCRDs), not from the
	// reconcile loop; skip them here so a Site event does not re-apply schemas.
	if obj.GetKind() == crdKind {
		obj.Object = nil
		return nil
	}

	// Metalman RBAC ships in the machina manifests but is applied per-site by
	// reconcileMetalman; skip it here so the singleton apply does not create it.
	if isMetalmanSupportObject(obj) {
		obj.Object = nil
		return nil
	}

	return nil
}

// skipCRDObjects nils out CustomResourceDefinition objects so the reconcile
// loop never applies CRDs; the operator installs them once at startup via
// BootstrapCRDs.
func skipCRDObjects(obj *unstructured.Unstructured) error {
	if obj.GetKind() == crdKind {
		obj.Object = nil
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

// siteOwnerReference builds the owner reference used for per-site resources.
// Using ownerRefs rather than a Site finalizer lets Kubernetes garbage collect
// per-site workloads if a Site is deleted while the operator is down.
func siteOwnerReference(site *unboundedv1alpha3.Site) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: unboundedv1alpha3.GroupVersion.String(),
		Kind:       "Site",
		Name:       site.Name,
		UID:        site.UID,
	}
}

func upsertOwnerReference(refs []metav1.OwnerReference, owner metav1.OwnerReference) ([]metav1.OwnerReference, bool) {
	for i := range refs {
		if refs[i].UID == owner.UID && refs[i].Kind == owner.Kind && refs[i].APIVersion == owner.APIVersion {
			if refs[i].Name == owner.Name {
				return refs, false
			}

			out := append([]metav1.OwnerReference(nil), refs...)
			out[i] = owner

			return out, true
		}
	}

	out := append([]metav1.OwnerReference(nil), refs...)
	out = append(out, owner)

	return out, true
}

// siteNodeAffinity matches Nodes carrying either the canonical site label or the
// deprecated net-prefixed site label. The OR is required during migration: old
// net labels Nodes with the deprecated key, while the upgraded net dual-writes
// both. Remove the deprecated term when the deprecated label write is removed.
func siteNodeAffinity(siteName string) *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					siteNodeSelectorTerm(siteLabelKey, siteName),
					siteNodeSelectorTerm(deprecatedSiteLabelKey, siteName),
				},
			},
		},
	}
}

func siteNodeSelectorTerm(key, siteName string) corev1.NodeSelectorTerm {
	return corev1.NodeSelectorTerm{
		MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key:      key,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{siteName},
		}},
	}
}

// mutateStorageObject scopes the storage supervisor manifests to the Site. The
// DaemonSet is per-site (name, labels, node affinity, and config mount). The
// per-site ConfigMap is handled by ensureStorageConfig so existing config data
// is preserved; the SA and RBAC are shared across sites.
func mutateStorageObject(site *unboundedv1alpha3.Site, configHash string, obj *unstructured.Unstructured) error {
	switch {
	case obj.GetKind() == "DaemonSet" && obj.GetName() == "unbounded-storage-supervisor":
		return scopeStorageDaemonSetToSite(site, configHash, obj)
	case obj.GetKind() == "ConfigMap" && obj.GetName() == "unbounded-storage-config":
		obj.Object = nil

		return nil
	default:
		return nil
	}
}

func scopeStorageDaemonSetToSite(site *unboundedv1alpha3.Site, configHash string, obj *unstructured.Unstructured) error {
	obj.SetName(storageDaemonSetName(site.Name))
	obj.SetOwnerReferences([]metav1.OwnerReference{siteOwnerReference(site)})

	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}

	labels[siteLabelKey] = site.Name
	obj.SetLabels(labels)

	for _, path := range [][]string{
		{"spec", "selector", "matchLabels", siteLabelKey},
		{"spec", "template", "metadata", "labels", siteLabelKey},
	} {
		if err := unstructured.SetNestedField(obj.Object, site.Name, path...); err != nil {
			return fmt.Errorf("scope storage daemonset (%v): %w", path, err)
		}
	}

	affinityMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(siteNodeAffinity(site.Name))
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

	annotations[storageConfigHashAnnotation] = configHash
	if err := unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
		return fmt.Errorf("set storage config hash annotation: %w", err)
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
		if site.Spec.Components.Machina == nil {
			return false
		}

		return unboundedv1alpha3.ComponentEnabled(&site.Spec.Components.Machina.SiteComponentSpec)
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

	replicas := int32(1)
	if site.Spec.Components.Metalman.Replicas != nil {
		replicas = *site.Spec.Components.Metalman.Replicas
	}

	env := []corev1.EnvVar{{
		// Metalman resolves its leader-election lease namespace from
		// POD_NAMESPACE so the lease and its namespace-scoped RBAC stay
		// co-located with the Deployment under any install namespace.
		Name: "POD_NAMESPACE",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		},
	}}

	// Advertise the operator-resolved API server endpoint to metalman so the
	// kubeconfig it serves to provisioning nodes matches what machina writes,
	// and so metalman does not need to rediscover it (managed control planes
	// such as AKS do not publish kube-public/cluster-info). Metalman still
	// sources the CA from the in-cluster service-account mount when cluster-info
	// is unavailable.
	if cfg.APIServerEndpoint != "" {
		env = append(env, corev1.EnvVar{Name: "METALMAN_APISERVER_URL", Value: cfg.APIServerEndpoint})
	}

	// metalman is hostNetwork and binds host ports (DHCP/TFTP/HTTP), so a surge
	// pod cannot start while the old pod holds them on the same node. Terminate
	// the old pod before creating the new one to avoid a rollout deadlock.
	maxSurge := intstr.FromInt32(0)
	maxUnavailable := intstr.FromInt32(1)

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{siteOwnerReference(site)},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
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
					// Match either the canonical or deprecated site label during
					// the node-label deprecation window. Storage scopes its
					// DaemonSet the same way.
					Affinity: siteNodeAffinity(site.Name),
					Containers: []corev1.Container{{
						Name:            "metalman",
						Image:           image,
						ImagePullPolicy: corev1.PullAlways,
						Args:            args,
						Env:             env,
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
