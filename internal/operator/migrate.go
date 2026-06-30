// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"context"
	"fmt"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
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

	defaultReapInterval = 30 * time.Second
)

// LegacyNamespaces are the pre-consolidation namespaces components used before
// everything moved to the unified namespace.
var LegacyNamespaces = []string{"unbounded-kube", "unbounded-net"}

// workloadRef identifies a target workload whose readiness gates reaping of the
// corresponding legacy resources.
type workloadRef struct {
	kind string // "Deployment" or "DaemonSet"
	name string
}

// legacyComponent describes a single component's legacy footprint: where it
// used to run, the target workloads that must be healthy before its legacy
// resources may be removed, and the app.kubernetes.io/name label values that
// identify the operator-owned resources to delete.
type legacyComponent struct {
	name            string
	legacyNamespace string
	targets         []workloadRef
	appNames        []string
}

// legacyComponents is processed in order; net is intentionally last because the
// data-plane cutover (old net-node removed, new one already Ready) is briefly
// disruptive and should happen only after everything else has moved.
func legacyComponentsFor(target string) []legacyComponent {
	return []legacyComponent{
		{
			name:            ComponentMachina,
			legacyNamespace: "unbounded-kube",
			targets:         []workloadRef{{kind: "Deployment", name: "machina-controller"}},
			appNames:        []string{"machina-controller"},
		},
		{
			name:            ComponentMetalman,
			legacyNamespace: "unbounded-kube",
			// Per-site Deployments are recreated by the Site reconcile; gate on
			// the shared metalman ServiceAccount/RBAC being present in target.
			appNames: []string{"metalman-controller"},
		},
		{
			name:            ComponentUnboundedStorage,
			legacyNamespace: "unbounded-kube",
			targets:         []workloadRef{{kind: "DaemonSet", name: "unbounded-storage-supervisor"}},
			appNames:        []string{"unbounded-storage-supervisor"},
		},
		{
			name:            ComponentNet,
			legacyNamespace: "unbounded-net",
			targets: []workloadRef{
				{kind: "Deployment", name: "unbounded-net-controller"},
				{kind: "DaemonSet", name: "unbounded-net-node"},
			},
			appNames: []string{"unbounded-net-controller", "unbounded-net-node", "unbounded-net-kube-proxy"},
		},
	}
}

// secretNamespaceRewrite describes a cluster-scoped custom resource whose secret
// reference embeds a namespace that must follow the consolidation.
type secretNamespaceRewrite struct {
	gvk            schema.GroupVersionKind
	namespacePath  []string // path to the namespace string, relative to the object root
	requirePresent []string // intermediate path that must exist for the rewrite to apply
}

func clusterScopedSecretRewrites() []secretNamespaceRewrite {
	group := unboundedv1alpha3.GroupVersion.Group
	version := unboundedv1alpha3.GroupVersion.Version

	return []secretNamespaceRewrite{
		{
			gvk:            schema.GroupVersionKind{Group: group, Version: version, Kind: "MachineList"},
			namespacePath:  []string{"spec", "pxe", "redfish", "passwordRef", "namespace"},
			requirePresent: []string{"spec", "pxe", "redfish", "passwordRef"},
		},
		{
			gvk:            schema.GroupVersionKind{Group: group, Version: version, Kind: "MachineOperationCredentialList"},
			namespacePath:  []string{"spec", "auth", "secretRef", "namespace"},
			requirePresent: []string{"spec", "auth", "secretRef"},
		},
	}
}

// LegacyReaper migrates operator-owned state out of the legacy split namespaces
// into the unified namespace and then deletes the operator-owned resources left
// behind. It never deletes the legacy Namespace objects themselves: an operator
// removes those manually once they are confirmed empty.
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
}

// NeedLeaderElection ensures the reaper only runs on the elected leader.
func (*LegacyReaper) NeedLeaderElection() bool { return true }

// Start runs the migrate-then-reap loop until everything is drained or the
// context is cancelled. It is safe to run repeatedly: every step is idempotent.
func (r *LegacyReaper) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("legacy-reaper")

	if r.Interval <= 0 {
		r.Interval = defaultReapInterval
	}

	if len(r.LegacyNamespaces) == 0 {
		r.LegacyNamespaces = LegacyNamespaces
	}

	for {
		done, err := r.reapOnce(ctx, logger)
		if err != nil {
			logger.Error(err, "legacy reap iteration failed; will retry")
		} else if done {
			logger.Info("legacy reap complete; legacy namespaces may be deleted manually once empty",
				"namespaces", r.legacyNamespacesPresent(ctx))

			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.Interval):
		}
	}
}

// reapOnce performs one idempotent pass. It returns done=true when no legacy
// operator-owned resources remain in any legacy namespace.
func (r *LegacyReaper) reapOnce(ctx context.Context, logger logr.Logger) (bool, error) {
	target := r.TargetNamespace
	if target == "" {
		target = DefaultNamespace
	}

	// 1 & 2: copy non-regenerable state (secrets + named configmaps) into the
	// target namespace, and rewrite cluster-scoped CR secret references.
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

	// 4 & 5: per component, gate on the target workloads being healthy, then
	// delete the operator-owned resources in the legacy namespace.
	allReaped := true

	for _, component := range legacyComponentsFor(target) {
		if !r.namespaceExists(ctx, component.legacyNamespace) {
			continue
		}

		ready, err := r.targetsReady(ctx, target, component.targets)
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

	return allReaped, nil
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
func (r *LegacyReaper) migrateConfigMaps(ctx context.Context, logger logr.Logger, legacyNs, target string) error {
	for _, name := range r.CopyConfigMaps {
		var src corev1.ConfigMap
		if err := r.Get(ctx, client.ObjectKey{Namespace: legacyNs, Name: name}, &src); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return err
		}

		dst := &corev1.ConfigMap{
			ObjectMeta: copyObjectMeta(src.ObjectMeta, target),
			Data:       src.Data,
			BinaryData: src.BinaryData,
		}

		if err := r.createIfAbsent(ctx, dst); err != nil {
			return fmt.Errorf("copy configmap %s/%s: %w", legacyNs, name, err)
		}

		logger.V(1).Info("ensured configmap copied", "name", name, "from", legacyNs, "to", target)
	}

	return nil
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

// reapComponent deletes the operator-owned resources for a component in its
// legacy namespace. It returns remaining=true if anything matching the
// component's app labels still exists afterwards.
func (r *LegacyReaper) reapComponent(ctx context.Context, logger logr.Logger, component legacyComponent) (bool, error) {
	for _, appName := range component.appNames {
		selector := client.MatchingLabels{appNameLabel: appName}
		inNs := client.InNamespace(component.legacyNamespace)

		for _, obj := range reapableKinds() {
			if err := r.DeleteAllOf(ctx, obj, inNs, selector); err != nil && !apierrors.IsNotFound(err) {
				return true, fmt.Errorf("delete %T in %s for %s: %w", obj, component.legacyNamespace, appName, err)
			}
		}

		logger.V(1).Info("reaped legacy component resources",
			"component", component.name, "app", appName, "namespace", component.legacyNamespace)
	}

	return r.componentResourcesRemain(ctx, component)
}

// componentResourcesRemain reports whether any operator-owned workloads for the
// component still exist in its legacy namespace.
func (r *LegacyReaper) componentResourcesRemain(ctx context.Context, component legacyComponent) (bool, error) {
	for _, appName := range component.appNames {
		var deployments appsv1.DeploymentList
		if err := r.List(ctx, &deployments, client.InNamespace(component.legacyNamespace), client.MatchingLabels{appNameLabel: appName}); err != nil {
			return true, err
		}

		if len(deployments.Items) > 0 {
			return true, nil
		}

		var daemonsets appsv1.DaemonSetList
		if err := r.List(ctx, &daemonsets, client.InNamespace(component.legacyNamespace), client.MatchingLabels{appNameLabel: appName}); err != nil {
			return true, err
		}

		if len(daemonsets.Items) > 0 {
			return true, nil
		}
	}

	return false, nil
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

func (r *LegacyReaper) namespaceExists(ctx context.Context, name string) bool {
	var ns corev1.Namespace

	return r.Get(ctx, client.ObjectKey{Name: name}, &ns) == nil
}

func (r *LegacyReaper) legacyNamespacesPresent(ctx context.Context) []string {
	var present []string

	for _, ns := range r.LegacyNamespaces {
		if r.namespaceExists(ctx, ns) {
			present = append(present, ns)
		}
	}

	return present
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

// SetupWithManager registers the reaper as a leader-elected manager runnable.
func (r *LegacyReaper) SetupWithManager(mgr ctrl.Manager) error {
	return mgr.Add(r)
}
