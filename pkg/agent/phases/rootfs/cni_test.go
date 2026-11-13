// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"bytes"
	"fmt"
	"log/slog"
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

func TestDownloadCNIPluginsRetriesTruncatedArchive(t *testing.T) {
	t.Parallel()

	content := strings.Repeat("cni-plugin-", 512)
	archive := tarGzArchive(t, map[string]string{
		"vlan":       content,
		"bridge":     content,
		"host-local": content,
		"loopback":   content,
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

	source, err := artifactsource.Parse(server.URL + "/cni-plugins.tgz")
	require.NoError(t, err)

	destDir := t.TempDir()
	require.NoError(t, downloadCNIPlugins(t.Context(), source, destDir))
	require.EqualValues(t, 2, attempts.Load())

	for _, name := range []string{"vlan", "bridge", "host-local", "loopback"} {
		got, err := os.ReadFile(filepath.Join(destDir, name))
		require.NoError(t, err)
		require.Equal(t, content, string(got))
	}
}

func TestCNIDownloadURL(t *testing.T) {
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
			version: "1.5.1",
			arch:    "amd64",
			want:    "https://github.com/containernetworking/plugins/releases/download/v1.5.1/cni-plugins-linux-amd64-v1.5.1.tgz",
		},
		{
			name:     "base url override",
			override: &goalstates.DownloadSource{BaseURL: "https://mirror.example.com/cni/"},
			version:  "1.5.1",
			arch:     "amd64",
			want:     "https://mirror.example.com/cni/v1.5.1/cni-plugins-linux-amd64-v1.5.1.tgz",
		},
		{
			name:     "url override",
			override: &goalstates.DownloadSource{URL: "https://mirror.example.com/cni-%s-%s-%s.tgz"},
			version:  "1.5.1",
			arch:     "amd64",
			want:     "https://mirror.example.com/cni-1.5.1-amd64-1.5.1.tgz",
		},
	}

	for i := range tests {
		testCase := tests[i]
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			got, err := cniDownloadURL(testCase.override, testCase.version, testCase.arch)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got.String() != testCase.want {
				t.Fatalf("got URL %q, want %q", got.String(), testCase.want)
			}
		})
	}
}

// TestCNIPluginsVersionMatchStaysQuietForOldPlugins pins a claim the code makes
// about its own logging.
//
// Some CNI plugin versions do not support --version, so the probe failing is
// expected rather than a fault. Reporting it at Info means every provision of
// such a cluster logs a complaint about a plugin that is working correctly.
// Logging the operator's own message at Debug is not enough on its own: the
// default runner streams the command's stderr at Info, so the plugin's
// complaint arrives through that route before the message is ever reached.
func TestCNIPluginsVersionMatchStaysQuietForOldPlugins(t *testing.T) {
	dir := t.TempDir()

	// A plugin that rejects --version the way an older one does.
	script := "#!/bin/sh\necho 'unknown flag: --version' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "loopback"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake loopback plugin: %v", err)
	}

	var logged bytes.Buffer

	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if cniPluginsVersionMatch(t.Context(), log, dir, "v1.5.1") {
		t.Fatal("a plugin that cannot report its version must not be treated as matching")
	}

	if logged.Len() != 0 {
		t.Fatalf("logged at Info or above for an expected failure:\n%s", logged.String())
	}
}
