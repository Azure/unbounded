// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/nspawnlifecycle"
)

func newCmdNSpawnLifecycle(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "nspawn-lifecycle",
		Short:  "Run internal nspawn lifecycle hooks",
		Hidden: true,
	}

	cmd.AddCommand(
		newCmdNSpawnLifecyclePhase(cmdCtx, "pre-start", "Refresh host-side nspawn state before machine start", runNSpawnLifecyclePreStart),
		newCmdNSpawnLifecyclePhase(cmdCtx, "post-start", "Reconcile in-machine state after machine start", runNSpawnLifecyclePostStart),
		newCmdNSpawnLifecyclePhase(cmdCtx, "reconcile", "Restart a machine and run its lifecycle reconciliation", runNSpawnLifecycleReconcile),
	)

	return cmd
}

func newCmdNSpawnLifecyclePhase(
	cmdCtx *CommandContext,
	phase, short string,
	run func(context.Context, *slog.Logger, string) error,
) *cobra.Command {
	return &cobra.Command{
		Use:    phase + " MACHINE",
		Short:  short,
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		// systemd runs these with fixed arguments, so a failure is never a
		// usage problem and the usage block is just noise in the journal.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateNSpawnMachine(args[0]); err != nil {
				return err
			}

			cmdCtx.Setup()

			return run(cmd.Context(), cmdCtx.Logger, args[0])
		},
	}
}

func newNSpawnLifecycle(log *slog.Logger) (*nspawnlifecycle.Lifecycle, error) {
	return nspawnlifecycle.New(log, nspawnlifecycle.Hooks{
		LoadConfig: func(_ context.Context, machineName string) (*config.AgentConfig, bool, error) {
			return loadAppliedConfig(
				log,
				goalstates.AppliedConfigPath(machineName),
				goalstates.AppliedConfigChecksumPath(machineName),
			)
		},
	})
}

func runNSpawnLifecyclePreStart(ctx context.Context, log *slog.Logger, machineName string) error {
	loadApplied := func(machineName string) (*config.AgentConfig, bool, error) {
		return loadAppliedConfig(
			log,
			goalstates.AppliedConfigPath(machineName),
			goalstates.AppliedConfigChecksumPath(machineName),
		)
	}

	authorize := func() error {
		return authorizeNSpawnStart(ctx, log, machineName, loadApplied, daemon.LoadAllocationLaunchConfig, daemon.VerifyAllocationLaunch)
	}

	return nspawnPreStart(authorize, func() error {
		return preStartAfterMigrate(log, daemon.MigrateHostRoot, func() error {
			lifecycle, err := newNSpawnLifecycle(log)
			if err != nil {
				return err
			}

			return lifecycle.PreStart(ctx, machineName)
		})
	})
}

// nspawnPreStart authorizes the start, then regenerates the machine's config.
//
// A final allocation launch refusal exits with
// goalstates.NSpawnPreStartRefusedExitCode, which the config-regeneration unit
// does not restart: the start fails and the node stays down until the next
// start request, which checks again. Any other failure exits 1, and the unit
// runs the pre-start again, so an outage never strands an unfenced host.
func nspawnPreStart(authorize, regenerate func() error) error {
	if err := authorize(); err != nil {
		if daemon.IsFinalAllocationLaunchRefusal(err) {
			return &exitCodeError{code: goalstates.NSpawnPreStartRefusedExitCode, err: err}
		}

		return err
	}

	return regenerate()
}

// authorizeNSpawnStart runs the allocation launch guard before every start of
// the nspawn machine: at boot, from machinectl start, from systemctl restart
// and from Restart=on-failure. A refusal fails the pre-start, so the machine
// does not start. systemd runs the pre-start again only after a transient
// refusal. A final refusal exits with goalstates.NSpawnPreStartRefusedExitCode
// and is not retried; the next start request checks again.
//
// It checks the slot's applied config when that loads. Otherwise it checks the
// allocation launch record, which bootstrap writes before the node first
// starts: the slot has no applied config between the start and the end of
// bootstrap, and during a repave. A host with neither is not allocation-backed.
// An applied config that fails to load is still reported by the pre-start
// itself.
func authorizeNSpawnStart(
	ctx context.Context,
	log *slog.Logger,
	machineName string,
	loadApplied func(machineName string) (*config.AgentConfig, bool, error),
	loadRecord func() (*config.AgentConfig, bool, error),
	verify func(context.Context, *slog.Logger, *config.AgentConfig) error,
) error {
	cfg, found, err := loadApplied(machineName)
	if err != nil {
		log.Warn("cannot load the applied config for the allocation launch check; checking for the allocation launch record instead",
			"machine", machineName, "error", err)
	}

	if err != nil || !found {
		record, recordFound, recordErr := loadRecord()
		if recordErr != nil {
			log.Error("allocation launch refused", "machine", machineName, "cause", recordErr)

			return fmt.Errorf("%w: %w", daemon.ErrAllocationLaunchRefused, recordErr)
		}

		if !recordFound {
			return nil
		}

		cfg = record
	}

	return verify(ctx, log, cfg)
}

// preStartAfterMigrate regenerates the machine's nspawn config once the host
// root is migrated, since the config names the lifecycle helper under it. It is
// the only hook that writes a path under the host root; the others do not
// migrate it.
//
// If the host root cannot be migrated, the machine starts with the config it
// has rather than not at all. The daemon refuses to start on such a host, which
// is where the problem is reported.
func preStartAfterMigrate(log *slog.Logger, migrate func(*slog.Logger) error, regenerate func() error) error {
	if err := migrate(log); err != nil {
		log.Warn("not regenerating the nspawn config: the host root cannot be migrated; the machine starts with the config it has",
			"error", err)

		return nil
	}

	return regenerate()
}

func runNSpawnLifecyclePostStart(ctx context.Context, log *slog.Logger, machineName string) error {
	lifecycle, err := newNSpawnLifecycle(log)
	if err != nil {
		return err
	}

	return lifecycle.PostStart(ctx, machineName)
}

func runNSpawnLifecycleReconcile(ctx context.Context, log *slog.Logger, machineName string) error {
	lifecycle, err := newNSpawnLifecycle(log)
	if err != nil {
		return err
	}

	return lifecycle.Reconcile(ctx, machineName)
}

func validateNSpawnMachine(machineName string) error {
	if machineName != goalstates.NSpawnMachineKube1 && machineName != goalstates.NSpawnMachineKube2 {
		return fmt.Errorf("unknown nspawn machine %q", machineName)
	}

	return nil
}
