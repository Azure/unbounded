// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

func attestedMachine(name, nodeRef, credentialID string) *unboundedv1alpha3.Machine {
	m := &unboundedv1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if nodeRef != "" {
		m.Spec.Kubernetes = &unboundedv1alpha3.KubernetesSpec{NodeRef: &unboundedv1alpha3.LocalObjectReference{Name: nodeRef}}
	}

	if credentialID != "" {
		m.Status.Bootstrap = &unboundedv1alpha3.BootstrapStatus{CredentialID: credentialID}
	}

	return m
}

func metalmanKubeletCSR(t *testing.T, username, node string, credentialIDs ...string) *certificatesv1.CertificateSigningRequest {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: daemoncred.SystemNodeUserPrefix + node, Organization: []string{daemoncred.SystemNodesGroup}},
	}, key)
	require.NoError(t, err)

	spec := certificatesv1.CertificateSigningRequestSpec{
		Request:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
		SignerName: certificatesv1.KubeAPIServerClientKubeletSignerName,
		Username:   username,
		Groups:     []string{"system:serviceaccounts", "system:authenticated"},
		Usages:     []certificatesv1.KeyUsage{certificatesv1.UsageDigitalSignature, certificatesv1.UsageClientAuth},
	}
	if credentialIDs != nil {
		spec.Extra = map[string]certificatesv1.ExtraValue{daemoncred.CredentialIDExtraKey: credentialIDs}
	}

	return &certificatesv1.CertificateSigningRequest{ObjectMeta: metav1.ObjectMeta{Name: "csr-" + node}, Spec: spec}
}

func testMetalmanApprover(t *testing.T, objs ...client.Object) *daemoncred.CSRApproverReconciler {
	t.Helper()

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&unboundedv1alpha3.Machine{}, machineBootstrapCredentialIDField, machineBootstrapCredentialID).
		Build()

	r, err := NewMetalmanCSRApprover(c, kubefake.NewSimpleClientset())
	require.NoError(t, err)
	require.Equal(t, "metalman-csr-approver", r.Name)

	return r
}

func TestMetalmanBootstrapUsername(t *testing.T) {
	require.Equal(t, "system:serviceaccount:unbounded-system:metalman-bootstrap", metalmanBootstrapUsername())
}

func TestMachineBootstrapCredentialIDIndex(t *testing.T) {
	require.Equal(t, []string{"JTI=a"}, machineBootstrapCredentialID(attestedMachine("m", "", "JTI=a")))
	require.Nil(t, machineBootstrapCredentialID(attestedMachine("m", "", "")))
	require.Nil(t, machineBootstrapCredentialID(&unboundedv1alpha3.MachineConfiguration{}))
}

func TestMetalmanCSRApprover(t *testing.T) {
	user := metalmanBootstrapUsername()

	tests := map[string]struct {
		objs     []client.Object
		csr      func(t *testing.T) *certificatesv1.CertificateSigningRequest
		ignore   bool
		approve  bool
		contains string
	}{
		"approves machine name": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return metalmanKubeletCSR(t, user, "node-a", "JTI=a")
			},
			approve: true,
		},
		"approves nodeRef override": {
			objs: []client.Object{attestedMachine("machine-a", "node-x", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return metalmanKubeletCSR(t, user, "node-x", "JTI=a")
			},
			approve: true,
		},
		"denies machine name when nodeRef overrides": {
			objs: []client.Object{attestedMachine("machine-a", "node-x", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return metalmanKubeletCSR(t, user, "machine-a", "JTI=a")
			},
			contains: `issued to Machine "machine-a"`,
		},
		"denies other node": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a"), attestedMachine("node-b", "", "JTI=b")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return metalmanKubeletCSR(t, user, "node-b", "JTI=a")
			},
			contains: `issued to Machine "node-a"`,
		},
		"denies no matching machine": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return metalmanKubeletCSR(t, user, "node-a", "JTI=stale")
			},
			contains: "no Machine was issued",
		},
		"denies machine without bootstrap status": {
			objs: []client.Object{attestedMachine("node-a", "", "")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return metalmanKubeletCSR(t, user, "node-a", "JTI=a")
			},
			contains: "no Machine was issued",
		},
		"denies two matching machines": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a"), attestedMachine("node-b", "", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return metalmanKubeletCSR(t, user, "node-a", "JTI=a")
			},
			contains: "recorded on 2 Machines",
		},
		"denies missing credential id": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return metalmanKubeletCSR(t, user, "node-a")
			},
			contains: "exactly one non-empty",
		},
		"denies multiple credential ids": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return metalmanKubeletCSR(t, user, "node-a", "JTI=a", "JTI=b")
			},
			contains: "exactly one non-empty",
		},
		"ignores other requester": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return metalmanKubeletCSR(t, "system:bootstrap:abc123", "node-a", "JTI=a")
			},
			ignore: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := testMetalmanApprover(t, tt.objs...)

			decision, err := r.Approver.Evaluate(context.Background(), tt.csr(t))
			require.NoError(t, err)
			require.Equal(t, tt.ignore, decision.Ignore)

			if tt.ignore {
				return
			}

			require.Equal(t, tt.approve, decision.Approve, decision.Message)
			require.Contains(t, decision.Message, tt.contains)
		})
	}
}

func TestMetalmanCSRApproverReconcileWritesCondition(t *testing.T) {
	for name, tc := range map[string]struct {
		credentialID string
		want         certificatesv1.RequestConditionType
	}{
		"approve": {credentialID: "JTI=a", want: certificatesv1.CertificateApproved},
		"deny":    {credentialID: "JTI=other", want: certificatesv1.CertificateDenied},
	} {
		t.Run(name, func(t *testing.T) {
			csr := metalmanKubeletCSR(t, metalmanBootstrapUsername(), "node-a", tc.credentialID)
			kube := kubefake.NewSimpleClientset(csr.DeepCopy())
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(attestedMachine("node-a", "", "JTI=a"), csr).
				WithIndex(&unboundedv1alpha3.Machine{}, machineBootstrapCredentialIDField, machineBootstrapCredentialID).
				Build()

			r, err := NewMetalmanCSRApprover(c, kube)
			require.NoError(t, err)

			_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: csr.Name}})
			require.NoError(t, err)

			got, err := kube.CertificatesV1().CertificateSigningRequests().Get(context.Background(), csr.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Len(t, got.Status.Conditions, 1)
			require.Equal(t, tc.want, got.Status.Conditions[0].Type)
			require.Equal(t, corev1.ConditionTrue, got.Status.Conditions[0].Status)
		})
	}
}
