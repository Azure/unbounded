package reset

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/utilexec"
)

const hostSysctlPath = "/etc/sysctl.d/99-kubernetes.conf"

type removeSysctlConfig struct {
	log *slog.Logger
}

// RemoveSysctlConfig returns a task that removes the Kubernetes sysctl
// configuration file and reloads sysctl settings.
func RemoveSysctlConfig(log *slog.Logger) phases.Task {
	return &removeSysctlConfig{log: log}
}

func (t *removeSysctlConfig) Name() string { return "remove-sysctl-config" }

func (t *removeSysctlConfig) Do(ctx context.Context) error {
	t.log.Info("removing Kubernetes sysctl configuration")

	removeFileIfExists(hostSysctlPath)

	// Reload sysctl to apply the removal.
	_ = utilexec.RunCmd(ctx, t.log, utilexec.Sysctl(), "--system")

	return nil
}

type restoreDocker struct {
	log *slog.Logger
}

// RestoreDocker returns a task that unmasks the Docker service and socket, and
// removes the daemon.json configuration file.
func RestoreDocker(log *slog.Logger) phases.Task {
	return &restoreDocker{log: log}
}

func (t *restoreDocker) Name() string { return "restore-docker" }

func (t *restoreDocker) Do(ctx context.Context) error {
	t.log.Info("restoring Docker configuration")

	systemctl := utilexec.Systemctl()

	_ = utilexec.RunCmd(ctx, t.log, systemctl, "unmask", "docker.service")
	_ = utilexec.RunCmd(ctx, t.log, systemctl, "unmask", "docker.socket")

	removeFileIfExists("/etc/docker/daemon.json")

	return nil
}

const (
	fstabPath    = "/etc/fstab"
	fstabBakPath = "/etc/fstab.bak"
)

type restoreSwap struct {
	log *slog.Logger
}

// RestoreSwap returns a task that restores the original /etc/fstab from the
// backup created during bootstrap and re-enables swap.
func RestoreSwap(log *slog.Logger) phases.Task {
	return &restoreSwap{log: log}
}

func (t *restoreSwap) Name() string { return "restore-swap" }

func (t *restoreSwap) Do(ctx context.Context) error {
	if _, err := os.Stat(fstabBakPath); errors.Is(err, os.ErrNotExist) {
		t.log.Info("no fstab backup found, skipping swap restore")
		return nil
	}

	t.log.Info("restoring swap from fstab backup")

	data, err := os.ReadFile(fstabBakPath) //#nosec G304 -- trusted path
	if err != nil {
		return err
	}

	if err := os.WriteFile(fstabPath, data, 0o644); err != nil { //#nosec G306 -- standard fstab permissions
		return err
	}

	removeFileIfExists(fstabBakPath)

	// Re-enable swap (ignore errors if no swap partitions are present).
	_ = utilexec.RunCmd(ctx, t.log, swapon(), "-a")

	return nil
}
