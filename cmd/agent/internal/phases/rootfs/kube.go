package rootfs

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/utilexec"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/utilio"
)

const (
	// kubernetesURLTemplate is the download URL template for Kubernetes node binaries.
	// Parameters: version, arch.
	kubernetesURLTemplate = "https://acs-mirror.azureedge.net/kubernetes/v%s/binaries/kubernetes-node-linux-%s.tar.gz"

	// kubernetesTarPrefix is the path prefix for binaries within the Kubernetes tar archive.
	kubernetesTarPrefix = "kubernetes/node/bin/"
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
func DownloadKubeBinaries(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &downloadKubeBinaries{log: log, goalState: goalState}
}

func (d *downloadKubeBinaries) Name() string { return "download-kube-binaries" }

func (d *downloadKubeBinaries) Do(ctx context.Context) error {
	destDir := filepath.Join(d.goalState.MachineDir, goalstates.BinDir)
	downloadURL := fmt.Sprintf(kubernetesURLTemplate, d.goalState.KubernetesVersion, d.goalState.HostArch)

	if hasRequiredKubeBinaries(destDir) && kubeletVersionMatch(ctx, d.log, destDir, d.goalState.KubernetesVersion) {
		return nil
	}

	for tarFile, err := range utilio.DecompressTarGzFromRemote(ctx, downloadURL) {
		if err != nil {
			return fmt.Errorf("decompress kubernetes tar: %w", err)
		}

		if !strings.HasPrefix(tarFile.Name, kubernetesTarPrefix) {
			continue
		}

		binaryName := strings.TrimPrefix(tarFile.Name, kubernetesTarPrefix)
		targetFilePath := filepath.Join(destDir, binaryName)

		if err := utilio.InstallFile(targetFilePath, tarFile.Body, 0o755); err != nil {
			return fmt.Errorf("install kubernetes binary %q: %w", targetFilePath, err)
		}
	}

	return nil
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
