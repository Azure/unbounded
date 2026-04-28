// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilexec"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	// cniBinDir is the standard CNI binary directory relative to the machine root.
	cniBinDir = "opt/cni/bin"

	// cniPluginsDefaultBaseURL is the upstream base URL for CNI plugin
	// releases. Mirrors must preserve the <base>/v<ver>/<asset> layout.
	cniPluginsDefaultBaseURL = "https://github.com/containernetworking/plugins/releases/download"
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

	version := d.goalState.CNIPluginVersion

	var override *goalstates.DownloadSource
	if d.goalState.Downloads != nil {
		override = d.goalState.Downloads.CNI
	}

	if override != nil && override.Version != "" {
		version = override.Version
	}

	downloadURL := cniDownloadURL(override, version, d.goalState.HostArch)

	if hasRequiredCNIPlugins(destDir) && cniPluginsVersionMatch(ctx, d.log, destDir, version) {
		return nil
	}

	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
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
func cniDownloadURL(override *goalstates.DownloadSource, version, arch string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, version, arch, version)
	}

	base := cniPluginsDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	return fmt.Sprintf("%s/v%s/cni-plugins-linux-%s-v%s.tgz", base, version, arch, version)
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

	output, err := utilexec.OutputCmd(ctx, log, loopbackPath, "--version")
	if err != nil {
		// Some CNI plugin versions don't support --version; treat as not matching.
		return false
	}

	return strings.Contains(output, expectedVersion)
}
