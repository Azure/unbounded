// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Azure/unbounded/internal/agentartifacts"
	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// requiredKubeBinaries lists the Kubernetes binaries that must be present for a valid installation.
var requiredKubeBinaries = []string{
	"kubelet",
	"kubectl",
	"kube-proxy",
}

type downloadKubeBinaries struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

// DownloadKubeBinaries returns a task that downloads and installs Kubernetes node binaries into the rootfs.
// It skips the download if all required binaries are already installed and the kubelet version matches.
// Each binary is downloaded individually from the official Kubernetes release CDN (dl.k8s.io)
// and verified against its published SHA256 checksum.
func DownloadKubeBinaries(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &downloadKubeBinaries{log: log, goalState: goalState}
}

func (d *downloadKubeBinaries) Name() string { return "download-kube-binaries" }

func (d *downloadKubeBinaries) Do(ctx context.Context) error {
	destDir := filepath.Join(d.goalState.MachineDir, goalstates.BinDir)
	kubeOverride := kubernetesDownloadSource(d.goalState)
	crictlOverride := crictlDownloadSource(d.goalState)
	kubernetesVersion := downloadSourceVersion(d.goalState.KubernetesVersion, kubeOverride)

	crictlVersion, err := resolveCrictlVersion(crictlOverride, kubernetesVersion)
	if err != nil {
		return fmt.Errorf("resolve crictl version: %w", err)
	}

	needsKubeBinaries := !hasRequiredKubeBinaries(destDir) || !kubeletVersionMatch(ctx, d.log, destDir, kubernetesVersion)

	needsCrictl := !crictlVersionMatch(ctx, d.log, destDir, crictlVersion)
	if !needsKubeBinaries && !needsCrictl {
		return nil
	}

	arch := d.goalState.HostArch

	eg, ctx := errgroup.WithContext(ctx)

	if needsKubeBinaries {
		if err := d.enqueueKubernetesBinaryDownloads(ctx, eg, kubeOverride, kubernetesVersion, arch, destDir); err != nil {
			return err
		}
	}

	if needsCrictl {
		if err := d.enqueueCrictlDownload(ctx, eg, crictlOverride, crictlVersion, arch, destDir); err != nil {
			return err
		}
	}

	return eg.Wait()
}

func (d *downloadKubeBinaries) enqueueKubernetesBinaryDownloads(ctx context.Context, eg *errgroup.Group, override *goalstates.DownloadSource, kubernetesVersion, arch, destDir string) error {
	for _, binary := range requiredKubeBinaries {
		binaryURL, err := kubernetesBinaryURL(override, kubernetesVersion, arch, binary)
		if err != nil {
			return fmt.Errorf("resolve kubernetes binary download source %q: %w", binary, err)
		}

		targetFilePath := filepath.Join(destDir, binary)

		eg.Go(d.downloadBinary(ctx, binary, binaryURL, targetFilePath))
	}

	return nil
}

func (d *downloadKubeBinaries) enqueueCrictlDownload(ctx context.Context, eg *errgroup.Group, override *goalstates.DownloadSource, crictlVersion, arch, destDir string) error {
	downloadURL, err := crictlDownloadURL(override, crictlVersion, runtime.GOOS, arch)
	if err != nil {
		return fmt.Errorf("resolve crictl download source: %w", err)
	}

	targetFilePath := filepath.Join(destDir, "crictl")
	eg.Go(d.downloadCrictlBinary(ctx, downloadURL, targetFilePath))

	return nil
}

// downloadBinary returns a function that downloads a single Kubernetes binary,
// verifies its SHA256 checksum, and logs the duration of the download.
func (d *downloadKubeBinaries) downloadBinary(ctx context.Context, binary string, binaryURL downloadSource, targetFilePath string) func() error {
	return func() error {
		logger := d.log.With("binary", binary, "url", binaryURL.String())

		logger.Info("downloading kubernetes binary")

		start := time.Now()

		if err := binaryURL.downloadWithSHA256Verification(ctx, binaryURL.checksumSource(), targetFilePath, 0o755); err != nil {
			logger.Error("download failed", "error", err)
			return fmt.Errorf("download kubernetes binary %q: %w", binary, err)
		}

		logger.Info("downloaded kubernetes binary", "duration", time.Since(start))

		return nil
	}
}

// downloadCrictlBinary returns a function that downloads the crictl tarball and installs the crictl binary.
func (d *downloadKubeBinaries) downloadCrictlBinary(ctx context.Context, downloadURL downloadSource, targetFilePath string) func() error {
	return func() error {
		logger := d.log.With("binary", "crictl", "url", downloadURL.String())

		logger.Info("downloading cri-tools binary")

		start := time.Now()
		found := false

		for tarFile, err := range downloadURL.decompressTarGz(ctx) {
			if err != nil {
				logger.Error("download failed", "error", err)
				return fmt.Errorf("download crictl archive: %w", err)
			}

			if tarFile.Name != "crictl" {
				continue
			}

			found = true

			if err := utilio.InstallFile(targetFilePath, tarFile.Body, 0o755); err != nil {
				logger.Error("install failed", "error", err)
				return fmt.Errorf("install crictl binary %q: %w", targetFilePath, err)
			}

			break
		}

		if !found {
			return fmt.Errorf("crictl binary not found in archive %q", downloadURL.String())
		}

		logger.Info("downloaded cri-tools binary", "duration", time.Since(start))

		return nil
	}
}

// hasRequiredKubeBinaries checks if all required Kubernetes binaries are installed and executable.
func hasRequiredKubeBinaries(destDir string) bool {
	for _, binary := range requiredKubeBinaries {
		binaryPath := filepath.Join(destDir, binary)
		if !utilio.IsExecutable(binaryPath) {
			return false
		}
	}

	return true
}

// kubeletVersionMatch checks if the installed kubelet version matches the expected version.
func kubeletVersionMatch(ctx context.Context, log *slog.Logger, destDir, expectedVersion string) bool {
	kubeletPath := filepath.Join(destDir, "kubelet")
	if !utilio.IsExecutable(kubeletPath) {
		return false
	}

	output, err := executil.OutputCmd(ctx, log, kubeletPath, "--version")
	if err != nil {
		return false
	}

	// output example: "Kubernetes v1.27.3"
	parts := strings.Fields(output)
	if len(parts) != 2 {
		return false
	}

	kubeletVersion := strings.TrimPrefix(parts[1], "v")

	return kubeletVersion == expectedVersion
}

// crictlVersionMatch checks if the installed crictl version matches the expected version.
func crictlVersionMatch(ctx context.Context, log *slog.Logger, destDir, expectedVersion string) bool {
	crictlPath := filepath.Join(destDir, "crictl")
	if !utilio.IsExecutable(crictlPath) {
		return false
	}

	output, err := executil.OutputCmd(ctx, log, crictlPath, "--version")
	if err != nil {
		return false
	}

	parts := strings.Fields(output)
	if len(parts) != 3 {
		return false
	}

	return parts[2] == "v"+expectedVersion
}

// resolveCrictlVersion resolves the cri-tools version to use, preferring
// a user-supplied override and otherwise aligning to the cluster's
// Kubernetes minor version.
func resolveCrictlVersion(override *goalstates.DownloadSource, kubernetesVersion string) (string, error) {
	if version := downloadSourceVersion("", override); version != "" {
		return version, nil
	}

	return crictlVersionForKubernetesVersion(kubernetesVersion)
}

// crictlVersionForKubernetesVersion returns the cri-tools version for the Kubernetes major.minor release.
// cri-tools releases are published as v<major>.<minor>.0.
func crictlVersionForKubernetesVersion(kubernetesVersion string) (string, error) {
	return agentartifacts.CrictlVersionForKubernetesVersion(kubernetesVersion)
}

// kubernetesBinaryURL resolves the download URL for a kubernetes binary
// (kubelet, kubectl, kube-proxy) honoring the optional override.
func kubernetesBinaryURL(override *goalstates.DownloadSource, version, arch, binary string) (downloadSource, error) {
	return parseDownloadSource(agentartifacts.KubernetesBinary(override, version, arch, binary))
}

// crictlDownloadURL resolves the cri-tools crictl tarball URL honoring
// the optional override. Mirrors must publish assets under the same
// <base>/v<ver>/<asset> layout as GitHub releases.
func crictlDownloadURL(override *goalstates.DownloadSource, version, hostOS, hostArch string) (downloadSource, error) {
	return parseDownloadSource(agentartifacts.CrictlArchive(override, version, hostOS, hostArch))
}
