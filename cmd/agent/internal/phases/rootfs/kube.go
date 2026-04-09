// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilexec"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilio"
)

const (
	// kubernetesBinaryURLTemplate is the download URL template for individual Kubernetes binaries
	// from the official Kubernetes release CDN.
	// Parameters: version, arch, binary name.
	kubernetesBinaryURLTemplate = "https://dl.k8s.io/v%s/bin/linux/%s/%s"

	// kubernetesChecksumURLTemplate is the URL template for SHA256 checksum files
	// corresponding to each Kubernetes binary.
	// Parameters: version, arch, binary name.
	kubernetesChecksumURLTemplate = "https://dl.k8s.io/v%s/bin/linux/%s/%s.sha256"
)

// requiredKubeBinaries lists the Kubernetes binaries that must be present for a valid installation.
var requiredKubeBinaries = []string{
	"kubeadm",
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

	if hasRequiredKubeBinaries(destDir) && kubeletVersionMatch(ctx, d.log, destDir, d.goalState.KubernetesVersion) {
		return nil
	}

	version := d.goalState.KubernetesVersion
	arch := d.goalState.HostArch

	eg, ctx := errgroup.WithContext(ctx)

	for _, binary := range requiredKubeBinaries {
		binaryURL := fmt.Sprintf(kubernetesBinaryURLTemplate, version, arch, binary)
		checksumURL := fmt.Sprintf(kubernetesChecksumURLTemplate, version, arch, binary)
		targetFilePath := filepath.Join(destDir, binary)

		eg.Go(d.downloadBinary(ctx, binary, binaryURL, checksumURL, targetFilePath))
	}

	return eg.Wait()
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
