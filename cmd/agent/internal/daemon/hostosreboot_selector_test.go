// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

func TestNativeSelectorStartsEachQueuedHost(t *testing.T) {
	op := hostSoftRebootOperation("soft")
	op.Spec.MachineRef = ""
	op.Spec.MachineSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{
			"cohort": "a",
		},
	}
	machineA := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-a",
			Labels: map[string]string{
				"cohort": "a",
			},
			Generation: 7,
		},
	}
	machineB := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "machine-b",
			Labels: map[string]string{
				"cohort": "a",
			},
			Generation: 8,
		},
	}
	c := fakeStatusClient(op, machineA, machineB)

	for _, name := range []string{"machine-a", "machine-b"} {
		host := &fakeNodeOperator{bootID: "boot-1"}
		target := &machineOperationTarget{
			installation: installstate.NewStore(
				filepath.Join(t.TempDir(), "state"),
				filepath.Join(t.TempDir(), "install.lock"),
			),
			Client:       c,
			apiReader:    c,
			log:          discardLogger(),
			machineName:  name,
			nodeOperator: host,
		}
		r, err := agentdaemon.NewMachinaMachineOperationReconcilerWithReader(
			c,
			c,
			name,
			"node-"+name,
			target.handlers(),
		)
		require.NoError(t, err)

		_, err = r.ReconcileMachineOperation(context.Background(), "soft")
		require.NoError(t, err)
		require.Equal(t, 1, host.rebootCalls, name)
	}

	status := getMachineOperation(t, c, "soft").Status
	require.Len(t, status.Targets, 2)
	require.Equal(t, "machine-a", status.Targets[0].MachineRef)
	require.Equal(t, "machine-b", status.Targets[1].MachineRef)
}

func TestNativeSelectorPublishesEachIssuedTarget(t *testing.T) {
	op := issuedHostSoftRebootOperation("soft", "machine-a", "boot-a1")
	op.Spec.MachineRef = ""
	op.Spec.MachineSelector = &metav1.LabelSelector{}
	op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{
		MachineRef:         "machine-b",
		Phase:              v1alpha3.OperationPhaseInProgress,
		BootID:             "boot-b1",
		ObservedGeneration: 8,
	})
	c := fakeStatusClient(op)

	handlerA, err := agentdaemon.NewHostSoftRebootHandler(c, c, discardLogger(), hostSoftRebootTarget{machineName: "machine-a"}, &fakeNodeOperator{bootID: "boot-a2"}, &atomic.Bool{})
	require.NoError(t, err)
	require.NoError(t, handlerA.PublishStartup(context.Background()))
	first := getMachineOperation(t, c, "soft").Status
	require.Equal(t, v1alpha3.OperationPhaseComplete, first.Phase)
	require.NotNil(t, first.CompletedAt)
	require.Equal(t, v1alpha3.OperationPhaseComplete, first.Targets[0].Phase)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, first.Targets[1].Phase)

	handlerB, err := agentdaemon.NewHostSoftRebootHandler(c, c, discardLogger(), hostSoftRebootTarget{machineName: "machine-b"}, &fakeNodeOperator{bootID: "boot-b2"}, &atomic.Bool{})
	require.NoError(t, err)
	require.NoError(t, handlerB.PublishStartup(context.Background()))
	second := getMachineOperation(t, c, "soft").Status
	require.Equal(t, v1alpha3.OperationPhaseComplete, second.Targets[0].Phase)
	require.Equal(t, v1alpha3.OperationPhaseComplete, second.Targets[1].Phase)
	require.Equal(t, int64(8), second.ObservedMachineGeneration)
	require.Equal(t, first.Targets[0], second.Targets[0])

	require.NoError(t, handlerB.PublishStartup(context.Background()))
	require.Equal(t, second, getMachineOperation(t, c, "soft").Status)
}
