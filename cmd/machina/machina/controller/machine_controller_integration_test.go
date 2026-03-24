package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	unboundedv1alpha3 "github.com/project-unbounded/unbounded-kube/api/v1alpha3"
)

// ---------------------------------------------------------------------------
// Integration test helpers
// ---------------------------------------------------------------------------

func reconcileHelper(
	t *testing.T,
	reconciler *MachineReconciler,
	name string,
) (ctrl.Result, *unboundedv1alpha3.Machine) {
	t.Helper()

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	require.NoError(t, err)

	var machine unboundedv1alpha3.Machine

	err = reconciler.Get(context.Background(), client.ObjectKey{Name: name}, &machine)
	require.NoError(t, err)

	return result, &machine
}

func newIntegrationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, unboundedv1alpha3.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	return s
}

// ---------------------------------------------------------------------------
// Lifecycle: Pending -> Ready (no kubernetes config)
// ---------------------------------------------------------------------------

func TestIntegration_PendingToReady_NoKubernetes(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "ssh-key"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
	}

	// Reconcile 1: Machine is reachable, no kubernetes config -> Ready.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseReady, m.Status.Phase)
	require.Equal(t, "Machine is reachable", m.Status.Message)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)

	// Reconcile 2: Still reachable, still no kubernetes config -> stays Ready.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseReady, m.Status.Phase)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Pending -> Provisioning -> Joining
// ---------------------------------------------------------------------------

func TestIntegration_FullProvisioningLifecycle(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("supersecret"),
		},
	}

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "v1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshKeySecret, bootstrapSecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{KubeVersion: "v1.34.0"},
	}

	// Reconcile 1: Reachable + kubernetes config -> provisions and becomes Joining.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.True(t, provisioner.called, "provisioner should have been called")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
	require.Contains(t, m.Status.Message, "provisioned successfully")
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)

	// Verify Provisioned condition is True.
	require.NotEmpty(t, m.Status.Conditions)

	var provCond *metav1.Condition

	for i := range m.Status.Conditions {
		if m.Status.Conditions[i].Type == unboundedv1alpha3.MachineConditionProvisioned {
			provCond = &m.Status.Conditions[i]
			break
		}
	}

	require.NotNil(t, provCond)
	require.Equal(t, metav1.ConditionTrue, provCond.Status)

	// Reconcile 2: Phase is Joining -> routes to reconcileNodeJoin.
	// No Node with matching label exists, so stays Joining and requeues.
	provisioner.called = false
	result, m = reconcileHelper(t, reconciler, "m1")
	require.False(t, provisioner.called, "should not re-provision while in Joining phase")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Unreachable -> stays Pending until reachable
// ---------------------------------------------------------------------------

func TestIntegration_UnreachableThenReachable(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "testuser",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "ssh-key"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: fmt.Errorf("connection refused")}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
	}

	// Reconcile 1: Unreachable -> Pending.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhasePending, m.Status.Phase)
	require.Contains(t, m.Status.Message, "not reachable")
	require.Equal(t, RequeueAfterPending, result.RequeueAfter)

	// Reconcile 2: Still unreachable -> still Pending.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhasePending, m.Status.Phase)

	// Machine becomes reachable.
	checker.err = nil

	// Reconcile 3: Now reachable, no kubernetes config -> Ready.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseReady, m.Status.Phase)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Joining -> stays Joining while no Node appears
// (model generation changes are not acted upon while in Node-lifecycle phases)
// ---------------------------------------------------------------------------

func TestIntegration_JoiningStaysJoiningWithoutNode(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("supersecret"),
		},
	}

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "v1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshKeySecret, bootstrapSecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	// Reconcile 1: Initial provisioning -> Joining.
	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
	require.True(t, provisioner.called)

	// Reconcile 2: Machine is Joining, so Reconcile routes to
	// reconcileNodeJoin, NOT reconcileProvisioning.
	provisioner.called = false
	result, m := reconcileHelper(t, reconciler, "m1")
	require.False(t, provisioner.called, "should not re-provision while in Joining phase")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)

	// Reconcile 3: Still Joining, still goes through Node join check.
	provisioner.called = false
	result, m = reconcileHelper(t, reconciler, "m1")
	require.False(t, provisioner.called)
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Provisioning failure -> Failed -> retry succeeds
// ---------------------------------------------------------------------------

func TestIntegration_ProvisioningFailureThenRetry(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("supersecret"),
		},
	}

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "v1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshKeySecret, bootstrapSecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}
	provisioner := &mockProvisioner{err: fmt.Errorf("SSH connection timed out")}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	// Reconcile 1: Provisioner fails -> Failed.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseFailed, m.Status.Phase)
	require.Contains(t, m.Status.Message, "Provisioning failed")
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)
	require.True(t, provisioner.called)

	// Verify SSHReachable condition is True (machine was reachable).
	var sshCond *metav1.Condition

	for i := range m.Status.Conditions {
		if m.Status.Conditions[i].Type == unboundedv1alpha3.MachineConditionSSHReachable {
			sshCond = &m.Status.Conditions[i]
			break
		}
	}

	require.NotNil(t, sshCond)
	require.Equal(t, metav1.ConditionTrue, sshCond.Status)

	// Fix the provisioner.
	provisioner.err = nil
	provisioner.called = false

	// Reconcile 2: Retry succeeds -> Joining.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.True(t, provisioner.called, "provisioner should be called on retry from Failed")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Add kubernetes config to a Ready machine (triggers provisioning)
// ---------------------------------------------------------------------------

func TestIntegration_AddKubernetesConfigToReadyMachine(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("supersecret"),
		},
	}

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshKeySecret, bootstrapSecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	// Reconcile 1: No kubernetes config -> Ready.
	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseReady, m.Status.Phase)
	require.False(t, provisioner.called)

	// Add kubernetes config.
	m.Spec.Kubernetes = &unboundedv1alpha3.KubernetesSpec{
		Version:           "v1.34.0",
		BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
	}
	require.NoError(t, fakeClient.Update(context.Background(), m))

	// Reconcile 2: Now has kubernetes config + reachable -> provisions -> Joining.
	provisioner.called = false
	_, m = reconcileHelper(t, reconciler, "m1")
	require.True(t, provisioner.called, "should provision after kubernetes config is added")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
}

// ---------------------------------------------------------------------------
// Lifecycle: Joining machine becomes unreachable -> Pending
// ---------------------------------------------------------------------------

func TestIntegration_JoiningMachineBecomesUnreachable(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("supersecret"),
		},
	}

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "v1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshKeySecret, bootstrapSecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	// Reconcile 1: Provision successfully -> Joining.
	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)

	// Machine becomes unreachable.
	checker.err = fmt.Errorf("no route to host")

	// Reconcile 2: Unreachable -> Pending.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhasePending, m.Status.Phase)
	require.Contains(t, m.Status.Message, "not reachable")
	require.Equal(t, RequeueAfterPending, result.RequeueAfter)

	// Machine comes back.
	checker.err = nil
	provisioner.called = false

	// Reconcile 3: Reachable again + phase is Pending -> re-provisions.
	_, m = reconcileHelper(t, reconciler, "m1")
	require.True(t, provisioner.called, "should re-provision after coming back online")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
}

// ---------------------------------------------------------------------------
// Lifecycle: Multiple machines, some with kubernetes config, some without
// ---------------------------------------------------------------------------

func TestIntegration_MultipleMachines(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("supersecret"),
		},
	}

	m1 := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "v1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
			},
		},
	}
	m2 := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m2"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.2:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "v1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
			},
		},
	}
	m3 := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m3"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.3:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			// No kubernetes config — should stay Ready.
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(m1, m2, m3, sshKeySecret, bootstrapSecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	// Reconcile all three machines.
	_, machine1 := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, machine1.Status.Phase)

	provisioner.called = false
	_, machine2 := reconcileHelper(t, reconciler, "m2")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, machine2.Status.Phase)
	require.True(t, provisioner.called)

	provisioner.called = false
	_, machine3 := reconcileHelper(t, reconciler, "m3")
	require.Equal(t, unboundedv1alpha3.MachinePhaseReady, machine3.Status.Phase)
	require.False(t, provisioner.called)
}

// ---------------------------------------------------------------------------
// Lifecycle: Provisioning phase blocks re-provisioning
// ---------------------------------------------------------------------------

func TestIntegration_ProvisioningPhaseBlocksReProvision(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("supersecret"),
		},
	}

	// Machine already in Provisioning phase.
	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "v1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
			},
		},
		Status: unboundedv1alpha3.MachineStatus{
			Phase: unboundedv1alpha3.MachinePhaseProvisioning,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, bootstrapSecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	// Reconcile: Machine is already Provisioning -> should not call provisioner.
	result, _ := reconcileHelper(t, reconciler, "m1")
	require.False(t, provisioner.called, "should not re-provision while Provisioning")
	require.Equal(t, RequeueAfterPending, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Machine deleted during reconcile
// ---------------------------------------------------------------------------

func TestIntegration_MachineDeletedReturnsNoError(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)

	// No machine object — simulates deletion.
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "gone"},
	})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
}

// ---------------------------------------------------------------------------
// Lifecycle: Condition transitions across phases
// ---------------------------------------------------------------------------

func TestIntegration_ConditionTransitions(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("supersecret"),
		},
	}

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "v1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshKeySecret, bootstrapSecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: fmt.Errorf("unreachable")}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	getSSHCondition := func(m *unboundedv1alpha3.Machine) *metav1.Condition {
		for i := range m.Status.Conditions {
			if m.Status.Conditions[i].Type == unboundedv1alpha3.MachineConditionSSHReachable {
				return &m.Status.Conditions[i]
			}
		}

		return nil
	}

	// Step 1: Pending -> SSHReachable condition is False.
	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhasePending, m.Status.Phase)
	cond := getSSHCondition(m)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, "Unreachable", cond.Reason)

	// Step 2: Becomes reachable, provisions -> Joining, SSHReachable condition is True.
	checker.err = nil
	_, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
	cond = getSSHCondition(m)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "Reachable", cond.Reason)

	// Step 3: Becomes unreachable -> Pending, condition back to False.
	checker.err = fmt.Errorf("down again")
	_, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhasePending, m.Status.Phase)
	cond = getSSHCondition(m)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, "Unreachable", cond.Reason)
}

// ---------------------------------------------------------------------------
// Lifecycle: Provisioning with bootstrap token
// ---------------------------------------------------------------------------

func TestIntegration_ProvisioningWithBootstrapToken(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("supersecret"),
		},
	}

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, bootstrapSecret, sshKeySecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{KubeVersion: "v1.33.0"},
	}

	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
	require.True(t, provisioner.called)
	require.Equal(t, "abc123.supersecret", provisioner.bootstrapToken)
}

// ---------------------------------------------------------------------------
// Lifecycle: Bootstrap token secret missing -> Failed
// ---------------------------------------------------------------------------

func TestIntegration_BootstrapTokenMissing(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "v1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-missing"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshKeySecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseFailed, m.Status.Phase)
	require.Contains(t, m.Status.Message, "bootstrap token")
	require.False(t, provisioner.called, "provisioner should not be called when token is missing")
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Joining -> Ready -> Joining (Node lifecycle)
// ---------------------------------------------------------------------------

func TestIntegration_JoiningToReadyToJoining(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("supersecret"),
		},
	}

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "v1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshKeySecret, bootstrapSecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	ctx := context.Background()

	// Reconcile 1: provisions -> Joining.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.True(t, provisioner.called)
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)

	// Reconcile 2: Joining, no Node -> stays Joining.
	provisioner.called = false
	result, m = reconcileHelper(t, reconciler, "m1")
	require.False(t, provisioner.called)
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)

	// Create a Node with matching label.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{MachineNodeLabel: "m1"},
		},
	}
	require.NoError(t, fakeClient.Create(ctx, node))

	// Reconcile 3: Joining + Node found -> Ready, nodeRef set.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseReady, m.Status.Phase)
	require.NotNil(t, m.Spec.Kubernetes.NodeRef)
	require.Equal(t, "node-1", m.Spec.Kubernetes.NodeRef.Name)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)

	// Reconcile 4: Ready + Node still exists -> stays Ready.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseReady, m.Status.Phase)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)

	// Delete the Node.
	require.NoError(t, fakeClient.Delete(ctx, node))

	// Reconcile 5: Ready + Node gone -> Joining (waiting for Node to rejoin).
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)
	require.Equal(t, RequeueAfterJoining, result.RequeueAfter)

	// Recreate the Node.
	node = &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{MachineNodeLabel: "m1"},
		},
	}
	require.NoError(t, fakeClient.Create(ctx, node))

	// Reconcile 6: Joining + Node found -> Ready again.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseReady, m.Status.Phase)
	require.NotNil(t, m.Spec.Kubernetes.NodeRef)
	require.Equal(t, "node-1", m.Spec.Kubernetes.NodeRef.Name)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Ready machine becomes unreachable -> Pending
// ---------------------------------------------------------------------------

func TestIntegration_ReadyMachineBecomesUnreachable(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bootstrap-token-abc123",
			Namespace: "kube-system",
		},
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("supersecret"),
		},
	}

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: unboundedv1alpha3.MachineSpec{
			SSH: &unboundedv1alpha3.SSHSpec{
				Host:          "10.0.0.1:22",
				Username:      "user",
				PrivateKeyRef: unboundedv1alpha3.SecretKeySelector{Name: "key"},
			},
			Kubernetes: &unboundedv1alpha3.KubernetesSpec{
				Version:           "v1.34.0",
				BootstrapTokenRef: unboundedv1alpha3.LocalObjectReference{Name: "bootstrap-token-abc123"},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, sshKeySecret, bootstrapSecret).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	checker := &mockReachabilityChecker{}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	ctx := context.Background()

	// Reconcile 1: Provision -> Joining.
	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseJoining, m.Status.Phase)

	// Create Node and reconcile to Ready.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{MachineNodeLabel: "m1"},
		},
	}
	require.NoError(t, fakeClient.Create(ctx, node))

	_, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhaseReady, m.Status.Phase)

	// Machine becomes unreachable.
	checker.err = fmt.Errorf("connection refused")

	// Reconcile: Reachability check fails -> Pending.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, unboundedv1alpha3.MachinePhasePending, m.Status.Phase)
	require.Contains(t, m.Status.Message, "not reachable")
	require.Equal(t, RequeueAfterPending, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Machine without SSH config is skipped
// ---------------------------------------------------------------------------

func TestIntegration_MachineWithoutSSHIsSkipped(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)

	machine := &unboundedv1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec:       unboundedv1alpha3.MachineSpec{},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(&unboundedv1alpha3.Machine{}).
		Build()

	reconciler := &MachineReconciler{
		Client: fakeClient,
		Scheme: s,
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "m1"},
	})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)

	// Verify machine status is unchanged.
	var updated unboundedv1alpha3.Machine
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKey{Name: "m1"}, &updated))
	require.Empty(t, updated.Status.Phase)
}
