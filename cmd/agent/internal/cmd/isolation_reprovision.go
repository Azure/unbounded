// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

const (
	// reprovisionLockWait bounds how long the reset waits for the daemon that
	// staged the handoff to release installation ownership.
	reprovisionLockWait = 10 * time.Minute
	reprovisionLockPoll = 5 * time.Second
)

// reprovisionRunner holds the host-touching steps of the handoff so tests can
// replace them.
type reprovisionRunner struct {
	log *slog.Logger
	// reset is the agent reset: stop the daemon and remove the node.
	reset func(ctx context.Context) error
	// start is the bootstrap with the staged configuration.
	start func(ctx context.Context) error
	// check validates the staged configuration, builds the bootstrap plan for
	// its mode and resolves the plan's inputs, without changing the host.
	check func(ctx context.Context) error
	// abandon removes the unit and the staged binary and configuration once the
	// handoff has given up. The journal stays for the daemon that reports it.
	abandon func(ctx context.Context) error
	// report tells the Machine status that the handoff gave up, with the staged
	// credential, before abandon removes it. Best effort.
	report func(ctx context.Context, j migrate.Journal) error
	// giveUpAfter is how long after the handoff started its runs are retried.
	giveUpAfter time.Duration
	// now and sleep are the clock.
	now   func() time.Time
	sleep func(time.Duration)
}

// newCmdIsolationReprovision is the hidden command the reprovision unit runs.
// It is not for operators: the daemon stages its inputs (see the daemon's
// migration_reprovision.go) and starts the unit that runs it.
func newCmdIsolationReprovision(cmdCtx *CommandContext) *cobra.Command {
	var (
		dir   string
		check bool
	)

	cmd := &cobra.Command{
		Use:    "isolation-reprovision",
		Short:  "Finish an isolation mode change that reprovisions: reset, then bootstrap in the new mode",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			// The bootstrap reads its configuration from here.
			if err := os.Setenv(configFileEnv, filepath.Join(dir, daemon.ReprovisionConfigName)); err != nil {
				return err
			}

			log := cmdCtx.Logger
			r := &reprovisionRunner{
				log: log,
				reset: func(ctx context.Context) error {
					return daemon.ResetAgentForReprovision(log).Do(ctx)
				},
				abandon: func(ctx context.Context) error {
					return daemon.AbandonReprovision(ctx, log, dir)
				},
				report: func(ctx context.Context, j migrate.Journal) error {
					cfg, err := loadConfig(log)
					if err != nil {
						return err
					}

					return daemon.ReportReprovisionGaveUp(ctx, &cfg.AgentConfig, j)
				},
				start: func(ctx context.Context) error {
					cfg, err := loadConfig(log)
					if err != nil {
						return err
					}

					return runStart(ctx, log, cfg)
				},
				check:       func(ctx context.Context) error { return checkStagedConfig(ctx, log, agentModes()) },
				giveUpAfter: daemon.ReprovisionGiveUpAfter,
				now:         time.Now,
				sleep:       time.Sleep,
			}

			if check {
				return r.check(ctx)
			}

			return r.run(ctx, migrate.JournalStore{Path: filepath.Join(dir, daemon.ReprovisionJournalName)})
		},
	}

	cmd.Flags().StringVar(&dir, "handoff-dir", daemon.ReprovisionDir, "Directory the daemon staged the handoff in")
	cmd.Flags().BoolVar(&check, "check", false, "Only validate the staged configuration and resolve the bootstrap inputs; change nothing")

	return cmd
}

// checkStagedConfig loads and validates the staged configuration, builds the
// bootstrap plan for its mode from modes and runs the plan's ResolveInputs, so
// a configuration whose goal state cannot be resolved (an unknown Kubernetes
// version, an incomplete offline bundle) fails while the node still runs. It
// changes nothing on the host. It does not download the artifacts: a source
// that fails after the reset is retried by the handoff and reported if it
// keeps failing.
func checkStagedConfig(ctx context.Context, log *slog.Logger, modes agentModeTable) error {
	cfg, err := loadConfig(log)
	if err != nil {
		return err
	}

	return checkReprovisionConfig(ctx, log, modes, cfg)
}

func checkReprovisionConfig(ctx context.Context, log *slog.Logger, modes agentModeTable, cfg *provision.UnboundedAgentConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	mode, err := cfg.IsolationMode()
	if err != nil {
		return err
	}

	m, err := modes.lookup(mode)
	if err != nil {
		return err
	}

	s := &agentStages{log: log, cfg: cfg}
	if err := m.newPlan().ResolveInputs(ctx, s); err != nil {
		return fmt.Errorf("resolve the bootstrap inputs for isolation mode %s: %w", mode, err)
	}

	return nil
}

// run is idempotent and resumable: the journal says whether the reset is still
// to do, and the bootstrap decides what to do from the host.
func (r *reprovisionRunner) run(ctx context.Context, store migrate.JournalStore) error {
	j, err := store.Load()
	if err != nil {
		return fmt.Errorf("read the reprovision journal: %w", err)
	}

	switch j.Step {
	case migrate.StepBootstrapped:
		r.log.Info("reprovision already finished")

		return nil
	case migrate.StepFailed:
		// A run that gave up may have been interrupted before it cleaned up.
		return r.abandonHandoff(ctx)
	case migrate.StepReprovisioning, migrate.StepBootstrapping:
		if r.now().Sub(j.StartedAt) >= r.giveUpAfter {
			return r.giveUp(ctx, store, j)
		}

		// A run after a failed one waits longer each time, on top of the
		// unit's RestartSec, so a source that is down is not hammered.
		if delay := reprovisionBackoff(j.Attempts); delay > 0 {
			r.log.Info("waiting before the next reprovision attempt", "attempt", j.Attempts+1, "delay", delay)
			r.sleep(delay)
		}

		j.Attempts++
		if err := store.Save(j); err != nil {
			return err
		}
	default:
		return fmt.Errorf("the reprovision journal is at step %q, which the handoff does not run from", j.Step)
	}

	if j.Step == migrate.StepReprovisioning {
		if err := r.resetWithRetry(ctx); err != nil {
			return r.fail(store, j, fmt.Errorf("reset: %w", err))
		}

		j.Step = migrate.StepBootstrapping
		if err := store.Save(j); err != nil {
			return err
		}
	}

	if err := r.start(ctx); err != nil {
		return r.fail(store, j, fmt.Errorf("bootstrap in mode %s: %w", j.To, err))
	}

	j.Step = migrate.StepBootstrapped
	j.Message = ""

	// The daemon the bootstrap installed may already have reported and removed
	// the directory, which is the success it was waiting for. Saving would put a
	// lone journal back, so only a directory that is still there is written to.
	if _, err := os.Stat(filepath.Dir(store.Path)); err != nil {
		r.log.Info("the handoff directory is already gone; the reprovision was reported")

		return nil
	}

	if err := store.Save(j); err != nil {
		r.log.Warn("could not record that the bootstrap finished", "error", err)
	}

	return nil
}

// reprovisionBackoff is the delay before a run, after attempts runs already
// started: none before the first two, then doubling from the unit's
// RestartSec, capped at ReprovisionMaxBackoff, with RestartSec itself
// subtracted since systemd already waited that long.
func reprovisionBackoff(attempts int) time.Duration {
	if attempts < 2 {
		return 0
	}

	delay := daemon.ReprovisionRestartDelay
	for i := 2; i < attempts && delay < daemon.ReprovisionMaxBackoff; i++ {
		delay *= 2
	}

	delay = min(delay, daemon.ReprovisionMaxBackoff)

	return delay - daemon.ReprovisionRestartDelay
}

// resetWithRetry runs the reset, waiting while the daemon that started the
// handoff still holds installation ownership.
func (r *reprovisionRunner) resetWithRetry(ctx context.Context) error {
	deadline := r.now().Add(reprovisionLockWait)

	for {
		err := r.reset(ctx)
		if !errors.Is(err, installstate.ErrLockHeld) || r.now().After(deadline) {
			return err
		}

		r.log.Info("waiting for the daemon to release installation ownership before resetting")
		r.sleep(reprovisionLockPoll)
	}
}

// giveUp is the end of a handoff that has been retried for giveUpAfter. It
// records the failure as terminal, reports it in the Machine status while the
// staged credential is still there, removes the unit and the staged
// credentials, and returns nil so that systemd does not run the unit again.
// The host may be reset and have no daemon; the first daemon that starts on it
// reports the failure too.
func (r *reprovisionRunner) giveUp(ctx context.Context, store migrate.JournalStore, j migrate.Journal) error {
	last := j.Message
	if last == "" {
		last = "no error was recorded"
	}

	state := "the host was reset and is not bootstrapped"
	if j.Step == migrate.StepReprovisioning {
		state = "the reset did not complete, so the old node may still be partly in place"
	}

	j.Step = migrate.StepFailed
	j.Message = fmt.Sprintf("reprovision to isolation mode %s gave up after %d attempts over %s; %s: %s",
		j.To, j.Attempts, r.giveUpAfter, state, last)

	if err := store.Save(j); err != nil {
		return err
	}

	r.log.Error("reprovision gave up", "to", j.To, "attempts", j.Attempts, "last_error", last)

	if r.report != nil {
		if err := r.report(ctx, j); err != nil {
			r.log.Warn("could not report the reprovision that gave up; the next daemon on this host reports it", "error", err)
		}
	}

	return r.abandonHandoff(ctx)
}

func (r *reprovisionRunner) abandonHandoff(ctx context.Context) error {
	if r.abandon == nil {
		return nil
	}

	if err := r.abandon(ctx); err != nil {
		// The journal says the handoff is over; a leftover file does not change
		// that, and a reset removes it.
		r.log.Warn("could not remove the reprovision unit and staged files", "error", err)
	}

	return nil
}

// fail records why the handoff stopped, for operators, and returns the error so
// that systemd retries the unit.
func (r *reprovisionRunner) fail(store migrate.JournalStore, j migrate.Journal, err error) error {
	j.Message = err.Error()

	if saveErr := store.Save(j); saveErr != nil {
		r.log.Warn("could not record the reprovision failure", "error", saveErr)
	}

	return err
}
