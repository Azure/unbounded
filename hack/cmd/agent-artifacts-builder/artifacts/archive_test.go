// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteBundleArchive(t *testing.T) {
	t.Parallel()

	rootDir := filepath.Join(t.TempDir(), "bundle")
	require.NoError(t, os.MkdirAll(filepath.Join(rootDir, "runc", "v1.5.0"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "manifest.json"), []byte("manifest"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "LICENSE"), []byte("project license"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "NOTICE"), []byte("third-party notices"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "runc", "v1.5.0", "runc.amd64"), []byte("runc"), 0o755))

	archivePath := filepath.Join(t.TempDir(), "bootstrap-artifacts.tar.gz")
	require.NoError(t, WriteBundleArchive(rootDir, archivePath))

	files := readBundleArchive(t, archivePath)
	require.Equal(t, map[string]string{
		"LICENSE":                "project license",
		"NOTICE":                 "third-party notices",
		"manifest.json":          "manifest",
		"runc/v1.5.0/runc.amd64": "runc",
	}, files)

	archive, err := os.ReadFile(archivePath)
	require.NoError(t, err)

	secondArchivePath := filepath.Join(t.TempDir(), "bootstrap-artifacts.tar.gz")
	require.NoError(t, WriteBundleArchive(rootDir, secondArchivePath))

	secondArchive, err := os.ReadFile(secondArchivePath)
	require.NoError(t, err)
	require.Equal(t, archive, secondArchive)

	checksum, err := os.ReadFile(archivePath + ".sha256")
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("%x  bootstrap-artifacts.tar.gz\n", sha256.Sum256(archive)), string(checksum))
}

func TestWriteBundleArchiveRejectsOutputInsideBundle(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(rootDir, "manifest.json"), []byte("manifest"), 0o644))

	err := WriteBundleArchive(rootDir, filepath.Join(rootDir, "bundle.tar.gz"))
	require.ErrorContains(t, err, "must be outside bundle root")
}

func TestWriteBundleArchiveRejectsSymlink(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	externalPath := filepath.Join(t.TempDir(), "external")
	require.NoError(t, os.WriteFile(externalPath, []byte("secret"), 0o644))
	require.NoError(t, os.Symlink(externalPath, filepath.Join(rootDir, "linked-artifact")))

	err := WriteBundleArchive(rootDir, filepath.Join(t.TempDir(), "bundle.tar.gz"))
	require.ErrorContains(t, err, "is not a regular file")
}

func readBundleArchive(t *testing.T, archivePath string) map[string]string {
	t.Helper()

	file, err := os.Open(archivePath)
	require.NoError(t, err)

	defer file.Close() //nolint:errcheck // test cleanup

	gzipReader, err := gzip.NewReader(file)
	require.NoError(t, err)

	defer gzipReader.Close() //nolint:errcheck // test cleanup

	files := map[string]string{}
	tarReader := tar.NewReader(gzipReader)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}

		require.NoError(t, err)

		data, err := io.ReadAll(tarReader)
		require.NoError(t, err)

		files[header.Name] = string(data)
	}

	return files
}
