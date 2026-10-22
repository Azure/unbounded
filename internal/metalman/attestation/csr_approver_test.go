// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package attestation

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

	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/indexing"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

func csrTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	utilruntime.Must(v1alpha3.AddToScheme(s))
	utilruntime.Must(certificatesv1.AddToScheme(s))

	return s
}

// csrTestCache returns a fake cached client that indexes Machines by
// bootstrap credential ID, as the metalman manager cache does.
func csrTestCache(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()

	return fakecr.NewClientBuilder().
		WithScheme(csrTestScheme(t)).
		WithObjects(objs...).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexMachineByBootstrapCredentialID, indexing.IndexMachineByBootstrapCredentialIDFunc).
		Build()
}

func attestedMachine(name, nodeRef, credentialID string) *v1alpha3.Machine {
	m := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if nodeRef != "" {
		m.Spec.Kubernetes = &v1alpha3.KubernetesSpec{NodeRef: &v1alpha3.LocalObjectReference{Name: nodeRef}}
	}

	if credentialID != "" {
		m.Status.Bootstrap = &v1alpha3.BootstrapStatus{CredentialID: credentialID, NodeName: m.NodeName()}
	}

	return m
}

func bootstrapKubeletCSR(t *testing.T, username, node string, credentialIDs ...string) *certificatesv1.CertificateSigningRequest {
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

func testCSRApprover(t *testing.T, cache client.Client, live client.Reader, kube *kubefake.Clientset) *daemoncred.CSRApproverReconciler {
	t.Helper()

	if kube == nil {
		kube = kubefake.NewClientset()
	}

	r, err := (&CSRApprover{Client: cache, APIReader: live, KubeClient: kube}).reconciler()
	require.NoError(t, err)

	return r
}

func TestBootstrapUsername(t *testing.T) {
	require.Equal(t, "system:serviceaccount:unbounded-system:metalman-bootstrap", BootstrapUsername())
}

func TestIndexMachineByBootstrapCredentialID(t *testing.T) {
	require.Equal(t, []string{"JTI=a"}, indexing.IndexMachineByBootstrapCredentialIDFunc(attestedMachine("m", "", "JTI=a")))
	require.Nil(t, indexing.IndexMachineByBootstrapCredentialIDFunc(attestedMachine("m", "", "")))
}

func TestCSRApprover(t *testing.T) {
	user := BootstrapUsername()

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
				return bootstrapKubeletCSR(t, user, "node-a", "JTI=a")
			},
			approve: true,
		},
		"approves nodeRef override": {
			objs: []client.Object{attestedMachine("machine-a", "node-x", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return bootstrapKubeletCSR(t, user, "node-x", "JTI=a")
			},
			approve: true,
		},
		"denies machine name when nodeRef overrides": {
			objs: []client.Object{attestedMachine("machine-a", "node-x", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return bootstrapKubeletCSR(t, user, "machine-a", "JTI=a")
			},
			contains: `issued to Machine "machine-a"`,
		},
		"denies other node": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a"), attestedMachine("node-b", "", "JTI=b")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return bootstrapKubeletCSR(t, user, "node-b", "JTI=a")
			},
			contains: `issued to Machine "node-a"`,
		},
		// A credential this Site has no record of may belong to another
		// Site's metalman, or the cache may not have observed it yet.
		"ignores credential without machine": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return bootstrapKubeletCSR(t, user, "node-a", "JTI=other-site")
			},
			ignore: true,
		},
		"ignores machine without bootstrap status": {
			objs: []client.Object{attestedMachine("node-a", "", "")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return bootstrapKubeletCSR(t, user, "node-a", "JTI=a")
			},
			ignore: true,
		},
		"denies two matching machines": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a"), attestedMachine("node-b", "", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return bootstrapKubeletCSR(t, user, "node-a", "JTI=a")
			},
			contains: "recorded on 2 Machines",
		},
		"denies missing credential id": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return bootstrapKubeletCSR(t, user, "node-a")
			},
			contains: "exactly one non-empty",
		},
		"denies multiple credential ids": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return bootstrapKubeletCSR(t, user, "node-a", "JTI=a", "JTI=b")
			},
			contains: "exactly one non-empty",
		},
		"ignores other requester": {
			objs: []client.Object{attestedMachine("node-a", "", "JTI=a")},
			csr: func(t *testing.T) *certificatesv1.CertificateSigningRequest {
				return bootstrapKubeletCSR(t, "system:bootstrap:abc123", "node-a", "JTI=a")
			},
			ignore: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			c := csrTestCache(t, tt.objs...)
			r := testCSRApprover(t, c, c, nil)

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

func TestCSRApproverReconcileWritesCondition(t *testing.T) {
	for name, tc := range map[string]struct {
		node string
		want []certificatesv1.RequestConditionType
	}{
		"approve": {node: "node-a", want: []certificatesv1.RequestConditionType{certificatesv1.CertificateApproved}},
		"deny":    {node: "node-b", want: []certificatesv1.RequestConditionType{certificatesv1.CertificateDenied}},
	} {
		t.Run(name, func(t *testing.T) {
			csr := bootstrapKubeletCSR(t, BootstrapUsername(), tc.node, "JTI=a")
			kube := kubefake.NewClientset(csr.DeepCopy())
			c := csrTestCache(t, attestedMachine("node-a", "", "JTI=a"), csr)

			_, err := testCSRApprover(t, c, c, kube).Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: csr.Name}})
			require.NoError(t, err)

			got, err := kube.CertificatesV1().CertificateSigningRequests().Get(context.Background(), csr.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Len(t, got.Status.Conditions, len(tc.want))
			require.Equal(t, tc.want[0], got.Status.Conditions[0].Type)
			require.Equal(t, corev1.ConditionTrue, got.Status.Conditions[0].Status)
		})
	}
}

// Another Site's metalman, or this one before its cache observes the Machine,
// must leave the CSR undecided: a denial is terminal.
func TestCSRApproverReconcileLeavesUnknownCredentialUndecided(t *testing.T) {
	csr := bootstrapKubeletCSR(t, BootstrapUsername(), "node-a", "JTI=a")
	kube := kubefake.NewClientset(csr.DeepCopy())
	c := csrTestCache(t, csr)

	_, err := testCSRApprover(t, c, c, kube).Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: csr.Name}})
	require.NoError(t, err)

	got, err := kube.CertificatesV1().CertificateSigningRequests().Get(context.Background(), csr.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, got.Status.Conditions)
}

func TestCSRApproverConfirmsLiveMachine(t *testing.T) {
	user := BootstrapUsername()

	withUID := func(m *v1alpha3.Machine, uid types.UID) *v1alpha3.Machine {
		m.UID = uid
		return m
	}

	cached := func() *v1alpha3.Machine { return withUID(attestedMachine("node-a", "", "JTI=a"), "uid-1") }

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
			live := fakecr.NewClientBuilder().WithScheme(csrTestScheme(t)).WithObjects(tt.live...).Build()
			r := testCSRApprover(t, csrTestCache(t, cached()), live, nil)

			decision, err := r.Approver.Evaluate(context.Background(), bootstrapKubeletCSR(t, user, "node-a", "JTI=a"))
			require.NoError(t, err)
			require.False(t, decision.Ignore)
			require.Equal(t, tt.approve, decision.Approve, decision.Message)
			require.Contains(t, decision.Message, tt.contains)
		})
	}
}

// The approver matches the node name recorded with the credential, not the
// live nodeRef, so a nodeRef edit during a replacement keeps approving the
// name the host was configured with and the replacement waits for.
func TestCSRApproverUsesRecordedNodeName(t *testing.T) {
	user := BootstrapUsername()

	tests := map[string]struct {
		cachedRecorded, liveRecorded, liveRef, csrNode string
		approve                                        bool
		contains                                       string
	}{
		"approves recorded name after nodeRef edit": {
			cachedRecorded: "node-old", liveRecorded: "node-old", liveRef: "node-new", csrNode: "node-old",
			approve: true,
		},
		"denies new nodeRef name not recorded": {
			cachedRecorded: "node-old", liveRecorded: "node-old", liveRef: "node-new", csrNode: "node-new",
			contains: `issued to Machine "m1"`,
		},
		"approves live record over stale cache": {
			cachedRecorded: "node-old", liveRecorded: "node-new", liveRef: "node-new", csrNode: "node-new",
			approve: true,
		},
		"denies stale cached record": {
			cachedRecorded: "node-new", liveRecorded: "node-other", liveRef: "node-new", csrNode: "node-new",
			contains: `issued to Machine "m1"`,
		},
		"denies empty recorded name": {
			cachedRecorded: "", liveRecorded: "", liveRef: "node-new", csrNode: "node-new",
			contains: `issued to Machine "m1"`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			cachedM := attestedMachine("m1", tt.liveRef, "JTI=a")
			cachedM.UID = "uid-1"
			cachedM.Status.Bootstrap.NodeName = tt.cachedRecorded
			liveM := attestedMachine("m1", tt.liveRef, "JTI=a")
			liveM.UID = "uid-1"
			liveM.Status.Bootstrap.NodeName = tt.liveRecorded

			live := fakecr.NewClientBuilder().WithScheme(csrTestScheme(t)).WithObjects(liveM).Build()
			r := testCSRApprover(t, csrTestCache(t, cachedM), live, nil)

			decision, err := r.Approver.Evaluate(context.Background(), bootstrapKubeletCSR(t, user, tt.csrNode, "JTI=a"))
			require.NoError(t, err)
			require.False(t, decision.Ignore)
			require.Equal(t, tt.approve, decision.Approve, decision.Message)
			require.Contains(t, decision.Message, tt.contains)
		})
	}
}

func TestCSRApproverLiveReadErrorRetries(t *testing.T) {
	live := fakecr.NewClientBuilder().WithScheme(csrTestScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("boom")
		},
	}).Build()
	a := &CSRApprover{Client: csrTestCache(t, attestedMachine("node-a", "", "JTI=a")), APIReader: live}

	csr := bootstrapKubeletCSR(t, BootstrapUsername(), "node-a", "JTI=a")
	allowed, _, err := a.credentialMayClaimNode(context.Background(), csr, "node-a", "JTI=a")
	require.ErrorContains(t, err, "boom")
	require.False(t, allowed)
}

func TestCSRApproverRequiresAPIReader(t *testing.T) {
	_, err := (&CSRApprover{Client: csrTestCache(t), KubeClient: kubefake.NewClientset()}).reconciler()
	require.Error(t, err)
}

func TestBootstrapCSRPredicate(t *testing.T) {
	p := bootstrapCSRPredicate()
	bootstrap := bootstrapKubeletCSR(t, BootstrapUsername(), "node-a", "JTI=a")
	other := bootstrapKubeletCSR(t, "system:bootstrap:abc123", "node-a", "JTI=a")
	otherSigner := bootstrap.DeepCopy()
	otherSigner.Spec.SignerName = certificatesv1.KubeAPIServerClientSignerName

	require.True(t, p.Create(event.CreateEvent{Object: bootstrap}))
	require.True(t, p.Update(event.UpdateEvent{ObjectOld: bootstrap, ObjectNew: bootstrap}))
	require.False(t, p.Delete(event.DeleteEvent{Object: bootstrap}))
	require.False(t, p.Create(event.CreateEvent{Object: other}))
	require.False(t, p.Create(event.CreateEvent{Object: otherSigner}))
}

func TestCredentialRecordedPredicate(t *testing.T) {
	p := credentialRecordedPredicate()
	none := attestedMachine("m", "", "")
	a := attestedMachine("m", "", "JTI=a")
	b := attestedMachine("m", "", "JTI=b")

	require.True(t, p.Create(event.CreateEvent{Object: a}))
	require.False(t, p.Create(event.CreateEvent{Object: none}))
	require.True(t, p.Update(event.UpdateEvent{ObjectOld: none, ObjectNew: a}))
	require.True(t, p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: b}))
	require.False(t, p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: a}))
	require.False(t, p.Update(event.UpdateEvent{ObjectOld: a, ObjectNew: none}))
	require.False(t, p.Delete(event.DeleteEvent{Object: a}))
}

// A CSR left undecided because the cache had not yet observed its Machine is
// enqueued again once the Machine's credential is recorded.
func TestCSRApproverEnqueuesCSRsForRecordedCredential(t *testing.T) {
	user := BootstrapUsername()

	pending := bootstrapKubeletCSR(t, user, "node-a", "JTI=a")
	pending.Name = "pending"
	otherCredential := bootstrapKubeletCSR(t, user, "node-a", "JTI=b")
	otherCredential.Name = "other-credential"
	otherRequester := bootstrapKubeletCSR(t, "system:bootstrap:abc123", "node-a", "JTI=a")
	otherRequester.Name = "other-requester"
	decided := bootstrapKubeletCSR(t, user, "node-a", "JTI=a")
	decided.Name = "decided"
	decided.Status.Conditions = []certificatesv1.CertificateSigningRequestCondition{{
		Type: certificatesv1.CertificateDenied, Status: corev1.ConditionTrue,
	}}

	a := &CSRApprover{Client: csrTestCache(t, pending, otherCredential, otherRequester, decided)}

	got := a.csrsForMachine(context.Background(), attestedMachine("node-a", "", "JTI=a"))
	require.Equal(t, []reconcile.Request{{NamespacedName: types.NamespacedName{Name: "pending"}}}, got)
	require.Empty(t, a.csrsForMachine(context.Background(), attestedMachine("node-a", "", "")))
}

// End to end: the first evaluation finds no Machine and leaves the CSR
// undecided; once the credential is recorded, the enqueued CSR is approved.
func TestCSRApproverApprovesAfterCredentialRecorded(t *testing.T) {
	csr := bootstrapKubeletCSR(t, BootstrapUsername(), "node-a", "JTI=a")
	kube := kubefake.NewClientset(csr.DeepCopy())
	machine := attestedMachine("node-a", "", "")
	c := csrTestCache(t, csr, machine)
	r := testCSRApprover(t, c, c, kube)
	a := &CSRApprover{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: csr.Name}}

	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	got, err := kube.CertificatesV1().CertificateSigningRequests().Get(context.Background(), csr.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, got.Status.Conditions)

	machine.Status.Bootstrap = &v1alpha3.BootstrapStatus{CredentialID: "JTI=a", NodeName: "node-a"}
	require.NoError(t, c.Update(context.Background(), machine))
	require.Equal(t, []reconcile.Request{req}, a.csrsForMachine(context.Background(), machine))

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	got, err = kube.CertificatesV1().CertificateSigningRequests().Get(context.Background(), csr.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, got.Status.Conditions, 1)
	require.Equal(t, certificatesv1.CertificateApproved, got.Status.Conditions[0].Type)
}
