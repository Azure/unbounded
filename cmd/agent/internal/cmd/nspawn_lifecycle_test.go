// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNSpawnLifecycleCommandHasExplicitOperations(t *testing.T) {
	cmd := newCmdNSpawnLifecycle(&CommandContext{LogFormat: "text"})
	require.True(t, cmd.Hidden)
	require.Len(t, cmd.Commands(), 3)
	require.Equal(t, "post-start", cmd.Commands()[0].Name())
	require.Equal(t, "pre-start", cmd.Commands()[1].Name())
	require.Equal(t, "reconcile", cmd.Commands()[2].Name())

	cmd.SetArgs([]string{"pre-start", "other"})
	require.ErrorContains(t, cmd.ExecuteContext(context.Background()), "unknown nspawn machine")
}

func TestNewNSpawnLifecycle(t *testing.T) {
	t.Parallel()

	lifecycle, err := newNSpawnLifecycle(testLogger())
	require.NoError(t, err)
	require.NotNil(t, lifecycle)
}
