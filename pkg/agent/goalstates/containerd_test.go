// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package goalstates

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveContainerdOptions(t *testing.T) {
	t.Parallel()

	require.False(t, ResolveContainerd(ContainerdOptions{}).NvidiaRuntime.Enabled)
	resolved := ResolveContainerd(ContainerdOptions{SandboxImage: "example/sandbox:v1", NvidiaRequired: true})
	require.True(t, resolved.NvidiaRuntime.Enabled)
	require.Equal(t, "example/sandbox:v1", resolved.SandboxImage)
}

func TestNvidiaToolkitDefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	var zero NvidiaToolkit

	require.Equal(t, NvidiaCTKPath, zero.CTK())
	require.Equal(t, NvidiaContainerRuntimePath, zero.ContainerRuntime())

	custom := NvidiaToolkit{CTKPath: "/opt/tk/nvidia-ctk", ContainerRuntimePath: "/opt/tk/nvidia-container-runtime"}
	require.Equal(t, "/opt/tk/nvidia-ctk", custom.CTK())
	require.Equal(t, "/opt/tk/nvidia-container-runtime", custom.ContainerRuntime())

	partial := NvidiaToolkit{CTKPath: "/opt/tk/nvidia-ctk"}
	require.Equal(t, NvidiaContainerRuntimePath, partial.ContainerRuntime(), "an unset field keeps its default")
}

func TestResolveContainerdNvidiaRuntimePath(t *testing.T) {
	t.Parallel()

	require.Equal(t, NvidiaContainerRuntimePath,
		ResolveContainerd(ContainerdOptions{NvidiaRequired: true}).NvidiaRuntime.RuntimePath,
		"no toolkit setting leaves the runtime path as it was")

	resolved := ResolveContainerd(ContainerdOptions{
		NvidiaRequired: true,
		NvidiaToolkit:  NvidiaToolkit{ContainerRuntimePath: "/opt/tk/nvidia-container-runtime"},
	})
	require.Equal(t, "/opt/tk/nvidia-container-runtime", resolved.NvidiaRuntime.RuntimePath)
}
