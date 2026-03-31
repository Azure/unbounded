package rootfs

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/utilexec"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/utilio"
)

//go:embed assets/nspawn.conf
var nspawnConfig []byte

//go:embed assets/service-override.conf
var serviceOverrideConfig []byte

type ensureNSpawnWorkspace struct {
	log       *slog.Logger
	goalState *goalstates.RootFS
}

// EnsureNSpawnWorkspace returns a task that bootstraps an Ubuntu rootfs into the machine
// directory using debootstrap and writes the systemd-nspawn configuration files needed
// to run a Kubernetes node inside a nspawn container.
//
// NOTE: This currently uses debootstrap with the "noble" suite and is Ubuntu-only.
func EnsureNSpawnWorkspace(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &ensureNSpawnWorkspace{log: log, goalState: goalState}
}

func (e *ensureNSpawnWorkspace) Name() string { return "ensure-nspawn-workspace" }

func (e *ensureNSpawnWorkspace) Do(ctx context.Context) error {
	// Bootstrap the machine rootfs using debootstrap if the machine directory
	// is empty or does not exist yet.
	empty, err := isDirEmpty(e.goalState.MachineDir)
	if err != nil {
		return fmt.Errorf("check machine directory %s: %w", e.goalState.MachineDir, err)
	}

	if empty {
		const cacheDir = "/tmp/unbounded-agent/debootstrap-cache/noble"
		if err := os.MkdirAll(cacheDir, 0o644); err != nil {
			return err
		}

		// debootstrap is Ubuntu-only: it bootstraps an Ubuntu "noble" rootfs with
		// the minimum set of packages required to run a Kubernetes node.
		packages := []string{
			"systemd",
			"systemd-sysv",
			"dbus",
			"curl",
			"ca-certificates",
			"iproute2",
			"iptables",
			"kmod",
			"udev",
			"procps",
			"nano",
			// FIXME: cannot install linux headers here even with
			//   --extra-suites=noble noble-updates noble-security noble-backports
			// seeing error when trying to install libpam-modules-bin
			// Deferring this step to nodestart phase for now
			// "linux-headers-" + e.goalState.HostKernel,
		}

		if err := utilexec.RunCmd(ctx, e.log, utilexec.Debootstrap(),
			"--include="+strings.Join(packages, ","),
			"--cache-dir="+cacheDir,
			"noble",
			e.goalState.MachineDir,
		); err != nil {
			return fmt.Errorf("debootstrap into %s: %w", e.goalState.MachineDir, err)
		}
	}

	// Write the .nspawn configuration file.
	if err := utilio.WriteFile(e.goalState.NSpawnConfigFile, nspawnConfig, 0o644); err != nil {
		return fmt.Errorf("write nspawn config %s: %w", e.goalState.NSpawnConfigFile, err)
	}

	// Write the systemd service override drop-in.
	if err := utilio.WriteFile(e.goalState.ServiceOverrideFile, serviceOverrideConfig, 0o644); err != nil {
		return fmt.Errorf("write service override %s: %w", e.goalState.ServiceOverrideFile, err)
	}

	return nil
}

// isDirEmpty reports whether dir is empty or does not exist.
func isDirEmpty(dir string) (bool, error) {
	f, err := os.Open(dir)
	if os.IsNotExist(err) {
		return true, nil
	}

	if err != nil {
		return false, err
	}

	names, err := f.Readdirnames(1)
	closeErr := f.Close()

	if err != nil {
		// io.EOF means the directory is empty.
		return true, nil //nolint:nilerr // io.EOF is expected for empty dirs.
	}

	if closeErr != nil {
		return false, closeErr
	}

	return len(names) == 0, nil
}
