// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifactsource

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceOpenFileURL(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "artifact")
	require.NoError(t, os.WriteFile(path, []byte("artifact-data"), 0o644))

	source, err := Parse("file://" + path)
	require.NoError(t, err)

	body, err := source.Open(context.Background())
	require.NoError(t, err)
	defer body.Close() //nolint:errcheck // test cleanup

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "artifact-data", string(got))
}

func TestSourceOpenAbsolutePath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "artifact")
	require.NoError(t, os.WriteFile(path, []byte("artifact-data"), 0o644))

	source, err := Parse(path)
	require.NoError(t, err)

	body, err := source.Open(context.Background())
	require.NoError(t, err)
	defer body.Close() //nolint:errcheck // test cleanup

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "artifact-data", string(got))
}

func TestParseRejectsRelativePath(t *testing.T) {
	t.Parallel()

	_, err := Parse("relative/path")
	require.ErrorContains(t, err, "absolute path")
}

func TestReadExpectedSHA256(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "artifact.sha256")
	require.NoError(t, os.WriteFile(path, []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  artifact\n"), 0o644))

	source, err := Parse(path)
	require.NoError(t, err)

	got, err := ReadExpectedSHA256(context.Background(), source)
	require.NoError(t, err)
	require.Equal(t, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", got)
}
