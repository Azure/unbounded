// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
)

type RotationPolicy = authority.RotationPolicy

type KeyringReconciler struct {
	settings  frozenConfig
	Config    Config
	authority *authority.Authority
}

func (r *KeyringReconciler) runtimeConfig() Config { return r.settings.get(&r.Config) }

func (r *KeyringReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	delay, err := r.authority.ReconcileCredentials(ctx)
	if ctx.Err() != nil {
		return ctrl.Result{}, reconcile.TerminalError(ctx.Err())
	}

	if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
		return ctrl.Result{RequeueAfter: retryConflictDelay}, nil
	}

	return ctrl.Result{RequeueAfter: delay}, err
}

func (r *KeyringReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cfg := r.runtimeConfig()

	return ctrl.NewControllerManagedBy(mgr).
		Named("racer-credentials").
		WatchesRawSource(initialEnqueue()).
		Watches(&racerv1.ClusterCache{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(cacheChanges())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(namedChanges(cfg.Namespace, cfg.CredentialsSecretName))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(singleton), builder.WithPredicates(versionChanges(cfg))).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).Complete(r)
}
