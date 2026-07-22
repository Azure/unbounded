// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifactsource

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceOpenHTTPSURL(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("artifact-data"))
	}))
	defer server.Close()

	body, err := openHTTPWithClient(context.Background(), server.Client(), server.URL+"/artifact")
	require.NoError(t, err)

	defer body.Close() //nolint:errcheck // test cleanup

	got, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, "artifact-data", string(got))
}

func TestSourceOpenHTTPErrorRedactsQuery(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	_, err := openHTTPWithClient(context.Background(), server.Client(), server.URL+"/artifact?sp=r&sig=secret")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
	require.NotContains(t, err.Error(), "sig")
	require.Contains(t, err.Error(), "REDACTED")
}

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

func TestSourceOpenFileURLUnescapesPath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "artifact with space")
	require.NoError(t, os.WriteFile(path, []byte("artifact-data"), 0o644))

	source, err := Parse((&url.URL{Scheme: "file", Path: path}).String())
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

func TestParseRejectsOCIWithoutBlobTitle(t *testing.T) {
	t.Parallel()

	_, err := Parse("oci://registry.example.com/unbounded/bootstrap-artifacts:v1")
	require.ErrorContains(t, err, "blob title fragment")
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
