// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package host

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/Azure/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/phases"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilexec"
	"github.com/Azure/unbounded-kube/cmd/agent/internal/utilio"
)

//go:embed assets/unbounded-agent-daemon.service
var daemonServiceContent []byte

//go:embed assets/unbounded-agent-daemon-recovery.service
var daemonRecoveryServiceContent []byte

type enableDaemon struct {
	log *slog.Logger
}

// EnableDaemon returns a task that installs, enables, and starts the
// unbounded-agent-daemon systemd unit on the host. The unit runs
// "unbounded-agent daemon" which watches the Machine CR for this node
// and reconciles the local state to match.
func EnableDaemon(log *slog.Logger) phases.Task {
	return &enableDaemon{log: log}
}

func (d *enableDaemon) Name() string { return "enable-daemon" }

func (d *enableDaemon) Do(ctx context.Context) error {
	unitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonUnit)
	recoveryUnitPath := filepath.Join(goalstates.SystemdSystemDir, goalstates.DaemonRecoveryUnit)

	if err := utilio.WriteFile(unitPath, daemonServiceContent, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", unitPath, err)
	}

	if err := utilio.WriteFile(recoveryUnitPath, daemonRecoveryServiceContent, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", recoveryUnitPath, err)
	}

	systemctl := utilexec.Systemctl()

	if err := utilexec.RunCmd(ctx, d.log, systemctl, "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}

	if err := utilexec.RunCmd(ctx, d.log, systemctl, "enable", goalstates.DaemonUnit); err != nil {
		return fmt.Errorf("systemctl enable %s: %w", goalstates.DaemonUnit, err)
	}

	if err := utilexec.RunCmd(ctx, d.log, systemctl, "start", goalstates.DaemonUnit); err != nil {
		return fmt.Errorf("systemctl start %s: %w", goalstates.DaemonUnit, err)
	}

	d.log.Info("daemon unit started", "unit", goalstates.DaemonUnit)

	return nil
}
