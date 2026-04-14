// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package nodeupdate implements blue-green nspawn machine updates for the
// unbounded-agent daemon. When a NodeUpdateSpec task arrives with different
// configuration from the currently applied config, this package orchestrates:
//
//  1. Provisioning a new nspawn machine (the alternate of the current one)
//  2. Stopping the old machine
//  3. Starting the new machine and verifying kubelet is running
//  4. Removing the old machine
//  5. Persisting the new applied config
package nodeupdate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/nodestart"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/reset"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases/rootfs"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilexec"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilio"
	"github.com/Azure/unbounded-kube/internal/provision"

	agentv1 "github.com/Azure/unbounded-kube/agent/api/v1"
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

// HasDrift reports whether the NodeUpdateSpec differs from the applied config
// in any field that would require a machine update. Only non-empty fields in
// the spec are compared.
func HasDrift(applied *provision.AgentConfig, spec *agentv1.NodeUpdateSpec) bool {
	if spec.GetKubernetesVersion() != "" {
		appliedVersion := strings.TrimPrefix(applied.Cluster.Version, "v")
		specVersion := strings.TrimPrefix(spec.GetKubernetesVersion(), "v")
		if appliedVersion != specVersion {
			return true
		}
	}

	if spec.GetApiServer() != "" && spec.GetApiServer() != applied.Kubelet.ApiServer {
		return true
	}

	if spec.GetCaCertBase64() != "" && spec.GetCaCertBase64() != applied.Cluster.CaCertBase64 {
		return true
	}

	if spec.GetClusterDns() != "" && spec.GetClusterDns() != applied.Cluster.ClusterDNS {
		return true
	}

	if spec.GetBootstrapToken() != "" && spec.GetBootstrapToken() != applied.Kubelet.BootstrapToken {
		return true
	}

	if spec.GetOciImage() != "" && spec.GetOciImage() != applied.OCIImage {
		return true
	}

	return false
}

// MergeSpec creates a new AgentConfig by applying non-empty fields from the
// NodeUpdateSpec onto the existing config. Fields not present in the spec
// are preserved from the original config.
func MergeSpec(base *provision.AgentConfig, spec *agentv1.NodeUpdateSpec) *provision.AgentConfig {
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
	if base.TaskServer != nil {
		ts := *base.TaskServer
		merged.TaskServer = &ts
	}

	// Apply spec fields.
	if v := spec.GetKubernetesVersion(); v != "" {
		merged.Cluster.Version = strings.TrimPrefix(v, "v")
	}

	if v := spec.GetApiServer(); v != "" {
		merged.Kubelet.ApiServer = v
	}

	if v := spec.GetCaCertBase64(); v != "" {
		merged.Cluster.CaCertBase64 = v
	}

	if v := spec.GetClusterDns(); v != "" {
		merged.Cluster.ClusterDNS = v
	}

	if v := spec.GetBootstrapToken(); v != "" {
		merged.Kubelet.BootstrapToken = v
	}

	if v := spec.GetOciImage(); v != "" {
		merged.OCIImage = v
	}

	if labels := spec.GetNodeLabels(); len(labels) > 0 {
		merged.Kubelet.Labels = make(map[string]string, len(labels))
		for k, v := range labels {
			merged.Kubelet.Labels[k] = v
		}
	}

	if taints := spec.GetRegisterWithTaints(); len(taints) > 0 {
		merged.Kubelet.RegisterWithTaints = make([]string, len(taints))
		copy(merged.Kubelet.RegisterWithTaints, taints)
	}

	return &merged
}

// Execute performs the blue-green nspawn machine update:
//  1. Provision a new rootfs on the alternate machine
//  2. Configure containerd and kubelet (pre-boot)
//  3. Stop the old machine
//  4. Start the new machine, verify kubelet is running
//  5. Stop and remove the old machine
//  6. Persist the new applied config, remove the old one
func Execute(ctx context.Context, log *slog.Logger, active *ActiveMachine, newCfg *provision.AgentConfig) error {
	oldMachine := active.Name
	newMachine := goalstates.AlternateMachine(oldMachine)

	log.Info("starting blue-green node update",
		"old_machine", oldMachine,
		"new_machine", newMachine,
		"old_version", active.Config.Cluster.Version,
		"new_version", newCfg.Cluster.Version,
	)

	// Resolve goal states for the new machine.
	rootFSGoalState, err := resolveRootFSGoalState(log, newCfg, newMachine)
	if err != nil {
		return fmt.Errorf("resolve rootfs goal state: %w", err)
	}

	nodeStartGoalState, err := resolveNodeStartGoalState(newCfg, newMachine, rootFSGoalState.Nvidia)
	if err != nil {
		return fmt.Errorf("resolve nodestart goal state: %w", err)
	}

	// Step 1: Provision the new machine rootfs.
	log.Info("provisioning new machine rootfs", "machine", newMachine)
	provisionTasks := phases.Serial(log,
		rootfs.EnsureNSpawnWorkspace(log, rootFSGoalState),
		phases.Parallel(log,
			rootfs.DownloadKubeBinaries(log, rootFSGoalState),
			rootfs.DownloadCRIBinaries(log, rootFSGoalState),
			rootfs.DownloadCNIBinaries(log, rootFSGoalState),
			rootfs.ConfigureOS(rootFSGoalState),
			rootfs.DisableResolved(rootFSGoalState),
		),
	)
	if err := provisionTasks.Do(ctx); err != nil {
		return fmt.Errorf("provision new machine %s: %w", newMachine, err)
	}

	// Step 2: Configure containerd and kubelet (pre-boot).
	log.Info("configuring services for new machine", "machine", newMachine)
	configureTasks := phases.Parallel(log,
		nodestart.ConfigureContainerd(nodeStartGoalState),
		nodestart.ConfigureKubelet(nodeStartGoalState),
	)
	if err := configureTasks.Do(ctx); err != nil {
		return fmt.Errorf("configure services for %s: %w", newMachine, err)
	}

	// Step 3: Stop the old machine.
	log.Info("stopping old machine", "machine", oldMachine)
	if err := reset.StopMachine(log, oldMachine).Do(ctx); err != nil {
		return fmt.Errorf("stop old machine %s: %w", oldMachine, err)
	}

	// Step 4: Start the new machine and verify health.
	log.Info("starting new machine", "machine", newMachine)
	startTasks := phases.Serial(log,
		nodestart.StartNSpawnMachine(log, nodeStartGoalState),
		nodestart.SetupNVIDIA(log, nodeStartGoalState),
		nodestart.StartContainerd(log, nodeStartGoalState),
		nodestart.StartKubelet(log, nodeStartGoalState),
	)
	if err := startTasks.Do(ctx); err != nil {
		return fmt.Errorf("start new machine %s: %w", newMachine, err)
	}

	// Health check: verify kubelet is active inside the new machine.
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

	log.Info("blue-green node update completed",
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

// resolveRootFSGoalState builds the rootfs goal state for the given machine.
func resolveRootFSGoalState(log *slog.Logger, cfg *provision.AgentConfig, machineName string) (*goalstates.RootFS, error) {
	kubeVersion := cfg.Cluster.Version

	kernel, err := utilio.HostKernel()
	if err != nil {
		return nil, err
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("get host hostname: %w", err)
	}

	nvidia, err := goalstates.ResolveNvidiaHost(runtime.GOARCH)
	if err != nil {
		return nil, err
	}

	ociImage := cfg.OCIImage
	if ociImage == "" {
		if len(nvidia.GPUDevicePaths) > 0 {
			ociImage = goalstates.DefaultNvidiaOCImage
		} else {
			ociImage = goalstates.DefaultOCIImage
		}
		log.Info("no OCI image configured, using default", "image", ociImage)
	}

	return &goalstates.RootFS{
		MachineDir: filepath.Join("/var/lib/machines", machineName),
		NSpawnConfigFile: filepath.Join(
			goalstates.SystemdNSpawnDir,
			machineName+".nspawn",
		),
		ServiceOverrideFile: filepath.Join(
			goalstates.SystemdSystemDir,
			fmt.Sprintf("systemd-nspawn@%s.service.d", machineName),
			"override.conf",
		),
		HostArch:          runtime.GOARCH,
		HostKernel:        kernel,
		Hostname:          hostname,
		ContainerdVersion: goalstates.ContainerdVersion,
		RunCVersion:       goalstates.RunCVersion,
		CNIPluginVersion:  goalstates.CNIPluginVersion,
		KubernetesVersion: kubeVersion,
		OCIImage:          ociImage,
		Nvidia:            nvidia,
		HostDevicePaths:   goalstates.DiscoverHostDevicePaths(),
	}, nil
}

// resolveNodeStartGoalState builds the nodestart goal state for the given machine.
func resolveNodeStartGoalState(cfg *provision.AgentConfig, machineName string, nvidia goalstates.NvidiaHost) (*goalstates.NodeStart, error) {
	kubelet, err := resolveKubeletGoalState(cfg)
	if err != nil {
		return nil, err
	}

	return &goalstates.NodeStart{
		MachineName: machineName,
		MachineDir:  filepath.Join("/var/lib/machines", machineName),
		Containerd:  goalstates.ResolveContainerd(),
		Kubelet:     kubelet,
		Nvidia:      nvidia,
	}, nil
}

// resolveKubeletGoalState builds the kubelet goal state from agent config.
func resolveKubeletGoalState(cfg *provision.AgentConfig) (goalstates.Kubelet, error) {
	var zero goalstates.Kubelet

	caCert, err := base64.StdEncoding.DecodeString(cfg.Cluster.CaCertBase64)
	if err != nil {
		return zero, fmt.Errorf("decode CaCertBase64: %w", err)
	}

	labels := cfg.Kubelet.Labels
	if labels == nil {
		labels = make(map[string]string)
	}

	return goalstates.Kubelet{
		KubeletBinPath:     filepath.Join("/"+goalstates.BinDir, "kubelet"),
		BootstrapToken:     cfg.Kubelet.BootstrapToken,
		APIServer:          cfg.Kubelet.ApiServer,
		CACertData:         caCert,
		ClusterDNS:         cfg.Cluster.ClusterDNS,
		NodeLabels:         labels,
		RegisterWithTaints: cfg.Kubelet.RegisterWithTaints,
	}, nil
}

// machineRun executes a command inside the named nspawn machine using
// systemd-run --machine=<machine> --pipe --wait.
func machineRun(ctx context.Context, log *slog.Logger, machine string, args ...string) (string, error) {
	runArgs := make([]string, 0, 3+len(args))
	runArgs = append(runArgs, "--machine="+machine, "--pipe", "--wait")
	runArgs = append(runArgs, args...)

	return utilexec.OutputCmd(ctx, log, "systemd-run", runArgs...)
}

// waitForKubelet polls the kubelet systemd service inside the nspawn machine
// until it reports as active. This confirms the kubelet started successfully
// after a blue-green update.
func waitForKubelet(ctx context.Context, log *slog.Logger, machine string) error {
	const (
		pollInterval = 2 * time.Second
		timeout      = 60 * time.Second
	)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		out, err := machineRun(ctx, log, machine,
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
