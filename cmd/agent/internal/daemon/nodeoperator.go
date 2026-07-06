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
	"reflect"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestart"
	"github.com/Azure/unbounded/pkg/agent/phases/nodestop"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs"
)

// ActiveMachine holds the currently active nspawn machine name and its
// applied agent configuration.
type ActiveMachine struct {
	Name   string
	Config *provision.AgentConfig
}

// nodeOperator performs host-local nspawn node operations for the daemon.
// Reconcile code depends on this interface so tests can substitute the
// host-mutating implementation with a fake.
type nodeOperator interface {
	// FindActiveMachine returns the currently active nspawn machine and its
	// applied agent configuration.
	FindActiveMachine(*slog.Logger) (*ActiveMachine, error)
	// RestartNode restarts the provided active nspawn-backed node in place.
	RestartNode(context.Context, *slog.Logger, *ActiveMachine) error
	// ResetAgentResources removes the unbounded-agent and associated resources
	// without stopping the currently running daemon process.
	ResetAgentResources(context.Context, *slog.Logger) error
	// StopDaemon stops, disables, and removes the unbounded-agent-daemon unit.
	StopDaemon(context.Context, *slog.Logger) error
	// RepaveNode performs the nspawn machine update:
	//  1. Provision a new rootfs on the alternate machine
	//  2. Stop the old machine (graceful service shutdown + nspawn teardown)
	//  3. Start the new machine (configure, boot nspawn, start services, persist config)
	//  4. Verify kubelet health
	//  5. Remove the old machine and its applied config
	//
	// The caller is expected to trigger this only after the Kubernetes Node has
	// been cordoned, drained, and deleted. Repave reacts to Node deletion; it does
	// not perform Kubernetes eviction or CNI-specific dataplane cleanup itself.
	RepaveNode(context.Context, *slog.Logger, *ActiveMachine, *provision.UnboundedAgentConfig) error
	// StageAgentUpgrade stages a new host-side agent binary.
	StageAgentUpgrade(context.Context, *slog.Logger, string) error
	// RestartAgentDaemon restarts the host-side agent daemon after an upgrade
	// operation has been recorded as complete.
	RestartAgentDaemon(context.Context, *slog.Logger) error
}

type nspawnNodeOperator struct{}

func (nspawnNodeOperator) FindActiveMachine(log *slog.Logger) (*ActiveMachine, error) {
	// Verify the SHA-256 sidecar before trusting the applied config. A missing
	// sidecar is logged as a warning and not treated as an error.
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

	if applied.Kubelet.Auth.BootstrapToken != desired.Kubelet.Auth.BootstrapToken {
		return true
	}

	if !reflect.DeepEqual(applied.Kubelet.Labels, desired.Kubelet.Labels) {
		return true
	}

	if !reflect.DeepEqual(applied.Kubelet.RegisterWithTaints, desired.Kubelet.RegisterWithTaints) {
		return true
	}

	return false
}

func (nspawnNodeOperator) RestartNode(ctx context.Context, log *slog.Logger, active *ActiveMachine) error {
	_, containerImageArchives, err := goalstates.ResolveDownloadOverridesWithOfflineArtifacts(active.Config, nil)
	if err != nil {
		return fmt.Errorf("resolve download overrides: %w", err)
	}

	gs, err := goalstates.ResolveMachine(log, active.Config, active.Name, nil)
	if err != nil {
		return fmt.Errorf("resolve machine goal state: %w", err)
	}

	log.Info("restarting active node", "machine", active.Name)

	err = phases.Serial(log,
		rootfs.DownloadContainerImageArchives(log, containerImageArchives),
		rootfs.EnsureNSpawnWorkspace(log, gs.RootFS),
		nodestop.StopNode(log, active.Name),
		nodestart.StartNode(log, gs.NodeStart),
		nodestart.WaitForKubelet(log, active.Name),
	).Do(ctx)
	if err != nil {
		return err
	}

	log.Info("node restarted", "machine", active.Name)

	return nil
}

func (nspawnNodeOperator) ResetAgentResources(ctx context.Context, log *slog.Logger) error {
	return ResetAgentResources(log).Do(ctx)
}

func (nspawnNodeOperator) StopDaemon(ctx context.Context, log *slog.Logger) error {
	return StopDaemon(log).Do(ctx)
}

func (nspawnNodeOperator) RepaveNode(
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
	downloads, containerImageArchives, err := provision.ResolveDownloadOverridesWithOfflineArtifacts(newCfg)
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

func (nspawnNodeOperator) StageAgentUpgrade(ctx context.Context, log *slog.Logger, downloadURL string) error {
	return upgradeDaemonBinary(ctx, log, downloadURL)
}

func (nspawnNodeOperator) RestartAgentDaemon(ctx context.Context, log *slog.Logger) error {
	sc := executil.Systemctl()
	if err := executil.RunCmd(ctx, log, sc, "restart", "--no-block", goalstates.DaemonUnit); err != nil {
		return fmt.Errorf("systemctl restart %s: %w", goalstates.DaemonUnit, err)
	}

	return nil
}
