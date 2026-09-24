// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
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
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// TestRenderDaemonAssetFollowsThePrefix renders the three daemon assets under
// both prefixes and asserts every path they carry sits under the one asked for.
//
// The recovery unit is the case that motivated this. Its ExecStart is the only
// reference to the recovery script, so a render that resolved the script from
// the default while installing it under the prefix would produce a unit that
// points at a file that is not there. Nothing else would notice until recovery
// was needed, which is the worst time to find out.
//
// Resolving both layouts from the same prefix here is what the production
// callers do, so the test fails if they are ever resolved independently.
func TestRenderDaemonAssetFollowsThePrefix(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{"", "/opt/unbounded"} {
		t.Run("prefix "+goalstates.HostPrefixOrDefault(prefix), func(t *testing.T) {
			t.Parallel()

			paths, err := goalstates.ResolvedAgentUpgradePathsFor(prefix)
			require.NoError(t, err)

			hostPaths := goalstates.ResolveHostPaths(prefix)
			bin := filepath.Join(goalstates.HostPrefixOrDefault(prefix), "bin")

			service := renderAsset(t, "daemon-service", daemonServiceContent, paths, hostPaths)
			assert.Contains(t, service, goalstates.DaemonRecoveryUnit)
			assert.Contains(t, service, filepath.Join(bin, "unbounded-agent-current")+" daemon")

			recoveryUnit := renderAsset(t, "daemon-recovery-service", daemonRecoveryServiceContent, paths, hostPaths)
			assert.Contains(t, recoveryUnit, "ExecStart="+hostPaths.DaemonRecoveryScript)
			assert.Contains(t, hostPaths.DaemonRecoveryScript, bin)

			script := renderAsset(t, "daemon-recovery-script", daemonRecoveryScriptContent, paths, hostPaths)
			assert.Contains(t, script, filepath.Join(bin, "unbounded-agent-last-good"))
			assert.Contains(t, script, goalstates.DaemonUnit)
			assert.Contains(t, script, "record-agent-upgrade-failure-signal")

			// The signal path is state about an upgrade rather than part of the
			// installed layout, so it stays put no matter the prefix.
			assert.Contains(t, script, goalstates.DaemonAgentUpgradeSignalPath)
		})
	}
}

func renderAsset(
	t *testing.T,
	name string,
	content []byte,
	paths goalstates.AgentUpgradePaths,
	hostPaths goalstates.HostPaths,
) string {
	t.Helper()

	rendered, err := renderDaemonAssetForPaths(name, content, paths, hostPaths)
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
	}, goalstates.ResolveHostPaths(""))
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

	// --now is what stops it, and stopping it is the part that matters. The
	// unit is a oneshot with RemainAfterExit=yes, so it stays active after it
	// has run, and deleting the file does not change that. A host provisioned
	// again afterwards writes the unit back and starts it, systemd finds it
	// already active and does nothing, and the agent never runs. Nothing
	// reports an error, so the reinstall looks like it succeeded and the node
	// simply never appears.
	require.Contains(t, string(recorded), "disable --now "+goalstates.FirstBootBootstrapUnit,
		"disabling without stopping leaves the unit active, so a later start is a no-op")
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

// TestInstallBootstrapBinaryInstallsUnderThePrefix covers the first host
// mutation of a bootstrap.
//
// PrepareHost is the earliest stage that writes anything, and it writes the
// daemon binary. Installing it under the default while every later stage
// resolves the prefix would leave the binary somewhere nothing looks, on the
// one kind of host where the default is not writable at all.
//
// The already-usable check has to follow the prefix for the same reason: asking
// about the default would report a fresh host as already installed whenever the
// default happens to hold an executable of that name.
func TestInstallBootstrapBinaryInstallsUnderThePrefix(t *testing.T) {
	prefix := t.TempDir()

	require.NoError(t, InstallBootstrapBinary(prefix))

	installed := filepath.Join(prefix, "bin", "unbounded-agent")
	info, err := os.Stat(installed)
	require.NoError(t, err, "binary must land under the configured prefix")
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())

	// Nothing may appear under the default prefix as a side effect.
	assert.NotEqual(t, goalstates.DefaultHostPrefix, prefix)
}

// TestInstallBootstrapBinaryKeepsAnExistingBinary pins the retention rule: a
// host that already has a usable binary keeps it, so a repair does not replace
// the slot an upgrade activated.
func TestInstallBootstrapBinaryKeepsAnExistingBinary(t *testing.T) {
	prefix := t.TempDir()
	installed := filepath.Join(prefix, "bin", "unbounded-agent")

	require.NoError(t, os.MkdirAll(filepath.Dir(installed), 0o755))
	require.NoError(t, os.WriteFile(installed, []byte("incumbent"), 0o755))
	require.NoError(t, InstallBootstrapBinary(prefix))

	data, err := os.ReadFile(installed)
	require.NoError(t, err)
	assert.Equal(t, "incumbent", string(data), "an existing usable binary must be left alone")
}

// TestInstallBootstrapBinaryReplacesAnUnusableBinary is the other half: a
// present but non-executable file is the state a half-finished install leaves
// behind, and repair has to be able to get past it.
func TestInstallBootstrapBinaryReplacesAnUnusableBinary(t *testing.T) {
	prefix := t.TempDir()
	installed := filepath.Join(prefix, "bin", "unbounded-agent")

	require.NoError(t, os.MkdirAll(filepath.Dir(installed), 0o755))
	require.NoError(t, os.WriteFile(installed, []byte("not executable"), 0o644))
	require.NoError(t, InstallBootstrapBinary(prefix))

	data, err := os.ReadFile(installed)
	require.NoError(t, err)
	assert.NotEqual(t, "not executable", string(data), "an unusable binary must be replaced")
}

// TestRemoveAgentArtifactsSweepsEveryPrefix runs the teardown against a
// temporary tree and checks it removes the agent's files from both the
// configured prefix and the default.
//
// Sweeping only one of them orphans the files under the other, and a recovery
// script left behind there refuses the next bootstrap, on a host the operator
// was just told is clean.
func TestRemoveAgentArtifactsSweepsEveryPrefix(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	configured := filepath.Join(root, "opt", "unbounded")
	fallback := filepath.Join(root, "usr", "local")

	var files []string
	for _, prefix := range []string{configured, fallback} {
		files = append(files, goalstates.OwnedHostFiles(prefix)...)
	}

	for _, path := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte("installed"), 0o644))
	}

	configDir := filepath.Join(root, "etc", "unbounded", "agent")
	require.NoError(t, os.MkdirAll(configDir, 0o755))

	task := &removeAgentArtifacts{log: discardLogger(), files: files, dirs: []string{configDir}}
	require.NoError(t, task.Do(t.Context()))

	for _, path := range files {
		_, err := os.Stat(path)
		assert.ErrorIs(t, err, os.ErrNotExist, "%s must be removed", path)
	}

	_, err := os.Stat(configDir)
	assert.ErrorIs(t, err, os.ErrNotExist, "config directory must be removed")

	// Removing an already-absent file is the ordinary case on a partially
	// provisioned host, so a second pass has to succeed.
	require.NoError(t, task.Do(t.Context()), "teardown must be repeatable")
}

// TestRemoveAgentArtifactsIsBuiltFromThePrefix pins the wiring between the
// exported constructor and the swept layout, which the test above cannot see
// because it supplies the list itself.
func TestRemoveAgentArtifactsIsBuiltFromThePrefix(t *testing.T) {
	t.Parallel()

	task, ok := RemoveAgentArtifacts(discardLogger(), "/opt/unbounded").(*removeAgentArtifacts)
	require.True(t, ok)

	assert.Contains(t, task.files, "/opt/unbounded/bin/unbounded-agent")
	assert.Contains(t, task.files, "/usr/local/bin/unbounded-agent")
	assert.Contains(t, task.dirs, goalstates.AgentConfigDir)
}

// TestRemoveOwnedFileSkipsTheUnlinkWhenTheFileIsAbsent covers the failure that
// stopped a reset on an immutable host.
//
// Teardown sweeps every prefix the host might hold files under, and on such a
// host one of them is read-only. Unlinking a path that is not there returns
// EROFS rather than ENOENT, because the kernel checks the parent directory for
// write permission before it resolves the final component, so the ENOENT the
// old code tolerated never arrived and a reset failed over a file that had
// never existed.
//
// The unlink is asserted not to happen at all, rather than its error being
// tolerated. An unwritable directory is not a substitute: unlink returns ENOENT
// there, so a test built that way passes against the original bug.
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

// TestRemoveOwnedFileReportsAFailedUnlink keeps the tolerance narrow. A file
// that is present and cannot be removed is still an error, because a teardown
// reporting success would leave an installation the next bootstrap refuses.
func TestRemoveOwnedFileReportsAFailedUnlink(t *testing.T) {
	t.Parallel()

	present := func(string) (os.FileInfo, error) { return nil, nil } //nolint:nilnil // Only presence is read.
	remove := func(string) error { return syscall.EROFS }

	err := removeOwnedFileWith("/usr/local/bin/unbounded-agent", present, remove)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "/usr/local/bin/unbounded-agent")
}

// TestRemoveOwnedFileRemovesADanglingSymlink pins why the check uses Lstat.
//
// A dangling link is exactly what a partial install leaves behind, and it is
// still a file the agent owns. Stat would follow it, find nothing, and leave it
// on the host.
func TestRemoveOwnedFileRemovesADanglingSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	link := filepath.Join(dir, "unbounded-agent-current")
	require.NoError(t, os.Symlink(filepath.Join(dir, "gone"), link))
	require.NoError(t, removeOwnedFile(link))

	_, err := os.Lstat(link)
	assert.ErrorIs(t, err, os.ErrNotExist, "a dangling link must be removed, not skipped")
}
