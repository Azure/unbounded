// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// configProbe captures what the backends had done when the restore ran.
type configProbe struct {
	restoreSawTargetDestroyed bool
	restoreSawSourceStarted   bool
}

// withConfigHooks adds the snapshot hooks to h and returns the probe they fill.
func withConfigHooks(h *harness) *configProbe {
	probe := &configProbe{}

	hook := func(name string) func(context.Context) error {
		return func(context.Context) error {
			*h.log = append(*h.log, name)
			return h.fail[name]
		}
	}

	h.spec.SnapshotConfig = hook("snapshot-config")
	h.spec.RestoreConfig = func(ctx context.Context) error {
		probe.restoreSawTargetDestroyed = slices.Contains(h.target.Calls(), "destroy kube2")
		probe.restoreSawSourceStarted = slices.Contains(h.source.Calls(), "start kube1")

		return hook("restore-config")(ctx)
	}
	h.spec.DiscardConfig = hook("discard-config")

	return probe
}

func indexOf(t *testing.T, list []string, item string) int {
	t.Helper()

	i := slices.Index(list, item)
	require.GreaterOrEqual(t, i, 0, "%q not in %v", item, list)

	return i
}

func TestRunInPlaceSnapshotsBeforeAnyChangeAndDiscardsOnSuccess(t *testing.T) {
	t.Parallel()

	h := newHarness()
	withConfigHooks(h)

	var steps []Step

	recordInto(h, &steps)

	_, err := RunInPlace(context.Background(), h.spec)
	require.NoError(t, err)

	hooks := h.hooks()
	require.Equal(t, []string{
		"preflight", "download", "snapshot-config", "cleanup-network", "persist", "wait-ready", "remove-source-config", "discard-config",
	}, hooks)
	require.Equal(t, StepPreparing, steps[0], "the snapshot is taken before the first step is recorded")
	require.NotContains(t, hooks, "restore-config")
}

func TestRunInPlaceSnapshotFailureMutatesNothing(t *testing.T) {
	t.Parallel()

	h := newHarness()
	withConfigHooks(h)
	h.fail["snapshot-config"] = errors.New("disk full")

	var steps []Step

	recordInto(h, &steps)

	_, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "save shared configuration")
	require.ErrorContains(t, err, "disk full")
	require.Empty(t, steps, "no step is recorded")
	require.Empty(t, h.source.Calls())
	require.Empty(t, h.target.Calls())
}

func TestRunInPlaceRestoresSharedConfigOnEveryFailureAfterTheTargetExists(t *testing.T) {
	t.Parallel()

	for _, failing := range []string{"cleanup-network", "persist", "wait-ready"} {
		t.Run(failing, func(t *testing.T) {
			t.Parallel()

			h := newHarness()
			probe := withConfigHooks(h)
			h.fail[failing] = errors.New("boom")

			res, err := RunInPlace(context.Background(), h.spec)
			require.ErrorContains(t, err, "boom")
			require.True(t, res.RolledBack)

			hooks := h.hooks()
			require.Contains(t, hooks, "restore-config")
			require.Less(t, indexOf(t, hooks, "snapshot-config"), indexOf(t, hooks, "restore-config"))
			require.Equal(t, "discard-config", hooks[len(hooks)-1], "the snapshot goes after a restore that worked")
			require.True(t, probe.restoreSawTargetDestroyed, "nothing writes the files any more when they are restored")
			require.False(t, probe.restoreSawSourceStarted, "the source starts on the restored files")
			require.Contains(t, h.source.Calls(), "start kube1")
		})
	}
}

func TestRunInPlacePrepareFailureRestoresSharedConfig(t *testing.T) {
	t.Parallel()

	h := newHarness()
	withConfigHooks(h)
	h.target.PrepareErr = errors.New("unpack failed")

	_, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "unpack failed")
	require.Empty(t, h.source.Calls(), "the source was never touched")

	hooks := h.hooks()
	require.Contains(t, hooks, "restore-config", "Prepare may have written shared files before it failed")
	require.Equal(t, "discard-config", hooks[len(hooks)-1])
}

func TestRunInPlaceRestoreFailureKeepsTheSnapshotAndStillStartsTheSource(t *testing.T) {
	t.Parallel()

	h := newHarness()
	withConfigHooks(h)
	h.fail["wait-ready"] = errors.New("not ready")
	h.fail["restore-config"] = errors.New("read-only file system")

	_, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "not ready")
	require.ErrorContains(t, err, "restore shared configuration")
	require.ErrorContains(t, err, "read-only file system")
	require.NotContains(t, h.hooks(), "discard-config", "the snapshot is kept so the restore can be retried")
	require.Contains(t, h.source.Calls(), "start kube1")
}

func TestRunInPlaceWithoutConfigHooksIsUnchanged(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.fail["wait-ready"] = errors.New("not ready")

	_, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "not ready")
	require.NotContains(t, h.hooks(), "restore-config")
}

func TestResumeInPlaceRestoresSharedConfigWhenItRollsBack(t *testing.T) {
	t.Parallel()

	for _, at := range []Step{StepPreparing, StepStoppingSource, StepStartingTarget, StepPersisting} {
		t.Run(string(at), func(t *testing.T) {
			t.Parallel()

			h := resumeHarness()
			probe := withConfigHooks(h)

			_, err := ResumeInPlace(context.Background(), h.spec, at)
			require.ErrorIs(t, err, ErrInterrupted)

			hooks := h.hooks()
			require.Contains(t, hooks, "restore-config", "a crash must not leave the target's files behind")
			require.Equal(t, "discard-config", hooks[len(hooks)-1])
			require.True(t, probe.restoreSawTargetDestroyed)
			require.False(t, probe.restoreSawSourceStarted)
		})
	}
}

func TestResumeInPlaceDiscardsTheSnapshotWhenTheMigrationHoldsOrFinishes(t *testing.T) {
	t.Parallel()

	for _, at := range []Step{StepAwaitingReady, StepReady} {
		t.Run(string(at), func(t *testing.T) {
			t.Parallel()

			h := resumeHarness()
			withConfigHooks(h)

			_, err := ResumeInPlace(context.Background(), h.spec, at)
			require.NoError(t, err)
			require.NotContains(t, h.hooks(), "restore-config")
			require.Contains(t, h.hooks(), "discard-config")
		})
	}
}

func TestResumeInPlaceAwaitingReadyFailureRestoresSharedConfig(t *testing.T) {
	t.Parallel()

	h := resumeHarness()
	withConfigHooks(h)
	h.fail["wait-ready"] = errors.New("target is down")

	_, err := ResumeInPlace(context.Background(), h.spec, StepAwaitingReady)
	require.ErrorIs(t, err, ErrInterrupted)
	require.Contains(t, h.hooks(), "restore-config")
}

// Prepare fails and so does the rollback that follows: the node needs attention
// and the failure must say so, or the caller records a clean rollback and
// nothing retries it.
func TestRunInPlacePrepareFailureWithAFailedRollbackIsMarked(t *testing.T) {
	t.Parallel()

	t.Run("target destroy fails", func(t *testing.T) {
		t.Parallel()

		h := newHarness()
		withConfigHooks(h)
		h.spec.TargetRoot = isolation.SlotName("kube2")
		h.target.PrepareErr = errors.New("unpack failed")
		h.target.DestroyErr = errors.New("destroy failed")

		_, err := RunInPlace(context.Background(), h.spec)
		require.ErrorIs(t, err, ErrRollbackFailed)
		require.ErrorContains(t, err, "unpack failed")
		require.ErrorContains(t, err, "destroy failed")
	})

	t.Run("restore of the shared configuration fails", func(t *testing.T) {
		t.Parallel()

		h := newHarness()
		withConfigHooks(h)
		h.target.PrepareErr = errors.New("unpack failed")
		h.fail["restore-config"] = errors.New("restore failed")

		_, err := RunInPlace(context.Background(), h.spec)
		require.ErrorIs(t, err, ErrRollbackFailed)
		require.ErrorContains(t, err, "restore failed")
		require.NotContains(t, h.hooks(), "discard-config", "a snapshot that was not restored is kept")
	})
}

// The snapshot holds kubelet keys. An exit between taking it and the first
// recorded step must not leave it on disk with no journal to remove it.
func TestRunInPlaceDiscardsTheSnapshotWhenTheFirstStepCannotBeRecorded(t *testing.T) {
	t.Parallel()

	h := newHarness()
	withConfigHooks(h)
	h.spec.Record = func(context.Context, Step) error { return errors.New("journal is read-only") }

	_, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "journal is read-only")
	require.Contains(t, h.hooks(), "discard-config")
	require.Empty(t, h.source.Calls())
	require.Empty(t, h.target.Calls())
}

func TestRunInPlaceDiscardsAPartialSnapshotWhenTakingItFails(t *testing.T) {
	t.Parallel()

	h := newHarness()
	withConfigHooks(h)
	h.fail["snapshot-config"] = errors.New("disk full")

	_, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "disk full")
	require.Contains(t, h.hooks(), "discard-config")
}
