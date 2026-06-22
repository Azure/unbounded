// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/redfish"
)

func TestReconcilerCompletesMachineRefPowerOff(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-1", v1alpha3.OperationHostPowerOff)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op).Build()
	power := &recordingPowerClient{states: map[string]redfish.PowerState{machine.Name: redfish.PowerOn}}
	reconciler := testReconciler(c, power, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
	require.Len(t, updated.Status.Targets, 1)
	require.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Targets[0].Phase)
	require.Equal(t, []string{"machine-1:ForceOff"}, power.calls)
}

func TestReconcilerDoesNotCompletePowerOnForTransientState(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-poweron", v1alpha3.OperationHostPowerOn)
	op.Spec.MachineRef = machine.Name
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:    machine.Name,
		Phase:         v1alpha3.OperationPhaseInProgress,
		Stage:         v1alpha3.OperationStageWaitingOn,
		Attempts:      1,
		LastAttemptAt: ptrTo(metav1.NewTime(fixedNow().Add(-30 * time.Second))),
		StartedAt:     ptrTo(fixedNow()),
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op).Build()
	power := &recordingPowerClient{states: map[string]redfish.PowerState{machine.Name: redfish.PowerState("PoweringOn")}}
	reconciler := testReconciler(c, power, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Targets[0].Phase)
	require.Equal(t, "waiting for power on", updated.Status.Targets[0].Message)
	require.Empty(t, power.calls)
}

func TestReconcilerDoesNotCompleteRebootPowerOnForTransientState(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-reboot", v1alpha3.OperationHostReboot)
	op.Spec.MachineRef = machine.Name
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:    machine.Name,
		Phase:         v1alpha3.OperationPhaseInProgress,
		Stage:         v1alpha3.OperationStageWaitingOn,
		Attempts:      2,
		LastAttemptAt: ptrTo(metav1.NewTime(fixedNow().Add(-30 * time.Second))),
		StartedAt:     ptrTo(fixedNow()),
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op).Build()
	power := &recordingPowerClient{states: map[string]redfish.PowerState{machine.Name: redfish.PowerState("PoweringOn")}}
	reconciler := testReconciler(c, power, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Targets[0].Phase)
	require.Equal(t, "waiting for power on", updated.Status.Targets[0].Message)
	require.Empty(t, power.calls)
}

func TestReconcilerFailsPowerOffAfterIneffectiveResetAttempts(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-poweroff", v1alpha3.OperationHostPowerOff)
	op.Spec.MachineRef = machine.Name
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:    machine.Name,
		Phase:         v1alpha3.OperationPhaseInProgress,
		Stage:         v1alpha3.OperationStageWaitingOff,
		Attempts:      3,
		LastAttemptAt: ptrTo(metav1.NewTime(fixedNow().Add(-2 * time.Minute))),
		StartedAt:     ptrTo(fixedNow()),
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op).Build()
	power := &recordingPowerClient{states: map[string]redfish.PowerState{machine.Name: redfish.PowerOn}}
	reconciler := testReconciler(c, power, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Targets[0].Phase)
	require.Contains(t, updated.Status.Targets[0].Message, "timed out waiting for power off after 3 attempts")
	require.Empty(t, power.calls)
}

func TestRedfishPowerClientFactoryRejectsMissingSecretKey(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	secret := testRedfishSecret()
	secret.Data = map[string][]byte{}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, secret).Build()
	factory := &RedfishPowerClientFactory{Reader: c, Pool: redfish.NewPool()}

	_, err := factory.ForMachine(context.Background(), machine)
	require.ErrorContains(t, err, `redfish password secret unbounded-kube/redfish missing key "password"`)
}

func TestReconcilerSnapshotsSiteScopedSelectorTargets(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machineA := testBareMetalMachine("machine-a", "rack-a")
	machineB := testBareMetalMachine("machine-b", "rack-a")
	machineOtherSite := testBareMetalMachine("machine-c", "rack-b")
	external := testBareMetalMachine("machine-d", "rack-a")
	external.Spec.Provider = v1alpha3.ExternalProviderAzureVM
	external.Spec.ProviderID = "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/machine-d"
	op := testOperation("op-selector", v1alpha3.OperationHostPowerOff)
	op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{siteLabel: "rack-a"}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machineA, machineB, machineOtherSite, external, op, testRedfishSecret()).WithStatusSubresource(op).Build()
	reconciler := testReconciler(c, &recordingPowerClient{states: map[string]redfish.PowerState{}}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Equal(t, []string{"machine-a", "machine-b"}, targetNames(updated.Status.Targets))
}

func TestReconcilerFailsUnscopedSelector(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-selector", v1alpha3.OperationHostPowerOff)
	op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"rack": "a"}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "machineSelector must include unbounded-cloud.io/site=rack-a")
}

func TestReconcilerIgnoresMachineRefOutsideSite(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-b")
	op := testOperation("op-1", v1alpha3.OperationHostPowerOff)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Empty(t, updated.Status.Phase)
}

func TestReconcilerRequestsHostReplaceOnceAndCompletesAfterRepave(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	machine.Spec.Operations = &v1alpha3.OperationsSpec{RebootCounter: 3, RepaveCounter: 4}
	machine.Status.Operations = &v1alpha3.OperationsStatus{RebootCounter: 3, RepaveCounter: 4}
	op := testOperation("op-replace", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var patched v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: machine.Name}, &patched))
	require.Equal(t, int64(4), patched.Spec.Operations.RebootCounter)
	require.Equal(t, int64(5), patched.Spec.Operations.RepaveCounter)

	var inProgress v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &inProgress))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, inProgress.Status.Phase)
	require.Len(t, inProgress.Status.Targets, 1)
	require.Equal(t, int64(4), inProgress.Status.Targets[0].TargetOperations.RebootCounter)
	require.Equal(t, int64(5), inProgress.Status.Targets[0].TargetOperations.RepaveCounter)

	patched.Status.Operations = &v1alpha3.OperationsStatus{RebootCounter: 4, RepaveCounter: 5}
	patched.Status.Conditions = []metav1.Condition{{Type: v1alpha3.MachineConditionRepaved, Status: metav1.ConditionTrue, Reason: "Succeeded"}}
	require.NoError(t, c.Status().Update(context.Background(), &patched))
	require.NoError(t, c.Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: machine.Name}}))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var completed v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &completed))
	require.Equal(t, v1alpha3.OperationPhaseComplete, completed.Status.Phase)
}

func TestReconcilerKeepsHostReplaceInProgressUntilNodeExists(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	machine.Spec.Operations = &v1alpha3.OperationsSpec{RebootCounter: 4, RepaveCounter: 5}
	machine.Status.Operations = &v1alpha3.OperationsStatus{RebootCounter: 4, RepaveCounter: 5}
	machine.Status.Conditions = []metav1.Condition{{Type: v1alpha3.MachineConditionRepaved, Status: metav1.ConditionTrue, Reason: "Succeeded"}}
	ttl := int32(0)
	op := testOperation("op-replace-wait-node", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name
	op.Spec.TTLSecondsAfterFinished = &ttl
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              v1alpha3.OperationPhaseInProgress,
		TargetOperations:   &v1alpha3.OperationsStatus{RebootCounter: 4, RepaveCounter: 5},
		ObservedGeneration: machine.Generation,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Nil(t, updated.Status.CompletedAt)
	require.Len(t, updated.Status.Targets, 1)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Targets[0].Phase)
	require.Equal(t, v1alpha3.OperationStageWaitingNode, updated.Status.Targets[0].Stage)
	require.Equal(t, "waiting for Node machine-1 to exist", updated.Status.Targets[0].Message)
}

func TestReconcilerUsesKubernetesNodeRefForHostReplaceCompletion(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	machine.Spec.Kubernetes = &v1alpha3.KubernetesSpec{NodeRef: &v1alpha3.LocalObjectReference{Name: "custom-node"}}
	machine.Spec.Operations = &v1alpha3.OperationsSpec{RebootCounter: 4, RepaveCounter: 5}
	machine.Status.Operations = &v1alpha3.OperationsStatus{RebootCounter: 4, RepaveCounter: 5}
	machine.Status.Conditions = []metav1.Condition{{Type: v1alpha3.MachineConditionRepaved, Status: metav1.ConditionTrue, Reason: "Succeeded"}}
	op := testOperation("op-replace-custom-node", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              v1alpha3.OperationPhaseInProgress,
		TargetOperations:   &v1alpha3.OperationsStatus{RebootCounter: 4, RepaveCounter: 5},
		ObservedGeneration: machine.Generation,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var waiting v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &waiting))
	require.Equal(t, v1alpha3.OperationStageWaitingNode, waiting.Status.Targets[0].Stage)
	require.Equal(t, "waiting for Node custom-node to exist", waiting.Status.Targets[0].Message)
	require.NoError(t, c.Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "custom-node"}}))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var completed v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &completed))
	require.Equal(t, v1alpha3.OperationPhaseComplete, completed.Status.Phase)
}

func TestReconcilerWaitsForOlderOperation(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	older := testOperation("op-a", v1alpha3.OperationHostPowerOff)
	older.CreationTimestamp = metav1.NewTime(fixedNow().Add(-time.Minute))
	older.Spec.MachineRef = machine.Name
	older.Status.Phase = v1alpha3.OperationPhaseInProgress
	newer := testOperation("op-b", v1alpha3.OperationHostPowerOn)
	newer.CreationTimestamp = fixedNow()
	newer.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, older, newer, testRedfishSecret()).WithStatusSubresource(older, newer).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: newer.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: newer.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhasePending, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "waiting for older host operation op-a")
}

func testReconciler(c client.Client, power PowerClientFactory, site string) *Reconciler {
	return &Reconciler{
		Client:                c,
		APIReader:             c,
		Site:                  site,
		PowerClients:          power,
		MaxConcurrentMachines: 10,
		MaxAttempts:           3,
		PollInterval:          time.Millisecond,
		PowerActionTimeout:    time.Minute,
		Now:                   fixedNow,
	}
}

type recordingPowerClient struct {
	mu     sync.Mutex
	states map[string]redfish.PowerState
	calls  []string
}

func (r *recordingPowerClient) ForMachine(_ context.Context, machine *v1alpha3.Machine) (PowerClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.states == nil {
		r.states = map[string]redfish.PowerState{}
	}

	if _, ok := r.states[machine.Name]; !ok {
		r.states[machine.Name] = redfish.PowerOn
	}

	return &recordingMachinePowerClient{parent: r, machine: machine.Name}, nil
}

type recordingMachinePowerClient struct {
	parent  *recordingPowerClient
	machine string
}

func (c *recordingMachinePowerClient) PowerState(_ context.Context) (redfish.PowerState, error) {
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()

	return c.parent.states[c.machine], nil
}

func (c *recordingMachinePowerClient) Reset(_ context.Context, resetType redfish.ResetType) error {
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()

	c.parent.calls = append(c.parent.calls, fmt.Sprintf("%s:%s", c.machine, resetType))
	switch resetType {
	case redfish.ResetForceOff:
		c.parent.states[c.machine] = redfish.PowerOff
	case redfish.ResetOn:
		c.parent.states[c.machine] = redfish.PowerOn
	}

	return nil
}

func (c *recordingMachinePowerClient) DisableBootOverride(context.Context) error { return nil }

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, v1alpha3.AddToScheme(s))

	return s
}

func testOperation(name string, kind v1alpha3.OperationKind) *v1alpha3.MachineOperation {
	return &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: fixedNow()},
		Spec:       v1alpha3.MachineOperationSpec{OperationKind: kind},
	}
}

func testBareMetalMachine(name, site string) *v1alpha3.Machine {
	return &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{siteLabel: site}},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "ghcr.io/test/host:v1",
				Redfish: &v1alpha3.RedfishSpec{
					URL:         "https://bmc.example.com",
					Username:    "admin",
					DeviceID:    "1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "redfish", Namespace: "unbounded-kube", Key: "password"},
				},
			},
		},
		Status: v1alpha3.MachineStatus{Redfish: &v1alpha3.RedfishStatus{CertFingerprint: "fp"}},
	}
}

func testRedfishSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "redfish", Namespace: "unbounded-kube"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}
}

func fixedNow() metav1.Time {
	return metav1.NewTime(time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC))
}

func ptrTo[T any](value T) *T {
	return &value
}

func targetNames(targets []v1alpha3.MachineOperationTargetStatus) []string {
	names := make([]string, 0, len(targets))
	for _, target := range targets {
		names = append(names, target.MachineRef)
	}

	return names
}
