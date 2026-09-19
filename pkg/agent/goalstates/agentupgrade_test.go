// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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

	paths, err := ResolvedAgentUpgradePathsFor("")
	require.NoError(t, err)

	assert.Equal(t, binaryPath, paths.BinaryPath)
	assert.Equal(t, bluePath, paths.BluePath)
	assert.Equal(t, greenPath, paths.GreenPath)
	assert.Equal(t, currentPath, paths.CurrentPath)
	assert.Equal(t, lastGoodPath, paths.LastGoodPath)
	assert.Equal(t, signalPath, paths.SignalPath)
	assert.Equal(t, binaryPath, paths.CurrentTargetPath)
}

func TestResolvedAgentUpgradePaths_UsesDefaultsForBlankOverrides(t *testing.T) {
	t.Setenv(EnvDaemonBinary, "")
	t.Setenv(EnvDaemonBinaryBlue, " ")

	paths, err := ResolvedAgentUpgradePathsFor("")
	require.NoError(t, err)

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

func TestResolvedAgentUpgradePaths_ResolvesCurrentTarget(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "agent")
	currentTargetPath := filepath.Join(dir, "agent-blue")
	currentPath := filepath.Join(dir, "agent-current")

	require.NoError(t, os.WriteFile(currentTargetPath, []byte("agent"), 0o755))
	require.NoError(t, os.Symlink(currentTargetPath, currentPath))
	t.Setenv(EnvDaemonBinary, binaryPath)
	t.Setenv(EnvDaemonBinaryCurrent, currentPath)

	paths, err := ResolvedAgentUpgradePathsFor("")

	require.NoError(t, err)
	assert.Equal(t, currentTargetPath, paths.CurrentTargetPath)
}

func TestResolvedAgentUpgradePaths_CurrentTargetFallsBackToBinaryPath(t *testing.T) {
	t.Setenv(EnvDaemonBinary, "/agent")
	t.Setenv(EnvDaemonBinaryCurrent, filepath.Join(t.TempDir(), "missing-current"))

	paths, err := ResolvedAgentUpgradePathsFor("")

	require.NoError(t, err)
	assert.Equal(t, "/agent", paths.CurrentTargetPath)
}

// TestResolvedAgentUpgradePathsForPrefix covers the reason the prefix-aware
// entry point exists: a host whose /usr is read-only cannot hold the agent's
// own binaries under /usr/local, so they move with the prefix.
//
// The signal path deliberately does not move. It is state about an upgrade
// rather than part of the installed layout, and it lives under the agent config
// directory, which is writable on such hosts.
func TestResolvedAgentUpgradePathsForPrefix(t *testing.T) {
	paths, err := ResolvedAgentUpgradePathsFor("/opt/unbounded")
	require.NoError(t, err)

	assert.Equal(t, "/opt/unbounded/bin/unbounded-agent", paths.BinaryPath)
	assert.Equal(t, "/opt/unbounded/bin/unbounded-agent-blue", paths.BluePath)
	assert.Equal(t, "/opt/unbounded/bin/unbounded-agent-green", paths.GreenPath)
	assert.Equal(t, "/opt/unbounded/bin/unbounded-agent-current", paths.CurrentPath)
	assert.Equal(t, "/opt/unbounded/bin/unbounded-agent-last-good", paths.LastGoodPath)
	assert.Equal(t, DaemonAgentUpgradeSignalPath, paths.SignalPath)
}

// TestResolvedAgentUpgradePathsForDefaultMatchesLegacyConstants pins that a host
// which configures no prefix resolves exactly what this package resolved before
// the prefix existed.
//
// These paths are baked into generated systemd units and into the blue-green
// symlinks on every host already in the field. If the default drifted, an
// upgraded agent would look for its binaries somewhere the installed host does
// not have them, and the daemon would fail to start with nothing having changed
// on disk.
func TestResolvedAgentUpgradePathsForDefaultMatchesLegacyConstants(t *testing.T) {
	paths, err := ResolvedAgentUpgradePathsFor("")
	require.NoError(t, err)

	assert.Equal(t, DaemonBinaryPath, paths.BinaryPath)
	assert.Equal(t, DaemonBinaryBluePath, paths.BluePath)
	assert.Equal(t, DaemonBinaryGreenPath, paths.GreenPath)
	assert.Equal(t, DaemonBinaryCurrentPath, paths.CurrentPath)
	assert.Equal(t, DaemonBinaryLastGoodPath, paths.LastGoodPath)
	assert.Equal(t, DaemonAgentUpgradeSignalPath, paths.SignalPath)
}

// TestDeprecatedResolvedAgentUpgradePathsStillWorks keeps the compatibility
// promise honest. The entry point is deprecated rather than removed because it
// is published from pkg/, and callers outside this repository compose their own
// phases from it.
func TestDeprecatedResolvedAgentUpgradePathsStillWorks(t *testing.T) {
	//nolint:staticcheck // Exercising the deprecated entry point is the point.
	legacy, err := ResolvedAgentUpgradePaths()
	require.NoError(t, err)

	current, err := ResolvedAgentUpgradePathsFor("")
	require.NoError(t, err)

	assert.Equal(t, current, legacy, "the deprecated entry point must stay equivalent to an empty prefix")
}
