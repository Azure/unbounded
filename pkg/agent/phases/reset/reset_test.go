// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reset

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoveFileIfExists(t *testing.T) {
	t.Parallel()

	log := slog.Default()

	t.Run("file exists", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "test-file")
		require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

		removeFileIfExists(log, path)

		_, err := os.Stat(path)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("file does not exist", func(t *testing.T) {
		t.Parallel()

		// Should not panic or error.
		removeFileIfExists(log, filepath.Join(t.TempDir(), "nonexistent-file"))
	})
}

func TestRemoveAllIfExists(t *testing.T) {
	t.Parallel()

	log := slog.Default()

	t.Run("directory exists", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "subdir")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("data"), 0o644))

		removeAllIfExists(log, dir)

		_, statErr := os.Stat(dir)
		assert.True(t, os.IsNotExist(statErr))
	})

	t.Run("path does not exist", func(t *testing.T) {
		t.Parallel()

		// Should not panic or error.
		removeAllIfExists(log, filepath.Join(t.TempDir(), "nonexistent-dir"))
	})
}
