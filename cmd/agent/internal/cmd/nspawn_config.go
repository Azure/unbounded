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

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

type (
	resolveNSpawnConfigFunc  func(*provision.AgentConfig, string) (*goalstates.RootFS, error)
	executeLifecycleTaskFunc func(context.Context, *slog.Logger, phases.Task) error
	reloadSystemdFunc        func(context.Context, *slog.Logger) error
)

func runNSpawnLifecyclePreStart(ctx context.Context, log *slog.Logger, machineName string) error {
	return nspawnLifecyclePreStart(
		ctx,
		log,
		machineName,
		goalstates.AppliedConfigPath(machineName),
		goalstates.AppliedConfigChecksumPath(machineName),
		goalstates.ResolveNSpawnConfig,
		phases.ExecuteTask,
		func(ctx context.Context, log *slog.Logger) error {
			return executil.RunCmd(ctx, log, executil.Systemctl(), "daemon-reload")
		},
	)
}

func nspawnLifecyclePreStart(
	ctx context.Context,
	log *slog.Logger,
	machineName, configPath, checksumPath string,
	resolve resolveNSpawnConfigFunc,
	executeTask executeLifecycleTaskFunc,
	reloadSystemd reloadSystemdFunc,
) error {
	cfg, ok, err := loadAppliedConfig(log, configPath, checksumPath)
	if err != nil {
		return err
	}

	if !ok {
		log.Info("applied config not available during initial bootstrap; keeping freshly provisioned nspawn config", "machine", machineName)

		return nil
	}

	rootFS, err := resolve(cfg, machineName)
	if err != nil {
		return fmt.Errorf("resolve nspawn config goal state: %w", err)
	}

	if err := executeTask(ctx, log, rootfs.EnsureNSpawnConfig(log, rootFS)); err != nil {
		return fmt.Errorf("regenerate nspawn config for %s: %w", machineName, err)
	}

	// The nspawn start transaction loaded its drop-in before this dependency.
	// Reload so the pending start observes refreshed DeviceAllow properties.
	if err := reloadSystemd(ctx, log); err != nil {
		return fmt.Errorf("reload systemd after regenerating config for %s: %w", machineName, err)
	}

	return nil
}

// loadAppliedConfig is the single persisted applied-config loader used by the
// lifecycle path. A missing checksum remains backward compatible, while a
// present checksum must verify.
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
		log.Warn("no checksum sidecar found, skipping integrity check", "config_path", path, "checksum_path", checksumPath)
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

func validateNSpawnMachine(machineName string) error {
	if machineName != goalstates.NSpawnMachineKube1 && machineName != goalstates.NSpawnMachineKube2 {
		return fmt.Errorf("unknown nspawn machine %q", machineName)
	}

	return nil
}
