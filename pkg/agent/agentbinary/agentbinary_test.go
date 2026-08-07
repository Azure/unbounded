// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
	if err := os.WriteFile(path, []byte("#!/bin/sh\n(sleep 5) &\nexit 42\n"), 0o755); err != nil {
		t.Fatalf("write agent: %v", err)
	}

	start := time.Now()
	err := Verify(t.Context(), path)
	require.Error(t, err)
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
	return []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s\nexit %d\n", posixShellQuote(version), exitCode))
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
