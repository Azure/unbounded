// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func TestRenderDaemonAsset(t *testing.T) {
	t.Parallel()

	renderedBytes, err := renderDaemonAsset(discardLogger(), "daemon-service", daemonServiceContent)
	require.NoError(t, err)

	rendered := string(renderedBytes)

	require.NotContains(t, rendered, "{{")
	assert.Contains(t, rendered, goalstates.DaemonRecoveryUnit)
	assert.Contains(t, rendered, goalstates.DaemonBinaryCurrentPath)

	renderedRecoveryBytes, err := renderDaemonAsset(discardLogger(), "daemon-recovery-script", daemonRecoveryScriptContent)
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

// TestDaemonUnitDeclaresDeferredExitCode pins the agreement between the exit
// code the daemon returns when it stands down and the two directives that tell
// systemd to accept it.
//
// If they ever disagree, the daemon still exits quietly but systemd treats the
// code as a crash: it restarts the unit, exhausts StartLimitBurst, and runs
// OnFailure, which is the last-resort binary rollback. Nothing else would fail,
// so the only thing standing between a silent regression and a host rolling its
// agent back for an unfinished install is this test.
func TestDaemonUnitDeclaresDeferredExitCode(t *testing.T) {
	t.Parallel()

	rendered, err := renderDaemonAssetForPaths("daemon-service", daemonServiceContent, goalstates.AgentUpgradePaths{
		CurrentPath:  "/usr/local/bin/unbounded-agent-current",
		LastGoodPath: "/usr/local/bin/unbounded-agent-last-good",
		BinaryPath:   "/usr/local/bin/unbounded-agent",
		SignalPath:   "/var/lib/unbounded/agent/upgrade-signal",
	})
	require.NoError(t, err)

	unit := string(rendered)
	code := strconv.Itoa(DeferredExitCode)

	require.Contains(t, unit, "SuccessExitStatus="+code,
		"systemd must not treat standing down as a failure, or OnFailure runs the binary rollback")
	require.Contains(t, unit, "RestartPreventExitStatus="+code,
		"systemd must not restart a deferred daemon, or repeated starts exhaust the start limit")

	// The safety net for genuine crashes has to survive the above.
	require.Contains(t, unit, "Restart=always")
	require.Contains(t, unit, "OnFailure="+goalstates.DaemonRecoveryUnit)
}

// TestActivateDaemonUnitClearsFailureBeforeStarting pins the order that lets a
// retry recover a host this bug already broke.
//
// A daemon that exhausted its start limit sits in failed state, and systemd
// refuses to start it again until the failure is reset. That refusal applies to
// manual starts too, so a bootstrap retry that only ran enable and start would
// fail on exactly the hosts most in need of repair.
func TestActivateDaemonUnitClearsFailureBeforeStarting(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"),
		[]byte("#!/bin/sh\necho \"$@\" >> \""+calls+"\"\n"), 0o755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	require.NoError(t, activateDaemonUnit(t.Context(), discardLogger(), executil.Systemctl()))

	recorded, err := os.ReadFile(calls)
	require.NoError(t, err)

	got := string(recorded)
	resetAt := strings.Index(got, "reset-failed")
	startAt := strings.Index(got, "start ")

	require.NotEqual(t, -1, resetAt, "reset-failed must run; without it a rate-limited unit cannot be started:\n%s", got)
	require.NotEqual(t, -1, startAt, "start must run:\n%s", got)
	require.Less(t, resetAt, startAt, "reset-failed must precede start, or it cannot unblock it:\n%s", got)
}

// TestActivateDaemonUnitToleratesDeniedResetFailed covers hosts where policy
// withholds reset-failed. It unblocks a start rather than being required for
// one, so a denial must not fail the install.
func TestActivateDaemonUnitToleratesDeniedResetFailed(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"),
		[]byte("#!/bin/sh\ncase \"$1\" in reset-failed) exit 1 ;; esac\nexit 0\n"), 0o755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	require.NoError(t, activateDaemonUnit(t.Context(), discardLogger(), executil.Systemctl()))
}

// TestResetRemovesTheFirstBootBootstrapUnit covers the interaction between
// reset and an Ignition-provisioned host.
//
// The unit carries no completion condition and runs on every boot, deciding
// there is nothing to do from the agent's ownership record. Reset removes that
// record. A unit left behind would therefore find an uninstalled host on the
// next boot and bootstrap it, quietly undoing the reset.
func TestResetRemovesTheFirstBootBootstrapUnit(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"),
		[]byte("#!/bin/sh\necho \"$@\" >> \""+calls+"\"\n"), 0o755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	unitDir := t.TempDir()
	unitPath := filepath.Join(unitDir, goalstates.FirstBootBootstrapUnit)
	require.NoError(t, os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644))

	require.NoError(t, removeFirstBootBootstrapUnitIn(t.Context(), discardLogger(), unitDir))

	require.NoFileExists(t, unitPath, "the unit file must be gone, or systemd can still start it")

	recorded, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Contains(t, string(recorded), "disable "+goalstates.FirstBootBootstrapUnit,
		"removing the file alone leaves the enablement symlink in multi-user.target.wants")
}

// TestFirstBootBootstrapUnitAbsentIsSuccess covers every host not provisioned
// through Ignition, which is the common case. There is nothing to remove and
// nothing to report.
func TestFirstBootBootstrapUnitAbsentIsSuccess(t *testing.T) {
	t.Parallel()

	require.NoError(t, removeFirstBootBootstrapUnitIn(t.Context(), discardLogger(), t.TempDir()))
}

// TestFirstBootBootstrapUnitNameIsShared pins that the command writing the unit
// and the reset removing it agree on its name.
//
// They live in packages that cannot import each other, so the name is held in
// goalstates. If it were duplicated and drifted, reset would leave an enabled
// unit on a host it had just torn down, and the host would re-bootstrap on the
// next boot with nothing reporting why.
func TestFirstBootBootstrapUnitNameIsShared(t *testing.T) {
	t.Parallel()

	require.Equal(t, "unbounded-agent-bootstrap.service", goalstates.FirstBootBootstrapUnit)
}
