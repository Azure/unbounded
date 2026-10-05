// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/membership"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type TopologyReconciler struct {
	authority *authority.Authority
	settings  frozenConfig
	client.Client
	APIReader client.Reader
	Config    Config
}

func (r *TopologyReconciler) runtimeConfig() Config { return r.settings.get(&r.Config) }

// Reconcile builds from the synchronized cache, reads the version ConfigMap
// authoritatively, commits counters/hashes with CAS, then installs the result.
// Conflicts requeue from fresh inputs; missing established counters fail closed.
func (r *TopologyReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	if err := r.runtimeConfig().Validate(); err != nil {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	err := r.reconcile(ctx)
	// Never schedule retries from a canceled leadership operation, even if a
	// transport returned Conflict concurrently with cancellation.
	if ctx.Err() != nil {
		return ctrl.Result{}, reconcile.TerminalError(ctx.Err())
	}

	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: retryConflictDelay}, nil
	}

	return ctrl.Result{}, err
}

func (r *TopologyReconciler) reconcile(ctx context.Context) error {
	update, err := r.publish(ctx)
	if err != nil {
		return err
	}

	return r.annotate(ctx, update)
}

// publish protects authoritative reads, CAS, and local installation. Annotation
// writes are recovery hints, not authority, and must not block trust observation.
func (r *TopologyReconciler) publish(ctx context.Context) (authority.TopologyHints, error) {
	return r.authority.PublishTopology(ctx, r.observeTopology)
}

// TopologyObservation contains discovery inputs, not accepted history or proofs.
type TopologyObservation = authority.TopologyObservation

func (r *TopologyReconciler) observeTopology(ctx context.Context) (TopologyObservation, error) {
	cfg := r.runtimeConfig()

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return TopologyObservation{}, err
	}

	var caches racerv1.ClusterCacheList
	if err := r.APIReader.List(ctx, &caches); err != nil {
		return TopologyObservation{}, err
	}

	catalog, err := membership.BuildCatalog(caches.Items)
	if err != nil {
		return TopologyObservation{}, err
	}

	ownership, err := readManagedWorkloadIdentities(ctx, r.APIReader, cfg)
	if err != nil {
		return TopologyObservation{}, err
	}
	// Indexed namespace-scoped queries avoid scanning unrelated Pods for each
	// Node. Ownership is still verified against the current DaemonSet UID.
	podsByNode := make(map[string][]corev1.Pod, len(nodes.Items))

	for _, node := range nodes.Items {
		if err := ctx.Err(); err != nil {
			return TopologyObservation{}, err
		}

		var list corev1.PodList
		if err := r.List(ctx, &list, client.InNamespace(cfg.Namespace), client.MatchingFields{podNodeIndex: node.Name}); err != nil {
			return TopologyObservation{}, err
		}

		podsByNode[node.Name] = list.Items
	}

	return TopologyObservation{Nodes: nodes, Catalog: catalog, Input: membership.Input{
		Nodes: nodes.Items, PodsByNode: podsByNode, Ownership: ownership.observed(), PeerPort: cfg.PeerPort,
	}}, nil
}

func (r *TopologyReconciler) annotate(ctx context.Context, update authority.TopologyHints) error {
	for i := range update.Nodes.Items {
		node := &update.Nodes.Items[i]

		member, ok := update.Members[wire.NodeID(node.UID)]
		if !ok {
			if _, excluded := node.Labels[wire.ExclusionLabel]; excluded && node.Annotations[admittedMemberAnnotation] != "" {
				before := node.DeepCopy()
				delete(node.Annotations, admittedMemberAnnotation)

				if err := r.Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
					return err
				}
			}

			continue
		}

		encoded, err := json.Marshal(member)
		if err != nil {
			return err
		}

		if len(encoded) > 64*1024 {
			return wire.TooLarge
		}

		if node.Annotations[admittedMemberAnnotation] == string(encoded) {
			continue
		}

		before := node.DeepCopy()
		if node.Annotations == nil {
			node.Annotations = map[string]string{}
		}

		node.Annotations[admittedMemberAnnotation] = string(encoded)
		if err := r.Patch(ctx, node, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}

	return nil
}

func (r *TopologyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cfg := r.runtimeConfig()

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, podNodeIndex, podNodeKeys); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-topology").
		WatchesRawSource(initialEnqueue()).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(nodeChanges())).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(managedPodChanges(cfg))).
		Watches(&appsv1.DaemonSet{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(cfg.Namespace, managedWorkloadNames(cfg)...))).
		Watches(&racerv1.ClusterCache{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(cacheChanges())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(cfg.Namespace, cfg.CredentialsSecretName))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(versionChanges(cfg))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// singleton coalesces input changes without introducing a singleton CR.
func singleton(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "racer"}}}
}

// DataplaneWorkloadIdentities is a bounded snapshot of live workload identities.
// Refresh it for each authorization or topology pass; labels are not ownership.
type DataplaneWorkloadIdentities struct {
	namespace string
	workloads [2]workloadIdentity
}

type workloadIdentity struct {
	name string
	uid  types.UID
}

func (ids DataplaneWorkloadIdentities) observed() membership.WorkloadIdentities {
	observed := membership.WorkloadIdentities{Namespace: ids.namespace}
	for i, workload := range ids.workloads {
		observed.Workloads[i] = membership.WorkloadIdentity{Name: workload.name, UID: workload.uid}
	}

	return observed
}

// Custom standalone installations retain their single configured workload.
// Operator installations use both fixed names, never a label-derived allowlist.
func readManagedWorkloadIdentities(ctx context.Context, reader client.Reader, cfg Config) (DataplaneWorkloadIdentities, error) {
	ids := DataplaneWorkloadIdentities{namespace: cfg.Namespace}
	for i, name := range managedWorkloadNames(cfg) {
		ids.workloads[i].name = name

		var ds appsv1.DaemonSet
		if err := reader.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: name}, &ds); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return DataplaneWorkloadIdentities{}, err
		}

		if ds.DeletionTimestamp == nil {
			ids.workloads[i].uid = ds.UID
		}
	}

	return ids, nil
}

const (
	enrolledSharesAnnotation   = membership.EnrolledSharesAnnotation
	enrolledRDMANICsAnnotation = membership.EnrolledRDMANICsAnnotation
	admittedMemberAnnotation   = membership.AdmittedMemberAnnotation
)
