package reset

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoveFileIfExists(t *testing.T) {
	t.Parallel()

	t.Run("file exists", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "test-file")
		require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

		removeFileIfExists(path)

		_, err := os.Stat(path)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("file does not exist", func(t *testing.T) {
		t.Parallel()

		// Should not panic or error.
		removeFileIfExists(filepath.Join(t.TempDir(), "nonexistent-file"))
	})
}

func TestRemoveAllIfExists(t *testing.T) {
	t.Parallel()

	t.Run("directory exists", func(t *testing.T) {
		t.Parallel()

		dir := filepath.Join(t.TempDir(), "subdir")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "file"), []byte("data"), 0o644))

		err := removeAllIfExists(dir)
		require.NoError(t, err)

		_, statErr := os.Stat(dir)
		assert.True(t, os.IsNotExist(statErr))
	})

	t.Run("path does not exist", func(t *testing.T) {
		t.Parallel()

		err := removeAllIfExists(filepath.Join(t.TempDir(), "nonexistent-dir"))
		require.NoError(t, err)
	})
}
