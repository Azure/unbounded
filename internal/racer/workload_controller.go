// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

type WorkloadReconciler struct {
	client.Client
	Config Config
}

func (*WorkloadReconciler) Reconcile(_ context.Context, _ ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, pending("workload.reconcile")
}

// DesiredDaemonSet declares the token audience, common keyring/trust projection,
// node-private identity and slab storage, socket mounts, and exclusion affinity.
// It must never introduce a per-node Secret or trust a node-name as a Node UID.
func (*WorkloadReconciler) DesiredDaemonSet() (*appsv1.DaemonSet, error) {
	return nil, pending("workload.desired_daemonset")
}

func (r *WorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-workload").
		Watches(&appsv1.DaemonSet{}, handler.EnqueueRequestsFromMapFunc(singleton)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
