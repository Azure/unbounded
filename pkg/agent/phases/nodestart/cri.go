// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
}

// ConfigureContainerd returns a task that writes the containerd configuration, systemd unit,
// and optional GPU drop-in configs into the machine rootfs. It runs before the nspawn machine
// is started, so all paths are relative to the machine directory on the host filesystem.
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

	if err := c.ensureNVIDIAReadyServiceUnit(); err != nil {
		return fmt.Errorf("ensure NVIDIA ready service unit: %w", err)
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

	return utilio.WriteFile(dest, buf.Bytes(), 0o644)
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

	return utilio.WriteFile(dest, []byte(gantryHostsConfig), 0o644)
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

	return utilio.WriteFile(dest, buf.Bytes(), 0o644)
}

func (c *configureContainerd) ensureNVIDIAReadyServiceUnit() error {
	dest := filepath.Join(c.goalState.MachineDir, goalstates.SystemdSystemDir, goalstates.SystemdUnitNVIDIAReady)

	data, err := assets.ReadFile("assets/unbounded-nvidia-ready.service")
	if err != nil {
		return err
	}

	return utilio.WriteFile(dest, data, 0o644)
}

// ensureGPUDropInConfigs manages GPU-related containerd drop-in configs.
// When the nvidia runtime is enabled the drop-in is written; otherwise it is
// removed.
func (c *configureContainerd) ensureGPUDropInConfigs() error {
	nvidia := c.goalState.Containerd.NvidiaRuntime

	return ensureDropInConfig(
		c.goalState.MachineDir,
		filepath.Base(goalstates.NvidiaRuntimeDropInPath),
		nvidia.Enabled,
		map[string]any{
			"RuntimePath":                nvidia.RuntimePath,
			"RuntimeClassName":           nvidia.RuntimeClassName,
			"DisableSetAsDefaultRuntime": nvidia.DisableSetAsDefaultRuntime,
		},
	)
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

// ensureDropInConfig writes or removes a containerd drop-in config file in the
// machine rootfs. If enabled is true, the template is rendered and written.
// If enabled is false, the drop-in is removed if it exists.
func ensureDropInConfig(
	machineDir string,
	dropInName string,
	enabled bool,
	templateData map[string]any,
) error {
	dropInPath := filepath.Join(machineDir, goalstates.ContainerdConfDropInDir, dropInName)

	if !enabled {
		err := os.Remove(dropInPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}

	buf := &bytes.Buffer{}
	if err := assetsTemplate.ExecuteTemplate(buf, dropInName, templateData); err != nil {
		return err
	}

	return utilio.WriteFile(dropInPath, buf.Bytes(), 0o644)
}
