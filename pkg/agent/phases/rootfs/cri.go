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
	// containerdDefaultBaseURL is the upstream base URL for containerd releases.
	// Mirrors must preserve the <base>/v<ver>/<asset> layout.
	containerdDefaultBaseURL = "https://github.com/containerd/containerd/releases/download"

	// containerdTarPrefix is the path prefix for binaries within the containerd tar archive.
	containerdTarPrefix = "bin/"

	// runcDefaultBaseURL is the upstream base URL for runc releases.
	// Mirrors must preserve the <base>/v<ver>/<asset> layout.
	runcDefaultBaseURL = "https://github.com/opencontainers/runc/releases/download"
)

// containerdBinaries lists all binaries included in containerd releases.
var containerdBinaries = []string{
	"ctr",
	"containerd",
	"containerd-shim-runc-v2",
	"containerd-stress",
}

type downloadCRIBinaries struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

// DownloadCRIBinaries returns a task that downloads and installs containerd and runc binaries into the rootfs.
// It skips each download if the installed version already matches.
func DownloadCRIBinaries(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &downloadCRIBinaries{log: log, goalState: goalState}
}

func (d *downloadCRIBinaries) Name() string { return "download-cri-binaries" }

func (d *downloadCRIBinaries) Do(ctx context.Context) error {
	destDir := filepath.Join(d.goalState.MachineDir, goalstates.BinDir)

	containerdVersion := d.goalState.ContainerdVersion
	runcVersion := d.goalState.RunCVersion

	var (
		containerdOverride *goalstates.DownloadSource
		runcOverride       *goalstates.DownloadSource
	)

	if d.goalState.Downloads != nil {
		containerdOverride = d.goalState.Downloads.Containerd
		runcOverride = d.goalState.Downloads.Runc
	}

	if containerdOverride != nil && containerdOverride.Version != "" {
		containerdVersion = containerdOverride.Version
	}

	if runcOverride != nil && runcOverride.Version != "" {
		runcVersion = runcOverride.Version
	}

	containerdURL := containerdDownloadURL(containerdOverride, containerdVersion, d.goalState.HostArch)
	runcURL := runcDownloadURL(runcOverride, runcVersion, d.goalState.HostArch)

	if !containerdVersionMatch(ctx, d.log, destDir, containerdVersion) {
		if err := downloadContainerd(ctx, containerdURL, destDir); err != nil {
			return err
		}
	}

	if !runcVersionMatch(ctx, d.log, destDir, runcVersion) {
		if err := downloadRunc(ctx, runcURL, destDir); err != nil {
			return err
		}
	}

	return nil
}

// containerdDownloadURL resolves the containerd release tarball URL,
// honoring BaseURL / URL overrides. The upstream path-and-filename layout
// (containerd-<ver>-linux-<arch>.tar.gz) is preserved so mirrors must
// publish under the same structure.
func containerdDownloadURL(override *goalstates.DownloadSource, version, arch string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, version, version, arch)
	}

	base := containerdDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	return fmt.Sprintf("%s/v%s/containerd-%s-linux-%s.tar.gz", base, version, version, arch)
}

// runcDownloadURL resolves the runc binary URL, honoring BaseURL / URL
// overrides. The upstream filename (runc.<arch>) is preserved.
func runcDownloadURL(override *goalstates.DownloadSource, version, arch string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, version, arch)
	}

	base := runcDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	return fmt.Sprintf("%s/v%s/runc.%s", base, version, arch)
}

// downloadContainerd downloads and extracts containerd binaries from a tar.gz archive.
func downloadContainerd(ctx context.Context, downloadURL, destDir string) error {
	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return fmt.Errorf("decompress containerd tar: %w", err)
		}

		if !strings.HasPrefix(tarFile.Name, containerdTarPrefix) {
			continue
		}

		binaryName := strings.TrimPrefix(tarFile.Name, containerdTarPrefix)
		targetFilePath := filepath.Join(destDir, binaryName)

		if err := utilio.InstallFile(targetFilePath, tarFile.Body, 0o755); err != nil {
			return fmt.Errorf("install containerd binary %q: %w", targetFilePath, err)
		}
	}

	return nil
}

// downloadRunc downloads the runc binary directly.
func downloadRunc(ctx context.Context, downloadURL, destDir string) error {
	runcPath := filepath.Join(destDir, "runc")
	if err := utilio.DownloadToLocalFile(ctx, downloadURL, runcPath, 0o755); err != nil {
		return fmt.Errorf("download runc: %w", err)
	}

	return nil
}

// containerdVersionMatch checks if all containerd binaries are installed and the version matches.
func containerdVersionMatch(ctx context.Context, log *slog.Logger, destDir, expectedVersion string) bool {
	for _, binary := range containerdBinaries {
		binaryPath := filepath.Join(destDir, binary)
		if !utilio.IsExecutable(binaryPath) {
			return false
		}
	}

	containerdPath := filepath.Join(destDir, "containerd")

	output, err := utilexec.OutputCmd(ctx, log, containerdPath, "--version")
	if err != nil {
		return false
	}

	return strings.Contains(output, expectedVersion)
}

// runcVersionMatch checks if runc is installed and the version matches.
func runcVersionMatch(ctx context.Context, log *slog.Logger, destDir, expectedVersion string) bool {
	runcPath := filepath.Join(destDir, "runc")
	if !utilio.IsExecutable(runcPath) {
		return false
	}

	output, err := utilexec.OutputCmd(ctx, log, runcPath, "--version")
	if err != nil {
		return false
	}

	return strings.Contains(output, expectedVersion)
}
