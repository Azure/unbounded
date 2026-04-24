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

	"github.com/Masterminds/semver/v3"
	"golang.org/x/sync/errgroup"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/internal/utilexec"
	"github.com/Azure/unbounded/pkg/agent/internal/utilio"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	// kubernetesDefaultBaseURL is the upstream base URL for Kubernetes
	// binary releases. Mirrors must preserve the <base>/v<ver>/bin/linux/<arch>/
	// layout used by dl.k8s.io.
	kubernetesDefaultBaseURL = "https://dl.k8s.io"

	// criToolsDefaultBaseURL is the upstream base URL for cri-tools releases.
	criToolsDefaultBaseURL = "https://github.com/kubernetes-sigs/cri-tools/releases/download"
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
	kubernetesVersion := d.goalState.KubernetesVersion

	var (
		kubeOverride   *goalstates.DownloadSource
		crictlOverride *goalstates.DownloadSource
	)

	if d.goalState.Downloads != nil {
		kubeOverride = d.goalState.Downloads.Kubernetes
		crictlOverride = d.goalState.Downloads.Crictl
	}

	if kubeOverride != nil && kubeOverride.Version != "" {
		kubernetesVersion = kubeOverride.Version
	}

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
		d.enqueueKubernetesBinaryDownloads(ctx, eg, kubeOverride, kubernetesVersion, arch, destDir)
	}

	if needsCrictl {
		d.enqueueCrictlDownload(ctx, eg, crictlOverride, crictlVersion, arch, destDir)
	}

	return eg.Wait()
}

func (d *downloadKubeBinaries) enqueueKubernetesBinaryDownloads(ctx context.Context, eg *errgroup.Group, override *goalstates.DownloadSource, kubernetesVersion, arch, destDir string) {
	for _, binary := range requiredKubeBinaries {
		binaryURL := kubernetesBinaryURL(override, kubernetesVersion, arch, binary)
		checksumURL := kubernetesBinaryURL(override, kubernetesVersion, arch, binary) + ".sha256"
		targetFilePath := filepath.Join(destDir, binary)

		eg.Go(d.downloadBinary(ctx, binary, binaryURL, checksumURL, targetFilePath))
	}
}

func (d *downloadKubeBinaries) enqueueCrictlDownload(ctx context.Context, eg *errgroup.Group, override *goalstates.DownloadSource, crictlVersion, arch, destDir string) {
	downloadURL := crictlDownloadURL(override, crictlVersion, runtime.GOOS, arch)
	targetFilePath := filepath.Join(destDir, "crictl")
	eg.Go(d.downloadCrictlBinary(ctx, downloadURL, targetFilePath))
}

// downloadBinary returns a function that downloads a single Kubernetes binary,
// verifies its SHA256 checksum, and logs the duration of the download.
func (d *downloadKubeBinaries) downloadBinary(ctx context.Context, binary, binaryURL, checksumURL, targetFilePath string) func() error {
	return func() error {
		logger := d.log.With("binary", binary, "url", binaryURL)

		logger.Info("downloading kubernetes binary")

		start := time.Now()

		if err := utilio.DownloadWithSHA256Verification(ctx, binaryURL, checksumURL, targetFilePath, 0o755); err != nil {
			logger.Error("download failed", "error", err)
			return fmt.Errorf("download kubernetes binary %q: %w", binary, err)
		}

		logger.Info("downloaded kubernetes binary", "duration", time.Since(start))

		return nil
	}
}

// downloadCrictlBinary returns a function that downloads the crictl tarball and installs the crictl binary.
func (d *downloadKubeBinaries) downloadCrictlBinary(ctx context.Context, downloadURL, targetFilePath string) func() error {
	return func() error {
		logger := d.log.With("binary", "crictl", "url", downloadURL)

		logger.Info("downloading cri-tools binary")

		start := time.Now()
		found := false

		for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
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
			return fmt.Errorf("crictl binary not found in archive %q", downloadURL)
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

	output, err := utilexec.OutputCmd(ctx, log, kubeletPath, "--version")
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

	output, err := utilexec.OutputCmd(ctx, log, crictlPath, "--version")
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
	if override != nil && override.Version != "" {
		return override.Version, nil
	}

	return crictlVersionForKubernetesVersion(kubernetesVersion)
}

// crictlVersionForKubernetesVersion returns the cri-tools version for the Kubernetes major.minor release.
// cri-tools releases are published as v<major>.<minor>.0.
func crictlVersionForKubernetesVersion(kubernetesVersion string) (string, error) {
	version, err := semver.NewVersion(strings.TrimSpace(kubernetesVersion))
	if err != nil {
		return "", fmt.Errorf("parse kubernetes version %q: %w", kubernetesVersion, err)
	}

	return fmt.Sprintf("%d.%d.0", version.Major(), version.Minor()), nil
}

// kubernetesBinaryURL resolves the download URL for a kubernetes binary
// (kubelet, kubectl, kube-proxy) honoring the optional override.
func kubernetesBinaryURL(override *goalstates.DownloadSource, version, arch, binary string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, version, arch, binary)
	}

	base := kubernetesDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	return fmt.Sprintf("%s/v%s/bin/linux/%s/%s", base, version, arch, binary)
}

// crictlDownloadURL resolves the cri-tools crictl tarball URL honoring
// the optional override. Mirrors must publish assets under the same
// <base>/v<ver>/<asset> layout as GitHub releases.
func crictlDownloadURL(override *goalstates.DownloadSource, version, hostOS, hostArch string) string {
	if override != nil && override.URL != "" {
		return fmt.Sprintf(override.URL, version, version, hostOS, hostArch)
	}

	base := criToolsDefaultBaseURL
	if override != nil && override.BaseURL != "" {
		base = strings.TrimRight(override.BaseURL, "/")
	}

	return fmt.Sprintf("%s/v%s/crictl-v%s-%s-%s.tar.gz", base, version, version, hostOS, hostArch)
}
