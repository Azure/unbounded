// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package bootstrapartifacts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/artifactsource"
)

func TestMaterializeHTTPSArchive(t *testing.T) {
	t.Parallel()

	archivePath := filepath.Join(t.TempDir(), "offline-artifacts.tar.gz")
	writeTestTarGz(t, archivePath, map[string]string{
		"bundle/manifest.json": "{}",
		"bundle/runc":          "runc",
	})

	source, err := artifactsource.Parse(archivePath)
	require.NoError(t, err)

	storageRoot := filepath.Join(t.TempDir(), "archives")
	archive, err := materializeHTTPSArchive(context.Background(), source, storageRoot)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(archive.root, "manifest.json"))
	require.FileExists(t, filepath.Join(archive.root, "runc"))
	require.NoError(t, archive.markValidated())

	require.NoError(t, os.Remove(archivePath))

	cached, err := materializeHTTPSArchive(context.Background(), source, storageRoot)
	require.NoError(t, err)
	require.Equal(t, archive.root, cached.root)
}

func TestSourceKeyIgnoresSignedQuery(t *testing.T) {
	t.Parallel()

	withoutQuery := SourceKey("https://artifacts.example.test/bootstrap")
	firstSAS := SourceKey("https://artifacts.example.test/bootstrap?sp=r&sig=first-secret")
	rotatedSAS := SourceKey("https://artifacts.example.test/bootstrap?sp=r&sig=rotated-secret")

	require.Equal(t, withoutQuery, firstSAS)
	require.Equal(t, firstSAS, rotatedSAS)
	require.NotContains(t, firstSAS, "secret")
	require.NotContains(t, firstSAS, "sig")
}

func writeTestTarGz(t *testing.T, path string, files map[string]string) {
	t.Helper()

	var buffer bytes.Buffer

	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)

	for name, contents := range files {
		require.NoError(t, tarWriter.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(contents)),
		}))
		_, err := tarWriter.Write([]byte(contents))
		require.NoError(t, err)
	}

	require.NoError(t, tarWriter.Close())
	require.NoError(t, gzipWriter.Close())
	require.NoError(t, os.WriteFile(path, buffer.Bytes(), 0o644))
}
