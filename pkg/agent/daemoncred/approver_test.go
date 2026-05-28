// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemoncred

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCSRApproverBootstrapAllowed(t *testing.T) {
	approver := testCSRApprover(t, true, true)
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: DefaultControllerCertificateSignerName,
		Username:   "system:bootstrap:abc123",
		Groups:     []string{testBootstrapGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{SystemNodesGroup, testDaemonGroup},
	})

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.True(t, decision.Approve)
}

func TestCSRApproverRequiresDaemonGroup(t *testing.T) {
	_, err := NewCSRApprover(CSRApproverOptions{
		BootstrapGroup: testBootstrapGroup,
		AuthorizeBootstrap: func(context.Context, *certificatesv1.CertificateSigningRequest, string) (bool, error) {
			return true, nil
		},
		AuthorizeRenewal: func(context.Context, *certificatesv1.CertificateSigningRequest, string) (bool, error) {
			return true, nil
		},
	})
	require.ErrorContains(t, err, "daemon group is required")
}

func TestCSRApproverBootstrapRequiresGroup(t *testing.T) {
	approver := testCSRApprover(t, true, true)
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: DefaultControllerCertificateSignerName,
		Username:   "system:bootstrap:abc123",
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{SystemNodesGroup, testDaemonGroup},
	})

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "missing required group")
}

func TestCSRApproverBootstrapRejectsForbiddenRequesterGroup(t *testing.T) {
	for _, group := range []string{"system:masters", "cluster-admin"} {
		t.Run(group, func(t *testing.T) {
			approver := testCSRApprover(t, true, true)
			csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
				SignerName: DefaultControllerCertificateSignerName,
				Username:   "system:bootstrap:abc123",
				Groups:     []string{testBootstrapGroup, group},
				Usages:     clientAuthUsages(),
			}, csrSubject{
				CommonName:   "system:node:node-a",
				Organization: []string{SystemNodesGroup, testDaemonGroup},
			})

			decision, err := approver.Evaluate(context.Background(), csr)
			require.NoError(t, err)
			require.False(t, decision.Approve)
			require.Contains(t, decision.Message, "forbidden group")
			require.Contains(t, decision.Message, group)
		})
	}
}

func TestCSRApproverRenewalAllowed(t *testing.T) {
	approver := testCSRApprover(t, true, true)
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: DefaultControllerCertificateSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{SystemNodesGroup, testDaemonGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{SystemNodesGroup, testDaemonGroup},
	})

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.True(t, decision.Approve)
}

func TestCSRApproverStockKubeletCertCannotRenew(t *testing.T) {
	approver := testCSRApprover(t, true, true)
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: DefaultControllerCertificateSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{SystemNodesGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{SystemNodesGroup, testDaemonGroup},
	})

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "missing required node or daemon group")
}

func TestCSRApproverRenewalRejectsForbiddenRequesterGroup(t *testing.T) {
	for _, group := range []string{"system:masters", "cluster-admin"} {
		t.Run(group, func(t *testing.T) {
			approver := testCSRApprover(t, true, true)
			csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
				SignerName: DefaultControllerCertificateSignerName,
				Username:   "system:node:node-a",
				Groups:     []string{SystemNodesGroup, testDaemonGroup, group},
				Usages:     clientAuthUsages(),
			}, csrSubject{
				CommonName:   "system:node:node-a",
				Organization: []string{SystemNodesGroup, testDaemonGroup},
			})

			decision, err := approver.Evaluate(context.Background(), csr)
			require.NoError(t, err)
			require.False(t, decision.Approve)
			require.Contains(t, decision.Message, "forbidden group")
			require.Contains(t, decision.Message, group)
		})
	}
}

func TestCSRApproverRejectsUnexpectedGroup(t *testing.T) {
	approver := testCSRApprover(t, true, true)
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: DefaultControllerCertificateSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{SystemNodesGroup, testDaemonGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{SystemNodesGroup, testDaemonGroup, "system:masters"},
	})

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "organizations")
}

func TestCSRApproverRejectsSANs(t *testing.T) {
	approver := testCSRApprover(t, true, true)
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: DefaultControllerCertificateSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{SystemNodesGroup, testDaemonGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{SystemNodesGroup, testDaemonGroup},
		DNSNames:     []string{"node-a.example.test"},
	})

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "subject alternative names")
}

func TestCSRApproverRejectsUnexpectedSubjectFields(t *testing.T) {
	approver := testCSRApprover(t, true, true)
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: DefaultControllerCertificateSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{SystemNodesGroup, testDaemonGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:         "system:node:node-a",
		Organization:       []string{SystemNodesGroup, testDaemonGroup},
		OrganizationalUnit: []string{"unexpected"},
	})

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "subject")
}

func TestCSRApproverRejectsTooLongExpiration(t *testing.T) {
	approver := testCSRApprover(t, true, true)
	expiration := defaultMaxExpirationSeconds + 1
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName:        DefaultControllerCertificateSignerName,
		Username:          "system:node:node-a",
		Groups:            []string{SystemNodesGroup, testDaemonGroup},
		Usages:            clientAuthUsages(),
		ExpirationSeconds: &expiration,
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{SystemNodesGroup, testDaemonGroup},
	})

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "exceeds maximum")
}

type csrSubject struct {
	CommonName         string
	Organization       []string
	OrganizationalUnit []string
	DNSNames           []string
}

func testCSRApprover(t *testing.T, bootstrapAllowed bool, renewalAllowed bool) *CSRApprover {
	t.Helper()

	approver, err := NewCSRApprover(CSRApproverOptions{
		BootstrapGroup: testBootstrapGroup,
		DaemonGroup:    testDaemonGroup,
		AuthorizeBootstrap: func(context.Context, *certificatesv1.CertificateSigningRequest, string) (bool, error) {
			return bootstrapAllowed, nil
		},
		AuthorizeRenewal: func(context.Context, *certificatesv1.CertificateSigningRequest, string) (bool, error) {
			return renewalAllowed, nil
		},
	})
	require.NoError(t, err)

	return approver
}

const testBootstrapGroup = "system:bootstrappers:unbounded-agent-daemons"

func clientAuthUsages() []certificatesv1.KeyUsage {
	return []certificatesv1.KeyUsage{
		certificatesv1.UsageDigitalSignature,
		certificatesv1.UsageClientAuth,
	}
}

func csrFor(t *testing.T, spec certificatesv1.CertificateSigningRequestSpec, subject csrSubject) *certificatesv1.CertificateSigningRequest {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	requestDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:         subject.CommonName,
			Organization:       subject.Organization,
			OrganizationalUnit: subject.OrganizationalUnit,
		},
		DNSNames: subject.DNSNames,
	}, key)
	require.NoError(t, err)

	spec.Request = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: requestDER})

	return &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-csr"},
		Spec:       spec,
	}
}
