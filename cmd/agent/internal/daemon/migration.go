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

// migrateIsolation is MigrateIsolation for every source mode. The source and
// target backends are chosen from the active machine and the desired
// configuration, not from the operator that dispatched here. The caller has
// planned the strategy with migrate.Plan, settled any earlier change with
// settleIsolationChanges, and holds installation ownership.
func migrateIsolation(ctx context.Context, log *slog.Logger, active *ActiveMachine, desired *provision.UnboundedAgentConfig, strategy migrate.Strategy) error {
	target, err := desired.IsolationMode()
	if err != nil {
		return fmt.Errorf("cannot change %s to the declared isolation mode: %w", active.Mode, err)
	}

	// The target backend must exist in this agent. Failing here leaves the node
	// untouched.
	if err := isolation.Registered(target); err != nil {
		return fmt.Errorf("cannot change %s to %s: %w", active.Mode, target, err)
	}

	log.Info("isolation change requested", "from", active.Mode, "to", target, "strategy", strategy)

	switch strategy {
	case migrate.InPlace:
		return migrateInPlace(ctx, log, active, desired)
	case migrate.Reprovision:
		return migrateReprovision(ctx, log, active, desired)
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
// change's outcome: nil for success.
func resolveJournal(ctx context.Context, log *slog.Logger, j migrate.Journal) error {
	switch j.Step {
	case migrate.StepComplete, migrate.StepBootstrapped:
		return nil
	case migrate.StepRolledBack, migrate.StepFailed:
		return errors.New(j.Message)
	}

	if j.Strategy == migrate.Reprovision {
		return resolveReprovision(ctx, log, j)
	}

	return resumeInPlace(ctx, log, j)
}

// preflightMigrationTarget checks the target mode's host requirements without
// changing anything, before the first mutation.
var preflightMigrationTarget = func(ctx context.Context, log *slog.Logger, mode isolation.Mode) error {
	var checks []preflight.Checker

	switch mode {
	case isolation.ModeNSpawn:
		checks = append(checks, host.CheckNSpawnRuntime(log))
	case isolation.ModeContainer:
		checks = append(checks, host.CheckContainerBackend(log))
	case isolation.ModeNone:
		// The none backend's ownership check refuses a host that runs a kubelet
		// or containerd it does not own, and the node being migrated is one.
		// Bootstrap runs it after the reset, on the clean host. Before it, the
		// static checks: cgroups, required commands and writable paths.
		if err := (&none.Backend{Log: log, LookPath: exec.LookPath}).PreflightStatic(); err != nil {
			return fmt.Errorf("host cannot run isolation mode none: %w", err)
		}

		return nil
	default:
		return fmt.Errorf("unsupported isolation mode %q", mode)
	}

	return runMigrationPreflight(ctx, mode, checks)
}

// runMigrationPreflight runs the target mode's checks and returns what stops
// the migration. Bootstrap installs a missing nspawn tool, so the nspawn
// runtime check only warns about one there. A migration installs nothing, so
// for nspawn a warning is a failure: failing here leaves the node running,
// while finding the tool missing at start would stop the source first and then
// roll back.
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

// clearJournals removes the terminal journal j came from, and for a reprovision
// the unit and directory that carried it. It is called after the outcome was
// reported in the Machine status, so a crash before then reports it again.
func clearJournals(log *slog.Logger, j migrate.Journal) {
	if j.Strategy == migrate.Reprovision {
		removeReprovisionFiles(log)

		return
	}

	if err := inPlaceJournalStore().Remove(); err != nil {
		log.Warn("could not remove the isolation change journal", "error", err)
	}
}

// clearReportedJournals drops any terminal journal after the outcome was reported.
func clearReportedJournals(log *slog.Logger) {
	for _, store := range []migrate.JournalStore{inPlaceJournalStore(), reprovisionJournalFor()} {
		j, err := store.Load()
		if err == nil && j.Terminal() {
			clearJournals(log, j)
		}
	}
}
