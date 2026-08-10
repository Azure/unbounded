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
	"slices"
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

type resolveNVIDIAHostFunc func(string) (NvidiaHost, error)

// ResolveNSpawnConfig probes only the host state needed to render the
// systemd-nspawn configuration for a machine. Unlike ResolveMachine, it does
// not resolve network-dependent node services such as LocalDNS.
func ResolveNSpawnConfig(cfg *config.AgentConfig, machineName string) (*RootFS, error) {
	return resolveNSpawnConfig(cfg, machineName, nil, ResolveNvidiaHost)
}

// ResolveNSpawnConfigForNVIDIACapability refreshes nspawn host state without
// allowing discovery to change an existing machine's provisioned capability.
func ResolveNSpawnConfigForNVIDIACapability(cfg *config.AgentConfig, machineName string, nvidiaRequired bool) (*RootFS, error) {
	return resolveNSpawnConfig(cfg, machineName, &nvidiaRequired, ResolveNvidiaHost)
}

func resolveNSpawnConfig(
	cfg *config.AgentConfig,
	machineName string,
	provisionedNVIDIARequired *bool,
	resolveNVIDIA resolveNVIDIAHostFunc,
) (*RootFS, error) {
	if err := config.ValidateAdditionalHostDevices(cfg.AdditionalHostDevices); err != nil {
		return nil, err
	}

	additionalHostMounts, err := resolveAdditionalHostMounts(cfg.AdditionalHostMounts)
	if err != nil {
		return nil, err
	}

	nvidia, err := resolveNVIDIA(runtime.GOARCH)
	if err != nil {
		return nil, fmt.Errorf("resolve nvidia host: %w", err)
	}

	nvidiaRequired := len(nvidia.GPUDevicePaths) > 0
	if provisionedNVIDIARequired != nil {
		nvidiaRequired = *provisionedNVIDIARequired
	}

	if nvidiaRequired && !NVIDIAStateAvailable(nvidia) {
		return nil, fmt.Errorf("%w for machine %s: fresh host state is incomplete", ErrNVIDIAStateUnavailable, machineName)
	}

	if !nvidiaRequired {
		nvidia = NvidiaHost{}
	}

	return &RootFS{
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
		LifecycleStateFile:     NSpawnLifecycleStatePath(machineName),
		ConfigRegenerationFile: filepath.Join(SystemdSystemDir, ConfigRegenerationUnit(machineName)),
		NSpawnConfigInput: NSpawnConfigInput{
			AdditionalHostDevices: slices.Clone(cfg.AdditionalHostDevices),
			AdditionalHostMounts:  slices.Clone(cfg.AdditionalHostMounts),
		},
		NVIDIARequired:       nvidiaRequired,
		Nvidia:               nvidia,
		AMD:                  ResolveAMDHost(),
		HostDevices:          DiscoverHostDevices(cfg.AdditionalHostDevices),
		AdditionalHostMounts: additionalHostMounts,
	}, nil
}

// ResolveMachine probes the host (kernel version, hostname, GPU hardware) and
// resolves the complete goal state for the named nspawn machine from an agent
// config and caller-provided download overrides.
func ResolveMachine(log *slog.Logger, cfg *config.AgentConfig, machineName string, downloads *DownloadOverrides) (*MachineGoalState, error) {
	return resolveMachine(log, cfg, machineName, downloads, nil, ResolveNvidiaHost)
}

// ResolveExistingMachine resolves a managed restart using the validated
// capability persisted when the machine was provisioned. Fresh host state is
// still discovered, but it cannot change a GPU machine into a CPU machine or
// enable NVIDIA on a CPU machine.
func ResolveExistingMachine(log *slog.Logger, cfg *config.AgentConfig, machineName string, downloads *DownloadOverrides) (*MachineGoalState, error) {
	return resolveExistingMachine(
		log,
		cfg,
		machineName,
		downloads,
		NSpawnLifecycleStatePath(machineName),
		ResolveNvidiaHost,
	)
}

// ResolveExistingLifecycle resolves only the host and in-machine state needed
// to migrate lifecycle hooks for an already provisioned machine.
func ResolveExistingLifecycle(cfg *config.AgentConfig, machineName string) (*MachineGoalState, error) {
	return resolveExistingLifecycle(
		cfg,
		machineName,
		NSpawnLifecycleStatePath(machineName),
		legacyNVIDIADropInPath(machineName),
		ResolveNvidiaHost,
	)
}

func resolveExistingLifecycle(
	cfg *config.AgentConfig,
	machineName, lifecycleStatePath, legacyDropInPath string,
	resolveNVIDIA resolveNVIDIAHostFunc,
) (*MachineGoalState, error) {
	nvidiaRequired, err := LoadOrInferNVIDIACapability(lifecycleStatePath, legacyDropInPath, machineName)
	if err != nil {
		return nil, err
	}

	rootFS, err := resolveNSpawnConfig(cfg, machineName, &nvidiaRequired, resolveNVIDIA)
	if err != nil {
		return nil, err
	}

	return &MachineGoalState{
		RootFS: rootFS,
		NodeStart: &NodeStart{
			MachineName:    machineName,
			MachineDir:     rootFS.MachineDir,
			Containerd:     ResolveContainerdForNVIDIACapability(cfg.CRI.Containerd.SandboxImage, nvidiaRequired),
			Kubelet:        Kubelet{KubeletBinPath: filepath.Join("/"+BinDir, "kubelet")},
			NVIDIARequired: nvidiaRequired,
			Nvidia:         rootFS.Nvidia,
		},
	}, nil
}

func legacyNVIDIADropInPath(machineName string) string {
	return filepath.Join("/var/lib/machines", machineName, strings.TrimPrefix(NvidiaRuntimeDropInPath, "/"))
}

func resolveExistingMachine(
	log *slog.Logger,
	cfg *config.AgentConfig,
	machineName string,
	downloads *DownloadOverrides,
	lifecycleStatePath string,
	resolveNVIDIA resolveNVIDIAHostFunc,
) (*MachineGoalState, error) {
	nvidiaRequired, err := LoadOrInferNVIDIACapability(
		lifecycleStatePath,
		legacyNVIDIADropInPath(machineName),
		machineName,
	)
	if err != nil {
		return nil, err
	}

	return resolveMachine(log, cfg, machineName, downloads, &nvidiaRequired, resolveNVIDIA)
}

func resolveMachine(
	log *slog.Logger,
	cfg *config.AgentConfig,
	machineName string,
	downloads *DownloadOverrides,
	provisionedNVIDIARequired *bool,
	resolveNVIDIA resolveNVIDIAHostFunc,
) (*MachineGoalState, error) {
	sandboxImage := cfg.CRI.Containerd.SandboxImage

	nspawnConfig, err := resolveNSpawnConfig(cfg, machineName, provisionedNVIDIARequired, resolveNVIDIA)
	if err != nil {
		return nil, err
	}

	kernel, err := hostKernel()
	if err != nil {
		return nil, fmt.Errorf("get host kernel: %w", err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("get host hostname: %w", err)
	}

	nvidia := nspawnConfig.Nvidia
	amd := nspawnConfig.AMD

	ociImage := ResolveOCIImage(log, cfg.OCIImage, len(nvidia.GPUDevicePaths) > 0)

	kubelet, err := resolveKubelet(cfg)
	if err != nil {
		return nil, fmt.Errorf("resolve kubelet config: %w", err)
	}

	localDNS, err := resolveLocalDNS(cfg, downloads)
	if err != nil {
		return nil, fmt.Errorf("resolve LocalDNS config: %w", err)
	}

	if localDNS.Enabled {
		kubelet.ClusterDNS = localDNS.ClusterListenerIP.String()
		kubelet.ResolvConf = LocalDNSResolvConfPath
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
		MachineDir:             nspawnConfig.MachineDir,
		NSpawnConfigFile:       nspawnConfig.NSpawnConfigFile,
		ServiceOverrideFile:    nspawnConfig.ServiceOverrideFile,
		LifecycleStateFile:     nspawnConfig.LifecycleStateFile,
		ConfigRegenerationFile: nspawnConfig.ConfigRegenerationFile,
		NSpawnConfigInput:      nspawnConfig.NSpawnConfigInput,
		HostArch:               runtime.GOARCH,
		HostKernel:             kernel,
		Hostname:               hostname,
		ContainerdVersion:      containerdVersion,
		RunCVersion:            runcVersion,
		CNIPluginVersion:       cniVersion,
		KubernetesVersion:      cfg.Cluster.Version,
		LocalDNS:               localDNS,
		Downloads:              downloads,
		OCIImage:               ociImage,
		NVIDIARequired:         nspawnConfig.NVIDIARequired,
		Nvidia:                 nvidia,
		AMD:                    amd,
		HostDevices:            nspawnConfig.HostDevices,
		AdditionalHostMounts:   nspawnConfig.AdditionalHostMounts,
	}

	containerd := ResolveContainerdForNVIDIACapability(sandboxImage, nspawnConfig.NVIDIARequired)

	nodeStart := &NodeStart{
		MachineName:     machineName,
		KubeMachineName: cfg.MachineName,
		NodeName:        cfg.NodeName,
		MachineDir:      filepath.Join("/var/lib/machines", machineName),
		Containerd:      containerd,
		Gantry:          ResolveGantry(cfg.Gantry),
		Kubelet:         kubelet,
		LocalDNS:        localDNS,
		NVIDIARequired:  nspawnConfig.NVIDIARequired,
		Nvidia:          nvidia,
	}

	return &MachineGoalState{
		RootFS:    rootFS,
		NodeStart: nodeStart,
	}, nil
}

func ResolveGantry(cfg *config.GantryConfig) Gantry {
	return Gantry{Disabled: cfg != nil && cfg.Disabled}
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

	if err := cfg.Kubelet.Validate(); err != nil {
		return zero, err
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

	var imageCredentialProvider *ImageCredentialProvider
	if cfg.Kubelet.ImageCredentialProvider != nil {
		imageCredentialProvider = &ImageCredentialProvider{
			ConfigPath: cfg.Kubelet.ImageCredentialProvider.ConfigPath,
			BinDir:     cfg.Kubelet.ImageCredentialProvider.BinDir,
		}
	}

	return Kubelet{
		KubeletBinPath:          filepath.Join("/"+BinDir, "kubelet"),
		KubeletAuthInfo:         cfg.Kubelet.Auth,
		APIServer:               cfg.Kubelet.ApiServer,
		CACertData:              caCert,
		ClusterDNS:              cfg.Cluster.ClusterDNS,
		ResolvConf:              "/etc/resolv.conf",
		NodeIP:                  nodeIP,
		NodeLabels:              labels,
		RegisterWithTaints:      cfg.Kubelet.RegisterWithTaints,
		Configuration:           cfg.Kubelet.Configuration,
		ImageCredentialProvider: imageCredentialProvider,
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

// resolveAdditionalHostMounts validates the supplied mount entries, clones the
// slice so the caller's config is not mutated, and defaults any empty Target to
// the corresponding Source.
func resolveAdditionalHostMounts(mounts []config.AdditionalHostMount) ([]config.AdditionalHostMount, error) {
	if err := config.ValidateAdditionalHostMounts(mounts); err != nil {
		return nil, err
	}

	resolved := slices.Clone(mounts)
	for i := range resolved {
		if resolved[i].Target == "" {
			resolved[i].Target = resolved[i].Source
		}
	}

	return resolved, nil
}
