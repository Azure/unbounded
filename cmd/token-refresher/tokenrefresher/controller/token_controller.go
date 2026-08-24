// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/kube"
)

const (
	clusterSiteName  = "cluster"
	fieldManager     = "token-refresher"
	machineSiteField = "token-refresher.machine-site"
)

type TokenReconciler struct {
	client.Client
	APIReader  client.Reader
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
	if err != nil && !errors.Is(err, kube.ErrBootstrapTokenNotFound) {
		return ctrl.Result{}, fmt.Errorf("get bootstrap token for Site %q: %w", site.Name, err)
	}

	if errors.Is(err, kube.ErrBootstrapTokenNotFound) {
		token, err = kube.NewBootstrapToken()
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("generate bootstrap token for Site %q: %w", site.Name, err)
		}

		token.WithLabel(unboundedv1alpha3.MachineSiteLabelKey, site.Name)

		if err := kube.ApplyBootstrapToken(ctx, r.KubeClient, fieldManager, token); err != nil {
			return ctrl.Result{}, fmt.Errorf("apply bootstrap token for Site %q: %w", site.Name, err)
		}

		ctrl.LoggerFrom(ctx).Info("Created bootstrap token", "site", site.Name, "expiresAt", token.ExpiresAt)
	}

	if err := r.reconcileMachines(ctx, site.Name, kube.BootstrapTokenSecretName(token.ID)); err != nil {
		return ctrl.Result{}, err
	}

	if token.ExpiresAt.IsZero() {
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: time.Until(token.ExpiresAt)}, nil
}

func (r *TokenReconciler) reconcileMachines(ctx context.Context, siteName, desiredSecretName string) error {
	machines := &unboundedv1alpha3.MachineList{}
	if err := r.List(ctx, machines, client.MatchingFields{machineSiteField: siteName}); err != nil {
		return fmt.Errorf("list Machines for Site %q: %w", siteName, err)
	}

	var errs []error

	for i := range machines.Items {
		machine := &machines.Items[i]
		if machine.Spec.Kubernetes == nil || machine.Spec.Kubernetes.BootstrapTokenRef == nil {
			continue
		}

		if err := r.ensureMachineTokenRef(ctx, machine.Name, siteName, desiredSecretName); err != nil {
			errs = append(errs, fmt.Errorf("update Machine %q: %w", machine.Name, err))
		}
	}

	return errors.Join(errs...)
}

func (r *TokenReconciler) ensureMachineTokenRef(ctx context.Context, machineName, siteName, desiredSecretName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		machine := &unboundedv1alpha3.Machine{}
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: machineName}, machine); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}

			return err
		}

		if machine.Labels[unboundedv1alpha3.MachineSiteLabelKey] != siteName ||
			machine.Spec.Kubernetes == nil || machine.Spec.Kubernetes.BootstrapTokenRef == nil {
			return nil
		}

		ref := machine.Spec.Kubernetes.BootstrapTokenRef
		if ref.Name != "" {
			secret, err := r.KubeClient.CoreV1().Secrets(metav1.NamespaceSystem).Get(ctx, ref.Name, metav1.GetOptions{})
			if err == nil && kube.ValidBootstrapTokenSecretForSite(secret, siteName, time.Now()) {
				return nil
			}

			if err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("get referenced bootstrap token %q: %w", ref.Name, err)
			}
		}

		base := machine.DeepCopy()

		machine.Spec.Kubernetes.BootstrapTokenRef = &unboundedv1alpha3.LocalObjectReference{Name: desiredSecretName}
		if err := r.Patch(ctx, machine, client.MergeFrom(base)); err != nil {
			return err
		}

		ctrl.LoggerFrom(ctx).Info("Updated Machine bootstrap token reference", "site", siteName, "machine", machine.Name, "secret", desiredSecretName)

		return nil
	})
}

func (r *TokenReconciler) requestsForSecret(_ context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok || secret.Namespace != metav1.NamespaceSystem {
		return nil
	}

	siteName := secret.Labels[unboundedv1alpha3.MachineSiteLabelKey]
	if siteName == "" || siteName == clusterSiteName {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: siteName}}}
}

func (r *TokenReconciler) requestsForMachine(_ context.Context, obj client.Object) []reconcile.Request {
	machine, ok := obj.(*unboundedv1alpha3.Machine)
	if !ok {
		return nil
	}

	siteName := machine.Labels[unboundedv1alpha3.MachineSiteLabelKey]
	if siteName == "" || siteName == clusterSiteName {
		return nil
	}

	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: siteName}}}
}

func machineSiteIndex(obj client.Object) []string {
	siteName := obj.GetLabels()[unboundedv1alpha3.MachineSiteLabelKey]
	if siteName == "" {
		return nil
	}

	return []string{siteName}
}

func machineTokenPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool { return true },
		DeleteFunc: func(event.DeleteEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldMachine, oldOK := e.ObjectOld.(*unboundedv1alpha3.Machine)

			newMachine, newOK := e.ObjectNew.(*unboundedv1alpha3.Machine)
			if !oldOK || !newOK {
				return false
			}

			return oldMachine.Labels[unboundedv1alpha3.MachineSiteLabelKey] != newMachine.Labels[unboundedv1alpha3.MachineSiteLabelKey] ||
				!reflect.DeepEqual(machineTokenRef(oldMachine), machineTokenRef(newMachine))
		},
	}
}

func machineTokenRef(machine *unboundedv1alpha3.Machine) *unboundedv1alpha3.LocalObjectReference {
	if machine.Spec.Kubernetes == nil {
		return nil
	}

	return machine.Spec.Kubernetes.BootstrapTokenRef
}

func (r *TokenReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&unboundedv1alpha3.Site{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.requestsForSecret)).
		Watches(&unboundedv1alpha3.Machine{}, handler.EnqueueRequestsFromMapFunc(r.requestsForMachine), builder.WithPredicates(machineTokenPredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}
