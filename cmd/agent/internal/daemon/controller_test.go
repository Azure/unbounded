// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

type fakeNodeOperator struct {
	active  *ActiveMachine
	findErr error

	restartCalled bool
	restartActive *ActiveMachine
	restartErr    error

	resetCalled bool
	resetErr    error

	stopCalled bool
	stopErr    error

	repaveActive *ActiveMachine
	repaveConfig *provision.UnboundedAgentConfig
	repaveErr    error

	stageUpgradeCalled bool
	stageUpgradeURL    string
	stageUpgradeErr    error

	restartAgentCalled bool
	restartAgentErr    error
}

func (op *fakeNodeOperator) FindActiveMachine(*slog.Logger) (*ActiveMachine, error) {
	if op.findErr != nil {
		return nil, op.findErr
	}

	return op.active, nil
}

func (op *fakeNodeOperator) RestartNode(_ context.Context, _ *slog.Logger, active *ActiveMachine) error {
	op.restartCalled = true
	op.restartActive = active

	return op.restartErr
}

func (op *fakeNodeOperator) ResetAgentResources(context.Context, *slog.Logger) error {
	op.resetCalled = true

	return op.resetErr
}

func (op *fakeNodeOperator) StopDaemon(context.Context, *slog.Logger) error {
	op.stopCalled = true

	return op.stopErr
}

func (op *fakeNodeOperator) RepaveNode(
	_ context.Context,
	_ *slog.Logger,
	active *ActiveMachine,
	cfg *provision.UnboundedAgentConfig,
) error {
	op.repaveActive = active
	op.repaveConfig = cfg

	return op.repaveErr
}

func (op *fakeNodeOperator) StageAgentUpgrade(_ context.Context, _ *slog.Logger, downloadURL string) error {
	op.stageUpgradeCalled = true
	op.stageUpgradeURL = downloadURL

	return op.stageUpgradeErr
}

func (op *fakeNodeOperator) RestartAgentDaemon(_ context.Context, _ *slog.Logger) error {
	op.restartAgentCalled = true

	return op.restartAgentErr
}

func fakeStatusClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(fakeScheme()).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(objs...).
		Build()
}

func setAgentUpgradeSignalPath(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	agentUpgradeSignalPath := filepath.Join(dir, "agent-upgrade-signal")
	t.Setenv(goalstates.EnvDaemonAgentUpgradeSignalPath, agentUpgradeSignalPath)

	return agentUpgradeSignalPath
}

func TestReconcileNodeReboot_Complete(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 7}}
	machineOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationNodeReboot,
		},
	}

	active := &ActiveMachine{Name: "kube1", Config: baseConfig()}
	op := &fakeNodeOperator{active: active}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, machineOp),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: op,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.True(t, op.restartCalled)
	assert.Same(t, active, op.restartActive)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
	assert.Equal(t, "NodeReboot completed", updated.Status.Message)
	assert.Equal(t, int64(7), updated.Status.ObservedMachineGeneration)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
}

func TestReconcileNodeReboot_Failed(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 7}}
	machineOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationNodeReboot,
		},
	}

	op := &fakeNodeOperator{restartErr: errors.New("restart failed")}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, machineOp),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: op,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "restart failed", updated.Status.Message)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
}

func TestReconcileNodeReboot_FindActiveMachineFailed(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 7}}
	machineOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationNodeReboot,
		},
	}

	op := &fakeNodeOperator{findErr: errors.New("no active machine")}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, machineOp),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: op,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.False(t, op.restartCalled)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "no active machine", updated.Status.Message)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
}

func TestReconcileAgentUpgrade_Complete(t *testing.T) {
	signalPath := setAgentUpgradeSignalPath(t)
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 9}}
	machineOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationAgentUpgrade,
			Parameters: map[string]string{
				agentUpgradeDownloadURLParameter: "https://example.com/unbounded-agent.tar.gz",
			},
		},
	}

	op := &fakeNodeOperator{}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, machineOp),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: op,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.True(t, op.stageUpgradeCalled)
	assert.Equal(t, "https://example.com/unbounded-agent.tar.gz", op.stageUpgradeURL)
	assert.True(t, op.restartAgentCalled)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseInProgress, updated.Status.Phase)
	require.NotNil(t, updated.Status.StartedAt)
	assert.Nil(t, updated.Status.CompletedAt)
	data, err := os.ReadFile(signalPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"operationName":"op-1","observedMachineGeneration":9}`, string(data))

	require.NoError(t, publishAndClearAgentUpgradeSignals(context.Background(), discardLogger(), reconciler.Client))
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
	assert.Equal(t, "AgentUpgrade completed", updated.Status.Message)
	assert.Equal(t, int64(9), updated.Status.ObservedMachineGeneration)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
	assert.NoFileExists(t, signalPath)
}

func TestReconcileAgentUpgrade_MissingDownloadURL(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 9}}
	machineOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationAgentUpgrade,
		},
	}

	op := &fakeNodeOperator{}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, machineOp),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: op,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.False(t, op.stageUpgradeCalled)
	assert.False(t, op.restartAgentCalled)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Contains(t, updated.Status.Message, agentUpgradeDownloadURLParameter)
}

func TestReconcileAgentUpgrade_Failed(t *testing.T) {
	signalPath := setAgentUpgradeSignalPath(t)
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 9}}
	machineOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationAgentUpgrade,
			Parameters: map[string]string{
				agentUpgradeDownloadURLParameter: "https://example.com/unbounded-agent.tar.gz",
			},
		},
	}

	op := &fakeNodeOperator{stageUpgradeErr: errors.New("upgrade failed")}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, machineOp),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: op,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "upgrade failed", updated.Status.Message)
	assert.False(t, op.restartAgentCalled)
	assert.NoFileExists(t, signalPath)
}

func TestReconcileAgentUpgrade_RestartFailureFailsOperation(t *testing.T) {
	signalPath := setAgentUpgradeSignalPath(t)
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 9}}
	machineOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationAgentUpgrade,
			Parameters: map[string]string{
				agentUpgradeDownloadURLParameter: "https://example.com/unbounded-agent.tar.gz",
			},
		},
	}

	op := &fakeNodeOperator{restartAgentErr: errors.New("restart failed")}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, machineOp),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: op,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.True(t, op.stageUpgradeCalled)
	assert.True(t, op.restartAgentCalled)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "restart failed", updated.Status.Message)
	assert.NoFileExists(t, signalPath)
}

func TestPublishAndClearAgentUpgradeSignals_Failure(t *testing.T) {
	signalPath := setAgentUpgradeSignalPath(t)
	const rollbackMessage = "rolled back to last good"
	machineOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationAgentUpgrade,
		},
		Status: v1alpha3.MachineOperationStatus{
			Phase:   v1alpha3.OperationPhaseComplete,
			Message: "AgentUpgrade completed",
		},
	}
	c := fakeStatusClient(machineOp)
	signals := newAgentUpgradeSignalOperatorForPath(signalPath)
	require.NoError(t, signals.RecordPending("op-1", 7))
	require.NoError(t, RecordAgentUpgradeFailureSignal(rollbackMessage))

	require.NoError(t, publishAndClearAgentUpgradeSignals(context.Background(), discardLogger(), c))

	var updated v1alpha3.MachineOperation
	require.NoError(t, c.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, rollbackMessage, updated.Status.Message)
	condition := apimeta.FindStatusCondition(updated.Status.Conditions, "Completed")
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "DaemonFailed", condition.Reason)
	assert.NoFileExists(t, signalPath)
}

func TestReconcileAgentReset_Complete(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 7}}
	machineOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationAgentReset,
		},
	}

	op := &fakeNodeOperator{}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, machineOp),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: op,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.True(t, op.resetCalled)
	assert.True(t, op.stopCalled)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseComplete, updated.Status.Phase)
	assert.Equal(t, "AgentReset completed", updated.Status.Message)
	assert.Equal(t, int64(7), updated.Status.ObservedMachineGeneration)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
}

func TestReconcileAgentReset_Failed(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 7}}
	machineOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationAgentReset,
		},
	}

	op := &fakeNodeOperator{resetErr: errors.New("reset failed")}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, machineOp),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: op,
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.True(t, op.resetCalled)
	assert.False(t, op.stopCalled)

	var updated v1alpha3.MachineOperation
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "op-1"}, &updated))
	assert.Equal(t, v1alpha3.OperationPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "reset failed", updated.Status.Message)
	require.NotNil(t, updated.Status.StartedAt)
	require.NotNil(t, updated.Status.CompletedAt)
}

func TestReconcileRepave_UsesDesiredMachineConfigurationVersion(t *testing.T) {
	version := int32(2)
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 8},
		Spec: v1alpha3.MachineSpec{
			ConfigurationRef: &v1alpha3.MachineConfigurationRef{
				Name:    "config-a",
				Version: ptr.To(version),
			},
		},
	}
	mcv := machineConfigurationVersion("config-a", version, v1alpha3.MachineConfigurationTemplate{
		Kubernetes: &v1alpha3.MachineConfigurationKubernetes{
			Version: "v1.34.1",
			NodeLabels: map[string]string{
				"env": "prod",
			},
			RegisterWithTaints: []corev1.Taint{{
				Key:    "dedicated",
				Value:  "prod",
				Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Agent: &v1alpha3.MachineConfigurationAgent{Image: "ghcr.io/test/image:v2"},
	})

	active := &ActiveMachine{Name: "kube1", Config: baseConfig()}
	op := &fakeNodeOperator{active: active}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, mcv),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeName:     "test-node",
		nodeOperator: op,
	}

	_, err := reconciler.Reconcile(context.Background(), daemonRequest{Kind: queueItemRepave, Name: "test-node"})
	require.NoError(t, err)
	require.Same(t, active, op.repaveActive)
	require.NotNil(t, op.repaveConfig)
	assert.Equal(t, "1.34.1", op.repaveConfig.Cluster.Version)
	assert.Equal(t, "ghcr.io/test/image:v2", op.repaveConfig.OCIImage)
	assert.Equal(t, map[string]string{"env": "prod"}, op.repaveConfig.Kubelet.Labels)
	assert.Equal(t, []string{"dedicated=prod:NoSchedule"}, op.repaveConfig.Kubelet.RegisterWithTaints)
	assert.Equal(t, active.Config.Kubelet.ApiServer, op.repaveConfig.Kubelet.ApiServer)
	assert.Equal(t, active.Config.Kubelet.Auth.BootstrapToken, op.repaveConfig.Kubelet.Auth.BootstrapToken)

	var updated v1alpha3.Machine
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &updated))
	require.NotNil(t, updated.Status.Configuration)
	assert.Equal(t, "config-a", updated.Status.Configuration.Name)
	assert.Equal(t, int32(2), updated.Status.Configuration.Version)
	assert.Equal(t, "config-a-v2", updated.Status.Configuration.VersionName)

	condition := apimeta.FindStatusCondition(
		updated.Status.Conditions,
		v1alpha3.MachineConditionRepavePending,
	)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "Applied", condition.Reason)
}

func TestReconcileRepave_NoDriftMarksDesiredConfigurationApplied(t *testing.T) {
	version := int32(2)
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 8},
		Spec: v1alpha3.MachineSpec{
			ConfigurationRef: &v1alpha3.MachineConfigurationRef{
				Name:    "config-a",
				Version: ptr.To(version),
			},
		},
	}
	base := baseConfig()
	mcv := machineConfigurationVersion("config-a", version, v1alpha3.MachineConfigurationTemplate{
		Kubernetes: &v1alpha3.MachineConfigurationKubernetes{
			Version: base.Cluster.Version,
		},
		Agent: &v1alpha3.MachineConfigurationAgent{Image: base.OCIImage},
	})

	op := &fakeNodeOperator{active: &ActiveMachine{Name: "kube1", Config: base}}
	reconciler := &daemonReconciler{
		Client:       fakeStatusClient(machine, mcv),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeName:     "test-node",
		nodeOperator: op,
	}

	_, err := reconciler.Reconcile(context.Background(), daemonRequest{Kind: queueItemRepave, Name: "test-node"})
	require.NoError(t, err)
	assert.Nil(t, op.repaveActive)
	assert.Nil(t, op.repaveConfig)

	var updated v1alpha3.Machine
	require.NoError(t, reconciler.Get(context.Background(), client.ObjectKey{Name: "test-machine"}, &updated))
	require.NotNil(t, updated.Status.Configuration)
	assert.Equal(t, "config-a", updated.Status.Configuration.Name)
	assert.Equal(t, int32(2), updated.Status.Configuration.Version)
	assert.Equal(t, "config-a-v2", updated.Status.Configuration.VersionName)

	condition := apimeta.FindStatusCondition(
		updated.Status.Conditions,
		v1alpha3.MachineConditionRepavePending,
	)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "Applied", condition.Reason)
}

func TestResolveDesiredRepaveConfig_UsesLatestWhenVersionOmitted(t *testing.T) {
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine"},
		Spec: v1alpha3.MachineSpec{
			ConfigurationRef: &v1alpha3.MachineConfigurationRef{Name: "config-a"},
		},
	}
	mcv1 := machineConfigurationVersion("config-a", 1, v1alpha3.MachineConfigurationTemplate{
		Kubernetes: &v1alpha3.MachineConfigurationKubernetes{Version: "v1.33.1"},
	})
	mcv3 := machineConfigurationVersion("config-a", 3, v1alpha3.MachineConfigurationTemplate{
		Kubernetes: &v1alpha3.MachineConfigurationKubernetes{Version: "v1.35.0"},
	})
	c := fakeStatusClient(machine, mcv1, mcv3)

	desired, appliedRef, err := resolveDesiredRepaveConfig(
		context.Background(),
		c,
		"test-machine",
		baseConfig(),
	)
	require.NoError(t, err)
	assert.Equal(t, "1.35.0", desired.Cluster.Version)
	require.NotNil(t, appliedRef)
	assert.Equal(t, int32(3), appliedRef.Version)
	assert.Equal(t, "config-a-v3", appliedRef.VersionName)
}

func machineConfigurationVersion(
	configurationName string,
	version int32,
	template v1alpha3.MachineConfigurationTemplate,
) *v1alpha3.MachineConfigurationVersion {
	return &v1alpha3.MachineConfigurationVersion{
		ObjectMeta: metav1.ObjectMeta{
			Name: v1alpha3.MachineConfigurationVersionName(configurationName, version),
			Labels: map[string]string{
				v1alpha3.MCVConfigurationLabelKey: configurationName,
			},
		},
		Spec: v1alpha3.MachineConfigurationVersionSpec{
			Version:  version,
			Template: template,
		},
	}
}
