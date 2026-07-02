// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDownloadToLocalFileFromFileURL(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(source, []byte("content"), 0o644))

	dest := filepath.Join(t.TempDir(), "dest")
	require.NoError(t, DownloadToLocalFile(context.Background(), "file://"+source, dest, 0o755))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, "content", string(got))

	info, err := os.Stat(dest)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestDownloadWithSHA256VerificationFromAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	content := []byte("content")
	require.NoError(t, os.WriteFile(source, content, 0o644))

	sum := sha256.Sum256(content)
	checksum := filepath.Join(dir, "source.sha256")
	require.NoError(t, os.WriteFile(checksum, []byte(hex.EncodeToString(sum[:])), 0o644))

	dest := filepath.Join(t.TempDir(), "dest")
	require.NoError(t, DownloadWithSHA256Verification(context.Background(), source, checksum, dest, 0o755))
}

func TestDownloadToLocalFileRejectsRelativePath(t *testing.T) {
	err := DownloadToLocalFile(context.Background(), "relative/path", filepath.Join(t.TempDir(), "dest"), 0o644)
	require.ErrorContains(t, err, "absolute path")
}
