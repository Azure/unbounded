// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestSetupNVIDIAProvisionedCapabilityControlsBehavior(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	require.NoError(t, SetupNVIDIA(log, &goalstates.NodeStart{}).Do(context.Background()))

	err := SetupNVIDIA(log, &goalstates.NodeStart{Nvidia: goalstates.NvidiaHost{Required: true}}).Do(context.Background())
	require.ErrorContains(t, err, "NVIDIA is required")
}

func TestReconcileNVIDIANonGPUIsNoop(t *testing.T) {
	t.Parallel()

	require.NoError(t, ReconcileNVIDIA(
		slog.New(slog.DiscardHandler),
		&goalstates.NodeStart{MachineName: "kube1"},
	).Do(context.Background()))
}

func TestConfigureNVIDIAContainerRuntime(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		runErr  error
		wantErr string
	}{
		{name: "successful setup uses CDI"},
		{
			name:    "configuration failure is explicit",
			runErr:  errors.New("exit status 1"),
			wantErr: "configure NVIDIA container runtime CDI mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotArgs []string

			task := &setupNVIDIA{
				log: slog.New(slog.DiscardHandler),
				goalState: &goalstates.NodeStart{
					MachineName: "kube1",
				},
				run: func(_ context.Context, _ *slog.Logger, machine string, args ...string) (string, error) {
					require.Equal(t, "kube1", machine)

					gotArgs = slices.Clone(args)

					return "", tt.runErr
				},
			}

			err := task.configureContainerRuntime(context.Background())
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, []string{
				goalstates.NvidiaCTKPath,
				"config",
				"--set",
				"nvidia-container-runtime.mode=cdi",
				"--in-place",
			}, gotArgs)
		})
	}
}

func TestNVIDIAContainerLibDirCommands(t *testing.T) {
	t.Parallel()

	cmds := nvidiaContainerLibDirCommands("/usr/lib/aarch64-linux-gnu")
	require.Len(t, cmds, 2)
	require.Equal(t, []string{"mkdir", "-p", "/usr/lib/aarch64-linux-gnu"}, cmds[0])
	require.Equal(t, nvidiaContainerLinkerConfig, cmds[1][len(cmds[1])-1])

	// Run the config command with a temporary target to check what it writes.
	dir := t.TempDir()
	target := filepath.Join(dir, "unbounded-nvidia.conf")
	args := slices.Clone(cmds[1])
	args[len(args)-1] = target

	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	require.NoError(t, err, string(out))

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "/usr/lib/aarch64-linux-gnu\n", string(got))
}

func TestRequiredVersionedNVIDIALibraryPaths(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{
		"/run/nvidia/driver/lib/x86_64-linux-gnu/libcuda.so.580.167.08",
		"/run/nvidia/driver/lib/x86_64-linux-gnu/libnvidia-ml.so.580.167.08",
	}, requiredVersionedNVIDIALibraryPaths("/run/nvidia/driver/lib/x86_64-linux-gnu", "580.167.08"))
}

func TestVersionedNVIDIALibraryCopiesMaterializesRequiredNames(t *testing.T) {
	t.Parallel()

	libs := []goalstates.NvidiaLibMapping{
		{
			HostPath:      "/usr/lib/x86_64-linux-gnu/libcuda.so",
			ContainerPath: "/run/host-nvidia/0/libcuda.so",
		},
		{
			HostPath:      "/usr/lib/x86_64-linux-gnu/libcuda.so.1",
			ContainerPath: "/run/host-nvidia/0/libcuda.so.1",
		},
		{
			HostPath:      "/usr/lib/x86_64-linux-gnu/libnvidia-ml.so.1",
			ContainerPath: "/run/host-nvidia/0/libnvidia-ml.so.1",
		},
	}

	copies := versionedNVIDIALibraryCopies(libs, "580.167.08")
	require.Equal(t, []nvidiaVersionedLibraryCopy{
		{
			source:              "/run/host-nvidia/0/libcuda.so.1",
			relativeDestination: "libcuda.so.580.167.08",
		},
		{
			source:              "/run/host-nvidia/0/libnvidia-ml.so.1",
			relativeDestination: "libnvidia-ml.so.580.167.08",
		},
	}, copies)
}

func TestVersionedNVIDIALibraryCopiesDoesNotReplaceDiscoveredVersionedFile(t *testing.T) {
	t.Parallel()

	libs := []goalstates.NvidiaLibMapping{
		{
			HostPath:      "/usr/lib/x86_64-linux-gnu/libcuda.so.1",
			ContainerPath: "/run/host-nvidia/0/libcuda.so.1",
		},
		{
			HostPath:      "/usr/lib/x86_64-linux-gnu/libcuda.so.580.167.08",
			ContainerPath: "/run/host-nvidia/0/libcuda.so.580.167.08",
		},
	}

	require.Empty(t, versionedNVIDIALibraryCopies(libs, "580.167.08"))
}
