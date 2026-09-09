// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/installstate"
)

func TestResetAgentResourcesIncludesBPFFSMountCleanup(t *testing.T) {
	t.Parallel()

	taskName := ResetAgentResources(slog.New(slog.DiscardHandler)).Name()

	assert.Contains(t, taskName, "parallel(remove-bpffs-mount, remove-bpffs-mount)")
	assert.Less(t, strings.Index(taskName, "parallel(remove-machine, remove-machine)"), strings.Index(taskName, "parallel(remove-bpffs-mount, remove-bpffs-mount)"))
	assert.Less(t, strings.Index(taskName, "parallel(remove-bpffs-mount, remove-bpffs-mount)"), strings.Index(taskName, "cleanup-routes"))
}

// TestResetMarksResettingBeforeRemovingAndClearsStateLast pins the ordering
// that keeps an interrupted teardown from looking like a resumable install.
//
// Marking has to happen before anything is removed, and the record has to
// outlive the removals because every path they resolve comes from it.
func TestResetAgentResourcesOrdersInstallStateCorrectly(t *testing.T) {
	t.Parallel()

	taskName := ResetAgentResources(slog.New(slog.DiscardHandler)).Name()

	markIdx := strings.Index(taskName, "mark-resetting")
	removeIdx := strings.Index(taskName, "remove-daemon-unit")
	artifactsIdx := strings.Index(taskName, "remove-agent-artifacts")
	clearIdx := strings.Index(taskName, "clear-install-state")

	require.NotEqual(t, -1, markIdx)
	require.NotEqual(t, -1, clearIdx)

	assert.Less(t, markIdx, removeIdx, "reset must be recorded before anything is removed")
	assert.Less(t, artifactsIdx, clearIdx, "the record must outlive the artifact cleanup that uses it")
}

// TestTeardownHostPrefixesUsesInstallRecord covers the failure the review
// found: cleanup discovered the prefix only from the applied config, which is
// written after the node starts. A bootstrap that failed before that point
// therefore fell back to /usr/local, left custom-prefix files behind, and
// deleted the configuration naming them.
func TestTeardownHostPrefixesUsesInstallRecord(t *testing.T) {
	original := installstate.Dir
	installstate.Dir = t.TempDir()

	t.Cleanup(func() { installstate.Dir = original })

	require.NoError(t, installstate.DefaultStore().Save(installstate.Record{
		InstallID:         "install-1",
		MachineName:       "node-1",
		HostPrefix:        "/opt/unbounded",
		ConfigFingerprint: "fingerprint-1",
		Checkpoint:        installstate.CheckpointPreparingRootFS,
	}))

	prefixes := teardownHostPrefixes()

	assert.Contains(t, prefixes, "/opt/unbounded",
		"the recorded prefix must be swept even with no applied config")
	assert.Contains(t, prefixes, goalstates.DefaultHostPrefix,
		"the default must always be swept for hosts provisioned before the prefix was configurable")
}

// TestTeardownHostPrefixesWithoutRecord keeps hosts provisioned by an older
// agent working: with no record, cleanup still sweeps the default.
func TestTeardownHostPrefixesWithoutRecord(t *testing.T) {
	original := installstate.Dir
	installstate.Dir = t.TempDir()

	t.Cleanup(func() { installstate.Dir = original })

	assert.Equal(t, []string{goalstates.DefaultHostPrefix}, teardownHostPrefixes())
}

// TestResetHoldsTheInstallLock covers reset racing a bootstrap. Both mutate the
// same files and the same installation record, and the bootstrap unit retries
// on a timer, so overlap is a real possibility rather than a theoretical one.
//
// Without this, the two interleave and the loser is a half-removed host that
// neither one owns.
func TestResetHoldsTheInstallLock(t *testing.T) {
	originalLock := installstate.LockPathForTest
	installstate.LockPathForTest = filepath.Join(t.TempDir(), "install.lock")

	t.Cleanup(func() { installstate.LockPathForTest = originalLock })

	held, err := installstate.AcquireLock()
	require.NoError(t, err)

	t.Cleanup(func() { _ = held.Release() })

	// A no-op inner task, so a failure can only come from the lock.
	inner := &recordingTask{}

	err = withInstallLock(slog.New(slog.DiscardHandler), inner).Do(context.Background())

	require.ErrorIs(t, err, installstate.ErrLockHeld)
	assert.False(t, inner.ran, "a reset that cannot take the lock must not remove anything")
}

// TestInstallLockIsReleased keeps the wrapper from wedging every later run.
func TestInstallLockIsReleased(t *testing.T) {
	originalLock := installstate.LockPathForTest
	installstate.LockPathForTest = filepath.Join(t.TempDir(), "install.lock")

	t.Cleanup(func() { installstate.LockPathForTest = originalLock })

	log := slog.New(slog.DiscardHandler)

	require.NoError(t, withInstallLock(log, &recordingTask{}).Do(context.Background()))

	// The lock is free again.
	second, err := installstate.AcquireLock()
	require.NoError(t, err)
	require.NoError(t, second.Release())
}

// recordingTask is a no-op task that records whether it ran.
type recordingTask struct{ ran bool }

func (t *recordingTask) Name() string { return "inner" }

func (t *recordingTask) Do(context.Context) error {
	t.ran = true

	return nil
}
