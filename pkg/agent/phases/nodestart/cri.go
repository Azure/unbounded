// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package nodestart

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"text/template"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

//go:embed assets/*
var assets embed.FS

var assetsTemplate = template.Must(template.New("assets").ParseFS(assets, "assets/*"))

const (
	gantryHostsManagedMarker = "# Managed by unbounded-agent for Gantry."
	gantryHostsConfig        = gantryHostsManagedMarker + `
[host."http://127.0.0.1:5000"]
  capabilities = ["pull", "resolve"]
  dial_timeout = "200ms"
`
)

type configureContainerd struct {
	goalState *goalstates.NodeStart

	changeTracker
}

// ConfigureContainerd returns a task that writes the containerd configuration, systemd unit,
// and optional GPU drop-in configs into the machine rootfs. It runs before the nspawn machine
// is started, so all paths are relative to the machine directory on the host filesystem.
//
// The agent reaches this work through StartNode, which builds the task directly
// so it can tell whether the configuration it wrote differed. This entry point
// remains for callers outside the agent that compose phases themselves.
func ConfigureContainerd(goalState *goalstates.NodeStart) phases.Task {
	return &configureContainerd{goalState: goalState}
}

func (c *configureContainerd) Name() string { return "configure-containerd" }

func (c *configureContainerd) Do(_ context.Context) error {
	if err := c.ensureContainerdConfig(); err != nil {
		return fmt.Errorf("ensure containerd config: %w", err)
	}

	if !c.goalState.Gantry.Disabled {
		if err := c.ensureGantryHostsConfig(); err != nil {
			return fmt.Errorf("ensure Gantry containerd hosts config: %w", err)
		}
	}

	if err := c.ensureContainerdServiceUnit(); err != nil {
		return fmt.Errorf("ensure containerd service unit: %w", err)
	}

	if err := c.ensureGPUDropInConfigs(); err != nil {
		return fmt.Errorf("ensure GPU drop-in configs: %w", err)
	}

	return nil
}

// ensureContainerdConfig renders and writes the main containerd config.toml
// into the machine rootfs.
func (c *configureContainerd) ensureContainerdConfig() error {
	spec := c.goalState.Containerd

	buf := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(buf, "containerd.toml", map[string]any{
		"SandboxImage":   spec.SandboxImage,
		"RuncBinaryPath": spec.RuncBinaryPath,
		"CNIBinDir":      spec.CNIBinDir,
		"CNIConfDir":     spec.CNIConfDir,
		"MetricsAddress": spec.MetricsAddress,
	}); err != nil {
		return err
	}

	dest := filepath.Join(c.goalState.MachineDir, goalstates.ContainerdConfigPath)

	return c.write(dest, buf.Bytes(), 0o644)
}

func (c *configureContainerd) ensureGantryHostsConfig() error {
	dest := filepath.Join(c.goalState.MachineDir, goalstates.ContainerdDefaultHostsPath)

	existing, err := os.ReadFile(dest)
	switch {
	case err == nil:
		if !hasGantryHostsManagedMarker(existing) {
			return fmt.Errorf("refusing to overwrite unmanaged containerd hosts file %s", goalstates.ContainerdDefaultHostsPath)
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}

	return c.write(dest, []byte(gantryHostsConfig), 0o644)
}

func hasGantryHostsManagedMarker(content []byte) bool {
	for _, line := range bytes.Split(content, []byte{'\n'}) {
		if string(bytes.TrimSpace(line)) == gantryHostsManagedMarker {
			return true
		}
	}

	return false
}

// ensureContainerdServiceUnit renders and writes the containerd systemd unit
// file into the machine rootfs.
func (c *configureContainerd) ensureContainerdServiceUnit() error {
	spec := c.goalState.Containerd

	buf := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(buf, "containerd.service", map[string]any{
		"ContainerdBinPath": spec.ContainerdBinPath,
	}); err != nil {
		return err
	}

	dest := filepath.Join(c.goalState.MachineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitContainerd)

	return c.write(dest, buf.Bytes(), 0o644)
}

// ensureGPUDropInConfigs manages GPU-related containerd drop-in configs. The
// NVIDIA runtime drop-in is written when the runtime is enabled and removed
// otherwise.
//
// It is also removed when the machine's last start could not set up its GPUs.
// The post-start hook removed it then and started containerd without it, and
// this drop-in is not change-tracked, so rewriting it on a running node would
// leave the file on disk disagreeing with containerd until the next start. The
// next machine start checks the GPUs again and writes it if they work.
func (c *configureContainerd) ensureGPUDropInConfigs() error {
	nvidia := c.goalState.Containerd.NvidiaRuntime
	if nvidia.Enabled && !gpuSetupFailedAtLastStart(c.goalState.MachineDir) {
		_, err := writeNVIDIARuntimeDropIn(c.goalState.MachineDir, nvidia)

		return err
	}

	_, err := removeNVIDIARuntimeDropIn(c.goalState.MachineDir)

	return err
}

// gpuSetupFailedAtLastStart reports whether the machine's last start recorded
// GPUs that could not be set up. An unreadable record counts as none: the next
// machine start rewrites it.
func gpuSetupFailedAtLastStart(machineDir string) bool {
	status, err := goalstates.ReadGPUStatus(machineDir)

	return err == nil && status != nil && !status.Ready
}

type startContainerd struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

type importContainerImages struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

// StartContainerd returns a task that enables and starts the containerd systemd service
// inside the running nspawn machine.
func StartContainerd(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &startContainerd{log: log, goalState: goalState}
}

// ImportContainerImages returns a task that preloads configured container
// image archives into containerd.
func ImportContainerImages(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &importContainerImages{log: log, goalState: goalState}
}

func (s *startContainerd) Name() string { return "start-containerd" }

func (s *startContainerd) Do(ctx context.Context) error {
	if _, err := executil.MachineRun(ctx, s.log, s.goalState.MachineName,
		"systemctl", "enable", "--now", goalstates.SystemdUnitContainerd,
	); err != nil {
		return fmt.Errorf("systemctl enable --now %s in %s: %w",
			goalstates.SystemdUnitContainerd, s.goalState.MachineName, err)
	}

	return nil
}

func (i *importContainerImages) Name() string { return "import-container-images" }

func (i *importContainerImages) Do(ctx context.Context) error {
	archives, err := stagedContainerImageArchives(goalstates.ContainerImageArchiveHostDir)
	if err != nil {
		return err
	}

	for _, archive := range archives {
		i.log.Info("importing container image archive",
			"archive", archive.machinePath,
			"host_archive", archive.hostPath,
		)

		if _, err := executil.MachineRun(ctx, i.log, i.goalState.MachineName,
			"ctr", "--namespace", "k8s.io", "images", "import", archive.machinePath,
		); err != nil {
			return fmt.Errorf("import container image archive %s in %s: %w", archive.machinePath, i.goalState.MachineName, err)
		}

		i.log.Info("imported container image archive", "archive", archive.machinePath)
	}

	return nil
}

type stagedContainerImageArchive struct {
	hostPath    string
	machinePath string
}

func stagedContainerImageArchives(hostDir string) ([]stagedContainerImageArchive, error) {
	entries, err := os.ReadDir(hostDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read staged container image archive directory %s: %w", hostDir, err)
	}

	archives := make([]stagedContainerImageArchive, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".tar" {
			continue
		}

		archives = append(archives, stagedContainerImageArchive{
			hostPath:    filepath.Join(hostDir, entry.Name()),
			machinePath: filepath.Join(goalstates.ContainerImageArchiveDir, entry.Name()),
		})
	}

	sort.Slice(archives, func(i, j int) bool {
		return archives[i].machinePath < archives[j].machinePath
	})

	return archives, nil
}

// writeNVIDIARuntimeDropIn writes the containerd drop-in that registers the
// NVIDIA runtime into the machine rootfs. It reports whether the file changed.
func writeNVIDIARuntimeDropIn(machineDir string, nvidia goalstates.NvidiaRuntime) (bool, error) {
	buf := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(buf, filepath.Base(goalstates.NvidiaRuntimeDropInPath), map[string]any{
		"RuntimePath":                nvidia.RuntimePath,
		"RuntimeClassName":           nvidia.RuntimeClassName,
		"DisableSetAsDefaultRuntime": nvidia.DisableSetAsDefaultRuntime,
	}); err != nil {
		return false, err
	}

	return utilio.WriteFileIfChanged(filepath.Join(machineDir, goalstates.NvidiaRuntimeDropInPath), buf.Bytes(), 0o644)
}

// removeNVIDIARuntimeDropIn removes the NVIDIA runtime drop-in from the machine
// rootfs. It reports whether there was one to remove.
func removeNVIDIARuntimeDropIn(machineDir string) (bool, error) {
	return removeFileIfPresent(filepath.Join(machineDir, goalstates.NvidiaRuntimeDropInPath))
}

// removeFileIfPresent removes path and reports whether it existed.
func removeFileIfPresent(path string) (bool, error) {
	err := os.Remove(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}
