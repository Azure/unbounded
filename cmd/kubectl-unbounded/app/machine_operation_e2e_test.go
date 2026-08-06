// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMachineOperationCommandsEndToEnd(t *testing.T) {
	ctx := context.Background()
	s := newMachineOperationTestScheme(t)

	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-01",
			UID:  types.UID("worker-01-uid"),
		},
	}
	completeOp := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "wait-complete"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    "worker-01",
			OperationKind: v1alpha3.OperationHostReboot,
		},
		Status: v1alpha3.MachineOperationStatus{
			Phase: v1alpha3.OperationPhaseComplete,
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(machine, completeOp).Build()
	rt := newMachineOperationE2ERuntime(c)

	out, err := executeKubectlUnboundedMachineCommand(
		ctx, rt,
		"machine", "operation", "create", "upgrade-worker-01",
		"--kind", string(v1alpha3.OperationAgentUpgrade),
		"--machine", "worker-01",
		"--param", "downloadURL=https://example.com/agent.tar.gz",
		"--ttl", "900",
	)
	require.NoError(t, err)
	require.Equal(t, "machineoperations/upgrade-worker-01 created\n", out)
	assertMachineOperation(t, ctx, c, "upgrade-worker-01", func(op v1alpha3.MachineOperation) {
		require.Equal(t, "worker-01", op.Spec.MachineRef)
		require.Nil(t, op.Spec.MachineSelector)
		require.Equal(t, v1alpha3.OperationAgentUpgrade, op.Spec.OperationKind)
		require.Equal(t, "https://example.com/agent.tar.gz", op.Spec.Parameters["downloadURL"])
		require.NotNil(t, op.Spec.TTLSecondsAfterFinished)
		require.Equal(t, int32(900), *op.Spec.TTLSecondsAfterFinished)
	})

	out, err = executeKubectlUnboundedMachineCommand(
		ctx, rt,
		"machine", "operation", "create", "reboot-gpu-workers",
		"--kind", string(v1alpha3.OperationNodeReboot),
		"--selector", "role=gpu,site=edge-1",
	)
	require.NoError(t, err)
	require.Equal(t, "machineoperations/reboot-gpu-workers created\n", out)
	assertMachineOperation(t, ctx, c, "reboot-gpu-workers", func(op v1alpha3.MachineOperation) {
		require.Empty(t, op.Spec.MachineRef)
		require.NotNil(t, op.Spec.MachineSelector)
		require.Equal(t, map[string]string{
			"role": "gpu",
			"site": "edge-1",
		}, op.Spec.MachineSelector.MatchLabels)
		require.Equal(t, v1alpha3.OperationNodeReboot, op.Spec.OperationKind)
		require.Nil(t, op.Spec.TTLSecondsAfterFinished)
	})

	out, err = executeKubectlUnboundedMachineCommand(
		ctx, rt,
		"machine", "host-reboot", "worker-01",
		"--operation-name", "worker-01-host-reboot",
		"--wait=false",
	)
	require.NoError(t, err)
	require.Equal(t, "machineoperations/worker-01-host-reboot created\n", out)
	assertMachineOperation(t, ctx, c, "worker-01-host-reboot", func(op v1alpha3.MachineOperation) {
		require.Equal(t, "worker-01", op.Spec.MachineRef)
		require.Equal(t, v1alpha3.OperationHostReboot, op.Spec.OperationKind)
		require.NotNil(t, op.Spec.TTLSecondsAfterFinished)
		require.Equal(t, int32(defaultTTLSeconds), *op.Spec.TTLSecondsAfterFinished)
		require.Len(t, op.OwnerReferences, 1)
		require.Equal(t, "worker-01", op.OwnerReferences[0].Name)
		require.Equal(t, types.UID("worker-01-uid"), op.OwnerReferences[0].UID)
	})

	out, err = executeKubectlUnboundedMachineCommand(
		ctx, rt,
		"machine", "agent-upgrade", "worker-01",
		"--operation-name", "worker-01-agent-upgrade",
		"--download-url", "https://example.com/new-agent.tar.gz",
		"--wait=false",
	)
	require.NoError(t, err)
	require.Equal(t, "machineoperations/worker-01-agent-upgrade created\n", out)
	assertMachineOperation(t, ctx, c, "worker-01-agent-upgrade", func(op v1alpha3.MachineOperation) {
		require.Equal(t, "worker-01", op.Spec.MachineRef)
		require.Equal(t, v1alpha3.OperationAgentUpgrade, op.Spec.OperationKind)
		require.Equal(t, "https://example.com/new-agent.tar.gz", op.Spec.Parameters["downloadURL"])
		require.NotNil(t, op.Spec.TTLSecondsAfterFinished)
		require.Equal(t, int32(defaultTTLSeconds), *op.Spec.TTLSecondsAfterFinished)
		require.Len(t, op.OwnerReferences, 1)
		require.Equal(t, "worker-01", op.OwnerReferences[0].Name)
	})

	out, err = executeKubectlUnboundedMachineCommand(
		ctx, rt,
		"machine", "replace", "worker-01",
		"--force",
		"--operation-name", "replace-worker-01",
		"--wait=false",
	)
	require.NoError(t, err)
	require.Contains(t, out, "Replacing Machine worker-01")
	assertMachineOperation(t, ctx, c, "replace-worker-01", func(op v1alpha3.MachineOperation) {
		require.Equal(t, "worker-01", op.Spec.MachineRef)
		require.Equal(t, v1alpha3.OperationHostReplace, op.Spec.OperationKind)
		require.Len(t, op.OwnerReferences, 1)
		require.Equal(t, "worker-01", op.OwnerReferences[0].Name)
	})

	out, err = executeKubectlUnboundedMachineCommand(
		ctx, rt,
		"machine", "operation", "wait", "wait-complete",
	)
	require.NoError(t, err)
	require.Contains(t, strings.TrimSpace(out), "ready")
}

func newMachineOperationE2ERuntime(c client.WithWatch) *machineCommandRuntime {
	return &machineCommandRuntime{
		newClientWithKubeconfig: func(string) (client.WithWatch, error) {
			return c, nil
		},
		commandContext: func(ctx context.Context) context.Context {
			return ctx
		},
	}
}

func executeKubectlUnboundedMachineCommand(ctx context.Context, rt *machineCommandRuntime, args ...string) (string, error) {
	var out bytes.Buffer

	root := &cobra.Command{
		Use:          "kubectl-unbounded",
		SilenceUsage: true,
	}
	root.AddCommand(newMachineCommandGroup(rt))
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)

	err := root.ExecuteContext(ctx)

	return out.String(), err
}

func assertMachineOperation(
	t *testing.T,
	ctx context.Context,
	c client.Client,
	name string,
	assert func(v1alpha3.MachineOperation),
) {
	t.Helper()

	var op v1alpha3.MachineOperation
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: name}, &op))
	assert(op)
}
