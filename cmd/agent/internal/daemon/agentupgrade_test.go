// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

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

func TestAgentUpgradeSignalOperator_RecordFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	operationPath := filepath.Join(dir, "agent-upgrade-operation")
	failurePath := filepath.Join(dir, "agent-upgrade-failure")
	signals := newAgentUpgradeSignalOperatorForPaths(goalstates.AgentUpgradeSignalPaths{
		OperationPath: operationPath,
		FailurePath:   failurePath,
	})

	require.NoError(t, signals.RecordPending("op-1", 7))
	require.NoError(t, signals.RecordFailure("rolled back"))

	assert.NoFileExists(t, operationPath)
	data, err := os.ReadFile(failurePath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"operationName":"op-1","message":"rolled back"}`, string(data))
}

func TestAgentUpgradeSignalOperator_ReadRejectsNonJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	operationPath := filepath.Join(dir, "agent-upgrade-operation")
	signals := newAgentUpgradeSignalOperatorForPaths(goalstates.AgentUpgradeSignalPaths{
		OperationPath: operationPath,
		FailurePath:   filepath.Join(dir, "agent-upgrade-failure"),
	})
	require.NoError(t, os.WriteFile(operationPath, []byte("op-1\n"), 0o600))

	_, err := signals.ReadPending()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode AgentUpgrade signal")
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
		require.NoError(t, writeAgentArchive(w, agentArchiveScript("new-agent-binary", 0)))
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
	assert.Equal(t, agentArchiveScript("new-agent-binary", 0), newData)
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
		require.NoError(t, writeAgentArchive(w, agentArchiveScript("green", 0)))
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
		{binary: agentArchiveScript("agent-a", 0)},
		{binary: agentArchiveScript("agent-b", 0)},
	})
	t.Cleanup(server.Close)

	require.NoError(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))
	assertSymlinkTarget(t, paths.current, paths.blue)
	assertSymlinkTarget(t, paths.lastGood, paths.legacy)

	require.NoError(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))
	assertSymlinkTarget(t, paths.current, paths.green)
	assertSymlinkTarget(t, paths.lastGood, paths.blue)
	assertFileContent(t, paths.blue, string(agentArchiveScript("agent-a", 0)))
	assertFileContent(t, paths.green, string(agentArchiveScript("agent-b", 0)))
}

func TestUpgradeDaemonBinary_SequentialSuccessThenFailure(t *testing.T) {
	paths := setupDaemonBinaryTest(t)
	server := newAgentArchiveSequenceServer(t, []archiveResponse{
		{binary: agentArchiveScript("agent-a", 0)},
		{status: http.StatusInternalServerError},
	})
	t.Cleanup(server.Close)

	require.NoError(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))
	require.Error(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))

	assertSymlinkTarget(t, paths.current, paths.blue)
	assertSymlinkTarget(t, paths.lastGood, paths.legacy)
	assertFileContent(t, paths.blue, string(agentArchiveScript("agent-a", 0)))
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
		{binary: agentArchiveScript("agent-b", 0)},
	})
	t.Cleanup(server.Close)

	require.Error(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))
	require.NoError(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))

	assertSymlinkTarget(t, paths.current, paths.blue)
	assertSymlinkTarget(t, paths.lastGood, paths.legacy)
	assertFileContent(t, paths.blue, string(agentArchiveScript("agent-b", 0)))
	assert.NoFileExists(t, paths.green)
}

func TestUpgradeDaemonBinary_RejectsBrokenBinary(t *testing.T) {
	paths := setupDaemonBinaryTest(t)
	server := newAgentArchiveSequenceServer(t, []archiveResponse{
		{binary: agentArchiveScript("agent-a", 42)},
	})
	t.Cleanup(server.Close)

	require.Error(t, upgradeDaemonBinary(context.Background(), slog.Default(), server.URL))

	assertSymlinkTarget(t, paths.current, paths.legacy)
	assert.NoFileExists(t, paths.lastGood)
	assertFileContent(t, paths.blue, string(agentArchiveScript("agent-a", 42)))
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

func TestAgentUpgradePathsInitialDaemonBinaryTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	paths := goalstates.AgentUpgradePaths{
		BinaryPath: filepath.Join(dir, "unbounded-agent"),
		BluePath:   filepath.Join(dir, "unbounded-agent-blue"),
		GreenPath:  filepath.Join(dir, "unbounded-agent-green"),
	}
	require.NoError(t, os.WriteFile(paths.BinaryPath, []byte("legacy"), 0o755))
	require.NoError(t, os.WriteFile(paths.BluePath, []byte("blue"), 0o755))
	require.NoError(t, os.WriteFile(paths.GreenPath, []byte("green"), 0o755))

	target, err := paths.InitialDaemonBinaryTarget()
	require.NoError(t, err)
	assert.Equal(t, paths.BluePath, target)

	require.NoError(t, os.Chmod(paths.BluePath, 0o644))
	target, err = paths.InitialDaemonBinaryTarget()
	require.NoError(t, err)
	assert.Equal(t, paths.GreenPath, target)

	require.NoError(t, os.Chmod(paths.GreenPath, 0o644))
	target, err = paths.InitialDaemonBinaryTarget()
	require.NoError(t, err)
	assert.Equal(t, paths.BinaryPath, target)

	require.NoError(t, os.Chmod(paths.BinaryPath, 0o644))
	_, err = paths.InitialDaemonBinaryTarget()
	require.Error(t, err)
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

func agentArchiveScript(version string, exitCode int) []byte {
	return []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' %s\nexit %d\n", shellQuote(version), exitCode))
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
