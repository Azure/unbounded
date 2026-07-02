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
	"strconv"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/config"
)

const (
	hostDistroUbuntu2404  = "ubuntu2404"
	hostDistroUbuntu2604  = "ubuntu2604"
	hostDistroAzureLinux3 = "azlinux3"
	hostOSReleasePath     = "/etc/os-release"
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

	if err := config.ValidateAdditionalHostDevices(cfg.AdditionalHostDevices); err != nil {
		return nil, err
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
		HostDevices:       DiscoverHostDevices(cfg.AdditionalHostDevices),
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
//  2. AGENT_DISABLE_OCI_IMAGE env var (truthy value disables OCI, returns "")
//  3. AGENT_OCI_IMAGE env var
//  4. Built-in default selected by host distro and GPU presence
func ResolveOCIImage(log *slog.Logger, configImage string, nvidiaGPUAvailable bool) string {
	return resolveOCIImage(log, configImage, nvidiaGPUAvailable, detectHostDistro())
}

func resolveOCIImage(log *slog.Logger, configImage string, nvidiaGPUAvailable bool, hostDistro string) string {
	if configImage != "" {
		return configImage
	}

	if disabled, err := strconv.ParseBool(os.Getenv("AGENT_DISABLE_OCI_IMAGE")); err == nil && disabled {
		log.Info("OCI image usage disabled via AGENT_DISABLE_OCI_IMAGE, falling back to debootstrap")
		return ""
	}

	if v := strings.TrimSpace(os.Getenv("AGENT_OCI_IMAGE")); v != "" {
		return v
	}

	image := defaultOCIImageForHostDistro(hostDistro, nvidiaGPUAvailable)

	log.Info("no OCI image configured, using default", "image", image, "hostDistro", hostDistro)

	return image
}

func defaultOCIImageForHostDistro(hostDistro string, nvidiaGPUAvailable bool) string {
	switch hostDistro {
	case hostDistroUbuntu2604:
		if nvidiaGPUAvailable {
			return DefaultUbuntu2604NvidiaOCIImage
		}

		return DefaultUbuntu2604OCIImage
	case hostDistroAzureLinux3:
		if nvidiaGPUAvailable {
			return DefaultAzureLinux3NvidiaOCIImage
		}

		return DefaultAzureLinux3OCIImage
	default:
		if nvidiaGPUAvailable {
			return DefaultNvidiaOCIImage
		}

		return DefaultOCIImage
	}
}

func detectHostDistro() string {
	hostDistro, err := hostDistroFromOSRelease(hostOSReleasePath)
	if err != nil {
		return ""
	}

	return hostDistro
}

func hostDistroFromOSRelease(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	return hostDistroFromOSReleaseData(data), nil
}

func hostDistroFromOSReleaseData(data []byte) string {
	values := make(map[string]string)

	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		values[key] = normalizeOSReleaseValue(value)
	}

	return hostDistroFromOSReleaseValues(values)
}

func hostDistroFromOSReleaseValues(values map[string]string) string {
	id := normalizeOSReleaseID(values["ID"])
	versionID := values["VERSION_ID"]

	switch id {
	case "ubuntu":
		switch {
		case strings.HasPrefix(versionID, "24.04"):
			return hostDistroUbuntu2404
		case strings.HasPrefix(versionID, "26.04"):
			return hostDistroUbuntu2604
		}
	case "azurelinux", "azlinux":
		if strings.HasPrefix(versionID, "3") {
			return hostDistroAzureLinux3
		}
	}

	if isRPMBasedOSRelease(id, values["ID_LIKE"]) {
		return hostDistroAzureLinux3
	}

	return ""
}

func isRPMBasedOSRelease(id, idLike string) bool {
	if isRPMBasedOSReleaseID(id) {
		return true
	}

	for _, token := range strings.Fields(idLike) {
		if isRPMBasedOSReleaseID(normalizeOSReleaseID(token)) {
			return true
		}
	}

	return false
}

func isRPMBasedOSReleaseID(id string) bool {
	switch id {
	case "almalinux", "amzn", "azurelinux", "centos", "fedora", "ol", "rhel", "rocky", "sles", "suse":
		return true
	default:
		return false
	}
}

func normalizeOSReleaseID(value string) string {
	var b strings.Builder

	for _, r := range strings.ToLower(value) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}

	return b.String()
}

func normalizeOSReleaseValue(value string) string {
	return strings.Trim(strings.TrimSpace(value), `"'`)
}
