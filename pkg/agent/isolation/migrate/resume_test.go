// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/pkg/agent/isolation"
)

func recordInto(h *harness, steps *[]Step) {
	h.spec.Record = func(_ context.Context, s Step) error {
		*steps = append(*steps, s)
		return nil
	}
}

func TestRunInPlaceRecordsStepsInOrder(t *testing.T) {
	t.Parallel()

	h := newHarness()

	var steps []Step

	recordInto(h, &steps)

	_, err := RunInPlace(context.Background(), h.spec)
	require.NoError(t, err)
	require.Equal(t, []Step{
		StepPreparing, StepStoppingSource, StepStartingTarget,
		StepPersisting, StepAwaitingReady, StepReady, StepComplete,
	}, steps)
}

func TestRunInPlacePreflightFailureRecordsNothing(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.fail["preflight"] = errors.New("no runtime")

	var steps []Step

	recordInto(h, &steps)

	_, err := RunInPlace(context.Background(), h.spec)
	require.Error(t, err)
	require.Empty(t, steps, "nothing was mutated so nothing may be journaled")
}

func TestRunInPlaceRecordFailureStopsBeforeTheStep(t *testing.T) {
	t.Parallel()

	h := newHarness()
	h.spec.Record = func(_ context.Context, s Step) error {
		if s == StepStoppingSource {
			return errors.New("disk full")
		}

		return nil
	}

	res, err := RunInPlace(context.Background(), h.spec)
	require.ErrorContains(t, err, "disk full")
	require.False(t, res.RolledBack)
	require.Empty(t, h.source.Calls(), "the source must not be stopped when the step could not be recorded")
	require.Equal(t, []string{"prepare kube2", "destroy kube2"}, h.target.Calls())
}

// resumeHarness is a harness whose spec is complete for ResumeInPlace.
func resumeHarness() *harness {
	h := newHarness()
	h.spec.TargetRoot = isolation.SlotName("kube2")

	return h
}

func TestResumeInPlace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		at         Step
		wantErr    error
		rolledBack bool
		source     []string
		target     []string
		hooks      []string
	}{
		{
			name: "preparing removes the partial target only", at: StepPreparing, wantErr: ErrInterrupted,
			target: []string{"destroy kube2"},
		},
		{
			name: "stopping source restarts it", at: StepStoppingSource, wantErr: ErrInterrupted, rolledBack: true,
			source: []string{"start kube1"}, target: []string{"stop kube2", "destroy kube2", "status kube2"},
		},
		{
			name: "starting target stops it and restarts the source", at: StepStartingTarget, wantErr: ErrInterrupted, rolledBack: true,
			source: []string{"start kube1"}, target: []string{"stop kube2", "destroy kube2", "status kube2"},
		},
		{
			name: "persisting also restores the source config", at: StepPersisting, wantErr: ErrInterrupted, rolledBack: true,
			source: []string{"start kube1"}, target: []string{"stop kube2", "destroy kube2", "status kube2"}, hooks: []string{"restore-source"},
		},
		{
			name: "awaiting ready finishes when ready", at: StepAwaitingReady,
			source: []string{"destroy kube1"}, hooks: []string{"wait-ready", "remove-source-config"},
		},
		{
			name: "ready only cleans up the source", at: StepReady,
			source: []string{"destroy kube1"}, hooks: []string{"remove-source-config"},
		},
		{name: "complete does nothing", at: StepComplete},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := resumeHarness()

			res, err := ResumeInPlace(context.Background(), h.spec, tt.at)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
				require.Equal(t, "kube2", res.Target.Slot())
			}

			require.Equal(t, tt.rolledBack, res.RolledBack)
			require.Equal(t, tt.source, nilIfEmpty(h.source.Calls()))
			require.Equal(t, tt.target, nilIfEmpty(h.target.Calls()))
			require.Equal(t, tt.hooks, nilIfEmpty(h.hooks()))
		})
	}
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}

	return s
}

func TestResumeInPlaceAwaitingReadyTimeoutRollsBack(t *testing.T) {
	t.Parallel()

	h := resumeHarness()
	h.fail["wait-ready"] = errors.New("timed out")

	res, err := ResumeInPlace(context.Background(), h.spec, StepAwaitingReady)
	require.ErrorIs(t, err, ErrInterrupted)
	require.ErrorContains(t, err, "timed out")
	require.True(t, res.RolledBack)
	require.Equal(t, []string{"stop kube2", "destroy kube2", "status kube2"}, h.target.Calls())
	require.Equal(t, []string{"start kube1"}, h.source.Calls())
	require.Contains(t, h.hooks(), "restore-source")
}

func TestResumeInPlaceRollbackFailureIsReported(t *testing.T) {
	t.Parallel()

	h := resumeHarness()
	h.source.StartErr = errors.New("source will not start")

	_, err := ResumeInPlace(context.Background(), h.spec, StepStartingTarget)
	require.ErrorIs(t, err, ErrInterrupted)
	require.ErrorIs(t, err, ErrRollbackFailed)
}

func TestResumeInPlaceRequiresTargetRoot(t *testing.T) {
	t.Parallel()

	h := newHarness()

	_, err := ResumeInPlace(context.Background(), h.spec, StepReady)
	require.ErrorContains(t, err, "incomplete resume spec")
}

func TestResumeInPlaceUnknownStep(t *testing.T) {
	t.Parallel()

	_, err := ResumeInPlace(context.Background(), resumeHarness().spec, Step("bogus"))
	require.ErrorContains(t, err, "unknown step")
}

func TestJournalStoreRoundTrip(t *testing.T) {
	t.Parallel()

	s := JournalStore{Path: filepath.Join(t.TempDir(), "sub", "journal.json")}

	_, err := s.Load()
	require.ErrorIs(t, err, ErrNoJournal)

	in := Journal{
		Strategy: InPlace, From: isolation.ModeNSpawn, To: isolation.ModeContainer,
		SourceSlot: "kube1", TargetSlot: "kube2", Step: StepStoppingSource,
	}
	require.NoError(t, s.Save(in))

	out, err := s.Load()
	require.NoError(t, err)
	require.Equal(t, JournalVersion, out.Version)
	require.Equal(t, StepStoppingSource, out.Step)
	require.False(t, out.StartedAt.IsZero())
	require.False(t, out.Terminal())

	info, err := os.Stat(s.Path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	in.Step = StepComplete
	in.StartedAt = out.StartedAt
	require.NoError(t, s.Save(in))

	out, err = s.Load()
	require.NoError(t, err)
	require.True(t, out.Terminal())
	require.Equal(t, in.StartedAt.UTC(), out.StartedAt.UTC())

	require.NoError(t, s.Remove())
	require.NoError(t, s.Remove(), "removing a missing journal is not an error")

	_, err = s.Load()
	require.ErrorIs(t, err, ErrNoJournal)
}

func TestJournalStoreRejectsDamagedFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "journal.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, err := JournalStore{Path: path}.Load()
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNoJournal)

	require.NoError(t, os.WriteFile(path, []byte(`{"version":99}`), 0o600))

	_, err = JournalStore{Path: path}.Load()
	require.ErrorContains(t, err, "unsupported version")
}

func TestJournalLoadIgnoresUnknownFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "journal.json")
	raw := `{"version":1,"operation":"op-1","strategy":"inPlace","from":"nspawn","to":"container",` +
		`"sourceSlot":"kube1","targetSlot":"kube2","step":"preparing","someFutureField":true,` +
		`"startedAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))

	j, err := JournalStore{Path: path}.Load()
	require.NoError(t, err)
	require.Equal(t, StepPreparing, j.Step)
	require.False(t, j.Terminal())
}
