// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/internal/agentartifacts"
	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/artifactsource"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	// cniBinDir is the standard CNI binary directory relative to the machine root.
	cniBinDir = "opt/cni/bin"
)

// requiredCNIPlugins lists the CNI plugins that must be present for a valid installation.
var requiredCNIPlugins = []string{
	"bridge",
	"host-local",
	"loopback",
}

type downloadCNIBinaries struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

// DownloadCNIBinaries returns a task that downloads and installs CNI plugin binaries into the rootfs.
// It skips the download if all required plugins are already installed and the version matches.
func DownloadCNIBinaries(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &downloadCNIBinaries{log: log, goalState: goalState}
}

func (d *downloadCNIBinaries) Name() string { return "download-cni-binaries" }

func (d *downloadCNIBinaries) Do(ctx context.Context) error {
	destDir := filepath.Join(d.goalState.MachineDir, cniBinDir)
	override := cniDownloadSource(d.goalState)
	version := downloadSourceVersion(d.goalState.CNIPluginVersion, override)

	downloadURL, err := cniDownloadURL(override, version, d.goalState.HostArch)
	if err != nil {
		return fmt.Errorf("resolve CNI download source: %w", err)
	}

	if hasRequiredCNIPlugins(destDir) && cniPluginsVersionMatch(ctx, d.log, destDir, version) {
		return nil
	}

	for tarFile, err := range downloadURL.DecompressTarGz(ctx) {
		if err != nil {
			return fmt.Errorf("decompress CNI plugins tar: %w", err)
		}

		targetFilePath := filepath.Join(destDir, tarFile.Name)

		if err := utilio.InstallFile(targetFilePath, tarFile.Body, 0o755); err != nil {
			return fmt.Errorf("install CNI plugin %q: %w", targetFilePath, err)
		}
	}

	return nil
}

// cniDownloadURL resolves the CNI plugins tarball URL honoring the
// optional override. Mirrors must publish under <base>/v<ver>/<asset>.
func cniDownloadURL(override *goalstates.DownloadSource, version, arch string) (artifactsource.Source, error) {
	return artifactsource.Parse(agentartifacts.CNIPluginsArchive(override, version, arch))
}

// hasRequiredCNIPlugins checks if all required CNI plugins are installed and executable.
func hasRequiredCNIPlugins(cniBinPath string) bool {
	for _, plugin := range requiredCNIPlugins {
		pluginPath := filepath.Join(cniBinPath, plugin)
		if !utilio.IsExecutable(pluginPath) {
			return false
		}
	}

	return true
}

// cniPluginsVersionMatch checks if the installed CNI plugins version matches the expected version.
// It uses the loopback plugin as the version check reference, as it is always present.
func cniPluginsVersionMatch(ctx context.Context, log *slog.Logger, cniBinPath, expectedVersion string) bool {
	loopbackPath := filepath.Join(cniBinPath, "loopback")
	if !utilio.IsExecutable(loopbackPath) {
		return false
	}

	// Debug rather than the default Info for stderr: some CNI plugin versions
	// do not support --version, so this failing is expected and the plugin's
	// own complaint should not be surfaced as though something were wrong.
	output, err := executil.OutputCmdAt(ctx, log, slog.LevelDebug, loopbackPath, "--version")
	if err != nil {
		// Treated as "not matching", which triggers a reinstall. Logged all the
		// same: without it an infrastructure failure, an ETXTBSY for instance,
		// looks exactly like an old plugin.
		log.Debug("could not read the installed CNI plugin version; assuming it needs reinstalling",
			"path", loopbackPath, "error", err)

		return false
	}

	return strings.Contains(output, expectedVersion)
}
