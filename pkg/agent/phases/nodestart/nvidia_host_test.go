// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// hostRoot maps every in-node path under dir, like a host-native root.
type hostRoot struct{ dir string }

func (hostRoot) Slot() string { return "v1" }

func (r hostRoot) Path(p string) string { return filepath.Join(r.dir, p) }

var _ isolation.NodeRoot = hostRoot{}

func gpuNodeStart(t *testing.T, calls *[][]string) *goalstates.NodeStart {
	t.Helper()

	return &goalstates.NodeStart{
		RootOverride: hostRoot{dir: t.TempDir()},
		ExecOverride: func(_ context.Context, _ *slog.Logger, args ...string) (string, error) {
			*calls = append(*calls, append([]string(nil), args...))

			return "", nil
		},
		Containerd: goalstates.ResolveContainerd(goalstates.ContainerdOptions{NvidiaRequired: true}),
		Nvidia: goalstates.NvidiaHost{
			Required:       true,
			GPUDevicePaths: []string{"/dev/nvidia0"},
			LibMappings:    []goalstates.NvidiaLibMapping{{HostPath: "/usr/lib/x86_64-linux-gnu/libcuda.so.1"}},
			DriverVersion:  "580.126.09",
		},
	}
}

func TestSetupNVIDIAOnHostNonGPUIsNoop(t *testing.T) {
	t.Parallel()

	var calls [][]string

	gs := gpuNodeStart(t, &calls)
	gs.Nvidia = goalstates.NvidiaHost{}

	require.NoError(t, SetupNVIDIAOnHost(slog.New(slog.DiscardHandler), gs).Do(t.Context()))
	require.Empty(t, calls, "a GPU-less host must not run any NVIDIA command")
	require.NoDirExists(t, gs.Root().Path(goalstates.CDISpecDir), "nor create the CDI directory")
}

func TestSetupNVIDIAOnHostSelectsCDIAndGeneratesTheSpec(t *testing.T) {
	t.Parallel()

	var calls [][]string

	gs := gpuNodeStart(t, &calls)
	task := SetupNVIDIAOnHost(slog.New(slog.DiscardHandler), gs).(*setupNVIDIAOnHost)
	task.stat = func(string) (os.FileInfo, error) { return nil, nil } //nolint:nilnil // only the error matters

	require.NoError(t, task.Do(t.Context()))

	require.Equal(t, [][]string{
		{goalstates.NvidiaCTKPath, "config", "--set", "nvidia-container-runtime.mode=cdi", "--in-place"},
		{
			goalstates.NvidiaCTKPath, "cdi", "generate",
			"--driver-root", "/", "--dev-root", "/",
			"--output", goalstates.CDISpecFile,
			"--disable-hook", "all", "--enable-hook", "create-symlinks", "--enable-hook", "update-ldcache",
		},
	}, calls)
	require.DirExists(t, gs.Root().Path(goalstates.CDISpecDir))
}

func TestSetupNVIDIAOnHostNamesAMissingToolkit(t *testing.T) {
	t.Parallel()

	for _, missing := range NVIDIAToolkitPaths(goalstates.NvidiaToolkit{}) {
		var calls [][]string

		gs := gpuNodeStart(t, &calls)
		task := SetupNVIDIAOnHost(slog.New(slog.DiscardHandler), gs).(*setupNVIDIAOnHost)
		task.stat = func(p string) (os.FileInfo, error) {
			if strings.HasSuffix(p, missing) {
				return nil, fs.ErrNotExist
			}

			return nil, nil //nolint:nilnil // only the error matters
		}

		err := task.Do(t.Context())
		require.ErrorContains(t, err, missing)
		require.ErrorContains(t, err, "NVIDIA Container Toolkit")
		require.Empty(t, calls, "nothing runs against a host without the toolkit")
	}
}

func TestSetupNVIDIAOnHostRequiresCompleteState(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*goalstates.NodeStart){
		"runtime not enabled": func(gs *goalstates.NodeStart) { gs.Containerd.NvidiaRuntime.Enabled = false },
		"no driver version":   func(gs *goalstates.NodeStart) { gs.Nvidia.DriverVersion = "" },
		"no libraries":        func(gs *goalstates.NodeStart) { gs.Nvidia.LibMappings = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var calls [][]string

			gs := gpuNodeStart(t, &calls)
			mutate(gs)

			err := SetupNVIDIAOnHost(slog.New(slog.DiscardHandler), gs).Do(t.Context())
			require.ErrorContains(t, err, "incomplete")
			require.Empty(t, calls)
		})
	}
}

func TestSetupNVIDIAOnHostReportsToolFailures(t *testing.T) {
	t.Parallel()

	for _, failing := range []string{"config", "cdi"} {
		var calls [][]string

		gs := gpuNodeStart(t, &calls)
		inner := gs.ExecOverride
		gs.ExecOverride = func(ctx context.Context, log *slog.Logger, args ...string) (string, error) {
			if args[1] == failing {
				return "", errors.New("boom")
			}

			return inner(ctx, log, args...)
		}

		task := SetupNVIDIAOnHost(slog.New(slog.DiscardHandler), gs).(*setupNVIDIAOnHost)
		task.stat = func(string) (os.FileInfo, error) { return nil, nil } //nolint:nilnil // only the error matters

		require.ErrorContains(t, task.Do(t.Context()), "boom", failing)
	}
}

// The shared NVIDIA tasks run through the goal state's Exec, so a node stack
// that is not an nspawn machine (the container backend) is driven the same way.
func TestSetupNVIDIARoutesCommandsThroughTheGoalStateExec(t *testing.T) {
	t.Parallel()

	var calls [][]string

	gs := gpuNodeStart(t, &calls)
	gs.Nvidia.ContainerLibDir = "/usr/lib/x86_64-linux-gnu"

	// The task fails somewhere (its fake exec produces no files); what matters
	// is that nothing escaped to nspawn and that the first commands went to the
	// override.
	_ = SetupNVIDIA(slog.New(slog.DiscardHandler), gs).Do(t.Context())

	require.NotEmpty(t, calls)
	require.Equal(t, []string{"mkdir", "-p", "/usr/lib/x86_64-linux-gnu"}, calls[0])
}

func TestReconcileNVIDIARoutesThroughTheGoalStateExec(t *testing.T) {
	t.Parallel()

	var calls [][]string

	gs := gpuNodeStart(t, &calls)
	task := ReconcileNVIDIA(slog.New(slog.DiscardHandler), gs).(*reconcileNVIDIA)
	task.execute = func(context.Context, *slog.Logger, phases.Task) error { return nil }

	require.NoError(t, task.Do(t.Context()))
	require.Equal(t, [][]string{
		{"systemctl", "stop", goalstates.SystemdUnitKubelet},
		{"systemctl", "stop", goalstates.SystemdUnitContainerd},
	}, calls)
}

func TestSetupNVIDIAOnHostUsesTheConfiguredToolkit(t *testing.T) {
	t.Parallel()

	const (
		ctk     = "/usr/local/nvidia/toolkit/nvidia-ctk"
		runtime = "/usr/local/nvidia/toolkit/nvidia-container-runtime"
	)

	var calls [][]string

	gs := gpuNodeStart(t, &calls)
	gs.NvidiaToolkit = goalstates.NvidiaToolkit{CTKPath: ctk, ContainerRuntimePath: runtime}

	var statted []string

	task := SetupNVIDIAOnHost(slog.New(slog.DiscardHandler), gs).(*setupNVIDIAOnHost)
	task.stat = func(p string) (os.FileInfo, error) {
		statted = append(statted, strings.TrimPrefix(p, strings.TrimSuffix(gs.Root().Path("/"), "/")))
		return nil, nil //nolint:nilnil // only the error matters
	}

	require.NoError(t, task.Do(t.Context()))

	require.Equal(t, []string{ctk, runtime}, statted,
		"the install check looks at the configured paths")
	require.Len(t, calls, 2)

	for _, call := range calls {
		require.Equal(t, ctk, call[0], "every command runs the configured nvidia-ctk")
	}

	require.NotContains(t, strings.Join(calls[0], " "), goalstates.NvidiaCTKPath)
}

func TestSetupNVIDIAOnHostNamesAMissingConfiguredToolkit(t *testing.T) {
	t.Parallel()

	var calls [][]string

	gs := gpuNodeStart(t, &calls)
	gs.NvidiaToolkit = goalstates.NvidiaToolkit{ContainerRuntimePath: "/opt/tk/nvidia-container-runtime"}

	task := SetupNVIDIAOnHost(slog.New(slog.DiscardHandler), gs).(*setupNVIDIAOnHost)
	task.stat = func(p string) (os.FileInfo, error) {
		if strings.HasSuffix(p, "/opt/tk/nvidia-container-runtime") {
			return nil, fs.ErrNotExist
		}

		return nil, nil //nolint:nilnil // only the error matters
	}

	err := task.Do(t.Context())
	require.ErrorContains(t, err, "/opt/tk/nvidia-container-runtime")
	require.Empty(t, calls, "nothing runs when the toolkit is missing")
}

func TestNVIDIAToolkitPathsDefaultAndConfigured(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{goalstates.NvidiaCTKPath, goalstates.NvidiaContainerRuntimePath}, NVIDIAToolkitPaths(goalstates.NvidiaToolkit{}))
	require.Equal(t, []string{"/a/ctk", "/a/runtime"}, NVIDIAToolkitPaths(goalstates.NvidiaToolkit{CTKPath: "/a/ctk", ContainerRuntimePath: "/a/runtime"}))
}
