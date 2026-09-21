// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
)

type fakeStages struct {
	store     *installstate.Store
	calls     []string
	fail      string
	verifyErr error
}

var errInjected = errors.New("injected stage failure")

func (f *fakeStages) run(name string) error {
	f.calls = append(f.calls, name)
	if name != "clean" {
		if _, err := f.store.Load(); err != nil {
			return err
		}
	}

	if name == f.fail {
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
		r, err := installstate.NewRecord("machine", "fingerprint")
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
			r, err := installstate.NewRecord("machine", "fingerprint")
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
			_, err = New(slog.New(slog.DiscardHandler), store, stages, nil).Run(t.Context(), id)
			require.Error(t, err)
			require.Empty(t, stages.calls)
		})
	}
}

func TestInterruptedRepairRemainsCompleteAndRetries(t *testing.T) {
	store := installstate.NewStore(t.TempDir(), filepath.Join(t.TempDir(), "lock"))
	r, err := installstate.NewRecord("machine", "fingerprint")
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
