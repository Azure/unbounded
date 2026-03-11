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

	machinav1alpha2 "github.com/project-unbounded/unbounded-kube/cmd/machina/machina/api/v1alpha2"
)

// ---------------------------------------------------------------------------
// Integration test helpers
// ---------------------------------------------------------------------------

// reconcileHelper reconciles a machine and returns the updated machine object.
func reconcileHelper(
	t *testing.T,
	reconciler *MachineReconciler,
	name string,
) (ctrl.Result, *machinav1alpha2.Machine) {
	t.Helper()

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name},
	})
	require.NoError(t, err)

	var machine machinav1alpha2.Machine

	err = reconciler.Get(context.Background(), client.ObjectKey{Name: name}, &machine)
	require.NoError(t, err)

	return result, &machine
}

// newIntegrationScheme creates a scheme with v1alpha2 and core types registered.
func newIntegrationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, machinav1alpha2.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))

	return s
}

// ---------------------------------------------------------------------------
// Lifecycle: Pending -> Ready (no modelRef)
// ---------------------------------------------------------------------------

func TestIntegration_PendingToReady_NoModel(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH: machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
	}

	// Reconcile 1: Machine is reachable, no modelRef -> Ready.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseReady, m.Status.Phase)
	require.Equal(t, "Machine is reachable", m.Status.Message)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)

	// Reconcile 2: Still reachable, still no modelRef -> stays Ready.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseReady, m.Status.Phase)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Pending -> Ready -> Provisioning -> Provisioned
// ---------------------------------------------------------------------------

func TestIntegration_FullProvisioningLifecycle(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "model-1"},
		},
	}

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "model-1",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "#!/bin/bash\necho hello",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshKeySecret).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{KubeVersion: "v1.34.0"},
	}

	// Reconcile 1: Reachable + modelRef -> provisions and becomes Provisioned.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.True(t, provisioner.called, "provisioner should have been called")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
	require.Equal(t, "Machine provisioned successfully", m.Status.Message)
	require.Equal(t, int64(1), m.Status.ProvisionedModelGeneration)
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)

	// Verify Ready condition is True.
	require.NotEmpty(t, m.Status.Conditions)

	var readyCond *metav1.Condition

	for i := range m.Status.Conditions {
		if m.Status.Conditions[i].Type == "Ready" {
			readyCond = &m.Status.Conditions[i]
			break
		}
	}

	require.NotNil(t, readyCond)
	require.Equal(t, metav1.ConditionTrue, readyCond.Status)

	// Reconcile 2: Phase is Provisioned -> routes to reconcileNodeJoin.
	// No Node with matching label exists, so stays Provisioned and requeues.
	provisioner.called = false
	result, m = reconcileHelper(t, reconciler, "m1")
	require.False(t, provisioner.called, "should not re-provision for same generation")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Unreachable -> stays Pending until reachable
// ---------------------------------------------------------------------------

func TestIntegration_UnreachableThenReachable(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH: machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: fmt.Errorf("connection refused")}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
	}

	// Reconcile 1: Unreachable -> Pending.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhasePending, m.Status.Phase)
	require.Contains(t, m.Status.Message, "not reachable")
	require.Equal(t, RequeueAfterPending, result.RequeueAfter)

	// Reconcile 2: Still unreachable -> still Pending.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhasePending, m.Status.Phase)

	// Machine becomes reachable.
	checker.err = nil

	// Reconcile 3: Now reachable -> Ready.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseReady, m.Status.Phase)
	require.Equal(t, RequeueAfterReady, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Provisioned -> model generation changes are not acted upon
// while in Node-lifecycle phases (manual intervention required)
// ---------------------------------------------------------------------------

func TestIntegration_ReProvisionOnModelGenerationChange(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "model-1"},
		},
	}

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "model-1",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "#!/bin/bash\necho v1",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshKeySecret).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	// Reconcile 1: Initial provisioning.
	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
	require.Equal(t, int64(1), m.Status.ProvisionedModelGeneration)
	require.True(t, provisioner.called)

	// Simulate model update: bump generation.
	var currentModel machinav1alpha2.MachineModel
	require.NoError(t, fakeClient.Get(context.Background(),
		client.ObjectKey{Name: "model-1"}, &currentModel))
	currentModel.Generation = 2
	currentModel.Spec.AgentInstallScript = "#!/bin/bash\necho v2"
	require.NoError(t, fakeClient.Update(context.Background(), &currentModel))

	// Reconcile 2: Machine is Provisioned, so Reconcile routes to
	// reconcileNodeJoin, NOT reconcileProvisioning. The model generation
	// change is not acted upon while in a Node-lifecycle phase.
	provisioner.called = false
	result, m := reconcileHelper(t, reconciler, "m1")
	require.False(t, provisioner.called, "should not re-provision while in Provisioned phase (Node-lifecycle)")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)

	// Reconcile 3: Still Provisioned, still goes through Node join check.
	provisioner.called = false
	result, m = reconcileHelper(t, reconciler, "m1")
	require.False(t, provisioner.called)
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Provisioning failure -> Failed -> retry succeeds
// ---------------------------------------------------------------------------

func TestIntegration_ProvisioningFailureThenRetry(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "model-1"},
		},
	}

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "model-1",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "#!/bin/bash\nexit 1",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshKeySecret).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}
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
	require.Equal(t, machinav1alpha2.MachinePhaseFailed, m.Status.Phase)
	require.Contains(t, m.Status.Message, "Provisioning failed")
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)
	require.True(t, provisioner.called)

	// Verify Ready condition is False.
	var readyCond *metav1.Condition

	for i := range m.Status.Conditions {
		if m.Status.Conditions[i].Type == "Ready" {
			readyCond = &m.Status.Conditions[i]
			break
		}
	}

	require.NotNil(t, readyCond)
	require.Equal(t, metav1.ConditionFalse, readyCond.Status)

	// Fix the provisioner.
	provisioner.err = nil
	provisioner.called = false

	// Reconcile 2: Retry succeeds -> Provisioned.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.True(t, provisioner.called, "provisioner should be called on retry from Failed")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
	require.Equal(t, int64(1), m.Status.ProvisionedModelGeneration)
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Model deleted while machine references it
// ---------------------------------------------------------------------------

func TestIntegration_ModelDeletedWhileReferenced(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "model-gone"},
		},
	}

	// No model object created — simulates deletion.
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
	}

	// Reconcile: Model not found -> Failed.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseFailed, m.Status.Phase)
	require.Contains(t, m.Status.Message, `"model-gone" not found`)
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Add modelRef to an already Ready machine
// ---------------------------------------------------------------------------

func TestIntegration_AddModelRefToReadyMachine(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH: machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
		},
	}

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "model-1",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "#!/bin/bash\necho install",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshKeySecret).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	// Reconcile 1: No modelRef -> Ready.
	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseReady, m.Status.Phase)
	require.False(t, provisioner.called)

	// Add modelRef.
	m.Spec.ModelRef = &machinav1alpha2.LocalObjectReference{Name: "model-1"}
	require.NoError(t, fakeClient.Update(context.Background(), m))

	// Reconcile 2: Now has modelRef + reachable -> provisions.
	provisioner.called = false
	_, m = reconcileHelper(t, reconciler, "m1")
	require.True(t, provisioner.called, "should provision after modelRef is added")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
	require.Equal(t, int64(1), m.Status.ProvisionedModelGeneration)
}

// ---------------------------------------------------------------------------
// Lifecycle: Machine becomes unreachable during steady-state
// ---------------------------------------------------------------------------

func TestIntegration_ProvisionedMachineBecomesUnreachable(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "model-1"},
		},
	}

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "model-1",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "#!/bin/bash\necho hello",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshKeySecret).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	// Reconcile 1: Provision successfully.
	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)

	// Machine becomes unreachable.
	checker.err = fmt.Errorf("no route to host")

	// Reconcile 2: Unreachable -> Pending.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhasePending, m.Status.Phase)
	require.Contains(t, m.Status.Message, "not reachable")
	require.Equal(t, RequeueAfterPending, result.RequeueAfter)
	// Note: ProvisionedModelGeneration is NOT cleared; it's preserved.
	require.Equal(t, int64(1), m.Status.ProvisionedModelGeneration)

	// Machine comes back.
	checker.err = nil
	provisioner.called = false

	// Reconcile 3: Reachable again + phase is Pending (not Provisioned/Joined/
	// Orphaned) so it goes through reconcileProvisioning -> re-provisions.
	_, m = reconcileHelper(t, reconciler, "m1")
	require.True(t, provisioner.called, "should re-provision after coming back online")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
}

// ---------------------------------------------------------------------------
// Lifecycle: Multiple machines sharing the same model
// ---------------------------------------------------------------------------

func TestIntegration_MultipleMachinesSameModel(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "shared-model",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "#!/bin/bash\necho install",
		},
	}

	m1 := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "shared-model"},
		},
	}
	m2 := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m2"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.2", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "shared-model"},
		},
	}
	m3 := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m3"},
		Spec: machinav1alpha2.MachineSpec{
			SSH: machinav1alpha2.MachineSSHSpec{Host: "10.0.0.3", Port: 22},
			// No modelRef — should stay Ready.
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(model, m1, m2, m3, sshKeySecret).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}
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
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, machine1.Status.Phase)

	provisioner.called = false
	_, machine2 := reconcileHelper(t, reconciler, "m2")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, machine2.Status.Phase)
	require.True(t, provisioner.called)

	provisioner.called = false
	_, machine3 := reconcileHelper(t, reconciler, "m3")
	require.Equal(t, machinav1alpha2.MachinePhaseReady, machine3.Status.Phase)
	require.False(t, provisioner.called)

	// Verify findMachinesForModel returns m1 and m2 but not m3.
	requests := reconciler.findMachinesForModel(context.Background(), model)
	require.Len(t, requests, 2)

	names := map[string]bool{}
	for _, req := range requests {
		names[req.Name] = true
	}

	require.True(t, names["m1"])
	require.True(t, names["m2"])
}

// ---------------------------------------------------------------------------
// Lifecycle: Provisioning phase blocks re-provisioning
// ---------------------------------------------------------------------------

func TestIntegration_ProvisioningPhaseBlocksReProvision(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)

	// Create a machine already in Provisioning phase.
	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "model-1"},
		},
		Status: machinav1alpha2.MachineStatus{
			Phase: machinav1alpha2.MachinePhaseProvisioning,
		},
	}

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "model-1",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "#!/bin/bash\necho test",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}
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
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}

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

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1"},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "model-1"},
		},
	}

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "model-1",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "echo test",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshKeySecret).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
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

	getReadyCondition := func(m *machinav1alpha2.Machine) *metav1.Condition {
		for i := range m.Status.Conditions {
			if m.Status.Conditions[i].Type == "Ready" {
				return &m.Status.Conditions[i]
			}
		}

		return nil
	}

	// Step 1: Pending -> Ready condition is False.
	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhasePending, m.Status.Phase)
	cond := getReadyCondition(m)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, "Pending", cond.Reason)

	// Step 2: Becomes reachable, provisions -> Provisioned, Ready condition is True.
	checker.err = nil
	_, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
	cond = getReadyCondition(m)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "Provisioned", cond.Reason)

	// Step 3: Becomes unreachable -> Pending, condition back to False.
	checker.err = fmt.Errorf("down again")
	_, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhasePending, m.Status.Phase)
	cond = getReadyCondition(m)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, "Pending", cond.Reason)
}

// ---------------------------------------------------------------------------
// Lifecycle: KubernetesProfile with bootstrap token
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

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "model-1",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "#!/bin/bash\ninstall",
			KubernetesProfile: &machinav1alpha2.KubernetesProfile{
				Version: "1.34.0",
				BootstrapTokenRef: machinav1alpha2.LocalObjectReference{
					Name: "bootstrap-token-abc123",
				},
			},
		},
	}

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "model-1"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, bootstrapSecret, sshKeySecret).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{KubeVersion: "v1.33.0"},
	}

	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
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

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "model-1",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "#!/bin/bash\ninstall",
			KubernetesProfile: &machinav1alpha2.KubernetesProfile{
				Version: "1.34.0",
				BootstrapTokenRef: machinav1alpha2.LocalObjectReference{
					Name: "bootstrap-token-missing",
				},
			},
		},
	}

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "model-1"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshKeySecret).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseFailed, m.Status.Phase)
	require.Contains(t, m.Status.Message, "bootstrap token")
	require.False(t, provisioner.called, "provisioner should not be called when token is missing")
	require.Equal(t, RequeueAfterFailed, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Provisioned -> Joined -> Orphaned -> Joined (Node lifecycle)
// ---------------------------------------------------------------------------

func TestIntegration_ProvisionedToJoinedToOrphaned(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "model-1"},
		},
	}

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "model-1",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "#!/bin/bash\necho hello",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshKeySecret).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	ctx := context.Background()

	// Reconcile 1: provisions -> Provisioned.
	result, m := reconcileHelper(t, reconciler, "m1")
	require.True(t, provisioner.called)
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)

	// Reconcile 2: Provisioned, no Node -> stays Provisioned (requeue 30s).
	provisioner.called = false
	result, m = reconcileHelper(t, reconciler, "m1")
	require.False(t, provisioner.called)
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)
	require.Equal(t, RequeueAfterProvisioned, result.RequeueAfter)

	// Create a Node with matching label.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{MachinaNodeLabel: "m1"},
		},
	}
	require.NoError(t, fakeClient.Create(ctx, node))

	// Reconcile 3: Provisioned + Node found -> Joined, nodeRef set, requeue 5m.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseJoined, m.Status.Phase)
	require.NotNil(t, m.Status.NodeRef)
	require.Equal(t, "node-1", m.Status.NodeRef.Name)
	require.Equal(t, RequeueAfterJoined, result.RequeueAfter)

	// Reconcile 4: Joined + Node still exists -> stays Joined.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseJoined, m.Status.Phase)
	require.Equal(t, RequeueAfterJoined, result.RequeueAfter)

	// Delete the Node.
	require.NoError(t, fakeClient.Delete(ctx, node))

	// Reconcile 5: Joined + Node gone -> Orphaned, requeue 1m.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseOrphaned, m.Status.Phase)
	require.Equal(t, RequeueAfterOrphaned, result.RequeueAfter)

	// Recreate the Node.
	node = &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{MachinaNodeLabel: "m1"},
		},
	}
	require.NoError(t, fakeClient.Create(ctx, node))

	// Reconcile 6: Orphaned + Node found -> Joined again.
	result, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseJoined, m.Status.Phase)
	require.NotNil(t, m.Status.NodeRef)
	require.Equal(t, "node-1", m.Status.NodeRef.Name)
	require.Equal(t, RequeueAfterJoined, result.RequeueAfter)
}

// ---------------------------------------------------------------------------
// Lifecycle: Joined machine becomes unreachable -> Pending
// ---------------------------------------------------------------------------

func TestIntegration_JoinedMachineBecomesUnreachable(t *testing.T) {
	t.Parallel()

	s := newIntegrationScheme(t)
	sshKeySecret := newSSHKeySecret("key")

	machine := &machinav1alpha2.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "m1"},
		Spec: machinav1alpha2.MachineSpec{
			SSH:      machinav1alpha2.MachineSSHSpec{Host: "10.0.0.1", Port: 22},
			ModelRef: &machinav1alpha2.LocalObjectReference{Name: "model-1"},
		},
	}

	model := &machinav1alpha2.MachineModel{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "model-1",
			Generation: 1,
		},
		Spec: machinav1alpha2.MachineModelSpec{
			SSHUsername:        "user",
			SSHPrivateKeyRef:   machinav1alpha2.SecretKeySelector{Name: "key"},
			AgentInstallScript: "#!/bin/bash\necho hello",
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(machine, model, sshKeySecret).
		WithStatusSubresource(&machinav1alpha2.Machine{}).
		Build()

	checker := &mockReachabilityChecker{err: nil}
	provisioner := &mockProvisioner{err: nil}

	reconciler := &MachineReconciler{
		Client:              fakeClient,
		Scheme:              s,
		ReachabilityChecker: checker,
		Provisioner:         provisioner,
		ClusterInfo:         &ClusterInfo{},
	}

	ctx := context.Background()

	// Reconcile 1: Provision.
	_, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseProvisioned, m.Status.Phase)

	// Create Node and reconcile to Joined.
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-1",
			Labels: map[string]string{MachinaNodeLabel: "m1"},
		},
	}
	require.NoError(t, fakeClient.Create(ctx, node))

	_, m = reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhaseJoined, m.Status.Phase)

	// Machine becomes unreachable.
	checker.err = fmt.Errorf("connection refused")

	// Reconcile: Reachability check fails -> Pending (takes precedence over Node join).
	result, m := reconcileHelper(t, reconciler, "m1")
	require.Equal(t, machinav1alpha2.MachinePhasePending, m.Status.Phase)
	require.Contains(t, m.Status.Message, "not reachable")
	require.Equal(t, RequeueAfterPending, result.RequeueAfter)
}
