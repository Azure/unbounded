// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemoncred

import (
	"context"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"slices"
	"sort"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
)

var privilegedRequesterGroups = map[string]struct{}{
	"admin":          {},
	"admins":         {},
	"cluster-admin":  {},
	"cluster-admins": {},
	"master":         {},
	"masters":        {},
	"root":           {},
	"system:masters": {},
}

const (
	SystemNodesGroup     = "system:nodes"
	SystemNodeUserPrefix = "system:node:"
	BootstrapUserPrefix  = "system:bootstrap:"

	defaultMaxExpirationSeconds = int32(365 * 24 * 60 * 60)
)

// RequestAuthorizationFunc authorizes a parsed daemon-controller CSR request for
// the requested node name. Callers provide implementation-specific binding
// checks, such as bootstrap token to Machine/Node validation.
type RequestAuthorizationFunc func(context.Context, *certificatesv1.CertificateSigningRequest, string) (bool, error)

// CSRApproverOptions configures daemon-controller CSR validation.
type CSRApproverOptions struct {
	// SignerName is the certificates.k8s.io signerName this approver handles.
	// Defaults to kubernetes.io/kube-apiserver-client when empty.
	SignerName string

	// DaemonGroup is the non-privileged group requested in the issued certificate
	// alongside system:nodes. It is required and must not use reserved names.
	DaemonGroup string

	// BootstrapGroup is the bootstrap-token requester group allowed to request
	// initial daemon-controller certificates. It is required and intentionally not
	// derived from DaemonGroup so integrations can choose their own bootstrap group.
	BootstrapGroup string

	// MaxExpirationSeconds is the maximum allowed spec.expirationSeconds value.
	// Defaults to 365 days when unset.
	MaxExpirationSeconds int32

	// AuthorizeBootstrap validates implementation-specific bootstrap-token to node
	// binding after the generic CSR shape and requester group checks pass.
	AuthorizeBootstrap RequestAuthorizationFunc

	// AuthorizeRenewal validates implementation-specific existing-cert to node
	// binding after the generic CSR shape and requester identity checks pass.
	AuthorizeRenewal RequestAuthorizationFunc
}

// CSRApprover validates daemon-controller CertificateSigningRequests and returns
// approval decisions. It enforces generic certificate shape and requester checks;
// callers provide resource-binding checks through callbacks.
type CSRApprover struct {
	opts CSRApproverOptions
}

// CSRDecision describes the outcome of evaluating one CertificateSigningRequest.
type CSRDecision struct {
	// Ignore is true when the CSR is for another signer and should not be touched.
	Ignore bool

	// AlreadyDecided is true when the CSR already has a terminal approval state or certificate.
	AlreadyDecided bool

	// Approve is true for approval decisions and false for denial decisions.
	Approve bool

	// Message is written to the CSR approval or denial condition.
	Message string
}

func NewCSRApprover(opts CSRApproverOptions) (*CSRApprover, error) {
	if opts.SignerName == "" {
		opts.SignerName = DefaultControllerCertificateSignerName
	}
	if opts.DaemonGroup == "" {
		return nil, fmt.Errorf("daemon group is required")
	}
	if isReservedDaemonGroup(opts.DaemonGroup) {
		return nil, fmt.Errorf("daemon group must not use reserved or privileged group name: %s", opts.DaemonGroup)
	}
	if opts.MaxExpirationSeconds == 0 {
		opts.MaxExpirationSeconds = defaultMaxExpirationSeconds
	}
	if opts.MaxExpirationSeconds < 0 {
		return nil, fmt.Errorf("max expiration seconds must be non-negative")
	}
	if opts.BootstrapGroup == "" {
		return nil, fmt.Errorf("bootstrap group is required")
	}
	if opts.AuthorizeBootstrap == nil {
		return nil, fmt.Errorf("bootstrap authorization callback is required")
	}
	if opts.AuthorizeRenewal == nil {
		return nil, fmt.Errorf("renewal authorization callback is required")
	}

	return &CSRApprover{opts: opts}, nil
}

func (a *CSRApprover) Evaluate(ctx context.Context, csr *certificatesv1.CertificateSigningRequest) (CSRDecision, error) {
	if csr.Spec.SignerName != a.opts.SignerName {
		return CSRDecision{Ignore: true}, nil
	}
	if HasApprovalDecision(csr) || len(csr.Status.Certificate) > 0 {
		return CSRDecision{AlreadyDecided: true}, nil
	}

	x509cr, err := parseCSR(csr.Spec.Request)
	if err != nil {
		if a.hasDaemonRequester(csr) {
			return Deny("unable to parse CSR: %v", err), nil
		}

		return CSRDecision{Ignore: true}, nil
	}
	if !a.handlesCSR(csr, x509cr) {
		return CSRDecision{Ignore: true}, nil
	}

	nodeName, decision, err := a.validateCSRShape(csr, x509cr)
	if err != nil || decision.Message != "" {
		return decision, err
	}

	return a.evaluateRequester(ctx, csr, nodeName)
}

func (a *CSRApprover) validateCSRShape(
	csr *certificatesv1.CertificateSigningRequest,
	x509cr *x509.CertificateRequest,
) (string, CSRDecision, error) {
	if err := x509cr.CheckSignature(); err != nil {
		return "", Deny("CSR signature is invalid: %v", err), nil
	}

	nodeName, ok := strings.CutPrefix(x509cr.Subject.CommonName, SystemNodeUserPrefix)
	if !ok || nodeName == "" {
		return "", Deny("common name must be %s<nodeName>", SystemNodeUserPrefix), nil
	}
	if hasUnexpectedSubjectFields(x509cr.Subject) {
		return "", Deny("subject must contain only common name and organizations"), nil
	}
	if !equalStringSet(x509cr.Subject.Organization, []string{SystemNodesGroup, a.opts.DaemonGroup}) {
		return "", Deny("organizations must be exactly %s and %s", SystemNodesGroup, a.opts.DaemonGroup), nil
	}
	if len(x509cr.DNSNames) > 0 || len(x509cr.EmailAddresses) > 0 || len(x509cr.IPAddresses) > 0 || len(x509cr.URIs) > 0 {
		return "", Deny("subject alternative names are not allowed"), nil
	}
	if len(x509cr.Extensions) > 0 || len(x509cr.ExtraExtensions) > 0 {
		return "", Deny("CSR extensions are not allowed"), nil
	}
	if err := validateClientAuthUsages(csr.Spec.Usages); err != nil {
		return "", Deny("invalid usages: %v", err), nil
	}
	if csr.Spec.ExpirationSeconds != nil && *csr.Spec.ExpirationSeconds > a.opts.MaxExpirationSeconds {
		return "", Deny("requested expiration %d exceeds maximum %d", *csr.Spec.ExpirationSeconds, a.opts.MaxExpirationSeconds), nil
	}

	return nodeName, CSRDecision{}, nil
}

func (a *CSRApprover) handlesCSR(csr *certificatesv1.CertificateSigningRequest, x509cr *x509.CertificateRequest) bool {
	return a.hasDaemonRequester(csr) || slices.Contains(x509cr.Subject.Organization, a.opts.DaemonGroup)
}

func (a *CSRApprover) hasDaemonRequester(csr *certificatesv1.CertificateSigningRequest) bool {
	if strings.HasPrefix(csr.Spec.Username, BootstrapUserPrefix) && slices.Contains(csr.Spec.Groups, a.opts.BootstrapGroup) {
		return true
	}

	return strings.HasPrefix(csr.Spec.Username, SystemNodeUserPrefix) && slices.Contains(csr.Spec.Groups, a.opts.DaemonGroup)
}

func (a *CSRApprover) evaluateRequester(ctx context.Context, csr *certificatesv1.CertificateSigningRequest, nodeName string) (CSRDecision, error) {
	if strings.HasPrefix(csr.Spec.Username, BootstrapUserPrefix) {
		return a.evaluateBootstrapRequester(ctx, csr, nodeName)
	}
	if constantTimeEqual(csr.Spec.Username, SystemNodeUserPrefix+nodeName) {
		return a.evaluateRenewalRequester(ctx, csr, nodeName)
	}

	return Deny("requester is neither an authorized bootstrap token nor matching daemon-controller identity"), nil
}

func (a *CSRApprover) evaluateBootstrapRequester(ctx context.Context, csr *certificatesv1.CertificateSigningRequest, nodeName string) (CSRDecision, error) {
	// validateCSRShape checks groups requested in the certificate subject.
	// csr.Spec.Groups is different: it is the authenticated requester groups
	// on the CSR create request. Bootstrap requests must come through the
	// configured bootstrap requester group before the binding callback is trusted.
	for _, group := range csr.Spec.Groups {
		if _, ok := privilegedRequesterGroups[strings.ToLower(strings.TrimSpace(group))]; ok {
			return Deny("bootstrap token requester has forbidden group %q", group), nil
		}
	}
	if !slices.Contains(csr.Spec.Groups, a.opts.BootstrapGroup) {
		return Deny("bootstrap token requester is missing required group %q", a.opts.BootstrapGroup), nil
	}
	allowed, err := a.opts.AuthorizeBootstrap(ctx, csr, nodeName)
	if err != nil {
		return CSRDecision{}, err
	}
	if !allowed {
		return Deny("bootstrap token is not allowed to claim node %q", nodeName), nil
	}

	return Approve("approved initial daemon-controller certificate for node %q", nodeName), nil
}

func (a *CSRApprover) evaluateRenewalRequester(ctx context.Context, csr *certificatesv1.CertificateSigningRequest, nodeName string) (CSRDecision, error) {
	// validateCSRShape checks the requested renewed certificate subject. For
	// renewal, csr.Spec.Groups must also prove the requester already authenticated
	// with the daemon-controller certificate identity. A stock kubelet cert has
	// system:nodes but not the daemon group and must not be able to renew into one.
	for _, group := range csr.Spec.Groups {
		if _, ok := privilegedRequesterGroups[strings.ToLower(strings.TrimSpace(group))]; ok {
			return Deny("renewal requester has forbidden group %q", group), nil
		}
	}
	if !slices.Contains(csr.Spec.Groups, SystemNodesGroup) || !slices.Contains(csr.Spec.Groups, a.opts.DaemonGroup) {
		return Deny("renewal requester is missing required node or daemon group"), nil
	}
	allowed, err := a.opts.AuthorizeRenewal(ctx, csr, nodeName)
	if err != nil {
		return CSRDecision{}, err
	}
	if !allowed {
		return Deny("node %q is not bound to a Machine", nodeName), nil
	}

	return Approve("approved daemon-controller certificate renewal for node %q", nodeName), nil
}

func HasApprovalDecision(csr *certificatesv1.CertificateSigningRequest) bool {
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

func Approve(format string, args ...any) CSRDecision {
	return CSRDecision{Approve: true, Message: fmt.Sprintf(format, args...)}
}

func Deny(format string, args ...any) CSRDecision {
	return CSRDecision{Approve: false, Message: fmt.Sprintf(format, args...)}
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

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
