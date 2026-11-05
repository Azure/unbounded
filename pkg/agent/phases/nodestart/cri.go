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

	// skipUnit leaves the containerd service unit alone. Host-native backends
	// own their unit and must never write the distro's containerd.service.
	skipUnit bool

	// configPath is where the main configuration is written. Empty means
	// goalstates.ContainerdConfigPath, the path the nspawn machine's
	// containerd reads. A host-native backend passes its own path so the
	// distro's /etc/containerd/config.toml is never written.
	configPath string

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

	if !c.skipUnit {
		if err := c.ensureContainerdServiceUnit(); err != nil {
			return fmt.Errorf("ensure containerd service unit: %w", err)
		}
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

	configPath := c.configPath
	if configPath == "" {
		configPath = goalstates.ContainerdConfigPath
	}

	dest := c.goalState.Root().Path(configPath)

	return c.write(dest, buf.Bytes(), 0o644)
}

func (c *configureContainerd) ensureGantryHostsConfig() error {
	dest := c.goalState.Root().Path(goalstates.ContainerdDefaultHostsPath)

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

	dest := c.goalState.Root().Path(filepath.Join(goalstates.SystemdSystemDir, goalstates.SystemdUnitContainerd))

	return c.write(dest, buf.Bytes(), 0o644)
}

// ensureGPUDropInConfigs manages GPU-related containerd drop-in configs.
// When the nvidia runtime is enabled the drop-in is written; otherwise it is
// removed.
func (c *configureContainerd) ensureGPUDropInConfigs() error {
	nvidia := c.goalState.Containerd.NvidiaRuntime

	return ensureDropInConfig(
		c.goalState.Root().Path("/"),
		filepath.Base(goalstates.NvidiaRuntimeDropInPath),
		nvidia.Enabled,
		map[string]any{
			"RuntimePath":                nvidia.RuntimePath,
			"RuntimeClassName":           nvidia.RuntimeClassName,
			"DisableSetAsDefaultRuntime": nvidia.DisableSetAsDefaultRuntime,
		},
	)
}

// HostConfigTask is a configuration task for a host-native node stack that can
// say whether it changed any file, so a reapply restarts only what it altered.
type HostConfigTask interface {
	phases.Task
	// Changed reports whether Do wrote content that differed from disk.
	Changed() bool
}

func (c *changeTracker) Changed() bool { return c.changed }

// ConfigureContainerdOnHost returns a task that writes the containerd
// configuration for a host-native node stack and leaves the service unit to the
// backend. The main configuration goes to configPath, which the backend's unit
// passes to containerd; the drop-ins and registry hosts stay at the paths the
// nspawn machine uses.
func ConfigureContainerdOnHost(goalState *goalstates.NodeStart, configPath string) HostConfigTask {
	return &configureContainerd{goalState: goalState, skipUnit: true, configPath: configPath}
}

type startContainerd struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

type importContainerImages struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

func (i *importContainerImages) archivePath(a stagedContainerImageArchive) string {
	// The nspawn machine sees the staged directory bind mounted at a fixed
	// path. A host-native node stack reads the staged files where they are.
	if i.goalState.HostNative() {
		return a.hostPath
	}

	return a.machinePath
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
	if _, err := s.goalState.Exec(ctx, s.log, "systemctl", "enable", "--now", goalstates.SystemdUnitContainerd); err != nil {
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
			"archive", i.archivePath(archive),
			"host_archive", archive.hostPath,
		)

		if _, err := i.goalState.Exec(ctx, i.log, "ctr", "--namespace", "k8s.io", "images", "import", i.archivePath(archive)); err != nil {
			return fmt.Errorf("import container image archive %s in %s: %w", i.archivePath(archive), i.goalState.MachineName, err)
		}

		i.log.Info("imported container image archive", "archive", i.archivePath(archive))
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
