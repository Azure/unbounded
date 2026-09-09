// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/installstate"
)

// fakeStages records what ran and fails on demand, so a test can describe a
// failure at a chosen boundary and then assert what the next attempt does.
type fakeStages struct {
	calls []string

	// failAt makes the named stage fail until cleared, which is how an
	// interrupted attempt is reproduced.
	failAt string

	// verifyErr is what VerifyInstalled reports.
	verifyErr error

	// rebuildRequests records the rebuildOwned argument for each PrepareRootFS
	// call, which is the safety-critical one.
	rebuildRequests []bool
}

var errInjected = errors.New("injected failure")

func (f *fakeStages) record(name string) error {
	f.calls = append(f.calls, name)

	if f.failAt == name {
		return errInjected
	}

	return nil
}

func (f *fakeStages) EnsureHostClean(context.Context) error { return f.record("ensure-host-clean") }
func (f *fakeStages) ResolveInputs(context.Context) error   { return f.record("resolve-inputs") }
func (f *fakeStages) PrepareHost(context.Context) error     { return f.record("prepare-host") }

func (f *fakeStages) PrepareRootFS(_ context.Context, rebuildOwned bool) error {
	f.rebuildRequests = append(f.rebuildRequests, rebuildOwned)
	return f.record("prepare-rootfs")
}

func (f *fakeStages) EnsureNodeStarted(context.Context) error { return f.record("ensure-node-started") }

func (f *fakeStages) EnsureDaemonInstalled(context.Context) error {
	return f.record("ensure-daemon-installed")
}

func (f *fakeStages) VerifyInstalled(context.Context) error {
	f.calls = append(f.calls, "verify-installed")
	return f.verifyErr
}

func testIdentity() Identity {
	return Identity{
		MachineName:       "node-1",
		HostPrefix:        "/usr/local",
		ConfigFingerprint: "fingerprint-1",
	}
}

func newTestCoordinator(t *testing.T, stages Stages) (*Coordinator, *installstate.Store) {
	t.Helper()

	// A real store on a real directory: the decisions under test are about what
	// survives a crash, so exercising the actual file behavior is the point.
	store := installstate.NewStore(t.TempDir())

	return New(slog.New(slog.DiscardHandler), store, stages, nil), store
}

// withTempLock points the host lock at a scratch path. Not parallel-safe, which
// is why these tests do not call t.Parallel.
func withTempLock(t *testing.T) {
	t.Helper()

	original := installstate.LockPathForTest
	installstate.LockPathForTest = t.TempDir() + "/install.lock"

	t.Cleanup(func() { installstate.LockPathForTest = original })
}

func TestRunCompletesAFreshInstall(t *testing.T) {
	withTempLock(t)

	stages := &fakeStages{}
	coordinator, store := newTestCoordinator(t, stages)

	outcome, err := coordinator.Run(context.Background(), testIdentity())
	require.NoError(t, err)

	assert.True(t, outcome.Installed)
	assert.False(t, outcome.Resumed)

	assert.Equal(t, []string{
		"ensure-host-clean",
		"resolve-inputs",
		"prepare-host",
		"prepare-rootfs",
		"ensure-node-started",
		"ensure-daemon-installed",
	}, stages.calls)

	rec, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, installstate.CheckpointComplete, rec.Checkpoint)

	matches, err := store.CompletionMatches(rec)
	require.NoError(t, err)
	assert.True(t, matches)
}

// TestRetryAfterRootFSFailureResumesThere is the failure this whole mechanism
// exists for: a download dies after the workspace was created, and the retry
// has to continue rather than be refused for finding its own leftovers.
func TestRetryAfterRootFSFailureResumesThere(t *testing.T) {
	withTempLock(t)

	stages := &fakeStages{failAt: "prepare-rootfs"}
	coordinator, store := newTestCoordinator(t, stages)

	_, err := coordinator.Run(context.Background(), testIdentity())
	require.ErrorIs(t, err, errInjected)

	rec, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, installstate.CheckpointPreparingRootFS, rec.Checkpoint,
		"a failed stage must stay the recorded checkpoint")

	// The retry.
	stages.failAt = ""
	stages.calls = nil

	outcome, err := coordinator.Run(context.Background(), testIdentity())
	require.NoError(t, err)
	assert.True(t, outcome.Resumed)

	assert.Equal(t, []string{
		"resolve-inputs",
		"prepare-rootfs",
		"ensure-node-started",
		"ensure-daemon-installed",
	}, stages.calls, "the retry must not re-run host preparation or the clean-host check")
}

// TestRetryAfterNodeStartedDoesNotRebuild is the safety property. Once a node
// may be running, recovery must not repeat host preparation or discard the
// rootfs the node is running from.
func TestRetryAfterNodeStartedDoesNotRebuild(t *testing.T) {
	withTempLock(t)

	stages := &fakeStages{failAt: "ensure-daemon-installed"}
	coordinator, store := newTestCoordinator(t, stages)

	_, err := coordinator.Run(context.Background(), testIdentity())
	require.ErrorIs(t, err, errInjected)

	rec, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, installstate.CheckpointInstallingDaemon, rec.Checkpoint)

	stages.failAt = ""
	stages.calls = nil
	stages.rebuildRequests = nil

	_, err = coordinator.Run(context.Background(), testIdentity())
	require.NoError(t, err)

	assert.Equal(t, []string{"resolve-inputs", "ensure-daemon-installed"}, stages.calls)
	assert.NotContains(t, stages.calls, "prepare-host")
	assert.NotContains(t, stages.calls, "prepare-rootfs")
	assert.Empty(t, stages.rebuildRequests,
		"nothing may ask to discard a rootfs once a node may be running")
}

// TestRebuildIsOnlyOfferedWhenResuming pins who may discard existing content.
// A fresh install has just proven the host is clean, so anything present would
// belong to something else.
func TestRebuildIsOnlyOfferedWhenResuming(t *testing.T) {
	withTempLock(t)

	stages := &fakeStages{failAt: "prepare-rootfs"}
	coordinator, _ := newTestCoordinator(t, stages)

	_, err := coordinator.Run(context.Background(), testIdentity())
	require.ErrorIs(t, err, errInjected)
	require.Equal(t, []bool{false}, stages.rebuildRequests,
		"a fresh install must not be allowed to discard anything")

	stages.failAt = ""

	_, err = coordinator.Run(context.Background(), testIdentity())
	require.NoError(t, err)
	assert.Equal(t, []bool{false, true}, stages.rebuildRequests,
		"only the resume may discard its own leftovers")
}

// TestAlreadyCompleteSkipsWork covers the ordinary reboot: the bootstrap unit
// runs on every boot, and an installed host must do nothing.
func TestAlreadyCompleteSkipsWork(t *testing.T) {
	withTempLock(t)

	stages := &fakeStages{}
	coordinator, _ := newTestCoordinator(t, stages)

	_, err := coordinator.Run(context.Background(), testIdentity())
	require.NoError(t, err)

	stages.calls = nil

	outcome, err := coordinator.Run(context.Background(), testIdentity())
	require.NoError(t, err)

	assert.True(t, outcome.AlreadyComplete)
	assert.Equal(t, []string{"verify-installed"}, stages.calls,
		"a complete host is verified, not rebuilt")
}

// TestCompleteRecordWithBrokenInstallIsRepaired covers a record that claims
// completion on a host that does not look installed. Believing the record
// there is exactly the failure the durable marker was introduced to prevent, so
// the daemon install is finished rather than skipped.
func TestCompleteRecordWithBrokenInstallIsRepaired(t *testing.T) {
	withTempLock(t)

	stages := &fakeStages{}
	coordinator, _ := newTestCoordinator(t, stages)

	_, err := coordinator.Run(context.Background(), testIdentity())
	require.NoError(t, err)

	stages.calls = nil
	stages.verifyErr = errors.New("daemon is not enabled")

	outcome, err := coordinator.Run(context.Background(), testIdentity())
	require.NoError(t, err)

	assert.False(t, outcome.AlreadyComplete)
	assert.Equal(t, []string{"verify-installed", "resolve-inputs", "ensure-daemon-installed"}, stages.calls)
	assert.NotContains(t, stages.calls, "prepare-rootfs",
		"repairing an install must not rebuild the node")
}

// TestRefusesForeignAndResettingHosts covers the states bootstrap must not
// touch. None of them may run a single stage.
func TestRefusesForeignAndResettingHosts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(installstate.Record) installstate.Record
		identity Identity
		want     string
	}{
		{
			name:     "another machine",
			mutate:   func(r installstate.Record) installstate.Record { r.MachineName = "someone-else"; return r },
			identity: testIdentity(),
			want:     "not \"node-1\"",
		},
		{
			name:     "different configuration",
			mutate:   func(r installstate.Record) installstate.Record { r.ConfigFingerprint = "other"; return r },
			identity: testIdentity(),
			want:     "different agent configuration",
		},
		{
			name: "interrupted reset",
			mutate: func(r installstate.Record) installstate.Record {
				r.Checkpoint = installstate.CheckpointResetting
				return r
			},
			identity: testIdentity(),
			want:     "reset did not finish",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withTempLock(t)

			stages := &fakeStages{}
			coordinator, store := newTestCoordinator(t, stages)

			seed := installstate.Record{
				InstallID:         "install-1",
				MachineName:       tc.identity.MachineName,
				HostPrefix:        tc.identity.HostPrefix,
				ConfigFingerprint: tc.identity.ConfigFingerprint,
				Checkpoint:        installstate.CheckpointPreparingRootFS,
			}
			require.NoError(t, store.Save(tc.mutate(seed)))

			_, err := coordinator.Run(context.Background(), tc.identity)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Empty(t, stages.calls, "a refused host must not be touched")
		})
	}
}

// TestConcurrentRunsAreSerialised covers bootstrap racing reset or a second
// bootstrap. The unit retries on a timer, so overlap is real rather than
// theoretical.
func TestConcurrentRunsAreSerialised(t *testing.T) {
	withTempLock(t)

	stages := &fakeStages{}
	coordinator, _ := newTestCoordinator(t, stages)

	held, err := installstate.AcquireLock()
	require.NoError(t, err)

	defer func() { require.NoError(t, held.Release()) }()

	_, err = coordinator.Run(context.Background(), testIdentity())
	require.ErrorIs(t, err, installstate.ErrLockHeld)
	assert.Empty(t, stages.calls, "a run that cannot take the lock must not mutate the host")
}

// TestContextCancellationStopsBeforeNextStage keeps a canceled bootstrap from
// starting further work.
func TestContextCancellationStopsBeforeNextStage(t *testing.T) {
	withTempLock(t)

	stages := &fakeStages{}
	coordinator, _ := newTestCoordinator(t, stages)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := coordinator.Run(ctx, testIdentity())
	require.ErrorIs(t, err, context.Canceled)
}

// TestResolveInputsRunsOnEveryResume pins the fix for a regression introduced
// when stages were first checkpointed.
//
// Attestation yields a bootstrap token and cluster CA that live only in memory.
// It was placed inside the host preparation stage, so a resume that started at
// a later checkpoint never ran it, and an attested host proceeded with no
// token and no way to join. Inputs are therefore resolved on every run,
// outside the checkpoint sequence.
func TestResolveInputsRunsOnEveryResume(t *testing.T) {
	withTempLock(t)

	stages := &fakeStages{failAt: "ensure-daemon-installed"}
	coordinator, _ := newTestCoordinator(t, stages)

	_, err := coordinator.Run(context.Background(), testIdentity())
	require.ErrorIs(t, err, errInjected)

	// Resume at the last checkpoint, well past host preparation.
	stages.failAt = ""
	stages.calls = nil

	_, err = coordinator.Run(context.Background(), testIdentity())
	require.NoError(t, err)

	require.NotEmpty(t, stages.calls)
	assert.Equal(t, "resolve-inputs", stages.calls[0],
		"a resume must resolve in-memory inputs before running any stage")
	assert.NotContains(t, stages.calls, "prepare-host",
		"resolving inputs must not drag host preparation back in")
}

// TestResolveInputsFailureStopsBeforeAnyStage keeps a host untouched when the
// credentials it needs cannot be obtained.
func TestResolveInputsFailureStopsBeforeAnyStage(t *testing.T) {
	withTempLock(t)

	stages := &fakeStages{failAt: "resolve-inputs"}
	coordinator, _ := newTestCoordinator(t, stages)

	_, err := coordinator.Run(context.Background(), testIdentity())
	require.ErrorIs(t, err, errInjected)

	assert.Equal(t, []string{"ensure-host-clean", "resolve-inputs"}, stages.calls,
		"nothing may be mutated when inputs cannot be resolved")
}
