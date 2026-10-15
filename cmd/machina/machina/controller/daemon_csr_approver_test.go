// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/daemoncred"
)

func TestDaemonCSRBootstrapTokenMayClaimNode(t *testing.T) {
	checker := testDaemonCSRClaimChecker(
		bootstrapToken("abc123", "site-a"),
		machineForToken("machine-a", "node-a", "abc123", "site-a"),
	)
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.bootstrapTokenMayClaimNode(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestDaemonCSRBootstrapTokenMayClaimNodeBeforeMachineExists(t *testing.T) {
	checker := testDaemonCSRClaimChecker(bootstrapToken("abc123", "site-a"))
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.bootstrapTokenMayClaimNode(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestDaemonCSRBootstrapTokenMayClaimNodeRejectsUnboundToken(t *testing.T) {
	checker := testDaemonCSRClaimChecker(bootstrapToken("abc123", ""))
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.bootstrapTokenMayClaimNode(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestDaemonCSRBootstrapTokenMayClaimNodeUsesExactMachineBindingWithoutSiteLabel(t *testing.T) {
	checker := testDaemonCSRClaimChecker(
		bootstrapToken("abc123", ""),
		machineForToken("machine-a", "node-a", "abc123", "site-a"),
	)
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.bootstrapTokenMayClaimNode(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestDaemonCSRBootstrapTokenMayClaimNodeRequiresBootstrapTokenSecret(t *testing.T) {
	token := bootstrapToken("abc123", "site-a")
	token.Type = corev1.SecretTypeOpaque
	checker := testDaemonCSRClaimChecker(token)
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.bootstrapTokenMayClaimNode(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestDaemonCSRBootstrapTokenMayClaimNodeRejectsExpiredToken(t *testing.T) {
	token := bootstrapToken("abc123", "site-a")
	token.Data = map[string][]byte{
		"expiration": []byte(time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)),
	}
	checker := testDaemonCSRClaimChecker(
		token,
		machineForToken("machine-a", "node-a", "abc123", "site-a"),
	)
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.bootstrapTokenMayClaimNode(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestDaemonCSRBootstrapTokenMayClaimNodeRejectsDifferentMachine(t *testing.T) {
	checker := testDaemonCSRClaimChecker(
		bootstrapToken("abc123", "site-a"),
		machineForToken("machine-a", "node-a", "abc123", "site-a"),
	)
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.bootstrapTokenMayClaimNode(context.Background(), csr, "node-b")
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestDaemonCSRBootstrapTokenMayClaimNodeAllowsBoundMachineWithUnlabeledToken(t *testing.T) {
	checker := testDaemonCSRClaimChecker(
		bootstrapToken("abc123", ""),
		machineForToken("machine-a", "node-a", "abc123", "site-a"),
	)
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.bootstrapTokenMayClaimNode(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestDaemonCSRBootstrapTokenMayClaimMetalmanMachine(t *testing.T) {
	token := bootstrapToken("abc123", "")
	token.Annotations = map[string]string{
		unboundedv1alpha3.MachineBootstrapTokenAnnotationKey: "machine-a",
	}
	checker := testDaemonCSRClaimChecker(
		token,
		machineForToken("machine-a", "node-a", "abc123", ""),
	)
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.bootstrapTokenMayClaimNode(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.True(t, allowed)

	allowed, err = checker.bootstrapTokenMayClaimNode(context.Background(), csr, "node-b")
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestDaemonCSRBootstrapTokenMayClaimMetalmanMachineFromAPIReader(t *testing.T) {
	token := bootstrapToken("abc123", "")
	token.Annotations = map[string]string{
		unboundedv1alpha3.MachineBootstrapTokenAnnotationKey: "machine-a",
	}
	apiReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(token, machineForToken("machine-a", "node-a", "abc123", "")).
		Build()
	cachedClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&unboundedv1alpha3.Machine{}, machineNodeRefNameField, func(client.Object) []string {
			return nil
		}).
		Build()
	checker := &daemonCSRClaimChecker{Client: cachedClient, APIReader: apiReader}
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.bootstrapTokenMayClaimNode(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestDaemonCSRLegacyBootstrapTokenMayClaimBeforeMachineExists(t *testing.T) {
	checker := testDaemonCSRClaimChecker(bootstrapToken("abc123", "site-a"))
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.legacyBootstrapTokenMayClaimNode(context.Background(), csr, "new-node")
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestDaemonCSRLegacyBootstrapTokenRequiresSiteOrDefaultBinding(t *testing.T) {
	checker := testDaemonCSRClaimChecker(bootstrapToken("abc123", ""))
	csr := csrForBinding("system:bootstrap:abc123")

	allowed, err := checker.legacyBootstrapTokenMayClaimNode(context.Background(), csr, "new-node")
	require.NoError(t, err)
	require.False(t, allowed)

	token := bootstrapToken("def456", "")
	token.Labels[defaultBootstrapTokenLabel] = "true"
	checker = testDaemonCSRClaimChecker(token)
	csr = csrForBinding("system:bootstrap:def456")

	allowed, err = checker.legacyBootstrapTokenMayClaimNode(context.Background(), csr, "new-node")
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestBootstrapGroupApproverRoutesOnlyMatchingBootstrapGroup(t *testing.T) {
	delegate := &recordingCSRApprover{}
	approver := bootstrapGroupApprover{
		group:    "system:bootstrappers:metalman",
		approver: delegate,
	}
	csr := csrForBinding("system:bootstrap:abc123")
	csr.Spec.Groups = []string{"system:bootstrappers:unbounded-agent-daemons"}

	decision, err := approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.True(t, decision.Ignore)
	require.False(t, delegate.called)

	csr.Spec.Groups = []string{"system:bootstrappers:metalman"}
	decision, err = approver.Evaluate(context.Background(), csr)
	require.NoError(t, err)
	require.False(t, decision.Ignore)
	require.True(t, delegate.called)
}

type recordingCSRApprover struct {
	called bool
}

func (a *recordingCSRApprover) Evaluate(
	context.Context,
	*certificatesv1.CertificateSigningRequest,
) (daemoncred.CSRDecision, error) {
	a.called = true

	return daemoncred.CSRDecision{Approve: true}, nil
}

func TestDaemonCSRNodeHasMachineBinding(t *testing.T) {
	checker := testDaemonCSRClaimChecker(machineForToken("machine-a", "node-a", "abc123", "site-a"))
	csr := csrForBinding("system:node:node-a")

	allowed, err := checker.nodeHasMachineBinding(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.True(t, allowed)
}

func TestDaemonCSRNodeHasMachineBindingRejectsMissingMachine(t *testing.T) {
	checker := testDaemonCSRClaimChecker()
	csr := csrForBinding("system:node:node-a")

	allowed, err := checker.nodeHasMachineBinding(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestDaemonCSRNodeHasMachineBindingRequiresExplicitNodeRef(t *testing.T) {
	checker := testDaemonCSRClaimChecker(machineWithoutNodeRef("node-a", "abc123", "site-a"))
	csr := csrForBinding("system:node:node-a")

	allowed, err := checker.nodeHasMachineBinding(context.Background(), csr, "node-a")
	require.NoError(t, err)
	require.False(t, allowed)
}

func testDaemonCSRClaimChecker(objs ...client.Object) *daemonCSRClaimChecker {
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&unboundedv1alpha3.Machine{}, machineNodeRefNameField, func(obj client.Object) []string {
			machine := obj.(*unboundedv1alpha3.Machine)
			if machine.Spec.Kubernetes == nil || machine.Spec.Kubernetes.NodeRef == nil || machine.Spec.Kubernetes.NodeRef.Name == "" {
				return nil
			}

			return []string{machine.Spec.Kubernetes.NodeRef.Name}
		}).
		Build()

	return &daemonCSRClaimChecker{Client: c, APIReader: c}
}

func csrForBinding(username string) *certificatesv1.CertificateSigningRequest {
	return &certificatesv1.CertificateSigningRequest{
		ObjectMeta: metav1.ObjectMeta{Name: "test-csr"},
		Spec: certificatesv1.CertificateSigningRequestSpec{
			Username: username,
			Groups:   []string{daemoncred.SystemNodesGroup, daemonGroup},
		},
	}
}

func bootstrapToken(tokenID, site string) *corev1.Secret {
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
				BootstrapTokenRef: &unboundedv1alpha3.LocalObjectReference{
					Name: "bootstrap-token-" + tokenID,
				},
			},
		},
	}
}

func machineWithoutNodeRef(machineName, tokenID, site string) *unboundedv1alpha3.Machine {
	machine := machineForToken(machineName, "", tokenID, site)
	machine.Spec.Kubernetes.NodeRef = nil

	return machine
}
