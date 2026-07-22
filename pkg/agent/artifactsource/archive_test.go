// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifactsource

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceMaterializeArchive(t *testing.T) {
	t.Parallel()

	archivePath := filepath.Join(t.TempDir(), "offline-artifacts.tar.gz")
	writeTestTarGz(t, archivePath, map[string]string{
		"bundle/manifest.json": "{}",
		"bundle/runc":          "runc",
	})

	source, err := Parse(archivePath)
	require.NoError(t, err)

	opts := ArchiveCacheOptions{
		CacheRoot:   filepath.Join(t.TempDir(), "cache"),
		RootMarker:  "manifest.json",
		ReadyMarker: ".ready",
	}
	cache, err := source.MaterializeArchive(context.Background(), opts)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(cache.Root(), "manifest.json"))
	require.FileExists(t, filepath.Join(cache.Root(), "runc"))
	require.NoError(t, cache.MarkReady())

	require.NoError(t, os.Remove(archivePath))

	cached, err := source.MaterializeArchive(context.Background(), opts)
	require.NoError(t, err)
	require.Equal(t, cache.Root(), cached.Root())
}

func TestCacheKeyIgnoresSignedQuery(t *testing.T) {
	t.Parallel()

	withoutQuery := CacheKey("https://artifacts.example.test/bootstrap")
	firstSAS := CacheKey("https://artifacts.example.test/bootstrap?sp=r&sig=first-secret")
	rotatedSAS := CacheKey("https://artifacts.example.test/bootstrap?sp=r&sig=rotated-secret")

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
