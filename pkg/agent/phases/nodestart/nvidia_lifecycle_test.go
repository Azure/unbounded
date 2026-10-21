// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"errors"
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

func TestReconcileNVIDIARestoresServicesAfterSetupFailure(t *testing.T) {
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
		if task.Name() == "setup-nvidia" {
			return errors.New("setup failed")
		}

		return nil
	}

	err := task.Do(context.Background())
	require.ErrorContains(t, err, "setup-nvidia: setup failed")
	require.Equal(t, []string{
		"stop:kubelet.service",
		"stop:containerd.service",
		"setup-nvidia",
		"start-containerd",
		"start-kubelet",
	}, operations)
}

func TestReconcileNVIDIAReportsServiceRestoreFailure(t *testing.T) {
	t.Parallel()

	state := &goalstates.NodeStart{
		MachineName: "kube1",
		Nvidia:      goalstates.NvidiaHost{Required: true},
	}
	task := ReconcileNVIDIA(slog.New(slog.DiscardHandler), state).(*reconcileNVIDIA)

	task.run = func(context.Context, *slog.Logger, string, ...string) (string, error) {
		return "", nil
	}
	task.execute = func(_ context.Context, _ *slog.Logger, task phases.Task) error {
		switch task.Name() {
		case "setup-nvidia":
			return errors.New("setup failed")
		case "start-kubelet":
			return errors.New("kubelet failed")
		default:
			return nil
		}
	}

	err := task.Do(context.Background())
	require.ErrorContains(t, err, "setup-nvidia: setup failed")
	require.ErrorContains(t, err, "restore node services after NVIDIA reconciliation failure")
	require.ErrorContains(t, err, "start-kubelet: kubelet failed")
}
