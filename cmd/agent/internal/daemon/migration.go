// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases/host"
	"github.com/Azure/unbounded/pkg/agent/preflight"
)

// isolationMigrator carries out a planned mode change. The repave reconciler
// depends on it so its tests can fake the change.
type isolationMigrator interface {
	// MigrateIsolation moves the active node to the isolation mode of the
	// desired configuration, using the already planned strategy. The caller
	// has planned it with migrate.Plan, settled any earlier change and holds
	// installation ownership. The target is built from the desired
	// configuration, so a new Kubernetes version in the same repave is
	// applied.
	//
	// It returns ErrMigrationHandedOff when the change continues without the
	// daemon (reprovision). The change then stays InProgress in the Machine
	// status.
	MigrateIsolation(ctx context.Context, log *slog.Logger, active *ActiveMachine, desired *provision.UnboundedAgentConfig, strategy migrate.Strategy) error
}

var _ isolationMigrator = (*migrationEnv)(nil)

// MigrateIsolation implements isolationMigrator. The source and target
// backends are chosen from the active machine and the desired configuration.
func (e *migrationEnv) MigrateIsolation(ctx context.Context, log *slog.Logger, active *ActiveMachine, desired *provision.UnboundedAgentConfig, strategy migrate.Strategy) error {
	target, err := desired.IsolationMode()
	if err != nil {
		return fmt.Errorf("cannot change %s to the declared isolation mode: %w", activeModeOf(active), err)
	}

	log.Info("isolation change requested", "from", activeModeOf(active), "to", target, "strategy", strategy)

	switch strategy {
	case migrate.InPlace:
		return e.migrateInPlace(ctx, log, active, desired)
	case migrate.Reprovision:
		return e.migrateReprovision(ctx, log, active, desired)
	default:
		return fmt.Errorf("unsupported isolation change strategy %q", strategy)
	}
}

func outcomeText(err error) string {
	if err == nil {
		return "completed"
	}

	return err.Error()
}

// resolveJournal brings the host to a known state for j and returns the
// change's outcome: nil for success, ErrMigrationHandedOff for a reprovision
// still running without this daemon, any other error for a failure. It may
// move the journal on disk to a later step; the caller reloads it to see
// whether the change is over.
func (e *migrationEnv) resolveJournal(ctx context.Context, log *slog.Logger, j migrate.Journal) error {
	switch j.Step {
	case migrate.StepComplete, migrate.StepBootstrapped:
		return nil
	case migrate.StepRolledBack, migrate.StepFailed:
		return errors.New(j.Message)
	}

	if j.Strategy == migrate.Reprovision {
		return e.resolveReprovision(ctx, log, j)
	}

	return e.resumeInPlace(ctx, log, j)
}

// preflightMigrationTarget checks the target mode's host requirements without
// changing anything, before the first mutation.
func preflightMigrationTarget(ctx context.Context, log *slog.Logger, mode isolation.Mode) error {
	switch mode {
	case isolation.ModeNSpawn:
		return runMigrationPreflight(ctx, mode, []preflight.Checker{host.CheckNSpawnRuntime(log)})
	case isolation.ModeNone:
		// The none backend's ownership check refuses a host that runs a kubelet
		// or containerd it does not own, and the node being migrated is one, so
		// it cannot run here. It runs after the reset, as the first step of the
		// bootstrap in mode none (nonePlan.EnsureHostClean), before anything is
		// installed. Before the reset, the static checks: cgroups, required
		// commands and writable paths.
		if err := (&none.Backend{Log: log, LookPath: exec.LookPath}).PreflightStatic(); err != nil {
			return fmt.Errorf("host cannot run isolation mode none: %w", err)
		}

		return nil
	default:
		return fmt.Errorf("unsupported isolation mode %q", mode)
	}
}

// runMigrationPreflight runs the target mode's checks and returns what stops
// the migration. Bootstrap installs a missing nspawn tool, so the nspawn
// runtime check only warns about one there. A migration installs nothing
// before the target starts, so for nspawn a warning is a failure: failing here
// leaves the node running, while finding the tool missing at start would stop
// the source first and then roll back.
func runMigrationPreflight(ctx context.Context, mode isolation.Mode, checks []preflight.Checker) error {
	failOnWarnings := mode == isolation.ModeNSpawn

	report := preflight.Run(ctx, checks, preflight.Options{})
	if !report.HasErrors() && (!failOnWarnings || !report.HasWarnings()) {
		return nil
	}

	var msgs []string

	for _, r := range report.Checks {
		if r.Severity == preflight.SeverityError || failOnWarnings && r.Severity == preflight.SeverityWarning {
			msgs = append(msgs, r.Name+": "+r.Message)
		}
	}

	return fmt.Errorf("host cannot run isolation mode %s: %s", mode, strings.Join(msgs, "; "))
}

// clearJournal removes the terminal journal j came from, and for a reprovision
// the unit and directory that carried it. It is called after the outcome was
// reported in the Machine status, so a crash before then reports it again.
func (e *migrationEnv) clearJournal(log *slog.Logger, j migrate.Journal) {
	if j.Strategy == migrate.Reprovision {
		e.removeReprovisionFiles(context.Background(), log)

		return
	}

	if err := e.inPlaceJournal().Remove(); err != nil {
		log.Warn("could not remove the isolation change journal", "error", err)
	}
}

// clearTerminalJournals drops every terminal journal after its outcome was
// reported. A journal that is not terminal is left alone: the change it
// records is not over.
func (e *migrationEnv) clearTerminalJournals(log *slog.Logger) {
	for _, store := range e.journals() {
		j, err := store.Load()
		if err == nil && j.Terminal() {
			e.clearJournal(log, j)
		}
	}
}
