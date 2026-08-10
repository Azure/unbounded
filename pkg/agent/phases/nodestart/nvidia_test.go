// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestSetupNVIDIAProvisionedCapabilityControlsBehavior(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	require.NoError(t, SetupNVIDIA(log, &goalstates.NodeStart{}).Do(context.Background()))

	err := SetupNVIDIA(log, &goalstates.NodeStart{NVIDIARequired: true}).Do(context.Background())
	require.ErrorContains(t, err, "NVIDIA is required")
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
