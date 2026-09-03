// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemoncred

import (
	"context"
	"fmt"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	approvalReasonApproved = "Approved"
	approvalReasonDenied   = "Denied"
)

// CSRApproverReconciler watches CertificateSigningRequest objects and writes
// approval or denial conditions for requests evaluated by CSRApprover.
type CSRApproverReconciler struct {
	// Client reads CertificateSigningRequest objects from the controller cache.
	client.Client

	// KubeClient updates the CSR approval subresource.
	KubeClient kubernetes.Interface

	// Approver evaluates daemon-controller CSR requests.
	Approver *CSRApprover

	// EventFilter optionally customizes which CSR events enqueue reconcile work.
	// When nil, create, update, and generic events are processed and deletes are ignored.
	EventFilter predicate.Predicate
}

func NewCSRApproverReconciler(
	c client.Client,
	kubeClient kubernetes.Interface,
	approver *CSRApprover,
) (*CSRApproverReconciler, error) {
	if c == nil {
		return nil, fmt.Errorf("client is required")
	}

	if kubeClient == nil {
		return nil, fmt.Errorf("kubernetes client is required")
	}

	if approver == nil {
		return nil, fmt.Errorf("approver is required")
	}

	return &CSRApproverReconciler{
		Client:     c,
		KubeClient: kubeClient,
		Approver:   approver,
	}, nil
}

func (r *CSRApproverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var csr certificatesv1.CertificateSigningRequest
	if err := r.Get(ctx, req.NamespacedName, &csr); apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("get CSR %s: %w", req.Name, err)
	}

	decision, err := r.Approver.Evaluate(ctx, &csr)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("evaluate CSR %s: %w", csr.Name, err)
	}

	if decision.Ignore || decision.AlreadyDecided {
		return ctrl.Result{}, nil
	}

	conditionType := certificatesv1.CertificateApproved
	reason := approvalReasonApproved

	if !decision.Approve {
		conditionType = certificatesv1.CertificateDenied
		reason = approvalReasonDenied
	}

	updated := csr.DeepCopy()
	updated.Status.Conditions = append(updated.Status.Conditions, certificatesv1.CertificateSigningRequestCondition{
		Type:           conditionType,
		Status:         corev1.ConditionTrue,
		Reason:         reason,
		Message:        decision.Message,
		LastUpdateTime: metav1.Now(),
	})

	if _, err := r.KubeClient.CertificatesV1().CertificateSigningRequests().UpdateApproval(ctx, updated.Name, updated, metav1.UpdateOptions{}); err != nil {
		return ctrl.Result{}, fmt.Errorf("update CSR approval %s: %w", updated.Name, err)
	}

	return ctrl.Result{}, nil
}

func (r *CSRApproverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	filter := r.EventFilter
	if filter == nil {
		filter = defaultCSRApproverEventFilter()
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&certificatesv1.CertificateSigningRequest{}).
		WithEventFilter(filter).
		Complete(r)
}

func defaultCSRApproverEventFilter() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		UpdateFunc:  func(event.UpdateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return true },
	}
}
