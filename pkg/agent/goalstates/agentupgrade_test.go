// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveAgentUpgrade_UsesBlueWhenCurrentIsNotBlue(t *testing.T) {
	t.Parallel()

	paths := AgentUpgradePaths{
		BluePath:     "/agent-blue",
		GreenPath:    "/agent-green",
		CurrentPath:  "/agent-current",
		LastGoodPath: "/agent-last-good",
	}

	upgrade := paths.ResolveAgentUpgrade("https://example.com/agent.tar.gz", "/agent")

	assert.Equal(t, "https://example.com/agent.tar.gz", upgrade.DownloadURL)
	assert.Equal(t, AgentUpgradeBinaryName, upgrade.BinaryName)
	assert.Equal(t, "/agent", upgrade.PreviousBinaryPath)
	assert.Equal(t, "/agent-blue", upgrade.TargetBinaryPath)
	assert.Equal(t, "/agent-current", upgrade.CurrentLinkPath)
	assert.Equal(t, "/agent-last-good", upgrade.LastGoodLinkPath)
}

func TestResolvedAgentUpgradePaths(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "agent")
	bluePath := filepath.Join(dir, "agent-blue")
	greenPath := filepath.Join(dir, "agent-green")
	currentPath := filepath.Join(dir, "agent-current")
	lastGoodPath := filepath.Join(dir, "agent-last-good")

	t.Setenv(EnvDaemonBinary, binaryPath)
	t.Setenv(EnvDaemonBinaryBlue, bluePath)
	t.Setenv(EnvDaemonBinaryGreen, greenPath)
	t.Setenv(EnvDaemonBinaryCurrent, currentPath)
	t.Setenv(EnvDaemonBinaryLastGood, lastGoodPath)

	paths := ResolvedAgentUpgradePaths()

	assert.Equal(t, binaryPath, paths.BinaryPath)
	assert.Equal(t, bluePath, paths.BluePath)
	assert.Equal(t, greenPath, paths.GreenPath)
	assert.Equal(t, currentPath, paths.CurrentPath)
	assert.Equal(t, lastGoodPath, paths.LastGoodPath)
}

func TestResolveAgentUpgrade_UsesGreenWhenCurrentIsBlue(t *testing.T) {
	t.Parallel()

	paths := AgentUpgradePaths{
		BluePath:     "/agent-blue",
		GreenPath:    "/agent-green",
		CurrentPath:  "/agent-current",
		LastGoodPath: "/agent-last-good",
	}

	upgrade := paths.ResolveAgentUpgrade("https://example.com/agent.tar.gz", "/agent-blue")

	assert.Equal(t, "/agent-green", upgrade.TargetBinaryPath)
	assert.Equal(t, "/agent-blue", upgrade.PreviousBinaryPath)
}
