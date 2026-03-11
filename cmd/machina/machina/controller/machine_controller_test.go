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

	machinav1alpha2 "github.com/project-unbounded/unbounded-kube/cmd/machina/machina/api/v1alpha2"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// mockReachabilityChecker is a mock implementation of ReachabilityChecker for testing.
type mockReachabilityChecker struct {
	err error
	// calledHost and calledPort record the last invocation for assertions.
	calledHost string
	calledPort int32
}

func (m *mockReachabilityChecker) CheckReachable(_ context.Context, host string, port int32) error {
	m.calledHost = host
	m.calledPort = port

	return m.err
}

// mockProvisioner is a mock implementation of MachineProvisioner for testing.
type mockProvisioner struct {
	err    error
	called bool
	// Capture args for assertions.
	machine        *machinav1alpha2.Machine
	model          *machinav1alpha2.MachineModel
	bootstrapToken string
}

func (m *mockProvisioner) ProvisionMachine(
	_ context.Context,
	machine *machinav1alpha2.Machine,
	model *machinav1alpha2.MachineModel,
	_ *ssh.ClientConfig,
	bootstrapToken string,
	_ *ClusterInfo,
) error {
	m.called = true
	m.machine = machine
	m.model = model
	m.bootstrapToken = bootstrapToken

	return m.err
}

// newTestScheme creates a runtime.Scheme with v1alpha2 and core types.
func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, machinav1alpha2.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	return s
}

// newTestMachine creates a Machine for tests with sensible defaults.
func newTestMachine(name, host string, port int32, modelRef *machinav1alpha2.LocalObjectReference) *machinav1alpha2.Machine {
	return &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: machinav1alpha2.MachineSpec{
			SSH: machinav1alpha2.MachineSSHSpec{
				Host: host,
				Port: port,
			},
			ModelRef: modelRef,
		},
	}
}

// newTestModel creates a MachineModel for tests with sensible defaults.
func newTestModel(name string) *machinav1alpha2.MachineModel {
	return &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername: "testuser",
			SSHPrivateKeyRef: machinav1alpha2.SecretKeySelector{
				Name: "ssh-key-secret",
				Key:  "ssh-privatekey",
			},
			AgentInstallScript: "#!/bin/bash\necho hello",
			KubernetesProfile: &machinav1alpha2.KubernetesProfile{
				Version: "1.34.0",
				BootstrapTokenRef: machinav1alpha2.LocalObjectReference{
					Name: "bootstrap-token-abc123",
				},
			},
		},
	}
}

// newSSHKeySecret creates a Secret containing a test SSH private key.
func newSSHKeySecret(name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: SecretNamespaceMachinaSystem,
		},
		Data: map[string][]byte{
			"ssh-privatekey": []byte(testRSAPrivateKey),
		},
	}
}

// newBootstrapTokenSecret creates a bootstrap token Secret in kube-system.
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

// startTCPListener starts a TCP listener on a random port and returns the port and cleanup function.
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

// testRSAPrivateKey is a test RSA private key for SSH tests.
// DO NOT use this key for anything other than testing.
const testRSAPrivateKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAABFwAAAAdzc2gtcn
NhAAAAAwEAAQAAAQEAlmJuT3YxODWuH1okayWYjtyNAYSbbC2BcJ5fJz+t498f0sUbhKmH
wYyJRrNnhsZtSzsWmxLQff55+UFjBKcRYjLryqQ2ga+xqRSWSNqbrEIgENz5Nn4+1wSYzd
TInK1WYL6MRYr8+4PwNWMV++08Wp3dGQAr+caLoQ6Ei25mCy9KH/1rfd6e214JDFpIulTt
DHRQH8BtSCEt9FW8G97lj4xGFYsWi7zFrqckJ8D7MHGG0nO7xuSpFZ6HnrlCG7ABe2Iqkf
RfvAhgb+UBL0XiBOLVEvuzdty1dBcOQhTwuFXetkvzzY3t3v0m8+x3xIq28BpJ13AgSatk
vAj7DVb07wAAA9DH7Mphx+zKYQAAAAdzc2gtcnNhAAABAQCWYm5PdjE4Na4fWiRrJZiO3I
0BhJtsLYFwnl8nP63j3x/SxRuEqYfBjIlGs2eGxm1LOxabEtB9/nn5QWMEpxFiMuvKpDaB
r7GpFJZI2pusQiAQ3Pk2fj7XBJjN1MicrVZgvoxFivz7g/A1YxX77Txand0ZACv5xouhDo
SLbmYLL0of/Wt93p7bXgkMWki6VO0MdFAfwG1IIS30Vbwb3uWPjEYVixaLvMWupyQnwPsw
cYbSc7vG5KkVnoeeuUIbsAF7YiqR9F+8CGBv5QEvReIE4tUS+7N23LV0Fw5CFPC4Vd62S/
PNje3e/Sbz7HfEirbwGknXcCBJq2S8CPsNVvTvAAAAAwEAAQAAAQAQT7/gTZUcIDJxQyFF
H/BSuphuzDfhfXQXR45RnwYY+9gjT+7irlLDyx8OtKHri/VJ3jBfBKTpraMERrPbStXHXX
eW5MXmvixahxmf8FpHTmrU+WrsnrfpMZ3zYXubBvAiETj8yA0VqONynvtA9qP/vjS/o/Wh
I4h8oSr+Rqy51K419o2mRJGxWK2ynp6AMZzL98SHsrCCRNvVIEQqV8l9vgHTc2n4RaZ4lT
4Q6HS1oO67yQ9JOXfD0o5ly1xlF7KcrVkForipFDUfgsT87Rs7qdl2oilyelhIHYWPBCcT
GP8P+FDA9eK21hrk7CaY24fKLWWZmFTF2y3OsQ6lVRgJAAAAgQDEjXTSejDhjC/iJKfqyi
Kk1bLLReSRvFlix92wLjKCZxtiKV0mt9H4SwITKW+YZVZOoMHNhYZR170MMTLIOxcSHq/K
HqCJyVGzCATBzMY+AX9JdrhRKrVahHup/BTuigYBE3l/lJp2W4P58S8Ylew1AqqjUnz1nL
zeaMNvnDfPUgAAAIEAyIN3/vdxQNWw8xFDglqiAA8IquH0Igu68jmaa8QBgRWm0XwaLT93
mli2OGXYqvJNUyPy5awVDROmz1izDrHmOCycFXnHKw7RvRVAVC6sh7758pnilq4vCsFiOM
IkHn9FMGu517Y+Oa00sDWSmGcep9F/Sc5cNnXpCV71Ut2/hZkAAACBAL//yDlyYMOZuypa
gzlxW/17KIjvrWxQyaBMAWcwjt8i2jeFwE4qDaVpgbBP7MHA3ULDaph50p3shzl+jkCBdq
2wqL4Rr3kSqt0OsfuTflrgJsA1ArWVPbE8ZFst8vFUTn3kBwlfS/hgpIzkBO9DtD4E8Hew
j0yoopZbn4UqwdPHAAAAE3BoaWxAd29ya2JveC1jYW13aW4BAgMEBQYH
-----END OPENSSH PRIVATE KEY-----`

// ---------------------------------------------------------------------------
// Reconcile tests
// ---------------------------------------------------------------------------

func TestMachineReconciler_Reconcile(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	tests := []struct {
		name              string
		machine           *machinav1alpha2.Machine
		checkErr          error
		expectedPhase     machinav1alpha2.MachinePhase
		expectedRequeue   time.Duration
		expectedCondition metav1.ConditionStatus
		expectNotFound    bool
	}{
		{
			name:           "machine not found returns no error",
			machine:        nil,
			expectNotFound: true,
		},
		{
			name:              "reachable machine without modelRef transitions to Ready",
			machine:           newTestMachine("test-machine", "10.0.0.1", 22, nil),
			checkErr:          nil,
			expectedPhase:     machinav1alpha2.MachinePhaseReady,
			expectedRequeue:   RequeueAfterReady,
			expectedCondition: metav1.ConditionTrue,
		},
		{
			name:              "unreachable machine stays Pending",
			machine:           newTestMachine("test-machine", "10.0.0.1", 22, nil),
			checkErr:          fmt.Errorf("TCP dial 10.0.0.1:22: connection refused"),
			expectedPhase:     machinav1alpha2.MachinePhasePending,
			expectedRequeue:   RequeueAfterPending,
			expectedCondition: metav1.ConditionFalse,
		},
		{
			name:              "port defaults to 22 when not specified",
			machine:           newTestMachine("test-machine", "10.0.0.1", 0, nil),
			checkErr:          nil,
			expectedPhase:     machinav1alpha2.MachinePhaseReady,
			expectedRequeue:   RequeueAfterReady,
			expectedCondition: metav1.ConditionTrue,
		},
		{
			name:              "custom port is respected",
			machine:           newTestMachine("test-machine", "10.0.0.1", 2222, nil),
			checkErr:          nil,
			expectedPhase:     machinav1alpha2.MachinePhaseReady,
			expectedRequeue:   RequeueAfterReady,
			expectedCondition: metav1.ConditionTrue,
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
				NamespacedName: types.NamespacedName{
					Name: "test-machine",
				},
			}

			result, err := reconciler.Reconcile(context.Background(), req)

			if tt.expectNotFound {
				require.NoError(t, err)
				require.Equal(t, ctrl.Result{}, result)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.expectedRequeue, result.RequeueAfter)

			var updated machinav1alpha2.Machine

			err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
			require.NoError(t, err)

			require.Equal(t, tt.expectedPhase, updated.Status.Phase)
			require.NotNil(t, updated.Status.LastProbeTime)

			require.Len(t, updated.Status.Conditions, 1)
			require.Equal(t, "Ready", updated.Status.Conditions[0].Type)
			require.Equal(t, tt.expectedCondition, updated.Status.Conditions[0].Status)
		})
	}
}

// ---------------------------------------------------------------------------
// Provisioning flow tests
// ---------------------------------------------------------------------------

func TestMachineReconciler_Provisioning_Success(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")
	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	sshSecret := newSSHKeySecret("ssh-key-secret")
	bootstrapSecret := newBootstrapTokenSecret("bootstrap-token-abc123")

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshSecret, bootstrapSecret).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
		Provisioner:         provisioner,
		ClusterInfo: &ClusterInfo{
			APIServer:    "api.example.com:443",
			CACertBase64: "dGVzdC1jYQ==",
			ClusterDNS:   "10.0.0.10",
			ClusterRG:    "mc_rg",
			KubeVersion:  "v1.34.2",
		},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	// First reconcile: sets Provisioning and then calls provisioner.
	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.True(t, provisioner.called, "provisioner should have been called")
	require.Equal(t, "abc123.def456ghi789jkl0", provisioner.bootstrapToken)
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)

	// Check machine ended up Provisioned.
	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, updated.Status.Phase)
	require.Equal(t, int64(1), updated.Status.ProvisionedModelGeneration)
}

func TestMachineReconciler_Provisioning_ModelNotFound(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	// Machine references a model that doesn't exist.
	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "missing-model"})

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)

	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, machinav1alpha2.MachinePhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "not found")
}

func TestMachineReconciler_Provisioning_ProvisionerFails(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")
	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	sshSecret := newSSHKeySecret("ssh-key-secret")
	bootstrapSecret := newBootstrapTokenSecret("bootstrap-token-abc123")

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshSecret, bootstrapSecret).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{err: fmt.Errorf("SSH connection refused")}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)

	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, machinav1alpha2.MachinePhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "SSH connection refused")
}

func TestMachineReconciler_Provisioning_AlreadyProvisionedSameGeneration(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")
	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine.Status = machinav1alpha2.MachineStatus{
		Phase:                      machinav1alpha2.MachinePhaseProvisioned,
		ProvisionedModelGeneration: 1, // Same as model.Generation
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
		Provisioner:         provisioner,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.False(t, provisioner.called, "provisioner should NOT be called when already provisioned at same generation")
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)
}

func TestMachineReconciler_Provisioned_NodeJoinInsteadOfReProvision(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")
	model.Generation = 2 // Model has been updated

	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine.Status = machinav1alpha2.MachineStatus{
		Phase:                      machinav1alpha2.MachinePhaseProvisioned,
		ProvisionedModelGeneration: 1, // Old generation — but Provisioned routes to reconcileNodeJoin now
	}

	sshSecret := newSSHKeySecret("ssh-key-secret")
	bootstrapSecret := newBootstrapTokenSecret("bootstrap-token-abc123")

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshSecret, bootstrapSecret).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.False(t, provisioner.called, "provisioner should NOT be called — Provisioned phase routes to Node join")
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)

	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, updated.Status.Phase)
	require.Equal(t, int64(1), updated.Status.ProvisionedModelGeneration, "generation should NOT be updated")
}

func TestMachineReconciler_Provisioning_NoKubernetesProfile(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")
	model.Spec.KubernetesProfile = nil // No kubernetes profile

	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	sshSecret := newSSHKeySecret("ssh-key-secret")

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshSecret).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.True(t, provisioner.called)
	require.Equal(t, "", provisioner.bootstrapToken, "bootstrap token should be empty when no kubernetes profile")
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)
}

func TestMachineReconciler_Provisioning_SSHKeyNotFound(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")
	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	// No SSH key secret created.

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)

	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, machinav1alpha2.MachinePhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "SSH config")
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
			ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "machina-system"},
			Data:       map[string][]byte{"custom-key": []byte("secret-value")},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		val, err := reconciler.getSecretValue(context.Background(),
			&machinav1alpha2.SecretKeySelector{Name: "my-secret", Key: "custom-key"})
		require.NoError(t, err)
		require.Equal(t, "secret-value", val)
	})

	t.Run("defaults to ssh-privatekey key", func(t *testing.T) {
		t.Parallel()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "machina-system"},
			Data:       map[string][]byte{"ssh-privatekey": []byte("my-key")},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		val, err := reconciler.getSecretValue(context.Background(),
			&machinav1alpha2.SecretKeySelector{Name: "my-secret"})
		require.NoError(t, err)
		require.Equal(t, "my-key", val)
	})

	t.Run("returns error when secret not found", func(t *testing.T) {
		t.Parallel()

		fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		_, err := reconciler.getSecretValue(context.Background(),
			&machinav1alpha2.SecretKeySelector{Name: "missing-secret"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing-secret")
	})

	t.Run("returns error when key not found in secret", func(t *testing.T) {
		t.Parallel()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "machina-system"},
			Data:       map[string][]byte{"other-key": []byte("value")},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		_, err := reconciler.getSecretValue(context.Background(),
			&machinav1alpha2.SecretKeySelector{Name: "my-secret", Key: "missing-key"})
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

		sshSecret := newSSHKeySecret("ssh-key-secret")

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(sshSecret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		model := newTestModel("test-model")

		cfg, err := reconciler.buildSSHConfig(context.Background(), model)
		require.NoError(t, err)
		require.Equal(t, "testuser", cfg.User)
		require.Equal(t, SSHConnectTimeout, cfg.Timeout)
		require.Len(t, cfg.Auth, 1)
	})

	t.Run("defaults to azureuser when username is empty", func(t *testing.T) {
		t.Parallel()

		sshSecret := newSSHKeySecret("ssh-key-secret")

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(sshSecret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		model := newTestModel("test-model")
		model.Spec.SSHUsername = ""

		cfg, err := reconciler.buildSSHConfig(context.Background(), model)
		require.NoError(t, err)
		require.Equal(t, "azureuser", cfg.User)
	})

	t.Run("returns error when SSH key secret not found", func(t *testing.T) {
		t.Parallel()

		fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		model := newTestModel("test-model")

		_, err := reconciler.buildSSHConfig(context.Background(), model)
		require.Error(t, err)
		require.Contains(t, err.Error(), "SSH private key")
	})

	t.Run("returns error when private key is invalid", func(t *testing.T) {
		t.Parallel()

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "ssh-key-secret", Namespace: "machina-system"},
			Data:       map[string][]byte{"ssh-privatekey": []byte("not-a-valid-key")},
		}

		fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
		reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

		model := newTestModel("test-model")

		_, err := reconciler.buildSSHConfig(context.Background(), model)
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
		err := checker.CheckReachable(context.Background(), "127.0.0.1", int32(port))

		require.NoError(t, err)
	})

	t.Run("unreachable when no listener", func(t *testing.T) {
		t.Parallel()

		checker := &DefaultReachabilityChecker{Timeout: 100 * time.Millisecond}
		err := checker.CheckReachable(context.Background(), "127.0.0.1", 59999)

		require.Error(t, err)
		require.Contains(t, err.Error(), "TCP dial 127.0.0.1:59999")
	})

	t.Run("unreachable on invalid IP", func(t *testing.T) {
		t.Parallel()

		checker := &DefaultReachabilityChecker{Timeout: 100 * time.Millisecond}
		err := checker.CheckReachable(context.Background(), "invalid-ip", 22)

		require.Error(t, err)
		require.Contains(t, err.Error(), "TCP dial invalid-ip:22")
	})

	t.Run("uses default timeout when not specified", func(t *testing.T) {
		t.Parallel()

		port, cleanup := startTCPListener(t)
		defer cleanup()

		checker := &DefaultReachabilityChecker{}
		err := checker.CheckReachable(context.Background(), "127.0.0.1", int32(port))

		require.NoError(t, err)
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		checker := &DefaultReachabilityChecker{Timeout: 5 * time.Second}
		err := checker.CheckReachable(ctx, "127.0.0.1", 22)

		require.Error(t, err)
	})
}

// ---------------------------------------------------------------------------
// UpdateStatus tests
// ---------------------------------------------------------------------------

func TestMachineReconciler_UpdateStatus_ConditionUpdate(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1", 22, nil)
	machine.Status = machinav1alpha2.MachineStatus{
		Phase: machinav1alpha2.MachinePhasePending,
		Conditions: []metav1.Condition{
			{
				Type:               "Ready",
				Status:             metav1.ConditionFalse,
				Reason:             "Pending",
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
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-machine"},
	}

	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)

	// Should still have only one condition (updated, not added).
	require.Len(t, updated.Status.Conditions, 1)
	require.Equal(t, "Ready", updated.Status.Conditions[0].Type)
	require.Equal(t, metav1.ConditionTrue, updated.Status.Conditions[0].Status)
	require.Equal(t, "Ready", updated.Status.Conditions[0].Reason)
}

func TestMachineReconciler_UpdateStatus_PhaseDeterminesRequeue(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	tests := []struct {
		name            string
		phase           machinav1alpha2.MachinePhase
		expectedRequeue time.Duration
	}{
		{"Ready requeues at Ready interval", machinav1alpha2.MachinePhaseReady, RequeueAfterReady},
		{"Provisioned requeues at Provisioned interval", machinav1alpha2.MachinePhaseProvisioned, RequeueAfterProvisioned},
		{"Failed requeues at Failed interval", machinav1alpha2.MachinePhaseFailed, RequeueAfterFailed},
		{"Pending requeues at Pending interval", machinav1alpha2.MachinePhasePending, RequeueAfterPending},
		{"Joined requeues at Joined interval", machinav1alpha2.MachinePhaseJoined, RequeueAfterJoined},
		{"Orphaned requeues at Orphaned interval", machinav1alpha2.MachinePhaseOrphaned, RequeueAfterOrphaned},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			machine := newTestMachine("test-machine", "10.0.0.1", 22, nil)

			fakeClient := fake.NewClientBuilder().
				WithScheme(s).
				WithObjects(machine).
				WithStatusSubresource(machine).
				Build()

			reconciler := &MachineReconciler{
				Client: fakeClient,
				Scheme: s,
			}

			result, err := reconciler.updateStatus(context.Background(), machine, tt.phase, "test message", 0)
			require.NoError(t, err)
			require.Equal(t, tt.expectedRequeue, result.RequeueAfter)
		})
	}
}

// ---------------------------------------------------------------------------
// findMachinesForModel tests
// ---------------------------------------------------------------------------

func TestFindMachinesForModel(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")

	machine1 := newTestMachine("machine-1", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine2 := newTestMachine("machine-2", "10.0.0.2", 22,
		&machinav1alpha2.LocalObjectReference{Name: "other-model"})
	machine3 := newTestMachine("machine-3", "10.0.0.3", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine4 := newTestMachine("machine-4", "10.0.0.4", 22, nil) // no modelRef

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine1, machine2, machine3, machine4).
		Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
	}

	requests := reconciler.findMachinesForModel(context.Background(), model)
	require.Len(t, requests, 2)

	names := make(map[string]bool)
	for _, req := range requests {
		names[req.Name] = true
	}

	require.True(t, names["machine-1"])
	require.True(t, names["machine-3"])
	require.False(t, names["machine-2"])
	require.False(t, names["machine-4"])
}

func TestFindMachinesForModel_DifferentModelName(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")

	// Machine referencing a different model name should NOT be included.
	machine := newTestMachine("machine-1", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "other-model"})

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
	}

	requests := reconciler.findMachinesForModel(context.Background(), model)
	require.Len(t, requests, 0)
}

// ---------------------------------------------------------------------------
// Reachability checker captures correct args
// ---------------------------------------------------------------------------

func TestMachineReconciler_CheckerCalledWithCorrectArgs(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	t.Run("uses specified port", func(t *testing.T) {
		t.Parallel()

		machine := newTestMachine("test-machine", "10.0.0.1", 2222, nil)

		fakeClient := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(machine).
			WithStatusSubresource(machine).
			Build()

		checker := &mockReachabilityChecker{err: nil}
		reconciler := &MachineReconciler{
			Client:              fakeClient,
			Scheme:              s,
			ReachabilityChecker: checker,
		}

		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}
		_, err := reconciler.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, "10.0.0.1", checker.calledHost)
		require.Equal(t, int32(2222), checker.calledPort)
	})

	t.Run("defaults port to 22", func(t *testing.T) {
		t.Parallel()

		machine := newTestMachine("test-machine", "10.0.0.1", 0, nil)

		fakeClient := fake.NewClientBuilder().
			WithScheme(s).
			WithObjects(machine).
			WithStatusSubresource(machine).
			Build()

		checker := &mockReachabilityChecker{err: nil}
		reconciler := &MachineReconciler{
			Client:              fakeClient,
			Scheme:              s,
			ReachabilityChecker: checker,
		}

		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}
		_, err := reconciler.Reconcile(context.Background(), req)
		require.NoError(t, err)
		require.Equal(t, int32(22), checker.calledPort)
	})
}

// ---------------------------------------------------------------------------
// Provisioning phase gate tests
// ---------------------------------------------------------------------------

func TestMachineReconciler_ProvisioningPhaseIsNotReProvisionable(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")
	model.Generation = 2

	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine.Status = machinav1alpha2.MachineStatus{
		Phase: machinav1alpha2.MachinePhaseProvisioning,
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
		Provisioner:         provisioner,
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.False(t, provisioner.called, "provisioner should not be called when already Provisioning")
	require.Equal(t, RequeueAfterPending, result.RequeueAfter)
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
}

// ---------------------------------------------------------------------------
// Scheme registration test
// ---------------------------------------------------------------------------

func TestSchemeRegistration(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	// Verify Machine and MachineModel are registered.
	gvks, _, err := s.ObjectKinds(&machinav1alpha2.Machine{})
	require.NoError(t, err)
	require.Len(t, gvks, 1)
	require.Equal(t, "Machine", gvks[0].Kind)
	require.Equal(t, "v1alpha2", gvks[0].Version)

	gvks, _, err = s.ObjectKinds(&machinav1alpha2.MachineModel{})
	require.NoError(t, err)
	require.Len(t, gvks, 1)
	require.Equal(t, "MachineModel", gvks[0].Kind)
	require.Equal(t, "v1alpha2", gvks[0].Version)
}

// ---------------------------------------------------------------------------
// Edge case: machine with modelRef but missing bootstrap token secret
// ---------------------------------------------------------------------------

func TestMachineReconciler_Provisioning_BootstrapTokenSecretMissing(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")
	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	sshSecret := newSSHKeySecret("ssh-key-secret")
	// No bootstrap token secret created.

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshSecret).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)

	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, machinav1alpha2.MachinePhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "bootstrap token")
}

// ---------------------------------------------------------------------------
// findMachinesForModel with list error
// ---------------------------------------------------------------------------

func TestFindMachinesForModel_ListError(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	// Use a scheme without Machine registered to force a list error.
	emptyScheme := runtime.NewScheme()

	model := newTestModel("gpu-model")

	fakeClient := fake.NewClientBuilder().WithScheme(emptyScheme).Build()
	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
	}

	requests := reconciler.findMachinesForModel(context.Background(), model)
	require.Nil(t, requests)
}

// ---------------------------------------------------------------------------
// Provisioning from Failed phase (retry)
// ---------------------------------------------------------------------------

func TestMachineReconciler_Provisioning_RetryFromFailed(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")
	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine.Status = machinav1alpha2.MachineStatus{
		Phase:   machinav1alpha2.MachinePhaseFailed,
		Message: "Previous provisioning failed",
	}

	sshSecret := newSSHKeySecret("ssh-key-secret")
	bootstrapSecret := newBootstrapTokenSecret("bootstrap-token-abc123")

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshSecret, bootstrapSecret).
		WithStatusSubresource(machine).
		Build()

	provisioner := &mockProvisioner{}
	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.True(t, provisioner.called, "provisioner should be called to retry from Failed phase")
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Multiple machines - only matching ones get enqueued
// ---------------------------------------------------------------------------

func TestFindMachinesForModel_NoMatchingMachines(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	model := newTestModel("gpu-model")

	machine1 := newTestMachine("machine-1", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "other-model"})
	machine2 := newTestMachine("machine-2", "10.0.0.2", 22, nil)

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine1, machine2).
		Build()

	reconciler := &MachineReconciler{Client: fakeClient, Scheme: s}

	requests := reconciler.findMachinesForModel(context.Background(), model)
	require.Len(t, requests, 0)
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
				Labels: map[string]string{MachinaNodeLabel: "my-machine"},
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

func TestReconcileNodeJoin_Provisioned_NoNode(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine.Status = machinav1alpha2.MachineStatus{
		Phase:                      machinav1alpha2.MachinePhaseProvisioned,
		ProvisionedModelGeneration: 1,
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(machine).
		Build()

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)

	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, updated.Status.Phase)
}

func TestReconcileNodeJoin_Provisioned_NodeFound(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine.Status = machinav1alpha2.MachineStatus{
		Phase:                      machinav1alpha2.MachinePhaseProvisioned,
		ProvisionedModelGeneration: 1,
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "worker-1",
			Labels: map[string]string{MachinaNodeLabel: "test-machine"},
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
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterJoined, result.RequeueAfter)

	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, machinav1alpha2.MachinePhaseJoined, updated.Status.Phase)
	require.NotNil(t, updated.Status.NodeRef)
	require.Equal(t, "worker-1", updated.Status.NodeRef.Name)
}

func TestReconcileNodeJoin_Joined_NodeStillExists(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine.Status = machinav1alpha2.MachineStatus{
		Phase:                      machinav1alpha2.MachinePhaseJoined,
		ProvisionedModelGeneration: 1,
		NodeRef:                    &machinav1alpha2.NodeReference{Name: "worker-1"},
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "worker-1",
			Labels: map[string]string{MachinaNodeLabel: "test-machine"},
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
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterJoined, result.RequeueAfter)
}

func TestReconcileNodeJoin_Joined_NodeDisappears(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine.Status = machinav1alpha2.MachineStatus{
		Phase:                      machinav1alpha2.MachinePhaseJoined,
		ProvisionedModelGeneration: 1,
		NodeRef:                    &machinav1alpha2.NodeReference{Name: "worker-1"},
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
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterOrphaned, result.RequeueAfter)

	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, machinav1alpha2.MachinePhaseOrphaned, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "Node disappeared")
}

func TestReconcileNodeJoin_Orphaned_NodeReappears(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine.Status = machinav1alpha2.MachineStatus{
		Phase:                      machinav1alpha2.MachinePhaseOrphaned,
		ProvisionedModelGeneration: 1,
		NodeRef:                    &machinav1alpha2.NodeReference{Name: "worker-1"},
	}

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "worker-1",
			Labels: map[string]string{MachinaNodeLabel: "test-machine"},
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
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterJoined, result.RequeueAfter)

	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, machinav1alpha2.MachinePhaseJoined, updated.Status.Phase)
	require.NotNil(t, updated.Status.NodeRef)
	require.Equal(t, "worker-1", updated.Status.NodeRef.Name)
}

func TestReconcileNodeJoin_Orphaned_StillOrphaned(t *testing.T) {
	t.Parallel()

	s := newTestScheme(t)

	machine := newTestMachine("test-machine", "10.0.0.1", 22,
		&machinav1alpha2.LocalObjectReference{Name: "gpu-model"})
	machine.Status = machinav1alpha2.MachineStatus{
		Phase:                      machinav1alpha2.MachinePhaseOrphaned,
		ProvisionedModelGeneration: 1,
		NodeRef:                    &machinav1alpha2.NodeReference{Name: "worker-1"},
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
		ReachabilityChecker: &mockReachabilityChecker{err: nil},
	}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "test-machine"}}

	result, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, RequeueAfterOrphaned, result.RequeueAfter)

	var updated machinav1alpha2.Machine

	err = fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, machinav1alpha2.MachinePhaseOrphaned, updated.Status.Phase)
}

// Ensure fake client is used for interface compatibility check.
var _ client.Client = (fake.NewClientBuilder().Build())
