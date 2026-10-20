// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nspawnlifecycle

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
)

func testLifecycle(t *testing.T, loader ConfigLoader) *Lifecycle {
	t.Helper()

	lifecycle, err := New(slog.New(slog.DiscardHandler), Hooks{LoadConfig: loader})
	require.NoError(t, err)

	return lifecycle
}

func TestPreStartUsesApplicationConfigLoader(t *testing.T) {
	t.Parallel()

	cfg := &config.AgentConfig{AdditionalHostDevices: []string{"/dev/uinput"}}
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

// postStartCapture records what PostStart hands to the NVIDIA reconcile.
type postStartCapture struct {
	waited    bool
	nodeStart *goalstates.NodeStart
	opts      nodestart.NVIDIAReconcileOptions
}

func capturePostStart(t *testing.T, host goalstates.NvidiaHost, discoverErr error, env map[string]string) (*Lifecycle, *postStartCapture) {
	t.Helper()

	captured := &postStartCapture{}
	lifecycle := testLifecycle(t, func(context.Context, string) (*config.AgentConfig, bool, error) {
		return nil, false, nil
	})
	lifecycle.resolveNVIDIA = func(string) (goalstates.NvidiaHost, error) { return host, discoverErr }
	lifecycle.getenv = func(name string) string { return env[name] }
	lifecycle.waitMachine = func(_ context.Context, _ *slog.Logger, machine string) error {
		require.Equal(t, "kube1", machine)

		captured.waited = true

		return nil
	}
	lifecycle.reconcileNVIDIA = func(log *slog.Logger, nodeStart *goalstates.NodeStart, opts nodestart.NVIDIAReconcileOptions) phases.Task {
		captured.nodeStart = nodeStart
		captured.opts = opts

		return nodestart.ReconcileNVIDIA(log, nodeStart, opts)
	}
	lifecycle.execute = func(context.Context, *slog.Logger, phases.Task) error { return nil }

	return lifecycle, captured
}

func TestPostStartPassesNVIDIAAssessment(t *testing.T) {
	t.Parallel()

	complete := goalstates.NvidiaHost{
		GPUDevicePaths: []string{"/dev/nvidia0"},
		LibMappings:    []goalstates.NvidiaLibMapping{{HostPath: "/host/libcuda.so.1"}},
		DriverVersion:  "580.1",
	}
	requireNVIDIA := map[string]string{goalstates.EnvRequiredHardware: "NvidiaGPU"}

	t.Run("usable GPUs are set up", func(t *testing.T) {
		t.Parallel()

		lifecycle, captured := capturePostStart(t, complete, nil, nil)
		require.NoError(t, lifecycle.PostStart(context.Background(), "kube1"))

		require.True(t, captured.waited)
		require.True(t, captured.opts.Assessment.Usable)
		require.False(t, captured.opts.Required)
		require.True(t, captured.nodeStart.Nvidia.Required)
		require.Equal(t, complete.GPUDevicePaths, captured.nodeStart.Nvidia.GPUDevicePaths)
		require.Equal(t, "/var/lib/machines/kube1", captured.nodeStart.MachineDir)
		require.True(t, captured.nodeStart.Containerd.NvidiaRuntime.Enabled)
	})

	t.Run("required hardware comes from the unit environment", func(t *testing.T) {
		t.Parallel()

		lifecycle, captured := capturePostStart(t, complete, nil, requireNVIDIA)
		require.NoError(t, lifecycle.PostStart(context.Background(), "kube1"))
		require.True(t, captured.opts.Required)
	})

	t.Run("unusable GPUs are passed on, not rejected", func(t *testing.T) {
		t.Parallel()

		lifecycle, captured := capturePostStart(t, goalstates.NvidiaHost{GPUDevicePaths: []string{"/dev/nvidia0"}}, nil, nil)
		require.NoError(t, lifecycle.PostStart(context.Background(), "kube1"))

		require.True(t, captured.waited)
		require.False(t, captured.opts.Assessment.Usable)
		require.Equal(t, goalstates.GPUReasonDriverStateIncomplete, captured.opts.Assessment.Reason)
		require.False(t, captured.nodeStart.Nvidia.Required)
	})

	t.Run("discovery errors are passed on, not returned", func(t *testing.T) {
		t.Parallel()

		lifecycle, captured := capturePostStart(t, goalstates.NvidiaHost{}, errors.New("nvidia-modprobe failed"), nil)
		require.NoError(t, lifecycle.PostStart(context.Background(), "kube1"))
		require.Equal(t, goalstates.GPUReasonDiscoveryFailed, captured.opts.Assessment.Reason)
	})

	t.Run("CPU hosts do not wait for the machine", func(t *testing.T) {
		t.Parallel()

		lifecycle, captured := capturePostStart(t, goalstates.NvidiaHost{}, nil, nil)
		require.NoError(t, lifecycle.PostStart(context.Background(), "kube1"))

		require.False(t, captured.waited)
		require.False(t, captured.opts.Assessment.Present)
	})

	t.Run("reconcile failures are returned", func(t *testing.T) {
		t.Parallel()

		lifecycle, _ := capturePostStart(t, goalstates.NvidiaHost{}, nil, requireNVIDIA)
		lifecycle.execute = func(context.Context, *slog.Logger, phases.Task) error {
			return goalstates.ErrRequiredHardwareUnavailable
		}

		err := lifecycle.PostStart(context.Background(), "kube1")
		require.ErrorIs(t, err, goalstates.ErrRequiredHardwareUnavailable)
		require.ErrorContains(t, err, "reconcile NVIDIA state for kube1")
	})
}

func TestParseRequiredHardware(t *testing.T) {
	t.Parallel()

	require.Nil(t, parseRequiredHardware(""))
	require.Equal(t, []string{"NvidiaGPU"}, parseRequiredHardware("NvidiaGPU"))
	require.Equal(t, []string{"NvidiaGPU", "Other"}, parseRequiredHardware(" NvidiaGPU,, Other "))
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
