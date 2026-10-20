// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemoncred

import (
	"context"
	"errors"
	"fmt"
	"strings"

	certificatesv1 "k8s.io/api/certificates/v1"
)

const (
	// KubeletClientSignerName is the signer used for kubelet client certificates.
	KubeletClientSignerName = certificatesv1.KubeAPIServerClientKubeletSignerName

	// CredentialIDExtraKey is the user extra key the API server attaches to
	// requests authenticated with a bound ServiceAccount token (KEP-4193). For
	// ServiceAccount tokens its value is "JTI=<jti>".
	CredentialIDExtraKey = "authentication.kubernetes.io/credential-id"
)

// ErrUnknownCredential is returned, possibly wrapped, by a
// KubeletNodeAuthorizationFunc that has no record of the credential. The CSR
// is left undecided rather than denied, so an approver that does know the
// credential, or a later evaluation once the record is observed, decides it.
var ErrUnknownCredential = errors.New("unknown credential")

// KubeletNodeAuthorizationFunc decides whether the credential identified by
// credentialID may obtain a kubelet client certificate for nodeName. A false
// result must carry a human-readable denial reason. An error wrapping
// ErrUnknownCredential leaves the CSR undecided.
type KubeletNodeAuthorizationFunc func(ctx context.Context, csr *certificatesv1.CertificateSigningRequest, nodeName, credentialID string) (bool, string, error)

// KubeletClientCSRApproverOptions configures KubeletClientCSRApprover.
type KubeletClientCSRApproverOptions struct {
	// Requester is the exact CSR spec.username this approver handles. CSRs
	// from any other requester are ignored so other approvers keep working.
	Requester string

	// MaxExpirationSeconds is the maximum allowed spec.expirationSeconds value.
	// Defaults to 365 days when unset.
	MaxExpirationSeconds int32

	// AuthorizeNode binds the requester credential to the requested node.
	AuthorizeNode KubeletNodeAuthorizationFunc
}

// KubeletClientCSRApprover approves kubelet client CSRs submitted by a single
// requester identity, typically a ServiceAccount whose tokens are minted per
// Machine. Every handled CSR is either approved or denied, unless AuthorizeNode
// reports ErrUnknownCredential.
type KubeletClientCSRApprover struct {
	opts KubeletClientCSRApproverOptions
}

// NewKubeletClientCSRApprover validates options and returns an approver.
func NewKubeletClientCSRApprover(opts KubeletClientCSRApproverOptions) (*KubeletClientCSRApprover, error) {
	if strings.TrimSpace(opts.Requester) == "" {
		return nil, fmt.Errorf("requester is required")
	}

	if opts.MaxExpirationSeconds == 0 {
		opts.MaxExpirationSeconds = defaultMaxExpirationSeconds
	}

	if opts.MaxExpirationSeconds < 0 {
		return nil, fmt.Errorf("max expiration seconds must be non-negative")
	}

	if opts.AuthorizeNode == nil {
		return nil, fmt.Errorf("node authorization callback is required")
	}

	return &KubeletClientCSRApprover{opts: opts}, nil
}

// Evaluate returns the decision for csr.
func (a *KubeletClientCSRApprover) Evaluate(ctx context.Context, csr *certificatesv1.CertificateSigningRequest) (CSRDecision, error) {
	if csr.Spec.SignerName != KubeletClientSignerName || csr.Spec.Username != a.opts.Requester {
		return CSRDecision{Ignore: true}, nil
	}

	if HasApprovalDecision(csr) || len(csr.Status.Certificate) > 0 {
		return CSRDecision{AlreadyDecided: true}, nil
	}

	x509cr, err := parseCSR(csr.Spec.Request)
	if err != nil {
		return Deny("unable to parse CSR: %v", err), nil
	}

	if err := x509cr.CheckSignature(); err != nil {
		return Deny("CSR signature is invalid: %v", err), nil
	}

	nodeName, ok := strings.CutPrefix(x509cr.Subject.CommonName, SystemNodeUserPrefix)
	if !ok || nodeName == "" {
		return Deny("common name must be %s<nodeName>", SystemNodeUserPrefix), nil
	}

	if hasUnexpectedSubjectFields(x509cr.Subject) {
		return Deny("subject must contain only common name and organizations"), nil
	}

	if len(x509cr.Subject.Organization) != 1 || x509cr.Subject.Organization[0] != SystemNodesGroup {
		return Deny("organizations must be exactly %s", SystemNodesGroup), nil
	}

	if len(x509cr.DNSNames) > 0 || len(x509cr.EmailAddresses) > 0 || len(x509cr.IPAddresses) > 0 || len(x509cr.URIs) > 0 {
		return Deny("subject alternative names are not allowed"), nil
	}

	if len(x509cr.Extensions) > 0 || len(x509cr.ExtraExtensions) > 0 {
		return Deny("CSR extensions are not allowed"), nil
	}

	if err := validateClientAuthUsages(csr.Spec.Usages); err != nil {
		return Deny("invalid usages: %v", err), nil
	}

	if csr.Spec.ExpirationSeconds != nil && *csr.Spec.ExpirationSeconds > a.opts.MaxExpirationSeconds {
		return Deny("requested expiration %d exceeds maximum %d", *csr.Spec.ExpirationSeconds, a.opts.MaxExpirationSeconds), nil
	}

	for _, group := range csr.Spec.Groups {
		if _, ok := privilegedRequesterGroups[strings.ToLower(strings.TrimSpace(group))]; ok {
			return Deny("requester has forbidden group %q", group), nil
		}
	}

	credentialID, credentialValid := singleCredentialID(csr)
	if !credentialValid {
		return Deny("requester must carry exactly one non-empty %s", CredentialIDExtraKey), nil
	}

	allowed, reason, err := a.opts.AuthorizeNode(ctx, csr, nodeName, credentialID)
	if errors.Is(err, ErrUnknownCredential) {
		return CSRDecision{Ignore: true}, nil
	}

	if err != nil {
		return CSRDecision{}, err
	}

	if !allowed {
		return Deny("requester credential is not allowed to claim node %q: %s", nodeName, reason), nil
	}

	return Approve("approved kubelet client certificate for attested node %q", nodeName), nil
}

func singleCredentialID(csr *certificatesv1.CertificateSigningRequest) (string, bool) {
	credentialIDs := csr.Spec.Extra[CredentialIDExtraKey]
	if len(credentialIDs) != 1 || strings.TrimSpace(credentialIDs[0]) == "" {
		return "", false
	}

	return credentialIDs[0], true
}
