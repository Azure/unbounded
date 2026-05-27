// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEvaluate_BootstrapTokenAllowed(t *testing.T) {
	evaluator := testEvaluator()
	c := fake.NewClientBuilder().WithScheme(runtimeScheme()).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testSignerName,
		Username:   "system:bootstrap:abc123",
		Groups:     []string{testDaemonGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.True(t, decision.Approve)
	require.False(t, decision.Ignore)
	require.Contains(t, decision.Message, "node-a")
}

func TestEvaluate_BootstrapTokenRequiresDaemonGroup(t *testing.T) {
	evaluator := testEvaluator()
	c := fake.NewClientBuilder().WithScheme(runtimeScheme()).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testSignerName,
		Username:   "system:bootstrap:abc123",
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "missing required group")
}

func TestEvaluate_RenewalAllowedForSameIdentity(t *testing.T) {
	evaluator := testEvaluator()
	c := fake.NewClientBuilder().WithScheme(runtimeScheme()).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{systemNodesGroup, testDaemonGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.True(t, decision.Approve)
	require.Contains(t, decision.Message, "renewal")
}

func TestEvaluate_StockKubeletCertCannotRenew(t *testing.T) {
	evaluator := testEvaluator()
	c := fake.NewClientBuilder().WithScheme(runtimeScheme()).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{systemNodesGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "neither")
}

func TestEvaluate_RejectsUnexpectedGroup(t *testing.T) {
	evaluator := testEvaluator()
	c := fake.NewClientBuilder().WithScheme(runtimeScheme()).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{systemNodesGroup, testDaemonGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonGroup, "system:masters"},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "organizations")
}

const (
	testSignerName  = "kubernetes.io/kube-apiserver-client"
	testDaemonGroup = "unbounded-agent-daemons"
)

type csrSubject struct {
	CommonName   string
	Organization []string
}

func testEvaluator() csrEvaluator {
	return csrEvaluator{
		SignerName:  testSignerName,
		DaemonGroup: testDaemonGroup,
	}
}

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
			CommonName:   subject.CommonName,
			Organization: subject.Organization,
		},
	}, key)
	require.NoError(t, err)

	spec.Request = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: requestDER})

	return &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-csr"},
		Spec:       spec,
	}
}
