// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentUpgradePathsNextTargetPathUsesBlueWhenCurrentIsNotBlue(t *testing.T) {
	t.Parallel()

	paths := AgentUpgradePaths{
		BluePath:          "/agent-blue",
		GreenPath:         "/agent-green",
		CurrentTargetPath: "/agent",
	}

	assert.Equal(t, "/agent-blue", paths.NextTargetPath())
}

func TestResolvedAgentUpgradePaths(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "agent")
	bluePath := filepath.Join(dir, "agent-blue")
	greenPath := filepath.Join(dir, "agent-green")
	currentPath := filepath.Join(dir, "agent-current")
	lastGoodPath := filepath.Join(dir, "agent-last-good")
	signalPath := filepath.Join(dir, "agent-upgrade-signal")

	t.Setenv(EnvDaemonBinary, binaryPath)
	t.Setenv(EnvDaemonBinaryBlue, bluePath)
	t.Setenv(EnvDaemonBinaryGreen, greenPath)
	t.Setenv(EnvDaemonBinaryCurrent, currentPath)
	t.Setenv(EnvDaemonBinaryLastGood, lastGoodPath)
	t.Setenv(EnvDaemonAgentUpgradeSignalPath, signalPath)

	paths := ResolvedAgentUpgradePaths()

	assert.Equal(t, binaryPath, paths.BinaryPath)
	assert.Equal(t, bluePath, paths.BluePath)
	assert.Equal(t, greenPath, paths.GreenPath)
	assert.Equal(t, currentPath, paths.CurrentPath)
	assert.Equal(t, lastGoodPath, paths.LastGoodPath)
	assert.Equal(t, signalPath, paths.SignalPath)
}

func TestResolvedAgentUpgradePaths_UsesDefaultsForBlankOverrides(t *testing.T) {
	t.Setenv(EnvDaemonBinary, "")
	t.Setenv(EnvDaemonBinaryBlue, " ")

	paths := ResolvedAgentUpgradePaths()

	assert.Equal(t, DaemonBinaryPath, paths.BinaryPath)
	assert.Equal(t, DaemonBinaryBluePath, paths.BluePath)
	assert.Equal(t, DaemonAgentUpgradeSignalPath, paths.SignalPath)
}

func TestAgentUpgradePathsNextTargetPathUsesGreenWhenCurrentIsBlue(t *testing.T) {
	t.Parallel()

	paths := AgentUpgradePaths{
		BluePath:          "/agent-blue",
		GreenPath:         "/agent-green",
		CurrentTargetPath: "/agent-blue",
	}

	assert.Equal(t, "/agent-green", paths.NextTargetPath())
}

func TestAgentUpgradePathsResolveCurrent(t *testing.T) {
	dir := t.TempDir()
	currentTargetPath := filepath.Join(dir, "agent-blue")
	currentPath := filepath.Join(dir, "agent-current")
	require.NoError(t, os.WriteFile(currentTargetPath, []byte("agent"), 0o755))
	require.NoError(t, os.Symlink(currentTargetPath, currentPath))

	paths, err := (AgentUpgradePaths{
		BinaryPath:  filepath.Join(dir, "agent"),
		CurrentPath: currentPath,
	}).ResolveCurrent()

	require.NoError(t, err)
	assert.Equal(t, currentTargetPath, paths.CurrentTargetPath)
}

func TestAgentUpgradePathsResolveCurrentFallsBackToBinaryPath(t *testing.T) {
	t.Parallel()

	paths, err := (AgentUpgradePaths{
		BinaryPath:  "/agent",
		CurrentPath: filepath.Join(t.TempDir(), "missing-current"),
	}).ResolveCurrent()

	require.NoError(t, err)
	assert.Equal(t, "/agent", paths.CurrentTargetPath)
}
