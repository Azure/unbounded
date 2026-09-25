// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
)

type RotationPolicy struct {
	Interval   time.Duration
	PrepareFor time.Duration
	RetainFor  time.Duration
}

// RotationState lives beside bundle.json in the shared Secret. It is sufficient
// to resume transitions after restart; no rotation-job or acknowledgment objects.
type RotationState struct {
	NextTransition time.Time            `json:"next_transition"`
	NextRotation   time.Time            `json:"next_rotation"`
	ActivateAt     time.Time            `json:"activate_at"`
	ActiveIssuer   string               `json:"active_issuer"`
	PreparedIssuer string               `json:"prepared_issuer"`
	Retiring       map[string]time.Time `json:"retiring"`
}

type KeyringReconciler struct {
	client.Client
	APIReader client.Reader
	Config    Config
	Issuer    *Issuer
	Lifecycle *Lifecycle
	Now       func() time.Time
}

// Reconcile creates/rotates issuer and cache keys through ordinary Secret CAS,
// stages trust before using a new issuer, and returns RequeueAfter for deadlines.
// Enforce projected size bounds including overlapping keys before committing.
func (r *KeyringReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	result, err := r.reconcileKeys(ctx)
	if ctx.Err() != nil {
		err = ctx.Err()
	}

	if r.Lifecycle != nil {
		r.Lifecycle.SetIssuerReady(err == nil)
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ctrl.Result{}, reconcile.TerminalError(err)
	}

	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return ctrl.Result{RequeueAfter: retryConflictDelay}, nil
	}

	return result, err
}

func (r *KeyringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-keyring").
		WatchesRawSource(initialEnqueue()).
		Watches(&racerv1.ClusterCache{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(cacheChanges())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(r.Config.Namespace, r.Config.IssuerSecretName, r.Config.KeyringSecretName))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(versionChanges(r.Config))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
