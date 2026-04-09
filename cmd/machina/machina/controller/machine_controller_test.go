// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/Azure/unbounded-kube/api/v1alpha3"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type mockReachabilityChecker struct {
	err error
}

func (m *mockReachabilityChecker) CheckReachable(_ context.Context, _ *unboundedv1alpha3.Machine) error {
	return m.err
}

type mockProvisioner struct {
	err            error
	called         bool
	machine        *unboundedv1alpha3.Machine
	bootstrapToken string
}

func (m *mockProvisioner) ProvisionMachine(
	_ context.Context,
	machine *unboundedv1alpha3.Machine,
	_ *ssh.ClientConfig,
	bootstrapToken string,
	_ *ClusterInfo,
) error {
	m.called = true
	m.machine = machine
	m.bootstrapToken = bootstrapToken

	return m.err
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	return s
}

// newTestMachine builds a Machine with SSH config inline.
// Pass kubernetes as nil for reachable-only machines (no provisioning).
func newTestMachine(name, host, username string, kubernetes *unboundedv1alpha3.KubernetesSpec) *unboundedv1alpha3.Machine {
	return &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          host,
				Username:      username,
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "ssh-key-secret"},
			},
			Kubernetes: kubernetes,
		},
	}
}

// defaultKubernetes returns a KubernetesSpec with sensible test defaults.
func defaultKubernetes() *unboundedv1alpha3.KubernetesSpec {
	return &unboundedv1alpha3.KubernetesSpec{
		Version:           "v1.34.0",
		BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
	}
}

func newSSHKeySecret(t *testing.T, name string) *corev1.Secret {
	t.Helper()

	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: SecretNamespaceUnboundedKube,
		},
		Data: map[string][]byte{
			"ssh-privatekey": generateTestSSHKeyPEM(t),
		},
	}
}

func newBootstrapTokenSecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("def456ghi789jkl0"),
		},
	}
}

func startTCPListener(t *testing.T) (int, func()) {
	t.Helper()

	lc := net.ListenConfig{}
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err, "failed to start TCP listener")

	port := listener.Addr().(*net.TCPAddr).Port

	return port, func() {
		_ = listener.Close()
	}
}

func findCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}

	return nil
}

// generateTestSSHKeyPEM creates a fresh RSA private key in PEM format for testing.
// It uses generateTestRSAKey and marshalPrivateKeyPEM from ssh_integration_test.go.
func generateTestSSHKeyPEM(t *testing.T) []byte {
	t.Helper()

	key, _ := generateTestRSAKey(t)

	return marshalPrivateKeyPEM(t, key)
}

// ---------------------------------------------------------------------------
// Reconcile tests
// ---------------------------------------------------------------------------

func TestMachineReconciler_Reconcile(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	tests := []struct {
		name            string
		machine         *unboundedv1alpha3.Machine
		checkErr        error
		expectedPhase   unboundedv1alpha3.MachinePhase
		expectedRequeue time.Duration
		expectNotFound  bool
	}{
		{
			name:           "machine not found returns no error",
			machine:        nil,
			expectNotFound: true,
		},
		{
			name:            "reachable machine without kubernetes config transitions to Ready",
			machine:         newTestMachine("test-machine", "192.168.1.100:22", "testuser", nil),
			expectedPhase:   unboundedv1alpha3.MachinePhaseReady,
			expectedRequeue: RequeueAfterReady,
		},
		{
			name:            "unreachable machine stays Pending",
			machine:         newTestMachine("test-machine", "192.168.1.100:22", "testuser", nil),
			checkErr:        fmt.Errorf("TCP dial 192.168.1.100:22: connection refused"),
			expectedPhase:   unboundedv1alpha3.MachinePhasePending,
			expectedRequeue: RequeueAfterPending,
		},
		{
			name:            "host without port defaults to 22",
			machine:         newTestMachine("test-machine", "192.168.1.100", "testuser", nil),
			expectedPhase:   unboundedv1alpha3.MachinePhaseReady,
			expectedRequeue: RequeueAfterReady,
		},
		{
			name:            "custom port is respected",
			machine:         newTestMachine("test-machine", "192.168.1.100:2222", "testuser", nil),
			expectedPhase:   unboundedv1alpha3.MachinePhaseReady,
			expectedRequeue: RequeueAfterReady,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clientBuilder := fake.NewClientBuilder().WithScheme(s)
			if tt.machine != nil {
				clientBuilder = clientBuilder.WithObjects(tt.machine).WithStatusSubresource(tt.machine)
			}

			fakeClient := clientBuilder.Build()

			reconciler := &MachineReconciler{
				Client:              fakeClient,
				Scheme:              s,
				ReachabilityChecker: &mockReachabilityChecker{err: tt.checkErr},
			}

			req := ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-machine"},
			}

			result, err := reconciler.Reconcile(context.Background(), req)

			if tt.expectNotFound {
				require.NoError(t, err)
				require.Equal(t, ctrl.Result{}, result)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expectedRequeue, result.RequeueAfter)

			var updated unboundedv1alpha3.Machine

			err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
			require.NoError(t, err)

			require.Equal(t, tt.expectedPhase, updated.Status.Phase)

			// SSHReachable condition should be present.
			sshCond := findCondition(updated.Status.Conditions, unboundedv1alpha3.MachineConditionSSHReachable)
			require.NotNil(t, sshCond, "SSHReachable condition should be set")
		})
	}
}

// ---------------------------------------------------------------------------
// Machine with nil SSH spec is skipped
// ---------------------------------------------------------------------------

func TestMachineReconciler_NilSSHSpec_Skipped(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec:       unboundedv1alpha3.MachineSpec{},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
}

// ---------------------------------------------------------------------------
// Provisioning flow tests
// ---------------------------------------------------------------------------

func TestMachineReconciler_Provisioning_Success(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	sshSecret := newSSHKeySecret(t, "ssh-key-secret")
	bootstrapSecret := newBootstrapTokenSecret("bootstrap-token-abc123")

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshSecret, bootstrapSecret).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
		Provisioner:         provisioner,
		ClusterInfo: &ClusterInfo{
			APIServer:    "api.example.com:443",
			CACertBase64: "dGVzdC1jYQ==",
			ClusterDNS:   "10.0.0.10",
			KubeVersion:  "v1.34.2",
		},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.True(t, provisioner.called, "provisioner should have been called")
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, updated.Status.Phase)

	// Provisioned condition should be set.
	provCond := findCondition(updated.Status.Conditions, unboundedv1alpha3.MachineConditionProvisioned)
	require.NotNil(t, provCond, "Provisioned condition should be set")
	require.Equal(t, metav1.ConditionTrue, provCond.Status)

	// Provisioning condition should be cleared (False/Completed).
	provingCond := findCondition(updated.Status.Conditions, unboundedv1alpha3.MachineConditionProvisioning)
	require.NotNil(t, provingCond, "Provisioning condition should be present")
	require.Equal(t, metav1.ConditionFalse, provingCond.Status)
	require.Equal(t, "Completed", provingCond.Reason)
}

func TestMachineReconciler_Provisioning_SetsAgentStatus(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Spec.Agent = &unboundedv1alpha3.AgentSpec{
		Image: "ghcr.io/azure/rootfs:v1.0.0",
	}

	sshSecret := newSSHKeySecret(t, "ssh-key-secret")
	bootstrapSecret := newBootstrapTokenSecret("bootstrap-token-abc123")

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshSecret, bootstrapSecret).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{KubeVersion: "v1.34.0"},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.True(t, provisioner.called)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, updated.Status.Phase)

	// Agent status should reflect the applied spec.
	require.NotNil(t, updated.Status.Agent, "Status.Agent should be set after provisioning")
	require.Equal(t, "ghcr.io/azure/rootfs:v1.0.0", updated.Status.Agent.Image)
}

func TestMachineReconciler_Provisioning_NoAgentSpec_NilAgentStatus(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	// No Agent spec set.

	sshSecret := newSSHKeySecret(t, "ssh-key-secret")
	bootstrapSecret := newBootstrapTokenSecret("bootstrap-token-abc123")

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshSecret, bootstrapSecret).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{KubeVersion: "v1.34.0"},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, updated.Status.Phase)

	// Agent status should remain nil when no Agent spec is provided.
	require.Nil(t, updated.Status.Agent, "Status.Agent should be nil when Spec.Agent is not set")
}

func TestMachineReconciler_Provisioning_ProvisionerFails(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	sshSecret := newSSHKeySecret(t, "ssh-key-secret")
	bootstrapSecret := newBootstrapTokenSecret("bootstrap-token-abc123")

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshSecret, bootstrapSecret).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{err: fmt.Errorf("SSH connection refused")}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "SSH connection refused")

	// Provisioning condition should be cleared (False/Failed).
	provingCond := findCondition(updated.Status.Conditions, unboundedv1alpha3.MachineConditionProvisioning)
	require.NotNil(t, provingCond, "Provisioning condition should be present")
	require.Equal(t, metav1.ConditionFalse, provingCond.Status)
	require.Equal(t, "Failed", provingCond.Reason)
}

func TestMachineReconciler_Provisioning_JoiningSkipsReProvision(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase: unboundedv1alpha3.MachinePhaseJoining,
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
		Provisioner:         provisioner,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.False(t, provisioner.called, "provisioner should NOT be called — Joining phase routes to Node join")
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)
}

func TestMachineReconciler_Provisioning_SSHKeyNotFound(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	// No SSH key secret created.

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "SSH config")
}

func TestMachineReconciler_Provisioning_BootstrapTokenSecretMissing(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	sshSecret := newSSHKeySecret(t, "ssh-key-secret")
	// No bootstrap token secret created.

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshSecret).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "bootstrap token")
}

// ---------------------------------------------------------------------------
// Provisioning phase gate tests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Provisioning timeout tests
// ---------------------------------------------------------------------------

func TestMachineReconciler_ProvisioningPhase_TimesOutToFailed(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase: unboundedv1alpha3.MachinePhaseProvisioning,
		Conditions: []metav1.Condition{
			{
				Type:               unboundedv1alpha3.MachineConditionProvisioning,
				Status:             metav1.ConditionTrue,
				Reason:             "InProgress",
				Message:            "Provisioning in progress",
				LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
		Provisioner:         provisioner,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.False(t, provisioner.called, "provisioner should NOT be called when timing out")
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "timed out")
}

func TestMachineReconciler_ProvisioningPhase_RecentProvisioningRequeues(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase: unboundedv1alpha3.MachinePhaseProvisioning,
		Conditions: []metav1.Condition{
			{
				Type:               unboundedv1alpha3.MachineConditionProvisioning,
				Status:             metav1.ConditionTrue,
				Reason:             "InProgress",
				Message:            "Provisioning in progress",
				LastTransitionTime: metav1.NewTime(time.Now().Add(-1 * time.Minute)),
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
		Provisioner:         provisioner,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.False(t, provisioner.called, "provisioner should NOT be called — still within timeout")
	require.Equal(t, RequeueAfterPending, result.RequeueAfter)

	// Phase should remain Provisioning (no status update).
	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseProvisioning, updated.Status.Phase)
}

func TestMachineReconciler_ProvisioningPhase_MissingConditionTimesOut(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	// Machine in Provisioning phase but without the Provisioning condition
	// (e.g. pre-existing machine from before this feature was added).
	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase: unboundedv1alpha3.MachinePhaseProvisioning,
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
		Provisioner:         provisioner,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.False(t, provisioner.called, "provisioner should NOT be called")
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "timed out")
}

func TestMachineReconciler_Provisioning_RetryFromFailed(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase:   unboundedv1alpha3.MachinePhaseFailed,
		Message: "Previous provisioning failed",
	}

	sshSecret := newSSHKeySecret(t, "ssh-key-secret")
	bootstrapSecret := newBootstrapTokenSecret("bootstrap-token-abc123")

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshSecret, bootstrapSecret).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.True(t, provisioner.called, "provisioner should be called to retry from Failed phase")
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Helper function tests
// ---------------------------------------------------------------------------

func TestGetSecretValue(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	t.Run("reads secret with specified key", func(t *testing.T) {
		t.Parallel()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "unbounded-kube"},
			Data:       map[string][]byte{"custom-key": []byte("secret-value")},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

		val, err := getSecretValue(context.Background(), fakeClient,
			&unboundedv1alpha3.SecretKeySelector{Name: "my-secret", Key: "custom-key"})
		require.NoError(t, err)
		require.Equal(t, "secret-value", val)
	})

	t.Run("defaults to ssh-privatekey key", func(t *testing.T) {
		t.Parallel()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "unbounded-kube"},
			Data:       map[string][]byte{"ssh-privatekey": []byte("my-key")},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

		val, err := getSecretValue(context.Background(), fakeClient,
			&unboundedv1alpha3.SecretKeySelector{Name: "my-secret"})
		require.NoError(t, err)
		require.Equal(t, "my-key", val)
	})

	t.Run("returns error when secret not found", func(t *testing.T) {
		t.Parallel()

		fakeClient := fake.NewClientBuilder().WithScheme(s).Build()

		_, err := getSecretValue(context.Background(), fakeClient,
			&unboundedv1alpha3.SecretKeySelector{Name: "missing-secret"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing-secret")
	})

	t.Run("returns error when key not found in secret", func(t *testing.T) {
		t.Parallel()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "unbounded-kube"},
			Data:       map[string][]byte{"other-key": []byte("value")},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()

		_, err := getSecretValue(context.Background(), fakeClient,
			&unboundedv1alpha3.SecretKeySelector{Name: "my-secret", Key: "missing-key"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing-key")
	})
}

func TestGetBootstrapToken(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	t.Run("returns formatted token", func(t *testing.T) {
		t.Parallel()

		secret := newBootstrapTokenSecret("bootstrap-token-test")

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		token, err := reconciler.getBootstrapToken(context.Background(), "bootstrap-token-test")
		require.NoError(t, err)
		require.Equal(t, "abc123.def456ghi789jkl0", token)
	})

	t.Run("returns error when secret not found", func(t *testing.T) {
		t.Parallel()

		fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		_, err := reconciler.getBootstrapToken(context.Background(), "missing-secret")
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing-secret")
	})

	t.Run("returns error when token-id key missing", func(t *testing.T) {
		t.Parallel()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-test", Namespace: "kube-system"},
			Data:       map[string][]byte{"token-secret": []byte("secret")},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		_, err := reconciler.getBootstrapToken(context.Background(), "bootstrap-token-test")
		require.Error(t, err)
		require.Contains(t, err.Error(), "token-id")
	})

	t.Run("returns error when token-secret key missing", func(t *testing.T) {
		t.Parallel()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-test", Namespace: "kube-system"},
			Data:       map[string][]byte{"token-id": []byte("abc123")},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		_, err := reconciler.getBootstrapToken(context.Background(), "bootstrap-token-test")
		require.Error(t, err)
		require.Contains(t, err.Error(), "token-secret")
	})
}

func TestBuildSSHConfig(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	t.Run("builds valid SSH config", func(t *testing.T) {
		t.Parallel()

		sshSecret := newSSHKeySecret(t, "ssh-key-secret")

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(sshSecret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", nil)

		cfg, err := reconciler.buildSSHConfig(context.Background(), machine)
		require.NoError(t, err)
		require.Equal(t, "testuser", cfg.User)
		require.Equal(t, SSHConnectTimeout, cfg.Timeout)
		require.Len(t, cfg.Auth, 1)
	})

	t.Run("defaults to azureuser when username is empty", func(t *testing.T) {
		t.Parallel()

		sshSecret := newSSHKeySecret(t, "ssh-key-secret")

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(sshSecret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		machine := newTestMachine("test-machine", "10.0.0.1:22", "", nil)

		cfg, err := reconciler.buildSSHConfig(context.Background(), machine)
		require.NoError(t, err)
		require.Equal(t, "azureuser", cfg.User)
	})

	t.Run("returns error when SSH key secret not found", func(t *testing.T) {
		t.Parallel()

		fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", nil)

		_, err := reconciler.buildSSHConfig(context.Background(), machine)
		require.Error(t, err)
		require.Contains(t, err.Error(), "SSH private key")
	})

	t.Run("returns error when private key is invalid", func(t *testing.T) {
		t.Parallel()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "ssh-key-secret", Namespace: "unbounded-kube"},
			Data:       map[string][]byte{"ssh-privatekey": []byte("not-a-valid-key")},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", nil)

		_, err := reconciler.buildSSHConfig(context.Background(), machine)
		require.Error(t, err)
		require.Contains(t, err.Error(), "parse SSH private key")
	})
}

// ---------------------------------------------------------------------------
// DefaultReachabilityChecker tests
// ---------------------------------------------------------------------------

func TestDefaultReachabilityChecker_CheckReachable(t *testing.T) {
	t.Parallel()

	t.Run("reachable when listener is running", func(t *testing.T) {
		t.Parallel()

		port, cleanup := startTCPListener(t)
		defer cleanup()

		checker := &DefaultReachabilityChecker{Timeout: time.Second}
		machine := newTestMachine("m", fmt.Sprintf("127.0.0.1:%d", port), "u", nil)

		err := checker.CheckReachable(context.Background(), machine)

		require.NoError(t, err)
	})

	t.Run("unreachable when no listener", func(t *testing.T) {
		t.Parallel()

		checker := &DefaultReachabilityChecker{Timeout: 100 * time.Millisecond}
		machine := newTestMachine("m", "127.0.0.1:59999", "u", nil)

		err := checker.CheckReachable(context.Background(), machine)

		require.Error(t, err)
		require.Contains(t, err.Error(), "TCP dial 127.0.0.1:59999")
	})

	t.Run("unreachable on invalid host", func(t *testing.T) {
		t.Parallel()

		checker := &DefaultReachabilityChecker{Timeout: 100 * time.Millisecond}
		machine := newTestMachine("m", "invalid-ip:22", "u", nil)

		err := checker.CheckReachable(context.Background(), machine)

		require.Error(t, err)
		require.Contains(t, err.Error(), "TCP dial invalid-ip:22")
	})

	t.Run("uses default timeout when not specified", func(t *testing.T) {
		t.Parallel()

		port, cleanup := startTCPListener(t)
		defer cleanup()

		checker := &DefaultReachabilityChecker{}
		machine := newTestMachine("m", fmt.Sprintf("127.0.0.1:%d", port), "u", nil)

		err := checker.CheckReachable(context.Background(), machine)

		require.NoError(t, err)
	})

	t.Run("host without port defaults to 22", func(t *testing.T) {
		t.Parallel()

		// Use a non-routable IP to guarantee connection failure.
		checker := &DefaultReachabilityChecker{Timeout: 100 * time.Millisecond}
		machine := newTestMachine("m", "192.0.2.1", "u", nil)

		err := checker.CheckReachable(context.Background(), machine)

		require.Error(t, err)
		require.Contains(t, err.Error(), "TCP dial 192.0.2.1:22")
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		checker := &DefaultReachabilityChecker{Timeout: 5 * time.Second}
		machine := newTestMachine("m", "127.0.0.1:22", "u", nil)

		err := checker.CheckReachable(ctx, machine)

		require.Error(t, err)
	})
}

// ---------------------------------------------------------------------------
// UpdateStatus tests
// ---------------------------------------------------------------------------

func TestMachineReconciler_UpdateStatus_ConditionUpdate(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", nil)
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase: unboundedv1alpha3.MachinePhasePending,
		Conditions: []metav1.Condition{
			{
				Type:               unboundedv1alpha3.MachineConditionSSHReachable,
				Status:             metav1.ConditionFalse,
				Reason:             "Unreachable",
				Message:            "Machine is not reachable",
				LastTransitionTime: metav1.Now(),
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-machine"},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)

	sshCond := findCondition(updated.Status.Conditions, unboundedv1alpha3.MachineConditionSSHReachable)
	require.NotNil(t, sshCond)
	require.Equal(t, metav1.ConditionTrue, sshCond.Status)
	require.Equal(t, "Reachable", sshCond.Reason)
}

func TestMachineReconciler_UpdateStatus_PhaseDeterminesRequeue(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	tests := []struct {
		name            string
		phase           unboundedv1alpha3.MachinePhase
		expectedRequeue time.Duration
	}{
		{"Ready requeues at Ready interval", unboundedv1alpha3.MachinePhaseReady, RequeueAfterReady},
		{"Joining requeues at Joining interval", unboundedv1alpha3.MachinePhaseJoining, RequeueAfterJoining},
		{"Failed requeues at Failed interval", unboundedv1alpha3.MachinePhaseFailed, RequeueAfterFailed},
		{"Pending requeues at Pending interval", unboundedv1alpha3.MachinePhasePending, RequeueAfterPending},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", nil)

			fakeClient := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(machine).
				WithStatusSubresource(machine).
				Build()

			reconciler := &MachineReconciler{
				Client: fakeClient,
				Scheme: s,
			}

			result, err := reconciler.updateStatus(context.Background(), machine, tt.phase, "test message")
			require.NoError(t, err)
			require.Equal(t, tt.expectedRequeue, result.RequeueAfter)
		})
	}
}

// ---------------------------------------------------------------------------
// findMachineForNode tests
// ---------------------------------------------------------------------------

func TestFindMachineForNode(t *testing.T) {
	t.Parallel()

	t.Run("returns request when Node has matching label", func(t *testing.T) {
		t.Parallel()

		s := newTestScheme(t)

		fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "worker-1",
				Labels: map[string]string{MachineNodeLabel: "my-machine"},
			},
		}

		requests := reconciler.findMachineForNode(context.Background(), node)
		require.Len(t, requests, 1)
		require.Equal(t, "my-machine", requests[0].Name)
	})

	t.Run("returns nil when Node has no label", func(t *testing.T) {
		t.Parallel()

		s := newTestScheme(t)

		fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-1",
			},
		}

		requests := reconciler.findMachineForNode(context.Background(), node)
		require.Nil(t, requests)
	})

	t.Run("returns nil for non-Node object", func(t *testing.T) {
		t.Parallel()

		s := newTestScheme(t)

		fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "some-pod",
			},
		}

		requests := reconciler.findMachineForNode(context.Background(), pod)
		require.Nil(t, requests)
	})
}

// ---------------------------------------------------------------------------
// reconcileNodeJoin tests
// ---------------------------------------------------------------------------

func TestReconcileNodeJoin_Joining_NoNode(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase: unboundedv1alpha3.MachinePhaseJoining,
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, updated.Status.Phase)
}

func TestReconcileNodeJoin_Joining_NodeFound(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase: unboundedv1alpha3.MachinePhaseJoining,
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "worker-1",
			Labels: map[string]string{MachineNodeLabel: "test-machine"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, node).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseReady, updated.Status.Phase)
}

func TestReconcileNodeJoin_Ready_NodeStillExists(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	k8s := defaultKubernetes()
	k8s.NodeRef = &unboundedv1alpha3.LocalObjectReference{Name: "worker-1"}

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", k8s)
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase: unboundedv1alpha3.MachinePhaseReady,
		Conditions: []metav1.Condition{
			{Type: unboundedv1alpha3.MachineConditionProvisioned, Status: metav1.ConditionTrue, Reason: "Provisioned"},
		},
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "worker-1",
			Labels: map[string]string{MachineNodeLabel: "test-machine"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, node).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)
}

func TestReconcileNodeJoin_Ready_NodeDisappears(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	k8s := defaultKubernetes()
	k8s.NodeRef = &unboundedv1alpha3.LocalObjectReference{Name: "worker-1"}

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", k8s)
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase: unboundedv1alpha3.MachinePhaseReady,
		Conditions: []metav1.Condition{
			{Type: unboundedv1alpha3.MachineConditionProvisioned, Status: metav1.ConditionTrue, Reason: "Provisioned"},
		},
	}

	// No Node in the cluster — it disappeared.
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "Node disappeared")
}

func TestReconcileNodeJoin_Joining_NodeReappears(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase: unboundedv1alpha3.MachinePhaseJoining,
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "worker-1",
			Labels: map[string]string{MachineNodeLabel: "test-machine"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, node).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseReady, updated.Status.Phase)
}

func TestReconcileNodeJoin_Joining_StillWaiting(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1:22", "testuser", defaultKubernetes())
	machine.Status = unboundedv1alpha3.MachineStatus{
		Phase: unboundedv1alpha3.MachinePhaseJoining,
	}

	// No Node in the cluster.
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)

	var updated unboundedv1alpha3.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, updated.Status.Phase)
}

// ---------------------------------------------------------------------------
// Config tests
// ---------------------------------------------------------------------------

func TestDefaultConfig(t *testing.T) {
	t.Parallel()

	cfg := DefaultConfig()
	require.Equal(t, ":8080", cfg.MetricsAddr)
	require.Equal(t, ":8081", cfg.ProbeAddr)
	require.False(t, cfg.EnableLeaderElection)
	require.Equal(t, 10, cfg.MaxConcurrentReconciles)
	require.Equal(t, ProvisioningTimeout, cfg.ProvisioningTimeout)
}

// ---------------------------------------------------------------------------
// Scheme registration test
// ---------------------------------------------------------------------------

func TestSchemeRegistration(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	gvks, _, err := s.ObjectKinds(&unboundedv1alpha3.Machine{})
	require.NoError(t, err)
	require.Len(t, gvks, 1)
	require.Equal(t, "Machine", gvks[0].Kind)
	require.Equal(t, "v1alpha3", gvks[0].Version)
}

// Ensure fake client satisfies the client.Client interface.
var _ client.Client = (fake.NewClientBuilder().Build())
