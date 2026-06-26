// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/config"
)

// MachineGoalState holds the fully resolved goal state for provisioning and
// starting an nspawn machine. Callers use RootFS for the rootfs provisioning
// phases and NodeStart for the service configuration and boot phases.
type MachineGoalState struct {
	RootFS    *RootFS
	NodeStart *NodeStart
}

// ResolveMachine probes the host (kernel version, hostname, GPU hardware) and
// resolves the complete goal state for the named nspawn machine from an agent
// config.
func ResolveMachine(log *slog.Logger, cfg *config.AgentConfig, machineName string, downloads *DownloadOverrides) (*MachineGoalState, error) {
	kernel, err := hostKernel()
	if err != nil {
		return nil, fmt.Errorf("get host kernel: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("get host hostname: %w", err)
	}

	nvidia, err := ResolveNvidiaHost(runtime.GOARCH)
	if err != nil {
		return nil, fmt.Errorf("resolve nvidia host: %w", err)
	}

	amd := ResolveAMDHost()

	ociImage := ResolveOCIImage(log, cfg.OCIImage, len(nvidia.GPUDevicePaths) > 0)

	kubelet, err := resolveKubelet(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve kubelet config: %w", err)
	}

	containerdVersion := cfg.CRI.Containerd.Version
	if containerdVersion == "" {
		containerdVersion = ContainerdVersion
	}

	runcVersion := cfg.CRI.Runc.Version
	if runcVersion == "" {
		runcVersion = RunCVersion
	}

	cniVersion := cfg.CNI.PluginVersion
	if cniVersion == "" {
		cniVersion = CNIPluginVersion
	}

	rootFS := &RootFS{
		MachineDir: filepath.Join("/var/lib/machines", machineName),
		NSpawnConfigFile: filepath.Join(
			SystemdNSpawnDir,
			machineName+".nspawn",
		),
		ServiceOverrideFile: filepath.Join(
			SystemdSystemDir,
			fmt.Sprintf("systemd-nspawn@%s.service.d", machineName),
			"override.conf",
		),
		HostArch:          runtime.GOARCH,
		HostKernel:        kernel,
		Hostname:          hostname,
		ContainerdVersion: containerdVersion,
		RunCVersion:       runcVersion,
		CNIPluginVersion:  cniVersion,
		KubernetesVersion: cfg.Cluster.Version,
		Downloads:         downloads,
		OCIImage:          ociImage,
		Nvidia:            nvidia,
		AMD:               amd,
		HostDevices:       DiscoverHostDevices(),
	}

	nodeStart := &NodeStart{
		MachineName:     machineName,
		KubeMachineName: cfg.MachineName,
		NodeName:        cfg.NodeName,
		MachineDir:      filepath.Join("/var/lib/machines", machineName),
		Containerd:      ResolveContainerd(),
		Kubelet:         kubelet,
		Nvidia:          nvidia,
	}

	return &MachineGoalState{
		RootFS:    rootFS,
		NodeStart: nodeStart,
	}, nil
}

// resolveKubelet builds the kubelet goal state from an agent config.
func resolveKubelet(cfg *config.AgentConfig) (Kubelet, error) {
	var zero Kubelet

	caCert, err := base64.StdEncoding.DecodeString(cfg.Cluster.CaCertBase64)
	if err != nil {
		return zero, fmt.Errorf("decode CaCertBase64: %w", err)
	}

	labels := cfg.Kubelet.Labels
	if labels == nil {
		labels = make(map[string]string)
	}

	nodeIP := strings.TrimSpace(cfg.Kubelet.NodeIP)
	if nodeIP != "" {
		for _, value := range strings.Split(nodeIP, ",") {
			if _, err := netip.ParseAddr(strings.TrimSpace(value)); err != nil {
				return zero, fmt.Errorf("invalid Kubelet.NodeIP %q: %w", cfg.Kubelet.NodeIP, err)
			}
		}
	}

	// Skip the "must have one" check when both fields are empty: in the
	// metalman PXE/attestation flow the agent config intentionally ships
	// with an empty Kubelet.Auth and the bootstrap token is filled in
	// later by the apply-attestation phase. The downstream
	// configure-kubelet step ("ensureKubeconfig") fails with
	// "no kubelet auth method configured" if attestation is also absent.
	// Genuine misconfigurations (both fields set, or ExecCredential
	// without a Command) are still caught here.
	if cfg.Kubelet.Auth.BootstrapToken != "" || cfg.Kubelet.Auth.ExecCredential != nil {
		if err := cfg.Kubelet.Auth.Validate(); err != nil {
			return zero, fmt.Errorf("kubelet auth: %w", err)
		}
	}

	return Kubelet{
		KubeletBinPath:     filepath.Join("/"+BinDir, "kubelet"),
		KubeletAuthInfo:    cfg.Kubelet.Auth,
		APIServer:          cfg.Kubelet.ApiServer,
		CACertData:         caCert,
		ClusterDNS:         cfg.Cluster.ClusterDNS,
		NodeIP:             nodeIP,
		NodeLabels:         labels,
		RegisterWithTaints: cfg.Kubelet.RegisterWithTaints,
	}, nil
}

// ResolveOCIImage determines the OCI image to use for the nspawn rootfs.
//
// Priority (highest to lowest):
//  1. configImage from the agent config
//  2. AGENT_OCI_IMAGE env var
//  3. Built-in default selected by GPU presence
func ResolveOCIImage(log *slog.Logger, configImage string, nvidiaGPUAvailable bool) string {
	if configImage != "" {
		return configImage
	}

	if v := strings.TrimSpace(os.Getenv("AGENT_OCI_IMAGE")); v != "" {
		return v
	}

	var image string
	if nvidiaGPUAvailable {
		image = DefaultNvidiaOCImage
	} else {
		image = DefaultOCIImage
	}

	log.Info("no OCI image configured, using default", "image", image)

	return image
}
