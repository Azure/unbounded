// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package oci

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestDownloadRootFSSkipsCompletedRootFS covers the ordinary re-run: a rootfs
// that finished extracting is left alone, and no image is fetched.
//
// The empty image reference would fail if extraction were attempted, which is
// what makes this assert a skip rather than a success.
func TestDownloadRootFSSkipsCompletedRootFS(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "payload"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, rootfsCompleteMarker), nil, 0o644))

	task := DownloadRootFS(discardLogger(), dir, "amd64", "", RebuildNever)
	require.NoError(t, task.Do(context.Background()))

	_, err := os.Stat(filepath.Join(dir, "payload"))
	assert.NoError(t, err)
}

// TestDownloadRootFSRefusesUnownedContent is the safety property. A rootfs
// without the completion marker is not necessarily incomplete: installations
// made before the marker existed have content and no marker, and so does a
// directory belonging to something else.
//
// Deleting on that evidence would destroy a working node, so the default must
// be to refuse and say so.
func TestDownloadRootFSRefusesUnownedContent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	legacy := filepath.Join(dir, "usr")
	require.NoError(t, os.MkdirAll(legacy, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "payload"), []byte("x"), 0o644))

	task := DownloadRootFS(discardLogger(), dir, "amd64", "", RebuildNever)
	err := task.Do(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not marked complete")
	assert.Contains(t, err.Error(), "reset")

	// Nothing was touched.
	_, statErr := os.Stat(filepath.Join(legacy, "payload"))
	assert.NoError(t, statErr, "an unowned rootfs must survive untouched")
}

// TestDownloadRootFSRebuildsOwnedIncompleteRootFS covers the interrupted
// extraction that recovery exists for. Only a caller that has established
// ownership may ask for this.
func TestDownloadRootFSRebuildsOwnedIncompleteRootFS(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	partial := filepath.Join(dir, "half-written")
	require.NoError(t, os.WriteFile(partial, []byte("x"), 0o644))

	task := DownloadRootFS(discardLogger(), dir, "amd64", "", RebuildOwned)
	err := task.Do(context.Background())

	// Extraction was attempted rather than skipped; it fails only because this
	// test supplies no usable image.
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not marked complete")

	_, statErr := os.Stat(partial)
	assert.True(t, os.IsNotExist(statErr),
		"an owned incomplete rootfs must not survive into the retry")
}

// TestDownloadRootFSTreatsMissingDirAsFresh keeps the first-run path working
// under the safe default.
func TestDownloadRootFSTreatsMissingDirAsFresh(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "absent")

	task := DownloadRootFS(discardLogger(), dir, "amd64", "", RebuildNever)
	err := task.Do(context.Background())

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "not marked complete")
	assert.NotContains(t, err.Error(), "check machine directory")
}
