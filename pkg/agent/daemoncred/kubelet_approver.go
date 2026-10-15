// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemoncred

import (
	"context"
	"crypto/x509"
	"fmt"
	"slices"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
)

// KubeletCSRApproverOptions configures Machine-bound kubelet CSR approval.
type KubeletCSRApproverOptions struct {
	// BootstrapGroup is the authenticated bootstrap-token group allowed to
	// request initial kubelet client certificates.
	BootstrapGroup string

	// AuthorizeBootstrap verifies the token-to-Machine/node binding.
	AuthorizeBootstrap RequestAuthorizationFunc

	// MaxExpirationSeconds limits requested certificate lifetime.
	MaxExpirationSeconds int32
}

// KubeletCSRApprover validates initial kubelet client certificate requests.
type KubeletCSRApprover struct {
	opts KubeletCSRApproverOptions
}

// NewKubeletCSRApprover constructs a kubelet client CSR approver.
func NewKubeletCSRApprover(opts KubeletCSRApproverOptions) (*KubeletCSRApprover, error) {
	if opts.BootstrapGroup == "" {
		return nil, fmt.Errorf("bootstrap group is required")
	}

	if opts.AuthorizeBootstrap == nil {
		return nil, fmt.Errorf("bootstrap authorization callback is required")
	}

	if opts.MaxExpirationSeconds == 0 {
		opts.MaxExpirationSeconds = defaultMaxExpirationSeconds
	}

	if opts.MaxExpirationSeconds < 0 {
		return nil, fmt.Errorf("max expiration seconds must be non-negative")
	}

	return &KubeletCSRApprover{opts: opts}, nil
}

// Evaluate returns the approval decision for a kubelet client CSR.
func (a *KubeletCSRApprover) Evaluate(ctx context.Context, csr *certificatesv1.CertificateSigningRequest) (CSRDecision, error) {
	if csr.Spec.SignerName != certificatesv1.KubeAPIServerClientKubeletSignerName {
		return CSRDecision{Ignore: true}, nil
	}

	if HasApprovalDecision(csr) || len(csr.Status.Certificate) > 0 {
		return CSRDecision{AlreadyDecided: true}, nil
	}

	if !strings.HasPrefix(csr.Spec.Username, BootstrapUserPrefix) ||
		!slices.Contains(csr.Spec.Groups, a.opts.BootstrapGroup) {
		return CSRDecision{Ignore: true}, nil
	}

	x509cr, err := parseCSR(csr.Spec.Request)
	if err != nil {
		return Deny("unable to parse CSR: %v", err), nil
	}

	nodeName, decision := a.validateCSRShape(csr, x509cr)
	if decision.Message != "" {
		return decision, nil
	}

	for _, group := range csr.Spec.Groups {
		if _, ok := privilegedRequesterGroups[strings.ToLower(strings.TrimSpace(group))]; ok {
			return Deny("bootstrap token requester has forbidden group %q", group), nil
		}
	}

	allowed, err := a.opts.AuthorizeBootstrap(ctx, csr, nodeName)
	if err != nil {
		return CSRDecision{}, err
	}

	if !allowed {
		return Deny("bootstrap token is not allowed to claim node %q", nodeName), nil
	}

	return Approve("approved kubelet client certificate for node %q", nodeName), nil
}

func (a *KubeletCSRApprover) validateCSRShape(
	csr *certificatesv1.CertificateSigningRequest,
	x509cr *x509.CertificateRequest,
) (string, CSRDecision) {
	if err := x509cr.CheckSignature(); err != nil {
		return "", Deny("CSR signature is invalid: %v", err)
	}

	nodeName, ok := strings.CutPrefix(x509cr.Subject.CommonName, SystemNodeUserPrefix)
	if !ok || nodeName == "" {
		return "", Deny("common name must be %s<nodeName>", SystemNodeUserPrefix)
	}

	if hasUnexpectedSubjectFields(x509cr.Subject) {
		return "", Deny("subject must contain only common name and organizations")
	}

	if !equalStringSet(x509cr.Subject.Organization, []string{SystemNodesGroup}) {
		return "", Deny("organizations must be exactly %s", SystemNodesGroup)
	}

	if len(x509cr.DNSNames) > 0 || len(x509cr.EmailAddresses) > 0 || len(x509cr.IPAddresses) > 0 || len(x509cr.URIs) > 0 {
		return "", Deny("subject alternative names are not allowed")
	}

	if len(x509cr.Extensions) > 0 || len(x509cr.ExtraExtensions) > 0 {
		return "", Deny("CSR extensions are not allowed")
	}

	if err := validateClientAuthUsages(csr.Spec.Usages); err != nil {
		return "", Deny("invalid usages: %v", err)
	}

	if csr.Spec.ExpirationSeconds != nil && *csr.Spec.ExpirationSeconds > a.opts.MaxExpirationSeconds {
		return "", Deny("requested expiration %d exceeds maximum %d", *csr.Spec.ExpirationSeconds, a.opts.MaxExpirationSeconds)
	}

	return nodeName, CSRDecision{}
}
