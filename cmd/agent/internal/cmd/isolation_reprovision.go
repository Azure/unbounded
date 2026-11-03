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
	// reset is the agent reset: stop the daemon and run resetResources.
	reset func(ctx context.Context) error
	// start is the bootstrap: the start command with the staged configuration.
	start func(ctx context.Context) error
	// loadPlan validates the staged configuration and that a bootstrap plan
	// exists for its mode.
	loadPlan func() error
	// abandon removes the unit and the staged binary and configuration once the
	// handoff has given up. The journal stays for the daemon that reports it.
	abandon func(ctx context.Context) error
	// maxAttempts bounds the runs that may start before the handoff gives up.
	maxAttempts int
	sleep       func(time.Duration)
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

			configPath := filepath.Join(dir, daemon.ReprovisionConfigName)

			// The bootstrap reads its configuration from here.
			if err := os.Setenv(configFileEnv, configPath); err != nil {
				return err
			}

			r := &reprovisionRunner{
				log: cmdCtx.Logger,
				reset: func(ctx context.Context) error {
					return daemon.ResetAgentForReprovision(cmdCtx.Logger).Do(ctx)
				},
				abandon: func(ctx context.Context) error {
					return daemon.AbandonReprovision(ctx, cmdCtx.Logger, dir)
				},
				maxAttempts: daemon.ReprovisionMaxAttempts,
				start:       func(ctx context.Context) error { return runStart(ctx, cmdCtx, configOverrides{}) },
				loadPlan:    func() error { return checkStagedConfig(cmdCtx.Logger) },
				sleep:       time.Sleep,
			}

			store := migrate.JournalStore{Path: filepath.Join(dir, daemon.ReprovisionJournalName)}

			if check {
				return r.loadPlan()
			}

			return r.run(ctx, store)
		},
	}

	cmd.Flags().StringVar(&dir, "handoff-dir", daemon.ReprovisionDir, "Directory the daemon staged the handoff in")
	cmd.Flags().BoolVar(&check, "check", false, "Only validate the staged configuration and the bootstrap plan; change nothing")

	return cmd
}

// checkStagedConfig loads and validates the staged configuration and builds the
// bootstrap plan for its mode, without changing the host.
func checkStagedConfig(log *slog.Logger) error {
	cfg, err := loadConfig(log)
	if err != nil {
		return err
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	mode, err := cfg.IsolationMode()
	if err != nil {
		return err
	}

	_, err = newStagePlan(mode)

	return err
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
		if j.Attempts >= r.maxAttempts {
			return r.giveUp(ctx, store, j)
		}

		j.Attempts++
		if err := store.Save(j); err != nil {
			return err
		}
	}

	switch j.Step {
	case migrate.StepReprovisioning:
		if err := r.resetWithRetry(ctx); err != nil {
			return r.fail(store, j, fmt.Errorf("reset: %w", err))
		}

		j.Step = migrate.StepBootstrapping
		if err := store.Save(j); err != nil {
			return err
		}
	case migrate.StepBootstrapping:
		// The reset finished in an earlier run; bootstrap continues.
	default:
		return fmt.Errorf("the reprovision journal is at step %q, which the handoff does not run from", j.Step)
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

// resetWithRetry runs the reset, waiting while the daemon that started the
// handoff still holds installation ownership.
func (r *reprovisionRunner) resetWithRetry(ctx context.Context) error {
	deadline := time.Now().Add(reprovisionLockWait)

	for {
		err := r.reset(ctx)
		if !errors.Is(err, installstate.ErrLockHeld) || time.Now().After(deadline) {
			return err
		}

		r.log.Info("waiting for the daemon to release installation ownership before resetting")
		r.sleep(reprovisionLockPoll)
	}
}

// giveUp is the end of a handoff whose runs are used up. It records the failure
// as terminal, removes the unit and the staged credentials, and returns nil so
// that systemd does not run the unit again. The host is reset and has no daemon
// to tell the Machine status; the first daemon that starts on it does.
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
	j.Message = fmt.Sprintf("reprovision to isolation mode %s gave up after %d attempts; %s: %s",
		j.To, j.Attempts, state, last)

	if err := store.Save(j); err != nil {
		return err
	}

	r.log.Error("reprovision gave up", "to", j.To, "attempts", j.Attempts, "last_error", last)

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
