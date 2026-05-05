// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
)

func fakeStatusClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(fakeScheme()).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithStatusSubresource(&v1alpha3.MachineOperation{}).
		WithObjects(objs...).
		Build()
}

func TestReconcileNodeReboot_Complete(t *testing.T) {
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 7}}
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationNodeReboot,
		},
	}

	called := false
	reconciler := &daemonReconciler{
		Client:      fakeStatusClient(machine, op),
		log:         discardLogger(),
		machineName: "test-machine",
		restartActiveNode: func(context.Context, *slog.Logger) error {
			called = true
			return nil
		},
	}

	_, err := reconciler.reconcileMachineOperation(context.Background(), "op-1")
	require.NoError(t, err)
	assert.True(t, called)

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
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "op-1"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "test-machine",
			OperationKind: v1alpha3.OperationNodeReboot,
		},
	}

	reconciler := &daemonReconciler{
		Client:      fakeStatusClient(machine, op),
		log:         discardLogger(),
		machineName: "test-machine",
		restartActiveNode: func(context.Context, *slog.Logger) error {
			return errors.New("restart failed")
		},
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
	var got *provision.UnboundedAgentConfig
	reconciler := &daemonReconciler{
		Client:      fakeStatusClient(machine, mcv),
		log:         discardLogger(),
		machineName: "test-machine",
		nodeName:    "test-node",
	}
	reconciler.reconcileRepave = func(
		ctx context.Context,
		log *slog.Logger,
		c client.Client,
		machineName string,
	) (reconcile.Result, error) {
		return reconcileRepaveWithDeps(
			ctx,
			log,
			c,
			machineName,
			func(*slog.Logger) (*ActiveMachine, error) {
				return active, nil
			},
			func(
				_ context.Context,
				_ *slog.Logger,
				actualActive *ActiveMachine,
				cfg *provision.UnboundedAgentConfig,
			) error {
				require.Same(t, active, actualActive)
				got = cfg

				return nil
			},
		)
	}

	_, err := reconciler.Reconcile(context.Background(), daemonRequest{Kind: queueItemRepave, Name: "test-node"})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "1.34.1", got.Cluster.Version)
	assert.Equal(t, "ghcr.io/test/image:v2", got.OCIImage)
	assert.Equal(t, map[string]string{"env": "prod"}, got.Kubelet.Labels)
	assert.Equal(t, []string{"dedicated=prod:NoSchedule"}, got.Kubelet.RegisterWithTaints)
	assert.Equal(t, active.Config.Kubelet.ApiServer, got.Kubelet.ApiServer)
	assert.Equal(t, active.Config.Kubelet.Auth.BootstrapToken, got.Kubelet.Auth.BootstrapToken)

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
