// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

func TestParseAgentUpgradeRequest(t *testing.T) {
	t.Parallel()

	request, err := parseAgentUpgradeRequest(map[string]string{
		agentUpgradeDownloadURLParameter: " https://example.com/agent.tar.gz ",
		agentUpgradeSHA256Parameter:      " aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ",
	})
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/agent.tar.gz", request.downloadURL)
	assert.Equal(t, testAgentUpgradeSHA256, request.sha256)

	_, err = parseAgentUpgradeRequest(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), agentUpgradeDownloadURLParameter)

	request, err = parseAgentUpgradeRequest(map[string]string{
		agentUpgradeDownloadURLParameter: "http://example.com/agent.tar.gz",
	})
	require.NoError(t, err)
	assert.Empty(t, request.sha256)
}

// TestAgentUpgradeInstallOptionsAcceptReleaseArchives covers the archives
// GoReleaser has published: LICENSE and README.md beside the binary before
// v0.7.0, LICENSE and NOTICE after it, and the binary alone.
func TestAgentUpgradeInstallOptionsAcceptReleaseArchives(t *testing.T) {
	t.Parallel()

	binary := []byte("#!/bin/sh\nexit 0\n")

	tests := map[string][]string{
		"license and readme": {"LICENSE", "README.md", goalstates.AgentUpgradeBinaryName},
		"license and notice": {"LICENSE", "NOTICE", goalstates.AgentUpgradeBinaryName},
		"binary only":        {goalstates.AgentUpgradeBinaryName},
	}
	for name, members := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			archive := agentUpgradeTestArchive(t, members, binary)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(archive)
			}))
			t.Cleanup(server.Close)

			dir := t.TempDir()
			layout := agentbinary.Layout{
				BluePath:     filepath.Join(dir, "unbounded-agent-blue"),
				GreenPath:    filepath.Join(dir, "unbounded-agent-green"),
				CurrentPath:  filepath.Join(dir, "unbounded-agent-current"),
				LastGoodPath: filepath.Join(dir, "unbounded-agent-last-good"),
			}
			require.NoError(t, os.WriteFile(layout.BluePath, binary, 0o755))
			require.NoError(t, os.Symlink(layout.BluePath, layout.CurrentPath))
			require.NoError(t, os.Symlink(layout.BluePath, layout.LastGoodPath))

			options := agentUpgradeInstallOptions(agentUpgradeRequest{downloadURL: server.URL + "/unbounded-agent-linux-amd64.tar.gz"})
			result, err := agentbinary.InstallAndSwitchFromTarGz(t.Context(), slog.Default(), layout, options)
			require.NoError(t, err)
			assert.Equal(t, layout.GreenPath, result.CurrentPath)

			installed, err := os.ReadFile(layout.GreenPath)
			require.NoError(t, err)
			assert.Equal(t, binary, installed)
		})
	}
}

func agentUpgradeTestArchive(t *testing.T, members []string, binary []byte) []byte {
	t.Helper()

	var archive bytes.Buffer

	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)

	for _, member := range members {
		body := []byte(member + "\n")
		if member == goalstates.AgentUpgradeBinaryName {
			body = binary
		}

		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name:     member,
			Mode:     0o755,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}))

		_, err := tw.Write(body)
		require.NoError(t, err)
	}

	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	return archive.Bytes()
}

func TestAgentUpgradeSignalOperatorRecordFailure(t *testing.T) {
	t.Parallel()

	signalPath := filepath.Join(t.TempDir(), "agent-upgrade-signal")
	signals := newAgentUpgradeSignalOperatorForPath(signalPath)

	require.NoError(t, signals.RecordPending("op-1", 7))
	require.NoError(t, signals.RecordFailure("rolled back"))

	data, err := os.ReadFile(signalPath)
	require.NoError(t, err)
	assert.JSONEq(t, `{"operationName":"op-1","observedMachineGeneration":7,"failureMessage":"rolled back"}`, string(data))
}

func TestAgentUpgradeSignalOperatorReadRejectsNonJSON(t *testing.T) {
	t.Parallel()

	signalPath := filepath.Join(t.TempDir(), "agent-upgrade-signal")
	signals := newAgentUpgradeSignalOperatorForPath(signalPath)
	require.NoError(t, os.WriteFile(signalPath, []byte("op-1\n"), 0o600))

	_, err := signals.Read()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode AgentUpgrade signal")
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
