// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractTarGzip(t *testing.T) {
	t.Parallel()

	archive := testTarArchive(t, true, map[string]string{
		"bundle/manifest.json": "{}",
		"bundle/bin/kubelet":   "kubelet",
	})
	dest := t.TempDir()

	require.NoError(t, ExtractTar(bytes.NewReader(archive), dest))

	got, err := os.ReadFile(filepath.Join(dest, "bundle", "bin", "kubelet"))
	require.NoError(t, err)
	require.Equal(t, "kubelet", string(got))
}

func TestExtractTarPlain(t *testing.T) {
	t.Parallel()

	archive := testTarArchive(t, false, map[string]string{"oci-layout": "{}"})
	dest := t.TempDir()

	require.NoError(t, ExtractTar(bytes.NewReader(archive), dest))
	require.FileExists(t, filepath.Join(dest, "oci-layout"))
}

func TestExtractTarAcceptsPAXGlobalHeader(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer

	writer := tar.NewWriter(&buffer)

	require.NoError(t, writer.WriteHeader(&tar.Header{
		Name:     "pax_global_header",
		Typeflag: tar.TypeXGlobalHeader,
		PAXRecords: map[string]string{
			"comment": "test archive",
		},
	}))
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: 2}))
	_, err := writer.Write([]byte("{}"))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	dest := t.TempDir()
	require.NoError(t, ExtractTar(bytes.NewReader(buffer.Bytes()), dest))
	require.FileExists(t, filepath.Join(dest, "manifest.json"))
}

func TestExtractTarRejectsTraversal(t *testing.T) {
	t.Parallel()

	archive := testTarArchive(t, false, map[string]string{"../escape": "bad"})
	err := ExtractTar(bytes.NewReader(archive), t.TempDir())
	require.ErrorContains(t, err, "invalid tar entry")
}

func TestExtractTarRejectsSymlink(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer

	writer := tar.NewWriter(&buffer)
	require.NoError(t, writer.WriteHeader(&tar.Header{
		Name:     "link",
		Typeflag: tar.TypeSymlink,
		Linkname: "/etc/passwd",
		Mode:     0o777,
	}))
	require.NoError(t, writer.Close())

	err := ExtractTar(bytes.NewReader(buffer.Bytes()), t.TempDir())
	require.ErrorContains(t, err, "unsupported tar entry type")
}

func testTarArchive(t *testing.T, compressed bool, files map[string]string) []byte {
	t.Helper()

	var (
		buffer     bytes.Buffer
		body       io.Writer = &buffer
		gzipWriter *gzip.Writer
	)

	if compressed {
		gzipWriter = gzip.NewWriter(&buffer)
		body = gzipWriter
	}

	writer := tar.NewWriter(body)

	for name, contents := range files {
		require.NoError(t, writer.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(contents)),
		}))
		_, err := writer.Write([]byte(contents))
		require.NoError(t, err)
	}

	require.NoError(t, writer.Close())

	if gzipWriter != nil {
		require.NoError(t, gzipWriter.Close())
	}

	return buffer.Bytes()
}
