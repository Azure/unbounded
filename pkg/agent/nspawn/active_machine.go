// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package nspawn exposes host-side helpers for nspawn-backed agent machines.
package nspawn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// ActiveMachine holds the currently active nspawn machine name and its
// applied agent configuration.
type ActiveMachine struct {
	Name   string
	Config *config.AgentConfig
}

var (
	// ErrNoActiveMachine indicates neither nspawn machine has an applied
	// config on disk.
	ErrNoActiveMachine = errors.New("no active nspawn machine")

	// ErrMultipleActiveMachines indicates both nspawn machines have applied
	// configs on disk, so the active side is ambiguous.
	ErrMultipleActiveMachines = errors.New("multiple active nspawn machines")
)

// FindActiveMachine returns the currently active nspawn machine and its
// applied agent configuration.
func FindActiveMachine(ctx context.Context, log *slog.Logger) (*ActiveMachine, error) {
	return findActiveMachine(ctx, log, goalstates.AppliedConfigPath, goalstates.AppliedConfigChecksumPath, goalstates.AgentConfigDir)
}

func findActiveMachine(
	ctx context.Context,
	log *slog.Logger,
	appliedConfigPath func(string) string,
	checksumPath func(string) string,
	configDir string,
) (*ActiveMachine, error) {
	if log == nil {
		log = slog.Default()
	}

	var active []*ActiveMachine
	for _, name := range []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		path := appliedConfigPath(name)

		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("read applied config %s: %w", path, err)
		}

		checksum := checksumPath(name)
		if err := goalstates.VerifyChecksum(data, checksum); err != nil {
			return nil, fmt.Errorf("verify applied config checksum for %s: %w", name, err)
		}

		if _, statErr := os.Stat(checksum); errors.Is(statErr, os.ErrNotExist) {
			log.Warn("no checksum sidecar found, skipping integrity check",
				"config_path", path,
				"checksum_path", checksum,
			)
		}

		var cfg config.AgentConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("decode applied config %s: %w", path, err)
		}

		active = append(active, &ActiveMachine{Name: name, Config: &cfg})
	}

	switch len(active) {
	case 0:
		return nil, fmt.Errorf("%w in %s", ErrNoActiveMachine, configDir)
	case 1:
		return active[0], nil
	default:
		names := make([]string, 0, len(active))
		for _, machine := range active {
			names = append(names, machine.Name)
		}

		return nil, fmt.Errorf("%w: %s", ErrMultipleActiveMachines, strings.Join(names, ", "))
	}
}
