// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nspawnlifecycle

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

func testLifecycle(t *testing.T, loader ConfigLoader) *Lifecycle {
	t.Helper()

	lifecycle, err := New(slog.New(slog.DiscardHandler), Hooks{LoadConfig: loader})
	require.NoError(t, err)

	return lifecycle
}

func TestPreStartUsesApplicationConfigLoader(t *testing.T) {
	t.Parallel()

	cfg := &config.AgentConfig{AdditionalHostDevices: []config.AdditionalHostDevice{{Path: "/dev/uinput"}}}
	lifecycle := testLifecycle(t, func(_ context.Context, machine string) (*config.AgentConfig, bool, error) {
		require.Equal(t, "kube1", machine)

		return cfg, true, nil
	})
	lifecycle.resolveConfig = func(got *config.AgentConfig, machine string) (*goalstates.RootFS, error) {
		require.Same(t, cfg, got)
		require.Equal(t, "kube1", machine)

		return &goalstates.RootFS{MachineDir: "/var/lib/machines/kube1"}, nil
	}

	var executed, reloaded bool

	lifecycle.execute = func(_ context.Context, _ *slog.Logger, task phases.Task) error {
		executed = true

		require.Equal(t, "ensure-nspawn-config", task.Name())

		return nil
	}
	lifecycle.reloadSystemd = func(context.Context, *slog.Logger) error {
		reloaded = true

		return nil
	}

	require.NoError(t, lifecycle.PreStart(context.Background(), "kube1"))
	require.True(t, executed)
	require.True(t, reloaded)
}

func TestPreStartMissingConfigKeepsBootstrapConfig(t *testing.T) {
	t.Parallel()

	lifecycle := testLifecycle(t, func(context.Context, string) (*config.AgentConfig, bool, error) {
		return nil, false, nil
	})
	lifecycle.execute = func(context.Context, *slog.Logger, phases.Task) error {
		t.Fatal("unexpected task execution")

		return nil
	}

	require.NoError(t, lifecycle.PreStart(context.Background(), "kube1"))
}

func TestPostStartDispatchesReusableNVIDIAFlow(t *testing.T) {
	t.Parallel()

	lifecycle := testLifecycle(t, func(context.Context, string) (*config.AgentConfig, bool, error) {
		return nil, false, nil
	})
	lifecycle.resolveNVIDIA = func(string) (goalstates.NvidiaHost, error) {
		return goalstates.NvidiaHost{
			GPUDevicePaths: []string{"/dev/nvidia0"},
			LibMappings:    []goalstates.NvidiaLibMapping{{HostPath: "/host/libcuda.so.1"}},
			DriverVersion:  "580.1",
		}, nil
	}
	lifecycle.waitMachine = func(context.Context, *slog.Logger, string) error { return nil }

	var executed bool

	lifecycle.execute = func(_ context.Context, _ *slog.Logger, task phases.Task) error {
		executed = true

		require.Equal(t, "reconcile-nvidia", task.Name())

		return nil
	}

	require.NoError(t, lifecycle.PostStart(context.Background(), "kube1"))
	require.True(t, executed)
}

func TestReconcileDispatchesManagedRestart(t *testing.T) {
	t.Parallel()

	lifecycle := testLifecycle(t, func(context.Context, string) (*config.AgentConfig, bool, error) {
		return nil, false, nil
	})

	var executed bool

	lifecycle.execute = func(_ context.Context, _ *slog.Logger, task phases.Task) error {
		executed = true

		require.Equal(t, "reconcile-nspawn-lifecycle", task.Name())

		return nil
	}

	require.NoError(t, lifecycle.Reconcile(context.Background(), "kube1"))
	require.True(t, executed)
}
