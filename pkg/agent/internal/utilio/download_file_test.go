// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package utilio

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDownloadFromRemoteFileURL(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(source, []byte("content"), 0o644))

	body, err := downloadFromRemote(context.Background(), "file://"+source)
	require.NoError(t, err)

	defer body.Close() //nolint:errcheck // test cleanup

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "content", string(got))
}

func TestDownloadFromRemoteAbsolutePath(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(source, []byte("content"), 0o644))

	body, err := downloadFromRemote(context.Background(), source)
	require.NoError(t, err)

	defer body.Close() //nolint:errcheck // test cleanup

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "content", string(got))
}

func TestDownloadFromRemoteRejectsRelativePath(t *testing.T) {
	_, err := downloadFromRemote(context.Background(), "relative/path")
	require.ErrorContains(t, err, "absolute path")
}
