// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"log/slog"
	"path/filepath"
	"text/template"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs/oci"
)

//go:embed assets/nspawn.conf assets/service-override.conf
var nspawnAssets embed.FS

var nspawnTemplates = template.Must(
	template.New("nspawn").ParseFS(nspawnAssets, "assets/nspawn.conf", "assets/service-override.conf"),
)

type ensureNSpawnWorkspace struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

// EnsureNSpawnWorkspace returns a task that bootstraps an OCI rootfs into the
// machine directory (if it is empty or missing) and writes the
// systemd-nspawn configuration files needed to run a Kubernetes node inside a
// nspawn container.
func EnsureNSpawnWorkspace(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &ensureNSpawnWorkspace{log: log, goalState: goalState}
}

func (e *ensureNSpawnWorkspace) Name() string { return "ensure-nspawn-workspace" }

func (e *ensureNSpawnWorkspace) Do(ctx context.Context) error {
	if err := e.bootstrapWorkspace(ctx); err != nil {
		return fmt.Errorf("bootstrap machine directory %s: %w", e.goalState.MachineDir, err)
	}

	if err := e.writeNSpawnConfigs(); err != nil {
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
	MachineName          string
	BPFFSMountPath       string
	HostDevicePaths      []string
	NvidiaGPUDevicePaths []string
	NvidiaLibDirMounts   []goalstates.NvidiaLibDirMount
	AMDGPUDevicePaths    []string
	AMDSysFSPaths        []string
}

// writeNSpawnConfigs renders the nspawn and service-override templates with
// device and GPU data (when present) and writes them to their configured paths.
func (e *ensureNSpawnWorkspace) writeNSpawnConfigs() error {
	// MachineName is the basename of MachineDir (e.g. "kube1" from
	// "/var/lib/machines/kube1"); nspawn always names the machine after that
	// directory.
	machineName := filepath.Base(e.goalState.MachineDir)
	hostDevicePaths := e.goalState.HostDevices.Paths()
	amdGPUDevicePaths := pathsExcluding(e.goalState.AMD.GPUDevicePaths, e.goalState.Nvidia.GPUDevicePaths)
	templateData := nspawnTemplateData{
		MachineName:          machineName,
		BPFFSMountPath:       goalstates.BPFFSMountPath(machineName),
		HostDevicePaths:      hostDevicePaths,
		NvidiaGPUDevicePaths: e.goalState.Nvidia.GPUDevicePaths,
		NvidiaLibDirMounts:   e.goalState.Nvidia.LibDirMounts,
		AMDGPUDevicePaths:    amdGPUDevicePaths,
		AMDSysFSPaths:        e.goalState.AMD.SysFSPaths,
	}

	if len(hostDevicePaths) > 0 {
		e.log.Info("host devices detected, configuring nspawn bind-mounts",
			"total", len(hostDevicePaths),
			"kvm", len(e.goalState.HostDevices.KVM),
			"block", len(e.goalState.HostDevices.Block),
			"infiniband", len(e.goalState.HostDevices.Infiniband))
	}

	if len(e.goalState.Nvidia.GPUDevicePaths) > 0 {
		e.log.Info("GPU devices detected, configuring nspawn bind-mounts",
			"count", len(e.goalState.Nvidia.GPUDevicePaths))
	}

	if len(amdGPUDevicePaths) > 0 {
		e.log.Info("AMD GPU devices detected, configuring nspawn bind-mounts",
			"count", len(amdGPUDevicePaths))
	}

	// Render and write the .nspawn configuration file.
	nspawnBuf := &bytes.Buffer{}
	if err := nspawnTemplates.ExecuteTemplate(nspawnBuf, "nspawn.conf", templateData); err != nil {
		return fmt.Errorf("render nspawn config template: %w", err)
	}

	if err := utilio.WriteFile(e.goalState.NSpawnConfigFile, nspawnBuf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write nspawn config %s: %w", e.goalState.NSpawnConfigFile, err)
	}

	// Render and write the systemd service override drop-in.
	overrideBuf := &bytes.Buffer{}
	if err := nspawnTemplates.ExecuteTemplate(overrideBuf, "service-override.conf", templateData); err != nil {
		return fmt.Errorf("render service override template: %w", err)
	}

	if err := utilio.WriteFile(e.goalState.ServiceOverrideFile, overrideBuf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write service override %s: %w", e.goalState.ServiceOverrideFile, err)
	}

	return nil
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
