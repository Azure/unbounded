package nodestart

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
)

type installKernelHeader struct {
	log       *slog.Logger
	goalState *goalstates.NodeStart
}

// InstallKernelHeader returns a task that installs the linux-headers package matching the host
// kernel version inside the running nspawn machine.
// TODO: this should be moved back to rootfs phase once switch to OCI image base impl
func InstallKernelHeader(log *slog.Logger, goalState *goalstates.NodeStart) phases.Task {
	return &installKernelHeader{log: log, goalState: goalState}
}

func (i *installKernelHeader) Name() string { return "install-kernel-header" }

func (i *installKernelHeader) Do(ctx context.Context) error {
	headerPkg := "linux-headers-" + i.goalState.HostKernel

	// Skip if already installed.
	if machineDebianPackageInstalled(ctx, i.log, i.goalState.MachineName, headerPkg) {
		return nil
	}

	// Run apt-get update inside the machine.
	if _, err := machineRun(ctx, i.log, i.goalState.MachineName,
		"apt-get", "update", "-y",
	); err != nil {
		return fmt.Errorf("apt-get update in %s: %w", i.goalState.MachineName, err)
	}

	// Install the linux-headers package matching the host kernel version.
	if _, err := machineRun(ctx, i.log, i.goalState.MachineName,
		"apt-get", "install", "-y", "--no-install-recommends", headerPkg,
	); err != nil {
		return fmt.Errorf("apt-get install %s in %s: %w", headerPkg, i.goalState.MachineName, err)
	}

	return nil
}

// machineDebianPackageInstalled checks whether a Debian package is fully
// installed inside the named nspawn machine using dpkg-query.
func machineDebianPackageInstalled(ctx context.Context, log *slog.Logger, machine, pkg string) bool {
	output, err := machineRun(ctx, log, machine,
		"dpkg-query", "--show", "--showformat=${db:Status-Status}", pkg,
	)
	if err != nil {
		return false
	}

	return strings.TrimSpace(output) == "installed"
}
