// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"

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

func runNSpawnLifecycleReconcile(ctx context.Context, log *slog.Logger, machineName string) error {
	return phases.ExecuteTask(ctx, log, nodestart.ReconcileNSpawnLifecycle(log, machineName))
}

type (
	waitForLifecycleMachineFunc func(context.Context, *slog.Logger, string) error
	reconcileNVIDIATaskFunc     func(*slog.Logger, *goalstates.NodeStart) phases.Task
	resolveNVIDIAHostFunc       func(string) (goalstates.NvidiaHost, error)
)

func runNSpawnLifecyclePostStart(ctx context.Context, log *slog.Logger, machineName string) error {
	return nspawnLifecyclePostStart(
		ctx,
		log,
		machineName,
		goalstates.ResolveNvidiaHost,
		nodestart.WaitForMachine,
		nodestart.ReconcileNVIDIA,
		phases.ExecuteTask,
	)
}

func nspawnLifecyclePostStart(
	ctx context.Context,
	log *slog.Logger,
	machineName string,
	resolveNVIDIA resolveNVIDIAHostFunc,
	waitForMachine waitForLifecycleMachineFunc,
	reconcileNVIDIA reconcileNVIDIATaskFunc,
	executeTask executeLifecycleTaskFunc,
) error {
	nvidia, err := resolveNVIDIA(runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("resolve NVIDIA host state: %w", err)
	}

	if len(nvidia.GPUDevicePaths) == 0 {
		log.Info("no NVIDIA devices discovered; skipping post-start rewiring", "machine", machineName)

		return nil
	}

	if !goalstates.NVIDIAStateAvailable(nvidia) {
		return fmt.Errorf("%w for machine %s", goalstates.ErrNVIDIAStateUnavailable, machineName)
	}

	nvidia.Required = true

	if err := waitForMachine(ctx, log, machineName); err != nil {
		return fmt.Errorf("wait for machine %s: %w", machineName, err)
	}

	containerd := goalstates.ResolveContainerd(goalstates.ContainerdOptions{NvidiaRequired: true})
	nodeStart := &goalstates.NodeStart{
		MachineName: machineName,
		MachineDir:  "/var/lib/machines/" + machineName,
		Containerd:  containerd,
		Nvidia:      nvidia,
	}

	if err := executeTask(ctx, log, reconcileNVIDIA(log, nodeStart)); err != nil {
		return fmt.Errorf("reconcile NVIDIA state for %s: %w", machineName, err)
	}

	return nil
}
