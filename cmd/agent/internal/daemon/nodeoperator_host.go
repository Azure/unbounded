// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// hostOperator implements hostOperations on the real host.
type hostOperator struct{}

var _ hostOperations = hostOperator{}

func (hostOperator) FindActiveMachine(log *slog.Logger) (*ActiveMachine, error) {
	return findActiveMachineIn(log, goalstates.AppliedConfigPath)
}

// findActiveMachineIn searches every applied config location, with pathOf
// mapping a location name to its file. Verify the SHA-256 sidecar before
// trusting the applied config. A missing sidecar is logged as a warning and not
// treated as an error.
func findActiveMachineIn(log *slog.Logger, pathOf func(name string) string) (*ActiveMachine, error) {
	for _, loc := range goalstates.AppliedConfigLocations() {
		path := pathOf(loc.Name)

		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("read applied config %s: %w", path, err)
		}

		// Verify the sidecar checksum before trusting the config data.
		checksumPath := path + ".sha256"
		if err := goalstates.VerifyChecksum(data, checksumPath); err != nil {
			return nil, fmt.Errorf("verify applied config checksum for %s: %w", loc.Name, err)
		}

		// If the sidecar file is missing, log a warning so operators
		// know the integrity check was skipped.
		if _, statErr := os.Stat(checksumPath); errors.Is(statErr, os.ErrNotExist) {
			log.Warn("no checksum sidecar found, skipping integrity check",
				"config_path", path,
				"checksum_path", checksumPath,
			)
		}

		active, err := decodeActiveMachine(log, loc.Slot, path, data)
		if err != nil {
			return nil, err
		}

		// The file name says which mode wrote it. A config that records
		// another mode was not written by an agent, and neither mode's
		// operations can be trusted with it.
		if active.Mode != loc.Mode {
			return nil, fmt.Errorf("applied config %s records isolation mode %q but is stored as the %q config", path, active.Mode, loc.Mode)
		}

		return active, nil
	}

	return nil, fmt.Errorf("no applied config found in %s", goalstates.AgentConfigDir)
}

func (hostOperator) ResetAgentResources(ctx context.Context, log *slog.Logger) error {
	// The MachineOperation holds installation ownership through daemon stop.
	return resetUnderLock(ctx, log, installstate.DefaultStore(), resetResources(log))
}

func (hostOperator) StopDaemon(ctx context.Context, log *slog.Logger) error {
	return StopDaemon(log).Do(ctx)
}

func (hostOperator) BootID() (string, error) {
	return readBootID()
}

func (hostOperator) RebootHost(ctx context.Context, log *slog.Logger) error {
	return daemon.RebootHost(ctx, log)
}

func (hostOperator) StageAgentUpgrade(ctx context.Context, log *slog.Logger, request agentUpgradeRequest) error {
	return upgradeDaemonBinary(ctx, log, request)
}

func (hostOperator) ReconcileHostRoot(ctx context.Context, log *slog.Logger, active *ActiveMachine, lifecycle lifecycleMigrator) (bool, error) {
	return reconcileHostRoot(ctx, log, lifecycle, active)
}

// decodeActiveMachine builds the active machine from the applied config data
// of slot name, read from path, with its isolation mode.
func decodeActiveMachine(log *slog.Logger, name, path string, data []byte) (*ActiveMachine, error) {
	var cfg provision.AgentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode applied config %s: %w", path, err)
	}

	// MachineName must be resolved before the node name because
	// BackfillNodeName falls back to MachineName. The applied config is
	// normally persisted with a name already set, so this is usually a
	// no-op; it covers configs written by older agents or hand-edited.
	source, err := provision.ResolveMachineName(&cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve applied config machine name %s: %w", path, err)
	}

	if source != "config" {
		log.Info("resolved unbounded MachineName", "name", cfg.MachineName, "source", source)
	}

	if err := cfg.BackfillNodeName(); err != nil {
		return nil, fmt.Errorf("backfill applied config node name %s: %w", path, err)
	}

	// The applied config records the mode the machine was built in. A
	// config without one is nspawn. A mode this agent cannot parse fails
	// closed: operating it as nspawn could destroy a node stack the agent
	// does not understand.
	mode, err := cfg.IsolationMode()
	if err != nil {
		return nil, fmt.Errorf("applied config %s: %w", path, err)
	}

	return &ActiveMachine{Name: name, Config: &cfg, Mode: mode}, nil
}
