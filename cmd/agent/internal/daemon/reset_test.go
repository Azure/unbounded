// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
)

func TestResetResourcesIncludesBPFFSMountCleanup(t *testing.T) {
	t.Parallel()

	taskName := resetResources(slog.New(slog.DiscardHandler)).Name()

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
			r, err := installstate.NewRecord("machine", "f", "")
			require.NoError(t, err)

			r.Phase = installstate.Resetting
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

// TestTeardownProceedsThroughAnUnreadableRecord covers the one file that can
// strand a host through both of its exits.
//
// decide rejects a record it cannot parse, so start is refused. If reset also
// refuses, nothing documented recovers the machine, and the guide tells
// operators to keep this file intact rather than delete it. Reset deletes it
// moments later regardless, so reading it is a courtesy, not a prerequisite.
//
// resetUnderLock itself syncs real host filesystems and needs root, so this
// covers the decision it delegates. Reintroducing a direct store.Load there
// would leave this helper uncalled, which staticcheck reports.
func TestTeardownProceedsThroughAnUnreadableRecord(t *testing.T) {
	t.Parallel()

	for _, content := range []string{"{", "null", `{"schemaVersion":99}`, `{"schemaVersion":1,"phase":"bogus"}`} {
		t.Run(content, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			store := installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock"))

			require.NoError(t, os.MkdirAll(store.Root(), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(store.Root(), "install-state.json"), []byte(content), 0o600))

			// Confirm the premise: this is a record start would refuse.
			_, loadErr := store.Load()
			require.Error(t, loadErr, "fixture must be a record the store rejects")

			r, err := recordForTeardown(discardLogger(), store)
			require.NoError(t, err, "an unreadable record must not block the thing that deletes it")
			require.NoError(t, r.Validate(), "the replacement must be usable for teardown")
		})
	}
}

// TestTeardownKeepsAReadableRecord confirms the replacement above is a fallback
// and not the normal path: a record reset can read is the one it tears down,
// so the machine name and fingerprint stay accurate through reset.
func TestTeardownKeepsAReadableRecord(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	store := installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock"))

	saved, err := installstate.NewRecord("machine-1", "fingerprint-1", "")
	require.NoError(t, err)
	require.NoError(t, store.Save(saved))

	r, err := recordForTeardown(discardLogger(), store)
	require.NoError(t, err)
	require.Equal(t, "machine-1", r.MachineName)
	require.Equal(t, "fingerprint-1", r.ConfigFingerprint)
}

// TestResetRemovesTheFirstBootUnitBeforeArtifacts pins that reset actually runs
// the removal, not merely that the removal works.
//
// The unit runs on every boot and decides there is nothing to do from the
// ownership record that reset is about to delete. Left behind, it would find an
// uninstalled host and bootstrap it again, undoing the reset with nothing
// reporting why. Ordering it before the artifacts means a failure stops the
// reset while the host is still recognizably installed.
func TestResetRemovesTheFirstBootUnitBeforeArtifacts(t *testing.T) {
	t.Parallel()

	taskName := resetResources(slog.New(slog.DiscardHandler)).Name()

	assert.Contains(t, taskName, "remove-first-boot-unit",
		"reset must remove the Ignition bootstrap unit or the host re-bootstraps on next boot")
	assert.Less(t,
		strings.Index(taskName, "remove-first-boot-unit"),
		strings.Index(taskName, "remove-agent-artifacts"),
		"a failure here must stop the reset while the host is still recognizably installed")
}
