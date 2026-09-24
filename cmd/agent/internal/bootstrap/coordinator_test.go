// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
)

type fakeStages struct {
	store     *installstate.Store
	calls     []string
	fail      string
	verifyErr error
}

var (
	errInjected = errors.New("injected stage failure")
	// errRepairFailed is distinct from errInjected so a test can tell whether
	// the reported error is the fault that triggered a repair or the failure of
	// the repair itself.
	errRepairFailed = errors.New("injected repair failure")
)

func (f *fakeStages) run(name string) error {
	f.calls = append(f.calls, name)
	if name != "clean" {
		if _, err := f.store.Load(); err != nil {
			return err
		}
	}

	if name == f.fail {
		if name == "repair" {
			return errRepairFailed
		}

		return errInjected
	}

	return nil
}
func (f *fakeStages) EnsureHostClean(context.Context) error       { return f.run("clean") }
func (f *fakeStages) ResolveInputs(context.Context) error         { return f.run("resolve") }
func (f *fakeStages) PrepareHost(context.Context) error           { return f.run("host") }
func (f *fakeStages) PrepareRootFS(context.Context) error         { return f.run("rootfs") }
func (f *fakeStages) EnsureNodeStarted(context.Context) error     { return f.run("node") }
func (f *fakeStages) EnsureDaemonInstalled(context.Context) error { return f.run("daemon") }
func (f *fakeStages) RepairDaemon(context.Context) error          { f.verifyErr = nil; return f.run("repair") }

func (f *fakeStages) VerifyInstalled(context.Context) error {
	if err := f.run("verify"); err != nil {
		return err
	}

	return f.verifyErr
}

// TestEveryStageRunsOnEveryAttempt is the core of the reapply model.
//
// An earlier design recorded which stage had been reached and skipped anything
// before it. That made the record a claim about the host, and a host changed in
// between would be skipped past rather than repaired. Every stage now runs every
// time and decides from the host what it still has to do, so a retry after a
// failure at any point does the same thing: all of them, in order.
func TestEveryStageRunsOnEveryAttempt(t *testing.T) {
	t.Parallel()

	all := []string{"resolve", "host", "rootfs", "node", "daemon"}

	for _, failAt := range []string{"host", "rootfs", "node", "daemon"} {
		t.Run(failAt, func(t *testing.T) {
			dir := t.TempDir()
			store := installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock"))
			stages := &fakeStages{store: store, fail: failAt}
			c := New(slog.New(slog.DiscardHandler), store, stages, nil)
			id := Identity{MachineName: "machine", ConfigFingerprint: "fingerprint"}

			_, err := c.Run(t.Context(), id)
			require.ErrorIs(t, err, errInjected)

			record, err := store.Load()
			require.NoError(t, err)
			require.Equal(t, installstate.Installing, record.Phase,
				"an unfinished installation records only that it is under way")

			stages.calls = nil
			stages.fail = ""

			outcome, err := c.Run(t.Context(), id)
			require.NoError(t, err)
			require.False(t, outcome.AlreadyComplete)
			require.Equal(t, all, stages.calls, "the retry reapplies every stage regardless of where it failed")

			complete, err := store.Load()
			require.NoError(t, err)
			require.Equal(t, record.InstallID, complete.InstallID, "the retry is the same installation")
			require.Equal(t, installstate.Complete, complete.Phase)
		})
	}
}

func TestCompletedRecoveryDoesNotResolveRetiredBootstrapInputs(t *testing.T) {
	t.Parallel()

	for _, repair := range []bool{false, true} {
		dir := t.TempDir()
		store := installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock"))
		r, err := installstate.NewRecord("machine", "fingerprint", "")
		require.NoError(t, err)

		r.Phase = installstate.Complete
		require.NoError(t, store.Save(r))

		stages := &fakeStages{store: store, fail: "resolve"}
		if repair {
			stages.verifyErr = errInjected
		}

		c := New(slog.New(slog.DiscardHandler), store, stages, nil)
		outcome, err := c.Run(t.Context(), Identity{MachineName: r.MachineName, ConfigFingerprint: r.ConfigFingerprint})
		require.NoError(t, err)
		require.True(t, outcome.AlreadyComplete)

		want := []string{"verify"}
		if repair {
			want = append(want, "repair", "verify")
		}

		require.Equal(t, want, stages.calls)

		complete, err := store.Load()
		require.NoError(t, err)
		require.Equal(t, installstate.Complete, complete.Phase)
	}
}

func TestAdmissionFailurePreventsAllStageWork(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"different-intent", "resetting", "locked"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			store := installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock"))
			r, err := installstate.NewRecord("machine", "fingerprint", "")
			require.NoError(t, err)

			if mode == "resetting" {
				r.Phase = installstate.Resetting
			}

			require.NoError(t, store.Save(r))

			id := Identity{MachineName: r.MachineName, ConfigFingerprint: r.ConfigFingerprint}
			if mode == "different-intent" {
				id.ConfigFingerprint = "different"
			}

			if mode == "locked" {
				lock, err := store.AcquireLock()
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, lock.Release()) })
			}

			stages := &fakeStages{store: store}
			c := New(slog.New(slog.DiscardHandler), store, stages, nil)
			c.lockWait = 0 // waiting is covered by TestRunWaitsForTheInstallationLock

			_, err = c.Run(t.Context(), id)
			require.Error(t, err)
			require.Empty(t, stages.calls)
		})
	}
}

func TestInterruptedRepairRemainsCompleteAndRetries(t *testing.T) {
	store := installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
	r, err := installstate.NewRecord("machine", "fingerprint", "")
	require.NoError(t, err)
	require.NoError(t, store.MarkComplete(r))
	stages := &fakeStages{store: store, fail: "repair", verifyErr: errInjected}
	c := New(slog.New(slog.DiscardHandler), store, stages, nil)
	id := Identity{MachineName: r.MachineName, ConfigFingerprint: r.ConfigFingerprint}
	_, err = c.Run(t.Context(), id)
	require.ErrorIs(t, err, errInjected)
	loaded, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, installstate.Complete, loaded.Phase)

	stages.fail = ""
	stages.verifyErr = errInjected
	stages.calls = nil
	_, err = c.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, []string{"verify", "repair", "verify"}, stages.calls)
}

// recordInode identifies the record file itself rather than its contents.
//
// MarkComplete on an already-complete record writes the same bytes, so
// comparing content cannot tell a rewrite from a no-op. The store replaces the
// file atomically, so any write at all produces a new inode.
func recordInode(t *testing.T, store *installstate.Store) uint64 {
	t.Helper()

	info, err := os.Stat(filepath.Join(store.Root(), "install-state.json"))
	require.NoError(t, err)

	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok, "inode is how this test distinguishes a rewrite from a no-op")

	return stat.Ino
}

// TestHealthyCompletedInstallIsNotRewritten covers the cost of an Ignition unit
// that carries no completion condition.
//
// That unit runs on every boot and reaches this path each time. Rewriting the
// record when nothing changed would be a durable write per boot on every node,
// and a write is a chance to fail: a host that is entirely healthy would be
// taking one for no reason.
func TestHealthyCompletedInstallIsNotRewritten(t *testing.T) {
	store := installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
	r, err := installstate.NewRecord("machine", "fingerprint", "")
	require.NoError(t, err)
	require.NoError(t, store.MarkComplete(r))

	before := recordInode(t, store)

	stages := &fakeStages{store: store}
	c := New(slog.New(slog.DiscardHandler), store, stages, nil)

	outcome, err := c.Run(t.Context(), Identity{MachineName: r.MachineName, ConfigFingerprint: r.ConfigFingerprint})
	require.NoError(t, err)
	require.True(t, outcome.AlreadyComplete)
	require.Equal(t, []string{"verify"}, stages.calls, "a healthy host needs no repair")

	require.Equal(t, before, recordInode(t, store),
		"nothing changed, so the record must not have been written at all")
}

// TestRepairedInstallIsCommitted is the other half: when a repair did happen,
// the result has to be durable before the process exits.
func TestRepairedInstallIsCommitted(t *testing.T) {
	store := installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
	r, err := installstate.NewRecord("machine", "fingerprint", "")
	require.NoError(t, err)
	require.NoError(t, store.MarkComplete(r))

	before := recordInode(t, store)

	stages := &fakeStages{store: store, verifyErr: errInjected}
	c := New(slog.New(slog.DiscardHandler), store, stages, nil)

	_, err = c.Run(t.Context(), Identity{MachineName: r.MachineName, ConfigFingerprint: r.ConfigFingerprint})
	require.NoError(t, err)
	require.Equal(t, []string{"verify", "repair", "verify"}, stages.calls)

	loaded, err := store.Load()
	require.NoError(t, err)
	require.Equal(t, installstate.Complete, loaded.Phase)
	require.NotEqual(t, before, recordInode(t, store),
		"a repair changed the host, so the result has to be made durable")
}

// TestFailedRepairReportsWhatWasWrong pins that the original fault survives.
//
// The first verify says what is broken; the repair failure says only that
// fixing it did not work. Reporting the second alone sends an operator after
// the wrong thing.
func TestFailedRepairReportsWhatWasWrong(t *testing.T) {
	store := installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
	r, err := installstate.NewRecord("machine", "fingerprint", "")
	require.NoError(t, err)
	require.NoError(t, store.MarkComplete(r))

	stages := &fakeStages{store: store, fail: "repair", verifyErr: errInjected}
	c := New(slog.New(slog.DiscardHandler), store, stages, nil)

	_, err = c.Run(t.Context(), Identity{MachineName: r.MachineName, ConfigFingerprint: r.ConfigFingerprint})
	require.Error(t, err)
	require.ErrorIs(t, err, errInjected, "the fault that triggered the repair must still be reported")
	require.ErrorIs(t, err, errRepairFailed, "and so must the reason repairing it did not work")
}

// TestRunWaitsForTheInstallationLock covers a reboot, where the daemon holds
// the lock while it migrates the host and the first-boot unit runs start at
// the same time.
func TestRunWaitsForTheInstallationLock(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		releaseIn time.Duration
		lockWait  time.Duration
		cancel    bool
		wantErr   error
		wantCalls []string
	}{
		{
			name:      "released within the wait",
			releaseIn: 100 * time.Millisecond,
			lockWait:  10 * time.Second,
			wantCalls: []string{"verify"},
		},
		{
			name:      "still held at the deadline",
			releaseIn: time.Hour,
			lockWait:  100 * time.Millisecond,
			wantErr:   installstate.ErrLockHeld,
		},
		{
			name:      "canceled while waiting",
			releaseIn: time.Hour,
			lockWait:  10 * time.Second,
			cancel:    true,
			wantErr:   context.Canceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
			r, err := installstate.NewRecord("machine", "fingerprint", "")
			require.NoError(t, err)
			require.NoError(t, store.MarkComplete(r))

			held, err := store.AcquireLock()
			require.NoError(t, err)

			release := time.AfterFunc(tt.releaseIn, func() { _ = held.Release() })

			t.Cleanup(func() {
				release.Stop()

				_ = held.Release()
			})

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			if tt.cancel {
				time.AfterFunc(100*time.Millisecond, cancel)
			}

			stages := &fakeStages{store: store}
			c := New(slog.New(slog.DiscardHandler), store, stages, nil)
			c.lockWait = tt.lockWait
			c.lockPoll = 10 * time.Millisecond

			_, err = c.Run(ctx, Identity{MachineName: r.MachineName, ConfigFingerprint: r.ConfigFingerprint})
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}

			require.Equal(t, tt.wantCalls, stages.calls)
		})
	}
}
