// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package agentbinary

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
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
)

func TestInstallAndSwitchFromTarGzWithOptions(t *testing.T) {
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

	result, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), paths, InstallOptions{
		DownloadURL:    server.URL + "/agent.tar.gz?sig=secret",
		ExpectedSHA256: fmt.Sprintf("%x", digest),
		ExpectedMember: "custom-agent",
		Mode:           0o755,
		HTTPClient:     server.Client(),
	})
	if err != nil {
		t.Fatalf("InstallAndSwitchFromTarGz: %v", err)
	}

	if result.PreviousPath != paths.BluePath || result.CurrentPath != paths.GreenPath {
		t.Fatalf("result = %#v", result)
	}

	assertSecureUpgradeLink(t, paths.CurrentPath, paths.GreenPath)
	assertSecureUpgradeLink(t, paths.LastGoodPath, paths.BluePath)
}

func TestInstallAndSwitchFromTarGzWithOptionsRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeTestPaths(t)
	if err := os.WriteFile(paths.BluePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write blue: %v", err)
	}

	if err := os.Symlink(paths.BluePath, paths.CurrentPath); err != nil {
		t.Fatalf("symlink current: %v", err)
	}

	tests := map[string]InstallOptions{
		"unsupported URL": {
			DownloadURL:    "ftp://example.com/agent.tar.gz",
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
		"dot member": {
			DownloadURL:    "https://example.com/agent.tar.gz",
			ExpectedSHA256: strings.Repeat("a", 64),
			ExpectedMember: ".",
		},
		"dot-dot member": {
			DownloadURL:    "https://example.com/agent.tar.gz",
			ExpectedSHA256: strings.Repeat("a", 64),
			ExpectedMember: "..",
		},
		"invalid mode": {
			DownloadURL:    "https://example.com/agent.tar.gz",
			ExpectedSHA256: strings.Repeat("a", 64),
			ExpectedMember: "custom-agent",
			Mode:           os.ModeSetuid | 0o755,
		},
	}
	for name, opts := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), paths, opts); err == nil {
				t.Fatal("InstallAndSwitchFromTarGz error = nil")
			}
		})
	}
}

func TestValidateLayout(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeTestPaths(t)
	if err := validateLayout(paths); err != nil {
		t.Fatalf("ValidateLayout: %v", err)
	}

	paths.BinaryPath = ""
	if err := validateLayout(paths); err != nil {
		t.Fatalf("ValidateLayout without optional BinaryPath: %v", err)
	}

	paths.LastGoodPath = paths.CurrentPath
	if err := validateLayout(paths); err == nil {
		t.Fatal("ValidateLayout duplicate path error = nil")
	}
}

func TestValidateLayoutRejectsAliasedEntries(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeTestPaths(t)

	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realDir, 0o750); err != nil {
		t.Fatalf("create real directory: %v", err)
	}

	aliasDir := filepath.Join(filepath.Dir(realDir), "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Fatalf("create directory alias: %v", err)
	}

	paths.BluePath = filepath.Join(realDir, "agent")
	paths.GreenPath = filepath.Join(aliasDir, "agent")

	if err := validateLayout(paths); err == nil {
		t.Fatal("validateLayout aliased path error = nil")
	}
}

func TestInstallAndSwitchFromTarGzWithOptionsPreservesCurrentOnVerificationFailures(t *testing.T) {
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

			_, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), paths, InstallOptions{
				DownloadURL:    server.URL + "/agent.tar.gz?sig=secret",
				ExpectedSHA256: digest,
				ExpectedMember: "custom-agent",
				HTTPClient:     server.Client(),
			})
			if err == nil {
				t.Fatal("InstallAndSwitchFromTarGz error = nil")
			}

			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error leaked URL query: %v", err)
			}

			assertSecureUpgradeLink(t, paths.CurrentPath, paths.BluePath)
			assertSecureUpgradeLink(t, paths.LastGoodPath, paths.BluePath)
		})
	}
}

func TestInstallAndSwitchFromTarGzProtectsDanglingLastGood(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeReadyPaths(t)
	if err := os.Remove(paths.LastGoodPath); err != nil {
		t.Fatalf("remove last-good link: %v", err)
	}

	if err := os.Symlink(paths.GreenPath, paths.LastGoodPath); err != nil {
		t.Fatalf("symlink dangling last-good: %v", err)
	}

	payload := secureUpgradeArchive(t, "custom-agent", []byte("#!/bin/sh\nexit 42\n"))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	_, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), paths, InstallOptions{
		DownloadURL:    server.URL + "/agent.tar.gz",
		ExpectedMember: "custom-agent",
		HTTPClient:     server.Client(),
	})
	if err == nil {
		t.Fatal("InstallAndSwitchFromTarGz error = nil")
	}

	assertSecureUpgradeLink(t, paths.CurrentPath, paths.BluePath)
	assertSecureUpgradeLink(t, paths.LastGoodPath, paths.BluePath)
}

func TestInstallAndSwitchFromTarGzWithOptionsPreservesDistinctLastGoodOnCandidateFailure(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeReadyPaths(t)
	if err := os.WriteFile(paths.BinaryPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write distinct last-good: %v", err)
	}

	if err := os.Remove(paths.LastGoodPath); err != nil {
		t.Fatalf("remove last-good link: %v", err)
	}

	if err := os.Symlink(paths.BinaryPath, paths.LastGoodPath); err != nil {
		t.Fatalf("symlink distinct last-good: %v", err)
	}

	payload := secureUpgradeArchive(t, "custom-agent", []byte("#!/bin/sh\nexit 42\n"))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	digest := sha256.Sum256(payload)

	_, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), paths, InstallOptions{
		DownloadURL:    server.URL + "/agent.tar.gz",
		ExpectedSHA256: fmt.Sprintf("%x", digest),
		ExpectedMember: "custom-agent",
		HTTPClient:     server.Client(),
	})
	if err == nil {
		t.Fatal("InstallAndSwitchFromTarGz error = nil")
	}

	assertSecureUpgradeLink(t, paths.CurrentPath, paths.BluePath)
	assertSecureUpgradeLink(t, paths.LastGoodPath, paths.BinaryPath)
}

func TestInstallAndSwitchFromTarGzInstallsRootMemberAndIgnoresOthers(t *testing.T) {
	t.Parallel()

	binary := []byte("#!/bin/sh\nexit 0\n")
	// A decoy fails verification, so installing one fails the upgrade.
	decoy := []byte("#!/bin/sh\nexit 42\n")

	tests := map[string][]secureTarMember{
		"release archive": {
			{name: "LICENSE", body: []byte("license")},
			{name: "NOTICE", body: []byte("notice")},
			{name: "custom-agent", body: binary},
		},
		"binary only": {
			{name: "custom-agent", body: binary},
		},
		"dot prefixed": {
			{name: "./", typeflag: tar.TypeDir},
			{name: "./LICENSE", body: []byte("license")},
			{name: "./custom-agent", body: binary},
		},
		"nested members with the same name": {
			{name: "bin/custom-agent", body: decoy},
			{name: "completions/bash/custom-agent", body: decoy},
			{name: "custom-agent", body: binary},
			{name: "docs/custom-agent/", typeflag: tar.TypeDir},
			{name: "docs/custom-agent/README", body: []byte("readme")},
		},
		"other member types": {
			{name: "config/", typeflag: tar.TypeDir},
			{name: "config/agent.yaml", body: []byte("key: value\n")},
			{name: "custom-agent", body: binary},
			{name: "agent-link", typeflag: tar.TypeSymlink, linkname: "custom-agent"},
			{name: "agent-hardlink", typeflag: tar.TypeLink, linkname: "custom-agent"},
		},
		"plugins and config beside the binary": {
			{name: "custom-agent", body: binary},
			{name: "plugins/", typeflag: tar.TypeDir},
			{name: "plugins/example-plugin", body: decoy},
			{name: "config/", typeflag: tar.TypeDir},
			{name: "config/example.yaml", body: []byte("key: value\n")},
		},
		"unsafe names in other members": {
			{name: "../LICENSE", body: []byte("license")},
			{name: "/etc/NOTICE", body: []byte("notice")},
			{name: `docs\README`, body: []byte("readme")},
			{name: "bin/../plugin", body: decoy},
			{name: "custom-agent", body: binary},
		},
		"duplicate root member": {
			{name: "custom-agent", body: binary},
			{name: "./custom-agent", body: decoy},
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

			_, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), paths, InstallOptions{
				DownloadURL:    server.URL + "/agent.tar.gz",
				ExpectedSHA256: fmt.Sprintf("%x", digest),
				ExpectedMember: "custom-agent",
				HTTPClient:     server.Client(),
			})
			if err != nil {
				t.Fatalf("InstallAndSwitchFromTarGz: %v", err)
			}

			assertSecureUpgradeLink(t, paths.CurrentPath, paths.GreenPath)
			assertSecureUpgradeLink(t, paths.LastGoodPath, paths.BluePath)

			installed, err := os.ReadFile(paths.GreenPath)
			if err != nil {
				t.Fatalf("read installed binary: %v", err)
			}

			if !bytes.Equal(installed, binary) {
				t.Fatalf("installed binary = %q, want the root member %q", installed, binary)
			}

			assertNoStagedAgentBinaries(t, filepath.Dir(paths.GreenPath))
		})
	}
}

func TestInstallAndSwitchFromTarGzRejectsInvalidRootMemberArchives(t *testing.T) {
	t.Parallel()

	binary := []byte("#!/bin/sh\nexit 0\n")

	tests := map[string]struct {
		members []secureTarMember
		wantErr string
	}{
		"missing root member": {
			members: []secureTarMember{
				{name: "LICENSE", body: []byte("license")},
				{name: "bin/custom-agent", body: binary},
			},
			wantErr: `does not contain "custom-agent" at its root`,
		},
		"root directory": {
			members: []secureTarMember{
				{name: "custom-agent/", typeflag: tar.TypeDir},
				{name: "custom-agent/unbounded", body: binary},
			},
			wantErr: `does not contain "custom-agent" at its root`,
		},
		"root member through a parent reference": {
			members: []secureTarMember{
				{name: "bin/../custom-agent", body: binary},
			},
			wantErr: `does not contain "custom-agent" at its root`,
		},
		"member outside the archive root": {
			members: []secureTarMember{
				{name: "../custom-agent", body: binary},
			},
			wantErr: `does not contain "custom-agent" at its root`,
		},
		"root symlink": {
			members: []secureTarMember{
				{name: "bin/agent", body: binary},
				{name: "custom-agent", typeflag: tar.TypeSymlink, linkname: "bin/agent"},
			},
			wantErr: "not a regular file",
		},
		"root hardlink": {
			members: []secureTarMember{
				{name: "bin/agent", body: binary},
				{name: "custom-agent", typeflag: tar.TypeLink, linkname: "bin/agent"},
			},
			wantErr: "not a regular file",
		},
		"empty root member": {
			members: []secureTarMember{
				{name: "custom-agent"},
			},
			wantErr: "is empty",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			paths := secureUpgradeReadyPaths(t)
			payload := secureUpgradeArchiveWithMembers(t, tt.members)
			digest := sha256.Sum256(payload)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			}))
			t.Cleanup(server.Close)

			_, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), paths, InstallOptions{
				DownloadURL:    server.URL + "/agent.tar.gz",
				ExpectedSHA256: fmt.Sprintf("%x", digest),
				ExpectedMember: "custom-agent",
				HTTPClient:     server.Client(),
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}

			assertSecureUpgradeLink(t, paths.CurrentPath, paths.BluePath)

			if _, statErr := os.Stat(paths.GreenPath); !os.IsNotExist(statErr) {
				t.Fatalf("inactive slot changed on invalid archive: %v", statErr)
			}

			assertNoStagedAgentBinaries(t, filepath.Dir(paths.GreenPath))
		})
	}
}

func TestInstallAndSwitchFromTarGzRejectsCorruptArchiveWithoutDigest(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeReadyPaths(t)
	payload := secureUpgradeArchive(t, "custom-agent", []byte("#!/bin/sh\nexit 0\n"))
	// The gzip trailer is the CRC-32 of the content, then its size.
	payload[len(payload)-8] ^= 0xff

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)

	_, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), paths, InstallOptions{
		DownloadURL:    server.URL + "/agent.tar.gz",
		ExpectedMember: "custom-agent",
		HTTPClient:     server.Client(),
	})
	if !errors.Is(err, gzip.ErrChecksum) {
		t.Fatalf("error = %v, want %v", err, gzip.ErrChecksum)
	}

	assertSecureUpgradeLink(t, paths.CurrentPath, paths.BluePath)

	if _, statErr := os.Stat(paths.GreenPath); !os.IsNotExist(statErr) {
		t.Fatalf("inactive slot changed on corrupt archive: %v", statErr)
	}

	assertNoStagedAgentBinaries(t, filepath.Dir(paths.GreenPath))
}

func TestInstallAndSwitchFromTarGzWithOptionsAllowsHTTPRedirect(t *testing.T) {
	t.Parallel()

	paths := secureUpgradeReadyPaths(t)
	payload := secureUpgradeArchive(t, "custom-agent", []byte("#!/bin/sh\nexit 0\n"))
	insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(insecure.Close)

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, insecure.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	digest := sha256.Sum256(payload)

	_, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), paths, InstallOptions{
		DownloadURL:    redirector.URL + "/agent.tar.gz",
		ExpectedSHA256: fmt.Sprintf("%x", digest),
		ExpectedMember: "custom-agent",
	})
	if err != nil {
		t.Fatalf("InstallAndSwitchFromTarGz: %v", err)
	}

	assertSecureUpgradeLink(t, paths.CurrentPath, paths.GreenPath)
}

func TestInstallAndSwitchFromTarGzRejectsHTTPSDowngrade(t *testing.T) {
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

	_, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), paths, InstallOptions{
		DownloadURL:    secure.URL + "/agent.tar.gz",
		ExpectedMember: "custom-agent",
		HTTPClient:     secure.Client(),
	})
	if err == nil {
		t.Fatal("InstallAndSwitchFromTarGz error = nil")
	}

	assertSecureUpgradeLink(t, paths.CurrentPath, paths.BluePath)
}

func TestRedactedURL(t *testing.T) {
	t.Parallel()

	if got := redactedURL(nil); got != "" {
		t.Fatalf("redactedURL(nil) = %q", got)
	}

	parsed, err := url.Parse("https://example.com/agent.tar.gz?sig=secret")
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}

	if got := redactedURL(parsed); got != "https://example.com/agent.tar.gz" {
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
	name     string
	body     []byte
	typeflag byte
	linkname string
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
		typeflag := member.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}

		header := &tar.Header{
			Name:     member.name,
			Mode:     0o755,
			Size:     int64(len(member.body)),
			Typeflag: typeflag,
			Linkname: member.linkname,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
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

func assertNoStagedAgentBinaries(t *testing.T, dir string) {
	t.Helper()

	staged, err := filepath.Glob(filepath.Join(dir, ".agent-upgrade-*"))
	if err != nil {
		t.Fatalf("glob staged binaries: %v", err)
	}

	if len(staged) != 0 {
		t.Fatalf("staged binaries left behind: %v", staged)
	}
}

// A candidate the caller's check refuses is never selected: the running agent
// stays current and last-good keeps pointing at it. A candidate the check
// accepts is selected as usual.
func TestInstallAndSwitchFromTarGzRunsTheCandidateCheck(t *testing.T) {
	t.Parallel()

	for name, refuse := range map[string]bool{"refused": true, "accepted": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			paths := secureUpgradeReadyPaths(t)
			payload := secureUpgradeArchive(t, "custom-agent", []byte("#!/bin/sh\nexit 0\n"))
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			}))
			t.Cleanup(server.Close)

			var checked string

			_, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), paths, InstallOptions{
				DownloadURL:    server.URL + "/agent.tar.gz",
				ExpectedMember: "custom-agent",
				HTTPClient:     server.Client(),
				Check: func(_ context.Context, path string) error {
					checked = path

					if refuse {
						return errors.New("cannot operate isolation mode none")
					}

					return nil
				},
			})

			if checked != paths.GreenPath {
				t.Fatalf("check ran on %q, want the staged candidate %q", checked, paths.GreenPath)
			}

			if refuse {
				if err == nil || !strings.Contains(err.Error(), "cannot operate isolation mode none") {
					t.Fatalf("err = %v", err)
				}

				assertSecureUpgradeLink(t, paths.CurrentPath, paths.BluePath)
				assertSecureUpgradeLink(t, paths.LastGoodPath, paths.BluePath)

				return
			}

			if err != nil {
				t.Fatalf("InstallAndSwitchFromTarGz: %v", err)
			}

			assertSecureUpgradeLink(t, paths.CurrentPath, paths.GreenPath)
		})
	}
}
