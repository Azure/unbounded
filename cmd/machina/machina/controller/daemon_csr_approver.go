// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"sort"
	"strings"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
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
	DaemonControllerSignerName     = "kubernetes.io/kube-apiserver-client"
	DaemonControllerGroup          = "unbounded-agent-daemons"
	DaemonControllerBootstrapGroup = "system:bootstrappers:unbounded-agent-daemons"

	systemNodesGroup     = "system:nodes"
	systemNodeUserPrefix = "system:node:"
	bootstrapUserPrefix  = "system:bootstrap:"
	bootstrapSecretPref  = "bootstrap-token-"

	maxDaemonCertificateExpirationSeconds = int32(365 * 24 * 60 * 60)

	approvalReasonApproved = "Approved"
	approvalReasonDenied   = "Denied"
)

type DaemonCSRApproverReconciler struct {
	client.Client
	KubeClient kubernetes.Interface
	Evaluator  daemonCSREvaluator
}

func NewDaemonCSRApproverReconciler(c client.Client, kubeClient kubernetes.Interface) *DaemonCSRApproverReconciler {
	return &DaemonCSRApproverReconciler{
		Client:     c,
		KubeClient: kubeClient,
		Evaluator: daemonCSREvaluator{
			SignerName:     DaemonControllerSignerName,
			DaemonGroup:    DaemonControllerGroup,
			BootstrapGroup: DaemonControllerBootstrapGroup,
		},
	}
}

func (r *DaemonCSRApproverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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

func (r *DaemonCSRApproverReconciler) SetupWithManager(mgr ctrl.Manager) error {
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

type daemonCSREvaluator struct {
	SignerName     string
	DaemonGroup    string
	BootstrapGroup string
}

type daemonCSRDecision struct {
	Ignore         bool
	AlreadyDecided bool
	Approve        bool
	Message        string
}

func (e daemonCSREvaluator) Evaluate(ctx context.Context, c client.Client, csr *certificatesv1.CertificateSigningRequest) (daemonCSRDecision, error) {
	if csr.Spec.SignerName != e.SignerName {
		return daemonCSRDecision{Ignore: true}, nil
	}
	if hasApprovalDecision(csr) || len(csr.Status.Certificate) > 0 {
		return daemonCSRDecision{AlreadyDecided: true}, nil
	}

	x509cr, err := parseCSR(csr.Spec.Request)
	if err != nil {
		return deny("unable to parse CSR: %v", err), nil
	}
	if err := x509cr.CheckSignature(); err != nil {
		return deny("CSR signature is invalid: %v", err), nil
	}

	nodeName, ok := strings.CutPrefix(x509cr.Subject.CommonName, systemNodeUserPrefix)
	if !ok || nodeName == "" {
		return deny("common name must be %s<nodeName>", systemNodeUserPrefix), nil
	}
	if hasUnexpectedSubjectFields(x509cr.Subject) {
		return deny("subject must contain only common name and organizations"), nil
	}

	if !equalStringSet(x509cr.Subject.Organization, []string{systemNodesGroup, e.DaemonGroup}) {
		return deny("organizations must be exactly %s and %s", systemNodesGroup, e.DaemonGroup), nil
	}

	if len(x509cr.DNSNames) > 0 || len(x509cr.EmailAddresses) > 0 || len(x509cr.IPAddresses) > 0 || len(x509cr.URIs) > 0 {
		return deny("subject alternative names are not allowed"), nil
	}
	if len(x509cr.Extensions) > 0 || len(x509cr.ExtraExtensions) > 0 {
		return deny("CSR extensions are not allowed"), nil
	}

	if err := validateClientAuthUsages(csr.Spec.Usages); err != nil {
		return deny("invalid usages: %v", err), nil
	}
	if csr.Spec.ExpirationSeconds != nil && *csr.Spec.ExpirationSeconds > maxDaemonCertificateExpirationSeconds {
		return deny("requested expiration %d exceeds maximum %d", *csr.Spec.ExpirationSeconds, maxDaemonCertificateExpirationSeconds), nil
	}

	if strings.HasPrefix(csr.Spec.Username, bootstrapUserPrefix) {
		// This group check is only a coarse bootstrap gate. Production use needs
		// a stronger node-claim check that proves this bootstrap token is allowed
		// to request the specific system:node:<nodeName> identity in the CSR.
		if !hasString(csr.Spec.Groups, e.BootstrapGroup) {
			return deny("bootstrap token requester is missing required group %q", e.BootstrapGroup), nil
		}
		allowed, err := e.bootstrapTokenMayClaimNode(ctx, c, csr.Spec.Username, nodeName)
		if err != nil {
			return daemonCSRDecision{}, err
		}
		if !allowed {
			return deny("bootstrap token is not allowed to claim node %q", nodeName), nil
		}

		return approve("approved initial daemon-controller certificate for node %q", nodeName), nil
	}

	if csr.Spec.Username == x509cr.Subject.CommonName && hasString(csr.Spec.Groups, systemNodesGroup) && hasString(csr.Spec.Groups, e.DaemonGroup) {
		return approve("approved daemon-controller certificate renewal for node %q", nodeName), nil
	}

	return deny("requester is neither an authorized bootstrap token nor matching daemon-controller identity"), nil
}

func (e daemonCSREvaluator) bootstrapTokenMayClaimNode(ctx context.Context, c client.Client, username string, nodeName string) (bool, error) {
	tokenID := strings.TrimPrefix(username, bootstrapUserPrefix)
	if tokenID == "" {
		return false, nil
	}

	secretName := bootstrapSecretPref + tokenID
	var token corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: metav1.NamespaceSystem, Name: secretName}, &token); err != nil {
		return false, client.IgnoreNotFound(err)
	}

	siteName := strings.TrimSpace(token.Labels[unboundedv1alpha3.MachineSiteLabelKey])
	if siteName == "" {
		return false, nil
	}

	var machines unboundedv1alpha3.MachineList
	if err := c.List(ctx, &machines); err != nil {
		return false, fmt.Errorf("list Machines for bootstrap token claim check: %w", err)
	}

	for _, machine := range machines.Items {
		if machine.Labels[unboundedv1alpha3.MachineSiteLabelKey] != siteName {
			continue
		}
		if machine.Spec.Kubernetes == nil {
			continue
		}
		if machine.Spec.Kubernetes.BootstrapTokenRef.Name != secretName {
			continue
		}

		expectedNodeName := machine.Name
		if machine.Spec.Kubernetes.NodeRef != nil && machine.Spec.Kubernetes.NodeRef.Name != "" {
			expectedNodeName = machine.Spec.Kubernetes.NodeRef.Name
		}

		return expectedNodeName == nodeName, nil
	}

	return false, nil
}

func hasUnexpectedSubjectFields(subject pkix.Name) bool {
	return len(subject.Country) > 0 ||
		len(subject.OrganizationalUnit) > 0 ||
		len(subject.Locality) > 0 ||
		len(subject.Province) > 0 ||
		len(subject.StreetAddress) > 0 ||
		len(subject.PostalCode) > 0 ||
		subject.SerialNumber != "" ||
		len(subject.ExtraNames) > 0
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

func approve(format string, args ...any) daemonCSRDecision {
	return daemonCSRDecision{Approve: true, Message: fmt.Sprintf(format, args...)}
}

func deny(format string, args ...any) daemonCSRDecision {
	return daemonCSRDecision{Approve: false, Message: fmt.Sprintf(format, args...)}
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
