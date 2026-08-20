// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/kube"
)

const (
	clusterSiteName = "cluster"
	fieldManager    = "token-refresher"
)

type TokenReconciler struct {
	client.Client
	KubeClient kubernetes.Interface
}

func (r *TokenReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	site := &unboundedv1alpha3.Site{}
	if err := r.Get(ctx, req.NamespacedName, site); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("get Site: %w", err)
	}

	if site.Name == clusterSiteName {
		return ctrl.Result{}, nil
	}

	component := site.Spec.Components.TokenRefresher
	if component != nil && component.Enabled != nil && !*component.Enabled {
		return ctrl.Result{}, nil
	}

	token, err := kube.GetBootstrapTokenForSite(ctx, r.KubeClient, site.Name)
	if err == nil {
		if token.ExpiresAt.IsZero() {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{RequeueAfter: time.Until(token.ExpiresAt)}, nil
	}

	if !errors.Is(err, kube.ErrBootstrapTokenNotFound) {
		return ctrl.Result{}, fmt.Errorf("get bootstrap token for Site %q: %w", site.Name, err)
	}

	token, err = kube.NewBootstrapToken()
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("generate bootstrap token for Site %q: %w", site.Name, err)
	}

	token.WithLabel(unboundedv1alpha3.MachineSiteLabelKey, site.Name)

	if err := kube.ApplyBootstrapToken(ctx, r.KubeClient, fieldManager, token); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply bootstrap token for Site %q: %w", site.Name, err)
	}

	ctrl.LoggerFrom(ctx).Info("Created bootstrap token", "site", site.Name, "expiresAt", token.ExpiresAt)

	return ctrl.Result{RequeueAfter: time.Until(token.ExpiresAt)}, nil
}

func (r *TokenReconciler) requestsForSecret(_ context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret.Namespace != metav1.NamespaceSystem || secret.Type != corev1.SecretTypeBootstrapToken {
		return nil
	}

	siteName := secret.Labels[unboundedv1alpha3.MachineSiteLabelKey]
	if siteName == "" || siteName == clusterSiteName {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: siteName}}}
}

func (r *TokenReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&unboundedv1alpha3.Site{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForSecret)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
