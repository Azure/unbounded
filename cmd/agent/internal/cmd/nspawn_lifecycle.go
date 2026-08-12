// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

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
	lifecycle, err := newNSpawnLifecycle(log)
	if err != nil {
		return err
	}

	return lifecycle.PreStart(ctx, machineName)
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
