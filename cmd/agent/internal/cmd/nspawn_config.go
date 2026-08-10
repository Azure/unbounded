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
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

func newCmdRegenerateConfig(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "regenerate-config MACHINE_NAME",
		Short:  "Regenerate host-side configuration for a machine",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			return regenerateConfig(ctx, cmdCtx.Logger, args[0])
		},
	}

	return cmd
}

func newCmdRegenerateNSpawnConfig(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "regenerate-nspawn-config MACHINE_NAME",
		Short:  "Regenerate host-side nspawn configuration for a machine",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			return regenerateNSpawnConfig(ctx, cmdCtx.Logger, args[0])
		},
	}

	return cmd
}

func regenerateConfig(ctx context.Context, log *slog.Logger, machineName string) error {
	return regenerateNSpawnConfig(ctx, log, machineName)
}

func regenerateNSpawnConfig(ctx context.Context, log *slog.Logger, machineName string) error {
	cfg, ok, err := loadAppliedConfigForMachine(log, machineName)
	if err != nil {
		return err
	}

	if !ok {
		log.Info("applied config not found, skipping nspawn config regeneration", "machine", machineName)
		return nil
	}

	rootFS, err := goalstates.ResolveNSpawnConfig(cfg, machineName)
	if err != nil {
		return fmt.Errorf("resolve nspawn config goal state: %w", err)
	}

	if err := phases.ExecuteTask(ctx, log, rootfs.EnsureNSpawnConfig(log, rootFS)); err != nil {
		return fmt.Errorf("regenerate nspawn config for %s: %w", machineName, err)
	}

	// systemd loaded the nspawn service drop-in before starting this required
	// oneshot unit. Reload the manager so the pending nspawn start observes the
	// regenerated service properties, including path-specific DeviceAllow entries.
	if err := executil.RunCmd(ctx, log, executil.Systemctl(), "daemon-reload"); err != nil {
		return fmt.Errorf("reload systemd after regenerating config for %s: %w", machineName, err)
	}

	return nil
}

func loadAppliedConfigForMachine(log *slog.Logger, machineName string) (*provision.AgentConfig, bool, error) {
	if machineName != goalstates.NSpawnMachineKube1 && machineName != goalstates.NSpawnMachineKube2 {
		return nil, false, fmt.Errorf("unsupported nspawn machine %q", machineName)
	}

	return loadAppliedConfig(log, goalstates.AppliedConfigPath(machineName), goalstates.AppliedConfigChecksumPath(machineName))
}

func loadAppliedConfig(log *slog.Logger, path, checksumPath string) (*provision.AgentConfig, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("read applied config %s: %w", path, err)
	}

	if err := goalstates.VerifyChecksum(data, checksumPath); err != nil {
		return nil, false, fmt.Errorf("verify applied config checksum for %s: %w", path, err)
	}

	if _, statErr := os.Stat(checksumPath); errors.Is(statErr, os.ErrNotExist) {
		log.Warn(
			"no checksum sidecar found, skipping integrity check",
			"config_path", path,
			"checksum_path", checksumPath,
		)
	}

	var cfg provision.AgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, false, fmt.Errorf("decode applied config %s: %w", path, err)
	}

	source, err := provision.ResolveMachineName(&cfg)
	if err != nil {
		return nil, false, fmt.Errorf("resolve applied config machine name %s: %w", path, err)
	}

	if source != "config" {
		log.Info("resolved unbounded MachineName", "name", cfg.MachineName, "source", source)
	}

	if err := cfg.BackfillNodeName(); err != nil {
		return nil, false, fmt.Errorf("backfill applied config node name %s: %w", path, err)
	}

	return &cfg, true, nil
}
