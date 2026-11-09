// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

type reprovisionHarness struct {
	events []string
	clock  time.Time
	report []migrate.Journal

	reportErr error
	store     migrate.JournalStore
	runner    *reprovisionRunner

	resetErrs  []error
	startErr   error
	abandonErr error
}

func newReprovisionHarness(t *testing.T, step migrate.Step) *reprovisionHarness {
	t.Helper()

	h := &reprovisionHarness{clock: time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)}
	h.store = migrate.JournalStore{Path: filepath.Join(t.TempDir(), "journal.json"), Now: h.now}

	require.NoError(t, h.store.Save(migrate.Journal{
		Strategy: migrate.Reprovision,
		From:     isolation.ModeNSpawn, To: isolation.ModeNone, Step: step,
	}))

	h.runner = &reprovisionRunner{
		log: slog.New(slog.DiscardHandler),
		reset: func(context.Context) error {
			h.events = append(h.events, "reset")

			if len(h.resetErrs) == 0 {
				return nil
			}

			err := h.resetErrs[0]
			h.resetErrs = h.resetErrs[1:]

			return err
		},
		start: func(context.Context) error {
			h.events = append(h.events, "start")
			return h.startErr
		},
		abandon: func(context.Context) error {
			h.events = append(h.events, "abandon")
			return h.abandonErr
		},
		report: func(_ context.Context, j migrate.Journal) error {
			h.events = append(h.events, "report")
			h.report = append(h.report, j)

			return h.reportErr
		},
		giveUpAfter: time.Hour,
		now:         h.now,
		sleep: func(d time.Duration) {
			h.events = append(h.events, "sleep")
			h.clock = h.clock.Add(d)
		},
	}

	return h
}

func (h *reprovisionHarness) now() time.Time { return h.clock }

// failRuns runs the handoff n times, each failing, with every run taking
// step of the clock.
func (h *reprovisionHarness) failRuns(t *testing.T, n int, step time.Duration) {
	t.Helper()

	for range n {
		require.Error(t, h.runner.run(t.Context(), h.store))
		h.clock = h.clock.Add(step)
	}
}

func (h *reprovisionHarness) step(t *testing.T) migrate.Step {
	t.Helper()

	j, err := h.store.Load()
	require.NoError(t, err)

	return j.Step
}

func TestReprovisionRunsResetThenBootstrap(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)

	require.NoError(t, h.runner.run(t.Context(), h.store))

	assert.Equal(t, []string{"reset", "start"}, h.events)
	assert.Equal(t, migrate.StepBootstrapped, h.step(t))
}

func TestReprovisionWaitsForTheDaemonToReleaseOwnership(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)
	h.resetErrs = []error{installstate.ErrLockHeld, fmt.Errorf("wrapped: %w", installstate.ErrLockHeld)}

	require.NoError(t, h.runner.run(t.Context(), h.store))

	assert.Equal(t, []string{"reset", "sleep", "reset", "sleep", "reset", "start"}, h.events)
}

func TestReprovisionResetFailureIsRecordedAndRetriedByTheUnit(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)
	h.resetErrs = []error{errors.New("cannot unmount")}

	err := h.runner.run(t.Context(), h.store)
	require.ErrorContains(t, err, "cannot unmount")

	assert.Equal(t, []string{"reset"}, h.events, "bootstrap does not run on a host that did not reset")
	assert.Equal(t, migrate.StepReprovisioning, h.step(t), "the next attempt resets again")

	j, _ := h.store.Load() //nolint:errcheck // saved above
	assert.Contains(t, j.Message, "cannot unmount")

	// systemd runs the unit again.
	h.events = nil
	require.NoError(t, h.runner.run(t.Context(), h.store))
	assert.Equal(t, []string{"reset", "start"}, h.events)
}

func TestReprovisionBootstrapFailureResumesWithoutResettingAgain(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)
	h.startErr = errors.New("kubelet did not join")

	err := h.runner.run(t.Context(), h.store)
	require.ErrorContains(t, err, "kubelet did not join")
	require.ErrorContains(t, err, "mode none")
	assert.Equal(t, migrate.StepBootstrapping, h.step(t))

	h.events, h.startErr = nil, nil
	require.NoError(t, h.runner.run(t.Context(), h.store))
	assert.Equal(t, []string{"start"}, h.events, "the reset is not repeated; bootstrap decides from the host")
	assert.Equal(t, migrate.StepBootstrapped, h.step(t))
}

func TestReprovisionAlreadyFinishedDoesNothing(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepBootstrapped)

	require.NoError(t, h.runner.run(t.Context(), h.store))
	assert.Empty(t, h.events)
}

func TestReprovisionRefusesAJournalItDoesNotRunFrom(t *testing.T) {
	for _, step := range []migrate.Step{migrate.StepReady, migrate.StepRolledBack} {
		h := newReprovisionHarness(t, step)

		require.ErrorContains(t, h.runner.run(t.Context(), h.store), "does not run from")
		assert.Empty(t, h.events, "nothing may be reset for a journal that is not a handoff")
	}
}

func TestReprovisionNeedsAJournal(t *testing.T) {
	r := &reprovisionRunner{log: slog.New(slog.DiscardHandler)}

	require.ErrorContains(t, r.run(t.Context(), migrate.JournalStore{Path: filepath.Join(t.TempDir(), "missing.json")}), "journal")
}

func TestCheckStagedConfig(t *testing.T) {
	good := `{"MachineName":"m1","NodeName":"n1","Isolation":"%s",
"Cluster":{"CaCertBase64":"dGVzdA==","ClusterDNS":"10.0.0.10","Version":"1.33.1"},
"Kubelet":{"ApiServer":"https://api.example.com:6443","Auth":{"BootstrapToken":"abc123.xyz789"}}}`

	stage := func(t *testing.T, mode string) {
		t.Helper()

		path := filepath.Join(t.TempDir(), "config.json")
		require.NoError(t, os.WriteFile(path, fmt.Appendf(nil, good, mode), 0o600))
		t.Setenv(configFileEnv, path)
	}

	// A table whose plans record the resolve, so no goal state is resolved
	// against the host.
	recording := func(err error) (agentModeTable, *recordingPlan) {
		plan := &recordingPlan{err: err}
		m := agentMode{
			newBackend:  func(*slog.Logger, *provision.AgentConfig) isolation.Backend { return &isolation.FakeBackend{} },
			newOperator: func(daemon.BackendFactory) daemon.ModeOperator { return daemon.NoneOperator{} },
			newPlan:     func() stagePlan { return plan },
		}

		return agentModeTable{isolation.ModeNSpawn: m, isolation.ModeNone: m}, plan
	}

	for _, mode := range []string{"nspawn", "none"} {
		t.Run(mode, func(t *testing.T) {
			stage(t, mode)

			modes, plan := recording(nil)
			require.NoError(t, checkStagedConfig(t.Context(), slog.New(slog.DiscardHandler), modes))
			assert.Equal(t, []string{"resolve"}, plan.calls, "the inputs are resolved, nothing else")
		})
	}

	t.Run("inputs that do not resolve", func(t *testing.T) {
		stage(t, "none")

		modes, _ := recording(errors.New("offline bundle has no kubelet"))
		require.ErrorContains(t, checkStagedConfig(t.Context(), slog.New(slog.DiscardHandler), modes), "offline bundle has no kubelet")
	})

	t.Run("a mode this agent has no plan for", func(t *testing.T) {
		stage(t, "none")

		modes, _ := recording(nil)
		delete(modes, isolation.ModeNone)
		require.ErrorContains(t, checkStagedConfig(t.Context(), slog.New(slog.DiscardHandler), modes), "cannot operate")
	})

	t.Run("unknown mode", func(t *testing.T) {
		stage(t, "microvm")

		require.ErrorContains(t, checkStagedConfig(t.Context(), slog.New(slog.DiscardHandler), agentModes()), "Isolation")
	})

	t.Run("missing file", func(t *testing.T) {
		t.Setenv(configFileEnv, filepath.Join(t.TempDir(), "absent.json"))

		require.Error(t, checkStagedConfig(t.Context(), slog.New(slog.DiscardHandler), agentModes()))
	})
}

func TestIsolationReprovisionIsHiddenAndRegistered(t *testing.T) {
	c := newCmdIsolationReprovision(&CommandContext{})

	assert.True(t, c.Hidden)
	assert.NotNil(t, c.Flags().Lookup("handoff-dir"))
	assert.NotNil(t, c.Flags().Lookup("check"))
}

func (h *reprovisionHarness) journal(t *testing.T) migrate.Journal {
	t.Helper()

	j, err := h.store.Load()
	require.NoError(t, err)

	return j
}

func TestReprovisionRetriesThenSucceedsBeforeTheDeadline(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)
	h.startErr = errors.New("kubelet did not join")

	// Two runs fail, as systemd would run the unit again after each.
	for attempt := 1; attempt <= 2; attempt++ {
		require.ErrorContains(t, h.runner.run(t.Context(), h.store), "kubelet did not join")
		assert.Equal(t, attempt, h.journal(t).Attempts)
	}

	h.startErr = nil
	h.events = nil
	require.NoError(t, h.runner.run(t.Context(), h.store))

	assert.Equal(t, []string{"start"}, h.events)
	assert.Equal(t, migrate.StepBootstrapped, h.step(t))
}

// The handoff gives up by time, not by count: runs that keep failing are
// retried until giveUpAfter has passed since the handoff started.
func TestReprovisionGivesUpAfterTheDeadline(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)
	h.startErr = errors.New("kubelet did not join")

	// Many quick failures stay within the deadline.
	h.failRuns(t, 6, time.Minute)
	assert.Equal(t, migrate.StepBootstrapping, h.step(t))

	h.clock = h.clock.Add(time.Hour)
	h.events = nil

	// The next run starts nothing: it records the failure, reports it and
	// stops systemd from running the unit again by succeeding.
	require.NoError(t, h.runner.run(t.Context(), h.store))

	assert.Equal(t, []string{"report", "abandon"}, h.events, "neither reset nor bootstrap runs again")

	j := h.journal(t)
	assert.Equal(t, migrate.StepFailed, j.Step)
	assert.Equal(t, 6, j.Attempts)
	assert.Contains(t, j.Message, "gave up after 6 attempts over 1h0m0s")
	assert.Contains(t, j.Message, "isolation mode none")
	assert.Contains(t, j.Message, "kubelet did not join", "the last error is kept for the report")

	require.Len(t, h.report, 1, "the failure is reported before the staged credential is removed")
	assert.Equal(t, j.Message, h.report[0].Message)
}

// A failed report does not keep the handoff alive: the next daemon reports.
func TestReprovisionGiveUpSurvivesAFailedReport(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepBootstrapping)
	h.reportErr = errors.New("no route to host")
	h.clock = h.clock.Add(2 * time.Hour)

	require.NoError(t, h.runner.run(t.Context(), h.store))
	assert.Equal(t, []string{"report", "abandon"}, h.events)
	assert.Equal(t, migrate.StepFailed, h.step(t))
}

func TestReprovisionGivesUpWhenTheResetNeverWorks(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)
	h.resetErrs = []error{errors.New("busy"), errors.New("busy"), errors.New("busy")}

	h.failRuns(t, 3, 20*time.Minute)

	h.events = nil
	require.NoError(t, h.runner.run(t.Context(), h.store))

	assert.Equal(t, migrate.StepFailed, h.step(t))
	assert.Contains(t, h.journal(t).Message, "busy")
}

// The wording tells an operator whether the old node is still there: a reset
// that never completed leaves it partly intact, a failed bootstrap leaves a
// reset host.
func TestReprovisionGiveUpMessageSaysWhetherTheHostWasReset(t *testing.T) {
	t.Run("the reset never completed", func(t *testing.T) {
		h := newReprovisionHarness(t, migrate.StepReprovisioning)
		h.resetErrs = []error{errors.New("busy"), errors.New("busy"), errors.New("busy")}

		h.failRuns(t, 3, 20*time.Minute)

		require.NoError(t, h.runner.run(t.Context(), h.store))

		msg := h.journal(t).Message
		assert.Contains(t, msg, "reset did not complete")
		assert.NotContains(t, msg, "was reset and is not bootstrapped")
	})

	t.Run("the bootstrap never completed", func(t *testing.T) {
		h := newReprovisionHarness(t, migrate.StepReprovisioning)
		h.startErr = errors.New("kubelet did not join")

		h.failRuns(t, 3, 20*time.Minute)

		require.NoError(t, h.runner.run(t.Context(), h.store))
		assert.Contains(t, h.journal(t).Message, "the host was reset and is not bootstrapped")
	})
}

// Each run after the second waits longer, up to the cap, on top of the unit's
// RestartSec.
func TestReprovisionBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{
		0:  0,
		1:  0,
		2:  0,
		3:  30 * time.Second,
		4:  90 * time.Second,
		5:  210 * time.Second,
		9:  10*time.Minute - 30*time.Second,
		50: 10*time.Minute - 30*time.Second,
	} {
		assert.Equal(t, want, reprovisionBackoff(attempts), "after %d attempts", attempts)
	}
}

func TestReprovisionWaitsLongerAfterRepeatedFailures(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepBootstrapping)
	h.startErr = errors.New("mirror down")

	h.failRuns(t, 4, 0)

	assert.Equal(t, []string{"start", "start", "start", "sleep", "start"}, h.events)
}

// The daemon the bootstrap installed reports the operation and removes the
// handoff directory. The handoff that is still finishing must not put it back.
func TestReprovisionDoesNotRecreateTheHandoffDirectoryTheDaemonRemoved(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepReprovisioning)
	dir := filepath.Dir(h.store.Path)

	h.runner.start = func(context.Context) error {
		return os.RemoveAll(dir)
	}

	require.NoError(t, h.runner.run(t.Context(), h.store))
	assert.NoDirExists(t, dir)
}

func TestReprovisionGaveUpFinishesAnInterruptedCleanup(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepFailed)

	// The previous run saved StepFailed and died before it removed the unit.
	require.NoError(t, h.runner.run(t.Context(), h.store))

	assert.Equal(t, []string{"abandon"}, h.events)
	assert.Equal(t, migrate.StepFailed, h.step(t))
}

func TestReprovisionGiveUpSurvivesACleanupFailure(t *testing.T) {
	h := newReprovisionHarness(t, migrate.StepFailed)
	h.abandonErr = errors.New("read-only file system")

	require.NoError(t, h.runner.run(t.Context(), h.store), "the failure is recorded; a leftover file must not make systemd retry")
	assert.Equal(t, migrate.StepFailed, h.step(t))
}
