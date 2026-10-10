// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"log/slog"
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

// TestPreStartAfterMigrate: a host root that cannot be migrated must not keep
// the machine from starting. It starts with the config it has.
func TestPreStartAfterMigrate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		migrateErr     error
		regenerateErr  error
		wantRegenerate bool
		wantErr        string
	}{
		{name: "migrated", wantRegenerate: true},
		{name: "the host root cannot be migrated", migrateErr: errors.New("installed under both")},
		{name: "regeneration fails", regenerateErr: errors.New("bad config"), wantRegenerate: true, wantErr: "bad config"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			regenerated := false
			err := preStartAfterMigrate(testLogger(),
				func(*slog.Logger) error { return tt.migrateErr },
				func() error {
					regenerated = true

					return tt.regenerateErr
				})

			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}

			require.Equal(t, tt.wantRegenerate, regenerated)
		})
	}
}
