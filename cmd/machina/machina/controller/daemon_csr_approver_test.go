// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDaemonCSREvaluate_BootstrapTokenAllowed(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		bootstrapToken("abc123", "site-a"),
		machineForToken("machine-a", "node-a", "abc123", "site-a"),
	).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testDaemonCSRSignerName,
		Username:   "system:bootstrap:abc123",
		Groups:     []string{testDaemonCSRBootstrapGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonCSRDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.True(t, decision.Approve)
	require.False(t, decision.Ignore)
	require.Contains(t, decision.Message, "node-a")
}

func TestDaemonCSREvaluate_BootstrapTokenAllowedBeforeMachineExists(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		bootstrapToken("abc123", "site-a"),
	).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testDaemonCSRSignerName,
		Username:   "system:bootstrap:abc123",
		Groups:     []string{testDaemonCSRBootstrapGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonCSRDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.True(t, decision.Approve)
	require.Contains(t, decision.Message, "node-a")
}

func TestDaemonCSREvaluate_BootstrapTokenRequiresDaemonGroup(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		bootstrapToken("abc123", "site-a"),
		machineForToken("machine-a", "node-a", "abc123", "site-a"),
	).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testDaemonCSRSignerName,
		Username:   "system:bootstrap:abc123",
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonCSRDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "missing required group")
}

func TestDaemonCSREvaluate_BootstrapTokenRequiresSiteBinding(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		bootstrapToken("abc123", ""),
		machineForToken("machine-a", "node-a", "abc123", "site-a"),
	).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testDaemonCSRSignerName,
		Username:   "system:bootstrap:abc123",
		Groups:     []string{testDaemonCSRBootstrapGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonCSRDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "not allowed")
}

func TestDaemonCSREvaluate_BootstrapTokenRejectsWrongNode(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		bootstrapToken("abc123", "site-a"),
		machineForToken("machine-a", "node-a", "abc123", "site-a"),
	).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testDaemonCSRSignerName,
		Username:   "system:bootstrap:abc123",
		Groups:     []string{testDaemonCSRBootstrapGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-b",
		Organization: []string{systemNodesGroup, testDaemonCSRDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "not allowed")
}

func TestDaemonCSREvaluate_BootstrapTokenRejectsWrongSite(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		bootstrapToken("abc123", "site-a"),
		machineForToken("machine-a", "node-a", "abc123", "site-b"),
	).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testDaemonCSRSignerName,
		Username:   "system:bootstrap:abc123",
		Groups:     []string{testDaemonCSRBootstrapGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonCSRDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "not allowed")
}

func TestDaemonCSREvaluate_RenewalAllowedForSameIdentity(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		machineForToken("machine-a", "node-a", "abc123", "site-a"),
	).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testDaemonCSRSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{systemNodesGroup, testDaemonCSRDaemonGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonCSRDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.True(t, decision.Approve)
	require.Contains(t, decision.Message, "renewal")
}

func TestDaemonCSREvaluate_StockKubeletCertCannotRenew(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testDaemonCSRSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{systemNodesGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonCSRDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "missing required node or daemon group")
}

func TestDaemonCSREvaluate_RejectsUnexpectedGroup(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testDaemonCSRSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{systemNodesGroup, testDaemonCSRDaemonGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonCSRDaemonGroup, "system:masters"},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "organizations")
}

func TestDaemonCSREvaluate_RejectsSANs(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testDaemonCSRSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{systemNodesGroup, testDaemonCSRDaemonGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonCSRDaemonGroup},
		DNSNames:     []string{"node-a.example.test"},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "subject alternative names")
}

func TestDaemonCSREvaluate_RejectsUnexpectedSubjectFields(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: testDaemonCSRSignerName,
		Username:   "system:node:node-a",
		Groups:     []string{systemNodesGroup, testDaemonCSRDaemonGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:         "system:node:node-a",
		Organization:       []string{systemNodesGroup, testDaemonCSRDaemonGroup},
		OrganizationalUnit: []string{"unexpected"},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "subject")
}

func TestDaemonCSREvaluate_RejectsTooLongExpiration(t *testing.T) {
	evaluator := testDaemonCSREvaluator()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	expiration := maxDaemonCertificateExpirationSeconds + 1
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName:        testDaemonCSRSignerName,
		Username:          "system:node:node-a",
		Groups:            []string{systemNodesGroup, testDaemonCSRDaemonGroup},
		Usages:            clientAuthUsages(),
		ExpirationSeconds: &expiration,
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{systemNodesGroup, testDaemonCSRDaemonGroup},
	})

	decision, err := evaluator.Evaluate(context.Background(), c, csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "exceeds maximum")
}

const (
	testDaemonCSRSignerName     = "kubernetes.io/kube-apiserver-client"
	testDaemonCSRDaemonGroup    = "unbounded-agent-daemons"
	testDaemonCSRBootstrapGroup = "system:bootstrappers:unbounded-agent-daemons"
)

type csrSubject struct {
	CommonName         string
	Organization       []string
	OrganizationalUnit []string
	DNSNames           []string
}

func testDaemonCSREvaluator() daemonCSREvaluator {
	return daemonCSREvaluator{
		SignerName:     testDaemonCSRSignerName,
		DaemonGroup:    testDaemonCSRDaemonGroup,
		BootstrapGroup: testDaemonCSRBootstrapGroup,
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

func bootstrapToken(tokenID string, site string) *corev1.Secret {
	labels := map[string]string{}
	if site != "" {
		labels[unboundedv1alpha3.MachineSiteLabelKey] = site
	}

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-" + tokenID,
			Namespace: metav1.NamespaceSystem,
			Labels:    labels,
		},
		Type: corev1.SecretType("bootstrap.kubernetes.io/token"),
	}
}

func machineForToken(machineName, nodeName, tokenID, site string) *unboundedv1alpha3.Machine {
	labels := map[string]string{}
	if site != "" {
		labels[unboundedv1alpha3.MachineSiteLabelKey] = site
	}

	return &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   machineName,
			Labels: labels,
		},
		Spec: unboundedv1alpha3.MachineSpec{
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				NodeRef: &unboundedv1alpha3.LocalObjectReference{Name: nodeName},
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{
					Name: "bootstrap-token-" + tokenID,
				},
			},
		},
	}
}
