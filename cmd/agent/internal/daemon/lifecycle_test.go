// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/fsutil"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// TestRenderDaemonAsset checks every path the daemon assets carry comes from
// the same host root. The recovery unit is the only reference to the recovery
// script, so a mismatch would go unnoticed until recovery was needed.
func TestRenderDaemonAsset(t *testing.T) {
	t.Parallel()

	paths, err := goalstates.ResolvedAgentUpgradePaths()
	require.NoError(t, err)

	hostPaths := goalstates.ResolveHostPaths()
	require.Equal(t, hostPaths.BinDir, filepath.Dir(paths.CurrentPath), "the binaries must be under the host root")
	require.Equal(t, hostPaths.BinDir, filepath.Dir(hostPaths.DaemonRecoveryScript), "the recovery script must be under the host root")

	service := renderAsset(t, "daemon-service", daemonServiceContent)
	assert.Contains(t, service, goalstates.DaemonRecoveryUnit)
	assert.Contains(t, service, paths.CurrentPath+" daemon")

	recoveryUnit := renderAsset(t, "daemon-recovery-service", daemonRecoveryServiceContent)
	assert.Contains(t, recoveryUnit, "ExecStart="+hostPaths.DaemonRecoveryScript)

	script := renderAsset(t, "daemon-recovery-script", daemonRecoveryScriptContent)
	assert.Contains(t, script, paths.LastGoodPath)
	assert.Contains(t, script, goalstates.DaemonUnit)
	assert.Contains(t, script, goalstates.DaemonAgentUpgradeSignalPath)
	assert.Contains(t, script, "record-agent-upgrade-failure-signal")
}

func renderAsset(t *testing.T, name string, content []byte) string {
	t.Helper()

	rendered, err := renderDaemonAsset(name, content)
	require.NoError(t, err)
	require.NotContains(t, string(rendered), "{{")

	return string(rendered)
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

func TestRemoveFirstBootBootstrapUnit(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "systemctl"),
		[]byte("#!/bin/sh\necho \"$@\" >> \""+calls+"\"\n"), 0o755))
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	task := &removeFirstBootUnit{log: discardLogger(), unitDir: t.TempDir()}
	unitPath := filepath.Join(task.unitDir, provision.FirstBootBootstrapUnit)
	require.NoError(t, os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644))

	require.NoError(t, task.Do(t.Context()))
	require.NoFileExists(t, unitPath, "the unit file must be gone, or systemd can still start it")

	recorded, err := os.ReadFile(calls)
	require.NoError(t, err)
	require.Equal(t, "disable --now "+provision.FirstBootBootstrapUnit+"\n", string(recorded),
		"disabling without stopping leaves the unit active, so a later start is a no-op")

	require.NoError(t, task.Do(t.Context()), "a host without the unit has nothing to remove")
}

// TestInstallBootstrapBinaryInstallsWhereTheDaemonLooks: the binary has to land
// at the path the rest of the install resolves, including an environment
// override, or VerifyDaemonInstalled looks elsewhere.
func TestInstallBootstrapBinaryInstallsWhereTheDaemonLooks(t *testing.T) {
	installed := filepath.Join(t.TempDir(), "bin", "unbounded-agent")
	t.Setenv(goalstates.EnvDaemonBinary, installed)

	require.NoError(t, InstallBootstrapBinary())

	info, err := os.Stat(installed)
	require.NoError(t, err, "binary must land at the resolved path")
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

// TestRemoveAgentArtifactsRemovesTheRootLast runs the teardown against a
// temporary tree. The root goes last, so its directories are empty by then.
func TestRemoveAgentArtifactsRemovesTheRootLast(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	var files []string
	for _, name := range []string{"bin/unbounded-agent", "bin/unbounded-agent-current", "libexec/unbounded-localdns-network"} {
		files = append(files, filepath.Join(root, "opt", "unbounded", "agent", name))
	}

	files = append(files, filepath.Join(root, "usr", "local", "bin", "unbounded-agent-install.sh"))

	for _, path := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("installed"), 0o644))
	}

	configDir := filepath.Join(root, "etc", "unbounded", "agent")
	require.NoError(t, os.MkdirAll(configDir, 0o755))

	rootRemovals := 0
	task := &removeAgentArtifacts{
		log:   discardLogger(),
		files: files,
		dirs:  []string{configDir},
		removeRoot: func() error {
			rootRemovals++

			for _, path := range files {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("%s is still present when the root is removed", path)
				}
			}

			return nil
		},
	}
	require.NoError(t, task.Do(t.Context()))
	assert.Equal(t, 1, rootRemovals, "the root must be removed")

	_, err := os.Stat(configDir)
	assert.ErrorIs(t, err, os.ErrNotExist, "config directory must be removed")

	// Removing an already-absent file is the ordinary case on a partially
	// provisioned host, so a second pass has to succeed.
	require.NoError(t, task.Do(t.Context()), "teardown must be repeatable")
}

// TestRemoveOwnedFileSkipsTheUnlinkWhenTheFileIsAbsent covers the failure that
// stopped a reset on an immutable host; see removeOwnedFile. An unwritable
// directory is no substitute for a read-only mount: unlink returns ENOENT there.
func TestRemoveOwnedFileSkipsTheUnlinkWhenTheFileIsAbsent(t *testing.T) {
	t.Parallel()

	called := false
	remove := func(string) error {
		called = true

		return syscall.EROFS
	}
	absent := func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }

	require.NoError(t, removeOwnedFileWith("/usr/local/bin/unbounded-agent", absent, remove))
	assert.False(t, called, "an absent file must not be unlinked, whatever the filesystem would say")
}

// TestRemoveOwnedFileReportsAFailedUnlink keeps the tolerance narrow: a file
// that is present and cannot be removed is still an error.
func TestRemoveOwnedFileReportsAFailedUnlink(t *testing.T) {
	t.Parallel()

	present := func(string) (os.FileInfo, error) { return nil, nil } //nolint:nilnil // Only presence is read.
	remove := func(string) error { return syscall.EROFS }

	err := removeOwnedFileWith("/usr/local/bin/unbounded-agent", present, remove)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "/usr/local/bin/unbounded-agent")
}

// TestRemoveOwnedFileRemovesADanglingSymlink pins why the check uses Lstat.
func TestRemoveOwnedFileRemovesADanglingSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	link := filepath.Join(dir, "unbounded-agent-current")
	require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), link))
	require.NoError(t, removeOwnedFile(link))

	_, err := os.Lstat(link)
	assert.ErrorIs(t, err, os.ErrNotExist, "a dangling link must be removed, not skipped")
}
