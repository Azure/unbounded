// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agentbinary

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestSecureInstallAndSwitch(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeTestPaths(t)
	if err := os.WriteFile(paths.BluePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write blue: %v", err)
	}

	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatalf("symlink current: %v", err)
	}

	if err := os.Symlink(paths.BluePath, paths.LastGoodPath); err != nil {
		t.Fatalf("symlink last-good: %v", err)
	}

	payload := secureUpgradeArchive(t, "custom-agent", []byte("#!/bin/sh\nexit 0\n"))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	digest := sha256.Sum256(payload)

	result, err := SecureInstallAndSwitch(t.Context(), slog.Default(), paths, SecureInstallOptions{
		DownloadURL:       server.URL + "/agent.tar.gz?sig=secret",
		ExpectedSHA256:    fmt.Sprintf("%x", digest),
		ExpectedMember:    "custom-agent",
		Mode:              0o755,
		MaxArchiveBytes:   1 << 20,
		MaxExtractedBytes: 1 << 20,
		HTTPClient:        server.Client(),
	})
	if err != nil {
		t.Fatalf("SecureInstallAndSwitch: %v", err)
	}

	if result.PreviousPath != paths.BluePath || result.CurrentPath != paths.GreenPath {
		t.Fatalf("result = %#v", result)
	}

	assertSecureUpgradeLink(t, paths.CurrentPath, paths.GreenPath)
	assertSecureUpgradeLink(t, paths.LastGoodPath, paths.BluePath)
}

func TestSecureInstallAndSwitchRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeTestPaths(t)
	if err := os.WriteFile(paths.BluePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write blue: %v", err)
	}

	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatalf("symlink current: %v", err)
	}

	tests := map[string]SecureInstallOptions{
		"HTTP URL": {
			DownloadURL:    "http://example.com/agent.tar.gz",
			ExpectedSHA256: strings.Repeat("a", 64),
			ExpectedMember: "custom-agent",
		},
		"invalid digest": {
			DownloadURL:    "https://example.com/agent.tar.gz",
			ExpectedSHA256: "bad",
			ExpectedMember: "custom-agent",
		},
		"nested member": {
			DownloadURL:    "https://example.com/agent.tar.gz",
			ExpectedSHA256: strings.Repeat("a", 64),
			ExpectedMember: "bin/custom-agent",
		},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := SecureInstallAndSwitch(t.Context(), slog.Default(), paths, opts); err == nil {
				t.Fatal("SecureInstallAndSwitch error = nil")
			}
		})
	}
}

func TestSecureInstallAndSwitchRejectsUnexpectedMember(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeTestPaths(t)
	if err := os.WriteFile(paths.BluePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write blue: %v", err)
	}

	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatalf("symlink current: %v", err)
	}

	payload := secureUpgradeArchive(t, "other-agent", []byte("binary"))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	digest := sha256.Sum256(payload)

	_, err := SecureInstallAndSwitch(t.Context(), slog.Default(), paths, SecureInstallOptions{
		DownloadURL:    server.URL + "/agent.tar.gz",
		ExpectedSHA256: fmt.Sprintf("%x", digest),
		ExpectedMember: "custom-agent",
		HTTPClient:     server.Client(),
	})
	if err == nil || !strings.Contains(err.Error(), "unexpected member") {
		t.Fatalf("error = %v", err)
	}
}

func TestRedactedURL(t *testing.T) {
	t.Parallel()

	parsed, err := url.Parse("https://example.com/agent.tar.gz?sig=secret")
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	if got := RedactedURL(parsed); got != "https://example.com/agent.tar.gz" {
		t.Fatalf("RedactedURL = %q", got)
	}
}

func secureUpgradeTestPaths(t *testing.T) goalstates.AgentUpgradePaths {
	t.Helper()
	dir := t.TempDir()
	paths := goalstates.AgentUpgradePaths{
		BinaryPath:        filepath.Join(dir, "agent"),
		BluePath:          filepath.Join(dir, "agent-blue"),
		GreenPath:         filepath.Join(dir, "agent-green"),
		CurrentPath:       filepath.Join(dir, "agent-current"),
		LastGoodPath:      filepath.Join(dir, "agent-last-good"),
		SignalPath:        filepath.Join(dir, "agent-signal"),
		CurrentTargetPath: filepath.Join(dir, "agent-blue"),
	}

	return paths
}

func secureUpgradeArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()

	var archive bytes.Buffer

	gz := gzip.NewWriter(&archive)

	tarWriter := tar.NewWriter(gz)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}

	if _, err := io.Copy(tarWriter, bytes.NewReader(body)); err != nil {
		t.Fatalf("write tar body: %v", err)
	}

	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	return archive.Bytes()
}

func assertSecureUpgradeLink(t *testing.T, path, want string) {
	t.Helper()

	got, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}

	if got != want {
		t.Fatalf("resolved %s = %s, want %s", path, got, want)
	}
}
