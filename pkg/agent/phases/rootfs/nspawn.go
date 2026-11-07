// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/isolation/nspawn"
	"github.com/Azure/unbounded/pkg/agent/phases"
	"github.com/Azure/unbounded/pkg/agent/phases/rootfs/oci"
)

// nspawnTemplateData aliases the nspawn backend's rendering data, which owns
// the templates and the shape of their data.
type nspawnTemplateData = nspawn.TemplateData

// NSpawnBind is one Bind= or BindReadOnly= entry of the .nspawn file.
//
// Deprecated: use nspawn.Bind. This alias is kept for one release.
type NSpawnBind = nspawn.Bind

// NSpawnDeviceAllow is one DeviceAllow= entry of the service override.
//
// Deprecated: use nspawn.DeviceAllow. This alias is kept for one release.
type NSpawnDeviceAllow = nspawn.DeviceAllow

// NSpawnDeviceTarget pairs a device bind with its cgroup rule.
//
// Deprecated: use nspawn.DeviceTarget. This alias is kept for one release.
type NSpawnDeviceTarget = nspawn.DeviceTarget

type ensureNSpawnWorkspace struct {
	log         *slog.Logger
	goalState   *goalstates.RootFS
	ownedReplay bool
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
	log          *slog.Logger
	goalState    *goalstates.RootFS
	repairHelper phases.Task
}

// EnsureNSpawnConfig returns a task that only writes the host-side
// systemd-nspawn configuration files for a machine. When the machine's hooks
// run the lifecycle helper and it is missing, it reinstalls the helper first,
// so the files it writes never run a helper that isn't there.
func EnsureNSpawnConfig(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &ensureNSpawnConfig{
		log:          log,
		goalState:    goalState,
		repairHelper: RepairNSpawnLifecycleHelper(goalState),
	}
}

func (e *ensureNSpawnConfig) Name() string { return "ensure-nspawn-config" }

func (e *ensureNSpawnConfig) Do(ctx context.Context) error {
	if err := phases.ExecuteTask(ctx, e.log, e.repairHelper); err != nil {
		return fmt.Errorf("repair nspawn lifecycle helper: %w", err)
	}

	return writeNSpawnConfigs(e.log, e.goalState)
}

func (e *ensureNSpawnWorkspace) Do(ctx context.Context) error {
	if err := e.bootstrapWorkspace(ctx); err != nil {
		return fmt.Errorf("bootstrap machine directory %s: %w", e.goalState.Root().Path("/"), err)
	}

	if err := phases.ExecuteTask(ctx, e.log, EnsureNSpawnLifecycleHelperFor(e.goalState)); err != nil {
		return fmt.Errorf("install nspawn lifecycle helper: %w", err)
	}

	if err := writeNSpawnConfigs(e.log, e.goalState); err != nil {
		return err
	}

	return nil
}

func (e *ensureNSpawnWorkspace) bootstrapWorkspace(ctx context.Context) error {
	bootstrapTask := oci.DownloadRootFS(e.log, e.goalState.Root().Path("/"), e.goalState.HostArch, e.goalState.OCIImage)
	if e.ownedReplay {
		bootstrapTask = oci.DownloadOwnedRootFS(e.log, e.goalState.Root().Path("/"), e.goalState.HostArch, e.goalState.OCIImage)
	}

	return phases.ExecuteTask(ctx, e.log, bootstrapTask)
}

// writeNSpawnConfigs renders the nspawn-related templates with device and GPU
// data (when present) and writes them to their configured paths.
func writeNSpawnConfigs(log *slog.Logger, goalState *goalstates.RootFS) error {
	// MachineName is the slot, the basename of the machine directory (e.g. "kube1" from
	// "/var/lib/machines/kube1"); nspawn always names the machine after that
	// directory.
	machineName := goalState.Root().Slot()

	// The resolvers validate these, but the goal state may come from elsewhere,
	// and an empty command would clear its hook's command list in the unit.
	if err := config.ValidateNSpawnLifecycle(goalState.NSpawnLifecycle); err != nil {
		return err
	}

	archiveDir := goalState.Root().Path(goalstates.ContainerImageArchiveDir)
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		return fmt.Errorf("create container image archive mount point: %w", err)
	}

	templateData := nspawnTemplateData{
		MachineName:                  machineName,
		BPFFSMountPath:               goalstates.BPFFSMountPath(machineName),
		ContainerImageArchiveDir:     goalstates.ContainerImageArchiveDir,
		ContainerImageArchiveHostDir: goalstates.ContainerImageArchiveHostDir,
		ConfigRegenerationUnit:       goalstates.ConfigRegenerationUnit(machineName),
	}
	templateData.PreStartCommand, templateData.PostStartCommand = nspawnLifecycleCommands(goalState, machineName)
	templateData.ApplyGrants(goalState.Grants())

	if n := len(templateData.HostDevicePaths); n > 0 {
		log.Info("host devices detected, configuring nspawn bind-mounts",
			"total", n,
			"kvm", len(goalState.HostDevices.KVM),
			"mshv", len(goalState.HostDevices.MSHV),
			"network", len(goalState.HostDevices.Network),
			"block", len(goalState.HostDevices.Block),
			"deviceDirectories", len(goalState.HostDevices.DeviceDirectories),
			"additional", len(goalState.HostDevices.Additional))
	}

	if n := len(templateData.AdditionalHostMounts); n > 0 {
		log.Info("additional host mounts configured",
			"count", n)
	}

	if len(goalState.Nvidia.GPUDevicePaths) > 0 {
		log.Info("GPU devices detected, configuring nspawn bind-mounts",
			"count", len(goalState.Nvidia.GPUDevicePaths))
	}

	if n := len(templateData.AMDGPUDevicePaths); n > 0 {
		log.Info("AMD GPU devices detected, configuring nspawn bind-mounts",
			"count", n)
	}

	rendered, err := nspawn.Render(templateData)
	if err != nil {
		return err
	}

	if err := utilio.WriteFile(goalState.NSpawnConfigFile, rendered.Settings, 0o644); err != nil {
		return fmt.Errorf("write nspawn config %s: %w", goalState.NSpawnConfigFile, err)
	}

	if err := utilio.WriteFile(goalState.ServiceOverrideFile, rendered.ServiceOverride, 0o644); err != nil {
		return fmt.Errorf("write service override %s: %w", goalState.ServiceOverrideFile, err)
	}

	unitFile := goalState.ConfigRegenerationFile
	if unitFile == "" {
		unitFile = filepath.Join(goalstates.SystemdSystemDir, templateData.ConfigRegenerationUnit)
	}

	if err := utilio.WriteFile(unitFile, rendered.ConfigRegenerationUnit, 0o644); err != nil {
		return fmt.Errorf("write config regeneration unit %s: %w", unitFile, err)
	}

	return nil
}

// nspawnLifecycleCommands returns the configured lifecycle hook commands, or
// the lifecycle helper's for the machine.
func nspawnLifecycleCommands(goalState *goalstates.RootFS, machineName string) (preStart, postStart string) {
	if !goalState.UsesLifecycleHelper() {
		return goalState.NSpawnLifecycle.PreStartCommand, goalState.NSpawnLifecycle.PostStartCommand
	}

	helper := goalstates.ResolveHostPaths().NSpawnLifecycleBinary

	return helper + " nspawn-lifecycle pre-start " + machineName, helper + " nspawn-lifecycle post-start " + machineName
}
