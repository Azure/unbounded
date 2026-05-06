// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/agentbinary"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestAgentUpgradeDownloadURL(t *testing.T) {
	t.Parallel()

	downloadURL, err := agentUpgradeDownloadURL(map[string]string{
		agentUpgradeDownloadURLParameter: " https://example.com/agent.tar.gz ",
	})
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/agent.tar.gz", downloadURL)

	_, err = agentUpgradeDownloadURL(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), agentUpgradeDownloadURLParameter)
}

func TestUpgradeDaemonBinary(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "unbounded-agent")
	currentPath := filepath.Join(dir, "unbounded-agent-current")
	lastGoodPath := filepath.Join(dir, "unbounded-agent-last-good")
	bluePath := filepath.Join(dir, "unbounded-agent-blue")
	greenPath := filepath.Join(dir, "unbounded-agent-green")

	require.NoError(t, os.WriteFile(legacyPath, []byte("legacy"), 0o755))
	require.NoError(t, os.Symlink(legacyPath, currentPath))

	t.Setenv(goalstates.EnvDaemonBinary, legacyPath)
	t.Setenv(goalstates.EnvDaemonBinaryCurrent, currentPath)
	t.Setenv(goalstates.EnvDaemonBinaryLastGood, lastGoodPath)
	t.Setenv(goalstates.EnvDaemonBinaryBlue, bluePath)
	t.Setenv(goalstates.EnvDaemonBinaryGreen, greenPath)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		require.NoError(t, writeAgentArchive(w, []byte("new-agent-binary")))
	}))
	t.Cleanup(server.Close)

	require.NoError(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))

	target, err := filepath.EvalSymlinks(currentPath)
	require.NoError(t, err)
	assert.Equal(t, bluePath, target)

	lastGoodTarget, err := filepath.EvalSymlinks(lastGoodPath)
	require.NoError(t, err)
	assert.Equal(t, legacyPath, lastGoodTarget)

	newData, err := os.ReadFile(bluePath)
	require.NoError(t, err)
	assert.Equal(t, []byte("new-agent-binary"), newData)
}

func TestUpgradeDaemonBinary_AlternatesFromBlueToGreen(t *testing.T) {
	dir := t.TempDir()
	currentPath := filepath.Join(dir, "unbounded-agent-current")
	lastGoodPath := filepath.Join(dir, "unbounded-agent-last-good")
	bluePath := filepath.Join(dir, "unbounded-agent-blue")
	greenPath := filepath.Join(dir, "unbounded-agent-green")

	require.NoError(t, os.WriteFile(bluePath, []byte("blue"), 0o755))
	require.NoError(t, os.Symlink(bluePath, currentPath))

	t.Setenv(goalstates.EnvDaemonBinaryCurrent, currentPath)
	t.Setenv(goalstates.EnvDaemonBinaryLastGood, lastGoodPath)
	t.Setenv(goalstates.EnvDaemonBinaryBlue, bluePath)
	t.Setenv(goalstates.EnvDaemonBinaryGreen, greenPath)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.NoError(t, writeAgentArchive(w, []byte("green")))
	}))
	t.Cleanup(server.Close)

	require.NoError(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))

	target, err := filepath.EvalSymlinks(currentPath)
	require.NoError(t, err)
	assert.Equal(t, greenPath, target)

	lastGoodTarget, err := filepath.EvalSymlinks(lastGoodPath)
	require.NoError(t, err)
	assert.Equal(t, bluePath, lastGoodTarget)
}

func TestUpgradeDaemonBinary_SequentialSuccesses(t *testing.T) {
	paths := setupDaemonBinaryTest(t)
	server := newAgentArchiveSequenceServer(t, []archiveResponse{
		{binary: []byte("agent-a")},
		{binary: []byte("agent-b")},
	})
	t.Cleanup(server.Close)

	require.NoError(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))
	assertSymlinkTarget(t, paths.current, paths.blue)
	assertSymlinkTarget(t, paths.lastGood, paths.legacy)

	require.NoError(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))
	assertSymlinkTarget(t, paths.current, paths.green)
	assertSymlinkTarget(t, paths.lastGood, paths.blue)
	assertFileContent(t, paths.blue, "agent-a")
	assertFileContent(t, paths.green, "agent-b")
}

func TestUpgradeDaemonBinary_SequentialSuccessThenFailure(t *testing.T) {
	paths := setupDaemonBinaryTest(t)
	server := newAgentArchiveSequenceServer(t, []archiveResponse{
		{binary: []byte("agent-a")},
		{status: http.StatusInternalServerError},
	})
	t.Cleanup(server.Close)

	require.NoError(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))
	require.Error(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))

	assertSymlinkTarget(t, paths.current, paths.blue)
	assertSymlinkTarget(t, paths.lastGood, paths.legacy)
	assertFileContent(t, paths.blue, "agent-a")
	assert.NoFileExists(t, paths.green)
}

func TestUpgradeDaemonBinary_SequentialFailureThenFailure(t *testing.T) {
	paths := setupDaemonBinaryTest(t)
	server := newAgentArchiveSequenceServer(t, []archiveResponse{
		{status: http.StatusInternalServerError},
		{status: http.StatusInternalServerError},
	})
	t.Cleanup(server.Close)

	require.Error(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))
	require.Error(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))

	assertSymlinkTarget(t, paths.current, paths.legacy)
	assert.NoFileExists(t, paths.lastGood)
	assert.NoFileExists(t, paths.blue)
	assert.NoFileExists(t, paths.green)
}

func TestUpgradeDaemonBinary_SequentialFailureThenSuccess(t *testing.T) {
	paths := setupDaemonBinaryTest(t)
	server := newAgentArchiveSequenceServer(t, []archiveResponse{
		{status: http.StatusInternalServerError},
		{binary: []byte("agent-b")},
	})
	t.Cleanup(server.Close)

	require.Error(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))
	require.NoError(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))

	assertSymlinkTarget(t, paths.current, paths.blue)
	assertSymlinkTarget(t, paths.lastGood, paths.legacy)
	assertFileContent(t, paths.blue, "agent-b")
	assert.NoFileExists(t, paths.green)
}

func TestDownloadAgentBinaryFromTarGz_RejectsUnsupportedScheme(t *testing.T) {
	t.Parallel()

	err := agentbinary.InstallFromTarGz(context.Background(), "file:///tmp/unbounded-agent.tar.gz", filepath.Join(t.TempDir(), "agent"), goalstates.AgentUpgradeBinaryName, 0o755)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported agent download URL scheme")
}

func TestEnsureDaemonBinaryLinks_InitializesFromBlue(t *testing.T) {
	paths := setupDaemonBinaryTestWithoutLinks(t)
	require.NoError(t, os.WriteFile(paths.blue, []byte("blue"), 0o755))

	require.NoError(t, ensureDaemonBinaryLinks(slog.Default()))

	assertSymlinkTarget(t, paths.current, paths.blue)
	assertSymlinkTarget(t, paths.lastGood, paths.blue)
	assertSymlinkTarget(t, paths.legacy, paths.blue)
}

func TestEnsureDaemonBinaryLinks_PreservesExistingLinks(t *testing.T) {
	paths := setupDaemonBinaryTestWithoutLinks(t)
	require.NoError(t, os.WriteFile(paths.blue, []byte("blue"), 0o755))
	require.NoError(t, os.WriteFile(paths.green, []byte("green"), 0o755))
	require.NoError(t, os.Symlink(paths.green, paths.current))
	require.NoError(t, os.Symlink(paths.blue, paths.lastGood))

	require.NoError(t, ensureDaemonBinaryLinks(slog.Default()))

	assertSymlinkTarget(t, paths.current, paths.green)
	assertSymlinkTarget(t, paths.lastGood, paths.blue)
	assertSymlinkTarget(t, paths.legacy, paths.green)
}

type daemonBinaryTestPaths struct {
	legacy   string
	current  string
	lastGood string
	blue     string
	green    string
}

func setupDaemonBinaryTest(t *testing.T) daemonBinaryTestPaths {
	t.Helper()

	paths := setupDaemonBinaryTestWithoutLinks(t)
	require.NoError(t, os.WriteFile(paths.legacy, []byte("legacy"), 0o755))
	require.NoError(t, os.Symlink(paths.legacy, paths.current))

	return paths
}

func setupDaemonBinaryTestWithoutLinks(t *testing.T) daemonBinaryTestPaths {
	t.Helper()

	dir := t.TempDir()
	paths := daemonBinaryTestPaths{
		legacy:   filepath.Join(dir, "unbounded-agent"),
		current:  filepath.Join(dir, "unbounded-agent-current"),
		lastGood: filepath.Join(dir, "unbounded-agent-last-good"),
		blue:     filepath.Join(dir, "unbounded-agent-blue"),
		green:    filepath.Join(dir, "unbounded-agent-green"),
	}

	t.Setenv(goalstates.EnvDaemonBinary, paths.legacy)
	t.Setenv(goalstates.EnvDaemonBinaryCurrent, paths.current)
	t.Setenv(goalstates.EnvDaemonBinaryLastGood, paths.lastGood)
	t.Setenv(goalstates.EnvDaemonBinaryBlue, paths.blue)
	t.Setenv(goalstates.EnvDaemonBinaryGreen, paths.green)

	return paths
}

type archiveResponse struct {
	binary []byte
	status int
}

func newAgentArchiveSequenceServer(t *testing.T, responses []archiveResponse) *httptest.Server {
	t.Helper()

	next := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		require.Less(t, next, len(responses))
		response := responses[next]
		next++
		if response.status != 0 {
			http.Error(w, "failed", response.status)
			return
		}

		w.Header().Set("Content-Type", "application/gzip")
		require.NoError(t, writeAgentArchive(w, response.binary))
	}))
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

func writeAgentArchive(w io.Writer, binary []byte) error {
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
