// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package nodeupdate implements nspawn machine updates for the unbounded-agent.
// When a NodeUpdateSpec arrives with different configuration from the currently
// applied config, this package orchestrates:
//
//  1. Provisioning a new nspawn machine (the alternate of the current one)
//  2. Stopping the old machine
//  3. Starting the new machine and verifying kubelet is running
//  4. Removing the old machine
//  5. Persisting the new applied config
package nodeupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/nodestart"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/nodestop"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/reset"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/rootfs"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilexec"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilio"
	"github.com/Azure/unbounded-kube/internal/provision"
)

// ActiveMachine holds the currently active nspawn machine name and its
// applied agent configuration.
type ActiveMachine struct {
	Name   string
	Config *provision.AgentConfig
}

// FindActiveMachine scans the agent config directory for an applied config
// file and returns the active machine name and config. Returns an error if
// no applied config is found.
func FindActiveMachine() (*ActiveMachine, error) {
	for _, name := range []string{goalstates.NSpawnMachineKube1, goalstates.NSpawnMachineKube2} {
		path := goalstates.AppliedConfigPath(name)

		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read applied config %s: %w", path, err)
		}

		var cfg provision.AgentConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("decode applied config %s: %w", path, err)
		}

		return &ActiveMachine{Name: name, Config: &cfg}, nil
	}

	return nil, fmt.Errorf("no applied config found in %s", goalstates.AgentConfigDir)
}

// NodeUpdateSpec describes the desired node configuration. Non-empty fields
// are applied on top of the current applied config during an update.
type NodeUpdateSpec struct {
	KubernetesVersion  string
	ApiServer          string
	CaCertBase64       string
	ClusterDNS         string
	BootstrapToken     string
	OciImage           string
	NodeLabels         map[string]string
	RegisterWithTaints []string
}

// HasDrift reports whether the NodeUpdateSpec differs from the applied config
// in any field that would require a machine update. Only non-empty fields in
// the spec are compared.
func HasDrift(applied *provision.AgentConfig, spec *NodeUpdateSpec) bool {
	if spec.KubernetesVersion != "" {
		appliedVersion := strings.TrimPrefix(applied.Cluster.Version, "v")
		specVersion := strings.TrimPrefix(spec.KubernetesVersion, "v")
		if appliedVersion != specVersion {
			return true
		}
	}

	if spec.ApiServer != "" && spec.ApiServer != applied.Kubelet.ApiServer {
		return true
	}

	if spec.CaCertBase64 != "" && spec.CaCertBase64 != applied.Cluster.CaCertBase64 {
		return true
	}

	if spec.ClusterDNS != "" && spec.ClusterDNS != applied.Cluster.ClusterDNS {
		return true
	}

	if spec.BootstrapToken != "" && spec.BootstrapToken != applied.Kubelet.BootstrapToken {
		return true
	}

	if spec.OciImage != "" && spec.OciImage != applied.OCIImage {
		return true
	}

	return false
}

// MergeSpec creates a new AgentConfig by applying non-empty fields from the
// NodeUpdateSpec onto the existing config. Fields not present in the spec
// are preserved from the original config.
func MergeSpec(base *provision.AgentConfig, spec *NodeUpdateSpec) *provision.AgentConfig {
	// Deep copy the base config.
	merged := *base
	merged.Cluster = base.Cluster
	merged.Kubelet = base.Kubelet

	// Copy labels map to avoid aliasing.
	if base.Kubelet.Labels != nil {
		merged.Kubelet.Labels = make(map[string]string, len(base.Kubelet.Labels))
		for k, v := range base.Kubelet.Labels {
			merged.Kubelet.Labels[k] = v
		}
	}

	// Copy taints slice.
	if base.Kubelet.RegisterWithTaints != nil {
		merged.Kubelet.RegisterWithTaints = make([]string, len(base.Kubelet.RegisterWithTaints))
		copy(merged.Kubelet.RegisterWithTaints, base.Kubelet.RegisterWithTaints)
	}

	// Preserve pointers.
	if base.Attest != nil {
		a := *base.Attest
		merged.Attest = &a
	}

	// Apply spec fields.
	if v := spec.KubernetesVersion; v != "" {
		merged.Cluster.Version = strings.TrimPrefix(v, "v")
	}

	if v := spec.ApiServer; v != "" {
		merged.Kubelet.ApiServer = v
	}

	if v := spec.CaCertBase64; v != "" {
		merged.Cluster.CaCertBase64 = v
	}

	if v := spec.ClusterDNS; v != "" {
		merged.Cluster.ClusterDNS = v
	}

	if v := spec.BootstrapToken; v != "" {
		merged.Kubelet.BootstrapToken = v
	}

	if v := spec.OciImage; v != "" {
		merged.OCIImage = v
	}

	if labels := spec.NodeLabels; len(labels) > 0 {
		merged.Kubelet.Labels = make(map[string]string, len(labels))
		for k, v := range labels {
			merged.Kubelet.Labels[k] = v
		}
	}

	if taints := spec.RegisterWithTaints; len(taints) > 0 {
		merged.Kubelet.RegisterWithTaints = make([]string, len(taints))
		copy(merged.Kubelet.RegisterWithTaints, taints)
	}

	return &merged
}

// Execute performs the nspawn machine update:
//  1. Provision a new rootfs on the alternate machine
//  2. Stop the old machine (graceful service shutdown + nspawn teardown)
//  3. Start the new machine (configure, boot nspawn, start services)
//  4. Verify kubelet health
//  5. Remove the old machine
//  6. Persist the new applied config, remove the old one
func Execute(ctx context.Context, log *slog.Logger, active *ActiveMachine, newCfg *provision.AgentConfig) error {
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
	rootFSGoalState := gs.RootFS
	nodeStartGoalState := gs.NodeStart

	// Step 1: Provision the new machine rootfs.
	log.Info("provisioning new machine rootfs", "machine", newMachine)
	if err := rootfs.Provision(log, rootFSGoalState).Do(ctx); err != nil {
		return fmt.Errorf("provision new machine %s: %w", newMachine, err)
	}

	// Step 2: Stop the old machine.
	log.Info("stopping old machine", "machine", oldMachine)
	if err := nodestop.StopNode(log, oldMachine).Do(ctx); err != nil {
		return fmt.Errorf("stop old machine %s: %w", oldMachine, err)
	}

	// Step 3: Start the new machine and verify health.
	log.Info("starting new machine", "machine", newMachine)
	if err := nodestart.StartNode(log, nodeStartGoalState).Do(ctx); err != nil {
		return fmt.Errorf("start new machine %s: %w", newMachine, err)
	}

	// Step 4: Verify kubelet health.
	log.Info("verifying kubelet health", "machine", newMachine)
	if err := waitForKubelet(ctx, log, newMachine); err != nil {
		return fmt.Errorf("kubelet health check on %s: %w", newMachine, err)
	}

	// Step 5: Remove the old machine.
	log.Info("removing old machine", "machine", oldMachine)
	removeTasks := phases.Serial(log,
		reset.RemoveNSpawnConfig(log, oldMachine),
		reset.RemoveMachine(log, oldMachine),
	)
	if err := removeTasks.Do(ctx); err != nil {
		// Log but don't fail - the new machine is already running.
		log.Warn("failed to fully remove old machine (non-fatal)",
			"machine", oldMachine, "error", err)
	}

	// Step 6: Persist the new applied config and remove the old one.
	if err := persistAppliedConfig(newCfg, newMachine); err != nil {
		return fmt.Errorf("persist applied config for %s: %w", newMachine, err)
	}

	oldConfigPath := goalstates.AppliedConfigPath(oldMachine)
	if err := os.Remove(oldConfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Warn("failed to remove old applied config (non-fatal)",
			"path", oldConfigPath, "error", err)
	}

	log.Info("node update completed",
		"active_machine", newMachine,
		"version", newCfg.Cluster.Version,
	)

	return nil
}

// persistAppliedConfig writes the agent config to the applied config file.
func persistAppliedConfig(cfg *provision.AgentConfig, machineName string) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal applied config: %w", err)
	}

	path := goalstates.AppliedConfigPath(machineName)
	if err := utilio.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write applied config to %s: %w", path, err)
	}

	return nil
}

// waitForKubelet polls the kubelet systemd service inside the nspawn machine
// until it reports as active. This confirms the kubelet started successfully
// after a machine update.
func waitForKubelet(ctx context.Context, log *slog.Logger, machine string) error {
	const (
		pollInterval = 2 * time.Second
		timeout      = 60 * time.Second
	)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		out, err := utilexec.MachineRun(ctx, log, machine,
			"systemctl", "is-active", goalstates.SystemdUnitKubelet,
		)
		if err == nil && strings.TrimSpace(out) == "active" {
			log.Info("kubelet is active", "machine", machine)
			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("kubelet not active in %s after %s: %w", machine, timeout, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
