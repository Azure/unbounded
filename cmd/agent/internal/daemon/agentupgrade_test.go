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
)

func TestAgentUpgradeDownloadURL(t *testing.T) {
	t.Parallel()

	url, err := agentUpgradeDownloadURL(map[string]string{
		agentUpgradeDownloadURLParameter: "https://example.com/agent.tar.gz",
	})
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/agent.tar.gz", url)

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

	t.Setenv("UNBOUNDED_AGENT_DAEMON_BINARY", legacyPath)
	t.Setenv("UNBOUNDED_AGENT_DAEMON_BINARY_CURRENT", currentPath)
	t.Setenv("UNBOUNDED_AGENT_DAEMON_BINARY_LAST_GOOD", lastGoodPath)
	t.Setenv("UNBOUNDED_AGENT_DAEMON_BINARY_BLUE", bluePath)
	t.Setenv("UNBOUNDED_AGENT_DAEMON_BINARY_GREEN", greenPath)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
