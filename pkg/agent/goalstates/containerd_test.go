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
