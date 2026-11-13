// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestDownloadContainerdRetriesTruncatedArchive(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("containerd-binary-", 512)
	archive := tarGzArchive(t, map[string]string{
		"bin/containerd":        content,
		"bin/ctr":               content,
		"bin/containerd-stress": content,
		"README.md":             "ignored",
	})

	var attempts atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(archive)))

		if attempts.Add(1) == 1 {
			_, _ = w.Write(archive[:len(archive)/2])

			return
		}

		_, _ = w.Write(archive)
	}))
	defer server.Close()

	source, err := artifactsource.Parse(server.URL + "/containerd.tar.gz")
	require.NoError(t, err)

	destDir := t.TempDir()
	require.NoError(t, downloadContainerd(t.Context(), source, destDir))
	require.EqualValues(t, 2, attempts.Load())

	for _, name := range []string{"containerd", "ctr", "containerd-stress"} {
		got, err := os.ReadFile(filepath.Join(destDir, name))
		require.NoError(t, err)
		require.Equal(t, content, string(got))
	}

	require.NoFileExists(t, filepath.Join(destDir, "README.md"))
}

func TestContainerdDownloadURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override *goalstates.DownloadSource
		version  string
		arch     string
		want     string
	}{
		{
			name:    "default",
			version: "2.0.4",
			arch:    "amd64",
			want:    "https://github.com/containerd/containerd/releases/download/v2.0.4/containerd-2.0.4-linux-amd64.tar.gz",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/containerd/"},
			version:  "2.0.4",
			arch:     "amd64",
			want:     "https://mirror.example.com/containerd/v2.0.4/containerd-2.0.4-linux-amd64.tar.gz",
		},
		{
			name:     "url override",
			override: &goalstates.DownloadSource{URL: "https://mirror.example.com/containerd-%s-%s-%s.tar.gz"},
			version:  "2.0.4",
			arch:     "amd64",
			want:     "https://mirror.example.com/containerd-2.0.4-2.0.4-amd64.tar.gz",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := containerdDownloadURL(testCase.override, testCase.version, testCase.arch)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got.String() != testCase.want {
				t.Fatalf("got URL %q, want %q", got.String(), testCase.want)
			}
		})
	}
}

func TestRuncDownloadURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		override *goalstates.DownloadSource
		version  string
		arch     string
		want     string
	}{
		{
			name:    "default",
			version: "1.1.12",
			arch:    "amd64",
			want:    "https://github.com/opencontainers/runc/releases/download/v1.1.12/runc.amd64",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/runc/"},
			version:  "1.1.12",
			arch:     "amd64",
			want:     "https://mirror.example.com/runc/v1.1.12/runc.amd64",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := runcDownloadURL(testCase.override, testCase.version, testCase.arch)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got.String() != testCase.want {
				t.Fatalf("got URL %q, want %q", got.String(), testCase.want)
			}
		})
	}
}
