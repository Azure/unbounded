// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package agentbinary

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/hostroot"
)

func TestInstallFromTarGzVerifiesInstalledBinary(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		exitCode int
		wantErr  string
	}{
		{name: "valid", exitCode: 0},
		{name: "broken", exitCode: 42, wantErr: "verify agent binary"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				require.NoError(t, writeTestAgentArchive(w, testAgentScript(tt.name, tt.exitCode)))
			}))
			t.Cleanup(server.Close)

			targetPath := filepath.Join(t.TempDir(), "unbounded-agent")

			err := installFromTarGz(context.Background(), targetPath, InstallOptions{
				DownloadURL:    server.URL,
				ExpectedMember: "unbounded-agent",
				Mode:           0o755,
			})
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
		})
	}
}

func TestInstallAndSwitchFromTarGz(t *testing.T) {
	t.Parallel()

	paths := setupDaemonBinaryTestPaths(t)

	release := testAgentScript("release", 0)
	if err := os.WriteFile(paths.BinaryPath, testAgentScript("current", 0), 0o755); err != nil {
		t.Fatalf("write current binary: %v", err)
	}

	if err := os.Symlink(paths.BinaryPath, paths.CurrentPath); err != nil {
		t.Fatalf("symlink current binary: %v", err)
	}

	if err := os.Symlink(paths.BinaryPath, paths.LastGoodPath); err != nil {
		t.Fatalf("symlink last-good binary: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if err := writeTestAgentArchive(w, release); err != nil {
			t.Errorf("write archive: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	_, err := InstallAndSwitchFromTarGz(t.Context(), slog.Default(), Layout{
		BinaryPath:   paths.BinaryPath,
		BluePath:     paths.BluePath,
		GreenPath:    paths.GreenPath,
		CurrentPath:  paths.CurrentPath,
		LastGoodPath: paths.LastGoodPath,
	}, InstallOptions{
		DownloadURL:    server.URL,
		ExpectedMember: goalstates.AgentUpgradeBinaryName,
		Mode:           0o755,
	})
	if err != nil {
		t.Fatalf("InstallAndSwitchFromTarGz: %v", err)
	}

	assertSymlinkTarget(t, paths.CurrentPath, paths.BluePath)
	assertSymlinkTarget(t, paths.LastGoodPath, paths.BinaryPath)
	assertFileContent(t, paths.BluePath, string(release))
}

func TestInstallFromTarGzRejectsUnsupportedScheme(t *testing.T) {
	t.Parallel()

	err := installFromTarGz(context.Background(), filepath.Join(t.TempDir(), "agent"), InstallOptions{
		DownloadURL:    "file:///tmp/unbounded-agent.tar.gz",
		ExpectedMember: "unbounded-agent",
		Mode:           0o755,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported agent download URL scheme")
}

func TestVerifyBoundsInheritedOutputWait(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf 'candidate-secret\\n' >&2\n(sleep 5) &\nexit 42\n"), 0o755); err != nil {
		t.Fatalf("write agent: %v", err)
	}

	start := time.Now()
	err := Verify(t.Context(), path)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "candidate-secret")
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestEnsureDaemonBinaryLinks_InitializesFromBlue(t *testing.T) {
	paths := setupDaemonBinaryTestPaths(t)
	require.NoError(t, os.WriteFile(paths.BluePath, []byte("blue"), 0o755))

	require.NoError(t, EnsureDaemonBinaryLinks(context.Background(), slog.Default(), paths))

	assertSymlinkTarget(t, paths.CurrentPath, paths.BluePath)
	assertSymlinkTarget(t, paths.LastGoodPath, paths.BluePath)
	assertSymlinkTarget(t, paths.BinaryPath, paths.BluePath)
}

func TestEnsureDaemonBinaryLinks_SeedsBlueFromLegacyBinary(t *testing.T) {
	paths := setupDaemonBinaryTestPaths(t)
	require.NoError(t, os.WriteFile(paths.BinaryPath, []byte("legacy"), 0o755))

	require.NoError(t, EnsureDaemonBinaryLinks(context.Background(), slog.Default(), paths))

	assertFileContent(t, paths.BluePath, "legacy")
	assertSymlinkTarget(t, paths.CurrentPath, paths.BluePath)
	assertSymlinkTarget(t, paths.LastGoodPath, paths.BluePath)
	assertSymlinkTarget(t, paths.BinaryPath, paths.BluePath)
}

func TestEnsureDaemonBinaryLinks_PreservesExistingLinks(t *testing.T) {
	paths := setupDaemonBinaryTestPaths(t)
	paths.CurrentTargetPath = paths.GreenPath
	require.NoError(t, os.WriteFile(paths.BluePath, []byte("blue"), 0o755))
	require.NoError(t, os.WriteFile(paths.GreenPath, []byte("green"), 0o755))
	require.NoError(t, os.Symlink(paths.GreenPath, paths.CurrentPath))
	require.NoError(t, os.Symlink(paths.BluePath, paths.LastGoodPath))

	require.NoError(t, EnsureDaemonBinaryLinks(context.Background(), slog.Default(), paths))

	assertSymlinkTarget(t, paths.CurrentPath, paths.GreenPath)
	assertSymlinkTarget(t, paths.LastGoodPath, paths.BluePath)
	assertSymlinkTarget(t, paths.BinaryPath, paths.GreenPath)
}

func writeTestAgentArchive(w io.Writer, binary []byte) error {
	gz := gzip.NewWriter(w)
	defer gz.Close()

	tw := tar.NewWriter(gz)
	defer tw.Close()

	header := &tar.Header{
		Name: "unbounded-agent",
		Mode: 0o755,
		Size: int64(len(binary)),
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}

	_, err := io.Copy(tw, bytes.NewReader(binary))

	return err
}

func testAgentScript(version string, exitCode int) []byte {
	return []byte(fmt.Sprintf("#!/bin/sh\n%sprintf '%%s\\n' %s\nexit %d\n", hostRootAnswer(hostroot.Resolve()), posixShellQuote(version), exitCode))
}

// hostRootAnswer is the start of a fake agent that answers host-root with
// root. Verify asks every candidate, and a current agent answers with the root
// this host resolves.
func hostRootAnswer(root string) string {
	return fmt.Sprintf("if [ \"$1\" = host-root ]; then printf '%%s\\n' %s; exit 0; fi\n", posixShellQuote(root))
}

// TestVerifyHostRoot covers the guard against activating an agent that looks
// for its files somewhere other than where this host keeps them. An agent
// released before the host root answers host-root with an error, because it
// has no such command.
func TestVerifyHostRoot(t *testing.T) {
	t.Parallel()

	const root = "/opt/unbounded"

	tests := []struct {
		name    string
		script  string
		root    string
		wantErr string
	}{
		{name: "same root", script: hostRootAnswer(root) + "exit 1\n", root: root},
		{name: "trailing whitespace is ignored", script: "printf '%s \\n\\n' " + root + "\n", root: root},
		{name: "older agent", script: "echo unknown command >&2\nexit 1\n", root: root, wantErr: "predates the host root"},
		{name: "different root", script: hostRootAnswer("/usr/local"), root: root, wantErr: "uses the host root /usr/local, but this host uses /opt/unbounded"},
		{name: "empty answer", script: "exit 0\n", root: root, wantErr: "uses the host root , but"},
		// Every agent finds the legacy root, so a migrated host accepts any
		// candidate without asking it.
		{name: "legacy root accepts an older agent", script: "exit 1\n", root: hostroot.LegacyPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "unbounded-agent")
			require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+tt.script), 0o755))

			err := verifyHostRoot(t.Context(), path, tt.root)
			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestVerifyAsksTheCandidateForItsHostRoot pins that Verify runs the host-root
// check, not only that the check works. An agent that passes version but has no
// host-root is exactly an older release.
func TestVerifyAsksTheCandidateForItsHostRoot(t *testing.T) {
	t.Parallel()

	if hostroot.Resolve() == hostroot.LegacyPath {
		t.Skip("this host's root is linked to the legacy root, where every agent is accepted")
	}

	path := filepath.Join(t.TempDir(), "unbounded-agent")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n[ \"$1\" = version ]\n"), 0o755))

	require.ErrorContains(t, Verify(t.Context(), path), "predates the host root")
}

func posixShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func setupDaemonBinaryTestPaths(t *testing.T) goalstates.AgentUpgradePaths {
	t.Helper()

	dir := t.TempDir()
	paths := goalstates.AgentUpgradePaths{
		BinaryPath:   filepath.Join(dir, "unbounded-agent"),
		CurrentPath:  filepath.Join(dir, "unbounded-agent-current"),
		LastGoodPath: filepath.Join(dir, "unbounded-agent-last-good"),
		BluePath:     filepath.Join(dir, "unbounded-agent-blue"),
		GreenPath:    filepath.Join(dir, "unbounded-agent-green"),
	}
	paths.CurrentTargetPath = paths.BinaryPath

	return paths
}

func assertSymlinkTarget(t *testing.T, linkPath, expectedTarget string) {
	t.Helper()

	target, err := filepath.EvalSymlinks(linkPath)
	require.NoError(t, err)
	assert.Equal(t, expectedTarget, target)
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, expected, string(data))
}

// TestEnsureDaemonBinaryLinks_RepairsDanglingCurrent covers the one fault that
// verify could report and repair could not fix.
//
// VerifyDaemonInstalled resolves the current link and fails when its target is
// gone. Link initialization used to stat the link itself, which succeeds on a
// dangling symlink, so it saw a healthy link and left it. start on a completed
// installation then verified, repaired nothing, verified again, and returned
// the same stat error on every run forever.
func TestEnsureDaemonBinaryLinks_RepairsDanglingCurrent(t *testing.T) {
	paths := setupDaemonBinaryTestPaths(t)
	require.NoError(t, os.WriteFile(paths.BluePath, []byte("blue"), 0o755))

	// A current link whose target no longer exists, as an interrupted
	// activation or a removed slot leaves behind.
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "removed-slot"), paths.CurrentPath))

	_, err := filepath.EvalSymlinks(paths.CurrentPath)
	require.Error(t, err, "fixture must be a link that cannot resolve")

	require.NoError(t, EnsureDaemonBinaryLinks(context.Background(), slog.Default(), paths))

	assertSymlinkTarget(t, paths.CurrentPath, paths.BluePath)

	resolved, err := filepath.EvalSymlinks(paths.CurrentPath)
	require.NoError(t, err, "the repaired link must resolve, or verify still fails")
	require.Equal(t, paths.BluePath, resolved)
}
