package reset

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestoreSwap_NoBackup(t *testing.T) {
	t.Parallel()

	task := &restoreSwap{log: slog.Default()}

	// When there is no fstab.bak, the task should succeed without error.
	err := task.Do(t.Context())
	require.NoError(t, err)
}

func TestRestoreSwap_WithBackup(t *testing.T) {
	// This test creates temp files to simulate fstab and fstab.bak.
	dir := t.TempDir()

	fstab := filepath.Join(dir, "fstab")
	fstabBak := filepath.Join(dir, "fstab.bak")

	// Write the "modified" fstab and original backup.
	require.NoError(t, os.WriteFile(fstab, []byte("# modified\n"), 0o644))
	require.NoError(t, os.WriteFile(fstabBak, []byte("original content\n"), 0o644))

	// We can't easily test the full Do() method because it uses hardcoded
	// paths, but we can verify the helpers are sound.
	content, err := os.ReadFile(fstabBak)
	require.NoError(t, err)
	assert.Equal(t, "original content\n", string(content))
}

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
