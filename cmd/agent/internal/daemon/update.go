// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package daemon implements nspawn machine updates for the unbounded-agent.
// When a desired AgentConfig differs from the currently applied config, this
// package orchestrates:
//
//  1. Provisioning a new nspawn machine (the alternate of the current one)
//  2. Stopping the old machine
//  3. Starting the new machine and verifying kubelet is running
//  4. Removing the old machine
//  5. Persisting the new applied config
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Azure/unbounded/agent/goalstates"
	"github.com/Azure/unbounded/agent/phases"
	"github.com/Azure/unbounded/agent/phases/nodestart"
	"github.com/Azure/unbounded/agent/phases/nodestop"
	"github.com/Azure/unbounded/agent/phases/reset"
	"github.com/Azure/unbounded/agent/phases/rootfs"
	"github.com/Azure/unbounded/internal/provision"
)

// ActiveMachine holds the currently active nspawn machine name and its
// applied agent configuration.
type ActiveMachine struct {
	Name   string
	Config *provision.AgentConfig
}

// findActiveMachine scans the agent config directory for an applied config
// file and returns the active machine name and config. Returns an error if
// no applied config is found.
//
// After reading the config JSON, the function verifies the SHA-256 sidecar
// checksum (if present). A missing sidecar is logged as a warning and not
// treated as an error - see goalstates.VerifyChecksum for rationale.
func findActiveMachine(log *slog.Logger) (*ActiveMachine, error) {
	for _, name := range []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2} {
		path := goalstates.AppliedConfigPath(name)

		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("read applied config %s: %w", path, err)
		}

		// Verify the sidecar checksum before trusting the config data.
		checksumPath := goalstates.AppliedConfigChecksumPath(name)
		if err := goalstates.VerifyChecksum(data, checksumPath); err != nil {
			return nil, fmt.Errorf("verify applied config checksum for %s: %w", name, err)
		}

		// If the sidecar file is missing, log a warning so operators
		// know the integrity check was skipped.
		if _, statErr := os.Stat(checksumPath); errors.Is(statErr, os.ErrNotExist) {
			log.Warn("no checksum sidecar found, skipping integrity check",
				"config_path", path,
				"checksum_path", checksumPath,
			)
		}

		var cfg provision.AgentConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("decode applied config %s: %w", path, err)
		}

		return &ActiveMachine{Name: name, Config: &cfg}, nil
	}

	return nil, fmt.Errorf("no applied config found in %s", goalstates.AgentConfigDir)
}

// hasDrift reports whether the desired AgentConfig differs from the applied
// config in any field that would require a machine update.
func hasDrift(applied, desired *provision.AgentConfig) bool {
	appliedVersion := strings.TrimPrefix(applied.Cluster.Version, "v")

	desiredVersion := strings.TrimPrefix(desired.Cluster.Version, "v")
	if appliedVersion != desiredVersion {
		return true
	}

	if applied.OCIImage != desired.OCIImage {
		return true
	}

	if applied.Kubelet.ApiServer != desired.Kubelet.ApiServer {
		return true
	}

	if applied.Cluster.CaCertBase64 != desired.Cluster.CaCertBase64 {
		return true
	}

	if applied.Cluster.ClusterDNS != desired.Cluster.ClusterDNS {
		return true
	}

	if applied.Kubelet.BootstrapToken != desired.Kubelet.BootstrapToken {
		return true
	}

	return false
}

// UpdateNode performs the nspawn machine update:
//  1. Provision a new rootfs on the alternate machine
//  2. Stop the old machine (graceful service shutdown + nspawn teardown)
//  3. Start the new machine (configure, boot nspawn, start services, persist config)
//  4. Verify kubelet health
//  5. Remove the old machine and its applied config
func UpdateNode(ctx context.Context, log *slog.Logger, active *ActiveMachine, newCfg *provision.AgentConfig) error {
	// Skip the update if the desired config matches the applied config.
	if !hasDrift(active.Config, newCfg) {
		log.Info("no config drift detected, skipping node update")
		return nil
	}

	oldMachine := active.Name
	newMachine := goalstates.AlternateMachine(oldMachine)

	log.Info("starting node update",
		"old_machine", oldMachine,
		"new_machine", newMachine,
		"old_version", active.Config.Cluster.Version,
		"new_version", newCfg.Cluster.Version,
	)

	// Resolve goal states for the new machine.
	gs, err := goalstates.ResolveMachine(log, newCfg, newMachine)
	if err != nil {
		return fmt.Errorf("resolve machine goal state: %w", err)
	}

	err = phases.Serial(log,
		rootfs.Provision(log, gs.RootFS),
		nodestop.StopNode(log, oldMachine),
		nodestart.StartNode(log, gs.NodeStart, newCfg),
		nodestart.WaitForKubelet(log, newMachine),
		reset.CleanupMachine(log, oldMachine),
	).Do(ctx)
	if err != nil {
		return err
	}

	log.Info("node update completed",
		"active_machine", newMachine,
		"version", newCfg.Cluster.Version,
	)

	return nil
}
