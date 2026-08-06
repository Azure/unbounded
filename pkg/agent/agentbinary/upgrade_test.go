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
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"invalid mode": {
			DownloadURL:    "https://example.com/agent.tar.gz",
			ExpectedSHA256: strings.Repeat("a", 64),
			ExpectedMember: "custom-agent",
			Mode:           os.ModeSetuid | 0o755,
		},
		"negative size": {
			DownloadURL:     "https://example.com/agent.tar.gz",
			ExpectedSHA256:  strings.Repeat("a", 64),
			ExpectedMember:  "custom-agent",
			MaxArchiveBytes: -1,
		},
		"archive size overflow": {
			DownloadURL:     "https://example.com/agent.tar.gz",
			ExpectedSHA256:  strings.Repeat("a", 64),
			ExpectedMember:  "custom-agent",
			MaxArchiveBytes: math.MaxInt64,
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

func TestValidateLayout(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeTestPaths(t)
	if err := ValidateLayout(paths); err != nil {
		t.Fatalf("ValidateLayout: %v", err)
	}

	paths.LastGoodPath = paths.CurrentPath
	if err := ValidateLayout(paths); err == nil {
		t.Fatal("ValidateLayout duplicate path error = nil")
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

func TestSecureInstallAndSwitchPreservesCurrentOnVerificationFailures(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		binary []byte
		digest string
	}{
		"digest mismatch": {
			binary: []byte("#!/bin/sh\nexit 0\n"),
			digest: strings.Repeat("0", 64),
		},
		"candidate version failure": {
			binary: []byte("#!/bin/sh\nexit 42\n"),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := secureUpgradeReadyPaths(t)
			payload := secureUpgradeArchive(t, "custom-agent", tt.binary)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			}))
			t.Cleanup(server.Close)

			digest := tt.digest
			if digest == "" {
				sum := sha256.Sum256(payload)
				digest = fmt.Sprintf("%x", sum)
			}

			_, err := SecureInstallAndSwitch(t.Context(), slog.Default(), paths, SecureInstallOptions{
				DownloadURL:    server.URL + "/agent.tar.gz?sig=secret",
				ExpectedSHA256: digest,
				ExpectedMember: "custom-agent",
				HTTPClient:     server.Client(),
			})
			if err == nil {
				t.Fatal("SecureInstallAndSwitch error = nil")
			}

			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked URL query: %v", err)
			}

			assertSecureUpgradeLink(t, paths.CurrentPath, paths.BluePath)
			assertSecureUpgradeLink(t, paths.LastGoodPath, paths.BluePath)
		})
	}
}

func TestSecureInstallAndSwitchEnforcesSizeLimits(t *testing.T) {
	t.Parallel()

	payload := secureUpgradeArchive(t, "custom-agent", []byte("#!/bin/sh\nexit 0\n"))
	digest := sha256.Sum256(payload)

	tests := map[string]SecureInstallOptions{
		"compressed": {
			ExpectedSHA256:    fmt.Sprintf("%x", digest),
			ExpectedMember:    "custom-agent",
			MaxArchiveBytes:   int64(len(payload) - 1),
			MaxExtractedBytes: 1 << 20,
		},
		"extracted": {
			ExpectedSHA256:    fmt.Sprintf("%x", digest),
			ExpectedMember:    "custom-agent",
			MaxArchiveBytes:   1 << 20,
			MaxExtractedBytes: 4,
		},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := secureUpgradeReadyPaths(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			}))
			t.Cleanup(server.Close)
			opts.DownloadURL = server.URL + "/agent.tar.gz"

			opts.HTTPClient = server.Client()
			if _, err := SecureInstallAndSwitch(t.Context(), slog.Default(), paths, opts); err == nil {
				t.Fatal("SecureInstallAndSwitch error = nil")
			}

			assertSecureUpgradeLink(t, paths.CurrentPath, paths.BluePath)
		})
	}
}

func TestSecureInstallAndSwitchRejectsUnsafeAndDuplicateMembers(t *testing.T) {
	t.Parallel()

	tests := map[string][]secureTarMember{
		"unsafe":        {{name: "../custom-agent", body: []byte("binary")}},
		"path prefixed": {{name: "./custom-agent", body: []byte("binary")}},
		"duplicate": {
			{name: "custom-agent", body: []byte("#!/bin/sh\nexit 0\n")},
			{name: "custom-agent", body: []byte("#!/bin/sh\nexit 0\n")},
		},
	}
	for name, members := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := secureUpgradeReadyPaths(t)
			payload := secureUpgradeArchiveWithMembers(t, members)
			digest := sha256.Sum256(payload)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			}))
			t.Cleanup(server.Close)

			_, err := SecureInstallAndSwitch(t.Context(), slog.Default(), paths, SecureInstallOptions{
				DownloadURL:    server.URL + "/agent.tar.gz",
				ExpectedSHA256: fmt.Sprintf("%x", digest),
				ExpectedMember: "custom-agent",
				HTTPClient:     server.Client(),
			})
			if err == nil {
				t.Fatal("SecureInstallAndSwitch error = nil")
			}

			assertSecureUpgradeLink(t, paths.CurrentPath, paths.BluePath)
		})
	}
}

func TestSecureInstallAndSwitchRejectsHTTPRedirect(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeReadyPaths(t)
	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not reached"))
	}))
	t.Cleanup(insecure.Close)

	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, insecure.URL, http.StatusFound)
	}))
	t.Cleanup(secure.Close)

	_, err := SecureInstallAndSwitch(t.Context(), slog.Default(), paths, SecureInstallOptions{
		DownloadURL:    secure.URL + "/agent.tar.gz",
		ExpectedSHA256: strings.Repeat("0", 64),
		ExpectedMember: "custom-agent",
		HTTPClient:     secure.Client(),
	})
	if err == nil {
		t.Fatal("SecureInstallAndSwitch error = nil")
	}

	assertSecureUpgradeLink(t, paths.CurrentPath, paths.BluePath)
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

func secureUpgradeTestPaths(t *testing.T) Layout {
	t.Helper()
	dir := t.TempDir()
	paths := Layout{
		BinaryPath:   filepath.Join(dir, "agent"),
		BluePath:     filepath.Join(dir, "agent-blue"),
		GreenPath:    filepath.Join(dir, "agent-green"),
		CurrentPath:  filepath.Join(dir, "agent-current"),
		LastGoodPath: filepath.Join(dir, "agent-last-good"),
	}

	return paths
}

func secureUpgradeReadyPaths(t *testing.T) Layout {
	t.Helper()

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

	return paths
}

type secureTarMember struct {
	name string
	body []byte
}

func secureUpgradeArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()

	return secureUpgradeArchiveWithMembers(t, []secureTarMember{{name: name, body: body}})
}

func secureUpgradeArchiveWithMembers(t *testing.T, members []secureTarMember) []byte {
	t.Helper()

	var archive bytes.Buffer

	gz := gzip.NewWriter(&archive)

	tarWriter := tar.NewWriter(gz)
	for _, member := range members {
		if err := tarWriter.WriteHeader(&tar.Header{Name: member.name, Mode: 0o755, Size: int64(len(member.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("write tar header: %v", err)
		}

		if _, err := io.Copy(tarWriter, bytes.NewReader(member.body)); err != nil {
			t.Fatalf("write tar body: %v", err)
		}
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
