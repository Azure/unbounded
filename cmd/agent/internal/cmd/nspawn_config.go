// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

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
