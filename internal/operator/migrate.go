// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/unbounded"
)

const (
	// appNameLabel is the well-known label every operator-managed component
	// workload carries; the reaper uses it to scope deletions in the legacy
	// namespaces to operator-owned resources only.
	appNameLabel = "app.kubernetes.io/name"

	// serviceAccountTokenSecretType is an auto-managed Secret type that must
	// never be copied across namespaces; Kubernetes mints a fresh one.
	serviceAccountTokenSecretType = "kubernetes.io/service-account-token" //nolint:gosec // not a credential

	// helmReleaseSecretType is Helm's release bookkeeping; it is tied to the
	// release in its origin namespace and must not be copied.
	helmReleaseSecretType = "helm.sh/release.v1"

	// legacyKubeNamespace is where machina, metalman, and storage ran before
	// the move to unbounded-system. Sourced from internal/unbounded so the CLI
	// install guard and the reaper share a single source of truth.
	legacyKubeNamespace = unbounded.LegacyKubeNamespace

	// legacyNetNamespace is where unbounded-net ran before the move.
	legacyNetNamespace = unbounded.LegacyNetNamespace

	// clusterSiteName is the conventional name of the Site that represents the
	// main control-plane cluster (created by `kubectl unbounded site init`).
	// The machina controller singleton is enabled on this Site.
	clusterSiteName = "cluster"

	defaultReapInterval = 30 * time.Second
)

// LegacyNamespaces are the pre-consolidation namespaces components used before
// everything moved to the unified namespace.
var LegacyNamespaces = []string{legacyKubeNamespace, legacyNetNamespace}

// legacySiteGVK is the pre-redesign cluster-scoped Site type in the net group.
// Existing clusters carry these; the reaper translates them into the machina
// group Site the operator reconciles.
var legacySiteGVK = schema.GroupVersionKind{
	Group:   "net.unbounded-cloud.io",
	Version: "v1alpha1",
	Kind:    "Site",
}

// legacySiteCRDName is the CustomResourceDefinition name for the pre-redesign
// net-group Site; it is deleted once every legacy Site has been translated.
const legacySiteCRDName = "sites.net.unbounded-cloud.io"

// newSiteGVK is the redesigned cluster-scoped Site type in the machina group.
func newSiteGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   unboundedv1alpha3.GroupVersion.Group,
		Version: unboundedv1alpha3.GroupVersion.Version,
		Kind:    "Site",
	}
}

// workloadRef identifies a target workload whose readiness gates reaping of the
// corresponding legacy resources.
type workloadRef struct {
	kind string // "Deployment" or "DaemonSet"
	name string
}

// legacyComponent describes a single component's legacy footprint: where it
// used to run, the target workloads that must be healthy before its legacy
// resources may be removed, and the label selectors that identify the
// operator-owned resources to delete. A resource is reaped if it matches ANY
// selector (each selector is an AND of its labels).
type legacyComponent struct {
	name            string
	legacyNamespace string
	targets         []workloadRef
	selectors       []map[string]string
}

// legacyComponentsFor is processed in order; net is intentionally last because
// the data-plane cutover (old net-node removed, new one already Ready) is
// briefly disruptive and should happen only after everything else has moved.
//
// Selectors match the labels the component manifests actually carry: machina
// uses the bare `app` label, while net, storage, and the operator-created
// metalman Deployment use `app.kubernetes.io/name`.
func legacyComponentsFor(string) []legacyComponent {
	return []legacyComponent{
		{
			name:            ComponentMachina,
			legacyNamespace: legacyKubeNamespace,
			targets:         []workloadRef{{kind: "Deployment", name: "machina-controller"}},
			selectors:       []map[string]string{{"app": "machina-controller"}},
		},
		{
			name:            ComponentMetalman,
			legacyNamespace: legacyKubeNamespace,
			// Per-site Deployments are recreated by the Site reconcile. Match
			// both the operator-created label and the legacy deploy-pxe label.
			selectors: []map[string]string{
				{appNameLabel: "metalman-controller"},
				{"app": "unbounded-pxe"},
			},
		},
		{
			name:            ComponentStorage,
			legacyNamespace: legacyKubeNamespace,
			// Storage readiness is gated dynamically on the per-site
			// unbounded-storage-supervisor-<site> DaemonSets (see
			// storageTargetsReady); there is no single fixed target name.
			selectors: []map[string]string{{appNameLabel: "unbounded-storage-supervisor"}},
		},
		{
			name:            ComponentNet,
			legacyNamespace: legacyNetNamespace,
			// Net is NOT health-gated: the old and new net-node DaemonSets both
			// use hostNetwork and cannot occupy the same node host ports at
			// once, so the new net pods stay Pending until the old net is
			// reaped (see netTargetsPresent). A brief net data-plane gap during
			// cutover is expected and accepted.
			selectors: []map[string]string{
				{appNameLabel: "unbounded-net-controller"},
				{appNameLabel: "unbounded-net-node"},
				{appNameLabel: "unbounded-net-kube-proxy"},
			},
		},
	}
}

// secretNamespaceRewrite describes a cluster-scoped custom resource whose secret
// reference embeds a namespace that must follow the consolidation.
type secretNamespaceRewrite struct {
	gvk           schema.GroupVersionKind
	namespacePath []string // path to the namespace string, relative to the object root
}

func clusterScopedSecretRewrites() []secretNamespaceRewrite {
	group := unboundedv1alpha3.GroupVersion.Group
	version := unboundedv1alpha3.GroupVersion.Version

	return []secretNamespaceRewrite{
		{
			gvk:           schema.GroupVersionKind{Group: group, Version: version, Kind: "MachineList"},
			namespacePath: []string{"spec", "pxe", "redfish", "passwordRef", "namespace"},
		},
		{
			gvk:           schema.GroupVersionKind{Group: group, Version: version, Kind: "MachineOperationCredentialList"},
			namespacePath: []string{"spec", "auth", "secretRef", "namespace"},
		},
	}
}

// LegacyReaper migrates operator-owned state out of the legacy split namespaces
// into the unified namespace, translates the pre-redesign net-group Sites into
// the machina-group Sites the operator reconciles, and then deletes the
// operator-owned resources (and the now-empty legacy namespaces) left behind.
type LegacyReaper struct {
	client.Client

	// TargetNamespace is the unified namespace components are consolidated into.
	TargetNamespace string

	// LegacyNamespaces is the set of pre-consolidation namespaces to drain.
	LegacyNamespaces []string

	// SkipSecretNames are regenerable secrets that must not be copied (e.g. the
	// net controller serving cert, which is reissued on startup).
	SkipSecretNames map[string]struct{}

	// CopyConfigMaps are operator-owned ConfigMaps to carry across by name.
	CopyConfigMaps []string

	// Interval is the requeue period while waiting for target workloads to
	// become healthy.
	Interval time.Duration

	// Recorder, when set, receives Events for notable migration actions (for
	// example a legacy namespace that still holds non-operator workloads at
	// delete time). Optional; nil disables Event emission.
	Recorder record.EventRecorder
}

// NeedLeaderElection ensures the reaper only runs on the elected leader.
func (*LegacyReaper) NeedLeaderElection() bool { return true }

// Start runs the migrate-then-reap loop until everything is drained or the
// context is cancelled (manager shutdown). It is the manager.Runnable entry
// point; context cancellation is a clean stop, not an error.
func (r *LegacyReaper) Start(ctx context.Context) error {
	if err := r.RunToCompletion(ctx); err != nil && ctx.Err() == nil {
		return err
	}

	return nil
}

// RunToCompletion performs idempotent translate-migrate-reap passes until every
// legacy namespace is drained and deleted, the context is cancelled, or an
// unexpected error occurs. It returns nil only once fully reaped.
func (r *LegacyReaper) RunToCompletion(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("legacy-reaper")

	r.applyDefaults()

	for {
		done, err := r.reapOnce(ctx, logger)
		if err != nil {
			logger.Error(err, "legacy reap iteration failed; will retry")
		} else if done {
			logger.Info("legacy migration complete; legacy namespaces removed")

			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.Interval):
		}
	}
}

func (r *LegacyReaper) applyDefaults() {
	if r.Interval <= 0 {
		r.Interval = defaultReapInterval
	}

	if len(r.LegacyNamespaces) == 0 {
		r.LegacyNamespaces = LegacyNamespaces
	}

	// Never treat the migration target as a legacy namespace: draining and
	// deleting it in cleanup() would destroy the namespace we just migrated
	// into. This guards against the operator running with
	// --namespace unbounded-kube (or unbounded-net). A fresh slice is built so
	// the shared package-level LegacyNamespaces var is never mutated.
	target := r.TargetNamespace
	if target == "" {
		target = DefaultNamespace
	}

	kept := make([]string, 0, len(r.LegacyNamespaces))

	for _, ns := range r.LegacyNamespaces {
		if ns == target {
			continue
		}

		kept = append(kept, ns)
	}

	r.LegacyNamespaces = kept
}

// reapOnce performs one idempotent pass. It returns done=true when every legacy
// namespace (and the legacy Site CRD) has been removed.
func (r *LegacyReaper) reapOnce(ctx context.Context, logger logr.Logger) (bool, error) {
	target := r.TargetNamespace
	if target == "" {
		target = DefaultNamespace
	}

	// 0: translate pre-redesign net-group Sites into machina-group Sites so the
	// operator has Sites to reconcile before anything else moves.
	if err := r.translateSites(ctx, logger); err != nil {
		return false, fmt.Errorf("translate legacy sites: %w", err)
	}

	// 1 & 2: copy non-regenerable state (secrets + named configmaps) into the
	// target namespace.
	for _, legacyNs := range r.LegacyNamespaces {
		if !r.namespaceExists(ctx, legacyNs) {
			continue
		}

		if err := r.migrateSecrets(ctx, logger, legacyNs, target); err != nil {
			return false, fmt.Errorf("migrate secrets from %s: %w", legacyNs, err)
		}

		if err := r.migrateConfigMaps(ctx, logger, legacyNs, target); err != nil {
			return false, fmt.Errorf("migrate configmaps from %s: %w", legacyNs, err)
		}
	}

	// 3: rewrite cluster-scoped CR secret references that name a legacy ns.
	if err := r.rewriteClusterScopedRefs(ctx, logger, target); err != nil {
		return false, fmt.Errorf("rewrite cluster-scoped secret references: %w", err)
	}

	// 3b: copy Machine cloud-init ConfigMaps out of the legacy namespaces and
	// repoint their refs, so they survive the legacy namespace deletion.
	if err := r.migrateMachineCloudInitConfigMaps(ctx, logger, target); err != nil {
		return false, fmt.Errorf("migrate machine cloud-init configmaps: %w", err)
	}

	// 3c: migrate legacy storage config into the per-site ConfigMaps the
	// operator now uses as the storage config source of truth.
	if err := r.migrateStorageConfigMaps(ctx, logger, target); err != nil {
		return false, fmt.Errorf("migrate storage configmaps: %w", err)
	}

	// 4 & 5: per component, gate on the target workloads being healthy, then
	// delete the operator-owned resources in the legacy namespace.
	allReaped := true

	for _, component := range legacyComponentsFor(target) {
		if !r.namespaceExists(ctx, component.legacyNamespace) {
			continue
		}

		// Skip components with no legacy footprint: there is nothing to reap and
		// we must not block on a target workload that will never exist (e.g. a
		// cluster that never installed storage).
		footprint, err := r.componentResourcesRemain(ctx, component)
		if err != nil {
			return false, err
		}

		if !footprint {
			continue
		}

		ready, err := r.componentReady(ctx, target, component)
		if err != nil {
			return false, err
		}

		if !ready {
			logger.Info("waiting for target workloads before reaping component",
				"component", component.name, "namespace", target)

			allReaped = false

			continue
		}

		remaining, err := r.reapComponent(ctx, logger, component)
		if err != nil {
			return false, fmt.Errorf("reap component %s: %w", component.name, err)
		}

		if remaining {
			allReaped = false
		}
	}

	if !allReaped {
		return false, nil
	}

	// 6: everything is migrated and reaped; remove the legacy Site CRD and the
	// now-drained legacy namespaces.
	return r.cleanup(ctx, logger, target)
}

// translateSites lists the pre-redesign net-group Sites and creates an
// equivalent machina-group Site for each one, preserving the networking spec
// verbatim and setting spec.components from the detected running workloads.
func (r *LegacyReaper) translateSites(ctx context.Context, logger logr.Logger) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   legacySiteGVK.Group,
		Version: legacySiteGVK.Version,
		Kind:    legacySiteGVK.Kind + "List",
	})

	if err := r.List(ctx, list); err != nil {
		// The legacy Site CRD may already be gone (the cleanup step deletes it):
		// a reloaded RESTMapper surfaces that as a no-match error, while a stale
		// mapper issues a request that 404s. Either means nothing to translate.
		if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	for i := range list.Items {
		if err := r.translateSite(ctx, logger, &list.Items[i]); err != nil {
			return err
		}
	}

	return nil
}

func (r *LegacyReaper) translateSite(ctx context.Context, logger logr.Logger, src *unstructured.Unstructured) error {
	name := src.GetName()

	// Do not clobber a Site that already exists in the machina group (already
	// translated on a previous pass, or created directly against the new API).
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(newSiteGVK())

	err := r.Get(ctx, client.ObjectKey{Name: name}, existing)
	if err == nil {
		return nil
	}

	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get machina site %s: %w", name, err)
	}

	spec, _, err := unstructured.NestedMap(src.Object, "spec")
	if err != nil {
		return fmt.Errorf("read legacy site %s spec: %w", name, err)
	}

	if spec == nil {
		spec = map[string]any{}
	}

	// The networking spec is identical between the two Site types; only the
	// operator-managed components block is new.
	delete(spec, "components")

	components, err := r.detectComponents(ctx, name)
	if err != nil {
		return fmt.Errorf("detect components for site %s: %w", name, err)
	}

	spec["components"] = components

	dst := &unstructured.Unstructured{}
	dst.SetGroupVersionKind(newSiteGVK())
	dst.SetName(name)

	if labels := src.GetLabels(); len(labels) > 0 {
		dst.SetLabels(labels)
	}

	dst.Object["spec"] = spec

	if err := r.createIfAbsent(ctx, dst); err != nil {
		return fmt.Errorf("create machina site %s: %w", name, err)
	}

	logger.Info("translated legacy net Site into machina Site", "site", name, "components", components)

	return nil
}

// detectComponents infers spec.components for a translated Site from the
// workloads still running in the legacy namespaces. Storage is enabled on every
// Site that has a legacy storage DaemonSet (each Site then gets its own
// node-selected DaemonSet); machina is enabled only on the cluster Site; and
// metalman is enabled where a per-site metalman Deployment is detected.
func (r *LegacyReaper) detectComponents(ctx context.Context, siteName string) (map[string]any, error) {
	components := map[string]any{}

	storage, err := r.legacyWorkloadExists(ctx, "DaemonSet", map[string]string{appNameLabel: "unbounded-storage-supervisor"})
	if err != nil {
		return nil, err
	}

	if storage {
		components["storage"] = map[string]any{"enabled": true}
	}

	if siteName == clusterSiteName {
		machina, err := r.legacyWorkloadExists(ctx, "Deployment", map[string]string{"app": "machina-controller"})
		if err != nil {
			return nil, err
		}

		if machina {
			components["machina"] = map[string]any{"enabled": true}
		}
	}

	metalman, err := r.legacyMetalmanExistsForSite(ctx, siteName)
	if err != nil {
		return nil, err
	}

	if metalman {
		components["metalman"] = map[string]any{"enabled": true}
	}

	return components, nil
}

// legacyMetalmanExistsForSite reports whether a legacy per-site metalman
// Deployment for siteName exists in any legacy namespace. It matches the
// released deploy-pxe Deployment robustly: any `app: unbounded-pxe` Deployment
// whose name is metalman-controller-<site> OR whose canonical/deprecated site
// label equals siteName. Matching the name (which both the released installer
// and the operator use) guards against the site label being carried under the
// deprecated key on older clusters.
func (r *LegacyReaper) legacyMetalmanExistsForSite(ctx context.Context, siteName string) (bool, error) {
	for _, legacyNs := range r.LegacyNamespaces {
		var list appsv1.DeploymentList
		if err := r.List(ctx, &list, client.InNamespace(legacyNs), client.MatchingLabels{"app": "unbounded-pxe"}); err != nil {
			return false, err
		}

		for i := range list.Items {
			d := &list.Items[i]
			if d.Name == metalmanDeploymentName(siteName) {
				return true, nil
			}

			if d.Labels[unboundedv1alpha3.MachineSiteLabelKey] == siteName || d.Labels[deprecatedSiteLabelKey] == siteName {
				return true, nil
			}
		}
	}

	return false, nil
}

// legacyWorkloadExists reports whether a Deployment or DaemonSet matching the
// given labels exists in any legacy namespace.
func (r *LegacyReaper) legacyWorkloadExists(ctx context.Context, kind string, matchLabels map[string]string) (bool, error) {
	for _, legacyNs := range r.LegacyNamespaces {
		opts := []client.ListOption{client.InNamespace(legacyNs), client.MatchingLabels(matchLabels)}

		switch kind {
		case "Deployment":
			var list appsv1.DeploymentList
			if err := r.List(ctx, &list, opts...); err != nil {
				return false, err
			}

			if len(list.Items) > 0 {
				return true, nil
			}
		case "DaemonSet":
			var list appsv1.DaemonSetList
			if err := r.List(ctx, &list, opts...); err != nil {
				return false, err
			}

			if len(list.Items) > 0 {
				return true, nil
			}
		}
	}

	return false, nil
}

// migrateSecrets copies every non-auto-managed, non-skipped Secret from the
// legacy namespace into the target namespace, creating it only if absent.
func (r *LegacyReaper) migrateSecrets(ctx context.Context, logger logr.Logger, legacyNs, target string) error {
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(legacyNs)); err != nil {
		return err
	}

	for i := range secrets.Items {
		src := &secrets.Items[i]
		if isAutoManagedSecret(src.Type) {
			continue
		}

		if _, skip := r.SkipSecretNames[src.Name]; skip {
			continue
		}

		dst := &corev1.Secret{
			ObjectMeta: copyObjectMeta(src.ObjectMeta, target),
			Type:       src.Type,
			Data:       src.Data,
		}

		if err := r.createIfAbsent(ctx, dst); err != nil {
			return fmt.Errorf("copy secret %s/%s: %w", legacyNs, src.Name, err)
		}

		logger.V(1).Info("ensured secret copied", "name", src.Name, "from", legacyNs, "to", target)
	}

	return nil
}

// migrateConfigMaps copies the configured operator-owned ConfigMaps by name.
// Unlike secrets, these are upserted (their data overwrites the target) so that
// config migrated from the legacy namespace wins over any default the component
// reconciler may have already created in the target. Once the legacy namespace
// is drained the source is gone and this becomes a no-op, so the migrated data
// is preserved thereafter.
func (r *LegacyReaper) migrateConfigMaps(ctx context.Context, logger logr.Logger, legacyNs, target string) error {
	for _, name := range r.CopyConfigMaps {
		var src corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKey{Namespace: legacyNs, Name: name}, &src); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return err
		}

		if err := r.upsertConfigMap(ctx, copyObjectMeta(src.ObjectMeta, target), src.Data, src.BinaryData); err != nil {
			return fmt.Errorf("copy configmap %s/%s: %w", legacyNs, name, err)
		}

		logger.V(1).Info("ensured configmap copied", "name", name, "from", legacyNs, "to", target)
	}

	return nil
}

// upsertConfigMap creates the target ConfigMap, or updates its data if it
// already exists.
func (r *LegacyReaper) upsertConfigMap(ctx context.Context, meta metav1.ObjectMeta, data map[string]string, binaryData map[string][]byte) error {
	existing := &corev1.ConfigMap{}

	err := r.Get(ctx, client.ObjectKey{Namespace: meta.Namespace, Name: meta.Name}, existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.Create(ctx, &corev1.ConfigMap{ObjectMeta: meta, Data: data, BinaryData: binaryData})
	case err != nil:
		return err
	default:
		existing.Data = data
		existing.BinaryData = binaryData

		return r.Update(ctx, existing)
	}
}

// rewriteClusterScopedRefs repoints secret-reference namespaces embedded in
// cluster-scoped custom resources from a legacy namespace to the target.
func (r *LegacyReaper) rewriteClusterScopedRefs(ctx context.Context, logger logr.Logger, target string) error {
	legacy := map[string]struct{}{}
	for _, ns := range r.LegacyNamespaces {
		legacy[ns] = struct{}{}
	}

	for _, rewrite := range clusterScopedSecretRewrites() {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(rewrite.gvk)

		if err := r.List(ctx, list); err != nil {
			if apimeta.IsNoMatchError(err) {
				continue // CRD not installed; nothing to rewrite.
			}

			return err
		}

		for i := range list.Items {
			obj := &list.Items[i]

			current, found, err := unstructured.NestedString(obj.Object, rewrite.namespacePath...)
			if err != nil || !found {
				continue
			}

			if _, isLegacy := legacy[current]; !isLegacy {
				continue
			}

			if err := unstructured.SetNestedField(obj.Object, target, rewrite.namespacePath...); err != nil {
				return err
			}

			if err := r.Update(ctx, obj); err != nil {
				return fmt.Errorf("rewrite %s/%s secret ref namespace: %w", obj.GetKind(), obj.GetName(), err)
			}

			logger.Info("rewrote cluster-scoped secret reference namespace",
				"kind", obj.GetKind(), "name", obj.GetName(), "from", current, "to", target)
		}
	}

	return nil
}

// machineCloudInitConfigMapPath is the path to the Machine cloud-init user-data
// ConfigMap reference (Machine.spec.pxe.cloudInit.userDataConfigMapRef).
var machineCloudInitConfigMapPath = []string{"spec", "pxe", "cloudInit", "userDataConfigMapRef"}

// migrateMachineCloudInitConfigMaps copies the ConfigMap each Machine references
// for cloud-init user-data out of a legacy namespace into the target namespace
// and repoints the reference. Unlike secret refs (whose secrets are copied
// wholesale by migrateSecrets), these ConfigMaps have arbitrary names and are
// not covered by migrateConfigMaps, so they would otherwise be lost when the
// legacy namespace is deleted.
func (r *LegacyReaper) migrateMachineCloudInitConfigMaps(ctx context.Context, logger logr.Logger, target string) error {
	legacy := map[string]struct{}{}
	for _, ns := range r.LegacyNamespaces {
		legacy[ns] = struct{}{}
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   unboundedv1alpha3.GroupVersion.Group,
		Version: unboundedv1alpha3.GroupVersion.Version,
		Kind:    "MachineList",
	})

	if err := r.List(ctx, list); err != nil {
		if apimeta.IsNoMatchError(err) {
			return nil // Machine CRD not installed; nothing to migrate.
		}

		return err
	}

	for i := range list.Items {
		obj := &list.Items[i]

		ref, found, err := unstructured.NestedMap(obj.Object, machineCloudInitConfigMapPath...)
		if err != nil || !found {
			continue
		}

		name, ok := ref["name"].(string)
		if !ok || name == "" {
			continue
		}

		namespace, ok := ref["namespace"].(string)
		if !ok {
			continue
		}

		if _, isLegacy := legacy[namespace]; !isLegacy {
			continue
		}

		if err := r.copyConfigMapByName(ctx, namespace, name, target); err != nil {
			return err
		}

		ref["namespace"] = target
		if err := unstructured.SetNestedMap(obj.Object, ref, machineCloudInitConfigMapPath...); err != nil {
			return err
		}

		if err := r.Update(ctx, obj); err != nil {
			return fmt.Errorf("rewrite machine %s cloud-init configmap ref: %w", obj.GetName(), err)
		}

		logger.Info("migrated machine cloud-init configmap",
			"machine", obj.GetName(), "configmap", name, "from", namespace, "to", target)
	}

	return nil
}

// copyConfigMapByName copies a single ConfigMap from a source namespace into the
// target namespace, creating it only if absent. A missing source is treated as
// success (it may already have been copied and the source namespace drained).
func (r *LegacyReaper) copyConfigMapByName(ctx context.Context, srcNs, name, target string) error {
	var src corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: srcNs, Name: name}, &src); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	dst := &corev1.ConfigMap{
		ObjectMeta: copyObjectMeta(src.ObjectMeta, target),
		Data:       src.Data,
		BinaryData: src.BinaryData,
	}

	if err := r.createIfAbsent(ctx, dst); err != nil {
		return fmt.Errorf("copy configmap %s/%s: %w", srcNs, name, err)
	}

	return nil
}

// migrateStorageConfigMaps copies the legacy shared storage config into the new
// per-site storage ConfigMaps the operator uses as the config source of truth.
// It upserts so migrated config wins any race with the operator creating a
// default per-site ConfigMap before the reaper sees the legacy source.
func (r *LegacyReaper) migrateStorageConfigMaps(ctx context.Context, logger logr.Logger, target string) error {
	legacy, found, err := r.legacyStorageConfigMap(ctx)
	if err != nil || !found {
		return err
	}

	var sites unboundedv1alpha3.SiteList
	if err := r.List(ctx, &sites); err != nil {
		return fmt.Errorf("list sites: %w", err)
	}

	for i := range sites.Items {
		site := &sites.Items[i]
		if !componentEnabled(site, ComponentStorage) {
			continue
		}

		meta := copyObjectMeta(legacy.ObjectMeta, target)
		meta.Name = storageConfigName(site.Name)

		if err := r.upsertConfigMap(ctx, meta, legacy.Data, legacy.BinaryData); err != nil {
			return fmt.Errorf("copy storage configmap for Site %s: %w", site.Name, err)
		}

		logger.V(1).Info("ensured per-site storage config migrated", "site", site.Name, "name", meta.Name, "to", target)
	}

	return nil
}

func (r *LegacyReaper) legacyStorageConfigMap(ctx context.Context) (*corev1.ConfigMap, bool, error) {
	for _, legacyNs := range r.LegacyNamespaces {
		var cm corev1.ConfigMap

		err := r.Get(ctx, client.ObjectKey{Namespace: legacyNs, Name: "unbounded-storage-config"}, &cm)
		if apierrors.IsNotFound(err) {
			continue
		}

		if err != nil {
			return nil, false, err
		}

		return &cm, true, nil
	}

	return nil, false, nil
}

// reapComponent deletes the operator-owned resources for a component in its
// legacy namespace. It returns remaining=true if anything matching the
// component's selectors still exists afterwards.
func (r *LegacyReaper) reapComponent(ctx context.Context, logger logr.Logger, component legacyComponent) (bool, error) {
	for _, selector := range component.selectors {
		inNs := client.InNamespace(component.legacyNamespace)
		match := client.MatchingLabels(selector)

		for _, obj := range reapableKinds() {
			if err := r.DeleteAllOf(ctx, obj, inNs, match); err != nil && !apierrors.IsNotFound(err) {
				return true, fmt.Errorf("delete %T in %s for %v: %w", obj, component.legacyNamespace, selector, err)
			}
		}

		logger.V(1).Info("reaped legacy component resources",
			"component", component.name, "selector", selector, "namespace", component.legacyNamespace)
	}

	return r.componentResourcesRemain(ctx, component)
}

// componentResourcesRemain reports whether any operator-owned workloads for the
// component still exist in its legacy namespace.
func (r *LegacyReaper) componentResourcesRemain(ctx context.Context, component legacyComponent) (bool, error) {
	for _, selector := range component.selectors {
		opts := []client.ListOption{client.InNamespace(component.legacyNamespace), client.MatchingLabels(selector)}

		var deployments appsv1.DeploymentList
		if err := r.List(ctx, &deployments, opts...); err != nil {
			return true, err
		}

		if len(deployments.Items) > 0 {
			return true, nil
		}

		var daemonsets appsv1.DaemonSetList
		if err := r.List(ctx, &daemonsets, opts...); err != nil {
			return true, err
		}

		if len(daemonsets.Items) > 0 {
			return true, nil
		}
	}

	return false, nil
}

// componentReady reports whether the new workloads that must be healthy before
// a component's legacy resources may be reaped are Ready. Storage is gated on
// the per-site unbounded-storage-supervisor-<site> DaemonSets the operator
// creates; net is gated only on the new net workloads being created (not Ready)
// because old and new net cannot coexist on the same node host ports; every
// other component uses its static gating workloads.
func (r *LegacyReaper) componentReady(ctx context.Context, target string, component legacyComponent) (bool, error) {
	switch component.name {
	case ComponentStorage:
		return r.storageTargetsReady(ctx, target)
	case ComponentNet:
		return r.netTargetsPresent(ctx, target)
	case ComponentMetalman:
		return r.metalmanTargetsPresent(ctx, target)
	default:
		return r.targetsReady(ctx, target, component.targets)
	}
}

// metalmanTargetsPresent reports whether the operator has created the per-site
// metalman Deployment for every metalman-enabled Site. Like net, metalman is
// hostNetwork and binds host ports (DHCP/TFTP/HTTP), so the new per-site pod
// cannot become Ready while the legacy metalman on the same node still holds
// those ports; gating on creation (not readiness) frees the ports without
// deadlocking. If a legacy metalman footprint reached this gate but no Site
// enables metalman (for example a detection miss), reaping is refused so the
// workload is not silently dropped.
func (r *LegacyReaper) metalmanTargetsPresent(ctx context.Context, target string) (bool, error) {
	var sites unboundedv1alpha3.SiteList
	if err := r.List(ctx, &sites); err != nil {
		return false, fmt.Errorf("list sites: %w", err)
	}

	enabled := 0

	for i := range sites.Items {
		site := &sites.Items[i]
		if !componentEnabled(site, ComponentMetalman) {
			continue
		}

		enabled++

		var deploy appsv1.Deployment
		if err := r.Get(ctx, client.ObjectKey{Namespace: target, Name: metalmanDeploymentName(site.Name)}, &deploy); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}

			return false, err
		}
	}

	return enabled > 0, nil
}

// netTargetsPresent reports whether the operator has created the new net
// controller and node in the target namespace. Net readiness is deliberately
// NOT required: the old and new net-node DaemonSets both use hostNetwork and
// contend for the same node host ports, so the new net pods remain Pending
// until the old net is reaped. Gating on creation (rather than readiness) lets
// the reaper remove the old net once the operator has taken over, freeing the
// ports so the new net can schedule and become Ready.
func (r *LegacyReaper) netTargetsPresent(ctx context.Context, target string) (bool, error) {
	var deploy appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: target, Name: "unbounded-net-controller"}, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, err
	}

	var ds appsv1.DaemonSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: target, Name: "unbounded-net-node"}, &ds); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, err
	}

	return true, nil
}

// storageTargetsReady reports whether legacy storage may be reaped. It is a
// conjunction of two invariants:
//
//   - Every per-site unbounded-storage-supervisor-<site> DaemonSet that exists
//     in the target namespace must be Ready, and at least one must exist (do not
//     reap before the operator has created a replacement).
//   - Every storage-enabled translated Site must have its per-site DaemonSet
//     present, so a multi-site cluster never loses the legacy supervisor before
//     every storage-enabled Site has its own replacement.
func (r *LegacyReaper) storageTargetsReady(ctx context.Context, target string) (bool, error) {
	var list appsv1.DaemonSetList
	if err := r.List(ctx, &list, client.InNamespace(target)); err != nil {
		return false, err
	}

	found := false

	for i := range list.Items {
		ds := &list.Items[i]
		if !strings.HasPrefix(ds.Name, "unbounded-storage-supervisor-") {
			continue
		}

		found = true

		if !storageDaemonSetReady(ds) {
			return false, nil
		}
	}

	// Require every storage-enabled Site to have its per-site DaemonSet present
	// before reaping the legacy supervisor.
	var sites unboundedv1alpha3.SiteList
	if err := r.List(ctx, &sites); err != nil {
		return false, fmt.Errorf("list sites: %w", err)
	}

	for i := range sites.Items {
		site := &sites.Items[i]
		if !componentEnabled(site, ComponentStorage) {
			continue
		}

		var ds appsv1.DaemonSet
		if err := r.Get(ctx, client.ObjectKey{Namespace: target, Name: storageDaemonSetName(site.Name)}, &ds); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}

			return false, err
		}
	}

	return found, nil
}

// targetsReady reports whether every gating workload is healthy in the target
// namespace. Components without gating workloads are always considered ready.
func (r *LegacyReaper) targetsReady(ctx context.Context, target string, refs []workloadRef) (bool, error) {
	for _, ref := range refs {
		switch ref.kind {
		case "Deployment":
			var deploy appsv1.Deployment
			if err := r.Get(ctx, client.ObjectKey{Namespace: target, Name: ref.name}, &deploy); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}

				return false, err
			}

			if !deploymentAvailable(&deploy) {
				return false, nil
			}
		case "DaemonSet":
			var ds appsv1.DaemonSet
			if err := r.Get(ctx, client.ObjectKey{Namespace: target, Name: ref.name}, &ds); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}

				return false, err
			}

			if !daemonSetReady(&ds) {
				return false, nil
			}
		}
	}

	return true, nil
}

// cleanup removes the legacy Site CRD and the drained legacy namespaces. It
// returns done=true only once the CRD and every legacy namespace are gone
// (namespace deletion is asynchronous, so a later pass observes completion).
func (r *LegacyReaper) cleanup(ctx context.Context, logger logr.Logger, target string) (bool, error) {
	done := true

	crdGone, err := r.cleanupLegacySiteCRD(ctx, logger, target)
	if err != nil {
		return false, err
	}

	if !crdGone {
		done = false
	}

	for _, nsName := range r.LegacyNamespaces {
		if !r.namespaceExists(ctx, nsName) {
			continue
		}

		r.warnOnForeignWorkloads(ctx, logger, nsName)

		if err := r.deleteNamespace(ctx, nsName); err != nil {
			return false, fmt.Errorf("delete legacy namespace %s: %w", nsName, err)
		}

		logger.Info("deleting drained legacy namespace", "namespace", nsName)

		done = false
	}

	return done, nil
}

// cleanupLegacySiteCRD deletes the legacy net-group Site CRD once it is safe to
// do so. When the CRD still exists it is only deleted after the new net
// controller is Available: the new controller re-owns the SiteNodeSlices under
// the v1alpha3 Site GVK and recreates them, so deleting the legacy CRD (and its
// Sites) beforehand would let the garbage collector transiently remove slices
// still owned by the legacy Sites, widening the cutover gap. When the CRD is
// already gone there is nothing to protect and it reports gone=true.
func (r *LegacyReaper) cleanupLegacySiteCRD(ctx context.Context, logger logr.Logger, target string) (bool, error) {
	exists, err := r.legacySiteCRDExists(ctx)
	if err != nil {
		return false, err
	}

	if !exists {
		return true, nil
	}

	netReady, err := r.netControllerAvailable(ctx, target)
	if err != nil {
		return false, err
	}

	if !netReady {
		logger.Info("waiting for the new net controller to be Available before deleting the legacy Site CRD", "namespace", target)

		return false, nil
	}

	return r.deleteLegacySiteCRD(ctx, logger)
}

// legacySiteCRDExists reports whether the pre-redesign net-group Site CRD is
// still installed.
func (r *LegacyReaper) legacySiteCRDExists(ctx context.Context) (bool, error) {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	})

	err := r.Get(ctx, client.ObjectKey{Name: legacySiteCRDName}, crd)
	switch {
	case err == nil:
		return true, nil
	case apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err):
		return false, nil
	default:
		return false, fmt.Errorf("get legacy site crd: %w", err)
	}
}

// netControllerAvailable reports whether the new net controller Deployment in the
// target namespace is Available.
func (r *LegacyReaper) netControllerAvailable(ctx context.Context, target string) (bool, error) {
	var deploy appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKey{Namespace: target, Name: "unbounded-net-controller"}, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, err
	}

	return deploymentAvailable(&deploy), nil
}

// warnOnForeignWorkloads logs a warning and emits an Event when a legacy
// namespace still holds workloads the reaper did not create, so operators are
// alerted before the whole-namespace delete removes them too. Deletion still
// proceeds (the namespace is the migration target); the warning surfaces
// unexpected residents rather than blocking the migration.
func (r *LegacyReaper) warnOnForeignWorkloads(ctx context.Context, logger logr.Logger, nsName string) {
	foreign, err := r.foreignWorkloads(ctx, nsName)
	if err != nil {
		logger.Error(err, "failed to check for foreign workloads before deleting legacy namespace", "namespace", nsName)
		return
	}

	if len(foreign) == 0 {
		return
	}

	logger.Info("legacy namespace still holds non-operator workloads; they will be deleted with the namespace",
		"namespace", nsName, "workloads", foreign)

	if r.Recorder != nil {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName}}
		r.Recorder.Eventf(ns, corev1.EventTypeWarning, "ForeignWorkloadsDeleted",
			"deleting drained legacy namespace %s which still holds non-operator workloads: %v", nsName, foreign)
	}
}

// foreignWorkloads returns the "Kind/name" of workload controllers in the
// namespace that the reaper did not create. cleanup runs only after every
// operator-owned Deployment/DaemonSet has been reaped, so any remaining
// Deployment/DaemonSet/StatefulSet here is foreign.
func (r *LegacyReaper) foreignWorkloads(ctx context.Context, nsName string) ([]string, error) {
	var names []string

	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(nsName)); err != nil {
		return nil, err
	}

	for i := range deployments.Items {
		names = append(names, "Deployment/"+deployments.Items[i].Name)
	}

	var daemonsets appsv1.DaemonSetList
	if err := r.List(ctx, &daemonsets, client.InNamespace(nsName)); err != nil {
		return nil, err
	}

	for i := range daemonsets.Items {
		names = append(names, "DaemonSet/"+daemonsets.Items[i].Name)
	}

	var statefulsets appsv1.StatefulSetList
	if err := r.List(ctx, &statefulsets, client.InNamespace(nsName)); err != nil {
		return nil, err
	}

	for i := range statefulsets.Items {
		names = append(names, "StatefulSet/"+statefulsets.Items[i].Name)
	}

	return names, nil
}

// deleteLegacySiteCRD deletes the pre-redesign net-group Site CRD. It reports
// gone=true once the CRD no longer exists.
func (r *LegacyReaper) deleteLegacySiteCRD(ctx context.Context, logger logr.Logger) (bool, error) {
	// The legacy net-group Sites carry the net controller's protection
	// finalizer (added once nodes are assigned). The old net controller is
	// already reaped, so nothing remains to remove it; clearing it here lets the
	// CRD's cascade delete complete instead of deadlocking on a stuck Site.
	if err := r.clearLegacyNetSiteFinalizers(ctx, logger); err != nil {
		return false, err
	}

	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	})
	crd.SetName(legacySiteCRDName)

	err := r.Delete(ctx, crd)
	switch {
	case err == nil:
		logger.Info("deleting legacy net Site CRD", "name", legacySiteCRDName)

		return false, nil
	case apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err):
		return true, nil
	default:
		return false, fmt.Errorf("delete legacy site crd: %w", err)
	}
}

// clearLegacyNetSiteFinalizers strips finalizers from any remaining legacy
// net-group Sites so they (and their CRD) can be deleted. By the time this runs
// the Sites have been translated into machina-group Sites and the old net
// controller that owned their protection finalizer is gone, so the finalizers
// would otherwise block deletion forever.
func (r *LegacyReaper) clearLegacyNetSiteFinalizers(ctx context.Context, logger logr.Logger) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   legacySiteGVK.Group,
		Version: legacySiteGVK.Version,
		Kind:    legacySiteGVK.Kind + "List",
	})

	if err := r.List(ctx, list); err != nil {
		if apimeta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return nil
		}

		return err
	}

	for i := range list.Items {
		site := &list.Items[i]
		if len(site.GetFinalizers()) == 0 {
			continue
		}

		site.SetFinalizers(nil)

		if err := r.Update(ctx, site); err != nil {
			if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
				continue
			}

			return fmt.Errorf("clear finalizers on legacy net Site %s: %w", site.GetName(), err)
		}

		logger.Info("cleared finalizers on legacy net Site to allow CRD deletion", "site", site.GetName())
	}

	return nil
}

func (r *LegacyReaper) deleteNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := r.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return nil
}

func (r *LegacyReaper) namespaceExists(ctx context.Context, name string) bool {
	var ns corev1.Namespace

	return r.Get(ctx, client.ObjectKey{Name: name}, &ns) == nil
}

func (r *LegacyReaper) createIfAbsent(ctx context.Context, obj client.Object) error {
	err := r.Create(ctx, obj)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}

	return err
}

// isAutoManagedSecret reports whether a Secret type is auto-managed and must
// never be copied across namespaces.
func isAutoManagedSecret(secretType corev1.SecretType) bool {
	switch string(secretType) {
	case serviceAccountTokenSecretType, helmReleaseSecretType:
		return true
	default:
		return false
	}
}

// reapableKinds returns fresh empty objects for the resource kinds the reaper
// deletes by label in a legacy namespace.
func reapableKinds() []client.Object {
	return []client.Object{
		&appsv1.Deployment{},
		&appsv1.DaemonSet{},
		&corev1.Service{},
		&corev1.ConfigMap{},
		&corev1.Secret{},
		&corev1.ServiceAccount{},
		&rbacv1.Role{},
		&rbacv1.RoleBinding{},
	}
}

// copyObjectMeta produces clean metadata for re-creating a resource in another
// namespace: it keeps name, labels, and annotations (minus the last-applied
// annotation) and drops all server-managed fields.
func copyObjectMeta(src metav1.ObjectMeta, namespace string) metav1.ObjectMeta {
	annotations := map[string]string{}

	for k, v := range src.Annotations {
		if k == "kubectl.kubernetes.io/last-applied-configuration" {
			continue
		}

		annotations[k] = v
	}

	if len(annotations) == 0 {
		annotations = nil
	}

	return metav1.ObjectMeta{
		Name:        src.Name,
		Namespace:   namespace,
		Labels:      src.Labels,
		Annotations: annotations,
	}
}

func deploymentAvailable(deploy *appsv1.Deployment) bool {
	desired := int32(1)
	if deploy.Spec.Replicas != nil {
		desired = *deploy.Spec.Replicas
	}

	return deploy.Status.ObservedGeneration >= deploy.Generation && deploy.Status.AvailableReplicas >= desired
}

func daemonSetReady(ds *appsv1.DaemonSet) bool {
	if ds.Status.ObservedGeneration < ds.Generation {
		return false
	}

	if ds.Status.DesiredNumberScheduled == 0 {
		return true
	}

	return ds.Status.NumberReady >= ds.Status.DesiredNumberScheduled
}

// storageDaemonSetReady is stricter than daemonSetReady: a per-site storage
// DaemonSet must actually schedule at least one pod (DesiredNumberScheduled >= 1)
// and have all of them Ready before the legacy supervisor is reaped. This
// prevents dropping storage while the replacement has zero pods (for example
// before the node site labels have converged), where a DesiredNumberScheduled of
// 0 would otherwise read as "ready".
func storageDaemonSetReady(ds *appsv1.DaemonSet) bool {
	if ds.Status.ObservedGeneration < ds.Generation {
		return false
	}

	if ds.Status.DesiredNumberScheduled < 1 {
		return false
	}

	return ds.Status.NumberReady >= ds.Status.DesiredNumberScheduled
}

// SetupWithManager registers the reaper as a leader-elected manager runnable.
func (r *LegacyReaper) SetupWithManager(mgr ctrl.Manager) error {
	return mgr.Add(r)
}
