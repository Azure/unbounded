// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOwnedReplayDiscardsIncompleteTreeBeforeRetry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	partial := filepath.Join(dir, "partial-layer")
	require.NoError(t, os.WriteFile(partial, []byte("incomplete"), 0o600))
	task := DownloadOwnedRootFS(slog.New(slog.DiscardHandler), dir, "amd64", "oci-layout://"+filepath.Join(t.TempDir(), "missing"))
	require.Error(t, task.Do(t.Context()))

	_, err := os.Stat(partial)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(dir, ".unbounded-rootfs-complete"))
	require.ErrorIs(t, err, os.ErrNotExist, "failed extraction must remain replayable")
}

func TestRootFSReplayPreservesCompletedOrLegacyTree(t *testing.T) {
	t.Parallel()

	for _, owned := range []bool{false, true} {
		dir := t.TempDir()
		payload := filepath.Join(dir, "payload")
		require.NoError(t, os.WriteFile(payload, []byte("preserve"), 0o600))

		if owned {
			require.NoError(t, os.WriteFile(filepath.Join(dir, ".unbounded-rootfs-complete"), []byte("complete\n"), 0o600))
		}

		task := DownloadRootFS(slog.New(slog.DiscardHandler), dir, "amd64", "unavailable")
		if owned {
			task = DownloadOwnedRootFS(slog.New(slog.DiscardHandler), dir, "amd64", "unavailable")
		}

		require.NoError(t, task.Do(t.Context()))

		data, err := os.ReadFile(payload)
		require.NoError(t, err)
		require.Equal(t, "preserve", string(data))
	}
}
