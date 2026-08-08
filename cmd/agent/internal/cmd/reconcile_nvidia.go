// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
)

var reconcileNVIDIAOnMachineStart = runNVIDIAReconciliation

func newCmdReconcileNVIDIA(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "reconcile-nvidia MACHINE",
		Short:  "Reconcile NVIDIA state after an nspawn machine starts",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			machine := args[0]
			if machine != goalstates.NSpawnMachineKube1 && machine != goalstates.NSpawnMachineKube2 {
				return fmt.Errorf("unknown nspawn machine %q", machine)
			}

			cmdCtx.Setup()

			return reconcileNVIDIAOnMachineStart(cmd.Context(), cmdCtx.Logger, machine)
		},
	}

	return cmd
}

func runNVIDIAReconciliation(ctx context.Context, log *slog.Logger, machine string) error {
	path := goalstates.AppliedConfigPath(machine)

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		log.Info("applied config not available; managed node start will reconcile NVIDIA state",
			"machine", machine)

		return nil
	}

	if err != nil {
		return fmt.Errorf("read applied config %s: %w", path, err)
	}

	if err := goalstates.VerifyChecksum(data, goalstates.AppliedConfigChecksumPath(machine)); err != nil {
		return fmt.Errorf("verify applied config checksum for %s: %w", machine, err)
	}

	var cfg provision.AgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("decode applied config %s: %w", path, err)
	}

	goalState, err := goalstates.ResolveMachine(log, &cfg, machine, nil)
	if err != nil {
		return fmt.Errorf("resolve machine goal state: %w", err)
	}

	if !goalState.NodeStart.Containerd.NvidiaRuntime.Enabled || len(goalState.NodeStart.Nvidia.LibMappings) == 0 {
		return fmt.Errorf("NVIDIA setup state is unavailable for machine %s", machine)
	}

	if err := nodestart.WaitForMachine(ctx, log, machine); err != nil {
		return fmt.Errorf("wait for machine %s: %w", machine, err)
	}

	return phases.ExecuteTask(ctx, log, nodestart.SetupNVIDIA(log, goalState.NodeStart))
}
