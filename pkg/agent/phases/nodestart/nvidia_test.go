// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"context"
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
