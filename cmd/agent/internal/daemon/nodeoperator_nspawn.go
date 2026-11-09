// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// NSpawnOperator is the ModeOperator of the nspawn mode.
type NSpawnOperator struct {
	// NewBackend builds the nspawn backend for a configuration. It is the
	// backend of the mode composition table's nspawn entry.
	NewBackend BackendFactory
}

var _ ModeOperator = NSpawnOperator{}

// NewNSpawnOperator returns the nspawn ModeOperator, whose node restarts run
// through the backend newBackend builds.
func NewNSpawnOperator(newBackend BackendFactory) NSpawnOperator {
	return NSpawnOperator{NewBackend: newBackend}
}

func (NSpawnOperator) EnsureLifecycleMigration(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	rootFS, err := goalstates.ResolveNSpawnConfig(active.Config, active.Name)
	if err != nil {
		return fmt.Errorf("resolve existing machine lifecycle: %w", err)
	}

	if err := phases.Serial(
		log,
		rootfs.EnsureNSpawnLifecycleHelperFor(rootFS),
		rootfs.EnsureNSpawnConfig(log, rootFS),
	).Do(ctx); err != nil {
		return fmt.Errorf("write existing machine lifecycle: %w", err)
	}

	if err := executil.RunCmd(ctx, log, executil.Systemctl(), "daemon-reload"); err != nil {
		return fmt.Errorf("reload host systemd after lifecycle migration: %w", err)
	}

	log.Info("nspawn lifecycle configuration reconciled", "machine", active.Name)

	return nil
}

func (op NSpawnOperator) RestartNode(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	if op.NewBackend == nil {
		return fmt.Errorf("restart node %s: the nspawn operator has no backend", active.Name)
	}

	backend := op.NewBackend(log, active.Config)

	log.Info("restarting active node", "machine", active.Name)

	if err := backend.Restart(ctx, isolation.SlotName(active.Name)); err != nil {
		return err
	}

	log.Info("node restarted", "machine", active.Name)

	return nil
}

// RepaveNode performs the nspawn machine update:
//  1. Provision a new rootfs on the alternate machine
//  2. Stop the old machine (graceful service shutdown + nspawn teardown)
//  3. Start the new machine (configure, boot nspawn, start services, persist config)
//  4. Verify kubelet health
//  5. Remove the old machine and its applied config
func (NSpawnOperator) RepaveNode(
	ctx context.Context,
	log *slog.Logger,
	active *ActiveMachine,
	newCfg *provision.UnboundedAgentConfig,
) error {
	oldMachine := active.Name
	newMachine := goalstates.AlternateMachine(oldMachine)

	log.Info("starting node repave",
		"old_machine", oldMachine,
		"new_machine", newMachine,
		"old_version", active.Config.Cluster.Version,
		"new_version", newCfg.Cluster.Version,
	)

	// Resolve goal states for the new machine.
	downloads, containerImageArchives, err := provision.ResolveDownloadOverridesWithOfflineArtifacts(ctx, newCfg)
	if err != nil {
		return fmt.Errorf("resolve download overrides: %w", err)
	}

	gs, err := goalstates.ResolveMachine(log, &newCfg.AgentConfig, newMachine, downloads)
	if err != nil {
		return fmt.Errorf("resolve machine goal state: %w", err)
	}

	err = phases.Serial(log,
		rootfs.DownloadContainerImageArchives(log, containerImageArchives),
		rootfs.Provision(log, gs.RootFS),
		nodestop.StopNode(log, oldMachine),
		reset.CleanupNetwork(log),
		nodestart.StartNode(log, gs.NodeStart),
		PersistAppliedConfig(log, gs.NodeStart.MachineName, &newCfg.AgentConfig),
		nodestart.WaitForKubelet(log, newMachine),
		reset.CleanupMachine(log, oldMachine),
		RemoveAppliedConfig(log, oldMachine),
	).Do(ctx)
	if err != nil {
		return err
	}

	log.Info("node repave completed",
		"active_machine", newMachine,
		"version", newCfg.Cluster.Version,
	)

	return nil
}
