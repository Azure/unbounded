// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
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

type (
	waitForLifecycleMachineFunc func(context.Context, *slog.Logger, string) error
	setupNVIDIATaskFunc         func(*slog.Logger, *goalstates.NodeStart) phases.Task
)

func runNSpawnLifecyclePostStart(ctx context.Context, log *slog.Logger, machineName string) error {
	return nspawnLifecyclePostStart(
		ctx,
		log,
		machineName,
		goalstates.NSpawnLifecycleStatePath(machineName),
		nodestart.WaitForMachine,
		nodestart.SetupNVIDIA,
		phases.ExecuteTask,
	)
}

func nspawnLifecyclePostStart(
	ctx context.Context,
	log *slog.Logger,
	machineName, statePath string,
	waitForMachine waitForLifecycleMachineFunc,
	setupNVIDIA setupNVIDIATaskFunc,
	executeTask executeLifecycleTaskFunc,
) error {
	state, err := goalstates.LoadNSpawnLifecycleState(statePath, machineName)
	if err != nil {
		return err
	}

	if !state.NVIDIARequired {
		log.Info("NVIDIA was not provisioned for machine; skipping post-start setup", "machine", machineName)

		return nil
	}

	if err := waitForMachine(ctx, log, machineName); err != nil {
		return fmt.Errorf("wait for machine %s: %w", machineName, err)
	}

	containerd := goalstates.ResolveContainerdForNVIDIACapability("", true)
	nodeStart := &goalstates.NodeStart{
		MachineName:    machineName,
		MachineDir:     "/var/lib/machines/" + machineName,
		Containerd:     containerd,
		NVIDIARequired: true,
		Nvidia:         state.NVIDIA,
	}

	if err := executeTask(ctx, log, setupNVIDIA(log, nodeStart)); err != nil {
		return fmt.Errorf("reconcile NVIDIA state for %s: %w", machineName, err)
	}

	return nil
}
