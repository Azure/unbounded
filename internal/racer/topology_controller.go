// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
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
func (*TopologyReconciler) Reconcile(_ context.Context, _ ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, pending("topology.reconcile")
}

func (*TopologyReconciler) CommitVersion(_ context.Context, _ *PreparedPublication) (*CommittedPublication, error) {
	return nil, pending("topology.commit_version")
}

// InitializeVersion is an explicit first-install boundary, never an automatic
// reconcile fallback. It must reject existing state and ambiguous recovery.
func (*TopologyReconciler) InitializeVersion(_ context.Context) error {
	return pending("topology.initialize_version")
}

func (r *TopologyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-topology").
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(singleton)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(singleton)).
		Watches(&racerv1.ClusterCache{}, handler.EnqueueRequestsFromMapFunc(singleton)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// singleton coalesces input changes without introducing a singleton CR.
func singleton(_ context.Context, _ client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "racer"}}}
}
