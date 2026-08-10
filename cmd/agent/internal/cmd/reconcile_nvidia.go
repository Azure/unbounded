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

type resolveNVIDIASetupFunc func(*provision.AgentConfig, string) (*goalstates.NodeStart, error)

type waitForNVIDIAMachineFunc func(context.Context, *slog.Logger, string) error

type executeNVIDIATaskFunc func(context.Context, *slog.Logger, phases.Task) error

func runNVIDIAReconciliation(ctx context.Context, log *slog.Logger, machine string) error {
	return reconcileNVIDIA(ctx, log, machine,
		goalstates.AppliedConfigPath(machine),
		goalstates.AppliedConfigChecksumPath(machine),
		goalstates.ResolveNVIDIASetup,
		nodestart.WaitForMachine,
		phases.ExecuteTask,
	)
}

func reconcileNVIDIA(
	ctx context.Context,
	log *slog.Logger,
	machine string,
	configPath string,
	checksumPath string,
	resolve resolveNVIDIASetupFunc,
	waitForMachine waitForNVIDIAMachineFunc,
	executeTask executeNVIDIATaskFunc,
) error {
	data, err := os.ReadFile(configPath)
	if errors.Is(err, os.ErrNotExist) {
		log.Info("applied config not available; managed node start will reconcile NVIDIA state",
			"machine", machine)

		return nil
	}

	if err != nil {
		return fmt.Errorf("read applied config %s: %w", configPath, err)
	}

	if err := goalstates.VerifyChecksum(data, checksumPath); err != nil {
		return fmt.Errorf("verify applied config checksum for %s: %w", machine, err)
	}

	var cfg provision.AgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("decode applied config %s: %w", configPath, err)
	}

	nodeStart, err := resolve(&cfg, machine)
	if err != nil {
		return fmt.Errorf("resolve NVIDIA setup goal state: %w", err)
	}

	if !nodeStart.Containerd.NvidiaRuntime.Enabled || len(nodeStart.Nvidia.LibMappings) == 0 {
		return fmt.Errorf("NVIDIA setup state is unavailable for machine %s", machine)
	}

	if err := waitForMachine(ctx, log, machine); err != nil {
		return fmt.Errorf("wait for machine %s: %w", machine, err)
	}

	return executeTask(ctx, log, nodestart.SetupNVIDIA(log, nodeStart))
}
