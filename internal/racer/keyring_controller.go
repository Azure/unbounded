// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

type RotationPolicy struct {
	Interval   time.Duration
	PrepareFor time.Duration
	RetainFor  time.Duration
}

// RotationState lives beside bundle.json in the shared Secret. It is sufficient
// to resume transitions after restart; no rotation-job or acknowledgment objects.
type RotationState struct {
	NextTransition time.Time `json:"next_transition"`
}

type KeyringReconciler struct {
	client.Client
	APIReader client.Reader
	Config    Config
	Issuer    *Issuer
}

// Reconcile creates/rotates issuer and cache keys through ordinary Secret CAS,
// stages trust before using a new issuer, and returns RequeueAfter for deadlines.
// Enforce projected size bounds including overlapping keys before committing.
func (*KeyringReconciler) Reconcile(_ context.Context, _ ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, pending("keyring.reconcile")
}

func (*KeyringReconciler) PlanRotation(_ wire.KeyringBundle, _ RotationState, _ []wire.CacheDefinition, _ time.Time) (wire.KeyringBundle, RotationState, error) {
	return wire.KeyringBundle{}, RotationState{}, pending("keyring.plan_rotation")
}

func (r *KeyringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-keyring").
		Watches(&racerv1.ClusterCache{}, handler.EnqueueRequestsFromMapFunc(singleton)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(singleton)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
