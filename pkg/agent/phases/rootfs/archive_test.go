// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/stretchr/testify/require"
)

// tarGzArchive builds a gzip-compressed tar archive from name to content.
func tarGzArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var output bytes.Buffer

	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)

	for name, content := range files {
		require.NoError(t, tarWriter.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o755,
			Size: int64(len(content)),
		}))
		_, err := tarWriter.Write([]byte(content))
		require.NoError(t, err)
	}

	require.NoError(t, tarWriter.Close())
	require.NoError(t, gzipWriter.Close())

	return output.Bytes()
}
