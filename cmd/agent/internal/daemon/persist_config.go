// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/utilio"
)

type persistAppliedConfig struct {
	log         *slog.Logger
	machineName string
	cfg         *config.AgentConfig
}

// PersistAppliedConfig returns a task that writes the agent config to the
// applied config file for the nspawn machine. The daemon reads this file on
// startup to detect configuration drift.
func PersistAppliedConfig(log *slog.Logger, machineName string, cfg *config.AgentConfig) phases.Task {
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

type removeAppliedConfig struct {
	log         *slog.Logger
	machineName string
}

// RemoveAppliedConfig returns a task that removes the applied agent config
// file and its checksum sidecar for the named machine. Errors are logged but
// do not fail the task.
func RemoveAppliedConfig(log *slog.Logger, machineName string) phases.Task {
	return &removeAppliedConfig{log: log, machineName: machineName}
}

func (t *removeAppliedConfig) Name() string { return "remove-applied-config" }

func (t *removeAppliedConfig) Do(_ context.Context) error {
	path := goalstates.AppliedConfigPath(t.machineName)
	removeFileIfExists(t.log, path)

	checksumPath := goalstates.AppliedConfigChecksumPath(t.machineName)
	removeFileIfExists(t.log, checksumPath)

	return nil
}

// removeFileIfExists removes a file if it exists. Non-ENOENT errors are
// logged at Warn so we have a trace but don't abort the flow.
func removeFileIfExists(log *slog.Logger, path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("failed to remove file", "path", path, "error", err)
	}
}

// removeAllIfExists removes a path and all children if it exists. Errors are
// logged at Warn so we have a trace but don't abort the flow.
func removeAllIfExists(log *slog.Logger, path string) {
	if err := os.RemoveAll(path); err != nil {
		log.Warn("failed to remove directory", "path", path, "error", err)
	}
}
