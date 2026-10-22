// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func TestReconcileNVIDIAOrdersServiceRestartAroundSetup(t *testing.T) {
	t.Parallel()

	state := &goalstates.NodeStart{
		MachineName: "kube1",
		Nvidia:      goalstates.NvidiaHost{Required: true},
	}
	task := ReconcileNVIDIA(slog.New(slog.DiscardHandler), state).(*reconcileNVIDIA)

	var operations []string

	task.run = func(_ context.Context, _ *slog.Logger, _ string, args ...string) (string, error) {
		operations = append(operations, args[1]+":"+args[2])

		return "", nil
	}
	task.execute = func(_ context.Context, _ *slog.Logger, task phases.Task) error {
		operations = append(operations, task.Name())

		return nil
	}

	require.NoError(t, task.Do(context.Background()))
	require.Equal(t, []string{
		"stop:kubelet.service",
		"stop:containerd.service",
		"setup-nvidia",
		"start-containerd",
		"import-container-images",
		"start-kubelet",
	}, operations)
}
