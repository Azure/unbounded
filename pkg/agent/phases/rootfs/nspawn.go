// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs/oci"
)

//go:embed assets/nspawn.conf assets/config-regeneration.service assets/service-override.conf
var nspawnAssets embed.FS

var nspawnTemplates = template.Must(
	template.New("nspawn").ParseFS(nspawnAssets, "assets/nspawn.conf", "assets/config-regeneration.service", "assets/service-override.conf"),
)

type ensureNSpawnWorkspace struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

type NSpawnBind struct {
	// Source is the host path passed to Bind=/BindReadOnly=.
	Source string
	// Target is the optional container path. Empty means "bind at Source".
	Target string
	// ReadOnly controls whether BindReadOnly= is used instead of Bind=.
	ReadOnly bool
}

type NSpawnDeviceAllow struct {
	// Specifier is the systemd DeviceAllow selector (path or char-* class).
	Specifier string
	// Access is the device permission mode, usually "rwm".
	Access string
}

type NSpawnDeviceTarget struct {
	// Bind controls visibility of a host path inside nspawn.
	Bind NSpawnBind
	// Allow controls cgroup device access for the corresponding target.
	Allow NSpawnDeviceAllow
}

// EnsureNSpawnWorkspace returns a task that bootstraps an OCI rootfs into the
// machine directory (if it is empty or missing) and writes the
// systemd-nspawn configuration files needed to run a Kubernetes node inside a
// nspawn container.
func EnsureNSpawnWorkspace(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &ensureNSpawnWorkspace{log: log, goalState: goalState}
}

func (e *ensureNSpawnWorkspace) Name() string { return "ensure-nspawn-workspace" }

type ensureNSpawnConfig struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

// EnsureNSpawnConfig returns a task that only writes the host-side
// systemd-nspawn configuration files for a machine.
func EnsureNSpawnConfig(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &ensureNSpawnConfig{log: log, goalState: goalState}
}

func (e *ensureNSpawnConfig) Name() string { return "ensure-nspawn-config" }

func (e *ensureNSpawnConfig) Do(_ context.Context) error {
	return writeNSpawnConfigs(e.log, e.goalState)
}

func (e *ensureNSpawnWorkspace) Do(ctx context.Context) error {
	if err := e.bootstrapWorkspace(ctx); err != nil {
		return fmt.Errorf("bootstrap machine directory %s: %w", e.goalState.MachineDir, err)
	}

	if err := writeNSpawnConfigs(e.log, e.goalState); err != nil {
		return err
	}

	return nil
}

func (e *ensureNSpawnWorkspace) bootstrapWorkspace(ctx context.Context) error {
	bootstrapTask := oci.DownloadRootFS(e.log, e.goalState.MachineDir, e.goalState.HostArch, e.goalState.OCIImage)
	return phases.ExecuteTask(ctx, e.log, bootstrapTask)
}

// nspawnTemplateData holds the data passed to the nspawn.conf and
// service-override.conf templates. Using a struct (rather than map[string]any)
// lets us attach helper methods that the templates can call directly.
type nspawnTemplateData struct {
	// MachineName is the nspawn machine name (e.g. "kube1"). Used by the
	// service drop-in for the ExecStartPre `machinectl terminate` cleanup.
	MachineName                  string
	BPFFSMountPath               string
	ContainerImageArchiveDir     string
	ContainerImageArchiveHostDir string
	HostDevicePaths              []string
	HostDeviceGroupSpecifiers    []string
	AdditionalHostMounts         []config.AdditionalHostMount
	// NvidiaDeviceTargets contains render-ready Bind+DeviceAllow pairs.
	NvidiaDeviceTargets    []NSpawnDeviceTarget
	NvidiaLibDirMounts     []goalstates.NvidiaLibDirMount
	NvidiaI386LibDirMounts []goalstates.NvidiaLibDirMount
	NvidiaBinDir           string
	AMDGPUDevicePaths      []string
	AMDSysFSPaths          []string
	ConfigRegenerationUnit string
	AgentBinaryPath        string
}

// TODO: migrate AdditionalHostMounts, HostDevicePaths/HostDeviceGroupSpecifiers,
// and AMDGPUDevicePaths to structured bind/device-allow targets in a follow-up PR.

// writeNSpawnConfigs renders the nspawn-related templates with device and GPU
// data (when present) and writes them to their configured paths.
func writeNSpawnConfigs(log *slog.Logger, goalState *goalstates.RootFS) error {
	// MachineName is the basename of MachineDir (e.g. "kube1" from
	// "/var/lib/machines/kube1"); nspawn always names the machine after that
	// directory.
	machineName := filepath.Base(goalState.MachineDir)
	hostDevicePaths := goalState.HostDevices.Paths()
	hostDeviceGroupSpecifiers := goalState.HostDevices.DeviceGroupSpecifiers()
	amdGPUDevicePaths := pathsExcluding(goalState.AMD.GPUDevicePaths, goalState.Nvidia.GPUDevicePaths)

	archiveDir := filepath.Join(goalState.MachineDir, strings.TrimPrefix(goalstates.ContainerImageArchiveDir, "/"))
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return fmt.Errorf("create container image archive mount point: %w", err)
	}

	templateData := nspawnTemplateData{
		MachineName:                  machineName,
		BPFFSMountPath:               goalstates.BPFFSMountPath(machineName),
		ContainerImageArchiveDir:     goalstates.ContainerImageArchiveDir,
		ContainerImageArchiveHostDir: goalstates.ContainerImageArchiveHostDir,
		HostDevicePaths:              hostDevicePaths,
		HostDeviceGroupSpecifiers:    hostDeviceGroupSpecifiers,
		AdditionalHostMounts:         goalState.AdditionalHostMounts,
		NvidiaDeviceTargets:          nvidiaNSpawnDeviceTargets(goalState.Nvidia.GPUDevicePaths),
		NvidiaLibDirMounts:           goalState.Nvidia.LibDirMounts,
		NvidiaI386LibDirMounts:       goalState.Nvidia.I386LibDirMounts,
		NvidiaBinDir:                 nvidiaHostBinDir(goalState.Nvidia),
		AMDGPUDevicePaths:            amdGPUDevicePaths,
		AMDSysFSPaths:                goalState.AMD.SysFSPaths,
		ConfigRegenerationUnit:       goalstates.ConfigRegenerationUnit(machineName),
		AgentBinaryPath:              goalstates.DaemonBinaryPath,
	}

	if len(hostDevicePaths) > 0 {
		log.Info("host devices detected, configuring nspawn bind-mounts",
			"total", len(hostDevicePaths),
			"kvm", len(goalState.HostDevices.KVM),
			"network", len(goalState.HostDevices.Network),
			"block", len(goalState.HostDevices.Block),
			"infiniband", len(goalState.HostDevices.Infiniband),
			"additional", len(goalState.HostDevices.Additional))
	}

	if len(goalState.AdditionalHostMounts) > 0 {
		log.Info("additional host mounts configured",
			"count", len(goalState.AdditionalHostMounts))
	}

	if len(goalState.Nvidia.GPUDevicePaths) > 0 {
		log.Info("GPU devices detected, configuring nspawn bind-mounts",
			"count", len(goalState.Nvidia.GPUDevicePaths))
	}

	if len(amdGPUDevicePaths) > 0 {
		log.Info("AMD GPU devices detected, configuring nspawn bind-mounts",
			"count", len(amdGPUDevicePaths))
	}

	// Render and write the .nspawn configuration file.
	nspawnBuf := &bytes.Buffer{}
	if err := nspawnTemplates.ExecuteTemplate(nspawnBuf, "nspawn.conf", templateData); err != nil {
		return fmt.Errorf("render nspawn config template: %w", err)
	}

	if err := utilio.WriteFile(goalState.NSpawnConfigFile, nspawnBuf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write nspawn config %s: %w", goalState.NSpawnConfigFile, err)
	}

	// Render and write the systemd service override drop-in.
	overrideBuf := &bytes.Buffer{}
	if err := nspawnTemplates.ExecuteTemplate(overrideBuf, "service-override.conf", templateData); err != nil {
		return fmt.Errorf("render service override template: %w", err)
	}

	if err := utilio.WriteFile(goalState.ServiceOverrideFile, overrideBuf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write service override %s: %w", goalState.ServiceOverrideFile, err)
	}

	unitFile := filepath.Join(goalstates.SystemdSystemDir, templateData.ConfigRegenerationUnit)

	unitBuf := &bytes.Buffer{}
	if err := nspawnTemplates.ExecuteTemplate(unitBuf, "config-regeneration.service", templateData); err != nil {
		return fmt.Errorf("render config regeneration unit template: %w", err)
	}

	if err := utilio.WriteFile(unitFile, unitBuf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write config regeneration unit %s: %w", unitFile, err)
	}

	return nil
}

func nvidiaHostBinDir(nvidia goalstates.NvidiaHost) string {
	for _, path := range []string{nvidia.NvidiaSMIPath, nvidia.NvidiaIMEXPath, nvidia.NvidiaIMEXCtlPath} {
		if path != "" {
			return filepath.Dir(path)
		}
	}

	return ""
}

func nvidiaNSpawnDeviceTargets(paths []string) []NSpawnDeviceTarget {
	targets := make([]NSpawnDeviceTarget, 0, len(paths))

	for _, path := range paths {
		target := NSpawnDeviceTarget{
			Bind: NSpawnBind{Source: path},
			Allow: NSpawnDeviceAllow{
				Specifier: path,
				Access:    "rwm",
			},
		}

		// The caps paths are directories that must be bind-mounted so dynamic
		// NVIDIA capability and channel device nodes are visible inside nspawn.
		// DeviceAllow operates on character device nodes and classes, so use
		// the kernel device-class names to grant current and future nodes access.
		switch path {
		case "/dev/nvidia-caps":
			target.Allow.Specifier = "char-nvidia-caps"
		case "/dev/nvidia-caps-imex-channels":
			target.Allow.Specifier = "char-nvidia-caps-imex-channels"
		}

		targets = append(targets, target)
	}

	return targets
}

func pathsExcluding(paths, excluded []string) []string {
	if len(paths) == 0 || len(excluded) == 0 {
		return paths
	}

	seen := make(map[string]bool, len(excluded))
	for _, p := range excluded {
		seen[p] = true
	}

	var out []string

	for _, p := range paths {
		if !seen[p] {
			out = append(out, p)
		}
	}

	return out
}
