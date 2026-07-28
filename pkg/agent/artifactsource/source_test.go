// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifactsource

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"oras.land/oras-go/v2/registry/remote/retry"
)

func TestParseRedactsInvalidURL(t *testing.T) {
	t.Parallel()

	_, err := Parse("https://artifacts.example.test/%zz?sig=secret")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
}

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

func TestDownloadToLocalFileRetriesInterruptedHTTPBody(t *testing.T) {
	t.Parallel()

	const content = "artifact-data"

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "13")

		if attempts.Add(1) == 1 {
			_, _ = w.Write([]byte("partial"))

			return
		}

		_, _ = w.Write([]byte(content))
	}))
	defer server.Close()

	source, err := Parse(server.URL + "/artifact")
	require.NoError(t, err)

	dest := filepath.Join(t.TempDir(), "artifact")
	require.NoError(t, source.DownloadToLocalFile(t.Context(), dest, 0o644))
	require.EqualValues(t, 2, attempts.Load())

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.Equal(t, content, string(got))
}

func TestHTTPClientUsesRetryTransport(t *testing.T) {
	t.Parallel()

	transport, ok := httpClient.Transport.(*retry.Transport)
	require.Truef(t, ok, "httpClient.Transport = %T, want *retry.Transport", httpClient.Transport)
	require.NotNil(t, transport.Policy)
}

func TestHTTPDownloadRetryPolicyRetriesTransportErrors(t *testing.T) {
	t.Parallel()

	tests := []error{
		&net.DNSError{
			Err:        "no such host",
			Name:       "artifacts.example.test",
			IsNotFound: true,
		},
		&net.OpError{
			Op:  "dial",
			Err: syscall.ECONNREFUSED,
		},
	}

	for _, transportErr := range tests {
		delay, err := newHTTPDownloadRetryPolicy().Retry(0, nil, transportErr)
		require.NoError(t, err)
		require.Equal(t, httpDownloadRetryDelay, delay)
	}
}

func TestHTTPDownloadRetryPolicySkipsPermanentFailures(t *testing.T) {
	t.Parallel()

	tests := []error{
		errors.New("tls: failed to verify certificate"),
		context.Canceled,
	}

	for _, transportErr := range tests {
		delay, err := newHTTPDownloadRetryPolicy().Retry(0, nil, transportErr)
		require.NoError(t, err)
		require.Negative(t, delay)
	}
}

func TestHTTPDownloadRetryPolicyUsesORASStatusPredicate(t *testing.T) {
	t.Parallel()

	policy := newHTTPDownloadRetryPolicy()

	delay, err := policy.Retry(0, &http.Response{StatusCode: http.StatusServiceUnavailable}, nil)
	require.NoError(t, err)
	require.Equal(t, httpDownloadRetryDelay, delay)

	delay, err = policy.Retry(0, &http.Response{StatusCode: http.StatusNotFound}, nil)
	require.NoError(t, err)
	require.Negative(t, delay)
}

func TestHTTPDownloadRetryPolicyStopsAfterMaxAttempts(t *testing.T) {
	t.Parallel()

	delay, err := newHTTPDownloadRetryPolicy().Retry(httpDownloadMaxAttempts-1, nil, &net.DNSError{
		Err:        "no such host",
		Name:       "artifacts.example.test",
		IsNotFound: true,
	})
	require.NoError(t, err)
	require.Negative(t, delay)
	require.Equal(t, 16*time.Second, maxHTTPDownloadRetryDelay())
}

func TestRetryableHTTPBodyReadErrorHandlesClientTimeout(t *testing.T) {
	t.Parallel()

	timeoutErr := &url.Error{Op: http.MethodGet, URL: "https://artifacts.example.test/artifact", Err: context.DeadlineExceeded}
	require.True(t, retryableHTTPBodyReadError(context.Background(), timeoutErr))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, retryableHTTPBodyReadError(ctx, timeoutErr))
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

func TestSourceOpenHTTPRequestErrorRedactsQuery(t *testing.T) {
	t.Parallel()

	_, err := openHTTPWithClient(context.Background(), http.DefaultClient, "https://artifacts.example.test/%zz?sig=secret")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "secret")
	require.Contains(t, err.Error(), "redacted")
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

func TestParseRejectsInvalidOCISource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		source string
		error  string
	}{
		{name: "missing repository", source: "oci://registry.example.test#manifest.json", error: "registry and repository"},
		{name: "user info", source: "oci://user@registry.example.test/artifacts:v1#manifest.json", error: "user info"},
		{name: "query", source: "oci://registry.example.test/artifacts:v1?token=secret#manifest.json", error: "query parameters"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Parse(tt.source)
			require.ErrorContains(t, err, tt.error)
		})
	}
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
