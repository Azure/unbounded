// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package nodestart

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded/cmd/agent/internal/phases"
	"github.com/Azure/unbounded/cmd/agent/internal/utilio"
	"github.com/Azure/unbounded/internal/provision"
)

type persistAppliedConfig struct {
	log         *slog.Logger
	machineName string
	cfg         *provision.AgentConfig
}

// PersistAppliedConfig returns a task that writes the agent config to the
// applied config file for the nspawn machine. The daemon reads this file on
// startup to detect configuration drift.
func PersistAppliedConfig(log *slog.Logger, machineName string, cfg *provision.AgentConfig) phases.Task {
	return &persistAppliedConfig{log: log, machineName: machineName, cfg: cfg}
}

func (p *persistAppliedConfig) Name() string { return "persist-applied-config" }

func (p *persistAppliedConfig) Do(_ context.Context) error {
	data, err := json.MarshalIndent(p.cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal applied config: %w", err)
	}

	path := goalstates.AppliedConfigPath(p.machineName)
	if err := utilio.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write applied config to %s: %w", path, err)
	}

	// Write a SHA-256 sidecar file next to the config. Both files are
	// written atomically via renameio so each is never half-written. A
	// crash between the two writes leaves a missing sidecar, which the
	// read path treats as a warning (not an error).
	checksumPath := goalstates.AppliedConfigChecksumPath(p.machineName)
	checksum := goalstates.ComputeChecksum(data)
	if err := utilio.WriteFile(checksumPath, []byte(checksum+"\n"), 0o600); err != nil {
		return fmt.Errorf("write checksum to %s: %w", checksumPath, err)
	}

	p.log.Info("applied config persisted", "path", path, "checksum_path", checksumPath)

	return nil
}
