// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/netboot"
	"github.com/Azure/unbounded/internal/metalman/redfish"
)

func TestReconcilerAddsMachineOperationNameToHandlerLogs(t *testing.T) {
	s := testScheme(t)
	op := testOperation("op-logged", v1alpha3.OperationHostPowerOff)
	op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"missing": "true"}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(op).WithStatusSubresource(op).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	var logs bytes.Buffer

	previousLogger := slog.Default()

	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	require.Contains(t, logs.String(), "machineOperation=op-logged")
}

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
	require.Equal(t, []string{"machine-1:DisableBootOverride"}, power.calls)
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
	require.Equal(t, []string{"machine-1:DisableBootOverride"}, power.calls)
}

func TestReconcilerRecordsBootDisableFallbackOnOperationTarget(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-poweron-fallback", v1alpha3.OperationHostPowerOn)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{
		states:                 map[string]redfish.PowerState{machine.Name: redfish.PowerOff},
		disableBootUnsupported: map[string]bool{machine.Name: true},
	}
	reconciler := testReconciler(c, power, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Len(t, updated.Status.Targets, 1)
	require.Equal(t, v1alpha3.OperationStageWaitingOn, updated.Status.Targets[0].Stage)
	require.Equal(t, []string{"machine-1:DisableBootOverride", "machine-1:SetBootOverride:Hdd:Continuous", "machine-1:On"}, power.calls)

	cond := apimeta.FindStatusCondition(updated.Status.Targets[0].Conditions, v1alpha3.MachineOperationTargetConditionRedfishDisableBootOverrideUnsupported)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "Unsupported", cond.Reason)

	var updatedMachine v1alpha3.Machine
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: machine.Name}, &updatedMachine))
	require.Empty(t, updatedMachine.Status.Conditions)
}

func TestReconcilerDoesNotReuseBootDisableFallbackAcrossOperations(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	previous := testOperation("op-previous", v1alpha3.OperationHostPowerOn)
	previous.Spec.MachineRef = machine.Name
	previous.Status.Phase = v1alpha3.OperationPhaseComplete
	previous.Status.StartedAt = ptrTo(fixedNow())
	previous.Status.CompletedAt = ptrTo(fixedNow())
	previous.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: machine.Name,
		Phase:      v1alpha3.OperationPhaseComplete,
		Conditions: []metav1.Condition{{
			Type:   v1alpha3.MachineOperationTargetConditionRedfishDisableBootOverrideUnsupported,
			Status: metav1.ConditionTrue,
			Reason: "Unsupported",
		}},
	}}
	current := testOperation("op-current", v1alpha3.OperationHostPowerOn)
	current.CreationTimestamp = metav1.NewTime(fixedNow().Add(time.Minute))
	current.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, previous, current, testRedfishSecret()).WithStatusSubresource(previous, current, machine).Build()
	power := &recordingPowerClient{states: map[string]redfish.PowerState{machine.Name: redfish.PowerOff}}
	reconciler := testReconciler(c, power, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: current.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: current.Name}, &updated))
	require.Equal(t, []string{"machine-1:DisableBootOverride", "machine-1:On"}, power.calls)
	require.Nil(t, apimeta.FindStatusCondition(updated.Status.Targets[0].Conditions, v1alpha3.MachineOperationTargetConditionRedfishDisableBootOverrideUnsupported))
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
	require.ErrorContains(t, err, `redfish password secret unbounded-system/redfish missing key "password"`)
}

func TestReconcilerSnapshotsSiteScopedSelectorTargets(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machineA := testBareMetalMachine("machine-a", "rack-a")
	machineA.Spec.Host = &v1alpha3.HostSpec{Netboot: machineA.Spec.PXE}
	machineA.Spec.PXE = nil
	machineB := testBareMetalMachine("machine-b", "rack-a")
	machineOtherSite := testBareMetalMachine("machine-c", "rack-b")
	external := testBareMetalMachine("machine-d", "rack-a")
	external.Spec.PXE = nil
	external.Spec.Host = &v1alpha3.HostSpec{Azure: &v1alpha3.AzureHostSpec{
		ResourceID: "azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/machine-d",
	}}
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
	op := testOperation("op-replace", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{}
	reconciler := testReconciler(c, power, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var poweringOff v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &poweringOff))
	require.Equal(t, v1alpha3.OperationStageWaitingOff, poweringOff.Status.Targets[0].Stage)
	require.Equal(t, []string{"machine-1:ForceOff"}, power.calls)

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var inProgress v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &inProgress))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, inProgress.Status.Phase)
	require.Len(t, inProgress.Status.Targets, 1)
	require.Equal(t, v1alpha3.OperationStageWaitingRepave, inProgress.Status.Targets[0].Stage)
	require.Equal(t, int32(1), inProgress.Status.Targets[0].Attempts)
	require.Equal(t, []string{"machine-1:ForceOff", "machine-1:SetBootOverride:Pxe:Continuous", "machine-1:On"}, power.calls)

	bootLoaderCond := apimeta.FindStatusCondition(inProgress.Status.Conditions, v1alpha3.MachineOperationConditionBootLoaderDownloaded)
	require.NotNil(t, bootLoaderCond)
	require.Equal(t, metav1.ConditionUnknown, bootLoaderCond.Status)
	require.Equal(t, "Pending", bootLoaderCond.Reason)

	bootImageCond := apimeta.FindStatusCondition(inProgress.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten)
	require.NotNil(t, bootImageCond)
	require.Equal(t, metav1.ConditionUnknown, bootImageCond.Status)
	require.Equal(t, "Pending", bootImageCond.Reason)

	cloudInitCond := apimeta.FindStatusCondition(inProgress.Status.Conditions, v1alpha3.MachineOperationConditionCloudInitDone)
	require.NotNil(t, cloudInitCond)
	require.Equal(t, metav1.ConditionUnknown, cloudInitCond.Status)
	require.Equal(t, "Pending", cloudInitCond.Reason)

	markTargetCondition(t, c, op.Name, machine.Name, v1alpha3.MachineOperationConditionBootImageWritten, metav1.ConditionTrue, "Succeeded", "boot image written")
	markTargetCondition(t, c, op.Name, machine.Name, v1alpha3.MachineOperationConditionCloudInitDone, metav1.ConditionTrue, "Succeeded", "cloud-init completed successfully")
	require.NoError(t, c.Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: machine.Name}}))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var completed v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &completed))
	require.Equal(t, v1alpha3.OperationPhaseComplete, completed.Status.Phase)
}

func TestReconcilerPersistsReadySessionBeforeHostReplaceSideEffects(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-session", "rack-a")
	machine.UID = "machine-uid"
	machine.Generation = 3
	op := testOperation("op-session", v1alpha3.OperationHostReplace)
	op.UID = "operation-uid"
	op.Generation = 2
	op.Spec.MachineRef = machine.Name

	session := &v1alpha3.NetbootSession{
		ObjectMeta: metav1.ObjectMeta{Name: "netboot-session", UID: "session-uid"},
		Status:     v1alpha3.NetbootSessionStatus{Phase: v1alpha3.NetbootSessionPhaseReady},
	}
	sessions := &recordingSessionManager{session: session}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{states: map[string]redfish.PowerState{machine.Name: redfish.PowerOff}}
	reconciler := testReconciler(c, power, "rack-a")
	reconciler.Sessions = sessions

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	require.Empty(t, power.calls)

	var preparing v1alpha3.MachineOperation
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: op.Name}, &preparing))
	require.Equal(t, &v1alpha3.NetbootSessionReference{Name: session.Name, UID: session.UID}, preparing.Status.Targets[0].Input.NetbootSessionRef)
	require.Equal(t, "persisted netboot session netboot-session", preparing.Status.Targets[0].Message)

	_, err = reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	require.Equal(t, []string{"machine-session:SetBootOverride:Pxe:Continuous", "machine-session:On"}, power.calls)
}

func TestReconcilerUsesSessionCapabilityURLForHTTPBoot(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-session-http", "rack-a")
	machine.UID = "machine-uid"
	machine.Generation = 3
	machine.Spec.PXE.Transport = v1alpha3.NetbootTransportTFTP
	machine.Spec.PXE.DHCPLeases = []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:ff", IPv4: "192.0.2.99"}}
	op := testOperation("op-session-http", v1alpha3.OperationHostReplace)
	op.UID = "operation-uid"
	op.Generation = 2
	op.Spec.MachineRef = machine.Name
	session := &v1alpha3.NetbootSession{
		ObjectMeta: metav1.ObjectMeta{Name: "netboot-session", UID: "session-uid"},
		Spec: v1alpha3.NetbootSessionSpec{
			Endpoint: v1alpha3.NetbootSessionEndpointSnapshot{ExternalURL: "https://boot.example.com"},
			Boot: v1alpha3.NetbootSessionBoot{
				Transport:        v1alpha3.NetbootTransportHTTP,
				FirmwareArtifact: "bootx64.efi",
				DHCPLeases:       []v1alpha3.DHCPLease{httpBootLease()},
			},
		},
		Status: v1alpha3.NetbootSessionStatus{Phase: v1alpha3.NetbootSessionPhaseReady},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{states: map[string]redfish.PowerState{machine.Name: redfish.PowerOff}}
	reconciler := testReconciler(c, power, "rack-a")
	reconciler.Sessions = &recordingSessionManager{session: session}
	reconciler.SessionHTTPBootURL = func(got *v1alpha3.NetbootSession) (string, error) {
		require.Equal(t, session, got)

		return "https://boot.example.com/v1/netboot/sessions/netboot-session/capability/artifacts/bootx64.efi", nil
	}

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	require.Contains(t, power.calls, "machine-session-http:SetHTTPBootOverride:https://boot.example.com/v1/netboot/sessions/netboot-session/capability/artifacts/bootx64.efi")
}

func TestConfigureRepaveBootSupportsIndependentBootAxes(t *testing.T) {
	t.Parallel()

	const bootURL = "https://boot.example.com/v1/netboot/sessions/session/capability/artifacts/bootx64.efi"

	tests := []struct {
		name                string
		transport           v1alpha3.NetbootTransport
		configurationSource v1alpha3.NetbootConfigurationSource
		networkMode         v1alpha3.NetbootNetworkMode
		wantCalls           []string
		wantURLResolution   bool
	}{
		{
			name:                "TFTP configured by DHCP",
			transport:           v1alpha3.NetbootTransportTFTP,
			configurationSource: v1alpha3.NetbootConfigurationSourceDHCP,
			networkMode:         v1alpha3.NetbootNetworkModeDHCP,
			wantCalls:           []string{"machine:SetBootOverride:Pxe:Continuous"},
		},
		{
			name:                "HTTP configured by DHCP",
			transport:           v1alpha3.NetbootTransportHTTP,
			configurationSource: v1alpha3.NetbootConfigurationSourceDHCP,
			networkMode:         v1alpha3.NetbootNetworkModeDHCP,
			wantCalls:           []string{"machine:SetBootOverride:UefiHttp:Once"},
		},
		{
			name:                "HTTP URL configured by Redfish with DHCP networking",
			transport:           v1alpha3.NetbootTransportHTTP,
			configurationSource: v1alpha3.NetbootConfigurationSourceRedfish,
			networkMode:         v1alpha3.NetbootNetworkModeDHCP,
			wantURLResolution:   true,
			wantCalls: []string{
				"machine:GetBootConfig",
				"machine:SetHTTPBootOverride:" + bootURL,
				"machine:SetBIOSHTTPBootURI:" + bootURL,
			},
		},
		{
			name:                "HTTP URL and static network configured by Redfish",
			transport:           v1alpha3.NetbootTransportHTTP,
			configurationSource: v1alpha3.NetbootConfigurationSourceRedfish,
			networkMode:         v1alpha3.NetbootNetworkModeStatic,
			wantURLResolution:   true,
			wantCalls: []string{
				"machine:GetBootConfig",
				"machine:SetStaticIPv4:aa:bb:cc:dd:ee:01:10.0.0.20:255.255.255.0:10.0.0.1:10.0.0.53",
				"machine:SetHTTPBootOverride:" + bootURL,
				"machine:SetBIOSStaticIPv4:10.0.0.20:255.255.255.0:10.0.0.1",
				"machine:SetBIOSHTTPBootURI:" + bootURL,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			power := &recordingPowerClient{}
			client, err := power.ForMachine(t.Context(), &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine"}})
			require.NoError(t, err)

			resolved := false
			reconciler := &Reconciler{SessionHTTPBootURL: func(*v1alpha3.NetbootSession) (string, error) {
				resolved = true

				return bootURL, nil
			}}
			session := &v1alpha3.NetbootSession{Spec: v1alpha3.NetbootSessionSpec{Boot: v1alpha3.NetbootSessionBoot{
				Transport:           tt.transport,
				ConfigurationSource: tt.configurationSource,
				NetworkMode:         tt.networkMode,
				DHCPLeases:          []v1alpha3.DHCPLease{httpBootLease()},
			}}}

			err = reconciler.configureRepaveBoot(t.Context(), client, testBareMetalMachine("machine", "rack-a"), session)
			require.NoError(t, err)
			require.Equal(t, tt.wantURLResolution, resolved)
			require.Equal(t, tt.wantCalls, power.calls)
		})
	}
}

func TestReconcilerPowersOnHostReplaceTargetWhenOff(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-replace-off", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{states: map[string]redfish.PowerState{machine.Name: redfish.PowerOff}}
	reconciler := testReconciler(c, power, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var inProgress v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &inProgress))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, inProgress.Status.Phase)
	require.Len(t, inProgress.Status.Targets, 1)
	require.Equal(t, v1alpha3.OperationStageWaitingRepave, inProgress.Status.Targets[0].Stage)
	require.Equal(t, int32(1), inProgress.Status.Targets[0].Attempts)
	require.Equal(t, []string{"machine-1:SetBootOverride:Pxe:Continuous", "machine-1:On"}, power.calls)
}

func TestReconcilerRetriesHostReplacePowerOffAfterTimeout(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-replace-poweroff-failed", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:    machine.Name,
		Phase:         v1alpha3.OperationPhaseInProgress,
		Stage:         v1alpha3.OperationStageWaitingOff,
		LastAttemptAt: ptrTo(metav1.NewTime(fixedNow().Add(-2 * time.Minute))),
		StartedAt:     ptrTo(fixedNow()),
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{states: map[string]redfish.PowerState{machine.Name: redfish.PowerOn}}
	reconciler := testReconciler(c, power, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Targets[0].Phase)
	require.Equal(t, v1alpha3.OperationStageWaitingOff, updated.Status.Targets[0].Stage)
	require.Zero(t, updated.Status.Targets[0].Attempts)
	require.Equal(t, "sent ForceOff before configuring repave boot", updated.Status.Targets[0].Message)
	require.Equal(t, []string{"machine-1:ForceOff"}, power.calls)
}

func TestReconcilerReturnsHostReplacePowerOnFailure(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-replace-poweron-failed", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{
		states:      map[string]redfish.PowerState{machine.Name: redfish.PowerOff},
		resetErrors: map[redfish.ResetType]error{redfish.ResetOn: errors.New("power on failed")},
	}
	reconciler := testReconciler(c, power, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.ErrorContains(t, err, "power on failed")

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Equal(t, v1alpha3.OperationPhasePending, updated.Status.Targets[0].Phase)
	require.Equal(t, int32(0), updated.Status.Targets[0].Attempts)
	require.Equal(t, "target snapshotted", updated.Status.Targets[0].Message)
	require.Equal(t, []string{"machine-1:SetBootOverride:Pxe:Continuous", "machine-1:On"}, power.calls)
}

func TestReconcilerFallsBackToBIOSHTTPBootURIForHostReplace(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	machine.Spec.PXE.Transport = v1alpha3.NetbootTransportHTTP
	machine.Spec.PXE.ConfigurationSource = v1alpha3.NetbootConfigurationSourceRedfish
	machine.Spec.PXE.NetworkMode = v1alpha3.NetbootNetworkModeStatic
	machine.Spec.PXE.DHCPLeases = []v1alpha3.DHCPLease{httpBootLease()}
	op := testOperation("op-replace-http", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{httpBootUnsupported: map[string]bool{machine.Name: true}}
	reconciler := testReconciler(c, power, "rack-a")
	reconciler.HTTPBootURL = func(m *v1alpha3.Machine) (string, error) {
		require.Equal(t, machine.Name, m.Name)

		return "http://10.0.0.10:8880/http/shimx64.efi", nil
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var inProgress v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &inProgress))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, inProgress.Status.Phase)
	require.Len(t, inProgress.Status.Targets, 1)
	require.Equal(t, v1alpha3.OperationStageWaitingRepave, inProgress.Status.Targets[0].Stage)
	require.Equal(t, []string{
		"machine-1:ForceOff",
		"machine-1:GetBootConfig",
		"machine-1:SetStaticIPv4:aa:bb:cc:dd:ee:01:10.0.0.20:255.255.255.0:10.0.0.1:10.0.0.53",
		"machine-1:SetHTTPBootOverride:http://10.0.0.10:8880/http/shimx64.efi",
		"machine-1:SetBIOSStaticIPv4:10.0.0.20:255.255.255.0:10.0.0.1",
		"machine-1:SetBIOSHTTPBootURI:http://10.0.0.10:8880/http/shimx64.efi",
		"machine-1:SetBootOverride:UefiHttp:Once",
		"machine-1:On",
	}, power.calls)
}

func TestReconcilerUsesBIOSHTTPBootURIWhenStandardURIAbsent(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	machine.Spec.PXE.Transport = v1alpha3.NetbootTransportHTTP
	machine.Spec.PXE.ConfigurationSource = v1alpha3.NetbootConfigurationSourceRedfish
	machine.Spec.PXE.NetworkMode = v1alpha3.NetbootNetworkModeStatic
	machine.Spec.PXE.DHCPLeases = []v1alpha3.DHCPLease{httpBootLease()}
	op := testOperation("op-replace-http-bios", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{httpBootURIAbsent: map[string]bool{machine.Name: true}}
	reconciler := testReconciler(c, power, "rack-a")
	reconciler.HTTPBootURL = func(m *v1alpha3.Machine) (string, error) {
		require.Equal(t, machine.Name, m.Name)

		return "http://10.0.0.10:8880/http/shimx64.efi", nil
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var inProgress v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &inProgress))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, inProgress.Status.Phase)
	require.Len(t, inProgress.Status.Targets, 1)
	require.Equal(t, v1alpha3.OperationStageWaitingRepave, inProgress.Status.Targets[0].Stage)
	require.Equal(t, []string{
		"machine-1:ForceOff",
		"machine-1:GetBootConfig",
		"machine-1:SetBIOSStaticIPv4:10.0.0.20:255.255.255.0:10.0.0.1",
		"machine-1:SetBIOSHTTPBootURI:http://10.0.0.10:8880/http/shimx64.efi",
		"machine-1:SetBootOverride:UefiHttp:Once",
		"machine-1:On",
	}, power.calls)
}

func TestReconcilerFallsBackToBIOSWhenStaticInterfaceIsReadOnly(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	machine.Spec.PXE.Transport = v1alpha3.NetbootTransportHTTP
	machine.Spec.PXE.ConfigurationSource = v1alpha3.NetbootConfigurationSourceRedfish
	machine.Spec.PXE.NetworkMode = v1alpha3.NetbootNetworkModeStatic
	machine.Spec.PXE.DHCPLeases = []v1alpha3.DHCPLease{httpBootLease()}
	op := testOperation("op-replace-http-read-only-nic", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{staticIPv4Unsupported: map[string]bool{machine.Name: true}}
	reconciler := testReconciler(c, power, "rack-a")
	reconciler.HTTPBootURL = func(m *v1alpha3.Machine) (string, error) {
		require.Equal(t, machine.Name, m.Name)

		return "http://10.0.0.10:8880/http/shimx64.efi", nil
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var inProgress v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &inProgress))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, inProgress.Status.Phase)
	require.Equal(t, v1alpha3.OperationStageWaitingRepave, inProgress.Status.Targets[0].Stage)
	require.Equal(t, []string{
		"machine-1:ForceOff",
		"machine-1:GetBootConfig",
		"machine-1:SetStaticIPv4:aa:bb:cc:dd:ee:01:10.0.0.20:255.255.255.0:10.0.0.1:10.0.0.53",
		"machine-1:SetBIOSStaticIPv4:10.0.0.20:255.255.255.0:10.0.0.1",
		"machine-1:SetBIOSHTTPBootURI:http://10.0.0.10:8880/http/shimx64.efi",
		"machine-1:SetBootOverride:UefiHttp:Once",
		"machine-1:On",
	}, power.calls)
}

func TestSetHTTPBootOverrideRefreshesStandardAndBIOSURLs(t *testing.T) {
	t.Parallel()

	power := &recordingPowerClient{}
	client := &recordingMachinePowerClient{parent: power, machine: "machine-1"}
	staticConfig := redfish.StaticIPv4Config{
		MAC:        "aa:bb:cc:dd:ee:01",
		Address:    "10.0.0.20",
		SubnetMask: "255.255.255.0",
		Gateway:    "10.0.0.1",
		DNS:        []string{"10.0.0.53"},
	}

	require.NoError(t, setHTTPBootOverride(t.Context(), client, "http://10.0.0.10:8880/http/shimx64.efi", staticConfig, true))
	require.Equal(t, []string{
		"machine-1:GetBootConfig",
		"machine-1:SetStaticIPv4:aa:bb:cc:dd:ee:01:10.0.0.20:255.255.255.0:10.0.0.1:10.0.0.53",
		"machine-1:SetHTTPBootOverride:http://10.0.0.10:8880/http/shimx64.efi",
		"machine-1:SetBIOSStaticIPv4:10.0.0.20:255.255.255.0:10.0.0.1",
		"machine-1:SetBIOSHTTPBootURI:http://10.0.0.10:8880/http/shimx64.efi",
	}, power.calls)
}

func TestSetHTTPBootOverrideAllowsUnsupportedBIOSURL(t *testing.T) {
	t.Parallel()

	power := &recordingPowerClient{biosHTTPBootUnsupported: map[string]bool{"machine-1": true}}
	client := &recordingMachinePowerClient{parent: power, machine: "machine-1"}

	require.NoError(t, setHTTPBootOverride(t.Context(), client, "http://10.0.0.10:8880/http/shimx64.efi", redfish.StaticIPv4Config{}, false))
	require.Equal(t, []string{
		"machine-1:GetBootConfig",
		"machine-1:SetHTTPBootOverride:http://10.0.0.10:8880/http/shimx64.efi",
		"machine-1:SetBIOSHTTPBootURI:http://10.0.0.10:8880/http/shimx64.efi",
	}, power.calls)
}

func TestReconcilerRetriesHTTPHostReplaceWithoutStaticLease(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	machine.Spec.PXE.Transport = v1alpha3.NetbootTransportHTTP
	machine.Spec.PXE.ConfigurationSource = v1alpha3.NetbootConfigurationSourceRedfish
	machine.Spec.PXE.NetworkMode = v1alpha3.NetbootNetworkModeStatic
	op := testOperation("op-replace-http-no-lease", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{}
	reconciler := testReconciler(c, power, "rack-a")
	reconciler.HTTPBootURL = func(m *v1alpha3.Machine) (string, error) {
		require.Equal(t, machine.Name, m.Name)

		return "http://10.0.0.10:8880/http/shimx64.efi", nil
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Targets[0].Phase)
	require.Equal(t, "HTTP boot requires at least one static lease in spec.host.netboot.dhcpLeases", updated.Status.Targets[0].Message)
	require.Empty(t, power.calls)
}

func TestReconcilerWaitsForHTTPBootImage(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	machine.Spec.PXE.Transport = v1alpha3.NetbootTransportHTTP
	machine.Spec.PXE.ConfigurationSource = v1alpha3.NetbootConfigurationSourceRedfish
	machine.Spec.PXE.NetworkMode = v1alpha3.NetbootNetworkModeStatic
	machine.Spec.PXE.DHCPLeases = []v1alpha3.DHCPLease{httpBootLease()}
	op := testOperation("op-replace-http-wait-image", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	power := &recordingPowerClient{}
	reconciler := testReconciler(c, power, "rack-a")
	imageAvailable := false
	reconciler.HTTPBootURL = func(m *v1alpha3.Machine) (string, error) {
		require.Equal(t, machine.Name, m.Name)

		if !imageAvailable {
			return "", fmt.Errorf("resolve HTTP boot image: %w", netboot.ErrNotYetDownloaded)
		}

		return "http://10.0.0.10:8880/http/shimx64.efi", nil
	}

	for range reconciler.MaxAttempts + 1 {
		_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
		require.NoError(t, err)
	}

	var waiting v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &waiting))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, waiting.Status.Phase)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, waiting.Status.Targets[0].Phase)
	require.Zero(t, waiting.Status.Targets[0].Attempts)
	require.Equal(t, "waiting for OCI image to become available", waiting.Status.Targets[0].Message)
	require.Empty(t, power.calls)

	imageAvailable = true
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var resumed v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &resumed))
	require.Equal(t, v1alpha3.OperationStageWaitingRepave, resumed.Status.Targets[0].Stage)
	require.Equal(t, int32(1), resumed.Status.Targets[0].Attempts)
	require.NotEmpty(t, power.calls)
}

func TestReconcilerRestoresMissingHostReplaceTriggerConditions(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-replace-conditions", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Conditions = []metav1.Condition{{
		Type:    v1alpha3.MachineOperationConditionBootImageWritten,
		Status:  metav1.ConditionTrue,
		Reason:  "Succeeded",
		Message: "Machine machine-1 finished writing the boot image to disk",
	}}
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              v1alpha3.OperationPhaseInProgress,
		Stage:              v1alpha3.OperationStageWaitingRepave,
		ObservedGeneration: machine.Generation,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret()).WithStatusSubresource(op, machine).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	bootLoaderCond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootLoaderDownloaded)
	require.NotNil(t, bootLoaderCond)
	require.Equal(t, metav1.ConditionUnknown, bootLoaderCond.Status)
	require.Equal(t, "Pending", bootLoaderCond.Reason)

	bootImageCond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionBootImageWritten)
	require.NotNil(t, bootImageCond)
	require.Equal(t, metav1.ConditionTrue, bootImageCond.Status)
	require.Equal(t, "Succeeded", bootImageCond.Reason)

	cloudInitCond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha3.MachineOperationConditionCloudInitDone)
	require.NotNil(t, cloudInitCond)
	require.Equal(t, metav1.ConditionUnknown, cloudInitCond.Status)
	require.Equal(t, "Pending", cloudInitCond.Reason)
}

func TestReconcilerKeepsHostReplaceInProgressUntilNodeExists(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	ttl := int32(0)
	op := testOperation("op-replace-wait-node", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name
	op.Spec.TTLSecondsAfterFinished = &ttl
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              v1alpha3.OperationPhaseInProgress,
		Stage:              v1alpha3.OperationStageWaitingRepave,
		ObservedGeneration: machine.Generation,
		Conditions:         hostReplaceConditions(metav1.ConditionTrue, metav1.ConditionTrue),
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

func TestReconcilerKeepsHostReplaceInProgressUntilCloudInitCompletes(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-replace-wait-cloudinit", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              v1alpha3.OperationPhaseInProgress,
		Stage:              v1alpha3.OperationStageWaitingRepave,
		ObservedGeneration: machine.Generation,
		Conditions:         hostReplaceConditions(metav1.ConditionTrue, metav1.ConditionUnknown),
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: machine.Name}}).WithStatusSubresource(op, machine).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var waiting v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &waiting))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, waiting.Status.Phase)
	require.Nil(t, waiting.Status.CompletedAt)
	require.Len(t, waiting.Status.Targets, 1)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, waiting.Status.Targets[0].Phase)
	require.Equal(t, v1alpha3.OperationStageWaitingCloudInit, waiting.Status.Targets[0].Stage)
	require.Equal(t, "waiting for first-boot cloud-init to complete", waiting.Status.Targets[0].Message)

	markTargetCondition(t, c, op.Name, machine.Name, v1alpha3.MachineOperationConditionCloudInitDone, metav1.ConditionTrue, "Succeeded", "cloud-init completed successfully")

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var completed v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &completed))
	require.Equal(t, v1alpha3.OperationPhaseComplete, completed.Status.Phase)
	require.Equal(t, v1alpha3.OperationPhaseComplete, completed.Status.Targets[0].Phase)
}

func TestReconcilerFailsHostReplaceWhenCloudInitFails(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-replace-cloudinit-failed", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              v1alpha3.OperationPhaseInProgress,
		Stage:              v1alpha3.OperationStageWaitingRepave,
		ObservedGeneration: machine.Generation,
		Conditions:         hostReplaceConditions(metav1.ConditionTrue, metav1.ConditionFalse),
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: machine.Name}}).WithStatusSubresource(op, machine).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Targets[0].Phase)
	require.Contains(t, updated.Status.Targets[0].Message, "first-boot cloud-init failed")
}

func TestReconcilerTimesOutHostReplaceCloudInitCondition(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	op := testOperation("op-replace-cloudinit-timeout", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	conditions := hostReplaceConditions(metav1.ConditionTrue, metav1.ConditionUnknown)
	apimeta.SetStatusCondition(&conditions, metav1.Condition{
		Type:               v1alpha3.MachineOperationConditionCloudInitDone,
		Status:             metav1.ConditionFalse,
		Reason:             "Running",
		Message:            "stage \"modules-config\" started",
		LastTransitionTime: metav1.NewTime(fixedNow().Add(-cloudInitTimeout)),
	})
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              v1alpha3.OperationPhaseInProgress,
		Stage:              v1alpha3.OperationStageWaitingCloudInit,
		ObservedGeneration: machine.Generation,
		Conditions:         conditions,
	}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, op, testRedfishSecret(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: machine.Name}}).WithStatusSubresource(op, machine).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	require.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Targets[0].Phase)
	require.Contains(t, updated.Status.Targets[0].Message, "cloud-init did not complete within 5m0s")
}

func TestReconcilerUsesKubernetesNodeRefForHostReplaceCompletion(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machine := testBareMetalMachine("machine-1", "rack-a")
	machine.Spec.Kubernetes = &v1alpha3.KubernetesSpec{NodeRef: &v1alpha3.LocalObjectReference{Name: "custom-node"}}
	op := testOperation("op-replace-custom-node", v1alpha3.OperationHostReplace)
	op.Spec.MachineRef = machine.Name
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef:         machine.Name,
		Phase:              v1alpha3.OperationPhaseInProgress,
		Stage:              v1alpha3.OperationStageWaitingRepave,
		ObservedGeneration: machine.Generation,
		Conditions:         hostReplaceConditions(metav1.ConditionTrue, metav1.ConditionTrue),
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

func TestReconcilerCompletesAllHostReplaceTargetsWhenOperationMilestonesComplete(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machineA := testBareMetalMachine("machine-a", "rack-a")
	machineB := testBareMetalMachine("machine-b", "rack-a")
	op := testOperation("op-replace-multi", v1alpha3.OperationHostReplace)
	op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{siteLabel: "rack-a"}}
	op.Status.Phase = v1alpha3.OperationPhaseInProgress
	op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{
		{
			MachineRef: machineA.Name,
			Phase:      v1alpha3.OperationPhaseInProgress,
			Stage:      v1alpha3.OperationStageWaitingRepave,
			Conditions: hostReplaceConditions(metav1.ConditionTrue, metav1.ConditionTrue),
		},
		{
			MachineRef: machineB.Name,
			Phase:      v1alpha3.OperationPhaseInProgress,
			Stage:      v1alpha3.OperationStageWaitingCloudInit,
			Conditions: hostReplaceConditions(metav1.ConditionTrue, metav1.ConditionTrue),
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(
			machineA,
			machineB,
			op,
			testRedfishSecret(),
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: machineA.Name}},
			&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: machineB.Name}},
		).
		WithStatusSubresource(machineA, machineB, op).
		Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: op.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
	require.Len(t, updated.Status.Targets, 2)
	require.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Targets[0].Phase)
	require.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Targets[1].Phase)
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

func TestReconcilerStartsDisjointHostReplaceWhileOlderReplaceInProgress(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machineA := testBareMetalMachine("machine-a", "rack-a")
	machineA.Spec.PXE.Redfish.URL = "https://bmc-a.example.com"
	machineB := testBareMetalMachine("machine-b", "rack-a")
	machineB.Spec.PXE.Redfish.URL = "https://bmc-b.example.com"
	older := testOperation("op-a", v1alpha3.OperationHostReplace)
	older.CreationTimestamp = metav1.NewTime(fixedNow().Add(-time.Minute))
	older.Spec.MachineRef = machineA.Name
	older.Status.Phase = v1alpha3.OperationPhaseInProgress
	older.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: machineA.Name,
		Phase:      v1alpha3.OperationPhaseInProgress,
		Stage:      v1alpha3.OperationStageWaitingRepave,
	}}
	newer := testOperation("op-b", v1alpha3.OperationHostReplace)
	newer.CreationTimestamp = fixedNow()
	newer.Spec.MachineRef = machineB.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machineA, machineB, older, newer, testRedfishSecret()).WithStatusSubresource(older, newer, machineB).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: newer.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: newer.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: newer.Name}, &updated))
	require.NotEqual(t, v1alpha3.OperationPhasePending, updated.Status.Phase)
	require.NotContains(t, updated.Status.Message, "waiting for older host operation")
	require.Equal(t, []string{machineB.Name}, targetNames(updated.Status.Targets))
	require.Equal(t, v1alpha3.OperationStageWaitingRepave, updated.Status.Targets[0].Stage)
	require.Equal(t, int32(1), updated.Status.Targets[0].Attempts)
}

func TestReconcilerWaitsForOlderHostReplaceSelectorOverlap(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machineA := testBareMetalMachine("machine-a", "rack-a")
	machineA.Spec.PXE.Redfish.URL = "https://bmc-a.example.com"
	machineB := testBareMetalMachine("machine-b", "rack-a")
	machineB.Spec.PXE.Redfish.URL = "https://bmc-b.example.com"
	older := testOperation("op-a", v1alpha3.OperationHostReplace)
	older.CreationTimestamp = metav1.NewTime(fixedNow().Add(-time.Minute))
	older.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{siteLabel: "rack-a"}}
	older.Status.Phase = v1alpha3.OperationPhaseInProgress
	newer := testOperation("op-b", v1alpha3.OperationHostReplace)
	newer.CreationTimestamp = fixedNow()
	newer.Spec.MachineRef = machineB.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machineA, machineB, older, newer, testRedfishSecret()).WithStatusSubresource(older, newer).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: newer.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: newer.Name}, &updated))
	require.Equal(t, v1alpha3.OperationPhasePending, updated.Status.Phase)
	require.Contains(t, updated.Status.Message, "waiting for older host operation op-a")
}

func TestReconcilerStartsDisjointHostReplaceSelector(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machineA := testBareMetalMachine("machine-a", "rack-a")
	machineA.Labels["pool"] = "a"
	machineA.Spec.PXE.Redfish.URL = "https://bmc-a.example.com"
	machineB := testBareMetalMachine("machine-b", "rack-a")
	machineB.Labels["pool"] = "b"
	machineB.Spec.PXE.Redfish.URL = "https://bmc-b.example.com"
	older := testOperation("op-a", v1alpha3.OperationHostReplace)
	older.CreationTimestamp = metav1.NewTime(fixedNow().Add(-time.Minute))
	older.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{siteLabel: "rack-a", "pool": "a"}}
	older.Status.Phase = v1alpha3.OperationPhaseInProgress
	newer := testOperation("op-b", v1alpha3.OperationHostReplace)
	newer.CreationTimestamp = fixedNow()
	newer.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{siteLabel: "rack-a", "pool": "b"}}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machineA, machineB, older, newer, testRedfishSecret()).WithStatusSubresource(older, newer, machineB).Build()
	reconciler := testReconciler(c, &recordingPowerClient{}, "rack-a")

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: newer.Name}})
	require.NoError(t, err)
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: newer.Name}})
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: newer.Name}, &updated))
	require.NotEqual(t, v1alpha3.OperationPhasePending, updated.Status.Phase)
	require.NotContains(t, updated.Status.Message, "waiting for older host operation")
	require.Equal(t, []string{machineB.Name}, targetNames(updated.Status.Targets))
	require.Equal(t, v1alpha3.OperationStageWaitingRepave, updated.Status.Targets[0].Stage)
	require.Equal(t, int32(1), updated.Status.Targets[0].Attempts)
}

func TestReconcilerWaitsForOlderHostReplaceSharedRedfishEndpoint(t *testing.T) {
	t.Parallel()

	s := testScheme(t)
	machineA := testBareMetalMachine("machine-a", "rack-a")
	machineA.Spec.PXE.Redfish.URL = "https://bmc.example.com/"
	machineB := testBareMetalMachine("machine-b", "rack-a")
	machineB.Spec.PXE.Redfish.URL = "https://bmc.example.com"
	older := testOperation("op-a", v1alpha3.OperationHostReplace)
	older.CreationTimestamp = metav1.NewTime(fixedNow().Add(-time.Minute))
	older.Spec.MachineRef = machineA.Name
	older.Status.Phase = v1alpha3.OperationPhaseInProgress
	older.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{
		MachineRef: machineA.Name,
		Phase:      v1alpha3.OperationPhaseInProgress,
		Stage:      v1alpha3.OperationStageWaitingRepave,
	}}
	newer := testOperation("op-b", v1alpha3.OperationHostReplace)
	newer.CreationTimestamp = fixedNow()
	newer.Spec.MachineRef = machineB.Name

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machineA, machineB, older, newer, testRedfishSecret()).WithStatusSubresource(older, newer).Build()
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
	mu                      sync.Mutex
	states                  map[string]redfish.PowerState
	disableBootUnsupported  map[string]bool
	httpBootUnsupported     map[string]bool
	httpBootURIAbsent       map[string]bool
	staticIPv4Unsupported   map[string]bool
	biosHTTPBootUnsupported map[string]bool
	resetErrors             map[redfish.ResetType]error
	calls                   []string
}

type recordingSessionManager struct {
	session *v1alpha3.NetbootSession
}

func (m *recordingSessionManager) Ensure(_ context.Context, _ *v1alpha3.MachineOperation, _ *v1alpha3.Machine) (*v1alpha3.NetbootSession, error) {
	return m.session.DeepCopy(), nil
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
	if err := c.parent.resetErrors[resetType]; err != nil {
		return err
	}

	switch resetType {
	case redfish.ResetForceOff:
		c.parent.states[c.machine] = redfish.PowerOff
	case redfish.ResetOn, redfish.ResetForceRestart:
		c.parent.states[c.machine] = redfish.PowerOn
	}

	return nil
}

func (c *recordingMachinePowerClient) DisableBootOverride(context.Context) error {
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()

	c.parent.calls = append(c.parent.calls, fmt.Sprintf("%s:DisableBootOverride", c.machine))
	if c.parent.disableBootUnsupported[c.machine] {
		return fmt.Errorf("disable unsupported: %w", redfish.ErrUnsupported)
	}

	return nil
}

func (c *recordingMachinePowerClient) SetBootOverride(_ context.Context, target redfish.BootTarget, enabled redfish.BootEnabled) error {
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()

	c.parent.calls = append(c.parent.calls, fmt.Sprintf("%s:SetBootOverride:%s:%s", c.machine, target, enabled))

	return nil
}

func (c *recordingMachinePowerClient) GetBootConfig(context.Context) (redfish.BootConfig, error) {
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()

	c.parent.calls = append(c.parent.calls, fmt.Sprintf("%s:GetBootConfig", c.machine))

	return redfish.BootConfig{HasHTTPBootURI: !c.parent.httpBootURIAbsent[c.machine]}, nil
}

func (c *recordingMachinePowerClient) SetStaticIPv4(_ context.Context, config redfish.StaticIPv4Config) error {
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()

	c.parent.calls = append(c.parent.calls, fmt.Sprintf(
		"%s:SetStaticIPv4:%s:%s:%s:%s:%s",
		c.machine,
		config.MAC,
		config.Address,
		config.SubnetMask,
		config.Gateway,
		strings.Join(config.DNS, ","),
	))
	if c.parent.staticIPv4Unsupported[c.machine] {
		return fmt.Errorf("static IPv4 unsupported: %w", redfish.ErrUnsupported)
	}

	return nil
}

func (c *recordingMachinePowerClient) SetHTTPBootOverride(_ context.Context, bootURL string) error {
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()

	c.parent.calls = append(c.parent.calls, fmt.Sprintf("%s:SetHTTPBootOverride:%s", c.machine, bootURL))
	if c.parent.httpBootUnsupported[c.machine] {
		return fmt.Errorf("http boot unsupported: %w", redfish.ErrUnsupported)
	}

	return nil
}

func (c *recordingMachinePowerClient) SetBIOSStaticIPv4(_ context.Context, config redfish.StaticIPv4Config) error {
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()

	c.parent.calls = append(c.parent.calls, fmt.Sprintf(
		"%s:SetBIOSStaticIPv4:%s:%s:%s",
		c.machine,
		config.Address,
		config.SubnetMask,
		config.Gateway,
	))

	return nil
}

func (c *recordingMachinePowerClient) SetBIOSHTTPBootURI(_ context.Context, bootURL string) error {
	c.parent.mu.Lock()
	defer c.parent.mu.Unlock()

	c.parent.calls = append(c.parent.calls, fmt.Sprintf("%s:SetBIOSHTTPBootURI:%s", c.machine, bootURL))
	if c.parent.biosHTTPBootUnsupported[c.machine] {
		return fmt.Errorf("BIOS HTTP boot unsupported: %w", redfish.ErrUnsupported)
	}

	return nil
}

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
					PasswordRef: v1alpha3.SecretKeySelector{Name: "redfish", Namespace: "unbounded-system", Key: "password"},
				},
			},
		},
		Status: v1alpha3.MachineStatus{Redfish: &v1alpha3.RedfishStatus{CertFingerprint: "fp"}},
	}
}

func httpBootLease() v1alpha3.DHCPLease {
	return v1alpha3.DHCPLease{
		MAC:        "aa:bb:cc:dd:ee:01",
		IPv4:       "10.0.0.20",
		SubnetMask: "255.255.255.0",
		Gateway:    "10.0.0.1",
		DNS:        []string{"10.0.0.53"},
	}
}

func testRedfishSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "redfish", Namespace: "unbounded-system"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}
}

func markTargetCondition(t *testing.T, c client.Client, opName, machineName, conditionType string, status metav1.ConditionStatus, reason, message string) {
	t.Helper()

	var op v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: opName}, &op))

	for i := range op.Status.Targets {
		if op.Status.Targets[i].MachineRef != machineName {
			continue
		}

		apimeta.SetStatusCondition(&op.Status.Targets[i].Conditions, metav1.Condition{
			Type:               conditionType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: op.Status.Targets[i].ObservedGeneration,
		})
		require.NoError(t, c.Status().Update(context.Background(), &op))

		return
	}

	t.Fatalf("operation %s has no target %s", opName, machineName)
}

func hostReplaceConditions(bootImage, cloudInit metav1.ConditionStatus) []metav1.Condition {
	return []metav1.Condition{
		{Type: v1alpha3.MachineOperationConditionBootLoaderDownloaded, Status: metav1.ConditionUnknown, Reason: "Pending"},
		{Type: v1alpha3.MachineOperationConditionBootImageWritten, Status: bootImage, Reason: reasonForConditionStatus(bootImage)},
		{Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: cloudInit, Reason: reasonForConditionStatus(cloudInit), Message: messageForCloudInitStatus(cloudInit)},
	}
}

func reasonForConditionStatus(status metav1.ConditionStatus) string {
	if status == metav1.ConditionTrue {
		return "Succeeded"
	}

	if status == metav1.ConditionFalse {
		return "Failed"
	}

	return "Pending"
}

func messageForCloudInitStatus(status metav1.ConditionStatus) string {
	if status == metav1.ConditionFalse {
		return "Machine machine-1 first-boot cloud-init failed: modules-final failed"
	}

	return ""
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
