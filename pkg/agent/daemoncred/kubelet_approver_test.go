// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemoncred

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
)

func TestKubeletCSRApproverAllowsBoundNode(t *testing.T) {
	approver := testKubeletCSRApprover(t, true)
	csr := kubeletCSRFor(t, "node-a")

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.True(t, decision.Approve)
}

func TestKubeletCSRApproverRejectsUnboundNode(t *testing.T) {
	approver := testKubeletCSRApprover(t, false)
	csr := kubeletCSRFor(t, "node-b")

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "not allowed to claim")
}

func TestKubeletCSRApproverRejectsDaemonOrganization(t *testing.T) {
	approver := testKubeletCSRApprover(t, true)
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: certificatesv1.KubeAPIServerClientKubeletSignerName,
		Username:   "system:bootstrap:abc123",
		Groups:     []string{testBootstrapGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{SystemNodesGroup, testDaemonGroup},
	})

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "organizations")
}

func TestKubeletCSRApproverIgnoresNonBootstrapRequester(t *testing.T) {
	approver := testKubeletCSRApprover(t, true)
	csr := kubeletCSRFor(t, "node-a")
	csr.Spec.Username = "system:node:node-a"
	csr.Spec.Groups = []string{SystemNodesGroup}

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.True(t, decision.Ignore)
}

func TestKubeletCSRApproverIgnoresWrongSigner(t *testing.T) {
	approver := testKubeletCSRApprover(t, true)
	csr := kubeletCSRFor(t, "node-a")
	csr.Spec.SignerName = certificatesv1.KubeAPIServerClientSignerName

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.True(t, decision.Ignore)
}

func TestKubeletCSRApproverRejectsSANs(t *testing.T) {
	approver := testKubeletCSRApprover(t, true)
	csr := csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: certificatesv1.KubeAPIServerClientKubeletSignerName,
		Username:   "system:bootstrap:abc123",
		Groups:     []string{testBootstrapGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   "system:node:node-a",
		Organization: []string{SystemNodesGroup},
		DNSNames:     []string{"node-a.example.test"},
	})

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "subject alternative names")
}

func TestKubeletCSRApproverRejectsInvalidUsages(t *testing.T) {
	approver := testKubeletCSRApprover(t, true)
	csr := kubeletCSRFor(t, "node-a")
	csr.Spec.Usages = []certificatesv1.KeyUsage{certificatesv1.UsageServerAuth}

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.False(t, decision.Approve)
	require.Contains(t, decision.Message, "invalid usages")
}

func testKubeletCSRApprover(t *testing.T, allowed bool) *KubeletCSRApprover {
	t.Helper()

	approver, err := NewKubeletCSRApprover(KubeletCSRApproverOptions{
		BootstrapGroup: testBootstrapGroup,
		AuthorizeBootstrap: func(context.Context, *certificatesv1.CertificateSigningRequest, string) (bool, error) {
			return allowed, nil
		},
	})
	require.NoError(t, err)

	return approver
}

func kubeletCSRFor(t *testing.T, nodeName string) *certificatesv1.CertificateSigningRequest {
	t.Helper()

	return csrFor(t, certificatesv1.CertificateSigningRequestSpec{
		SignerName: certificatesv1.KubeAPIServerClientKubeletSignerName,
		Username:   "system:bootstrap:abc123",
		Groups:     []string{testBootstrapGroup},
		Usages:     clientAuthUsages(),
	}, csrSubject{
		CommonName:   SystemNodeUserPrefix + nodeName,
		Organization: []string{SystemNodesGroup},
	})
}
