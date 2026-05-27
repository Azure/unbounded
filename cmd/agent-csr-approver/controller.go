// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"

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
	systemNodesGroup     = "system:nodes"
	systemNodeUserPrefix = "system:node:"
	bootstrapUserPrefix  = "system:bootstrap:"

	approvalReasonApproved = "Approved"
	approvalReasonDenied   = "Denied"
)

type csrApproverReconciler struct {
	client.Client
	KubeClient kubernetes.Interface
	Evaluator  csrEvaluator
}

func (r *csrApproverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var csr certificatesv1.CertificateSigningRequest
	if err := r.Get(ctx, req.NamespacedName, &csr); apierrors.IsNotFound(err) {
		return ctrl.Result{}, nil
	} else if err != nil {
		return ctrl.Result{}, fmt.Errorf("get CSR %s: %w", req.Name, err)
	}

	decision, err := r.Evaluator.Evaluate(ctx, r.Client, &csr)
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

func (r *csrApproverReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&certificatesv1.CertificateSigningRequest{}).
		WithEventFilter(predicate.Funcs{
			CreateFunc:  func(event.CreateEvent) bool { return true },
			UpdateFunc:  func(event.UpdateEvent) bool { return true },
			DeleteFunc:  func(event.DeleteEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return true },
		}).
		Complete(r)
}

type csrEvaluator struct {
	SignerName  string
	DaemonGroup string
}

type csrDecision struct {
	Ignore         bool
	AlreadyDecided bool
	Approve        bool
	Message        string
}

func (e csrEvaluator) Evaluate(ctx context.Context, c client.Client, csr *certificatesv1.CertificateSigningRequest) (csrDecision, error) {
	if csr.Spec.SignerName != e.SignerName {
		return csrDecision{Ignore: true}, nil
	}
	if hasApprovalDecision(csr) || len(csr.Status.Certificate) > 0 {
		return csrDecision{AlreadyDecided: true}, nil
	}

	x509cr, err := parseCSR(csr.Spec.Request)
	if err != nil {
		return deny("unable to parse CSR: %v", err), nil
	}

	nodeName, ok := strings.CutPrefix(x509cr.Subject.CommonName, systemNodeUserPrefix)
	if !ok || nodeName == "" {
		return deny("common name must be %s<nodeName>", systemNodeUserPrefix), nil
	}

	if !equalStringSet(x509cr.Subject.Organization, []string{systemNodesGroup, e.DaemonGroup}) {
		return deny("organizations must be exactly %s and %s", systemNodesGroup, e.DaemonGroup), nil
	}

	if len(x509cr.DNSNames) > 0 || len(x509cr.EmailAddresses) > 0 || len(x509cr.IPAddresses) > 0 || len(x509cr.URIs) > 0 {
		return deny("subject alternative names are not allowed"), nil
	}

	if err := validateClientAuthUsages(csr.Spec.Usages); err != nil {
		return deny("invalid usages: %v", err), nil
	}

	if strings.HasPrefix(csr.Spec.Username, bootstrapUserPrefix) {
		// This group check is only a coarse bootstrap gate. Production use needs
		// a stronger node-claim check that proves this bootstrap token is allowed
		// to request the specific system:node:<nodeName> identity in the CSR.
		if !hasString(csr.Spec.Groups, e.DaemonGroup) {
			return deny("bootstrap token requester is missing required group %q", e.DaemonGroup), nil
		}

		return approve("approved initial daemon-controller certificate for node %q", nodeName), nil
	}

	if csr.Spec.Username == x509cr.Subject.CommonName && hasString(csr.Spec.Groups, systemNodesGroup) && hasString(csr.Spec.Groups, e.DaemonGroup) {
		return approve("approved daemon-controller certificate renewal for node %q", nodeName), nil
	}

	return deny("requester is neither an authorized bootstrap token nor matching daemon-controller identity"), nil
}

func parseCSR(data []byte) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("PEM block type must be CERTIFICATE REQUEST")
	}

	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}

	return request, nil
}

func validateClientAuthUsages(usages []certificatesv1.KeyUsage) error {
	allowed := map[certificatesv1.KeyUsage]bool{
		certificatesv1.UsageDigitalSignature: true,
		certificatesv1.UsageKeyEncipherment:  true,
		certificatesv1.UsageClientAuth:       true,
	}
	seenClientAuth := false
	for _, usage := range usages {
		if !allowed[usage] {
			return fmt.Errorf("unsupported usage %q", usage)
		}
		if usage == certificatesv1.UsageClientAuth {
			seenClientAuth = true
		}
	}
	if !seenClientAuth {
		return fmt.Errorf("missing %q", certificatesv1.UsageClientAuth)
	}

	return nil
}

func hasApprovalDecision(csr *certificatesv1.CertificateSigningRequest) bool {
	for _, condition := range csr.Status.Conditions {
		switch condition.Type {
		case certificatesv1.CertificateApproved, certificatesv1.CertificateDenied, certificatesv1.CertificateFailed:
			if condition.Status == corev1.ConditionTrue {
				return true
			}
		}
	}

	return false
}

func approve(format string, args ...any) csrDecision {
	return csrDecision{Approve: true, Message: fmt.Sprintf(format, args...)}
}

func deny(format string, args ...any) csrDecision {
	return csrDecision{Approve: false, Message: fmt.Sprintf(format, args...)}
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}

	return true
}

func hasString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}

	return false
}
