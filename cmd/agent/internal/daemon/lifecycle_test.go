// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestRenderDaemonAsset(t *testing.T) {
	t.Parallel()

	renderedBytes, err := renderDaemonAsset("daemon-service", daemonServiceContent)
	require.NoError(t, err)

	rendered := string(renderedBytes)

	require.NotContains(t, rendered, "{{")
	assert.Contains(t, rendered, goalstates.DaemonRecoveryUnit)
	assert.Contains(t, rendered, goalstates.DaemonBinaryCurrentPath)

	renderedRecoveryBytes, err := renderDaemonAsset("daemon-recovery-script", daemonRecoveryScriptContent)
	require.NoError(t, err)

	renderedRecovery := string(renderedRecoveryBytes)
	require.NotContains(t, renderedRecovery, "{{")
	assert.Contains(t, renderedRecovery, goalstates.DaemonBinaryLastGoodPath)
	assert.Contains(t, renderedRecovery, goalstates.DaemonUnit)
	assert.Contains(t, renderedRecovery, goalstates.DaemonAgentUpgradeSignalPath)
	assert.Contains(t, renderedRecovery, "record-agent-upgrade-failure-signal")
}

func TestInstallBinaryStreamsAndReplacesAtomically(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	source, target := filepath.Join(dir, "source"), filepath.Join(dir, "bin", "target")
	require.NoError(t, os.WriteFile(source, []byte("candidate"), 0o600))
	require.NoError(t, fsutil.InstallFile(source, target, 0o755))
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "candidate", string(data))

	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	require.Error(t, fsutil.InstallFile(filepath.Join(dir, "missing"), target, 0o755))
	data, err = os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "candidate", string(data))
}

// TestUsableDaemonBinaryRequiresAResolvableExecutable pins what counts as "the
// host already has a daemon binary". Only the resolved target matters: a broken
// or non-executable link is the state that sends a completed install into
// repair, and repair cannot replace the link, so it must not be mistaken for a
// working installation.
func TestUsableDaemonBinaryRequiresAResolvableExecutable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	executable := filepath.Join(dir, "executable")
	require.NoError(t, os.WriteFile(executable, []byte("binary"), 0o755))

	plain := filepath.Join(dir, "plain")
	require.NoError(t, os.WriteFile(plain, []byte("data"), 0o644))

	// The production layout reaches the active slot through a symlink chain.
	current := filepath.Join(dir, "current")
	require.NoError(t, os.Symlink(executable, current))

	chained := filepath.Join(dir, "chained")
	require.NoError(t, os.Symlink(current, chained))

	dangling := filepath.Join(dir, "dangling")
	require.NoError(t, os.Symlink(filepath.Join(dir, "absent"), dangling))

	toPlain := filepath.Join(dir, "to-plain")
	require.NoError(t, os.Symlink(plain, toPlain))

	directory := filepath.Join(dir, "directory")
	require.NoError(t, os.Mkdir(directory, 0o755))

	for path, want := range map[string]bool{
		executable:                   true,
		current:                      true,
		chained:                      true,
		dangling:                     false,
		toPlain:                      false,
		plain:                        false,
		directory:                    false,
		filepath.Join(dir, "absent"): false,
	} {
		assert.Equal(t, want, usableDaemonBinary(path), "path %s", path)
	}
}
