// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/installstate"
)

func TestResetAgentResourcesIncludesBPFFSMountCleanup(t *testing.T) {
	t.Parallel()

	taskName := ResetAgentResources(slog.New(slog.DiscardHandler)).Name()

	assert.Contains(t, taskName, "parallel(remove-bpffs-mount, remove-bpffs-mount)")
	assert.Less(t, strings.Index(taskName, "parallel(remove-machine, remove-machine)"), strings.Index(taskName, "parallel(remove-bpffs-mount, remove-bpffs-mount)"))
	assert.Less(t, strings.Index(taskName, "parallel(remove-bpffs-mount, remove-bpffs-mount)"), strings.Index(taskName, "cleanup-routes"))
}

func TestResetRetainsOwnershipUntilTeardownAndSyncSucceed(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"cleanup", "sync", ""} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			store := installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock"))
			r, err := installstate.NewRecord("machine", "f")
			require.NoError(t, err)

			r.Checkpoint = installstate.Resetting
			require.NoError(t, store.Save(r))

			injected := errors.New("injected reset failure")
			cleaned := false
			task := lifecycleTask{name: "cleanup", run: func(context.Context) error {
				if failure == "cleanup" {
					return injected
				}

				cleaned = true

				return nil
			}}
			synced := false

			err = durableReset(t.Context(), store, task, []string{store.Root()}, func(int) error {
				require.True(t, cleaned)

				_, err := store.Load()
				require.NoError(t, err, "ownership must remain during filesystem barrier")

				if failure == "sync" {
					return injected
				}

				synced = true

				return nil
			})
			if failure != "" {
				require.ErrorIs(t, err, injected)
				loaded, err := store.Load()
				require.NoError(t, err)
				require.Equal(t, r, loaded)
			} else {
				require.NoError(t, err)
				require.True(t, synced)

				_, err = store.Load()
				require.ErrorIs(t, err, installstate.ErrNotFound)
			}
		})
	}
}
