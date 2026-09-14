// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestArtifactFilesSortsForStableManifests(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, name := range []string{"unbounded-nspawn.raw.sha256", "unbounded-nspawn.raw", "unbounded-nspawn.provenance"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600))
	}

	got, err := artifactFiles(dir)
	require.NoError(t, err)

	// Sorted, so the layer order and therefore the manifest digest is stable
	// across builds of identical content.
	assert.Equal(t, []string{
		"unbounded-nspawn.provenance",
		"unbounded-nspawn.raw",
		"unbounded-nspawn.raw.sha256",
	}, got)
}

func TestArtifactFilesRejectsEmptyDirectory(t *testing.T) {
	t.Parallel()

	_, err := artifactFiles(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is empty")
}

// TestArtifactFilesRejectsNestedDirectory guards against silently publishing a
// partial artifact if the build ever emits a subdirectory.
func TestArtifactFilesRejectsNestedDirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "unbounded-nspawn.raw"), []byte("x"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o750))

	_, err := artifactFiles(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected directory")
}

func TestArtifactFilesRejectsMissingDirectory(t *testing.T) {
	t.Parallel()

	_, err := artifactFiles(filepath.Join(t.TempDir(), "does-not-exist"))
	require.Error(t, err)
}
