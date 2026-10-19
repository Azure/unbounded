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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

	r, err := NewMetalmanCSRApprover(c, c, kubefake.NewSimpleClientset())
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

			r, err := NewMetalmanCSRApprover(c, c, kube)
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

func TestMetalmanCSRApproverRetriesYoungCSRWithoutMachine(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&unboundedv1alpha3.Machine{}, machineBootstrapCredentialIDField, machineBootstrapCredentialID).
		Build()
	checker := &metalmanCSRClaimChecker{Client: c, apiReader: c, clock: func() time.Time { return now }}

	csr := metalmanKubeletCSR(t, metalmanBootstrapUsername(), "node-a", "JTI=a")

	csr.CreationTimestamp = metav1.NewTime(now.Add(-metalmanCSRCacheGrace + time.Second))
	_, _, err := checker.credentialMayClaimNode(context.Background(), csr, "node-a", "JTI=a")
	require.ErrorContains(t, err, "retrying")

	csr.CreationTimestamp = metav1.NewTime(now.Add(-metalmanCSRCacheGrace))
	allowed, reason, err := checker.credentialMayClaimNode(context.Background(), csr, "node-a", "JTI=a")
	require.NoError(t, err)
	require.False(t, allowed)
	require.Contains(t, reason, "no Machine was issued")
}

func TestMetalmanCSRApproverReconcileRequeuesYoungCSR(t *testing.T) {
	csr := metalmanKubeletCSR(t, metalmanBootstrapUsername(), "node-a", "JTI=a")
	csr.CreationTimestamp = metav1.Now()
	kube := kubefake.NewSimpleClientset(csr.DeepCopy())
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(csr).
		WithIndex(&unboundedv1alpha3.Machine{}, machineBootstrapCredentialIDField, machineBootstrapCredentialID).
		Build()

	r, err := NewMetalmanCSRApprover(c, c, kube)
	require.NoError(t, err)

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: csr.Name}})
	require.Error(t, err)

	got, err := kube.CertificatesV1().CertificateSigningRequests().Get(context.Background(), csr.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, got.Status.Conditions)
}

func TestMetalmanCSRApproverConfirmsLiveMachine(t *testing.T) {
	user := metalmanBootstrapUsername()

	withUID := func(m *unboundedv1alpha3.Machine, uid types.UID) *unboundedv1alpha3.Machine {
		m.UID = uid
		return m
	}

	cached := func() *unboundedv1alpha3.Machine { return withUID(attestedMachine("node-a", "", "JTI=a"), "uid-1") }

	tests := map[string]struct {
		live     []client.Object
		approve  bool
		contains string
	}{
		"approves when live matches": {
			live:    []client.Object{cached()},
			approve: true,
		},
		"denies superseded credential": {
			live:     []client.Object{withUID(attestedMachine("node-a", "", "JTI=b"), "uid-1")},
			contains: "superseded",
		},
		"denies cleared bootstrap status": {
			live:     []client.Object{withUID(attestedMachine("node-a", "", ""), "uid-1")},
			contains: "superseded",
		},
		"denies deleted machine": {
			contains: "no longer exists",
		},
		"denies recreated machine": {
			live:     []client.Object{withUID(attestedMachine("node-a", "", "JTI=a"), "uid-2")},
			contains: "recreated",
		},
		"denies changed node name": {
			live:     []client.Object{withUID(attestedMachine("node-a", "node-z", "JTI=a"), "uid-1")},
			contains: `issued to Machine "node-a"`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cache := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(cached()).
				WithIndex(&unboundedv1alpha3.Machine{}, machineBootstrapCredentialIDField, machineBootstrapCredentialID).
				Build()
			live := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.live...).Build()

			r, err := NewMetalmanCSRApprover(cache, live, kubefake.NewSimpleClientset())
			require.NoError(t, err)

			decision, err := r.Approver.Evaluate(context.Background(), metalmanKubeletCSR(t, user, "node-a", "JTI=a"))
			require.NoError(t, err)
			require.False(t, decision.Ignore)
			require.Equal(t, tt.approve, decision.Approve, decision.Message)
			require.Contains(t, decision.Message, tt.contains)
		})
	}
}

func TestMetalmanCSRApproverLiveReadErrorRetries(t *testing.T) {
	cache := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(attestedMachine("node-a", "", "JTI=a")).
		WithIndex(&unboundedv1alpha3.Machine{}, machineBootstrapCredentialIDField, machineBootstrapCredentialID).
		Build()
	live := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("boom")
		},
	}).Build()
	checker := &metalmanCSRClaimChecker{Client: cache, apiReader: live}

	csr := metalmanKubeletCSR(t, metalmanBootstrapUsername(), "node-a", "JTI=a")
	allowed, _, err := checker.credentialMayClaimNode(context.Background(), csr, "node-a", "JTI=a")
	require.ErrorContains(t, err, "boom")
	require.False(t, allowed)
}

func TestNewMetalmanCSRApproverRequiresAPIReader(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	_, err := NewMetalmanCSRApprover(c, nil, kubefake.NewSimpleClientset())
	require.Error(t, err)
}
