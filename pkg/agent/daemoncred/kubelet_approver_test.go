// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemoncred

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	testKubeletRequester    = "system:serviceaccount:unbounded-system:metalman-bootstrap"
	testKubeletCredentialID = "JTI=abc"
)

type kubeletAuthCall struct {
	nodeName     string
	credentialID string
}

func testKubeletApprover(t *testing.T, allowed bool, calls *[]kubeletAuthCall) *KubeletClientCSRApprover {
	t.Helper()

	a, err := NewKubeletClientCSRApprover(KubeletClientCSRApproverOptions{
		Requester: testKubeletRequester,
		AuthorizeNode: func(_ context.Context, _ *certificatesv1.CertificateSigningRequest, nodeName, credentialID string) (bool, string, error) {
			if calls != nil {
				*calls = append(*calls, kubeletAuthCall{nodeName: nodeName, credentialID: credentialID})
			}

			if !allowed {
				return false, "test denial", nil
			}

			return true, "", nil
		},
	})
	require.NoError(t, err)

	return a
}

func kubeletSpec() certificatesv1.CertificateSigningRequestSpec {
	return certificatesv1.CertificateSigningRequestSpec{
		SignerName: KubeletClientSignerName,
		Username:   testKubeletRequester,
		Groups:     []string{"system:serviceaccounts", "system:serviceaccounts:unbounded-system", "system:authenticated"},
		Usages:     []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageKeyEncipherment, certificatesv1.UsageClientAuth},
		Extra:      map[string]certificatesv1.ExtraValue{CredentialIDExtraKey: {testKubeletCredentialID}},
	}
}

func kubeletSubject(node string) csrSubject {
	return csrSubject{CommonName: SystemNodeUserPrefix + node, Organization: []string{SystemNodesGroup}}
}

func TestNewKubeletClientCSRApproverValidatesOptions(t *testing.T) {
	_, err := NewKubeletClientCSRApprover(KubeletClientCSRApproverOptions{
		AuthorizeNode: func(context.Context, *certificatesv1.CertificateSigningRequest, string, string) (bool, string, error) {
			return true, "", nil
		},
	})
	require.ErrorContains(t, err, "requester is required")

	_, err = NewKubeletClientCSRApprover(KubeletClientCSRApproverOptions{Requester: testKubeletRequester})
	require.ErrorContains(t, err, "callback is required")

	_, err = NewKubeletClientCSRApprover(KubeletClientCSRApproverOptions{
		Requester:            testKubeletRequester,
		MaxExpirationSeconds: -1,
		AuthorizeNode: func(context.Context, *certificatesv1.CertificateSigningRequest, string, string) (bool, string, error) {
			return true, "", nil
		},
	})
	require.ErrorContains(t, err, "non-negative")
}

func TestKubeletClientCSRApproverApproves(t *testing.T) {
	var calls []kubeletAuthCall

	a := testKubeletApprover(t, true, &calls)

	decision, err := a.Evaluate(context.Background(), csrFor(t, kubeletSpec(), kubeletSubject("node-a")))
	require.NoError(t, err)
	require.False(t, decision.Ignore)
	require.True(t, decision.Approve, decision.Message)
	require.Equal(t, []kubeletAuthCall{{nodeName: "node-a", credentialID: testKubeletCredentialID}}, calls)
}

func TestKubeletClientCSRApproverIgnores(t *testing.T) {
	for name, mutate := range map[string]func(*certificatesv1.CertificateSigningRequestSpec){
		"other requester": func(s *certificatesv1.CertificateSigningRequestSpec) { s.Username = "system:bootstrap:abc123" },
		"other SA": func(s *certificatesv1.CertificateSigningRequestSpec) {
			s.Username = "system:serviceaccount:default:metalman-bootstrap"
		},
		"other signer": func(s *certificatesv1.CertificateSigningRequestSpec) {
			s.SignerName = DefaultControllerCertificateSignerName
		},
		"kubelet serving csr": func(s *certificatesv1.CertificateSigningRequestSpec) {
			s.SignerName = certificatesv1.KubeletServingSignerName
		},
	} {
		t.Run(name, func(t *testing.T) {
			var calls []kubeletAuthCall

			spec := kubeletSpec()
			mutate(&spec)

			decision, err := testKubeletApprover(t, true, &calls).Evaluate(context.Background(), csrFor(t, spec, kubeletSubject("node-a")))
			require.NoError(t, err)
			require.True(t, decision.Ignore)
			require.Empty(t, calls)
		})
	}
}

func TestKubeletClientCSRApproverSkipsAlreadyDecided(t *testing.T) {
	for _, condType := range []certificatesv1.RequestConditionType{certificatesv1.CertificateApproved, certificatesv1.CertificateDenied, certificatesv1.CertificateFailed} {
		csr := csrFor(t, kubeletSpec(), kubeletSubject("node-a"))
		csr.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{Type: condType, Status: corev1.ConditionTrue}}

		decision, err := testKubeletApprover(t, true, nil).Evaluate(context.Background(), csr)
		require.NoError(t, err)
		require.True(t, decision.AlreadyDecided, condType)
	}

	csr := csrFor(t, kubeletSpec(), kubeletSubject("node-a"))
	csr.Status.Certificate = []byte("issued")

	decision, err := testKubeletApprover(t, true, nil).Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.True(t, decision.AlreadyDecided)
}

func TestKubeletClientCSRApproverDenies(t *testing.T) {
	maxExp := defaultMaxExpirationSeconds + 1
	okExp := int32(3600)

	tests := map[string]struct {
		spec    func(*certificatesv1.CertificateSigningRequestSpec)
		subject csrSubject
		raw     []byte
		want    string
	}{
		"malformed PEM":   {raw: []byte("nope"), want: "unable to parse CSR"},
		"wrong CN":        {subject: csrSubject{CommonName: "admin", Organization: []string{SystemNodesGroup}}, want: "common name must be"},
		"empty node name": {subject: csrSubject{CommonName: SystemNodeUserPrefix, Organization: []string{SystemNodesGroup}}, want: "common name must be"},
		"extra O":         {subject: csrSubject{CommonName: "system:node:node-a", Organization: []string{SystemNodesGroup, "system:masters"}}, want: "organizations must be exactly"},
		"missing O":       {subject: csrSubject{CommonName: "system:node:node-a"}, want: "organizations must be exactly"},
		"wrong O":         {subject: csrSubject{CommonName: "system:node:node-a", Organization: []string{"system:masters"}}, want: "organizations must be exactly"},
		"OU":              {subject: csrSubject{CommonName: "system:node:node-a", Organization: []string{SystemNodesGroup}, OrganizationalUnit: []string{"x"}}, want: "subject must contain only"},
		"DNS SAN":         {subject: csrSubject{CommonName: "system:node:node-a", Organization: []string{SystemNodesGroup}, DNSNames: []string{"node-a"}}, want: "subject alternative names"},
		"server auth usage": {spec: func(s *certificatesv1.CertificateSigningRequestSpec) {
			s.Usages = append(s.Usages, certificatesv1.UsageServerAuth)
		}, want: "invalid usages"},
		"no client auth": {spec: func(s *certificatesv1.CertificateSigningRequestSpec) {
			s.Usages = []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature}
		}, want: "invalid usages"},
		"expiration too long":  {spec: func(s *certificatesv1.CertificateSigningRequestSpec) { s.ExpirationSeconds = &maxExp }, want: "exceeds maximum"},
		"privileged group":     {spec: func(s *certificatesv1.CertificateSigningRequestSpec) { s.Groups = append(s.Groups, "system:masters") }, want: "forbidden group"},
		"missing credential":   {spec: func(s *certificatesv1.CertificateSigningRequestSpec) { s.Extra = nil }, want: "exactly one non-empty"},
		"empty credential":     {spec: func(s *certificatesv1.CertificateSigningRequestSpec) { s.Extra[CredentialIDExtraKey] = []string{" "} }, want: "exactly one non-empty"},
		"no credential values": {spec: func(s *certificatesv1.CertificateSigningRequestSpec) { s.Extra[CredentialIDExtraKey] = nil }, want: "exactly one non-empty"},
		"multiple credentials": {spec: func(s *certificatesv1.CertificateSigningRequestSpec) {
			s.Extra[CredentialIDExtraKey] = []string{"JTI=a", "JTI=b"}
		}, want: "exactly one non-empty"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var calls []kubeletAuthCall

			spec := kubeletSpec()
			spec.ExpirationSeconds = &okExp

			if tt.spec != nil {
				tt.spec(&spec)
			}

			subject := tt.subject
			if subject.CommonName == "" && subject.Organization == nil {
				subject = kubeletSubject("node-a")
			}

			csr := csrFor(t, spec, subject)
			if tt.raw != nil {
				csr.Spec.Request = tt.raw
			}

			decision, err := testKubeletApprover(t, true, &calls).Evaluate(context.Background(), csr)
			require.NoError(t, err)
			require.False(t, decision.Ignore)
			require.False(t, decision.AlreadyDecided)
			require.False(t, decision.Approve)
			require.Contains(t, decision.Message, tt.want)
			require.Empty(t, calls, "authorization must not run for rejected shapes")
		})
	}
}

func TestKubeletClientCSRApproverDeniesWhenAuthorizationFails(t *testing.T) {
	decision, err := testKubeletApprover(t, false, nil).Evaluate(context.Background(), csrFor(t, kubeletSpec(), kubeletSubject("node-a")))
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "test denial")
}

func TestKubeletClientCSRApproverPropagatesAuthorizationError(t *testing.T) {
	a, err := NewKubeletClientCSRApprover(KubeletClientCSRApproverOptions{
		Requester: testKubeletRequester,
		AuthorizeNode: func(context.Context, *certificatesv1.CertificateSigningRequest, string, string) (bool, string, error) {
			return false, "", errors.New("boom")
		},
	})
	require.NoError(t, err)

	_, err = a.Evaluate(context.Background(), csrFor(t, kubeletSpec(), kubeletSubject("node-a")))
	require.ErrorContains(t, err, "boom")
}
