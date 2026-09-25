// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"

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
)

type TopologyReconciler struct {
	client.Client
	APIReader    client.Reader
	Config       Config
	Publications *Publications
	Accepted     AcceptedMembers
}

// Reconcile builds from the synchronized cache, reads the version ConfigMap
// authoritatively, commits counters/hashes with CAS, then installs the result.
// Conflicts requeue from fresh inputs; missing established counters fail closed.
func (r *TopologyReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if err := ctx.Err(); err != nil {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	if err := r.Config.Validate(); err != nil {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	err := r.reconcile(ctx)
	// Never schedule retries from a canceled leadership operation, even if a
	// transport returned Conflict concurrently with cancellation.
	if ctx.Err() != nil {
		return ctrl.Result{}, reconcile.TerminalError(ctx.Err())
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: retryConflictDelay}, nil
	}

	return ctrl.Result{}, err
}

func (r *TopologyReconciler) reconcile(ctx context.Context) error {
	cm, previous, err := r.readVersion(ctx)
	if err != nil {
		r.Publications.Suspend()
		return err
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return err
	}

	var caches racerv1.ClusterCacheList
	if err := r.List(ctx, &caches); err != nil {
		return err
	}

	catalog, err := BuildCatalog(caches.Items)
	if err != nil {
		return err
	}

	var ds appsv1.DaemonSet
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Config.Namespace, Name: r.Config.DaemonSetName}, &ds); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	// Indexed namespace-scoped queries avoid scanning unrelated Pods for each
	// Node. Ownership is still verified against the current DaemonSet UID.
	var pods []corev1.Pod

	for _, node := range nodes.Items {
		if err := ctx.Err(); err != nil {
			return err
		}

		var list corev1.PodList
		if err := r.List(ctx, &list, client.InNamespace(r.Config.Namespace), client.MatchingFields{podNodeIndex: node.Name}); err != nil {
			return err
		}

		pods = append(pods, list.Items...)
	}

	candidate, diagnostics, err := ReconcileMembers(nodes.Items, pods, ds.UID, r.Accepted, r.Config.PeerPort)
	if err != nil {
		return err
	}

	for _, d := range diagnostics {
		ctrl.LoggerFrom(ctx).Info("membership input rejected", "object", d.Object, "field", d.Field, "reason", d.Reason)
	}

	prepared, err := r.Publications.Prepare(previous, cm.ResourceVersion, candidate, catalog)
	if err != nil {
		return err
	}

	committed, err := r.CommitVersion(ctx, prepared)
	if err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if err := r.Publications.Install(committed); err != nil {
		return err
	}

	r.Accepted = candidate

	return nil
}

func (r *TopologyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Pod{}, podNodeIndex, podNodeKeys); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-topology").
		WatchesRawSource(initialEnqueue()).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(nodeChanges())).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(managedPodChanges(r.Config))).
		Watches(&appsv1.DaemonSet{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(r.Config.Namespace, r.Config.DaemonSetName))).
		Watches(&racerv1.ClusterCache{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(cacheChanges())).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(versionChanges(r.Config))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// singleton coalesces input changes without introducing a singleton CR.
func singleton(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "racer"}}}
}
