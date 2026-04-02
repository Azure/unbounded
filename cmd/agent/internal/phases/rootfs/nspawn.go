package rootfs

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases/rootfs/debootstrap"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases/rootfs/oci"
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

// EnsureNSpawnWorkspace returns a task that bootstraps an Ubuntu rootfs into
// the machine directory (if it is empty or missing) and writes the
// systemd-nspawn configuration files needed to run a Kubernetes node inside a
// nspawn container.
func EnsureNSpawnWorkspace(log *slog.Logger, goalState *goalstates.RootFS) phases.Task {
	return &ensureNSpawnWorkspace{log: log, goalState: goalState}
}

func (e *ensureNSpawnWorkspace) Name() string { return "ensure-nspawn-workspace" }

func (e *ensureNSpawnWorkspace) Do(ctx context.Context) error {
	if err := e.bootstrapWorkspace(ctx); err != nil {
		return fmt.Errorf("bootstrap machine directory %s: %w", e.goalState.MachineDir, err)
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

func (e *ensureNSpawnWorkspace) bootstrapWorkspace(ctx context.Context) error {
	var bootstrapTask phases.Task

	if image := e.goalState.OCIImage; image != "" {
		bootstrapTask = oci.DownloadRootFS(e.log, e.goalState.MachineDir, e.goalState.HostArch, image)
	} else {
		bootstrapTask = debootstrap.Ubuntu(e.log, e.goalState.MachineDir)
	}

	return phases.ExecuteTask(ctx, e.log, bootstrapTask)
}
