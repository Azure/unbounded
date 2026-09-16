// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/installstate"
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

func TestInterruptedStagesResumeWithoutReplayingEarlierStages(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		fail       string
		checkpoint installstate.Checkpoint
		want       []string
	}{
		{"host", installstate.PreparingHost, []string{"resolve", "host", "rootfs", "node", "daemon"}},
		{"rootfs", installstate.PreparingRootFS, []string{"resolve", "rootfs", "node", "daemon"}},
		{"node", installstate.StartingNode, []string{"resolve", "node", "daemon"}},
		{"daemon", installstate.InstallingDaemon, []string{"resolve", "daemon"}},
	} {
		t.Run(tc.fail, func(t *testing.T) {
			dir := t.TempDir()
			store := installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock"))
			stages := &fakeStages{store: store, fail: tc.fail}
			c := New(slog.New(slog.DiscardHandler), store, stages, nil)
			id := Identity{MachineName: "machine", ConfigFingerprint: "fingerprint"}
			_, err := c.Run(t.Context(), id)
			require.ErrorIs(t, err, errInjected)
			record, err := store.Load()
			require.NoError(t, err)
			require.Equal(t, tc.checkpoint, record.Checkpoint)

			_, err = os.Stat(store.CompletePath())
			require.ErrorIs(t, err, os.ErrNotExist)

			stages.calls = nil
			stages.fail = ""
			outcome, err := c.Run(t.Context(), id)
			require.NoError(t, err)
			require.False(t, outcome.AlreadyComplete)
			require.Equal(t, tc.want, stages.calls)

			complete, err := store.Load()
			require.NoError(t, err)
			require.Equal(t, record.InstallID, complete.InstallID)
			require.Equal(t, installstate.Complete, complete.Checkpoint)
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

		r.Checkpoint = installstate.Complete
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

		marker, err := store.CheckMarker(r)
		require.NoError(t, err)
		require.True(t, marker)
	}
}

func TestAdmissionFailurePreventsAllStageWork(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"different-intent", "resetting", "marker-conflict", "locked"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			store := installstate.NewStore(filepath.Join(dir, "state"), filepath.Join(dir, "lock"))
			r, err := installstate.NewRecord("machine", "fingerprint")
			require.NoError(t, err)

			if mode == "resetting" {
				r.Checkpoint = installstate.Resetting
			}

			require.NoError(t, store.Save(r))

			id := Identity{MachineName: r.MachineName, ConfigFingerprint: r.ConfigFingerprint}
			if mode == "different-intent" {
				id.ConfigFingerprint = "different"
			}

			if mode == "marker-conflict" {
				require.NoError(t, os.WriteFile(store.CompletePath(), []byte("other"), 0o644))
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

func TestSyncBarrierDeduplicatesFilesystem(t *testing.T) {
	dir := t.TempDir()
	a, err := os.Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, a.Close()) })

	b, err := os.Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })

	calls := 0

	require.NoError(t, SyncOpenFilesystems([]*os.File{a, b}, func(int) error { calls++; return nil }))
	require.Equal(t, 1, calls)
	require.ErrorIs(t, SyncOpenFilesystems([]*os.File{a, b}, func(int) error { return errInjected }), errInjected)
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
	require.Equal(t, installstate.Complete, loaded.Checkpoint)

	stages.fail = ""
	stages.verifyErr = errInjected
	stages.calls = nil
	_, err = c.Run(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, []string{"verify", "repair", "verify"}, stages.calls)
}
